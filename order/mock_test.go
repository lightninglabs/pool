package order

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/agora/client/account"
)

type mockStore struct {
	orders   map[Nonce]Order
	accounts map[*btcec.PublicKey]*account.Account
}

func newMockStore() *mockStore {
	return &mockStore{
		orders:   make(map[Nonce]Order),
		accounts: make(map[*btcec.PublicKey]*account.Account),
	}
}

// SubmitOrder stores an order by using the orders's nonce as an
// identifier. If an order with the given nonce already exists in the
// store, ErrOrderExists is returned.
func (s *mockStore) SubmitOrder(o Order) error {
	_, ok := s.orders[o.Nonce()]
	if ok {
		return fmt.Errorf("order already exists")
	}
	s.orders[o.Nonce()] = o
	return nil
}

// UpdateOrder updates an order in the database according to the given
// modifiers.
func (s *mockStore) UpdateOrder(nonce Nonce, modifiers ...Modifier) error {
	o, ok := s.orders[nonce]
	if !ok {
		return fmt.Errorf("order not found")
	}

	for _, modifier := range modifiers {
		modifier(o.Details())
	}

	return nil
}

// UpdateOrders atomically updates a list of orders in the database
// according to the given modifiers.
func (s *mockStore) UpdateOrders(nonces []Nonce, modifiers [][]Modifier) error {
	if len(nonces) != len(modifiers) {
		return fmt.Errorf("modifier length mismatch")
	}

	for idx, nonce := range nonces {
		err := s.UpdateOrder(nonce, modifiers[idx]...)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetOrder returns an order by looking up the nonce. If no order with
// that nonce exists in the store, ErrNoOrder is returned.
func (s *mockStore) GetOrder(nonce Nonce) (Order, error) {
	o, ok := s.orders[nonce]
	if !ok {
		return nil, fmt.Errorf("order not found")
	}
	return o, nil
}

// GetOrders returns all orders that are currently known to the store.
func (s *mockStore) GetOrders() ([]Order, error) {
	orders := make([]Order, 0, len(s.orders))
	for _, o := range s.orders {
		orders = append(orders, o)
	}
	return orders, nil
}

// DelOrder removes the order with the given nonce from the local store.
func (s *mockStore) DelOrder(nonce Nonce) error {
	delete(s.orders, nonce)
	return nil
}

// PersistBatchResult atomically updates all modified orders/accounts.
// If any single operation fails, the whole set of changes is rolled
// back.
func (s *mockStore) PersistBatchResult(orders []Nonce,
	orderModifiers [][]Modifier, accts []*account.Account,
	acctModifiers [][]account.Modifier) error {

	err := s.UpdateOrders(orders, orderModifiers)
	if err != nil {
		return err
	}

	return s.updateAccounts(accts, acctModifiers)
}

func (s *mockStore) getAccount(acctKey *btcec.PublicKey) (
	*account.Account, error) {

	acct, ok := s.accounts[acctKey]
	if !ok {
		return nil, fmt.Errorf("account not found")
	}
	return acct, nil
}

func (s *mockStore) updateAccounts(accts []*account.Account,
	modifiers [][]account.Modifier) error {

	if len(accts) != len(modifiers) {
		return fmt.Errorf("modifier length mismatch")
	}

	for idx, acctKey := range accts {
		err := s.updateAccount(acctKey, modifiers[idx]...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *mockStore) updateAccount(acct *account.Account,
	modifiers ...account.Modifier) error {

	a, ok := s.accounts[acct.TraderKey.PubKey]
	if !ok {
		return errors.New("account not found")
	}

	for _, modifier := range modifiers {
		modifier(a)
	}

	return nil
}
