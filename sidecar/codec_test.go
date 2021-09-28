package sidecar

import (
	"math/big"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil/base58"
	"github.com/stretchr/testify/require"
)

var (
	hardcodedTicket = "sidecar13HtPsD1UiB2EA4ZfUbnVejG1Pu3p1GW2ydq65NZBuQg" +
		"TaD9LRESHtKasXwNrQPYqkwkMyJNFiNoBig4PBQQKoKeXz5jHWXZ3pMZNX5" +
		"JW2FXyf6zKTPWthRQczNQuf2pM8ppjTqHK6tYTEReZk7pFynYJoHnAeBtix" +
		"28k2PiRSz5qVVmHpWHWFgVjxihEmcD1T1V1g5LwzbXTj6Ype6a5r18BuMDF" +
		"8w7qxEim535S7tjU2MtbX1XtKBSq8TP3JY2TCy52yShkTgxSUTQ5mboE42h" +
		"TDosSJ71QbZNDGEPERJLhXDGn56Gae45WS9AaZ2v71mxYfZGFiRCmsR8ZL9" +
		"KzYR7nHi96GYZxYjP6pUEeyXyAr31w1uhFeAx64RHMpVXTo4ihppUWgguiV" +
		"8Abjr3UHQa4eRzhVk2uJQT3XDsAPbCP5wzPpaQdm32VSYmgHJXBYUEgWGw5" +
		"uavxES39FxvPPFEJKZLFkCJsrBXqKzUjMqdztiSmaz5Rcr"
)

// TestEncodeDecode tests that a ticket can be encoded and decoded from/to a
// string and back.
func TestEncodeDecode(t *testing.T) {
	t.Parallel()

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
			Auto: true,
		},
		Recipient: &Recipient{
			NodePubKey:       testPubKey,
			MultiSigPubKey:   testPubKey2,
			MultiSigKeyIndex: 77,
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

// TestDecodeRandomInputs tests that given an input string that may or may not
// conform to our expectations, feeding in the input string into decoding
// doesn't result in a crash of the routine.
func TestDecodeRandomInputs(t *testing.T) {
	t.Parallel()

	rand.Seed(time.Now().Unix())

	type stringInput [500]byte
	mainScenario := func(s stringInput) bool {
		// We'll take the string and then slice into a random
		// sub-string of it, then append our usual prefix w/ a 50/50
		// chance.
		strLength := rand.Int31n(int32(len(s))) // nolint:gosec

		inputStr := base58.Encode(s[:strLength])

		includePrefix := rand.Int() % 2 // nolint:gosec
		if includePrefix == 1 {
			inputStr = sidecarPrefix + inputStr
		}

		_, _ = DecodeString(inputStr)
		return true
	}

	if err := quick.Check(mainScenario, nil); err != nil {
		t.Fatalf("quick check failed: %v", err)
	}
}
