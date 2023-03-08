package order

import (
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	// FeeRateTotalParts defines the granularity of the fixed rate used to
	// compute the per-block interest rate.  Throughout the codebase, we'll
	// use fix based arithmetic to compute fees.
	FeeRateTotalParts = 1e9

	// dustLimitP2WPKH is the minimum size of a P2WPKH output to not be
	// considered dust.
	dustLimitP2WPKH = lnwallet.DustLimitForSize(
		input.P2WPKHSize,
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

// PerBlockPremium calculates the absolute premium in fractions of satoshis for
// a one block duration from the amount and the specified fee rate in parts per
// billion.
func PerBlockPremium(amt btcutil.Amount, fixedRate uint32) float64 {
	return float64(amt) * float64(fixedRate) / FeeRateTotalParts
}

// EstimateTraderFee calculates the chain fees a trader has to pay for their
// part of a batch transaction. The more outputs a trader creates (channels),
// the higher fee they will pay.
func EstimateTraderFee(numTraderChans uint32, feeRate chainfee.SatPerKWeight,
	accountVersion account.Version) btcutil.Amount {

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
	switch accountVersion {
	case account.VersionTaprootEnabled, account.VersionMuSig2V100RC2:
		weightEstimate += poolscript.TaprootMultiSigWitnessSize

	default:
		weightEstimate += poolscript.MultiSigWitnessSize
	}

	return feeRate.FeeForWeight(weightEstimate)
}

// Quote is a struct holding the result of an order quote calculation.
type Quote struct {
	// TotalPremium is the total order premium in satoshis for filling the
	// entire order.
	TotalPremium btcutil.Amount

	// RatePerBlock is the fixed order rate expressed as a fraction instead
	// of parts per billion.
	RatePerBlock float64

	// TotalExecutionFee is the total execution fee in satoshis that needs
	// to be paid to the auctioneer for executing the entire order.
	TotalExecutionFee btcutil.Amount

	// WorstCaseChainFee is the chain fees that have to be paid in the worst
	// case scenario where fees spike up to the given max batch fee rate and
	// the order is executed in the maximum parts possible (amount divided
	// by minimum channel amount).
	WorstCaseChainFee btcutil.Amount
}

// NewQuote returns a new quote for an order with the given parameters.
func NewQuote(amt, minChanAmt btcutil.Amount, rate FixedRatePremium,
	leaseDuration uint32, maxBatchFeeRate chainfee.SatPerKWeight,
	schedule terms.FeeSchedule) *Quote {

	exeFee := schedule.BaseFee() + schedule.ExecutionFee(amt)

	maxNumMatches := amt / minChanAmt

	// For an order quote we always return the worst case fees, which means
	// with a legacy account.
	chainFee := maxNumMatches * EstimateTraderFee(
		1, maxBatchFeeRate, account.VersionInitialNoVersion,
	)

	return &Quote{
		TotalPremium:      rate.LumpSumPremium(amt, leaseDuration),
		RatePerBlock:      float64(rate) / FeeRateTotalParts,
		TotalExecutionFee: exeFee,
		WorstCaseChainFee: chainFee,
	}
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
func makerDelta(feeSchedule terms.FeeSchedule, price FixedRatePremium,
	makerAmt, baseAmt btcutil.Amount, duration uint32) (btcutil.Amount,
	btcutil.Amount, btcutil.Amount) {

	// First, we'll need to subtract the total matched amount (the
	// channel size), from the balance of the maker.
	balanceDelta := -makerAmt

	// Calculate the premium based on the duration the capital will be
	// locked for.
	satsPremium := price.LumpSumPremium(baseAmt, duration)

	// The premium is paid to the maker.
	balanceDelta += satsPremium
	makerFeesAccrued := satsPremium

	// Finally, we'll subtract the paid execution fees from the balance.
	executionFee := executionFee(makerAmt, feeSchedule)
	balanceDelta -= executionFee

	return balanceDelta, makerFeesAccrued, executionFee
}

// CalcMakerDelta calculates an account's balance and fee difference for a
// single order where the account is involved on the maker side. It returns the
// execution fee that is collected by the auctioneer.
func (t *AccountTally) CalcMakerDelta(feeSchedule terms.FeeSchedule,
	price FixedRatePremium, makerAmt, premiumAmt btcutil.Amount,
	duration uint32) btcutil.Amount {

	// Calculate the deltas and update the tally.
	balanceDelta, makerFeesAccrued, executionFee := makerDelta(
		feeSchedule, price, makerAmt, premiumAmt, duration,
	)

	t.EndingBalance += balanceDelta
	t.TotalMakerFeesAccrued += makerFeesAccrued
	t.TotalExecutionFeesPaid += executionFee

	return executionFee
}

// takerDelta calculates an account's balance and fee difference for a single
// order where the account is involved on the taker side. It returns the
// balance delta, taker fees paid, and execution fee paid.
func takerDelta(feeSchedule terms.FeeSchedule,
	price FixedRatePremium, baseAmt, takerAmt btcutil.Amount,
	duration uint32) (btcutil.Amount, btcutil.Amount, btcutil.Amount) {

	// Calculate the premium based on the duration the capital will be
	// locked for.
	satsPremium := price.LumpSumPremium(baseAmt, duration)

	// The premium must be paid by the taker.
	balanceDelta := -satsPremium
	takerFeesPaid := satsPremium

	// If there is a self channel balance, it must also be paid out of the
	// taker account.
	balanceDelta -= takerAmt

	// Finally, we'll subtract the paid execution fees from the balance.
	executionFee := executionFee(baseAmt, feeSchedule)
	balanceDelta -= executionFee

	return balanceDelta, takerFeesPaid, executionFee
}

// CalcTakerDelta calculates an account's balance and fee difference for a
// single order where the account is involved on the taker side. It returns the
// execution fee that is collected by the auctioneer.
func (t *AccountTally) CalcTakerDelta(feeSchedule terms.FeeSchedule,
	price FixedRatePremium, takerAmt, premiumAmt btcutil.Amount,
	duration uint32) btcutil.Amount {

	// Calculate the deltas and update the tally.
	balanceDelta, takerFeesPaid, executionFee := takerDelta(
		feeSchedule, price, premiumAmt, takerAmt, duration,
	)

	t.EndingBalance += balanceDelta
	t.TotalTakerFeesPaid += takerFeesPaid
	t.TotalExecutionFeesPaid += executionFee

	return executionFee
}

// ChainFees estimates the chain fees that need to be paid for the number of
// channels created for this account and subtracts that value from the ending
// balance.
func (t *AccountTally) ChainFees(feeRate chainfee.SatPerKWeight,
	accountVersion account.Version) {

	chainFeesDue := EstimateTraderFee(
		t.NumChansCreated, feeRate, accountVersion,
	)
	t.EndingBalance -= chainFeesDue
}

// minNoDustAccountSize returns the minimum number of satoshis an account output
// value needs to be to not be considered a dust output. This is the cost of a
// spend TX at 1 sat/byte plus the minimum non-dust output size.
func minNoDustAccountSize() btcutil.Amount {
	// Calculate the minimum fee we would need to pay to coop close the
	// account to a P2WKH output.
	var weightEstimator input.TxWeightEstimator
	weightEstimator.AddWitnessInput(poolscript.MultiSigWitnessSize)
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
func executionFee(amount btcutil.Amount,
	schedule terms.FeeSchedule) btcutil.Amount {

	return schedule.BaseFee() + schedule.ExecutionFee(amount)
}
