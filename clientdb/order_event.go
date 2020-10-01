package clientdb

import (
	"fmt"
	"io"
	"time"

	"github.com/lightninglabs/pool/event"
	"github.com/lightninglabs/pool/order"
)

// OrderEvent is the main interface for order specific events.
type OrderEvent interface {
	event.Event

	// Nonce returns the nonce of the order this event refers to.
	Nonce() order.Nonce
}

// CreatedEvent is an event implementation that tracks the creation of an order.
// This is distinct from the order state change to allow us to efficiently
// filter all events by their type to get the creation timestamps of all orders.
type CreatedEvent struct {
	// timestamp is the unique timestamp the event was created/recorded at.
	timestamp time.Time

	// Nonce of the order this event refers to.
	nonce order.Nonce
}

// NewCreatedEvent creates a new CreatedEvent from an order with the current
// system time as the timestamp.
func NewCreatedEvent(o order.Order) *CreatedEvent {
	return &CreatedEvent{
		timestamp: time.Now(),
		nonce:     o.Nonce(),
	}
}

// Type returns the type of the event.
//
// NOTE: This is part of the event.Event interface.
func (e *CreatedEvent) Type() event.Type {
	return event.TypeOrderCreated
}

// Timestamp is the time the event happened. This will be made unique once it is
// stored. To avoid collisions, the timestamp is adjusted on the nanosecond
// scale to reach uniqueness.
//
// NOTE: This is part of the event.Event interface.
func (e *CreatedEvent) Timestamp() time.Time {
	return e.timestamp
}

// SetTimestamp updates the timestamp of the event. This is needed to adjust
// timestamps in case they collide to ensure the global uniqueness of all event
// timestamps.
//
// NOTE: This is part of the event.Event interface.
func (e *CreatedEvent) SetTimestamp(ts time.Time) {
	e.timestamp = ts
}

// String returns a human readable representation of the event.
//
// NOTE: This is part of the event.Event interface.
func (e *CreatedEvent) String() string {
	return "OrderCreated"
}

// Serialize writes the event data to a binary storage format. This does not
// serialize the event type as that's handled generically to allow for easy
// filtering.
//
// NOTE: This is part of the event.Event interface.
func (e *CreatedEvent) Serialize(w io.Writer) error {
	return WriteElements(w, e.nonce)
}

// Deserialize reads the event data from a binary storage format. This does not
// deserialize the event type as that's handled generically to allow for easy
// filtering.
//
// NOTE: This is part of the event.Event interface.
func (e *CreatedEvent) Deserialize(r io.Reader) error {
	return ReadElements(r, &e.nonce)
}

// Nonce returns the nonce of the order this event refers to.
//
// NOTE: This is part of the order.OrderEvent interface.
func (e *CreatedEvent) Nonce() order.Nonce {
	return e.nonce
}

// A compile time assertion to make sure CreatedEvent implements both the
// event.Event and order.OrderEvent interface.
var _ event.Event = (*CreatedEvent)(nil)
var _ OrderEvent = (*CreatedEvent)(nil)

// UpdatedEvent is an event implementation that tracks the updates of an order.
// This event is only meant for updates that we also persist in the database.
// Temporary state changes and other updates that occur during match making are
// tracked by MatchEvent.
type UpdatedEvent struct {
	// timestamp is the unique timestamp the event was created/recorded at.
	timestamp time.Time

	// Nonce of the order this event refers to.
	nonce order.Nonce

	// PrevState is the state the order had previous to the state change.
	PrevState order.State

	// NewState is the state the order had after the state change.
	NewState order.State

	// UnitsFilled is the number of units that was filled at the moment of
	// the update.
	UnitsFilled order.SupplyUnit
}

// NewUpdatedEvent creates a new UpdatedEvent from an order and its previous
// state with the current system time as the timestamp.
func NewUpdatedEvent(prevState order.State,
	o order.Order) *UpdatedEvent {

	return &UpdatedEvent{
		timestamp:   time.Now(),
		nonce:       o.Nonce(),
		PrevState:   prevState,
		NewState:    o.Details().State,
		UnitsFilled: o.Details().Units - o.Details().UnitsUnfulfilled,
	}
}

// Type returns the type of the event.
//
// NOTE: This is part of the event.Event interface.
func (e *UpdatedEvent) Type() event.Type {
	return event.TypeOrderStateChange
}

// Timestamp is the time the event happened. This will be made unique once it is
// stored. To avoid collisions, the timestamp is adjusted on the nanosecond
// scale to reach uniqueness.
//
// NOTE: This is part of the event.Event interface.
func (e *UpdatedEvent) Timestamp() time.Time {
	return e.timestamp
}

// SetTimestamp updates the timestamp of the event. This is needed to adjust
// timestamps in case they collide to ensure the global uniqueness of all event
// timestamps.
//
// NOTE: This is part of the event.Event interface.
func (e *UpdatedEvent) SetTimestamp(ts time.Time) {
	e.timestamp = ts
}

