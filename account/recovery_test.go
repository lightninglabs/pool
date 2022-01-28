package account

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getInitialBatchKey returns the first batchkey value.
func getInitialBatchKey() *btcec.PublicKey {
	batchKey, _ := DecodeAndParseKey(InitialBatchKey)

	return batchKey
}

// getInitialBatchKey returns an auctioneer key for testing.
func getAuctioneerKey() *btcec.PublicKey {
	key, _ := DecodeAndParseKey(testnetAuctioneerKey)
	return key
}

// getSecret random (fixed) secret for testing.
func getSecret() [32]byte {
	return [32]byte{102, 97, 108, 99, 111, 110}
}

// getAccount returns a new account pointer.
func getAccount(key *btcec.PublicKey, traderKey string, idx uint32) *Account {
	pubKey, _ := DecodeAndParseKey(traderKey)
	kd := &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Index: idx,
		},
		PubKey: pubKey,
	}

	return &Account{
		TraderKey:     kd,
		AuctioneerKey: key,
		Secret:        getSecret(),
	}
}

var findAccountTestCases = []struct {
	name             string
	config           RecoveryConfig
	traderKeys       []string
	expectedAccounts int
}{{
	name: "we are able to find the two initial accounts successfully",
	config: RecoveryConfig{
		AccountTarget:    2,
		InitialBatchKey:  getInitialBatchKey(),
		AuctioneerPubKey: getAuctioneerKey(),
		FirstBlock:       100,
		LastBlock:        200,
		Transactions:     []*wire.MsgTx{},
	},
	traderKeys: []string{
		"0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366a2e1" +
			"51fcee7",
		"027b27d419683ea2f58feff2da1a49c7defbddb77da0ab1e514c4c44961" +
			"c792d07",
	},
	expectedAccounts: 2,
}, {
	name: "if we already found the account target we stop early",
	config: RecoveryConfig{
		AccountTarget:    1,
		InitialBatchKey:  getInitialBatchKey(),
		AuctioneerPubKey: getAuctioneerKey(),
		FirstBlock:       100,
		LastBlock:        200,
		Transactions:     []*wire.MsgTx{},
	},
	traderKeys: []string{
		"0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366a2e1" +
			"51fcee7",
		"027b27d419683ea2f58feff2da1a49c7defbddb77da0ab1e514c4c44961" +
			"c792d07",
	},
	expectedAccounts: 1,
}}

// TestFindInitialAccountState checks that we are able to find the initial state
// for some lost accounts.
func TestFindInitialAccountState(t *testing.T) {
	for _, tc := range findAccountTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			possibleAccounts := make(
				[]*Account, 0, len(tc.traderKeys),
			)

			for idx, tk := range tc.traderKeys {
				acc := getAccount(
					tc.config.AuctioneerPubKey, tk,
					uint32(idx),
				)
				possibleAccounts = append(possibleAccounts, acc)

				batchKey := poolscript.IncrementKey(
					tc.config.InitialBatchKey,
				)
				script, _ := poolscript.AccountScript(
					177, acc.TraderKey.PubKey,
					tc.config.AuctioneerPubKey,
					batchKey, acc.Secret,
				)

				tc.config.Transactions = append(
					tc.config.Transactions,
					&wire.MsgTx{
						TxOut: []*wire.TxOut{
							{
								PkScript: script,
							},
						},
					},
				)
			}

			accounts := findAccounts(tc.config, possibleAccounts)

			require.Len(t, accounts, tc.expectedAccounts)
		})
	}
}

var findAccountUpdateTestCases = []struct {
	name           string
	config         RecoveryConfig
	traderKey      string
	expectedExpiry uint32
	expectedValue  int64
	expectedError  string
}{{
	name: "match account update with the same expiry height",
	config: RecoveryConfig{
		FirstBlock:       100,
		LastBlock:        200,
		AuctioneerPubKey: getAuctioneerKey(),
	},
	traderKey: "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366" +
		"a2e151fcee7",
	expectedExpiry: 120,
	expectedValue:  100,
}, {
	name: "match account update with new expiry height",
	config: RecoveryConfig{
		FirstBlock:       100,
		LastBlock:        200,
		AuctioneerPubKey: getAuctioneerKey(),
	},
	traderKey: "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366" +
		"a2e151fcee7",
	expectedExpiry: 180,
	expectedValue:  100,
}, {
	name: "match account update with multiple batchKey updates (same expiry)",
	config: RecoveryConfig{
		FirstBlock:       100,
		LastBlock:        200,
		AuctioneerPubKey: getAuctioneerKey(),
	},
	traderKey: "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366" +
		"a2e151fcee7",
	expectedExpiry: 120,
	expectedValue:  100,
}, {
	name: "match account update with multiple batchKey updates (new expiry)",
	config: RecoveryConfig{
		FirstBlock:       100,
		LastBlock:        200,
		AuctioneerPubKey: getAuctioneerKey(),
	},
	traderKey: "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366" +
		"a2e151fcee7",
	expectedExpiry: 180,
	expectedValue:  100,
}, {
	name: "unable to match update returns an error",
	config: RecoveryConfig{
		FirstBlock:       100,
		LastBlock:        200,
		AuctioneerPubKey: getAuctioneerKey(),
	},
	traderKey: "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366" +
		"a2e151fcee7",
	expectedExpiry: 220,
	expectedValue:  100,
	expectedError:  "account update not found",
}}

// TestFindAccountUpdate checks that we are able to find the changes of
// an update.
func TestFindAccountUpdate(t *testing.T) {
	for _, tc := range findAccountUpdateTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			acc := getAccount(getAuctioneerKey(), tc.traderKey, 2)
			acc.BatchKey = getInitialBatchKey()
			acc.Expiry = 120
			acc.Value = 130

			batchKey := poolscript.IncrementKey(acc.BatchKey)
			script, _ := poolscript.AccountScript(
				tc.expectedExpiry, acc.TraderKey.PubKey,
				acc.AuctioneerKey, batchKey, acc.Secret,
			)

			tx := &wire.MsgTx{
				TxOut: []*wire.TxOut{
					{
						Value:    tc.expectedValue,
						PkScript: script,
					},
				},
			}

			res, err := findAccountUpdate(
				tc.config, acc, tx,
			)
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
				return
			}
			require.NoError(t, err)

			require.Equal(t, tc.expectedExpiry, res.Expiry)

			require.Equal(
				t, btcutil.Amount(tc.expectedValue), res.Value,
			)
		})
	}
}

// TestGenerateRecoveryKeys tests that a certain number of keys can be created
// for account recovery.
func TestGenerateRecoveryKeys(t *testing.T) {
	t.Parallel()

	walletKit := test.NewMockWalletKit()
	keys, err := GenerateRecoveryKeys(
		context.Background(), DefaultAccountKeyWindow, walletKit,
	)
	if err != nil {
		t.Fatalf("could not generate keys: %v", err)
	}

	if len(keys) != int(DefaultAccountKeyWindow) {
		t.Fatalf("unexpected number of keys, got %d wanted %d",
			len(keys), DefaultAccountKeyWindow)
	}
}
