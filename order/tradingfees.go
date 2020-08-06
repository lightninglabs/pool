package order

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/lightninglabs/llm/clmscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// FeeRateTotalParts defines the granularity of the fixed rate used to
	// compute the per-block interest rate.  Throughout the codebase, we'll
	// use fix based arithmetic to compute fees.
	FeeRateTotalParts = 1e9

	// dustLimitP2WPKH is the minimum size of a P2WPKH output to not be
	// considered dust.
	dustLimitP2WPKH = txrules.GetDustThreshold(
		input.P2WPKHSize, txrules.DefaultRelayFeePerKb,
	)

	// MinNoDustAccountSize is the minimum number of satoshis an account
	// output value needs to be to not be considered a dust output. This is
	// the cost of a spend TX at 1 sat/byte plus the minimum non-dust output
	// size.
	MinNoDustAccountSize = minNoDustAccountSize()
)

// FixedRatePremium is the unit that we'll use to express the "lease" rate of
// the funds within a channel. This value is compounded every N blocks. As a
// result, a trader will pay for the longer they wish to allocate liquidity to
// another agent in the market. In other words, this is our period interest
// rate.
type FixedRatePremium uint32

// LumpSumPremium calculates the total amount that will be paid out to lease an
// asset at the target FixedRatePremium, for the specified amount. This
// computes the total amount that will be paid over the lifetime of the asset.
// We'll use this to compute the total amount that a taker needs to set aside
// once they're matched.
func (f FixedRatePremium) LumpSumPremium(amt btcutil.Amount,
	durationBlocks uint32) btcutil.Amount {

	// First, we'll compute the premium that will be paid each block over
	// the lifetime of the asset. This can be a fraction of a satoshi as one
	// block is a very short period.
	premiumPerBlock := PerBlockPremium(amt, uint32(f))

	// Once we have this value, we can then multiply the premium paid per
	// block times the number of compounding periods, or the total lease
	// duration.
	return btcutil.Amount(premiumPerBlock * float64(durationBlocks))
}

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

// PerBlockPremium calculates the absolute premium in fractions of satoshis for
// a one block duration from the amount and the specified fee rate in parts per
// million.
func PerBlockPremium(amt btcutil.Amount, fixedRate uint32) float64 {
	return float64(amt) * float64(fixedRate) / FeeRateTotalParts
}

// EstimateTraderFee calculates the chain fees a trader has to pay for their
// part of a batch transaction. The more outputs a trader creates (channels),
// the higher fee they will pay.
func EstimateTraderFee(numTraderChans uint32,
	feeRate chainfee.SatPerKWeight) btcutil.Amount {

	var weightEstimate int64

	// First we'll tack on the size of their account output that will be
	// threaded through in this batch.
	weightEstimate += input.P2WSHOutputSize

	// Next we'll add the size of a typical input to account for the input
	// spending their account outpoint.
	weightEstimate += input.InputSize

	// Next, for each channel that will be created involving this trader,
	// we'll add the size of a regular P2WSH output. We divide value by two
	// as the maker and taker will split this fees.
	chanOutputSize := uint32(input.P2WSHOutputSize)
	weightEstimate += int64(chanOutputSize*numTraderChans+1) / 2

	// At this point we've tallied all the non-witness data, so we multiply
	// by 4 to scale it up.
	weightEstimate *= blockchain.WitnessScaleFactor

	// Finally, we tack on the size of the witness spending the account
	// outpoint.
	weightEstimate += clmscript.MultiSigWitnessSize

	return feeRate.FeeForWeight(weightEstimate)
}

// AccountTally keeps track of an account's balance and fees for all orders in
// a batch that spend from/use that account.
type AccountTally struct {
	// EndingBalance is the ending balance for a trader's account.
	EndingBalance btcutil.Amount

	// TotalExecutionFeesPaid is the total amount of fees a trader paid to
	// the venue.
	TotalExecutionFeesPaid btcutil.Amount

	// TotalTakerFeesPaid is the total amount of fees the trader paid to
	// purchase any channels in this batch.
	TotalTakerFeesPaid btcutil.Amount

	// TotalMakerFeesAccrued is the total amount of fees the trader gained
	// by selling channels in this batch.
	TotalMakerFeesAccrued btcutil.Amount

	// NumChansCreated is the number of new channels that were created for
	// one account in a batch. This is needed to calculate the chain fees
	// that need to be paid from that account.
	NumChansCreated uint32
}

