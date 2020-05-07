package account

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/lightninglabs/agora/client/account/watcher"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
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

// witnessType denotes the possible witness types of an account.
type witnessType uint8

const (
	// expiryWitness is the type used for a witness taking the expiration
	// path of an account.
	expiryWitness witnessType = iota

	// multiSigWitness is the type used for a witness taking the multi-sig
	// path of an account.
	multiSigWitness
)

// spendPackage tracks useful information regarding an account spend.
type spendPackage struct {
	// tx is the spending transaction of the account.
	tx *wire.MsgTx

	// witnessScript is the witness script of the account input being spent.
	witnessScript []byte

	// ourSig is our signature of the spending transaction above. If the
	// spend is taking the multi-sig path, then the auctioneer's signature
	// will be required as well for a valid spend.
	ourSig []byte
}

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
	// between the trader and auctioneer and signing account-related
	// transactions.
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

	// watchMtx guards access to watchingExpiry.
	watchMtx sync.Mutex

	// watchingExpiry is the set of accounts we're currently tracking the
	// expiration of.
	watchingExpiry map[[33]byte]struct{}

	// pendingBatchMtx guards access to any database calls involving pending
	// batches. This is mostly used to prevent race conditions when handling
	// multiple accounts spends as part of a batch that we didn't receive a
	// Finalize message for.
	pendingBatchMtx sync.Mutex

	wg   sync.WaitGroup
	quit chan struct{}
}

