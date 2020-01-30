package account

import (
	"context"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightningnetwork/lnd/keychain"
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

	// StateOpen denotes that the account's funding transaction has been
	// included in the chain with sufficient depth.
	StateOpen State = 2

	// StateExpired denotes that the chain has reached an account's
	// expiration height. An account in this state can still be used if
	// renewed.
	StateExpired = 4

	// StatePendingClosed denotes that an account was fully spent by a
	// transaction broadcast by the trader and is pending its confirmation.
	StatePendingClosed = 5

	// StateClosed denotes that an account was closed by a transaction
	// broadcast by the trader that fully spent the account. An account in
	// this state can no longer be used.
	StateClosed = 6
)

// String returns a human-readable description of an account's state.
func (s State) String() string {
	switch s {
	case StateInitiated:
		return "StateInitiated"
	case StatePendingOpen:
		return "StatePendingOpen"
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

// Modifier abstracts the modification of an account through a function.
type Modifier func(*Account)

// StateModifier is a functional option that modifies the state of an account.
func StateModifier(state State) Modifier {
	return func(account *Account) {
		account.State = state
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
}

// Auctioneer provides us with the different ways we are able to communicate
// with our auctioneer during the process of opening/closing/modifying accounts.
type Auctioneer interface {
	// ReserveAccount reserves an account with the auctioneer. It returns a
	// the public key we should use for them in our 2-of-2 multi-sig
	// construction.
	ReserveAccount(context.Context) (*Reservation, error)

	// InitAccount initializes an account with the auctioneer such that it
	// can be used once fully confirmed.
	InitAccount(context.Context, *Account) error

	// CloseAccount sends an intent to the auctioneer that we'd like to
	// close the account with the associated trader key by withdrawing the
	// funds to the given outputs. The auctioneer's signature is returned,
	// allowing us to broadcast a transaction sweeping the account.
	CloseAccount(context.Context, *btcec.PublicKey,
		[]*wire.TxOut) ([]byte, error)
}

// TxSource is a source that provides us with transactions previously broadcast
// by us.
type TxSource interface {
	// ListTransactions returns a list of transactions previously broadcast
	// by us.
	ListTransactions(ctx context.Context) ([]*wire.MsgTx, error)
}
