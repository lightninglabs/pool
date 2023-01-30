package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/btcutil/txsort"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account/watcher"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// minConfs and maxConfs represent the thresholds at both extremes for
	// valid number of confirmations on an account before it is considered
	// open.
	minConfs = 3
	maxConfs = 6

	// MinAccountValue is the minimum value for an account output in
	// satoshis.
	MinAccountValue btcutil.Amount = 100000

	// minAccountExpiry and maxAccountExpiry represent the thresholds at
	// both extremes for valid account expirations.
	minAccountExpiry = 144       // One day worth of blocks.
	maxAccountExpiry = 144 * 365 // A year worth of blocks.

	txLabelPrefixTag = "poold -- "
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

	// expiryTaproot is the type used for a witness taking the expiration
	// path of a Taproot account.
	expiryTaproot

	// muSig2Taproot is the type used for a witness taking the MuSig2
	// combined signature key spend path of a Taproot account.
	muSig2Taproot
)

// scriptVersion returns the Pool script version the witness type uses.
func (wt witnessType) scriptVersion() poolscript.Version {
	switch wt {
	case expiryTaproot, muSig2Taproot:
		return poolscript.VersionTaprootMuSig2

	default:
		return poolscript.VersionWitnessScript
	}
}

// witnessSize returns the estimated weight units for an account input witness.
func (wt witnessType) witnessSize() (int, error) {
	switch wt {
	case expiryWitness:
		return poolscript.ExpiryWitnessSize, nil
	case multiSigWitness:
		return poolscript.MultiSigWitnessSize, nil
	case expiryTaproot:
		return poolscript.TaprootExpiryWitnessSize, nil
	case muSig2Taproot:
		return poolscript.TaprootMultiSigWitnessSize, nil
	default:
		return 0, fmt.Errorf("unknown witness type %v", wt)
	}
}

// IsExpirySpend returns true if the witness is taking an expiration path.
func (wt witnessType) IsExpirySpend() bool {
	switch wt {
	case expiryWitness, expiryTaproot:
		return true
	default:
		return false
	}
}

// Action is a type of account modification.
type Action string

const (
	CREATE   Action = "create"
	DEPOSIT  Action = "deposit"
	WITHDRAW Action = "withdraw"
	RENEW    Action = "renew"
	CLOSE    Action = "close"
)

type TxLabel struct {
	Account AccountTxLabel `json:"account"`
}

type AccountTxLabel struct {
	Key           string          `json:"key"`
	Action        Action          `json:"action"`
	ExpiryHeight  uint32          `json:"expiry_height"`
	OutputIndex   uint32          `json:"output_index"`
	IsExpirySpend bool            `json:"expiry_spend"`
	TxFee         *btcutil.Amount `json:"tx_fee"`
	BalanceDiff   btcutil.Amount  `json:"balance_diff"`
}

// actionTxLabel returns a transaction label for use with account
// modification actions.
func actionTxLabel(account *Account, action Action, isExpirySpend bool,
	txFee *btcutil.Amount, balanceDiff btcutil.Amount) string {

	acctKey := account.TraderKey.PubKey.SerializeCompressed()
	key := fmt.Sprintf("%x", acctKey)
	label := TxLabel{
		Account: AccountTxLabel{
			Key:           key,
			Action:        action,
			ExpiryHeight:  account.Expiry,
			OutputIndex:   account.OutPoint.Index,
			IsExpirySpend: isExpirySpend,
			TxFee:         txFee,
			BalanceDiff:   balanceDiff,
		},
	}
	labelJson, err := json.Marshal(label)
	if err != nil {
		log.Errorf("Internal error: failed to serialize json "+
			"from %v: %v", label, err)
		return fmt.Sprintf("%s%s", txLabelPrefixTag, action)
	}

	return fmt.Sprintf("%s%s", txLabelPrefixTag, labelJson)
}

// IsPoolTx returns true if the given transaction is related to pool.
func IsPoolTx(tx *lnrpc.Transaction) bool {
	return strings.HasPrefix(tx.Label, txLabelPrefixTag)
}

// ParseTxLabel parses and returns data fields stored in a given transaction
// label.
func ParseTxLabel(label string) (*TxLabel, error) {
	label = strings.TrimPrefix(label, txLabelPrefixTag)

	var data TxLabel
	err := json.Unmarshal([]byte(label), &data)
	if err != nil {
		return nil, err
	}

	return &data, nil
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

	// TxFeeEstimator is an estimator that can calculate the total on-chain
	// fees to send to an account output.
	TxFeeEstimator TxFeeEstimator

	// TxLabelPrefix is set, then all transactions the account manager
	// makes will use this string as a prefix for added transaction labels.
	TxLabelPrefix string

	// ChainParams are the currently used chain parameters.
	ChainParams *chaincfg.Params

	// LndVersion is the version of the connected lnd node.
	LndVersion *verrpc.Version
}

// Manager is responsible for the management of accounts on-chain.
type manager struct {
	started sync.Once
	stopped sync.Once

	cfg         ManagerConfig
	watcherCtrl watcher.Controller

	// pendingBatchMtx guards access to any database calls involving pending
	// batches. This is mostly used to prevent race conditions when handling
	// multiple accounts spends as part of a batch that we didn't receive a
	// Finalize message for.
	pendingBatchMtx sync.Mutex

	// reservationMtx prevents a trader from attempting to have more than
	// once active reservation at a time when creating new accounts. This is
	// done to ensure an account picks up the correct reservation once its
	// time to fund it.
	reservationMtx sync.Mutex

	wg   sync.WaitGroup
	quit chan struct{}
}

// Compile time assertion that manager implements the Manager interface.
var _ Manager = (*manager)(nil)

// NewManager instantiates a new Manager backed by the given config.
func NewManager(cfg *ManagerConfig) *manager { // nolint:golint
	m := &manager{
		cfg:  *cfg,
		quit: make(chan struct{}),
	}

	m.watcherCtrl = watcher.NewController(&watcher.CtrlConfig{
		ChainNotifier: cfg.ChainNotifier,

		// The manager implements the EventHandler interface.
		Handlers: m,
	})

	return m
}

// Start resumes all account on-chain operation after a restart.
func (m *manager) Start() error {
	var err error
	m.started.Do(func() {
		err = m.start()
	})
	return err
}

// start resumes all account on-chain operation after a restart.
func (m *manager) start() error {
	ctx := context.Background()

	// We'll start by resuming all of our accounts. This requires the
	// watcher to be started first.
	if err := m.watcherCtrl.Start(); err != nil {
		return err
	}

	// Then, we'll resume all complete accounts, followed by partial
	// accounts. If we were to do it the other way around, we'd resume
	// partial accounts twice.
	accounts, err := m.cfg.Store.Accounts()
	if err != nil {
		return fmt.Errorf("unable to retrieve accounts: %v", err)
	}

	// We calculate the default fee rate that will be used
	// for resuming accounts for which we haven't created and broadcast
	// a transaction yet
	feeRate, err := m.cfg.Wallet.EstimateFeeRate(
		ctx, int32(DefaultFundingConfTarget),
	)
	if err != nil {
		return fmt.Errorf("unable to estimate default fees %w", err)
	}

	for _, account := range accounts {
		acctKey := account.TraderKey.PubKey.SerializeCompressed()

		// Detect if poold is using a different LND Signer
		// than the one used for creating this account.
		if err := m.verifyAccountSigner(ctx, account); err != nil {
			return fmt.Errorf("unable to resume account %x: %v",
				acctKey, err)
		}

		// Try to resume the account now.
		//
		// TODO(guggero): Refactor this to extract the init/funding
		// part so we properly abandon the account if it fails before
		// publishing the TX instead of trying to re-fund on startup.
		if err := m.resumeAccount(
			ctx, account, true, false, feeRate,
		); err != nil {
			return fmt.Errorf("unable to resume account %x: %v",
				acctKey, err)
		}
	}

	return nil
}

// Stop safely stops any ongoing operations within the Manager.
func (m *manager) Stop() {
	m.stopped.Do(func() {
		m.watcherCtrl.Stop()

		close(m.quit)
		m.wg.Wait()
	})
}

// QuoteAccount returns the expected fee rate and total miner fee to send to an
// account funding output with the given confTarget.
func (m *manager) QuoteAccount(ctx context.Context, value btcutil.Amount,
	confTarget uint32) (chainfee.SatPerKWeight, btcutil.Amount, error) {

	// First, make sure we have a valid amount to create the account. We
	// need to ask the auctioneer for the maximum as it dynamically defines
	// that value.
	terms, err := m.cfg.Auctioneer.Terms(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("could not query auctioneer terms: %v",
			err)
	}
	err = validateAccountValue(value, terms.MaxAccountValue)
	if err != nil {
		return 0, 0, err
	}

	// Now calculate the estimated fee rate from the confTarget.
	feeRate, err := m.cfg.Wallet.EstimateFeeRate(ctx, int32(confTarget))
	if err != nil {
		return 0, 0, fmt.Errorf("error estimating fee rate: %v", err)
	}

	// Then calculate the total fee to pay. This asks lnd to create a full
	// transaction to spend to a P2WSH output. If not enough confirmed funds
	// are available in the wallet, this will return an error.
	totalMinerFee, err := m.cfg.TxFeeEstimator.EstimateFeeToP2WSH(
		ctx, value, int32(confTarget),
	)
	if err != nil {
		return 0, 0, fmt.Errorf("error estimating total on-chain fee: "+
			"%v", err)
	}

	return feeRate, totalMinerFee, nil
}

