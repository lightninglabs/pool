package pool

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/subscribe"
)

// SidecarAcceptor is a type that is exclusively responsible for managing the
// recipient's tasks of executing a sidecar channel. The two tasks are:
// 1. Verify a sidecar ticket and the offer contained within then add the
//    recipient node information to the ticket so it can be returned to the
//    sidecar provider. This is step 2/4 of the entire sidecar execution
//    protocol.
// 2. Interact with the auction server and connect out to an asker's node in the
//    right moment then accept the incoming channel. This is step 4/4 of the
//    entire sidecar execution protocol.
// The code for these two tasks are kept separate from the default funding
// manager to make it easier to extract a standalone sidecar acceptor client
// later on. It also makes it easier to see what code would need to be re-
// implemented in another language to integrate just the acceptor part.
type SidecarAcceptor struct {
	store      sidecar.Store
	signer     lndclient.SignerClient
	wallet     lndclient.WalletKitClient
	baseClient funding.BaseClient
	nodePubKey *btcec.PublicKey
	acceptor   *ChannelAcceptor

	clientCfg             *auctioneer.Config
	client                *auctioneer.Client
	fundingManager        *funding.Manager
	pendingOpenChanClient *subscribe.Client

	pendingSidecarOrders    map[order.Nonce]*sidecar.Ticket
	pendingSidecarOrdersMtx sync.Mutex
	pendingBatch            *order.Batch

	sync.Mutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// NewSidecarAcceptor creates a new sidecar acceptor.
func NewSidecarAcceptor(store sidecar.Store, signer lndclient.SignerClient,
	wallet lndclient.WalletKitClient, baseClient funding.BaseClient,
	acceptor *ChannelAcceptor, nodePubKey *btcec.PublicKey,
	clientCfg auctioneer.Config,
	fundingManager *funding.Manager) *SidecarAcceptor {

	clientCfg.ConnectSidecar = true

	return &SidecarAcceptor{
		store:                store,
		signer:               signer,
		wallet:               wallet,
		baseClient:           baseClient,
		nodePubKey:           nodePubKey,
		acceptor:             acceptor,
		clientCfg:            &clientCfg,
		fundingManager:       fundingManager,
		pendingSidecarOrders: make(map[order.Nonce]*sidecar.Ticket),
		quit:                 make(chan struct{}),
	}
}

// Start starts the sidecar acceptor.
func (a *SidecarAcceptor) Start(errChan chan error) error {
	var err error
	a.client, err = auctioneer.NewClient(a.clientCfg)
	if err != nil {
		return fmt.Errorf("error creating auctioneer client: %v", err)
	}
	if err := a.client.Start(); err != nil {
		return fmt.Errorf("error starting auctioneer client: %v", err)
	}
	if err := a.acceptor.Start(errChan); err != nil {
		return fmt.Errorf("error starting channel acceptor: %v", err)
	}

	// We want to make sure we don't miss any channel updates as long as we
	// are running.
	a.pendingOpenChanClient, err = a.fundingManager.SubscribePendingOpenChan()
	if err != nil {
		return fmt.Errorf("error subscribing to pending open channel "+
			"events: %v", err)
	}

	a.wg.Add(1)
	go a.subscribe()

	return nil
}

// subscribe subscribes to auction messages coming in from the server. Since we
// are only on the receiving end of a sidecar order if we receive a message here
// we only have to do three things during the match making process: Connect out
// to the maker and register the funding shim in the prepare step and wait for
// the incoming channel in the sign step. The rest is just cleanup of pending
// states.
func (a *SidecarAcceptor) subscribe() {
	defer a.wg.Done()

	for {
		select {
		case serverMsg, ok := <-a.client.FromServerChan:
			// The client is shutting down.
			if !ok {
				return
			}

			if err := a.handleServerMessage(serverMsg); err != nil {
				sdcrLog.Errorf("Error while handling server "+
					"message: %v", err)
			}

		case <-a.quit:
			return
		}
	}
}

// Stop stops the sidecar acceptor.
func (a *SidecarAcceptor) Stop() error {
	var returnErr error
	if err := a.client.Stop(); err != nil {
		sdcrLog.Errorf("Error stopping auctioneer client: %v", err)
		returnErr = err
	}

	a.pendingOpenChanClient.Cancel()
	a.acceptor.Stop()
	close(a.quit)

	a.wg.Wait()

	return returnErr
}

// RegisterSidecar derives a new multisig key for a potential future channel
// bought over a sidecar order and adds that to the offered ticket. If
// successful, the updated ticket is added to the local database.
func (a *SidecarAcceptor) RegisterSidecar(ctx context.Context,
	ticket *sidecar.Ticket) error {

	// The ticket needs to be in the correct state for us to register it.
	if err := sidecar.VerifyOffer(ctx, ticket, a.signer); err != nil {
		return fmt.Errorf("error verifying sidecar offer: %v", err)
	}

	// Do we already have a ticket with that ID?
	_, err := a.store.Sidecar(ticket.ID, ticket.Offer.SignPubKey)
	if err != clientdb.ErrNoSidecar {
		return fmt.Errorf("ticket with ID %x already exists",
			ticket.ID[:])
	}

	// First we'll need a new multisig key for the channel that will be
	// opened through this sidecar order.
	keyDesc, err := a.wallet.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyMultiSig),
	)
	if err != nil {
		return fmt.Errorf("error deriving multisig key: %v", err)
	}

	ticket.State = sidecar.StateRegistered
	ticket.Recipient = &sidecar.Recipient{
		NodePubKey:       a.nodePubKey,
		MultiSigPubKey:   keyDesc.PubKey,
		MultiSigKeyIndex: keyDesc.Index,
	}
	if err := a.store.AddSidecar(ticket); err != nil {
		return fmt.Errorf("error storing sidecar: %v", err)
	}

	return nil
}

