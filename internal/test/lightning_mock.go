package test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/zpay32"
)

// PaymentChannelMessage is the data that passed through SendPaymentChannel.
type PaymentChannelMessage struct {
	PaymentRequest string
	Done           chan lndclient.PaymentResult
}

type ScbKeyRing struct {
	EncryptionKey keychain.KeyDescriptor
}

func (k *ScbKeyRing) DeriveNextKey(
	keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	return k.EncryptionKey, nil
}

func (k *ScbKeyRing) DeriveKey(
	keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	return k.EncryptionKey, nil
}

func NewMockLightning() *MockLightning {
	return &MockLightning{
		SendPaymentChannel: nil,
		NodePubkey:         testNodePubkey,
		ScbKeyRing:         &ScbKeyRing{},
		ChainParams:        &chaincfg.TestNet3Params,
		Invoices:           make(map[lntypes.Hash]*lndclient.Invoice),
		connections:        make(map[route.Vertex]string),
	}
}

type MockLightning struct {
	lndclient.LightningClient

	SendPaymentChannel chan PaymentChannelMessage
	NodePubkey         string
	ChainParams        *chaincfg.Params
	ScbKeyRing         *ScbKeyRing

	Transactions []lndclient.Transaction
	Sweeps       []string

	// Invoices is a set of invoices that have been created by the mock,
	// keyed by hash string.
	Invoices map[lntypes.Hash]*lndclient.Invoice

	connections map[route.Vertex]string

	Channels         []lndclient.ChannelInfo
	ChannelsClosed   []lndclient.ClosedChannel
	ChannelsPending  []lndclient.PendingChannel
	ForwardingEvents []lndclient.ForwardingEvent
	Payments         []lndclient.Payment

	lock sync.Mutex
	wg   sync.WaitGroup
}

var _ lndclient.LightningClient = (*MockLightning)(nil)

// PayInvoice pays an invoice.
func (m *MockLightning) PayInvoice(_ context.Context, invoice string,
	_ btcutil.Amount, _ *uint64) chan lndclient.PaymentResult {

	done := make(chan lndclient.PaymentResult, 1)

	m.SendPaymentChannel <- PaymentChannelMessage{
		PaymentRequest: invoice,
		Done:           done,
	}

	return done
}

func (m *MockLightning) WaitForFinished() {
	m.wg.Wait()
}

func (m *MockLightning) ConfirmedWalletBalance(context.Context) (
	btcutil.Amount, error) {

	return 1000000, nil
}

func (m *MockLightning) GetInfo(context.Context) (*lndclient.Info,
	error) {

	pubKeyBytes, err := hex.DecodeString(m.NodePubkey)
	if err != nil {
		return nil, err
	}
	var pubKey [33]byte
	copy(pubKey[:], pubKeyBytes)
	return &lndclient.Info{
		BlockHeight:    600,
		IdentityPubkey: pubKey,
		Uris:           []string{m.NodePubkey + "@127.0.0.1:9735"},
	}, nil
}

func (m *MockLightning) EstimateFeeToP2WSH(context.Context,
	btcutil.Amount, int32) (btcutil.Amount, error) {

	return 3000, nil
}

