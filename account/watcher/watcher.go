package watcher

import (
	"context"
	"errors"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// expiryReq is an internal message we'll sumbit to the Watcher to process for
// external expiration requests.
type expiryReq struct {
	// traderKey is the base trader key of the account.
	traderKey *btcec.PublicKey

	// expiry is the expiry of the account as a block height.
	expiry uint32
}

// Config contains all of the Watcher's dependencies in order to carry out its
// duties.
type Config struct {
	// ChainNotifier is responsible for requesting confirmation and spend
	// notifications for accounts.
	ChainNotifier lndclient.ChainNotifierClient

	// HandleAccountConf abstracts the operations that should be performed
	// for an account once we detect its confirmation. The account is
	// identified by its user sub key (i.e., trader key).
	HandleAccountConf func(*btcec.PublicKey, *chainntnfs.TxConfirmation) error

	// HandleAccountSpend abstracts the operations that should be performed
	// for an account once we detect its spend. The account is identified by
	// its user sub key (i.e., trader key).
	HandleAccountSpend func(*btcec.PublicKey, *chainntnfs.SpendDetail) error

	// HandleAccountExpiry the operations that should be perform for an
	// account once it's expired. The account is identified by its user sub
	// key (i.e., trader key).
	HandleAccountExpiry func(*btcec.PublicKey, uint32) error
}

// Watcher is responsible for the on-chain interaction of an account, whether
// that is confirmation or spend.
type Watcher struct {
	started sync.Once
	stopped sync.Once

	cfg Config

	expiryReqs chan *expiryReq

	wg         sync.WaitGroup
	quit       chan struct{}
	ctxCancels []func()

	cancelMtx    sync.Mutex
	spendCancels map[[33]byte]func()
	confCancels  map[[33]byte]func()
}

// New instantiates a new chain watcher backed by the given config.
func New(cfg *Config) *Watcher {
	return &Watcher{
		cfg:          *cfg,
		expiryReqs:   make(chan *expiryReq),
		quit:         make(chan struct{}),
		spendCancels: make(map[[33]byte]func()),
		confCancels:  make(map[[33]byte]func()),
	}
}

// Start allows the Watcher to begin accepting watch requests.
func (w *Watcher) Start() error {
	var err error
	w.started.Do(func() {
		err = w.start()
	})
	return err
}

// start allows the Watcher to begin accepting watch requests.
func (w *Watcher) start() error {
	ctxc, cancel := context.WithCancel(context.Background())
	blockChan, errChan, err := w.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		ctxc,
	)
	if err != nil {
		cancel()
		return err
	}
	w.ctxCancels = append(w.ctxCancels, cancel)

	w.wg.Add(1)
	go w.expiryHandler(blockChan, errChan)

	return nil
}

// Stop safely stops any ongoing requests within the Watcher.
func (w *Watcher) Stop() {
	w.stopped.Do(func() {
		close(w.quit)
		w.wg.Wait()

		for _, cancel := range w.ctxCancels {
			cancel()
		}

		w.cancelMtx.Lock()
		for _, cancel := range w.spendCancels {
			cancel()
		}
		for _, cancel := range w.confCancels {
			cancel()
		}
		w.cancelMtx.Unlock()
	})
}

