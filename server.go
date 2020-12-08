package pool

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

var (
	// minimalCompatibleVersion is the minimum version and build tags
	// required in lnd to run pool.
	minimalCompatibleVersion = &verrpc.Version{
		AppMajor: 0,
		AppMinor: 11,
		AppPatch: 1,

		// We don't actually require the invoicesrpc calls. But if we
		// try to use lndclient on an lnd that doesn't have it enabled,
		// the library will try to load the invoices.macaroon anyway and
		// fail. So until that bug is fixed in lndclient, we require the
		// build tag to be active.
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}
)

// Server is the main poold trader server.
type Server struct {
	// To be used atomically.
	started int32

	*rpcServer

	// AuctioneerClient is the wrapper around the connection from the trader
	// client to the auctioneer server. It is exported so we can replace
	// the connection with a new one in the itest, if the server is
	// restarted.
	AuctioneerClient *auctioneer.Client

	// GetIdentity returns the current LSAT identification of the trader
	// client or an error if none has been established yet.
	GetIdentity func() (*lsat.TokenID, error)

	cfg             *Config
	db              *clientdb.DB
	fundingManager  *funding.Manager
	lsatStore       *lsat.FileStore
	lndServices     *lndclient.GrpcLndServices
	lndClient       lnrpc.LightningClient
	grpcServer      *grpc.Server
	restProxy       *http.Server
	grpcListener    net.Listener
	restListener    net.Listener
	restCancel      func()
	macaroonService *macaroons.Service
	wg              sync.WaitGroup
}

// NewServer creates a new trader server.
func NewServer(cfg *Config) *Server {
	return &Server{
		cfg: cfg,
	}
}

