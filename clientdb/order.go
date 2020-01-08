package clientdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/coreos/bbolt"
	"github.com/lightninglabs/agora/client/order"
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
	err := db.fetchOrder(ordersBucketKey, order.Nonce(), nil)
	switch err {
	// No error means there is an order with that nonce in the DB already.
	case nil:
		return ErrOrderExists

	// This is what we want, no order with that nonce should be known.
	case ErrNoOrder:

	// Surface any other error.
	default:
		return err
	}

	// Serialize and store the order now that we know it doesn't exist yet.
	var w bytes.Buffer
	err = SerializeOrder(order, &w)
	if err != nil {
		return err
	}
	return db.storeOrder(ordersBucketKey, order.Nonce(), w.Bytes())
}

// UpdateOrder updates an order in the database according to the given
// modifiers.
//
// NOTE: This is part of the Store interface.
func (db *DB) UpdateOrder(nonce order.Nonce,
	modifiers ...order.Modifier) error {

	// Retrieve the order stored in the database.
	dbOrder, err := db.GetOrder(nonce)
	if err != nil {
		return err
	}

	// Apply the given modifications to it and store it back.
	for _, modifier := range modifiers {
		modifier(dbOrder.Details())
	}
	var w bytes.Buffer
	err = SerializeOrder(dbOrder, &w)
	if err != nil {
		return err
	}
	return db.storeOrder(ordersBucketKey, nonce, w.Bytes())
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

	dbOrders := make([][]byte, len(nonces))
	for idx, nonce := range nonces {
		// Read the current version from the DB and apply the
		// modifications to it.
		dbOrder, err := db.GetOrder(nonce)
		if err != nil {
			return err
		}
		for _, modifier := range modifiers[idx] {
			modifier(dbOrder.Details())
		}

		// Serialize it back into binary form.
		var w bytes.Buffer
		err = SerializeOrder(dbOrder, &w)
		if err != nil {
			return err
		}
		dbOrders[idx] = w.Bytes()
	}

	// Write the orders in one single transaction that they are updated
	// atomically.
	return db.storeOrders(ordersBucketKey, nonces, dbOrders)
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
	err = db.fetchOrder(ordersBucketKey, nonce, callback)
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
	err := db.fetchOrders(ordersBucketKey, callback)
	if err != nil {
		return nil, err
	}
	return orders, nil
}

// DelOrder removes the order with the given nonce from the local store.
//
// NOTE: This is part of the Store interface.
func (db *DB) DelOrder(nonce order.Nonce) error {
	return db.removeOrder(ordersBucketKey, nonce)
}

// storeOrder saves a byte serialized order in its specific sub bucket.
// Depending on the type, the path is:
// askBucket/bidBucket -> orderBucket[nonce] -> orderKey -> order
func (db *DB) storeOrder(bucketKey []byte, nonce order.Nonce,
	orderBytes []byte) error {

	return db.Update(func(tx *bbolt.Tx) error {
		// First, we'll grab the root bucket that houses all of our
		// main orders.
		rootBucket, err := tx.CreateBucketIfNotExists(
			bucketKey,
		)
		if err != nil {
			return err
		}

		// From the root bucket, we'll make a new sub order bucket using
		// the order nonce.
		orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
		if err != nil {
			return err
		}

		// With the order bucket created, we'll store the order itself.
		return orderBucket.Put(orderKey, orderBytes)
	})
}

// storeOrders saves multiple byte serialized orders in their specific sub
// bucket. It does so atomically, meaning that either all orders are updated or
// none are (in case of an error) by rolling back the transaction.
// Depending on the type, the storage path for the individual orders is:
// askBucket/bidBucket -> orderBucket[nonce] -> orderKey -> order
func (db *DB) storeOrders(bucketKey []byte, nonces []order.Nonce,
	orderBytes [][]byte) error {

	return db.Update(func(tx *bbolt.Tx) error {
		// First, we'll grab the root bucket that houses all of our
		// main orders.
		rootBucket, err := tx.CreateBucketIfNotExists(
			bucketKey,
		)
		if err != nil {
			return err
		}

		for idx, nonce := range nonces {
			// From the root bucket, we'll make a new sub order
			// bucket using the order nonce.
			orderBucket, err := rootBucket.CreateBucketIfNotExists(
				nonce[:],
			)
			if err != nil {
				return err
			}

			// With the order bucket created, we'll store the order
			// itself.
			err = orderBucket.Put(orderKey, orderBytes[idx])
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// fetchOrder fetches the binary data of one order specified by its order type
// bucket key and nonce.
func (db *DB) fetchOrder(bucketKey []byte, nonce order.Nonce,
	callback orderCallback) error {

	return db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main order bucket key.
		rootBucket := tx.Bucket(bucketKey)
		if rootBucket == nil {
			return fmt.Errorf("bucket %v does not exist", bucketKey)
		}

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
	})
}

// fetchOrders retrieves all orders in a specific order type bucket.
func (db *DB) fetchOrders(bucketKey []byte,
	callback orderCallback) error {

	return db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main order bucket key.
		rootBucket := tx.Bucket(bucketKey)
		if rootBucket == nil {
			return fmt.Errorf("bucket %v does not exist", bucketKey)
		}

		// We'll now traverse the root bucket for all orders. The
		// primary key is the order nonce itself.
		return rootBucket.ForEach(func(nonceBytes, val []byte) error {
			// Only go into things that we know are sub-bucket keys.
			if val != nil {
				return nil
			}

			// Get the main order bucket next.
			orderBucket := rootBucket.Bucket(nonceBytes)
			if orderBucket == nil {
				return fmt.Errorf("bucket for order %v does "+
					"not exist", nonceBytes)
			}

			// With the main order bucket obtained, we'll grab the
			// raw order bytes and pass them back to the caller.
			orderBytes := orderBucket.Get(orderKey)
			if orderBytes == nil {
				return fmt.Errorf("order bucket not found")
			}
			var nonce order.Nonce
			copy(nonce[:], nonceBytes)
			return callback(nonce, orderBytes)
		})
	})
}

// removeOrder deletes the whole order bucket of an order specified by the order
// type bucket key and the nonce. If the order does not exist, ErrNoOrder is
// returned.
func (db *DB) removeOrder(bucketKey []byte, nonce order.Nonce) error {
	return db.Update(func(tx *bbolt.Tx) error {
		// First, we'll grab the root bucket that houses all of our
		// main orders.
		rootBucket := tx.Bucket(bucketKey)
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
