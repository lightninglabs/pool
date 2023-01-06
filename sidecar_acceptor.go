package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/subscribe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SidecarAcceptor is a type that is exclusively responsible for managing the
// recipient's tasks of executing a sidecar channel. The two tasks are:
//  1. Verify a sidecar ticket and the offer contained within then add the
//     recipient node information to the ticket so it can be returned to the
//     sidecar provider. This is step 2/4 of the entire sidecar execution
//     protocol.
//  2. Interact with the auction server and connect out to an asker's node in the
//     right moment then accept the incoming channel. This is step 4/4 of the
//     entire sidecar execution protocol.
//
// The code for these two tasks are kept separate from the default funding
// manager to make it easier to extract a standalone sidecar acceptor client
// later on. It also makes it easier to see what code would need to be re-
// implemented in another language to integrate just the acceptor part.
type SidecarAcceptor struct {
	cfg *SidecarAcceptorConfig

	client                *auctioneer.Client
	pendingOpenChanClient *subscribe.Client

	pendingSidecarOrders    map[order.Nonce]*sidecar.Ticket
	pendingSidecarOrdersMtx sync.Mutex
	pendingBatch            *order.Batch

	sync.Mutex

	// negotiators maps a potential stream ID (of the recipient) to the
	// active sidecar negotiator. We'll maintain this map to be able to
	// shutdown the negotiators, as well as notify them that the ticket has
	// been fully executed.
	negotiators map[[64]byte]*SidecarNegotiator

	quit chan struct{}
	wg   sync.WaitGroup
}

// SidecarAcceptorConfig holds all the configuration information that sidecar
// acceptor needs in order to carry out its duties.
type SidecarAcceptorConfig struct {
	SidecarDB sidecar.Store

	AcctDB account.Store

	Signer lndclient.SignerClient

	Wallet lndclient.WalletKitClient

	BaseClient funding.BaseClient

	Acceptor *ChannelAcceptor

	NodePubKey *btcec.PublicKey

	ClientCfg auctioneer.Config

	PrepareOrder orderPreparer

	FundingManager *funding.Manager

	FetchSidecarBid func(*sidecar.Ticket) (*order.Bid, error)
}

// NewSidecarAcceptor creates a new sidecar acceptor.
func NewSidecarAcceptor(cfg *SidecarAcceptorConfig) *SidecarAcceptor {
	cfg.ClientCfg.ConnectSidecar = true

	return &SidecarAcceptor{
		cfg:                  cfg,
		pendingSidecarOrders: make(map[order.Nonce]*sidecar.Ticket),
		quit:                 make(chan struct{}),
		negotiators:          make(map[[64]byte]*SidecarNegotiator),
	}
}

