package order

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/llm/account"
	"github.com/lightninglabs/llm/clmrpc"
)

const (
	// heightHintPadding is the padding we add to our best known height to
	// avoid any discrepancies in block propagation between us and the
	// auctioneer.
	heightHintPadding = -3
)

// batchStorer is a type that implements BatchStorer and can persist a batch to
// the local trader database.
type batchStorer struct {
	orderStore Store
	getAccount func(*btcec.PublicKey) (*account.Account, error)
}

// StorePendingBatch makes sure all changes executed by a batch are correctly
// and atomically staged to the database. It is assumed that the batch has
// previously been fully validated and that all diffs contained are consistent!
// Once the batch has been finalized/confirmed on-chain, then the stage
// modifications will be applied atomically as a result of MarkBatchComplete.
//
// NOTE: This method is part of the BatchStorer interface.
func (s *batchStorer) StorePendingBatch(batch *Batch, bestHeight uint32) error {
	// Prepare the order modifications first.
	orders := make([]Nonce, len(batch.MatchedOrders))
	orderModifiers := make([][]Modifier, len(orders))
	orderIndex := 0
	for nonce, theirOrders := range batch.MatchedOrders {
		// Get our order first to find out the number of unfulfilled
		// units.
		ourOrder, err := s.orderStore.GetOrder(nonce)
		if err != nil {
			return fmt.Errorf("error getting order: %v", err)
		}
		orders[orderIndex] = nonce

		// Find out if the order has unfulfilled units left or not.
		unitsUnfulfilled := ourOrder.Details().UnitsUnfulfilled
		for _, theirOrder := range theirOrders {
			unitsUnfulfilled -= theirOrder.UnitsFilled
		}
		switch {
		// The order has been fully filled and can be archived.
		case unitsUnfulfilled == 0:
			orderModifiers[orderIndex] = []Modifier{
				StateModifier(StateExecuted),
				UnitsFulfilledModifier(0),
			}

		// Some units were not yet filled.
		default:
			orderModifiers[orderIndex] = []Modifier{
				StateModifier(StatePartiallyFilled),
				UnitsFulfilledModifier(unitsUnfulfilled),
			}
		}

		orderIndex++
	}

	// Next create our account modifiers.
	accounts := make([]*account.Account, len(batch.AccountDiffs))
	accountModifiers := make([][]account.Modifier, len(accounts))

	// Each account will have the same height hint applied.
	heightHint := int64(bestHeight) + heightHintPadding
	if heightHint < 0 {
		heightHint = 0
	}

	for idx, diff := range batch.AccountDiffs {
		// Get the current state of the account first so we can create
		// a proper diff.
		acct, err := s.getAccount(diff.AccountKey)
		if err != nil {
			return fmt.Errorf("error getting account: %v", err)
		}
		accounts[idx] = acct
		var modifiers []account.Modifier

		// Determine the new state of the account and set the on-chain
		// attributes accordingly.
		switch diff.EndingState {
		// The account output has been recreated and needs to wait to be
		// confirmed again.
		case clmrpc.AccountDiff_OUTPUT_RECREATED:
			modifiers = append(
				modifiers,
				account.StateModifier(account.StatePendingUpdate),
				account.OutPointModifier(wire.OutPoint{
					Index: uint32(diff.OutpointIndex),
					Hash:  batch.BatchTX.TxHash(),
				}),
				account.IncrementBatchKey(),
			)

		// The account was fully spent on-chain. We need to wait for the
		// batch (spend) TX to be confirmed still.
		case clmrpc.AccountDiff_OUTPUT_FULLY_SPENT,
			clmrpc.AccountDiff_OUTPUT_DUST_ADDED_TO_FEES,
			clmrpc.AccountDiff_OUTPUT_DUST_EXTENDED_OFFCHAIN:

			modifiers = append(
				modifiers,
				account.StateModifier(account.StatePendingClosed),
				account.CloseTxModifier(batch.BatchTX),
			)

		default:
			return fmt.Errorf("invalid ending account state %d",
				diff.EndingState)
		}

		// Finally update the account value and height hint.
		modifiers = append(
			modifiers, account.ValueModifier(diff.EndingBalance),
		)
		modifiers = append(
			modifiers, account.HeightHintModifier(uint32(heightHint)),
		)
		accountModifiers[idx] = modifiers
	}

	// Everything is ready to be persisted now.
	return s.orderStore.StorePendingBatch(
		batch.ID, batch.BatchTX, orders, orderModifiers, accounts,
		accountModifiers,
	)
}

// MarkBatchComplete marks a pending batch as complete, allowing a trader to
// participate in a new batch.
func (s *batchStorer) MarkBatchComplete() error {
	return s.orderStore.MarkBatchComplete()
}

// A compile-time constraint to ensure batchStorer implements BatchStorer.
var _ BatchStorer = (*batchStorer)(nil)