// ExpectChannel informs the acceptor that a new bid order was submitted for the
// given sidecar ticket. We subscribe to auction events using the multisig key
// we gave out when we registered the ticket.
func (a *SidecarAcceptor) ExpectChannel(ctx context.Context,
	t *sidecar.Ticket) error {

	if t.Order == nil {
		return fmt.Errorf("order in sidecar ticket is missing")
	}

	// Multiple channels should be registered serially, we'll hold the mutex
	// for the whole duration.
	a.pendingSidecarOrdersMtx.Lock()
	defer a.pendingSidecarOrdersMtx.Unlock()

	nonce := t.Order.BidNonce
	_, ok := a.pendingSidecarOrders[nonce]
	if ok {
		return fmt.Errorf("sidecar with order nonce %x is already "+
			"registered", nonce[:])
	}

	// We didn't know about this ticket for this nonce before so let's now
	// update its state in the database and start expecting a channel for it
	// now.
	t.State = sidecar.StateExpectingChannel
	if err := a.store.UpdateSidecar(t); err != nil {
		return fmt.Errorf("error updating sidecar: %v", err)
	}

	a.pendingSidecarOrders[nonce] = t

	// Authenticate our fake account with the server now to receive updates
	// about possible matches. This method will return as soon as the
	// authentication itself is completed, after which we can read the
	// server messages on a.client.FromServerChan.
	return a.client.StartAccountSubscription(ctx, &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamilyMultiSig,
			Index:  t.Recipient.MultiSigKeyIndex,
		},
		PubKey: t.Recipient.MultiSigPubKey,
	})
}

