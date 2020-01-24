package watcher

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

const (
	timeout = 500 * time.Millisecond
)

var (
	zeroOutPoint        wire.OutPoint
	rawTestTraderKey, _ = hex.DecodeString("02d0de0999f50eaacaae5b6e178eec7c8bd99dd797bc9f7cfb497e2188884d59f3")
	testTraderKey, _    = btcec.ParsePubKey(rawTestTraderKey, btcec.S256())
	testScript, _       = hex.DecodeString("00149589c15e7a8a8065f75aad5f3337cfccf909174a")
)

// TestWatcherConf ensures that the watcher performs its expected operations
// once an account confirmation has been detected.
func TestWatcherConf(t *testing.T) {
	t.Parallel()

	// Set up the required dependencies of the Watcher.
	notifier := newMockChainNotifier()

	// The HandleAccountConf closure will use a signal to indicate that it's
	// been invoked once a confirmation notification is received.
	confSignal := make(chan struct{})
	handleConf := func(*btcec.PublicKey, *chainntnfs.TxConfirmation) error {
		close(confSignal)
		return nil
	}

	watcher := New(&Config{
		ChainNotifier:     notifier,
		HandleAccountConf: handleConf,
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("unable to start watcher: %v", err)
	}
	defer watcher.Stop()

	// Watch for an account's confirmation.
	err := watcher.WatchAccountConf(
		testTraderKey, zeroOutPoint.Hash, testScript, 1, 1,
	)
	if err != nil {
		t.Fatalf("unable to watch account conf: %v", err)
	}

	// HandleAccountConf should not be invoked until after the confirmation.
	select {
	case <-confSignal:
		t.Fatal("unexpected conf signal")
	case <-time.After(timeout):
	}

	select {
	case notifier.confChan <- &chainntnfs.TxConfirmation{}:
	case <-time.After(timeout):
		t.Fatal("unable to notify conf")
	}

	select {
	case <-confSignal:
	case <-time.After(timeout):
		t.Fatal("expected conf signal")
	}
}

// TestWatcherSpend ensures that the watcher performs its expected operations
// once an account spend has been detected.
func TestWatcherSpend(t *testing.T) {
	t.Parallel()

	// Set up the required dependencies of the Watcher.
	notifier := newMockChainNotifier()

	// The HandleAccountSpend closure will use a signal to indicate that
	// it's been invoked once a spend notification is received.
	spendSignal := make(chan struct{})
	handleSpend := func(*btcec.PublicKey, *chainntnfs.SpendDetail) error {
		close(spendSignal)
		return nil
	}

	watcher := New(&Config{
		ChainNotifier:      notifier,
		HandleAccountSpend: handleSpend,
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("unable to start watcher: %v", err)
	}
	defer watcher.Stop()

	// Watch for an account's spend.
	err := watcher.WatchAccountSpend(
		testTraderKey, zeroOutPoint, testScript, 1,
	)
	if err != nil {
		t.Fatalf("unable to watch account spend: %v", err)
	}

	// HandleAccountSpend should not be invoked until after the spend.
	select {
	case <-spendSignal:
		t.Fatal("unexpected spend signal")
	case <-time.After(timeout):
	}

	select {
	case notifier.spendChan <- &chainntnfs.SpendDetail{}:
	case <-time.After(timeout):
		t.Fatal("unable to notify spend")
	}

	select {
	case <-spendSignal:
	case <-time.After(timeout):
		t.Fatal("expected spend signal")
	}
}

// TestWatcherExpiry ensures that the watcher performs its expected operations
// once an account expiration has been detected.
func TestWatcherExpiry(t *testing.T) {
	t.Parallel()

	const (
		startHeight  = 100
		expiryHeight = startHeight * 2
	)

	// Set up the required dependencies of the Watcher.
	notifier := newMockChainNotifier()

	// The HandleAccountExpiry closure will use a signal to indicate that
	// it's been invoked once an expiry notification is received.
	expirySignal := make(chan struct{})
	handleExpiry := func(*btcec.PublicKey) error {
		close(expirySignal)
		return nil
	}

	watcher := New(&Config{
		ChainNotifier:       notifier,
		HandleAccountExpiry: handleExpiry,
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("unable to start watcher: %v", err)
	}
	defer watcher.Stop()

	select {
	case notifier.blockChan <- startHeight:
	case <-time.After(timeout):
		t.Fatal("unable to notify block")
	}

	// Watch for an account's expiration that has yet to expire.
	err := watcher.WatchAccountExpiration(testTraderKey, expiryHeight)
	if err != nil {
		t.Fatalf("unable to watch account expiry: %v", err)
	}

	// HandleAccountExpiry should not be invoked until after the expiration.
	select {
	case <-expirySignal:
		t.Fatal("unexpected expiry signal")
	case <-time.After(timeout):
	}

	select {
	case notifier.blockChan <- expiryHeight:
	case <-time.After(timeout):
		t.Fatal("unable to notify expiry")
	}

	select {
	case <-expirySignal:
	case <-time.After(timeout):
		t.Fatal("expected expiry signal")
	}
}

// TestWatcherAccountAlreadyExpired ensures that the watcher performs its
// expected operations once an account expiration has already happened at the
// time of registration.
func TestWatcherAccountAlreadyExpired(t *testing.T) {
	t.Parallel()

	const startHeight = 100

	// Set up the required dependencies of the Watcher.
	notifier := newMockChainNotifier()

	// The HandleAccountExpiry closure will use a signal to indicate that
	// it's been invoked once an expiry notification is received.
	expirySignal := make(chan struct{})
	handleExpiry := func(*btcec.PublicKey) error {
		close(expirySignal)
		return nil
	}

	watcher := New(&Config{
		ChainNotifier:       notifier,
		HandleAccountExpiry: handleExpiry,
	})
	if err := watcher.Start(); err != nil {
		t.Fatalf("unable to start watcher: %v", err)
	}
	defer watcher.Stop()

	select {
	case notifier.blockChan <- startHeight:
	case <-time.After(timeout):
		t.Fatal("unable to notify block")
	}

	// Watch for an account's expiration that has already expired.
	err := watcher.WatchAccountExpiration(testTraderKey, startHeight)
	if err != nil {
		t.Fatalf("unable to watch account expiry: %v", err)
	}

	// HandleAccountExpiry should have been invoked since the expiration was
	// already reached at the time of registration.
	select {
	case <-expirySignal:
	case <-time.After(timeout):
		t.Fatal("expected expiry signal")
	}
}