// NewManager instantiates a new Manager backed by the given config.
func NewManager(cfg *ManagerConfig) *Manager {
	m := &Manager{
		cfg:            *cfg,
		watchingExpiry: make(map[[33]byte]struct{}),
		quit:           make(chan struct{}),
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

	// In StatePendingUpdate, we've processed an account update due to
	// either a matched order or trader modification, so we'll need to wait
	// for its confirmation. Once it confirms, handleAccountConf will take
	// care of the rest of the flow.
	case StatePendingUpdate:
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

	// In StateOpen, the funding transaction for the account has already
	// confirmed, so we only need to watch for its spend and expiration and
	// register for account updates.
	case StateOpen:
		if err := m.handleStateOpen(ctx, account); err != nil {
			return err
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

	// In StatePendingClosed, we'll wait for the account's closing
	// transaction to confirm so that we can transition the account to its
	// final state.
	case StatePendingClosed:
		err := m.cfg.Wallet.PublishTransaction(ctx, account.CloseTx)
		if err != nil {
			return err
		}

		log.Infof("Watching account %x for spend", account.TraderKey)
		err = m.watcher.WatchAccountSpend(
			account.TraderKey.PubKey, account.OutPoint,
			accountOutput.PkScript, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for spend: %v", err)
		}

	// If the account has already  been closed, there's nothing to be done.
	case StateClosed:
		break

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

// handleStateOpen performs the necessary operations for accounts found in
// StateOpen.
func (m *Manager) handleStateOpen(ctx context.Context, account *Account) error {
	var traderKey [33]byte
	copy(traderKey[:], account.TraderKey.PubKey.SerializeCompressed())

	log.Infof("Watching spend of %v for account %x", account.OutPoint,
		traderKey)

	accountOutput, err := account.Output()
	if err != nil {
		return err
	}

	err = m.watcher.WatchAccountSpend(
		account.TraderKey.PubKey, account.OutPoint,
		accountOutput.PkScript, account.HeightHint,
	)
	if err != nil {
		return fmt.Errorf("unable to watch for spend: %v", err)
	}

	// Make sure we don't track the expiry again if we don't have to.
	m.watchMtx.Lock()
	if _, ok := m.watchingExpiry[traderKey]; !ok {
		err = m.watcher.WatchAccountExpiration(
			account.TraderKey.PubKey, account.Expiry,
		)
		if err != nil {
			m.watchMtx.Unlock()
			return fmt.Errorf("unable to watch for expiration: %v",
				err)
		}
	}
	m.watchMtx.Unlock()

	// Now that we have an open account, subscribe for updates to it to the
	// server. We subscribe for the account instead of the individual orders
	// because all signing operations will need to be executed on an account
	// level anyway. And we might end up executing multiple orders for the
	// same account in one batch. The messages from the server are received
	// and dispatched to the correct manager by the rpcServer.
	err = m.cfg.Auctioneer.SubscribeAccountUpdates(ctx, account)
	if err != nil {
		return fmt.Errorf("unable to subscribe for account updates: %v",
			err)
	}

	return nil
}

// handleAccountConf takes the necessary steps after detecting the confirmation
// of an account on-chain.
func (m *Manager) handleAccountConf(traderKey *btcec.PublicKey,
	confDetails *chainntnfs.TxConfirmation) error {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	// Ensure we don't transition an account that's been closed back to open
	// if the account was closed before it was open.
	if account.State != StatePendingOpen {
		return nil
	}

	log.Infof("Account %x is now confirmed at height %v!",
		traderKey.SerializeCompressed(), confDetails.BlockHeight)

	// Mark the account as open and proceed with the rest of the flow.
	err = m.cfg.Store.UpdateAccount(account, StateModifier(StateOpen))
	if err != nil {
		return err
	}

	return m.handleStateOpen(context.Background(), account)
}

// handleAccountSpend handles the different spend paths of an account. If an
// account is spent by the expiration path, it'll always be marked as closed
// thereafter. If it spent by the cooperative path with the auctioneer, then the
// account will only remain open if the spending transaction recreates the
// account with the expected next account script. Otherwise, it is also marked
// as closed.
func (m *Manager) handleAccountSpend(traderKey *btcec.PublicKey,
	spendDetails *chainntnfs.SpendDetail) error {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	// We'll need to perform different operations based on the witness of
	// the spending input of the account.
	spendTx := spendDetails.SpendingTx
	spendWitness := spendTx.TxIn[spendDetails.SpenderInputIndex].Witness

	switch {
	// If the witness is for a spend of the account expiration path, then
	// we'll mark the account as closed as the account has expired and all
	// the funds have been withdrawn.
	case clmscript.IsExpirySpend(spendWitness):
		break

	// If the witness is for a multi-sig spend, then either an order by the
	// trader was matched, or the account was closed. If it was closed, then
	// the account output shouldn't have been recreated.
	case clmscript.IsMultiSigSpend(spendWitness):
		// If there's a pending batch which has yet to be completed,
		// we'll mark it as so now. This can happen if the trader is not
		// connected to the auctioneer when the auctioneer sends them
		// the finalize message.
		//
		// We'll acquire the pending batch lock to ensure that there
		// aren't multiple handleAccountSpend threads (in the case of
		// multiple accounts participating in a batch) attempting to
		// mark the same batch as complete and prevent entering into an
		// erroneous state.
		m.pendingBatchMtx.Lock()
		err := m.cfg.Store.PendingBatch()
		switch err {
		// If there's no pending batch, we can proceed as normal.
		case ErrNoPendingBatch:
			break

		// If there is, we'll commit it and refresh the account state.
		case nil:
			if err := m.cfg.Store.MarkBatchComplete(); err != nil {
				m.pendingBatchMtx.Unlock()
				return err
			}
			account, err = m.cfg.Store.Account(traderKey)
			if err != nil {
				m.pendingBatchMtx.Unlock()
				return err
			}

		default:
			m.pendingBatchMtx.Unlock()
			return err
		}
		m.pendingBatchMtx.Unlock()

		// An account cannot be spent without our knowledge, so we'll
		// assume we always persist account updates before a broadcast
		// of the spending transaction. Therefore, since we should
		// already have the updates applied, we can just look for our
		// current output in the transaction.
		accountOutput, err := account.Output()
		if err != nil {
			return err
		}
		_, ok := clmscript.LocateOutputScript(
			spendTx, accountOutput.PkScript,
		)
		if ok {
			// Proceed with the rest of the flow.
			return m.resumeAccount(
				context.Background(), account, false,
			)
		}

	default:
		return fmt.Errorf("unknown spend witness %x", spendWitness)
	}

	log.Infof("Account %x has been closed on-chain with transaction %v",
		account.TraderKey.PubKey.SerializeCompressed(), spendTx.TxHash())

	// Write the spending transaction once again in case the one we
	// previously broadcast was replaced with a higher fee one.
	return m.cfg.Store.UpdateAccount(
		account, StateModifier(StateClosed), CloseTxModifier(spendTx),
	)
}

// handleAccountExpiry marks an account as expired within the database.
func (m *Manager) handleAccountExpiry(traderKey *btcec.PublicKey) error {
	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	// If the account has already been closed or is in the process of doing
	// so, there's no need to mark it as expired.
	if account.State == StatePendingClosed || account.State == StateClosed {
		return nil
	}

	log.Infof("Account %x has expired as of height %v",
		traderKey.SerializeCompressed(), account.Expiry)

	err = m.cfg.Store.UpdateAccount(account, StateModifier(StateExpired))
	if err != nil {
		return err
	}

	return nil
}

// CloseAccount attempts to close the account associated with the given trader
// key. Closing the account requires a signature of the auctioneer since the
// account is composed of a 2-of-2 multi-sig. The account is closed to a P2WPKH
// output of the account's trader key.
func (m *Manager) CloseAccount(ctx context.Context, traderKey *btcec.PublicKey,
	closeOutputs []*wire.TxOut, bestHeight uint32) (*wire.MsgTx, error) {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return nil, err
	}

	// Make sure the account hasn't already been closed, or is in the
	// process of doing so.
	if account.State == StatePendingClosed || account.State == StateClosed {
		return nil, errors.New("account has already been closed")
	}

	// TODO(wilmer): Reject if account has pending orders.

	// TODO(wilmer): Expose fee rate or allow fee bump.
	feeRate := chainfee.FeePerKwFloor
	witnessType := determineWitnessType(account, bestHeight)

	// If no outputs were provided, we'll close the account to an output
	// under the backing lnd node's control.
	if len(closeOutputs) == 0 {
		output, err := m.toWalletOutput(
			ctx, account.Value, feeRate, witnessType,
		)
		if err != nil {
			return nil, err
		}
		closeOutputs = append(closeOutputs, output)
	}

	_, spendPkg, err := m.spendAccount(
		ctx, account, closeOutputs, witnessType, bestHeight,
	)
	if err != nil {
		return nil, err
	}

	return spendPkg.tx, nil
}

// spendAccount houses most of the logic required to properly spend an account
// by creating the spending transaction, updating persisted account states,
// requesting a signature from the auctioneer if necessary, and finally
// broadcasting the spending transaction. These operations are performed in this
// order to ensure trader are able to resume the spend of an account upon
// restarts if they happen to shutdown mid-process.
func (m *Manager) spendAccount(ctx context.Context, account *Account,
	outputs []*wire.TxOut, witnessType witnessType,
	bestHeight uint32) (*Account, *spendPackage, error) {

	// Create the spending transaction of an account based on the provided
	// witness type.
	var (
		spendPkg *spendPackage
		err      error
	)
	switch witnessType {
	case expiryWitness:
		spendPkg, err = m.spendAccountExpiry(
			ctx, account, outputs, bestHeight,
		)

	case multiSigWitness:
		spendPkg, err = m.createSpendTx(ctx, account, outputs, 0)
	}
	if err != nil {
		return nil, nil, err
	}

	// With the transaction crafted, update our on-disk state and broadcast
	// the transaction.
	modifiers := []Modifier{
		StateModifier(StatePendingClosed), CloseTxModifier(spendPkg.tx),
	}
	err = m.cfg.Store.UpdateAccount(account, modifiers...)
	if err != nil {
		return nil, nil, err
	}

	// If we require the auctioneer's signature, request it now.
	if witnessType == multiSigWitness {
		witness, err := m.constructMultiSigWitness(
			ctx, account, spendPkg,
		)
		if err != nil {
			return nil, nil, err
		}
		// TODO(wilmer): Use proper input index when multiple inputs are
		// supported.
		spendPkg.tx.TxIn[0].Witness = witness
	}

	if err := m.cfg.Wallet.PublishTransaction(ctx, spendPkg.tx); err != nil {
		return nil, nil, err
	}

	return account, spendPkg, nil
}

// determineWitnessType determines the appropriate witness type to use for the
// spending transaction for an account based on whether it has expired or not.
func determineWitnessType(account *Account, bestHeight uint32) witnessType {
	if account.State == StateExpired || bestHeight >= account.Expiry {
		return expiryWitness
	}
	return multiSigWitness
}

// spendAccountExpiry creates the closing transaction of an account based on the
// expiration script path and signs it. bestHeight is used as the lock time of
// the transaction in order to satisfy the output's CHECKLOCKTIMEVERIFY.
func (m *Manager) spendAccountExpiry(ctx context.Context, account *Account,
	outputs []*wire.TxOut, bestHeight uint32) (*spendPackage, error) {

	spendPkg, err := m.createSpendTx(ctx, account, outputs, bestHeight)
	if err != nil {
		return nil, err
	}

	spendPkg.tx.TxIn[0].Witness = clmscript.SpendExpiry(
		spendPkg.witnessScript, spendPkg.ourSig,
	)

	return spendPkg, nil
}

// constructMultiSigWitness requests a signature from the auctioneer for the
// given spending transaction of an account and returns the fully constructed
// witness to spend the account input.
func (m *Manager) constructMultiSigWitness(ctx context.Context,
	account *Account, spendPkg *spendPackage) (wire.TxWitness, error) {

	auctioneerSig, err := m.cfg.Auctioneer.CloseAccount(
		ctx, account.TraderKey.PubKey, spendPkg.tx.TxOut,
	)
	if err != nil {
		return nil, err
	}

	return clmscript.SpendMultiSig(
		spendPkg.witnessScript, spendPkg.ourSig, auctioneerSig,
	), nil
}

// createSpendTx creates the spending transaction of an account and signs it.
// If the spending transaction takes the expiration path, bestHeight is used as
// the lock time of the transaction, otherwise it is 0. The transaction has its
// inputs and outputs sorted according to BIP-69.
func (m *Manager) createSpendTx(ctx context.Context, account *Account,
	outputs []*wire.TxOut, bestHeight uint32) (*spendPackage, error) {

	// Construct the transaction that we'll sign.
	tx := wire.NewMsgTx(2)
	tx.LockTime = bestHeight
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: account.OutPoint})
	for _, output := range outputs {
		tx.AddTxOut(output)
	}

	// The transaction should have its inputs and outputs sorted according
	// to BIP-69.
	txsort.InPlaceSort(tx)

	// Ensure the transaction crafted passes some basic sanity checks before
	// we attempt to sign it.
	if err := sanityCheckAccountSpendTx(tx, account); err != nil {
		return nil, err
	}

	// Gather the remaining components required to sign the transaction
	// fully.
	sigHashes := txscript.NewTxSigHashes(tx)
	sigHashType := txscript.SigHashAll

	// TODO(wilmer): Should sign proper input once multiple inputs are
	// supported.
	witnessScript, ourSig, err := m.signAccountInput(
		ctx, tx, account, sigHashType, sigHashes,
	)
	if err != nil {
		return nil, err
	}

	return &spendPackage{
		tx:            tx,
		witnessScript: witnessScript,
		ourSig:        ourSig,
	}, nil
}

