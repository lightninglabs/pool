package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/llm/auctioneer"
	"github.com/lightninglabs/llm/clientdb"
	"github.com/lightninglabs/llm/clmrpc"
	"github.com/lightninglabs/llm/order"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

var (
	// minimalCompatibleVersion is the minimum version and build tags
	// required in lnd to run llm.
	minimalCompatibleVersion = &verrpc.Version{
		AppMajor:  0,
		AppMinor:  11,
		AppPatch:  0,
		BuildTags: []string{"signrpc", "walletrpc", "chainrpc"},
	}
)

// Server is the main llmd trader server.
type Server struct {
	// AuctioneerClient is the wrapper around the connection from the trader
	// client to the auctioneer server. It is exported so we can replace
	// the connection with a new one in the itest, if the server is
	// restarted.
	AuctioneerClient *auctioneer.Client

	// GetIdentity returns the current LSAT identification of the trader
	// client or an error if none has been established yet.
	GetIdentity func() (*lsat.TokenID, error)

	cfg          *Config
	db           *clientdb.DB
	lsatStore    *lsat.FileStore
	lndServices  *lndclient.GrpcLndServices
	lndClient    lnrpc.LightningClient
	traderServer *rpcServer
	grpcServer   *grpc.Server
	restProxy    *http.Server
	grpcListener net.Listener
	restListener net.Listener
	wg           sync.WaitGroup
}

// NewServer creates a new trader server.
func NewServer(cfg *Config) (*Server, error) {
	// Print the version before executing either primary directive.
	log.Infof("Version: %v", Version())

	lndServices, err := getLnd(cfg.Network, cfg.Lnd)
	if err != nil {
		return nil, err
	}

	// If no auction server is specified, use the default addresses for
	// mainnet and testnet.
	if cfg.AuctionServer == "" && len(cfg.AuctioneerDialOpts) == 0 {
		switch cfg.Network {
		case "mainnet":
			cfg.AuctionServer = MainnetServer
		case "testnet":
			cfg.AuctionServer = TestnetServer
		default:
			return nil, errors.New("no auction server address " +
				"specified")
		}
	}

	log.Infof("Auction server address: %v", cfg.AuctionServer)

	// Open the main database.
	networkDir := filepath.Join(cfg.BaseDir, cfg.Network)
	db, err := clientdb.New(networkDir)
	if err != nil {
		return nil, err
	}

	// Setup the LSAT interceptor for the client.
	fileStore, err := lsat.NewFileStore(networkDir)
	if err != nil {
		return nil, err
	}
	var interceptor Interceptor = lsat.NewInterceptor(
		&lndServices.LndServices, fileStore, defaultRPCTimeout,
		defaultLsatMaxCost, defaultLsatMaxFee, false,
	)

	// getIdentity can be used to determine the current LSAT identification
	// of the trader.
	getIdentity := func() (*lsat.TokenID, error) {
		token, err := fileStore.CurrentToken()
		if err != nil {
			return nil, err
		}
		macID, err := lsat.DecodeIdentifier(
			bytes.NewBuffer(token.BaseMacaroon().Id()),
		)
		if err != nil {
			return nil, err
		}
		return &macID.TokenID, nil
	}

	// For any net that isn't mainnet, we allow LSAT auth to be disabled and
	// create a fixed identity that is used for the whole runtime of the
	// trader instead.
	if cfg.FakeAuth && cfg.Network == "mainnet" {
		return nil, fmt.Errorf("cannot use fake LSAT auth for mainnet")
	}
	if cfg.FakeAuth {
		var tokenID lsat.TokenID
		_, _ = rand.Read(tokenID[:])
		interceptor = &regtestInterceptor{id: tokenID}
		getIdentity = func() (*lsat.TokenID, error) {
			return &tokenID, nil
		}
	}
	activeLoggers := logWriter.SubLoggers()
	cfg.AuctioneerDialOpts = append(
		cfg.AuctioneerDialOpts,
		grpc.WithChainUnaryInterceptor(
			interceptor.UnaryInterceptor,
			errorLogUnaryClientInterceptor(
				activeLoggers[auctioneer.Subsystem],
			),
		),
		grpc.WithChainStreamInterceptor(
			interceptor.StreamInterceptor,
			errorLogStreamClientInterceptor(
				activeLoggers[auctioneer.Subsystem],
			),
		),
	)

	// Create an instance of the auctioneer client library.
	clientCfg := &auctioneer.Config{
		ServerAddress: cfg.AuctionServer,
		Insecure:      cfg.Insecure,
		TLSPathServer: cfg.TLSPathAuctSrv,
		DialOpts:      cfg.AuctioneerDialOpts,
		Signer:        lndServices.Signer,
		MinBackoff:    cfg.MinBackoff,
		MaxBackoff:    cfg.MaxBackoff,
		BatchSource:   db,
	}
	auctioneerClient, err := auctioneer.NewClient(clientCfg)
	if err != nil {
		return nil, err
	}

	// As there're some other lower-level operations we may need access to,
	// we'll also make a connection for a "basic client".
	//
	// TODO(roasbeef): more granular macaroons, can ask user to make just
	// what we need
	baseClient, err := lndclient.NewBasicClient(
		cfg.Lnd.Host, cfg.Lnd.TLSPath, cfg.Lnd.MacaroonDir, cfg.Network,
	)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:              cfg,
		db:               db,
		lsatStore:        fileStore,
		lndServices:      lndServices,
		lndClient:        baseClient,
		AuctioneerClient: auctioneerClient,
		GetIdentity:      getIdentity,
	}, nil
}

