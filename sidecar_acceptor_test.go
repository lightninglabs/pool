package pool

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"
)

var (
	_, providerPubKey = btcec.PrivKeyFromBytes([]byte{0x02})
	_, ourNodePubKey  = btcec.PrivKeyFromBytes([]byte{0x03})
	testOfferSig      = test.NewSignatureFromInt(44, 22)
)

func getVerifyParameters(ticket *sidecar.Ticket) ([]byte, []byte, [33]byte) {
	var offerPubKeyRaw [33]byte
	copy(offerPubKeyRaw[:], ticket.Offer.SignPubKey.SerializeCompressed())

	// Make sure the provider's signature over the offer is valid.
	offerDigest, _ := ticket.OfferDigest()
	sigOfferDigest := ticket.Offer.SigOfferDigest.Serialize()

	return offerDigest[:], sigOfferDigest, offerPubKeyRaw
}

var registerSidecarTestCases = []struct {
	name        string
	ticket      *sidecar.Ticket
	expectedErr string
	mockSetter  func(ticket *sidecar.Ticket, sc *test.MockSignerClient,
		wc *test.MockWalletKitClient, store *sidecar.MockStore,
	)
	check func(t *testing.T, ticket *sidecar.Ticket)
}{{
	name: "unable to register sidecar if ticket does not have a " +
		"valid state",
	ticket:      &sidecar.Ticket{},
	expectedErr: "ticket is in invalid state",
}, {
	name: "unable to register sidecar if signature is missing",
	ticket: &sidecar.Ticket{
		State: sidecar.StateOffered,
	},
	expectedErr: "offer in ticket is not signed",
}, {
	name: "unable to register sidecar if signature is invalid",
	ticket: &sidecar.Ticket{
		State: sidecar.StateOffered,
		Offer: sidecar.Offer{
			SignPubKey:     providerPubKey,
			SigOfferDigest: test.NewSignatureFromInt(33, 33),
		},
	},
	expectedErr: "signature not valid for public key",
	mockSetter: func(ticket *sidecar.Ticket, sc *test.MockSignerClient,
		wc *test.MockWalletKitClient, store *sidecar.MockStore,
	) {
		p1, p2, p3 := getVerifyParameters(ticket)
		sc.EXPECT().
			VerifyMessage(
				gomock.Any(),
				p1,
				p2,
				p3,
			).
			Return(false, nil)
	},
}, {
	name: "unable to register sidecar if ticket already exists",
	ticket: &sidecar.Ticket{
		ID:    [8]byte{1, 2, 3, 4},
		State: sidecar.StateOffered,
		Offer: sidecar.Offer{
			SignPubKey:     providerPubKey,
			SigOfferDigest: testOfferSig,
		},
	},
	expectedErr: "already exists",
	mockSetter: func(ticket *sidecar.Ticket, sc *test.MockSignerClient,
		wc *test.MockWalletKitClient, store *sidecar.MockStore,
	) {
		p1, p2, p3 := getVerifyParameters(ticket)

		sc.EXPECT().
			VerifyMessage(
				gomock.Any(),
				p1,
				p2,
				p3,
			).
			Return(true, nil)

		store.EXPECT().
			Sidecar(
				[8]byte{1, 2, 3, 4},
				providerPubKey,
			).
			Return(nil, nil)
	},
}, {
	name: "register sidecar happy path",
	ticket: &sidecar.Ticket{
		ID:    [8]byte{1, 2, 3, 4},
		State: sidecar.StateOffered,
		Offer: sidecar.Offer{
			SignPubKey:     providerPubKey,
			SigOfferDigest: testOfferSig,
		},
	},
	mockSetter: func(ticket *sidecar.Ticket, sc *test.MockSignerClient,
		wc *test.MockWalletKitClient, store *sidecar.MockStore,
	) {
		p1, p2, p3 := getVerifyParameters(ticket)

		sc.EXPECT().
			VerifyMessage(
				gomock.Any(),
				p1,
				p2,
				p3,
			).
			Return(true, nil)

		store.EXPECT().
			Sidecar(
				[8]byte{1, 2, 3, 4},
				providerPubKey,
			).
			Return(nil, clientdb.ErrNoSidecar)

		index := 7
		_, pubkey := test.CreateKey(int32(index))
		wc.EXPECT().
			DeriveNextKey(gomock.Any(), gomock.Any()).
			Return(&keychain.KeyDescriptor{
				KeyLocator: keychain.KeyLocator{
					Index: uint32(index),
				},
				PubKey: pubkey,
			}, nil)

		store.EXPECT().
			AddSidecar(gomock.Any())
	},
	check: func(t *testing.T, ticket *sidecar.Ticket) {
		index := 7
		_, pubkey := test.CreateKey(int32(index))

		require.Equal(t, ticket.State, sidecar.StateRegistered)

		require.Equal(t, ticket.Recipient.NodePubKey, ourNodePubKey)
		require.Equal(t, ticket.Recipient.MultiSigPubKey, pubkey)
		require.Equal(
			t, ticket.Recipient.MultiSigKeyIndex, uint32(index),
		)
	},
}}

