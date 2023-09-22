package pool

import (
	"context"
	"crypto/sha512"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// MailBox is an interface that abstracts over the HashMail functionality to
// represent a generic mailbox that both sides will use to communicate with
// each other.
type MailBox interface {
	// RecvSidecarPkt attempts to receive a new sidecar packet from the
	// relevant mailbox defined by the ticket and sidecar ticket role.
	RecvSidecarPkt(ctx context.Context, pkt *sidecar.Ticket,
		provider bool) (*sidecar.Ticket, error)

	// SendSidecarPkt attempts to send the specified sidecar ticket to the
	// party designated by the provider bool.
	SendSidecarPkt(ctx context.Context, pkt *sidecar.Ticket,
		provider bool) error

	// InitSidecarMailbox attempts to create the mailbox with the given
	// stream ID using the sidecar ticket authentication mechanism. If the
	// mailbox already exists, then a nil error is to be returned.
	InitSidecarMailbox(streamID [64]byte, ticket *sidecar.Ticket) error

	// DelSidecarMailbox tears down the mailbox the sidecar ticket
	// recipient used to communicate with the provider.
	DelSidecarMailbox(streamID [64]byte, ticket *sidecar.Ticket) error

	// InitAcctMailbox attempts to create the mailbox with the given stream
	// ID using account signature authentication mechanism. If the mailbox
	// already exists, then a nil error is to be returned.
	InitAcctMailbox(streamID [64]byte, pubKey *keychain.KeyDescriptor) error

	// DelAcctMailbox tears down the mailbox that the sidecar ticket
	// provider used to communicate with the recipient.
	DelAcctMailbox(streamID [64]byte, pubKey *keychain.KeyDescriptor) error
}

// SidecarPacket encapsulates the current state of an auto sidecar negotiator.
// Note that the state of the negotiator, and the ticket may differ, this is
// what will trigger a state transition.
type SidecarPacket struct {
	// CurrentState is the current state of the negotiator.
	CurrentState sidecar.State

	// ReceiverTicket is the current ticket of the receiver.
	ReceiverTicket *sidecar.Ticket

	// ProviderTicket is the current ticket of the provider.
	ProviderTicket *sidecar.Ticket
}

// deriveProviderStreamID derives the stream ID of the provider's cipher box,
// we'll use this to allow the recipient to send messages to the provider.
func deriveProviderStreamID(ticket *sidecar.Ticket) ([64]byte, error) {
	var streamID [64]byte

	// This stream ID will simply be the fixed 64-byte signature of our
	// sidecar ticket offer.
	wireSig, err := lnwire.NewSigFromSignature(ticket.Offer.SigOfferDigest)
	if err != nil {
		return streamID, err
	}

	copy(streamID[:], wireSig.RawBytes())

	return streamID, nil
}

// deriveRecipientStreamID derives the stream ID of the cipher box that the
// provider of the sidecar ticket will use to send messages to the receiver.
func deriveRecipientStreamID(ticket *sidecar.Ticket) ([64]byte, error) {
	// In order to ensure our retransmission case for the provider works
	// (on start up, it resends the offered ticket in case it got the
	// registered but didn't commit to disk), the provider needs to be able
	// to compute the recipient's stream ID using the base offered ticket.
	//
	// To enable this, we'll use the sha256 of the offer sig as this is
	// static for the lifetime of the entire ticket.
	wireSig, err := lnwire.NewSigFromSignature(ticket.Offer.SigOfferDigest)
	if err != nil {
		return [64]byte{}, err
	}

	streamID := sha512.Sum512(wireSig.RawBytes())

	return streamID, nil
}

// deriveStreamID derives corresponding stream ID for the provider of the
// receiver based on the passed sidecar ticket.
func deriveStreamID(ticket *sidecar.Ticket, provider bool) ([64]byte, error) {
	if provider {
		return deriveProviderStreamID(ticket)
	}

	return deriveRecipientStreamID(ticket)
}

// SidecarDriver houses a series of methods needed to drive a given sidecar
// channel towards completion.
type SidecarDriver interface {
	// ValidateOrderedTicket attempts to validate that a given ticket
	// has properly transitioned to the ordered state.
	ValidateOrderedTicket(tkt *sidecar.Ticket) error

	// ExpectChannel is called by the receiver of a channel once the
	// negotiation process has been finalized, and they need to await a new
	// channel funding flow initiated by the auctioneer server.
	ExpectChannel(ctx context.Context, tkt *sidecar.Ticket) error

	// UpdateSidecar writes the passed sidecar ticket to persistent
	// storage.
	UpdateSidecar(tkt *sidecar.Ticket) error

	// SubmitSidecarOrder submits a bid derived from the sidecar ticket,
	// account, and bid template to the auctioneer.
	SubmitSidecarOrder(*sidecar.Ticket, *order.Bid,
		*account.Account) (*sidecar.Ticket, error)
}

// AutoAcceptorConfig houses all the functionality the sidecar negotiator needs
// to carry out its duties.
type AutoAcceptorConfig struct {
	// Provider denotes if the negotiator is the provider or not.
	Provider bool

	// ProviderBid is the provider's bid template.
	ProviderBid *order.Bid

	// ProviderAccount points to the active account of the provider of the
	// ticket.
	ProviderAccount *account.Account

	// StartingPkt is the starting packet, or the starting state from the
	// PoV of the negotiator.
	StartingPkt *SidecarPacket

	// Drive contains functionality needed to drive a new sidecar ticket
	// towards completion.
	Driver SidecarDriver

	// MailBox is used to allow negotiators to send messages back and forth
	// to each other.
	MailBox MailBox
}

// finalization is a struct that contains the reason (state) and initiator of a
// finalization message we receive.
type finalization struct {
	// state is the new state of the ticket after the finalization. This is
	// mainly meant as an indication whether the finalization was part of
	// the normal flow or caused by a cancellation.
	state sidecar.State

	// otherSide indicates that the other side caused the finalization of
	// the ticket. This should only be set to true for cancellations.
	otherSide bool
}

// SidecarNegotiator is a sub-system that uses a mailbox abstraction between a
// provider and recipient of a sidecar channel to complete the manual steps in
// automated manner.
type SidecarNegotiator struct {
	currentState uint32

	cfg AutoAcceptorConfig

	wg sync.WaitGroup

	ticketFinalized chan *finalization
	quit            chan struct{}

	stopOnce sync.Once
}

// NewSidecarNegotiator returns a new instance of the sidecar negotiator given
// a valid config.
func NewSidecarNegotiator(cfg AutoAcceptorConfig) *SidecarNegotiator {
	return &SidecarNegotiator{
		cfg:             cfg,
		currentState:    uint32(cfg.StartingPkt.CurrentState),
		ticketFinalized: make(chan *finalization),
		quit:            make(chan struct{}),
	}
}

// Start kicks off the set of goroutines needed for the sidecar channel to be
// negotiated.
func (a *SidecarNegotiator) Start() error {
	// In order to ensure we can exit properly if signalled, we'll launch a
	// goroutine that will cancel a global context if we need to exit.
	a.wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer a.wg.Done()

		<-a.quit

		cancel()
	}()

	if a.cfg.Provider {
		a.wg.Add(1)
		go a.autoSidecarProvider(
			ctx, a.cfg.StartingPkt, a.cfg.ProviderBid,
			a.cfg.ProviderAccount,
		)
	} else {
		a.wg.Add(1)
		go a.autoSidecarReceiver(ctx, a.cfg.StartingPkt)
	}

	return nil
}