// Start runs llmd in daemon mode. It will listen for grpc connections,
// execute commands and pass back auction status information.
func (s *Server) Start() error {
	var err error

	// Instantiate the llmd gRPC server.
	s.traderServer = newRPCServer(s)

	serverOpts := []grpc.ServerOption{
		grpc.StreamInterceptor(
			errorLogStreamServerInterceptor(rpcLog),
		),
		grpc.UnaryInterceptor(
			errorLogUnaryServerInterceptor(rpcLog),
		),
	}
	s.grpcServer = grpc.NewServer(serverOpts...)
	clmrpc.RegisterTraderServer(s.grpcServer, s.traderServer)

	// Next, start the gRPC server listening for HTTP/2 connections.
	// If the provided grpcListener is not nil, it means llmd is being
	// used as a library and the listener might not be a real network
	// connection (but maybe a UNIX socket or bufconn). So we don't spin up
	// a REST listener in that case.
	log.Infof("Starting gRPC listener")
	s.grpcListener = s.cfg.RPCListener
	if s.grpcListener == nil {
		s.grpcListener, err = net.Listen("tcp", s.cfg.RPCListen)
		if err != nil {
			return fmt.Errorf("RPC server unable to listen on %s",
				s.cfg.RPCListen)

		}

		// We'll also create and start an accompanying proxy to serve
		// clients through REST.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mux := proxy.NewServeMux()
		proxyOpts := []grpc.DialOption{grpc.WithInsecure()}
		err = clmrpc.RegisterTraderHandlerFromEndpoint(
			ctx, mux, s.cfg.RPCListen, proxyOpts,
		)
		if err != nil {
			return err
		}

		log.Infof("Starting REST proxy listener")
		s.restListener, err = net.Listen("tcp", s.cfg.RESTListen)
		if err != nil {
			return fmt.Errorf("REST proxy unable to listen on %s",
				s.cfg.RESTListen)
		}
		s.restProxy = &http.Server{Handler: mux}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			err := s.restProxy.Serve(s.restListener)
			if err != nil && err != http.ErrServerClosed {
				log.Errorf("Could not start rest listener: %v",
					err)
			}
		}()
	}

	err = s.AuctioneerClient.Start()
	if err != nil {
		return err
	}

	// Start the trader server itself.
	err = s.traderServer.Start()
	if err != nil {
		return err
	}

	// Start the grpc server.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		log.Infof("RPC server listening on %s", s.grpcListener.Addr())
		if s.restListener != nil {
			log.Infof("REST proxy listening on %s",
				s.restListener.Addr())
		}

		err = s.grpcServer.Serve(s.grpcListener)
		if err != nil {
			log.Errorf("Unable to server gRPC: %v", err)
		}
	}()

	// The final thing we'll do on start up is sync the order state of the
	// auctioneer with what we have on disk.
	return s.syncLocalOrderState()
}

// Stop shuts down the server, including the auction server connection, all
// client connections and network listeners.
func (s *Server) Stop() error {
	log.Info("Received shutdown signal, stopping server")

	err := s.AuctioneerClient.Stop()
	if err != nil {
		return err
	}

	// Don't return any errors yet, give everything else a chance to shut
	// down first.
	err = s.traderServer.Stop()
	s.grpcServer.GracefulStop()
	if s.restProxy != nil {
		err := s.restProxy.Shutdown(context.Background())
		if err != nil {
			log.Errorf("Error shutting down REST proxy: %v", err)
		}
	}
	if err := s.db.Close(); err != nil {
		log.Errorf("Error closing DB: %v", err)
	}
	s.lndServices.Close()
	s.wg.Wait()

	if err != nil {
		return fmt.Errorf("error shutting down server: %v", err)
	}
	return nil
}

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *LndConfig) (*lndclient.GrpcLndServices, error) {
	// We'll want to wait for lnd to be fully synced to its chain backend.
	// The call to NewLndServices will block until the sync is completed.
	// But we still want to be able to shutdown the daemon if the user
	// decides to not wait. For that we can pass down a context that we
	// cancel on shutdown.
	ctxc, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Make sure the context is canceled if the user requests shutdown.
	go func() {
		select {
		// Client requests shutdown, cancel the wait.
		case <-signal.ShutdownChannel():
			cancel()

		// The check was completed and the above defer canceled the
		// context. We can just exit the goroutine, nothing more to do.
		case <-ctxc.Done():
		}
	}()

	return lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:            cfg.Host,
		Network:               lndclient.Network(network),
		MacaroonDir:           cfg.MacaroonDir,
		TLSPath:               cfg.TLSPath,
		CheckVersion:          minimalCompatibleVersion,
		BlockUntilChainSynced: true,
		ChainSyncCtx:          ctxc,
	})
}

