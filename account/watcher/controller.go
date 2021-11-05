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

// Config contains all of the Controller's dependencies in order to carry out its
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

// controller implements the Controller interface
type controller struct {
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

// Compile time assertion that controller implements the Controller interface?
var _ Controller = (*controller)(nil)

// NewController returns an internal struct type that implements the
// Controller interface.
func NewController(cfg *Config) *controller { // nolint:golint
	return &controller{
		cfg:          *cfg,
		expiryReqs:   make(chan *expiryReq),
		quit:         make(chan struct{}),
		spendCancels: make(map[[33]byte]func()),
		confCancels:  make(map[[33]byte]func()),
	}
}

// Start allows the Watcher to begin accepting watch requests.
func (c *controller) Start() error {
	var err error
	c.started.Do(func() {
		err = c.start()
	})
	return err
}

// start allows the Watcher to begin accepting watch requests.
func (c *controller) start() error {
	ctxc, cancel := context.WithCancel(context.Background())
	blockChan, errChan, err := c.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		ctxc,
	)
	if err != nil {
		cancel()
		return err
	}
	c.ctxCancels = append(c.ctxCancels, cancel)

	c.wg.Add(1)
	go c.expiryHandler(blockChan, errChan)

	return nil
}

// Stop safely stops any ongoing requests within the Watcher.
func (c *controller) Stop() {
	c.stopped.Do(func() {
		close(c.quit)
		c.wg.Wait()

		for _, cancel := range c.ctxCancels {
			cancel()
		}

		c.cancelMtx.Lock()
		for _, cancel := range c.spendCancels {
			cancel()
		}
		for _, cancel := range c.confCancels {
			cancel()
		}
		c.cancelMtx.Unlock()
	})
}

// expiryHandler receives block notifications to determine when accounts expire.
//
// NOTE: This must be run as a goroutine.
func (c *controller) expiryHandler(blockChan chan int32, errChan chan error) {
	defer c.wg.Done()

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
	case <-c.quit:
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

				err := c.cfg.HandleAccountExpiry(
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
		case req := <-c.expiryReqs:
			var accountKey [33]byte
			copy(accountKey[:], req.traderKey.SerializeCompressed())

			// If it's already expired, we don't need to track it.
			if req.expiry <= bestHeight {
				err := c.cfg.HandleAccountExpiry(
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

		case <-c.quit:
			return
		}
	}
}

// WatchAccountConf watches a new account on-chain for its confirmation. Only
// one conf watcher per account can be used at any time.
//
// NOTE: If there is a previous conf watcher for the given account that has not
// finished yet, it will be canceled!
func (c *controller) WatchAccountConf(traderKey *btcec.PublicKey,
	txHash chainhash.Hash, script []byte, numConfs, heightHint uint32) error {

	c.cancelMtx.Lock()
	defer c.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	// Cancel a previous conf watcher if one still exists.
	cancel, ok := c.confCancels[traderKeyRaw]
	if ok {
		cancel()
	}

	ctxc, cancel := context.WithCancel(context.Background())
	confChan, errChan, err := c.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctxc, &txHash, script, int32(numConfs), int32(heightHint),
	)
	if err != nil {
		cancel()
		return err
	}
	c.confCancels[traderKeyRaw] = cancel

	c.wg.Add(1)
	go c.waitForAccountConf(traderKey, traderKeyRaw, confChan, errChan)

	return nil
}

// waitForAccountConf waits for an account's confirmation and takes the
// necessary steps once confirmed.
//
// NOTE: This method must be run as a goroutine.
func (c *controller) waitForAccountConf(traderKey *btcec.PublicKey,
	traderKeyRaw [33]byte, confChan chan *chainntnfs.TxConfirmation,
	errChan chan error) {

	defer func() {
		c.wg.Done()

		c.cancelMtx.Lock()
		delete(c.confCancels, traderKeyRaw)
		c.cancelMtx.Unlock()
	}()

	select {
	case conf := <-confChan:
		if err := c.cfg.HandleAccountConf(traderKey, conf); err != nil {
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

	case <-c.quit:
		return
	}
}

// WatchAccountSpend watches for the spend of an account. Only one spend watcher
// per account can be used at any time.
//
// NOTE: If there is a previous spend watcher for the given account that has not
// finished yet, it will be canceled!
func (c *controller) WatchAccountSpend(traderKey *btcec.PublicKey,
	accountPoint wire.OutPoint, script []byte, heightHint uint32) error {

	c.cancelMtx.Lock()
	defer c.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	// Cancel a previous spend watcher if one still exists.
	cancel, ok := c.spendCancels[traderKeyRaw]
	if ok {
		cancel()
	}

	ctxc, cancel := context.WithCancel(context.Background())
	spendChan, errChan, err := c.cfg.ChainNotifier.RegisterSpendNtfn(
		ctxc, &accountPoint, script, int32(heightHint),
	)
	if err != nil {
		cancel()
		return err
	}
	c.spendCancels[traderKeyRaw] = cancel

	c.wg.Add(1)
	go c.waitForAccountSpend(traderKey, traderKeyRaw, spendChan, errChan)

	return nil
}

// waitForAccountSpend waits for an account's spend and takes the necessary
// steps once spent.
//
// NOTE: This method must be run as a goroutine.
func (c *controller) waitForAccountSpend(traderKey *btcec.PublicKey,
	traderKeyRaw [33]byte, spendChan chan *chainntnfs.SpendDetail,
	errChan chan error) {

	defer func() {
		c.wg.Done()

		c.cancelMtx.Lock()
		delete(c.spendCancels, traderKeyRaw)
		c.cancelMtx.Unlock()
	}()

	select {
	case spend := <-spendChan:
		err := c.cfg.HandleAccountSpend(traderKey, spend)
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

	case <-c.quit:
		return
	}
}

// WatchAccountExpiration watches for the expiration of an account on-chain.
// Successive calls for the same account will cancel any previous expiration
// watch requests and the new expiration will be tracked instead.
func (c *controller) WatchAccountExpiration(traderKey *btcec.PublicKey,
	expiry uint32) error {

	select {
	case c.expiryReqs <- &expiryReq{
		traderKey: traderKey,
		expiry:    expiry,
	}:
		return nil

	case <-c.quit:
		return errors.New("watcher shutting down")
	}
}

// CancelAccountSpend cancels the spend watcher of the given account, if one is
// active.
func (c *controller) CancelAccountSpend(traderKey *btcec.PublicKey) {
	c.cancelMtx.Lock()
	defer c.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	cancel, ok := c.spendCancels[traderKeyRaw]
	if ok {
		cancel()
	}
}

// CancelAccountConf cancels the conf watcher of the given account, if one is
// active.
func (c *controller) CancelAccountConf(traderKey *btcec.PublicKey) {
	c.cancelMtx.Lock()
	defer c.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	cancel, ok := c.confCancels[traderKeyRaw]
	if ok {
		cancel()
	}
}