// InitAccount handles a request to create a new account with the provided
// parameters.
func (m *manager) InitAccount(ctx context.Context, value btcutil.Amount,
	version Version, feeRate chainfee.SatPerKWeight, expiry,
	bestHeight uint32) (*Account, error) {

	// We'll make sure to acquire the reservation lock throughout the
	// account funding process to ensure we use the same reservation, as
	// only one can be active per trader LSAT.
	m.reservationMtx.Lock()
	defer m.reservationMtx.Unlock()

	// First, make sure we have a valid amount to create the account. We
	// need to ask the auctioneer for the maximum as it dynamically defines
	// that value.
	terms, err := m.cfg.Auctioneer.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query auctioneer terms: %v",
			err)
	}
	err = validateAccountParams(
		value, terms.MaxAccountValue, expiry, bestHeight, version,
	)
	if err != nil {
		return nil, err
	}

	// We'll start by deriving a key for ourselves that we'll use in our
	// 2-of-2 multi-sig construction.
	keyDesc, err := m.cfg.Wallet.DeriveNextKey(
		ctx, int32(poolscript.AccountKeyFamily),
	)
	if err != nil {
		return nil, err
	}

	// With our key obtained, we'll reserve an account with our auctioneer,
	// who will provide us with their base key and our initial per-batch
	// key.
	reservation, err := m.cfg.Auctioneer.ReserveAccount(
		ctx, value, expiry, keyDesc.PubKey, version,
	)
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
		Version:       version,
	}
	if err := m.cfg.Store.AddAccount(account); err != nil {
		return nil, err
	}

	log.Infof("Creating new account %x of %v that expires at height %v",
		keyDesc.PubKey.SerializeCompressed(), value, expiry)

	err = m.resumeAccount(ctx, account, false, false, feeRate)
	if err != nil {
		return nil, err
	}

	return account, nil
}

// WatchMatchedAccounts resumes accounts that were just matched in a batch and
// are expecting the batch transaction to confirm as their next account output.
// This will cancel all previous spend and conf watchers of all accounts
// involved in the batch.
func (m *manager) WatchMatchedAccounts(ctx context.Context,
	matchedAccounts []*btcec.PublicKey) error {

	for _, matchedAccount := range matchedAccounts {
		acct, err := m.cfg.Store.Account(matchedAccount)
		if err != nil {
			return fmt.Errorf("error reading account %x: %v",
				matchedAccount.SerializeCompressed(), err)
		}

		// The account was just involved in a batch. That means our
		// account output was spent by a batch transaction. Since we
		// know that a batch transaction cannot simply be rolled back or
		// replaced without us being involved, we know that the batch TX
		// will eventually confirm. To handle the case where an account
		// is involved in multiple consecutive batches that are all
		// unconfirmed, we make sure we only track the latest state by
		// canceling all previous spend and confirmation watchers. We
		// then only watch the latest batch and once it confirms, create
		// a new spend watcher on that.
		m.watcherCtrl.CancelAccountSpend(matchedAccount)
		m.watcherCtrl.CancelAccountConf(matchedAccount)

		// After taking part in a batch, the account is either pending
		// closed because it was used up or pending batch update because
		// it was recreated. Either way, let's resume it now by creating
		// the appropriate watchers again.
		// We set feerate to 0 because we know that we won't need to
		// create a new transaction for resuming the account.
		err = m.resumeAccount(ctx, acct, false, false, 0)
		if err != nil {
			return fmt.Errorf("error resuming account %x: %v",
				matchedAccount.SerializeCompressed(), err)
		}
	}

	return nil
}

// maybeBroadcastTx attempts to broadcast the transaction only if all of its
// inputs have been signed for.
func (m *manager) maybeBroadcastTx(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	// If any of the transaction inputs aren't signed, don't broadcast.
	for _, txIn := range tx.TxIn {
		if len(txIn.Witness) == 0 && len(txIn.SignatureScript) == 0 {
			return nil
		}
	}

	return m.cfg.Wallet.PublishTransaction(ctx, tx, label)
}

// verifyAccountSigner ensures that we are able to recreate the account
// secret for active accounts. That means that the LND signerClient did
// not change and we are able to generate valid signatures for this account.
func (m *manager) verifyAccountSigner(ctx context.Context,
	account *Account) error {

	// The secret was based on both base keys, the trader and auctioneer's.
	secret, err := m.cfg.Signer.DeriveSharedKey(
		ctx, account.AuctioneerKey, &account.TraderKey.KeyLocator,
	)
	if err != nil {
		return fmt.Errorf("unable to regenerate secret: %v", err)
	}

	// Here we would detect if the backend LND node (signer) changed.
	if !bytes.Equal(secret[:], account.Secret[:]) {
		return fmt.Errorf("couldn't derive account secret; make sure " +
			"you are using the same lnd node/seed that was used " +
			"for creating the account")
	}

	return nil
}

