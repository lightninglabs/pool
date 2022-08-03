package account

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
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

	// InitialBatchKey is the hard coded starting point for the auctioneer's
	// batch key in every environment.
	InitialBatchKey = "02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2" +
		"ae66afefb8eaf27f"

	// maxBatchCounter is the maximum number of batches that we consider
	// worth checking.
	//
	// NOTE: currently there are about 1100 batches (Jan 2022) on mainnet.
	maxBatchCounter = 5000
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

// DecodeAndParseKey decode and parse a btc public key.
func DecodeAndParseKey(key string) (*btcec.PublicKey, error) {
	kStr, err := hex.DecodeString(key)
	if err != nil {
		return nil, err
	}
	k, err := btcec.ParsePubKey(kStr)
	if err != nil {
		return nil, err
	}
	return k, nil
}

// BitcoinConfig defines exported config options for the connection to the
// btcd/bitcoind backend.
type BitcoinConfig struct {
	// Host is the bitcoind/btcd instance address.
	Host string

	// User is the bitcoind/btcd user name.
	User string

	// Password is the bitcoind/btcd password.
	Password string

	// HTTPPostMode uses HTTP Post mode if true.
	//
	// Note: bitcoind only supports this mode.
	HTTPPostMode bool

	// UseTLS use TLS to connect if ture.
	//
	// Note: bitcoind only supports non-TLS connections.
	UseTLS bool

	// TLSPath is the path to btcd's TLS certificate. This field is Ignored
	// if TLS is not enabled.
	TLSPath string `long:"tlspath" description:"Path to btcd's TLS certificate, if TLS is enabled"`
}

// RecoveryConfig contains all the required dependencies for carrying out the
// recovery process duties.
type RecoveryConfig struct {
	// Network to run recovery on.
	Network string

	// Number of accounts that we are trying to find.
	AccountTarget uint32

	// FirstBlock block marks the initial height for our search.
	FirstBlock uint32

	// LastBlock block marks the final height for our search.
	LastBlock uint32

	// CurrentBlockHeight is the best known height of the main chain.
	CurrentBlockHeight int64

	// BitcoinConfig defines exported config options for the connection to
	// the btcd/bitcoind backend.
	BitcoinConfig *BitcoinConfig

	// bitcoinClient is the rpc client used to interact with the bitcoin
	// backend.
	bitcoinClient *rpcclient.Client

	// Transactions is a list of all transactions that the lnd wallet is
	// currently aware of.
	Transactions []*wire.MsgTx

	// Signer is the signrpc client used to derive the shared key between
	// the server and client.
	Signer lndclient.SignerClient

	// Wallet is the walletrpc client used to derive all possible account
	// keys.
	Wallet lndclient.WalletKitClient

	// InitialBatchKey is the starting value for the auctioneer's batch key.
	InitialBatchKey *btcec.PublicKey

	// AuctioneerPubKey is the static, per-environment public key of the
	// auctioneer.
	AuctioneerPubKey *btcec.PublicKey

	// Quit is a channel that is closed when the server is shutting down.
	Quit chan struct{}
}

// getBitcoinConn returns a new rpc client for the btc backend node.
func getBitcoinConn(cfg *BitcoinConfig) (*rpcclient.Client, error) {
	// In case we use TLS and a certificate argument is provided, we need to
	// read that file and provide it to the RPC connection as byte slice.
	var rpcCert []byte
	if cfg.UseTLS && cfg.TLSPath != "" {
		certFile, err := os.Open(cfg.TLSPath)
		if err != nil {
			return nil, err
		}
		rpcCert, err = ioutil.ReadAll(certFile)
		if err != nil {
			return nil, err
		}
		if err := certFile.Close(); err != nil {
			return nil, err
		}
	}

	// Connect to the backend with the certs we just loaded.
	connCfg := &rpcclient.ConnConfig{
		Host:         cfg.Host,
		User:         cfg.User,
		Pass:         cfg.Password,
		HTTPPostMode: cfg.HTTPPostMode,
		DisableTLS:   !cfg.UseTLS,
		Certificates: rpcCert,
	}

	// Notice the notification parameter is nil since notifications are
	// not supported in HTTP POST mode.
	return rpcclient.New(connCfg, nil)
}

