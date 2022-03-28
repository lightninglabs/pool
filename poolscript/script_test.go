package poolscript

import (
	"encoding/hex"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

const (
	numOperations          = 10000
	numOperationsQuickTest = 1000
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
	// by G 10000 times.
	batchKeyIncremented10kTimesBytes, _ = hex.DecodeString(
		"03d9dfc4971c9cbabb1b9a4c991914211aa21286e007c15d7e9d828da0b8" +
			"f07763",
	)
)

// TestIncrementDecrementKey makes sure that incrementing and decrementing an EC
// public key are inverse operations to each other.
func TestIncrementDecrementKey(t *testing.T) {
	t.Parallel()

	rand.Seed(time.Now().Unix())

	type byteInput [32]byte
	mainScenario := func(b byteInput) bool {
		_, randomStartBatchKey := btcec.PrivKeyFromBytes(b[:])

		// Increment the key numOperations times.
		currentKey := randomStartBatchKey
		for i := 0; i < numOperationsQuickTest; i++ {
			currentKey = IncrementKey(currentKey)
		}

		// Decrement the key again.
		for i := 0; i < numOperationsQuickTest; i++ {
			currentKey = DecrementKey(currentKey)
		}

		// We should arrive at the same start key again.
		return randomStartBatchKey.IsEqual(currentKey)
	}

	require.NoError(t, quick.Check(mainScenario, nil))
}

// TestIncrementBatchKey tests that incrementing the static, hard-coded batch
// key 1000 times gives a specific key and decrementing the same number of times
// gives the batch key again.
func TestIncrementBatchKey(t *testing.T) {
	t.Parallel()

	startBatchKey, err := btcec.ParsePubKey(initialBatchKeyBytes)
	require.NoError(t, err)

	batchKeyIncremented10kTimes, err := btcec.ParsePubKey(
		batchKeyIncremented10kTimesBytes,
	)
	require.NoError(t, err)

	currentKey := startBatchKey
	for i := 0; i < numOperations; i++ {
		currentKey = IncrementKey(currentKey)
	}

	require.Equal(t, batchKeyIncremented10kTimes, currentKey)

	for i := 0; i < numOperations; i++ {
		currentKey = DecrementKey(currentKey)
	}

	require.Equal(t, startBatchKey, currentKey)
}
