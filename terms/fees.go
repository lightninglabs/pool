package terms

import "github.com/btcsuite/btcd/btcutil"

// FeeSchedule is an interface that represents the configuration source that
// the auctioneer will use to determine how much to charge in fees for each
// trader.
type FeeSchedule interface {
	// BaseFee is the base fee the auctioneer will charge the traders for
	// each executed order.
	BaseFee() btcutil.Amount

	// ExecutionFee computes the execution fee (usually based off of a
	// rate) for the target amount.
	ExecutionFee(amt btcutil.Amount) btcutil.Amount
}

// LinearFeeSchedule is a FeeSchedule that calculates the execution fee based
// upon a static base fee and a variable fee rate in parts per million.
type LinearFeeSchedule struct {
	baseFee btcutil.Amount
	feeRate btcutil.Amount
}

// BaseFee is the base fee the auctioneer will charge the traders for each
// executed order.
//
// NOTE: This method is part of the orderT.FeeSchedule interface.
func (s *LinearFeeSchedule) BaseFee() btcutil.Amount {
	return s.baseFee
}

// FeeRate is the variable fee rate in parts per million.
func (s *LinearFeeSchedule) FeeRate() btcutil.Amount {
	return s.feeRate
}

// ExecutionFee computes the execution fee (usually based off of a rate) for
// the target amount.
//
// NOTE: This method is part of the orderT.FeeSchedule interface.
func (s *LinearFeeSchedule) ExecutionFee(amt btcutil.Amount) btcutil.Amount {
	return amt * s.feeRate / 1_000_000
}

// NewLinearFeeSchedule creates a new linear fee schedule based upon a static
// base fee and a relative fee rate in parts per million.
func NewLinearFeeSchedule(baseFee, feeRate btcutil.Amount) *LinearFeeSchedule {
	return &LinearFeeSchedule{
		baseFee: baseFee,
		feeRate: feeRate,
	}
}

// This is a compile time check to make certain that LinearFeeSchedule
// implements the orderT.FeeSchedule interface.
var _ FeeSchedule = (*LinearFeeSchedule)(nil)