// Stop signals all goroutines to enter a graceful shutdown.
func (a *SidecarNegotiator) Stop() {
	a.stopOnce.Do(func() {
		close(a.quit)
		a.wg.Wait()
	})
}

// TicketExecuted is a clean up function that should be called once the ticket
// has been executed, meaning a channel defined by it was confirmed in a batch
// on chain.
func (a *SidecarNegotiator) TicketExecuted(state sidecar.State, otherSide bool) {
	select {
	case a.ticketFinalized <- &finalization{
		state:     state,
		otherSide: otherSide,
	}:
	case <-a.quit:
	}

	a.Stop()
}

// autoSidecarReceiver is a goroutine that will attempt to advance a new
// sidecar ticket through the process until it reaches its final state.
func (a *SidecarNegotiator) autoSidecarReceiver(ctx context.Context,
	startingPkt *SidecarPacket) {

	defer a.wg.Done()

	packetChan := make(chan *sidecar.Ticket, 1)
	cancelChan := make(chan struct{})

	atomic.StoreUint32(&a.currentState, uint32(startingPkt.CurrentState))
	localTicket := startingPkt.ReceiverTicket

	// We'll start with a simulated starting message from the sidecar
	// provider.
	packetChan <- startingPkt.ProviderTicket

	// Before we enter our main read loop below, we'll attempt to re-create
	// out mailbox as the recipient.
	recipientStreamID, err := deriveRecipientStreamID(localTicket)
	if err != nil {
		log.Errorf("unable to derive recipient ID: %v", err)
		return
	}

	log.Infof("Creating receiver reply mailbox for ticket=%x, "+
		"stream_id=%x", startingPkt.ReceiverTicket.ID[:],
		recipientStreamID[:])

	err = a.cfg.MailBox.InitSidecarMailbox(
		recipientStreamID, startingPkt.ReceiverTicket,
	)
	if err != nil && !isErrAlreadyExists(err) {
		log.Errorf("unable to init cipher box: %v", err)
		return
	}

	// Launch a goroutine to continually read new packets off the wire and
	// send them to our state step routine. We'll always read packets until
	// things are finished, as the other side may retransmit messages until
	// the process has been finalized.
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		// We'll continue to read out new messages from the cipherbox
		// stream and deliver them to the main gorotuine until we
		// receive a message over the cancel channel.
		var retryTimer backOffer
		backoffLabel := fmt.Sprintf("ticket_id=%x",
			startingPkt.ReceiverTicket.ID[:])
		for {
			newTicket, err := a.cfg.MailBox.RecvSidecarPkt(
				ctx, startingPkt.ReceiverTicket, false,
			)
			if err != nil {
				log.Error(err)

				select {
				case <-retryTimer.backOff(backoffLabel):
					// It is possible that we were not able to receive the
					// packet because the server went down. In that case,
					// we will try to init the sidecar mailbox again.
					// There is no need to log/check this error. There are
					// three possibilities:
					//
					// 1) If the error was not related with the sever being
					// down, we will get an `AlreadyExists` error.
					//
					// 2) If the server is back, we will be able to reconnect
					// successfully and receive the sidecar pkt in the next
					// iteration.
					//
					// 3) If the server is down, we won't be able to reconnect.
					_ = a.cfg.MailBox.InitSidecarMailbox(
						recipientStreamID, startingPkt.ReceiverTicket,
					)
					continue
				case <-a.quit:
					return
				}
			}

			select {
			case packetChan <- newTicket:

			case <-cancelChan:
				return
			case <-a.quit:
				return
			}
		}
	}()

	for {
		select {
		case newTicket := <-packetChan:
			newPktState, err := a.stateStepRecipient(ctx, &SidecarPacket{
				CurrentState:   sidecar.State(a.currentState),
				ProviderTicket: newTicket,
				ReceiverTicket: localTicket,
			})
			if err != nil {
				log.Errorf("unable to transition state: %v", err)
				continue
			}

			// TODO(roasbeef): make into method for easier
			// assertions?
			atomic.StoreUint32(
				&a.currentState, uint32(newPktState.CurrentState),
			)

			localTicket = newPktState.ReceiverTicket

		case fin := <-a.ticketFinalized:
			log.Infof("Receiver negotiation for SidecarTicket(%x) "+
				"complete with state '%v'!", localTicket.ID[:],
				fin.state)

			// The ticket has been marked as finalized, so we'll
			// update it as being in a final state in the database.
			localTicket.State = fin.state
			if err := a.cfg.Driver.UpdateSidecar(localTicket); err != nil {
				log.Errorf("unable to update ticket to "+
					"complete state: %v", err)
				return
			}

			// Did we receive the cancellation from the provider or
			// was it us that canceled the ticket?
			switch {
			// Our side canceled the ticket and a recipient
			// registered for it. We need to inform them about the
			// cancellation. They will then go ahead and remove the
			// mailbox from their end.
			case !fin.otherSide &&
				fin.state == sidecar.StateCanceled:

				// Sending a message doesn't block until it is
				// received. To make sure we don't cancel the
				// context right after we've sent the message
				// (but maybe _before_ it is received), we use
				// a background context here.
				ctxb := context.Background()
				err := a.cfg.MailBox.SendSidecarPkt(
					ctxb, localTicket, true,
				)
				if err != nil {
					log.Errorf("unable to send cancel "+
						"msg to provider: %v", err)
					return
				}

			// The other side informed us about the new state of the
			// ticket. Because they don't know when we're done
			// reading from the mailbox, they want us to remove it
			// once we've received the update.
			default:
				err := a.cfg.MailBox.DelSidecarMailbox(
					recipientStreamID, localTicket,
				)
				if err != nil {
					log.Errorf("unable to reclaim "+
						"mailbox: %v", err)
					return
				}
			}

		case <-a.quit:
			return
		}
	}
}

