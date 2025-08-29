package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/poolscript"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

var (
	testRawAuctioneerKey, _ = hex.DecodeString("02187d1a0e30f4e5016fc1137363ee9e7ed5dde1e6c50f367422336df7a108b716")
	testAuctioneerKey, _    = btcec.ParsePubKey(testRawAuctioneerKey)

	testRawTraderKey, _ = hex.DecodeString("036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09")
	testTraderKey, _    = btcec.ParsePubKey(testRawTraderKey)

	testTraderKeyDesc = &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: poolscript.AccountKeyFamily,
			Index:  0,
		},
		PubKey: testTraderKey,
	}

	testRawBatchKey, _ = hex.DecodeString("02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2ae66afefb8eaf27f")
	testBatchKey, _    = btcec.ParsePubKey(testRawBatchKey)

	sharedSecret = [32]byte{0x73, 0x65, 0x63, 0x72, 0x65, 0x74}
)

type mockStore struct {
	Store

	mu               sync.Mutex
	accounts         map[[33]byte]Account
	onFinalizedBatch func() error
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

	return s.updateAccount(account, modifiers...)
}

func (s *mockStore) updateAccount(account *Account, modifiers ...Modifier) error {
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
		accounts = append(accounts, &account)
	}
	return accounts, nil
}

func (s *mockStore) setPendingBatch(onFinalizedBatch func() error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onFinalizedBatch = onFinalizedBatch
}

func (s *mockStore) PendingBatch() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.onFinalizedBatch == nil {
		return ErrNoPendingBatch
	}
	return nil
}

func (s *mockStore) MarkBatchComplete() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.onFinalizedBatch(); err != nil {
		return err
	}

	s.onFinalizedBatch = nil
	return nil
}

func (s *mockStore) LockID() (wtxmgr.LockID, error) {
	return wtxmgr.LockID{1}, nil
}

type mockAuctioneer struct {
	Auctioneer

	mu                  sync.Mutex
	subscribed          map[[33]byte]struct{}
	inputsReceived      []wire.TxIn
	outputsReceived     []wire.TxOut
	noncesReceived      []byte
	prevOutputsReceived []*wire.TxOut
}

var _ Auctioneer = (*mockAuctioneer)(nil)

func newMockAuctioneer() *mockAuctioneer {
	return &mockAuctioneer{
		subscribed: make(map[[33]byte]struct{}),
	}
}

func (a *mockAuctioneer) ReserveAccount(context.Context,
	btcutil.Amount, uint32, *btcec.PublicKey, Version) (*Reservation,
	error) {

	return &Reservation{
		AuctioneerKey:   testAuctioneerKey,
		InitialBatchKey: testBatchKey,
	}, nil
}

func (a *mockAuctioneer) InitAccount(context.Context, *Account) error {
	return nil
}

func (a *mockAuctioneer) ModifyAccount(_ context.Context, _ *Account,
	inputs []*wire.TxIn, outputs []*wire.TxOut, _ []Modifier, nonces []byte,
	prevOutputs []*wire.TxOut) ([]byte, []byte, error) {

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, input := range inputs {
		a.inputsReceived = append(a.inputsReceived, *input)
	}
	for _, output := range outputs {
		a.outputsReceived = append(a.outputsReceived, *output)
	}
	a.noncesReceived = nonces
	a.prevOutputsReceived = prevOutputs

	return []byte("auctioneer sig"), nil, nil
}

func (a *mockAuctioneer) StartAccountSubscription(_ context.Context,
	accountKey *keychain.KeyDescriptor) error {

	var traderKey [33]byte
	copy(traderKey[:], accountKey.PubKey.SerializeCompressed())

	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.subscribed[traderKey]; ok {
		return nil
	}

	a.subscribed[traderKey] = struct{}{}
	return nil
}

