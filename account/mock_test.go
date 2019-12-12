package account

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

func init() {
	copy(testTraderKey[:], testRawTraderKey)
}

var (
	testAuctioneerKey [33]byte
	testTraderKey     [33]byte

	testRawTraderKey, _ = hex.DecodeString("036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09")
	testTraderPubKey, _ = btcec.ParsePubKey(testRawTraderKey, btcec.S256())
	testTraderKeyDesc   = &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: clmscript.AccountKeyFamily,
			Index:  0,
		},
		PubKey: testTraderPubKey,
	}
)

type mockStore struct {
	Store

	mu       sync.Mutex
	accounts map[[33]byte]Account
}

func newMockStore() *mockStore {
	return &mockStore{
		accounts: make(map[[33]byte]Account),
	}
}

func (s *mockStore) AddAccount(account *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.accounts[account.TraderKey] = *account
	return nil
}

func (s *mockStore) UpdateAccount(account *Account, modifiers ...Modifier) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.accounts[account.TraderKey]; !ok {
		return errors.New("account not found")
	}

	for _, modifier := range modifiers {
		modifier(account)
	}

	s.accounts[account.TraderKey] = *account
	return nil
}

func (s *mockStore) Account(accountKey [33]byte) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	account, ok := s.accounts[accountKey]
	if !ok {
		return nil, errors.New("account not found")
	}
	return &account, nil
}

func (s *mockStore) Accounts() ([]*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	accounts := make([]*Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		account := account
		accounts = append(accounts, &account)
	}
	return accounts, nil
}

type mockAuctioneer struct {
	Auctioneer
}

func (a *mockAuctioneer) ReserveAccount(context.Context, [33]byte) ([33]byte, error) {
	return testAuctioneerKey, nil
}

func (a *mockAuctioneer) InitAccount(context.Context, *Account) error {
	return nil
}

type mockWallet struct {
	TxSource
	lndclient.WalletKitClient

	txs []*wire.MsgTx

	sendOutputs func(context.Context, []*wire.TxOut,
		chainfee.SatPerKWeight) (*wire.MsgTx, error)
}

func (w *mockWallet) DeriveNextKey(ctx context.Context,
	family int32) (*keychain.KeyDescriptor, error) {

	return testTraderKeyDesc, nil
}

func (w *mockWallet) PublishTransaction(ctx context.Context, tx *wire.MsgTx) error {
	return nil
}

func (w *mockWallet) SendOutputs(ctx context.Context, outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight) (*wire.MsgTx, error) {

	if w.sendOutputs != nil {
		return w.sendOutputs(ctx, outputs, feeRate)
	}

	tx := &wire.MsgTx{
		Version: 2,
		TxOut:   outputs,
	}
	w.addTx(tx)
	return tx, nil
}

func (w *mockWallet) ListTransactions(ctx context.Context) ([]*wire.MsgTx, error) {
	return w.txs, nil
}

func (w *mockWallet) addTx(tx *wire.MsgTx) {
	w.txs = append(w.txs, tx)
}

func (w *mockWallet) interceptSendOutputs(f func(context.Context, []*wire.TxOut,
	chainfee.SatPerKWeight) (*wire.MsgTx, error)) {

	w.sendOutputs = f
}

type mockChainNotifier struct {
	lndclient.ChainNotifierClient

	confChan  chan *chainntnfs.TxConfirmation
	spendChan chan *chainntnfs.SpendDetail
	blockChan chan int32
	errChan   chan error
}

func newMockChainNotifier() *mockChainNotifier {
	return &mockChainNotifier{
		confChan:  make(chan *chainntnfs.TxConfirmation),
		spendChan: make(chan *chainntnfs.SpendDetail),
		blockChan: make(chan int32),
		errChan:   make(chan error),
	}
}

func (n *mockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs,
	heightHint int32) (chan *chainntnfs.TxConfirmation, chan error, error) {

	return n.confChan, n.errChan, nil
}

func (n *mockChainNotifier) RegisterSpendNtfn(ctx context.Context,
	outpoint *wire.OutPoint, pkScript []byte,
	heightHint int32) (chan *chainntnfs.SpendDetail, chan error, error) {

	return n.spendChan, n.errChan, nil
}

func (n *mockChainNotifier) RegisterBlockEpochNtfn(
	ctx context.Context) (chan int32, chan error, error) {

	return n.blockChan, n.errChan, nil
}