// String returns a human readable representation of the event.
//
// NOTE: This is part of the event.Event interface.
func (e *UpdatedEvent) String() string {
	return fmt.Sprintf("OrderUpdate(%v)", e.NewState)
}

// Serialize writes the event data to a binary storage format. This does not
// serialize the event type as that's handled generically to allow for easy
// filtering.
//
// NOTE: This is part of the event.Event interface.
func (e *UpdatedEvent) Serialize(w io.Writer) error {
	return WriteElements(w, e.nonce, e.PrevState, e.NewState, e.UnitsFilled)
}

// Deserialize reads the event data from a binary storage format. This does not
// deserialize the event type as that's handled generically to allow for easy
// filtering.
//
// NOTE: This is part of the event.Event interface.
func (e *UpdatedEvent) Deserialize(r io.Reader) error {
	return ReadElements(
		r, &e.nonce, &e.PrevState, &e.NewState, &e.UnitsFilled,
	)
}

// Nonce returns the nonce of the order this event refers to.
//
// NOTE: This is part of the order.OrderEvent interface.
func (e *UpdatedEvent) Nonce() order.Nonce {
	return e.nonce
}

// A compile time assertion to make sure UpdatedEvent implements both the
// event.Event and order.OrderEvent interface.
var _ event.Event = (*UpdatedEvent)(nil)
var _ OrderEvent = (*UpdatedEvent)(nil)

// MatchEvent is an event implementation that tracks the match making process of
// an order.
type MatchEvent struct {
	// timestamp is the unique timestamp the event was created/recorded at.
	timestamp time.Time

	// Nonce of the order this event refers to.
	nonce order.Nonce

	// MatchState is the state of the order matching process the event was
	// created in.
	MatchState order.MatchState

	// UnitsFilled is the number of units that was or would have been filled
	// for the current match attempt the order was in when this event was
	// created.
	UnitsFilled order.SupplyUnit

	// MatchedOrder is the order counterpart that our order was matched to
	// in the current match attempt this event was created for.
	MatchedOrder order.Nonce

	// RejectReason is set to the reason we rejected the match in case the
	// MatchState is set to MatchStateRejected.
	RejectReason uint32
}

// NewMatchEvent creates a new MatchEvent from an order and its matched
// counterpart with the given time as the timestamp.
func NewMatchEvent(ts time.Time, nonce order.Nonce, state order.MatchState,
	unitsFilled order.SupplyUnit, matchedOrder order.Nonce,
	rejectReason uint32) *MatchEvent {

	return &MatchEvent{
		timestamp:    ts,
		nonce:        nonce,
		MatchState:   state,
		UnitsFilled:  unitsFilled,
		MatchedOrder: matchedOrder,
		RejectReason: rejectReason,
	}
}

// Type returns the type of the event.
//
// NOTE: This is part of the event.Event interface.
func (e *MatchEvent) Type() event.Type {
	return event.TypeOrderMatch
}

// Timestamp is the time the event happened. This will be made unique once it is
// stored. To avoid collisions, the timestamp is adjusted on the nanosecond
// scale to reach uniqueness.
//
// NOTE: This is part of the event.Event interface.
func (e *MatchEvent) Timestamp() time.Time {
	return e.timestamp
}

// SetTimestamp updates the timestamp of the event. This is needed to adjust
// timestamps in case they collide to ensure the global uniqueness of all event
// timestamps.
//
// NOTE: This is part of the event.Event interface.
func (e *MatchEvent) SetTimestamp(ts time.Time) {
	e.timestamp = ts
}

// String returns a human readable representation of the event.
//
// NOTE: This is part of the event.Event interface.
func (e *MatchEvent) String() string {
	return fmt.Sprintf("OrderMatch(%v)", e.MatchState)
}

// Serialize writes the event data to a binary storage format. This does not
// serialize the event type as that's handled generically to allow for easy
// filtering.
//
// NOTE: This is part of the event.Event interface.
func (e *MatchEvent) Serialize(w io.Writer) error {
	return WriteElements(
		w, e.nonce, e.MatchState, e.UnitsFilled, e.MatchedOrder,
		e.RejectReason,
	)
}

// Deserialize reads the event data from a binary storage format. This does not
// deserialize the event type as that's handled generically to allow for easy
// filtering.
//
// NOTE: This is part of the event.Event interface.
func (e *MatchEvent) Deserialize(r io.Reader) error {
	return ReadElements(
		r, &e.nonce, &e.MatchState, &e.UnitsFilled, &e.MatchedOrder,
		&e.RejectReason,
	)
}

// Nonce returns the nonce of the order this event refers to.
//
// NOTE: This is part of the order.OrderEvent interface.
func (e *MatchEvent) Nonce() order.Nonce {
	return e.nonce
}

// A compile time assertion to make sure MatchEvent implements both the
// event.Event and order.OrderEvent interface.
var _ event.Event = (*MatchEvent)(nil)
var _ OrderEvent = (*MatchEvent)(nil)
