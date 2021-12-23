package account

import (
	"context"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/poolscript"
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

const (
	mainnetAuctioneerKey = "028e87bdd134238f8347f845d9ecc827b843d0d1e27cd" +
		"cb46da704d916613f4fce"
	testnetAuctioneerKey = "025dea8f5c67fb3bdfffb3123d2b7045dc0a3c75e822f" +
		"abb39eb357480e64c4a8a"
)

// GetAuctioneerData returns the auctioneer data for a given environment.
func GetAuctioneerData(network string) (string, uint32) {
	var auctioneerKey string
	var fstBlock uint32

	switch network {
	case "mainnet":
		auctioneerKey = mainnetAuctioneerKey
		fstBlock = 648168
	case "testnet":
		auctioneerKey = testnetAuctioneerKey
		fstBlock = 1834898
	}
	return auctioneerKey, fstBlock
}

// GenerateRecoveryKeys generates a list of key descriptors for all possible
// keys that could be used for trader accounts, up to a hard coHashded limit.
func GenerateRecoveryKeys(ctx context.Context, accountTarget uint32,
	wallet lndclient.WalletKitClient) (
	[]*keychain.KeyDescriptor, error) {

	acctKeys := make([]*keychain.KeyDescriptor, accountTarget)
	for i := uint32(0); i < accountTarget; i++ {
		key, err := wallet.DeriveKey(ctx, &keychain.KeyLocator{
			Family: poolscript.AccountKeyFamily,
			Index:  i,
		})
		if err != nil {
			return nil, err
		}

		acctKeys[i] = key
	}
	return acctKeys, nil
}
