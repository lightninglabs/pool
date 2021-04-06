package sidecar

import (
	"context"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

var (
	_, providerPubKey = btcec.PrivKeyFromBytes(btcec.S256(), []byte{0x02})
	testOfferSig      = &btcec.Signature{
		R: new(big.Int).SetInt64(44),
		S: new(big.Int).SetInt64(22),
	}
)

// TestSignOffer makes sure that a sidecar ticket's offer part can be signed
// correctly.
func TestSignOffer(t *testing.T) {
	t.Parallel()

	mockSigner := test.NewMockSigner()
	mockSigner.Signature = testOfferSig.Serialize()

	testCases := []struct {
		name        string
		ticket      *Ticket
		expectedErr string
	}{{
		name:        "no ticket",
		expectedErr: "ticket is in invalid state",
	}, {
		name: "non offered ticket",
		ticket: &Ticket{
			State: StateCreated,
		},
		expectedErr: "ticket is in invalid state",
	}, {
		name: "offered ticket with no sign pubkey",
		ticket: &Ticket{
			State: StateOffered,
			Offer: Offer{},
		},
		expectedErr: "offer in ticket is not in expected state to be",
	}, {
		name: "offered ticket has signature",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOffered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
		},
		expectedErr: "offer in ticket is not in expected state to be",
	}, {
		name: "all valid",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOffered,
			Offer: Offer{
				SignPubKey: providerPubKey,
			},
		},
		expectedErr: "",
	}}

	for _, tc := range testCases {
		tc := tc

		if tc.ticket != nil {
			digest, err := tc.ticket.OfferDigest()
			require.NoError(t, err)
			mockSigner.SignatureMsg = string(digest[:])
		}

		err := SignOffer(
			context.Background(), tc.ticket, keychain.KeyLocator{},
			mockSigner,
		)

		if tc.expectedErr == "" {
			require.NoError(t, err)
			require.Equal(
				t, testOfferSig, tc.ticket.Offer.SigOfferDigest,
			)
		} else {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		}
	}
}

// TestVerifyOffer makes sure that a sidecar ticket's offer part can be verified
// correctly.
func TestVerifyOffer(t *testing.T) {
	t.Parallel()

	mockSigner := test.NewMockSigner()
	mockSigner.Signature = testOfferSig.Serialize()

	testCases := []struct {
		name        string
		ticket      *Ticket
		expectedErr string
	}{{
		name:        "no ticket",
		expectedErr: "ticket is in invalid state",
	}, {
		name: "non offered ticket",
		ticket: &Ticket{
			State: StateCreated,
		},
		expectedErr: "ticket is in invalid state",
	}, {
		name: "offered ticket with no sign pubkey",
		ticket: &Ticket{
			State: StateOffered,
			Offer: Offer{},
		},
		expectedErr: "offer in ticket is not signed",
	}, {
		name: "invalid sig",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOffered,
			Offer: Offer{
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
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOffered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
		},
		expectedErr: "",
	}}

	for _, tc := range testCases {
		tc := tc

		if tc.ticket != nil {
			digest, err := tc.ticket.OfferDigest()
			require.NoError(t, err)
			mockSigner.SignatureMsg = string(digest[:])
		}

		err := VerifyOffer(
			context.Background(), tc.ticket, mockSigner,
		)

		if tc.expectedErr == "" {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		}
	}
}

// TestSignOrder makes sure that a sidecar ticket's order part can be signed
// correctly.
func TestSignOrder(t *testing.T) {
	t.Parallel()

	mockSigner := test.NewMockSigner()
	mockSigner.Signature = testOfferSig.Serialize()

	testCases := []struct {
		name        string
		ticket      *Ticket
		expectedErr string
	}{{
		name:        "no ticket",
		expectedErr: "ticket is in invalid state",
	}, {
		name: "non registered ticket",
		ticket: &Ticket{
			State: StateCreated,
		},
		expectedErr: "ticket is in invalid state",
	}, {
		name: "non signed offer",
		ticket: &Ticket{
			State: StateRegistered,
			Offer: Offer{
				SignPubKey: providerPubKey,
			},
		},
		expectedErr: "offer in ticket is not signed",
	}, {
		name: "all valid",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateRegistered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
		},
		expectedErr: "",
	}}

	for _, tc := range testCases {
		tc := tc

		if tc.ticket != nil {
			digest, err := tc.ticket.OfferDigest()
			require.NoError(t, err)
			mockSigner.SignatureMsg = string(digest[:])
		}

		err := SignOrder(
			context.Background(), tc.ticket, [32]byte{},
			keychain.KeyLocator{}, mockSigner,
		)

		if tc.expectedErr == "" {
			require.NoError(t, err)

			require.NotNil(t, tc.ticket.Order)
			require.Equal(
				t, testOfferSig, tc.ticket.Order.SigOrderDigest,
			)
			require.Equal(t, StateOrdered, tc.ticket.State)
		} else {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectedErr)
		}
	}
}

// TestVerifyOrder makes sure that a sidecar ticket's order part can be verified
// correctly.
func TestVerifyOrder(t *testing.T) {
	t.Parallel()

	mockSigner := test.NewMockSigner()
	mockSigner.Signature = testOfferSig.Serialize()

	testCases := []struct {
		name        string
		ticket      *Ticket
		expectedErr string
	}{{
		name:        "no ticket",
		expectedErr: "ticket is in invalid state",
	}, {
		name: "non ordered ticket",
		ticket: &Ticket{
			State: StateOffered,
		},
		expectedErr: "ticket is in invalid state",
	}, {
		name: "invalid sig",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOrdered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
			Order: &Order{
				SigOrderDigest: &btcec.Signature{
					R: new(big.Int).SetInt64(33),
					S: new(big.Int).SetInt64(33),
				},
				BidNonce: [32]byte{1, 2, 3},
			},
		},
		expectedErr: "signature not valid for public key",
	}, {
		name: "ordered ticket with no signature",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOrdered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
			Order: &Order{
				BidNonce: [32]byte{1, 2, 3},
			},
		},
		expectedErr: "order in ticket is not signed",
	}, {
		name: "empty nonce",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOrdered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
			Order: &Order{
				SigOrderDigest: testOfferSig,
			},
		},
		expectedErr: "nonce in order part of ticket is empty",
	}, {
		name: "all valid",
		ticket: &Ticket{
			ID:    [8]byte{1, 2, 3, 4},
			State: StateOrdered,
			Offer: Offer{
				SignPubKey:     providerPubKey,
				SigOfferDigest: testOfferSig,
			},
			Order: &Order{
				SigOrderDigest: testOfferSig,
				BidNonce:       [32]byte{1, 2, 3},
			},
		},
		expectedErr: "",
	}}

	for _, tc := range testCases {
		tc := tc

		if tc.ticket != nil && tc.ticket.Order != nil {
			digest, err := tc.ticket.OrderDigest()
			require.NoError(t, err)
			mockSigner.SignatureMsg = string(digest[:])
		}

		err := VerifyOrder(
			context.Background(), tc.ticket, mockSigner,
		)

		if tc.expectedErr == "" {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
			require.Contains(t, err.Error(), tc.expectedErr, tc.name)
		}
	}
}
