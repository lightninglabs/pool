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
type CtrlConfig struct {
	// ChainNotifier is responsible for requesting confirmation and spend
	// notifications for accounts.
	ChainNotifier lndclient.ChainNotifierClient
}

type ControllerInterface interface {
	// Start allows the Controller to begin accepting watch requests.
	Start() error

	// Stop safely stops any ongoing requests within the Controller.
	Stop()

	// WatchAccountConf watches a new account on-chain for its confirmation. Only
	// one conf watcher per account can be used at any time.
	//
	// NOTE: If there is a previous conf watcher for the given account that has not
	// finished yet, it will be canceled!
	WatchAccountConf(traderKey *btcec.PublicKey,
		txHash chainhash.Hash, script []byte, numConfs, heightHint uint32) error

	// CancelAccountConf cancels the conf watcher of the given account, if one is
	// active.
	CancelAccountConf(traderKey *btcec.PublicKey)

	// WatchAccountSpend watches for the spend of an account. Only one spend watcher
	// per account can be used at any time.
	//
	// NOTE: If there is a previous spend watcher for the given account that has not
	// finished yet, it will be canceled!
	WatchAccountSpend(traderKey *btcec.PublicKey,
		accountPoint wire.OutPoint, script []byte, heightHint uint32) error

	// CancelAccountSpend cancels the spend watcher of the given account, if one is
	// active.
	CancelAccountSpend(traderKey *btcec.PublicKey)

	// WatchAccountExpiration watches for the expiration of an account on-chain.
	// Successive calls for the same account will cancel any previous expiration
	// watch requests and the new expiration will be tracked instead.
	WatchAccountExpiration(traderKey *btcec.PublicKey, expiry uint32) error
}

// Controller implements the watcher.Controller interface
type Controller struct {
	started sync.Once
	stopped sync.Once

	cfg *CtrlConfig

	watcher WatcherInterface

	expiryReqs chan *expiryReq

	wg         sync.WaitGroup
	quit       chan struct{}
	ctxCancels []func()

	cancelMtx    sync.Mutex
	spendCancels map[[33]byte]func()
	confCancels  map[[33]byte]func()
}

// New instantiates a new chain watcher backed by the given config.
func NewController(watcher WatcherInterface, cfg *CtrlConfig) *Controller {
	return &Controller{
		cfg:          cfg,
		watcher:      watcher,
		expiryReqs:   make(chan *expiryReq),
		quit:         make(chan struct{}),
		spendCancels: make(map[[33]byte]func()),
		confCancels:  make(map[[33]byte]func()),
	}
}

// Start allows the Watcher to begin accepting watch requests.
func (wc *Controller) Start() error {
	var err error
	wc.started.Do(func() {
		err = wc.start()
	})
	return err
}

// start allows the Watcher to begin accepting watch requests.
func (wc *Controller) start() error {
	ctxc, cancel := context.WithCancel(context.Background())
	blockChan, errChan, err := wc.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		ctxc,
	)
	if err != nil {
		cancel()
		return err
	}
	wc.ctxCancels = append(wc.ctxCancels, cancel)

	wc.wg.Add(1)
	go wc.expiryHandler(blockChan, errChan)

	return nil
}

// Stop safely stops any ongoing requests within the Watcher.
func (wc *Controller) Stop() {
	wc.stopped.Do(func() {
		close(wc.quit)
		wc.wg.Wait()

		for _, cancel := range wc.ctxCancels {
			cancel()
		}

		wc.cancelMtx.Lock()
		for _, cancel := range wc.spendCancels {
			cancel()
		}
		for _, cancel := range wc.confCancels {
			cancel()
		}
		wc.cancelMtx.Unlock()
	})
}

// expiryHandler receives block notifications to determine when accounts expire.
//
// NOTE: This must be run as a goroutine.
func (wc *Controller) expiryHandler(blockChan chan int32, errChan chan error) {
	defer wc.wg.Done()

	// Wait for the initial block notification to be received before we
	// begin handling requests.
	select {
	case newBlock := <-blockChan:
		wc.watcher.NewBlock(uint32(newBlock))
		wc.watcher.OverdueExpirations(uint32(newBlock))
	case err := <-errChan:
		log.Errorf("Unable to receive initial block notification: %v",
			err)
	case <-wc.quit:
		return
	}

	for {
		select {
		// A new block notification has arrived, update our known
		// height and notify any newly expired accounts.
		case newBlock := <-blockChan:
			wc.watcher.NewBlock(uint32(newBlock))
			wc.watcher.OverdueExpirations(uint32(newBlock))

		// An error occurred while being sent a block notification.
		case err := <-errChan:
			log.Errorf("Unable to receive block notification: %v",
				err)

		// A new watch expiry request has been received for an account.
		case req := <-wc.expiryReqs:
			wc.watcher.AddAccountExpiration(
				req.traderKey, req.expiry,
			)
		case <-wc.quit:
			return
		}
	}
}

