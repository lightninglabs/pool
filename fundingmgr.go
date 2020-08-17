package llm

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/llm/chaninfo"
	"github.com/lightninglabs/llm/clientdb"
	"github.com/lightninglabs/llm/clmrpc"
	"github.com/lightninglabs/llm/order"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

var (
	// defaultBatchStepTimeout is the default time we allow an action that
	// blocks the batch conversation (like peer connection establishment or
	// channel open) to take. If any action takes longer, we might reject
	// the order from that slow peer. This value SHOULD be lower than the
	// defaultMsgTimeout on the server side otherwise nodes might get kicked
	// out of the match making process for timing out even though it was
	// their peer's fault.
	defaultBatchStepTimeout = 8 * time.Second
)

// matchRejectErr is an error type that is returned from the funding manager if
// the trader rejects certain orders instead of the whole batch.
type matchRejectErr struct {
	rejectedOrders map[order.Nonce]*clmrpc.OrderReject
}

// Error returns the underlying error string.
//
// NOTE: This is part of the error interface.
func (e *matchRejectErr) Error() string {
	return fmt.Sprintf("trader rejected orders: %v", e.rejectedOrders)
}

// fundingBaseClient is an interface that contains all methods necessary to open
// a channel with a funding shim and query peer connections.
type fundingBaseClient interface {
	// FundingStateStep is an advanced funding related call that allows the
	// caller to either execute some preparatory steps for a funding
	// workflow, or manually progress a funding workflow.
	FundingStateStep(ctx context.Context, req *lnrpc.FundingTransitionMsg,
		opts ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error)

	// OpenChannel attempts to open a singly funded channel specified in the
	// request to a remote peer.
	OpenChannel(ctx context.Context, req *lnrpc.OpenChannelRequest,
		opts ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient,
		error)

	// ListPeers returns a verbose listing of all currently active peers.
	ListPeers(ctx context.Context, req *lnrpc.ListPeersRequest,
		opts ...grpc.CallOption) (*lnrpc.ListPeersResponse, error)

	// SubscribePeerEvents creates a uni-directional stream from the server
	// to the client in which any events relevant to the state of peers are
	// sent over. Events include peers going online and offline.
	SubscribePeerEvents(ctx context.Context, r *lnrpc.PeerEventSubscription,
		opts ...grpc.CallOption) (
		lnrpc.Lightning_SubscribePeerEventsClient, error)
}

type fundingMgr struct {
	db              *clientdb.DB
	walletKit       lndclient.WalletKitClient
	lightningClient lndclient.LightningClient
	baseClient      fundingBaseClient

	// newNodesOnly specifies if the funding manager should only accept
	// matched orders with channels from new nodes that the connected lnd
	// node doesn't already have channels with.
	newNodesOnly bool

	// pendingOpenChannels is a channel through which we'll receive
	// notifications for pending open channels resulting from a successful
	// batch.
	pendingOpenChannels <-chan *lnrpc.ChannelEventUpdate_PendingOpenChannel

	batchStepTimeout time.Duration

	quit chan struct{}
}

