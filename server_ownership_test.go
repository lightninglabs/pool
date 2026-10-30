package pool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
)

const (
	// testMockLndHeight is the block height the mock lnd node reports.
	testMockLndHeight = 600

	// testMockLndPubkey is the identity pubkey the mock lnd node
	// reports.
	testMockLndPubkey = "03f5374b16f0b1f1b49101de1b9d89e0b460bc57ce9c2f9" +
		"132b73dfc76d3704daa"

	// testMacaroonHex is a pre-baked macaroon (root key
	// "dummy-root-key", id "42", location "lnd") that is used as the
	// macaroon for all lnd services. The mock lnd server does not
	// validate macaroons, the hex only needs to parse.
	testMacaroonHex = "0201036c6e6402023432000006205a6adf86f5fef1c3738c7" +
		"c299da15c0bb5e2c3d3f84e89090bbad9e070ee6663"
)

// The following mock servers implement the minimal subset of lnd's gRPC
// services that is required to create an lndclient.GrpcLndServices and to
// start an embedded pool trader server. They are used to verify that the
// injected lnd connection is not closed by the subserver on shutdown. Each
// service gets its own struct because the embedded Unimplemented types have
// overlapping method names.

// mockLightningServer implements lnd's main Lightning gRPC service.
type mockLightningServer struct {
	lnrpc.UnimplementedLightningServer
}

// GetInfo returns static node information.
func (m *mockLightningServer) GetInfo(_ context.Context,
	_ *lnrpc.GetInfoRequest) (*lnrpc.GetInfoResponse, error) {

	return &lnrpc.GetInfoResponse{
		IdentityPubkey: testMockLndPubkey,
		Alias:          "mock-lnd",
		Color:          "#3399ff",
		Version:        "0.21.0-beta",
		BlockHeight:    testMockLndHeight,
		BlockHash: "00000000000000000000000000000000" +
			"00000000000000000000000000000000",
		SyncedToChain: true,
		SyncedToGraph: true,
		Chains: []*lnrpc.Chain{
			{
				Chain:   "bitcoin",
				Network: "testnet",
			},
		},
	}, nil
}

// SubscribeChannelEvents blocks until the subscription is canceled.
func (m *mockLightningServer) SubscribeChannelEvents(
	_ *lnrpc.ChannelEventSubscription,
	stream lnrpc.Lightning_SubscribeChannelEventsServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// ChannelAcceptor blocks until the stream is canceled.
func (m *mockLightningServer) ChannelAcceptor(
	stream lnrpc.Lightning_ChannelAcceptorServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// mockStateServer implements lnd's State gRPC service.
type mockStateServer struct {
	lnrpc.UnimplementedStateServer
}

// SubscribeState reports the wallet as unlocked and then blocks.
func (m *mockStateServer) SubscribeState(_ *lnrpc.SubscribeStateRequest,
	stream lnrpc.State_SubscribeStateServer) error {

	err := stream.Send(&lnrpc.SubscribeStateResponse{
		State: lnrpc.WalletState_RPC_ACTIVE,
	})
	if err != nil {
		return err
	}

	<-stream.Context().Done()

	return stream.Context().Err()
}

// mockVersionerServer implements lnd's verrpc gRPC service.
type mockVersionerServer struct {
	verrpc.UnimplementedVersionerServer
}

// GetVersion returns a version that satisfies the minimum required lnd
// version of the pool daemon.
func (m *mockVersionerServer) GetVersion(_ context.Context,
	_ *verrpc.VersionRequest) (*verrpc.Version, error) {

	return &verrpc.Version{
		AppMajor: 0,
		AppMinor: 21,
		AppPatch: 0,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc",
			"invoicesrpc",
		},
	}, nil
}

// mockChainNotifierServer implements lnd's chainrpc gRPC service.
type mockChainNotifierServer struct {
	chainrpc.UnimplementedChainNotifierServer
}

// RegisterBlockEpochNtfn sends the current block height once (like lnd
// does on registration) and then blocks until the subscription is
// canceled.
func (m *mockChainNotifierServer) RegisterBlockEpochNtfn(
	_ *chainrpc.BlockEpoch,
	stream chainrpc.ChainNotifier_RegisterBlockEpochNtfnServer) error {

	err := stream.Send(&chainrpc.BlockEpoch{
		Height: testMockLndHeight,
	})
	if err != nil {
		return err
	}

	<-stream.Context().Done()

	return stream.Context().Err()
}

// RegisterConfirmationsNtfn blocks until the subscription is canceled.
func (m *mockChainNotifierServer) RegisterConfirmationsNtfn(
	_ *chainrpc.ConfRequest,
	stream chainrpc.ChainNotifier_RegisterConfirmationsNtfnServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// RegisterSpendNtfn blocks until the subscription is canceled.
func (m *mockChainNotifierServer) RegisterSpendNtfn(
	_ *chainrpc.SpendRequest,
	stream chainrpc.ChainNotifier_RegisterSpendNtfnServer) error {

	<-stream.Context().Done()

	return stream.Context().Err()
}

// mockWalletKitServer implements lnd's walletrpc gRPC service.
type mockWalletKitServer struct {
	walletrpc.UnimplementedWalletKitServer
}

// ListUnspent returns an empty UTXO list.
func (m *mockWalletKitServer) ListUnspent(_ context.Context,
	_ *walletrpc.ListUnspentRequest) (*walletrpc.ListUnspentResponse,
	error) {

	return &walletrpc.ListUnspentResponse{}, nil
}

// EstimateFee returns a static fee estimate.
func (m *mockWalletKitServer) EstimateFee(_ context.Context,
	_ *walletrpc.EstimateFeeRequest) (*walletrpc.EstimateFeeResponse,
	error) {

	return &walletrpc.EstimateFeeResponse{
		SatPerKw: 12500,
	}, nil
}

// startMockLnd starts the mock lnd gRPC server on an in-memory listener and
// returns the listener to dial.
func startMockLnd(t *testing.T) *bufconn.Listener {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(grpc.Creds(
		credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{testTLSCert(t)},
		}),
	))

	lnrpc.RegisterLightningServer(server, &mockLightningServer{})
	lnrpc.RegisterStateServer(server, &mockStateServer{})
	verrpc.RegisterVersionerServer(server, &mockVersionerServer{})
	chainrpc.RegisterChainNotifierServer(
		server, &mockChainNotifierServer{},
	)
	walletrpc.RegisterWalletKitServer(server, &mockWalletKitServer{})
	invoicesrpc.RegisterInvoicesServer(
		server, &invoicesrpc.UnimplementedInvoicesServer{},
	)
	routerrpc.RegisterRouterServer(
		server, &routerrpc.UnimplementedRouterServer{},
	)
	signrpc.RegisterSignerServer(
		server, &signrpc.UnimplementedSignerServer{},
	)

	go func() {
		_ = server.Serve(lis)
	}()

	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
	})

	return lis
}

