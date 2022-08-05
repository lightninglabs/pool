package watcher

import (
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
)

// expiryWatcher implements the ExpiryWatcher interface.
type expiryWatcher struct {
	handlers EventHandler

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

// NewExpiryWatcher instantiates a new ExpiryWatcher.
func NewExpiryWatcher(handlers EventHandler) *expiryWatcher { // nolint:golint
	return &expiryWatcher{
		handlers:             handlers,
		expirations:          make(map[[33]byte]uint32),
		expirationsPerHeight: make(map[uint32][]*btcec.PublicKey),
	}
}

// NewBlock updates the current bestHeight.
func (w *expiryWatcher) NewBlock(bestHeight uint32) {
	w.expirationsMtx.Lock()
	defer w.expirationsMtx.Unlock()

	w.bestHeight = bestHeight
	w.overdueExpirations(w.bestHeight)
}

// overdueExpirations handles the expirations for the given block.
func (w *expiryWatcher) overdueExpirations(blockHeight uint32) {
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

		err := w.handlers.HandleAccountExpiry(
			traderKey, blockHeight,
		)
		if err != nil {
			log.Errorf("Unable to handle "+
				"expiration of account %x: %v",
				traderKey.SerializeCompressed(),
				err)
		}
		delete(w.expirations, accountKey)
	}

	delete(w.expirationsPerHeight, blockHeight)
}

// AddAccountExpiration creates or updates the existing record for the traderKey.
func (w *expiryWatcher) AddAccountExpiration(traderKey *btcec.PublicKey,
	expiry uint32) {

	w.expirationsMtx.Lock()
	defer w.expirationsMtx.Unlock()

	var accountKey [33]byte
	copy(accountKey[:], traderKey.SerializeCompressed())

	// If it's already expired, we don't need to track it.
	if expiry <= w.bestHeight {
		// Delete the entry from the watcher.expirations
		// and handle the expiry in the background.
		go func() {
			if err := w.handlers.HandleAccountExpiry(
				traderKey, w.bestHeight,
			); err != nil {
				log.Errorf("Unable to handle "+
					"expiration of account %x: %v",
					traderKey.SerializeCompressed(),
					err)
			}
		}()

		delete(w.expirations, accountKey)
		return
	}

	w.expirations[accountKey] = expiry
	w.expirationsPerHeight[expiry] = append(
		w.expirationsPerHeight[expiry], traderKey,
	)
}
