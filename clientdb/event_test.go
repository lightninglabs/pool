package clientdb

import (
	"fmt"
	"testing"
	"time"

	"github.com/lightninglabs/pool/event"
	"github.com/lightninglabs/pool/order"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

// TestOrderEvents tests that storing and updating orders creates the correct
// events in the main event bucket but also the references in the order's
// bucket.
func TestOrderEvents(t *testing.T) {
	t.Parallel()

	store, cleanup := newTestDB(t)
	defer cleanup()

	// Store two dummy orders that we are going to update later.
	o1 := &order.Bid{
		Kit: *dummyOrder(500000, 1337),
	}
	err := store.SubmitOrder(o1)
	require.NoError(t, err)
	o2 := &order.Ask{
		Kit: *dummyOrder(500000, 1337),
	}
	err = store.SubmitOrder(o2)
	require.NoError(t, err)

	assertOrderCreateEvent(t, store, o1.Nonce())
	assertOrderCreateEvent(t, store, o2.Nonce())
	assertOrderStateEvents(t, store, o1.Nonce(), []order.State{})
	assertOrderStateEvents(t, store, o2.Nonce(), []order.State{})

	// Update the state of the first order and check that it is persisted.
	err = store.UpdateOrder(
		o1.Nonce(), order.StateModifier(order.StatePartiallyFilled),
	)
	require.NoError(t, err)
	storedOrder, err := store.GetOrder(o1.Nonce())
	require.NoError(t, err)
	require.Equal(
		t, order.StatePartiallyFilled, storedOrder.Details().State,
	)

	assertOrderStateEvents(t, store, o1.Nonce(), []order.State{
		order.StatePartiallyFilled,
	})

	// Bulk update the state of both orders and check that they are
	// persisted correctly.
	stateModifier := order.StateModifier(order.StateCleared)
	err = store.UpdateOrders(
		[]order.Nonce{o1.Nonce(), o2.Nonce()},
		[][]order.Modifier{{stateModifier}, {stateModifier}},
	)
	require.NoError(t, err)
	allOrders, err := store.GetOrders()
	require.NoError(t, err)
	require.Len(t, allOrders, 2)
	for _, o := range allOrders {
		require.Equal(t, order.StateCleared, o.Details().State)
	}

	assertOrderStateEvents(t, store, o1.Nonce(), []order.State{
		order.StatePartiallyFilled, order.StateCleared,
	})
	assertOrderStateEvents(t, store, o2.Nonce(), []order.State{
		order.StateCleared,
	})
}

func assertOrderStateEvents(t *testing.T, store *DB, o order.Nonce,
	expectedStates []order.State) {

	t.Helper()

	// Read all order events.
	orderEventTimestamps, err := getOrderEventTimestamps(
		store, o, event.TypeOrderStateChange,
	)
	require.NoError(t, err)
	require.Equal(t, len(expectedStates), len(orderEventTimestamps))

	// Now that we know the timestamps, we can go ahead and read the events
	// themselves.
	events, err := store.GetEvents(orderEventTimestamps)
	require.NoError(t, err)

	require.NoError(t, err)
	require.Equal(t, len(expectedStates), len(events))

	// Make sure we got the actual event changes in the correct order.
	for idx, evt := range events {
		orderStateEvent, ok := evt.(*UpdatedEvent)
		require.True(t, ok)
		require.Equal(t, expectedStates[idx], orderStateEvent.NewState)
	}
}

func assertOrderCreateEvent(t *testing.T, store *DB, o order.Nonce) {
	t.Helper()

	// Read all order events.
	orderEventTimestamps, err := getOrderEventTimestamps(
		store, o, event.TypeOrderCreated,
	)
	require.NoError(t, err)
	require.Equal(t, 1, len(orderEventTimestamps))
}

func getOrderEventTimestamps(store *DB, o order.Nonce, evtType event.Type) (
	map[time.Time]struct{}, error) {

	orderEventTimestamps := make(map[time.Time]struct{})
	err := store.DB.View(func(tx *bbolt.Tx) error {
		ordersBucket := tx.Bucket(ordersBucketKey)
		orderBucket := ordersBucket.Bucket(o[:])
		eventBucket := orderBucket.Bucket(eventRefSubBucket)
		return eventBucket.ForEach(func(k, v []byte) error {
			// Only look at keys with correct length.
			if len(k) != event.TimestampLength {
				return nil
			}

			// Assert there are no corrupt values, in the reference
			// key we only store the event's type, not the value
			// itself.
			if len(v) != 1 {
				return fmt.Errorf("unexpected timestamp "+
					"reference value length: %d", len(v))
			}

			// We filter by the requested order type.
			if event.Type(v[0]) != evtType {
				return nil
			}

			ts := time.Unix(0, int64(byteOrder.Uint64(k)))
			orderEventTimestamps[ts] = struct{}{}

			return nil
		})
	})
	return orderEventTimestamps, err
}
