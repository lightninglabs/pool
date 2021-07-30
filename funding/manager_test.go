package funding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/chaninfo"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testTimeout = 100 * time.Millisecond
)

var (
	node1Key = [33]byte{2, 3, 4}
	node2Key = [33]byte{3, 4, 5}
	addr1, _ = net.ResolveTCPAddr("tcp4", "10.0.1.1:9735")
	addr2, _ = net.ResolveTCPAddr("tcp4", "192.168.1.1:9735")
)

type openChannelStream struct {
	lnrpc.Lightning_OpenChannelClient

	updateChan chan *lnrpc.OpenStatusUpdate
	quit       chan struct{}
}

func (i *openChannelStream) Recv() (*lnrpc.OpenStatusUpdate, error) {
	select {
	case msg := <-i.updateChan:
		return msg, nil

	case <-i.quit:
		return nil, context.Canceled
	}
}

type peerEventStream struct {
	lnrpc.Lightning_SubscribePeerEventsClient

	updateChan         chan *lnrpc.PeerEvent
	ctx                context.Context
	cancelSubscription chan struct{}
	quit               chan struct{}
}

func (i *peerEventStream) Recv() (*lnrpc.PeerEvent, error) {
	select {
	case msg := <-i.updateChan:
		return msg, nil

	// To mimic the real GRPC client, we'll return an error status.
	case <-i.ctx.Done():
		return nil, status.Error(
			codes.DeadlineExceeded, i.ctx.Err().Error(),
		)

	case <-i.cancelSubscription:
		return nil, status.Error(
			codes.Canceled, "subscription canceled",
		)

	case <-i.quit:
		return nil, context.Canceled
	}
}

type channelEventStream struct {
	lnrpc.Lightning_SubscribeChannelEventsClient

	updateChan         chan *lnrpc.ChannelEventUpdate
	ctx                context.Context
	cancelSubscription chan struct{}
	quit               chan struct{}
}

func (c *channelEventStream) Recv() (*lnrpc.ChannelEventUpdate, error) {
	select {
	case msg := <-c.updateChan:
		return msg, nil

	// To mimic the real GRPC client, we'll return an error status.
	case <-c.ctx.Done():
		return nil, status.Error(
			codes.DeadlineExceeded, c.ctx.Err().Error(),
		)

	case <-c.cancelSubscription:
		return nil, status.Error(
			codes.Canceled, "subscription canceled",
		)

	case <-c.quit:
		return nil, context.Canceled
	}
}

type fundingBaseClientMock struct {
	lightningClient *test.MockLightning
	fundingShims    map[[32]byte]*lnrpc.ChanPointShim

	peerList      map[route.Vertex]string
	peerEvents    chan *lnrpc.PeerEvent
	channelEvents chan *lnrpc.ChannelEventUpdate
	cancelSub     chan struct{}

	quit chan struct{}
}

func (m *fundingBaseClientMock) FundingStateStep(_ context.Context,
	req *lnrpc.FundingTransitionMsg,
	_ ...grpc.CallOption) (*lnrpc.FundingStateStepResp, error) {

	register := req.GetShimRegister()
	if register == nil || register.GetChanPointShim() == nil {
		return nil, fmt.Errorf("invalid funding shim")
	}

	var tempChanID [32]byte
	copy(tempChanID[:], register.GetChanPointShim().PendingChanId)
	m.fundingShims[tempChanID] = register.GetChanPointShim()

	return nil, nil
}

func (m *fundingBaseClientMock) OpenChannel(ctx context.Context,
	req *lnrpc.OpenChannelRequest,
	_ ...grpc.CallOption) (lnrpc.Lightning_OpenChannelClient, error) {

	if req.FundingShim == nil || req.FundingShim.GetChanPointShim() == nil {
		return nil, fmt.Errorf("invalid funding shim")
	}
	var tempChanID [32]byte
	copy(tempChanID[:], req.FundingShim.GetChanPointShim().PendingChanId)
	_, ok := m.fundingShims[tempChanID]
	if !ok {
		return nil, fmt.Errorf("invalid funding shim")
	}

	node, err := route.NewVertexFromBytes(req.NodePubkey)
	if err != nil {
		return nil, err
	}

	op, _ := m.lightningClient.OpenChannel(
		ctx, node, btcutil.Amount(req.LocalFundingAmount),
		btcutil.Amount(req.PushSat), false,
	)

	stream := &openChannelStream{
		quit:       m.quit,
		updateChan: make(chan *lnrpc.OpenStatusUpdate),
	}
	go func() {
		select {
		case stream.updateChan <- &lnrpc.OpenStatusUpdate{
			Update: &lnrpc.OpenStatusUpdate_ChanPending{
				ChanPending: &lnrpc.PendingUpdate{
					Txid:        op.Hash[:],
					OutputIndex: op.Index,
				},
			},
		}:
		case <-m.quit:
		}
	}()

	return stream, nil
}

