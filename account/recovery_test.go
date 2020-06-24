package account

import (
	"context"
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
