package watcher

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// Config contains all of the Watcher's dependencies in order to carry out its
// duties.
type Config struct {
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

type Watcher struct {
	cfg Config

	// bestHeight is the height we believe the current chain is at.
	bestHeight uint32

	// expirations keeps track of the current accounts we're
	// watching expirations for.
	expirations map[[33]byte]uint32

	// expirationsPerHeight keeps track of all registered accounts
	// that expire at a certain height.
	expirationsPerHeight map[uint32][]*btcec.PublicKey

	expirationsMtx sync.Mutex
}

func NewWatcher(cfg *Config) *Watcher {
	return &Watcher{
		cfg:                  *cfg,
		expirations:          make(map[[33]byte]uint32),
		expirationsPerHeight: make(map[uint32][]*btcec.PublicKey),
	}
}

func (w *Watcher) NewBlock(bestHeight uint32) {
	w.bestHeight = bestHeight
}

func (w *Watcher) OverdueExpirations(blockHeight uint32) {
	w.expirationsMtx.Lock()
	defer w.expirationsMtx.Unlock()

	for _, traderKey := range w.expirationsPerHeight[blockHeight] {
		var accountKey [33]byte
		copy(accountKey[:], traderKey.SerializeCompressed())

		// If the account doesn't exist within the
		// expiration set, then the request was
		// canceled and there's nothing for us to do.
		// Similarly, if the request was updated to
		// track a new height, then we can skip it.
		curExpiry, ok := w.expirations[accountKey]
		if !ok || blockHeight != curExpiry {
			continue
		}

		err := w.cfg.HandleAccountExpiry(
			traderKey, blockHeight,
		)
		if err != nil {
			log.Errorf("Unable to handle "+
				"expiration of account %x: %v",
				traderKey.SerializeCompressed(),
				err)
		}
	}

	delete(w.expirationsPerHeight, blockHeight)
}

func (w *Watcher) AddAccountExpiration(traderKey *btcec.PublicKey,
	expiry uint32) {

	w.expirationsMtx.Lock()
	defer w.expirationsMtx.Unlock()

	var accountKey [33]byte
	copy(accountKey[:], traderKey.SerializeCompressed())

	// If it's already expired, we don't need to track it.
	if expiry <= w.bestHeight {
		err := w.HandleAccountExpiry(
			traderKey, w.bestHeight,
		)
		if err != nil {
			log.Errorf("Unable to handle "+
				"expiration of account %x: %v",
				traderKey.SerializeCompressed(),
				err)
		}
		delete(w.expirations, accountKey)
		return
	}

	w.expirations[accountKey] = expiry
	w.expirationsPerHeight[expiry] = append(
		w.expirationsPerHeight[expiry], traderKey,
	)
}

func (w *Watcher) HandleAccountConf(traderKey *btcec.PublicKey,
	confDetails *chainntnfs.TxConfirmation) error {

	return w.cfg.HandleAccountConf(traderKey, confDetails)
}

func (w *Watcher) HandleAccountSpend(traderKey *btcec.PublicKey,
	spendDetails *chainntnfs.SpendDetail) error {

	return w.cfg.HandleAccountSpend(traderKey, spendDetails)
}

func (w *Watcher) HandleAccountExpiry(traderKey *btcec.PublicKey,
	height uint32) error {

	return w.cfg.HandleAccountExpiry(traderKey, height)
}