// handleServerMessage reacts to a message sent by the server and sends back the
// appropriate response message (if needed). The main lock will be held during
// the full execution of this method.
func (a *SidecarAcceptor) handleServerMessage(
	serverMsg *auctioneerrpc.ServerAuctionMessage) error {

	// We hold the lock during the whole process of reacting to a server
	// message to make sure no user RPC calls interfere with the execution.
	a.Lock()
	defer a.Unlock()

	switch msg := serverMsg.Msg.(type) {
	case *auctioneerrpc.ServerAuctionMessage_Prepare:
		batchID := msg.Prepare.BatchId

		sdcrLog.Tracef("Received prepare msg from server, "+
			"batch_id=%x: %v", batchID, spew.Sdump(msg))

		newBatch, err := a.matchPrepare(a.pendingBatch, msg.Prepare)
		if err != nil {
			sdcrLog.Errorf("Error handling prepare message: %v",
				err)
			return a.sendRejectBatch(batchID, err)
		}

		// We know we're involved in a batch, so let's store it for the
		// next step.
		a.pendingBatch = newBatch

	case *auctioneerrpc.ServerAuctionMessage_Sign:
		batchID := msg.Sign.BatchId

		sdcrLog.Tracef("Received sign msg from server, batch_id=%x: %v",
			batchID, spew.Sdump(msg))

		// Assert we're in the correct state to receive a sign message.
		if a.pendingBatch == nil ||
			!bytes.Equal(batchID, a.pendingBatch.ID[:]) {

			err := fmt.Errorf("error processing batch sign "+
				"message, unknown batch with ID %x", batchID)
			sdcrLog.Errorf("Error handling sign message: %v", err)
			return a.sendRejectBatch(batchID, err)
		}

		if err := a.matchSign(a.pendingBatch); err != nil {
			sdcrLog.Errorf("Error handling sign message: %v", err)
			return a.sendRejectBatch(batchID, err)
		}

	case *auctioneerrpc.ServerAuctionMessage_Finalize:
		batchID := msg.Finalize.BatchId

		sdcrLog.Tracef("Received finalize msg from server, "+
			"batch_id=%x: %v", batchID, spew.Sdump(msg))

		// All we need to do now is some cleanup. Even if the cleanup
		// fails, we want to clear the pending batch as we won't receive
		// any more messages for it.
		batch := a.pendingBatch
		a.pendingBatch = nil

		a.matchFinalize(batch)

	default:
		sdcrLog.Debugf("Received msg %v from auctioneer on sidecar "+
			"client: %v", msg)
	}

	return nil
}

// matchPrepare handles an incoming OrderMatchPrepare message from the server.
// Since we're only on the receiving end of a sidecar channel (which is always
// a bid order) the tasks are simplified compared to normal bid order execution.
//
// NOTE: The lock must be held when calling this method.
func (a *SidecarAcceptor) matchPrepare(pendingBatch *order.Batch,
	msg *auctioneerrpc.OrderMatchPrepare) (*order.Batch, error) {

	// Parse and formally validate what we got from the server.
	batch, err := order.ParseRPCBatch(msg)
	if err != nil {
		return nil, fmt.Errorf("unable to parse batch: %v", err)
	}

	sdcrLog.Infof("Received PrepareMsg for batch=%x, num_orders=%v",
		batch.ID[:], len(batch.MatchedOrders))

	// If there is still a pending batch around from a previous iteration,
	// we need to clean up the pending channels first.
	if pendingBatch != nil {
		if err := a.removeShims(pendingBatch); err != nil {
			return nil, fmt.Errorf("unable to cleanup previous "+
				"batch: %v", err)
		}
	}

	// Before we accept the batch, we'll finish preparations on our end
	// which include applying any order match predicates, connecting out to
	// peers, and registering funding shim. We don't do a full batch
	// validation since we don't have any information about the account
	// that's being used to pay for the sidecar channel.
	err = a.fundingManager.PrepChannelFunding(batch, a.getSidecarAsOrder)
	if err != nil {
		return nil, fmt.Errorf("error preparing channel funding: %v",
			err)
	}

	// Accept the match now.
	sdcrLog.Infof("Accepting batch=%x", batch.ID[:])

	// Send the message to the server.
	err = a.client.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
		Msg: &auctioneerrpc.ClientAuctionMessage_Accept{
			Accept: &auctioneerrpc.OrderMatchAccept{
				BatchId: batch.ID[:],
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("error sending accept msg: %v", err)
	}

	return batch, nil
}

// matchSign handles an incoming OrderMatchSignBegin message from the server.
// Since we're only on the receiving end of a sidecar channel (which is always
// a bid order) the tasks are simplified compared to normal bid order execution.
//
// NOTE: The lock must be held when calling this method.
func (a *SidecarAcceptor) matchSign(batch *order.Batch) error {
	channelInfos, err := a.fundingManager.SidecarBatchChannelSetup(
		a.pendingBatch, a.pendingOpenChanClient, a.getSidecarAsOrder,
	)
	if err != nil {
		return fmt.Errorf("error setting up channels: %v", err)
	}

	rpcChannelInfos, err := marshallChannelInfo(channelInfos)
	if err != nil {
		return fmt.Errorf("error setting up channels: %v", err)
	}

	sdcrLog.Infof("Received OrderMatchSignBegin for batch=%x, "+
		"num_orders=%v", batch.ID[:], len(batch.MatchedOrders))

	sdcrLog.Infof("Sending OrderMatchSign for batch %x", batch.ID[:])
	return a.client.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
		Msg: &auctioneerrpc.ClientAuctionMessage_Sign{
			Sign: &auctioneerrpc.OrderMatchSign{
				BatchId:      batch.ID[:],
				ChannelInfos: rpcChannelInfos,
			},
		},
	})
}

