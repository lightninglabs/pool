package order

import "github.com/btcsuite/btcutil"

var (
	// FeeRateTotalParts defines the granularity of the fee rate.
	// Throughout the codebase, we'll use fix based arithmetic to compute
	// fees.
	FeeRateTotalParts = 1e6
)

// CalcFee calculates the absolute fee in satoshis from the amount, the
// specified fee rate in parts per million and the minimum duration.
func CalcFee(amt btcutil.Amount, fixedRate, minDuration uint32) btcutil.Amount {
	// TODO(guggero): take into account the min duration
	return amt * btcutil.Amount(fixedRate) /
		btcutil.Amount(FeeRateTotalParts)
}
