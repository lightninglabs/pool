package clientdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/lightninglabs/pool/event"
	"github.com/lightninglabs/pool/order"
	"go.etcd.io/bbolt"
)

var (
	// ErrNoOrder is the error returned if no order with the given nonce
	// exists in the store.
	ErrNoOrder = errors.New("no order found")

	// ErrOrderExists is returned if an order is submitted that is already
	// known to the store.
	ErrOrderExists = errors.New("order with this nonce already exists")

	// ordersBucketKey is a bucket that contains all orders that are
	// currently pending or completed. This bucket is keyed by the nonce and
	// leads to a nested sub-bucket that houses information for that order.
	ordersBucketKey = []byte("orders")

	// orderKey is the key that stores the serialized order. It is nested
	// within the sub-bucket for each active order.
	//
	// path: ordersBucketKey -> orderBucket[nonce] -> orderKey
	orderKey = []byte("order")

	// orderTierKey is a key within the order bucket for an order. It
	// stores the min node tier for an order. It allows an order to specify
	// the required minimum node tier for matching and execution.
	orderTierKey = []byte("order-tier")

	// orderMinUnitsMatchKey is the key that stores the serialized order's
	// minimum units match. It is nested within the sub-bucket for each
	// active order.
	//
	// path: ordersBucketKey -> orderBucket[nonce] -> orderMinUnitsMatchKey
	orderMinUnitsMatchKey = []byte("order-min-units-match")
)

type extraOrderData struct {
	minNodeTier   order.NodeTier
	minUnitsMatch order.SupplyUnit
}

// orderCallback is a function type that is used to pass as a callback into the
// store's fetch functions to deliver the results to the caller. It also
// includes any auxiliary order information as additional arguments after the
// raw order bytes.
type orderCallback func(nonce order.Nonce, rawOrder []byte,
	extraData *extraOrderData) error

// SubmitOrder stores an order by using the orders's nonce as an identifier. If
// an order with the given nonce already exists in the store, ErrOrderExists is
// returned.
//
// NOTE: This is part of the Store interface.
func (db *DB) SubmitOrder(newOrder order.Order) error {
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		err = fetchOrderTX(rootBucket, newOrder.Nonce(), nil)
		switch err {
		// No error means there is an order with that nonce in the DB
		// already.
		case nil:
			return ErrOrderExists

		// This is what we want, no order with that nonce should be
		// known.
		case ErrNoOrder:

		// Surface any other error.
		default:
			return err
		}

		// Serialize and store the order now that we know it doesn't
		// exist yet.
		var w bytes.Buffer
		err = SerializeOrder(newOrder, &w)
		if err != nil {
			return err
		}

		// Create an event for the initial order and store it in the
		// order's event bucket but also in the global events under an
		// unique timestamp.
		evt := NewCreatedEvent(newOrder)
		err = storeOrderTX(
			rootBucket, newOrder.Nonce(), w.Bytes(), evt,
		)
		if err != nil {
			return err
		}
		err = storeOrderMinUnitsMatchTX(
			rootBucket, newOrder.Nonce(),
			newOrder.Details().MinUnitsMatch,
		)
		if err != nil {
			return err
		}

		// Finally, store the min node tier, but only if this is a Bid
		// order.
		if bidOrder, ok := newOrder.(*order.Bid); ok {
			return storeOrderMinNoderTierTX(
				rootBucket, newOrder.Nonce(),
				bidOrder.MinNodeTier,
			)
		}

		return nil
	})
}

// UpdateOrder updates an order in the database according to the given
// modifiers.
//
// NOTE: This is part of the Store interface.
func (db *DB) UpdateOrder(nonce order.Nonce, modifiers ...order.Modifier) error {
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		_, err = updateOrder(rootBucket, rootBucket, nonce, modifiers)
		return err
	})
}

// UpdateOrders atomically updates a list of orders in the database according to
// the given modifiers.
//
// NOTE: This is part of the Store interface.
func (db *DB) UpdateOrders(nonces []order.Nonce,
	modifiers [][]order.Modifier) error {

	if len(nonces) != len(modifiers) {
		return fmt.Errorf("invalid number of modifiers")
	}

	// Read and update the orders in one single transaction that they are
	// updated atomically.
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		for idx, nonce := range nonces {
			_, err := updateOrder(
				rootBucket, rootBucket, nonce, modifiers[idx],
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// GetOrder returns an order by looking up the nonce. If no order with that
// nonce exists in the store, ErrNoOrder is returned.
//
// NOTE: This is part of the Store interface.
func (db *DB) GetOrder(nonce order.Nonce) (order.Order, error) {
	var (
		o        order.Order
		err      error
		callback = func(nonce order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			// TODO(roasbeef): factory func to de-dup w/ all other
			// instances?
			if bidOrder, ok := o.(*order.Bid); ok {
				bidOrder.MinNodeTier = extraData.minNodeTier
			}
			o.Details().MinUnitsMatch = extraData.minUnitsMatch

			return nil
		}
	)
	err = db.View(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		return fetchOrderTX(rootBucket, nonce, callback)
	})
	return o, err
}

// GetOrders returns all orders that are currently known to the store.
//
// NOTE: This is part of the Store interface.
func (db *DB) GetOrders() ([]order.Order, error) {
	var (
		orders   []order.Order
		callback = func(nonce order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			r := bytes.NewReader(rawOrder)
			o, err := DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			if bidOrder, ok := o.(*order.Bid); ok {
				bidOrder.MinNodeTier = extraData.minNodeTier
			}
			o.Details().MinUnitsMatch = extraData.minUnitsMatch

			orders = append(orders, o)
			return nil
		}
	)
	err := db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main order bucket key.
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}

		// We'll now traverse the root bucket for all orders. The
		// primary key is the order nonce itself.
		return rootBucket.ForEach(func(nonceBytes, val []byte) error {
			// Only go into things that we know are sub-bucket keys.
			if val != nil {
				return nil
			}

			// Get the order from the bucket and pass it to the
			// caller.
			var nonce order.Nonce
			copy(nonce[:], nonceBytes)
			return fetchOrderTX(rootBucket, nonce, callback)
		})
	})
	if err != nil {
		return nil, err
	}
	return orders, nil
}