// TestRegisterSidecar makes sure that registering a sidecar ticket verifies the
// offer signature contained within, adds the recipient node's information to
// the ticket and stores it to the local database.
func TestRegisterSidecar(t *testing.T) {
	for _, tc := range registerSidecarTestCases {

		t.Run(tc.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			signer := test.NewMockSignerClient(mockCtrl)
			wallet := test.NewMockWalletKitClient(mockCtrl)
			store := sidecar.NewMockStore(mockCtrl)

			if tc.mockSetter != nil {
				tc.mockSetter(tc.ticket, signer, wallet, store)
			}

			acceptor := NewSidecarAcceptor(&SidecarAcceptorConfig{
				SidecarDB:  store,
				AcctDB:     nil,
				Signer:     signer,
				Wallet:     wallet,
				NodePubKey: ourNodePubKey,
				ClientCfg:  auctioneer.Config{},
			})

			ticket, err := acceptor.RegisterSidecar(
				context.Background(), *tc.ticket,
			)

			if tc.expectedErr != "" {
				require.Error(t, err)
				require.Contains(
					t, err.Error(), tc.expectedErr,
				)

				return
			}

			require.NoError(t, err)

			if tc.check != nil {
				tc.check(t, ticket)
			}
		})
	}
}

type mockMailBox struct {
	providerChan     chan *sidecar.Ticket
	providerMsgAck   chan struct{}
	providerDel      chan struct{}
	providerDropChan chan struct{}

	receiverChan     chan *sidecar.Ticket
	receiverMsgAck   chan struct{}
	receiverDel      chan struct{}
	receiverDropChan chan struct{}
}

func newMockMailBox() *mockMailBox {
	return &mockMailBox{
		providerChan:     make(chan *sidecar.Ticket),
		providerMsgAck:   make(chan struct{}),
		providerDel:      make(chan struct{}),
		providerDropChan: make(chan struct{}, 1),

		receiverChan:     make(chan *sidecar.Ticket),
		receiverMsgAck:   make(chan struct{}),
		receiverDel:      make(chan struct{}),
		receiverDropChan: make(chan struct{}, 1),
	}
}

func (m *mockMailBox) RecvSidecarPkt(ctx context.Context, pkt *sidecar.Ticket,
	provider bool) (*sidecar.Ticket, error) {

	var (
		recvChan chan *sidecar.Ticket
		dropChan chan struct{}
		ackChan  chan struct{}
	)
	if provider {
		recvChan = m.providerChan
		ackChan = m.providerMsgAck
		dropChan = m.providerDropChan
	} else {
		recvChan = m.receiverChan
		ackChan = m.receiverMsgAck
		dropChan = m.receiverDropChan
	}

recvMsg:
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("mailbox shutting down")

	case tkt := <-recvChan:
		tktCopy := *tkt

		select {
		case ackChan <- struct{}{}:

		// If we get a signal to drop the message, then we'll just go
		// back to receiving as normal.
		case <-dropChan:
			goto recvMsg
		}

		return &tktCopy, nil
	}
}

