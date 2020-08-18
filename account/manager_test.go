package account

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	timeout = 500 * time.Millisecond

	maxAccountValue = 2 * btcutil.SatoshiPerBitcoin
)

var (
	p2wsh, _   = hex.DecodeString("00208c2865c87ffd33fc5d698c7df9cf2d0fb39d93103c637a06dea32c848ebc3e1d")
	p2wpkh, _  = hex.DecodeString("0014ccdeffed4f9c91d5bf45c34e4b8f03a5025ec062")
	np2wpkh, _ = hex.DecodeString("a91458c11505b54582ab04e96d36908f85a8b689459787")
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
			Store:          store,
			Auctioneer:     auctioneer,
			Wallet:         wallet,
			Signer:         wallet,
			ChainNotifier:  notifier,
			TxSource:       wallet,
			TxFeeEstimator: wallet,
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
			// Nil the public key curves before spew to prevent
			// extraneous output.
			found.TraderKey.PubKey.Curve = nil
			expected.TraderKey.PubKey.Curve = nil
			found.AuctioneerKey.Curve = nil
			expected.AuctioneerKey.Curve = nil
			found.BatchKey.Curve = nil
			expected.BatchKey.Curve = nil

			return fmt.Errorf("expected account: %v\ngot: %v",
				spew.Sdump(expected), spew.Sdump(found))
		}

		return nil
	}, 10*timeout)
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *testHarness) openAccount(value btcutil.Amount, expiry uint32, // nolint:unparam
	bestHeight uint32) *Account { // nolint:unparam

	h.t.Helper()

	// Create a new account. Its initial state should be StatePendingOpen.
	ctx := context.Background()
	account, err := h.manager.InitAccount(
		ctx, value, expiry, bestHeight, DefaultFundingConfTarget,
	)
	if err != nil {
		h.t.Fatalf("unable to create new account: %v", err)
	}

	// The same account should be found in the store.
	h.assertAccountExists(account)

	// Since the account is still pending confirmation, it should not be
	// subscribed to updates from the auctioneer yet.
	h.assertAccountNotSubscribed(account.TraderKey.PubKey)

	// Notify the confirmation of the account.
	confHeight := bestHeight + 6
	h.notifier.confChan <- &chainntnfs.TxConfirmation{
		BlockHeight: confHeight,
	}

	// This should prompt the account to now be in a StateOpen state and
	// the subscription for updates should now be realized.
	account.State = StateOpen
	account.HeightHint = confHeight
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
	closeTx := h.assertSpendTxBroadcast(account, nil, nil, nil)

	account.State = StatePendingClosed
	account.HeightHint = bestHeight
	account.CloseTx = closeTx
	h.assertAccountExists(account)

	// Notify the transaction as a spend of the account.
	spendHeight := bestHeight + 6
	h.notifier.spendChan <- &chainntnfs.SpendDetail{
		SpendingTx:     closeTx,
		SpendingHeight: int32(spendHeight),
	}

	// This should prompt the account to now be in a StateClosed state.
	account.State = StateClosed
	account.HeightHint = spendHeight
	h.assertAccountExists(account)

	return closeTx
}