// DelOrder removes the order with the given nonce from the local store.
//
// NOTE: This is part of the Store interface.
func (db *DB) DelOrder(nonce order.Nonce) error {
	return db.Update(func(tx *bbolt.Tx) error {
		// First, we'll grab the root bucket that houses all of our
		// main orders.
		rootBucket := tx.Bucket(ordersBucketKey)
		if rootBucket == nil {
			return fmt.Errorf("root bucket not found")
		}

		// If the order doesn't exist, we're probably in an inconsistent
		// state and need to return a specific error.
		if rootBucket.Bucket(nonce[:]) == nil {
			return ErrNoOrder
		}

		// Delete the whole order bucket with all of its sub buckets.
		return rootBucket.DeleteBucket(nonce[:])
	})
}

// storeOrderTX saves a byte serialized order in its specific sub bucket within
// the root orders bucket.
func storeOrderTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	orderBytes []byte, evt event.Event) error {

	// From the root bucket, we'll make a new sub order bucket using
	// the order nonce.
	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	// Add the event to the global event store but also add a reference to
	// it in the order bucket.
	if err := storeEventTX(orderBucket, evt); err != nil {
		return err
	}

	// With the order bucket created, we'll store the order itself.
	return orderBucket.Put(orderKey, orderBytes)
}

// storeOrderMinNoderTierTX saves the currnet node tier for a given order to
// the proper bucket.
func storeOrderMinNoderTierTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	minNodeTier order.NodeTier) error {

	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := WriteElements(&b, minNodeTier); err != nil {
		return err
	}

	return orderBucket.Put(orderTierKey, b.Bytes())
}

// storeOrderMinUnitsMatchTX saves the order's minimum units match within the
// order's root bucket.
func storeOrderMinUnitsMatchTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	minUnitsMatch order.SupplyUnit) error {

	// From the root bucket, we'll make a new sub order bucket using
	// the order nonce.
	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	// With the order bucket created, we'll store the order's min units
	// match.
	var buf bytes.Buffer
	if err := WriteElement(&buf, minUnitsMatch); err != nil {
		return err
	}
	return orderBucket.Put(orderMinUnitsMatchKey, buf.Bytes())
}

// fetchOrderTX fetches the binary data of one order specified by its nonce from
// the root orders bucket.
func fetchOrderTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	callback orderCallback) error {

	// Get the main order bucket next.
	orderBucket := rootBucket.Bucket(nonce[:])
	if orderBucket == nil {
		return ErrNoOrder
	}

	// With the main order bucket obtained, we'll grab the raw order
	// bytes and pass them back to the caller.
	orderBytes := orderBucket.Get(orderKey)
	if orderBytes == nil {
		return fmt.Errorf("order bucket not found")
	}
	if callback == nil {
		return nil
	}

	var minNodeTier uint32
	nodeTierBytes := orderBucket.Get(orderTierKey)
	if nodeTierBytes != nil {
		err := ReadElements(
			bytes.NewReader(nodeTierBytes), &minNodeTier,
		)
		if err != nil {
			return err
		}
	}

	// Use a default of 1 if the field does not exist (possible for older
	// orders).
	minUnitsMatch := order.SupplyUnit(1)
	minUnitsMatchBytes := orderBucket.Get(orderMinUnitsMatchKey)
	if minUnitsMatchBytes != nil {
		err := ReadElements(
			bytes.NewReader(minUnitsMatchBytes), &minUnitsMatch,
		)
		if err != nil {
			return err
		}
	}

	return callback(nonce, orderBytes, &extraOrderData{
		minNodeTier:   order.NodeTier(minNodeTier),
		minUnitsMatch: minUnitsMatch,
	})
}