func (m *fundingBaseClientMock) SubscribeChannelEvents(ctx context.Context,
	_ *lnrpc.ChannelEventSubscription, _ ...grpc.CallOption) (
	lnrpc.Lightning_SubscribeChannelEventsClient, error) {

	return &channelEventStream{
		quit:       m.quit,
		ctx:        ctx,
		updateChan: m.channelEvents,
	}, nil
}

func (m *fundingBaseClientMock) ListPeers(_ context.Context,
	_ *lnrpc.ListPeersRequest,
	_ ...grpc.CallOption) (*lnrpc.ListPeersResponse, error) {

	resp := &lnrpc.ListPeersResponse{}
	for nodeKey, addr := range m.peerList {
		resp.Peers = append(resp.Peers, &lnrpc.Peer{
			PubKey:  nodeKey.String(),
			Address: addr,
		})
	}

	return resp, nil
}

func (m *fundingBaseClientMock) SubscribePeerEvents(ctx context.Context,
	_ *lnrpc.PeerEventSubscription, _ ...grpc.CallOption) (
	lnrpc.Lightning_SubscribePeerEventsClient, error) {

	return &peerEventStream{
		quit:               m.quit,
		ctx:                ctx,
		cancelSubscription: m.cancelSub,
		updateChan:         m.peerEvents,
	}, nil
}

func (m *fundingBaseClientMock) AbandonChannel(_ context.Context,
	_ *lnrpc.AbandonChannelRequest,
	_ ...grpc.CallOption) (*lnrpc.AbandonChannelResponse, error) {

	return nil, nil
}

type managerHarness struct {
	t              *testing.T
	tempDir        string
	db             *clientdb.DB
	quit           chan struct{}
	lnMock         *test.MockLightning
	baseClientMock *fundingBaseClientMock
	signerMock     *test.MockSigner
	mgr            *Manager
}

func newManagerHarness(t *testing.T) *managerHarness {
	tempDir, err := ioutil.TempDir("", "client-db")
	require.NoError(t, err)

	db, err := clientdb.New(tempDir, clientdb.DBFilename)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		t.Fatalf("unable to create new db: %v", err)
	}

	quit := make(chan struct{})
	lightningClient := test.NewMockLightning()
	walletKitClient := test.NewMockWalletKit()
	signerClient := test.NewMockSigner()
	baseClientMock := &fundingBaseClientMock{
		lightningClient: lightningClient,
		fundingShims:    make(map[[32]byte]*lnrpc.ChanPointShim),
		peerList:        make(map[route.Vertex]string),
		peerEvents:      make(chan *lnrpc.PeerEvent),
		channelEvents:   make(chan *lnrpc.ChannelEventUpdate),
		cancelSub:       make(chan struct{}),
		quit:            quit,
	}
	mgr := NewManager(&ManagerConfig{
		DB:              db,
		WalletKit:       walletKitClient,
		LightningClient: lightningClient,
		BaseClient:      baseClientMock,
		SignerClient:    signerClient,
		NodePubKey: &btcec.PublicKey{
			Curve: btcec.S256(),
			X:     new(big.Int),
			Y:     new(big.Int),
		},
		NewNodesOnly:     true,
		BatchStepTimeout: 400 * time.Millisecond,
	})

	err = mgr.Start()
	require.NoError(t, err)

	return &managerHarness{
		t:              t,
		tempDir:        tempDir,
		db:             db,
		quit:           quit,
		lnMock:         lightningClient,
		baseClientMock: baseClientMock,
		signerMock:     signerClient,
		mgr:            mgr,
	}
}

func (m *managerHarness) stop() {
	close(m.quit)
	require.NoError(m.t, m.db.Close())
	require.NoError(m.t, os.RemoveAll(m.tempDir))
	require.NoError(m.t, m.mgr.Stop())
}

