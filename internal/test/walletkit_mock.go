package test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

func NewMockWalletKit() *MockWalletKit {
	return &MockWalletKit{
		TxPublishChannel:   make(chan *wire.MsgTx),
		SendOutputsChannel: make(chan wire.MsgTx),
	}
}

type MockWalletKit struct {
	keyIndex     int32
	feeEstimates map[int32]chainfee.SatPerKWeight

	lock sync.Mutex

	TxPublishChannel   chan *wire.MsgTx
	SendOutputsChannel chan wire.MsgTx
	Transactions       []*wire.MsgTx
	Sweeps             []string
}

var _ lndclient.WalletKitClient = (*MockWalletKit)(nil)

func (m *MockWalletKit) ListUnspent(context.Context, int32, int32,
	...lndclient.ListUnspentOption) ([]*lnwallet.Utxo, error) {

	return nil, nil
}

func (m *MockWalletKit) ListLeases(
	context.Context) ([]lndclient.LeaseDescriptor, error) {

	return nil, nil
}

func (m *MockWalletKit) LeaseOutput(context.Context, wtxmgr.LockID,
	wire.OutPoint, time.Duration) (time.Time, error) {

	return time.Now(), nil
}

func (m *MockWalletKit) ReleaseOutput(context.Context,
	wtxmgr.LockID, wire.OutPoint) error {

	return nil
}

func (m *MockWalletKit) DeriveNextKey(_ context.Context, family int32) (
	*keychain.KeyDescriptor, error) {

	index := m.keyIndex

	_, pubKey := CreateKey(index)
	m.keyIndex++

	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(family),
			Index:  uint32(index),
		},
		PubKey: pubKey,
	}, nil
}

func (m *MockWalletKit) DeriveKey(_ context.Context, in *keychain.KeyLocator) (
	*keychain.KeyDescriptor, error) {

	_, pubKey := CreateKey(int32(in.Index))

	return &keychain.KeyDescriptor{
		KeyLocator: *in,
		PubKey:     pubKey,
	}, nil
}

func (m *MockWalletKit) NextAddr(context.Context, string,
	walletrpc.AddressType, bool) (btcutil.Address, error) {

	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.TestNet3Params,
	)
	if err != nil {
		return nil, err
	}
	return addr, nil
}

func (m *MockWalletKit) PublishTransaction(_ context.Context,
	tx *wire.MsgTx, _ string) error {

	m.AddTx(tx)
	m.TxPublishChannel <- tx
	return nil
}

func (m *MockWalletKit) SendOutputs(_ context.Context, outputs []*wire.TxOut,
	_ chainfee.SatPerKWeight, label string) (*wire.MsgTx, error) {

	var inputTxHash chainhash.Hash

	tx := wire.MsgTx{}
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  inputTxHash,
			Index: 0,
		},
	})

	for _, out := range outputs {
		tx.AddTxOut(&wire.TxOut{
			PkScript: out.PkScript,
			Value:    out.Value,
		})
	}

	m.AddTx(&tx)
	m.SendOutputsChannel <- tx

	return &tx, nil
}

func (m *MockWalletKit) EstimateFeeRate(_ context.Context, confTarget int32) (
	chainfee.SatPerKWeight, error) {

	if confTarget <= 1 {
		return 0, errors.New("conf target must be greater than 1")
	}

	feeEstimate, ok := m.feeEstimates[confTarget]
	if !ok {
		return 10000, nil
	}

	return feeEstimate, nil
}

// ListSweeps returns a list of the sweep transaction ids known to our node.
func (m *MockWalletKit) ListSweeps(_ context.Context) ([]string, error) {
	return m.Sweeps, nil
}

func (m *MockWalletKit) ListSweepsVerbose(
	_ context.Context) ([]lnwallet.TransactionDetail, error) {

	return nil, nil
}

// AddTx marks the given transaction as relevant.
func (m *MockWalletKit) AddTx(tx *wire.MsgTx) {
	m.lock.Lock()
	m.Transactions = append(m.Transactions, tx.Copy())
	m.lock.Unlock()
}

func (m *MockWalletKit) BumpFee(context.Context, wire.OutPoint,
	chainfee.SatPerKWeight) error {

	panic("unimplemented")
}

// ListAccounts retrieves all accounts belonging to the wallet by default.
// Optional name and addressType can be provided to filter through all of the
// wallet accounts and return only those matching.
func (m *MockWalletKit) ListAccounts(context.Context, string,
	walletrpc.AddressType) ([]*walletrpc.Account, error) {

	return nil, nil
}

// FundPsbt creates a fully populated PSBT that contains enough inputs
// to fund the outputs specified in the template. There are two ways of
// specifying a template: Either by passing in a PSBT with at least one
// output declared or by passing in a raw TxTemplate message. If there
// are no inputs specified in the template, coin selection is performed
// automatically. If the template does contain any inputs, it is assumed
// that full coin selection happened externally and no additional inputs
// are added. If the specified inputs aren't enough to fund the outputs
// with the given fee rate, an error is returned.
// After either selecting or verifying the inputs, all input UTXOs are
// locked with an internal app ID.
//
// NOTE: If this method returns without an error, it is the caller's
// responsibility to either spend the locked UTXOs (by finalizing and
// then publishing the transaction) or to unlock/release the locked
// UTXOs in case of an error on the caller's side.
func (m *MockWalletKit) FundPsbt(_ context.Context,
	_ *walletrpc.FundPsbtRequest) (*psbt.Packet, int32,
	[]*walletrpc.UtxoLease, error) {

	return nil, 0, nil, nil
}

// SignPsbt expects a partial transaction with all inputs and outputs
// fully declared and tries to sign all unsigned inputs that have all
// required fields (UTXO information, BIP32 derivation information,
// witness or sig scripts) set.
// If no error is returned, the PSBT is ready to be given to the next
// signer or to be finalized if lnd was the last signer.
//
// NOTE: This RPC only signs inputs (and only those it can sign), it
// does not perform any other tasks (such as coin selection, UTXO
// locking or input/output/fee value validation, PSBT finalization). Any
// input that is incomplete will be skipped.
func (m *MockWalletKit) SignPsbt(_ context.Context,
	_ *psbt.Packet) (*psbt.Packet, error) {

	return nil, nil
}

// FinalizePsbt expects a partial transaction with all inputs and
// outputs fully declared and tries to sign all inputs that belong to
// the wallet. Lnd must be the last signer of the transaction. That
// means, if there are any unsigned non-witness inputs or inputs without
// UTXO information attached or inputs without witness data that do not
// belong to lnd's wallet, this method will fail. If no error is
// returned, the PSBT is ready to be extracted and the final TX within
// to be broadcast.
//
// NOTE: This method does NOT publish the transaction once finalized. It
// is the caller's responsibility to either publish the transaction on
// success or unlock/release any locked UTXOs in case of an error in
// this method.
func (m *MockWalletKit) FinalizePsbt(_ context.Context, _ *psbt.Packet,
	_ string) (*psbt.Packet, *wire.MsgTx, error) {

	return nil, nil, nil
}

// ImportPublicKey imports a public key as watch-only into the wallet.
func (m *MockWalletKit) ImportPublicKey(ctx context.Context,
	pubkey *btcec.PublicKey, addrType lnwallet.AddressType) error {

	return nil
}

// ImportTaprootScript imports a user-provided taproot script into the wallet.
// The imported script will act as a pay-to-taproot address.
func (m *MockWalletKit) ImportTaprootScript(_ context.Context,
	_ *waddrmgr.Tapscript) (btcutil.Address, error) {

	return nil, nil
}
