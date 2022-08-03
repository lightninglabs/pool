package watcher

import (
	"context"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CtrlConfig contains all of the Controller's dependencies in order to carry out its
// duties.
type CtrlConfig struct {
	// ChainNotifier is responsible for requesting confirmation and spend
	// notifications for accounts.
	ChainNotifier lndclient.ChainNotifierClient

	// Handlers define the handler to be used after receiving every event.
	Handlers EventHandler
}

// controller implements the Controller interface.
type controller struct {
	started sync.Once
	stopped sync.Once

	cfg *CtrlConfig

	watcher ExpiryWatcher

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
func NewController(cfg *CtrlConfig) *controller { // nolint:golint
	watcher := NewExpiryWatcher(cfg.Handlers)
	return &controller{
		cfg:          cfg,
		watcher:      watcher,
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

	// Wait for the initial block notification to be received before we
	// begin handling requests.
	select {
	case newBlock := <-blockChan:
		c.watcher.NewBlock(uint32(newBlock))
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
			c.watcher.NewBlock(uint32(newBlock))

		// An error occurred while being sent a block notification.
		case err := <-errChan:
			log.Errorf("Unable to receive block notification: %v",
				err)

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
		err := c.cfg.Handlers.HandleAccountConf(traderKey, conf)
		if err != nil {
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
		err := c.cfg.Handlers.HandleAccountSpend(traderKey, spend)
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
	expiry uint32) {

	c.watcher.AddAccountExpiration(traderKey, expiry)
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
