package account

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/clmscript"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	testRawAuctioneerKey, _ = hex.DecodeString("02187d1a0e30f4e5016fc1137363ee9e7ed5dde1e6c50f367422336df7a108b716")
	testAuctioneerKey, _    = btcec.ParsePubKey(testRawAuctioneerKey, btcec.S256())

	testRawTraderKey, _ = hex.DecodeString("036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09")
	testTraderKey, _    = btcec.ParsePubKey(testRawTraderKey, btcec.S256())

	testTraderKeyDesc = &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: clmscript.AccountKeyFamily,
			Index:  0,
		},
		PubKey: testTraderKey,
	}

	testRawBatchKey, _ = hex.DecodeString("02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2ae66afefb8eaf27f")
	testBatchKey, _    = btcec.ParsePubKey(testRawBatchKey, btcec.S256())

	sharedSecret = [32]byte{0x73, 0x65, 0x63, 0x72, 0x65, 0x74}
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

	var accountKey [33]byte
	copy(accountKey[:], account.TraderKey.PubKey.SerializeCompressed())

	s.accounts[accountKey] = *account
	return nil
}

func (s *mockStore) UpdateAccount(account *Account, modifiers ...Modifier) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var accountKey [33]byte
	copy(accountKey[:], account.TraderKey.PubKey.SerializeCompressed())

	if _, ok := s.accounts[accountKey]; !ok {
		return errors.New("account not found")
	}

	for _, modifier := range modifiers {
		modifier(account)
	}

	s.accounts[accountKey] = *account
	return nil
}

func (s *mockStore) Account(traderKey *btcec.PublicKey) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var accountKey [33]byte
	copy(accountKey[:], traderKey.SerializeCompressed())

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

func (a *mockAuctioneer) ReserveAccount(context.Context) (*Reservation, error) {
	return &Reservation{
		AuctioneerKey:   testAuctioneerKey,
		InitialBatchKey: testBatchKey,
	}, nil
}

func (a *mockAuctioneer) InitAccount(context.Context, *Account) error {
	return nil
}

func (a *mockAuctioneer) CloseAccount(context.Context, *btcec.PublicKey,
	[]*wire.TxOut) ([]byte, error) {

	return []byte("auctioneer sig"), nil
}

func (a *mockAuctioneer) SubscribeAccountUpdates(context.Context,
	*Account) error {

	return nil
}

type mockWallet struct {
	TxSource
	lndclient.WalletKitClient
	lndclient.SignerClient

	txs         []*wire.MsgTx
	publishChan chan *wire.MsgTx

	sendOutputs func(context.Context, []*wire.TxOut,
		chainfee.SatPerKWeight) (*wire.MsgTx, error)
}

func newMockWallet() *mockWallet {
	return &mockWallet{
		publishChan: make(chan *wire.MsgTx, 1),
	}
}

func (w *mockWallet) DeriveNextKey(ctx context.Context,
	family int32) (*keychain.KeyDescriptor, error) {

	return testTraderKeyDesc, nil
}

func (w *mockWallet) DeriveSharedKey(ctx context.Context,
	ephemeralKey *btcec.PublicKey,
	keyLocator *keychain.KeyLocator) ([32]byte, error) {

	return sharedSecret, nil
}

func (w *mockWallet) PublishTransaction(ctx context.Context, tx *wire.MsgTx) error {
	w.publishChan <- tx
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

func (w *mockWallet) NextAddr(ctx context.Context) (btcutil.Address, error) {
	pubKeyHash := btcutil.Hash160(testTraderKey.SerializeCompressed())
	return btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, &chaincfg.MainNetParams,
	)
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

func (w *mockWallet) SignOutputRaw(context.Context, *wire.MsgTx,
	[]*input.SignDescriptor) ([][]byte, error) {

	return [][]byte{[]byte("trader sig")}, nil
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

	// Mimic the actual ChainNotifier by sending a notification upon
	// registration.
	go func() {
		n.blockChan <- 0
	}()

	return n.blockChan, n.errChan, nil
}
