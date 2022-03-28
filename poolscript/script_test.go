package poolscript

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

const (
	numOperations = 1000
)

var (
	// initialBatchKey is the hard coded starting point for the auctioneer's
	// batch key in every environment. Copied here to avoid circular
	// dependency with the account package.
	initialBatchKeyBytes, _ = hex.DecodeString(
		"02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2ae66afefb8" +
			"eaf27f",
	)

	// batchKeyIncremented1kTimesBytes is the initial batch keys incremented
	// by G 1000 times.
	batchKeyIncremented1kTimesBytes, _ = hex.DecodeString(
		"0280488e115da2415389bbe07854133840de2741b31dabd60184c7f5d80c" +
			"057d79",
	)
)

// TestIncrementDecrementKey makes sure that incrementing and decrementing an EC
// public key are inverse operations to each other.
func TestIncrementDecrementKey(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	randomStartBatchKey := privKey.PubKey()

	// Increment the key numOperations times.
	currentKey := randomStartBatchKey
	for i := 0; i < numOperations; i++ {
		currentKey = IncrementKey(currentKey)
	}

	// Decrement the key again.
	for i := 0; i < numOperations; i++ {
		currentKey = DecrementKey(currentKey)
	}

	// We should arrive at the same start key again.
	require.Equal(t, randomStartBatchKey, currentKey)
}

// TestIncrementBatchKey tests that incrementing the static, hard-coded batch
// key 1000 times gives a specific key and decrementing the same number of times
// gives the batch key again.
func TestIncrementBatchKey(t *testing.T) {
	t.Parallel()

	startBatchKey, err := btcec.ParsePubKey(initialBatchKeyBytes)
	require.NoError(t, err)

	batchKeyIncremented1kTimes, err := btcec.ParsePubKey(
		batchKeyIncremented1kTimesBytes,
	)
	require.NoError(t, err)

	currentKey := startBatchKey
	for i := 0; i < numOperations; i++ {
		currentKey = IncrementKey(currentKey)
	}

	require.Equal(t, batchKeyIncremented1kTimes, currentKey)

	for i := 0; i < numOperations; i++ {
		currentKey = DecrementKey(currentKey)
	}

	require.Equal(t, startBatchKey, currentKey)
}