func (h *testHarness) assertSpendTxBroadcast(accountBeforeSpend *Account,
	externalInputs []*lnwallet.Utxo, externalOutputs []*wire.TxOut,
	newValue *btcutil.Amount) *wire.MsgTx {

	h.t.Helper()

	var spendTx *wire.MsgTx
	select {
	case spendTx = <-h.wallet.publishChan:
	case <-time.After(timeout):
		h.t.Fatal("expected spend transaction to be broadcast")
	}

	// The spending transaction should spend the account.
	foundAccountInput := false
	for _, txIn := range spendTx.TxIn {
		if txIn.PreviousOutPoint == accountBeforeSpend.OutPoint {
			foundAccountInput = true
		}
	}
	if !foundAccountInput {
		h.t.Fatalf("did not find account input %v in spend transaction",
			accountBeforeSpend.OutPoint)
	}

	// If no outputs were provided and the account wasn't re-created, we
	// should expect to see a single wallet output.
	if len(externalOutputs) == 0 && newValue == nil {
		if len(spendTx.TxOut) != 1 {
			h.t.Fatalf("expected 1 output in spend transaction, "+
				"found %d", len(spendTx.TxOut))
		}
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			spendTx.TxOut[0].PkScript, &chaincfg.MainNetParams,
		)
		if err != nil {
			h.t.Fatalf("unable to extract address: %v", err)
		}
		if len(addrs) != 1 {
			h.t.Fatalf("expected 1 address, found %d", len(addrs))
		}
		addr, ok := addrs[0].(*btcutil.AddressWitnessPubKeyHash)
		if !ok {
			h.t.Fatalf("expected P2WPKH address, found %T", addr)
		}
		// Witness program of address returned by the mock
		// implementation of NextAddr.
		witnessProgram := btcutil.Hash160(testRawTraderKey)
		if !bytes.Equal(addr.WitnessProgram(), witnessProgram) {
			h.t.Fatalf("expected witness program %x, got %x",
				witnessProgram, addr.WitnessProgram())
		}

		return spendTx
	}

	// Otherwise, the spending transaction should include the expected
	// inputs and outputs. If it recreates the account output, we should
	// also attempt to locate it.
	if len(spendTx.TxIn) != len(externalInputs)+1 {
		h.t.Fatalf("expected %d input(s) in spend transaction, found %d",
			len(externalInputs)+1, len(spendTx.TxIn))
	}
	nextPkScript, err := accountBeforeSpend.NextOutputScript()
	if err != nil {
		h.t.Fatalf("unable to generate next output script: %v",
			err)
	}
	outputs := append(externalOutputs, &wire.TxOut{
		Value:    int64(*newValue),
		PkScript: nextPkScript,
	})
	if len(spendTx.TxOut) != len(outputs) {
		h.t.Fatalf("expected %d output(s) in spend transaction, found %d",
			len(outputs), len(spendTx.TxOut))
	}

	// The input and output indices may not match due to BIP-69 sorting.
nextInput:
	for _, input := range externalInputs {
		for _, txIn := range spendTx.TxIn {
			if txIn.PreviousOutPoint != input.OutPoint {
				continue
			}
			continue nextInput
		}
		h.t.Fatalf("expected input %v in spend transaction",
			input.OutPoint)
	}
nextOutput:
	for _, output := range outputs {
		for _, txOut := range spendTx.TxOut {
			if !bytes.Equal(txOut.PkScript, output.PkScript) {
				continue
			}
			if txOut.Value != output.Value {
				h.t.Fatalf("expected value %v for output %x, "+
					"got %v", output.Value, output.PkScript,
					txOut.Value)
			}
			continue nextOutput
		}
		h.t.Fatalf("expected output script %x in spend transaction",
			output.PkScript)
	}

	return spendTx
}

// assertAuctioneerReceived asserts that auctioneer has received the correct
// information regarding an account modification.
func (h *testHarness) assertAuctioneerReceived(inputs []*lnwallet.Utxo,
	outputs []*wire.TxOut) {

	h.t.Helper()

	h.auctioneer.mu.Lock()
	defer h.auctioneer.mu.Unlock()

	if len(inputs) != len(h.auctioneer.inputsReceived) {
		h.t.Fatal("auctioneer input count mismatch")
	}
	for _, input := range inputs {
		found := false
		for _, inputReceived := range h.auctioneer.inputsReceived {
			if input.OutPoint == inputReceived.PreviousOutPoint {
				found = true
				break
			}
		}
		if !found {
			h.t.Fatalf("expected auctioneer to receive input %v",
				input.OutPoint)
		}
	}

	if len(outputs) != len(h.auctioneer.outputsReceived) {
		h.t.Fatal("auctioneer output count mismatch")
	}
	for _, output := range outputs {
		found := false
		for _, outputReceived := range h.auctioneer.outputsReceived {
			if reflect.DeepEqual(*output, outputReceived) {
				found = true
				break
			}
		}
		if !found {
			h.t.Fatalf("expected auctioneer to receive output %x",
				output.PkScript)
		}
	}

	h.auctioneer.inputsReceived = nil
	h.auctioneer.outputsReceived = nil
}

