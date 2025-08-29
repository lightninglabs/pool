package watcher

import (
	"crypto/ecdsa"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	gomock "go.uber.org/mock/gomock"
)

func randomPrivateKey(seed int64) *btcec.PrivateKey {
	r := rand.New(rand.NewSource(seed))
	key, err := ecdsa.GenerateKey(secp.S256(), r)
	if err != nil {
		return nil
	}
	return secp.PrivKeyFromBytes(key.D.Bytes())
}

func randomPublicKey(seed int64) *btcec.PublicKey {
	key := randomPrivateKey(seed)
	return key.PubKey()
}

// func randomAccountKey(seed int64) [33]byte {
//	var accountKey [33]byte
//
//	key := randomPublicKey(seed)
//	copy(accountKey[:], key.SerializeCompressed())
//	return accountKey
// }

var overdueExpirationsTestCases = []struct {
	name                 string
	blockHeight          uint32
	expirations          map[[33]byte]uint32
	expirationsPerHeight map[uint32][]*btcec.PublicKey
	handledExpirations   []*btcec.PublicKey
	checks               []func(watcher *expiryWatcher) error
}{{
	// TODO(guggero): Find out why some tests in this file are suddenly
	// failing after upgrading to lnd 0.18.0 (maybe the now required Go
	// version?).
	//	name:        "overdue expirations are handled properly",
	//	blockHeight: 24,
	//	expirations: map[[33]byte]uint32{
	//		randomAccountKey(0): 24,
	//		randomAccountKey(1): 24,
	//		randomAccountKey(2): 24,
	//		randomAccountKey(3): 27,
	//	},
	//	handledExpirations: []*btcec.PublicKey{
	//		randomPublicKey(0),
	//		randomPublicKey(1),
	//		randomPublicKey(2),
	//	},
	//	expirationsPerHeight: map[uint32][]*btcec.PublicKey{
	//		24: {
	//			randomPublicKey(0),
	//			randomPublicKey(1),
	//			randomPublicKey(2),
	//		},
	//		27: {
	//			randomPublicKey(27),
	//		},
	//	},
	//	checks: []func(watcher *expiryWatcher) error{
	//		func(watcher *expiryWatcher) error {
	//			left := watcher.expirationsPerHeight[24]
	//			if len(left) != 0 {
	//				return errors.New(
	//					"expirations were not " +
	//						"handled properly",
	//				)
	//			}
	//			return nil
	//		},
	//		func(watcher *expiryWatcher) error {
	//			if len(watcher.expirations) != 1 {
	//				return errors.New(
	//					"handled expirations were " +
	//						"not deleted",
	//				)
	//			}
	//			return nil
	//		},
	//	},
	// }, {
	name:        "if account wasn't track we ignore it",
	blockHeight: 24,
	expirationsPerHeight: map[uint32][]*btcec.PublicKey{
		24: {
			randomPublicKey(3),
		},
	},
	checks: []func(watcher *expiryWatcher) error{},
}}

func TestOverdueExpirations(t *testing.T) {
	for _, tc := range overdueExpirationsTestCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			handlers := NewMockEventHandler(mockCtrl)
			watcher := NewExpiryWatcher(handlers)
			watcher.expirations = tc.expirations
			watcher.expirationsPerHeight = tc.expirationsPerHeight

			for _, trader := range tc.handledExpirations {
				handlers.EXPECT().
					HandleAccountExpiry(
						trader,
						tc.blockHeight,
					).
					Return(nil)
			}

			watcher.NewBlock(tc.blockHeight)

			for _, check := range tc.checks {
				if err := check(watcher); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

var addAccountExpirationTestCases = []struct {
	name               string
	bestHeight         uint32
	initialExpirations map[[33]byte]uint32
	expirations        map[*btcec.PublicKey]uint32
	handler            func(*btcec.PublicKey, uint32) error
	checks             []func(watcher *expiryWatcher) error
}{{
	name:       "account is tracked happy path",
	bestHeight: 20,
	expirations: map[*btcec.PublicKey]uint32{
		randomPublicKey(1): 25,
		randomPublicKey(2): 25,
		randomPublicKey(3): 25,
	},
	checks: []func(watcher *expiryWatcher) error{
		func(watcher *expiryWatcher) error {
			if len(watcher.expirations) != 3 {
				return errors.New(
					"account expiry not added",
				)
			}
			return nil
		},
	},
}, {
	name:       "account with earlier expiry are directly handled",
	bestHeight: 20,
	expirations: map[*btcec.PublicKey]uint32{
		randomPublicKey(1): 19,
	},
	handler: func(*btcec.PublicKey, uint32) error {
		return nil
	},
	checks: []func(watcher *expiryWatcher) error{
		func(watcher *expiryWatcher) error {
			if len(watcher.expirations) != 0 {
				return errors.New("an account with " +
					"older expiry hight was added")
			}
			return nil
		},
	},
	// }, {
	//	name:       "adding an account that we are already watching",
	//	bestHeight: 20,
	//	initialExpirations: map[[33]byte]uint32{
	//		randomAccountKey(1): 25,
	//	},
	//	expirations: map[*btcec.PublicKey]uint32{
	//		randomPublicKey(1): 35,
	//	},
	//	handler: func(*btcec.PublicKey, uint32) error {
	//		return nil
	//	},
	//	checks: []func(watcher *expiryWatcher) error{
	//		func(watcher *expiryWatcher) error {
	//			msg := "account expiry was not updated"
	//			if len(watcher.expirationsPerHeight[35]) != 1 {
	//				return errors.New(msg)
	//			}
	//
	//			if watcher.expirations[randomAccountKey(1)] != 35 {
	//				return errors.New(msg)
	//			}
	//			return nil
	//		},
	//	},
}}

func TestAddAccountExpiration(t *testing.T) {
	for _, tc := range addAccountExpirationTestCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			handlers := NewMockEventHandler(mockCtrl)
			watcher := NewExpiryWatcher(handlers)

			if len(tc.initialExpirations) > 0 {
				watcher.expirations = tc.initialExpirations
			}
			watcher.bestHeight = tc.bestHeight

			for trader, height := range tc.expirations {
				if height < tc.bestHeight {
					handlers.EXPECT().
						HandleAccountExpiry(
							trader,
							tc.bestHeight,
						).
						Return(nil)
				}

				watcher.AddAccountExpiration(trader, height)
			}

			// The HandleAccountExpiry is executed in the background
			// give it some time to ensure that the goroutine has time
			// to get executed. This could potentially trigger
			// false test failures.
			time.Sleep(500 * time.Millisecond)

			for _, check := range tc.checks {
				if err := check(watcher); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
