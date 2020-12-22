package terms

import (
	"time"

	"github.com/btcsuite/btcutil"
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

	// LeaseDurations lists the current set of active lease durations in
	// the market at this instance. The duration in blocks is mapped to a
	// bool which indicates if this market is actively clearing order or
	// not.
	LeaseDurations map[uint32]bool

	// NextBatchConfTarget is the confirmation target the auctioneer will
	// use to estimate the fee rate of the next batch.
	NextBatchConfTarget uint32

	// NextBatchFeeRate is the fee rate estimate the auctioneer will use for
	// the next batch.
	NextBatchFeeRate chainfee.SatPerKWeight

	// NextBatchClear is the time at which the auctioneer will attempt to
	// clear the next batch.
	NextBatchClear time.Time
}

// FeeSchedule returns the execution fee as a FeeSchedule.
func (t *AuctioneerTerms) FeeSchedule() FeeSchedule {
	return NewLinearFeeSchedule(
		t.OrderExecBaseFee, t.OrderExecFeeRate,
	)
}
