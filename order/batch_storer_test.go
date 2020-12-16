package order

import (
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// TestBatchStorer makes sure a batch is prepared correctly for serialization by
// the batch storer.
func TestBatchStorer(t *testing.T) {
	t.Parallel()

	const bestHeight = 1337
	var (
		storeMock = newMockStore()
		storer    = &batchStorer{
			orderStore: storeMock,
			getAccount: storeMock.getAccount,
		}
		batchID     BatchID
		acctIDBig   [33]byte
		acctIDSmall [33]byte
	)
	copy(batchID[:], startBatchKey.SerializeCompressed())
	copy(acctIDBig[:], acctKeyBig.SerializeCompressed())
	copy(acctIDSmall[:], acctKeySmall.SerializeCompressed())

	// We'll create two accounts: A smaller one that has one ask for 4 units
	// that will be completely used up. Then a larger account that has two
	// bids that are both matched to the ask. This account is large enough
	// to be recreated. We assume here that no maker/taker fees are applied
	// and only the matched units are paid for.
	bigAcct := &account.Account{
		TraderKey: &keychain.KeyDescriptor{PubKey: acctKeyBig},
		Value:     1_000_000,
		Expiry:    144,
		State:     account.StateOpen,
		BatchKey:  startBatchKey,
	}
	smallAcct := &account.Account{
		TraderKey: &keychain.KeyDescriptor{PubKey: acctKeySmall},
		Value:     400_000,
		Expiry:    144,
		State:     account.StateOpen,
		BatchKey:  startBatchKey,
	}
	ask := &Ask{Kit: newKit(Nonce{0x01}, 4, 1)}
	bid1 := &Bid{Kit: newKit(Nonce{0x02}, 3, 1)}
	bid2 := &Bid{Kit: newKit(Nonce{0x03}, 3, 2)}
	batchTx := &wire.MsgTx{
		Version: 2,
		TxOut: []*wire.TxOut{{
			Value:    600_000,
			PkScript: []byte{77, 88, 99},
		}},
	}

	// Create a batch for us as if we were the trader for both accounts and
	// were matched against each other (not impossible but unlikely to
	// happen in the real system).
	accountDiffs := []*AccountDiff{
		{
			AccountKeyRaw: acctIDBig,
			AccountKey:    acctKeyBig,
			EndingState:   poolrpc.AccountDiff_OUTPUT_RECREATED,
			OutpointIndex: 0,
			EndingBalance: 600_000,
		},
		{
			AccountKeyRaw: acctIDSmall,
			AccountKey:    acctKeySmall,
			EndingState:   poolrpc.AccountDiff_OUTPUT_FULLY_SPENT,
			OutpointIndex: -1,
			EndingBalance: 0,
		},
	}
	matchedOrders := map[Nonce][]*MatchedOrder{
		ask.nonce: {
			{
				Order:       bid1,
				UnitsFilled: 2,
			},
			{
				Order:       bid2,
				UnitsFilled: 2,
			},
		},
		bid1.nonce: {{
			Order:       ask,
			UnitsFilled: 2,
		}},
		bid2.nonce: {{
			Order:       ask,
			UnitsFilled: 2,
		}},
	}
	batch := &Batch{
		ID:             batchID,
		Version:        DefaultBatchVersion,
		MatchedOrders:  matchedOrders,
		AccountDiffs:   accountDiffs,
		BatchTX:        batchTx,
		BatchTxFeeRate: chainfee.FeePerKwFloor,
	}

	// Create the starting database state now.
	storeMock.accounts = map[[33]byte]*account.Account{
		acctIDBig:   bigAcct,
		acctIDSmall: smallAcct,
	}
	storeMock.orders = map[Nonce]Order{
		ask.Nonce():  ask,
		bid1.Nonce(): bid1,
		bid2.Nonce(): bid2,
	}

	// Pass the assembled batch to the storer now.
	err := storer.StorePendingBatch(batch, bestHeight)
	if err != nil {
		t.Fatalf("error storing batch: %v", err)
	}

	// Because the store backend is an in-memory mock, all modifications are
	// performed on the actual instances, which makes it easy to check.
	// Check the order states first.
	if ask.State != StateExecuted {
		t.Fatalf("invalid order state, got %v wanted %v",
			ask.State, StateExecuted)
	}
	if ask.UnitsUnfulfilled != 0 {
		t.Fatalf("invalid units unfulfilled, got %d wanted %d",
			ask.UnitsUnfulfilled, 0)
	}
	if bid1.State != StatePartiallyFilled {
		t.Fatalf("invalid order state, got %v wanted %v",
			bid1.State, StatePartiallyFilled)
	}
	if bid1.UnitsUnfulfilled != 1 {
		t.Fatalf("invalid units unfulfilled, got %d wanted %d",
			bid1.UnitsUnfulfilled, 1)
	}
	if bid2.State != StateExecuted {
		t.Fatalf("invalid order state, got %v wanted %v",
			bid2.State, StateExecuted)
	}
	if bid2.UnitsUnfulfilled != 1 {
		t.Fatalf("invalid units unfulfilled, got %d wanted %d",
			bid2.UnitsUnfulfilled, 1)
	}

	// Check the account states next.
	if smallAcct.State != account.StatePendingClosed {
		t.Fatalf("invalid account state, got %d wanted %d",
			smallAcct.State, account.StatePendingClosed)
	}
	if smallAcct.Value != 0 {
		t.Fatalf("invalid account balance, got %d wanted %d",
			smallAcct.Value, 0)
	}
	if smallAcct.Expiry != 144 {
		t.Fatalf("invalid account expiry, got %d wanted %d",
			smallAcct.Value, 144)
	}
	heightHint := uint32(bestHeight + heightHintPadding)
	if smallAcct.HeightHint != heightHint {
		t.Fatalf("invalid account height hint, got %d wanted %d",
			smallAcct.Value, heightHint)
	}

	if bigAcct.State != account.StatePendingBatch {
		t.Fatalf("invalid account state, got %d wanted %d",
			bigAcct.State, account.StatePendingBatch)
	}
	if bigAcct.Value != 600_000 {
		t.Fatalf("invalid account balance, got %d wanted %d",
			bigAcct.Value, 600_000)
	}
	if bigAcct.Expiry != 144 {
		t.Fatalf("invalid account expiry, got %d wanted %d",
			bigAcct.Value, 144)
	}
	if bigAcct.HeightHint != heightHint {
		t.Fatalf("invalid account height hint, got %d wanted %d",
			bigAcct.Value, heightHint)
	}
}

func newKit(nonce Nonce, units, minUnitsMatch SupplyUnit) Kit {
	kit := NewKit(nonce)
	kit.Units = units
	kit.UnitsUnfulfilled = units
	kit.MinUnitsMatch = minUnitsMatch
	kit.State = StateSubmitted
	return *kit
}