// TestFundingManager tests that the two main steps of the funding manager (the
// channel funding preparation and the batch channel setup) are executed
// correctly. This involves checking the derived keys, funding shims and the
// established connections.
func TestFundingManager(t *testing.T) {
	h := newManagerHarness(t)
	defer h.stop()

	// Set up a simple batch with an ask and a bid that are matched to each
	// other twice, in each direction once. This is enough for our purposes
	// as we mainly want to see the channels getting set up.
	_, pubKeyAsk := test.CreateKey(0)
	_, pubKeyBid := test.CreateKey(1)
	ask := &order.Ask{
		Kit: newKitFromTemplate(order.Nonce{0x01}, &order.Kit{
			MultiSigKeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyTowerSession,
				Index:  0,
			},
			Units:            4,
			UnitsUnfulfilled: 4,
			FixedRate:        10000,
			LeaseDuration:    2500,
		}),
	}
	err := h.db.SubmitOrder(ask)
	require.NoError(t, err)
	bid := &order.Bid{
		Kit: newKitFromTemplate(order.Nonce{0x02}, &order.Kit{
			MultiSigKeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyTowerSession,
				Index:  1,
			},
			Units:            4,
			UnitsUnfulfilled: 4,
			FixedRate:        10000,
			LeaseDuration:    2500,
		}),
	}
	err = h.db.SubmitOrder(bid)
	require.NoError(t, err)
	matchedAsk := &order.MatchedOrder{
		Order:       ask,
		UnitsFilled: 4,
		MultiSigKey: [33]byte{2, 3, 4},
		NodeKey:     node1Key,
		NodeAddrs:   []net.Addr{addr1},
	}
	matchedBid := &order.MatchedOrder{
		Order:       bid,
		UnitsFilled: 4,
		MultiSigKey: [33]byte{3, 4, 5},
		NodeKey:     node2Key,
		NodeAddrs:   []net.Addr{addr2},
	}

	_, fundingOutput1, _ := input.GenFundingPkScript(
		pubKeyAsk.SerializeCompressed(),
		matchedBid.MultiSigKey[:], int64(4*order.BaseSupplyUnit),
	)
	_, fundingOutput2, _ := input.GenFundingPkScript(
		pubKeyBid.SerializeCompressed(),
		matchedAsk.MultiSigKey[:], int64(4*order.BaseSupplyUnit),
	)

	batchTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{fundingOutput1, fundingOutput2},
	}
	batch := &order.Batch{
		ID: order.BatchID{9, 8, 7},
		MatchedOrders: map[order.Nonce][]*order.MatchedOrder{
			ask.Nonce(): {matchedBid},
			bid.Nonce(): {matchedAsk},
		},
		BatchTX: batchTx,
	}
	pendingChanID := order.PendingChanKey(ask.Nonce(), bid.Nonce())

	// Make sure the channel preparations work as expected. We expect the
	// bidder to connect out to the asker so the bidder is waiting for a
	// connection to the asker's peer to be established.
	h.baseClientMock.peerList = map[route.Vertex]string{
		node1Key: "1.1.1.1",
	}
	err = h.mgr.PrepChannelFunding(batch, h.db.GetOrder)
	require.NoError(t, err)

	// Verify we have the expected connections and funding shims registered.
	// We expect the bidder to connect to the asker and having registered
	// the funding shim while the asker is opening the channel.
	require.Eventually(t, func() bool {
		return len(h.lnMock.Connections()) == 1
	}, testTimeout, testTimeout/10)
	conns := h.lnMock.Connections()
	require.Equal(t, 1, len(conns))
	require.Equal(t, addr1.String(), conns[node1Key])
	require.Equal(t, 1, len(h.baseClientMock.fundingShims))

	// Validate the shim.
	shim := h.baseClientMock.fundingShims[pendingChanID]
	require.NotNil(t, shim)
	assertFundingShim(
		t, shim, uint32(2500), int64(order.SupplyUnit(4).ToSatoshis()),
		batchTx, 1, &keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: bid.Kit.MultiSigKeyLocator.Family,
				Index:  bid.Kit.MultiSigKeyLocator.Index,
			},
			PubKey: pubKeyBid,
		}, matchedAsk.MultiSigKey[:])

	// Next, make sure we get a partial reject error if we enable the "new
	// nodes only" flag and already have a channel with the matched node.
	h.mgr.cfg.NewNodesOnly = true
	h.lnMock.Channels = append(h.lnMock.Channels, lndclient.ChannelInfo{
		PubKeyBytes: node1Key,
	})
	err = h.mgr.PrepChannelFunding(batch, h.db.GetOrder)
	require.Error(t, err)

	expectedErr := &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*auctioneerrpc.OrderReject{
			ask.Nonce(): {
				ReasonCode: auctioneerrpc.OrderReject_DUPLICATE_PEER,
				Reason: "already have open/pending channel " +
					"with peer",
			},
		},
	}
	require.Equal(t, expectedErr, err)

	// As a last check of the funding preparation, make sure we get a reject
	// error if the connections to the remote peers couldn't be established.
	h.mgr.cfg.NewNodesOnly = false
	h.baseClientMock.peerList = make(map[route.Vertex]string)
	err = h.mgr.PrepChannelFunding(batch, h.db.GetOrder)
	require.Error(t, err)

	expectedErr = &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*auctioneerrpc.OrderReject{
			ask.Nonce(): {
				ReasonCode: auctioneerrpc.OrderReject_CHANNEL_FUNDING_FAILED,
				Reason: "connection not established before " +
					"timeout",
			},
		},
	}
	require.Equal(t, expectedErr, err)

	// With everything set up, let's now test the normal and sidecar batch
	// channel setup.
	h.lnMock.ScbKeyRing.EncryptionKey.PubKey = pubKeyAsk
	callBatchChannelSetup(t, h, batch, false)
	callBatchChannelSetup(t, h, batch, true)

	// Finally, make sure we get a timeout error if no channel open messages
	// are received.
	_, err = h.mgr.BatchChannelSetup(batch)
	require.Error(t, err)

	code := &auctioneerrpc.OrderReject{
		ReasonCode: auctioneerrpc.OrderReject_CHANNEL_FUNDING_FAILED,
		Reason: "timed out waiting for pending open " +
			"channel notification",
	}
	expectedErr = &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*auctioneerrpc.OrderReject{
			ask.Nonce(): code,
			bid.Nonce(): code,
		},
	}
	require.Equal(t, expectedErr, err)

	// Make sure the sidecar setup also times out if no updates come in.
	_, err = h.mgr.SidecarBatchChannelSetup(
		batch, h.mgr.pendingOpenChanClient, h.db.GetOrder,
	)
	require.Error(t, err)
	require.Equal(t, expectedErr, err)
}