func (m *mockMailBox) SendSidecarPkt(ctx context.Context, pkt *sidecar.Ticket,
	provider bool) error {

	var sendChan chan *sidecar.Ticket
	if provider {
		sendChan = m.providerChan
	} else {
		sendChan = m.receiverChan
	}

	select {
	case <-ctx.Done():
	case sendChan <- pkt:
	}

	return nil
}

func (m *mockMailBox) InitSidecarMailbox(streamID [64]byte, ticket *sidecar.Ticket) error {
	return nil
}

func (m *mockMailBox) InitAcctMailbox(streamID [64]byte, pubKey *keychain.KeyDescriptor) error {
	return nil
}

func (m *mockMailBox) DelSidecarMailbox(streamID [64]byte, ticket *sidecar.Ticket) error {
	m.receiverDel <- struct{}{}
	return nil
}

func (m *mockMailBox) DelAcctMailbox(streamID [64]byte, pubKey *keychain.KeyDescriptor) error {
	m.providerDel <- struct{}{}
	return nil
}

type mockDriver struct {
	stateUpdates    chan sidecar.State
	bidSubmitted    chan struct{}
	ticketValidated chan struct{}
	channelExpected chan struct{}
}

func newMockDriver() *mockDriver {
	return &mockDriver{
		stateUpdates:    make(chan sidecar.State),
		bidSubmitted:    make(chan struct{}),
		ticketValidated: make(chan struct{}),
		channelExpected: make(chan struct{}),
	}
}

func (m *mockDriver) ValidateOrderedTicket(tkt *sidecar.Ticket) error {
	if tkt.State != sidecar.StateOrdered {
		return fmt.Errorf("sidecar not in state ordered: %v", tkt.State)
	}

	m.ticketValidated <- struct{}{}

	return nil
}

func (m *mockDriver) ExpectChannel(ctx context.Context, tkt *sidecar.Ticket) error {
	tkt.State = sidecar.StateExpectingChannel

	m.channelExpected <- struct{}{}

	return nil
}

func (m *mockDriver) UpdateSidecar(tkt *sidecar.Ticket) error {
	m.stateUpdates <- tkt.State

	return nil
}

func (m *mockDriver) SubmitSidecarOrder(tkt *sidecar.Ticket, bid *order.Bid,
	acct *account.Account) (*sidecar.Ticket, error) {

	tkt.State = sidecar.StateOrdered

	m.bidSubmitted <- struct{}{}

	return tkt, nil
}

type sidecarTestCtx struct {
	t *testing.T

	provider       *SidecarNegotiator
	providerDriver *mockDriver

	recipient       *SidecarNegotiator
	recipientDriver *mockDriver

	mailbox *mockMailBox
}

func (s *sidecarTestCtx) startNegotiators() error {
	if err := s.provider.Start(); err != nil {
		return err
	}

	return s.recipient.Start()
}

func (s *sidecarTestCtx) restartAllNegotiators() error {
	s.provider.Stop()
	s.recipient.Stop()

	s.provider.quit = make(chan struct{})
	s.provider.stopOnce = sync.Once{}
	s.recipient.quit = make(chan struct{})
	s.recipient.stopOnce = sync.Once{}

	s.provider.cfg.StartingPkt.CurrentState = sidecar.State(s.provider.currentState)
	s.recipient.cfg.StartingPkt.CurrentState = sidecar.State(s.recipient.currentState)

	if err := s.provider.Start(); err != nil {
		return err
	}

	return s.recipient.Start()
}

func (s *sidecarTestCtx) restartProvider() error {
	s.provider.Stop()

	s.provider.quit = make(chan struct{})

	// When we restart the provider in isolation, we'll have their state be
	// mapped to the _created_ state (as the SidecarAcceptor would),
	// which'll cause them to retransmit their last message.
	s.provider.cfg.StartingPkt.CurrentState = sidecar.StateCreated

	return s.provider.Start()
}