// assertAccountModification provides several assertions for an account
// modification to determine whether it was successful. The assertions ensure
// that the account is transitioned from its StatePendingUpdate state to
// StateOpen.
func (h *testHarness) assertAccountModification(account *Account,
	inputs []*lnwallet.Utxo, outputs []*wire.TxOut,
	newAccountValue btcutil.Amount, accountInputIdx,
	accountOutputIdx uint32, broadcastHeight uint32) {

	h.t.Helper()

	// We'll start by ensuring that the auctioneer received the intended
	// modification parameters.
	h.assertAuctioneerReceived(inputs, outputs)

	// A proper spend transaction should have been broadcast that contains
	// the expected inputs and outputs from above, and the recreated account
	// output.
	spendTx := h.assertSpendTxBroadcast(
		account, inputs, outputs, &newAccountValue,
	)

	// The account should be found within the store with the following
	// modifiers.
	mods := []Modifier{
		ValueModifier(newAccountValue),
		StateModifier(StatePendingUpdate),
		OutPointModifier(wire.OutPoint{
			Hash:  spendTx.TxHash(),
			Index: accountOutputIdx,
		}),
		HeightHintModifier(broadcastHeight),
		IncrementBatchKey(),
	}
	for _, mod := range mods {
		mod(account)
	}
	h.assertAccountExists(account)

	// Notify the transaction as a spend of the account. The account should
	// remain in StatePendingUpdate until it reaches the appropriate number
	// of confirmations.
	h.notifier.spendChan <- &chainntnfs.SpendDetail{
		SpendingTx:        spendTx,
		SpenderInputIndex: accountInputIdx,
	}
	h.assertAccountExists(account)

	// Notify the confirmation, causing the account to transition back to
	// StateOpen.
	confHeight := broadcastHeight + 6
	h.notifier.confChan <- &chainntnfs.TxConfirmation{
		Tx:          spendTx,
		BlockHeight: confHeight,
	}
	StateModifier(StateOpen)(account)
	HeightHintModifier(confHeight)(account)
	h.assertAccountExists(account)
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
			DefaultFundingConfTarget,
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
	confHeight := uint32(bestHeight + 6)
	h.notifier.confChan <- &chainntnfs.TxConfirmation{
		BlockHeight: confHeight,
	}

	// This should prompt the account to now be in a Confirmed state.
	account.State = StateOpen
	account.HeightHint = confHeight
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

// TestAccountWithdrawal ensures that we can process an account withdrawal
// through the happy flow.
func TestAccountWithdrawal(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	const bestHeight = 100
	account := h.openAccount(
		maxAccountValue, bestHeight+maxAccountExpiry, bestHeight,
	)

	// With our account created, we'll start our withdrawal by creating the
	// outputs we'll withdraw our funds to. We'll create three outputs, one
	// of each supported output type. Each output will have 1/4 of the
	// account's value.
	valuePerOutput := account.Value / 4
	outputs := []*wire.TxOut{
		{
			Value:    int64(valuePerOutput),
			PkScript: p2wsh,
		},
		{
			Value:    int64(valuePerOutput),
			PkScript: p2wpkh,
		},
		{
			Value:    int64(valuePerOutput),
			PkScript: np2wpkh,
		},
	}

	// We'll use the lowest fee rate possible, which should yield a
	// transaction fee of 260 satoshis when taking into account the outputs
	// we'll be withdrawing to.
	const feeRate = chainfee.FeePerKwFloor
	const expectedFee btcutil.Amount = 260

	// Attempt the withdrawal.
	//
	// If successful, we'll follow with a series of assertions to ensure it
	// was performed correctly.
	_, _, err := h.manager.WithdrawAccount(
		context.Background(), account.TraderKey.PubKey, outputs,
		feeRate, bestHeight,
	)
	if err != nil {
		t.Fatalf("unable to process account withdrawal: %v", err)
	}

	// The value of the account after the withdrawal depends on the
	// transaction fee and the amount of each output withdrawn to.
	withdrawOutputSum := valuePerOutput * btcutil.Amount(len(outputs))
	valueAfterWithdrawal := account.Value - withdrawOutputSum - expectedFee
	h.assertAccountModification(
		account, nil, outputs, valueAfterWithdrawal, 0, 0, bestHeight,
	)

	// Finally, close the account to ensure we can process another spend
	// after the withdrawal.
	_ = h.closeAccount(account, nil, bestHeight)
}

// TestAccountDeposit ensures that we can process an account deposit
// through the happy flow.
func TestAccountDeposit(t *testing.T) {
	t.Parallel()

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	// We'll start by defining our initial account value and its value after
	// a successful deposit.
	const initialAccountValue = MinAccountValue
	const valueAfterDeposit = initialAccountValue * 2
	const depositAmount = valueAfterDeposit - initialAccountValue

	const bestHeight = 100
	account := h.openAccount(
		initialAccountValue, bestHeight+maxAccountExpiry, bestHeight,
	)

	// We'll provide two outputs to the mock wallet that will be consumed by
	// the deposit and should result in a change output.
	h.wallet.utxos = []*lnwallet.Utxo{
		{
			AddressType: lnwallet.NestedWitnessPubKey,
			Value:       depositAmount,
			OutPoint:    wire.OutPoint{Index: 1},
			PkScript:    np2wpkh,
		},
		{
			AddressType: lnwallet.WitnessPubKey,
			Value:       depositAmount,
			// Use an outpoint greater than the current account to
			// test the filtering of inputs provided to the
			// auctioneer.
			OutPoint: wire.OutPoint{
				Hash:  account.OutPoint.Hash,
				Index: account.OutPoint.Index + 1,
			},
			PkScript: p2wpkh,
		},
	}

	// We'll use the lowest fee rate possible, which should yield a
	// transaction fee of 346 satoshis when taking into account the
	// additional inputs needed to satisfy the deposit.
	const feeRate = chainfee.FeePerKwFloor
	const expectedFee btcutil.Amount = 346

	// Attempt the deposit.
	//
	// If successful, we'll follow with a series of assertions to ensure it
	// was performed correctly.
	_, _, err := h.manager.DepositAccount(
		context.Background(), account.TraderKey.PubKey, depositAmount,
		feeRate, bestHeight,
	)
	if err != nil {
		t.Fatalf("unable to process account deposit: %v", err)
	}

	// We should expect to see a change output with the current UTXOs
	// available, so we'll reconstruct what we expect to see in the deposit
	// transaction.
	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		btcutil.Hash160(testRawTraderKey), &chaincfg.MainNetParams,
	)
	if err != nil {
		t.Fatalf("unable to create address: %v", err)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("unable to create address script: %v", err)
	}
	changeOutput := &wire.TxOut{
		Value:    int64(depositAmount - expectedFee),
		PkScript: pkScript,
	}

	// The transaction should have find the expected account input and
	// output at the following indices.
	const accountInputIdx = 1
	const accountOutputIdx = 1

	h.assertAccountModification(
		account, h.wallet.utxos, []*wire.TxOut{changeOutput},
		valueAfterDeposit, accountInputIdx, accountOutputIdx,
		bestHeight,
	)

	// Finally, close the account to ensure we can process another spend
	// after the withdrawal.
	_ = h.closeAccount(account, nil, bestHeight)
}