// createNewAccountOutput creates the next account output in the sequence for a
// an account spending transaction that spends to the provided outputs at the
// given fee rate.
func createNewAccountOutput(account *Account, outputs []*wire.TxOut,
	witnessType witnessType, feeRate chainfee.SatPerKWeight) (*wire.TxOut,
	error) {

	// To determine the new value of the account, we'll need to subtract the
	// values of all additional outputs and the resulting fee of the
	// transaction, which we'll need to compute based on its weight.
	//
	// Right off the bat, we'll add weight estimates for the existing
	// account output that we're spending, and the new account output being
	// created.
	var accountInputWitnessSize int
	switch witnessType {
	case expiryWitness:
		accountInputWitnessSize = clmscript.ExpiryWitnessSize
	case multiSigWitness:
		accountInputWitnessSize = clmscript.MultiSigWitnessSize
	default:
		return nil, fmt.Errorf("unknown witness type %v", witnessType)
	}

	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(accountInputWitnessSize)
	weightEstimator.AddP2WSHOutput()

	inputTotal := account.Value

	// We'll then add the weight estimates for any additional outputs
	// provided, keeping track of the total output value sum as we go.
	var outputTotal btcutil.Amount
	for _, out := range outputs {
		// To determine the proper weight of the output, we'll need to
		// know its type.
		pkScript, err := txscript.ParsePkScript(out.PkScript)
		if err != nil {
			return nil, fmt.Errorf("unable to parse output "+
				"script %x: %v", out.PkScript, err)
		}

		switch pkScript.Class() {
		case txscript.ScriptHashTy:
			weightEstimator.AddP2SHOutput()
		case txscript.WitnessV0PubKeyHashTy:
			weightEstimator.AddP2WKHOutput()
		case txscript.WitnessV0ScriptHashTy:
			weightEstimator.AddP2WSHOutput()
		default:
			return nil, fmt.Errorf("unsupported output script %x",
				out.PkScript)
		}

		outputTotal += btcutil.Amount(out.Value)
	}

	// With the weight estimated, compute the fee, which we'll then subtract
	// from our input total and ensure our new account value isn't below our
	// required minimum.
	fee := feeRate.FeeForWeight(int64(weightEstimator.Weight()))
	newAmount := inputTotal - outputTotal - fee
	if newAmount < minAccountValue {
		return nil, fmt.Errorf("new account value is below accepted "+
			"minimum of %v", minAccountValue)
	}

	// Use the next output script in the sequence to avoid script reuse.
	newPkScript, err := account.NextOutputScript()
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    int64(newAmount),
		PkScript: newPkScript,
	}, nil
}