func (s *sidecarTestCtx) restartRecipient() error {
	s.recipient.Stop()

	s.recipient.quit = make(chan struct{})

	s.recipient.cfg.StartingPkt.CurrentState = sidecar.State(s.recipient.currentState)

	return s.recipient.Start()
}

func (s *sidecarTestCtx) assertProviderMsgRecv() {
	s.t.Helper()

	select {
	case <-s.mailbox.providerMsgAck:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("no provider msg received")
	}
}

func (s *sidecarTestCtx) assertRecipientMsgRecv() {
	s.t.Helper()

	select {
	case <-s.mailbox.receiverMsgAck:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("no recipient msg received")
	}
}

func (s *sidecarTestCtx) dropReceiverMessage() {
	s.mailbox.receiverDropChan <- struct{}{}
}

func (s *sidecarTestCtx) dropProviderMessage() {
	s.mailbox.providerDropChan <- struct{}{}
}

func (s *sidecarTestCtx) assertNoProviderMsgsRecvd() {
	s.t.Helper()

	select {
	case <-s.mailbox.providerMsgAck:
		s.t.Fatalf("provider should've received no messages")
	case <-time.After(time.Second * 1):
	}
}

func (s *sidecarTestCtx) assertNoReceiverMsgsRecvd() {
	s.t.Helper()

	select {
	case <-s.mailbox.receiverMsgAck:
		s.t.Fatalf("receiver should've received no messages")
	case <-time.After(time.Second * 1):
	}
}

func (s *sidecarTestCtx) assertProviderTicketUpdated(expectedState sidecar.State) {
	s.t.Helper()

	select {
	case stateUpdate := <-s.providerDriver.stateUpdates:

		if stateUpdate != expectedState {
			s.t.Fatalf("expected state=%v, got: %v", expectedState,
				stateUpdate)
		}

	case <-time.After(time.Second * 5):
		s.t.Fatalf("provider ticket never updated")
	}
}

func (s *sidecarTestCtx) assertBidSubmited() {
	s.t.Helper()

	select {
	case <-s.providerDriver.bidSubmitted:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("provider bid never submitted")
	}
}

func (s *sidecarTestCtx) assertRecipientTicketValidated() {
	s.t.Helper()

	select {
	case <-s.recipientDriver.ticketValidated:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("recipient ticket never validated")
	}
}

func (s *sidecarTestCtx) assertRecipientExpectsChannel() {
	s.t.Helper()

	select {
	case <-s.recipientDriver.channelExpected:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("recipient channel never expected")
	}
}

func (s *sidecarTestCtx) assertRecipientTicketUpdated(expectedState sidecar.State) {
	s.t.Helper()

	select {
	case stateUpdate := <-s.recipientDriver.stateUpdates:

		if stateUpdate != expectedState {
			s.t.Fatalf("expected state=%v, got: %v", expectedState,
				stateUpdate)
		}

	case <-time.After(time.Second * 5):
		s.t.Fatalf("recipient ticket never updated")
	}
}

func (s *sidecarTestCtx) confirmSidecarBatch() {
	s.provider.TicketExecuted(sidecar.StateCompleted, false)

	s.recipient.TicketExecuted(sidecar.StateCompleted, false)
}

func (s *sidecarTestCtx) cancelTicket(fromProvider bool) {
	if fromProvider {
		s.provider.TicketExecuted(sidecar.StateCanceled, false)
	} else {
		s.recipient.TicketExecuted(sidecar.StateCanceled, false)
	}
}

func (s *sidecarTestCtx) assertProviderMailboxDel() {
	s.t.Helper()

	select {
	case <-s.mailbox.providerDel:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("provider mailbox not deleted")
	}
}

func (s *sidecarTestCtx) assertRecipientMailboxDel() {
	s.t.Helper()

	select {
	case <-s.mailbox.receiverDel:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("recipient mailbox not deleted")
	}
}

