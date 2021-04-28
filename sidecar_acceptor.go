package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
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
	cfg *SidecarAcceptorConfig

	client                *auctioneer.Client
	pendingOpenChanClient *subscribe.Client

	pendingSidecarOrders    map[order.Nonce]*sidecar.Ticket
	pendingSidecarOrdersMtx sync.Mutex
	pendingBatch            *order.Batch

	sync.Mutex

	quit chan struct{}
	wg   sync.WaitGroup
}

// SidecarAcceptorConfig holds all the configuration information that sidecar
// acceptor needs in order to carry out its dutes.
type SidecarAcceptorConfig struct {
	SidecarDB sidecar.Store

	AcctDB account.Store

	Signer lndclient.SignerClient

	Wallet lndclient.WalletKitClient

	BaseClient funding.BaseClient

	Acceptor *ChannelAcceptor

	NodePubKey *btcec.PublicKey

	ClientCfg auctioneer.Config

	OrderManager *order.Manager

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
		if ticket.State != sidecar.StateExpectingChannel {
			continue
		}

		if ticket.Recipient == nil {
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

// deriveProviderStreamID derives the stream ID of the provider's cipher box,
// we'll use this to allow the recipient to send messages to the provider.
func (a *SidecarAcceptor) deriveProviderStreamID(
	ticket *sidecar.Ticket) ([64]byte, error) {

	var streamID [64]byte

	// This stream ID will simply be the fixed 64-byte signature of our
	// sidecar ticket offer.
	wireSig, err := lnwire.NewSigFromRawSignature(
		ticket.Offer.SigOfferDigest.Serialize(),
	)
	if err != nil {
		return streamID, err
	}

	copy(streamID[:], wireSig[:])

	return streamID, nil
}

// deriveRecipientStreamID derives the stream ID of the cipher box that the
// provider of the sidecar ticket will use to send messages to the receiver.
func (a *SidecarAcceptor) deriveRecipientStreamID(ticket *sidecar.Ticket) [64]byte {
	receiverMultisig := ticket.Recipient.NodePubKey.SerializeCompressed()
	receiverNode := ticket.Recipient.MultiSigPubKey.SerializeCompressed()

	// The stream ID will be the concentration of the receiver's multi-sig
	// and node keys, ignoring the first byte of each key that essentially
	// communicates parity information.
	var (
		streamID [64]byte
		n        int
	)
	n += copy(streamID[:], receiverMultisig[1:])
	copy(streamID[n:], receiverNode[1:])

	return streamID
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
// for the reply side) to finalize negotiation, resulting in a
func (a *SidecarAcceptor) AutoAcceptSidecar(ticket *sidecar.Ticket) error {

	log.Infof("Attempting negotiation to receive sidecar ticket: %x",
		ticket.ID[:])

	ctx := context.Background()

	// We'll launch a new coroutine that'll handle negotiation in the
	// background all the way to the final state of the ticket.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		var ticketSent bool
		for {
			switch {
			// In this state, they've just sent us their version of
			// the ticket w/o our node information (and processed
			// it adding our information), we'll populate it then
			// send it to them over the cipherbox they've created
			// for this purpose.
			case !ticketSent && ticket.State == sidecar.StateRegistered:
				// Before we send our message over, we'll need
				// to create a cipher box stream they can use
				// to reply to us. Our authentication mechanism
				// here will be the sidecar ticket itself.
				//
				// TODO(roasbeef): pass in pubkey for encryption?
				//  * ECEIS?
				recipientStreamID := a.deriveRecipientStreamID(
					ticket,
				)

				log.Infof("Creating reply mailbox for ticket=%x, "+
					"stream_id=%x", ticket.ID[:], recipientStreamID[:])

				err := a.client.InitTicketCipherBox(
					ctx, recipientStreamID, ticket,
				)
				if err != nil {
					log.Errorf("unable to init cipher box: %v", err)
					return
				}

				providerStreamID, err := a.deriveProviderStreamID(ticket)
				if err != nil {
					log.Errorf("unable to derive provider stream ID: %v", err)
					continue
				}

				log.Infof("Sending registered ticket (id=%x) "+
					"to provider (trader_key=%x), stream_id=%x",
					ticket.ID[:],
					ticket.Offer.SignPubKey.SerializeCompressed(),
					providerStreamID[:])

				var ticketBuf bytes.Buffer
				err = sidecar.SerializeTicket(&ticketBuf, ticket)
				if err != nil {
					return
				}

				// TODO(roasbeef): send msg reliability?
				err = a.client.SendCipherBoxMsg(
					ctx, providerStreamID, ticketBuf.Bytes(),
				)
				if err != nil {
					log.Errorf("unable to send cipher box msg: %v", err)
					return
				}

				ticketSent = true

			// In this state, we'll wait for the provider to send
			// us back the ticket that contains their order
			// information.
			case ticketSent && ticket.State == sidecar.StateRegistered:

				// We'll wait for them to send us back a valid
				// ticket over our cipher box.
				recipientStreamID := a.deriveRecipientStreamID(
					ticket,
				)

				log.Infof("Waiting for ticket (id=%x) using "+
					"stream_id=%x", ticket.ID[:],
					recipientStreamID[:])

				msg, err := a.client.RecvCipherBoxMsg(
					ctx, recipientStreamID,
				)
				if err != nil {
					log.Errorf("unable to recv cipher box "+
						"msg: %w", err)
					return
				}

				ticket, err = sidecar.DeserializeTicket(
					bytes.NewReader(msg),
				)
				if err != nil {
					log.Errorf("unable to decode ticket: %v", err)
					continue
				}

				// At this point, we'll finish validating the
				// ticket, then await the ticket on the side lines
				// if it's valid.
				err = validateOrderedTicket(
					ctx, ticket, a.cfg.Signer, a.cfg.SidecarDB,
				)
				if err != nil {
					log.Errorf("unable to verify ticket: %v", err)
					continue
				}

				log.Infof("Auto negotiation for ticket=%x "+
					"complete! Expecting channel...",
					ticket.ID[:])

				// Now that we know the channel is valid, we'll
				// wait for the channel to show up at our node, and
				// allow things to advance to the completion state.
				err = a.ExpectChannel(ctx, ticket)
				if err != nil {
					log.Errorf("failed to expect channel: %v", err)
					continue
				}

				return
			}
		}
	}()

	return nil
}

// submitSidecarOrder attempts to submit a new bid that's bound to a finalized
// sidecar ticket that's in the registered phase. If this method returns
// successfully, then the ticket will have transitioned to the
// sidecar.StateOrdered state.
func (a *SidecarAcceptor) submitSidecarOrder(ctx context.Context,
	ticket *sidecar.Ticket, bid *order.Bid,
	acct *account.Account) (*sidecar.Ticket, error) {

	// We'll bind the ticket to the order now as the ticket has all the
	// necessary information included.
	bid.SidecarTicket = ticket

	auctionTerms, err := a.client.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query auctioneer terms: %v", err)
	}

	err = prepareAndSubmitOrder(
		ctx, bid, auctionTerms, acct, a.client, a.cfg.OrderManager,
	)
	if err != nil {
		return nil, err
	}

	return bid.SidecarTicket, nil
}

// CoordinateSidecar signals to the sidecar acceptor that it should attempt to
// automatically coordinate the negotiation of the ultimate order to be
// produced by the side car ticket with the recipient.
func (a *SidecarAcceptor) CoordinateSidecar(ticket *sidecar.Ticket,
	bid *order.Bid, acct *account.Account) error {

	log.Infof("Attempting negotiation to offer sidecar ticket: %x",
		ticket.ID[:])

	ctx := context.Background()

	// First, we'll need to derive the stream ID that we'll use to receive
	// new messages from the recipient.
	streamID, err := a.deriveProviderStreamID(ticket)
	if err != nil {
		return fmt.Errorf("unable to convert offer sig: %w", err)
	}

	log.Infof("Creating mailbox for ticket=%x, w/ stream_id=%x",
		ticket.ID[:], streamID[:])

	err = a.client.InitAccountCipherBox(
		ctx, streamID, acct.TraderKey,
	)
	if err != nil {
		// TODO(roasbef): catch error already init
		return fmt.Errorf("unable to init account cipher box: %w", err)
	}

	// From here, we'll launch a goroutine that will operate the state
	// machine to complete the negotiation process in the background.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		for {
			switch ticket.State {
			// In this state, we'll wait for the recipient to send
			// us the ticket will the information we need to
			// complete the order such as their multi-sig key in
			// tact.
			//
			// Transition: -> StateRegistered
			case sidecar.StateOffered:
				log.Infof("Waiting for registered ticket=%x "+
					"from recipient", ticket.ID[:])

				// We'll simply wait for the recipient to send
				// us a message over our cipher box stream.
				msg, err := a.client.RecvCipherBoxMsg(
					ctx, streamID,
				)
				if err != nil {
					log.Errorf("unable to recv cipher box "+
						"msg: %w", err)
					return
				}

				// The message should be a valid side car ticket,
				// otherwise it's garbage or the wrong message was
				// sent, so we'll just wait for another one to be
				// sent.
				updatedTicket, err := sidecar.DecodeString(
					string(msg),
				)
				if err != nil {
					log.Errorf("unable to parse ticket: %v", err)
					continue
				}

				// The ticket should have now advanced to the next
				// state, otherwise the protocol wasn't followed.
				if updatedTicket.State != sidecar.StateRegistered {
					log.Errorf("sidecar ticket in state %v, expected %v",
						updatedTicket.State,
						sidecar.StateRegistered)
					continue
				}

				// Now that we have the ticket, we'll update
				// the state on disk to checkpoint the new
				// state.
				err = a.cfg.SidecarDB.UpdateSidecar(updatedTicket)
				if err != nil {
					log.Errorf("unable to update ticket: %v", err)
					continue
				}

				ticket = updatedTicket

			// If we're in this state (possibly after a restart),
			// we have all the information we need to submit the
			// order, so we'll do that, then send the finalized
			// ticket back to the recipient.
			//
			// Transition: -> StateOffered
			case sidecar.StateRegistered:
				log.Infof("Submitting bid order for ticket=%x",
					ticket.ID[:])

				// Now we have the recipient's information, we
				// can attach it to our bid, and submit it as
				// normal.
				ticket, err = a.submitSidecarOrder(
					ctx, ticket, bid, acct,
				)
				switch {
				// If the order has already been submitted,
				// then we'll catch this error and go to the
				// next state. Submitting the order doesn't
				// persist the state update to the ticket, so
				// we don't risk a split brain state.
				case err == nil:
					fallthrough
				case errors.Is(err, clientdb.ErrOrderExists):
					continue

				case err != nil:
					log.Errorf("unable to submit sidecar order: %v", err)
					continue
				}

			// In this state, we've submitted the order and now
			// need to send back the completed order to the
			// recipient so they can expect the ultimate sidecar
			// channel.
			//
			// Transition: -> StateCompleted || StateCanceled
			case sidecar.StateOrdered:
				// Encode the latest ticket as we we can only
				// transmit bytes end to end.
				orderStringTicket, err := sidecar.EncodeToString(
					ticket,
				)
				if err != nil {
					log.Errorf("unable to encode ticket: %v", err)
					continue
				}

				// Next, we'll send the ticket back over to the
				// recipient so they can await the channel
				// (after it's been matched).
				//
				// The stream ID in this case will be the
				// concatenation of their node and multi-sig
				// keys.
				recipientStreamID := a.deriveRecipientStreamID(ticket)

				log.Infof("Sending finalized ticket=%x to "+
					"recipient using stream_id=%x", ticket.ID[:],
					recipientStreamID[:])

				err = a.client.SendCipherBoxMsg(
					ctx, recipientStreamID,
					[]byte(orderStringTicket),
				)
				if err != nil {
					log.Errorf("unable to send msg: %v", err)
					return
				}

				ticket.State = sidecar.StateExpectingChannel

			case sidecar.StateCompleted, sidecar.StateCanceled,
				sidecar.StateExpectingChannel:

				log.Infof("Negotiation for ticket=%x has been "+
					"completed!", ticket.ID[:])
				return
			}
		}
	}()

	return nil
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
	err = a.cfg.FundingManager.PrepChannelFunding(batch, a.getSidecarAsOrder)
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
	channelInfos, err := a.cfg.FundingManager.SidecarBatchChannelSetup(
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
		if err := a.cfg.SidecarDB.UpdateSidecar(ticket); err != nil {
			sdcrLog.Errorf("Error updating sidecar ticket to "+
				"state complete: %v", err)
		}

		delete(a.pendingSidecarOrders, ourOrder)
		a.pendingSidecarOrdersMtx.Unlock()

		a.cfg.Acceptor.ShimRemoved(dummyBid.(*order.Bid))
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
