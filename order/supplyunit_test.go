package order_test

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/order"
	"github.com/stretchr/testify/require"
)

// TestRoundToNextSupplyUnit runs a quick check scenario to ensure the
// correctness of RoundToNextSupplyUnit.
func TestRoundToNextSupplyUnit(t *testing.T) {
	t.Parallel()

	f := func(sats btcutil.Amount) bool {
		units := order.RoundToNextSupplyUnit(sats)
		if sats%order.BaseSupplyUnit == 0 {
			return units == order.NewSupplyFromSats(sats)
		}
		return units == order.NewSupplyFromSats(sats)+1
	}

	cfg := &quick.Config{
		Values: func(v []reflect.Value, r *rand.Rand) {
			v[0] = reflect.ValueOf(btcutil.Amount(r.Int63()))
		},
	}

	require.NoError(t, quick.Check(f, cfg))
}
