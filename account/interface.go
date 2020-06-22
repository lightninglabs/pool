package account

import (
	"context"
	"errors"
	"math/big"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/llm/clmscript"
	"github.com/lightningnetwork/lnd/keychain"
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
	// on-chain either as part of a matched order or a trader modification
	// and we are currently waiting for its confirmation.
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
	default:
		return "unknown"
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

	// CloseTx is the closing transaction of an account. This will only be
	// populated if the account is in any of the following states:
	//
	//	- StatePendingClosed
	//	- StateClosed
	CloseTx *wire.MsgTx
}

// Output returns the current on-chain output associated with the account.
func (a *Account) Output() (*wire.TxOut, error) {
	script, err := clmscript.AccountScript(
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
	nextBatchKey := clmscript.IncrementKey(a.BatchKey)
	return clmscript.AccountScript(
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

	if a.CloseTx != nil {
		accountCopy.CloseTx = a.CloseTx.Copy()
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
		account.BatchKey = clmscript.IncrementKey(account.BatchKey)
	}
}

// OutPointModifier is a functional option that modifies the outpoint of an
// account.
func OutPointModifier(op wire.OutPoint) Modifier {
	return func(account *Account) {
		account.OutPoint = op
	}
}

// CloseTxModifier is a functional option that modifies the closing transaction
// of an account.
func CloseTxModifier(tx *wire.MsgTx) Modifier {
	return func(account *Account) {
		account.CloseTx = tx
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
	// construction.
	ReserveAccount(context.Context, btcutil.Amount) (*Reservation, error)

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

	// SubscribeAccountUpdates opens a stream to the server and subscribes
	// to all updates that concern the given account, including all orders
	// that spend from that account. Only a single stream is ever open to
	// the server, so a second call to this method will send a second
	// subscription over the same stream, multiplexing all messages into the
	// same connection. A stream can be long-lived, so this can be called
	// for every account as soon as it's confirmed open.
	SubscribeAccountUpdates(context.Context, *keychain.KeyDescriptor) error
}

// TxSource is a source that provides us with transactions previously broadcast
// by us.
type TxSource interface {
	// ListTransactions returns a list of transactions previously broadcast
	// by us.
	ListTransactions(ctx context.Context) ([]*wire.MsgTx, error)
}