func (m *MockLightning) AddInvoice(_ context.Context,
	in *invoicesrpc.AddInvoiceData) (lntypes.Hash, string, error) {

	m.lock.Lock()
	defer m.lock.Unlock()

	var hash lntypes.Hash
	switch {
	case in.Hash != nil:
		hash = *in.Hash
	case in.Preimage != nil:
		hash = (*in.Preimage).Hash()
	default:
		if _, err := rand.Read(hash[:]); err != nil {
			return lntypes.Hash{}, "", err
		}
	}

	// Create and encode the payment request as a bech32 (zpay32) string.
	creationDate := time.Now()

	payReq, err := zpay32.NewInvoice(
		m.ChainParams, hash, creationDate,
		zpay32.Description(in.Memo),
		zpay32.CLTVExpiry(in.CltvExpiry),
		zpay32.Amount(in.Value),
	)
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	payReqString, err := payReq.Encode(
		zpay32.MessageSigner{
			SignCompact: func(hash []byte) ([]byte, error) {
				// ecdsa.SignCompact returns a
				// pubkey-recoverable signature.
				sig, err := ecdsa.SignCompact(
					privKey, hash, true,
				)
				if err != nil {
					return nil, fmt.Errorf("can't sign "+
						"the hash: %v", err)
				}

				return sig, nil
			},
		},
	)
	if err != nil {
		return lntypes.Hash{}, "", err
	}

	// Add the invoice we have created to our mock's set of invoices.
	m.Invoices[hash] = &lndclient.Invoice{
		Preimage:       nil,
		Hash:           hash,
		PaymentRequest: payReqString,
		Amount:         in.Value,
		CreationDate:   creationDate,
		State:          invoices.ContractOpen,
		IsKeysend:      false,
	}

	return hash, payReqString, nil
}

// LookupInvoice looks up an invoice in the mock's set of stored invoices.
// If it is not found, this call will fail. Note that these invoices should
// be settled using settleInvoice to have a preimage, settled state and settled
// date set.
func (m *MockLightning) LookupInvoice(_ context.Context,
	hash lntypes.Hash) (*lndclient.Invoice, error) {

	m.lock.Lock()
	defer m.lock.Unlock()

	inv, ok := m.Invoices[hash]
	if !ok {
		return nil, fmt.Errorf("invoice: %x not found", hash)
	}

	return inv, nil
}

// ListTransactions returns all known transactions of the backing lnd node.
func (m *MockLightning) ListTransactions(_ context.Context, _, _ int32,
	_ ...lndclient.ListTransactionsOption) ([]lndclient.Transaction,
	error) {

	m.lock.Lock()
	txs := m.Transactions
	m.lock.Unlock()

	return txs, nil
}

// ListChannels retrieves all channels of the backing lnd node.
func (m *MockLightning) ListChannels(context.Context, bool,
	bool) ([]lndclient.ChannelInfo, error) {

	return m.Channels, nil
}

// ClosedChannels returns a list of our closed channels.
func (m *MockLightning) ClosedChannels(
	context.Context) ([]lndclient.ClosedChannel, error) {

	return m.ChannelsClosed, nil
}

// ForwardingHistory returns the mock's set of forwarding events.
func (m *MockLightning) ForwardingHistory(context.Context,
	lndclient.ForwardingHistoryRequest) (*lndclient.ForwardingHistoryResponse,
	error) {

	return &lndclient.ForwardingHistoryResponse{
		LastIndexOffset: 0,
		Events:          m.ForwardingEvents,
	}, nil
}

// ListInvoices returns our mock's invoices.
func (m *MockLightning) ListInvoices(context.Context,
	lndclient.ListInvoicesRequest) (*lndclient.ListInvoicesResponse,
	error) {

	invoices := make([]lndclient.Invoice, 0, len(m.Invoices))
	for _, invoice := range m.Invoices {
		invoices = append(invoices, *invoice)
	}

	return &lndclient.ListInvoicesResponse{
		Invoices: invoices,
	}, nil
}

// ListPayments makes a paginated call to our list payments endpoint.
func (m *MockLightning) ListPayments(context.Context,
	lndclient.ListPaymentsRequest) (*lndclient.ListPaymentsResponse,
	error) {

	return &lndclient.ListPaymentsResponse{
		Payments: m.Payments,
	}, nil
}