func (a *mockAuctioneer) Terms(context.Context) (*terms.AuctioneerTerms, error) {
	return &terms.AuctioneerTerms{
		MaxAccountValue: maxAccountValue,
	}, nil
}

var _ Auctioneer = (*mockAuctioneer)(nil)

type mockWallet struct {
	TxSource
	lndclient.WalletKitClient

	txs               []lndclient.Transaction
	publishChan       chan *wire.MsgTx
	utxos             []*lnwallet.Utxo
	fundPsbt          *psbt.Packet
	fundPsbtChangeIdx int32

	sendOutputs func(context.Context, []*wire.TxOut,
		chainfee.SatPerKWeight) (*wire.MsgTx, error)
}

var _ lndclient.WalletKitClient = (*mockWallet)(nil)

func newMockWallet() *mockWallet {
	return &mockWallet{
		publishChan: make(chan *wire.MsgTx, 1),
	}
}

func (w *mockWallet) RawClientWithMacAuth(
	ctx context.Context) (context.Context, time.Duration,
	walletrpc.WalletKitClient) {

	return ctx, 0, nil
}

func (w *mockWallet) DeriveNextKey(ctx context.Context,
	family int32) (*keychain.KeyDescriptor, error) {

	return testTraderKeyDesc, nil
}

func (w *mockWallet) PublishTransaction(ctx context.Context, tx *wire.MsgTx,
	label string) error {

	w.publishChan <- tx
	return nil
}

