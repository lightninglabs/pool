package clientdb

import (
	"bytes"
	"net"
	"reflect"
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/terms"
	"go.etcd.io/bbolt"
)

var testAccount = &account.Account{
	Value:         btcutil.SatoshiPerBitcoin,
	Expiry:        1337,
	TraderKey:     testTraderKeyDesc,
	AuctioneerKey: testAuctioneerKey,
	BatchKey:      testBatchKey,
	Secret:        sharedSecret,
	State:         account.StateInitiated,
	HeightHint:    1,
}

var testNonce1 = order.Nonce([32]byte{1, 1, 1})
var testNonce2 = order.Nonce([32]byte{2, 2, 2})

var testSnapshot = &LocalBatchSnapshot{
	Version:        5,
	BatchID:        testBatchID,
	ClearingPrice:  999,
	ExecutionFee:   *terms.NewLinearFeeSchedule(101, 900),
	BatchTX:        testBatchTx,
	BatchTxFeeRate: 123456,
	Accounts: map[[33]byte]*account.Account{
		testRawTraderKeyArr: testAccount,
	},
	Orders: map[order.Nonce]order.Order{
		testNonce1: &order.Bid{
			Kit:         *order.NewKit(testNonce1),
			MinDuration: 144,
		},
		testNonce2: &order.Ask{
			Kit:         *order.NewKit(testNonce2),
			MaxDuration: 1024,
		},
	},

	MatchedOrders: map[order.Nonce][]*order.MatchedOrder{
		testNonce1: {
			{
				Order: &order.Ask{
					Kit:         *dummyOrder(50000),
					MaxDuration: 2048,
				},
				MultiSigKey: [33]byte{1, 2, 3},
				NodeKey:     [33]byte{1, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP:   net.IP{0x12, 0x34, 0x56, 0x78},
						Port: 8080,
					},
				},
				UnitsFilled: 19,
			},
			{
				Order: &order.Ask{
					Kit:         *dummyOrder(50000),
					MaxDuration: 2048,
				},
				MultiSigKey: [33]byte{2, 2, 3},
				NodeKey:     [33]byte{2, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP:   net.IP{0x12, 0x34, 0x56, 0x78},
						Port: 8080,
					},
				},

				UnitsFilled: 10,
			},
		},
		testNonce2: {
			{
				Order: &order.Bid{
					Kit:         *dummyOrder(50000),
					MinDuration: 144,
				},
				MultiSigKey: [33]byte{1, 2, 3},
				NodeKey:     [33]byte{1, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP:   net.IP{0x12, 0x34, 0x56, 0x78},
						Port: 8080,
					},
				},

				UnitsFilled: 100,
			},
			{
				Order: &order.Bid{
					Kit:         *dummyOrder(50000),
					MinDuration: 2048,
				},
				MultiSigKey: [33]byte{2, 2, 3},
				NodeKey:     [33]byte{2, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP:   net.IP{0x12, 0x34, 0x56, 0x78},
						Port: 8080,
					},
				},

				UnitsFilled: 10,
			},
		},
	},
}

// TestSerializeLocalBatchSnapshot checks that (de)serialization of local batch
// snapshots worsk as expected.
func TestSerializeLocalBatchSnapshot(t *testing.T) {
	pre := testSnapshot
	buf := bytes.Buffer{}
	if err := serializeLocalBatchSnapshot(&buf, pre); err != nil {
		t.Fatal(err)
	}

	post, err := deserializeLocalBatchSnapshot(&buf)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(pre, post) {
		for _, a := range pre.Accounts {
			a.TraderKey.PubKey.Curve = nil
			a.AuctioneerKey.Curve = nil
			a.BatchKey.Curve = nil
		}
		for _, a := range post.Accounts {
			a.TraderKey.PubKey.Curve = nil
			a.AuctioneerKey.Curve = nil
			a.BatchKey.Curve = nil
		}

		t.Fatalf("mismatch: %v vs %v", spew.Sdump(pre), spew.Sdump(post))
	}
}

// TestStoreLocalBatchSnapshot tests that snapshots stored to the database get
// returned in the same order.
func TestGetLocalBatchSnapshots(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()

	// There should be no snapshots to begin with.
	snapshots, err := store.GetLocalBatchSnapshots()
	if err != nil {
		t.Fatal(err)
	}

	if len(snapshots) != 0 {
		t.Fatalf("expected no snapshots, found %v", len(snapshots))
	}

	// Store the same batch 10 times, only changing the batch ID each time.
	for i := 0; i < 10; i++ {
		i := i
		err := store.Update(func(tx *bbolt.Tx) error {
			testSnapshot.BatchID[0] = byte(i)
			err := storePendingBatchSnapshot(tx, testSnapshot)
			if err != nil {
				return err
			}

			return finalizeBatchSnapshot(tx, testSnapshot.BatchID)
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Fetch all snapshots and check that they are returned in order.
	snapshots, err = store.GetLocalBatchSnapshots()
	if err != nil {
		t.Fatal(err)
	}

	for i, snapshot := range snapshots {
		testSnapshot.BatchID[0] = byte(i)
		if !reflect.DeepEqual(testSnapshot, snapshot) {
			t.Fatalf("mismatch: %v vs %v",
				spew.Sdump(testSnapshot), spew.Sdump(snapshot))
		}
	}

	// Store and delete a pending snapshot, and make sure all other batches are
	// still there.
	testSnapshot.BatchID[0] = byte(99)
	err = store.Update(func(tx *bbolt.Tx) error {
		return storePendingBatchSnapshot(tx, testSnapshot)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = store.Update(deletePendingSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	snapshots2, err := store.GetLocalBatchSnapshots()
	if err != nil {
		t.Fatal(err)
	}

	if len(snapshots2) != len(snapshots) {
		t.Fatalf("wrong number of snapshots")
	}

	for i := range snapshots2 {
		a := snapshots[i]
		b := snapshots2[i]
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("mismatch: %v vs %v",
				spew.Sdump(a), spew.Sdump(b))
		}
	}

	// Make sure we can get a snapshot by batch ID.
	id := testBatchID
	id[0] = 4
	snapshot, err := store.GetLocalBatchSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}

	testSnapshot.BatchID = id
	if !reflect.DeepEqual(testSnapshot, snapshot) {
		t.Fatalf("mismatch: %v vs %v",
			spew.Sdump(testSnapshot), spew.Sdump(snapshot))
	}
}