// Start starts the sidecar acceptor.
func (a *SidecarAcceptor) Start(errChan chan error) error {
	var err error
	a.client, err = auctioneer.NewClient(&a.cfg.ClientCfg)
	if err != nil {
		return fmt.Errorf("error creating auctioneer client: %v", err)
	}
	if err := a.client.Start(); err != nil {
		return fmt.Errorf("error starting auctioneer client: %v", err)
	}
	if err := a.cfg.Acceptor.Start(errChan); err != nil {
		return fmt.Errorf("error starting channel acceptor: %v", err)
	}

	// We want to make sure we don't miss any channel updates as long as we
	// are running.
	a.pendingOpenChanClient, err = a.cfg.FundingManager.SubscribePendingOpenChan()
	if err != nil {
		return fmt.Errorf("error subscribing to pending open channel "+
			"events: %v", err)
	}

	// If we weren't able to complete all expected sidecar channels, we want
	// to resume them now.
	tickets, err := a.cfg.SidecarDB.Sidecars()
	if err != nil {
		return fmt.Errorf("error reading sidecar tickets: %v", err)
	}
	for _, ticket := range tickets {
		switch {
		// If this ticket was intended to be negotiated in an automated
		// manner, then we'll launch a goroutine to manage the
		// remaining state transitions depending on if we're the
		// provider or responder.
		case ticket.Offer.Auto && !ticket.State.IsTerminal():
			// In order to determine our role, we'll first need to see
			// if the account for the offer exists in our database. If
			// not, then we're the recipient.
			acct, err := a.cfg.AcctDB.Account(ticket.Offer.SignPubKey)
			switch {
			// If we can't find the account, then we assume that
			// we're the recipient, so we'll attempt to accept the
			// sidecar ticket.
			case err == clientdb.ErrAccountNotFound:

				autoAcceptor := NewSidecarNegotiator(AutoAcceptorConfig{
					Provider: false,
					StartingPkt: &SidecarPacket{
						CurrentState:   ticket.State,
						ReceiverTicket: ticket,
						ProviderTicket: ticket,
					},
					Driver:  a,
					MailBox: a,
				})
				if err := autoAcceptor.Start(); err != nil {
					return err
				}

				streamID, err := deriveRecipientStreamID(ticket)
				if err != nil {
					return fmt.Errorf("unable to derive "+
						"stream IDs: %v", err)
				}

				a.Lock()
				a.negotiators[streamID] = autoAcceptor
				a.Unlock()

			// Otherwise, we're on the other end of things, so
			// we'll assume the role of the provider.
			case err == nil:
				// As we're the provider of this ticket, we'll
				// need to fetch the bid that goes along with
				// it so we can submit it to the auctioneer
				// once we've gathered all the necessary
				// materials.
				ticketBid, err := a.cfg.FetchSidecarBid(ticket)
				if err != nil {
					return fmt.Errorf("unable to fetch "+
						"sidecar bid: %w", err)
				}

				// If we're resuming the ticket, and it's still
				// in the offered state, then we'll reset our
				// state so wer send a message to the other
				// party to have them re-send their registered
				// ticket.
				state := ticket.State
				if state == sidecar.StateOffered {
					state = sidecar.StateCreated
				}

				autoAcceptor := NewSidecarNegotiator(AutoAcceptorConfig{
					Provider:    true,
					ProviderBid: ticketBid,
					StartingPkt: &SidecarPacket{
						CurrentState:   state,
						ReceiverTicket: ticket,
						ProviderTicket: ticket,
					},
					ProviderAccount: acct,
					Driver:          a,
					MailBox:         a,
				})
				if err := autoAcceptor.Start(); err != nil {
					return err
				}

				streamID, err := deriveRecipientStreamID(ticket)
				if err != nil {
					return fmt.Errorf("unable to derive "+
						"stream IDs: %v", err)
				}

				a.Lock()
				a.negotiators[streamID] = autoAcceptor
				a.Unlock()

			default:
				return fmt.Errorf("unable to fetch account "+
					"for sidecar: %w", err)
			}

			continue

		// If the ticket has no recipient or isn't in the expecting
		// state, then we can safely skip it.
		case ticket.State != sidecar.StateExpectingChannel:
			continue
		case ticket.Recipient == nil:
			continue
		}

		r := ticket.Recipient
		if !r.NodePubKey.IsEqual(a.cfg.NodePubKey) {
			continue
		}

		// This is a ticket for our node that is still being expected,
		// add it to our map of expected channels.
		ctxb := context.Background()
		if err := a.ExpectChannel(ctxb, ticket); err != nil {
			return fmt.Errorf("error subscribing to batch "+
				"updates for sidecar ticket: %v", err)
		}
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

	for _, negotiator := range a.negotiators {
		negotiator.Stop()
	}

	a.pendingOpenChanClient.Cancel()
	a.cfg.Acceptor.Stop()
	close(a.quit)

	a.wg.Wait()

	return returnErr
}

// RegisterSidecar derives a new multisig key for a potential future channel
// bought over a sidecar order and adds that to the offered ticket. If
// successful, the updated ticket is added to the local database.
func (a *SidecarAcceptor) RegisterSidecar(ctx context.Context,
	ticket sidecar.Ticket) (*sidecar.Ticket, error) {

	// The ticket needs to be in the correct state for us to register it.
	if err := sidecar.VerifyOffer(ctx, &ticket, a.cfg.Signer); err != nil {
		return nil, fmt.Errorf("error verifying sidecar offer: %v", err)
	}

	// Do we already have a ticket with that ID?
	_, err := a.cfg.SidecarDB.Sidecar(ticket.ID, ticket.Offer.SignPubKey)
	if err != clientdb.ErrNoSidecar {
		return nil, fmt.Errorf("ticket with ID %x already exists",
			ticket.ID[:])
	}

	// First we'll need a new multisig key for the channel that will be
	// opened through this sidecar order.
	keyDesc, err := a.cfg.Wallet.DeriveNextKey(
		ctx, int32(keychain.KeyFamilyMultiSig),
	)
	if err != nil {
		return nil, fmt.Errorf("error deriving multisig key: %v", err)
	}

	ticket.State = sidecar.StateRegistered
	ticket.Recipient = &sidecar.Recipient{
		NodePubKey:       a.cfg.NodePubKey,
		MultiSigPubKey:   keyDesc.PubKey,
		MultiSigKeyIndex: keyDesc.Index,
	}
	if err := a.cfg.SidecarDB.AddSidecar(&ticket); err != nil {
		return nil, fmt.Errorf("error storing sidecar: %v", err)
	}

	return &ticket, nil
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
	if err := a.cfg.SidecarDB.UpdateSidecar(t); err != nil {
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

// validateOrderedTicket validates a ticket in the ordered state to ensure all
// the details are in place, and signed properly.
func validateOrderedTicket(ctx context.Context, t *sidecar.Ticket,
	signer lndclient.SignerClient, db sidecar.Store) error {

	// The ticket should be in the ordered state at this point (has the bid
	// information).
	if t.State != sidecar.StateOrdered {
		return fmt.Errorf("sidecar ticket in state %v, expected %v",
			t.State, sidecar.StateOrdered)
	}

	// Let's make sure the ticket itself and the offer is valid.
	if err := sidecar.VerifyOffer(ctx, t, signer); err != nil {
		return fmt.Errorf("error validating order in sidecar "+
			"ticket: %v", err)
	}

	// Make sure the order signature is valid and the ticket actually exists
	// in our database. We need to have it stored already since must've done
	// the register part before.
	if err := sidecar.VerifyOrder(ctx, t, signer); err != nil {
		return fmt.Errorf("error validating order in sidecar "+
			"ticket: %v", err)
	}
	if _, err := db.Sidecar(t.ID, t.Offer.SignPubKey); err != nil {
		return fmt.Errorf("error looking up sidecar order for "+
			"ticket with ID %x: %v", t.ID[:], err)
	}

	return nil
}

// AutoAcceptSidecar signals to the acceptor that the recipient of a potential
// sidecar channel request automated acceptance of the sidecar channel. We'll
// use the cipher box of the provider of the ticket (and a new one we'll create
// for the reply side) to finalize negotiation, resulting in a bid order.
func (a *SidecarAcceptor) AutoAcceptSidecar(ticket *sidecar.Ticket) error {
	log.Infof("Attempting negotiation to receive sidecar ticket: %x",
		ticket.ID[:])

	autoAcceptor := NewSidecarNegotiator(AutoAcceptorConfig{
		Provider: false,
		StartingPkt: &SidecarPacket{
			CurrentState:   sidecar.StateRegistered,
			ReceiverTicket: ticket,
			ProviderTicket: ticket,
		},
		Driver:  a,
		MailBox: a,
	})

	streamID, err := deriveRecipientStreamID(ticket)
	if err != nil {
		return fmt.Errorf("unable to derive "+
			"stream IDs: %v", err)
	}

	a.Lock()
	a.negotiators[streamID] = autoAcceptor
	a.Unlock()

	return autoAcceptor.Start()
}

// SubmitSidecarOrder attempts to submit a new bid that's bound to a finalized
// sidecar ticket that's in the registered phase. If this method returns
// successfully, then the ticket will have transitioned to the
// sidecar.StateOrdered state.
func (a *SidecarAcceptor) SubmitSidecarOrder(ticket *sidecar.Ticket, bid *order.Bid,
	acct *account.Account) (*sidecar.Ticket, error) {

	// We'll bind the ticket to the order now as the ticket has all the
	// necessary information included.
	bid.SidecarTicket = ticket

	ctx := context.Background()
	auctionTerms, err := a.client.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query auctioneer terms: %v", err)
	}

	err = prepareAndSubmitOrder(
		ctx, bid, auctionTerms, acct, a.client, a.cfg.PrepareOrder,
	)
	if err != nil {
		return nil, err
	}

	return bid.SidecarTicket, nil
}

// CoordinateSidecar signals to the sidecar acceptor that it should attempt to
// automatically coordinate the negotiation of the ultimate order to be
// produced by the sidecar ticket with the recipient.
func (a *SidecarAcceptor) CoordinateSidecar(ticket *sidecar.Ticket,
	bid *order.Bid, acct *account.Account) error {

	log.Infof("Attempting negotiation to offer sidecar ticket: %x",
		ticket.ID[:])

	autoAcceptor := NewSidecarNegotiator(AutoAcceptorConfig{
		Provider:    true,
		ProviderBid: bid,
		StartingPkt: &SidecarPacket{
			CurrentState:   sidecar.StateOffered,
			ProviderTicket: ticket,
			ReceiverTicket: ticket,
		},
		ProviderAccount: acct,
		Driver:          a,
		MailBox:         a,
	})

	streamID, err := deriveRecipientStreamID(ticket)
	if err != nil {
		return fmt.Errorf("unable to derive "+
			"stream IDs: %v", err)
	}

	a.Lock()
	a.negotiators[streamID] = autoAcceptor
	a.Unlock()

	return autoAcceptor.Start()
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
		sdcrLog.Tracef("Received prepare msg from server, "+
			"batch_id=%x: %v", msg.Prepare.BatchId,
			poolrpc.PrintMsg(msg.Prepare))

		if err := a.matchPrepare(msg.Prepare); err != nil {
			sdcrLog.Errorf("unable to handle prepare message: %v",
				err)
			return a.sendRejectBatch(msg.Prepare.BatchId, nil, err)
		}

	case *auctioneerrpc.ServerAuctionMessage_Sign:
		sdcrLog.Tracef("Received sign msg from server, batch_id=%x: %v",
			msg.Sign.BatchId, poolrpc.PrintMsg(msg.Sign))

		if err := a.matchSign(msg.Sign); err != nil {
			sdcrLog.Errorf("unable to handle sign message: %v", err)
			return a.sendRejectBatch(
				a.pendingBatch.ID[:], a.pendingBatch, err,
			)
		}

	case *auctioneerrpc.ServerAuctionMessage_Finalize:
		batchID := msg.Finalize.BatchId

		sdcrLog.Tracef("Received finalize msg from server, "+
			"batch_id=%x: %v", batchID,
			poolrpc.PrintMsg(msg.Finalize))

		// This operation cannot fail.
		a.matchFinalize()

	default:
		sdcrLog.Debugf("Received msg %v from auctioneer on sidecar "+
			"client: %v", poolrpc.PrintMsg(serverMsg))
	}

	return nil
}

// matchPrepare handles an incoming OrderMatchPrepare message from the server.
// Since we're only on the receiving end of a sidecar channel (which is always
// a bid order) the tasks are simplified compared to normal bid order execution.
//
// NOTE: The lock must be held when calling this method.
func (a *SidecarAcceptor) matchPrepare(
	msg *auctioneerrpc.OrderMatchPrepare) error {

	// Parse and formally validate what we got from the server.
	batch, err := order.ParseRPCBatch(msg)
	if err != nil {
		return fmt.Errorf("unable to parse batch: %v", err)
	}

	sdcrLog.Infof("Received PrepareMsg for batch=%x, num_orders=%v",
		batch.ID[:], len(batch.MatchedOrders))

	// Ensure that we do not have any registered shims for the orders
	// in this batch. This is not supposed to happen but we have a bug.
	if err = a.removeShims(batch); err != nil {
		return fmt.Errorf("unable to cleanup shims before start "+
			"preparing the current batch: %v", err)
	}

	// If there is still a pending batch around from a previous iteration,
	// we need to clean up the pending channels first.
	if a.pendingBatch != nil {
		if err := a.removeShims(a.pendingBatch); err != nil {
			return fmt.Errorf("unable to cleanup previous batch: %v", err)
		}
		a.pendingBatch = nil
	}

	// Before we accept the batch, we'll finish preparations on our end
	// which include applying any order match predicates, connecting out to
	// peers, and registering funding shim. We don't do a full batch
	// validation since we don't have any information about the account
	// that's being used to pay for the sidecar channel.
	err = a.cfg.FundingManager.PrepChannelFunding(batch, a.getSidecarAsOrder)
	if err != nil {
		return fmt.Errorf("error preparing channel funding: %w", err)
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
		return fmt.Errorf("error sending accept msg: %v", err)
	}

	// We know we're involved in a batch, so let's store it for the
	// next step.
	a.pendingBatch = batch

	return nil
}

// isPending returns true if the provided batchID matches the current pending
// one.
func (a *SidecarAcceptor) isPending(batchID []byte) bool {
	if a.pendingBatch == nil || !bytes.Equal(batchID, a.pendingBatch.ID[:]) {
		sdcrLog.Errorf("error processing batch sign message, unknown batch "+
			"with ID %x", batchID)

		return false
	}

	return true
}

// matchSign handles an incoming OrderMatchSignBegin message from the server.
// Since we're only on the receiving end of a sidecar channel (which is always
// a bid order) the tasks are simplified compared to normal bid order execution.
//
// NOTE: The lock must be held when calling this method.
func (a *SidecarAcceptor) matchSign(
	msg *auctioneerrpc.OrderMatchSignBegin) error {

	// Assert we're in the correct state to receive a sign message.
	if !a.isPending(msg.BatchId) {
		return fmt.Errorf("pending batchID was: %x got: %x",
			a.pendingBatch.ID[:], msg.BatchId)
	}

	batch := a.pendingBatch
	batchID := a.pendingBatch.ID[:]

	channelInfos, err := a.cfg.FundingManager.SidecarBatchChannelSetup(
		batch, a.pendingOpenChanClient, a.getSidecarAsOrder,
	)
	if err != nil {
		return fmt.Errorf("error setting up channels: %w", err)
	}

	rpcChannelInfos, err := marshallChannelInfo(channelInfos)
	if err != nil {
		return fmt.Errorf("error setting up channels: %v", err)
	}

	sdcrLog.Infof("Received OrderMatchSignBegin for batch=%x, "+
		"num_orders=%v", batchID, len(batch.MatchedOrders))

	sdcrLog.Infof("Sending OrderMatchSign for batch %x", batchID)
	return a.client.SendAuctionMessage(&auctioneerrpc.ClientAuctionMessage{
		Msg: &auctioneerrpc.ClientAuctionMessage_Sign{
			Sign: &auctioneerrpc.OrderMatchSign{
				BatchId:      batchID,
				ChannelInfos: rpcChannelInfos,
			},
		},
	})
}

// finalizeTicketIfExists attempts to signal to the auto negotiator for a given
// sidecar ticket that it's been fully executed.
//
// NOTE: This function MUST be called with the main mutex held.
func (a *SidecarAcceptor) finalizeTicketIfExists(ticket *sidecar.Ticket) {
	streamID, err := deriveRecipientStreamID(ticket)
	if err != nil {
		log.Errorf("unable to derive stream IDs: %v", err)
	}

	// We'll also signal to the negotiator (if it exists) that the ticket
	// has been finalized so it can safely exit. We don't need to hold the
	// main lock here as handleServerMessage obtains the lock while these
	// methods are called.
	negotiator, ok := a.negotiators[streamID]
	if !ok {
		return
	}

	negotiator.TicketExecuted(ticket.State, false)

	delete(a.negotiators, streamID)
}

// matchFinalize handles an incoming OrderMatchFinalize message from the server.
// Since we're only on the receiving end of a sidecar channel (which is always
// a bid order) the tasks are simplified compared to normal bid order execution.
//
// NOTE: The lock must be held when calling this method.
func (a *SidecarAcceptor) matchFinalize() {
	sdcrLog.Infof("Received FinalizeMsg for batch=%x", a.pendingBatch.ID[:])

	// All we need to do now is some cleanup. Even if the cleanup
	// fails, we want to clear the pending batch as we won't receive
	// any more messages for it.
	batch := a.pendingBatch
	a.pendingBatch = nil

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
		if err := a.cfg.SidecarDB.UpdateSidecar(ticket); err != nil {
			sdcrLog.Errorf("Error updating sidecar ticket to "+
				"state complete: %v", err)
		}

		delete(a.pendingSidecarOrders, ourOrder)
		a.pendingSidecarOrdersMtx.Unlock()

		a.cfg.Acceptor.ShimRemoved(dummyBid.(*order.Bid))

		a.finalizeTicketIfExists(ticket)
	}
}

