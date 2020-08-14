package llm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/llm/chaninfo"
	"github.com/lightninglabs/llm/clientdb"
	"github.com/lightninglabs/llm/order"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/sync/errgroup"
)

type fundingMgr struct {
	db              *clientdb.DB
	walletKit       lndclient.WalletKitClient
	lightningClient lndclient.LightningClient
	baseClient      lnrpc.LightningClient

	// pendingOpenChannels is a channel through which we'll receive
	// notifications for pending open channels resulting from a successful
	// batch.
	pendingOpenChannels <-chan *lnrpc.ChannelEventUpdate_PendingOpenChannel

	quit chan struct{}
}

// deriveFundingShim generates the proper funding shim that should be used by
// the maker or taker to properly make a channel that stems off the main batch
// funding transaction.
func (f *fundingMgr) deriveFundingShim(ourOrder order.Order,
	matchedOrder *order.MatchedOrder,
	batchTx *wire.MsgTx) (*lnrpc.FundingShim, error) {

	rpcLog.Infof("Registering funding shim for Order(type=%v, amt=%v, "+
		"nonce=%v", ourOrder.Type(),
		matchedOrder.UnitsFilled.ToSatoshis(), ourOrder.Nonce())

	// First, we'll compute the pending channel ID key which will be unique
	// to this order pair.
	var (
		askNonce, bidNonce order.Nonce

		thawHeight uint32
	)
	if ourOrder.Type() == order.TypeBid {
		bidNonce = ourOrder.Nonce()
		askNonce = matchedOrder.Order.Nonce()

		thawHeight = ourOrder.(*order.Bid).MinDuration
	} else {
		bidNonce = matchedOrder.Order.Nonce()
		askNonce = ourOrder.Nonce()

		thawHeight = matchedOrder.Order.(*order.Bid).MinDuration
	}

	pendingChanID := order.PendingChanKey(
		askNonce, bidNonce,
	)
	chanSize := matchedOrder.UnitsFilled.ToSatoshis()

	// Next, we'll need to find the location of this channel output on the
	// funding transaction, so we'll re-compute the funding script from
	// scratch.
	ctxb := context.Background()
	ourKeyLocator := ourOrder.Details().MultiSigKeyLocator
	ourMultiSigKey, err := f.walletKit.DeriveKey(
		ctxb, &ourKeyLocator,
	)
	if err != nil {
		return nil, err
	}
	_, fundingOutput, err := input.GenFundingPkScript(
		ourMultiSigKey.PubKey.SerializeCompressed(),
		matchedOrder.MultiSigKey[:], int64(chanSize),
	)
	if err != nil {
		return nil, err
	}

	// Now that we have the funding script, we'll find the output index
	// within the batch execution transaction. We ignore the first
	// argument, as earlier during validation, we would've rejected the
	// batch if it wasn't found.
	batchTxID := batchTx.TxHash()
	_, chanOutputIndex := input.FindScriptOutputIndex(
		batchTx, fundingOutput.PkScript,
	)
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: batchTxID[:],
		},
		OutputIndex: chanOutputIndex,
	}

	// With all the components assembled, we'll now create the chan point
	// shim, and register it so we use the proper funding key when we
	// receive the marker's incoming funding request.
	chanPointShim := &lnrpc.ChanPointShim{
		Amt:       int64(chanSize),
		ChanPoint: chanPoint,
		LocalKey: &lnrpc.KeyDescriptor{
			RawKeyBytes: ourMultiSigKey.PubKey.SerializeCompressed(),
			KeyLoc: &lnrpc.KeyLocator{
				KeyFamily: int32(ourKeyLocator.Family),
				KeyIndex:  int32(ourKeyLocator.Index),
			},
		},
		RemoteKey:     matchedOrder.MultiSigKey[:],
		PendingChanId: pendingChanID[:],
		ThawHeight:    thawHeight,
	}

	return &lnrpc.FundingShim{
		Shim: &lnrpc.FundingShim_ChanPointShim{
			ChanPointShim: chanPointShim,
		},
	}, nil
}

