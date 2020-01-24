package account

import (
	"context"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightningnetwork/lnd/keychain"
)

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

	// TraderKey is the trader's key in the 2-of-2 multi-sig construction of
	// a CLM account. It is also the identifying component of an account.
	TraderKey *keychain.KeyDescriptor

	// AuctioneerKey is the auctioneer's key in the 2-of-2 multi-sig
	// construction of a CLM account.
	AuctioneerKey *btcec.PublicKey

	// State describes the state of the account.
	State State

	// HeightHint is the earliest height in the chain at which we can find
	// the account output in a block.
	HeightHint uint32

	// OutPoint is the outpoint of the output used to fund the account. This
	// only exists once the account has reached StatePendingOpen.
	OutPoint wire.OutPoint
}

// Output returns the on-chain output associated with the account.
func (a *Account) Output() (*wire.TxOut, error) {
	script, err := clmscript.AccountScript(
		a.Expiry, a.TraderKey.PubKey, a.AuctioneerKey,
	)
	if err != nil {
		return nil, err
	}
	return &wire.TxOut{
		Value:    int64(a.Value),
		PkScript: script,
	}, nil
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
	// construction. The trader key is provided and used as a tweak to the
	// auctioneer's long-term key to achieve deterministic account creation.
	ReserveAccount(context.Context, *btcec.PublicKey) (*btcec.PublicKey, error)

	// InitAccount initializes an account with the auctioneer such that it
	// can be used once fully confirmed.
	InitAccount(context.Context, *Account) error
}

// TxSource is a source that provides us with transactions previously broadcast
// by us.
type TxSource interface {
	// ListTransactions returns a list of transactions previously broadcast
	// by us.
	ListTransactions(ctx context.Context) ([]*wire.MsgTx, error)
}
