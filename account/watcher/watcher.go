package watcher

import (
	"context"
	"errors"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// expiryReq is an internal message we'll sumbit to the Watcher to process for
// external expiration requests.
type expiryReq struct {
	// accountKey is the user key of the account.
	accountKey [33]byte

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
	HandleAccountConf func([33]byte, *chainntnfs.TxConfirmation) error

	// HandleAccountSpend abstracts the operations that should be performed
	// for an account once we detect its spend. The account is identified by
	// its user sub key (i.e., trader key).
	HandleAccountSpend func([33]byte, *chainntnfs.SpendDetail) error

	// HandleAccountExpiry the operations that should be perform for an
	// account once it's expired. The account is identified by its user sub
	// key (i.e., trader key).
	HandleAccountExpiry func([33]byte) error
}

// Watcher is responsible for the on-chain interaction of an account, whether
// that is confirmation or spend.
type Watcher struct {
	started sync.Once
	stopped sync.Once

	cfg Config

	expiryReqs chan *expiryReq

	wg   sync.WaitGroup
	quit chan struct{}
}

// New instantiates a new chain watcher backed by the given config.
func New(cfg *Config) *Watcher {
	return &Watcher{
		cfg:        *cfg,
		expiryReqs: make(chan *expiryReq),
		quit:       make(chan struct{}),
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
	blockChan, errChan, err := w.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		context.Background(),
	)
	if err != nil {
		return err
	}

	w.wg.Add(1)
	go w.expiryHandler(blockChan, errChan)

	return nil
}

// Stop safely stops any ongoing requests within the Watcher.
func (w *Watcher) Stop() {
	w.stopped.Do(func() {
		close(w.quit)
		w.wg.Wait()
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

		// expirations keeps track of all registered accounts that
		// expire at a certain height.
		expirations = make(map[uint32][][33]byte)
	)

	for {
		select {
		// A new block notification has arrived, update our known
		// height and notify any newly expired accounts.
		case newBlock := <-blockChan:
			bestHeight = uint32(newBlock)

			for _, accountKey := range expirations[bestHeight] {
				err := w.cfg.HandleAccountExpiry(accountKey)
				if err != nil {
					log.Errorf("Unable to handle "+
						"expiration of account %x: %v",
						accountKey, err)
				}
			}

			delete(expirations, bestHeight)

		// An error occurred while being sent a block notification.
		case err := <-errChan:
			log.Errorf("Unable to receive block notification: %v",
				err)

		// A new watch expiry request has been received for an account.
		case req := <-w.expiryReqs:
			// If it's already expired, we don't need to track it.
			if req.expiry <= bestHeight {
				err := w.cfg.HandleAccountExpiry(req.accountKey)
				if err != nil {
					log.Errorf("Unable to handle "+
						"expiration of account %x: %v",
						req.accountKey, err)
				}

				continue
			}

			expirations[req.expiry] = append(
				expirations[req.expiry], req.accountKey,
			)

		case <-w.quit:
			return
		}
	}
}

// WatchAccountConf watches a new account on-chain for its confirmation.
func (w *Watcher) WatchAccountConf(accountKey [33]byte, txHash chainhash.Hash,
	script []byte, numConfs, heightHint uint32) error {

	ctx := context.Background()
	confChan, errChan, err := w.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, &txHash, script, int32(numConfs), int32(heightHint),
	)
	if err != nil {
		return err
	}

	w.wg.Add(1)
	go w.waitForAccountConf(accountKey, confChan, errChan)

	return nil
}

// waitForAccountConf waits for an account's confirmation and takes the
// necessary steps once confirmed.
//
// NOTE: This method must be run as a goroutine.
func (w *Watcher) waitForAccountConf(accountKey [33]byte,
	confChan chan *chainntnfs.TxConfirmation, errChan chan error) {

	defer w.wg.Done()

	select {
	case conf := <-confChan:
		if err := w.cfg.HandleAccountConf(accountKey, conf); err != nil {
			log.Errorf("Unable to handle confirmation for account "+
				"%x: %v", accountKey, err)
		}

	case err := <-errChan:
		if err != nil {
			log.Errorf("Unable to determine confirmation for "+
				"account %x: %v", accountKey, err)
		}

	case <-w.quit:
		return
	}
}

// WatchAccountSpend watches for the spend of an account.
func (w *Watcher) WatchAccountSpend(accountKey [33]byte,
	accountPoint wire.OutPoint, script []byte, heightHint uint32) error {

	ctx := context.Background()
	spendChan, errChan, err := w.cfg.ChainNotifier.RegisterSpendNtfn(
		ctx, &accountPoint, script, int32(heightHint),
	)
	if err != nil {
		return err
	}

	w.wg.Add(1)
	go w.waitForAccountSpend(accountKey, spendChan, errChan)

	return nil
}

// waitForAccountSpend waits for an account's spend and takes the necessary
// steps once spent.
//
// NOTE: This method must be run as a goroutine.
func (w *Watcher) waitForAccountSpend(accountKey [33]byte,
	spendChan chan *chainntnfs.SpendDetail, errChan chan error) {

	defer w.wg.Done()

	select {
	case spend := <-spendChan:
		err := w.cfg.HandleAccountSpend(accountKey, spend)
		if err != nil {
			log.Errorf("Unable to handle spend for account %x: %v",
				accountKey, err)
		}

	case err := <-errChan:
		if err != nil {
			log.Errorf("Unable to determine spend for account %x: "+
				"%v", accountKey, err)
		}

	case <-w.quit:
		return
	}
}

// WatchAccountExpiration watches for the expiration of an account on-chain.
func (w *Watcher) WatchAccountExpiration(accountKey [33]byte, expiry uint32) error {
	select {
	case w.expiryReqs <- &expiryReq{
		accountKey: accountKey,
		expiry:     expiry,
	}:
		return nil

	case <-w.quit:
		return errors.New("watcher shutting down")
	}
}
