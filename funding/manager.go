package funding

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/chaninfo"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// DefaultBatchStepTimeout is the default time we allow an action that
	// blocks the batch conversation (like peer connection establishment or
	// channel open) to take. If any action takes longer, we might reject
	// the order from that slow peer. This value SHOULD be lower than the
	// defaultMsgTimeout on the server side otherwise nodes might get kicked
	// out of the match making process for timing out even though it was
	// their peer's fault.
	DefaultBatchStepTimeout = 15 * time.Second
)

// MatchRejectErr is an error type that is returned from the funding manager if
// the trader rejects certain orders instead of the whole batch.
type MatchRejectErr struct {
	// RejectedOrders is the map of matches we reject, keyed with our order
	// nonce.
	RejectedOrders map[order.Nonce]*poolrpc.OrderReject
}

// Error returns the underlying error string.
//
// NOTE: This is part of the error interface.
func (e *MatchRejectErr) Error() string {
	return fmt.Sprintf("trader rejected orders: %v", e.RejectedOrders)
}

// BaseClient is an interface that contains all methods necessary to open a
// channel with a funding shim and query peer connections.
type BaseClient interface {
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

	// AbandonChannel removes all channel state from the database except for
	// a close summary. This method can be used to get rid of permanently
	// unusable channels due to bugs fixed in newer versions of lnd. This
	// method can also be used to remove externally funded channels where
	// the funding transaction was never broadcast. Only available for
	// non-externally funded channels in dev build.
	AbandonChannel(ctx context.Context, in *lnrpc.AbandonChannelRequest,
		opts ...grpc.CallOption) (*lnrpc.AbandonChannelResponse, error)
}

type Manager struct {
	// DB is the client database.
	DB *clientdb.DB

	// WalletKit is an lndclient wrapped walletrpc client.
	WalletKit lndclient.WalletKitClient

	// LightningClient is an lndclient wrapped lnrpc client.
	LightningClient lndclient.LightningClient

	// BaseClient is a raw lnrpc client that implements all methods the
	// funding manager needs.
	BaseClient BaseClient

	// NewNodesOnly specifies if the funding manager should only accept
	// matched orders with channels from new nodes that the connected lnd
	// node doesn't already have channels with.
	NewNodesOnly bool

	// PendingOpenChannels is a channel through which we'll receive
	// notifications for pending open channels resulting from a successful
	// batch.
	PendingOpenChannels chan *lnrpc.ChannelEventUpdate_PendingOpenChannel

	// BatchStepTimeout is the timeout the manager uses when executing a
	// single batch step.
	BatchStepTimeout time.Duration
}

