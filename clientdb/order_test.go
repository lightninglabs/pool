package clientdb

import (
	"crypto/rand"
	"fmt"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

var submitOrderTestCases = []struct {
	name     string
	getOrder func() order.Order
}{{
	name: "submit ask successfully",
	getOrder: func() order.Order {
		// Store a dummy order and see if we can retrieve it again.
		o := &order.Ask{
			Kit:                     *dummyOrder(500000, 1337),
			AnnouncementConstraints: order.OnlyUnannounced,
			ConfirmationConstraints: order.OnlyZeroConf,
		}
		o.Details().MinUnitsMatch = 10
		o.Details().ChannelType = order.ChannelTypeScriptEnforced
		o.Details().AuctionType = order.BTCOutboundLiquidity

		// It is not possible for an order to have AllowedNodeIDs and
		// NotAllowedNodeIDs at the same time but we want to test
		// serialization/deserialization here.
		o.Details().AllowedNodeIDs = [][33]byte{
			{1, 2, 3}, {2, 3, 4}, {3, 4, 5},
		}
		o.Details().NotAllowedNodeIDs = [][33]byte{{4, 5, 6}, {5, 6, 7}}
		o.Details().IsPublic = true

		return o
	},
}, {
	name: "submit bid successfully",
	getOrder: func() order.Order {
		// Store a dummy order and see if we can retrieve it again.
		o := &order.Bid{
			Kit:             *dummyOrder(500000, 1337),
			MinNodeTier:     2,
			SelfChanBalance: 123,
			SidecarTicket: &sidecar.Ticket{
				ID:    [8]byte{11, 22, 33, 44, 55, 66, 77},
				State: sidecar.StateRegistered,
				Offer: sidecar.Offer{
					Capacity:            1000000,
					PushAmt:             200000,
					LeaseDurationBlocks: 2016,
				},
				Recipient: &sidecar.Recipient{
					MultiSigPubKey:   testTraderKey,
					MultiSigKeyIndex: 7,
				},
			},
			UnannouncedChannel: true,
			ZeroConfChannel:    true,
		}
		o.Details().MinUnitsMatch = 10
		o.Details().ChannelType = order.ChannelTypeScriptEnforced

		// It is not possible for an order to have AllowedNodeIDs and
		// NotAllowedNodeIDs at the same time but we want to test
		// serialization/deserialization here.
		o.Details().AllowedNodeIDs = [][33]byte{
			{1, 2, 3}, {2, 3, 4}, {3, 4, 5},
		}
		o.Details().NotAllowedNodeIDs = [][33]byte{{4, 5, 6}, {5, 6, 7}}
		return o
	},
}}

// TestSubmitOrder tests that orders can be stored and retrieved correctly.
func TestSubmitOrder(t *testing.T) {
	for _, tc := range submitOrderTestCases {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, cleanup := newTestDB(t)
			defer cleanup()

			o := tc.getOrder()

			err := store.SubmitOrder(o)
			require.NoError(t, err)

			storedOrder, err := store.GetOrder(o.Nonce())
			require.NoError(t, err)
			require.Equal(t, o, storedOrder)

			// Check that we got the correct type back.
			require.Equal(t, storedOrder.Type(), o.Type())

			// Get all orders and check that we get the same as when
			// querying a specific one.
			allOrders, err := store.GetOrders()
			require.NoError(t, err)
			require.Len(t, allOrders, 1)
			require.True(
				t, reflect.DeepEqual(o, allOrders[0]),
				"expected order: %v\ngot: %v", spew.Sdump(o),
				spew.Sdump(allOrders[0]),
			)
		})
	}
}

// TestUpdateOrders tests that orders can be updated correctly.
func TestUpdateOrders(t *testing.T) {
	t.Parallel()

	store, cleanup := newTestDB(t)
	defer cleanup()

	// Store two dummy orders that we are going to update later.
	o1 := &order.Bid{
		Kit:         *dummyOrder(500000, 1337),
		MinNodeTier: 3,
	}
	o1.Details().MinUnitsMatch = 10
	o1.Details().ChannelType = order.ChannelTypeScriptEnforced
	err := store.SubmitOrder(o1)
	if err != nil {
		t.Fatalf("unable to store order: %v", err)
	}
	o2 := &order.Ask{
		Kit: *dummyOrder(500000, 1337),
	}
	o2.Details().ChannelType = order.ChannelTypeScriptEnforced
	err = store.SubmitOrder(o2)
	if err != nil {
		t.Fatalf("unable to store order: %v", err)
	}

	// Update the state of the first order and check that it is persisted.
	err = store.UpdateOrder(
		o1.Nonce(), order.StateModifier(order.StatePartiallyFilled),
	)
	if err != nil {
		t.Fatalf("unable to update order: %v", err)
	}
	storedOrder, err := store.GetOrder(o1.Nonce())
	if err != nil {
		t.Fatalf("unable to retrieve order: %v", err)
	}
	if storedOrder.Details().State != order.StatePartiallyFilled {
		t.Fatalf("unexpected order state. got %d expected %d",
			storedOrder.Details().State, order.StatePartiallyFilled)
	}

	// Bulk update the state of both orders and check that they are
	// persisted correctly.
	stateModifier := order.StateModifier(order.StateCleared)
	err = store.UpdateOrders(
		[]order.Nonce{o1.Nonce(), o2.Nonce()},
		[][]order.Modifier{{stateModifier}, {stateModifier}},
	)
	if err != nil {
		t.Fatalf("unable to update orders: %v", err)
	}
	allOrders, err := store.GetOrders()
	if err != nil {
		t.Fatalf("unable to get all orders: %v", err)
	}
	if len(allOrders) != 2 {
		t.Fatalf("unexpected number of orders. got %d expected %d",
			len(allOrders), 2)
	}
	for _, o := range allOrders {
		if o.Details().State != order.StateCleared {
			t.Fatalf("unexpected order state. got %d expected %d",
				o.Details().State, order.StateCleared)
		}
	}
}

func dummyOrder(amt btcutil.Amount, leaseDuration uint32) *order.Kit {
	var testPreimage lntypes.Preimage
	if _, err := rand.Read(testPreimage[:]); err != nil {
		panic(fmt.Sprintf("could not create private key: %v", err))
	}
	kit := order.NewKitWithPreimage(testPreimage)
	kit.AuctionType = order.BTCInboundLiquidity
	kit.Version = order.VersionLeaseDurationBuckets
	kit.State = order.StateExecuted
	kit.FixedRate = 21
	kit.Amt = amt
	kit.MultiSigKeyLocator = keychain.KeyLocator{
		Family: 123,
		Index:  345,
	}
	kit.MaxBatchFeeRate = chainfee.FeePerKwFloor
	copy(kit.AcctKey[:], testTraderKey.SerializeCompressed())
	kit.UnitsUnfulfilled = 741
	kit.LeaseDuration = leaseDuration
	return kit
}
