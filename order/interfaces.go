package order

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/codec"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// Nonce is a 32 byte pseudo randomly generated unique order ID.
type Nonce [32]byte

// The size of a SHA256 checksum in bytes.
//
// Note: this matches the sha256.Size definition. However, mockgen
// complains about not being able to find the sha256 package. When
// that bug is fixed, we can change back to [sha256.Size]byte.
const hashSize = 32

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

	// VersionNodeTierMinMatch is the order version that added recognition
	// of the new node tier and min matchable order size fields.
	VersionNodeTierMinMatch Version = 1

	// VersionLeaseDurationBuckets is the order version that added use of
	// multiple lease durations. Only orders with this version are allowed
	// to use lease durations outside of the default/legacy 2016 block
	// duration.
	VersionLeaseDurationBuckets Version = 2

	// VersionSelfChanBalance is the order version that added use of the
	// self channel balance field. Only orders with this version are allowed
	// to use the self channel balance field.
	VersionSelfChanBalance Version = 3

	// VersionSidecarChannel is the order version that added sidecar
	// channels for bid orders. Only orders with this version are allowed
	// to set the sidecar ticket field on bid orders. Since sidecar orders
	// also add the feature of push amounts on the leased channels, this
	// affects makers as well. Makers that don't want to support leasing out
	// channels with a push amount (because it might screw up their
	// accounting or whatever) can opt out by explicitly submitting their
	// ask orders with a version previous to this one.
	VersionSidecarChannel Version = 4

	// VersionChannelType is the order version that added use of the channel
	// type field. Only orders with this version are allowed to use the
	// channel type field.
	VersionChannelType Version = 5
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

// String returns a human read-able string describing the passed order type.
func (t Type) String() string {
	switch t {
	case TypeAsk:
		return "Ask"

	case TypeBid:
		return "Bid"

	default:
		return "<unknown>"
	}
}

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
	case StateExecuted, StateCanceled, StateExpired, StateFailed:
		return true

	default:
		return false
	}
}

// MatchState describes the distinct phases an order goes through as seen by the
// trader daemon. These states are not persisted on the orders themselves but
// rather as events with timestamps so a user can track what's happening to
// their orders.
type MatchState uint8

const (
	// MatchStatePrepare is the state an order is in after the
	// OrderMatchPrepare message was received initially.
	MatchStatePrepare MatchState = 0

	// MatchStatePrepare is the state an order is in after the
	// OrderMatchPrepare message was processed successfully and the batch
	// was accepted.
	MatchStateAccepted MatchState = 1

	// MatchStateRejected is the state an order is in after the trader
	// rejected it, either as an answer to a OrderMatchSignBegin or
	// OrderMatchFinalize message from the auctioneer.
	MatchStateRejected MatchState = 2

	// MatchStateSigned is the state an order is in after the
	// OrderMatchSignBegin message was processed successfully.
	MatchStateSigned MatchState = 3

	// MatchStateFinalized is the state an order is in after the
	// OrderMatchFinalize message was processed successfully.
	MatchStateFinalized MatchState = 4
)

// String returns a human readable string representation of the match state.
func (s MatchState) String() string {
	switch s {
	case MatchStatePrepare:
		return "prepare"

	case MatchStateAccepted:
		return "accepted"

	case MatchStateSigned:
		return "signed"

	case MatchStateFinalized:
		return "finalized"

	case MatchStateRejected:
		return "rejected"

	default:
		return fmt.Sprintf("unknown<%d>", s)
	}
}

// ChannelType is a numerical type that represents all possible channel types
// that are supported to be opened through the auction process.
type ChannelType uint8

// NOTE: We avoid the use of iota as this type is stored on disk.
const (
	// ChannelTypePeerDependent denotes that the resulting channel type from
	// an order match will depend on the shared features between its
	// participants.
	ChannelTypePeerDependent ChannelType = 0

	// ChannelTypeScriptEnforced represents a new channel type that builds
	// upon the anchors commitment format to enforce the maturity of a
	// leased channel in the commitment and HTLC outputs that pay directly
	// to the channel initiator.
	ChannelTypeScriptEnforced ChannelType = 1
)

// ChannelAnnouncementConstraints is a numerical type used to denote if the
// channels created from a match can be announced or not.
type ChannelAnnouncementConstraints uint8