// updateOrder fetches the binary data of one order specified by its nonce from
// the orders bucket, applies the modifiers, and stores it back into dst bucket.
// The dst bucket can be a different bucket than the orders bucket if an order
// update should be written to a staging area instead of being applied to the
// original order directly. Do not use this function for applying the staged
// changes to the final bucket but use copyOrder instead.
func updateOrder(ordersBucket, dst *bbolt.Bucket, nonce order.Nonce,
	modifiers []order.Modifier) (order.Order, error) {

	var (
		o        order.Order
		err      error
		callback = func(nonce order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			if bidOrder, ok := o.(*order.Bid); ok {
				bidOrder.MinNodeTier = extraData.minNodeTier
			}
			o.Details().MinUnitsMatch = extraData.minUnitsMatch

			return nil
		}
	)

	// Retrieve the order stored in the database.
	err = fetchOrderTX(ordersBucket, nonce, callback)
	if err != nil {
		return nil, err
	}

	// Preserve the original state to detect a state change later.
	prevState := o.Details().State

	// Apply the given modifications to it and store it back.
	for _, modifier := range modifiers {
		modifier(o.Details())
	}
	var w bytes.Buffer
	err = SerializeOrder(o, &w)
	if err != nil {
		return nil, err
	}

	// Create an event for the update now. We always store the event
	// reference to the order in the main order bucket, even if the
	// destination bucket is the pending orders bucket.
	evt := NewUpdatedEvent(prevState, o)
	orderBucket := ordersBucket.Bucket(nonce[:])
	if orderBucket == nil {
		return nil, ErrNoOrder
	}

	// Add the event to the global event store but also add a reference to
	// it in the order's event reference sub bucket.
	if err := storeEventTX(orderBucket, evt); err != nil {
		return nil, err
	}

	// Store the order to the destination bucket now, skipping the event as
	// we've already written that to the source bucket.
	if err := storeOrderTX(dst, nonce, w.Bytes(), nil); err != nil {
		return nil, err
	}
	err = storeOrderMinUnitsMatchTX(dst, nonce, o.Details().MinUnitsMatch)
	if err != nil {
		return nil, err
	}

	if bidOrder, ok := o.(*order.Bid); ok {
		err = storeOrderMinNoderTierTX(dst, nonce, bidOrder.MinNodeTier)
		if err != nil {
			return nil, err
		}
	}

	return o, nil
}

// copyOrder copies a single order from the source to destination bucket. No
// event references are copied, only the order itself.
func copyOrder(src, dst *bbolt.Bucket, nonce order.Nonce) error {
	var (
		o             order.Order
		orderBytes    []byte
		nodeTier      order.NodeTier
		minUnitsMatch order.SupplyUnit
		err           error
		callback      = func(_ order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			orderBytes = rawOrder

			// In order to know if we need the node tier at all,
			// we'll go ahead and decode fully this order in
			// question.
			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}
			nodeTier = extraData.minNodeTier
			minUnitsMatch = extraData.minUnitsMatch
			return nil
		}
	)

	// Retrieve the order stored in the source bucket.
	err = fetchOrderTX(src, nonce, callback)
	if err != nil {
		return err
	}

	// Store it in the new bucket.
	if err := storeOrderTX(dst, nonce, orderBytes, nil); err != nil {
		return err
	}
	if _, ok := o.(*order.Bid); ok {
		err := storeOrderMinNoderTierTX(dst, nonce, nodeTier)
		if err != nil {
			return err
		}
	}
	return storeOrderMinUnitsMatchTX(dst, nonce, minUnitsMatch)
}

// SerializeOrder binary serializes an order to a writer using the common LN
// wire format.
func SerializeOrder(o order.Order, w io.Writer) error {
	kit := o.Details()

	// We don't have to deserialize the nonce as it's the sub bucket name.
	return WriteElements(
		w, kit.Preimage, kit.Version, o.Type(), kit.State,
		kit.FixedRate, kit.Amt, kit.Units, kit.MultiSigKeyLocator,
		kit.MaxBatchFeeRate, kit.AcctKey, kit.UnitsUnfulfilled,
		kit.LeaseDuration,
	)
}

// DeserializeOrder deserializes an order from the binary LN wire format.
func DeserializeOrder(nonce order.Nonce, r io.Reader) (
	order.Order, error) {

	var (
		kit       = order.NewKit(nonce)
		orderType order.Type
	)

	// We don't serialize the nonce as it's part of the bucket name already.
	err := ReadElements(
		r, &kit.Preimage, &kit.Version, &orderType, &kit.State,
		&kit.FixedRate, &kit.Amt, &kit.Units, &kit.MultiSigKeyLocator,
		&kit.MaxBatchFeeRate, &kit.AcctKey, &kit.UnitsUnfulfilled,
		&kit.LeaseDuration,
	)
	if err != nil {
		return nil, err
	}

	// Now read the order type specific fields.
	switch orderType {
	case order.TypeAsk:
		return &order.Ask{
			Kit: *kit,
		}, nil

	case order.TypeBid:
		return &order.Bid{
			Kit: *kit,
		}, nil
	default:
		return nil, fmt.Errorf("unknown order type: %d", orderType)
	}
}