// resumeAccount performs different operations based on the account's state.
// This method serves as a way to consolidate the logic of resuming accounts on
// startup and during normal operation.
func (m *manager) resumeAccount(ctx context.Context, account *Account, // nolint
	onRestart bool, onRecovery bool, feeRate chainfee.SatPerKWeight) error {

	accountOutput, err := account.Output()
	if err != nil {
		return fmt.Errorf("unable to construct account output: %v", err)
	}

	switch account.State {
	// In StateInitiated, we'll attempt to fund our account.
	case StateInitiated:
		// If we're resuming the account from a restart, we'll want to
		// make sure we haven't created and broadcast a transaction for
		// this account already, so we'll inspect our TxSource to do so.
		var (
			accountTx *wire.MsgTx
			createTx  = true
		)
		if onRestart || onRecovery {
			tx, err := m.locateTxByOutput(
				ctx, accountOutput, account.LatestTx,
			)
			switch err {
			// If we do find one, we can rebroadcast it.
			case nil:
				accountTx = tx
				createTx = false

			// If we don't, we'll need to create one.
			case errTxNotFound:
				// If lnd doesn't know a transaction that sends
				// to the account output, it could be that it
				// was never published or it never confirmed.
				// In that case the funds should be SAFU and can
				// be double spent. We don't need to try a
				// recovery in that case. And we certainly don't
				// want to send funds again, so we exit here.
				if onRecovery {
					state := StateCanceledAfterRecovery
					err := m.cfg.Store.UpdateAccount(
						account, StateModifier(state),
					)
					if err != nil {
						return fmt.Errorf("account "+
							"funding TX not found "+
							"but was unable to "+
							"update account to "+
							"state recovery "+
							"failed: %v", err)
					}

					return fmt.Errorf("account funding "+
						"TX with output %x not found",
						accountOutput.PkScript)
				}

			default:
				return fmt.Errorf("unable to locate output "+
					"%x: %v", accountOutput.PkScript, err)
			}
		}

		if createTx {
			acctKey := account.TraderKey.PubKey.SerializeCompressed()

			if feeRate == 0 {
				return fmt.Errorf("unable to create "+
					" transaction for account with "+
					"  trader key %x, feeRate should "+
					"be greater than 0", acctKey)
			}

			// Attach additional meta data to transaction label.
			//
			// TODO(ffranr): Tx inputs are required to calculate the
			// fee. The fee will be added to the tx label.
			// m.cfg.Wallet.SendOutputs returns and selects tx
			// inputs but also requires the label as an argument.
			// Instead, the tx should be constructed via
			// m.cfg.Wallet.FundPsbt which would give us an
			// opportunity to inspect tx inputs before creating a
			// tx label.
			var txFee *btcutil.Amount = nil

			balanceDiff := account.Value
			contextLabel := actionTxLabel(
				account, CREATE, false, txFee,
				balanceDiff,
			)
			label := makeTxnLabel(m.cfg.TxLabelPrefix, contextLabel)

			// TODO(wilmer): Expose manual controls to bump fees.
			tx, err := m.cfg.Wallet.SendOutputs(
				ctx, []*wire.TxOut{accountOutput}, feeRate,
				label,
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
		outputIndex, ok := poolscript.LocateOutputScript(
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
			OutPointModifier(op), LatestTxModifier(accountTx),
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
			accountTx := account.LatestTx

			// Since we store the latest account modification TX in
			// the account itself, we don't need to rely on lnd
			// keeping track of all our TXns anymore. If what we
			// have in the DB is correct, we can just re-broadcast
			// that TX.
			if accountTx == nil ||
				accountTx.TxHash() != account.OutPoint.Hash {

				var err error
				accountTx, err = m.locateTxByHash(
					ctx, account.OutPoint.Hash,
				)
				if err != nil {
					return fmt.Errorf("unable to locate "+
						"transaction %v: %v",
						account.OutPoint.Hash, err)
				}
			}

			fee, err := m.deriveFeeFromTx(ctx, accountTx)
			if err != nil {
				log.Errorf("Failed to derive fee from "+
					"transaction: %v, %v", accountTx, err)
			}
			balanceDiff := account.Value
			contextLabel := actionTxLabel(
				account, CREATE, false, fee, balanceDiff,
			)
			label := makeTxnLabel(m.cfg.TxLabelPrefix, contextLabel)

			err = m.maybeBroadcastTx(ctx, accountTx, label)
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
		terms, err := m.cfg.Auctioneer.Terms(ctx)
		if err != nil {
			return fmt.Errorf("could not query auctioneer terms: "+
				"%v", err)
		}

		// Proceed to watch for the account on-chain.
		numConfs := NumConfsForValue(
			account.Value, terms.MaxAccountValue,
		)
		log.Infof("Waiting for %v confirmation(s) of account %x",
			numConfs, account.TraderKey.PubKey.SerializeCompressed())
		err = m.watcherCtrl.WatchAccountConf(
			account.TraderKey.PubKey, account.OutPoint.Hash,
			accountOutput.PkScript, numConfs, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for confirmation: "+
				"%v", err)
		}

	// In StatePendingUpdate or StatePendingBatch, we've processed an
	// account update due to either a matched order or trader modification,
	// so we'll need to wait for its confirmation. Once it confirms,
	// handleAccountConf will take care of the rest of the flow.
	//
	// TODO(wilmer): Handle restart case where the client shuts down after
	// the modification has been reflected on-disk, but the auctioneer's
	// signature hasn't been received.
	//
	// TODO(guggero): Handle the case of a malicious auctioneer that
	// replaces batch A with a batch A' that contains none of our accounts
	// and would therefore not be noticed by us. The account would stay
	// pending forever in that case.
	case StatePendingUpdate, StatePendingBatch:
		// We need to know the maximum account value to scale the number
		// of confirmations the same way the auctioneer does to avoid
		// getting the state out of sync.
		terms, err := m.cfg.Auctioneer.Terms(ctx)
		if err != nil {
			return fmt.Errorf("could not query auctioneer terms: "+
				"%v", err)
		}

		numConfs := NumConfsForValue(
			account.Value, terms.MaxAccountValue,
		)
		log.Infof("Waiting for %v confirmation(s) of account %x",
			numConfs, account.TraderKey.PubKey.SerializeCompressed())
		err = m.watcherCtrl.WatchAccountConf(
			account.TraderKey.PubKey, account.OutPoint.Hash,
			accountOutput.PkScript, numConfs, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for confirmation: "+
				"%v", err)
		}

		// Only subscribe to auction updates for this account if it's in
		// the pending batch state, to allow traders to participate in
		// consecutive batches. This isn't necessary for the pending
		// update state, as that state is ineligible for batch
		// execution.
		if account.State == StatePendingBatch {
			err = m.cfg.Auctioneer.StartAccountSubscription(
				ctx, account.TraderKey,
			)
			if err != nil {
				return fmt.Errorf("unable to subscribe for "+
					"account updates: %v", err)
			}
		}

	// In StateOpen, the funding transaction for the account has already
	// confirmed, so we only need to watch for its spend and expiration and
	// register for account updates.
	case StateOpen:
		if err := m.handleStateOpen(ctx, account); err != nil {
			return err
		}

	// In StateExpiredPendingUpdate, the account expired while having a
	// pending update. To make sure the account can be renewed, we'll wait
	// for the pending update to confirm and transition the account to
	// StateExpired then.
	case StateExpiredPendingUpdate:
		terms, err := m.cfg.Auctioneer.Terms(ctx)
		if err != nil {
			return fmt.Errorf("could not query auctioneer terms: "+
				"%v", err)
		}
		numConfs := NumConfsForValue(
			account.Value, terms.MaxAccountValue,
		)

		log.Infof("Waiting for %v confirmation(s) of expired account %x",
			numConfs, account.TraderKey.PubKey.SerializeCompressed())

		err = m.watcherCtrl.WatchAccountConf(
			account.TraderKey.PubKey, account.OutPoint.Hash,
			accountOutput.PkScript, numConfs, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for confirmation: "+
				"%v", err)
		}

	// In StateExpired, we'll wait for the account to be spent so that we
	// can detect whether its been closed or renewed.
	case StateExpired:
		log.Infof("Watching expired account %x for spend",
			account.TraderKey.PubKey.SerializeCompressed())

		err = m.watcherCtrl.WatchAccountSpend(
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
		fee, err := m.deriveFeeFromTx(ctx, account.LatestTx)
		if err != nil {
			log.Errorf("Failed to derive fee from "+
				"transaction: %v, %v", account.LatestTx, err)
		}
		balanceDiff := account.Value
		contextLabel := actionTxLabel(
			account, CLOSE, false, fee, balanceDiff,
		)
		label := makeTxnLabel(m.cfg.TxLabelPrefix, contextLabel)

		err = m.maybeBroadcastTx(ctx, account.LatestTx, label)
		if err != nil {
			return err
		}

		log.Infof("Watching account %x for spend",
			account.TraderKey.PubKey.SerializeCompressed())
		err = m.watcherCtrl.WatchAccountSpend(
			account.TraderKey.PubKey, account.OutPoint,
			accountOutput.PkScript, account.HeightHint,
		)
		if err != nil {
			return fmt.Errorf("unable to watch for spend: %v", err)
		}

	// If the account has already been closed or canceled, there's nothing
	// to be done.
	case StateClosed, StateCanceledAfterRecovery:
		break

	default:
		return fmt.Errorf("unhandled account state %v", account.State)
	}

	return nil
}

// deriveFeeFromTx derives the transaction fee from a given transaction message.
func (m *manager) deriveFeeFromTx(ctx context.Context,
	tx *wire.MsgTx) (*btcutil.Amount, error) {

	// Input and output values are required to calculate the tx fee. However,
	// tx messages do not contain input values. We will therefore locate those
	// input values via the outputs that they represent.
	var sumInputs int64
	for _, txIn := range tx.TxIn {
		spendTx, err := m.locateTxByHash(ctx, txIn.PreviousOutPoint.Hash)
		if err != nil {
			return nil, err
		}
		txOut := spendTx.TxOut[txIn.PreviousOutPoint.Index]
		sumInputs += txOut.Value
	}

	var sumOutputs int64
	for _, txOut := range tx.TxOut {
		sumOutputs += txOut.Value
	}

	fee := btcutil.Amount(sumInputs - sumOutputs)
	return &fee, nil
}

// locateTxByOutput locates a transaction from the Manager's TxSource by one of
// its outputs. If a transaction is not found containing the output, then
// errTxNotFound is returned.
func (m *manager) locateTxByOutput(ctx context.Context,
	output *wire.TxOut, fullTx *wire.MsgTx) (*wire.MsgTx, error) {

	// We now store the full raw transaction of the last modification. We
	// can just use that if available. If for some reason that TX doesn't
	// contain our current outpoint, we fall back to the previous behavior.
	if fullTx != nil {
		idx, ok := poolscript.LocateOutputScript(fullTx, output.PkScript)
		if ok && fullTx.TxOut[idx].Value == output.Value {
			return fullTx, nil
		}
	}

	// Get all transactions, starting from block 0 and including unconfirmed
	// TXes (end block = -1).
	txs, err := m.cfg.TxSource.ListTransactions(ctx, 0, -1)
	if err != nil {
		return nil, err
	}

	for _, tx := range txs {
		idx, ok := poolscript.LocateOutputScript(tx.Tx, output.PkScript)
		if !ok {
			continue
		}
		if tx.Tx.TxOut[idx].Value == output.Value {
			return tx.Tx, nil
		}
	}

	return nil, errTxNotFound
}

// locateTxByHash locates a transaction from the Manager's TxSource by its hash.
// If the transaction is not found, then errTxNotFound is returned.
func (m *manager) locateTxByHash(ctx context.Context,
	hash chainhash.Hash) (*wire.MsgTx, error) {

	// Get all transactions, starting from block 0 and including unconfirmed
	// TXes (end block = -1).
	txs, err := m.cfg.TxSource.ListTransactions(ctx, 0, -1)
	if err != nil {
		return nil, err
	}

	for _, tx := range txs {
		if tx.Tx.TxHash() == hash {
			return tx.Tx, nil
		}
	}

	return nil, errTxNotFound
}

// handleStateOpen performs the necessary operations for accounts found in
// StateOpen.
func (m *manager) handleStateOpen(ctx context.Context, account *Account) error {
	var traderKey [33]byte
	copy(traderKey[:], account.TraderKey.PubKey.SerializeCompressed())

	log.Infof("Watching spend of %v for account %x", account.OutPoint,
		traderKey)

	accountOutput, err := account.Output()
	if err != nil {
		return err
	}

	err = m.watcherCtrl.WatchAccountSpend(
		account.TraderKey.PubKey, account.OutPoint,
		accountOutput.PkScript, account.HeightHint,
	)
	if err != nil {
		return fmt.Errorf("unable to watch for spend: %v", err)
	}

	m.watcherCtrl.WatchAccountExpiration(
		account.TraderKey.PubKey, account.Expiry,
	)

	// Now that we have an open account, subscribe for updates to it to the
	// server. We subscribe for the account instead of the individual orders
	// because all signing operations will need to be executed on an account
	// level anyway. And we might end up executing multiple orders for the
	// same account in one batch. The messages from the server are received
	// and dispatched to the correct manager by the rpcServer.
	err = m.cfg.Auctioneer.StartAccountSubscription(ctx, account.TraderKey)
	if err != nil {
		return fmt.Errorf("unable to subscribe for account updates: %v",
			err)
	}

	return nil
}

// HandleAccountConf takes the necessary steps after detecting the confirmation
// of an account on-chain.
func (m *manager) HandleAccountConf(traderKey *btcec.PublicKey,
	confDetails *chainntnfs.TxConfirmation) error {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	log.Infof("Account %x is now confirmed at height %v!",
		traderKey.SerializeCompressed(), confDetails.BlockHeight)

	// The new state we'll transition to depends on the account's current
	// state.
	var newState State
	switch account.State {
	// Any pending states will transition to their confirmed state.
	case StatePendingOpen, StatePendingUpdate, StatePendingBatch:
		newState = StateOpen

	// An expired account with a pending update that has now confirmed will
	// transition to the confirmed expired case, allowing a trader to renew
	// their account.
	case StateExpiredPendingUpdate:
		newState = StateExpired

	default:
		return fmt.Errorf("unhandled state %v after confirmation",
			account.State)
	}

	// Update the account's state and proceed with the rest of the flow.
	mods := []Modifier{
		StateModifier(newState),
		HeightHintModifier(confDetails.BlockHeight),
	}
	if err := m.cfg.Store.UpdateAccount(account, mods...); err != nil {
		return err
	}

	return m.handleStateOpen(context.Background(), account)
}

// HandleAccountSpend handles the different spend paths of an account. If an
// account is spent by the expiration path, it'll always be marked as closed
// thereafter. If it is spent by the cooperative path with the auctioneer, then
// the account will only remain open if the spending transaction recreates the
// account with the expected next account script. Otherwise, it is also marked
// as closed. In case of multiple consecutive batches with the same account, we
// only track the spend of the latest batch, after it confirmed. So the account
// output in the spend transaction should always match our database state if
// it was a cooperative spend.
func (m *manager) HandleAccountSpend(traderKey *btcec.PublicKey,
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
	case poolscript.IsExpirySpend(spendWitness) ||
		poolscript.IsTaprootExpirySpend(spendWitness):

		break

	// If the witness is for a multi-sig spend, then either an order by the
	// trader was matched, the account was modified or the account was
	// closed. If it was closed, then the account output shouldn't have been
	// recreated.
	case poolscript.IsMultiSigSpend(spendWitness) ||
		poolscript.IsTaprootMultiSigSpend(spendWitness):

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
		_, ok := poolscript.LocateOutputScript(
			spendTx, accountOutput.PkScript,
		)
		if ok {
			// Proceed with the rest of the flow. We won't send to
			// the account output again, so we don't need to set
			// a valid feeRate.
			return m.resumeAccount(
				context.Background(), account, false, false, 0,
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
		account, StateModifier(StateClosed),
		HeightHintModifier(uint32(spendDetails.SpendingHeight)),
		LatestTxModifier(spendTx),
	)
}

// HandleAccountExpiry marks an account as expired within the database.
func (m *manager) HandleAccountExpiry(traderKey *btcec.PublicKey,
	height uint32) error {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	var expiredState State
	switch account.State {
	// If the account has already been closed or is in the process of doing
	// so, there's no need to mark it as expired.
	case StatePendingClosed, StateClosed:
		return nil

	// If the account is waiting for a confirmation, use the expired state
	// indicating so.
	case StatePendingUpdate, StatePendingBatch:
		expiredState = StateExpiredPendingUpdate

	// If the account is confirmed, use the default expired state.
	case StateOpen:
		expiredState = StateExpired

	default:
		return fmt.Errorf("unhandled state %v after expiration",
			account.State)
	}

	log.Infof("Account %x has expired as of height %v",
		traderKey.SerializeCompressed(), account.Expiry)

	return m.cfg.Store.UpdateAccount(account, StateModifier(expiredState))
}

// DepositAccount attempts to deposit funds into the account associated with the
// given trader key such that the new account value is met using inputs sourced
// from the backing lnd node's wallet. If needed, a change output that does back
// to lnd may be added to the deposit transaction.
func (m *manager) DepositAccount(ctx context.Context,
	traderKey *btcec.PublicKey, depositAmount btcutil.Amount,
	feeRate chainfee.SatPerKWeight, bestHeight, expiryHeight uint32,
	newVersion Version) (*Account, *wire.MsgTx, error) {

	// The account can only be modified in `StateOpen` and its new value
	// should not exceed the maximum allowed.
	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return nil, nil, err
	}
	if account.State != StateOpen {
		return nil, nil, fmt.Errorf("account must be in %v to be "+
			"modified", StateOpen)
	}

	// Can't downgrade an account.
	if newVersion < account.Version {
		return nil, nil, fmt.Errorf("cannot downgrade account "+
			"version to %s", newVersion)
	}

	// The auctioneer defines the maximum account size.
	terms, err := m.cfg.Auctioneer.Terms(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("could not query auctioneer "+
			"terms: %v", err)
	}

	newAccountValue := account.Value + depositAmount
	if newAccountValue > terms.MaxAccountValue {
		return nil, nil, fmt.Errorf("new account value is above "+
			"accepted maximum of %v", terms.MaxAccountValue)
	}

	var newExpiry *uint32
	if expiryHeight != 0 {
		// Validate the new expiry.
		err := validateAccountExpiry(expiryHeight, bestHeight)
		if err != nil {
			return nil, nil, err
		}
		newExpiry = &expiryHeight
	}

	// TODO(wilmer): Reject if account has pending orders.

	newAccountOutput, modifiers, err := createNewAccountOutput(
		account, newAccountValue, newExpiry, &newVersion,
	)
	if err != nil {
		return nil, nil, err
	}

	// To start, we'll need to perform coin selection in order to meet the
	// required new value of the account as part of the deposit. The
	// selected inputs, along with a change output if needed, will then be
	// included in the deposit transaction we'll broadcast.
	spendWitnessType := determineWitnessType(account, bestHeight)
	packet, releaseInputs, err := m.inputsForDeposit(
		ctx, account, newAccountOutput, depositAmount, spendWitnessType,
		feeRate,
	)
	if err != nil {
		return nil, nil, err
	}

	log.Tracef("Got funded PSBT packet %s", spew.Sdump(packet))

	// We'll tack on the change output if it was needed and an additional
	// `StatePendingUpdate` modifier to our account and proceed with the
	// rest of the flow. This should request a signature from the auctioneer
	// and assuming it's valid, broadcast the deposit transaction.
	modifiers = append(modifiers, StateModifier(StatePendingUpdate))
	modifiedAccount, spendTx, err := m.spendAccount(
		ctx, account, DEPOSIT, packet, spendWitnessType, modifiers, bestHeight,
	)
	if err != nil {
		releaseInputs()
		return nil, nil, err
	}

	return modifiedAccount, spendTx, nil
}

// WithdrawAccount attempts to withdraw funds from the account associated with
// the given trader key into the provided outputs.
func (m *manager) WithdrawAccount(ctx context.Context,
	traderKey *btcec.PublicKey, outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, bestHeight, expiryHeight uint32,
	newVersion Version) (*Account, *wire.MsgTx, error) {

	// The account can only be modified in `StateOpen`.
	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return nil, nil, err
	}
	if account.State != StateOpen {
		return nil, nil, fmt.Errorf("account must be in %v to be "+
			"modified", StateOpen)
	}

	// Can't downgrade an account.
	if newVersion < account.Version {
		return nil, nil, fmt.Errorf("cannot downgrade account "+
			"version to %s", newVersion)
	}

	var newExpiry *uint32
	if expiryHeight != 0 {
		// Validate the new expiry.
		err := validateAccountExpiry(expiryHeight, bestHeight)
		if err != nil {
			return nil, nil, err
		}
		newExpiry = &expiryHeight
	}

	// TODO(wilmer): Reject if account has pending orders.

	// To start, we'll need to determine the new value of the account after
	// creating the outputs specified as part of the withdrawal, which we'll
	// then use to create the new account output.
	spendWitnessType := determineWitnessType(account, bestHeight)
	newAccountValue, err := valueAfterAccountUpdate(
		account, outputs, spendWitnessType, feeRate,
	)
	if err != nil {
		return nil, nil, err
	}
	newAccountOutput, modifiers, err := createNewAccountOutput(
		account, newAccountValue, newExpiry, &newVersion,
	)
	if err != nil {
		return nil, nil, err
	}

	allOutputs := []*wire.TxOut{newAccountOutput}
	allOutputs = append(allOutputs, outputs...)
	packet, err := m.createSpendTx(account, allOutputs)
	if err != nil {
		return nil, nil, err
	}

	// With the output created, we'll tack on an additional
	// `StatePendingUpdate` modifier to our account and proceed with the
	// rest of the flow. This should request a signature from the auctioneer
	// and assuming it's valid, broadcast the withdrawal transaction.
	modifiers = append(modifiers, StateModifier(StatePendingUpdate))
	modifiedAccount, spendTx, err := m.spendAccount(
		ctx, account, WITHDRAW, packet, spendWitnessType, modifiers, bestHeight,
	)
	if err != nil {
		return nil, nil, err
	}

	return modifiedAccount, spendTx, nil
}