// stateStepRecipient is a state transition function that will walk the
// receiver through the sidecar negotiation process. It takes the current state
// (the state of the goroutine, and the incoming ticket) and maps that into a
// new state, with a possibly modified ticket.
func (a *SidecarNegotiator) stateStepRecipient(ctx context.Context,
	pkt *SidecarPacket) (*SidecarPacket, error) {

	switch {
	// If the state of the ticket shows up as offered, then this is the
	// remote party restarting and requesting we re-send our registered
	// ticket. So we'll fall through to our "starting" state below to
	// re-send them the packet.
	case pkt.ProviderTicket.State == sidecar.StateOffered:
		log.Infof("Provider retransmitted initial offer, re-sending "+
			"registered ticket=%x", pkt.ProviderTicket.ID[:])

		fallthrough

	// In this state, they've just sent us their version of the ticket w/o
	// our node information (and processed it adding our information),
	// we'll populate it then send it to them over the cipherbox they've
	// created for this purpose.
	case pkt.CurrentState == sidecar.StateRegistered &&
		pkt.ReceiverTicket.State == sidecar.StateRegistered &&
		pkt.ProviderTicket.State == sidecar.StateRegistered:

		log.Infof("Transmitting registered ticket=%x to provider",
			pkt.ProviderTicket.ID[:])

		err := a.cfg.MailBox.SendSidecarPkt(ctx, pkt.ReceiverTicket, true)
		if err != nil {
			return nil, fmt.Errorf("unable to send pkt: %w", err)
		}

		// We'll return a new packet that should reflect our state
		// after the above message is sent: both parties have the
		// ticket in the registered state.
		return &SidecarPacket{
			CurrentState:   sidecar.StateRegistered,
			ReceiverTicket: pkt.ReceiverTicket,
			ProviderTicket: pkt.ReceiverTicket,
		}, nil

	// This is effectively our final state transition: we're waiting with a
	// local registered ticket and receive a ticket in the ordered state.
	// We'll validate the ticket and start expecting the channel and
	// transition to our final state.
	case pkt.CurrentState == sidecar.StateRegistered &&
		pkt.ProviderTicket.State == sidecar.StateOrdered:

		// At this point, we'll finish validating the ticket, then
		// await the ticket on the side lines if it's valid.
		err := a.cfg.Driver.ValidateOrderedTicket(pkt.ProviderTicket)
		if err != nil {
			return nil, fmt.Errorf("unable to verify ticket: "+
				"%w", err)
		}

		log.Infof("Auto negotiation for ticket=%x complete! Expecting "+
			"channel...", pkt.ProviderTicket.ID[:])

		// Now that we know the channel is valid, we'll wait for the
		// channel to show up at our node, and allow things to advance
		// to the completion state.
		err = a.cfg.Driver.ExpectChannel(ctx, pkt.ProviderTicket)
		if err != nil {
			return nil, fmt.Errorf("failed to expect "+
				"channel: %w", err)
		}

		return &SidecarPacket{
			CurrentState:   sidecar.StateExpectingChannel,
			ReceiverTicket: pkt.ProviderTicket,
			ProviderTicket: pkt.ProviderTicket,
		}, nil

	// In case the provider cancels the ticket, we need to abort as well.
	case pkt.ProviderTicket.State == sidecar.StateCanceled:
		// We can now cancel this negotiator. Because stopping will
		// send another message on a channel that is read by the same
		// goroutine we are currently in, we need to do it in a new one.
		go a.TicketExecuted(sidecar.StateCanceled, true)

		return &SidecarPacket{
			CurrentState:   sidecar.StateCanceled,
			ReceiverTicket: pkt.ReceiverTicket,
			ProviderTicket: pkt.ProviderTicket,
		}, nil

	// If we come back up and we're already expecting the channel then we
	// need to make sure we expect it again to ensure we re-register with
	// the auctioneer to be able to receive the channel.
	case pkt.CurrentState == sidecar.StateExpectingChannel:
		err := a.cfg.Driver.ExpectChannel(ctx, pkt.ProviderTicket)
		if err != nil {
			return nil, fmt.Errorf("failed to expect "+
				"channel: %w", err)
		}
		return &SidecarPacket{
			CurrentState:   sidecar.StateExpectingChannel,
			ReceiverTicket: pkt.ReceiverTicket,
			ProviderTicket: pkt.ProviderTicket,
		}, nil

	// If we fall through here, then either we read a buffered message or
	// the remote party isn't following the protocol, so we'll just ignore
	// it.
	default:
		return nil, fmt.Errorf("unhandled receiver state transition "+
			"for ticket=%x, state=%v", pkt.ProviderTicket.ID[:],
			pkt.ProviderTicket.State)
	}
}