// matchFinalize handles an incoming OrderMatchFinalize message from the server.
// Since we're only on the receiving end of a sidecar channel (which is always
// a bid order) the tasks are simplified compared to normal bid order execution.
//
// NOTE: The lock must be held when calling this method.
func (a *SidecarAcceptor) matchFinalize(batch *order.Batch) {
	sdcrLog.Infof("Received FinalizeMsg for batch=%x", batch.ID[:])

	// Remove pending shim and update sidecar ticket.
	for ourOrder := range batch.MatchedOrders {
		dummyBid, err := a.getSidecarAsOrder(ourOrder)
		if err != nil {
			// Skip over matched orders that aren't sidecar ones.
			continue
		}

		// Make sure we don't expect this sidecar channel again.
		a.pendingSidecarOrdersMtx.Lock()
		ticket := a.pendingSidecarOrders[dummyBid.Nonce()]
		ticket.State = sidecar.StateCompleted
		if err := a.store.UpdateSidecar(ticket); err != nil {
			sdcrLog.Errorf("Error updating sidecar ticket to "+
				"state complete: %v", err)
		}

		delete(a.pendingSidecarOrders, ourOrder)
		a.pendingSidecarOrdersMtx.Unlock()

		a.acceptor.ShimRemoved(dummyBid.(*order.Bid))
	}
}

// getSidecarAsOrder tries to find a sidecar ticket for the order with the given
// nonce and returns a dummy order that contains all the necessary information
// needed for channel receiving.
func (a *SidecarAcceptor) getSidecarAsOrder(o order.Nonce) (order.Order, error) {
	a.pendingSidecarOrdersMtx.Lock()
	defer a.pendingSidecarOrdersMtx.Unlock()

	for _, ticket := range a.pendingSidecarOrders {
		if ticket.Order.BidNonce == o {
			kit := order.NewKit(ticket.Order.BidNonce)
			kit.LeaseDuration = ticket.Offer.LeaseDurationBlocks
			return &order.Bid{
				Kit:             *kit,
				SidecarTicket:   ticket,
				SelfChanBalance: ticket.Offer.PushAmt,
			}, nil
		}
	}

	return nil, clientdb.ErrNoOrder
}

// sendRejectBatch sends a reject message to the server with the properly
// decoded reason code and the full reason message as a string.
func (a *SidecarAcceptor) sendRejectBatch(batchID []byte, failure error) error {
	msg := &auctioneerrpc.ClientAuctionMessage_Reject{
		Reject: &auctioneerrpc.OrderMatchReject{
			BatchId:    batchID,
			Reason:     failure.Error(),
			ReasonCode: auctioneerrpc.OrderMatchReject_BATCH_VERSION_MISMATCH,
		},
	}
	return a.client.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
		Msg: msg,
	})
}

// removeShims removes any previously created channel shims for the given batch
// from lnd and the channel acceptor.
func (a *SidecarAcceptor) removeShims(batch *order.Batch) error {
	// As we're rejecting this batch, we'll now cancel all funding shims
	// that we may have registered since we may be matched with a distinct
	// set of channels if this batch is repeated.
	if err := funding.CancelPendingFundingShims(
		batch.MatchedOrders, a.baseClient, a.getSidecarAsOrder,
	); err != nil {
		return err
	}

	for ourOrder := range batch.MatchedOrders {
		dummyBid, err := a.getSidecarAsOrder(ourOrder)
		if err != nil {
			continue
		}

		a.acceptor.ShimRemoved(dummyBid.(*order.Bid))
	}

	return nil
}
