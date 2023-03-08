package order

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// MaxBatchIDHistoryLookup is the maximum number of iterations we do
	// when iterating batch IDs. It's unlikely to happen but in case we
	// start from an invalid value we want to avoid an endless loop. If we
	// ever have more than that many batches, we need to handle them in
	// multiple steps anyway.
	MaxBatchIDHistoryLookup = 10_000
)

var (
	// DefaultBatchStepTimeout is the default time we allow an action that
	// blocks the batch conversation (like peer connection establishment or
	// channel open) to take. If any action takes longer, we might reject
	// the order from that slow peer. This value SHOULD be lower than the
	// defaultMsgTimeout on the server side otherwise nodes might get kicked
	// out of the match making process for timing out even though it was
	// their peer's fault.
	DefaultBatchStepTimeout = 15 * time.Second
)

// BatchVersion is the type for the batch verification protocol.
type BatchVersion uint32

const (
	// DefaultBatchVersion is the first implemented version of the batch
	// verification protocol.
	DefaultBatchVersion BatchVersion = 0

	// ExtendAccountBatchVersion is the first version where accounts expiry
	// are extended after participating in a batch.
	ExtendAccountBatchVersion BatchVersion = 1

	// UnannouncedChannelsBatchVersion is the first version where orders
	// can set the flags to only match with announced/unannonced channels.
	//
	// NOTE: This feature requires the runtime support:
	// - The asker needs to open the channel with the right `private` value
	// - The bidder needs to be able to set the channel acceptor for the
	//   channel with the right `private` value.
	// For that reason this needs to be a batch version and not only an
	// order one.
	UnannouncedChannelsBatchVersion BatchVersion = 2

	// UpgradeAccountTaprootBatchVersion is the batch version where accounts
	// are automatically upgraded to Taproot accounts. We leave a gap up to
	// 10 on purpose to allow for in-between versions (that aren't dependent
	// on a lnd version) to be added. This version is used as the base
	// version when using a flag based versioning scheme, as this is now the
	// feature set that every lnd node supports.
	UpgradeAccountTaprootBatchVersion BatchVersion = 10

	// ZeroConfChannelsBatchVersion is the first version where orders can
	// set the flags to only match with confirmed/zeroconf channels.
	//
	// NOTE: This feature requires the runtime to support:
	// - The asker needs to open the channel with the right `zeroconf` bit.
	// - The bidder needs to be able to set the channel acceptor for the
	//   channel with the right  `Zeroconf` bool value && `MinDepth=0`.
	// The LND node should be running version v0.15.1-beta or newer.
	// Because this version already existed (in a deployed state) as a
	// config dependent version before we switched to a flag based version
	// scheme, this still has a linear version number. But the features
	// expressed by this version can also be expressed as
	// 	UpgradeAccountTaprootBatchVersion | ZeroConfChannelsFlag
	ZeroConfChannelsBatchVersion BatchVersion = 11
)

const (
	// LinearVersionEnd is the end of the linear version space. This is used
	// to determine whether a version is one of the old, linear ones or one
	// of the new, flag based ones.
	LinearVersionEnd BatchVersion = 0x0000_000F

	// ZeroConfChannelsFlag is the flag in the batch version that indicates
	// that orders can set the flags to only match with confirmed/zeroconf
	// channels.
	ZeroConfChannelsFlag BatchVersion = 0x0000_0010

	// UpgradeAccountTaprootV2Flag is the flag in the batch version that
	// indicates accounts can automatically be upgraded to Taproot v2 (using
	// the MuSig2 v1.0.0-rc2 spec).
	UpgradeAccountTaprootV2Flag BatchVersion = 0x0000_0020
)

// SupportsAccountExtension is a helper function to easily check if a version
// supports account extension after participating in a batch or not.
func (bv BatchVersion) SupportsAccountExtension() bool {
	return (bv & LinearVersionEnd) >= ExtendAccountBatchVersion
}

// SupportsUnannouncedChannels is a helper function to easily check if a version
// supports orders with unannounced channels or not.
func (bv BatchVersion) SupportsUnannouncedChannels() bool {
	return (bv & LinearVersionEnd) >= UnannouncedChannelsBatchVersion
}

// SupportsAccountTaprootUpgrade is a helper function to easily check if a
// version supports upgrading SegWit v0 (p2wsh) accounts to Taproot (p2tr) or
// not.
func (bv BatchVersion) SupportsAccountTaprootUpgrade() bool {
	return (bv & LinearVersionEnd) >= UpgradeAccountTaprootBatchVersion
}

