package clientdb

import (
	"fmt"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
	"go.etcd.io/bbolt"
)

var (
	testBatchID = order.BatchID{0x01, 0x02, 0x03}

	testBatchTx = &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Index: 1,
				},
				SignatureScript: []byte("aaa"),
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value:    4444,
				PkScript: []byte("ddd"),
			},
		},
	}

	testBatch = &order.Batch{
		ID:           testBatchID,
		ExecutionFee: terms.NewLinearFeeSchedule(10, 100),
		BatchTX:      testBatchTx,
	}

	testCases = []struct {
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

				return db.StorePendingBatch(
					testBatch,
					[]order.Nonce{a.Nonce()}, nil, nil, nil,
				)
			},
		},
		{
			name:        "len mismatch account",
			expectedErr: "account modifier length mismatch",
			runTest: func(db *DB, a *order.Ask, _ *order.Bid,
				acct *account.Account) error {

				return db.StorePendingBatch(
					testBatch, nil, nil,
					[]*account.Account{acct}, nil,
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
				return db.StorePendingBatch(
					testBatch,
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

				acct.TraderKey = &keychain.KeyDescriptor{
					KeyLocator: acct.TraderKey.KeyLocator,
					PubKey:     testBatchKey,
				}
				modifiers := [][]account.Modifier{{
					account.StateModifier(account.StateClosed),
				}}
				return db.StorePendingBatch(
					testBatch, nil, nil,
					[]*account.Account{acct}, modifiers,
				)
			},
		},
		{
			name:        "no pending batch",
			expectedErr: account.ErrNoPendingBatch.Error(),
			runTest: func(db *DB, a *order.Ask, b *order.Bid,
				acct *account.Account) error {

				_, err := db.PendingBatchSnapshot()
				return err
			},
		},
		{
			name:        "mark batch complete without pending",
			expectedErr: account.ErrNoPendingBatch.Error(),
			runTest: func(db *DB, a *order.Ask, b *order.Bid,
				acct *account.Account) error {

				return db.MarkBatchComplete()
			},
		},
		{
			name:        "happy path",
			expectedErr: "",
			runTest: func(db *DB, a *order.Ask, b *order.Bid,
				acct *account.Account) error {

				// Store some changes to the orders and account.
				orders := []order.Order{a, b}
				orderNonces := []order.Nonce{a.Nonce(), b.Nonce()}
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
				err := db.StorePendingBatch(
					testBatch, orderNonces,
					orderModifiers, accounts, acctModifiers,
				)
				if err != nil {
					return err
				}

				// The pending batch ID and transaction should
				// reflect correctly.
				dbSnapshot, err := db.PendingBatchSnapshot()
				if err != nil {
					return err
				}
				if dbSnapshot.BatchID != testBatchID {
					return fmt.Errorf("expected pending "+
						"batch id %x, got %x",
						testBatchID, dbSnapshot.BatchID)
				}
				if dbSnapshot.BatchTX.TxHash() != testBatch.BatchTX.TxHash() {
					return fmt.Errorf("expected pending "+
						"batch tx %v, got %v",
						testBatch.BatchTX.TxHash(),
						dbSnapshot.BatchTX.TxHash())
				}

				// Verify the updates have not been applied to
				// disk yet.
				err = checkUpdate(
					db, a.Nonce(), b.Nonce(),
					a.Details().UnitsUnfulfilled,
					b.Details().UnitsUnfulfilled,
					acct.TraderKey.PubKey, acct.State,
				)
				if err != nil {
					return err
				}

				// Mark the batch as complete.
				if err := db.MarkBatchComplete(); err != nil {
					return err
				}

				// Verify the updates have been applied to disk
				// properly.
				for i, a := range accounts {
					for _, modifier := range acctModifiers[i] {
						modifier(a)
					}
				}
				for i, o := range orders {
					for _, modifier := range orderModifiers[i] {
						modifier(o.Details())
					}
				}
				return checkUpdate(
					db, a.Nonce(), b.Nonce(),
					a.Details().UnitsUnfulfilled,
					b.Details().UnitsUnfulfilled,
					acct.TraderKey.PubKey, acct.State,
				)
			},
		},
		{
			name:        "overwrite pending batch",
			expectedErr: "",
			runTest: func(db *DB, a *order.Ask, b *order.Bid,
				acct *account.Account) error {

				// First, we'll store a version of the batch
				// that updates all order and accounts.
				orderModifier := order.UnitsFulfilledModifier(42)
				err := db.StorePendingBatch(
					testBatch,
					[]order.Nonce{a.Nonce(), b.Nonce()},
					[][]order.Modifier{
						{orderModifier}, {orderModifier},
					},
					[]*account.Account{acct},
					[][]account.Modifier{{account.StateModifier(
						account.StatePendingUpdate,
					)}},
				)
				if err != nil {
					return err
				}

				// Then, we'll assume the batch was overwritten,
				// and now only the ask order is part of it.
				err = db.StorePendingBatch(
					testBatch,
					[]order.Nonce{a.Nonce()},
					[][]order.Modifier{{orderModifier}},
					nil, nil,
				)
				if err != nil {
					return err
				}

				// Mark the batch as complete. We should only
				// see the update for our ask order applied, but
				// not the rest.
				if err := db.MarkBatchComplete(); err != nil {
					return err
				}

				return checkUpdate(
					db, a.Nonce(), b.Nonce(), 42,
					b.UnitsUnfulfilled,
					acct.TraderKey.PubKey, acct.State,
				)
			},
		},
	}
)