// autoSidecarProvider is a goroutine that will attempt to advance a new
// sidecar ticket through the negotiation process until it reaches its final
// state.
func (a *SidecarNegotiator) autoSidecarProvider(ctx context.Context,
	startingPkt *SidecarPacket, bid *order.Bid, acct *account.Account) {

	defer a.wg.Done()

	packetChan := make(chan *sidecar.Ticket, 1)
	cancelChan := make(chan struct{})

	atomic.StoreUint32(&a.currentState, uint32(startingPkt.CurrentState))
	localTicket := startingPkt.ProviderTicket

	// We'll start with a simulated starting message from the sidecar
	// receiver, but only if we're starting in the created state which
	// demands an internal retransmission.
	if startingPkt.CurrentState == sidecar.StateCreated {
		packetChan <- startingPkt.ReceiverTicket
	}

	// First, we'll need to derive the stream ID that we'll use to receive
	// new messages from the recipient.
	streamID, err := deriveProviderStreamID(localTicket)
	if err != nil {
		log.Errorf("unable to derive stream_id for ticket=%x",
			localTicket.ID[:])
		return
	}

	log.Infof("Creating provider mailbox for ticket=%x, w/ stream_id=%x",
		localTicket.ID[:], streamID[:])

	err = a.cfg.MailBox.InitAcctMailbox(streamID, acct.TraderKey)
	if err != nil && !isErrAlreadyExists(err) {
		log.Errorf("unable to init cipher box: %v", err)
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()

		// We'll continue to read out new messages from the cipherbox
		// stream and deliver them to the main gorotuine until we
		// receive a message over the cancel channel.
		var retryTimer backOffer
		backoffLabel := fmt.Sprintf("ticket_id=%x",
			startingPkt.ReceiverTicket.ID[:])
		for {
			newTicket, err := a.cfg.MailBox.RecvSidecarPkt(
				ctx, startingPkt.ProviderTicket, true,
			)
			if err != nil {
				log.Error(err)

				select {
				case <-retryTimer.backOff(backoffLabel):
					// It is possible that we were not able to receive the
					// packet because the server went down. In that case,
					// we will try to init the sidecar mailbox again.
					// There is no need to log/check this error. There are
					// three possibilities:
					//
					// 1) If the error was not related with the sever being
					// down, we will get an `AlreadyExists` error.
					//
					// 2) If the server is back, we will be able to reconnect
					// successfully and receive the sidecar pkt in the next
					// iteration.
					//
					// 3) If the server is down, we won't be able to reconnect.
					_ = a.cfg.MailBox.InitAcctMailbox(streamID, acct.TraderKey)
					continue
				case <-a.quit:
					return
				}
			}

			select {
			case packetChan <- newTicket:

			case <-cancelChan:
				return
			case <-a.quit:
				return
			}
		}
	}()

	for {
		select {
		case newTicket := <-packetChan:
			// The provider has more states it needs to transition
			// through, so we'll continue until we end up at the
			// same state (a noop).
		stateUpdateLoop:
			for {
				priorState := sidecar.State(atomic.LoadUint32(&a.currentState))

				newPktState, err := a.stateStepProvider(ctx, &SidecarPacket{
					CurrentState:   sidecar.State(a.currentState),
					ReceiverTicket: newTicket,
					ProviderTicket: localTicket,
				}, bid, acct)
				if err != nil {
					log.Errorf("unable to transition state: %v", err)
					break
				}

				localTicket = newPktState.ProviderTicket

				atomic.StoreUint32(
					&a.currentState, uint32(newPktState.CurrentState),
				)

				switch {
				case priorState == newPktState.CurrentState:
					break stateUpdateLoop
				case newPktState.CurrentState == sidecar.StateExpectingChannel:
					break stateUpdateLoop
				case newPktState.CurrentState == sidecar.StateCanceled:
					break stateUpdateLoop
				}
			}

		case fin := <-a.ticketFinalized:
			log.Infof("Provider negotiation for SidecarTicket(%x) "+
				"complete with state '%v'!", localTicket.ID[:],
				fin.state)

			// The ticket has been marked as finalized, so we'll
			// update it as being in a final state in the database.
			localTicket.State = fin.state
			if err := a.cfg.Driver.UpdateSidecar(localTicket); err != nil {
				log.Errorf("unable to update ticket to "+
					"complete state: %v", err)
				return
			}

			// Did we ever go into the registered state or further?
			// If no recipient registered, then nobody would listen
			// for the cancellation message.
			switch {
			// Our side canceled the ticket and a recipient
			// registered for it. We need to inform them about the
			// cancellation. They will then go ahead and remove the
			// mailbox from their end.
			case !fin.otherSide &&
				fin.state == sidecar.StateCanceled &&
				a.CurrentState() >= sidecar.StateRegistered:

				// Sending a message doesn't block until it is
				// received. To make sure we don't cancel the
				// context right after we've sent the message
				// (but maybe _before_ it is received), we use
				// a background context here.
				ctxb := context.Background()
				err := a.cfg.MailBox.SendSidecarPkt(
					ctxb, localTicket, false,
				)
				if err != nil {
					log.Errorf("unable to send cancel "+
						"msg to recipient: %v", err)
					return
				}

			// In every other case we can just remote the mailbox:
			//  - The other side cancelled the ticket.
			//  - We completed the ticket going through the normal
			//    process.
			//  - We cancelled the ticket, but we know nobody ever
			//    registered for it on the other side.
			default:
				err := a.cfg.MailBox.DelAcctMailbox(
					streamID, acct.TraderKey,
				)
				if err != nil {
					log.Errorf("unable to reclaim "+
						"mailbox: %v", err)
					return
				}
			}

		case <-a.quit:
			return
		}
	}
}

