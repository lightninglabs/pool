package account

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	timeout = 500 * time.Millisecond

	maxAccountValue = 2 * btcutil.SatoshiPerBitcoin

	bestHeight = 100
)

var (
	p2wsh, _   = hex.DecodeString("00208c2865c87ffd33fc5d698c7df9cf2d0fb39d93103c637a06dea32c848ebc3e1d")
	p2tr, _    = hex.DecodeString("5120bb91443dd777945ef4422cffddec00d5feed2aca2562a902fbe8d2a258b337da")
	p2wpkh, _  = hex.DecodeString("0014ccdeffed4f9c91d5bf45c34e4b8f03a5025ec062")
	np2wpkh, _ = hex.DecodeString("a91458c11505b54582ab04e96d36908f85a8b689459787")

	// defaultFeeExpr is a fee expression noting that the minimum fee rate
	// should be used for a wallet output.
	defaultFeeExpr = OutputWithFee{
		FeeRate: chainfee.FeePerKwFloor,
	}
)

type testCase struct {
	name        string
	feeExpr     FeeExpr
	fee         btcutil.Amount
	version     Version
	newVersion  Version
	expectedErr string

	// The following fields are only used by deposit tests.
	fundedOutputAmount btcutil.Amount
	fundingTxFee       btcutil.Amount
	utxo               *lnwallet.Utxo
}

func runSubTests(t *testing.T, testCases []*testCase,
	runTest func(t *testing.T, h *testHarness, tc *testCase)) {

	for _, tc := range testCases {
		tc := tc
		success := t.Run(tc.name, func(tt *testing.T) {
			tt.Parallel()

			h := newTestHarness(tt)
			h.start()
			defer h.stop()

			runTest(tt, h, tc)
		})
		if !success {
			return
		}
	}
}

type testHarness struct {
	t          *testing.T
	store      *mockStore
	notifier   *mockChainNotifier
	wallet     *mockWallet
	auctioneer *mockAuctioneer
	manager    Manager
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
			ChainParams:    &chaincfg.TestNet3Params,
			LndVersion: &verrpc.Version{
				AppMajor: 0,
				AppMinor: 15,
				AppPatch: 1,
			},
		}),
	}
}

func (h *testHarness) start() {
	require.NoError(h.t, h.manager.Start())
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

	_, ok := h.auctioneer.subscribed[rawTraderKey]
	require.Truef(h.t, ok, "account %x not subscribed", rawTraderKey)
}

func (h *testHarness) assertAccountNotSubscribed(traderKey *btcec.PublicKey) {
	h.t.Helper()

	var rawTraderKey [33]byte
	copy(rawTraderKey[:], traderKey.SerializeCompressed())

	h.auctioneer.mu.Lock()
	defer h.auctioneer.mu.Unlock()

	_, ok := h.auctioneer.subscribed[rawTraderKey]
	require.Falsef(h.t, ok, "account %x not subscribed", rawTraderKey)
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
	require.NoError(h.t, err)
}