// checkUpdate is a helper closure we'll use to check whether the account and
// order updates of a batch have been applied.
func checkUpdate(db *DB, askNonce, bidNonce order.Nonce,
	askUnitsUnfulfilled, bidUnitsUnfulfilled order.SupplyUnit,
	accountKey *btcec.PublicKey, accountState account.State) error {

	o1, err := db.GetOrder(askNonce)
	if err != nil {
		return err
	}
	if o1.Details().UnitsUnfulfilled != askUnitsUnfulfilled {
		return fmt.Errorf("unexpected number of unfulfilled "+
			"units, got %d wanted %d",
			o1.Details().UnitsUnfulfilled, askUnitsUnfulfilled)
	}

	o2, err := db.GetOrder(bidNonce)
	if err != nil {
		return err
	}
	if o2.Details().UnitsUnfulfilled != bidUnitsUnfulfilled {
		return fmt.Errorf("unexpected number of unfulfilled "+
			"units, got %d "+"wanted %d",
			o2.Details().UnitsUnfulfilled, bidUnitsUnfulfilled)
	}

	a2, err := db.Account(accountKey)
	if err != nil {
		return err
	}
	if a2.State != accountState {
		return fmt.Errorf("unexpected state of account, got "+
			"%v wanted %v", a2.State, accountState)
	}

	return nil
}

// TestPersistBatchResult tests that a batch result can be persisted correctly.
func TestPersistBatchResult(t *testing.T) {
	t.Parallel()

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
			accountOutput, err := acct.Output()
			if err != nil {
				t.Fatal(err)
			}
			acct.LatestTx = &wire.MsgTx{
				Version: 2,
				TxIn: []*wire.TxIn{
					{
						PreviousOutPoint: wire.OutPoint{
							Index: 1,
						},
						SignatureScript: []byte{0x40},
					},
				},
				TxOut: []*wire.TxOut{accountOutput},
			}
			ask := &order.Ask{
				Kit: *dummyOrder(900000, 1337),
			}
			ask.State = order.StateSubmitted
			bid := &order.Bid{
				Kit: *dummyOrder(900000, 1337),
			}
			bid.State = order.StateSubmitted

			// Prepare the DB state by storing our test account and
			// orders.
			err = store.AddAccount(acct)
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
			switch {
			case err == nil && tc.expectedErr != "":
			case err != nil && tc.expectedErr != "":
				if strings.Contains(err.Error(), tc.expectedErr) {
					return
				}
			case err != nil && tc.expectedErr == "":
			default:
				return
			}

			t.Fatalf("unexpected error '%s', expected '%s'",
				err.Error(), tc.expectedErr)
		})
	}
}

// TestDeletePendingBatch ensures that all references of a pending batch have
// been removed after an invocation of DeletePendingBatch.
func TestDeletePendingBatch(t *testing.T) {
	t.Parallel()

	db, cleanup := newTestDB(t)
	defer cleanup()

	// Helper closure to assert that the sub-keys and sub-buckets of the
	// root batch bucket exist or not.
	assertPendingBatch := func(exists bool) {
		t.Helper()

		err := db.View(func(tx *bbolt.Tx) error {
			root := tx.Bucket(batchBucketKey)

			if (root.Get(pendingBatchIDKey) != nil) != exists {
				return fmt.Errorf("found unexpected key %v",
					string(pendingBatchIDKey))
			}
			if (root.Bucket(pendingBatchAccountsBucketKey) != nil) != exists {
				return fmt.Errorf("found unexpected bucket %v",
					string(pendingBatchAccountsBucketKey))
			}
			if (root.Bucket(pendingBatchOrdersBucketKey) != nil) != exists {
				return fmt.Errorf("found unexpected bucket %v",
					string(pendingBatchOrdersBucketKey))
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Store a pending batch. We should expect to find valid values for all
	// sub-keys and sub-buckets.
	err := db.StorePendingBatch(testBatch, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unable to store pending batch: %v", err)
	}
	assertPendingBatch(true)

	// Now, delete the pending batch. We should expect to _not_ find any
	// existing sub-keys or sub-buckets.
	if err := db.DeletePendingBatch(); err != nil {
		t.Fatalf("unable to delete pending batch: %v", err)
	}
	assertPendingBatch(false)
}