// FinalizeTicket is called by the main batch processing logic of the provider
// of a ticket to signal to the underlying auto state machine (if on exists)
// that the channel has been finalized.
func (a *SidecarAcceptor) FinalizeTicket(t *sidecar.Ticket) {
	a.Lock()
	defer a.Unlock()

	a.finalizeTicketIfExists(t)
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
			unannounced := ticket.Offer.UnannouncedChannel
			zeroConf := ticket.Offer.ZeroConfChannel
			return &order.Bid{
				Kit:                *kit,
				SidecarTicket:      ticket,
				SelfChanBalance:    ticket.Offer.PushAmt,
				UnannouncedChannel: unannounced,
				ZeroConfChannel:    zeroConf,
			}, nil
		}
	}

	return nil, clientdb.ErrNoOrder
}

// sendRejectBatch sends a reject message to the server with the properly
// decoded reason code and the full reason message as a string.
func (a *SidecarAcceptor) sendRejectBatch(batchID []byte, batch *order.Batch,
	failure error) error {

	if batch != nil {
		// As we're rejecting this batch, we'll cancel all funding shims that
		// we may have registered.
		if err := a.removeShims(batch); err != nil {
			return err
		}
		a.pendingBatch = nil
	}

	msg := &auctioneerrpc.ClientAuctionMessage_Reject{
		Reject: &auctioneerrpc.OrderMatchReject{
			BatchId: batchID,
			Reason:  failure.Error(),
		},
	}

	// Attach the status code to the message to give a bit more context.
	var (
		partialReject   *funding.MatchRejectErr
		versionMismatch *order.ErrVersionMismatch
	)
	switch {
	case errors.As(failure, &versionMismatch):
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_BATCH_VERSION_MISMATCH

	case errors.Is(failure, order.ErrMismatchErr):
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_SERVER_MISBEHAVIOR

	case errors.As(failure, &partialReject):
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_PARTIAL_REJECT
		msg.Reject.RejectedOrders = make(map[string]*auctioneerrpc.OrderReject)
		for nonce, reject := range partialReject.RejectedOrders {
			msg.Reject.RejectedOrders[nonce.String()] = reject
		}

	default:
		msg.Reject.ReasonCode = auctioneerrpc.OrderMatchReject_UNKNOWN
	}

	rpcLog.Infof("Sending sidecar batch rejection message for batch %x with "+
		"code %v and message: %v", batchID, msg.Reject.ReasonCode,
		failure)

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
		batch.MatchedOrders, a.cfg.BaseClient, a.getSidecarAsOrder,
	); err != nil {
		return err
	}

	for ourOrder := range batch.MatchedOrders {
		dummyBid, err := a.getSidecarAsOrder(ourOrder)
		if err != nil {
			continue
		}

		a.cfg.Acceptor.ShimRemoved(dummyBid.(*order.Bid))
	}

	return nil
}

