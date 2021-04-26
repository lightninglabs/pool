package clientdb

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/pool/account"
	"github.com/stretchr/testify/require"
)

var (
	additionalDataKeyTest = []byte("key-test")
)

const (
	additionalDataTestDefaultValue uint32 = 144 * 7
)

// TestAdditionalData tests that the functionality of adding additional data to
// a sub bucket works as expected.
func TestAdditionalData(t *testing.T) {
	t.Parallel()

	db, cleanup := newTestDB(t)
	defer cleanup()

	// Create a test account we'll use to interact with the database.
	a := &account.Account{
		Value:         btcutil.SatoshiPerBitcoin,
		Expiry:        1337,
		TraderKey:     testTraderKeyDesc,
		AuctioneerKey: testAuctioneerKey,
		BatchKey:      testBatchKey,
		Secret:        sharedSecret,
		State:         account.StateInitiated,
		HeightHint:    1,
	}

	// First, we'll add it to the database. We should be able to retrieve
	// after.
	if err := db.AddAccount(a); err != nil {
		t.Fatalf("unable to add account: %v", err)
	}
	assertAccountExists(t, db, a)
	accountKey := getAccountKey(a)

	// Now try to read the additional value that does not exist yet. We
	// should instead get back the default value.
	myAdditionalValue := uint32(0)
	require.NoError(t, walletdb.View(db, func(tx walletdb.ReadTx) error {
		subBucket, err := getAdditionalDataReadBucket(
			tx.ReadBucket(accountBucketKey), accountKey,
		)
		if err != nil {
			return err
		}
		return readAdditionalValue(
			subBucket, additionalDataKeyTest, &myAdditionalValue,
			additionalDataTestDefaultValue,
		)
	}))
	require.Equal(t, additionalDataTestDefaultValue, myAdditionalValue)

	// Write the additional into the sub bucket of the account now.
	require.NoError(t, walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		subBucket, err := getAdditionalDataBucket(
			tx.ReadWriteBucket(accountBucketKey), accountKey, true,
		)
		if err != nil {
			return err
		}

		return writeAdditionalValue(
			subBucket, additionalDataKeyTest, uint32(6543),
		)
	}))

	// Read the additional info again, now we shouldn't get the default
	// value anymore.
	require.NoError(t, walletdb.View(db, func(tx walletdb.ReadTx) error {
		subBucket, err := getAdditionalDataReadBucket(
			tx.ReadBucket(accountBucketKey), accountKey,
		)
		if err != nil {
			return err
		}
		return readAdditionalValue(
			subBucket, additionalDataKeyTest, &myAdditionalValue,
			additionalDataTestDefaultValue,
		)
	}))
	require.Equal(t, uint32(6543), myAdditionalValue)

	// Finally, make sure we can't use the wrong type for the default value
	// accidentally when using the readAdditionalValue function.
	// Read the additional info again, now we shouldn't get the default
	// value anymore.
	require.Error(t, walletdb.View(db, func(tx walletdb.ReadTx) error {
		subBucket, err := getAdditionalDataReadBucket(
			tx.ReadBucket(accountBucketKey), accountKey,
		)
		if err != nil {
			return err
		}
		return readAdditionalValue(
			subBucket, additionalDataKeyTest, &myAdditionalValue,
			uint64(additionalDataTestDefaultValue),
		)
	}))
}
