package sidecar

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/codec"
)

// Version is the version of the sidecar ticket format.
type Version uint8

const (
	// VersionDefault is the initial version of the ticket format.
	VersionDefault Version = 0

	// VersionUnannouncedZeroConf is the fist version of the ticket format
	// that added support for unannounced and zero conf channels.
	VersionUnannouncedZeroConf Version = 1
)

// State is the state a sidecar ticket currently is in. Each updater of the
// ticket is responsible for also setting the state correctly after adding their
// data according to their role.
type State uint8

const (
	// StateCreated is the state a ticket is in after it's been first
	// created and the offer information was added but not signed yet.
	StateCreated State = 0

	// StateOffered is the state a ticket is in after the offer data was
	// signed by the sidecar provider.
	StateOffered State = 1

	// StateRegistered is the state a ticket is in after the recipient has
	// registered it in their system and added their information.
	StateRegistered State = 2

	// StateOrdered is the state a ticket is in after the bid order for the
	// sidecar channel was submitted and the order information was added to
	// the ticket. The ticket now also contains a signature of the provider
	// over the order digest.
	StateOrdered State = 3

	// StateExpectingChannel is the state a ticket is in after it's been
	// returned to the channel recipient and their node was instructed to
	// start listening for batch events for it.
	StateExpectingChannel State = 4

	// StateCompleted is the state a ticket is in after the sidecar channel
	// was successfully opened and the bid order completed.
	StateCompleted State = 5

	// StateCanceled is the state a ticket is in after the sidecar channel
	// bid order was canceled by the taker.
	StateCanceled State = 6
)

// String returns the string representation of a sidecar ticket state.
func (s State) String() string {
	switch s {
	case StateCreated:
		return "created"

	case StateOffered:
		return "offered"

	case StateRegistered:
		return "registered"

	case StateOrdered:
		return "ordered"

	case StateExpectingChannel:
		return "expecting channel"

	case StateCompleted:
		return "completed"

	case StateCanceled:
		return "canceled"

	default:
		return fmt.Sprintf("unknown <%d>", s)
	}
}

// IsTerminal returns true if the ticket is in a final state and will not be
// updated ever again.
func (s State) IsTerminal() bool {
	switch s {
	case StateCompleted, StateCanceled:
		return true

	default:
		return false
	}
}

// Offer is a struct holding the information that a sidecar channel provider is
// committing to when offering to buy a channel for the recipient. The sidecar
// channel flow is initiated by the provider creating a ticket and adding its
// signed offer to it.
type Offer struct {
	// Capacity is the channel capacity of the sidecar channel in satoshis.
	Capacity btcutil.Amount

	// PushAmt is the amount in satoshis that will be pushed to the
	// recipient of the channel to kick start them with inbound capacity.
	// If this is non-zero then the provider must pay for the push amount as
	// well as all other costs from their Pool account. Makers can opt out
	// of supporting push amounts when submitting ask orders so this will
	// reduce the matching chances somewhat.
	PushAmt btcutil.Amount

	// LeaseDurationBlocks is the number of blocks the offered channel in
	// this offer would be leased for.
	LeaseDurationBlocks uint32

	// SignPubKey is the public key for corresponding to the private key
	// that signed the SigOfferDigest below and, in a later state, the
	// SigOrderDigest of the Order struct.
	SignPubKey *btcec.PublicKey

	// SigOfferDigest is a signature over the offer digest, signed with the
	// private key that corresponds to the SignPubKey above.
	SigOfferDigest *ecdsa.Signature

	// Auto determines if the provider requires that the ticket be
	// completed using an automated negotiation sequence.
	Auto bool

	// UnannouncedChannel determines if this ticket is interested in
	// unannounced channels or not.
	UnannouncedChannel bool

	// ZeroConfChannel determines if this ticket is interested in zero conf
	// channels or not.
	ZeroConfChannel bool
}

// Recipient is a struct holding the information about the recipient of the
// sidecar channel in question.
type Recipient struct {
	// NodePubKey is the recipient nodes' identity public key that is
	// advertised in the bid order.
	NodePubKey *btcec.PublicKey

	// MultiSigPubKey is a public key to which the recipient node has the
	// private key to. It is the key that will be used as one of the 2-of-2
	// multisig keys of the channel funding transaction output and is
	// advertised in the bid order.
	MultiSigPubKey *btcec.PublicKey

	// MultiSigKeyIndex is the derivation index of the MultiSigPubKey.
	MultiSigKeyIndex uint32
}

// Order is a struct holding the information about the sidecar bid order after
// it's been submitted by the provider.
type Order struct {
	// BidNonce is the order nonce of the bid order that was submitted for
	// purchasing the sidecar channel.
	BidNonce [32]byte

	// SigOrderDigest is a signature over the order digest, signed with the
	// private key that corresponds to the SignPubKey in the Offer struct.
	SigOrderDigest *ecdsa.Signature
}

// Execution is a struct holding information about the sidecar bid order during
// its execution.
type Execution struct {
	// PendingChannelID is the pending channel ID of the currently
	// registered funding shim that was added by the recipient node to
	// receive the channel.
	PendingChannelID [32]byte
}

