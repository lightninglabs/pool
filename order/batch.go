package order

import (
	"bytes"
	"context"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// MaxBatchIDHistoryLookup is the maximum number of iterations we do
	// when iterating batch IDs. It's unlikely to happen but in case we
	// start from an invalid value we want to avoid an endless loop. If we
	// ever have more than that many batches, we need to handle them in
	// multiple steps anyway.
	MaxBatchIDHistoryLookup = 10_000
)

// BatchVersion is the type for the batch verification protocol.
type BatchVersion uint32

const (
	// DefaultVersion is the first implemented version of the batch
	// verification protocol.
	DefaultVersion BatchVersion = 0

	// CurrentVersion must point to the latest implemented version of the
	// batch verification protocol. Both server and client should always
	// refer to this constant. If a client's binary is not updated in time
	// it will point to a previous version than the server and the mismatch
	// will be detected during the OrderMatchPrepare call.
	CurrentVersion = DefaultVersion
)

// BatchID is a 33-byte point that uniquely identifies this batch. This ID
// will be used later for account key derivation when constructing the batch
// execution transaction.
type BatchID [33]byte

// NewBatchID returns a new batch ID for the given public key.
func NewBatchID(pub *btcec.PublicKey) BatchID {
	var b BatchID
	copy(b[:], pub.SerializeCompressed())
	return b
}

// AccountDiff represents a matching+clearing event for a trader's account.
// This diff shows the total balance delta along with a breakdown for each item
// for a trader's account.
type AccountDiff struct {
	// AccountKeyRaw is the raw serialized account public key this diff
	// refers to.
	AccountKeyRaw [33]byte

	// AccountKey is the parsed account public key this diff refers to.
	AccountKey *btcec.PublicKey

	// EndingState is the ending on-chain state of the account after the
	// executed batch as the auctioneer calculated it.
	EndingState poolrpc.AccountDiff_AccountState

	// EndingBalance is the ending balance for a trader's account.
	EndingBalance btcutil.Amount

	// OutpointIndex is the index of the re-created account output in the
	// batch transaction. This is set to -1 if no account output has been
	// created because the leftover value was considered to be dust.
	OutpointIndex int32
}

// validateEndingState validates that the ending state of an account as
// proposed by the server is correct.
func (d *AccountDiff) validateEndingState(tx *wire.MsgTx,
	acct *account.Account) error {

	state := d.EndingState
	wrongStateErr := fmt.Errorf(
		"unexpected state %d for ending balance %d", state,
		d.EndingBalance,
	)

	// Depending on the final amount of the account, we might get
	// dust which is handled differently.
	if d.EndingBalance < MinNoDustAccountSize {
		// The ending balance of the account is too small to be spent
		// by a simple transaction and not create a dust output. We
		// expect the server to set the state correctly and not re-
		// create an account outpoint.
		if state != poolrpc.AccountDiff_OUTPUT_DUST_EXTENDED_OFFCHAIN &&
			state != poolrpc.AccountDiff_OUTPUT_DUST_ADDED_TO_FEES &&
			state != poolrpc.AccountDiff_OUTPUT_FULLY_SPENT {

			return wrongStateErr
		}
		if d.OutpointIndex >= 0 {
			return fmt.Errorf("unexpected outpoint index for dust " +
				"account")
		}
	} else {
		// There should be enough balance left to justify a new account
		// output. We should get the outpoint from the server.
		if state != poolrpc.AccountDiff_OUTPUT_RECREATED {
			return wrongStateErr
		}
		if d.OutpointIndex < 0 {
			return fmt.Errorf("outpoint index invalid for non-"+
				"dust account with state %d and balance %d",
				state, d.EndingBalance)
		}

		// Make sure the outpoint index is correct and there is an
		// output with the correct amount there.
		if d.OutpointIndex >= int32(len(tx.TxOut)) {
			return fmt.Errorf("outpoint index out of bounds")
		}
		out := tx.TxOut[d.OutpointIndex]
		if btcutil.Amount(out.Value) != d.EndingBalance {
			return fmt.Errorf("invalid account output amount. got "+
				"%d expected %d", out.Value, d.EndingBalance)
		}

		// Final check, make sure we arrive at the same script for the
		// new account output.
		nextScript, err := acct.NextOutputScript()
		if err != nil {
			return fmt.Errorf("could not derive next account "+
				"script: %v", err)
		}
		if !bytes.Equal(out.PkScript, nextScript) {
			return fmt.Errorf("unexpected account output "+
				"script: want %x got %x", nextScript,
				out.PkScript)
		}
	}

	return nil
}

// Batch is all the information the auctioneer sends to each trader for them to
// validate a batch execution.
type Batch struct {
	// ID is the batch's unique ID. If multiple messages come in with the
	// same ID, they are to be considered to be the _same batch_ with
	// updated matches. Any previous version of a batch with that ID should
	// be discarded in that case.
	ID BatchID

	// BatchVersion is the version of the batch verification protocol.
	Version BatchVersion

	// MatchedOrders is a map between all trader's orders and the other
	// orders that were matched to them in the batch.
	MatchedOrders map[Nonce][]*MatchedOrder

	// AccountDiffs is the calculated difference for each trader's account
	// that was involved in the batch.
	AccountDiffs []*AccountDiff

	// ExecutionFee is the FeeSchedule that was used by the server to
	// calculate the execution fee.
	ExecutionFee terms.FeeSchedule

	// ClearingPrice is the fixed rate the orders were cleared at.
	ClearingPrice FixedRatePremium

	// BatchTX is the complete batch transaction with all non-witness data
	// fully populated.
	BatchTX *wire.MsgTx

	// BatchTxFeeRate is the miner fee rate in sat/kW that was chosen for
	// the batch transaction.
	BatchTxFeeRate chainfee.SatPerKWeight

	// FeeRebate is the rebate that was offered to the trader if another
	// batch participant wanted to pay more fees for a faster confirmation.
	FeeRebate btcutil.Amount
}

