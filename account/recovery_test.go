package account

import (
	"context"
	"math"
	"testing"

	"github.com/lightninglabs/loop/test"
)

// TestGenerateRecoveryKeys tests that a certain number of keys can be created
// for account recovery.
func TestGenerateRecoveryKeys(t *testing.T) {
	t.Parallel()

	lnd := test.NewMockLnd()
	keys, err := GenerateRecoveryKeys(context.Background(), lnd.WalletKit)
	if err != nil {
		t.Fatalf("could not generate keys: %v", err)
	}

	if len(keys) != int(DefaultAccountKeyWindow) {
		t.Fatalf("unexpected number of keys, got %d wanted %d",
			len(keys), DefaultAccountKeyWindow)
	}
}

// TestFindTraderKeyIndex tests that a trader key index can be found by just
// supplying the public key.
func TestFindTraderKeyIndex(t *testing.T) {
	t.Parallel()

	lnd := test.NewMockLnd()
	keys, err := GenerateRecoveryKeys(context.Background(), lnd.WalletKit)
	if err != nil {
		t.Fatalf("could not generate keys: %v", err)
	}

	// The mock doesn't support more than 256 private keys, but that's fine
	// for our purposes.
	for idx := 0; idx < math.MaxUint8; idx++ {
		key := keys[idx]
		keyIndex, err := findTraderKeyIndex(
			context.Background(), lnd.WalletKit, key.PubKey,
		)
		if err != nil {
			t.Fatalf("could not find trader key index: %v", err)
		}

		if keyIndex != key.Index {
			t.Fatalf("invalid key found, got %d wanted %d",
				keyIndex, key.Index)
		}
	}
}