// SupportsAccountTaprootV2Upgrade is a helper function to easily check if a
// version supports upgrading SegWit v0 (p2wsh) or Taproot v2 (p2tr) to
// Taproot v2 (p2tr) or not.
func (bv BatchVersion) SupportsAccountTaprootV2Upgrade() bool {
	return (bv & UpgradeAccountTaprootV2Flag) == UpgradeAccountTaprootV2Flag
}

// SupportsZeroConfChannels is the helper function to easily check if a version
// supports orders with zeroconf channels or not.
func (bv BatchVersion) SupportsZeroConfChannels() bool {
	return (bv&LinearVersionEnd) >= ZeroConfChannelsBatchVersion ||
		bv&ZeroConfChannelsFlag == ZeroConfChannelsFlag
}

const (
	// LatestBatchVersion points to the most recent batch version.
	LatestBatchVersion = UpgradeAccountTaprootBatchVersion |
		ZeroConfChannelsFlag | UpgradeAccountTaprootV2Flag

	// LegacyLeaseDurationBucket is the single static duration bucket that
	// was used for orders before dynamic duration buckets were added.
	LegacyLeaseDurationBucket uint32 = 2016
)

// BatchID is a 33-byte point that uniquely identifies this batch. This ID
// will be used later for account key derivation when constructing the batch
// execution transaction.
type BatchID [33]byte

// NewBatchID returns a new batch ID for the given public key.
func NewBatchID(pub *btcec.PublicKey) BatchID {
	var b BatchID
	copy(b[:], pub.SerializeCompressed())
	return b
}

// AccountDiff represents a matching+clearing event for a trader's account.
// This diff shows the total balance delta along with a breakdown for each item
// for a trader's account.
type AccountDiff struct {
	// AccountKeyRaw is the raw serialized account public key this diff
	// refers to.
	AccountKeyRaw [33]byte

	// AccountKey is the parsed account public key this diff refers to.
	AccountKey *btcec.PublicKey

	// EndingState is the ending on-chain state of the account after the
	// executed batch as the auctioneer calculated it.
	EndingState auctioneerrpc.AccountDiff_AccountState

	// EndingBalance is the ending balance for a trader's account.
	EndingBalance btcutil.Amount

	// OutpointIndex is the index of the re-created account output in the
	// batch transaction. This is set to -1 if no account output has been
	// created because the leftover value was considered to be dust.
	OutpointIndex int32

	// NewExpiry is the new expiry height for this account. This field
	// can be safely ignored if its value is 0.
	NewExpiry uint32

	// NewVersion is the version of the account that should be used after
	// the batch went through. This field influences the type of account
	// output that is created by this batch. If this differs from the
	// CurrentVersion, then the account was upgraded in this batch.
	NewVersion account.Version
}

// validateEndingState validates that the ending state of an account as
// proposed by the server is correct.
func (d *AccountDiff) validateEndingState(tx *wire.MsgTx,
	acct *account.Account) error {

	state := d.EndingState
	wrongStateErr := fmt.Errorf(
		"unexpected state %v for ending balance %d", state,
		d.EndingBalance,
	)

	// Depending on the final amount of the account, we might get
	// dust which is handled differently.
	if d.EndingBalance < MinNoDustAccountSize {
		// The ending balance of the account is too small to be spent
		// by a simple transaction and not create a dust output. We
		// expect the server to set the state correctly and not re-
		// create an account outpoint.
		if state != auctioneerrpc.AccountDiff_OUTPUT_DUST_EXTENDED_OFFCHAIN &&
			state != auctioneerrpc.AccountDiff_OUTPUT_DUST_ADDED_TO_FEES &&
			state != auctioneerrpc.AccountDiff_OUTPUT_FULLY_SPENT {

			return wrongStateErr
		}
		if d.OutpointIndex >= 0 {
			return fmt.Errorf("unexpected outpoint index for dust " +
				"account")
		}
	} else {
		// There should be enough balance left to justify a new account
		// output. We should get the outpoint from the server.
		if state != auctioneerrpc.AccountDiff_OUTPUT_RECREATED {
			return wrongStateErr
		}
		if d.OutpointIndex < 0 {
			return fmt.Errorf("outpoint index invalid for non-"+
				"dust account with state %d and balance %d",
				state, d.EndingBalance)
		}

		// Make sure the outpoint index is correct and there is an
		// output with the correct amount there.
		if d.OutpointIndex >= int32(len(tx.TxOut)) {
			return fmt.Errorf("outpoint index out of bounds")
		}
		out := tx.TxOut[d.OutpointIndex]
		if btcutil.Amount(out.Value) != d.EndingBalance {
			return fmt.Errorf("invalid account output amount. got "+
				"%d expected %d", out.Value, d.EndingBalance)
		}

		// Final check, make sure we arrive at the same script for the
		// new account output.
		nextScript, err := acct.NextOutputScript()
		if err != nil {
			return fmt.Errorf("could not derive next account "+
				"script: %v", err)
		}
		if !bytes.Equal(out.PkScript, nextScript) {
			return fmt.Errorf("unexpected account output "+
				"script: want %x got %x", nextScript,
				out.PkScript)
		}
	}

	return nil
}