// CurrentState returns the current state of the sidecar negotiator.
func (a *SidecarNegotiator) CurrentState() sidecar.State {
	state := atomic.LoadUint32(&a.currentState)
	return sidecar.State(state)
}

// stateStepProvider is the state transition function for the provider of a
// sidecar ticket. It takes the current transcript state, the provider's
// account, and canned bid and returns a new transition to a new ticket state.
func (a *SidecarNegotiator) stateStepProvider(ctx context.Context,
	pkt *SidecarPacket, bid *order.Bid,
	acct *account.Account) (*SidecarPacket, error) {

	switch {
	// In this case, we've just restarted, so we'll attempt to start from
	// scratch by sending the recipient a packet that has our ticket in the
	// offered state. This signals to them we never wrote the registered
	// ticket and need it again.
	case pkt.CurrentState == sidecar.StateCreated &&
		pkt.ProviderTicket.State == sidecar.StateOffered:

		log.Infof("Resuming negotiation for ticket=%x, requesting "+
			"registered ticket", pkt.ProviderTicket.ID[:])

		err := a.cfg.MailBox.SendSidecarPkt(ctx, pkt.ProviderTicket, false)
		if err != nil {
			return nil, err
		}

		return &SidecarPacket{
			CurrentState:   sidecar.StateOffered,
			ReceiverTicket: pkt.ProviderTicket,
			ProviderTicket: pkt.ReceiverTicket,
		}, nil

	// In this state, we've just started anew, and have received a ticket
	// from the receiver with their node information. We'll write this to
	// disk, then transition to the next state.
	//
	// Transition: -> StateRegistered
	case pkt.CurrentState == sidecar.StateOffered &&
		pkt.ReceiverTicket.State == sidecar.StateRegistered:

		log.Infof("Received registered ticket=%x from recipient",
			pkt.ReceiverTicket.ID[:])

		// Now that we have the ticket, we'll update the state on disk
		// to checkpoint the new state.
		err := a.cfg.Driver.UpdateSidecar(pkt.ReceiverTicket)
		if err != nil {
			return nil, fmt.Errorf("unable to update ticket: %w",
				err)
		}

		return &SidecarPacket{
			CurrentState:   sidecar.StateRegistered,
			ReceiverTicket: pkt.ReceiverTicket,
			ProviderTicket: pkt.ReceiverTicket,
		}, nil

	// In case the recipient cancels the ticket, we need to abort as well.
	case pkt.ReceiverTicket.State == sidecar.StateCanceled:
		// We can now cancel this negotiator. Because stopping will
		// send another message on a channel that is read by the same
		// goroutine we are currently in, we need to do it in a new one.
		go a.TicketExecuted(sidecar.StateCanceled, true)

		return &SidecarPacket{
			CurrentState:   sidecar.StateCanceled,
			ReceiverTicket: pkt.ReceiverTicket,
			ProviderTicket: pkt.ProviderTicket,
		}, nil

	// If we're in this state (possibly after a restart), we have all the
	// information we need to submit the order, so we'll do that, then send
	// the finalized ticket back to the recipient.
	//
	// Transition: -> StateOrdered
	case pkt.CurrentState == sidecar.StateRegistered:
		log.Infof("Submitting bid order for ticket=%x",
			pkt.ProviderTicket.ID[:])

		// Now we have the recipient's information, we can attach it to
		// our bid, and submit it as normal.
		updatedTicket, err := a.cfg.Driver.SubmitSidecarOrder(
			pkt.ProviderTicket, bid, acct,
		)
		switch {
		// If the order has already been submitted, then we'll catch
		// this error and go to the next state. Submitting the order
		// doesn't persist the state update to the ticket, so we don't
		// risk a split brain state.
		case err == nil:
		case errors.Is(err, clientdb.ErrOrderExists):

		default:
			return nil, fmt.Errorf("unable to submit sidecar "+
				"order: %v", err)
		}

		return &SidecarPacket{
			CurrentState:   sidecar.StateOrdered,
			ReceiverTicket: updatedTicket,
			ProviderTicket: updatedTicket,
		}, nil

	// In this state, we've already sent over the final ticket, but the
	// other party is requesting a re-transmission.
	case pkt.CurrentState == sidecar.StateExpectingChannel &&
		pkt.ReceiverTicket.State == sidecar.StateRegistered:

		fallthrough

	// In this state, we've submitted the order and now need to send back
	// the completed order to the recipient so they can expect the ultimate
	// sidecar channel. Notice that we don't persist this state, as upon
	// restart we'll always re-send the ticket to the other party until
	// things are finalized.
	//
	// Transition: -> StateExpectingChannel
	case pkt.CurrentState == sidecar.StateOrdered:
		log.Infof("Sending finalize ticket=%x to receiver, entering "+
			"final stage", pkt.ProviderTicket.ID[:])

		// We might be retransmitting here, so ensure that the ticket
		// we send over is in the state they expect.
		pkt.ProviderTicket.State = sidecar.StateOrdered

		err := a.cfg.MailBox.SendSidecarPkt(ctx, pkt.ProviderTicket, false)
		if err != nil {
			return nil, fmt.Errorf("unable to send sidecar "+
				"pkt: %v", err)
		}

		updatedTicket := *pkt.ProviderTicket
		updatedTicket.State = sidecar.StateExpectingChannel

		// Now that we have the final ticket, we'll update the state on
		// disk to checkpoint the new state. If the remote party ends
		// us any messages after we persist this state, then we'll
		// simply re-send the latest ticket.
		err = a.cfg.Driver.UpdateSidecar(&updatedTicket)
		if err != nil {
			return nil, fmt.Errorf("unable to update ticket: %w",
				err)
		}

		log.Infof("Negotiation for ticket=%x has been "+
			"completed!", pkt.ProviderTicket.ID[:])

		return &SidecarPacket{
			CurrentState:   sidecar.StateExpectingChannel,
			ReceiverTicket: &updatedTicket,
			ProviderTicket: &updatedTicket,
		}, nil

	default:
		return nil, fmt.Errorf("unhandled provider state "+
			"transition ticket=%x, state=%v",
			pkt.ReceiverTicket.ID[:], pkt.ReceiverTicket.State)
	}
}