func (s *sidecarTestCtx) assertNegotiatorStates(providerState, recepientState sidecar.State) {
	s.t.Helper()

	err := wait.Predicate(func() bool {
		return s.provider.CurrentState() == providerState
	}, time.Second*5)
	assert.NoError(s.t, err)
	err = wait.Predicate(func() bool {
		return s.recipient.CurrentState() == recepientState
	}, time.Second*5)
	assert.NoError(s.t, err)
}

func (s *sidecarTestCtx) assertProviderShutdown() {
	s.t.Helper()

	select {
	case <-s.provider.quit:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("provider not shut down")
	}

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)

		s.provider.wg.Wait()
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("provider not finished")
	}
}

func (s *sidecarTestCtx) assertRecipientShutdown() {
	s.t.Helper()

	select {
	case <-s.recipient.quit:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("recipient not shut down")
	}

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)

		s.recipient.wg.Wait()
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second * 5):
		s.t.Fatalf("recipient not finished")
	}
}

func newSidecarTestCtx(t *testing.T) *sidecarTestCtx {
	mailBox := newMockMailBox()
	providerDriver := newMockDriver()
	recipientDriver := newMockDriver()

	ticketID := [8]byte{1}

	provider := NewSidecarNegotiator(AutoAcceptorConfig{
		Provider: true,
		ProviderBid: &order.Bid{
			Kit: order.Kit{
				Version:          order.VersionSelfChanBalance,
				LeaseDuration:    144,
				MaxBatchFeeRate:  253,
				MinUnitsMatch:    1,
				Amt:              0,
				UnitsUnfulfilled: 0,
			},
			SelfChanBalance: 1,
		},
		ProviderAccount: &account.Account{},
		StartingPkt: &SidecarPacket{
			CurrentState: sidecar.StateOffered,
			ProviderTicket: &sidecar.Ticket{
				ID:    ticketID,
				State: sidecar.StateOffered,
				Offer: sidecar.Offer{
					SigOfferDigest: testOfferSig,
				},
			},
			ReceiverTicket: &sidecar.Ticket{
				ID:    ticketID,
				State: sidecar.StateOffered,
				Offer: sidecar.Offer{
					SigOfferDigest: testOfferSig,
				},
			},
		},
		Driver:  providerDriver,
		MailBox: mailBox,
	})

	recipient := NewSidecarNegotiator(AutoAcceptorConfig{
		Provider: false,
		StartingPkt: &SidecarPacket{
			CurrentState: sidecar.StateRegistered,
			ProviderTicket: &sidecar.Ticket{
				ID:    ticketID,
				State: sidecar.StateRegistered,
				Offer: sidecar.Offer{
					SigOfferDigest: testOfferSig,
				},
			},
			ReceiverTicket: &sidecar.Ticket{
				ID:    ticketID,
				State: sidecar.StateRegistered,
				Offer: sidecar.Offer{
					SigOfferDigest: testOfferSig,
				},
			},
		},
		Driver:  recipientDriver,
		MailBox: mailBox,
	})

	return &sidecarTestCtx{
		t:               t,
		provider:        provider,
		providerDriver:  providerDriver,
		recipient:       recipient,
		recipientDriver: recipientDriver,
		mailbox:         mailBox,
	}
}

