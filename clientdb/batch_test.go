package clientdb

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightninglabs/agora/client/order"
)

// TestPersistBatchResult tests that a batch result can be persisted correctly.
func TestPersistBatchResult(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		expectedErr string
		runTest     func(db *DB, a *order.Ask, b *order.Bid,
			acct *account.Account) error
	}{
		{
			name:        "len mismatch order",
			expectedErr: "order modifier length mismatch",
			runTest: func(db *DB, a *order.Ask, _ *order.Bid,
				_ *account.Account) error {

				return db.PersistBatchResult(
					[]order.Nonce{a.Nonce()}, nil, nil, nil,
				)
			},
		},
		{
			name:        "len mismatch account",
			expectedErr: "account modifier length mismatch",
			runTest: func(db *DB, a *order.Ask, _ *order.Bid,
				acct *account.Account) error {

				return db.PersistBatchResult(
					nil, nil, []*account.Account{acct}, nil,
				)
			},
		},
		{
			name:        "non-existent order",
			expectedErr: ErrNoOrder.Error(),
			runTest: func(db *DB, a *order.Ask, _ *order.Bid,
				acct *account.Account) error {

				modifiers := [][]order.Modifier{{
					order.StateModifier(order.StateExecuted),
				}}
				return db.PersistBatchResult(
					[]order.Nonce{{0, 1, 2}}, modifiers,
					nil, nil,
				)
			},
		},
		{
			name:        "non-existent account",
			expectedErr: ErrAccountNotFound.Error(),
			runTest: func(db *DB, a *order.Ask, _ *order.Bid,
				acct *account.Account) error {

				acct.TraderKey.PubKey = clmscript.IncrementKey(
					acct.TraderKey.PubKey,
				)
				modifiers := [][]account.Modifier{{
					account.StateModifier(account.StateClosed),
				}}
				return db.PersistBatchResult(
					nil, nil, []*account.Account{acct},
					modifiers,
				)
			},
		},
		{
			name:        "happy path",
			expectedErr: "",
			runTest: func(db *DB, a *order.Ask, b *order.Bid,
				acct *account.Account) error {

				// Store some changes to the orders and account.
				orders := []order.Nonce{a.Nonce(), b.Nonce()}
				orderModifiers := [][]order.Modifier{
					{order.UnitsFulfilledModifier(42)},
					{order.UnitsFulfilledModifier(21)},
				}
				accounts := []*account.Account{acct}
				acctModifiers := [][]account.Modifier{{
					account.StateModifier(
						account.StatePendingOpen,
					),
				}}
				err := db.PersistBatchResult(
					orders, orderModifiers,
					accounts, acctModifiers,
				)
				if err != nil {
					return err
				}

				// Verify get the right result.
				o1, err := db.GetOrder(a.Nonce())
				if err != nil {
					return err
				}
				if o1.Details().UnitsUnfulfilled != 42 {
					return fmt.Errorf("unexpected number "+
						"of unfulfilled units, got %d "+
						"wanted %d",
						o1.Details().UnitsUnfulfilled,
						42)
				}
				o2, err := db.GetOrder(b.Nonce())
				if err != nil {
					return err
				}
				if o2.Details().UnitsUnfulfilled != 21 {
					return fmt.Errorf("unexpected number "+
						"of unfulfilled units, got %d "+
						"wanted %d",
						o2.Details().UnitsUnfulfilled,
						21)
				}
				a2, err := db.Account(acct.TraderKey.PubKey)
				if err != nil {
					return err
				}
				if a2.State != account.StatePendingOpen {
					return fmt.Errorf("unexpected state "+
						"of account, got %d wanted %d",
						a2.State,
						account.StatePendingOpen)
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Create a new store every time to make sure we start
			// with a clean slate.
			store, cleanup := newTestDB(t)
			defer cleanup()

			// Create a test account and two matching orders that
			// spend from that account. This never happens in real
			// life but is good enough to just test the database.
			acct := &account.Account{
				Value:         btcutil.SatoshiPerBitcoin,
				Expiry:        1337,
				TraderKey:     testTraderKeyDesc,
				AuctioneerKey: testAuctioneerKey,
				BatchKey:      testBatchKey,
				Secret:        sharedSecret,
				State:         account.StateOpen,
				HeightHint:    1,
			}
			ask := &order.Ask{
				Kit:         *dummyOrder(t, 900000),
				MaxDuration: 1337,
			}
			ask.State = order.StateSubmitted
			bid := &order.Bid{
				Kit:         *dummyOrder(t, 900000),
				MinDuration: 1337,
			}
			bid.State = order.StateSubmitted

			// Prepare the DB state by storing our test account and
			// orders.
			err := store.AddAccount(acct)
			if err != nil {
				t.Fatalf("error storing test account: %v", err)
			}
			err = store.SubmitOrder(ask)
			if err != nil {
				t.Fatalf("error storing test ask: %v", err)
			}
			err = store.SubmitOrder(bid)
			if err != nil {
				t.Fatalf("error storing test bid: %v", err)
			}

			// Run the test case and verify the result.
			err = tc.runTest(store, ask, bid, acct)
			if (err == nil && tc.expectedErr != "") ||
				(err != nil && err.Error() != tc.expectedErr) {

				t.Fatalf("unexpected error '%s', expected '%s'",
					err.Error(), tc.expectedErr)
			}
		})
	}
}