// sanityCheckAccountSpendTx ensures that the spending transaction of an account
// is well-formed by performing various sanity checks on its inputs and outputs.
func sanityCheckAccountSpendTx(tx *wire.MsgTx, account *Account) error {
	err := blockchain.CheckTransactionSanity(btcutil.NewTx(tx))
	if err != nil {
		return err
	}

	// CheckTransactionSanity doesn't have enough context to attempt fee
	// calculation, but we do.
	//
	// TODO(wilmer): Calculate the fee for this transaction and assert that
	// it is greater than the lowest possible fee for it?
	inputTotal := account.Value
	var outputTotal btcutil.Amount
	for _, output := range tx.TxOut {
		outputTotal += btcutil.Amount(output.Value)
	}

	if inputTotal < outputTotal {
		return fmt.Errorf("output value of %v exceeds input value of %v",
			outputTotal, inputTotal)
	}

	return nil
}

// signAccountInput signs the account input in the spending transaction of an
// account. If the account is being spent with cooperation of the auctioneer,
// their signature will be required as well.
func (m *Manager) signAccountInput(ctx context.Context, tx *wire.MsgTx,
	account *Account, sigHashType txscript.SigHashType,
	sigHashes *txscript.TxSigHashes) ([]byte, []byte, error) {

	traderKeyTweak := clmscript.TraderKeyTweak(
		account.BatchKey, account.Secret, account.TraderKey.PubKey,
	)
	witnessScript, err := clmscript.AccountWitnessScript(
		account.Expiry, account.TraderKey.PubKey, account.AuctioneerKey,
		account.BatchKey, account.Secret,
	)
	if err != nil {
		return nil, nil, err
	}

	accountOutput, err := account.Output()
	if err != nil {
		return nil, nil, err
	}

	signDesc := &input.SignDescriptor{
		KeyDesc: keychain.KeyDescriptor{
			KeyLocator: account.TraderKey.KeyLocator,
		},
		SingleTweak:   traderKeyTweak,
		WitnessScript: witnessScript,
		Output:        accountOutput,
		HashType:      sigHashType,
		InputIndex:    0,
		SigHashes:     sigHashes,
	}
	sigs, err := m.cfg.Signer.SignOutputRaw(
		ctx, tx, []*input.SignDescriptor{signDesc},
	)
	if err != nil {
		return nil, nil, err
	}

	// We'll need to re-append the sighash flag since SignOutputRaw strips
	// it.
	ourSig := append(sigs[0], byte(signDesc.HashType))

	return witnessScript, ourSig, nil
}

