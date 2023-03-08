package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// ErrNoPendingBatch is an error returned when we attempt to retrieve
	// the ID of a pending batch, but one does not exist.
	ErrNoPendingBatch = errors.New("no pending batch found")
)

// Reservation contains information about the different keys required for to
// create a new account.
type Reservation struct {
	// AuctioneerKey is the base auctioneer's key in the 2-of-2 multi-sig
	// construction of a CLM account. This key will never be included in the
	// account script, but rather it will be tweaked with the per-batch
	// trader key to prevent script reuse and provide plausible deniability
	// between account outputs to third parties.
	AuctioneerKey *btcec.PublicKey

	// InitialBatchKey is the initial batch key that is used to tweak the
	// trader key of an account.
	InitialBatchKey *btcec.PublicKey
}

// Version represents the version of an account.
type Version uint8

const (
	// VersionInitialNoVersion is the initial version any legacy account has
	// that technically wasn't versioned at all. The version field isn't
	// even serialized for those accounts.
	VersionInitialNoVersion Version = 0

	// VersionTaprootEnabled is the version that introduced account
	// versioning and the upgrade to Taproot (with MuSig2 multi-sig).
	VersionTaprootEnabled Version = 1

	// VersionMuSig2V100RC2 is the version that bumped the MuSig2 protocol
	// version used to v1.0.0-rc2. This is the very verbose code internal
	// version name, in anything user facing, we'll just call this
	// "Taproot v2" to keep it simple and short.
	VersionMuSig2V100RC2 Version = 2
)

// String returns the string representation of the version.
func (v Version) String() string {
	switch v {
	case VersionInitialNoVersion:
		return "account_p2wsh"

	case VersionTaprootEnabled:
		return "account_p2tr"

	case VersionMuSig2V100RC2:
		return "account_p2tr_v2"

	default:
		return fmt.Sprintf("unknown <%d>", v)
	}
}

// ScriptVersion returns the version of the pool script used by this account
// version.
func (v Version) ScriptVersion() poolscript.Version {
	switch v {
	case VersionTaprootEnabled:
		return poolscript.VersionTaprootMuSig2

	case VersionMuSig2V100RC2:
		return poolscript.VersionTaprootMuSig2V100RC2

	default:
		return poolscript.VersionWitnessScript
	}
}

// ValidateVersion ensures that a given version is a valid and known version.
func ValidateVersion(version Version) error {
	switch version {
	case VersionInitialNoVersion, VersionTaprootEnabled,
		VersionMuSig2V100RC2:

		return nil

	default:
		return fmt.Errorf("unknown version <%d>", version)
	}
}

// State describes the different possible states of an account.
type State uint8

// NOTE: We avoid the use of iota as these can be persisted to disk.
const (
	// StateInitiated denotes the initial state of an account. When an
	// account is in this state, then it should be funded with a
	// transaction.
	StateInitiated State = 0

	// StatePendingOpen denotes that we've broadcast the account's funding
	// transaction and are currently waiting for its confirmation.
	StatePendingOpen State = 1

	// StatePendingUpdate denotes that the account has undergone an update
	// on-chain as part of a trader modification and we are currently
	// waiting for its confirmation.
	StatePendingUpdate State = 2

	// StateOpen denotes that the account's funding transaction has been
	// included in the chain with sufficient depth.
	StateOpen State = 3

	// StateExpired denotes that the chain has reached an account's
	// expiration height. An account in this state can still be used if
	// renewed.
	StateExpired State = 4

	// StatePendingClosed denotes that an account was fully spent by a
	// transaction broadcast by the trader and is pending its confirmation.
	StatePendingClosed State = 5

	// StateClosed denotes that an account was closed by a transaction
	// broadcast by the trader that fully spent the account. An account in
	// this state can no longer be used.
	StateClosed State = 6

	// StateCanceledAfterRecovery denotes that the account was attempted to
	// be recovered but failed because the opening transaction wasn't found
	// by lnd. This could be because it was never published or it never
	// confirmed. Then the funds are SAFU and the account can be considered
	// to never have been opened in the first place.
	StateCanceledAfterRecovery State = 7

	// StatePendingBatch denotes an account that recently participated in a
	// batch and is not yet confirmed.
	StatePendingBatch State = 8

	// StateExpiredPendingUpdate denotes that the chain has reached an
	// account's expiration height while the account had a pending update
	// that has yet to confirm. This state exists to ensure an account can
	// only be renewed once confirmed and expired.
	StateExpiredPendingUpdate State = 9
)