// TestAutoSidecarNegotiation tests the routine sidecar negotiation process
// including that both sides are able to properly handle retransmissions and
// also restarts assuming persistent storage is durable.
func TestAutoSidecarNegotiation(t *testing.T) {
	t.Parallel()

	testCtx := newSidecarTestCtx(t)

	// First, we'll start both negotiators. The provider should no-op, but
	// then the receiver should send over the ticket and complete
	// execution. At the end of the exchange, we expect that both sides are
	// waiting for the channel in its expectation state.
	err := testCtx.startNegotiators()
	assert.NoError(t, err, fmt.Errorf("unable to start negotiators: %v", err))

	// The recipient should send a new message to the provider with their
	// ticket in the registered state.
	testCtx.assertProviderMsgRecv()

	// Upon receiving the new ticket, the provider should write the new
	// registered state to disk, submit the bid, then send the new ticket
	// over to the recipient.
	testCtx.assertProviderTicketUpdated(sidecar.StateRegistered)
	testCtx.assertBidSubmited()
	testCtx.assertRecipientMsgRecv()

	// After sending the ticket, the provider should update the ticket in
	// its database as it waits in the expected state.
	testCtx.assertProviderTicketUpdated(sidecar.StateExpectingChannel)

	// The recipient, should now validate the ticket, then wait and expect
	// the channel.
	testCtx.assertRecipientTicketValidated()
	testCtx.assertRecipientExpectsChannel()

	// At this point, both sides should be waiting for the channel in its
	// expected state.
	testCtx.assertNegotiatorStates(
		sidecar.StateExpectingChannel, sidecar.StateExpectingChannel,
	)

	// We'll now simulate a restart on both sides by signalling their
	// goroutines to exit, then re-starting them anew with their persisted
	// state.
	err = testCtx.restartAllNegotiators()
	assert.NoError(t, err, fmt.Errorf("unable to restart negotiators: %v", err))

	// After the start, both sides should still show that they're expecting
	// the channel
	assert.Equal(
		t, sidecar.StateExpectingChannel, testCtx.provider.CurrentState(),
	)
	assert.Equal(
		t, sidecar.StateExpectingChannel, testCtx.recipient.CurrentState(),
	)

	// Finally there should be no additional message sent either since both
	// sides should now be in a terminal state
	testCtx.assertNoProviderMsgsRecvd()
	testCtx.assertNoReceiverMsgsRecvd()

	// The recipient of the ticket should re-expect the channel to
	// re-register with the auctioneer to ensure the channel can be
	// executed amidst their restarts.
	testCtx.assertRecipientExpectsChannel()

	// We'll now signal to both goroutines that the channel has been
	// finalized, at this point, we expect both ticket to transition to the
	// terminal state and the goroutines to exit.
	go testCtx.confirmSidecarBatch()

	// We expect that both sides now update their state one last time to
	// transition the ticket to a completed state, afterwards, they should
	// move clean up their mailboxes.
	testCtx.assertProviderTicketUpdated(sidecar.StateCompleted)
	testCtx.assertProviderMailboxDel()

	testCtx.assertRecipientTicketUpdated(sidecar.StateCompleted)
	testCtx.assertRecipientMailboxDel()

	// Once again, no messages should be received by either side.
	testCtx.assertNoProviderMsgsRecvd()
	testCtx.assertNoReceiverMsgsRecvd()

	// Both negotiators should be properly shut down.
	testCtx.assertProviderShutdown()
	testCtx.assertRecipientShutdown()
}

// TestAutoSidecarNegotiationRetransmission tests that if either side restarts,
// then the proper message is sent in order to ensure the negotiation state
// machine continues to be progressed.
func TestAutoSidecarNegotiationRetransmission(t *testing.T) {
	t.Parallel()

	testCtx := newSidecarTestCtx(t)

	// We'll start our negotiators as usual, however before we start them
	// we'll make sure that the message sent by the receiver is never
	// received by the provider.
	testCtx.dropProviderMessage()

	err := testCtx.startNegotiators()
	assert.NoError(t, err, fmt.Errorf("unable to start negotiators: %v", err))

	// At this point, both sides should still be in their starting state as
	// the initial message was never received.
	testCtx.assertNegotiatorStates(
		sidecar.StateOffered, sidecar.StateRegistered,
	)

	// We'll now restart only the provider. This should cause the provider
	// to retransmit a message of their offered ticket, which should cause
	// the recipient to re-send their registered ticket.
	//
	// In order to test the other retransmission case, we'll drop the
	// provider's message which carries the ticket in the ordered state.
	require.NoError(t, testCtx.restartProvider())
	testCtx.assertRecipientMsgRecv()

	// The provider receive the ticket, then update their local state as
	// normal.
	testCtx.assertProviderMsgRecv()
	testCtx.dropReceiverMessage()
	testCtx.assertProviderTicketUpdated(sidecar.StateRegistered)
	testCtx.assertBidSubmited()
	testCtx.assertProviderTicketUpdated(sidecar.StateExpectingChannel)

	// The provider should now have transitioned to the final state,
	// however the recipient should still be in their initial registered
	// state as they haven't received any messages yet.
	testCtx.assertNegotiatorStates(
		sidecar.StateExpectingChannel, sidecar.StateRegistered,
	)

	// We'll now restart the recipient, which should cause them to re-send
	// their registered ticket that'll cause the provider to re-send
	// _their_ ticket which should conclude the process with the ticket
	// being fully finalized.
	require.NoError(t, testCtx.restartRecipient())
	testCtx.assertProviderMsgRecv()

	testCtx.assertRecipientMsgRecv()
	testCtx.assertRecipientTicketValidated()
	testCtx.assertRecipientExpectsChannel()
}

