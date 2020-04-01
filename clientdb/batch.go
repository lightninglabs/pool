package clientdb

import (
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/order"
)

// PersistBatchResult atomically updates all modified orders/accounts. If any
// single operation fails, the whole set of changes is rolled back.
func (db *DB) PersistBatchResult(orders []order.Nonce,
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
		for idx, nonce := range orders {
			err := updateOrderTX(
				tx, ordersBucketKey, nonce, orderModifiers[idx],
			)
			if err != nil {
				return err
			}
		}

		// Then update the accounts.
		for idx, acct := range accounts {
			err := updateAccountTX(tx, acct, accountModifiers[idx])
			if err != nil {
				return err
			}
		}

		// No problems found, let's commit this TX.
		return nil
	})
}