// getBlockTxs returns all the transactions in the block for the given height.
func getBlockTxs(client *rpcclient.Client, height int64) ([]*wire.MsgTx, error) {
	blockHash, err := client.GetBlockHash(height)
	if err != nil {
		return nil, err
	}

	block, err := client.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	return block.Transactions, nil
}

// RecoverAccounts tries to recover valid accounts using the given configuration.
func RecoverAccounts(ctx context.Context, cfg RecoveryConfig) ([]*Account,
	error) {

	client, err := getBitcoinConn(cfg.BitcoinConfig)
	if err != nil {
		return nil, err
	}
	cfg.bitcoinClient = client

	currentHeight, err := client.GetBlockCount()
	if err != nil {
		return nil, err
	}
	cfg.CurrentBlockHeight = currentHeight
	// Look up for expiry height of up to 1 year from the best known height.
	cfg.LastBlock = uint32(currentHeight) + 144*365

	accounts, err := recoverInitialState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Are we shutting down?
	select {
	case <-cfg.Quit:
		return nil, fmt.Errorf("server shutting down")
	default:
	}

	return updateAccountStates(cfg, accounts)
}

// recoverInitialState finds accounts in their initial state (creation).
func recoverInitialState(ctx context.Context, cfg RecoveryConfig) ([]*Account,
	error) {

	log.Debugf("Recovering initial states for %d accounts...",
		cfg.AccountTarget)

	possibleAccounts, err := recreatePossibleAccounts(ctx, cfg)
	if err != nil {
		return nil, err
	}

	accounts := findAccounts(cfg, possibleAccounts)
	log.Debugf("Found initial tx for %d/%d accounts", len(accounts),
		cfg.AccountTarget)

	return accounts, nil
}

// recreatePossibleAccounts returns a set of potentially valid accounts
// by generating a set of recovery keys.
func recreatePossibleAccounts(ctx context.Context,
	cfg RecoveryConfig) ([]*Account, error) {

	possibleAccounts := make([]*Account, 0, cfg.AccountTarget)

	// Prepare the keys we are going to try. Possibly not all of them will
	// be used.
	KeyDescriptors, err := GenerateRecoveryKeys(
		ctx, cfg.AccountTarget, cfg.Wallet,
	)
	if err != nil {
		return nil, fmt.Errorf("error generating keys: %v", err)
	}

	for _, keyDes := range KeyDescriptors {
		secret, err := cfg.Signer.DeriveSharedKey(
			ctx, cfg.AuctioneerPubKey, &keyDes.KeyLocator,
		)
		if err != nil {
			return nil, fmt.Errorf("error deriving shared key: %v",
				err)
		}

		acc := &Account{
			TraderKey:     keyDes,
			AuctioneerKey: cfg.AuctioneerPubKey,
			Secret:        secret,
			State:         StateOpen,
		}
		possibleAccounts = append(possibleAccounts, acc)
	}

	return possibleAccounts, nil
}