// String returns a human-readable description of an account's state.
func (s State) String() string {
	switch s {
	case StateInitiated:
		return "StateInitiated"
	case StatePendingOpen:
		return "StatePendingOpen"
	case StatePendingUpdate:
		return "StatePendingUpdate"
	case StateOpen:
		return "StateOpen"
	case StateExpired:
		return "StateExpired"
	case StatePendingClosed:
		return "StatePendingClosed"
	case StateClosed:
		return "StateClosed"
	case StateCanceledAfterRecovery:
		return "StateCanceledAfterRecovery"
	case StatePendingBatch:
		return "StatePendingBatch"
	case StateExpiredPendingUpdate:
		return "StateExpiredPendingUpdate"
	default:
		return "unknown"
	}
}

// IsActive returns true if the state is considered to be an active account
// state.
func (s State) IsActive() bool {
	switch s {
	case StateClosed, StateCanceledAfterRecovery:
		return false

	default:
		return true
	}
}

// Account encapsulates all of the details of a CLM account on-chain from
// the trader's perspective.
type Account struct {
	// Value is the value of the account reflected in on-chain output that
	// backs the existence of an account.
	Value btcutil.Amount

	// Expiry is the expiration block height of an account. After this
	// point, the trader is able to withdraw the funds from their account
	// without cooperation of the auctioneer.
	Expiry uint32

	// TraderKey is the base trader's key in the 2-of-2 multi-sig
	// construction of a CLM account. This key will never be included in the
	// account script, but rather it will be tweaked with the per-batch key
	// and the account secret to prevent script reuse and provide plausible
	// deniability between account outputs to third parties.
	TraderKey *keychain.KeyDescriptor

	// AuctioneerKey is the base auctioneer's key in the 2-of-2 multi-sig
	// construction of a CLM account. This key will never be included in the
	// account script, but rather it will be tweaked with the per-batch
	// trader key to prevent script reuse and provide plausible deniability
	// between account outputs to third parties.
	AuctioneerKey *btcec.PublicKey

	// BatchKey is the batch key that is used to tweak the trader key of an
	// account with, along with the secret. This will be incremented by the
	// curve's base point each time the account is modified or participates
	// in a cleared batch to prevent output script reuse for accounts
	// on-chain.
	BatchKey *btcec.PublicKey

	// Secret is a static shared secret between the trader and the
	// auctioneer that is used to tweak the trader key of an account with,
	// along with the batch key. This ensures that only the trader and
	// auctioneer are able to successfully identify every past/future output
	// of an account.
	Secret [32]byte

	// State describes the state of the account.
	State State

	// HeightHint is the earliest height in the chain at which we can find
	// the account output in a block.
	HeightHint uint32

	// OutPoint is the outpoint of the output used to fund the account. This
	// only exists once the account has reached StatePendingOpen.
	OutPoint wire.OutPoint

	// LatestTx is the latest transaction of an account.
	//
	// NOTE: This is only nil within the StateInitiated phase. There are no
	// guarantees as to whether the transaction has its witness populated.
	LatestTx *wire.MsgTx

	// Version is the version of the account.
	Version Version
}

const (
	// DefaultFundingConfTarget is the default value used for the account
	// funding/init target number of blocks to confirmation. We choose a
	// very high value of one week to arrive at essentially 1 sat/vByte
	// which used to be the previous default when creating the transaction.
	DefaultFundingConfTarget uint32 = 144 * 7
)