// Fetcher describes a function that's able to fetch the latest version of an
// order based on its nonce.
type Fetcher func(Nonce) (Order, error)

// ChannelOutput returns the transaction output and output index of the channel
// created for an order of ours that was matched with another one in a batch.
func ChannelOutput(batchTx *wire.MsgTx, wallet lndclient.WalletKitClient,
	ourOrder Order, otherOrder *MatchedOrder) (*wire.TxOut, uint32, error) {

	// Re-derive our multisig key first.
	ctxt, cancel := context.WithTimeout(
		context.Background(), deriveKeyTimeout,
	)
	defer cancel()
	ourKey, err := wallet.DeriveKey(
		ctxt, &ourOrder.Details().MultiSigKeyLocator,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("could not derive our multisig key: "+
			"%v", err)
	}

	// Gather the information we expect to find in the batch TX.
	expectedOutputSize := otherOrder.UnitsFilled.ToSatoshis()
	_, expectedOut, err := input.GenFundingPkScript(
		ourKey.PubKey.SerializeCompressed(), otherOrder.MultiSigKey[:],
		int64(expectedOutputSize),
	)
	if err != nil {
		return nil, 0, fmt.Errorf("could not create multisig script: "+
			"%v", err)
	}

	// Locate the channel output now that we know what to look for.
	for idx, out := range batchTx.TxOut {
		if out.Value == expectedOut.Value &&
			bytes.Equal(out.PkScript, expectedOut.PkScript) {

			// Bingo, this is what we want.
			return out, uint32(idx), nil
		}
	}

	return nil, 0, fmt.Errorf("no channel output found in batch tx for "+
		"matched order %v", otherOrder.Order.Nonce())
}

// MatchedOrder is the other side to one of our matched orders. It contains all
// the information that is needed to validate the match and to start negotiating
// the channel opening with the matched trader's node.
type MatchedOrder struct {
	// Order contains the details of the other order as sent by the server.
	Order Order

	// MultiSigKey is a key of the node creating the order that will be used
	// to craft the channel funding TX's 2-of-2 multi signature output.
	MultiSigKey [33]byte

	// NodeKey is the identity public key of the node creating the order.
	NodeKey [33]byte

	// NodeAddrs is the list of network addresses of the node creating the
	// order.
	NodeAddrs []net.Addr

	// UnitsFilled is the number of units that were matched by this order.
	UnitsFilled SupplyUnit
}

// BatchSignature is a map type that is keyed by a trader's account key and
// contains the multi-sig signature for the input that
// spends from the current account in a batch.
type BatchSignature map[[33]byte]*btcec.Signature

// BatchVerifier is an interface that can verify a batch from the point of view
// of the trader.
type BatchVerifier interface {
	// Verify makes sure the batch prepared by the server is correct and
	// can be accepted by the trader.
	Verify(*Batch) error
}

// BatchSigner is an interface that can sign for a trader's account inputs in
// a batch.
type BatchSigner interface {
	// Sign returns the witness stack of all account inputs in a batch that
	// belong to the trader.
	Sign(*Batch) (BatchSignature, error)
}

// BatchStorer is an interface that can store a batch to the local database by
// applying all the diffs to the orders and accounts.
type BatchStorer interface {
	// StorePendingBatch makes sure all changes executed by a pending batch
	// are correctly and atomically stored to the database.
	StorePendingBatch(_ *Batch, bestHeight uint32) error

	// MarkBatchComplete marks a pending batch as complete, allowing a
	// trader to participate in a new batch.
	MarkBatchComplete() error
}

// DecrementingBatchIDs lists all possible batch IDs that can exist between a
// start and end key. Start key must be the larger/higher key, decrementing it a
// given number of times should finally result in the end key. If the keys
// aren't related or are too far apart, we return a maximum number of IDs that
// corresponds to our safety net parameter and the end key won't be contained in
// the list. The returned list is in descending order, meaning the first entry
// is the start key, the last entry is the end key.
func DecrementingBatchIDs(startKey, endKey *btcec.PublicKey) []BatchID {
	var (
		result       = []BatchID{NewBatchID(startKey)}
		tempBatchKey = startKey
		numIDs       = 0
	)

	// No need to loop if start and end are the same.
	if startKey.IsEqual(endKey) {
		return result
	}

	for !tempBatchKey.IsEqual(endKey) {
		numIDs++

		// Unlikely to happen but in case we start from an invalid value
		// we want to avoid an endless loop. This will cause the list to
		// be incomplete if we ever have more than the maximum number of
		// batches. But there is no scenario where it makes sense to
		// work with more than that number of batches in one step. So if
		// we ever do have more than that maximum number of batches, we
		// need to query them in multiple steps.
		if numIDs >= MaxBatchIDHistoryLookup {
			break
		}

		tempBatchKey = poolscript.DecrementKey(tempBatchKey)
		result = append(result, NewBatchID(tempBatchKey))
	}
	return result
}