const (
	// AnnouncementNoPreference denotes that the resulting channels can be
	// announced or not.
	AnnouncementNoPreference ChannelAnnouncementConstraints = 0

	// OnlyAnnounced denotes that the resulting channels must be announced
	// to the network.
	OnlyAnnounced ChannelAnnouncementConstraints = 1

	// OnlyUnannounced denotes that the resulting channels must not be
	// announced to the network.
	OnlyUnannounced ChannelAnnouncementConstraints = 2
)

// ChannelConfirmationConstraints is a numerical type used to denote if the
// channels created from a match is zero conf or not.
type ChannelConfirmationConstraints uint8

const (
	// ConfirmationNoPreference denotes that the resulting channel can be
	// zero conf or not.
	ConfirmationNoPreference ChannelConfirmationConstraints = 0

	// OnlyConfirmed denotes that the resulting channels must be confirmed
	// onchain before start routing.
	OnlyConfirmed ChannelConfirmationConstraints = 1

	// OnlyZeroConf denotes that the resulting channels can be used
	// without having to wait for onchain confirmations.
	OnlyZeroConf ChannelConfirmationConstraints = 2
)

// AuctionType is a numerical type used to denote in what auction market should
// this order be considered in.
type AuctionType uint32

const (
	// BTCInboundLiquidity is an auction type where the bidder pays the
	// asker a premium to get btc inbound liquidity from him.
	BTCInboundLiquidity AuctionType = 0

	// BTCOutboundLiquidity is an auction type where the bidder pays the
	// asker a premium to accept a btc channel from the bidder.
	BTCOutboundLiquidity AuctionType = 1
)

// String returns a human readable string representation of the auction type.
func (a AuctionType) String() string {
	switch a {
	case BTCInboundLiquidity:
		return "btc_inbound_liquidity"

	case BTCOutboundLiquidity:
		return "btc_outbound_liquidity"

	default:
		return fmt.Sprintf("unknown<%d>", a)
	}
}

var (
	// ErrInsufficientBalance is the error that is returned if an account
	// has insufficient balance to perform a requested action.
	ErrInsufficientBalance = errors.New("insufficient account balance")

	// ZeroNonce is used to find out if a user-provided nonce is empty.
	ZeroNonce Nonce
)

// MatchAnnouncementConstraints returns true when the asker announcement
// constraints match the bidder announcement preferences.
func MatchAnnouncementConstraints(asker ChannelAnnouncementConstraints,
	unannounced bool) bool {

	switch {
	case asker == AnnouncementNoPreference:
		return true

	case asker == OnlyAnnounced && !unannounced:
		return true

	case asker == OnlyUnannounced && unannounced:
		return true

	default:
		return false
	}
}

// MatchZeroConfConstraints returns true when the asker confirmation
// constraints match the bidder confirmation preferences.
func MatchZeroConfConstraints(asker ChannelConfirmationConstraints,
	zeroConf bool) bool {

	switch {
	case asker == ConfirmationNoPreference:
		return true

	case asker == OnlyConfirmed && !zeroConf:
		return true

	case asker == OnlyZeroConf && zeroConf:
		return true

	default:
		return false
	}
}

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
	Digest() ([hashSize]byte, error)

	// ReservedValue returns the maximum value that could be deducted from
	// the account if the order is matched, and therefore has to be
	// reserved to ensure the trader can afford it.
	ReservedValue(feeSchedule terms.FeeSchedule,
		accountVersion account.Version) btcutil.Amount
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

	// AuctionType is the market where this offer should be considered in.
	AuctionType AuctionType

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

	// MaxBatchFeeRate is the maximum fee rate the trader is willing to
	// pay for the batch transaction, in sat/kW.
	MaxBatchFeeRate chainfee.SatPerKWeight

	// AcctKey is key of the account the order belongs to.
	AcctKey [33]byte

	// LeaseDuration identifies how long this order wishes to acquire or
	// lease out capital in the Lightning Network for.
	LeaseDuration uint32

	// MinUnitsMatch signals the minimum number of units that must be
	// matched against an order.
	MinUnitsMatch SupplyUnit

	// ChannelType denotes the channel type that must be used for the
	// resulting matched channels.
	ChannelType ChannelType

	// AllowedNodeIDs is the list of node ids this order is allowed to
	// match with.
	AllowedNodeIDs [][33]byte

	// NotAllowedNodeIDs is the list of node ids this order is not allowed
	// to match with.
	NotAllowedNodeIDs [][33]byte

	// IsPublic is the flag used to signal if the details of this order can
	// be shared in public marketplaces or not.
	IsPublic bool
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
		Version:  VersionLeaseDurationBuckets,
	}
}

