package order

import "github.com/btcsuite/btcutil"

var (
	// FeeRateTotalParts defines the granularity of the fee rate.
	// Throughout the codebase, we'll use fix based arithmetic to compute
	// fees.
	FeeRateTotalParts = 1e6
)

// PerBlockPremium calculates the absolute premium in satoshis for a one block
// duration from the amount and the specified fee rate in parts per million.
func PerBlockPremium(amt btcutil.Amount, fixedRate uint32) btcutil.Amount {
	return amt * btcutil.Amount(fixedRate) /
		btcutil.Amount(FeeRateTotalParts)
}
