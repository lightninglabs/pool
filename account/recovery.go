package account

import (
	"context"

	"github.com/lightninglabs/llm/clmscript"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	// DefaultAccountKeyWindow is the number of account keys that are
	// derived to be checked on recovery. This is the absolute maximum
	// number of accounts that can ever be restored. But the trader doesn't
	// necessarily make as many requests on recovery, if no accounts are
	// found for a certain number of tries.
	DefaultAccountKeyWindow uint32 = 500
)

// GenerateRecoveryKeys generates a list of key descriptors for all possible
// keys that could be used for trader accounts, up to a hard coded limit.
func GenerateRecoveryKeys(ctx context.Context,
	wallet lndclient.WalletKitClient) ([]*keychain.KeyDescriptor, error) {

	acctKeys := make([]*keychain.KeyDescriptor, DefaultAccountKeyWindow)
	for i := uint32(0); i < DefaultAccountKeyWindow; i++ {
		key, err := wallet.DeriveKey(ctx, &keychain.KeyLocator{
			Family: clmscript.AccountKeyFamily,
			Index:  i,
		})
		if err != nil {
			return nil, err
		}

		acctKeys[i] = key
	}
	return acctKeys, nil
}
