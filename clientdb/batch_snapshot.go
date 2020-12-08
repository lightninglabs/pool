package clientdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"go.etcd.io/bbolt"
)

// batch-snapshot-bucket
//         |
//         |-- batch-snapshot-pending-key: <batch snapshot>
//         |
//         |-- batch-snapshot-seq-bucket
//         |              |
//         |              |-- <sequence num>
//         |              |        |
//         |              |        |-- batch-snapshot-batch: <batch snapshot>
//         |              |
//         |              |-- <sequence num>
//         |              |        |
//         |             ...      ...
//         |
//         |-- batch-snapshot-batchid-index-bucket
//                       |
//                       |-- <batch id>: <sequence-num>
//                       |-- <batch id>: <sequence-num>
//                       |
//                      ...
var (
	// batchSnapshotBucketKey is the top level bucket where we'll find
	// snapshot information about all batches we have participated in.
	batchSnapshotBucketKey = []byte("batch-snapshot-bucket")

	// batchSnapshotPendingKey is a key where we will store the snapshot
	// for a pending batch. When the batch has been finalizd we'll move it
	// into a sub-bucket for long-term record keeping.
	batchSnapshotPendingKey = []byte("batch-snapshot-pending")

	// batchSnapshotSeqBucketKey is a sub-bucket where we'll store batch
	// snapshots indexed by sequence number.
	batchSnapshotSeqBucketKey = []byte("batch-snapshot-seq-bucket")

	// batchSnapshotBatchIDIndexBucket is a sub-bucket where we'll map
	// batch IDs to their sequence number.
	batchSnapshotBatchIDIndexBucketKey = []byte("batch-snapshot-batchid-index-bucket")

	// batchSnapshotBatchKey is the key under where we'll store the
	// serialized batch snapshot.
	batchSnapshotBatchKey = []byte("batch-snapshot-batch")
)

// LocalBatchSnapshot holds key information about our participation in a batch.
type LocalBatchSnapshot struct {
	// Version is the version of the batch verification protocol.
	Version order.BatchVersion

	// BatchID is the batch's unique ID.
	BatchID order.BatchID

	// ClearingPrice is the fixed rate the orders were cleared at.
	ClearingPrice order.FixedRatePremium

	// ExecutionFee is the FeeSchedule that was used by the server to
	// calculate the execution fee.
	ExecutionFee terms.LinearFeeSchedule

	// BatchTX is the complete batch transaction with all non-witness data
	// fully populated.
	BatchTX *wire.MsgTx

	// BatchTxFeeRate is the miner fee rate in sat/kW that was chosen for
	// the batch transaction.
	BatchTxFeeRate chainfee.SatPerKWeight

	// Account holds snapshots of the ending state of the local accounts
	// that participated in this batch.
	Accounts map[[33]byte]*account.Account

	// Orders holds snapshots of the ending state of local orders that were
	// part of matches in this batch.
	Orders map[order.Nonce]order.Order

	// MatchedOrders is a map between all trader's orders and the other
	// orders that were matched to them in the batch.
	MatchedOrders map[order.Nonce][]*order.MatchedOrder
}

// NewSnapshots creates a new LocalBatchSnapshot from the passed order batched.
func NewSnapshot(batch *order.Batch, ourOrders []order.Order,
	accounts []*account.Account) (*LocalBatchSnapshot, error) {

	// We only support LinearFeeSchedule at this point (because of
	// serialization).
	feeSched, ok := batch.ExecutionFee.(*terms.LinearFeeSchedule)
	if !ok {
		return nil, fmt.Errorf("unsupported fee schedule: %T",
			batch.ExecutionFee)
	}

	as := make(map[[33]byte]*account.Account)
	for _, a := range accounts {
		var key [33]byte
		copy(key[:], a.TraderKey.PubKey.SerializeCompressed())
		as[key] = a
	}

	os := make(map[order.Nonce]order.Order)
	for _, o := range ourOrders {
		os[o.Nonce()] = o
	}

	snapshot := &LocalBatchSnapshot{
		Version:        batch.Version,
		BatchID:        batch.ID,
		ClearingPrice:  batch.ClearingPrice,
		ExecutionFee:   *feeSched,
		BatchTX:        batch.BatchTX,
		BatchTxFeeRate: batch.BatchTxFeeRate,
		Accounts:       as,
		Orders:         os,
		MatchedOrders:  batch.MatchedOrders,
	}

	return snapshot, nil
}

