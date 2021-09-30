package clientdb

import (
	"testing"

	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/stretchr/testify/require"
)

func assertSidecarExists(t *testing.T, db *DB, expected *sidecar.Ticket) {
	t.Helper()

	found, err := db.Sidecar(expected.ID, expected.Offer.SignPubKey)
	require.NoError(t, err)

	require.Equal(t, expected, found)
}

// TestSidecars ensures that all database operations involving sidecars run as
// expected.
func TestSidecars(t *testing.T) {
	t.Parallel()

	db, cleanup := newTestDB(t)
	defer cleanup()

	// Create a test sidecar we'll use to interact with the database.
	s := &sidecar.Ticket{
		ID:    [8]byte{12, 34, 56},
		State: sidecar.StateRegistered,
		Offer: sidecar.Offer{
			Capacity:            1000000,
			PushAmt:             200000,
			SignPubKey:          testTraderKey,
			LeaseDurationBlocks: 2016,
		},
		Recipient: &sidecar.Recipient{
			MultiSigPubKey:   testTraderKey,
			MultiSigKeyIndex: 7,
		},
	}

	// First, we'll add it to the database. We should be able to retrieve
	// after.
	err := db.AddSidecar(s)
	require.NoError(t, err)
	assertSidecarExists(t, db, s)

	// Transition the sidecar state from SidecarInitialized to
	// SidecarExpectingChannel and add the required information for that
	// state.
	s.State = sidecar.StateExpectingChannel
	s.Order = &sidecar.Order{
		BidNonce: order.Nonce{1, 2, 3},
	}
	err = db.UpdateSidecar(s)
	require.NoError(t, err)
	assertSidecarExists(t, db, s)

	// Retrieving all sidecars should show that we only have one sidecar,
	// the same one.
	sidecars, err := db.Sidecars()
	require.NoError(t, err)
	require.Len(t, sidecars, 1)
	require.Contains(t, sidecars, s)

	// Make sure we can query a sidecar ticket by its ID and offer pubkey.
	updatedTicket, err := db.Sidecar([8]byte{12, 34, 56}, testTraderKey)
	require.NoError(t, err)
	require.Equal(t, s, updatedTicket)
}

// TestSidecarsWithOrder tests that we're able to properly insert a new order
// into a sidecar sub-bucket along with the ticket, as well as retrieve it again
// in the future.
func TestSidecarsWithOrder(t *testing.T) {
	t.Parallel()

	db, cleanup := newTestDB(t)
	defer cleanup()

	// First, we'll make a new order that'll be matched along with a ticket
	// we'll create below.
	bid := &order.Bid{
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
	}
	bid.Details().MinUnitsMatch = 10

	// Next we'll craft a new ticket that we'll use to bind to the base
	// order.
	ticket := &sidecar.Ticket{
		ID:    [8]byte{12, 34, 56},
		State: sidecar.StateRegistered,
		Offer: sidecar.Offer{
			Capacity:            1000000,
			PushAmt:             200000,
			SignPubKey:          testTraderKey,
			LeaseDurationBlocks: 2016,
		},
		Recipient: &sidecar.Recipient{
			MultiSigPubKey:   testTraderKey,
			MultiSigKeyIndex: 7,
		},
	}

	err := db.AddSidecarWithBid(ticket, bid)
	require.NoError(t, err)
	assertSidecarExists(t, db, ticket)

	// We should be able to retrieve the bid again given the original
	// ticket.
	diskBid, err := db.SidecarBidTemplate(ticket)
	require.NoError(t, err)

	// This bid should match the one we inserted earlier exactly.
	require.Equal(t, diskBid, bid)

	// Once we put the ticket into a final state, the bid template for it
	// should be deleted.
	ticket.State = sidecar.StateCanceled
	err = db.UpdateSidecar(ticket)
	require.NoError(t, err)

	_, err = db.SidecarBidTemplate(ticket)
	require.Equal(t, ErrNoOrder, err)
}

// TestSidecarsWithSameID makes sure that sidecar tickets with the same ID but
// different offer public keys can be stored and retrieved separately.
func TestSidecarsWithSameID(t *testing.T) {
	t.Parallel()

	db, cleanup := newTestDB(t)
	defer cleanup()

	// Create a test sidecar we'll use to interact with the database.
	s := &sidecar.Ticket{
		ID:    [8]byte{12, 34, 56},
		State: sidecar.StateRegistered,
		Offer: sidecar.Offer{
			Capacity:            1000000,
			PushAmt:             200000,
			SignPubKey:          testTraderKey,
			LeaseDurationBlocks: 2016,
		},
		Recipient: &sidecar.Recipient{
			MultiSigPubKey:   testTraderKey,
			MultiSigKeyIndex: 7,
		},
	}

	// First, we'll add it to the database. We should be able to retrieve
	// after.
	require.NoError(t, db.AddSidecar(s))
	assertSidecarExists(t, db, s)

	// Now we'll just store it again with the same ID but with a different
	// offer public key. It shouldn't overwrite the original one but add a
	// second ticket.
	s.Offer.SignPubKey = testBatchKey
	require.NoError(t, db.AddSidecar(s))
	assertSidecarExists(t, db, s)

	// Just to make sure the seek of the cursor works correctly, we also add
	// two more tickets, both with different IDs.
	s.ID = [8]byte{99, 88, 77}
	require.NoError(t, db.AddSidecar(s))
	assertSidecarExists(t, db, s)
	s.ID = [8]byte{55, 44, 33}
	require.NoError(t, db.AddSidecar(s))
	assertSidecarExists(t, db, s)

	// All four should be retrievable now, but just the first two when
	// querying by the ID.
	all, err := db.Sidecars()
	require.NoError(t, err)
	require.Len(t, all, 4)

	byID, err := db.SidecarsByID([8]byte{12, 34, 56})
	require.NoError(t, err)
	require.Len(t, byID, 2)
	for _, tkt := range byID {
		require.Equal(t, [8]byte{12, 34, 56}, tkt.ID)
	}

	// The first ticket should be the one with the test batch public key
	// that we added as the second ticket since its public key is lower than
	// the test public key we used for the first ticket (because of the way
	// the bbolt cursor works).
	require.Equal(t, testBatchKey, byID[0].Offer.SignPubKey)
	require.Equal(t, testTraderKey, byID[1].Offer.SignPubKey)
}