// RenewAccount updates the expiration of an open/expired account. This will
// always require a signature from the auctioneer, even after the account has
// expired, to ensure the auctioneer is aware the account is being renewed.
func (m *manager) RenewAccount(ctx context.Context,
	traderKey *btcec.PublicKey, newExpiry uint32,
	feeRate chainfee.SatPerKWeight, bestHeight uint32,
	newVersion Version) (*Account, *wire.MsgTx, error) {

	// The account can only have its expiry updated if it has confirmed
	// and/or has expired.
	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return nil, nil, err
	}
	switch account.State {
	case StateOpen, StateExpired:
	default:
		return nil, nil, fmt.Errorf("account must be in either of %v "+
			"to be renewed",
			[]State{StateOpen, StateExpired})
	}

	// Validate the new expiry.
	if err := validateAccountExpiry(newExpiry, bestHeight); err != nil {
		return nil, nil, err
	}

	// Can't downgrade an account.
	if newVersion < account.Version {
		return nil, nil, fmt.Errorf("cannot downgrade account "+
			"version to %s", newVersion)
	}

	// Determine the new account output after attempting the expiry update.
	// We'll always use the multisig spend path, even if the account is
	// expired, to make sure the auctioneer is aware of the change. We
	// achieve this by setting the best height to 0 which means our account
	// is never seen as expired.
	spendWitnessType := multiSigWitness
	if account.Version >= VersionTaprootEnabled {
		spendWitnessType = muSig2Taproot
	}
	newAccountValue, err := valueAfterAccountUpdate(
		account, nil, spendWitnessType, feeRate,
	)
	if err != nil {
		return nil, nil, err
	}
	newAccountOutput, modifiers, err := createNewAccountOutput(
		account, newAccountValue, &newExpiry, &newVersion,
	)
	if err != nil {
		return nil, nil, err
	}

	packet, err := m.createSpendTx(account, []*wire.TxOut{newAccountOutput})
	if err != nil {
		return nil, nil, err
	}

	// With the output created, we'll tack on an additional
	// `StatePendingUpdate` modifier to our account and proceed with the
	// rest of the flow. This should request a signature from the auctioneer
	// and assuming it's valid, broadcast the update transaction.
	modifiers = append(modifiers, StateModifier(StatePendingUpdate))
	modifiedAccount, spendTx, err := m.spendAccount(
		ctx, account, RENEW, packet, spendWitnessType, modifiers, bestHeight,
	)
	if err != nil {
		return nil, nil, err
	}

	// Begin to track the new account expiration, which will overwrite the
	// existing expiration request.
	m.watcherCtrl.WatchAccountExpiration(traderKey, modifiedAccount.Expiry)

	return modifiedAccount, spendTx, nil
}