// deriveFundingShim generates the proper funding shim that should be used by
// the maker or taker to properly make a channel that stems off the main batch
// funding transaction.
func (m *Manager) deriveFundingShim(ourOrder order.Order,
	matchedOrder *order.MatchedOrder,
	batchTx *wire.MsgTx) (*lnrpc.FundingShim, error) {

	log.Infof("Registering funding shim for Order(type=%v, amt=%v, "+
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

		thawHeight = ourOrder.(*order.Bid).LeaseDuration
	} else {
		bidNonce = matchedOrder.Order.Nonce()
		askNonce = ourOrder.Nonce()

		thawHeight = matchedOrder.Order.(*order.Bid).LeaseDuration
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
	ourMultiSigKey, err := m.WalletKit.DeriveKey(
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
func (m *Manager) registerFundingShim(ourOrder order.Order,
	matchedOrder *order.MatchedOrder, batchTx *wire.MsgTx) error {

	ctxb := context.Background()

	fundingShim, err := m.deriveFundingShim(ourOrder, matchedOrder, batchTx)
	if err != nil {
		return err
	}
	_, err = m.BaseClient.FundingStateStep(
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

// PrepChannelFunding preps the backing node to either receive or initiate a
// channel funding based on the items in the order batch.
func (m *Manager) PrepChannelFunding(batch *order.Batch,
	traderBehindTor bool, quit <-chan struct{}) error {

	log.Infof("Batch(%x): preparing channel funding for %v orders",
		batch.ID[:], len(batch.MatchedOrders))

	// We need to make sure our whole process doesn't take too long overall
	// so we create a context that is valid for the whole funding step and
	// use that everywhere.
	setupCtx, cancel := context.WithTimeout(
		context.Background(), m.BatchStepTimeout,
	)
	defer cancel()

	// Before we connect out to peers, we check that we don't get any new
	// channels from peers we already have channels with, in case this is
	// requested by the trader.
	if m.NewNodesOnly {
		fundingRejects, err := m.rejectDuplicateChannels(batch)
		if err != nil {
			return err
		}

		// In case we have any orders we don't like, tell the auctioneer
		// now. They'll hopefully prepare and send us another batch.
		if len(fundingRejects) > 0 {
			return &MatchRejectErr{
				RejectedOrders: fundingRejects,
			}
		}
	}

	// Now that we know this batch passes our sanity checks, we'll register
	// all the funding shims we need to be able to respond
	connsInitiated := make(map[route.Vertex]struct{})
	for ourOrderNonce, matchedOrders := range batch.MatchedOrders {
		ourOrder, err := m.DB.GetOrder(ourOrderNonce)
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
			//
			// However, if we're currently running with a single
			// Tor address, or all Tor addresses, then we'll also
			// try to connect out to the taker, as they may not be
			// able to connect to hidden services.
			if orderIsAsk && !traderBehindTor {
				continue
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
			log.Debugf("Connecting to node=%x for order_nonce="+
				"%v", nodeKey[:], matchedOrder.Order.Nonce())
			_, initiated := connsInitiated[nodeKey]
			if !initiated {
				// Since we don't want to block on the
				// connection attempt to this trader, we start
				// it in a new go routine, and ignore the error
				// for now. Instead we will observe later
				// whether the peer gets connected before the
				// batch timeout.
				go m.connectToMatchedTrader(
					setupCtx, nodeKey,
					matchedOrder.NodeAddrs,
				)
				connsInitiated[nodeKey] = struct{}{}
			}

			// Only bid orders need to register for the shim.
			if orderIsAsk {
				continue
			}

			// At this point, one of our bids was matched with a
			// series of asks, so we'll now register all the
			// expected funding shims so we can execute the next
			// phase w/o any issues and accept the incoming channel
			// from the asker.
			err := m.registerFundingShim(
				ourOrder, matchedOrder, batch.BatchTX,
			)
			if err != nil {
				return fmt.Errorf("unable to register funding "+
					"shim: %v", err)
			}

		}
	}

	// We need to wait for all connections to be established now. Otherwise
	// the asker won't be able to open the channel as it doesn't know the
	// connection details of the bidder.
	return m.waitForPeerConnections(setupCtx, connsInitiated, batch, quit)
}

// BatchChannelSetup will attempt to establish new funding flows with all
// matched takers (people buying our channels) in the passed batch. This method
// will block until the channel is considered pending. Once this phase is
// complete, and the batch execution transaction broadcast, the channel will be
// finalized and locked in.
func (m *Manager) BatchChannelSetup(batch *order.Batch,
	quit <-chan struct{}) (map[wire.OutPoint]*chaninfo.ChannelInfo, error) {

	var (
		eg                errgroup.Group
		chanPoints        = make(map[wire.OutPoint]order.Nonce)
		fundingRejects    = make(map[order.Nonce]*poolrpc.OrderReject)
		fundingRejectsMtx sync.Mutex
	)
	partialReject := func(nonce order.Nonce, reason string,
		chanPoint wire.OutPoint) {

		fundingRejectsMtx.Lock()
		defer fundingRejectsMtx.Unlock()

		fundingRejects[nonce] = &poolrpc.OrderReject{
			ReasonCode: poolrpc.OrderReject_CHANNEL_FUNDING_FAILED,
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
		context.Background(), m.BatchStepTimeout,
	)
	defer cancel()

	log.Infof("Batch(%x): opening channels for %v matched orders",
		batch.ID[:], len(batch.MatchedOrders))

	// For each ask order of ours that's matched, we'll make a new funding
	// flow, blocking until they all progress to the final state.
	batchTxHash := batch.BatchTX.TxHash()
	for ourOrderNonce, matchedOrders := range batch.MatchedOrders {
		ourOrder, err := m.DB.GetOrder(ourOrderNonce)
		if err != nil {
			return nil, err
		}

		// We'll obtain the expected channel point for each matched
		// order, and complete the funding flow for each one in which
		// our order was the ask.
		for _, matchedOrder := range matchedOrders {
			fundingShim, err := m.deriveFundingShim(
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
			chanStream, err := m.BaseClient.OpenChannel(
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
				log.Warnf("Error when trying to open "+
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

					case <-quit:
						return fmt.Errorf("server " +
							"shutting down")
					default:
					}

					msg, err := chanStream.Recv()
					if err != nil {
						log.Errorf("unable to read "+
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
		case <-quit:
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

	log.Debugf("Waiting for pending open events for %v channel(s)",
		len(chanPoints))

	for {
		var chanPoint wire.OutPoint
		select {
		case channel := <-m.PendingOpenChannels:
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
			return nil, &MatchRejectErr{
				RejectedOrders: fundingRejects,
			}

		case <-quit:
			return nil, fmt.Errorf("server shutting down")
		}

		// If the notification is for a channel we're not interested in,
		// skip it. This can happen if a channel was opened out-of-band
		// at the same time the batch channels were.
		if _, ok := chanPoints[chanPoint]; !ok {
			continue
		}

		log.Debugf("Retrieving info for channel %v", chanPoint)

		chanInfo, err := chaninfo.GatherChannelInfo(
			setupCtx, m.LightningClient, m.WalletKit, chanPoint,
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
		return nil, &MatchRejectErr{
			RejectedOrders: fundingRejects,
		}
	}

	return channelKeys, nil
}

// DeletePendingBatch removes all references to the current pending batch
// without applying its staged updates to accounts and orders. If no pending
// batch exists, this acts as a no-op.
//
// NOTE: This is part of the auctioneer.BatchCleaner interface.
func (m *Manager) DeletePendingBatch() error {
	return m.DB.DeletePendingBatch()
}

// RemovePendingBatchArtifacts removes any funding shims or pending channels
// from a batch that was never finalized. Some non-terminal errors are logged
// only and not returned. Therefore if this method returns an error, it should
// be handled as terminal error.
//
// NOTE: This is part of the auctioneer.BatchCleaner interface.
func (m *Manager) RemovePendingBatchArtifacts(
	matchedOrders map[order.Nonce][]*order.MatchedOrder,
	batchTx *wire.MsgTx) error {

	err := CancelPendingFundingShims(
		matchedOrders, m.BaseClient, m.DB.GetOrder,
	)
	if err != nil {
		// CancelPendingFundingShims only returns hard errors that
		// justify us rejecting the batch or logging an actual error.
		return fmt.Errorf("error canceling funding shims "+
			"from previous pending batch: %v", err)
	}

	// Also abandon any channels that might still be pending
	// from a previous round of the same batch or a previous
	// batch that we didn't make it into the final round.
	err = AbandonCanceledChannels(
		matchedOrders, batchTx, m.WalletKit, m.BaseClient,
		m.DB.GetOrder,
	)
	if err != nil {
		// AbandonCanceledChannels also only returns hard errors that
		// justify us rejecting the batch or logging an actual error.
		return fmt.Errorf("error abandoning channels from "+
			"previous pending batch: %v", err)
	}

	return nil
}

// connectToMatchedTrader attempts to connect to a trader that we've had an
// order matched with, on all available addresses.
func (m *Manager) connectToMatchedTrader(ctx context.Context,
	nodeKey [33]byte, addrs []net.Addr) {

	for _, addr := range addrs {
		err := m.LightningClient.Connect(ctx, nodeKey, addr.String())
		if err != nil {
			// If we're already connected, then we can stop now.
			if strings.Contains(err.Error(), "already connected") {
				return
			}

			log.Warnf("unable to connect to trader at %x@%v",
				nodeKey[:], addr)

			continue
		}

		// We connected successfully, no need to try any of the
		// other addresses.
		break
	}
}

// waitForPeerConnections makes sure the connected lnd has a persistent
// connection to each of the peers in the given map. This method blocks until
// either all connections are established successfully or the passed context is
// canceled.
//
// NOTE: The passed context MUST have a timeout applied to it, otherwise this
// method will block forever in case a connection doesn't succeed.
func (m *Manager) waitForPeerConnections(ctx context.Context,
	peers map[route.Vertex]struct{}, batch *order.Batch,
	quit <-chan struct{}) error {

	// First of all, subscribe to new peer events so we certainly don't miss
	// an update while we look for the already connected peers.
	ctxc, cancel := context.WithCancel(ctx)
	defer cancel()
	subscription, err := m.BaseClient.SubscribePeerEvents(
		ctxc, &lnrpc.PeerEventSubscription{},
	)
	if err != nil {
		return fmt.Errorf("unable to subscribe to peer events: %v", err)
	}

	// Query all connected peers. This only returns active peers so once a
	// node key appears in this list, we can be reasonably sure the
	// connection is established (flapping peers notwithstanding).
	resp, err := m.BaseClient.ListPeers(
		ctx, &lnrpc.ListPeersRequest{},
	)
	if err != nil {
		return fmt.Errorf("unable to query peers: %v", err)
	}

	// Remove all connected peers from our previously de-duplicated map.
	for _, peer := range resp.Peers {
		peerKeyBytes, err := hex.DecodeString(peer.PubKey)
		if err != nil {
			return fmt.Errorf("error parsing node key: %v", err)
		}

		var peerKey [33]byte
		copy(peerKey[:], peerKeyBytes)
		delete(peers, peerKey)
	}

	// Some connection attempts are still ongoing, let's now wait for peer
	// events or the cancellation of the context.
	for {
		// Great, we're done now, all connections established!
		if len(peers) == 0 {
			return nil
		}

		select {
		// We can't wait forever, otherwise the auctioneer will kick us
		// out of the batch for not responding. In case we weren't able
		// to open all required connections, we reject the orders that
		// came from the peers with the failed connections.
		case <-ctx.Done():
			return &MatchRejectErr{
				RejectedOrders: m.rejectFailedConnections(
					peers, batch,
				),
			}

		// We're shutting down, nothing more to do here.
		case <-quit:
			return nil

		default:
		}

		// Block on reading the next peer event now.
		msg, err := subscription.Recv()
		if err != nil {
			// We must inspect the gprc status to uncover the
			// actual error.
			st, ok := status.FromError(err)
			if !ok {
				return fmt.Errorf("error reading peer event: "+
					"%v", err)
			}

			// We've run into the timeout. Let the code above return
			// the error, we should now run into the ctx.Done()
			// case.
			switch st.Code() {
			case codes.Canceled, codes.DeadlineExceeded:
				continue

			default:
				return fmt.Errorf("error reading peer events, "+
					"unhandled status: %v", err)
			}
		}

		// We're only interested in online events, skip everything else.
		if msg.Type != lnrpc.PeerEvent_PEER_ONLINE {
			continue
		}

		// Great, a new peer is online, let's remove it from the wait
		// list now.
		peerKeyBytes, err := hex.DecodeString(msg.PubKey)
		if err != nil {
			return fmt.Errorf("error parsing node key: %v", err)
		}

		var peerKey [33]byte
		copy(peerKey[:], peerKeyBytes)
		delete(peers, peerKey)
	}
}

// rejectDuplicateChannels gathers a list of matched orders that should be
// rejected because they would create channels to peers that the trader already
// has channels with.
func (m *Manager) rejectDuplicateChannels(
	batch *order.Batch) (map[order.Nonce]*poolrpc.OrderReject, error) {

	// We gather all peers from the open and pending channels.
	ctxb := context.Background()
	peers := make(map[route.Vertex]struct{})
	openChans, err := m.LightningClient.ListChannels(ctxb)
	if err != nil {
		return nil, fmt.Errorf("error listing open channels: %v", err)
	}
	pendingChans, err := m.LightningClient.PendingChannels(ctxb)
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
	const rejectCode = poolrpc.OrderReject_DUPLICATE_PEER
	fundingRejects := make(map[order.Nonce]*poolrpc.OrderReject)
	for _, matchedOrders := range batch.MatchedOrders {
		for _, matchedOrder := range matchedOrders {
			_, ok := peers[matchedOrder.NodeKey]
			if !ok {
				continue
			}

			log.Debugf("Rejecting channel to node %x: %v",
				matchedOrder.NodeKey[:], haveChannel)
			otherNonce := matchedOrder.Order.Nonce()
			fundingRejects[otherNonce] = &poolrpc.OrderReject{
				ReasonCode: rejectCode,
				Reason:     haveChannel,
			}
		}
	}

	return fundingRejects, nil
}

// rejectFailedConnections gathers a list of all matched orders that are to
// peers to which we couldn't connect and want to reject because of that.
func (m *Manager) rejectFailedConnections(peers map[route.Vertex]struct{},
	batch *order.Batch) map[order.Nonce]*poolrpc.OrderReject {

	// Gather the list of matches we reject because the connection to them
	// failed.
	const connFailed = "connection not established before timeout"
	const rejectCode = poolrpc.OrderReject_CHANNEL_FUNDING_FAILED
	fundingRejects := make(map[order.Nonce]*poolrpc.OrderReject)
	for _, matchedOrders := range batch.MatchedOrders {
		for _, matchedOrder := range matchedOrders {
			_, ok := peers[matchedOrder.NodeKey]
			if !ok {
				continue
			}

			log.Debugf("Rejecting channel to node %x: %v",
				matchedOrder.NodeKey[:], connFailed)
			otherNonce := matchedOrder.Order.Nonce()
			fundingRejects[otherNonce] = &poolrpc.OrderReject{
				ReasonCode: rejectCode,
				Reason:     connFailed,
			}
		}
	}

	return fundingRejects
}