// findAccounts tries to find on-chain footprint for the creation of a set
// of possible pool accounts.
func findAccounts(cfg RecoveryConfig, possibleAccounts []*Account) []*Account {
	// The process looks like:
	//     - Fix the batch counter.
	//     - Fix a block height.
	//     - Recreate the script for each of the possible accounts.
	//         - Look for the script in all of the node txs.

	target := cfg.AccountTarget
	accounts := make([]*Account, 0, target)

	// Once we find an account, we remove it from the target list to avoid
	// finding later account modification transactions for an account we
	// already found an initial TX for. We'll walk the chain of updates in
	// a secondary step. We use the account index as the map key since that
	// is unique across accounts.
	remainingAccounts := make(map[uint32]*Account, len(possibleAccounts))
	for _, possibleAccount := range possibleAccounts {
		remainingAccounts[possibleAccount.TraderKey.Index] = possibleAccount
	}

	helper := &poolscript.RecoveryHelper{
		BatchKey:      cfg.InitialBatchKey,
		AuctioneerKey: cfg.AuctioneerPubKey,
	}

	for batchCounter := 0; batchCounter < maxBatchCounter; batchCounter++ {
		batchKey := helper.BatchKey

	accountLoop:
		for acctIdx, acc := range remainingAccounts {
			acc := acc
			traderKey := acc.TraderKey.PubKey

			helper.NextAccount(traderKey, acc.Secret)
			for h := cfg.FirstBlock; h < cfg.LastBlock; h++ {
				// Are we shutting down?
				select {
				case <-cfg.Quit:
					return nil
				default:
				}

				tx, idx, ok, err := helper.LocateAnyOutput(
					h, cfg.Transactions,
				)
				if err != nil {
					log.Debugf("Unable to generate script "+
						"height=%v, batch_key=%x, "+
						"trader_key=%x): %v", h,
						batchKey.SerializeCompressed(),
						traderKey.SerializeCompressed(),
						err)

					// Go to next account.
					continue accountLoop
				}

				// If it's NOT a match, keep trying.
				if !ok {
					continue
				}

				log.Debugf("Found initial account state for "+
					"account %x",
					traderKey.SerializeCompressed())

				// If it's a match, populate the account
				// information.
				acc.Expiry = h
				acc.Value = btcutil.Amount(
					tx.TxOut[idx].Value,
				)
				acc.BatchKey = batchKey
				acc.OutPoint = wire.OutPoint{
					Hash:  tx.TxHash(),
					Index: idx,
				}
				acc.LatestTx = tx
				acc.State = StateOpen

				// Remove the current account, so we don't try
				// to find it again.
				delete(remainingAccounts, acctIdx)

				accounts = append(accounts, acc)

				// If we already found all the accounts that
				// we were looking for quit the search.
				if len(accounts) == int(target) {
					return accounts
				}
			}
		}
		helper.NextBatchKey()

		if batchCounter > 0 && batchCounter%100 == 0 {
			log.Debugf("Tried looking for accounts in %d batches, "+
				"will try up to %d", batchCounter,
				maxBatchCounter)
		}
	}

	return accounts
}

// updateAccountStates tries to update the states for every provided
// account up to their latest state by following the on chain
// modification footprints.
func updateAccountStates(cfg RecoveryConfig,
	accounts []*Account) ([]*Account, error) {

	for i := cfg.FirstBlock; i < uint32(cfg.CurrentBlockHeight); i++ {
		blockHeight := i

		txs, err := getBlockTxs(cfg.bitcoinClient, int64(blockHeight))
		if err != nil {
			return nil, err
		}

		for idx, acc := range accounts {
			// It's safe to ignore accounts that are in a
			// terminal state.
			if acc.State == StateClosed {
				continue
			}

			tx, found := poolscript.MatchPreviousOutPoint(
				acc.OutPoint, txs,
			)
			if !found {
				// Go to next account.
				continue
			}

			newAcc, err := findAccountUpdate(cfg, acc, tx)
			if err != nil {
				// If we cannot find the account update
				// we assume it was closed/spent.
				acc.State = StateClosed
				acc.HeightHint = blockHeight

				traderKey := acc.TraderKey.PubKey
				log.Debugf("Account %x was spent in tx %v but "+
					"not re-created. Assuming account was "+
					"fully spent or closed",
					traderKey.SerializeCompressed(),
					tx.TxHash())

				continue
			}

			newAcc.HeightHint = blockHeight
			accounts[idx] = newAcc
		}
	}
	return accounts, nil
}