// BumpAccountFee attempts to bump the fee of an account's most recent
// transaction. This is done by locating an eligible output for lnd to CPFP,
// otherwise the fee bump will not succeed. Further invocations of this call for
// the same account will result in the child being replaced by the higher fee
// transaction (RBF).
func (m *manager) BumpAccountFee(ctx context.Context,
	traderKey *btcec.PublicKey, newFeeRate chainfee.SatPerKWeight) error {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return err
	}

	// Only accounts in pending states can have their transaction fees
	// bumped.
	switch account.State {
	case StatePendingOpen, StatePendingUpdate, StatePendingClosed:
	default:
		return fmt.Errorf("cannot bump fee for account in state %v",
			account.State)
	}

	// Since we're using lnd's sweeper for fee bumps, we'll need to find an
	// output in the transaction under its control to perform the CPFP/RBF.
	op := wire.OutPoint{Hash: account.LatestTx.TxHash()}
	for i := range account.LatestTx.TxOut {
		op.Index = uint32(i)

		log.Debugf("Attempting CPFP with %v for account %x", op,
			traderKey.SerializeCompressed())

		err := m.cfg.Wallet.BumpFee(ctx, op, newFeeRate)
		if err != nil {
			// Output isn't known to lnd, continue to the next one.
			// Unfortunately there are two slightly different error
			// messages that can be returned, depending on what code
			// path is taken.
			if strings.Contains(err.Error(), lnwallet.ErrNotMine.Error()) {
				continue
			}
			if strings.Contains(err.Error(), wallet.ErrNotMine.Error()) {
				continue
			}

			// A fatal error occurred, return it.
			return err
		}

		// Once we've found an eligible output, we can return.
		log.Infof("Found eligible output %v for CPFP of account %x",
			op, traderKey.SerializeCompressed())
		return nil
	}

	// If we didn't find an eligible output, report it as an error.
	return fmt.Errorf("transaction %v did not contain any eligible "+
		"outputs to CPFP", op.Hash)
}

// CloseAccount attempts to close the account associated with the given trader
// key. Closing the account requires a signature of the auctioneer if the
// account has not yet expired. The account funds are swept according to the
// provided fee expression.
func (m *manager) CloseAccount(ctx context.Context, traderKey *btcec.PublicKey,
	feeExpr FeeExpr, bestHeight uint32) (*wire.MsgTx, error) {

	account, err := m.cfg.Store.Account(traderKey)
	if err != nil {
		return nil, err
	}

	// Make sure the account hasn't already been closed, or is in the
	// process of doing so.
	if account.State == StatePendingClosed || account.State == StateClosed {
		return nil, errors.New("account has already been closed")
	}

	// Determine the appropriate witness type for the account input based on
	// whether it's expired or not.
	spendWitnessType := determineWitnessType(account, bestHeight)
	scriptVersion := spendWitnessType.scriptVersion()

	// We'll then use the fee expression to determine the closing
	// transaction of the account.
	//
	// If a single output along with a fee rate was provided and the output
	// script was not populated, we'll generate one from the backing lnd
	// node's wallet.
	if feeExpr, ok := feeExpr.(*OutputWithFee); ok && feeExpr.PkScript == nil {
		// If the account is P2TR the wallet supports P2TR change
		// outputs as well.
		changeType := walletrpc.AddressType_WITNESS_PUBKEY_HASH
		if scriptVersion == poolscript.VersionTaprootMuSig2 {
			changeType = walletrpc.AddressType_TAPROOT_PUBKEY
		}

		addr, err := m.cfg.Wallet.NextAddr(ctx, "", changeType, false)
		if err != nil {
			return nil, err
		}
		feeExpr.PkScript, err = txscript.PayToAddrScript(addr)
		if err != nil {
			return nil, err
		}
	}
	closeOutputs, err := feeExpr.CloseOutputs(
		account.Value, spendWitnessType,
	)
	if err != nil {
		return nil, err
	}

	packet, err := m.createSpendTx(account, closeOutputs)
	if err != nil {
		return nil, err
	}

	// Proceed to create the closing transaction and perform any operations
	// thereby required.
	modifiers := []Modifier{
		ValueModifier(0), StateModifier(StatePendingClosed),
	}
	_, spendTx, err := m.spendAccount(
		ctx, account, CLOSE, packet, spendWitnessType, modifiers, bestHeight,
	)
	if err != nil {
		return nil, err
	}

	return spendTx, nil
}

// spendAccount houses most of the logic required to properly spend an account
// by creating the spending transaction, updating persisted account states,
// requesting a signature from the auctioneer if necessary, broadcasting the
// spending transaction, and finally watching for the new account state
// on-chain. These operations are performed in this order to ensure trader are
// able to resume the spend of an account upon restarts if they happen to
// shutdown mid-process.
func (m *manager) spendAccount(ctx context.Context, account *Account,
	action Action, packet *psbt.Packet, witnessType witnessType,
	modifiers []Modifier, bestHeight uint32) (*Account, *wire.MsgTx, error) {

	// In case we're not closing the account, let's now locate our
	// re-created account output.
	accountOutputIdx := -1
	if action != CLOSE {
		// The account output should be recreated, so we need to locate
		// the new account outpoint.
		newAccountOutput, err := account.Copy(modifiers...).Output()
		if err != nil {
			return nil, nil, err
		}
		idx, ok := poolscript.LocateOutputScript(
			packet.UnsignedTx, newAccountOutput.PkScript,
		)
		if !ok {
			return nil, nil, fmt.Errorf("new account output "+
				"script %x not found in spending transaction",
				newAccountOutput.PkScript)
		}

		accountOutputIdx = int(idx)

		// Make sure that we use the appropriate tx hash by including
		// the signature scripts to the inputs that needed it.
		unsignedTx, err := unsignedTxWithSignatureScripts(packet)
		if err != nil {
			return nil, nil, err
		}

		// The TX is finished now, only the witness is missing. So we
		// can create the modifier for the new outpoint now already.
		modifiers = append(modifiers, OutPointModifier(wire.OutPoint{
			Hash:  unsignedTx.TxHash(),
			Index: uint32(accountOutputIdx),
		}))
	}

	var lockTime uint32
	switch witnessType {
	case expiryWitness, expiryTaproot:
		if action != CLOSE {
			return nil, nil, errors.New("modifications for " +
				"expired accounts are not currently supported")
		}

		lockTime = bestHeight

	case multiSigWitness, muSig2Taproot:
		lockTime = 0

	default:
		return nil, nil, fmt.Errorf("unhandled witness type: %v",
			witnessType)
	}

	// Create the spending transaction of an account based on the provided
	// witness type.
	spendTx, err := m.signSpendTx(
		ctx, account, packet, lockTime, witnessType, accountOutputIdx,
		modifiers,
	)
	if err != nil {
		return nil, nil, err
	}

	// Update the account's height hint and latest transaction.
	modifiers = append(modifiers, HeightHintModifier(bestHeight))
	modifiers = append(modifiers, LatestTxModifier(spendTx))

	// With the transaction crafted, update our on-disk state and broadcast
	// the transaction. We'll need some additional modifiers if the account
	// is being modified.
	prevAccountState := account.Copy()
	if err := m.cfg.Store.UpdateAccount(account, modifiers...); err != nil {
		return nil, nil, err
	}

	// Generate transaction label.
	isExpirySpend := witnessType.IsExpirySpend()
	txFee := deriveFeeFromPsbt(packet)
	balanceDiff := account.Value - prevAccountState.Value
	contextLabel := actionTxLabel(
		account, action, isExpirySpend, &txFee, balanceDiff,
	)
	label := makeTxnLabel(m.cfg.TxLabelPrefix, contextLabel)

	if err := m.maybeBroadcastTx(ctx, spendTx, label); err != nil {
		return nil, nil, err
	}

	return account, spendTx, nil
}

// deriveFeeFromPsbt returns the transaction fee from a given PSBT packet.
func deriveFeeFromPsbt(packet *psbt.Packet) btcutil.Amount {
	var sumInputs int64
	for _, packageInput := range packet.Inputs {
		sumInputs += packageInput.WitnessUtxo.Value
	}

	var sumOutputs int64
	for _, txOut := range packet.UnsignedTx.TxOut {
		sumOutputs += txOut.Value
	}

	fee := sumInputs - sumOutputs
	return btcutil.Amount(fee)
}

// RecoverAccount re-introduces a recovered account into the database and starts
// all watchers necessary depending on the account's state.
func (m *manager) RecoverAccount(ctx context.Context, account *Account) error {
	if account.TraderKey == nil || account.TraderKey.PubKey == nil {
		return fmt.Errorf("account is missing trader key")
	}

	// The full trader key descriptor was restored previously and we can now
	// derive the shared secret.
	secret, err := m.cfg.Signer.DeriveSharedKey(
		ctx, account.AuctioneerKey, &account.TraderKey.KeyLocator,
	)
	if err != nil {
		return err
	}
	account.Secret = secret

	// Now store it to the database and start our watchers according to the
	// account's state.
	err = m.cfg.Store.AddAccount(account)
	if err != nil {
		return err
	}

	// Now let's try to resume the account based on the state of it. We set
	// the `onRestart` flag to false because that would try to re-publish
	// the opening transaction in some cases which we don't want. Instead we
	// set the `onRecovery` flag to true. We won't send to the account
	// output again, so we don't need to set a valid funding freeRate.
	return m.resumeAccount(ctx, account, false, true, 0)
}

// determineWitnessType determines the appropriate witness type to use for the
// spending transaction for an account based on its version and whether it has
// expired or not.
func determineWitnessType(account *Account, bestHeight uint32) witnessType {
	switch account.Version {
	case VersionTaprootEnabled, VersionMuSig2V100RC2:
		if account.State == StateExpired ||
			bestHeight >= account.Expiry {

			return expiryTaproot
		}

		return muSig2Taproot
	default:
		if account.State == StateExpired ||
			bestHeight >= account.Expiry {

			return expiryWitness
		}

		return multiSigWitness
	}
}