func (h *testHarness) openAccount(value btcutil.Amount, expiry uint32, // nolint:unparam
	bestHeight uint32, version Version) *Account { // nolint:unparam

	h.t.Helper()

	// Create a new account. Its initial state should be StatePendingOpen.
	ctx := context.Background()
	account, err := h.manager.InitAccount(
		ctx, value, version, chainfee.FeePerKwFloor, expiry, bestHeight,
	)
	require.NoError(h.t, err)

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

func (h *testHarness) closeAccount(account *Account, feeExpr FeeExpr,
	bestHeight uint32) *wire.MsgTx {

	h.t.Helper()

	// Close the account with the auctioneer.
	go func() {
		_, err := h.manager.CloseAccount(
			context.Background(), account.TraderKey.PubKey, feeExpr,
			bestHeight,
		)
		require.NoError(h.t, err)
	}()

	// This should prompt the account's closing transaction to be broadcast
	// and its state transitioned to StatePendingClosed.
	closeTx := h.assertSpendTxBroadcast(
		account, nil, nil, nil, account.Version,
	)

	account.Value = 0
	account.State = StatePendingClosed
	account.HeightHint = bestHeight
	account.LatestTx = closeTx
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
	newValue *btcutil.Amount, newVersion Version) *wire.MsgTx {

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
	require.True(h.t, foundAccountInput, "account input in transaction")

	// If no outputs were provided and the account wasn't re-created, we
	// should expect to see a single wallet output.
	if len(externalOutputs) == 0 && newValue == nil {
		require.Len(h.t, spendTx.TxOut, 1)
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			spendTx.TxOut[0].PkScript, &chaincfg.MainNetParams,
		)
		require.NoError(h.t, err)
		require.Len(h.t, addrs, 1)
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
	require.Len(h.t, spendTx.TxIn, len(externalInputs)+1)

	// Perhaps this is an in-flight account upgrade?
	upgradedAccount := accountBeforeSpend.Copy(VersionModifier(newVersion))
	nextPkScript, err := upgradedAccount.NextOutputScript()
	require.NoError(h.t, err)
	outputs := externalOutputs
	if newValue != nil {
		outputs = append(outputs, &wire.TxOut{
			Value:    int64(*newValue),
			PkScript: nextPkScript,
		})
	}
	require.Len(h.t, spendTx.TxOut, len(outputs))

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

	require.Len(h.t, inputs, len(h.auctioneer.inputsReceived))
	for _, inp := range inputs {
		found := false
		for _, inputReceived := range h.auctioneer.inputsReceived {
			if inp.OutPoint == inputReceived.PreviousOutPoint {
				found = true
				break
			}
		}
		require.Truef(h.t, found, "input %v missing", inp.OutPoint)
	}
	for _, inp := range inputs {
		found := false
		for _, outReceived := range h.auctioneer.prevOutputsReceived {
			if bytes.Equal(inp.PkScript, outReceived.PkScript) &&
				int64(inp.Value) == outReceived.Value {

				found = true
				break
			}
		}
		require.Truef(
			h.t, found, "input %v not in previous outputs",
			inp.OutPoint,
		)
	}

	require.Len(h.t, outputs, len(h.auctioneer.outputsReceived))
	for _, output := range outputs {
		found := false
		for _, outputReceived := range h.auctioneer.outputsReceived {
			if reflect.DeepEqual(*output, outputReceived) {
				found = true
				break
			}
		}
		require.Truef(h.t, found, "output %x missing", output.PkScript)
	}

	h.auctioneer.inputsReceived = nil
	h.auctioneer.outputsReceived = nil
	h.auctioneer.prevOutputsReceived = nil
}

// assertAuctioneerMuSig2NoncesReceived makes sure that the nonces we created for
// our local MuSig2 session were sent to the auctioneer.
func (h *testHarness) assertAuctioneerMuSig2NoncesReceived() {
	h.t.Helper()

	h.auctioneer.mu.Lock()
	defer h.auctioneer.mu.Unlock()

	h.wallet.Lock()
	defer h.wallet.Unlock()

	require.Len(h.t, h.wallet.muSig2Sessions, 0)
	require.Len(h.t, h.wallet.muSig2RemovedSessions, 1)

	var sessionInfo *input.MuSig2SessionInfo
	for _, info := range h.wallet.muSig2RemovedSessions {
		sessionInfo = info
		break
	}
	require.NotNil(h.t, sessionInfo)

	require.Equal(
		h.t, sessionInfo.PublicNonce[:], h.auctioneer.noncesReceived,
	)
}

// assertAccountModification provides several assertions for an account
// modification to determine whether it was successful. The assertions ensure
// that the account is transitioned from its StatePendingUpdate state to
// StateOpen.
func (h *testHarness) assertAccountModification(account *Account,
	inputs []*lnwallet.Utxo, outputs []*wire.TxOut,
	newAccountValue btcutil.Amount, newAccountVersion Version,
	accountInputIdx, accountOutputIdx uint32, broadcastHeight uint32) {

	h.t.Helper()

	// We'll start by ensuring that the auctioneer received the intended
	// modification parameters.
	h.assertAuctioneerReceived(inputs, outputs)

	// Make sure a MuSig2 signing session was created for a Taproot/MuSig2
	// spend.
	if account.Version >= VersionTaprootEnabled {
		h.assertAuctioneerMuSig2NoncesReceived()
	}

	// A proper spend transaction should have been broadcast that contains
	// the expected inputs and outputs from above, and the recreated account
	// output.
	spendTx := h.assertSpendTxBroadcast(
		account, inputs, outputs, &newAccountValue, newAccountVersion,
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
		LatestTxModifier(spendTx),
		VersionModifier(newAccountVersion),
	}
	for _, mod := range mods {
		mod(account)
	}
	h.assertAccountExists(account)

	// Notify the transaction as a spend of the account. The account should
	// remain in StatePendingUpdate until it reaches the appropriate number
	// of confirmations.
	select {
	case h.notifier.spendChan <- &chainntnfs.SpendDetail{
		SpendingTx:        spendTx,
		SpenderInputIndex: accountInputIdx,
	}:

	case <-time.After(timeout):
		h.t.Fatalf("couldn't send tx spend notification")
	}

	h.assertAccountExists(account)

	// Notify the confirmation, causing the account to transition back to
	// StateOpen.
	confHeight := broadcastHeight + 6

	select {
	case h.notifier.confChan <- &chainntnfs.TxConfirmation{
		Tx:          spendTx,
		BlockHeight: confHeight,
	}:

	case <-time.After(timeout):
		h.t.Fatalf("couldn't send tx confirmation")
	}

	StateModifier(StateOpen)(account)
	HeightHintModifier(confHeight)(account)
	h.assertAccountExists(account)
}

func (h *testHarness) restartManager() {
	h.t.Helper()

	h.manager.Stop()

	mgr, ok := h.manager.(*manager)
	require.True(h.t, ok, "invalid manager")

	auctioneer := newMockAuctioneer()
	h.auctioneer = auctioneer
	h.manager = NewManager(&ManagerConfig{
		Store:         mgr.cfg.Store,
		Auctioneer:    auctioneer,
		Wallet:        mgr.cfg.Wallet,
		Signer:        mgr.cfg.Signer,
		ChainNotifier: mgr.cfg.ChainNotifier,
		TxSource:      mgr.cfg.TxSource,
		ChainParams:   mgr.cfg.ChainParams,
		LndVersion:    mgr.cfg.LndVersion,
	})

	require.NoError(h.t, h.manager.Start())
}

// TestNewAccountHappyFlow ensures that we are able to create a new account
// and close it throughout the happy flow.
func TestNewAccountHappyFlow(t *testing.T) {
	t.Parallel()

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,
		},
		{
			name:    "happy path version 1",
			version: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			maxAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		expr := defaultFeeExpr
		h.closeAccount(account, &expr, bestHeight+1)
	})
}