// Start runs poold in daemon mode. It will listen for grpc connections, execute
// commands and pass back auction status information.
func (s *Server) Start() error {
	if atomic.AddInt32(&s.started, 1) != 1 {
		return fmt.Errorf("trader can only be started once")
	}

	// Print the version before executing either primary directive.
	log.Infof("Version: %v", Version())

	// Depending on how far we got in initializing the server, we might need
	// to clean up certain services that were already started. Keep track of
	// them with this map of service name to shutdown function.
	shutdownFuncs := make(map[string]func() error)
	defer func() {
		for serviceName, shutdownFn := range shutdownFuncs {
			if err := shutdownFn(); err != nil {
				log.Errorf("Error shutting down %s service: %v",
					serviceName, err)
			}
		}
	}()

	var err error
	s.lndServices, err = getLnd(s.cfg.Network, s.cfg.Lnd)
	if err != nil {
		return err
	}
	shutdownFuncs["lnd"] = func() error { // nolint:unparam
		s.lndServices.Close()
		return nil
	}

	// As there're some other lower-level operations we may need access to,
	// we'll also make a connection for a "basic client".
	//
	// TODO(roasbeef): more granular macaroons, can ask user to make just
	// what we need
	s.lndClient, err = lndclient.NewBasicClient(
		s.cfg.Lnd.Host, s.cfg.Lnd.TLSPath, s.cfg.Lnd.MacaroonDir,
		s.cfg.Network,
	)
	if err != nil {
		return err
	}

	// Start the macaroon service and let it create its default macaroon in
	// case it doesn't exist yet.
	if err := s.startMacaroonService(); err != nil {
		return err
	}
	shutdownFuncs["macaroon"] = s.stopMacaroonService

	// Setup the auctioneer client and interceptor.
	err = s.setupClient()
	if err != nil {
		return err
	}
	shutdownFuncs["clientdb"] = s.db.Close
	shutdownFuncs["auctioneer"] = s.AuctioneerClient.Stop

	// Instantiate the trader gRPC server and start it.
	s.rpcServer = newRPCServer(s)
	err = s.rpcServer.Start()
	if err != nil {
		return err
	}
	shutdownFuncs["rpcServer"] = s.rpcServer.Stop

	// Let's create our interceptor chain, starting with the security
	// interceptors that will check macaroons for their validity.
	unaryMacIntercept, streamMacIntercept := s.macaroonInterceptor()
	serverOpts := []grpc.ServerOption{
		grpc.ChainStreamInterceptor(
			errorLogStreamServerInterceptor(rpcLog),
			streamMacIntercept,
		),
		grpc.ChainUnaryInterceptor(
			errorLogUnaryServerInterceptor(rpcLog),
			unaryMacIntercept,
		),
	}
	s.grpcServer = grpc.NewServer(serverOpts...)
	poolrpc.RegisterTraderServer(s.grpcServer, s.rpcServer)

	// We'll need to start the server with TLS and connect the REST proxy
	// client to it.
	serverTLSCfg, restClientCreds, err := getTLSConfig(s.cfg)
	if err != nil {
		return fmt.Errorf("could not create gRPC server options: %v",
			err)
	}

	// Next, start the gRPC server listening for HTTP/2 connections.
	// If the provided grpcListener is not nil, it means poold is being
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
		var ctx context.Context
		ctx, s.restCancel = context.WithCancel(context.Background())
		mux := proxy.NewServeMux()
		proxyOpts := []grpc.DialOption{
			grpc.WithTransportCredentials(*restClientCreds),
		}

		// With TLS enabled by default, we cannot call 0.0.0.0
		// internally from the REST proxy as that IP address isn't in
		// the cert. We need to rewrite it to the loopback address.
		restProxyDest := s.cfg.RPCListen
		switch {
		case strings.Contains(restProxyDest, "0.0.0.0"):
			restProxyDest = strings.Replace(
				restProxyDest, "0.0.0.0", "127.0.0.1", 1,
			)

		case strings.Contains(restProxyDest, "[::]"):
			restProxyDest = strings.Replace(
				restProxyDest, "[::]", "[::1]", 1,
			)
		}
		err = poolrpc.RegisterTraderHandlerFromEndpoint(
			ctx, mux, restProxyDest, proxyOpts,
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
		s.restListener = tls.NewListener(s.restListener, serverTLSCfg)
		shutdownFuncs["restListener"] = s.restListener.Close

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
	s.grpcListener = tls.NewListener(s.grpcListener, serverTLSCfg)
	shutdownFuncs["rpcListener"] = s.grpcListener.Close

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
	err = s.syncLocalOrderState()
	if err != nil {
		return err
	}

	// If we got here successfully, there's no need to shutdown anything
	// anymore.
	shutdownFuncs = nil

	return nil
}

// StartAsSubserver is an alternative start method where the RPC server does not
// create its own gRPC server but registers on an existing one.
func (s *Server) StartAsSubserver(lndClient lnrpc.LightningClient,
	lndGrpc *lndclient.GrpcLndServices) error {

	if atomic.AddInt32(&s.started, 1) != 1 {
		return fmt.Errorf("trader can only be started once")
	}

	s.lndClient = lndClient
	s.lndServices = lndGrpc

	// Print the version before executing either primary directive.
	log.Infof("Version: %v", Version())

	// Depending on how far we got in initializing the server, we might need
	// to clean up certain services that were already started. Keep track of
	// them with this map of service name to shutdown function.
	shutdownFuncs := make(map[string]func() error)
	defer func() {
		for serviceName, shutdownFn := range shutdownFuncs {
			if err := shutdownFn(); err != nil {
				log.Errorf("Error shutting down %s service: %v",
					serviceName, err)
			}
		}
	}()

	// Start the macaroon service and let it create its default macaroon in
	// case it doesn't exist yet.
	if err := s.startMacaroonService(); err != nil {
		return err
	}
	shutdownFuncs["macaroon"] = s.stopMacaroonService

	// Setup the auctioneer client and interceptor.
	err := s.setupClient()
	if err != nil {
		return err
	}
	shutdownFuncs["clientdb"] = s.db.Close
	shutdownFuncs["auctioneer"] = s.AuctioneerClient.Stop

	// Instantiate the trader gRPC server and start it.
	s.rpcServer = newRPCServer(s)
	err = s.rpcServer.Start()
	if err != nil {
		return err
	}
	shutdownFuncs["rpcServer"] = s.rpcServer.Stop

	// The final thing we'll do on start up is sync the order state of the
	// auctioneer with what we have on disk.
	err = s.syncLocalOrderState()
	if err != nil {
		return err
	}

	// If we got here successfully, there's no need to shutdown anything
	// anymore.
	shutdownFuncs = nil

	return nil
}

// ValidateMacaroon extracts the macaroon from the context's gRPC metadata,
// checks its signature, makes sure all specified permissions for the called
// method are contained within and finally ensures all caveat conditions are
// met. A non-nil error is returned if any of the checks fail. This method is
// needed to enable poold running as an external subserver in the same process
// as lnd but still validate its own macaroons.
func (s *Server) ValidateMacaroon(ctx context.Context,
	requiredPermissions []bakery.Op, fullMethod string) error {

	// Delegate the call to pool's own macaroon validator service.
	return s.macaroonService.ValidateMacaroon(
		ctx, requiredPermissions, fullMethod,
	)
}

// setupClient initializes the auctioneer client and its interceptors.
func (s *Server) setupClient() error {
	// If no auction server is specified, use the default addresses for
	// mainnet and testnet.
	if s.cfg.AuctionServer == "" && len(s.cfg.AuctioneerDialOpts) == 0 {
		switch s.cfg.Network {
		case "mainnet":
			s.cfg.AuctionServer = MainnetServer
		case "testnet":
			s.cfg.AuctionServer = TestnetServer
		default:
			return errors.New("no auction server address specified")
		}
	}

	log.Infof("Auction server address: %v", s.cfg.AuctionServer)

	// Open the main database.
	var err error
	s.db, err = clientdb.New(s.cfg.BaseDir, clientdb.DBFilename)
	if err != nil {
		return err
	}

	// Setup the LSAT interceptor for the client.
	s.lsatStore, err = lsat.NewFileStore(s.cfg.BaseDir)
	if err != nil {
		return err
	}

	// GetIdentity can be used to determine the current LSAT identification
	// of the trader.
	s.GetIdentity = func() (*lsat.TokenID, error) {
		token, err := s.lsatStore.CurrentToken()
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
	var interceptor Interceptor = lsat.NewInterceptor(
		&s.lndServices.LndServices, s.lsatStore, defaultRPCTimeout,
		defaultLsatMaxCost, defaultLsatMaxFee, false,
	)
	if s.cfg.FakeAuth && s.cfg.Network == "mainnet" {
		return fmt.Errorf("cannot use fake LSAT auth for mainnet")
	}
	if s.cfg.FakeAuth {
		var tokenID lsat.TokenID
		_, _ = rand.Read(tokenID[:])
		interceptor = &regtestInterceptor{id: tokenID}
		s.GetIdentity = func() (*lsat.TokenID, error) {
			return &tokenID, nil
		}
	}
	activeLoggers := logWriter.SubLoggers()
	s.cfg.AuctioneerDialOpts = append(
		s.cfg.AuctioneerDialOpts,
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

	// Create the funding manager.
	s.fundingManager = &funding.Manager{
		DB:               s.db,
		WalletKit:        s.lndServices.WalletKit,
		LightningClient:  s.lndServices.Client,
		BaseClient:       s.lndClient,
		BatchStepTimeout: funding.DefaultBatchStepTimeout,
		NewNodesOnly:     s.cfg.NewNodesOnly,
		PendingOpenChannels: make(
			chan *lnrpc.ChannelEventUpdate_PendingOpenChannel,
		),
	}

	// Create an instance of the auctioneer client library.
	clientCfg := &auctioneer.Config{
		ServerAddress: s.cfg.AuctionServer,
		Insecure:      s.cfg.Insecure,
		TLSPathServer: s.cfg.TLSPathAuctSrv,
		DialOpts:      s.cfg.AuctioneerDialOpts,
		Signer:        s.lndServices.Signer,
		MinBackoff:    s.cfg.MinBackoff,
		MaxBackoff:    s.cfg.MaxBackoff,
		BatchSource:   s.db,
		BatchCleaner:  s.fundingManager,
	}
	s.AuctioneerClient, err = auctioneer.NewClient(clientCfg)
	if err != nil {
		return err
	}

	return s.AuctioneerClient.Start()
}

// Stop shuts down the server, including the auction server connection, all
// client connections and network listeners.
func (s *Server) Stop() error {
	log.Info("Received shutdown signal, stopping server")

	var shutdownErr error

	// Don't return any errors yet, give everything else a chance to shut
	// down first.
	err := s.AuctioneerClient.Stop()
	if err != nil {
		shutdownErr = err
	}
	err = s.rpcServer.Stop()
	if err != nil {
		shutdownErr = err
	}
	s.grpcServer.GracefulStop()

	if s.restCancel != nil {
		s.restCancel()
	}
	if s.restProxy != nil {
		err := s.restProxy.Shutdown(context.Background())
		if err != nil {
			log.Errorf("Error shutting down REST proxy: %v", err)
		}
	}
	if err := s.db.Close(); err != nil {
		log.Errorf("Error closing DB: %v", err)
	}
	if err := s.macaroonService.Close(); err != nil {
		log.Errorf("Error stopping macaroon service: %v", err)
	}
	s.lndServices.Close()
	s.wg.Wait()

	if shutdownErr != nil {
		return fmt.Errorf("error shutting down server: %v", shutdownErr)
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