// deriveFundingShim generates the proper funding shim that should be used by
// the maker or taker to properly make a channel that stems off the main batch
// funding transaction.
func (f *fundingMgr) deriveFundingShim(ourOrder order.Order,
	matchedOrder *order.MatchedOrder,
	batchTx *wire.MsgTx) (*lnrpc.FundingShim, error) {

	fndgLog.Infof("Registering funding shim for Order(type=%v, amt=%v, "+
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
	fndgLog.Infof("Batch(%x): preparing channel funding for %v orders",
		batch.ID[:], len(batch.MatchedOrders))

	ctxb := context.Background()
	connsInitiated := make(map[route.Vertex]struct{})

	// Before we connect out to peers, we check that we don't get any new
	// channels from peers we already have channels with, in case this is
	// requested by the trader.
	if f.newNodesOnly {
		fundingRejects, err := f.rejectDuplicateChannels(batch)
		if err != nil {
			return err
		}

		// In case we have any orders we don't like, tell the auctioneer
		// now. They'll hopefully prepare and send us another batch.
		if len(fundingRejects) > 0 {
			return &matchRejectErr{
				rejectedOrders: fundingRejects,
			}
		}
	}

	// Now that we know this batch passes our sanity checks, we'll register
	// all the funding shims we need to be able to respond
	for ourOrderNonce, matchedOrders := range batch.MatchedOrders {
		ourOrder, err := f.db.GetOrder(ourOrderNonce)
		if err != nil {
			return err
		}

		orderIsAsk := ourOrder.Type() == order.TypeAsk

		// Depending on if this is a bid or not, we'll either try to
		// connect out and register the full funding shim or do nothing
		// at all.
		for _, matchedOrder := range matchedOrders {
			// To allow bidders to be mobile phones or Tor only
			// nodes (which means, they aren't reachable directly
			// through clearnet), we only force the asker to be
			// reachable. For that to work, the connection has to
			// be established from the bidder to the asker. But the
			// channel itself will be opened from the asker to the
			// bidder which will succeed once the connection is
			// open. But that means, as an asker we don't have
			// anything to do here.
			if orderIsAsk {
				continue
			}

			// At this point, one of our bids was matched with a
			// series of asks, so we'll now register all the
			// expected funding shims so we can execute the next
			// phase w/o any issues and accept the incoming channel
			// from the asker.
			err := f.registerFundingShim(
				ourOrder, matchedOrder, batch.BatchTX,
			)
			if err != nil {
				return fmt.Errorf("unable to register funding "+
					"shim: %v", err)
			}

			// As the bidder, we're the one that needs to make the
			// connection as we're possibly not reachable from the
			// outside. Let's kick off the connection now. However
			// since we might be matching multiple orders from the
			// same remote node, we want to de-duplicate the peers
			// to not run into a problem in lnd when connecting to
			// the same node twice in a very short interval.
			//
			// TODO(roasbeef): info leaks?
			nodeKey := matchedOrder.NodeKey
			fndgLog.Debugf("Connecting to node=%x for order_nonce="+
				"%v", nodeKey[:], matchedOrder.Order.Nonce())
			_, initiated := connsInitiated[nodeKey]
			if !initiated {
				f.connectToMatchedTrader(
					ctxb, nodeKey, matchedOrder.NodeAddrs,
				)
				connsInitiated[nodeKey] = struct{}{}
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

	var (
		eg                errgroup.Group
		chanPoints        = make(map[wire.OutPoint]order.Nonce)
		fundingRejects    = make(map[order.Nonce]*clmrpc.OrderReject)
		fundingRejectsMtx sync.Mutex
	)
	partialReject := func(nonce order.Nonce, reason string,
		chanPoint wire.OutPoint) {

		fundingRejectsMtx.Lock()
		defer fundingRejectsMtx.Unlock()

		fundingRejects[nonce] = &clmrpc.OrderReject{
			ReasonCode: clmrpc.OrderReject_CHANNEL_FUNDING_FAILED,
			Reason:     reason,
		}

		// Also remove the channel from the map that tracks pending
		// channels, we won't receive any update on it if we failed
		// before opening the channel.
		delete(chanPoints, chanPoint)
	}

	// We need to make sure our whole process doesn't take too long overall
	// so we create a context that is valid for the whole funding step and
	// use that everywhere.
	setupCtx, cancel := context.WithTimeout(
		context.Background(), f.batchStepTimeout,
	)
	defer cancel()

	fndgLog.Infof("Batch(%x): opening channels for %v matched orders",
		batch.ID[:], len(batch.MatchedOrders))

	// For each ask order of ours that's matched, we'll make a new funding
	// flow, blocking until they all progress to the final state.
	batchTxHash := batch.BatchTX.TxHash()
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

			// Some goroutines are already running from previous
			// iterations so we need to acquire the lock here.
			fundingRejectsMtx.Lock()
			chanPoints[chanPoint] = matchedOrder.Order.Nonce()
			fundingRejectsMtx.Unlock()

			// If this is a bid order, then we don't need to do
			// anything, as we should've already connected and
			// registered the funding shim during the prior phase.
			// The asker is going to open the channel.
			orderIsBid := ourOrder.Type() == order.TypeBid
			if orderIsBid {
				continue
			}

			// Otherwise, we'll now initiate the funding request to
			// establish all the channels generated by this order
			// with the remote parties. It's the bidder's job to
			// connect to the asker. So if we get here we know they
			// connected and we'll launch off the request to
			// initiate channel funding with the remote peer.
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
				setupCtx, fundingReq,
			)

			// If we can't open the channel, it could be for a
			// number of reasons. One of them is that we still
			// aren't connected to the remote peer because it either
			// refuses connections or is offline. We want to track
			// these errors on the server side so we can track and
			// investigate/intervene. And this is also no reason to
			// fail the whole batch. So we just mark this order pair
			// as rejected.
			nonce := matchedOrder.Order.Nonce()
			nodeKey := matchedOrder.NodeKey
			if err != nil {
				fndgLog.Warnf("Error when trying to open "+
					"channel to node %x, going to reject "+
					"channel: %v", nodeKey[:], err)
				partialReject(nonce, err.Error(), chanPoint)

				continue
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
						fndgLog.Errorf("unable to read "+
							"chan open update "+
							"event from node %x, "+
							"going to reject "+
							"channel: %v",
							nodeKey[:], err)
						partialReject(
							nonce, err.Error(),
							chanPoint,
						)

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

	// There can only be two kinds of errors: Either we're shutting down, in
	// which case we'll just return that error. Or we had a problem opening
	// the channel which should have resulted in an entry in the rejects
	// map. We're not going to fail yet in the second case but wait for the
	// channel openings to proceed first. In case a timeout happens as well
	// we can report that together with the other errors.
	if err := eg.Wait(); err != nil {
		select {
		case <-f.quit:
			return nil, err
		default:
		}
	}

	// Once we've waited for the operations to complete, we'll wait to
	// receive each channel's pending open notification in order to retrieve
	// some keys from their SCB we'll need to submit to the auctioneer in
	// order for them to enforce the channel's service lifetime. We don't
	// need to acquire any lock anymore because all previously started
	// goroutines have now exited.
	channelKeys := make(
		map[wire.OutPoint]*chaninfo.ChannelInfo, len(chanPoints),
	)

	fndgLog.Debugf("Waiting for pending open events for %v channel(s)",
		len(chanPoints))

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

		case <-setupCtx.Done():
			// One or more of our peers timed out. At this point the
			// chanPoints map should only contain channels that
			// failed/timed out. So we can partially reject all of
			// them.
			const timeoutErrStr = "timed out waiting for pending " +
				"open channel notification"
			for chanPoint, nonce := range chanPoints {
				partialReject(nonce, timeoutErrStr, chanPoint)
			}
			return nil, &matchRejectErr{
				rejectedOrders: fundingRejects,
			}

		case <-f.quit:
			return nil, fmt.Errorf("server shutting down")
		}

		// If the notification is for a channel we're not interested in,
		// skip it. This can happen if a channel was opened out-of-band
		// at the same time the batch channels were.
		if _, ok := chanPoints[chanPoint]; !ok {
			continue
		}

		fndgLog.Debugf("Retrieving info for channel %v", chanPoint)

		chanInfo, err := chaninfo.GatherChannelInfo(
			setupCtx, f.lightningClient, f.walletKit, chanPoint,
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

	// If we got to this point with entries in the reject map it means some
	// channels failed directly when opening but the rest succeeded without
	// a timeout. We still want to reject the batch because of the failures.
	if len(fundingRejects) > 0 {
		return nil, &matchRejectErr{
			rejectedOrders: fundingRejects,
		}
	}

	return channelKeys, nil
}

// connectToMatchedTrader attempts to connect to a trader that we've had an
// order matched with. We'll attempt to establish a permanent connection as
// well, so we can use the connection for any batch retries that may happen.
func (f *fundingMgr) connectToMatchedTrader(ctx context.Context,
	nodeKey [33]byte, addrs []net.Addr) {

	for _, addr := range addrs {
		err := f.lightningClient.Connect(ctx, nodeKey, addr.String())
		if err != nil {
			// If we're already connected, then we can stop now.
			if strings.Contains(
				err.Error(), "already connected",
			) {

				return
			}

			fndgLog.Warnf("unable to connect to trader at %x@%v",
				nodeKey[:], addr)

			continue
		}

		// We connected successfully, not need to try any of the
		// other addresses.
		break
	}

	// Since we specified perm, the error is async, and not fully
	// communicated to the caller, so we can't really return an error.
	// Later on, if we can't fund the channel, then we'll fail with a hard
	// error.
}

// rejectDuplicateChannels gathers a list of matched orders that should be
// rejected because they would create channels to peers that the trader already
// has channels with.
func (f *fundingMgr) rejectDuplicateChannels(
	batch *order.Batch) (map[order.Nonce]*clmrpc.OrderReject, error) {

	// We gather all peers from the open and pending channels.
	ctxb := context.Background()
	peers := make(map[route.Vertex]struct{})
	openChans, err := f.lightningClient.ListChannels(ctxb)
	if err != nil {
		return nil, fmt.Errorf("error listing open channels: %v", err)
	}
	pendingChans, err := f.lightningClient.PendingChannels(ctxb)
	if err != nil {
		return nil, fmt.Errorf("error listing pending channels: %v",
			err)
	}

	for _, openChan := range openChans {
		peers[openChan.PubKeyBytes] = struct{}{}
	}
	for _, pendingChan := range pendingChans.PendingOpen {
		peers[pendingChan.PubKeyBytes] = struct{}{}
	}

	// Gather the list of matches we reject because they are to peers we
	// already have channels with.
	const haveChannel = "already have open/pending channel with peer"
	const rejectCode = clmrpc.OrderReject_DUPLICATE_PEER
	fundingRejects := make(map[order.Nonce]*clmrpc.OrderReject)
	for _, matchedOrders := range batch.MatchedOrders {
		for _, matchedOrder := range matchedOrders {
			_, ok := peers[matchedOrder.NodeKey]
			if !ok {
				continue
			}

			fndgLog.Debugf("Rejecting channel to node %x: %v",
				matchedOrder.NodeKey[:], haveChannel)
			otherNonce := matchedOrder.Order.Nonce()
			fundingRejects[otherNonce] = &clmrpc.OrderReject{
				ReasonCode: rejectCode,
				Reason:     haveChannel,
			}
		}
	}

	return fundingRejects, nil
}