// Ticket is a struct holding all the information for establishing/buying a
// sidecar channel. It is meant to be used in a PSBT like manner where each
// participant incrementally adds their information according to their role and
// the current step.
type Ticket struct {
	// ID is a pseudorandom identifier of the ticket.
	ID [8]byte

	// Version is the version of the ticket serialization format.
	Version Version

	// State is the current state of the ticket. The state is updated by
	// each participant and is therefore not covered in any of the signature
	// digests.
	State State

	// Offer contains the initial conditions offered by the sidecar channel
	// provider. Every ticket must start with an offer and therefore this
	// member can never be empty or nil.
	Offer Offer

	// Recipient contains the information about the receiver node of the
	// sidecar channel. This field must be set for all states greater or
	// equal to StateRegistered.
	Recipient *Recipient

	// Order contains the information about the order that was submitted for
	// leasing the sidecar channel represented by this ticket. This field
	// must be set for all states greater or equal to StateOrdered.
	Order *Order

	// Execution contains the information about the execution part of the
	// sidecar channel. This information is not meant to be exchanged
	// between the participating parties but is included in the ticket to
	// make it easy to serialize/deserialize a ticket state within the local
	// database.
	Execution *Execution
}

// NewTicket creates a new sidecar ticket with the given version and offer
// information.
func NewTicket(capacity, pushAmt btcutil.Amount,
	duration uint32, offerPubKey *btcec.PublicKey,
	auto bool, unannounced, zeroConf bool) (*Ticket, error) {

	// The ticket should always use the minimum version that supports
	// all the features encoded in it so it can be used by clients using
	// older pool versions.
	version := VersionDefault
	if unannounced || zeroConf {
		version = VersionUnannouncedZeroConf
	}

	t := &Ticket{
		Version: version,
		State:   StateOffered,
		Offer: Offer{
			Capacity:            capacity,
			PushAmt:             pushAmt,
			LeaseDurationBlocks: duration,
			SignPubKey:          offerPubKey,
			Auto:                auto,
			UnannouncedChannel:  unannounced,
			ZeroConfChannel:     zeroConf,
		},
	}

	// The linter flags this, even though crypto/rand is being used...
	if _, err := rand.Read(t.ID[:]); err != nil { // nolint:gosec
		return nil, err
	}

	return t, nil
}

// OfferDigest returns a message digest over the offer fields.
func (t *Ticket) OfferDigest() ([32]byte, error) {
	var (
		msg    bytes.Buffer
		result [sha256.Size]byte
	)
	switch t.Version {
	case VersionDefault:
		err := codec.WriteElements(
			&msg, t.ID[:], uint8(t.Version), t.Offer.Capacity,
			t.Offer.PushAmt, t.Offer.Auto,
		)
		if err != nil {
			return result, err
		}

	case VersionUnannouncedZeroConf:
		err := codec.WriteElements(
			&msg, t.ID[:], uint8(t.Version), t.Offer.Capacity,
			t.Offer.PushAmt, t.Offer.Auto,
			t.Offer.UnannouncedChannel, t.Offer.ZeroConfChannel,
		)
		if err != nil {
			return result, err
		}

	default:
		return result, fmt.Errorf("unknown version %d", t.Version)
	}
	return sha256.Sum256(msg.Bytes()), nil
}

// OrderDigest returns a message digest over the order fields.
func (t *Ticket) OrderDigest() ([32]byte, error) {
	var (
		msg    bytes.Buffer
		result [sha256.Size]byte
	)

	if t.State < StateOrdered || t.Order == nil {
		return result, fmt.Errorf("invalid state for order digest")
	}

	switch t.Version {
	case VersionDefault:
		err := codec.WriteElements(
			&msg, t.ID[:], uint8(t.Version), t.Offer.Capacity,
			t.Offer.PushAmt, t.Order.BidNonce[:],
		)
		if err != nil {
			return result, err
		}

	case VersionUnannouncedZeroConf:
		err := codec.WriteElements(
			&msg, t.ID[:], uint8(t.Version), t.Offer.Capacity,
			t.Offer.PushAmt, t.Offer.Auto,
			t.Offer.UnannouncedChannel, t.Offer.ZeroConfChannel,
		)
		if err != nil {
			return result, err
		}

	default:
		return result, fmt.Errorf("unknown version %d", t.Version)
	}
	return sha256.Sum256(msg.Bytes()), nil
}

// Store is the interface a persistent storage must implement for storing and
// retrieving sidecar tickets.
type Store interface {
	// AddSidecar adds a record for the sidecar order to the database.
	AddSidecar(sidecar *Ticket) error

	// UpdateSidecar updates a sidecar order in the database.
	UpdateSidecar(sidecar *Ticket) error

	// Sidecar retrieves a specific sidecar by its ID and provider signing
	// key (offer signature pubkey) or returns ErrNoSidecar if it's not
	// found.
	Sidecar(id [8]byte, offerSignPubKey *btcec.PublicKey) (*Ticket, error)

	// Sidecars retrieves all known sidecar orders from the database.
	Sidecars() ([]*Ticket, error)
}