// TestResumeAccountAfterRestart ensures we're able to properly create a new
// account even if we've shut down during the process.
func TestResumeAccountAfterRestart(t *testing.T) {
	t.Parallel()

	const (
		value  = maxAccountValue
		expiry = maxAccountExpiry
	)

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,
		},
		{
			name:    "happy path version 1",
			version: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		// We'll create an interceptor for SendOutputs to simulate a
		// transaction being crafted and persisted for the account, but
		// it not being returned to the account manager because of a
		// crash, etc.
		sendOutputsChan := make(chan *wire.MsgTx)
		sendOutputsInterceptor := func(_ context.Context,
			outputs []*wire.TxOut,
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

		// We'll then proceed to create a new account. We expect this to
		// fail given the interceptor above, but that's fine. We should
		// still have a persisted intent to create the account.
		go func() {
			_, _ = h.manager.InitAccount(
				context.Background(), value, tc.version,
				chainfee.FeePerKwFloor, expiry, bestHeight,
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
			Version:       tc.version,
		}
		h.assertAccountExists(account)

		// Then, we'll create a new interceptor to send us a signal if
		// SendOutputs is invoked again. This shouldn't happen as the
		// transaction originally crafted the first time should be in our
		// TxSource, so we should pick it from there.
		sendOutputsSignal := make(chan struct{}, 1)
		sendOutputsInterceptor = func(_ context.Context,
			outputs []*wire.TxOut,
			_ chainfee.SatPerKWeight) (*wire.MsgTx, error) {

			close(sendOutputsSignal)
			return nil, errors.New("error")
		}
		h.wallet.interceptSendOutputs(sendOutputsInterceptor)

		// Restart the manager. This should cause any pending accounts
		// to be resumed and their transaction rebroadcast/recreated. In
		// our case, we should have an account transaction in our
		// TxSource, so a new one shouldn't be created.
		h.restartManager()

		select {
		case accountTx := <-h.wallet.publishChan:
			require.Equal(t, accountTx.TxHash(), tx.TxHash())

		case <-time.After(timeout):
			h.t.Fatal("expected account transaction to be " +
				"rebroadcast")
		}

		select {
		case <-sendOutputsSignal:
			t.Fatal("unexpected call to SendOutputs")
		case <-time.After(2 * timeout):
		}

		// With the account resumed, it should now be in a
		// StatePendingOpen state.
		account.State = StatePendingOpen
		account.LatestTx = tx
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

		// This should prompt the account to now be in a Confirmed
		// state.
		account.State = StateOpen
		account.HeightHint = confHeight
		h.assertAccountExists(account)
	})
}