// expiryHandler receives block notifications to determine when accounts expire.
//
// NOTE: This must be run as a goroutine.
func (w *Watcher) expiryHandler(blockChan chan int32, errChan chan error) {
	defer w.wg.Done()

	var (
		// bestHeight is the height we believe the current chain is at.
		bestHeight uint32

		// expirations keeps track of the current accounts we're
		// watching expirations for.
		expirations = make(map[[33]byte]uint32)

		// expirationsPerHeight keeps track of all registered accounts
		// that expire at a certain height.
		expirationsPerHeight = make(map[uint32][]*btcec.PublicKey)
	)

	// Wait for the initial block notification to be received before we
	// begin handling requests.
	select {
	case newBlock := <-blockChan:
		bestHeight = uint32(newBlock)
	case err := <-errChan:
		log.Errorf("Unable to receive initial block notification: %v",
			err)
	case <-w.quit:
		return
	}

	for {
		select {
		// A new block notification has arrived, update our known
		// height and notify any newly expired accounts.
		case newBlock := <-blockChan:
			bestHeight = uint32(newBlock)

			for _, traderKey := range expirationsPerHeight[bestHeight] {
				var accountKey [33]byte
				copy(accountKey[:], traderKey.SerializeCompressed())

				// If the account doesn't exist within the
				// expiration set, then the request was
				// canceled and there's nothing for us to do.
				// Similarly, if the request was updated to
				// track a new height, then we can skip it.
				curExpiry, ok := expirations[accountKey]
				if !ok || bestHeight != curExpiry {
					continue
				}

				err := w.cfg.HandleAccountExpiry(
					traderKey, bestHeight,
				)
				if err != nil {
					log.Errorf("Unable to handle "+
						"expiration of account %x: %v",
						traderKey.SerializeCompressed(),
						err)
				}
			}

			delete(expirationsPerHeight, bestHeight)

		// An error occurred while being sent a block notification.
		case err := <-errChan:
			log.Errorf("Unable to receive block notification: %v",
				err)

		// A new watch expiry request has been received for an account.
		case req := <-w.expiryReqs:
			var accountKey [33]byte
			copy(accountKey[:], req.traderKey.SerializeCompressed())

			// If it's already expired, we don't need to track it.
			if req.expiry <= bestHeight {
				err := w.cfg.HandleAccountExpiry(
					req.traderKey, bestHeight,
				)
				if err != nil {
					log.Errorf("Unable to handle "+
						"expiration of account %x: %v",
						req.traderKey.SerializeCompressed(),
						err)
				}
				delete(expirations, accountKey)

				continue
			}

			expirations[accountKey] = req.expiry
			expirationsPerHeight[req.expiry] = append(
				expirationsPerHeight[req.expiry], req.traderKey,
			)

		case <-w.quit:
			return
		}
	}
}

// WatchAccountConf watches a new account on-chain for its confirmation. Only
// one conf watcher per account can be used at any time.
//
// NOTE: If there is a previous conf watcher for the given account that has not
// finished yet, it will be canceled!
func (w *Watcher) WatchAccountConf(traderKey *btcec.PublicKey,
	txHash chainhash.Hash, script []byte, numConfs, heightHint uint32) error {

	w.cancelMtx.Lock()
	defer w.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	// Cancel a previous conf watcher if one still exists.
	cancel, ok := w.confCancels[traderKeyRaw]
	if ok {
		cancel()
	}

	ctxc, cancel := context.WithCancel(context.Background())
	confChan, errChan, err := w.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctxc, &txHash, script, int32(numConfs), int32(heightHint),
	)
	if err != nil {
		cancel()
		return err
	}
	w.confCancels[traderKeyRaw] = cancel

	w.wg.Add(1)
	go w.waitForAccountConf(traderKey, traderKeyRaw, confChan, errChan)

	return nil
}

// waitForAccountConf waits for an account's confirmation and takes the
// necessary steps once confirmed.
//
// NOTE: This method must be run as a goroutine.
func (w *Watcher) waitForAccountConf(traderKey *btcec.PublicKey,
	traderKeyRaw [33]byte, confChan chan *chainntnfs.TxConfirmation,
	errChan chan error) {

	defer func() {
		w.wg.Done()

		w.cancelMtx.Lock()
		delete(w.confCancels, traderKeyRaw)
		w.cancelMtx.Unlock()
	}()

	select {
	case conf := <-confChan:
		if err := w.cfg.HandleAccountConf(traderKey, conf); err != nil {
			log.Errorf("Unable to handle confirmation for account "+
				"%x: %v", traderKey.SerializeCompressed(), err)
		}

	case err := <-errChan:
		if err != nil {
			// Ignore context canceled error due to possible manual
			// cancellation.
			s, ok := status.FromError(err)
			if ok && s.Code() == codes.Canceled {
				return
			}

			log.Errorf("Unable to determine confirmation for "+
				"account %x: %v",
				traderKey.SerializeCompressed(), err)
		}

	case <-w.quit:
		return
	}
}