// UpdateSidecar writes the passed sidecar ticket to persistent storage.
func (a *SidecarAcceptor) UpdateSidecar(tkt *sidecar.Ticket) error {
	return a.cfg.SidecarDB.UpdateSidecar(tkt)
}

// ValidateOrderedTicket ctx attempts to validate that a given ticket has
// properly transitioned to the ordered state.
func (a *SidecarAcceptor) ValidateOrderedTicket(tkt *sidecar.Ticket) error {
	ctx := context.Background()
	return validateOrderedTicket(ctx, tkt, a.cfg.Signer, a.cfg.SidecarDB)
}

// InitAcctMailbox attempts to create the mailbox with the given stream ID
// using account signature authentication mechanism. If the mailbox already
// exists, then a nil error is to be returned.
func (a *SidecarAcceptor) InitAcctMailbox(streamID [64]byte,
	traderKey *keychain.KeyDescriptor) error {

	err := a.client.InitAccountCipherBox(
		context.Background(), streamID, traderKey,
	)
	if err != nil && !isErrAlreadyExists(err) {
		return fmt.Errorf("unable to init cipher box: %v", err)
	}

	return nil
}

// InitSidecarMailbox attempts to create the mailbox with the given stream ID
// using the sidecar ticket authentication mechanism. If the mailbox already
// exists, then a nil error is to be returned.
func (a *SidecarAcceptor) InitSidecarMailbox(streamID [64]byte,
	tkt *sidecar.Ticket) error {

	err := a.client.InitTicketCipherBox(context.Background(), streamID, tkt)
	if err != nil && !isErrAlreadyExists(err) {
		return fmt.Errorf("unable to init cipher box: %v", err)
	}

	return nil
}