// Output returns the current on-chain output associated with the account.
func (a *Account) Output() (*wire.TxOut, error) {
	script, err := poolscript.AccountScript(
		a.Version.ScriptVersion(), a.Expiry, a.TraderKey.PubKey,
		a.AuctioneerKey, a.BatchKey, a.Secret,
	)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value:    int64(a.Value),
		PkScript: script,
	}, nil
}

// NextOutputScript returns the next on-chain output script that is to be
// associated with the account. This is done by using the next batch key, which
// results from incrementing the current one by its curve's base point.
func (a *Account) NextOutputScript() ([]byte, error) {
	nextBatchKey := poolscript.IncrementKey(a.BatchKey)
	return poolscript.AccountScript(
		a.Version.ScriptVersion(), a.Expiry, a.TraderKey.PubKey,
		a.AuctioneerKey, nextBatchKey, a.Secret,
	)
}

// CopyPubKey creates a copy of a public key.
func CopyPubKey(pub *btcec.PublicKey) *btcec.PublicKey {
	newPubKey, _ := btcec.ParsePubKey(pub.SerializeCompressed())
	return newPubKey
}

// Copy returns a deep copy of the account with the given modifiers applied.
func (a *Account) Copy(modifiers ...Modifier) *Account {
	accountCopy := &Account{
		Value:  a.Value,
		Expiry: a.Expiry,
		TraderKey: &keychain.KeyDescriptor{
			KeyLocator: a.TraderKey.KeyLocator,
			PubKey:     CopyPubKey(a.TraderKey.PubKey),
		},
		AuctioneerKey: CopyPubKey(a.AuctioneerKey),
		BatchKey:      CopyPubKey(a.BatchKey),
		Secret:        a.Secret,
		State:         a.State,
		HeightHint:    a.HeightHint,
		OutPoint:      a.OutPoint,
		Version:       a.Version,
	}
	if a.State != StateInitiated {
		accountCopy.LatestTx = a.LatestTx.Copy()
	}

	for _, modifier := range modifiers {
		modifier(accountCopy)
	}

	return accountCopy
}

// Modifier abstracts the modification of an account through a function.
type Modifier func(*Account)

// StateModifier is a functional option that modifies the state of an account.
func StateModifier(state State) Modifier {
	return func(account *Account) {
		account.State = state
	}
}

// ValueModifier is a functional option that modifies the value of an account.
func ValueModifier(value btcutil.Amount) Modifier {
	return func(account *Account) {
		account.Value = value
	}
}

// ExpiryModifier is a functional option that modifies the expiry of an account.
func ExpiryModifier(expiry uint32) Modifier {
	return func(account *Account) {
		account.Expiry = expiry
	}
}

// IncrementBatchKey is a functional option that increments the batch key of an
// account by adding the curve's base point.
func IncrementBatchKey() Modifier {
	return func(account *Account) {
		account.BatchKey = poolscript.IncrementKey(account.BatchKey)
	}
}

// OutPointModifier is a functional option that modifies the outpoint of an
// account.
func OutPointModifier(op wire.OutPoint) Modifier {
	return func(account *Account) {
		account.OutPoint = op
	}
}

// HeightHintModifier is a functional option that modifies the height hint of an
// account.
func HeightHintModifier(heightHint uint32) Modifier {
	return func(account *Account) {
		account.HeightHint = heightHint
	}
}

// LatestTxModifier is a functional option that modifies the latest transaction
// of an account.
func LatestTxModifier(tx *wire.MsgTx) Modifier {
	return func(account *Account) {
		account.LatestTx = tx
	}
}

// VersionModifier is a functional option that modifies the version of an
// account.
func VersionModifier(version Version) Modifier {
	return func(account *Account) {
		account.Version = version
	}
}

