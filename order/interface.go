package order

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Nonce is a 32 byte pseudo randomly generated unique order ID.
type Nonce [32]byte

// String returns the hex encoded representation of the nonce.
func (n Nonce) String() string {
	return hex.EncodeToString(n[:])
}

// Version is the version of an order. We don't use iota for the constants due
// to the order type being persisted to disk.
type Version uint32

const (
	// VersionDefault is the default initial version of orders.
	VersionDefault Version = 0
)

// Type is the type of an order. We don't use iota for the constants due to the
// order type being persisted to disk.
type Type uint8

const (
	// TypeAsk is the constant to represent the "ask" order type.
	TypeAsk Type = 0

	// TypeBid is the constant to represent the "bid" order type.
	TypeBid Type = 1
)

// State describes the different possible states of an order. We don't use iota
// for the constants due to the order state being persisted to disk.
type State uint8

const (
	// StateSubmitted is the state an order has after it's been submitted
	// successfully.
	StateSubmitted State = 0

	// StateCleared is the state an order has after it's been accepted as
	// part of a batch but has not been executed yet.
	StateCleared State = 1

	// StatePartiallyFilled is the state an order has after some but not all
	// parts of it have been filled.
	StatePartiallyFilled State = 2

	// StateExecuted is the state an order has after it has been matched
	// with another order in the order book and fully processed.
	StateExecuted State = 3

	// StateCanceled is the state an order has after a user cancels the
	// order manually.
	StateCanceled State = 4

	// StateExpired is the state an order has after it's maximum lifetime
	// has passed.
	StateExpired State = 5

	// StateFailed is the state an order has if any irrecoverable error
	// happens in its lifetime.
	StateFailed State = 6
)

// String returns a human readable string representation of the order state.
func (s State) String() string {
	switch s {
	case StateSubmitted:
		return "submitted"

	case StateCleared:
		return "cleared"

	case StatePartiallyFilled:
		return "partially_filled"

	case StateExecuted:
		return "executed"

	case StateCanceled:
		return "canceled"

	case StateExpired:
		return "expired"

	case StateFailed:
		return "failed"

	default:
		return fmt.Sprintf("unknown<%d>", s)
	}
}

// Archived returns true if the order is in a state that is considered to be
// fully executed and no more modifications will be done to it.
func (s State) Archived() bool {
	switch s {
	case StateExecuted, StateCanceled, StateFailed:
		return true

	default:
		return false
	}
}

var (
	// ErrInsufficientBalance is the error that is returned if an account
	// has insufficient balance to perform a requested action.
	ErrInsufficientBalance = errors.New("insufficient account balance")

	// ZeroNonce is used to find out if a user-provided nonce is empty.
	ZeroNonce Nonce
)

// Order is an interface to allow generic handling of both ask and bid orders
// by both store and manager.
type Order interface {
	// Nonce is the unique identifier of each order and MUST be created by
	// hashing a new random preimage for each new order. The nonce is what
	// is signed in the order signature.
	Nonce() Nonce

	// Details returns the Kit of the order.
	Details() *Kit

	// Type returns the order type.
	Type() Type

	// Digest returns a deterministic SHA256 hash over the contents of an
	// order. Deterministic in this context means that if two orders have
	// the same content, their digest have to be identical as well.
	Digest() ([sha256.Size]byte, error)
}

// Kit stores all the common fields that are used to express the decision to
// participate in the auction process. A kit is always wrapped by either a bid
// or an ask.
type Kit struct {
	// nonce is the hash of the preimage and acts as the unique identifier
	// of an order.
	nonce Nonce

	// Preimage is the randomly generated preimage to the nonce hash. It is
	// only known to the trader client.
	Preimage lntypes.Preimage

	// Version is the feature version of this order. Can be used to
	// distinguish between certain feature sets or to signal feature flags.
	Version Version

	// State is the current state the order is in as it was last seen by the
	// client. The real state is tracked on the auction server, so this can
	// be out of sync if there was no connection for a while.
	State State

	// FixedRate is the fixed order rate expressed in parts per million.
	FixedRate uint32

	// Amt is the order amount in satoshis.
	Amt btcutil.Amount

	// Units the total amount of units that the target amount maps to.
	Units SupplyUnit

	// UnitsUnfulfilled is the number of units that have not been filled yet
	// and are still available for matching against other orders.
	UnitsUnfulfilled SupplyUnit

	// MultiSigKeyLocator is the key locator used to obtain the multi sig
	// key. This will be needed for operations that require a signature
	// under said key and will therefore only be known to the trader client.
	// This key will only be derived from the connected lnd after the order
	// has been formally validated.
	MultiSigKeyLocator keychain.KeyLocator

	// FundingFeeRate is the preferred fee rate to be used for the channel
	// funding transaction in sat/kW.
	FundingFeeRate chainfee.SatPerKWeight

	// AcctKey is key of the account the order belongs to.
	AcctKey *btcec.PublicKey
}

// Nonce is the unique identifier of each order and MUST be created by hashing a
// new random preimage for each new order. The nonce is what is signed in the
// order signature.
//
// NOTE: This method is part of the Order interface.
func (k *Kit) Nonce() Nonce {
	return k.nonce
}

// Details returns the Kit of the order.
//
// NOTE: This method is part of the Order interface.
func (k *Kit) Details() *Kit {
	return k
}

// NewKitWithPreimage creates a new kit by hashing the preimage to generate the
// unique nonce.
func NewKitWithPreimage(preimage lntypes.Preimage) *Kit {
	var nonce Nonce
	hash := preimage.Hash()
	copy(nonce[:], hash[:])
	return &Kit{
		nonce:    nonce,
		Preimage: preimage,
		Version:  VersionDefault,
	}
}

