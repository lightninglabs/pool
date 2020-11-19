package order

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/stretchr/testify/require"
)

var (
	initialKeyBytes, _ = hex.DecodeString(
		"034ba8df633bc689472b1fecb9ca0a7d9462467477c975108029f87ca6fb7fc1c1",
	)
	initialKey, _ = btcec.ParsePubKey(initialKeyBytes, btcec.S256())

	unrelatedKeyBytes, _ = hex.DecodeString(
		"02975a91ca82550b19355b4826e67c6d2b064f9ec62fb4fd44e044ffbeb8d35127",
	)
	unrelatedKey, _ = btcec.ParsePubKey(unrelatedKeyBytes, btcec.S256())
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