// TestAccountCloseFundsDestination ensures the different possible destinations
// for the funds of an account being closed work as intended.
func TestAccountClose(t *testing.T) {
	t.Parallel()

	const highFeeRate = 10_000 * chainfee.FeePerKwFloor

	cases := []*testCase{
		{
			name: "below P2WKH dust limit",
			feeExpr: &OutputWithFee{
				PkScript: p2wpkh,
				FeeRate:  highFeeRate,
			},
			expectedErr: "with 2530000 sat/kw results in dust",
		},
		{
			name: "below P2SH dust limit",
			feeExpr: &OutputWithFee{
				PkScript: np2wpkh,
				FeeRate:  highFeeRate,
			},
			expectedErr: "with 2530000 sat/kw results in dust",
		},
		{
			name: "below P2WSH dust limit",
			feeExpr: &OutputWithFee{
				PkScript: p2wsh,
				FeeRate:  highFeeRate,
			},
			expectedErr: "with 2530000 sat/kw results in dust",
		},
		{
			name: "single wallet P2WKH output",
			feeExpr: &OutputWithFee{
				PkScript: nil,
				FeeRate:  chainfee.FeePerKwFloor,
			},
			fee: btcutil.Amount(141),
		},
		{
			name: "single external P2WKH output",
			feeExpr: &OutputWithFee{
				PkScript: p2wpkh,
				FeeRate:  2 * chainfee.FeePerKwFloor,
			},
			fee: btcutil.Amount(282),
		},
		{
			name: "multiple external outputs",
			feeExpr: &OutputsWithImplicitFee{
				{
					PkScript: p2wpkh,
					Value:    10_000,
				},
				{
					PkScript: np2wpkh,
					Value:    10_000,
				},
				{
					PkScript: p2wsh,
					Value:    10_000,
				},
			},
			fee: btcutil.Amount(70_000),
		},
		{
			name: "taproot single wallet P2WKH output",
			feeExpr: &OutputWithFee{
				PkScript: nil,
				FeeRate:  chainfee.FeePerKwFloor,
			},
			fee:     btcutil.Amount(100),
			version: VersionTaprootEnabled,
		},
		{
			name: "taproot single wallet P2TR output",
			feeExpr: &OutputWithFee{
				PkScript: p2tr,
				FeeRate:  chainfee.FeePerKwFloor,
			},
			fee:     btcutil.Amount(112),
			version: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		// We'll start by creating a new account of the minimum value
		// for each test.
		account := h.openAccount(
			MinAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		// We'll immediately attempt to close the account with the
		// test's fee expression.
		_, err := h.manager.CloseAccount(
			context.Background(), account.TraderKey.PubKey,
			tc.feeExpr, bestHeight,
		)

		// If the test's fee expression is not valid, we should expect
		// to see an error.
		if tc.expectedErr != "" {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)

			return
		}

		// Otherwise, we'll proceed to check whether our fee expression
		// was honored.
		require.NoError(t, err)

		// Any external outputs (not sourced from the backing lnd node)
		// should be found in the closing transaction.
		var externalOutputs []*wire.TxOut
		switch feeExpr := tc.feeExpr.(type) {
		case *OutputWithFee:
			if feeExpr.PkScript != nil {
				output := &wire.TxOut{
					PkScript: feeExpr.PkScript,
					Value: int64(
						account.Value - tc.fee,
					),
				}
				externalOutputs = append(
					externalOutputs, output,
				)
			}

		case *OutputsWithImplicitFee:
			externalOutputs = feeExpr.Outputs()

		default:
			t.Fatal("unhandled fee expr")
		}

		// The account's closing transaction should be broadcast.
		closeTx := h.assertSpendTxBroadcast(
			account, nil, externalOutputs, nil, tc.version,
		)

		// Finally, compute the resulting fee of the transaction and
		// ensure it matches what we expect.
		var outputTotal btcutil.Amount
		for _, output := range closeTx.TxOut {
			outputTotal += btcutil.Amount(output.Value)
		}
		fee := account.Value - outputTotal
		require.Equal(t, fee, tc.fee)
	})
}

