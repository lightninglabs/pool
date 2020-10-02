package clientdb

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/lightninglabs/pool/event"
	"go.etcd.io/bbolt"
)

var (
	// eventBucketKey is the top level bucket where we can find all events
	// of the system. These events are indexed by their timestamps which are
	// guaranteed to be unique by the clientdb API.
	eventBucketKey = []byte("event")

	// eventRefSubBucket is the sub bucket for event references. We only
	// store a reference to the event's timestamp and type in the event
	// "owner"'s sub bucket (for example the sub bucket of the order the
	// event belongs to).
	eventRefSubBucket = []byte("event-ref")
)

const (
	// eventsAllocSize is the average number of events we expect an order to
	// have over its lifetime and use that to pre-allocate the event slice.
	eventsAllocSize = 50
)

// GetEvents returns all events identified by their unique timestamps.
func (db *DB) GetEvents(timestamps map[time.Time]struct{}) ([]event.Event,
	error) {

	return db.getEvents(func(ts time.Time, _ event.Type) bool {
		_, ok := timestamps[ts]
		return ok
	})
}

// GetEventsInRange returns all events that have their timestamps between the
// given start and end time (inclusive) and are of the given type. Use
// event.TypeAny to not filter by type.
func (db *DB) GetEventsInRange(start, end time.Time,
	evtType event.Type) ([]event.Event, error) {

	return db.getEvents(func(ts time.Time, t event.Type) bool {
		typeOk := evtType == event.TypeAny || evtType == t
		return typeOk && !ts.Before(start) && !ts.After(end)
	})
}

// AllEvents returns all events that are of the given type. Use event.TypeAny to
// not filter by type.
func (db *DB) AllEvents(evtType event.Type) ([]event.Event, error) {
	return db.getEvents(func(_ time.Time, t event.Type) bool {
		return evtType == event.TypeAny || evtType == t
	})
}

// getEvents iterates through all keys in the main events bucket and returns all
// events that match the given predicate.
func (db *DB) getEvents(predicate event.Predicate) ([]event.Event, error) {
	var events []event.Event
	err := db.View(func(tx *bbolt.Tx) error {
		var err error
		events, err = getEventsTX(tx, predicate)
		return err
	})
	if err != nil {
		return nil, err
	}

	return events, nil
}

// getEventsTX retrieves all events that match the given predicate from the main
// event bucket within the given database transaction.
func getEventsTX(tx *bbolt.Tx, predicate event.Predicate) ([]event.Event,
	error) {

	events := make([]event.Event, 0, eventsAllocSize)
	eventBucket := tx.Bucket(eventBucketKey)
	err := eventBucket.ForEach(func(k, v []byte) error {
		// There shouldn't be any other keys below the main
		// event bucket so we fail hard if a key doesn't match
		// our required length.
		if len(k) != event.TimestampLength {
			return fmt.Errorf("unexpected timestamp "+
				"key length: %d", len(k))
		}

		// The value must contain at least one byte which is the
		// event type.
		if len(v) < 1 {
			return fmt.Errorf("unexpected timestamp "+
				"value length: %d", len(v))
		}

		// Decode the timestamp and type to make sure this event
		// matches our given filter predicate.
		ts := time.Unix(0, int64(byteOrder.Uint64(k)))
		evtType := event.Type(v[0])
		if !predicate(ts, evtType) {
			return nil
		}

		// Deserialize the event according to its type.
		reader := bytes.NewReader(v[1:])
		evt, err := deserializeEvent(reader, ts, evtType)
		if err != nil {
			return err
		}

		events = append(events, evt)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Make sure we always return a sorted list, even if the underlying
	// storage might scramble them.
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp().Before(events[j].Timestamp())
	})

	return events, nil
}

// storeEventTX stores a single event both in the main events bucket (ensuring
// the uniqueness of its timestamp in the process) and a reference to it in the
// "owner"'s bucket under an event reference sub bucket.
func storeEventTX(ownerBucket *bbolt.Bucket, evt event.Event) error {
	// For convenience, the event is allowed to be nil, in case no update
	// really happened.
	if evt == nil {
		return nil
	}

	// First of all, write the event to the global event bucket.
	evtBucket := ownerBucket.Tx().Bucket(eventBucketKey)
	err := event.StoreEvent(evtBucket, evt)
	if err != nil {
		return err
	}

	// If this is the first event for this "owner" object, the event sub
	// bucket might not yet exist.
	eventSubBucket, err := ownerBucket.CreateBucketIfNotExists(
		eventRefSubBucket,
	)
	if err != nil {
		return err
	}

	// Now that we know we have a unique timestamp, we can also store a
	// reference to the given reference bucket. The important thing there is
	// the key itself. But we write the event type as the value to allow a
	// low-level filtering when reading all events for a certain object
	// (order/acct).
	var refKey [event.TimestampLength]byte
	byteOrder.PutUint64(refKey[:], uint64(evt.Timestamp().UnixNano()))
	return eventSubBucket.Put(refKey[:], []byte{byte(evt.Type())})
}

// deserializeEvent reads a single event from the given reader and deserializes
// it according to the given event type.
func deserializeEvent(reader io.Reader, timestamp time.Time,
	eventType event.Type) (event.Event, error) {

	// Read the content of the event according to its type.
	var evt event.Event
	switch eventType {
	case event.TypeOrderCreated:
		evt = &CreatedEvent{}

	case event.TypeOrderStateChange:
		evt = &UpdatedEvent{}

	case event.TypeOrderMatch:
		evt = &MatchEvent{}

	default:
		return nil, fmt.Errorf("unknown event type <%d>", eventType)
	}

	evt.SetTimestamp(timestamp)
	if err := evt.Deserialize(reader); err != nil {
		return nil, err
	}

	return evt, nil
}