// NewKit creates a new kit from a nonce in case the preimage is not known.
func NewKit(nonce Nonce) *Kit {
	return &Kit{
		nonce:   nonce,
		Version: VersionDefault,
	}
}

// Ask is the specific order type representing the willingness of an auction
// participant to lend out their funds by opening channels to other auction
// participants.
type Ask struct {
	// Kit contains all the common order parameters.
	Kit

	// MaxDuration is the maximum number of blocks the liquidity provider is
	// willing to provide the channel funds for.
	MaxDuration uint32
}

// Type returns the order type.
//
// NOTE: This method is part of the Order interface.
func (a *Ask) Type() Type {
	return TypeAsk
}

// Digest returns a deterministic SHA256 hash over the contents of an ask order.
// Deterministic in this context means that if two orders have the same content,
// their digest have to be identical as well.
//
// NOTE: This method is part of the Order interface.
func (a *Ask) Digest() ([sha256.Size]byte, error) {
	var (
		msg    bytes.Buffer
		result [sha256.Size]byte
	)
	switch a.Kit.Version {
	case VersionDefault:
		err := lnwire.WriteElements(
			&msg, a.nonce[:], uint32(a.Version), a.FixedRate,
			a.Amt, a.MaxDuration, uint64(a.FundingFeeRate),
		)
		if err != nil {
			return result, err
		}

	default:
		return result, fmt.Errorf("unknown version %d", a.Kit.Version)
	}
	return sha256.Sum256(msg.Bytes()), nil
}

// Bid is the specific order type representing the willingness of an auction
// participant to pay for inbound liquidity provided by other auction
// participants.
type Bid struct {
	// Kit contains all the common order parameters.
	Kit

	// MinDuration is the minimal duration the channel resulting from this
	// bid should be kept open, expressed in blocks.
	MinDuration uint32
}

// Type returns the order type.
//
// NOTE: This method is part of the Order interface.
func (b *Bid) Type() Type {
	return TypeBid
}

// Digest returns a deterministic SHA256 hash over the contents of a bid order.
// Deterministic in this context means that if two orders have the same content,
// their digest have to be identical as well.
//
// NOTE: This method is part of the Order interface.
func (b *Bid) Digest() ([sha256.Size]byte, error) {
	var (
		msg    bytes.Buffer
		result [sha256.Size]byte
	)
	switch b.Kit.Version {
	case VersionDefault:
		err := lnwire.WriteElements(
			&msg, b.nonce[:], uint32(b.Version), b.FixedRate,
			b.Amt, b.MinDuration, uint64(b.FundingFeeRate),
		)
		if err != nil {
			return result, err
		}

	default:
		return result, fmt.Errorf("unknown version %d", b.Kit.Version)
	}
	return sha256.Sum256(msg.Bytes()), nil
}

// This is a compile time check to make certain that both Ask and Bid implement
// the Order interface.
var _ Order = (*Ask)(nil)
var _ Order = (*Bid)(nil)

// Modifier abstracts the modification of an account through a function.
type Modifier func(*Kit)

// StateModifier is a functional option that modifies the state of an order.
func StateModifier(state State) Modifier {
	return func(order *Kit) {
		order.State = state
	}
}

// UnitsFulfilledModifier is a functional option that modifies the number of
// unfulfilled units of an order.
func UnitsFulfilledModifier(newUnfulfilledUnits SupplyUnit) Modifier {
	return func(order *Kit) {
		order.UnitsUnfulfilled = newUnfulfilledUnits
	}
}

// Store is the interface a store has to implement to support persisting orders.
type Store interface {
	// SubmitOrder stores an order by using the orders's nonce as an
	// identifier. If an order with the given nonce already exists in the
	// store, ErrOrderExists is returned.
	SubmitOrder(Order) error

	// UpdateOrder updates an order in the database according to the given
	// modifiers.
	UpdateOrder(Nonce, ...Modifier) error

	// UpdateOrders atomically updates a list of orders in the database
	// according to the given modifiers.
	UpdateOrders([]Nonce, [][]Modifier) error

	// GetOrder returns an order by looking up the nonce. If no order with
	// that nonce exists in the store, ErrNoOrder is returned.
	GetOrder(Nonce) (Order, error)

	// GetOrders returns all orders that are currently known to the store.
	GetOrders() ([]Order, error)

	// DelOrder removes the order with the given nonce from the local store.
	DelOrder(Nonce) error

	// PersistBatchResult atomically updates all modified orders/accounts.
	// If any single operation fails, the whole set of changes is rolled
	// back.
	PersistBatchResult(orders []Nonce, orderModifiers [][]Modifier,
		accounts []*account.Account,
		accountModifiers [][]account.Modifier) error
}

// UserError is an error type that is returned if an action fails because of
// an invalid action or information provided by the user.
type UserError struct {
	FailMsg string
	Details *clmrpc.InvalidOrder
}

// Error returns the string representation of the underlying failure message.
func (e *UserError) Error() string {
	return e.FailMsg
}

// A compile-time constraint to ensure UserError implements the error interface.
var _ error = (*UserError)(nil)

// ServerOrderParams is the list of values that we have to send to the server
// when submitting an order that doesn't need to be persisted in the local DB.
type ServerOrderParams struct {
	// MultiSigKey is a key of the node creating the order that will be used
	// to craft the channel funding TX's 2-of-2 multi signature output.
	MultiSigKey [33]byte

	// NodePubkey is the identity public key of the node submitting the
	// order.
	NodePubkey [33]byte

	// Addrs is a list of network addresses through which the node
	// submitting the order can be reached.
	Addrs []net.Addr

	// RawSig is the raw signature over the order digest signed with the
	// trader's account key.
	RawSig []byte
}
