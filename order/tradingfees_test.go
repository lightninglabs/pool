package order

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

// TestNewQuote makes sure the order quote calculations are correct.
func TestNewQuote(t *testing.T) {
	testCases := []struct {
		amt             btcutil.Amount
		minChanAmt      btcutil.Amount
		rate            FixedRatePremium
		leaseDuration   uint32
		maxBatchFeeRate chainfee.SatPerKWeight
		schedule        terms.FeeSchedule
		result          *Quote
	}{{
		amt:             1_000_000,
		minChanAmt:      100_000,
		rate:            1,
		leaseDuration:   1,
		maxBatchFeeRate: chainfee.FeePerKwFloor,
		schedule:        terms.NewLinearFeeSchedule(1, 1),
		result: &Quote{
			TotalPremium:      0,
			RatePerBlock:      0.000000001,
			TotalExecutionFee: 2,
			WorstCaseChainFee: 1650,
		},
	}, {
		amt:             5_000_000,
		minChanAmt:      200_000,
		rate:            14880,
		leaseDuration:   2016,
		maxBatchFeeRate: chainfee.SatPerKVByte(100_000).FeePerKWeight(),
		schedule:        terms.NewLinearFeeSchedule(1, 1000),
		result: &Quote{
			TotalPremium:      149990,
			RatePerBlock:      0.00001488,
			TotalExecutionFee: 5001,
			WorstCaseChainFee: 408125,
		},
	}, {
		amt:             10_000_000,
		minChanAmt:      10_000_000,
		rate:            12444,
		leaseDuration:   8036,
		maxBatchFeeRate: chainfee.FeePerKwFloor,
		schedule:        terms.NewLinearFeeSchedule(1, 1000),
		result: &Quote{
			TotalPremium:      999999,
			RatePerBlock:      0.000012444,
			TotalExecutionFee: 10001,
			WorstCaseChainFee: 165,
		},
	}, {
		amt:             10_000_000,
		minChanAmt:      1_000_000,
		rate:            49603,
		leaseDuration:   2016,
		maxBatchFeeRate: chainfee.SatPerKVByte(200_000).FeePerKWeight(),
		schedule:        terms.NewLinearFeeSchedule(1, 1000),
		result: &Quote{
			TotalPremium:      999996,
			RatePerBlock:      0.000049603,
			TotalExecutionFee: 10001,
			WorstCaseChainFee: 326500,
		},
	}}

	for _, tc := range testCases {

		q := NewQuote(
			tc.amt, tc.minChanAmt, tc.rate, tc.leaseDuration,
			tc.maxBatchFeeRate, tc.schedule,
		)
		require.Equal(t, tc.result, q)
	}
}