// TestAccountExpiration ensures that we properly detect when an account expires
// on-chain. As a result, the account should be marked as StateExpired in the
// database.
func TestAccountExpiration(t *testing.T) {
	t.Parallel()

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,
		},
		{
			name:    "happy path version 1",
			version: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			maxAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		h.expireAccount(account)
	})
}

// TestAccountSpendBatchNotFinalized ensures that if a pending batch exists at
// the time of an account spend, then its updates are applied to the account in
// order to properly locate the latest account output.
func TestAccountSpendBatchNotFinalized(t *testing.T) {
	t.Parallel()

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,
		},
		{
			name:       "happy path version 1",
			version:    VersionTaprootEnabled,
			newVersion: VersionTaprootEnabled,
		},
		{
			name:       "happy path version upgrade during batch",
			version:    VersionInitialNoVersion,
			newVersion: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			maxAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		// Create an account spend which we'll notify later. This spend
		// should take the multi-sig path to trigger the pending batch
		// logic.
		const newValue = maxAccountValue / 2
		upgradedAccount := account.Copy(VersionModifier(tc.newVersion))
		newPkScript, err := upgradedAccount.NextOutputScript()
		require.NoError(h.t, err)
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

		// Then, we'll simulate a pending batch by staging some account
		// updates that should be applied once the spend arrives.
		mods := []Modifier{
			ValueModifier(newValue),
			StateModifier(StatePendingUpdate),
			OutPointModifier(wire.OutPoint{
				Hash:  spendTx.TxHash(),
				Index: 0,
			}),
			IncrementBatchKey(),
			VersionModifier(tc.newVersion),
		}
		h.store.setPendingBatch(func() error {
			return h.store.updateAccount(account, mods...)
		})

		// Notify the spend.
		h.notifier.spendChan <- &chainntnfs.SpendDetail{
			SpendingTx: spendTx,
		}

		// Assert that the account updates have been applied. Note that
		// it may seem like our account pointer hasn't had the updates
		// applied, but the updateAccount call above does so implicitly.
		h.assertAccountExists(account)
	})
}

