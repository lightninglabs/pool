package order

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/terms"
	"github.com/stretchr/testify/require"
)

// TestOrderReservedValue checks orders' ReservedValue method returning the
// expected worst case value.
func TestOrderReservedValue(t *testing.T) {
	t.Parallel()

	simpleFeeSchedule := terms.NewLinearFeeSchedule(1, 100)

	type testCase struct {
		name  string
		order Order
	}
	testCases := []*testCase{
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
			name: "ask 10 units 5 min units match",
			order: &Ask{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10_000_000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    5,
				},
			},
		},
		{
			name: "bid 10 units 5 min units match",
			order: &Bid{
				Kit: Kit{
					State:            StateSubmitted,
					UnitsUnfulfilled: 10,
					FixedRate:        10_000_000,
					MaxBatchFeeRate:  1000,
					LeaseDuration:    144,
					MinUnitsMatch:    5,
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

	runTestCase := func(t *testing.T, tc *testCase, v account.Version) {
		// Count the worst case we will expect.
		var expValue btcutil.Amount

		switch o := tc.order.(type) {
		case *Bid:
			// Expect no reseved value in these states.
			if o.State.Archived() {
				break
			}

			// For bids the taker pays the most fees if the min
			// units get matched every block. There's an edge case
			// where if the units unfulfilled is not divisible by
			// the min units match, then the last match will consume
			// all the remaining units left.
			numBlocks := int(o.UnitsUnfulfilled / o.MinUnitsMatch)
			unitsRem := o.UnitsUnfulfilled % o.MinUnitsMatch
			lastMatch := o.MinUnitsMatch
			if unitsRem != 0 {
				lastMatch += unitsRem
			}
			for i := 0; i < numBlocks; i++ {
				amt := o.MinUnitsMatch.ToSatoshis()
				if i == numBlocks-1 {
					amt = lastMatch.ToSatoshis()
				}

				lumpSum := FixedRatePremium(o.FixedRate).
					LumpSumPremium(amt, o.LeaseDuration)
				exeFee := executionFee(amt, simpleFeeSchedule)
				chainFee := EstimateTraderFee(
					1, o.MaxBatchFeeRate, v,
				)

				// For bids the lump sum, chain fee and the
				// execution fee must be reserved.
				expValue += lumpSum + chainFee + exeFee
			}

		case *Ask:
			// Expect no reseved value in these states.
			if o.State.Archived() {
				break
			}

			// For asks the maker pays the most fees if min units
			// get matched every block. There's an edge case where
			// if the units unfulfilled is not divisible by the min
			// units match, then the last match will consume all the
			// remaining units left.
			numBlocks := int(o.UnitsUnfulfilled / o.MinUnitsMatch)
			unitsRem := o.UnitsUnfulfilled % o.MinUnitsMatch
			lastMatch := o.MinUnitsMatch
			if unitsRem != 0 {
				lastMatch += unitsRem
			}
			for i := 0; i < numBlocks; i++ {
				amt := o.MinUnitsMatch.ToSatoshis()
				if i == numBlocks-1 {
					amt = lastMatch.ToSatoshis()
				}

				// In the worst case, the maker will be paid
				// only one lump sum for a 144 block duration,
				// since that is the minimum duration.
				lumpSum := FixedRatePremium(o.FixedRate).
					LumpSumPremium(amt, 144)
				exeFee := executionFee(amt, simpleFeeSchedule)
				chainFee := EstimateTraderFee(
					1, o.MaxBatchFeeRate, v,
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

		val := tc.order.ReservedValue(simpleFeeSchedule, v)
		if val < 0 {
			t.Fatalf("reserved value cannot be "+
				"negative: %v", val)
		}
		if val != expValue {
			t.Fatalf("%s: expected reserved value "+
				"%v, got '%v'", tc.name, expValue, val)
		}
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name+"/version_0", func(t *testing.T) {
			runTestCase(t, tc, account.VersionInitialNoVersion)
		})
		t.Run(tc.name+"/version_1", func(t *testing.T) {
			runTestCase(t, tc, account.VersionTaprootEnabled)
		})
	}
}

var channelAnnouncementConstrainsTestCases = []struct {
	name               string
	askerConstrains    ChannelAnnouncementConstraints
	unannouncedChannel bool
	result             bool
}{{
	name:               "ask no preference bid announced channel",
	askerConstrains:    AnnouncementNoPreference,
	unannouncedChannel: false,
	result:             true,
}, {
	name:               "ask no preference bid unannounced channel",
	askerConstrains:    AnnouncementNoPreference,
	unannouncedChannel: true,
	result:             true,
}, {
	name:               "ask only announced bid announced channel",
	askerConstrains:    OnlyAnnounced,
	unannouncedChannel: false,
	result:             true,
}, {
	name:               "ask only announced bid unannounced channel",
	askerConstrains:    OnlyAnnounced,
	unannouncedChannel: true,
	result:             false,
}, {
	name:               "ask only unannounced bid announced channel",
	askerConstrains:    OnlyUnannounced,
	unannouncedChannel: false,
	result:             false,
}, {
	name:               "ask only unannounced bid unannounced channel",
	askerConstrains:    OnlyUnannounced,
	unannouncedChannel: true,
	result:             true,
}}

func TestChannelAnnouncementConstrainsCompatibility(t *testing.T) {
	for _, tc := range channelAnnouncementConstrainsTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := MatchAnnouncementConstraints(
				tc.askerConstrains, tc.unannouncedChannel,
			)

			require.Equal(t, tc.result, res)
		})
	}
}

var channelConfirmationConstrainsTestCases = []struct {
	name            string
	askerConstrains ChannelConfirmationConstraints
	zeroConfChannel bool
	result          bool
}{{
	name:            "ask no preference bid zero confirmed channel",
	askerConstrains: ConfirmationNoPreference,
	zeroConfChannel: false,
	result:          true,
}, {
	name:            "ask no preference bid zero conf channel",
	askerConstrains: ConfirmationNoPreference,
	zeroConfChannel: true,
	result:          true,
}, {
	name:            "ask only confirmed channels bid confirmed channel",
	askerConstrains: OnlyConfirmed,
	zeroConfChannel: false,
	result:          true,
}, {
	name:            "ask only confirmed bid zero conf channel",
	askerConstrains: OnlyConfirmed,
	zeroConfChannel: true,
	result:          false,
}, {
	name:            "ask only zero conf channels bid confirmed channel",
	askerConstrains: OnlyZeroConf,
	zeroConfChannel: false,
	result:          false,
}, {
	name:            "ask only zero conf channels bid zero conf channel",
	askerConstrains: OnlyZeroConf,
	zeroConfChannel: true,
	result:          true,
}}

func TestChannelConstrainsCompatibility(t *testing.T) {
	for _, tc := range channelConfirmationConstrainsTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := MatchZeroConfConstraints(
				tc.askerConstrains, tc.zeroConfChannel,
			)

			require.Equal(t, tc.result, res)
		})
	}
}
