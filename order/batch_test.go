package order

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

var (
	initialKeyBytes, _ = hex.DecodeString(
		"034ba8df633bc689472b1fecb9ca0a7d9462467477c975108029f87ca6fb7fc1c1",
	)
	initialKey, _ = btcec.ParsePubKey(initialKeyBytes)

	unrelatedKeyBytes, _ = hex.DecodeString(
		"02975a91ca82550b19355b4826e67c6d2b064f9ec62fb4fd44e044ffbeb8d35127",
	)
	unrelatedKey, _ = btcec.ParsePubKey(unrelatedKeyBytes)
)

func TestDecrementingBatchIDs(t *testing.T) {
	// Test single and one step apart keys.
	require.Len(t, DecrementingBatchIDs(initialKey, initialKey), 1)
	require.Len(t, DecrementingBatchIDs(
		poolscript.IncrementKey(initialKey), initialKey,
	), 2)

	// Try 10 keys now.
	tenKey := initialKey
	for i := 0; i < 10; i++ {
		tenKey = poolscript.IncrementKey(tenKey)
	}
	ids := DecrementingBatchIDs(tenKey, initialKey)
	require.Len(t, ids, 11)
	require.Equal(t, NewBatchID(tenKey), ids[0])
	require.Equal(t, NewBatchID(initialKey), ids[10])

	// Make sure we don't loop forever if unrelated keys are used.
	ids = DecrementingBatchIDs(unrelatedKey, initialKey)

	// We don't use require.Len() here, otherwise if this ever fails, the
	// library prints the whole result which is huuuuuge.
	require.Equal(t, MaxBatchIDHistoryLookup, len(ids))
}

// TestChannelOutput makes sure the correct keys are used for looking up an
// expected channel output.
func TestChannelOutput(t *testing.T) {
	var (
		bidKeyIndex      int32 = 101
		sidecarKeyIndex  int32 = 102
		_, pubKeyAsk           = test.CreateKey(100)
		_, pubKeyBid           = test.CreateKey(bidKeyIndex)
		_, pubKeySidecar       = test.CreateKey(sidecarKeyIndex)
		mockWallet             = test.NewMockWalletKit()
		askNonce               = Nonce{1, 2, 3}
		bidNonce               = Nonce{3, 2, 1}
		batchTx                = &wire.MsgTx{
			TxOut: []*wire.TxOut{{}},
		}
	)

	askKit := NewKit(askNonce)
	matchedAsk := &MatchedOrder{
		Order:       &Ask{Kit: *askKit},
		UnitsFilled: 4,
	}
	copy(matchedAsk.MultiSigKey[:], pubKeyAsk.SerializeCompressed())

	// First test is a normal bid where the funding key should be derived
	// from the wallet
	bid := &Bid{
		Kit: newKitFromTemplate(bidNonce, &Kit{
			MultiSigKeyLocator: keychain.KeyLocator{
				Family: 1234,
				Index:  uint32(bidKeyIndex),
			},
			Units:         4,
			LeaseDuration: 12345,
		}),
	}
	_, batchTx.TxOut[0], _ = input.GenFundingPkScript(
		pubKeyBid.SerializeCompressed(),
		matchedAsk.MultiSigKey[:], int64(4*BaseSupplyUnit),
	)
	out, idx, err := ChannelOutput(batchTx, mockWallet, bid, matchedAsk)
	require.NoError(t, err)
	require.Equal(t, uint32(0), idx)
	require.Equal(t, batchTx.TxOut[0], out)

	// And the second test is with a sidecar channel bid.
	ticket, err := sidecar.NewTicket(
		sidecar.VersionDefault, 400_000, 0, 12345, pubKeyBid, false,
	)
	require.NoError(t, err)
	ticket.Recipient = &sidecar.Recipient{
		MultiSigPubKey:   pubKeySidecar,
		MultiSigKeyIndex: uint32(sidecarKeyIndex),
	}
	bid = &Bid{
		Kit: newKitFromTemplate(bidNonce, &Kit{
			MultiSigKeyLocator: keychain.KeyLocator{
				Family: 1234,
				Index:  uint32(bidKeyIndex),
			},
			Units:         4,
			LeaseDuration: 12345,
		}),
		SidecarTicket: ticket,
	}
	_, batchTx.TxOut[0], _ = input.GenFundingPkScript(
		pubKeySidecar.SerializeCompressed(),
		matchedAsk.MultiSigKey[:], int64(4*BaseSupplyUnit),
	)

	out, idx, err = ChannelOutput(batchTx, mockWallet, bid, matchedAsk)
	require.NoError(t, err)
	require.Equal(t, uint32(0), idx)
	require.Equal(t, batchTx.TxOut[0], out)
}
