package clientdb

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"go.etcd.io/bbolt"
)

var (
	// batchBucketKey is the top level bucket where we can find all
	// information about batches we've participated in.
	batchBucketKey = []byte("batch")

	// pendingBatchIDKey is a key we'll use to store the ID of a batch we're
	// currently participating in.
	pendingBatchIDKey = []byte("pending-id")

	// pendingBatchTxKey is a key we'll use to store the transaction of a
	// batch we're currently participating in.
	pendingBatchTxKey = []byte("pending-tx")

	// pendingBatchAccountsBucketKey is the key of a bucket nested within
	// the top level batch bucket that is responsible for storing the
	// updates of an account that has participated in a batch.
	pendingBatchAccountsBucketKey = []byte("pending-accounts")

	// pendingBatchOrdersBucketKey is the key of a bucket nested within
	// the top level batch bucket that is responsible for storing the
	// updates of an order that has matched in a batch.
	pendingBatchOrdersBucketKey = []byte("pending-orders")

	zeroBatchID order.BatchID
)

// StorePendingBatch atomically stages all modified orders/accounts as a result
// of a pending batch. If any single operation fails, the whole set of changes
// is rolled back. Once the batch has been finalized/confirmed on-chain, then
// the stage modifications will be applied atomically as a result of
// MarkBatchComplete.
func (db *DB) StorePendingBatch(batchID order.BatchID, batchTx *wire.MsgTx,
	orders []order.Nonce, orderModifiers [][]order.Modifier,
	accounts []*account.Account, accountModifiers [][]account.Modifier) error {

	// Catch the most obvious problems first.
	if len(orders) != len(orderModifiers) {
		return fmt.Errorf("order modifier length mismatch")
	}
	if len(accounts) != len(accountModifiers) {
		return fmt.Errorf("account modifier length mismatch")
	}

	// Wrap the whole batch update in a single update transaction.
	return db.Update(func(tx *bbolt.Tx) error {
		// Before updating the set of orders and accounts, we'll first
		// delete the buckets containing any existing staged updates.
		// This is to done to handle the case where the first version of
		// a batch updated an order/account, but its second version
		// didn't. Without this, our state would become desynchronized
		// with the auction.
		bucket, err := getBucket(tx, batchBucketKey)
		if err != nil {
			return err
		}
		err = bucket.DeleteBucket(pendingBatchAccountsBucketKey)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		err = bucket.DeleteBucket(pendingBatchOrdersBucketKey)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		// Update orders first.
		ordersBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		pendingOrdersBucket, err := getNestedBucket(
			bucket, pendingBatchOrdersBucketKey, true,
		)
		if err != nil {
			return err
		}
		for idx, nonce := range orders {
			err := updateOrder(
				ordersBucket, pendingOrdersBucket, nonce,
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
		pendingAccountsBucket, err := getNestedBucket(
			bucket, pendingBatchAccountsBucketKey, true,
		)
		if err != nil {
			return err
		}
		for idx, acct := range accounts {
			accountKey := getAccountKey(acct)
			err := updateAccount(
				accountsBucket, pendingAccountsBucket,
				accountKey, accountModifiers[idx],
			)
			if err != nil {
				return err
			}
		}

		// Finally, write the ID and transaction of the pending batch.
		if err := bucket.Put(pendingBatchIDKey, batchID[:]); err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := WriteElement(&buf, batchTx); err != nil {
			return err
		}
		return bucket.Put(pendingBatchTxKey, buf.Bytes())
	})
}

// PendingBatchID retrieves the ID of the currently pending batch. If there
// isn't one, account.ErrNoPendingBatch is returned.
func (db *DB) PendingBatch() (order.BatchID, *wire.MsgTx, error) {
	var (
		batchID order.BatchID
		batchTx *wire.MsgTx
	)
	err := db.View(func(tx *bbolt.Tx) error {
		var err error
		batchID, err = pendingBatchID(tx)
		if err != nil {
			return err
		}

		batchTx, err = pendingBatchTx(tx)
		return err
	})
	return batchID, batchTx, err
}

// pendingBatchID retrieves the stored pending batch ID within a database
// transaction.
func pendingBatchID(tx *bbolt.Tx) (order.BatchID, error) {
	bucket, err := getBucket(tx, batchBucketKey)
	if err != nil {
		return zeroBatchID, err
	}

	pendingBatchID := bucket.Get(pendingBatchIDKey)
	if pendingBatchID == nil {
		return zeroBatchID, account.ErrNoPendingBatch
	}

	var batchID order.BatchID
	copy(batchID[:], pendingBatchID)
	return batchID, nil
}

// pendingBatchTx retrieves the stored pending batch transaction within a
// database transaction.
func pendingBatchTx(tx *bbolt.Tx) (*wire.MsgTx, error) {
	bucket, err := getBucket(tx, batchBucketKey)
	if err != nil {
		return nil, err
	}

	rawBatchTx := bucket.Get(pendingBatchTxKey)
	if rawBatchTx == nil {
		return nil, account.ErrNoPendingBatch
	}

	var batchTx *wire.MsgTx
	if err := ReadElement(bytes.NewReader(rawBatchTx), &batchTx); err != nil {
		return nil, err
	}

	return batchTx, nil
}

// DeletePendingBatch removes all references to the current pending batch
// without applying its staged updates to accounts and orders. If no pending
// batch exists, this acts as a no-op.
func (db *DB) DeletePendingBatch() error {
	return db.Update(func(tx *bbolt.Tx) error {
		bucket, err := getBucket(tx, batchBucketKey)
		if err != nil {
			return err
		}

		if err := bucket.Delete(pendingBatchIDKey); err != nil {
			return err
		}
		if err := bucket.Delete(pendingBatchTxKey); err != nil {
			return err
		}
		err = bucket.DeleteBucket(pendingBatchAccountsBucketKey)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		err = bucket.DeleteBucket(pendingBatchOrdersBucketKey)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		return nil
	})
}

// MarkBatchComplete marks a pending batch as complete, applying any staged
// modifications necessary, and allowing a trader to participate in a new batch.
// If a pending batch is not found, account.ErrNoPendingBatch is returned.
func (db *DB) MarkBatchComplete() error {
	return db.Update(func(tx *bbolt.Tx) error {
		if _, err := pendingBatchID(tx); err != nil {
			return err
		}
		return applyBatchUpdates(tx)
	})
}

// applyBatchUpdates applies the staged updates for any accounts and orders that
// participated in the latest pending batch.
func applyBatchUpdates(tx *bbolt.Tx) error {
	bucket, err := getBucket(tx, batchBucketKey)
	if err != nil {
		return err
	}

	// We'll start by first applying the account updates. This simply
	// involves fetching the updated state as part of the batch, and copying
	// it over to the main account state.
	pendingAccounts, err := getNestedBucket(
		bucket, pendingBatchAccountsBucketKey, false,
	)
	if err != nil {
		return err
	}
	accounts, err := getBucket(tx, accountBucketKey)
	if err != nil {
		return err
	}
	err = pendingAccounts.ForEach(func(k, v []byte) error {
		// Filter out any keys that are not for accounts.
		if len(k) != 33 {
			return nil
		}
		return updateAccount(pendingAccounts, accounts, k, nil)
	})
	if err != nil {
		return err
	}

	// Once we've updated all of the accounts, we can remove the pending
	// updates as they've been committed.
	if err := bucket.DeleteBucket(pendingBatchAccountsBucketKey); err != nil {
		return err
	}

	// We'll do the same for orders as well.
	pendingOrders, err := getNestedBucket(
		bucket, pendingBatchOrdersBucketKey, false,
	)
	if err != nil {
		return err
	}
	orders, err := getBucket(tx, ordersBucketKey)
	if err != nil {
		return err
	}
	err = pendingOrders.ForEach(func(k, v []byte) error {
		// Filter out any keys that are not nonces.
		var nonce order.Nonce
		if len(k) != len(nonce) {
			return nil
		}
		copy(nonce[:], k)
		return updateOrder(pendingOrders, orders, nonce, nil)
	})
	if err != nil {
		return err
	}

	// Once again, remove the pending order updates as they've been
	// committed.
	if err := bucket.DeleteBucket(pendingBatchOrdersBucketKey); err != nil {
		return err
	}

	// Finally, remove the reference to the pending batch ID and
	// transaction.
	if err := bucket.Delete(pendingBatchIDKey); err != nil {
		return err
	}
	return bucket.Delete(pendingBatchTxKey)
}