func callBatchChannelSetup(t *testing.T, h *managerHarness, batch *order.Batch,
	sidecar bool) {

	txidHash := batch.BatchTX.TxHash()

	// Next, make sure we can complete the channel funding by opening the
	// channel for which we are the bidder. We'll also expect a channel open
	// message for the one where we are the asker so we simulate two msgs.
	go func() {
		timeout := time.After(time.Second)
		msg := &lnrpc.ChannelEventUpdate{
			Channel: &lnrpc.ChannelEventUpdate_PendingOpenChannel{
				PendingOpenChannel: &lnrpc.PendingUpdate{
					Txid:        txidHash[:],
					OutputIndex: 0,
				},
			},
		}
		// Send the message for the first channel.
		select {
		case h.baseClientMock.channelEvents <- msg:
		case <-timeout:
		}

		// And again for the second channel.
		msg2 := &lnrpc.ChannelEventUpdate{
			Channel: &lnrpc.ChannelEventUpdate_PendingOpenChannel{
				PendingOpenChannel: &lnrpc.PendingUpdate{
					Txid:        txidHash[:],
					OutputIndex: 1,
				},
			},
		}
		select {
		case h.baseClientMock.channelEvents <- msg2:
		case <-timeout:
		}
	}()

	// We need to fake channel backups as well. The mock creates them from
	// the open channels, so let's add two of those.
	h.lnMock.Channels = append(h.lnMock.Channels, lndclient.ChannelInfo{
		ChannelPoint: fmt.Sprintf("%s:0", txidHash.String()),
	})
	h.lnMock.Channels = append(h.lnMock.Channels, lndclient.ChannelInfo{
		ChannelPoint: fmt.Sprintf("%s:1", txidHash.String()),
	})

	var (
		chanInfo map[wire.OutPoint]*chaninfo.ChannelInfo
		err      error
	)
	if sidecar {
		chanInfo, err = h.mgr.SidecarBatchChannelSetup(
			batch, h.mgr.pendingOpenChanClient, h.db.GetOrder,
		)
	} else {
		chanInfo, err = h.mgr.BatchChannelSetup(batch)
	}
	require.NoError(t, err)
	require.Equal(t, 2, len(chanInfo))
}

