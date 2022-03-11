package terms

import (
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// AuctioneerTerms is a struct that holds all dynamic terms the auctioneer defines.
type AuctioneerTerms struct {
	// MaxAccountValue is the current maximum allowed account value.
	MaxAccountValue btcutil.Amount

	// OrderExecBaseFee is the base fee charged per order, regardless of the
	// matched size.
	OrderExecBaseFee btcutil.Amount

	// OrderExecFeeRate is the fee rate in parts per million.
	OrderExecFeeRate btcutil.Amount

	// LeaseDurationBuckets lists the current set of lease durations and
	// maps them to their current market state.
	LeaseDurationBuckets map[uint32]auctioneerrpc.DurationBucketState

	// NextBatchConfTarget is the confirmation target the auctioneer will
	// use to estimate the fee rate of the next batch.
	NextBatchConfTarget uint32

	// NextBatchFeeRate is the fee rate estimate the auctioneer will use for
	// the next batch.
	NextBatchFeeRate chainfee.SatPerKWeight

	// NextBatchClear is the time at which the auctioneer will attempt to
	// clear the next batch.
	NextBatchClear time.Time

	// AutoRenewExtensionBlocks is the threshold used to extend the expiry height
	// of the accounts that are close to expire after participating in a batch.
	AutoRenewExtensionBlocks uint32
}

// FeeSchedule returns the execution fee as a FeeSchedule.
func (t *AuctioneerTerms) FeeSchedule() FeeSchedule {
	return NewLinearFeeSchedule(
		t.OrderExecBaseFee, t.OrderExecFeeRate,
	)
}
