package event

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

const (
	// TimestampLength is the length in bytes it takes to serialize a
	// timestamp with nanosecond precision.
	TimestampLength = 8
)

var (
	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

// Type denotes the type of an event. The numeric representation of the types
// must be unique, that's why we define all of them in this package. But the
// actual implementation is left to the business package.
type Type uint8

const (
	// TypeAny denotes no specific type and should only be used for querying
	// the event database, not as an actual event type.
	TypeAny Type = 0

	// TypeOrderCreated is the type of event that is emitted when an order
	// is first created.
	TypeOrderCreated Type = 1

	// TypeOrderStateChange is the type of event that is emitted when an
	// order changes its state in the order database due to it being
	// modified.
	TypeOrderStateChange Type = 2

	// TypeOrderMatch is the type of event that is emitted when an order is
	// being matched in a batch. An event of this type only denotes a
	// participation in a batch attempt and not necessarily final inclusion.
	// If a reject happens for any reason, the order might not make it to
	// the final batch and not all match states would therefore be present.
	TypeOrderMatch Type = 3
)

// Event is the main interface all events have to implement.
type Event interface {
	// Type returns the type of the event.
	Type() Type

	// Timestamp is the time the event happened. This will be made unique
	// once it is stored. To avoid collisions, the timestamp is adjusted on
	// the nanosecond scale to reach uniqueness.
	Timestamp() time.Time

	// SetTimestamp updates the timestamp of the event. This is needed to
	// adjust timestamps in case they collide to ensure the global
	// uniqueness of all event timestamps.
	SetTimestamp(time.Time)

	// String returns a human readable representation of the event.
	String() string

	// Serialize writes the event data to a binary storage format. This does
	// not serialize the event type as that's handled generically to allow
	// for easy filtering.
	Serialize(*bytes.Buffer) error

	// Deserialize reads the event data from a binary storage format. This
	// does not deserialize the event type as that's handled generically to
	// allow for easy filtering.
	Deserialize(io.Reader) error
}

// Predicate is a function type that can be used to filter events. It gets the
// timestamp and type of an event passed in and can return whether that event
// is relevant or not.
type Predicate func(time.Time, Type) bool

// StoreEvent tries to store an event into the given bucket by trying to avoid
// collisions. If a key for the event timestamp already exists in the database,
// the timestamp is incremented in nanosecond intervals until a "free" slot is
// found. The given scratch space byte slice should ideally point to an array to
// avoid allocating new memory with each iteration. The value found in it after
// the function returns with a nil-error is the actual storage key that was
// used.
//
// NOTE: When storing many events at the same time, the MakeUniqueTimestamps
// function should be called first to ensure the timestamps are already unique
// among themselves. If no "gap" in the timestamps can be found after 1000 tries
// an error is returned.
func StoreEvent(bucket *bbolt.Bucket, event Event) error {
	// We use a fixed length array as our buffer so we don't allocate new
	// memory for each try.
	var tsScratchSpace [TimestampLength]byte

	// First, we'll serialize this timestamp into our timestamp buffer.
	byteOrder.PutUint64(
		tsScratchSpace[:], uint64(event.Timestamp().UnixNano()),
	)

	// Next we'll loop until we find a "free" slot in the bucket to store
	// the event under. This should almost never happen unless we're running
	// on a system that has a very bad system clock that doesn't properly
	// resolve to nanosecond scale. We try up to 1000 times (which would
	// come to a maximum shift of 1 microsecond which is acceptable for most
	// use cases). If we don't find a free slot, we don't want to lose any
	// data by just overwriting an existing event and therefore throw an
	// error.
	const maxTries = 1000
	tries := 0
	for tries < maxTries {
		val := bucket.Get(tsScratchSpace[:])
		if val == nil {
			break
		}

		// Collision, try the next nanosecond timestamp.
		nextNano := event.Timestamp().UnixNano() + 1
		event.SetTimestamp(time.Unix(0, nextNano))
		byteOrder.PutUint64(tsScratchSpace[:], uint64(nextNano))
		tries++
	}
	if tries == maxTries {
		return fmt.Errorf("error finding unique timestamp slot for "+
			"event ts=%v, type=%v", event.Timestamp(), event.Type())
	}

	// With the key encoded, we'll then encode the event
	// into our buffer, then write it out to disk.
	var eventBuf bytes.Buffer
	if err := eventBuf.WriteByte(byte(event.Type())); err != nil {
		return err
	}
	err := event.Serialize(&eventBuf)
	if err != nil {
		return err
	}
	return bucket.Put(tsScratchSpace[:], eventBuf.Bytes())
}

// MakeUniqueTimestamps takes a slice of event records, sorts it by the event
// timestamps and then makes sure there are no duplicates in the timestamps. If
// duplicates are found, some of the timestamps are increased on the nanosecond
// scale until only unique values remain.
func MakeUniqueTimestamps(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp().Before(events[j].Timestamp())
	})

	// Now that we know the events are sorted by timestamp, we can go
	// through the list and fix all duplicates until only unique values
	// remain.
	for i := 0; i < len(events)-1; i++ {
		current := events[i].Timestamp().UnixNano()
		next := events[i+1].Timestamp().UnixNano()

		// We initially sorted the slice. So if the current is now
		// greater or equal to the next one, it's either because it's a
		// duplicate or because we increased the current in the last
		// iteration.
		if current >= next {
			next = current + 1
			events[i+1].SetTimestamp(time.Unix(0, next))
		}
	}
}