// getAuctioneerSig requests a signature from the auctioneer for the
// given spending transaction of an account and returns the fully constructed
// witness to spend the account input.
func (m *manager) getAuctioneerSig(ctx context.Context,
	account *Account, spendTx *wire.MsgTx, accountInputIdx,
	accountOutputIdx int, modifiers []Modifier, traderNonces []byte,
	prevOutputs []*wire.TxOut) ([]byte, []byte, error) {

	if accountOutputIdx < 0 {
		// If the account is being closed, we shouldn't provide any
		// modifiers.
		return m.cfg.Auctioneer.ModifyAccount(
			ctx, account, nil, spendTx.TxOut, nil, traderNonces,
			prevOutputs,
		)
	}

	// Otherwise, the account output is being re-created due to a
	// modification, so we need to filter out its spent input and re-created
	// output from the spending transaction as the auctioneer can
	// reconstruct those themselves.
	inputs := make([]*wire.TxIn, 0, len(spendTx.TxIn)-1)
	inputs = append(inputs, spendTx.TxIn[:accountInputIdx]...)
	inputs = append(inputs, spendTx.TxIn[accountInputIdx+1:]...)

	outputs := make([]*wire.TxOut, 0, len(spendTx.TxOut)-1)
	outputs = append(outputs, spendTx.TxOut[:accountOutputIdx]...)
	outputs = append(outputs, spendTx.TxOut[accountOutputIdx+1:]...)

	return m.cfg.Auctioneer.ModifyAccount(
		ctx, account, inputs, outputs, modifiers, traderNonces,
		prevOutputs,
	)
}

// createSpendTx creates a PSBT that spends the current account output.
func (m *manager) createSpendTx(account *Account,
	outputs []*wire.TxOut) (*psbt.Packet, error) {

	tx := wire.NewMsgTx(2)
	tx.TxOut = append(tx.TxOut, outputs...)
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: account.OutPoint,
	}}

	// The transaction should have its inputs and outputs sorted according
	// to BIP-69.
	txsort.InPlaceSort(tx)

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, err
	}
	packet.Inputs[0].WitnessUtxo, err = account.Output()
	if err != nil {
		return nil, err
	}

	return packet, nil
}

// signSpendTx creates the spending transaction of an account and signs it.
// If the spending transaction takes the expiration path, bestHeight is used as
// the lock time of the transaction, otherwise it is 0. The transaction has its
// inputs and outputs sorted according to BIP-69.
func (m *manager) signSpendTx(ctx context.Context, account *Account,
	packet *psbt.Packet, lockTime uint32, witnessType witnessType,
	accountOutputIndex int, modifiers []Modifier) (*wire.MsgTx, error) {

	// lockTime is used as the lock time of the transaction in order to
	// satisfy the output's CHECKLOCKTIMEVERIFY in case we're using the
	// expiry witness.
	packet.UnsignedTx.LockTime = lockTime

	// Ensure the transaction crafted passes some basic sanity checks before
	// we attempt to sign it.
	err := sanityCheckAccountSpendTx(account, packet, witnessType)
	if err != nil {
		return nil, err
	}

	// Now let's try and add the signature for the account input that's
	// being spent. Depending on the expiry of the account, this might need
	// the cooperation of the auctioneer to get a second signature.
	signedPacket, err := m.addAccountSpendSignature(
		ctx, account, packet, witnessType, accountOutputIndex, modifiers,
	)
	if err != nil {
		return nil, err
	}

	// We either have a single account input (renew, withdraw, close) that
	// has a final script witness set or we have additional inputs (deposit)
	// that we need to sign for now. We use the FinalizePsbt method for the
	// additional inputs as they belong to the normal wallet and can be
	// signed for without additional PSBT metadata fields.
	var signedTx *wire.MsgTx
	if signedPacket.IsComplete() {
		err = psbt.MaybeFinalizeAll(signedPacket)
		if err != nil {
			return nil, fmt.Errorf("error finalizing PSBT: %v", err)
		}

		signedTx, err = psbt.Extract(signedPacket)
		if err != nil {
			return nil, fmt.Errorf("error extracting TX: %v", err)
		}
	} else {
		// We should be able to extract the final TX now, even if the
		// witness isn't yet fully correct just yet.
		_, signedTx, err = m.cfg.Wallet.FinalizePsbt(
			ctx, signedPacket, "",
		)
		if err != nil {
			return nil, fmt.Errorf("error finalizing TX: %v", err)
		}
	}

	return signedTx, nil
}

// addAccountSpendSignature returns a new PSBT packet with the final witness of
// the account input to spend fully populated.
func (m *manager) addAccountSpendSignature(ctx context.Context, account *Account,
	packet *psbt.Packet, witnessType witnessType, accountOutputIndex int,
	modifiers []Modifier) (*psbt.Packet, error) {

	// Use a deep copy of the packet.UnsignedTx that includes the
	// SignatureScripts so the auctioneer can calculate the proper
	// outpoint for the account.
	unsignedTx, err := unsignedTxWithSignatureScripts(packet)
	if err != nil {
		return nil, err
	}

	// Determine the new index of the account input now that we know we have
	// the full transaction.
	accountInputIdx, err := locateAccountInput(unsignedTx, account)
	if err != nil {
		return nil, err
	}

	// We now need to add all the PSBT meta information about our account
	// input to the packet, even if we're going to sign the input using
	// MuSig2.
	controlBlock, err := m.decorateAccountInput(
		account, packet, accountInputIdx,
	)
	if err != nil {
		return nil, err
	}

	// Collect all previous outputs that we need to know in case we're
	// signing a Taproot input.
	prevOutputs := make([]*wire.TxOut, len(unsignedTx.TxIn))
	for idx := range unsignedTx.TxIn {
		prevOutputs[idx] = packet.Inputs[idx].WitnessUtxo
	}

	// The collaborative MuSig2 case is fairly simple when it comes to the
	// witness. It's a single signature put on the stack. To get the
	// final signature by combining the trader's and auctioneer's partial
	// sigs is a bit more involved though.
	if witnessType == muSig2Taproot {
		combinedSig, err := m.signAccountMuSig2(
			ctx, account, unsignedTx, accountOutputIndex,
			accountInputIdx, modifiers, prevOutputs,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating account "+
				"MuSig2 combined signature: %v", err)
		}

		pIn := &packet.Inputs[accountInputIdx]
		witness := poolscript.SpendMuSig2Taproot(combinedSig)
		pIn.FinalScriptWitness, err = serializeWitness(witness)
		if err != nil {
			return nil, fmt.Errorf("error serializing witness: %v",
				err)
		}

		return packet, nil
	}

	// Let the wallet sign each input. This will add a partial signature for
	// the account input which we'll later turn into the correct witness
	// depending on the spend path.
	signedPacket, err := m.cfg.Wallet.SignPsbt(ctx, packet)
	if err != nil {
		return nil, err
	}

	pIn := &signedPacket.Inputs[accountInputIdx]
	witnessScript := pIn.WitnessScript
	var ourSig []byte

	switch witnessType {
	case expiryTaproot:
		if len(pIn.TaprootScriptSpendSig) != 1 {
			return nil, fmt.Errorf("unexpected number of "+
				"signatures in signed PSBT, got %d wanted 1",
				len(pIn.PartialSigs))
		}

		ourSig = pIn.TaprootScriptSpendSig[0].Signature
		if pIn.TaprootScriptSpendSig[0].SigHash != txscript.SigHashDefault {
			ourSig = append(ourSig, byte(
				pIn.TaprootScriptSpendSig[0].SigHash,
			))
		}

	default:
		if len(pIn.PartialSigs) != 1 {
			return nil, fmt.Errorf("unexpected number of "+
				"signatures in signed PSBT, got %d wanted 1",
				len(pIn.PartialSigs))
		}

		ourSig = pIn.PartialSigs[0].Signature
	}

	// We temporarily set the final witness to the partial sig to allow the
	// extraction of the final TX. Unless we're using the expiry path in
	// which case we _can_ create the full and final witness.
	switch witnessType {
	case expiryTaproot:
		pIn.FinalScriptWitness, err = serializeWitness(
			poolscript.SpendExpiryTaproot(
				witnessScript, ourSig, controlBlock,
			),
		)

	case expiryWitness:
		pIn.FinalScriptWitness, err = serializeWitness(
			poolscript.SpendExpiry(witnessScript, ourSig),
		)

	case multiSigWitness:
		// We're not signing a Taproot input, so we don't need to
		// specify any trader nonces.
		var auctioneerSig []byte
		auctioneerSig, _, err = m.getAuctioneerSig(
			ctx, account, unsignedTx, accountInputIdx,
			accountOutputIndex, modifiers, nil, prevOutputs,
		)
		if err != nil {
			return nil, err
		}
		witness := poolscript.SpendMultiSig(
			witnessScript, ourSig, auctioneerSig,
		)
		pIn.FinalScriptWitness, err = serializeWitness(witness)

	default:
		return nil, fmt.Errorf("invalid state, should never get here")
	}
	if err != nil {
		return nil, fmt.Errorf("error serializing witness: %v",
			err)
	}

	return signedPacket, nil
}

// signAccountMuSig2 creates the combined MuSig2 signature to spend a Taproot
// account output through the collaborative key spend path. This sets up a
// MuSig2 signing session on the local signer instance and then asks the
// auctioneer to also send its partial signature.
func (m *manager) signAccountMuSig2(ctx context.Context, account *Account,
	spendTx *wire.MsgTx, accountOutputIndex, accountInputIdx int,
	modifiers []Modifier, previousOutputs []*wire.TxOut) ([]byte, error) {

	sessionInfo, cleanup, err := poolscript.TaprootMuSig2SigningSession(
		ctx, account.Version.ScriptVersion(), account.Expiry,
		account.TraderKey.PubKey, account.BatchKey, account.Secret,
		account.AuctioneerKey, m.cfg.Signer,
		&account.TraderKey.KeyLocator, nil,
	)
	if err != nil {
		return nil, err
	}

	auctioneerSigBytes, auctioneerNonceBytes, err := m.getAuctioneerSig(
		ctx, account, spendTx, accountInputIdx, accountOutputIndex,
		modifiers, sessionInfo.PublicNonce[:], previousOutputs,
	)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("error getting auctioneer MuSig2 "+
			"partial signature: %v", err)
	}

	var (
		remoteNonces     poolscript.MuSig2Nonces
		remotePartialSig [input.MuSig2PartialSigSize]byte
	)
	copy(remoteNonces[:], auctioneerNonceBytes)
	copy(remotePartialSig[:], auctioneerSigBytes)

	finalSig, err := poolscript.TaprootMuSig2Sign(
		ctx, accountInputIdx, sessionInfo, m.cfg.Signer, spendTx,
		previousOutputs, &remoteNonces, &remotePartialSig,
	)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("error signing batch TX: %v", err)
	}

	return finalSig, nil
}

