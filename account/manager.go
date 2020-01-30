package account

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/account/watcher"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// minConfs and maxConfs represent the thresholds at both extremes for
	// valid number of confirmations on an account before it is considered
	// open.
	minConfs = 3
	maxConfs = 6

	// minAccountValue and maxAccountValue represent the thresholds at both
	// extremes for valid account values in satoshis. The maximum value is
	// based on the maximum channel size plus some leeway to account for
	// chain fees.
	minAccountValue btcutil.Amount = 100000
	maxAccountValue btcutil.Amount = minAccountValue + (1 << 24) - 1

	// minAccountExpiry and maxAccountExpiry represent the thresholds at
	// both extremes for valid account expirations.
	minAccountExpiry = 144       // One day worth of blocks.
	maxAccountExpiry = 144 * 365 // A year worth of blocks.
)

var (
	// errTxNotFound is an error returned when we attempt to locate a
	// transaction but we are unable to find it.
	errTxNotFound = errors.New("transaction not found")
)

// ManagerConfig contains all of the required dependencies for the Manager to
// carry out its duties.
type ManagerConfig struct {
	// Store is responsible for storing and retrieving account information
	// reliably.
	Store Store

	// Auctioneer provides us with the different ways we are able to
	// communicate with our auctioneer during the process of
	// opening/closing/modifying accounts.
	Auctioneer Auctioneer

	// Wallet handles all of our on-chain transaction interaction, whether
	// that is deriving keys, creating transactions, etc.
	Wallet lndclient.WalletKitClient

	// Signer is responsible for deriving shared secrets for accounts
	// between the trader and auctioneer.
	Signer lndclient.SignerClient

	// ChainNotifier is responsible for requesting confirmation and spend
	// notifications for accounts.
	ChainNotifier lndclient.ChainNotifierClient

	// TxSource is a source that provides us with transactions previously
	// broadcast by us.
	TxSource TxSource
}