// makerDelta calculates an account's balance and fee difference for a single
// order where the account is involved on the maker side. It returns the
// balance delta, fees accrued, and execution fee paid.
func makerDelta(feeSchedule FeeSchedule, price FixedRatePremium,
	totalSats btcutil.Amount, duration uint32) (btcutil.Amount,
	btcutil.Amount, btcutil.Amount) {

	// First, we'll need to subtract the total amount of sats cleared (the
	// channel size), from the balance of the maker.
	balanceDelta := -totalSats

	// Calculate the premium based on the duration the capital will be
	// locked for.
	satsPremium := price.LumpSumPremium(totalSats, duration)

	// The premium is paid to the maker.
	balanceDelta += satsPremium
	makerFeesAccrued := satsPremium

	// Finally, we'll subtract the paid execution fees from the balance.
	executionFee := executionFee(totalSats, feeSchedule)
	balanceDelta -= executionFee

	return balanceDelta, makerFeesAccrued, executionFee
}

// CalcMakerDelta calculates an account's balance and fee difference for a
// single order where the account is involved on the maker side. It returns the
// execution fee that is collected by the auctioneer.
func (t *AccountTally) CalcMakerDelta(feeSchedule FeeSchedule,
	price FixedRatePremium, totalSats btcutil.Amount,
	duration uint32) btcutil.Amount {

	// Calculate the deltas and update the tally.
	balanceDelta, makerFeesAccrued, executionFee := makerDelta(
		feeSchedule, price, totalSats, duration,
	)

	t.EndingBalance += balanceDelta
	t.TotalMakerFeesAccrued += makerFeesAccrued
	t.TotalExecutionFeesPaid += executionFee

	return executionFee
}

// takerDelta calculates an account's balance and fee difference for a single
// order where the account is involved on the taker side. It returns the
// balance delta, taker fees paid, and execution fee paid.
func takerDelta(feeSchedule FeeSchedule,
	price FixedRatePremium, totalSats btcutil.Amount,
	duration uint32) (btcutil.Amount, btcutil.Amount, btcutil.Amount) {

	// Calculate the premium based on the duration the capital will be
	// locked for.
	satsPremium := price.LumpSumPremium(totalSats, duration)

	// The premium must be paid by the taker.
	balanceDelta := -satsPremium
	takerFeesPaid := satsPremium

	// Finally, we'll subtract the paid execution fees from the balance.
	executionFee := executionFee(totalSats, feeSchedule)
	balanceDelta -= executionFee

	return balanceDelta, takerFeesPaid, executionFee
}

// CalcTakerDelta calculates an account's balance and fee difference for a
// single order where the account is involved on the taker side. It returns the
// execution fee that is collected by the auctioneer.
func (t *AccountTally) CalcTakerDelta(feeSchedule FeeSchedule,
	price FixedRatePremium, totalSats btcutil.Amount,
	duration uint32) btcutil.Amount {

	// Calculate the deltas and update the tally.
	balanceDelta, takerFeesPaid, executionFee := takerDelta(
		feeSchedule, price, totalSats, duration,
	)

	t.EndingBalance += balanceDelta
	t.TotalTakerFeesPaid += takerFeesPaid
	t.TotalExecutionFeesPaid += executionFee

	return executionFee
}

// ChainFees estimates the chain fees that need to be paid for the number of
// channels created for this account and subtracts that value from the ending
// balance.
func (t *AccountTally) ChainFees(feeRate chainfee.SatPerKWeight) {
	chainFeesDue := EstimateTraderFee(t.NumChansCreated, feeRate)
	t.EndingBalance -= chainFeesDue
}

// minNoDustAccountSize returns the minimum number of satoshis an account output
// value needs to be to not be considered a dust output. This is the cost of a
// spend TX at 1 sat/byte plus the minimum non-dust output size.
func minNoDustAccountSize() btcutil.Amount {
	// Calculate the minimum fee we would need to pay to coop close the
	// account to a P2WKH output.
	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(clmscript.MultiSigWitnessSize)
	weightEstimator.AddP2WKHOutput()
	minimumFee := chainfee.FeePerKwFloor.FeeForWeight(
		int64(weightEstimator.Weight()),
	)

	// After paying the fee, more than dust needs to remain, otherwise it
	// doesn't make sense to sweep the account.
	return minimumFee + dustLimitP2WPKH
}

// executionFee calculates the execution fee which is the base fee plus the
// execution fee which scales based on the order size.
func executionFee(amount btcutil.Amount, schedule FeeSchedule) btcutil.Amount {
	return schedule.BaseFee() + schedule.ExecutionFee(amount)
}