// registerFundingShim is used when we're on the taker (our bid was executed)
// side of a new matched order. To prepare ourselves for their incoming funding
// request, we'll register a shim with all the expected parameters.
func (f *fundingMgr) registerFundingShim(ourOrder order.Order,
	matchedOrder *order.MatchedOrder, batchTx *wire.MsgTx) error {

	ctxb := context.Background()

	fundingShim, err := f.deriveFundingShim(ourOrder, matchedOrder, batchTx)
	if err != nil {
		return err
	}
	_, err = f.baseClient.FundingStateStep(
		ctxb, &lnrpc.FundingTransitionMsg{
			Trigger: &lnrpc.FundingTransitionMsg_ShimRegister{
				ShimRegister: fundingShim,
			},
		},
	)
	if err != nil {
		return fmt.Errorf("unable to register funding shim: %v", err)
	}

	return nil
}

// prepChannelFunding preps the backing node to either receive or initiate a
// channel funding based on the items in the order batch.
func (f *fundingMgr) prepChannelFunding(batch *order.Batch) error {
	rpcLog.Infof("Batch(%x): preparing channel funding for %v orders",
		batch.ID[:], len(batch.MatchedOrders))

	// Now that we know this batch passes our sanity checks, we'll register
	// all the funding shims we need to be able to respond
	for ourOrderNonce, matchedOrders := range batch.MatchedOrders {
		ourOrder, err := f.db.GetOrder(ourOrderNonce)
		if err != nil {
			return err
		}

		orderIsAsk := ourOrder.Type() == order.TypeAsk

		// Depending on if this is a ask or not, we'll either just try
		// to connect out, or register the full funding shim.
		for _, matchedOrder := range matchedOrders {
			// We only need to create a shim if we're the taker, so
			// if we had an ask matched, then we can skip this
			// step, as we'll be the ones creating the channel.
			if orderIsAsk {
				// However, if we're the one that needs to make
				// the channel, then we'll kick off a
				// persistent connection request here, so we
				// know a connection should be established by
				// the time we actually need to fund the
				// channel.
				//
				// TODO(roasbeef): info leaks?
				err := connectToMatchedTrader(
					f.baseClient, matchedOrder,
				)
				if err != nil {
					return err
				}

				continue
			}

			// At this point, one of our bids was matched with a
			// series of asks, so we'll now register all the
			// expected funding shims so we can execute the next
			// phase w/o any issues.
			err := f.registerFundingShim(
				ourOrder, matchedOrder, batch.BatchTX,
			)
			if err != nil {
				return fmt.Errorf("unable to register "+
					"funding shim: %v", err)
			}

		}
	}

	return nil
}