// Store is responsible for storing and retrieving account information reliably.
type Store interface {
	// AddAccount adds a record for the account to the database.
	AddAccount(*Account) error

	// UpdateAccount updates an account in the database according to the
	// given modifiers.
	UpdateAccount(*Account, ...Modifier) error

	// Account retrieves the account associated with the given trader key
	// from the database.
	Account(*btcec.PublicKey) (*Account, error)

	// Accounts retrieves all existing accounts.
	Accounts() ([]*Account, error)

	// PendingBatch determines whether we currently have a pending batch.
	// If a batch doesn't exist, ErrNoPendingBatch is returned.
	PendingBatch() error

	// MarkBatchComplete marks the batch with the given ID as complete,
	// indicating that the staged account updates can be applied to disk.
	MarkBatchComplete() error

	// LockID retrieves the global lock ID we'll use to lock any outputs
	// when performing coin selection.
	LockID() (wtxmgr.LockID, error)
}

// Auctioneer provides us with the different ways we are able to communicate
// with our auctioneer during the process of opening/closing/modifying accounts.
type Auctioneer interface {
	// ReserveAccount reserves an account of the specified value with the
	// auctioneer. The auctioneer checks the account value against current
	// min/max values configured. If the value is valid, it returns the
	// public key we should use for them in our 2-of-2 multi-sig
	// construction. To address an edge case in the account recovery where
	// the trader crashes before confirming the account with the auctioneer,
	// we also send the trader key and expiry along with the reservation.
	ReserveAccount(context.Context, btcutil.Amount, uint32,
		*btcec.PublicKey, Version) (*Reservation, error)

	// InitAccount initializes an account with the auctioneer such that it
	// can be used once fully confirmed.
	InitAccount(context.Context, *Account) error

	// ModifyAccount sends an intent to the auctioneer that we'd like to
	// modify the account with the associated trader key. The auctioneer's
	// signature is returned, allowing us to broadcast a transaction
	// spending from the account allowing our modifications to take place.
	// If the account spend is a MuSig2 spend, then the trader's nonces must
	// be sent and the server's nonces are returned upon success. The inputs
	// and outputs provided should exclude the account input being spent and
	// the account output potentially being recreated, since the auctioneer
	// can construct those themselves. If no modifiers are present, then the
	// auctioneer will interpret the request as an account closure. The
	// previous outputs must always contain the UTXO information for _every_
	// input of the transaction, so inputs+account_input.
	ModifyAccount(ctx context.Context, acct *Account, inputs []*wire.TxIn,
		outputs []*wire.TxOut, modifiers []Modifier,
		traderNonces []byte, previousOutputs []*wire.TxOut) ([]byte,
		[]byte, error)

	// StartAccountSubscription opens a stream to the server and subscribes
	// to all updates that concern the given account, including all orders
	// that spend from that account. Only a single stream is ever open to
	// the server, so a second call to this method will send a second
	// subscription over the same stream, multiplexing all messages into the
	// same connection. A stream can be long-lived, so this can be called
	// for every account as soon as it's confirmed open. This method will
	// return as soon as the authentication was successful. Messages sent
	// from the server can then be received on the FromServerChan channel.
	StartAccountSubscription(context.Context, *keychain.KeyDescriptor) error

	// Terms returns the current dynamic auctioneer terms like max account
	// size, max order duration in blocks and the auction fee schedule.
	Terms(ctx context.Context) (*terms.AuctioneerTerms, error)
}

// TxSource is a source that provides us with transactions previously broadcast
// by us.
type TxSource interface {
	// ListTransactions returns all known transactions of the backing lnd
	// node. It takes a start and end block height which can be used to
	// limit the block range that we query over. These values can be left
	// as zero to include all blocks. To include unconfirmed transactions
	// in the query, endHeight must be set to -1.
	ListTransactions(ctx context.Context, startHeight, endHeight int32,
		opts ...lndclient.ListTransactionsOption) (
		[]lndclient.Transaction, error)
}

// TxFeeEstimator is a type that provides us with a realistic fee estimation to
// send coins in a transaction.
type TxFeeEstimator interface {
	// EstimateFeeToP2WSH estimates the total chain fees in satoshis to send
	// the given amount to a single P2WSH output with the given target
	// confirmation.
	EstimateFeeToP2WSH(ctx context.Context, amt btcutil.Amount,
		confTarget int32) (btcutil.Amount, error)
}

