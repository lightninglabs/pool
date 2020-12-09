package auctioneer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
)

var (
	// ErrBatchNotFinalized is an error returned by the auctioneer when we
	// attempt to query it for a batch snapshot, but the batch has not been
	// finalized by them yet.
	ErrBatchNotFinalized = errors.New("batch snapshot not found")
)

// BatchSource abstracts the source of a trader's pending batch.
type BatchSource interface {
	// PendingBatchSnapshot retrieves the snapshot of the currently pending
	// batch. If there isn't one, account.ErrNoPendingBatch is returned.
	PendingBatchSnapshot() (*clientdb.LocalBatchSnapshot, error)
}

// BatchCleaner abstracts the cleaning up of a trader's pending batch.
type BatchCleaner interface {
	// DeletePendingBatch removes all references to the current pending
	// batch without applying its staged updates to accounts and orders. If
	// no pending batch exists, this acts as a no-op.
	DeletePendingBatch() error

	// RemovePendingBatchArtifacts removes any funding shims or pending
	// channels from a batch that was never finalized. Some non-terminal
	// errors are logged only and not returned. Therefore if this method
	// returns an error, it should be handled as terminal error.
	RemovePendingBatchArtifacts(
		matchedOrders map[order.Nonce][]*order.MatchedOrder,
		batchTx *wire.MsgTx) error
}

// checkPendingBatch cross-checks the trader's pending batch with what the
// auctioneer considers finalized. If they don't match, then the pending batch
// is deleted without applying its staged updates.
func (c *Client) checkPendingBatch() error {
	snapshot, err := c.cfg.BatchSource.PendingBatchSnapshot()
	if err == account.ErrNoPendingBatch {
		// If there's no pending batch, there's nothing to do.
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading pending batch failed: %v", err)
	}

	finalizedTx, err := c.finalizedBatchTx(snapshot)
	// If the batch has not been finalized yet, there's nothing to do but
	// wait to receive its Finalize message.
	//
	// TODO(wilmer): Does gRPC wrap errors returned from an RPC?
	if err != nil && strings.Contains(err.Error(), ErrBatchNotFinalized.Error()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("querying finalized batch TX failed: %v", err)
	}

	if snapshot.BatchTX.TxHash() != finalizedTx.TxHash() {
		err := c.cfg.BatchCleaner.RemovePendingBatchArtifacts(
			snapshot.MatchedOrders, snapshot.BatchTX,
		)
		if err != nil {
			return fmt.Errorf("error removing pending batch "+
				"artifacts: %v", err)
		}

		return c.cfg.BatchCleaner.DeletePendingBatch()
	}

	return nil
}

// finalizedBatchTx retrieves the finalized transaction of a batch according to
// the auctioneer, i.e., the transaction that will be broadcast to the network.
func (c *Client) finalizedBatchTx(
	snapshot *clientdb.LocalBatchSnapshot) (*wire.MsgTx, error) {

	req := &poolrpc.BatchSnapshotRequest{BatchId: snapshot.BatchID[:]}
	batch, err := c.client.BatchSnapshot(context.Background(), req)
	if err != nil {
		return nil, fmt.Errorf("querying relevant batch snapshot "+
			"failed: %v", err)
	}

	var batchTx wire.MsgTx
	err = batchTx.Deserialize(bytes.NewReader(batch.BatchTx))
	if err != nil {
		return nil, fmt.Errorf("deserializing batch TX failed: %v", err)
	}

	return &batchTx, nil
}