// TestAccountWithdrawal ensures that we can process an account withdrawal
// through the happy flow.
func TestAccountWithdrawal(t *testing.T) {
	t.Parallel()

	const feeRate = chainfee.FeePerKwFloor

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,

			// We'll use the lowest fee rate possible, which should
			// yield a transaction fee of 260 satoshis when taking
			// into account the outputs we'll be withdrawing to.
			fee: 260,
		},
		{
			name:       "happy path version 1",
			version:    VersionTaprootEnabled,
			newVersion: VersionTaprootEnabled,
			fee:        219,
		},
		{
			name: "happy path version upgrade during " +
				"withdrawal",
			version:    VersionInitialNoVersion,
			newVersion: VersionTaprootEnabled,
			fee:        260,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			maxAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		// With our account created, we'll start with an invalid
		// withdrawal to a dust output, which should fail.
		dustOutput := &wire.TxOut{Value: 0, PkScript: p2wsh}
		_, _, err := h.manager.WithdrawAccount(
			context.Background(), account.TraderKey.PubKey,
			[]*wire.TxOut{dustOutput}, feeRate, bestHeight, 0,
			tc.newVersion,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "dust output")

		// We'll now attempt a withdrawal that should succeed. We'll
		// start by creating the outputs we'll withdraw our funds to.
		// We'll create three outputs, one of each supported output
		// type. Each output will have 1/4 of the account's value.
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

		// Attempt the withdrawal.
		//
		// If successful, we'll follow with a series of assertions to
		// ensure it was performed correctly.
		_, _, err = h.manager.WithdrawAccount(
			context.Background(), account.TraderKey.PubKey, outputs,
			feeRate, bestHeight, 0, tc.newVersion,
		)
		require.NoError(h.t, err)

		// The value of the account after the withdrawal depends on the
		// transaction fee and the amount of each output withdrawn to.
		withdrawOutputSum := valuePerOutput * btcutil.Amount(
			len(outputs),
		)
		valueAfterWithdrawal := account.Value - withdrawOutputSum -
			tc.fee
		h.assertAccountModification(
			account, nil, outputs, valueAfterWithdrawal,
			tc.newVersion, 0, 0, bestHeight,
		)

		// Finally, close the account to ensure we can process another
		// spend after the withdrawal.
		expr := defaultFeeExpr
		_ = h.closeAccount(account, &expr, bestHeight)
	})
}

