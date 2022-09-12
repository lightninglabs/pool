package pool

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/terms"
)

// marshalerConfig contains all of the marshaler's dependencies in order to
// carry out its duties.
type marshalerConfig struct {
	// GetOrders returns all orders that are currently known to the store.
	GetOrders func() ([]order.Order, error)

	// Terms returns the current dynamic auctioneer terms like max account
	// size, max order duration in blocks and the auction fee schedule.
	Terms func(ctx context.Context) (*terms.AuctioneerTerms, error)
}

// marshaler is an internal struct type that implements the Marshaler interface.
type marshaler struct {
	cfg *marshalerConfig
}

// NewMarshaler returns an internal type that implements the Marshaler interface.
func NewMarshaler(cfg *marshalerConfig) *marshaler { // nolint:golint
	return &marshaler{
		cfg: cfg,
	}
}

// MarshallAccountsWithAvailableBalance returns the RPC representation of an account
// with the account.AvailableBalance value populated.
func (m *marshaler) MarshallAccountsWithAvailableBalance(ctx context.Context,
	accounts []*account.Account) ([]*poolrpc.Account, error) {

	rpcAccounts := make([]*poolrpc.Account, 0, len(accounts))
	for _, acct := range accounts {
		rpcAccount, err := MarshallAccount(acct)
		if err != nil {
			return nil, err
		}
		rpcAccounts = append(rpcAccounts, rpcAccount)
	}

	// For each account, we'll need to compute the available balance, which
	// requires us to sum up all the debits from outstanding orders.
	orders, err := m.cfg.GetOrders()
	if err != nil {
		return nil, err
	}

	// Get the current fee schedule so we can compute the worst-case
	// account debit assuming all our standing orders were matched.
	auctionTerms, err := m.cfg.Terms(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to query auctioneer terms: %v",
			err)
	}

	// For each active account, consume the worst-case account delta if the
	// order were to be matched.
	accountDebits := make(map[[33]byte]btcutil.Amount)
	auctionFeeSchedule := auctionTerms.FeeSchedule()
	for _, acct := range accounts {
		var (
			debitAmt btcutil.Amount
			acctKey  [33]byte
		)

		copy(
			acctKey[:],
			acct.TraderKey.PubKey.SerializeCompressed(),
		)

		// We'll make sure to accumulate a distinct sum for each
		// outstanding account the user has.
		for _, o := range orders {
			if o.Details().AcctKey != acctKey {
				continue
			}

			debitAmt += o.ReservedValue(
				auctionFeeSchedule, acct.Version,
			)
		}

		accountDebits[acctKey] = debitAmt
	}

	// Finally, we'll populate the available balance value for each of the
	// existing accounts.
	for _, rpcAccount := range rpcAccounts {
		var acctKey [33]byte
		copy(acctKey[:], rpcAccount.TraderKey)

		accountDebit := accountDebits[acctKey]
		availableBalance := rpcAccount.Value - uint64(accountDebit)

		rpcAccount.AvailableBalance = availableBalance
	}
	return rpcAccounts, nil
}
