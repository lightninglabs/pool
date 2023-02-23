package clientdb

import (
	"encoding/hex"
	"io/ioutil"
	"os"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	testOutPoint = wire.OutPoint{Index: 1}

	testRawAuctioneerKey, _ = hex.DecodeString(
		"02187d1a0e30f4e5016fc1137363ee9e7ed5dde1e6c50f367422336df7a1" +
			"08b716",
	)
	testAuctioneerKey, _ = btcec.ParsePubKey(testRawAuctioneerKey)

	testRawTraderKey, _ = hex.DecodeString(
		"036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c93954" +
			"46ba09",
	)
	testTraderKey, _    = btcec.ParsePubKey(testRawTraderKey)
	testRawTraderKeyArr = [33]byte{
		0x03, 0x6b, 0x51, 0xe0, 0xcc, 0x2d, 0x9e, 0x59, 0x88, 0xee,
		0x49, 0x67, 0xe0, 0xba, 0x67, 0xef, 0x37, 0x27, 0xbb, 0x63,
		0x3f, 0xea, 0x21, 0xa0, 0xaf, 0x58, 0xe0, 0xc9, 0x39, 0x54,
		0x46, 0xba, 0x09,
	}

	testTraderKeyDesc = &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: poolscript.AccountKeyFamily,
			Index:  0,
		},
		PubKey: testTraderKey,
	}

	testRawBatchKey, _ = hex.DecodeString(
		"02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2ae66afefb8" +
			"eaf27f",
	)
	testBatchKey, _ = btcec.ParsePubKey(testRawBatchKey)

	sharedSecret = [32]byte{0x73, 0x65, 0x63, 0x72, 0x65, 0x74}
)

func newTestDB(t *testing.T) (*DB, func()) {
	tempDir, err := ioutil.TempDir("", "client-db")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	db, err := New(tempDir, DBFilename)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("unable to create new db: %v", err)
	}

	return db, func() {
		db.Close()
		os.RemoveAll(tempDir)
	}
}

func assertAccountExists(t *testing.T, db *DB, expected *account.Account) {
	t.Helper()

	found, err := db.Account(expected.TraderKey.PubKey)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(found, expected) {
		t.Fatalf("expected account: %v\ngot: %v", spew.Sdump(expected),
			spew.Sdump(found))
	}
}

// TestAccounts ensures that all database operations involving accounts run as
// expected.
func TestAccounts(t *testing.T) {
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
		Version:       account.VersionTaro,
		TaroLeaf: &txscript.TapLeaf{
			LeafVersion: txscript.BaseLeafVersion,
			Script:      []byte("testscript"),
		},
	}

	// First, we'll add it to the database. We should be able to retrieve
	// after.
	if err := db.AddAccount(a); err != nil {
		t.Fatalf("unable to add account: %v", err)
	}
	assertAccountExists(t, db, a)

	// Transition the account from StateInitiated to StatePendingOpen. If
	// the database update is successful, the in-memory account should be
	// updated as well.
	accountOutput, err := a.Output()
	if err != nil {
		t.Fatal(err)
	}
	accountTx := &wire.MsgTx{
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
	accountPoint := wire.OutPoint{
		Hash:  accountTx.TxHash(),
		Index: 0,
	}
	err = db.UpdateAccount(
		a, account.StateModifier(account.StatePendingOpen),
		account.OutPointModifier(accountPoint),
		account.LatestTxModifier(accountTx),
		account.VersionModifier(account.VersionTaprootEnabled),
	)
	if err != nil {
		t.Fatalf("unable to update account: %v", err)
	}
	assertAccountExists(t, db, a)

	// Now, transition the account from StatePendingOpen to
	// StatePendingClosed and include a closing transaction. If the database
	// update is successful, the in-memory account should be updated as
	// well.
	closeTx := &wire.MsgTx{
		Version: 2,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: testOutPoint,
				SignatureScript:  []byte{},
			},
		},
		TxOut: []*wire.TxOut{},
	}
	err = db.UpdateAccount(
		a, account.StateModifier(account.StatePendingClosed),
		account.LatestTxModifier(closeTx),
	)
	if err != nil {
		t.Fatalf("unable to update account: %v", err)
	}
	assertAccountExists(t, db, a)

	// Retrieving all accounts should show that we only have one account,
	// the same one.
	accounts, err := db.Accounts()
	if err != nil {
		t.Fatalf("unable to retrieve accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, found %v", len(accounts))
	}
	if !reflect.DeepEqual(accounts[0], a) {
		t.Fatalf("expected account: %v\ngot: %v", spew.Sdump(a),
			spew.Sdump(accounts[0]))
	}
}