// batchChannelSetup will attempt to establish new funding flows with all
// matched takers (people buying our channels) in the passed batch. This method
// will block until the channel is considered pending. Once this phase is
// complete, and the batch execution transaction broadcast, the channel will be
// finalized and locked in.
func (f *fundingMgr) batchChannelSetup(batch *order.Batch) (
	map[wire.OutPoint]*chaninfo.ChannelInfo, error) {

	var eg errgroup.Group
	ctx := context.Background()

	rpcLog.Infof("Batch(%x): opening channels for %v matched orders",
		batch.ID[:], len(batch.MatchedOrders))

	// For each ask order of ours that's matched, we'll make a new funding
	// flow, blocking until they all progress to the final state.
	batchTxHash := batch.BatchTX.TxHash()
	chanPoints := make(map[wire.OutPoint]struct{})
	for ourOrderNonce, matchedOrders := range batch.MatchedOrders {
		ourOrder, err := f.db.GetOrder(ourOrderNonce)
		if err != nil {
			return nil, err
		}

		// We'll obtain the expected channel point for each matched
		// order, and complete the funding flow for each one in which
		// our order was the ask.
		for _, matchedOrder := range matchedOrders {
			fundingShim, err := f.deriveFundingShim(
				ourOrder, matchedOrder, batch.BatchTX,
			)
			if err != nil {
				return nil, err
			}
			chanPoint := wire.OutPoint{
				Hash: batchTxHash,
				Index: fundingShim.GetChanPointShim().ChanPoint.
					OutputIndex,
			}
			chanPoints[chanPoint] = struct{}{}

			// If this is a bid order, then we don't need to do
			// anything, as we should've already registered the
			// funding shim during the prior phase.
			orderIsBid := ourOrder.Type() == order.TypeBid
			if orderIsBid {
				continue
			}

			// Otherwise, we'll now initiate the funding request to
			// establish all the channels generated by this order
			// with the remote parties.
			//
			// Before we make the funding request, we'll make sure
			// that we're connected to the other party.
			err = connectToMatchedTrader(
				f.baseClient, matchedOrder,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to connect to "+
					"trader: %v", err)
			}

			// Now that we know we're connected, we'll launch off
			// the request to initiate channel funding with the
			// remote peer.
			//
			// TODO(roasbeef): sat per byte from order?
			//  * also other params to set as well
			chanAmt := int64(matchedOrder.UnitsFilled.ToSatoshis())
			fundingReq := &lnrpc.OpenChannelRequest{
				NodePubkey:         matchedOrder.NodeKey[:],
				LocalFundingAmount: chanAmt,
				FundingShim:        fundingShim,
			}
			chanStream, err := f.baseClient.OpenChannel(
				ctx, fundingReq,
			)
			if err != nil {
				return nil, err
			}

			// We'll launch a new goroutine to wait until chan
			// pending (funding flow finished) update has been
			// sent.
			eg.Go(func() error {
				for {
					select {

					case <-f.quit:
						return fmt.Errorf("server " +
							"shutting down")
					default:
					}

					msg, err := chanStream.Recv()
					if err != nil {
						rpcLog.Errorf("unable to read "+
							"chan open update event: %v", err)
						return err
					}

					_, ok := msg.Update.(*lnrpc.OpenStatusUpdate_ChanPending)
					if ok {
						return nil
					}
				}
			})
		}
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	// Once we've waited for the operations to complete, we'll wait to
	// receive each channel's pending open notification in order to retrieve
	// some keys from their SCB we'll need to submit to the auctioneer in
	// order for them to enforce the channel's service lifetime.
	channelKeys := make(
		map[wire.OutPoint]*chaninfo.ChannelInfo, len(chanPoints),
	)

	rpcLog.Debugf("Waiting for pending open events for %v channel(s)",
		len(chanPoints))

	timeout := time.After(15 * time.Second)
	for {
		var chanPoint wire.OutPoint
		select {
		case channel := <-f.pendingOpenChannels:
			var hash chainhash.Hash
			copy(hash[:], channel.PendingOpenChannel.Txid)
			chanPoint = wire.OutPoint{
				Hash:  hash,
				Index: channel.PendingOpenChannel.OutputIndex,
			}

		case <-timeout:
			return nil, errors.New("timed out waiting for pending " +
				"open channel notification")

		case <-f.quit:
			return nil, fmt.Errorf("server shutting down")
		}

		// If the notification if for a channel we're not interested in,
		// skip it. This can happen if a channel was opened out-of-band
		// at the same time the batch channels were.
		if _, ok := chanPoints[chanPoint]; !ok {
			continue
		}

		rpcLog.Debugf("Retrieving info for channel %v", chanPoint)

		chanInfo, err := chaninfo.GatherChannelInfo(
			ctx, f.lightningClient, f.walletKit, chanPoint,
		)
		if err != nil {
			return nil, err
		}

		// Once we've retrieved the keys for all channels, we can exit.
		channelKeys[chanPoint] = chanInfo
		delete(chanPoints, chanPoint)
		if len(chanPoints) == 0 {
			break
		}
	}

	return channelKeys, nil
}

// connectToMatchedTrader attempts to connect to a trader that we've had an
// order matched with. We'll attempt to establish a permanent connection as
// well, so we can use the connection for any batch retries that may happen.
func connectToMatchedTrader(lndClient lnrpc.LightningClient,
	matchedOrder *order.MatchedOrder) error {

	ctxb := context.Background()
	nodeKey := hex.EncodeToString(matchedOrder.NodeKey[:])

	for _, addr := range matchedOrder.NodeAddrs {
		rpcLog.Debugf("Connecting to node=%v for order_nonce=%v",
			nodeKey, matchedOrder.Order.Nonce())

		_, err := lndClient.ConnectPeer(ctxb, &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{
				Pubkey: nodeKey,
				Host:   addr.String(),
			},
		})

		if err != nil {
			// If we're already connected, then we can stop now.
			if strings.Contains(err.Error(), "already connected") {
				return nil
			}

			rpcLog.Warnf("unable to connect to trader at %v@%v",
				nodeKey, addr)

			continue
		}

		// Don't spam peer connections. This can lead to race errors in
		// the itest when we try to connect to the same node in very
		// short intervals.
		//
		// TODO(guggero): Fix this problem in lnd and also try to
		// de-duplicate peers and their addresses to reduce connection
		// tries.
		time.Sleep(200 * time.Millisecond)

		// We connected successfully, not need to try any of the other
		// addresses.
		break
	}

	// Since we specified perm, the error is async, and not fully
	// communicated to the caller, so we return nil here. Later on, if we
	// can't fund the channel, then we'll fail with a hard error.
	return nil
}