// FeeExpr represents the different ways a transaction fee can be expressed in
// terms of a transaction's resulting outputs.
type FeeExpr interface {
	// CloseOutputs is the list of outputs that should be used for the
	// closing transaction of an account based on the concrete fee
	// expression implementation.
	CloseOutputs(btcutil.Amount, witnessType) ([]*wire.TxOut, error)
}

// OutputWithFee signals that a single transaction output along with a fee rate
// is used to determine the transaction fee.
type OutputWithFee struct {
	// PkScript is the destination output script. Note that this may be nil,
	// in which case a wallet-derived P2WKH script should be used.
	PkScript []byte

	// FeeRate is the accompanying fee rate to use to determine the
	// transaction fee.
	FeeRate chainfee.SatPerKWeight
}

func (o *OutputWithFee) CloseOutputs(accountValue btcutil.Amount,
	witnessType witnessType) ([]*wire.TxOut, error) {

	// Calculate the transaction's weight to determine its fee according to
	// the provided fee rate. The transaction will contain one P2WSH input
	// (the account input) and one output.
	var weightEstimator input.TxWeightEstimator

	// Determine the appropriate witness size based on the input and output
	// type.
	witnessSize, err := witnessType.witnessSize()
	if err != nil {
		return nil, err
	}
	weightEstimator.AddWitnessInput(witnessSize)

	pkScript, err := txscript.ParsePkScript(o.PkScript)
	if err != nil {
		return nil, err
	}

	// We'll also note the dust limit for each output script type along the
	// way to determine if the output can even be created.
	var dustLimit btcutil.Amount
	switch pkScript.Class() {
	case txscript.WitnessV0PubKeyHashTy:
		weightEstimator.AddP2WKHOutput()
		dustLimit = lnwallet.DustLimitForSize(
			input.P2WPKHSize,
		)

	case txscript.ScriptHashTy:
		weightEstimator.AddP2SHOutput()
		dustLimit = lnwallet.DustLimitForSize(
			input.P2SHSize,
		)

	case txscript.WitnessV0ScriptHashTy:
		weightEstimator.AddP2WSHOutput()
		dustLimit = lnwallet.DustLimitForSize(
			input.P2WSHSize,
		)

	case txscript.WitnessV1TaprootTy:
		weightEstimator.AddP2TROutput()
		dustLimit = lnwallet.DustLimitForSize(
			input.P2TRSize,
		)
	}

	fee := o.FeeRate.FeeForWeight(int64(weightEstimator.Weight()))
	outputValue := accountValue - fee
	if outputValue < dustLimit {
		return nil, fmt.Errorf("closing to output %x with %v results "+
			"in dust", pkScript, o.FeeRate)
	}

	return []*wire.TxOut{{
		Value:    int64(outputValue),
		PkScript: pkScript.Script(),
	}}, nil
}

// OutputsWithImplicitFee signals that the transaction fee is implicitly defined
// by the output values provided, i.e., the fee is determined by subtracting
// the total output value from the total input value.
type OutputsWithImplicitFee []*wire.TxOut

// Outputs returns the set of outputs.
func (o OutputsWithImplicitFee) Outputs() []*wire.TxOut {
	return o
}

// CloseOutputs is the list of outputs that should be used for the closing
// transaction of an account using an implicit fee expression.
func (o OutputsWithImplicitFee) CloseOutputs(_ btcutil.Amount,
	_ witnessType) ([]*wire.TxOut, error) {

	return o, nil
}