// TestAccountDeposit ensures that we can process an account deposit
// through the happy flow.
func TestAccountDeposit(t *testing.T) {
	t.Parallel()

	// We'll start by defining our initial account value and its value after
	// a successful deposit.
	const initialAccountValue = MinAccountValue
	const valueAfterDeposit = initialAccountValue * 2
	const utxoAmount = initialAccountValue * 3
	const depositAmount = valueAfterDeposit - initialAccountValue

	const feeRate = chainfee.FeePerKwFloor
	const accountInputFees = 110
	const accountInputFeesTaproot = 68

	cases := []*testCase{
		{
			name:               "happy path version 0",
			fundedOutputAmount: depositAmount + accountInputFees,

			// We'll use the lowest fee rate possible, which should
			// yield a transaction fee of 346 satoshis when taking
			// into account the additional inputs needed to satisfy
			// the deposit.
			fundingTxFee: accountInputFees + 236,
			utxo: &lnwallet.Utxo{
				AddressType: lnwallet.WitnessPubKey,
				Value:       utxoAmount,
				PkScript:    p2wpkh,
				OutPoint:    wire.OutPoint{Index: 1},
			},
			version: VersionInitialNoVersion,
		},
		{
			name:               "insufficient fees",
			fundedOutputAmount: depositAmount + accountInputFees,

			// We'll use a fee that is lower than the lowest fee
			// we can get with the lowest rate possible (which would
			// be 346 satoshis), so we should get an error.
			fundingTxFee: accountInputFees + 10,
			utxo: &lnwallet.Utxo{
				AddressType: lnwallet.WitnessPubKey,
				Value:       utxoAmount,
				PkScript:    p2wpkh,
				OutPoint:    wire.OutPoint{Index: 1},
			},
			version: VersionInitialNoVersion,
			expectedErr: "signed transaction only pays 120 sats " +
				"in fees while 255 are required for relay",
		},
		{
			name: "happy path version 1",
			fundedOutputAmount: depositAmount +
				accountInputFeesTaproot,

			// We'll use the lowest fee rate possible, which should
			// yield a transaction fee of 304 satoshis when taking
			// into account the additional inputs needed to satisfy
			// the deposit.
			fundingTxFee: accountInputFeesTaproot + 236,
			utxo: &lnwallet.Utxo{
				AddressType: lnwallet.WitnessPubKey,
				Value:       utxoAmount,
				PkScript:    p2wpkh,
				OutPoint:    wire.OutPoint{Index: 1},
			},
			version:    VersionTaprootEnabled,
			newVersion: VersionTaprootEnabled,
		},
		{
			name: "happy path version upgrade during deposit",
			fundedOutputAmount: depositAmount +
				accountInputFees,

			// We'll use the lowest fee rate possible, which should
			// yield a transaction fee of 346 satoshis when taking
			// into account the additional inputs needed to satisfy
			// the deposit.
			fundingTxFee: accountInputFees + 236,
			utxo: &lnwallet.Utxo{
				AddressType: lnwallet.WitnessPubKey,
				Value:       utxoAmount,
				PkScript:    p2wpkh,
				OutPoint:    wire.OutPoint{Index: 1},
			},
			version:    VersionInitialNoVersion,
			newVersion: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			initialAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		upgradedAccount := account.Copy(VersionModifier(tc.newVersion))
		accountOutputScript, _ := upgradedAccount.NextOutputScript()

		// We'll provide a funded PSBT to the manager that has a change
		// output.
		h.wallet.utxos = []*lnwallet.Utxo{tc.utxo}
		h.wallet.fundPsbt = &psbt.Packet{
			UnsignedTx: &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: tc.utxo.OutPoint,
				}},
				TxOut: []*wire.TxOut{{
					Value:    int64(tc.fundedOutputAmount),
					PkScript: accountOutputScript,
				}, {
					Value: int64(
						tc.utxo.Value - depositAmount -
							tc.fundingTxFee,
					),
					PkScript: np2wpkh,
				}},
			},
			Inputs: []psbt.PInput{{
				WitnessUtxo: &wire.TxOut{
					Value:    int64(tc.utxo.Value),
					PkScript: tc.utxo.PkScript,
				},
				// Normally the FinalizePsbt call would add the
				// signatures for the wallet UTXOs. Since we're
				// only using a mock, we add some signatures
				// manually.
				PartialSigs: []*psbt.PartialSig{{
					Signature: []byte{1, 2, 3},
				}},
			}},
			Outputs: []psbt.POutput{{}, {}},
		}
		h.wallet.fundPsbtChangeIdx = 1

		// Attempt the deposit.
		//
		// If successful, we'll follow with a series of assertions to
		// ensure it was performed correctly.
		_, _, err := h.manager.DepositAccount(
			context.Background(), account.TraderKey.PubKey,
			depositAmount, feeRate, bestHeight, 0, tc.newVersion,
		)

		if tc.expectedErr != "" {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)

			return
		}

		require.NoError(t, err)

		// The transaction should have found the expected account input
		// and output at the following indices.
		const accountInputIdx = 1
		const accountOutputIdx = 1

		h.assertAccountModification(
			account, h.wallet.utxos,
			[]*wire.TxOut{h.wallet.fundPsbt.UnsignedTx.TxOut[0]},
			valueAfterDeposit, tc.newVersion, accountInputIdx,
			accountOutputIdx, bestHeight,
		)

		// Finally, close the account to ensure we can process another
		// spend after the withdrawal.
		expr := defaultFeeExpr
		_ = h.closeAccount(account, &expr, bestHeight)
	})
}

