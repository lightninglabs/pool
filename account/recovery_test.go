package account

import (
	"context"
	"testing"

	"github.com/lightninglabs/pool/internal/test"
)

// TestGenerateRecoveryKeys tests that a certain number of keys can be created
// for account recovery.
func TestGenerateRecoveryKeys(t *testing.T) {
	t.Parallel()

	walletKit := test.NewMockWalletKit()
	keys, err := GenerateRecoveryKeys(context.Background(), walletKit)
	if err != nil {
		t.Fatalf("could not generate keys: %v", err)
	}

	if len(keys) != int(DefaultAccountKeyWindow) {
		t.Fatalf("unexpected number of keys, got %d wanted %d",
			len(keys), DefaultAccountKeyWindow)
	}
}
