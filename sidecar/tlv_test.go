package sidecar

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/stretchr/testify/require"
)

var (
	testPubKeyRaw, _ = hex.DecodeString(
		"02d52e6e60001ed587b784bf3f18dc2905d83b2f36e496263f283b1a7589" +
			"2879c7",
	)
	testPubKeyRaw2, _ = hex.DecodeString(
		"0319d616f6a373b24fe5d45fb2964bea4054196820f41ef1a093a9c8e234" +
			"e57cf7",
	)
	testPubKey, _  = btcec.ParsePubKey(testPubKeyRaw, btcec.S256())
	testPubKey2, _ = btcec.ParsePubKey(testPubKeyRaw2, btcec.S256())
)

func TestSerializeTicket(t *testing.T) {
	var buf bytes.Buffer

	// Test serialization of nil struct members.
	ticketMinimal := &Ticket{
		ID:      [8]byte{7, 6, 5, 4, 3, 2, 1, 0},
		Version: Version(99),
		State:   StateRegistered,
	}
	err := SerializeTicket(&buf, ticketMinimal)
	require.NoError(t, err)

	deserializedTicket, err := DeserializeTicket(
		bytes.NewReader(buf.Bytes()),
	)
	require.NoError(t, err)
	require.Equal(t, ticketMinimal, deserializedTicket)

	buf.Reset()

	// Test serialization of nil sub struct members.
	ticketEmptySubStructs := &Ticket{
		ID:        [8]byte{7, 6, 5, 4, 3, 2, 1, 0},
		Version:   Version(99),
		State:     StateRegistered,
		Offer:     Offer{},
		Recipient: &Recipient{},
		Order:     &Order{},
		Execution: &Execution{},
	}
	err = SerializeTicket(&buf, ticketEmptySubStructs)
	require.NoError(t, err)

	deserializedTicket, err = DeserializeTicket(
		bytes.NewReader(buf.Bytes()),
	)
	require.NoError(t, err)
	require.Equal(t, ticketEmptySubStructs, deserializedTicket)

	buf.Reset()

	// Test with all struct members set.
	ticketMaximal := &Ticket{
		ID:      [8]byte{7, 6, 5, 4, 3, 2, 1, 0},
		Version: Version(99),
		State:   StateRegistered,
		Offer: Offer{
			Capacity:   777,
			PushAmt:    888,
			SignPubKey: testPubKey,
			SigOfferDigest: &btcec.Signature{
				R: new(big.Int).SetInt64(22),
				S: new(big.Int).SetInt64(55),
			},
			Auto: true,
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
	err = SerializeTicket(&buf, ticketMaximal)
	require.NoError(t, err)

	deserializedTicket, err = DeserializeTicket(
		bytes.NewReader(buf.Bytes()),
	)
	require.NoError(t, err)
	require.Equal(t, ticketMaximal, deserializedTicket)
}