// Manager is the interface a manager implements to deal with the accounts.
type Manager interface {
	// Start resumes all account on-chain operation after a restart.
	Start() error

	// Stop safely stops any ongoing operations within the Manager.
	Stop()

	// QuoteAccount returns the expected fee rate and total miner fee to
	// send to an account funding output with the given confTarget.
	QuoteAccount(ctx context.Context, value btcutil.Amount,
		confTarget uint32) (chainfee.SatPerKWeight, btcutil.Amount,
		error)

	// InitAccount handles a request to create a new account with the
	// provided parameters.
	InitAccount(ctx context.Context, value btcutil.Amount, version Version,
		feeRate chainfee.SatPerKWeight, expiry,
		bestHeight uint32) (*Account, error)

	// WatchMatchedAccounts resumes accounts that were just matched in a
	// batch and are expecting the batch transaction to confirm as their
	// next account output. This will cancel all previous spend and conf
	// watchers of all accounts involved in the batch.
	WatchMatchedAccounts(ctx context.Context,
		matchedAccounts []*btcec.PublicKey) error

	// HandleAccountConf takes the necessary steps after detecting the
	// confirmation of an account on-chain.
	HandleAccountConf(traderKey *btcec.PublicKey,
		confDetails *chainntnfs.TxConfirmation) error

	// HandleAccountSpend handles the different spend paths of an account.
	// If an account is spent by the expiration path, it'll always be marked
	// as closed thereafter. If it is spent by the cooperative path with the
	// auctioneer, then the account will only remain open if the spending
	// transaction recreates the account with the expected next account
	// script. Otherwise, it is also marked as closed. In case of multiple
	// consecutive batches with the same account, we only track the spend of
	// the latest batch, after it confirmed. So the account/ output in the
	// spend transaction should always match our database state if it was a
	// cooperative spend.
	HandleAccountSpend(traderKey *btcec.PublicKey,
		spendDetails *chainntnfs.SpendDetail) error

	// HandleAccountExpiry marks an account as expired within the database.
	HandleAccountExpiry(traderKey *btcec.PublicKey, height uint32) error

	// DepositAccount attempts to deposit funds into the account associated
	// with the given trader key such that the new account value is met
	// using inputs sourced from the backing lnd node's wallet. If needed, a
	// change output that does back to lnd may be added to the deposit
	// transaction.
	DepositAccount(ctx context.Context, traderKey *btcec.PublicKey,
		depositAmount btcutil.Amount, feeRate chainfee.SatPerKWeight,
		bestHeight, expiryHeight uint32, newVersion Version) (*Account,
		*wire.MsgTx, error)

	// WithdrawAccount attempts to withdraw funds from the account
	// associated with the given trader key into the provided outputs.
	WithdrawAccount(ctx context.Context, traderKey *btcec.PublicKey,
		outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight,
		bestHeight, expiryHeight uint32, newVersion Version) (*Account,
		*wire.MsgTx, error)

	// RenewAccount updates the expiration of an open/expired account. This
	// will always require a signature from the auctioneer, even after the
	// account has expired, to ensure the auctioneer is aware the account is
	// being renewed.
	RenewAccount(ctx context.Context, traderKey *btcec.PublicKey,
		newExpiry uint32, feeRate chainfee.SatPerKWeight,
		bestHeight uint32, newVersion Version) (*Account, *wire.MsgTx,
		error)

	// BumpAccountFee attempts to bump the fee of an account's most recent
	// transaction. This is done by locating an eligible output for lnd to
	// CPFP, otherwise the fee bump will not succeed. Further invocations of
	// this call for the same account will result in the child being
	// replaced by the higher fee transaction (RBF).
	BumpAccountFee(ctx context.Context, traderKey *btcec.PublicKey,
		newFeeRate chainfee.SatPerKWeight) error

	// CloseAccount attempts to close the account associated with the given
	// trader key. Closing the account requires a signature of the
	// auctioneer if the account has not yet expired. The account funds are
	// swept according to the provided fee expression.
	CloseAccount(ctx context.Context, traderKey *btcec.PublicKey,
		feeExpr FeeExpr, bestHeight uint32) (*wire.MsgTx, error)

	// RecoverAccount re-introduces a recovered account into the database
	// and starts all watchers necessary depending on the account's state.
	RecoverAccount(ctx context.Context, account *Account) error
}
