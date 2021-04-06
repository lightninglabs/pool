package sidecar

import (
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/stretchr/testify/require"
)

var (
	hardcodedTicket = "sidecarAAQgHBgUEAwIBAAIBYwMBAgp_CwgAAAAAAAADCQwIAA" +
		"AAAAAAA3gNBAAAB-AOIQLVLm5gAB7Vh7eEvz8Y3CkF2DsvNuSWJj8oOxp1iS" +
		"h5xw9AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACwAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAFhRMFSEC1S5uYAAe1Ye3hL8_GNwpBd" +
		"g7LzbkliY_KDsadYkoeccWIQMZ1hb2o3OyT-XUX7KWS-pAVBloIPQe8aCTqc" +
		"jiNOV89xcEAAAAAB5kHyALFiEsAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAACBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGMAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAISgiKSBjWE0AAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAHcU6Y4="
)

// TestEncodeDecode tests that a ticket can be encoded and decoded from/to a
// string and back.
func TestEncodeDecode(t *testing.T) {
	// Test with all struct members set.
	ticketMaximal := &Ticket{
		ID:      [8]byte{7, 6, 5, 4, 3, 2, 1, 0},
		Version: Version(99),
		State:   StateRegistered,
		Offer: Offer{
			Capacity:            777,
			PushAmt:             888,
			LeaseDurationBlocks: 2016,
			SignPubKey:          testPubKey,
			SigOfferDigest: &btcec.Signature{
				R: new(big.Int).SetInt64(44),
				S: new(big.Int).SetInt64(22),
			},
		},
		Recipient: &Recipient{
			NodePubKey:     testPubKey,
			MultiSigPubKey: testPubKey2,
		},
		Order: &Order{
			BidNonce: [32]byte{11, 22, 33, 44},
			SigOrderDigest: &btcec.Signature{
				R: new(big.Int).SetInt64(99),
				S: new(big.Int).SetInt64(33),
			},
		},
		Execution: &Execution{
			PendingChannelID: [32]byte{99, 88, 77},
		},
	}

	serialized, err := EncodeToString(ticketMaximal)
	require.NoError(t, err)

	deserializedTicket, err := DecodeString(serialized)
	require.NoError(t, err)
	require.Equal(t, ticketMaximal, deserializedTicket)

	// Make sure nothing changed in the encoding without us noticing by
	// comparing it to a hard coded version.
	deserializedTicket, err = DecodeString(hardcodedTicket)
	require.NoError(t, err)
	require.Equal(t, ticketMaximal, deserializedTicket)
}
