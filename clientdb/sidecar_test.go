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
