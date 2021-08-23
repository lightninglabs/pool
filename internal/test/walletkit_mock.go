package test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
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

func (m *MockWalletKit) ListUnspent(context.Context, int32,
	int32) ([]*lnwallet.Utxo, error) {

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

func (m *MockWalletKit) NextAddr(context.Context) (btcutil.Address, error) {
	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.TestNet3Params,
	)
	if err != nil {
		return nil, err
	}
	return addr, nil
}

func (m *MockWalletKit) PublishTransaction(_ context.Context,
	tx *wire.MsgTx, label string) error {

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

func (m *MockWalletKit) EstimateFee(_ context.Context, confTarget int32) (
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

// AddRelevantTx marks the given transaction as relevant.
func (m *MockWalletKit) AddTx(tx *wire.MsgTx) {
	m.lock.Lock()
	m.Transactions = append(m.Transactions, tx.Copy())
	m.lock.Unlock()
}

func (m *MockWalletKit) BumpFee(context.Context, wire.OutPoint,
	chainfee.SatPerKWeight) error {

	panic("unimplemented")
}