// SendSidecarPkt attempts to send a sidecar packet to the opposite party using
// their registered cipherbox stream.
func (a *SidecarAcceptor) SendSidecarPkt(ctx context.Context, pkt *sidecar.Ticket,
	provider bool) error {

	var ticketBuf bytes.Buffer
	err := sidecar.SerializeTicket(&ticketBuf, pkt)
	if err != nil {
		return err
	}

	streamID, err := deriveStreamID(pkt, provider)
	if err != nil {
		return err
	}

	target := "receiver"
	if provider {
		target = "provider"
	}

	log.Infof("Sending ticket(state=%v, id=%x) to %v stream_id=%x",
		pkt.State, pkt.ID[:], target, streamID[:])

	return a.client.SendCipherBoxMsg(ctx, streamID, ticketBuf.Bytes())
}

// RecvSidecarPkt attempts to receive a new sidecar packet from the opposite
// party using their registered cipherbox stream.
func (a *SidecarAcceptor) RecvSidecarPkt(pCtx context.Context,
	ticket *sidecar.Ticket, provider bool) (*sidecar.Ticket, error) {

	streamID, err := deriveStreamID(ticket, provider)
	if err != nil {
		return nil, err
	}

	log.Infof("Waiting for ticket (id=%x) using stream_id=%x, provider=%v",
		ticket.ID[:], streamID[:], provider)

	ctx, cancel := context.WithCancel(pCtx)
	defer cancel()

	msg, err := a.client.RecvCipherBoxMsg(ctx, streamID)
	if err != nil {
		return nil, fmt.Errorf("unable to recv cipher box "+
			"msg: %w", err)
	}

	log.Infof("Receive new message for ticket (id=%x) "+
		"via stream_id=%x, provider=%v", ticket.ID[:], streamID,
		provider)

	return sidecar.DeserializeTicket(bytes.NewReader(msg))
}

// DelSidecarMailbox tears down the mailbox the sidecar ticket recipient used
// to communicate with the provider.
func (a *SidecarAcceptor) DelSidecarMailbox(streamID [64]byte,
	ticket *sidecar.Ticket) error {

	return a.client.DelSidecarMailbox(
		context.Background(), streamID, ticket,
	)
}

// DelAcctMailbox tears down the mailbox that the sidecar ticket provider used
// to communicate with the recipient.
func (a *SidecarAcceptor) DelAcctMailbox(streamID [64]byte,
	pubKey *keychain.KeyDescriptor) error {

	return a.client.DelAcctMailbox(
		context.Background(), streamID, pubKey,
	)
}

// isErrAlreadyExists returns true if the passed error is the "already exists"
// error within the error wrapped error which is returned by the hash mail
// server when a stream we're attempting to create already exists.
func isErrAlreadyExists(err error) bool {
	statusCode, ok := status.FromError(err)
	if !ok {
		return false
	}

	return statusCode.Code() == codes.AlreadyExists
}
