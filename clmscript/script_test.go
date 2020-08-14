package clmscript

import (
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/stretchr/testify/require"
)

const (
	numOperations = 1000
)

// TestIncrementDecrementKey makes sure that incrementing and decrementing an EC
// public key are inverse operations to each other.
func TestIncrementDecrementKey(t *testing.T) {
	t.Parallel()

	privKey, err := btcec.NewPrivateKey(btcec.S256())
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