// ChannelBackup retrieves the backup for a particular channel. The
// backup is returned as an encrypted chanbackup.Single payload.
func (m *MockLightning) ChannelBackup(_ context.Context,
	op wire.OutPoint) ([]byte, error) {

	fakeKey, _ := btcec.NewPrivateKey()
	pubKey := fakeKey.PubKey()

	for _, chanInfo := range m.Channels {
		if chanInfo.ChannelPoint == op.String() {
			desc := keychain.KeyDescriptor{
				PubKey: pubKey,
			}

			// We only add the fields that are strictly necessary
			// for serializing.
			openChan := &channeldb.OpenChannel{
				RevocationProducer: shachain.NewRevocationProducer(
					op.Hash,
				),
				IdentityPub: pubKey,
				RemoteChanCfg: channeldb.ChannelConfig{
					MultiSigKey:         desc,
					RevocationBasePoint: desc,
					PaymentBasePoint:    desc,
					DelayBasePoint:      desc,
					HtlcBasePoint:       desc,
				},
				ChanType: channeldb.AnchorOutputsBit,
			}
			single := chanbackup.NewSingle(openChan, nil)

			var buf bytes.Buffer
			err := single.PackToWriter(&buf, m.ScbKeyRing)
			if err != nil {
				return nil, err
			}
			return buf.Bytes(), nil
		}
	}

	return nil, fmt.Errorf("channel %v not found", op)
}

// ChannelBackups retrieves backups for all existing pending open and
// open channels. The backups are returned as an encrypted
// chanbackup.Multi payload.
func (m *MockLightning) ChannelBackups(context.Context) ([]byte, error) {
	return nil, nil
}

func (m *MockLightning) PendingChannels(
	context.Context) (*lndclient.PendingChannels, error) {

	return &lndclient.PendingChannels{
		PendingOpen: m.ChannelsPending,
	}, nil
}

func (m *MockLightning) DecodePaymentRequest(context.Context,
	string) (*lndclient.PaymentRequest, error) {

	return nil, nil
}

func (m *MockLightning) OpenChannel(_ context.Context, peer route.Vertex,
	localSat, pushSat btcutil.Amount, _ bool,
	opts ...lndclient.OpenChannelOption) (*wire.OutPoint, error) {

	var randomHash chainhash.Hash
	if _, err := rand.Read(randomHash[:]); err != nil {
		return nil, err
	}

	op := &wire.OutPoint{
		Hash:  randomHash,
		Index: 0,
	}
	m.ChannelsPending = append(m.ChannelsPending, lndclient.PendingChannel{
		ChannelPoint:     op,
		PubKeyBytes:      peer,
		Capacity:         localSat + pushSat,
		ChannelInitiator: lndclient.InitiatorLocal,
	})

	return op, nil
}

func (m *MockLightning) CloseChannel(context.Context, *wire.OutPoint,
	bool, int32, btcutil.Address) (chan lndclient.CloseChannelUpdate,
	chan error, error) {

	return nil, nil, nil
}

func (m *MockLightning) Connect(_ context.Context, peer route.Vertex,
	host string, _ bool) error {

	m.lock.Lock()
	defer m.lock.Unlock()

	m.connections[peer] = host
	return nil
}

func (m *MockLightning) Connections() map[route.Vertex]string {
	m.lock.Lock()
	defer m.lock.Unlock()

	connections := make(map[route.Vertex]string)
	for k, v := range m.connections {
		connections[k] = v
	}

	return connections
}

func (m *MockLightning) GetChanInfo(ctx context.Context,
	cid uint64) (*lndclient.ChannelEdge, error) {

	return nil, nil
}

func (m *MockLightning) SubscribeChannelBackups(ctx context.Context,
) (<-chan lnrpc.ChanBackupSnapshot, <-chan error, error) {

	return nil, nil, nil
}

func (m *MockLightning) UpdateChanPolicy(ctx context.Context,
	req lndclient.PolicyUpdateRequest, chanPoint *wire.OutPoint) error {

	return nil
}

func (m *MockLightning) SubscribeChannelEvents(ctx context.Context) (
	<-chan *lndclient.ChannelEventUpdate, <-chan error, error) {

	return nil, nil, nil
}