// TestDeriveFundingShim makes sure the correct keys are used for creating a
// funding shim.
func TestDeriveFundingShim(t *testing.T) {
	h := newManagerHarness(t)
	defer h.stop()

	var (
		bidKeyIndex        int32 = 101
		sidecarKeyIndex    int32 = 102
		_, pubKeyAsk             = test.CreateKey(100)
		_, pubKeyBid             = test.CreateKey(bidKeyIndex)
		_, pubKeySidecar         = test.CreateKey(sidecarKeyIndex)
		askNonce                 = order.Nonce{1, 2, 3}
		bidNonce                 = order.Nonce{3, 2, 1}
		expectedKeyLocator       = keychain.KeyLocator{
			Family: 1234,
			Index:  uint32(bidKeyIndex),
		}
		expectedKeyLocatorSidecar = keychain.KeyLocator{
			Family: keychain.KeyFamilyMultiSig,
			Index:  uint32(sidecarKeyIndex),
		}
		batchTx = &wire.MsgTx{
			TxOut: []*wire.TxOut{{}},
		}
		batchHeightHint uint32 = 1337
	)

	askKit := order.NewKit(askNonce)
	matchedAsk := &order.MatchedOrder{
		Order:       &order.Ask{Kit: *askKit},
		UnitsFilled: 4,
	}
	copy(matchedAsk.MultiSigKey[:], pubKeyAsk.SerializeCompressed())

	// First test is a normal bid where the funding key should be derived
	// from the wallet
	bid := &order.Bid{
		Kit: newKitFromTemplate(bidNonce, &order.Kit{
			MultiSigKeyLocator: expectedKeyLocator,
			Units:              4,
			LeaseDuration:      12345,
		}),
	}
	_, batchTx.TxOut[0], _ = input.GenFundingPkScript(
		pubKeyBid.SerializeCompressed(),
		matchedAsk.MultiSigKey[:], int64(4*order.BaseSupplyUnit),
	)
	shim, pendingChanID, err := h.mgr.deriveFundingShim(
		bid, matchedAsk, batchTx, batchHeightHint,
	)
	require.NoError(t, err)
	require.Equal(t, order.PendingChanKey(askNonce, bidNonce), pendingChanID)
	require.IsType(t, shim.Shim, &lnrpc.FundingShim_ChanPointShim{})

	expectedKeyDescriptor := &keychain.KeyDescriptor{
		PubKey:     pubKeyBid,
		KeyLocator: expectedKeyLocator,
	}
	chanPointShim := shim.Shim.(*lnrpc.FundingShim_ChanPointShim)
	assertFundingShim(
		t, chanPointShim.ChanPointShim, 12345, 400_000, batchTx, 0,
		expectedKeyDescriptor, pubKeyAsk.SerializeCompressed(),
	)

	// And the second test is with a sidecar channel bid.
	ticket, err := sidecar.NewTicket(
		sidecar.VersionDefault, 400_000, 0, 12345, pubKeyBid, false,
	)
	require.NoError(t, err)
	ticket.Recipient = &sidecar.Recipient{
		MultiSigPubKey:   pubKeySidecar,
		MultiSigKeyIndex: uint32(sidecarKeyIndex),
	}
	bid = &order.Bid{
		Kit: newKitFromTemplate(bidNonce, &order.Kit{
			MultiSigKeyLocator: expectedKeyLocator,
			Units:              4,
			LeaseDuration:      12345,
		}),
		SidecarTicket: ticket,
	}
	_, batchTx.TxOut[0], _ = input.GenFundingPkScript(
		pubKeySidecar.SerializeCompressed(),
		matchedAsk.MultiSigKey[:], int64(4*order.BaseSupplyUnit),
	)
	shim, pendingChanID, err = h.mgr.deriveFundingShim(
		bid, matchedAsk, batchTx, batchHeightHint,
	)
	require.NoError(t, err)
	require.Equal(t, order.PendingChanKey(askNonce, bidNonce), pendingChanID)
	require.IsType(t, shim.Shim, &lnrpc.FundingShim_ChanPointShim{})

	expectedKeyDescriptor = &keychain.KeyDescriptor{
		PubKey:     pubKeySidecar,
		KeyLocator: expectedKeyLocatorSidecar,
	}
	chanPointShim = shim.Shim.(*lnrpc.FundingShim_ChanPointShim)
	assertFundingShim(
		t, chanPointShim.ChanPointShim, 12345, 400_000, batchTx, 0,
		expectedKeyDescriptor, pubKeyAsk.SerializeCompressed(),
	)
}