// TestAccountConsecutiveBatches ensures that we can process an account update
// through multiple consecutive batches that only confirm after we've already
// updated our database state.
func TestAccountConsecutiveBatches(t *testing.T) {
	t.Parallel()

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,
		},
		{
			name:       "happy path version 1",
			version:    VersionTaprootEnabled,
			newVersion: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			maxAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		// Then, we'll simulate the maximum number of unconfirmed
		// batches to happen that'll all confirm in the same block.
		const newValue = maxAccountValue / 2
		const numBatches = 10
		batchTxs := make([]*wire.MsgTx, numBatches)
		for i := 0; i < numBatches; i++ {
			newPkScript, err := account.NextOutputScript()
			require.NoError(t, err)

			// Create an account spend which we'll notify later.
			// This spend should take the multi-sig path to trigger
			// the logic to lookup previous outpoints.
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
				VersionModifier(tc.newVersion),
			}
			err = h.store.updateAccount(account, mods...)
			require.NoError(t, err)

			// The RPC server will notify the manager each time a
			// batch is finalized, we do the same here.
			err = h.manager.WatchMatchedAccounts(
				context.Background(),
				[]*btcec.PublicKey{account.TraderKey.PubKey},
			)
			require.NoError(t, err)
		}

		// Notify the confirmation, causing the account to transition
		// back to StateOpen.
		confHeight := bestHeight + 6
		h.notifier.confChan <- &chainntnfs.TxConfirmation{
			Tx:          batchTxs[len(batchTxs)-1],
			BlockHeight: uint32(confHeight),
		}
		StateModifier(StateOpen)(account)
		HeightHintModifier(uint32(confHeight))(account)
		h.assertAccountExists(account)
	})
}

// TestAccountUpdateSubscriptionOnRestart ensures that the account manager
// subscribes accounts in certain states for auction updates after a restart.
func TestAccountUpdateSubscriptionOnRestart(t *testing.T) {
	t.Parallel()

	cases := []*testCase{
		{
			name:    "happy path version 0",
			version: VersionInitialNoVersion,
		},
		{
			name:    "happy path version 1",
			version: VersionTaprootEnabled,
		},
	}

	runSubTests(t, cases, func(t *testing.T, h *testHarness, tc *testCase) {
		account := h.openAccount(
			maxAccountValue, bestHeight+maxAccountExpiry,
			bestHeight, tc.version,
		)

		// StateOpen case.
		h.restartManager()
		h.assertAccountSubscribed(account.TraderKey.PubKey)

		err := h.store.UpdateAccount(
			account, StateModifier(StatePendingBatch),
		)
		require.NoError(t, err)

		// StatePendingBatch case.
		h.restartManager()
		h.assertAccountSubscribed(account.TraderKey.PubKey)

		err = h.store.UpdateAccount(
			account, StateModifier(StatePendingUpdate),
		)
		require.NoError(t, err)

		// StatePendingUpdate case.
		h.restartManager()
		h.assertAccountNotSubscribed(account.TraderKey.PubKey)

		// Confirm the account.
		confHeight := uint32(bestHeight + 6)
		h.notifier.confChan <- &chainntnfs.TxConfirmation{
			BlockHeight: confHeight,
		}
		account.State = StateOpen
		account.HeightHint = confHeight
		h.assertAccountExists(account)

		// It should now be subscribed since it's eligible for batch
		// execution.
		h.assertAccountSubscribed(account.TraderKey.PubKey)
	})
}

// TestMakeTxnLabel tests that the label will be formatted properly, and also
// truncated if needed.
func TestMakeTxnLabel(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		prefix       string
		labelContext string

		label string
	}{
		// Normal case, prefix present, should add a space.
		{
			prefix:       "ok",
			labelContext: "important stuff",
			label:        "ok important stuff",
		},

		// No prefix, should be no leading space.
		{
			prefix:       "",
			labelContext: "important stuff",
			label:        "important stuff",
		},

		// No prefix, prefix itself is so long the context can't fit,
		// should be truncated.
		{
			prefix:       strings.Repeat("o", 500),
			labelContext: "will be left off",
			label:        strings.Repeat("o", 500),
		},

		// Prefix itself is too long, it'll be truncated as well.
		{
			prefix:       strings.Repeat("o", 501),
			labelContext: "will be left off",
			label:        strings.Repeat("o", 500),
		},
	}
	for _, testCase := range testCases {
		genLabel := makeTxnLabel(
			testCase.prefix, testCase.labelContext,
		)

		require.Equal(t, genLabel, testCase.label)
	}
}
