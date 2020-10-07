package order

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/terms"
)

// TestOrderReservedValue checks orders' ReservedValue merhod returning the
// expected worst case value.
func TestOrderReservedValue(t *testing.T) {
	t.Parallel()

	simpleFeeSchedule := terms.NewLinearFeeSchedule(1, 100)

	testCases := []struct {
		name  string
		order Order
	}{
		{
			name: "bid 1 unit",
			order: &Bid{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 1,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "ask 1 unit",
			order: &Ask{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 1,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "bid 10 units",
			order: &Bid{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "ask 10 units",
			order: &Ask{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "cancelled order",
			order: &Ask{
				Kit: Kit{
					State:            StateCanceled,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "expired order",
			order: &Bid{
				Kit: Kit{
					State:            StateExpired,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "failed order",
			order: &Bid{
				Kit: Kit{
					State:            StateFailed,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "ask 10 units partially filled",
			order: &Ask{
				Kit: Kit{
					State:            StatePartiallyFilled,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "ask 10 units cleared",
			order: &Ask{
				Kit: Kit{
					State:            StateCleared,
					UnitsUnfulfilled: 10,
					FixedRate:        10000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "ask massive rate",
			order: &Ask{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10_000_000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    1,
				},
			},
		},
		{
			name: "ask 10 units 4 min units match",
			order: &Ask{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10_000_000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    4,
				},
			},
		},
		{
			name: "bid 10 units 4 min units match",
			order: &Bid{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10_000_000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    4,
				},
			},
		},
	}

	for i, tc := range testCases {
		tc := tc

		// Count the worst case we will expect.
		var expValue btcutil.Amount

		switch o := tc.order.(type) {
		case *Bid:
			// Expect no reseved value in these states.
			if o.State.Archived() {
				break
			}

			// For bids the taker pays the most fees if the min
			// units get matched every block.
			numBlocks := int(o.UnitsUnfulfilled / o.MinUnitsMatch)
			amt := o.MinUnitsMatch.ToSatoshis()
			for i := 0; i < numBlocks; i++ {
				lumpSum := FixedRatePremium(o.FixedRate).
					LumpSumPremium(amt, o.LeaseDuration)
				exeFee := executionFee(amt, simpleFeeSchedule)
				chainFee := EstimateTraderFee(
					1, o.MaxBatchFeeRate,
				)

				// For bids the lump sum, chain fee  and the
				// execution fee must be reserved.
				expValue += lumpSum + chainFee + exeFee
			}

		case *Ask:
			// Expect no reseved value in these states.
			if o.State.Archived() {
				break
			}

			// For asks the maker pays the most fees if min units
			// get matched every block.
			numBlocks := int(o.UnitsUnfulfilled / o.MinUnitsMatch)
			amt := o.MinUnitsMatch.ToSatoshis()
			for i := 0; i < numBlocks; i++ {
				// In the worst case, the maker will be paid
				// only one lump sum for a 144 block duration,
				// since that is the minimum duration.
				lumpSum := FixedRatePremium(o.FixedRate).
					LumpSumPremium(amt, 144)
				exeFee := executionFee(amt, simpleFeeSchedule)
				chainFee := EstimateTraderFee(
					1, o.MaxBatchFeeRate,
				)

				// For asks the amount itself, the chain fee
				// and the execution fee must be reserved,
				// while the lump sum the maker gets back.
				expValue += amt + chainFee + exeFee - lumpSum
			}

		default:
			t.Fatalf("unknown type %T", tc.order)
		}

		// We don't ever expect negative reserved values.
		if expValue < 0 {
			expValue = 0
		}

		// Check the value returned.
		i := i
		t.Run(tc.name, func(t *testing.T) {
			val := tc.order.ReservedValue(simpleFeeSchedule)
			if val < 0 {
				t.Fatalf("reserved value cannot be "+
					"negative: %v", val)
			}
			if val != expValue {
				t.Fatalf("test #%v: expected reserved value "+
					"%v, got '%v'", i, expValue, val)
			}
		})
	}
}