// assertFundingShim asserts that the funding shim contains the data that we
// expect.
func assertFundingShim(t *testing.T, shim *lnrpc.ChanPointShim,
	thawHeight uint32, amt int64, fundingTx *wire.MsgTx,
	fundingOutputIndex uint32, localKey *keychain.KeyDescriptor,
	remoteKey []byte) {

	require.Equal(t, thawHeight, shim.ThawHeight)
	require.Equal(t, amt, shim.Amt)

	fundingTxHash := fundingTx.TxHash()
	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: fundingTxHash[:],
		},
		OutputIndex: fundingOutputIndex,
	}
	require.Equal(t, chanPoint, shim.ChanPoint)
	require.Equal(
		t, localKey.PubKey.SerializeCompressed(),
		shim.LocalKey.RawKeyBytes,
	)
	require.Equal(t, int32(localKey.Family), shim.LocalKey.KeyLoc.KeyFamily)
	require.Equal(t, int32(localKey.Index), shim.LocalKey.KeyLoc.KeyIndex)
	require.Equal(t, remoteKey, shim.RemoteKey)
}

// TestWaitForPeerConnections tests the function that waits for peer connections
// to be established before continuing with the funding process.
func TestWaitForPeerConnections(t *testing.T) {
	h := newManagerHarness(t)
	defer h.stop()

	// First we make sure that if all peer are already connected, no peer
	// subscription is created.
	h.baseClientMock.peerList = map[route.Vertex]string{
		node1Key: "1.1.1.1",
		node2Key: "2.2.2.2",
	}
	ctxt, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	expectedConnections := map[route.Vertex]struct{}{
		node1Key: {},
		node2Key: {},
	}
	err := h.mgr.waitForPeerConnections(ctxt, expectedConnections, nil)
	require.NoError(t, err)

	// Next, make sure that connections established while waiting are
	// notified correctly. We simulate one connection already being done and
	// one finishing while we wait.
	h.baseClientMock.peerList = map[route.Vertex]string{
		node1Key: "1.1.1.1",
	}
	go func() {
		// Send an offline event first just to make sure it's consumed
		// but ignored.
		h.baseClientMock.peerEvents <- &lnrpc.PeerEvent{
			PubKey: hex.EncodeToString(node2Key[:]),
			Type:   lnrpc.PeerEvent_PEER_OFFLINE,
		}

		// Now send the event that the manager is waiting for.
		h.baseClientMock.peerEvents <- &lnrpc.PeerEvent{
			PubKey: hex.EncodeToString(node2Key[:]),
			Type:   lnrpc.PeerEvent_PEER_ONLINE,
		}
	}()
	ctxt, cancel = context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	expectedConnections = map[route.Vertex]struct{}{
		node1Key: {},
		node2Key: {},
	}
	err = h.mgr.waitForPeerConnections(ctxt, expectedConnections, nil)
	require.NoError(t, err)

	// Final test, make sure we get the correct error message back if we
	// run into a timeout or context cancellation when waiting for the
	// second node to be connected.
	fakeNonce1, fakeNonce2 := order.Nonce{77, 88}, order.Nonce{88, 99}
	fakeBatch := &order.Batch{
		MatchedOrders: map[order.Nonce][]*order.MatchedOrder{
			fakeNonce1: {{
				NodeKey: node1Key,
				Order: &order.Ask{
					Kit: newKitFromTemplate(
						fakeNonce1, &order.Kit{},
					),
				},
			}},
			fakeNonce2: {{
				NodeKey: node2Key,
				Order: &order.Ask{
					Kit: newKitFromTemplate(
						fakeNonce2, &order.Kit{},
					),
				},
			}},
		},
	}

	// Trigger a deadline exceeded error after a short timeout.
	ctxt, cancel = context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	expectedConnections = map[route.Vertex]struct{}{
		node1Key: {},
		node2Key: {},
	}
	err = h.mgr.waitForPeerConnections(ctxt, expectedConnections, fakeBatch)
	require.Error(t, err)

	code := &auctioneerrpc.OrderReject{
		ReasonCode: auctioneerrpc.OrderReject_CHANNEL_FUNDING_FAILED,
		Reason:     "connection not established before timeout",
	}
	expectedErr := &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*auctioneerrpc.OrderReject{
			fakeNonce2: code,
		},
	}
	require.Equal(t, expectedErr, err)

	// Do the same again, but now trigger a context cancel. We make the
	// context expire afterwards, since that will trigger the actual error
	// return.
	ctxt, cancel = context.WithTimeout(context.Background(), 2*testTimeout)
	defer cancel()

	go func() {
		time.Sleep(testTimeout)
		close(h.baseClientMock.cancelSub)
	}()

	err = h.mgr.waitForPeerConnections(ctxt, expectedConnections, fakeBatch)
	require.Error(t, err)
	require.Equal(t, expectedErr, err)
}