// Manager is responsible for the management of accounts on-chain.
type Manager struct {
	started sync.Once
	stopped sync.Once

	cfg     ManagerConfig
	watcher *watcher.Watcher

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewManager instantiates a new Manager backed by the given config.
func NewManager(cfg *ManagerConfig) *Manager {
	m := &Manager{
		cfg:  *cfg,
		quit: make(chan struct{}),
	}

	m.watcher = watcher.New(&watcher.Config{
		ChainNotifier:       cfg.ChainNotifier,
		HandleAccountConf:   m.handleAccountConf,
		HandleAccountSpend:  m.handleAccountSpend,
		HandleAccountExpiry: m.handleAccountExpiry,
	})

	return m
}

// Start resumes all account on-chain operation after a restart.
func (m *Manager) Start() error {
	var err error
	m.started.Do(func() {
		err = m.start()
	})
	return err
}

// start resumes all account on-chain operation after a restart.
func (m *Manager) start() error {
	ctx := context.Background()

	// We'll start by resuming all of our accounts. This requires the
	// watcher to be started first.
	if err := m.watcher.Start(); err != nil {
		return err
	}

	// Then, we'll resume all complete accounts, followed by partial
	// accounts. If we were to do it the other way around, we'd resume
	// partial accounts twice.
	accounts, err := m.cfg.Store.Accounts()
	if err != nil {
		return fmt.Errorf("unable to retrieve accounts: %v", err)
	}
	for _, account := range accounts {
		if err := m.resumeAccount(ctx, account, true); err != nil {
			return fmt.Errorf("unable to resume account %x: %v",
				account.TraderKey.PubKey.SerializeCompressed(),
				err)
		}
	}

	return nil
}

// Stop safely stops any ongoing operations within the Manager.
func (m *Manager) Stop() {
	m.stopped.Do(func() {
		m.watcher.Stop()

		close(m.quit)
		m.wg.Wait()
	})
}

// InitAccount handles a request to create a new account with the provided
// parameters.
func (m *Manager) InitAccount(ctx context.Context, value btcutil.Amount,
	expiry uint32, bestHeight uint32) (*Account, error) {

	// First, make sure we have valid parameters to create the account.
	if err := validateAccountParams(value, expiry, bestHeight); err != nil {
		return nil, err
	}

	// We'll start by deriving a key for ourselves that we'll use in our
	// 2-of-2 multi-sig construction. and create an
	// output that will fund the account.
	keyDesc, err := m.cfg.Wallet.DeriveNextKey(
		ctx, int32(clmscript.AccountKeyFamily),
	)
	if err != nil {
		return nil, err
	}

	// With our key obtained, we'll reserve an account with our auctioneer,
	// who will provide us with their base key and our initial per-batch
	// key.
	reservation, err := m.cfg.Auctioneer.ReserveAccount(ctx)
	if err != nil {
		return nil, err
	}

	// We'll also need to compute a shared secret based on both base keys
	// (the trader and auctioneer's) to ensure only they are able to
	// successfully identify every past/future output of the account.
	secret, err := m.cfg.Signer.DeriveSharedKey(
		ctx, reservation.AuctioneerKey, &keyDesc.KeyLocator,
	)
	if err != nil {
		return nil, err
	}

	// With all of the details gathered, we'll persist our intent to create
	// an account to disk and proceed to fund it and wait for its
	// confirmation.
	account := &Account{
		Value:         value,
		Expiry:        expiry,
		TraderKey:     keyDesc,
		AuctioneerKey: reservation.AuctioneerKey,
		BatchKey:      reservation.InitialBatchKey,
		Secret:        secret,
		State:         StateInitiated,
		HeightHint:    bestHeight,
	}
	if err := m.cfg.Store.AddAccount(account); err != nil {
		return nil, err
	}

	log.Infof("Creating new account %x of %v that expires at height %v",
		keyDesc.PubKey.SerializeCompressed(), value, expiry)

	if err := m.resumeAccount(ctx, account, false); err != nil {
		return nil, err
	}

	return account, nil
}

// resumeAccount performs different operations based on the account's state.
// This method serves as a way to consolidate the logic of resuming accounts on
// startup and during normal operation.
func (m *Manager) resumeAccount(ctx context.Context, account *Account,
	onRestart bool) error {

	accountOutput, err := account.Output()
	if err != nil {
		return fmt.Errorf("unable to construct account output: %v", err)
	}

	var accountTx *wire.MsgTx
	switch account.State {
	// In StateInitiated, we'll attempt to fund our account.
	case StateInitiated:
		// If we're resuming the account from a restart, we'll want to
		// make sure we haven't created and broadcast a transaction for
		// this account already, so we'll inspect our TxSource to do so.
		createTx := true
		if onRestart {
			tx, err := m.locateTxByOutput(ctx, accountOutput)
			switch err {
			// If we do find one, we can rebroadcast it.
			case nil:
				accountTx = tx
				createTx = false

			// If we don't, we'll need to create one.
			case errTxNotFound:
				break

			default:
				return fmt.Errorf("unable to locate output "+
					"%x: %v", accountOutput.PkScript, err)
			}
		}

		if createTx {
			// TODO(wilmer): Expose fee rate and manual controls to
			// bump fees.
			tx, err := m.cfg.Wallet.SendOutputs(
				ctx, []*wire.TxOut{accountOutput},
				chainfee.FeePerKwFloor,
			)
			if err != nil {
				return err
			}
			accountTx = tx

			log.Infof("Funded new account %x with transaction %v",
				account.TraderKey.PubKey.SerializeCompressed(),
				tx.TxHash())
		}

		// With the transaction obtained, we'll locate the index of our
		// account output in the transaction to obtain our account
		// outpoint and store it to disk. This will be the main way we
		// identify our accounts, and is also required to watch for its
		// spend.
		outputIndex, ok := clmscript.LocateOutputScript(
			accountTx, accountOutput.PkScript,
		)
		if !ok {
			return fmt.Errorf("transaction %v does not include "+
				"expected script %x", accountTx.TxHash(),
				accountOutput.PkScript)
		}
		op := wire.OutPoint{Hash: accountTx.TxHash(), Index: outputIndex}

		err := m.cfg.Store.UpdateAccount(
			account, StateModifier(StatePendingOpen),
			OutPointModifier(op),
		)
		if err != nil {
			return err
		}

		fallthrough

	// In StatePendingOpen, we should already have broadcast a funding
	// transaction for the account, so the most we can do is attempt to
	// rebroadcast it and wait for its confirmation.
	case StatePendingOpen:
		// If we're resuming from a restart, we'll have to locate the
		// transaction in our TxSource by its hash. We should definitely
		// find one in this state, so if we don't, that would indicate
		// something has gone wrong.
		if onRestart {
			var err error
			accountTx, err = m.locateTxByHash(
				ctx, account.OutPoint.Hash,
			)
			if err != nil {
				return fmt.Errorf("unable to locate "+
					"transaction %v: %v",
					account.OutPoint.Hash, err)
			}
			err = m.cfg.Wallet.PublishTransaction(ctx, accountTx)
			if err != nil {
				return err
			}
		}

		// Send the account parameters over to the auctioneer so that
		// they're also aware of the account.
		err := m.cfg.Auctioneer.InitAccount(ctx, account)
		if err != nil {
			return err
		}

		// Proceed to watch for the account on-chain.
		numConfs := numConfsForValue(account.Value)
		log.Infof("Waiting for %v confirmation(s) of account %x",
			numConfs, account.TraderKey.PubKey.SerializeCompressed())
		err = m.watcher.WatchAccountConf(
			account.TraderKey.PubKey, account.OutPoint.Hash,
			accountOutput.PkScript, numConfs, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for confirmation: "+
				"%v", err)
		}

		fallthrough

	// In StateOpen, the funding transaction for the account has already
	// confirmed, so we only need to watch for its spend and expiration.
	case StateOpen:
		log.Infof("Watching account %x for spend and expiration",
			account.TraderKey.PubKey.SerializeCompressed())
		err := m.watcher.WatchAccountSpend(
			account.TraderKey.PubKey, account.OutPoint,
			accountOutput.PkScript, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for spend: %v", err)
		}
		err = m.watcher.WatchAccountExpiration(
			account.TraderKey.PubKey, account.Expiry,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for expiration: %v",
				err)
		}

	// In StateExpired, we'll wait for the account to be spent such that it
	// can be marked as closed if we decide to close it.
	case StateExpired:
		log.Infof("Watching expired account %x for spend",
			account.TraderKey)

		err = m.watcher.WatchAccountSpend(
			account.TraderKey.PubKey, account.OutPoint,
			accountOutput.PkScript, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for spend: %v", err)
		}

	default:
		return fmt.Errorf("unhandled account state %v", account.State)
	}

	return nil
}

