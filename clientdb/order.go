package clientdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/coreos/bbolt"
	"github.com/lightninglabs/llm/order"
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
)

// orderCallback is a function type that is used to pass as a callback into the
// store's fetch functions to deliver the results to the caller.
type orderCallback func(nonce order.Nonce, rawOrder []byte) error

// SubmitOrder stores an order by using the orders's nonce as an identifier. If
// an order with the given nonce already exists in the store, ErrOrderExists is
// returned.
//
// NOTE: This is part of the Store interface.
func (db *DB) SubmitOrder(order order.Order) error {
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		err = fetchOrderTX(rootBucket, order.Nonce(), nil)
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
		err = SerializeOrder(order, &w)
		if err != nil {
			return err
		}
		return storeOrderTX(rootBucket, order.Nonce(), w.Bytes())
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
		return updateOrder(rootBucket, rootBucket, nonce, modifiers)
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
			err := updateOrder(
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
		callback = func(nonce order.Nonce, rawOrder []byte) error {
			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			return err
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
		callback = func(nonce order.Nonce, rawOrder []byte) error {
			r := bytes.NewReader(rawOrder)
			o, err := DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}
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
		if rootBucket.Get(nonce[:]) == nil {
			return ErrNoOrder
		}

		// Delete the whole order bucket with all of its sub buckets.
		return rootBucket.DeleteBucket(nonce[:])
	})
}

// storeOrderTX saves a byte serialized order in its specific sub bucket within
// the root orders bucket.
func storeOrderTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	orderBytes []byte) error {

	// From the root bucket, we'll make a new sub order bucket using
	// the order nonce.
	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	// With the order bucket created, we'll store the order itself.
	return orderBucket.Put(orderKey, orderBytes)
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
	return callback(nonce, orderBytes)
}

// updateOrder fetches the binary data of one order specified by its nonce from
// the src bucket, applies the modifiers, and stores it back into dst bucket.
func updateOrder(src, dst *bbolt.Bucket, nonce order.Nonce,
	modifiers []order.Modifier) error {

	var (
		o        order.Order
		err      error
		callback = func(nonce order.Nonce, rawOrder []byte) error {
			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			return err
		}
	)
	// Retrieve the order stored in the database.
	err = fetchOrderTX(src, nonce, callback)
	if err != nil {
		return err
	}

	// Apply the given modifications to it and store it back.
	for _, modifier := range modifiers {
		modifier(o.Details())
	}
	var w bytes.Buffer
	err = SerializeOrder(o, &w)
	if err != nil {
		return err
	}

	return storeOrderTX(dst, nonce, w.Bytes())
}

// SerializeOrder binary serializes an order to a writer using the common LN
// wire format.
func SerializeOrder(o order.Order, w io.Writer) error {
	kit := o.Details()

	// We don't have to deserialize the nonce as it's the sub bucket name.
	err := WriteElements(
		w, kit.Preimage, kit.Version, o.Type(), kit.State,
		kit.FixedRate, kit.Amt, kit.Units, kit.MultiSigKeyLocator,
		kit.FundingFeeRate, kit.AcctKey, kit.UnitsUnfulfilled,
	)
	if err != nil {
		return err
	}

	// Write order type specific fields.
	switch t := o.(type) {
	case *order.Ask:
		err := WriteElement(w, t.MaxDuration)
		if err != nil {
			return err
		}

	case *order.Bid:
		err := WriteElement(w, t.MinDuration)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown order type: %d", o.Type())
	}

	return nil
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
		&kit.FundingFeeRate, &kit.AcctKey, &kit.UnitsUnfulfilled,
	)
	if err != nil {
		return nil, err
	}

	// Now read the order type specific fields.
	switch orderType {
	case order.TypeAsk:
		ask := &order.Ask{Kit: *kit}
		err = ReadElement(r, &ask.MaxDuration)
		if err != nil {
			return nil, err
		}
		return ask, nil

	case order.TypeBid:
		bid := &order.Bid{Kit: *kit}
		err = ReadElement(r, &bid.MinDuration)
		if err != nil {
			return nil, err
		}
		return bid, nil

	default:
		return nil, fmt.Errorf("unknown order type: %d", orderType)
	}
}
