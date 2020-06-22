package account

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
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

// findTraderKeyIndex tries to find the derivation index of a trader's account
// public key within the account family. Only DefaultAccountKeyWindow number of
// keys will be tried. If the index wasn't found up to that number, an error is
// returned.
func findTraderKeyIndex(ctx context.Context, wallet lndclient.WalletKitClient,
	traderKey *btcec.PublicKey) (uint32, error) {

	for i := uint32(0); i < DefaultAccountKeyWindow; i++ {
		desc, err := wallet.DeriveKey(ctx, &keychain.KeyLocator{
			Family: clmscript.AccountKeyFamily,
			Index:  i,
		})
		if err != nil {
			return 0, err
		}

		// We've found what we're looking for.
		if desc.PubKey.IsEqual(traderKey) {
			return i, nil
		}
	}

	// Should never happen unless the wrong lnd instance is connected or
	// someone has more than DefaultAccountKeyWindow accounts in which case
	// this constant probably needs to be increased.
	return 0, fmt.Errorf("account key unable to locate in %d keys, "+
		"possibly the connected lnd instance doesn't use the correct "+
		"seed", DefaultAccountKeyWindow)
}
