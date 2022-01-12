package account

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
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

	// InitialBatchKey is the hard coded starting point for the auctioneer's
	// batch key in every environment.
	InitialBatchKey = "02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2" +
		"ae66afefb8eaf27f"
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
	k, err := btcec.ParsePubKey(kStr, btcec.S256())
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

// RecoverAccounts tries to recover valid accounts using the given configuration.
func RecoverAccounts(ctx context.Context, cfg RecoveryConfig) ([]*Account,
	error) {

	client, err := getBitcoinConn(&BitcoinConfig{})
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

	// TODO (positiveblue): recover initial state

	return nil, nil
}

// updateAccountStates tries to update the states for every provided
// account up to their latest state by following the on chain
// modification footprints.
func updateAccountStates(cfg RecoveryConfig,
	accounts []*Account) ([]*Account, error) {

	// TODO (positiveblue): update account states

	return nil, nil
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
