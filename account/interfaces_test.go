package account

import (
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/wire"
)

// TestAccountCopy ensures that a deep copy of an account is constructed
// successfully.
func TestAccountCopy(t *testing.T) {
	t.Parallel()

	a := &Account{
		Value:         1,
		Expiry:        2,
		TraderKey:     testTraderKeyDesc,
		AuctioneerKey: testAuctioneerKey,
		BatchKey:      testBatchKey,
		Secret:        [32]byte{0x1, 0x2, 0x3},
		State:         StateOpen,
		HeightHint:    1,
		OutPoint:      wire.OutPoint{Index: 1},
		LatestTx:      wire.NewMsgTx(2),
		Version:       VersionTaprootEnabled,
	}

	a.Value = 2
	aCopy := a.Copy(ValueModifier(2))

	if !reflect.DeepEqual(aCopy, a) {
		t.Fatal("deep copy with modifier does not match expected result")
	}
}