// locateTxByOutput locates a transaction from the Manager's TxSource by one of
// its outputs. If a transaction is not found containing the output, then
// errTxNotFound is returned.
func (m *Manager) locateTxByOutput(ctx context.Context,
	output *wire.TxOut) (*wire.MsgTx, error) {

	txs, err := m.cfg.TxSource.ListTransactions(ctx)
	if err != nil {
		return nil, err
	}

	for _, tx := range txs {
		idx, ok := clmscript.LocateOutputScript(tx, output.PkScript)
		if !ok {
			continue
		}
		if tx.TxOut[idx].Value == output.Value {
			return tx, nil
		}
	}

	return nil, errTxNotFound
}

// locateTxByHash locates a transaction from the Manager's TxSource by its hash.
// If the transaction is not found, then errTxNotFound is returned.
func (m *Manager) locateTxByHash(ctx context.Context,
	hash chainhash.Hash) (*wire.MsgTx, error) {

	txs, err := m.cfg.TxSource.ListTransactions(ctx)
	if err != nil {
		return nil, err
	}

	for _, tx := range txs {
		if tx.TxHash() == hash {
			return tx, nil
		}
	}

	return nil, errTxNotFound
}

// handleAccountConf takes the necessary steps after detecting the confirmation
// of an account on-chain.
func (m *Manager) handleAccountConf(traderKey *btcec.PublicKey,
	confDetails *chainntnfs.TxConfirmation) error {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	// To not rely on the order of confirmation and block notifications, if
	// the account confirms at the same height as it expires, we can exit
	// now and let the account be marked as expired by the watcher.
	if confDetails.BlockHeight == account.Expiry {
		return nil
	}

	// Ensure we don't transition an account that's been closed back to open
	// if the account was closed before it was open.
	if account.State != StatePendingOpen {
		return nil
	}

	log.Infof("Account %x is now confirmed at height %v!",
		traderKey.SerializeCompressed(), confDetails.BlockHeight)

	return m.cfg.Store.UpdateAccount(account, StateModifier(StateOpen))
}

// handleAccountSpend handles the different spend paths of an account.
func (m *Manager) handleAccountSpend(traderKey *btcec.PublicKey,
	spendDetails *chainntnfs.SpendDetail) error {

	// TODO(wilmer): Handle different spend paths.
	return nil
}

// handleAccountExpiry marks an account as expired within the database.
func (m *Manager) handleAccountExpiry(traderKey *btcec.PublicKey) error {
	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	log.Infof("Account %x has expired as of height %v",
		traderKey.SerializeCompressed(), account.Expiry)

	err = m.cfg.Store.UpdateAccount(account, StateModifier(StateExpired))
	if err != nil {
		return err
	}

	return nil
}

// validateAccountParams ensures that a trader has provided sane parameters for
// the creation of a new account.
func validateAccountParams(value btcutil.Amount, expiry, bestHeight uint32) error {
	if value < minAccountValue {
		return fmt.Errorf("minimum account value allowed is %v",
			minAccountValue)
	}
	if value > maxAccountValue {
		return fmt.Errorf("maximum account value allowed is %v",
			maxAccountValue)
	}

	if expiry < bestHeight+minAccountExpiry {
		return fmt.Errorf("current minimum account expiry allowed is "+
			"height %v", bestHeight+minAccountExpiry)
	}
	if expiry > bestHeight+maxAccountExpiry {
		return fmt.Errorf("current maximum account expiry allowed is "+
			"height %v", bestHeight+maxAccountExpiry)
	}

	return nil
}

// numConfsForValue chooses an appropriate number of confirmations to wait for
// an account based on its initial value.
//
// TODO(wilmer): Determine the recommend number of blocks to wait for a
// particular output size given the current block reward and a user's "risk
// threshold" (basically a multiplier for the amount of work/fiat-burnt that
// would need to be done to undo N blocks).
func numConfsForValue(value btcutil.Amount) uint32 {
	confs := maxConfs * value / maxAccountValue
	if confs < minConfs {
		confs = minConfs
	}
	if confs > maxConfs {
		confs = maxConfs
	}
	return uint32(confs)
}