// TestAutoSidecarNegotiationCancellation tests that if either side cancels,
// then the proper message is sent in order to ensure the negotiation state
// machine properly stops on both ends.
func TestAutoSidecarNegotiationCancellation(t *testing.T) {
	t.Run("provider cancels", func(tt *testing.T) {
		runAutoSidecarNegotiationCancellation(tt, true)
	})
	t.Run("recipient cancels", func(tt *testing.T) {
		runAutoSidecarNegotiationCancellation(tt, false)
	})
}

func runAutoSidecarNegotiationCancellation(t *testing.T, providerCancels bool) {
	t.Parallel()

	testCtx := newSidecarTestCtx(t)

	// First, we'll start both negotiators. The provider should no-op, but
	// then the receiver should send over the ticket and complete
	// execution. At the end of the exchange, we expect that both sides are
	// waiting for the channel in its expectation state.
	err := testCtx.startNegotiators()
	assert.NoError(t, err, fmt.Errorf("unable to start negotiators: %v", err))

	// The recipient should send a new message to the provider with their
	// ticket in the registered state.
	testCtx.assertProviderMsgRecv()

	// Upon receiving the new ticket, the provider should write the new
	// registered state to disk, submit the bid, then send the new ticket
	// over to the recipient.
	testCtx.assertProviderTicketUpdated(sidecar.StateRegistered)
	testCtx.assertBidSubmited()
	testCtx.assertRecipientMsgRecv()

	// After sending the ticket, the provider should update the ticket in
	// its database as it waits in the expected state.
	testCtx.assertProviderTicketUpdated(sidecar.StateExpectingChannel)

	// The recipient, should now validate the ticket, then wait and expect
	// the channel.
	testCtx.assertRecipientTicketValidated()
	testCtx.assertRecipientExpectsChannel()

	// At this point, both sides should be waiting for the channel in its
	// expected state.
	testCtx.assertNegotiatorStates(
		sidecar.StateExpectingChannel, sidecar.StateExpectingChannel,
	)

	// We now cancel the ticket on the requested side. This cancellation
	// would normally come in as an RPC request, so from its own goroutine.
	go testCtx.cancelTicket(providerCancels)

	// The other side should now receive a message.
	if providerCancels {
		testCtx.assertProviderTicketUpdated(sidecar.StateCanceled)
		testCtx.assertRecipientMsgRecv()
		testCtx.assertRecipientTicketUpdated(sidecar.StateCanceled)
		testCtx.assertRecipientMailboxDel()
	} else {
		testCtx.assertRecipientTicketUpdated(sidecar.StateCanceled)
		testCtx.assertProviderMsgRecv()
		testCtx.assertProviderTicketUpdated(sidecar.StateCanceled)
		testCtx.assertProviderMailboxDel()
	}

	// Once again, no messages should be received by either side.
	testCtx.assertNoProviderMsgsRecvd()
	testCtx.assertNoReceiverMsgsRecvd()
}