// newInjectedLndServices creates a real lndclient.GrpcLndServices backed by
// a real gRPC connection to the mock lnd server. This mirrors exactly what
// LiT creates once and then injects into its subservers.
func newInjectedLndServices(t *testing.T,
	lis *bufconn.Listener) *lndclient.GrpcLndServices {

	t.Helper()

	services, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:        "bufnet",
		Network:           lndclient.NetworkTestnet,
		CustomMacaroonHex: testMacaroonHex,
		Insecure:          true,
		Dialer: func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		},
		CheckVersion: minimalCompatibleVersion,
	})
	require.NoError(t, err)

	return services
}

// testTLSCert creates an ephemeral self-signed TLS certificate for the mock
// lnd server. The lndclient is configured with Insecure=true, which skips
// certificate verification but still requires the server to speak TLS.
func testTLSCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "mock-lnd",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature |
			x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"mock-lnd"},
	}

	der, err := x509.CreateCertificate(
		rand.Reader, template, template, &key.PublicKey, key,
	)
	require.NoError(t, err)

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
}

// TestSubserverDoesNotCloseSharedLndConn is a regression test for the
// connection ownership bug that caused an embedded pool trader server to
// close the lnd connection that LiT shares among all its subservers.
// Stopping the embedded server must not close the injected lnd connection.
func TestSubserverDoesNotCloseSharedLndConn(t *testing.T) {
	// Start the mock lnd node and create the shared lnd connection,
	// exactly like LiT does before starting its subservers.
	lis := startMockLnd(t)
	services := newInjectedLndServices(t, lis)
	conn := services.ClientConn

	cfg := DefaultConfig()
	cfg.Network = "testnet"
	cfg.BaseDir = t.TempDir()
	cfg.LogDir = cfg.BaseDir
	cfg.AuctionServer = "127.0.0.1:1"
	cfg.Insecure = true
	cfg.FakeAuth = true

	// Start the trader as a subserver with the shared (borrowed) lnd
	// connection and a basic client on the same connection, exactly like
	// LiT passes them.
	server := NewServer(&cfg)
	basicClient := lnrpc.NewLightningClient(conn)
	err := server.StartAsSubserver(basicClient, services, false)
	require.NoError(t, err)

	// Stopping the trader server must not close the shared lnd
	// connection, it is owned by the parent process.
	require.NoError(t, server.Stop())
	require.NotEqual(
		t, connectivity.Shutdown, conn.GetState(),
		"subserver closed the shared lnd connection",
	)

	// The owner of the connection (the test, acting as the parent
	// process) cleans up.
	services.Close()
	require.Equal(t, connectivity.Shutdown, conn.GetState())
}

// TestOwnedLndConnClosedOnStop verifies that the trader server still closes
// the lnd connection when it owns it (standalone mode), i.e. that the
// ownership guard does not leak connections that the server created itself.
func TestOwnedLndConnClosedOnStop(t *testing.T) {
	lis := startMockLnd(t)
	services := newInjectedLndServices(t, lis)
	conn := services.ClientConn

	// Simulate a server that created its own lnd connection (standalone
	// mode via Start) by marking the connection as owned.
	cfg := DefaultConfig()
	cfg.Network = "testnet"
	cfg.BaseDir = t.TempDir()

	server := NewServer(&cfg)
	server.lndServices = services
	server.lndOwned = true

	// Stopping the server must close the connection it owns. We call the
	// relevant part of the shutdown logic directly as the rest of the
	// server was never started.
	server.stopLnd()
	require.Equal(t, connectivity.Shutdown, conn.GetState())
}
