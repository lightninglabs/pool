package pool

import (
	"context"

	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolrpc"
)

// Marshaler interface to transform internal types to decorated RPC ones.
type Marshaler interface {
	// MarshallAccountsWithAvailableBalance returns the RPC representation
	// of an account with the account.AvailableBalance value populated.
	MarshallAccountsWithAvailableBalance(ctx context.Context,
		accounts []*account.Account) ([]*poolrpc.Account, error)
}