// findAccountUpdate tries to find the new account values after an update.
func findAccountUpdate(cfg RecoveryConfig, acc *Account,
	tx *wire.MsgTx) (*Account, error) {

	newAcc := acc.Copy()
	newAcc.BatchKey = poolscript.IncrementKey(newAcc.BatchKey)

	helper := &poolscript.RecoveryHelper{
		BatchKey:      newAcc.BatchKey,
		AuctioneerKey: cfg.AuctioneerPubKey,
	}

	helper.NextAccount(acc.TraderKey.PubKey, acc.Secret)
	// We only have a tx so if there is a match we know what tx it is.
	_, idx, ok, err := helper.LocateAnyOutput(
		newAcc.Expiry, []*wire.MsgTx{tx},
	)
	if err != nil {
		return nil, fmt.Errorf("account update not found")
	}
	if ok {
		newAcc.Value = btcutil.Amount(tx.TxOut[idx].Value)
		newAcc.OutPoint = wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: idx,
		}
		newAcc.LatestTx = tx

		return newAcc, nil
	}

	// If the update included a new expiration date we need to brute force
	// our new expiration date again.
	for height := cfg.FirstBlock; height <= cfg.LastBlock; height++ {
		_, idx, ok, err := helper.LocateAnyOutput(
			height, []*wire.MsgTx{tx},
		)
		if err != nil {
			return nil, fmt.Errorf("account update not found")
		}
		if !ok {
			continue
		}

		newAcc.Expiry = height
		newAcc.Value = btcutil.Amount(tx.TxOut[idx].Value)
		newAcc.OutPoint = wire.OutPoint{
			Hash:  tx.TxHash(),
			Index: idx,
		}
		newAcc.LatestTx = tx

		return newAcc, nil
	}

	return nil, fmt.Errorf("account update not found")
}

// GenerateRecoveryKeys generates a list of key descriptors for all possible
// keys that could be used for trader accounts, up to a hard coded limit.
func GenerateRecoveryKeys(ctx context.Context, accountTarget uint32,
	wallet lndclient.WalletKitClient) ([]*keychain.KeyDescriptor, error) {

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

// AdvanceAccountDerivationIndex asserts that the internal wallet has at least
// the given minimum index for deriving account keys.
func AdvanceAccountDerivationIndex(ctx context.Context, minimumIndex uint32,
	wallet lndclient.WalletKitClient, chainParams *chaincfg.Params) error {

	allAccounts, err := wallet.ListAccounts(
		ctx, "", walletrpc.AddressType_UNKNOWN,
	)
	if err != nil {
		return fmt.Errorf("error listing accounts: %v", err)
	}

	poolAccountsPath := fmt.Sprintf("m/%d'/%d'/%d'",
		keychain.BIP0043Purpose, chainParams.HDCoinType,
		poolscript.AccountKeyFamily)

	// Before we change anything in the wallet, let's check if maybe we
	// already have enough keys?
	for _, acct := range allAccounts {
		if acct.DerivationPath == poolAccountsPath {
			// If we did find our account in the list and already
			// have enough keys derived, we don't need to do
			// anything here. This is the key _count_, so it must be
			// strictly greater than the minimum _index_ that we
			// want.
			if acct.ExternalKeyCount > minimumIndex {
				log.Debugf("Account %s already has %d "+
					"external keys (want minimum index "+
					"%d), not deriving any keys",
					poolAccountsPath, acct.ExternalKeyCount,
					minimumIndex)

				return nil
			}

			break
		}
	}

	for {
		key, err := wallet.DeriveNextKey(
			ctx, int32(poolscript.AccountKeyFamily),
		)
		if err != nil {
			return fmt.Errorf("error deriving next key: %v", err)
		}

		log.Debugf("Derived next account key with index %d", key.Index)
		if key.Index >= minimumIndex {
			return nil
		}
	}
}