// addBaseAccountModificationWeight adds the estimated weight units for a
// transaction that modifies an account by spending the current account input
// and creating the new account output according to the provided `witnessType`.
func addBaseAccountModificationWeight(weightEstimator *input.TxWeightEstimator,
	witnessType witnessType) error {

	witnessSize, err := witnessType.witnessSize()
	if err != nil {
		return err
	}

	weightEstimator.AddWitnessInput(witnessSize)

	weightEstimator.AddP2WSHOutput()

	return nil
}

// valueAfterAccountUpdate determines the new value of an account after
// processing a withdrawal to the specified outputs at the provided fee rate.
func valueAfterAccountUpdate(account *Account, outputs []*wire.TxOut,
	witnessType witnessType,
	feeRate chainfee.SatPerKWeight) (btcutil.Amount, error) {

	// To determine the new value of the account, we'll need to subtract the
	// values of all additional outputs and the resulting fee of the
	// transaction, which we'll need to compute based on its weight.
	//
	// Right off the bat, we'll add weight estimates for the existing
	// account output that we're spending, and the new account output being
	// created.
	var weightEstimator input.TxWeightEstimator
	err := addBaseAccountModificationWeight(&weightEstimator, witnessType)
	if err != nil {
		return 0, err
	}

	// We'll then add the weight estimates for any additional outputs
	// provided, keeping track of the total output value sum as we go.
	var outputTotal btcutil.Amount
	for _, out := range outputs {
		// To determine the proper weight of the output, we'll need to
		// know its type.
		pkScript, err := txscript.ParsePkScript(out.PkScript)
		if err != nil {
			return 0, fmt.Errorf("unable to parse output script "+
				"%x: %v", out.PkScript, err)
		}

		switch pkScript.Class() {
		case txscript.ScriptHashTy:
			weightEstimator.AddP2SHOutput()
		case txscript.WitnessV0PubKeyHashTy:
			weightEstimator.AddP2WKHOutput()
		case txscript.WitnessV0ScriptHashTy:
			weightEstimator.AddP2WSHOutput()
		case txscript.WitnessV1TaprootTy:
			weightEstimator.AddP2TROutput()
		default:
			return 0, fmt.Errorf("unsupported output script %x",
				out.PkScript)
		}

		outputTotal += btcutil.Amount(out.Value)
	}

	// With the weight estimated, compute the fee, which we'll then subtract
	// from our input total and ensure our new account value isn't below our
	// required minimum.
	fee := feeRate.FeeForWeight(int64(weightEstimator.Weight()))
	newAccountValue := account.Value - outputTotal - fee
	if newAccountValue < MinAccountValue {
		return 0, fmt.Errorf("new account value is below accepted "+
			"minimum of %v", MinAccountValue)
	}

	return newAccountValue, nil
}

// inputsForDeposit returns a list of inputs sources from the backing lnd node's
// wallet which we can use to satisfy an account deposit. A closure to release
// the inputs is also provided to use when coming across an unexpected failure.
// If needed, a change output from the backing lnd node's wallet may be returned
// as well.
func (m *manager) inputsForDeposit(ctx context.Context, account *Account,
	newAccountOutput *wire.TxOut, depositAmount btcutil.Amount,
	witnessType witnessType, feeRate chainfee.SatPerKWeight) (*psbt.Packet,
	func(), error) {

	// Unfortunately the FundPsbt call doesn't allow us to specify _any_
	// inputs, otherwise it won't perform coin selection at all. So what we
	// do instead is to fund our account output just for the funding amount
	// plus whatever we need to pay for the additional input (which we know
	// exactly how big it will be). Then we add the account input and its
	// value to the account output.
	var acctInputEstimator input.TxWeightEstimator
	witnessSize, err := witnessType.witnessSize()
	if err != nil {
		return nil, nil, err
	}
	acctInputEstimator.AddWitnessInput(witnessSize)
	acctInputWeight := int64(acctInputEstimator.Weight())
	acctInputFee := feeRate.FeeForWeight(acctInputWeight)

	outputToFund := &wire.TxOut{
		Value:    int64(depositAmount + acctInputFee),
		PkScript: newAccountOutput.PkScript,
	}
	tplPacket, err := psbt.New(nil, []*wire.TxOut{outputToFund}, 2, 0, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating template PSBT: %v",
			err)
	}

	var tplBytes bytes.Buffer
	if err := tplPacket.Serialize(&tplBytes); err != nil {
		return nil, nil, fmt.Errorf("error serializing template PSBT: "+
			"%v", err)
	}

	packet, changeOutputIdx, lockedCoins, err := m.cfg.Wallet.FundPsbt(
		ctx, &walletrpc.FundPsbtRequest{
			Template: &walletrpc.FundPsbtRequest_Psbt{
				Psbt: tplBytes.Bytes(),
			},
			MinConfs: 1,
			Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
				SatPerVbyte: uint64(
					feeRate.FeePerKVByte() / 1000,
				),
			},
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error funding PSBT: %v", err)
	}

	releaseInputs := func() {
		for _, coin := range lockedCoins {
			var lockID wtxmgr.LockID
			copy(lockID[:], coin.Id)

			hash, _ := chainhash.NewHash(coin.Outpoint.TxidBytes)
			op := wire.OutPoint{
				Hash:  *hash,
				Index: coin.Outpoint.OutputIndex,
			}
			_ = m.cfg.Wallet.ReleaseOutput(ctx, lockID, op)
		}
	}

	// Due to a bug in lnd 0.14.2 up to 0.15.0 we can't use SignPsbt for
	// np2wkh inputs. Unfortunately our only choice in the case that we get
	// such an input selected is to tell the user to upgrade or "migrate"
	// their coins. We only check for lnd version > 0.15.0 because our
	// minimum required version is 0.14.3 anyway.
	lnd151 := &verrpc.Version{
		AppMajor: 0,
		AppMinor: 15,
		AppPatch: 1,
	}
	err = lndclient.AssertVersionCompatible(m.cfg.LndVersion, lnd151)
	isOldLnd := err != nil

	// Unfortunately we can't send an input's sequence to the server, that
	// field doesn't exist in the RPC. And the server always assumes a
	// sequence of 0. So we need to overwrite the value that lnd set in the
	// funding call, otherwise we arrive at a different sighash.
	for idx := range packet.UnsignedTx.TxIn {
		packet.UnsignedTx.TxIn[idx].Sequence = 0

		// Abort if we have any np2wkh inputs with an old lnd.
		if len(packet.Inputs[idx].RedeemScript) > 0 && isOldLnd {
			releaseInputs()
			return nil, nil, fmt.Errorf("due to a bug in lnd " +
				"versions prior to v0.15.1-beta, depositing " +
				"from np2wkh inputs is not possible; please " +
				"upgrade your lnd or forward your coins to a " +
				"native SegWit (p2wkh) address")
		}
	}

	// Make sure the previous account is spent into a new output.
	packet.UnsignedTx.TxIn = append(packet.UnsignedTx.TxIn, &wire.TxIn{
		PreviousOutPoint: account.OutPoint,
	})
	packet.Inputs = append(packet.Inputs, psbt.PInput{})

	// Make sure we have our account output in there and at the same time
	// fix that output's value now to the actual account balance we want to
	// end up at.
	for idx, txOut := range packet.UnsignedTx.TxOut {
		// Skip the change output if there is any.
		if changeOutputIdx >= 0 && changeOutputIdx == int32(idx) {
			continue
		}

		// If we get here, this _must_ be the account output. Otherwise,
		// there is a weird output in the transaction, and we need to
		// abort.
		if !bytes.Equal(txOut.PkScript, newAccountOutput.PkScript) {
			releaseInputs()
			return nil, nil, fmt.Errorf("account output not " +
				"found in funded packet")
		}

		// We expect the output to be funded to exactly the amount we
		// specified.
		if txOut.Value != int64(depositAmount+acctInputFee) {
			releaseInputs()
			return nil, nil, fmt.Errorf("account output funded "+
				"with incorrect value %d, expected %d",
				txOut.Value, depositAmount+acctInputFee)
		}

		packet.UnsignedTx.TxOut[idx].Value = newAccountOutput.Value
	}

	// We now need to make sure we sort the whole transaction according to
	// BIP69.
	if err := psbt.InPlaceSort(packet); err != nil {
		releaseInputs()
		return nil, nil, err
	}

	return packet, releaseInputs, nil
}

// createNewAccountOutput creates the next account output in the sequence using
// the new account value and optional new account expiry.
func createNewAccountOutput(account *Account, newAccountValue btcutil.Amount,
	newAccountExpiry *uint32, newVersion *Version) (*wire.TxOut, []Modifier,
	error) {

	modifiers := []Modifier{
		ValueModifier(newAccountValue),
		IncrementBatchKey(),
	}
	if newAccountExpiry != nil {
		modifiers = append(modifiers, ExpiryModifier(*newAccountExpiry))
	}
	if newVersion != nil && *newVersion > account.Version {
		modifiers = append(modifiers, VersionModifier(*newVersion))
	}

	newAccountOutput, err := account.Copy(modifiers...).Output()
	if err != nil {
		return nil, nil, err
	}

	return newAccountOutput, modifiers, nil
}