// GetLocalBatchSnapshots returns snapshots for all batches the trader has
// participated in.
func (db *DB) GetLocalBatchSnapshots() ([]*LocalBatchSnapshot, error) {
	var snapshots []*LocalBatchSnapshot
	err := db.View(func(tx *bbolt.Tx) error {
		var err error
		snapshots, err = db.fetchLocalBatchSnapshots(tx)
		return err
	})
	if err != nil {
		return nil, err
	}

	return snapshots, nil
}

// GetLocalBatchSnapshot fetches the batch snapshot for the given batch ID.
func (db *DB) GetLocalBatchSnapshot(id order.BatchID) (
	*LocalBatchSnapshot, error) {

	var snapshot *LocalBatchSnapshot
	err := db.View(func(tx *bbolt.Tx) error {
		_, seqBucket, indexBucket, err := getSnapshotBuckets(tx)
		if err != nil {
			return err
		}

		seq := indexBucket.Get(id[:])
		if seq == nil {
			return fmt.Errorf("snapshot of batch %x not found",
				id[:])
		}
		rootOrderBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}

		snapshot, err = fetchLocalBatchSnapshot(
			seqBucket, seq, rootOrderBucket,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

func (db *DB) fetchLocalBatchSnapshots(tx *bbolt.Tx) ([]*LocalBatchSnapshot,
	error) {

	_, seqBucket, _, err := getSnapshotBuckets(tx)
	if err != nil {
		return nil, err
	}
	rootOrderBucket, err := getBucket(tx, ordersBucketKey)
	if err != nil {
		return nil, err
	}

	// Each entry in the top-level bucket is a sub-bucket index by the
	// sequence number.
	var snapshots []*LocalBatchSnapshot
	err = seqBucket.ForEach(func(seq, v []byte) error {
		batchSnapshot, err := fetchLocalBatchSnapshot(
			seqBucket, seq, rootOrderBucket,
		)
		if err != nil {
			return err
		}

		// Add this batch to our list of snapshots.
		snapshots = append(snapshots, batchSnapshot)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return snapshots, nil
}

func fetchLocalBatchSnapshot(seqBucket *bbolt.Bucket, seqNum []byte,
	rootOrderBucket *bbolt.Bucket) (*LocalBatchSnapshot, error) {

	snapshotBucket, err := getNestedBucket(seqBucket, seqNum, false)
	if err != nil {
		return nil, err
	}

	// Get the serialized batch.
	rawBatch := snapshotBucket.Get(batchSnapshotBatchKey)
	if rawBatch == nil {
		return nil, fmt.Errorf("batch not found for snapshot")
	}

	// Deserialize it.
	batchSnapshot, err := deserializeLocalBatchSnapshot(
		bytes.NewReader(rawBatch),
	)
	if err != nil {
		return nil, err
	}

	// The snapshot doesn't contain all the order information needed, so
	// we'll retrieve the missing data.
	for nonce, o := range batchSnapshot.Orders {
		orderBucket, err := getNestedBucket(
			rootOrderBucket, nonce[:], false,
		)
		if err != nil {
			return nil, ErrNoOrder
		}

		minUnitsMatchBytes := orderBucket.Get(orderMinUnitsMatchKey)
		if minUnitsMatchBytes == nil {
			// Assume a base unit minimum match for older orders
			// which were not aware of the field.
			o.Details().MinUnitsMatch = 1
		} else {
			var minUnitsMatch order.SupplyUnit
			err := ReadElement(
				bytes.NewReader(minUnitsMatchBytes),
				&minUnitsMatch,
			)
			if err != nil {
				return nil, err
			}
			o.Details().MinUnitsMatch = minUnitsMatch
		}

		// We'll only need to populate the values below for bid orders.
		bidOrder, ok := o.(*order.Bid)
		if !ok {
			continue
		}

		minNodeTierBytes := orderBucket.Get(orderTierKey)
		if minNodeTierBytes == nil {
			// If not found, then assume the current default value.
			bidOrder.MinNodeTier = order.DefaultMinNodeTier
		} else {
			var minNodeTier order.NodeTier
			err := ReadElement(
				bytes.NewReader(minNodeTierBytes),
				&minNodeTier,
			)
			if err != nil {
				return nil, err
			}
			bidOrder.MinNodeTier = minNodeTier
		}
	}

	return batchSnapshot, nil
}

func storePendingBatchSnapshot(tx *bbolt.Tx,
	snapshot *LocalBatchSnapshot) error {

	topBucket, err := getBucket(tx, batchSnapshotBucketKey)
	if err != nil {
		return err
	}

	buf := bytes.Buffer{}
	if err := serializeLocalBatchSnapshot(&buf, snapshot); err != nil {
		return err
	}

	// Store the batch under the pending key, we'll move it when it is
	// finalized.
	return topBucket.Put(batchSnapshotPendingKey, buf.Bytes())
}

// fetchPendingBatchSnapshot retrieves the currently pending batch snapshot from
// the database or returns the account.ErrNoPendingBatch error if none exists.
func fetchPendingBatchSnapshot(tx *bbolt.Tx) (*LocalBatchSnapshot, error) {
	topBucket, err := getBucket(tx, batchSnapshotBucketKey)
	if err != nil {
		return nil, err
	}

	snapshotBytes := topBucket.Get(batchSnapshotPendingKey)
	if len(snapshotBytes) == 0 {
		return nil, account.ErrNoPendingBatch
	}

	return deserializeLocalBatchSnapshot(bytes.NewReader(snapshotBytes))
}

// finalizeBatchSnapshot moves the pending batch snapshot into the sub-bucket
// indexed by sequence numbers.
func finalizeBatchSnapshot(tx *bbolt.Tx, batchID order.BatchID) error {
	topBucket, seqBucket, indexBucket, err := getSnapshotBuckets(tx)
	if err != nil {
		return err
	}

	rawSnapshot := topBucket.Get(batchSnapshotPendingKey)
	if rawSnapshot == nil {
		return fmt.Errorf("pending snapshot not found")
	}

	err = topBucket.Delete(batchSnapshotPendingKey)
	if err != nil {
		return err
	}

	// Get the next sequence number we will store this batch under.
	sequence, err := seqBucket.NextSequence()
	if err != nil {
		return err
	}
	var seqBytes [8]byte
	binary.BigEndian.PutUint64(seqBytes[:], sequence)

	// Create a sub-bucket for this sequence number.
	snapshotBucket, err := getNestedBucket(seqBucket, seqBytes[:], true)
	if err != nil {
		return err
	}

	err = snapshotBucket.Put(batchSnapshotBatchKey, rawSnapshot)
	if err != nil {
		return err
	}

	// Finally add a entry mapping this batch ID to the sequence number.
	err = indexBucket.Put(batchID[:], seqBytes[:])
	if err != nil {
		return err
	}

	return nil
}

func getSnapshotBuckets(tx *bbolt.Tx) (*bbolt.Bucket, *bbolt.Bucket,
	*bbolt.Bucket, error) {

	topBucket, err := getBucket(tx, batchSnapshotBucketKey)
	if err != nil {
		return nil, nil, nil, err
	}

	// Get the sub-buckets. We expect them to be created at DB init, so we
	// don't attempt to create them if non-existent.
	seqBucket, err := getNestedBucket(
		topBucket, batchSnapshotSeqBucketKey, false,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	indexBucket, err := getNestedBucket(
		topBucket, batchSnapshotBatchIDIndexBucketKey, false,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	return topBucket, seqBucket, indexBucket, nil
}

func serializeLocalBatchSnapshot(w io.Writer, b *LocalBatchSnapshot) error {
	err := WriteElements(
		w, uint32(b.Version), b.BatchID[:], b.ClearingPrice,
		b.ExecutionFee, b.BatchTX, b.BatchTxFeeRate,
	)
	if err != nil {
		return err
	}

	if err := serializeAccounts(w, b.Accounts); err != nil {
		return err
	}

	if err := serializeOrders(w, b.Orders); err != nil {
		return err
	}

	type match struct {
		nonce order.Nonce
		match *order.MatchedOrder
	}

	var matchedOrders []*match
	for nonce, matches := range b.MatchedOrders {
		for _, m := range matches {
			matchedOrders = append(matchedOrders, &match{
				nonce: nonce,
				match: m,
			})
		}
	}

	numMatches := uint32(len(matchedOrders))
	err = WriteElements(w, numMatches)
	if err != nil {
		return err
	}

	for _, m := range matchedOrders {
		err := serializeMatchedOrder(w, m.nonce, m.match)
		if err != nil {
			return err
		}
	}

	return nil
}

func deserializeLocalBatchSnapshot(r io.Reader) (*LocalBatchSnapshot, error) {
	b := &LocalBatchSnapshot{}
	var version uint32
	err := ReadElements(
		r, &version, b.BatchID[:], &b.ClearingPrice, &b.ExecutionFee,
		&b.BatchTX, &b.BatchTxFeeRate,
	)
	if err != nil {
		return nil, err
	}

	b.Version = order.BatchVersion(version)

	b.Accounts, err = deserializeAccounts(r)
	if err != nil {
		return nil, err
	}

	b.Orders, err = deserializeOrders(r)
	if err != nil {
		return nil, err
	}

	var numMatches uint32
	err = ReadElements(r, &numMatches)
	if err != nil {
		return nil, err
	}

	b.MatchedOrders = make(map[order.Nonce][]*order.MatchedOrder)
	for i := uint32(0); i < numMatches; i++ {
		nonce, m, err := deserializeMatchedOrder(r)
		if err != nil {
			return nil, err
		}

		b.MatchedOrders[nonce] = append(b.MatchedOrders[nonce], m)
	}

	return b, nil
}

func serializeAccounts(w io.Writer,
	accounts map[[33]byte]*account.Account) error {

	err := WriteElements(w, uint32(len(accounts)))
	if err != nil {
		return err
	}

	for key, a := range accounts {
		// Write key, then account.
		err := WriteElements(w, key)
		if err != nil {
			return err
		}

		err = serializeAccount(w, a)
		if err != nil {
			return err
		}
	}

	return nil
}

func deserializeAccounts(r io.Reader) (map[[33]byte]*account.Account, error) {
	var numAccounts uint32
	err := ReadElements(r, &numAccounts)
	if err != nil {
		return nil, err
	}

	accs := make(map[[33]byte]*account.Account)
	for i := uint32(0); i < numAccounts; i++ {

		var key [33]byte
		err := ReadElements(r, &key)
		if err != nil {
			return nil, err
		}

		a, err := deserializeAccount(r)
		if err != nil {
			return nil, err
		}

		accs[key] = a
	}

	return accs, nil
}

func serializeOrders(w io.Writer, orders map[order.Nonce]order.Order) error {
	err := WriteElements(w, uint32(len(orders)))
	if err != nil {
		return err
	}

	for nonce, o := range orders {
		err := WriteElements(w, nonce)
		if err != nil {
			return err
		}

		err = SerializeOrder(o, w)
		if err != nil {
			return err
		}
	}

	return nil
}

func deserializeOrders(r io.Reader) (map[order.Nonce]order.Order, error) {
	var numOrders uint32
	err := ReadElements(r, &numOrders)
	if err != nil {
		return nil, err
	}

	orders := make(map[order.Nonce]order.Order)
	for i := uint32(0); i < numOrders; i++ {
		var nonce order.Nonce
		err := ReadElements(r, &nonce)
		if err != nil {
			return nil, err
		}

		o, err := DeserializeOrder(nonce, r)
		if err != nil {
			return nil, err
		}

		orders[nonce] = o
	}

	return orders, nil
}

func serializeMatchedOrder(w io.Writer, ourNonce order.Nonce,
	m *order.MatchedOrder) error {

	err := WriteElements(w, ourNonce, m.Order.Nonce())
	if err != nil {
		return err
	}

	err = SerializeOrder(m.Order, w)
	if err != nil {
		return err
	}

	err = WriteElements(
		w, m.MultiSigKey, m.NodeKey, m.NodeAddrs,
		m.UnitsFilled,
	)
	if err != nil {
		return err
	}

	return nil
}

func deserializeMatchedOrder(r io.Reader) (order.Nonce,
	*order.MatchedOrder, error) {

	var ourNonce, theirNonce order.Nonce
	err := ReadElements(r, &ourNonce, &theirNonce)
	if err != nil {
		return order.Nonce{}, nil, err
	}

	m := &order.MatchedOrder{}

	o, err := DeserializeOrder(theirNonce, r)
	if err != nil {
		return order.Nonce{}, nil, err
	}

	m.Order = o

	err = ReadElements(
		r, &m.MultiSigKey, &m.NodeKey, &m.NodeAddrs,
		&m.UnitsFilled,
	)
	if err != nil {
		return order.Nonce{}, nil, err
	}

	return ourNonce, m, nil
}

// deletePendingSnapshot deletes the pending batch snapshot.
func deletePendingSnapshot(tx *bbolt.Tx) error {
	topBucket, err := getBucket(tx, batchSnapshotBucketKey)
	if err != nil {
		return err
	}

	return topBucket.Delete(batchSnapshotPendingKey)
}
