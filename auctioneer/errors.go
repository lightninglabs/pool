package auctioneer

import (
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/clientdb"
)

var (
	// ErrAcctResNotCompleted is the error wrapped by
	// AcctResNotCompletedError that can be used with errors.Is() to
	// avoid type assertions.
	ErrAcctResNotCompleted = errors.New("account reservation not completed")
)

// AcctResNotCompletedError is returned if a trader tries to subscribe to an account
// that we don't know. This should only happen during recovery where the trader
// goes through a number of their keys to find accounts we still know the state
// of.
type AcctResNotCompletedError struct {
	// Value is the value of the account reflected in the on-chain output
	// that backs the existence of an account.
	Value btcutil.Amount

	// AcctKey is the raw account key of the account we didn't find in our
	// database.
	AcctKey [33]byte

	// AuctioneerKey is the raw base auctioneer's key in the 2-of-2
	// multi-sig construction of a CLM account.
	AuctioneerKey [33]byte

	// InitialBatchKey is the raw initial batch key that is used to tweak
	// the trader key of an account.
	InitialBatchKey [33]byte

	// HeightHint is the block height at the time of the reservation. Even
	// if the transaction was published, it cannot have happened in an
	// earlier block.
	HeightHint uint32

	// Expiry is the expiration block height of an account. After this
	// point, the trader is able to withdraw the funds from their account
	// without cooperation of the auctioneer.
	Expiry uint32

	// Version is the version of the account.
	Version uint32
}

// Error implements the error interface.
func (e *AcctResNotCompletedError) Error() string {
	return fmt.Sprintf("account %x reservation not completed", e.AcctKey)
}

// Unwrap returns the underlying error type.
func (e *AcctResNotCompletedError) Unwrap() error {
	return ErrAcctResNotCompleted
}

// AcctResNotCompletedErrFromRPC creates a new AcctResNotCompletedError from an
// RPC account message.
func AcctResNotCompletedErrFromRPC(
	rpcAcc *auctioneerrpc.AuctionAccount) *AcctResNotCompletedError {

	result := &AcctResNotCompletedError{
		Value:      btcutil.Amount(rpcAcc.Value),
		Expiry:     rpcAcc.Expiry,
		HeightHint: rpcAcc.HeightHint,
		Version:    rpcAcc.Version,
	}
	copy(result.AcctKey[:], rpcAcc.TraderKey)
	copy(result.AuctioneerKey[:], rpcAcc.AuctioneerKey)
	copy(result.InitialBatchKey[:], rpcAcc.BatchKey)
	return result
}

// IsOrderNotFoundErr tries to match the given error with ErrNoOrder. Useful to
// check rpc errors from the server.
func IsOrderNotFoundErr(err error) bool {
	// The rpc error will look like
	//     rpc error: code = Unknown desc = no order found
	return strings.Contains(err.Error(), clientdb.ErrNoOrder.Error())
}