// WatchAccountSpend watches for the spend of an account. Only one spend watcher
// per account can be used at any time.
//
// NOTE: If there is a previous spend watcher for the given account that has not
// finished yet, it will be canceled!
func (w *Watcher) WatchAccountSpend(traderKey *btcec.PublicKey,
	accountPoint wire.OutPoint, script []byte, heightHint uint32) error {

	w.cancelMtx.Lock()
	defer w.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	// Cancel a previous spend watcher if one still exists.
	cancel, ok := w.spendCancels[traderKeyRaw]
	if ok {
		cancel()
	}

	ctxc, cancel := context.WithCancel(context.Background())
	spendChan, errChan, err := w.cfg.ChainNotifier.RegisterSpendNtfn(
		ctxc, &accountPoint, script, int32(heightHint),
	)
	if err != nil {
		cancel()
		return err
	}
	w.spendCancels[traderKeyRaw] = cancel

	w.wg.Add(1)
	go w.waitForAccountSpend(traderKey, traderKeyRaw, spendChan, errChan)

	return nil
}

// waitForAccountSpend waits for an account's spend and takes the necessary
// steps once spent.
//
// NOTE: This method must be run as a goroutine.
func (w *Watcher) waitForAccountSpend(traderKey *btcec.PublicKey,
	traderKeyRaw [33]byte, spendChan chan *chainntnfs.SpendDetail,
	errChan chan error) {

	defer func() {
		w.wg.Done()

		w.cancelMtx.Lock()
		delete(w.spendCancels, traderKeyRaw)
		w.cancelMtx.Unlock()
	}()

	select {
	case spend := <-spendChan:
		err := w.cfg.HandleAccountSpend(traderKey, spend)
		if err != nil {
			log.Errorf("Unable to handle spend for account %x: %v",
				traderKey.SerializeCompressed(), err)
		}

	case err := <-errChan:
		if err != nil {
			// Ignore context canceled error due to possible manual
			// cancellation.
			s, ok := status.FromError(err)
			if ok && s.Code() == codes.Canceled {
				return
			}

			log.Errorf("Unable to determine spend for account %x: "+
				"%v", traderKey.SerializeCompressed(), err)
		}

	case <-w.quit:
		return
	}
}

// WatchAccountExpiration watches for the expiration of an account on-chain.
// Successive calls for the same account will cancel any previous expiration
// watch requests and the new expiration will be tracked instead.
func (w *Watcher) WatchAccountExpiration(traderKey *btcec.PublicKey,
	expiry uint32) error {

	select {
	case w.expiryReqs <- &expiryReq{
		traderKey: traderKey,
		expiry:    expiry,
	}:
		return nil

	case <-w.quit:
		return errors.New("watcher shutting down")
	}
}

// CancelAccountSpend cancels the spend watcher of the given account, if one is
// active.
func (w *Watcher) CancelAccountSpend(traderKey *btcec.PublicKey) {
	w.cancelMtx.Lock()
	defer w.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	cancel, ok := w.spendCancels[traderKeyRaw]
	if ok {
		cancel()
	}
}

// CancelAccountConf cancels the conf watcher of the given account, if one is
// active.
func (w *Watcher) CancelAccountConf(traderKey *btcec.PublicKey) {
	w.cancelMtx.Lock()
	defer w.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	cancel, ok := w.confCancels[traderKeyRaw]
	if ok {
		cancel()
	}
}