// TestOfferSidecarValidation checks the validation logic in the sidecar
// offering process.
func TestOfferSidecarValidation(t *testing.T) {
	h := newManagerHarness(t)
	defer h.stop()

	negativeCases := []struct {
		name        string
		capacity    btcutil.Amount
		pushAmt     btcutil.Amount
		expectedErr string
	}{{
		name:        "empty capacity",
		expectedErr: "channel capacity must be positive multiple of",
	}, {
		name:        "invalid capacity",
		capacity:    123,
		expectedErr: "channel capacity must be positive multiple of",
	}, {
		name:     "invalid push amount",
		capacity: 100000,
		pushAmt:  100001,
		expectedErr: "self channel balance must be smaller than " +
			"or equal to capacity",
	}}
	for _, testCase := range negativeCases {
		_, err := h.mgr.OfferSidecar(
			context.Background(), testCase.capacity,
			testCase.pushAmt, 2016, nil, nil, false,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), testCase.expectedErr)
	}
}

// TestOfferSidecar makes sure sidecar offers can be created and signed with the
// lnd node's identity key.
func TestOfferSidecar(t *testing.T) {
	h := newManagerHarness(t)
	defer h.stop()

	// We'll need a formally valid signature to pass the parsing. So we'll
	// just create a dummy signature from a random key pair.
	privKey, err := btcec.NewPrivateKey(btcec.S256())
	require.NoError(t, err)
	hash := sha256.New()
	_, _ = hash.Write([]byte("foo"))
	digest := hash.Sum(nil)
	sig, err := privKey.Sign(digest)
	require.NoError(t, err)

	h.mgr.cfg.NodePubKey = privKey.PubKey()
	h.signerMock.Signature = sig.Serialize()
	var nodeKeyRaw [33]byte
	copy(nodeKeyRaw[:], privKey.PubKey().SerializeCompressed())

	// Let's create our offer now.
	capacity, pushAmt := btcutil.Amount(100_000), btcutil.Amount(40_000)
	ticket, err := h.mgr.OfferSidecar(
		context.Background(), capacity, pushAmt, 2016,
		&keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		}, nil, false,
	)
	require.NoError(t, err)

	require.Equal(t, capacity, ticket.Offer.Capacity)
	require.Equal(t, pushAmt, ticket.Offer.PushAmt)
	require.Equal(t, privKey.PubKey(), ticket.Offer.SignPubKey)
	require.Equal(t, sig, ticket.Offer.SigOfferDigest)

	// Make sure the DB has the exact same ticket now.
	dbTicket, err := h.db.Sidecar(ticket.ID, privKey.PubKey())
	require.NoError(t, err)
	require.Equal(t, ticket, dbTicket)
}

func newKitFromTemplate(nonce order.Nonce, tpl *order.Kit) order.Kit {
	kit := order.NewKit(nonce)
	kit.Version = tpl.Version
	kit.State = tpl.State
	kit.FixedRate = tpl.FixedRate
	kit.Amt = tpl.Amt
	kit.Units = tpl.Units
	kit.UnitsUnfulfilled = tpl.UnitsUnfulfilled
	kit.MultiSigKeyLocator = tpl.MultiSigKeyLocator
	kit.MaxBatchFeeRate = tpl.MaxBatchFeeRate
	kit.AcctKey = tpl.AcctKey
	kit.LeaseDuration = tpl.LeaseDuration
	return *kit
}
