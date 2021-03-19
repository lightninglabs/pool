package pool

import (
	"context"
	"io/ioutil"
	"math/big"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/stretchr/testify/require"
)

var (
	_, providerPubKey = btcec.PrivKeyFromBytes(btcec.S256(), []byte{0x02})
	_, ourNodePubKey  = btcec.PrivKeyFromBytes(btcec.S256(), []byte{0x03})
	testOfferSig      = &btcec.Signature{
		R: new(big.Int).SetInt64(44),
		S: new(big.Int).SetInt64(22),
	}
)

func newTestDB(t *testing.T) (*clientdb.DB, func()) {
	tempDir, err := ioutil.TempDir("", "client-db")
	require.NoError(t, err)

	db, err := clientdb.New(tempDir, clientdb.DBFilename)
	if err != nil {
		require.NoError(t, os.RemoveAll(tempDir))
		t.Fatalf("unable to create new db: %v", err)
	}

	return db, func() {
		require.NoError(t, db.Close())
		require.NoError(t, os.RemoveAll(tempDir))
	}
}

// TestRegisterSidecar makes sure that registering a sidecar ticket verifies the
// offer signature contained within, adds the recipient node's information to
// the ticket and stores it to the local database.
func TestRegisterSidecar(t *testing.T) {
	t.Parallel()

	mockSigner := test.NewMockSigner()
	mockWallet := test.NewMockWalletKit()
	mockSigner.Signature = testOfferSig.Serialize()

	acceptor := NewSidecarAcceptor(
		nil, mockSigner, mockWallet, nil, nil, ourNodePubKey,
		auctioneer.Config{}, nil,
	)

	existingTicket, err := sidecar.NewTicket(
		sidecar.VersionDefault, 1_000_000, 200_000, 2016,
		providerPubKey,
	)
	require.NoError(t, err)

	testCases := []struct {
		name        string
		ticket      *sidecar.Ticket
		expectedErr string
		check       func(t *testing.T, ticket *sidecar.Ticket)
	}{{
		name: "ticket with ID exists",
		ticket: &sidecar.Ticket{
			ID:    existingTicket.ID,
			State: sidecar.StateOffered,
			Offer: sidecar.Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
		},
		expectedErr: "ticket with ID",
	}, {
		name: "invalid sig",
		ticket: &sidecar.Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: sidecar.StateOffered,
			Offer: sidecar.Offer{
				SignPubKey: providerPubKey,
				SigOfferDigest: &btcec.Signature{
					R: new(big.Int).SetInt64(33),
					S: new(big.Int).SetInt64(33),
				},
			},
		},
		expectedErr: "signature not valid for public key",
	}, {
		name: "all valid",
		ticket: &sidecar.Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: sidecar.StateOffered,
			Offer: sidecar.Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
		},
		expectedErr: "",
		check: func(t *testing.T, ticket *sidecar.Ticket) {
			_, pubKey := test.CreateKey(0)

			require.NotNil(t, ticket.Recipient)
			require.Equal(
				t, ticket.Recipient.NodePubKey, ourNodePubKey,
			)
			require.Equal(
				t, ticket.Recipient.MultiSigPubKey, pubKey,
			)

			id := [8]byte{1, 2, 3, 4}
			newTicket, err := acceptor.store.Sidecar(
				id, providerPubKey,
			)
			require.NoError(t, err)
			require.Equal(t, newTicket, ticket)
		},
	}}

	for _, tc := range testCases {
		tc := tc

		if tc.ticket != nil {
			digest, err := tc.ticket.OfferDigest()
			require.NoError(t, err)
			mockSigner.SignatureMsg = string(digest[:])
		}

		store, cleanup := newTestDB(t)
		err = store.AddSidecar(existingTicket)
		require.NoError(t, err)

		acceptor.store = store
		err := acceptor.RegisterSidecar(context.Background(), tc.ticket)

		if tc.expectedErr == "" {
			require.NoError(t, err)

			tc.check(t, tc.ticket)
		} else {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		}

		cleanup()
	}
}