// toWalletOutput returns an output under the backing lnd node's control to
// sweep the funds of an account to.
func (m *Manager) toWalletOutput(ctx context.Context,
	accountValue btcutil.Amount, feeRate chainfee.SatPerKWeight,
	witnessType witnessType) (*wire.TxOut, error) {

	// Determine the appropriate witness size based on the type.
	var witnessSize int
	switch witnessType {
	case expiryWitness:
		witnessSize = clmscript.ExpiryWitnessSize
	case multiSigWitness:
		witnessSize = clmscript.MultiSigWitnessSize
	default:
		return nil, fmt.Errorf("unhandled witness type %v", witnessType)
	}

	// Calculate the transaction's weight to determine its fee along with
	// the provided fee rate. The transaction will contain one P2WSH input,
	// the account output, and one P2WPKH output.
	//
	// TODO(wilmer): Check dust.
	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(witnessSize)
	weightEstimator.AddP2WKHOutput()
	fee := feeRate.FeeForWeight(int64(weightEstimator.Weight()))
	outputValue := accountValue - fee

	// With the fee calculated, compute the accompanying output script.
	// Using the mainnet parameters for the address doesn't have an impact
	// on the script.
	addr, err := m.cfg.Wallet.NextAddr(ctx)
	if err != nil {
		return nil, err
	}
	outputScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    int64(outputValue),
		PkScript: outputScript,
	}, nil
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