// Batch is all the information the auctioneer sends to each trader for them to
// validate a batch execution.
type Batch struct {
	// ID is the batch's unique ID. If multiple messages come in with the
	// same ID, they are to be considered to be the _same batch_ with
	// updated matches. Any previous version of a batch with that ID should
	// be discarded in that case.
	ID BatchID

	// BatchVersion is the version of the batch verification protocol.
	Version BatchVersion

	// MatchedOrders is a map between all trader's orders and the other
	// orders that were matched to them in the batch.
	MatchedOrders map[Nonce][]*MatchedOrder

	// AccountDiffs is the calculated difference for each trader's account
	// that was involved in the batch.
	AccountDiffs []*AccountDiff

	// ExecutionFee is the FeeSchedule that was used by the server to
	// calculate the execution fee.
	ExecutionFee terms.FeeSchedule

	// ClearingPrices is a map of the lease duration markets and the fixed
	// rate the orders were cleared at within that market.
	ClearingPrices map[uint32]FixedRatePremium

	// BatchTX is the complete batch transaction with all non-witness data
	// fully populated.
	BatchTX *wire.MsgTx

	// BatchTxFeeRate is the miner fee rate in sat/kW that was chosen for
	// the batch transaction.
	BatchTxFeeRate chainfee.SatPerKWeight

	// FeeRebate is the rebate that was offered to the trader if another
	// batch participant wanted to pay more fees for a faster confirmation.
	FeeRebate btcutil.Amount

	// HeightHint represents the earliest absolute height in the chain in
	// which the batch transaction can be found within. This will be used by
	// traders to base off their absolute channel lease maturity height.
	HeightHint uint32

	// ServerNonces is the map of all the auctioneer's public nonces for
	// each of the (Taproot enabled) accounts in the batch, keyed by the
	// account's trader key. This is volatile (in-memory only) information
	// that is _not_ persisted as part of the batch snapshot.
	ServerNonces AccountNonces

	// PreviousOutputs is the list of previous output scripts and amounts
	// (UTXO information) for all the inputs being spent by the batch
	// transaction. This is volatile (in-memory only) information that is
	// _not_ persisted as part of the batch snapshot.
	PreviousOutputs []*wire.TxOut
}

// Fetcher describes a function that's able to fetch the latest version of an
// order based on its nonce.
type Fetcher func(Nonce) (Order, error)

// ChannelOutput returns the transaction output and output index of the channel
// created for an order of ours that was matched with another one in a batch.
func ChannelOutput(batchTx *wire.MsgTx, wallet lndclient.WalletKitClient,
	ourOrder Order, otherOrder *MatchedOrder) (*wire.TxOut, uint32, error) {

	var ourKey []byte
	ourOrderBid, isBid := ourOrder.(*Bid)
	if isBid && ourOrderBid.SidecarTicket != nil {
		r := ourOrderBid.SidecarTicket.Recipient
		if r == nil || r.MultiSigPubKey == nil {
			return nil, 0, fmt.Errorf("recipient information in " +
				"sidecar ticket missing")
		}

		ourKey = r.MultiSigPubKey.SerializeCompressed()
	} else {
		// Re-derive our multisig key first.
		ctxt, cancel := context.WithTimeout(
			context.Background(), deriveKeyTimeout,
		)
		defer cancel()
		ourKeyDesc, err := wallet.DeriveKey(
			ctxt, &ourOrder.Details().MultiSigKeyLocator,
		)
		if err != nil {
			return nil, 0, fmt.Errorf("could not derive our "+
				"multisig key: %v", err)
		}

		ourKey = ourKeyDesc.PubKey.SerializeCompressed()
	}

	// A self channel balance is added on top of the number of units filled.
	var selfChanBalance btcutil.Amount
	if ourOrder.Type() == TypeBid {
		selfChanBalance = ourOrder.(*Bid).SelfChanBalance
	} else {
		selfChanBalance = otherOrder.Order.(*Bid).SelfChanBalance
	}

	// Gather the information we expect to find in the batch TX.
	expectedOutputSize := selfChanBalance + otherOrder.UnitsFilled.ToSatoshis()
	_, expectedOut, err := input.GenFundingPkScript(
		ourKey, otherOrder.MultiSigKey[:], int64(expectedOutputSize),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("could not create multisig script: "+
			"%v", err)
	}

	// Locate the channel output now that we know what to look for.
	for idx, out := range batchTx.TxOut {
		if out.Value == expectedOut.Value &&
			bytes.Equal(out.PkScript, expectedOut.PkScript) {

			// Bingo, this is what we want.
			return out, uint32(idx), nil
		}
	}

	return nil, 0, fmt.Errorf("no channel output found in batch tx for "+
		"matched order %v", otherOrder.Order.Nonce())
}

