package clientdb

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/terms"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

var (
	testAccount = &account.Account{
		Value:         btcutil.SatoshiPerBitcoin,
		Expiry:        1337,
		TraderKey:     testTraderKeyDesc,
		AuctioneerKey: testAuctioneerKey,
		BatchKey:      testBatchKey,
		Secret:        sharedSecret,
		State:         account.StateInitiated,
		HeightHint:    1,
	}
	testAccountTaproot = &account.Account{
		Value:         btcutil.SatoshiPerBitcoin,
		Expiry:        1337,
		TraderKey:     testTraderKeyDesc,
		AuctioneerKey: testAuctioneerKey,
		BatchKey:      testBatchKey,
		Secret:        sharedSecret,
		State:         account.StateInitiated,
		HeightHint:    1,
		Version:       account.VersionTaprootEnabled,
	}

	testNonce1 = order.Nonce([32]byte{1, 1, 1})
	testNonce2 = order.Nonce([32]byte{2, 2, 2})

	testDuration uint32 = 144

	testAccounts = map[[33]byte]*account.Account{
		testRawTraderKeyArr: testAccount,
		{1, 2, 3}:           testAccountTaproot,
	}

	testOrders = map[order.Nonce]order.Order{
		testNonce1: &order.Bid{
			Kit: *newOrderKit(testNonce1, testDuration),
		},
		testNonce2: &order.Ask{
			Kit: *newOrderKit(testNonce2, testDuration),
		},
	}

	testMatchedOrders = map[order.Nonce][]*order.MatchedOrder{
		testNonce1: {
			{
				Order: &order.Ask{
					Kit: *dummyOrder(50000, 2048),
				},
				MultiSigKey: [33]byte{1, 2, 3},
				NodeKey:     [33]byte{1, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP: net.IP{
							0x12, 0x34, 0x56, 0x78,
						},
						Port: 8080,
					},
				},
				UnitsFilled: 19,
			},
			{
				Order: &order.Ask{
					Kit: *dummyOrder(50000, 2048),
				},
				MultiSigKey: [33]byte{2, 2, 3},
				NodeKey:     [33]byte{2, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP: net.IP{
							0x12, 0x34, 0x56, 0x78,
						},
						Port: 8080,
					},
				},

				UnitsFilled: 10,
			},
		},
		testNonce2: {
			{
				Order: &order.Bid{
					Kit: *dummyOrder(50000, 144),
				},
				MultiSigKey: [33]byte{1, 2, 3},
				NodeKey:     [33]byte{1, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP: net.IP{
							0x12, 0x34, 0x56, 0x78,
						},
						Port: 8080,
					},
				},

				UnitsFilled: 100,
			},
			{
				Order: &order.Bid{
					Kit: *dummyOrder(50000, 2048),
				},
				MultiSigKey: [33]byte{2, 2, 3},
				NodeKey:     [33]byte{2, 2, 3},
				NodeAddrs: []net.Addr{
					&net.TCPAddr{
						IP: net.IP{
							0x12, 0x34, 0x56, 0x78,
						},
						Port: 8080,
					},
				},

				UnitsFilled: 10,
			},
		},
	}

	testSnapshot = &LocalBatchSnapshot{
		Version: order.DefaultBatchVersion,
		BatchID: testBatchID,
		ClearingPrices: map[uint32]order.FixedRatePremium{
			testDuration:     999,
			testDuration * 2: 1234,
		},
		ExecutionFee:   *terms.NewLinearFeeSchedule(101, 900),
		BatchTX:        testBatchTx,
		BatchTxFeeRate: 123456,
		Accounts:       testAccounts,
		Orders:         testOrders,
		MatchedOrders:  testMatchedOrders,
	}

	allSnapshots = []*LocalBatchSnapshot{
		testSnapshot,
		{
			Version: order.DefaultBatchVersion,
			BatchID: testBatchID,
			ClearingPrices: map[uint32]order.FixedRatePremium{
				order.LegacyLeaseDurationBucket: 999,
			},
			ExecutionFee:   *terms.NewLinearFeeSchedule(101, 900),
			BatchTX:        testBatchTx,
			BatchTxFeeRate: 123456,
			Accounts:       testAccounts,
			Orders:         testOrders,
			MatchedOrders:  testMatchedOrders,
		},
	}
)

func newOrderKit(nonce order.Nonce, duration uint32) *order.Kit {
	kit := order.NewKit(nonce)
	kit.LeaseDuration = duration
	return kit
}

// TestSerializeLocalBatchSnapshot checks that (de)serialization of local batch
// snapshots works as expected.
func TestSerializeLocalBatchSnapshot(t *testing.T) {
	for idx, testSnapshot := range allSnapshots {
		pre := testSnapshot
		t.Run(fmt.Sprintf("snapshot-%d", idx), func(t *testing.T) {
			t.Parallel()

			buf := bytes.Buffer{}
			if err := serializeLocalBatchSnapshot(&buf, pre); err != nil {
				t.Fatal(err)
			}

			post, err := deserializeLocalBatchSnapshot(&buf)
			if err != nil {
				t.Fatal(err)
			}

			require.Equal(t, pre, post)
		})
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

	// Storing a batch snapshot requires its orders to be stored as well.
	for _, order := range testSnapshot.Orders {
		err := store.SubmitOrder(order)
		require.NoError(t, err)
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
		require.Equal(t, testSnapshot, snapshot)
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