// NewKit creates a new kit from a nonce in case the preimage is not known.
func NewKit(nonce Nonce) *Kit {
	return &Kit{
		nonce:   nonce,
		Version: VersionLeaseDurationBuckets,
	}
}

// Ask is the specific order type representing the willingness of an auction
// participant to lend out their funds by opening channels to other auction
// participants.
type Ask struct {
	// Kit contains all the common order parameters.
	Kit

	// AnnouncementConstraints specifies the constraints for the matched
	// channels in terms of announced/unannounced.
	AnnouncementConstraints ChannelAnnouncementConstraints

	// ConfirmationConstraints specifies the constraints for the matched
	// channels in terms of confirmed/zero conf.
	ConfirmationConstraints ChannelConfirmationConstraints
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
func (a *Ask) Digest() ([hashSize]byte, error) {
	var (
		msg    bytes.Buffer
		result [hashSize]byte
	)
	switch a.Kit.Version {
	case VersionDefault:
		err := codec.WriteElements(
			&msg, a.nonce[:], uint32(a.Version), a.FixedRate,
			a.Amt, a.LeaseDuration, uint64(a.MaxBatchFeeRate),
		)
		if err != nil {
			return result, err
		}

	case VersionNodeTierMinMatch, VersionLeaseDurationBuckets,
		VersionSelfChanBalance, VersionSidecarChannel:

		err := codec.WriteElements(
			&msg, a.nonce[:], uint32(a.Version), a.FixedRate,
			a.Amt, a.LeaseDuration, uint64(a.MaxBatchFeeRate),
			uint32(a.MinUnitsMatch),
		)
		if err != nil {
			return result, err
		}

	case VersionChannelType:
		err := codec.WriteElements(
			&msg, a.nonce[:], uint32(a.Version), a.FixedRate,
			a.Amt, a.LeaseDuration, uint64(a.MaxBatchFeeRate),
			uint32(a.MinUnitsMatch), uint8(a.ChannelType),
		)
		if err != nil {
			return result, err
		}

	default:
		return result, fmt.Errorf("unknown version %d", a.Kit.Version)
	}
	return sha256.Sum256(msg.Bytes()), nil
}

// reservedValue returns the maximum value that could be deducted from a single
// account if the given order is matched under the worst case fee conditions.
// This usually means the order is partially matched with the minimum match
// size, all in different batches, leading to maximum chain and execution fees
// being paid.
//
// The passed function should be set to either calculate the maker or taker
// balance delta for a single match of the given amount.
func reservedValue(o Order, perMatchDelta func(btcutil.Amount) btcutil.Amount,
	accountVersion account.Version) btcutil.Amount {

	// If this order is in a state where it cannot be matched, return 0.
	if o.Details().State.Archived() {
		return 0
	}

	// The situation where the trader needs to pay the largest amount of
	// fees is when the order gets partially matched by its minimum possible
	// units per batch. This situation results in the most chain and
	// execution fees possible.
	totalSats := o.Details().UnitsUnfulfilled.ToSatoshis()
	minMatchSize := o.Details().MinUnitsMatch.ToSatoshis()
	maxNumMatches := totalSats / minMatchSize

	// We handle the case where the last match consumes the remainder of
	// the order size.
	rem := btcutil.Amount(0)
	if maxNumMatches*minMatchSize < totalSats {
		maxNumMatches--
		rem = totalSats - maxNumMatches*minMatchSize
	}

	// We'll calculate the worst case possible wrt. fees paid by the
	// account if the order is filled by minimum size matched.
	balanceDelta := maxNumMatches * perMatchDelta(minMatchSize)
	if rem > 0 {
		balanceDelta += perMatchDelta(rem)
	}

	// Subtract the worst case chain fee from the balance.
	maxFeeRate := o.Details().MaxBatchFeeRate
	balanceDelta -= maxNumMatches * EstimateTraderFee(
		1, maxFeeRate, accountVersion,
	)
	if rem > 0 {
		balanceDelta -= EstimateTraderFee(1, maxFeeRate, accountVersion)
	}

	// If the balance delta is negative, meaning this order will decrease
	// the balance, the reserved value is the negative balance delta.
	if balanceDelta < 0 {
		return -balanceDelta
	}

	// Otherwise this order will increase the balance if matched, and we
	// don't have to reserve any amount.
	return 0
}

// ReservedValue returns the maximum value that could be deducted from a single
// account if the ask is matched under the worst case fee conditions.
func (a *Ask) ReservedValue(feeSchedule terms.FeeSchedule,
	accountVersion account.Version) btcutil.Amount {

	// For an ask the clearing price will be no lower than the ask's fixed
	// rate, resulting in the smallest gain for the asker.
	clearingPrice := FixedRatePremium(a.FixedRate)

	return reservedValue(a, func(amt btcutil.Amount) btcutil.Amount {
		delta, _, _ := makerDelta(
			feeSchedule, clearingPrice, amt, amt, a.LeaseDuration,
		)
		return delta
	}, accountVersion)
}

// NodeTier an enum-like variable that presents which "tier" a node is in. A
// higher tier is better. Node tiers are used to allow clients to express their
// preference w.r.t the "quality" of a node they wish to buy channels from.
type NodeTier uint32

const (
	// NodeTierDefault only exists in-memory as allows users to specify
	// that they want to opt-into the default "node tier". The
	// DefaultMinNodeTier constant should point to what the current default
	// node tier is.
	NodeTierDefault NodeTier = 0

	// NodeTier0 is the tier for nodes which may not be explicitly ranked.
	// Orders submitted with this min tier express that they don't care
	// about the "quality" of the node they're matched with.
	NodeTier0 NodeTier = 1

	// NodeTier1 is the "base" node tier. Nodes on this tier are considered
	// to be relatively good. We have this be the first value in the enum
	// so it can be the default within the codebase and for order
	// submission/matching.
	NodeTier1 NodeTier = 2
)

// DefaultMinNodeTier is the default node tier. With this current value, Bids
// will default to only matching with nodes in the first tier and above.
const DefaultMinNodeTier = NodeTier1

// String returns the string representation of the target NodeTier.
func (n NodeTier) String() string {
	switch n {
	case NodeTier0:
		return "NodeTier0"

	case NodeTier1:
		return "NodeTier1"

	case NodeTierDefault:
		return "NodeTierDefault"

	default:
		return fmt.Sprintf("UnknownNodeTier(%v)", uint32(n))
	}
}

// Bid is the specific order type representing the willingness of an auction
// participant to pay for inbound liquidity provided by other auction
// participants.
type Bid struct {
	// Kit contains all the common order parameters.
	Kit

	// MinNodeTier is the minimum node tier that this order should be
	// matched with. Only Asks backed by nodes on this tier or above will
	// be matched with this bid.
	MinNodeTier NodeTier

	// SelfChanBalance is the initial outbound balance that should be added
	// to the channel resulting from matching this bid by moving additional
	// funds from the taker's account into the channel.
	SelfChanBalance btcutil.Amount

	// SidecarTicket indicates, if non-nil, that the channel being purchased
	// with this bid should be opened to a node other than the caller's
	// node. The lease recipient is another Pool (light) node that
	// authenticates itself to the auctioneer using the information in this
	// ticket (the information exchange between bidder and lease recipient
	// happens out of band). This will only be used if the order version is
	// VersionSidecarChannel or greater.
	SidecarTicket *sidecar.Ticket

	// UnannouncedChannel signals if the resulting channel needs to be
	// announced or not.
	UnannouncedChannel bool

	// ZeroConfChannel signals if the resulting channels need to be zero
	// conf or not.
	ZeroConfChannel bool
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
func (b *Bid) Digest() ([hashSize]byte, error) {
	var (
		msg    bytes.Buffer
		result [hashSize]byte
	)
	switch b.Kit.Version {
	case VersionDefault:
		err := codec.WriteElements(
			&msg, b.nonce[:], uint32(b.Version), b.FixedRate,
			b.Amt, b.LeaseDuration, uint64(b.MaxBatchFeeRate),
		)
		if err != nil {
			return result, err
		}

	case VersionNodeTierMinMatch, VersionLeaseDurationBuckets:
		err := codec.WriteElements(
			&msg, b.nonce[:], uint32(b.Version), b.FixedRate,
			b.Amt, b.LeaseDuration, uint64(b.MaxBatchFeeRate),
			uint32(b.MinNodeTier), uint32(b.MinUnitsMatch),
		)
		if err != nil {
			return result, err
		}

	case VersionSelfChanBalance:
		err := codec.WriteElements(
			&msg, b.nonce[:], uint32(b.Version), b.FixedRate,
			b.Amt, b.LeaseDuration, uint64(b.MaxBatchFeeRate),
			uint32(b.MinNodeTier), uint32(b.MinUnitsMatch),
			uint64(b.SelfChanBalance),
		)
		if err != nil {
			return result, err
		}

	case VersionSidecarChannel:
		var isSidecar uint8
		if b.SidecarTicket != nil {
			isSidecar = 1
		}

		err := codec.WriteElements(
			&msg, b.nonce[:], uint32(b.Version), b.FixedRate,
			b.Amt, b.LeaseDuration, uint64(b.MaxBatchFeeRate),
			uint32(b.MinNodeTier), uint32(b.MinUnitsMatch),
			uint64(b.SelfChanBalance), isSidecar,
		)
		if err != nil {
			return result, err
		}

	case VersionChannelType:
		var isSidecar uint8
		if b.SidecarTicket != nil {
			isSidecar = 1
		}

		err := codec.WriteElements(
			&msg, b.nonce[:], uint32(b.Version), b.FixedRate,
			b.Amt, b.LeaseDuration, uint64(b.MaxBatchFeeRate),
			uint32(b.MinNodeTier), uint32(b.MinUnitsMatch),
			uint64(b.SelfChanBalance), isSidecar,
			uint8(b.ChannelType),
		)
		if err != nil {
			return result, err
		}

	default:
		return result, fmt.Errorf("unknown version %d", b.Kit.Version)
	}
	return sha256.Sum256(msg.Bytes()), nil
}

// ReservedValue returns the maximum value that could be deducted from a single
// account if the bid is matched under the worst case fee conditions.
func (b *Bid) ReservedValue(feeSchedule terms.FeeSchedule,
	accountVersion account.Version) btcutil.Amount {

	// For a bid, the final clearing price is never higher that the bid's
	// fixed rate, resulting in the highest possible premium paid by the
	// bidder.
	clearingPrice := FixedRatePremium(b.FixedRate)

	return reservedValue(b, func(amt btcutil.Amount) btcutil.Amount {
		premiumAmt := amt
		if b.Details().AuctionType == BTCOutboundLiquidity {
			premiumAmt += b.SelfChanBalance
		}

		delta, _, _ := takerDelta(
			feeSchedule, clearingPrice, premiumAmt,
			b.SelfChanBalance, b.LeaseDuration,
		)
		return delta
	}, accountVersion)
}

// CheckOfferParams makes sure the offer parameters of an offer are valid and
// sane.
func CheckOfferParams(auctionType AuctionType, capacity, pushAmt,
	baseSupplyUnit btcutil.Amount) error {

	if capacity == 0 || capacity%baseSupplyUnit != 0 {
		return fmt.Errorf("channel capacity must be positive multiple "+
			"of %d", baseSupplyUnit)
	}

	if auctionType == BTCInboundLiquidity && pushAmt > capacity {
		return fmt.Errorf("self channel balance must be smaller than " +
			"or equal to capacity")
	}

	if auctionType == BTCOutboundLiquidity {
		// Only multiples of 100k sats are allowed in the outbound
		// market.
		if pushAmt == 0 || pushAmt%baseSupplyUnit != 0 {
			return fmt.Errorf("self balance must be a positive "+
				"multiple of %d", baseSupplyUnit)
		}
	}

	return nil
}

// CheckOfferParamsForOrder makes sure that the order parameters in a
// sidecar offer are formally valid, sane and match the order parameters.
func CheckOfferParamsForOrder(auctionType AuctionType, offer sidecar.Offer,
	bidAmt, bidMinUnitsMatch, baseSupplyUnit btcutil.Amount) error {

	if auctionType != BTCInboundLiquidity {
		return fmt.Errorf("%s market does not support sidecar tickets",
			auctionType)
	}

	err := CheckOfferParams(
		auctionType, offer.Capacity, offer.PushAmt, baseSupplyUnit,
	)
	if err != nil {
		return err
	}

	if offer.Capacity != bidAmt {
		return fmt.Errorf("invalid bid amount %v, must match sidecar "+
			"ticket's capacity %v", bidAmt, offer.Capacity)
	}

	if offer.Capacity != bidMinUnitsMatch*baseSupplyUnit {
		return fmt.Errorf("invalid min units match %v, must match "+
			"sidecar ticket's capacity %v",
			bidMinUnitsMatch*baseSupplyUnit, offer.Capacity)
	}

	return nil
}

// ValidateSelfChanBalance makes sure that all conditions to use the
// SelfChanBalance field on a bid order are met.
func (b *Bid) ValidateSelfChanBalance() error {
	if b.Version < VersionSelfChanBalance {
		return fmt.Errorf("cannot use self chan balance with old " +
			"order version")
	}

	if err := CheckOfferParams(
		b.AuctionType, b.Amt, b.SelfChanBalance, BaseSupplyUnit,
	); err != nil {
		return fmt.Errorf("invalid self chan balance: %v", err)
	}

	if b.Units != b.MinUnitsMatch {
		return fmt.Errorf("to use self chan balance the min units " +
			"match must be equal to the order amount in units")
	}

	if b.AuctionType == BTCOutboundLiquidity &&
		b.SelfChanBalance < BaseSupplyUnit {

		return fmt.Errorf("to participate in the outbound liquidity " +
			"market the self chan balance should be at least " +
			"100k sats")
	}

	return nil
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

	// DeleteOrder removes the order with the given Nonce.
	//
	// Note: this method deletes the order without checking if it is
	// referenced somewhere else (e.g. pending batch).
	DeleteOrder(Nonce) error

	// StorePendingBatch atomically stages all modified orders/accounts as a
	// result of a pending batch. If any single operation fails, the whole
	// set of changes is rolled back. Once the batch has been
	// finalized/confirmed on-chain, then the stage modifications will be
	// applied atomically as a result of MarkBatchComplete.
	StorePendingBatch(_ *Batch, orders []Nonce,
		orderModifiers [][]Modifier, accounts []*account.Account,
		accountModifiers [][]account.Modifier) error

	// MarkBatchComplete marks a pending batch as complete, applying any
	// staged modifications necessary, and allowing a trader to participate
	// in a new batch. If a pending batch is not found, ErrNoPendingBatch is
	// returned.
	MarkBatchComplete() error
}

// UserError is an error type that is returned if an action fails because of
// an invalid action or information provided by the user.
type UserError struct {
	FailMsg string
	Details *auctioneerrpc.InvalidOrder
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

// PendingChanKey calculates the pending channel ID to be used for funding
// purposes for a given bid and ask. The pending channel ID must be unique, so
// we use the hash of the concatenation of the two nonces: sha256(askNonce ||
// bidNonce).
func PendingChanKey(askNonce, bidNonce Nonce) [32]byte {
	var pid [32]byte

	h := sha256.New()
	_, _ = h.Write(askNonce[:])
	_, _ = h.Write(bidNonce[:])

	copy(pid[:], h.Sum(nil))

	return pid
}

// Manager is the interface a manager implements to deal with
// the orders.
type Manager interface {
	// Start starts all concurrent tasks the manager is responsible for.
	Start() error

	// Stop stops all concurrent tasks the manager is responsible for.
	Stop()

	// PrepareOrder validates an order, signs it and then stores it locally.
	PrepareOrder(ctx context.Context, order Order, acct *account.Account,
		terms *terms.AuctioneerTerms) (*ServerOrderParams, error)

	// OrderMatchValidate verifies an incoming batch is sane before
	// accepting it.
	OrderMatchValidate(batch *Batch, bestHeight uint32) error

	// HasPendingBatch returns whether a pending batch is currently being
	// processed.
	HasPendingBatch() bool

	// PendingBatch returns the current pending batch being validated.
	PendingBatch() *Batch

	// BatchSign returns the witness stack of all account inputs in a batch
	// that belong to the trader.
	BatchSign() (BatchSignature, AccountNonces, error)

	// BatchFinalize marks a batch as complete upon receiving the finalize
	// message from the auctioneer.
	BatchFinalize(batchID BatchID) error

	// OurNodePubkey returns our lnd node's public identity key or an error
	// if the manager wasn't fully started yet.
	OurNodePubkey() ([33]byte, error)
}