// Interceptor is the interface a client side gRPC interceptor has to implement.
type Interceptor interface {
	// UnaryInterceptor intercepts normal, non-streaming requests from the
	// client to the server.
	UnaryInterceptor(context.Context, string, interface{}, interface{},
		*grpc.ClientConn, grpc.UnaryInvoker, ...grpc.CallOption) error

	// StreamInterceptor intercepts streaming requests from the client to
	// the server.
	StreamInterceptor(context.Context, *grpc.StreamDesc, *grpc.ClientConn,
		string, grpc.Streamer, ...grpc.CallOption) (grpc.ClientStream,
		error)
}

// regtestInterceptor is a dummy gRPC interceptor that can be used on regtest to
// simulate identification through LSAT.
type regtestInterceptor struct {
	id lsat.TokenID
}

// UnaryInterceptor intercepts non-streaming requests and appends the dummy LSAT
// ID.
func (i *regtestInterceptor) UnaryInterceptor(ctx context.Context, method string,
	req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {

	idStr := fmt.Sprintf("LSATID %x", i.id[:])
	idCtx := metadata.AppendToOutgoingContext(
		ctx, lsat.HeaderAuthorization, idStr,
	)
	return invoker(idCtx, method, req, reply, cc, opts...)
}

// StreamingInterceptor intercepts streaming requests and appends the dummy LSAT
// ID.
func (i *regtestInterceptor) StreamInterceptor(ctx context.Context,
	desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
	streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream,
	error) {

	idStr := fmt.Sprintf("LSATID %x", i.id[:])
	idCtx := metadata.AppendToOutgoingContext(
		ctx, lsat.HeaderAuthorization, idStr,
	)
	return streamer(idCtx, desc, cc, method, opts...)
}

// syncLocalOrderState synchronizes the order state of the local database with
// that of the remote auctioneer. Whenever order state diverges, we take the
// order state of the server but only if the new state is a cancel. Any other
// order discrepancies should be resolved once we process any possibly
// partially processed batches.
func (s *Server) syncLocalOrderState() error {
	// First, we'll read out the on-disk DB state so we can compare with what
	// the server has shortly.
	dbOrders, err := s.db.GetOrders()
	if err != nil {
		return fmt.Errorf("unable to read db orders: %v", err)
	}

	var (
		noncesToUpdate []order.Nonce
		ordersToUpdate [][]order.Modifier
	)
	for _, dbOrder := range dbOrders {
		orderNonce := dbOrder.Nonce()
		localOrderState := dbOrder.Details().State

		// If the order is cancelled or in any other terminal state, we
		// don't need to query the server for its current status.
		if localOrderState.Archived() {
			continue
		}

		// For our comparison below, we'll poll the auctioneer to find
		// out what state they think this order is in.
		orderStateResp, err := s.AuctioneerClient.OrderState(
			context.Background(), orderNonce,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch order state: %v", err)
		}
		remoteOrderState, err := rpcOrderStateToDBState(
			orderStateResp.State,
		)
		if err != nil {
			return err
		}

		// If the local and remote order states match, then there's no
		// need to update the local state.
		if localOrderState == remoteOrderState {
			continue
		}

		// Only if the remote state is a cancelled order will we update
		// our local state. Otherwise, this should be handled after we
		// complete the handshake and poll to see if there're any
		// pending batches we need to process.
		if remoteOrderState == order.StateCanceled {
			noncesToUpdate = append(noncesToUpdate, orderNonce)
			ordersToUpdate = append(
				ordersToUpdate,
				[]order.Modifier{
					order.StateModifier(order.StateCanceled),
				},
			)

			log.Debugf("Order(%v) is cancelled, updating local "+
				"state", orderNonce)
		} else {
			log.Warnf("Order(%v) has de-synced state, "+
				"local_state=%v, remote_state=%v", localOrderState,
				remoteOrderState)
		}
	}

	// Now that we know the set of orders we need to update, we'll update
	// them all in an atomic batch.
	return s.db.UpdateOrders(noncesToUpdate, ordersToUpdate)
}
