package terms

import (
	"github.com/btcsuite/btcutil"
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

	// LeaseDurations lists the current set of active lease durations in
	// the market at this instance. The duration in blocks is mapped to a
	// bool which indicates if this market is actively clearing order or
	// not.
	LeaseDurations map[uint32]bool
}

// FeeSchedule returns the execution fee as a FeeSchedule.
func (t *AuctioneerTerms) FeeSchedule() FeeSchedule {
	return NewLinearFeeSchedule(
		t.OrderExecBaseFee, t.OrderExecFeeRate,
	)
}
