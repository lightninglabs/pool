package account

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
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
		a.Expiry, a.TraderKey.PubKey, a.AuctioneerKey, a.BatchKey,
		a.Secret,
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
		a.Expiry, a.TraderKey.PubKey, a.AuctioneerKey, nextBatchKey,
		a.Secret,
	)
}

// Copy returns a deep copy of the account with the given modifiers applied.
func (a *Account) Copy(modifiers ...Modifier) *Account {
	accountCopy := &Account{
		Value:  a.Value,
		Expiry: a.Expiry,
		TraderKey: &keychain.KeyDescriptor{
			KeyLocator: a.TraderKey.KeyLocator,
			PubKey: &btcec.PublicKey{
				X:     big.NewInt(0).Set(a.TraderKey.PubKey.X),
				Y:     big.NewInt(0).Set(a.TraderKey.PubKey.Y),
				Curve: a.TraderKey.PubKey.Curve,
			},
		},
		AuctioneerKey: &btcec.PublicKey{
			X:     big.NewInt(0).Set(a.AuctioneerKey.X),
			Y:     big.NewInt(0).Set(a.AuctioneerKey.Y),
			Curve: a.AuctioneerKey.Curve,
		},
		BatchKey: &btcec.PublicKey{
			X:     big.NewInt(0).Set(a.BatchKey.X),
			Y:     big.NewInt(0).Set(a.BatchKey.Y),
			Curve: a.BatchKey.Curve,
		},
		Secret:     a.Secret,
		State:      a.State,
		HeightHint: a.HeightHint,
		OutPoint:   a.OutPoint,
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
		*btcec.PublicKey) (*Reservation, error)

	// InitAccount initializes an account with the auctioneer such that it
	// can be used once fully confirmed.
	InitAccount(context.Context, *Account) error

	// ModifyAccount sends an intent to the auctioneer that we'd like to
	// modify the account with the associated trader key. The auctioneer's
	// signature is returned, allowing us to broadcast a transaction
	// spending from the account allowing our modifications to take place.
	// The inputs and outputs provided should exclude the account input
	// being spent and the account output potentially being recreated, since
	// the auctioneer can construct those themselves.
	ModifyAccount(context.Context, *Account, []*wire.TxIn,
		[]*wire.TxOut, []Modifier) ([]byte, error)

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
	ListTransactions(ctx context.Context, startHeight,
		endHeight int32) ([]lndclient.Transaction, error)
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
	switch witnessType {
	case expiryWitness:
		weightEstimator.AddWitnessInput(poolscript.ExpiryWitnessSize)
	case multiSigWitness:
		weightEstimator.AddWitnessInput(poolscript.MultiSigWitnessSize)
	default:
		return nil, fmt.Errorf("unhandled witness type %v", witnessType)
	}

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
	}

	fee := o.FeeRate.FeeForWeight(int64(weightEstimator.Weight()))
	outputValue := accountValue - fee
	if outputValue < dustLimit {
		return nil, fmt.Errorf("closing to output %x with %v results "+
			"in dust", pkScript, o.FeeRate)
	}

	return []*wire.TxOut{
		{
			Value:    int64(outputValue),
			PkScript: pkScript.Script(),
		},
	}, nil
}

// OutputsWithImplicitFee signals that the transaction fee is implicitly defined
// by the output values provided, i.e., the fee is determined by subtracting
// the total output value from the total input value.
type OutputsWithImplicitFee []*wire.TxOut

// Outputs returns the set of outputs.
func (o OutputsWithImplicitFee) Outputs() []*wire.TxOut {
	return o
}

// Outputs is the list of outputs that should be used for the closing
// transaction of an account using an implicit fee expression.
func (o OutputsWithImplicitFee) CloseOutputs(accountValue btcutil.Amount,
	witnessType witnessType) ([]*wire.TxOut, error) {

	return o, nil
}