// WatchAccountConf watches a new account on-chain for its confirmation. Only
// one conf watcher per account can be used at any time.
//
// NOTE: If there is a previous conf watcher for the given account that has not
// finished yet, it will be canceled!
func (wc *Controller) WatchAccountConf(traderKey *btcec.PublicKey,
	txHash chainhash.Hash, script []byte, numConfs, heightHint uint32) error {

	wc.cancelMtx.Lock()
	defer wc.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	// Cancel a previous conf watcher if one still exists.
	cancel, ok := wc.confCancels[traderKeyRaw]
	if ok {
		cancel()
	}

	ctxc, cancel := context.WithCancel(context.Background())
	confChan, errChan, err := wc.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		ctxc, &txHash, script, int32(numConfs), int32(heightHint),
	)
	if err != nil {
		cancel()
		return err
	}
	wc.confCancels[traderKeyRaw] = cancel

	wc.wg.Add(1)
	go wc.waitForAccountConf(traderKey, traderKeyRaw, confChan, errChan)

	return nil
}

// waitForAccountConf waits for an account's confirmation and takes the
// necessary steps once confirmed.
//
// NOTE: This method must be run as a goroutine.
func (wc *Controller) waitForAccountConf(traderKey *btcec.PublicKey,
	traderKeyRaw [33]byte, confChan chan *chainntnfs.TxConfirmation,
	errChan chan error) {

	defer func() {
		wc.wg.Done()

		wc.cancelMtx.Lock()
		delete(wc.confCancels, traderKeyRaw)
		wc.cancelMtx.Unlock()
	}()

	select {
	case conf := <-confChan:
		if err := wc.watcher.HandleAccountConf(traderKey, conf); err != nil {
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

	case <-wc.quit:
		return
	}
}

// WatchAccountSpend watches for the spend of an account. Only one spend watcher
// per account can be used at any time.
//
// NOTE: If there is a previous spend watcher for the given account that has not
// finished yet, it will be canceled!
func (wc *Controller) WatchAccountSpend(traderKey *btcec.PublicKey,
	accountPoint wire.OutPoint, script []byte, heightHint uint32) error {

	wc.cancelMtx.Lock()
	defer wc.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	// Cancel a previous spend watcher if one still exists.
	cancel, ok := wc.spendCancels[traderKeyRaw]
	if ok {
		cancel()
	}

	ctxc, cancel := context.WithCancel(context.Background())
	spendChan, errChan, err := wc.cfg.ChainNotifier.RegisterSpendNtfn(
		ctxc, &accountPoint, script, int32(heightHint),
	)
	if err != nil {
		cancel()
		return err
	}
	wc.spendCancels[traderKeyRaw] = cancel

	wc.wg.Add(1)
	go wc.waitForAccountSpend(traderKey, traderKeyRaw, spendChan, errChan)

	return nil
}

// waitForAccountSpend waits for an account's spend and takes the necessary
// steps once spent.
//
// NOTE: This method must be run as a goroutine.
func (wc *Controller) waitForAccountSpend(traderKey *btcec.PublicKey,
	traderKeyRaw [33]byte, spendChan chan *chainntnfs.SpendDetail,
	errChan chan error) {

	defer func() {
		wc.wg.Done()

		wc.cancelMtx.Lock()
		delete(wc.spendCancels, traderKeyRaw)
		wc.cancelMtx.Unlock()
	}()

	select {
	case spend := <-spendChan:
		err := wc.watcher.HandleAccountSpend(traderKey, spend)
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

	case <-wc.quit:
		return
	}
}

// WatchAccountExpiration watches for the expiration of an account on-chain.
// Successive calls for the same account will cancel any previous expiration
// watch requests and the new expiration will be tracked instead.
func (wc *Controller) WatchAccountExpiration(traderKey *btcec.PublicKey,
	expiry uint32) error {

	select {
	case wc.expiryReqs <- &expiryReq{
		traderKey: traderKey,
		expiry:    expiry,
	}:
		return nil

	case <-wc.quit:
		return errors.New("watcher shutting down")
	}
}

// CancelAccountSpend cancels the spend watcher of the given account, if one is
// active.
func (wc *Controller) CancelAccountSpend(traderKey *btcec.PublicKey) {
	wc.cancelMtx.Lock()
	defer wc.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	cancel, ok := wc.spendCancels[traderKeyRaw]
	if ok {
		cancel()
	}
}

// CancelAccountConf cancels the conf watcher of the given account, if one is
// active.
func (wc *Controller) CancelAccountConf(traderKey *btcec.PublicKey) {
	wc.cancelMtx.Lock()
	defer wc.cancelMtx.Unlock()

	var traderKeyRaw [33]byte
	copy(traderKeyRaw[:], traderKey.SerializeCompressed())

	cancel, ok := wc.confCancels[traderKeyRaw]
	if ok {
		cancel()
	}
}
