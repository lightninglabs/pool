package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	timeout = 500 * time.Millisecond
)

type testHarness struct {
	t          *testing.T
	store      *mockStore
	notifier   *mockChainNotifier
	wallet     *mockWallet
	auctioneer *mockAuctioneer
	manager    *Manager
}

func newTestHarness(t *testing.T) *testHarness {
	store := newMockStore()
	wallet := newMockWallet()
	notifier := newMockChainNotifier()
	auctioneer := newMockAuctioneer()

	return &testHarness{
		t:          t,
		store:      store,
		wallet:     wallet,
		notifier:   notifier,
		auctioneer: auctioneer,
		manager: NewManager(&ManagerConfig{
			Store:         store,
			Auctioneer:    auctioneer,
			Wallet:        wallet,
			Signer:        wallet,
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

func (h *testHarness) assertAccountSubscribed(traderKey *btcec.PublicKey) {
	h.t.Helper()

	var rawTraderKey [33]byte
	copy(rawTraderKey[:], traderKey.SerializeCompressed())

	h.auctioneer.mu.Lock()
	defer h.auctioneer.mu.Unlock()

	if _, ok := h.auctioneer.subscribed[rawTraderKey]; !ok {
		h.t.Fatalf("account %x not subscribed", traderKey)
	}
}

func (h *testHarness) assertAccountNotSubscribed(traderKey *btcec.PublicKey) {
	h.t.Helper()

	var rawTraderKey [33]byte
	copy(rawTraderKey[:], traderKey.SerializeCompressed())

	h.auctioneer.mu.Lock()
	defer h.auctioneer.mu.Unlock()

	if _, ok := h.auctioneer.subscribed[rawTraderKey]; ok {
		h.t.Fatalf("account %x is subscribed", traderKey)
	}
}

func (h *testHarness) assertAccountExists(expected *Account) {
	h.t.Helper()

	err := wait.NoError(func() error {
		found, err := h.store.Account(expected.TraderKey.PubKey)
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

func (h *testHarness) openAccount(value btcutil.Amount, expiry uint32,
	bestHeight uint32) *Account {

	h.t.Helper()

	// Create a new account. Its initial state should be StatePendingOpen.
	ctx := context.Background()
	account, err := h.manager.InitAccount(ctx, value, expiry, bestHeight)
	if err != nil {
		h.t.Fatalf("unable to create new account: %v", err)
	}

	// The same account should be found in the store.
	h.assertAccountExists(account)

	// Since the account is still pending confirmation, it should not be
	// subscribed to updates from the auctioneer yet.
	h.assertAccountNotSubscribed(account.TraderKey.PubKey)

	// Notify the confirmation of the account.
	h.notifier.confChan <- &chainntnfs.TxConfirmation{}

	// This should prompt the account to now be in a StateOpen state and
	// the subscription for updates should now be realized.
	account.State = StateOpen
	h.assertAccountExists(account)
	h.assertAccountSubscribed(account.TraderKey.PubKey)

	return account
}

func (h *testHarness) expireAccount(account *Account) {
	h.t.Helper()

	// Notify the height at which the account expires.
	h.notifier.blockChan <- int32(account.Expiry)

	// This should prompt the account to now be in a StateExpired state.
	account.State = StateExpired
	h.assertAccountExists(account)
}

func (h *testHarness) closeAccount(account *Account, outputs []*wire.TxOut,
	bestHeight uint32) *wire.MsgTx {

	h.t.Helper()

	// Close the account with the auctioneer.
	go func() {
		_, err := h.manager.CloseAccount(
			context.Background(), account.TraderKey.PubKey, outputs,
			bestHeight,
		)
		if err != nil {
			h.t.Logf("unable to close account: %v", err)
		}
	}()

	// This should prompt the account's closing transaction to be broadcast
	// and its state transitioned to StatePendingClosed.
	var closeTx *wire.MsgTx
	select {
	case closeTx = <-h.wallet.publishChan:
	case <-time.After(timeout):
		h.t.Fatal("expected close transaction to be broadcast")
	}

	checkCloseTx(h.t, closeTx, account)

	account.State = StatePendingClosed
	account.CloseTx = closeTx
	h.assertAccountExists(account)

	// Notify the transaction as a spend of the account.
	h.notifier.spendChan <- &chainntnfs.SpendDetail{SpendingTx: closeTx}

	// This should prompt the account to now be in a StateClosed state.
	account.State = StateClosed
	h.assertAccountExists(account)

	return closeTx
}

func checkCloseTx(t *testing.T, closeTx *wire.MsgTx, account *Account) {
	t.Helper()

	// The closing transaction should only include one output, which should
	// be a P2WPKH output of the account's trader key.
	if len(closeTx.TxOut) != 1 {
		t.Fatalf("expected 1 output in close transaction, found %d",
			len(closeTx.TxOut))
	}

	_, addrs, _, err := txscript.ExtractPkScriptAddrs(
		closeTx.TxOut[0].PkScript, &chaincfg.MainNetParams,
	)
	if err != nil {
		t.Fatalf("unable to extract address: %v", err)
	}
	if len(addrs) != 1 {
		t.Fatalf("expected 1 address, found %d", len(addrs))
	}
	addr, ok := addrs[0].(*btcutil.AddressWitnessPubKeyHash)
	if !ok {
		t.Fatalf("expected P2WPKH address, found %T", addr)
	}

	witnessProgram := btcutil.Hash160(
		account.TraderKey.PubKey.SerializeCompressed(),
	)
	if !bytes.Equal(addr.WitnessProgram(), witnessProgram) {
		t.Fatalf("expected witness program %x, got %x", witnessProgram,
			addr.WitnessProgram())
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
// and close it throughout the happy flow.
func TestNewAccountHappyFlow(t *testing.T) {
	t.Parallel()

	const bestHeight = 100

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	account := h.openAccount(
		maxAccountValue, bestHeight+maxAccountExpiry, bestHeight,
	)

	h.closeAccount(account, nil, bestHeight+1)
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
		Value:         value,
		Expiry:        expiry,
		TraderKey:     testTraderKeyDesc,
		AuctioneerKey: testAuctioneerKey,
		BatchKey:      testBatchKey,
		Secret:        sharedSecret,
		HeightHint:    bestHeight,
		State:         StateInitiated,
	}
	h.assertAccountExists(account)

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
	// resumed and their transaction rebroadcast/recreated. In our case, we
	// should have an account transaction in our TxSource, so a new one
	// shouldn't be created.
	h.restartManager()

	select {
	case accountTx := <-h.wallet.publishChan:
		if accountTx.TxHash() != tx.TxHash() {
			t.Fatalf("transaction mismatch after restart: "+
				"%v vs %v", accountTx.TxHash(),
				tx.TxHash())
		}

	case <-time.After(timeout):
		h.t.Fatal("expected account transaction to be " +
			"rebroadcast")
	}

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
	h.assertAccountExists(account)

	// Notify the confirmation of the account.
	h.notifier.confChan <- &chainntnfs.TxConfirmation{}

	// This should prompt the account to now be in a Confirmed state.
	account.State = StateOpen
	h.assertAccountExists(account)
}

// TestAccountExpiration ensures that we properly detect when an account expires
// on-chain. As a result, the account should be marked as StateExpired in the
// database.
func TestAccountExpiration(t *testing.T) {
	t.Parallel()

	const bestHeight = 100

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	account := h.openAccount(
		maxAccountValue, bestHeight+maxAccountExpiry, bestHeight,
	)

	h.expireAccount(account)
}

// TestAccountSpendBatchNotFinalized ensures that if a pending batch exists at
// the time of an account spend, then its updates are applied to the account in
// order to properly locate the latest account output.
func TestAccountSpendBatchNotFinalized(t *testing.T) {
	t.Parallel()

	const bestHeight = 100

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	account := h.openAccount(
		maxAccountValue, bestHeight+maxAccountExpiry, bestHeight,
	)

	// Create an account spend which we'll notify later. This spend should
	// take the multi-sig path to trigger the pending batch logic.
	const newValue = maxAccountValue / 2
	newPkScript, err := account.NextOutputScript()
	if err != nil {
		t.Fatalf("unable to generate next output script: %v", err)
	}
	spendTx := &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: account.OutPoint,
			Witness: wire.TxWitness{
				{0x01}, // Use multi-sig path.
				{},
				{},
			},
		}},
		TxOut: []*wire.TxOut{{
			Value:    int64(newValue),
			PkScript: newPkScript,
		}},
	}

	// Then, we'll simulate a pending batch by staging some account updates
	// that should be applied once the spend arrives.
	mods := []Modifier{
		ValueModifier(newValue),
		StateModifier(StatePendingUpdate),
		OutPointModifier(wire.OutPoint{Hash: spendTx.TxHash(), Index: 0}),
		IncrementBatchKey(),
	}
	h.store.setPendingBatch(func() error {
		return h.store.updateAccount(account, mods...)
	})

	// Notify the spend.
	h.notifier.spendChan <- &chainntnfs.SpendDetail{
		SpendingTx: spendTx,
	}

	// Assert that the account updates have been applied. Note that it may
	// seem like our account pointer hasn't had the updates applied, but the
	// updateAccount call above does so implicitly.
	h.assertAccountExists(account)
}
