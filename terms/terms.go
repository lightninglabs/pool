package terms

import (
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/poolrpc"
)

// AuctioneerTerms is a struct that holds all dynamic terms the auctioneer defines.
type AuctioneerTerms struct {
	// MaxAccountValue is the current maximum allowed account value.
	MaxAccountValue btcutil.Amount

	// MaxOrderDuration is the current maximum value for an order duration.
	// That means that an ask's MaxDuration or a bid's MinDuration cannot
	// exceed this value.
	MaxOrderDuration uint32

	// OrderExecBaseFee is the base fee charged per order, regardless of the
	// matched size.
	OrderExecBaseFee btcutil.Amount

	// OrderExecFeeRate is the fee rate in parts per million.
	OrderExecFeeRate btcutil.Amount

	// LeaseDurationBuckets lists the current set of lease durations and
	// maps them to their current market state.
	LeaseDurationBuckets map[uint32]poolrpc.DurationBucketState
}

// FeeSchedule returns the execution fee as a FeeSchedule.
func (t *AuctioneerTerms) FeeSchedule() FeeSchedule {
	return NewLinearFeeSchedule(
		t.OrderExecBaseFee, t.OrderExecFeeRate,
	)
}