func (w *mockWallet) SendOutputs(ctx context.Context, outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight,
	label string) (*wire.MsgTx, error) {

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

func (w *mockWallet) NextAddr(context.Context, string,
	walletrpc.AddressType, bool) (btcutil.Address, error) {

	pubKeyHash := btcutil.Hash160(testTraderKey.SerializeCompressed())
	return btcutil.NewAddressWitnessPubKeyHash(
		pubKeyHash, &chaincfg.MainNetParams,
	)
}

func (w *mockWallet) ListTransactions(context.Context, int32,
	int32, ...lndclient.ListTransactionsOption) ([]lndclient.Transaction,
	error) {

	return w.txs, nil
}

func (w *mockWallet) addTx(tx *wire.MsgTx) {
	w.txs = append(w.txs, lndclient.Transaction{Tx: tx})
}

func (w *mockWallet) interceptSendOutputs(f func(context.Context, []*wire.TxOut,
	chainfee.SatPerKWeight) (*wire.MsgTx, error)) {

	w.sendOutputs = f
}

func (w *mockWallet) ListUnspent(context.Context, int32, int32,
	...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	return w.utxos, nil
}

func (w *mockWallet) LeaseOutput(_ context.Context, lockID wtxmgr.LockID,
	op wire.OutPoint, _ time.Duration) (time.Time, error) {

	return time.Now().Add(10 * time.Minute), nil
}

func (w *mockWallet) ReleaseOutput(_ context.Context, lockID wtxmgr.LockID,
	op wire.OutPoint) error {

	return nil
}

func (w *mockWallet) EstimateFeeRate(_ context.Context,
	_ int32) (chainfee.SatPerKWeight, error) {

	return chainfee.FeePerKwFloor, nil
}

func (w *mockWallet) EstimateFeeToP2WSH(_ context.Context, _ btcutil.Amount,
	_ int32) (btcutil.Amount, error) {

	return btcutil.Amount(chainfee.FeePerKwFloor), nil
}

func (w *mockWallet) FundPsbt(_ context.Context,
	req *walletrpc.FundPsbtRequest) (*psbt.Packet, int32,
	[]*walletrpc.UtxoLease, error) {

	return w.fundPsbt, w.fundPsbtChangeIdx, nil, nil
}

func (w *mockWallet) SignPsbt(_ context.Context,
	packet *psbt.Packet) (*psbt.Packet, error) {

	for idx := range packet.Inputs {
		packet.Inputs[idx].PartialSigs = []*psbt.PartialSig{{
			Signature: []byte{
				// A dummy signature must still have the sighash
				// flag appended correctly.
				33, 44, 55, 66, byte(txscript.SigHashAll),
			},
		}}
	}

	return packet, nil
}

func (w *mockWallet) FinalizePsbt(_ context.Context, packet *psbt.Packet,
	_ string) (*psbt.Packet, *wire.MsgTx, error) {

	// Just copy over any sigs we might have. This is copy/paste code from
	// the psbt Finalizer, minus the IsComplete() check.
	tx := packet.UnsignedTx
	for idx := range tx.TxIn {
		pIn := &packet.Inputs[idx]

		switch {
		case len(pIn.FinalScriptSig) > 0:
			tx.TxIn[idx].SignatureScript = pIn.FinalScriptSig

		case len(pIn.FinalScriptWitness) > 0:
			witnessReader := bytes.NewReader(
				pIn.FinalScriptWitness,
			)

			witCount, err := wire.ReadVarInt(witnessReader, 0)
			if err != nil {
				return nil, nil, err
			}

			tx.TxIn[idx].Witness = make(wire.TxWitness, witCount)
			for j := uint64(0); j < witCount; j++ {
				wit, err := wire.ReadVarBytes(
					witnessReader, 0,
					txscript.MaxScriptSize, "witness",
				)
				if err != nil {
					return nil, nil, err
				}
				tx.TxIn[idx].Witness[j] = wit
			}

		case len(pIn.PartialSigs) > 0:
			tx.TxIn[idx].Witness = [][]byte{
				pIn.PartialSigs[0].Signature,
			}
		}
	}

	return packet, packet.UnsignedTx, nil
}

type mockSigner struct {
	lndclient.SignerClient
	sync.Mutex

	muSig2Sessions        map[input.MuSig2SessionID]*input.MuSig2SessionInfo
	muSig2RemovedSessions map[input.MuSig2SessionID]*input.MuSig2SessionInfo
}

var _ lndclient.SignerClient = (*mockSigner)(nil)

func newMockSigner() *mockSigner {
	return &mockSigner{
		muSig2Sessions: make(
			map[input.MuSig2SessionID]*input.MuSig2SessionInfo,
		),
		muSig2RemovedSessions: make(
			map[input.MuSig2SessionID]*input.MuSig2SessionInfo,
		),
	}
}

func (w *mockSigner) DeriveSharedKey(ctx context.Context,
	ephemeralKey *btcec.PublicKey,
	keyLocator *keychain.KeyLocator) ([32]byte, error) {

	return sharedSecret, nil
}

func (w *mockSigner) SignOutputRaw(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor, []*wire.TxOut) ([][]byte, error) {

	return [][]byte{[]byte("trader sig")}, nil
}

func (w *mockSigner) ComputeInputScript(context.Context, *wire.MsgTx,
	[]*lndclient.SignDescriptor, []*wire.TxOut) ([]*input.Script, error) {

	return []*input.Script{{
		SigScript: []byte("input sig script"),
		Witness: wire.TxWitness{
			[]byte("input"),
			[]byte("witness"),
		},
	}}, nil
}

// MuSig2CreateSession creates a new musig session with the key and signers
// provided.
func (w *mockSigner) MuSig2CreateSession(_ context.Context,
	version input.MuSig2Version, _ *keychain.KeyLocator, _ [][]byte,
	opts ...lndclient.MuSig2SessionOpts) (*input.MuSig2SessionInfo, error) {

	var (
		sessionID   [32]byte
		publicNonce [66]byte
	)
	_, _ = rand.Read(sessionID[:])
	_, _ = rand.Read(publicNonce[:])

	req := &signrpc.MuSig2SessionRequest{}
	for _, opt := range opts {
		opt(req)
	}

	session := &input.MuSig2SessionInfo{
		SessionID:          sessionID,
		PublicNonce:        publicNonce,
		CombinedKey:        testBatchKey,
		TaprootTweak:       req.TaprootTweak != nil,
		TaprootInternalKey: nil,
		HaveAllNonces:      false,
		HaveAllSigs:        false,
		Version:            version,
	}

	w.Lock()
	defer w.Unlock()

	w.muSig2Sessions[sessionID] = session

	return session, nil
}

// MuSig2RegisterNonces registers additional public nonces for a musig2 session.
// It returns a boolean indicating whether we have all of our nonces present.
func (w *mockSigner) MuSig2RegisterNonces(_ context.Context, _ [32]byte,
	_ [][66]byte) (bool, error) {

	return true, nil
}

// MuSig2Sign creates a partial signature for the 32 byte SHA256 digest of a
// message. This can only be called once all public nonces have been created. If
// the caller will not be responsible for combining the signatures, the cleanup
// bool should be set.
func (w *mockSigner) MuSig2Sign(_ context.Context, sessionID [32]byte,
	_ [32]byte, cleanup bool) ([]byte, error) {

	var (
		partialSig [input.MuSig2PartialSigSize]byte
	)
	_, _ = rand.Read(partialSig[:])

	w.Lock()
	defer w.Unlock()

	if _, ok := w.muSig2Sessions[sessionID]; !ok {
		return nil, fmt.Errorf("session %x not found", sessionID[:])
	}

	if cleanup {
		w.muSig2RemovedSessions[sessionID] = w.muSig2Sessions[sessionID]
		delete(w.muSig2Sessions, sessionID)
	}

	return partialSig[:], nil
}

// MuSig2CombineSig combines the given partial signature(s) with the local one,
// if it already exists. Once a partial signature of all participants are
// registered, the final signature will be combined and returned.
func (w *mockSigner) MuSig2CombineSig(_ context.Context, sessionID [32]byte,
	_ [][]byte) (bool, []byte, error) {

	var (
		sig [schnorr.SignatureSize]byte
	)
	_, _ = rand.Read(sig[:])

	w.Lock()
	defer w.Unlock()

	if _, ok := w.muSig2Sessions[sessionID]; !ok {
		return false, nil, fmt.Errorf("session %x not found",
			sessionID[:])
	}

	w.muSig2RemovedSessions[sessionID] = w.muSig2Sessions[sessionID]
	delete(w.muSig2Sessions, sessionID)

	return true, sig[:], nil
}

// MuSig2Cleanup removes a session from memory to free up resources.
func (w *mockSigner) MuSig2Cleanup(_ context.Context,
	sessionID [32]byte) error {

	w.Lock()
	defer w.Unlock()

	if _, ok := w.muSig2Sessions[sessionID]; !ok {
		return fmt.Errorf("session %x not found", sessionID[:])
	}

	w.muSig2RemovedSessions[sessionID] = w.muSig2Sessions[sessionID]
	delete(w.muSig2Sessions, sessionID)

	return nil
}

type mockChainNotifier struct {
	lndclient.ChainNotifierClient

	confChan  chan *chainntnfs.TxConfirmation
	spendChan chan *chainntnfs.SpendDetail
	blockChan chan int32
	errChan   chan error
}

var _ lndclient.ChainNotifierClient = (*mockChainNotifier)(nil)

func newMockChainNotifier() *mockChainNotifier {
	return &mockChainNotifier{
		confChan:  make(chan *chainntnfs.TxConfirmation),
		spendChan: make(chan *chainntnfs.SpendDetail),
		blockChan: make(chan int32),
		errChan:   make(chan error),
	}
}

func (n *mockChainNotifier) RawClientWithMacAuth(
	ctx context.Context) (context.Context, time.Duration,
	chainrpc.ChainNotifierClient) {

	return ctx, 0, nil
}

func (n *mockChainNotifier) RegisterConfirmationsNtfn(ctx context.Context,
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint int32,
	opts ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

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