// sanityCheckAccountSpendTx ensures that the spending transaction of an account
// is well-formed by performing various sanity checks on its inputs and outputs.
// It returns the total amount of fees in satoshis paid by the transaction.
func sanityCheckAccountSpendTx(account *Account, packet *psbt.Packet,
	witnessType witnessType) error {

	tx, err := unsignedTxWithSignatureScripts(packet)
	if err != nil {
		return err
	}
	err = blockchain.CheckTransactionSanity(btcutil.NewTx(tx))
	if err != nil {
		return err
	}

	// None of the outputs should be dust.
	for _, output := range tx.TxOut {
		if txrules.IsDustOutput(output, txrules.DefaultRelayFeePerKb) {
			return fmt.Errorf("dust output %x", output.PkScript)
		}
	}

	// CheckTransactionSanity doesn't have enough context to attempt fee
	// calculation, but we do.
	var (
		inputTotal, outputTotal btcutil.Amount
		witnessSize             int64
	)
	for idx, inp := range tx.TxIn {
		pIn := packet.Inputs[idx]
		if inp.PreviousOutPoint == account.OutPoint {
			inputTotal += account.Value

			acctWitnessSize, err := witnessType.witnessSize()
			if err != nil {
				return err
			}
			witnessSize += int64(acctWitnessSize)
		} else {
			utxo := pIn.WitnessUtxo
			inputTotal += btcutil.Amount(utxo.Value)

			pkScript, err := txscript.ParsePkScript(utxo.PkScript)
			if err != nil {
				return err
			}

			switch pkScript.Class() {
			case txscript.WitnessV0PubKeyHashTy:
				witnessSize += input.P2WKHWitnessSize

			case txscript.ScriptHashTy:
				// The witness of a np2wkh input is the same as
				// a p2wkh input. The only difference is the
				// additional scriptSig which will be added to
				// the TX before calculating its weight.
				witnessSize += input.P2WKHWitnessSize

			case txscript.WitnessV1TaprootTy:
				witnessSize += 1 + 1 + schnorr.SignatureSize

			default:
				return fmt.Errorf("unsupported deposit input "+
					"of class <%s>", pkScript.Class())
			}
		}
	}
	for _, output := range tx.TxOut {
		outputTotal += btcutil.Amount(output.Value)
	}

	if inputTotal < outputTotal {
		return fmt.Errorf("output value of %v exceeds input value "+
			"of %v", outputTotal, inputTotal)
	}

	feesPaid := inputTotal - outputTotal

	// The unsigned TX within the package doesn't have any witness set.
	// We'll add the witness weight manually in the next step. Fortunately
	// with the PSBT funding all the fees should've already been calculated
	// properly before.
	txWeightNoWitness := blockchain.GetTransactionWeight(btcutil.NewTx(tx))

	// The witness size can be translated to weight directly, no scale
	// factor is needed. But we need to add the 2 bytes for the marker and
	// flag fields that weren't counted above because the unsigned TX has no
	// witness.
	fullWeight := txWeightNoWitness + 2 + witnessSize
	minRelayFee := chainfee.FeePerKwFloor.FeeForWeight(fullWeight)
	if feesPaid < minRelayFee {
		return fmt.Errorf("signed transaction only pays %d sats "+
			"in fees while %d are required for relay", feesPaid,
			minRelayFee)
	}

	return nil
}

// locateAccountInput locates the index of the account input in the provided
// transaction or returns an error.
func locateAccountInput(tx *wire.MsgTx, account *Account) (int, error) {
	for i, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == account.OutPoint {
			return i, nil
		}
	}
	return 0, errors.New("account input not found")
}

// unsignedTxWithSignatureScripts returns a deep copy of the unsigned tx
// in the packet but includes the SignatureScripts for the inputs that have
// an associated RedeemScript.
//
// Note: an unsigned tx with and without the related SignatureScripts have a
// different TxHash. However, SignatureScripts are not part of the data signed.
func unsignedTxWithSignatureScripts(packet *psbt.Packet) (*wire.MsgTx, error) {
	tx := packet.UnsignedTx.Copy()
	for idx := range packet.Inputs {
		if len(packet.Inputs[idx].RedeemScript) > 0 {
			builder := txscript.NewScriptBuilder()
			builder.AddData(packet.Inputs[idx].RedeemScript)
			sigScript, err := builder.Script()
			if err != nil {
				return nil, err
			}
			tx.TxIn[idx].SignatureScript = sigScript
		}
	}

	return tx, nil
}

// decorateAccountInput signs the account input in the spending transaction of an
// account. If the account is being spent with cooperation of the auctioneer,
// their signature will be required as well.
func (m *manager) decorateAccountInput(account *Account, packet *psbt.Packet,
	idx int) ([]byte, error) {

	traderKeyTweak := poolscript.TraderKeyTweak(
		account.BatchKey, account.Secret, account.TraderKey.PubKey,
	)
	witnessScript, err := poolscript.AccountWitnessScript(
		account.Expiry, account.TraderKey.PubKey, account.AuctioneerKey,
		account.BatchKey, account.Secret,
	)
	if err != nil {
		return nil, err
	}

	accountOutput, err := account.Output()
	if err != nil {
		return nil, err
	}

	pIn := &packet.Inputs[idx]
	pIn.WitnessUtxo = accountOutput
	pIn.SighashType = sigHashForScript(accountOutput.PkScript)
	pIn.WitnessScript = witnessScript

	bip32Path := []uint32{
		keychain.BIP0043Purpose + hdkeychain.HardenedKeyStart,
		m.cfg.ChainParams.HDCoinType + hdkeychain.HardenedKeyStart,
		uint32(account.TraderKey.Family) + hdkeychain.HardenedKeyStart,
		0,
		account.TraderKey.Index,
	}
	pIn.Bip32Derivation = []*psbt.Bip32Derivation{{
		Bip32Path: bip32Path,
		PubKey:    account.TraderKey.PubKey.SerializeCompressed(),
	}}
	pIn.Unknowns = append(pIn.Unknowns, &psbt.Unknown{
		Key:   btcwallet.PsbtKeyTypeInputSignatureTweakSingle,
		Value: traderKeyTweak,
	})

	var controlBlockBytes []byte
	if account.Version >= VersionTaprootEnabled {
		scriptVersion := account.Version.ScriptVersion()
		aggregateKey, expiryScript, err := poolscript.TaprootKey(
			scriptVersion, account.Expiry, account.TraderKey.PubKey,
			account.AuctioneerKey, account.BatchKey, account.Secret,
		)
		if err != nil {
			return nil, fmt.Errorf("error creating taproot key: %v",
				err)
		}
		pIn.WitnessScript = expiryScript.Script

		tapscript := input.TapscriptFullTree(
			aggregateKey.PreTweakedKey, *expiryScript,
		)
		controlBlockBytes, err = tapscript.ControlBlock.ToBytes()
		if err != nil {
			return nil, fmt.Errorf("error serializing control "+
				"block: %v", err)
		}

		expiryScriptLeafHash := expiryScript.TapHash()
		pIn.TaprootBip32Derivation = []*psbt.TaprootBip32Derivation{{
			XOnlyPubKey: schnorr.SerializePubKey(
				account.TraderKey.PubKey,
			),
			LeafHashes: [][]byte{expiryScriptLeafHash[:]},
			Bip32Path:  bip32Path,
		}}
		pIn.TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
			ControlBlock: controlBlockBytes,
			Script:       expiryScript.Script,
			LeafVersion:  expiryScript.LeafVersion,
		}}
	}

	return controlBlockBytes, nil
}

// validateAccountValue ensures that a trader has provided a sane account value
// for the creation of a new account.
func validateAccountValue(value, maxValue btcutil.Amount) error {
	if value < MinAccountValue {
		return fmt.Errorf("minimum account value allowed is %v",
			MinAccountValue)
	}
	if value > maxValue {
		return fmt.Errorf("maximum account value allowed is %v",
			maxValue)
	}

	return nil
}

// validateAccountExpiry ensures that a trader has provided a sane account expiry
// for the creation/modification of an account.
func validateAccountExpiry(expiry, bestHeight uint32) error {
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

// validateAccountParams ensures that a trader has provided sane parameters for
// the creation of a new account.
func validateAccountParams(value, maxValue btcutil.Amount, expiry,
	bestHeight uint32, version Version) error {

	err := validateAccountValue(value, maxValue)
	if err != nil {
		return err
	}
	err = validateAccountExpiry(expiry, bestHeight)
	if err != nil {
		return err
	}
	return ValidateVersion(version)
}

// NumConfsForValue chooses an appropriate number of confirmations to wait for
// an account based on its initial value.
//
// TODO(wilmer): Determine the recommend number of blocks to wait for a
// particular output size given the current block reward and a user's "risk
// threshold" (basically a multiplier for the amount of work/fiat-burnt that
// would need to be done to undo N blocks).
func NumConfsForValue(value, maxAccountValue btcutil.Amount) uint32 {
	confs := maxConfs * value / maxAccountValue
	if confs < minConfs {
		confs = minConfs
	}
	if confs > maxConfs {
		confs = maxConfs
	}
	return uint32(confs)
}

// makeTxnLabel makes a transaction label for a given account given a static
// label prefix and a context-specific label.
func makeTxnLabel(labelPrefix, contextLabel string) string {
	var label string

	// If we have a label prefix, then we'll apply that now and leave a space
	// at the end as well to separate it from the contextLabel.
	if labelPrefix != "" {
		label += labelPrefix + " "
	}

	label += contextLabel

	// If after applying our context label, the label is too long (exceeds
	// the 500 char limit), we'll truncate the label to ensure we continue
	// operation, and send a warning message to the user.
	if len(label) > wtxmgr.TxLabelLimit {
		log.Warnf("label=%v is too long (size=%v, max_size=%v)",
			label, len(label), wtxmgr.TxLabelLimit)

		label = label[:wtxmgr.TxLabelLimit]
	}

	return label
}

// serializeWitness turns a wire witness into its serialized form.
func serializeWitness(witness wire.TxWitness) ([]byte, error) {
	var buf bytes.Buffer
	if err := psbt.WriteTxWitness(&buf, witness); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// sigHashForScript returns the sighash flag for the given UTXO's pkScript.
func sigHashForScript(pkScript []byte) txscript.SigHashType {
	switch {
	case txscript.IsPayToTaproot(pkScript):
		return txscript.SigHashDefault

	default:
		return txscript.SigHashAll
	}
}
