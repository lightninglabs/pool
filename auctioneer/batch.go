package auctioneer

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/order"
)

var (
	// ErrBatchNotFinalized is an error returned by the auctioneer when we
	// attempt to query it for a batch snapshot, but the batch has not been
	// finalized by them yet.
	ErrBatchNotFinalized = errors.New("batch snapshot not found")
)

// BatchSource abstracts the source of a trader's pending batch.
type BatchSource interface {
	// PendingBatch retrieves the ID and transaction of the current pending
	// batch. If one does not exist, account.ErrNoPendingBatch is returned.
	PendingBatch() (order.BatchID, *wire.MsgTx, error)

	// DeletePendingBatch removes all references to the current pending
	// batch without applying its staged updates to accounts and orders. If
	// no pending batch exists, this acts as a no-op.
	DeletePendingBatch() error
}

// checkPendingBatch cross-checks the trader's pending batch with what the
// auctioneer considers finalized. If they don't match, then the pending batch
// is deleted without applying its staged updates.
func (c *Client) checkPendingBatch() error {
	id, tx, err := c.cfg.BatchSource.PendingBatch()
	if err == account.ErrNoPendingBatch {
		// If there's no pending batch, there's nothing to do.
		return nil
	}
	if err != nil {
		return err
	}

	finalizedTx, err := c.finalizedBatchTx(id)
	// If the batch has not been finalized yet, there's nothing to do but
	// wait to receive its Finalize message.
	//
	// TODO(wilmer): Does gRPC wrap errors returned from an RPC?
	if err != nil && strings.Contains(err.Error(), ErrBatchNotFinalized.Error()) {
		return nil
	}
	if err != nil {
		return err
	}

	if tx.TxHash() != finalizedTx.TxHash() {
		return c.cfg.BatchSource.DeletePendingBatch()
	}

	return nil
}

// finalizedBatchTx retrieves the finalized transaction of a batch according to
// the auctioneer, i.e., the transaction that will be broadcast to the network.
func (c *Client) finalizedBatchTx(id order.BatchID) (*wire.MsgTx, error) {
	req := &poolrpc.RelevantBatchRequest{Id: id[:]}
	batch, err := c.client.RelevantBatchSnapshot(context.Background(), req)
	if err != nil {
		return nil, err
	}

	var batchTx wire.MsgTx
	err = batchTx.Deserialize(bytes.NewReader(batch.Transaction))
	if err != nil {
		return nil, err
	}

	return &batchTx, nil
}
