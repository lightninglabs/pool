package account

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	timeout = 500 * time.Millisecond
)

type testHarness struct {
	t        *testing.T
	store    *mockStore
	notifier *mockChainNotifier
	wallet   *mockWallet
	manager  *Manager
}

func newTestHarness(t *testing.T) *testHarness {
	store := newMockStore()
	wallet := &mockWallet{}
	notifier := newMockChainNotifier()

	return &testHarness{
		t:        t,
		store:    store,
		wallet:   wallet,
		notifier: notifier,
		manager: NewManager(&ManagerConfig{
			Store:         store,
			Auctioneer:    &mockAuctioneer{},
			Wallet:        wallet,
			ChainNotifier: notifier,
			TxSource:      wallet,
		}),
	}
}

func (h *testHarness) start() {
	if err := h.manager.Start(); err != nil {
		h.t.Fatalf("unable to start account manager: %v", err)
	}
}

func (h *testHarness) stop() {
	h.manager.Stop()
}

func (h *testHarness) notifyAccountConf(conf *chainntnfs.TxConfirmation) {
	h.notifier.confChan <- conf
}

func (h *testHarness) assertAcccountExists(expected *Account) {
	h.t.Helper()

	err := wait.NoError(func() error {
		found, err := h.store.Account(expected.TraderKey)
		if err != nil {
			return err
		}

		if !reflect.DeepEqual(found, expected) {
			return fmt.Errorf("expected account: %v\ngot: %v",
				spew.Sdump(expected), spew.Sdump(found))
		}

		return nil
	}, 10*timeout)
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *testHarness) restartManager() {
	h.t.Helper()

	h.manager.Stop()

	h.manager = NewManager(&ManagerConfig{
		Store:         h.manager.cfg.Store,
		Auctioneer:    h.manager.cfg.Auctioneer,
		Wallet:        h.manager.cfg.Wallet,
		ChainNotifier: h.manager.cfg.ChainNotifier,
		TxSource:      h.manager.cfg.TxSource,
	})

	if err := h.manager.Start(); err != nil {
		h.t.Fatalf("unable to restart account manager: %v", err)
	}
}

// TestNewAccountHappyFlow ensures that we are able to create a new account
// throughout the happy flow.
func TestNewAccountHappyFlow(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	// Create a new account. Its initial state should be Pending.
	ctx := context.Background()
	account, err := h.manager.InitAccount(
		ctx, maxAccountValue, maxAccountExpiry, 100,
	)
	if err != nil {
		t.Fatalf("unable to create new account: %v", err)
	}
	if account.State != StatePendingOpen {
		t.Fatalf("expected account state %v, got %v", StatePendingOpen,
			account.State)
	}

	// The same account should be found in the store.
	h.assertAcccountExists(account)

	// Notify the confirmation of the account.
	h.notifyAccountConf(&chainntnfs.TxConfirmation{})

	// This should prompt the account to now be in a Confirmed state.
	account.State = StateOpen
	h.assertAcccountExists(account)
}

// TestResumeAccountAfterRestart ensures we're able to properly create a new
// account even if we've shut down during the process.
func TestResumeAccountAfterRestart(t *testing.T) {
	t.Parallel()

	const (
		value      = maxAccountValue
		expiry     = maxAccountExpiry
		bestHeight = 100
	)

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	// We'll create an interceptor for SendOutputs to simulate a transaction
	// being crafted and persisted for the account, but it not being
	// returned to the account manager because of a crash, etc.
	sendOutputsChan := make(chan *wire.MsgTx)
	sendOutputsInterceptor := func(_ context.Context, outputs []*wire.TxOut,
		_ chainfee.SatPerKWeight) (*wire.MsgTx, error) {

		tx := &wire.MsgTx{
			Version: 2,
			TxOut:   outputs,
		}

		h.wallet.addTx(tx)
		sendOutputsChan <- tx

		return nil, errors.New("error")
	}
	h.wallet.interceptSendOutputs(sendOutputsInterceptor)

	// We'll then proceed to create a new account. We expect this to fail
	// given the interceptor above, but that's fine. We should still have a
	// persisted intent to create the account.
	go func() {
		_, _ = h.manager.InitAccount(
			context.Background(), value, expiry, bestHeight,
		)
	}()

	var tx *wire.MsgTx
	select {
	case tx = <-sendOutputsChan:
	case <-time.After(timeout):
		t.Fatal("expected call to SendOutputs")
	}

	account := &Account{
		Value:            value,
		Expiry:           expiry,
		TraderKey:        testTraderKey,
		TraderKeyLocator: testTraderKeyDesc.KeyLocator,
		AuctioneerKey:    testAuctioneerKey,
		HeightHint:       bestHeight,
		State:            StateInitiated,
	}
	h.assertAcccountExists(account)

	// Then, we'll create a new interceptor to send us a signal if
	// SendOutputs is invoked again. This shouldn't happen as the
	// transaction originally crafted the first time should be in our
	// TxSource, so we should pick it from there.
	sendOutputsSignal := make(chan struct{}, 1)
	sendOutputsInterceptor = func(_ context.Context, outputs []*wire.TxOut,
		_ chainfee.SatPerKWeight) (*wire.MsgTx, error) {

		close(sendOutputsSignal)
		return nil, errors.New("error")
	}
	h.wallet.interceptSendOutputs(sendOutputsInterceptor)

	// Restart the manager. This should cause any pending accounts to be
	// resumed. In our case, we should have an account transaction in our
	// TxSource, so a new one shouldn't be created.
	h.restartManager()

	select {
	case <-sendOutputsSignal:
		t.Fatal("unexpected call to SendOutputs")
	case <-time.After(2 * timeout):
	}

	// With the account resumed, it should now be in a StatePendingOpen
	// state.
	account.State = StatePendingOpen
	account.OutPoint = wire.OutPoint{
		Hash:  tx.TxHash(),
		Index: 0,
	}
	h.assertAcccountExists(account)

	// Notify the confirmation of the account.
	h.notifyAccountConf(&chainntnfs.TxConfirmation{})

	// This should prompt the account to now be in a Confirmed state.
	account.State = StateOpen
	h.assertAcccountExists(account)
}
