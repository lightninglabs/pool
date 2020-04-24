package clientdb

import (
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/order"
)

var (
	// batchBucketKey is the top level bucket where we can find all
	// information about batches we've participated in.
	batchBucketKey = []byte("batch")

	// pendingBatchIDKey is a key we'll use to store the ID of a batch we're
	// currently participating in.
	pendingBatchIDKey = []byte("pending-id")

	zeroBatchID order.BatchID
)

// StorePendingBatch atomically updates all modified orders/accounts as a result
// of a pending batch. If any single operation fails, the whole set of changes
// is rolled back.
func (db *DB) StorePendingBatch(batchID order.BatchID, orders []order.Nonce,
	orderModifiers [][]order.Modifier, accounts []*account.Account,
	accountModifiers [][]account.Modifier) error {

	// Catch the most obvious problems first.
	if len(orders) != len(orderModifiers) {
		return fmt.Errorf("order modifier length mismatch")
	}
	if len(accounts) != len(accountModifiers) {
		return fmt.Errorf("account modifier length mismatch")
	}

	// Wrap the whole batch update in a single update transaction.
	return db.Update(func(tx *bbolt.Tx) error {
		// Update orders first.
		ordersBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		for idx, nonce := range orders {
			err := updateOrder(
				ordersBucket, ordersBucket, nonce,
				orderModifiers[idx],
			)
			if err != nil {
				return err
			}
		}

		// Then update the accounts.
		accountsBucket, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}
		for idx, acct := range accounts {
			accountKey := getAccountKey(acct)
			err := updateAccount(
				accountsBucket, accountsBucket, accountKey,
				accountModifiers[idx],
			)
			if err != nil {
				return err
			}
		}

		// Finally, write the ID of the pending batch.
		batchBkt, err := getBucket(tx, batchBucketKey)
		if err != nil {
			return err
		}
		return batchBkt.Put(pendingBatchIDKey, batchID[:])
	})
}

// PendingBatchID retrieves the ID of the currently pending batch. If there
// isn't one, order.ErrNoPendingBatch is returned.
func (db *DB) PendingBatchID() (order.BatchID, error) {
	var batchID order.BatchID
	err := db.View(func(tx *bbolt.Tx) error {
		batchBkt, err := getBucket(tx, batchBucketKey)
		if err != nil {
			return err
		}

		copy(batchID[:], batchBkt.Get(pendingBatchIDKey))
		if batchID == zeroBatchID {
			return order.ErrNoPendingBatch
		}

		return nil
	})
	return batchID, err
}

// MarkBatchComplete marks a pending batch as complete, allowing a trader to
// participate in a new batch. If there isn't one, ErrNoPendingBatch is
// returned.
func (db *DB) MarkBatchComplete(id order.BatchID) error {
	return db.Update(func(tx *bbolt.Tx) error {
		batchBkt, err := getBucket(tx, batchBucketKey)
		if err != nil {
			return err
		}

		var batchID order.BatchID
		copy(batchID[:], batchBkt.Get(pendingBatchIDKey))

		if batchID == zeroBatchID {
			return order.ErrNoPendingBatch
		}
		if batchID != id {
			return fmt.Errorf("batch id mismatch: pending id "+
				"should be %x, got %x", batchID[:], id[:])
		}

		return batchBkt.Delete(pendingBatchIDKey)
	})
}
