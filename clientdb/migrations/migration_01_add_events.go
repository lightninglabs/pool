package migrations

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/lightninglabs/pool/event"
	"go.etcd.io/bbolt"
)

var (
	// ordersBucketKey is a bucket that contains all orders that are
	// currently pending or completed. This bucket is keyed by the nonce and
	// leads to a nested sub-bucket that houses information for that order.
	ordersBucketKey = []byte("orders")

	// eventBucketKey is the top level bucket where we can find all events
	// of the system. These events are indexed by their timestamps which are
	// guaranteed to be unique by the clientdb API.
	eventBucketKey = []byte("event")

	// eventRefSubBucket is the sub bucket for event references. We only
	// store a reference to the event's timestamp and type in the event
	// "owner"'s sub bucket (for example the sub bucket of the order the
	// event belongs to).
	eventRefSubBucket = []byte("event-ref")

	// byteOrder is the default byte order we'll use for serialization
	// within the database.
	byteOrder = binary.BigEndian
)

// AddInitialOrderTimestamps creates an "order created" timestamp for each of
// the existing orders. This will only be executed once, when the main event
// bucket is created for the first time.
func AddInitialOrderTimestamps(tx *bbolt.Tx) error {
	// We back date all existing orders to the beginning of the year of hell
	// to signal this isn't the real timestamp it was created at but
	// something we added later.
	fixedTimestamp := time.Date(
		2020, time.January, 1, 0, 0, 0, 0, time.UTC,
	).UnixNano()

	// First, we'll grab our main order bucket key.
	ordersBucket := tx.Bucket(ordersBucketKey)
	if ordersBucket == nil {
		return fmt.Errorf("bucket \"%v\" does not exist",
			string(ordersBucketKey))
	}

	// We'll now traverse the root bucket for all orders. The primary key is
	// the order nonce itself. We create a new event for each order with a
	// timestamp that's unique for each event.
	var (
		idx            int64
		tsScratchSpace [event.TimestampLength]byte
		eventType      = byte(event.TypeOrderCreated)
		eventRefs      = make(map[[32]byte]uint64)
	)
	err := ordersBucket.ForEach(func(nonceBytes, val []byte) error {
		// Only go into things that we know are sub-bucket keys.
		if val != nil {
			return nil
		}

		// Get the order nonce and make sure we can add the initial
		// creation event for that order.
		var nonce [32]byte
		copy(nonce[:], nonceBytes)
		orderBucket := ordersBucket.Bucket(nonce[:])
		if orderBucket == nil {
			return fmt.Errorf("order bucket not found")
		}

		// First of all, encode the timestamp to its binary value and
		// keep track of it so we can create a reference in the order's
		// event reference sub bucket later.
		ts := uint64(fixedTimestamp + idx)
		eventRefs[nonce] = ts
		byteOrder.PutUint64(tsScratchSpace[:], ts)

		// With the key encoded, we'll then encode the event into our
		// buffer, then write it out to disk.
		evtBucket := tx.Bucket(eventBucketKey)
		var eventBuf bytes.Buffer
		if err := eventBuf.WriteByte(eventType); err != nil {
			return err
		}
		if _, err := eventBuf.Write(nonceBytes); err != nil {
			return err
		}
		err := evtBucket.Put(tsScratchSpace[:], eventBuf.Bytes())
		if err != nil {
			return err
		}

		idx++

		return nil
	})
	if err != nil {
		return err
	}

	// Because we can't modify the order bucket in the ForEach above
	// directly, we need to insert the references outside of the callback.
	for nonce, ts := range eventRefs {
		orderBucket := ordersBucket.Bucket(nonce[:])
		if orderBucket == nil {
			return fmt.Errorf("order bucket not found")
		}

		// We also need to store a reference entry in the order bucket.
		eventSubBucket, err := orderBucket.CreateBucketIfNotExists(
			eventRefSubBucket,
		)
		if err != nil {
			return err
		}
		byteOrder.PutUint64(tsScratchSpace[:], ts)
		err = eventSubBucket.Put(tsScratchSpace[:], []byte{eventType})
		if err != nil {
			return err
		}
	}

	return nil
}
