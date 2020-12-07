package funding

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
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

type fundingBaseClientMock struct {
	lightningClient *test.MockLightning
	fundingShims    map[[32]byte]*lnrpc.ChanPointShim

	peerList   map[route.Vertex]string
	peerEvents chan *lnrpc.PeerEvent
	cancelSub  chan struct{}

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
	msgChan        chan *lnrpc.ChannelEventUpdate_PendingOpenChannel
	lnMock         *test.MockLightning
	baseClientMock *fundingBaseClientMock
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
	msgChan := make(chan *lnrpc.ChannelEventUpdate_PendingOpenChannel)
	lightningClient := test.NewMockLightning()
	walletKitClient := test.NewMockWalletKit()
	baseClientMock := &fundingBaseClientMock{
		lightningClient: lightningClient,
		fundingShims:    make(map[[32]byte]*lnrpc.ChanPointShim),
		peerList:        make(map[route.Vertex]string),
		peerEvents:      make(chan *lnrpc.PeerEvent),
		cancelSub:       make(chan struct{}),
		quit:            quit,
	}
	return &managerHarness{
		t:              t,
		tempDir:        tempDir,
		db:             db,
		quit:           quit,
		msgChan:        msgChan,
		lnMock:         lightningClient,
		baseClientMock: baseClientMock,
		mgr: &Manager{
			DB:                  db,
			WalletKit:           walletKitClient,
			LightningClient:     lightningClient,
			BaseClient:          baseClientMock,
			NewNodesOnly:        true,
			PendingOpenChannels: msgChan,
			BatchStepTimeout:    400 * time.Millisecond,
		},
	}
}

func (m *managerHarness) stop() {
	close(m.quit)
	require.NoError(m.t, m.db.Close())
	require.NoError(m.t, os.RemoveAll(m.tempDir))
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
	txidHash := batchTx.TxHash()
	pendingChanID := order.PendingChanKey(ask.Nonce(), bid.Nonce())

	// Make sure the channel preparations work as expected. We expect the
	// bidder to connect out to the asker so the bidder is waiting for a
	// connection to the asker's peer to be established.
	h.baseClientMock.peerList = map[route.Vertex]string{
		node1Key: "1.1.1.1",
	}
	err = h.mgr.PrepChannelFunding(batch, false, h.quit)
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
	require.Equal(t, uint32(2500), shim.ThawHeight)
	require.Equal(t, int64(order.SupplyUnit(4).ToSatoshis()), shim.Amt)

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: txidHash[:],
		},
		OutputIndex: 1,
	}
	require.Equal(t, chanPoint, shim.ChanPoint)
	require.Equal(
		t, pubKeyBid.SerializeCompressed(), shim.LocalKey.RawKeyBytes,
	)
	require.Equal(
		t, int32(bid.Kit.MultiSigKeyLocator.Family),
		shim.LocalKey.KeyLoc.KeyFamily,
	)
	require.Equal(
		t, int32(bid.Kit.MultiSigKeyLocator.Index),
		shim.LocalKey.KeyLoc.KeyIndex,
	)
	require.Equal(t, matchedAsk.MultiSigKey[:], shim.RemoteKey)

	// Next, make sure we get a partial reject error if we enable the "new
	// nodes only" flag and already have a channel with the matched node.
	h.mgr.NewNodesOnly = true
	h.lnMock.Channels = append(h.lnMock.Channels, lndclient.ChannelInfo{
		PubKeyBytes: node1Key,
	})
	err = h.mgr.PrepChannelFunding(batch, false, h.quit)
	require.Error(t, err)

	expectedErr := &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*poolrpc.OrderReject{
			ask.Nonce(): {
				ReasonCode: poolrpc.OrderReject_DUPLICATE_PEER,
				Reason: "already have open/pending channel " +
					"with peer",
			},
		},
	}
	require.Equal(t, expectedErr, err)

	// As a last check of the funding preparation, make sure we get a reject
	// error if the connections to the remote peers couldn't be established.
	h.mgr.NewNodesOnly = false
	h.baseClientMock.peerList = make(map[route.Vertex]string)
	err = h.mgr.PrepChannelFunding(batch, false, h.quit)
	require.Error(t, err)

	expectedErr = &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*poolrpc.OrderReject{
			ask.Nonce(): {
				ReasonCode: poolrpc.OrderReject_CHANNEL_FUNDING_FAILED,
				Reason: "connection not established before " +
					"timeout",
			},
		},
	}
	require.Equal(t, expectedErr, err)

	// Next, make sure we can complete the channel funding by opening the
	// channel for which we are the bidder. We'll also expect a channel open
	// message for the one where we are the asker so we simulate two msgs.
	go func() {
		timeout := time.After(time.Second)
		msg := &lnrpc.ChannelEventUpdate_PendingOpenChannel{
			PendingOpenChannel: &lnrpc.PendingUpdate{
				Txid:        txidHash[:],
				OutputIndex: 0,
			},
		}
		// Send the message for the first channel.
		select {
		case h.msgChan <- msg:
		case <-timeout:
		}

		// And again for the second channel.
		msg2 := &lnrpc.ChannelEventUpdate_PendingOpenChannel{
			PendingOpenChannel: &lnrpc.PendingUpdate{
				Txid:        txidHash[:],
				OutputIndex: 1,
			},
		}
		select {
		case h.msgChan <- msg2:
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
	h.lnMock.ScbKeyRing.EncryptionKey.PubKey = pubKeyAsk
	chanInfo, err := h.mgr.BatchChannelSetup(batch, h.quit)
	require.NoError(t, err)
	require.Equal(t, 2, len(chanInfo))

	// Finally, make sure we get a timeout error if no channel open messages
	// are received.
	_, err = h.mgr.BatchChannelSetup(batch, h.quit)
	require.Error(t, err)

	code := &poolrpc.OrderReject{
		ReasonCode: poolrpc.OrderReject_CHANNEL_FUNDING_FAILED,
		Reason: "timed out waiting for pending open " +
			"channel notification",
	}
	expectedErr = &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*poolrpc.OrderReject{
			ask.Nonce(): code,
			bid.Nonce(): code,
		},
	}
	require.Equal(t, expectedErr, err)
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
	err := h.mgr.waitForPeerConnections(
		ctxt, expectedConnections, nil, h.quit,
	)
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
	err = h.mgr.waitForPeerConnections(
		ctxt, expectedConnections, nil, h.quit,
	)
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
	err = h.mgr.waitForPeerConnections(
		ctxt, expectedConnections, fakeBatch, h.quit,
	)
	require.Error(t, err)

	code := &poolrpc.OrderReject{
		ReasonCode: poolrpc.OrderReject_CHANNEL_FUNDING_FAILED,
		Reason:     "connection not established before timeout",
	}
	expectedErr := &MatchRejectErr{
		RejectedOrders: map[order.Nonce]*poolrpc.OrderReject{
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

	err = h.mgr.waitForPeerConnections(
		ctxt, expectedConnections, fakeBatch, h.quit,
	)
	require.Error(t, err)
	require.Equal(t, expectedErr, err)
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