// TestAccountConsecutiveBatches ensures that we can process an account update
// through multiple consecutive batches that only confirm after we've already
// updated our database state.
func TestAccountConsecutiveBatches(t *testing.T) {
	t.Parallel()

	const bestHeight = 100

	h := newTestHarness(t)
	h.start()
	defer h.stop()

	account := h.openAccount(
		maxAccountValue, bestHeight+maxAccountExpiry, bestHeight,
	)

	// Then, we'll simulate the maximum number of unconfirmed batches to
	// happen that'll all confirm in the same block.
	const newValue = maxAccountValue / 2
	const numBatches = 10
	batchTxs := make([]*wire.MsgTx, numBatches)
	for i := 0; i < numBatches; i++ {
		newPkScript, err := account.NextOutputScript()
		require.NoError(t, err)

		// Create an account spend which we'll notify later. This spend
		// should take the multi-sig path to trigger the logic to lookup
		// previous outpoints.
		batchTx := &wire.MsgTx{
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
		batchTxs[i] = batchTx

		mods := []Modifier{
			ValueModifier(newValue - btcutil.Amount(i)),
			StateModifier(StatePendingBatch),
			OutPointModifier(wire.OutPoint{
				Hash:  batchTx.TxHash(),
				Index: 0,
			}),
			IncrementBatchKey(),
		}
		err = h.store.updateAccount(account, mods...)
		require.NoError(t, err)

		// The RPC server will notify the manager each time a batch is
		// finalized, we do the same here.
		err = h.manager.WatchMatchedAccounts(
			context.Background(),
			[]*btcec.PublicKey{account.TraderKey.PubKey},
		)
		require.NoError(t, err)
	}

	// Notify the confirmation, causing the account to transition back to
	// StateOpen.
	confHeight := bestHeight + 6
	h.notifier.confChan <- &chainntnfs.TxConfirmation{
		Tx:          batchTxs[len(batchTxs)-1],
		BlockHeight: uint32(confHeight),
	}
	StateModifier(StateOpen)(account)
	HeightHintModifier(uint32(confHeight))(account)
	h.assertAccountExists(account)
}