// MatchedOrder is the other side to one of our matched orders. It contains all
// the information that is needed to validate the match and to start negotiating
// the channel opening with the matched trader's node.
type MatchedOrder struct {
	// Order contains the details of the other order as sent by the server.
	Order Order

	// MultiSigKey is a key of the node creating the order that will be used
	// to craft the channel funding TX's 2-of-2 multi signature output.
	MultiSigKey [33]byte

	// NodeKey is the identity public key of the node creating the order.
	NodeKey [33]byte

	// NodeAddrs is the list of network addresses of the node creating the
	// order.
	NodeAddrs []net.Addr

	// UnitsFilled is the number of units that were matched by this order.
	UnitsFilled SupplyUnit
}

// BatchSignature is a map type that is keyed by a trader's account key and
// contains the multi-sig signature for the input that spends from the current
// account in a batch.
type BatchSignature map[[33]byte][]byte

// AccountNonces is a map of all server or client nonces for a batch signing
// session, keyed by the account key.
type AccountNonces map[[33]byte]poolscript.MuSig2Nonces

// BatchVerifier is an interface that can verify a batch from the point of view
// of the trader.
type BatchVerifier interface {
	// Verify makes sure the batch prepared by the server is correct and
	// can be accepted by the trader.
	Verify(batch *Batch, bestHeight uint32) error
}

// BatchSigner is an interface that can sign for a trader's account inputs in
// a batch.
type BatchSigner interface {
	// Sign returns the witness stack of all account inputs in a batch that
	// belong to the trader.
	Sign(*Batch) (BatchSignature, AccountNonces, error)
}

// BatchStorer is an interface that can store a batch to the local database by
// applying all the diffs to the orders and accounts.
type BatchStorer interface {
	// StorePendingBatch makes sure all changes executed by a pending batch
	// are correctly and atomically stored to the database.
	StorePendingBatch(_ *Batch) error

	// MarkBatchComplete marks a pending batch as complete, allowing a
	// trader to participate in a new batch.
	MarkBatchComplete() error
}

// DecrementingBatchIDs lists all possible batch IDs that can exist between a
// start and end key. Start key must be the larger/higher key, decrementing it a
// given number of times should finally result in the end key. If the keys
// aren't related or are too far apart, we return a maximum number of IDs that
// corresponds to our safety net parameter and the end key won't be contained in
// the list. The returned list is in descending order, meaning the first entry
// is the start key, the last entry is the end key.
func DecrementingBatchIDs(startKey, endKey *btcec.PublicKey) []BatchID {
	var (
		result       = []BatchID{NewBatchID(startKey)}
		tempBatchKey = startKey
		numIDs       = 0
	)

	// No need to loop if start and end are the same.
	if startKey.IsEqual(endKey) {
		return result
	}

	for !tempBatchKey.IsEqual(endKey) {
		numIDs++

		// Unlikely to happen but in case we start from an invalid value
		// we want to avoid an endless loop. This will cause the list to
		// be incomplete if we ever have more than the maximum number of
		// batches. But there is no scenario where it makes sense to
		// work with more than that number of batches in one step. So if
		// we ever do have more than that maximum number of batches, we
		// need to query them in multiple steps.
		if numIDs >= MaxBatchIDHistoryLookup {
			break
		}

		tempBatchKey = poolscript.DecrementKey(tempBatchKey)
		result = append(result, NewBatchID(tempBatchKey))
	}
	return result
}
