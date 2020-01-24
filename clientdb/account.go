package clientdb

import (
	"bytes"
	"errors"
	"io"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"
	"github.com/lightninglabs/agora/client/account"
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
	accountKey := getAccountKey(account)
	var accountBuf bytes.Buffer
	if err := serializeAccount(&accountBuf, account); err != nil {
		return err
	}

	return db.Update(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}
		return accounts.Put(accountKey, accountBuf.Bytes())
	})
}

// UpdateAccount updates an account in the database according to the given
// modifiers.
func (db *DB) UpdateAccount(account *account.Account,
	modifiers ...account.Modifier) error {

	accountKey := getAccountKey(account)
	err := db.Update(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}
		accountBytes := accounts.Get(accountKey)
		if accountBytes == nil {
			return ErrAccountNotFound
		}
		dbAccount, err := deserializeAccount(
			bytes.NewReader(accountBytes),
		)
		if err != nil {
			return err
		}

		for _, modifier := range modifiers {
			modifier(dbAccount)
		}

		var accountBuf bytes.Buffer
		if err := serializeAccount(&accountBuf, dbAccount); err != nil {
			return err
		}
		return accounts.Put(accountKey, accountBuf.Bytes())
	})
	if err != nil {
		return err
	}

	for _, modifier := range modifiers {
		modifier(account)
	}

	return nil
}

// Account retrieves a specific account by trader key or returns
// ErrAccountNotFound if it's not found.
func (db *DB) Account(traderKey *btcec.PublicKey) (*account.Account, error) {
	var account *account.Account
	err := db.View(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		accountBytes := accounts.Get(traderKey.SerializeCompressed())
		if accountBytes == nil {
			return ErrAccountNotFound
		}

		account, err = deserializeAccount(bytes.NewReader(accountBytes))
		return err
	})
	if err != nil {
		return nil, err
	}

	return account, nil
}

// Accounts retrieves all known accounts from the database.
func (db *DB) Accounts() ([]*account.Account, error) {
	var res []*account.Account
	err := db.View(func(tx *bbolt.Tx) error {
		accounts, err := getBucket(tx, accountBucketKey)
		if err != nil {
			return err
		}

		return accounts.ForEach(func(k, v []byte) error {
			account, err := deserializeAccount(bytes.NewReader(v))
			if err != nil {
				return err
			}
			res = append(res, account)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func serializeAccount(w io.Writer, a *account.Account) error {
	return WriteElements(
		w, a.Value, a.Expiry, a.TraderKey, a.AuctioneerKey, a.BatchKey,
		a.Secret, a.State, a.HeightHint, a.OutPoint,
	)
}

func deserializeAccount(r io.Reader) (*account.Account, error) {
	var a account.Account
	err := ReadElements(
		r, &a.Value, &a.Expiry, &a.TraderKey, &a.AuctioneerKey,
		&a.BatchKey, &a.Secret, &a.State, &a.HeightHint, &a.OutPoint,
	)
	return &a, err
}
