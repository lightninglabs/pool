package clientdb

import (
	"bytes"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/pool/account"
)

var (
	// accountBucketKey is the top level bucket where we can find all
	// information about complete accounts. These accounts are indexed by
	// their trader key locator.
	accountBucketKey = []byte("account")

	// ErrAccountNotFound is an error returned when we attempt to retrieve
	// information about an account but it is not found.
	ErrAccountNotFound = errors.New("account not found")
)

// getAccountKey returns the key for an account which is not partial.
func getAccountKey(account *account.Account) []byte {
	return account.TraderKey.PubKey.SerializeCompressed()
}

// AddAccount adds a record for the account to the database.
func (db *DB) AddAccount(account *account.Account) error {
	return walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		accounts, err := getWriteBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		return storeAccount(accounts, account)
	})
}

// UpdateAccount updates an account in the database according to the given
// modifiers.
func (db *DB) UpdateAccount(acct *account.Account,
	modifiers ...account.Modifier) error {

	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		accounts, err := getWriteBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}
		accountKey := getAccountKey(acct)
		_, err = updateAccount(accounts, accounts, accountKey, modifiers)
		return err
	})
	if err != nil {
		return err
	}

	for _, modifier := range modifiers {
		modifier(acct)
	}

	return nil
}

// updateAccount reads an account from the src bucket, applies the given
// modifiers to it, and store it back into dst bucket.
func updateAccount(src, dst walletdb.ReadWriteBucket, accountKey []byte,
	modifiers []account.Modifier) (*account.Account, error) {

	dbAccount, err := readAccount(src, accountKey)
	if err != nil {
		return nil, err
	}

	for _, modifier := range modifiers {
		modifier(dbAccount)
	}

	return dbAccount, storeAccount(dst, dbAccount)
}

// Account retrieves a specific account by trader key or returns
// ErrAccountNotFound if it's not found.
func (db *DB) Account(traderKey *btcec.PublicKey) (*account.Account, error) {
	var acct *account.Account
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		accounts, err := getReadBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		acct, err = readAccount(
			accounts, traderKey.SerializeCompressed(),
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return acct, nil
}

// Accounts retrieves all known accounts from the database.
func (db *DB) Accounts() ([]*account.Account, error) {
	var res []*account.Account
	err := walletdb.View(db, func(tx walletdb.ReadTx) error {
		accounts, err := getReadBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		return accounts.ForEach(func(k, v []byte) error {
			// We'll also get buckets here, skip those (identified
			// by nil value).
			if v == nil {
				return nil
			}

			acct, err := readAccount(accounts, k)
			if err != nil {
				return err
			}
			res = append(res, acct)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func storeAccount(targetBucket walletdb.ReadWriteBucket, a *account.Account) error {
	accountKey := getAccountKey(a)

	var accountBuf bytes.Buffer
	if err := serializeAccount(&accountBuf, a); err != nil {
		return err
	}

	return targetBucket.Put(accountKey, accountBuf.Bytes())
}

func readAccount(sourceBucket walletdb.ReadBucket,
	accountKey []byte) (*account.Account, error) {

	accountBytes := sourceBucket.Get(accountKey)
	if accountBytes == nil {
		return nil, ErrAccountNotFound
	}

	return deserializeAccount(bytes.NewReader(accountBytes))
}

func serializeAccount(w io.Writer, a *account.Account) error {
	err := WriteElements(
		w, a.Value, a.Expiry, a.TraderKey, a.AuctioneerKey, a.BatchKey,
		a.Secret, a.State, a.HeightHint, a.OutPoint,
	)
	if err != nil {
		return err
	}

	// The latest transaction is not found within StateInitiated and
	// StateCanceledAfterRecovery.
	switch a.State {
	case account.StateInitiated, account.StateCanceledAfterRecovery:

	default:
		if err := WriteElement(w, a.LatestTx); err != nil {
			return err
		}
	}

	return nil
}

func deserializeAccount(r io.Reader) (*account.Account, error) {
	var a account.Account
	err := ReadElements(
		r, &a.Value, &a.Expiry, &a.TraderKey, &a.AuctioneerKey,
		&a.BatchKey, &a.Secret, &a.State, &a.HeightHint, &a.OutPoint,
	)
	if err != nil {
		return nil, err
	}

	// The latest transaction is not found within StateInitiated and
	// StateCanceledAfterRecovery.
	switch a.State {
	case account.StateInitiated, account.StateCanceledAfterRecovery:

	default:
		if err := ReadElement(r, &a.LatestTx); err != nil {
			return nil, err
		}
	}

	return &a, nil
}
