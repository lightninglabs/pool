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
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	proxy "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightninglabs/aperture/lsat"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/funding"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/perms"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

var (
	// minimalCompatibleVersion is the minimum version and build tags
	// required in lnd to run pool.
	minimalCompatibleVersion = &verrpc.Version{
		AppMajor: 0,
		AppMinor: 15,
		AppPatch: 4,

		// We don't actually require the invoicesrpc calls. But if we
		// try to use lndclient on an lnd that doesn't have it enabled,
		// the library will try to load the invoices.macaroon anyway and
		// fail. So until that bug is fixed in lndclient, we require the
		// build tag to be active.
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}

	// muSig2V100RC2Version is the version of lnd that enabled the MuSig2
	// v1.0.0-rc2 protocol in its MuSig2 RPC. We'll use this to decide what
	// account version to default to.
	muSig2V100RC2Version = &verrpc.Version{
		AppMajor: 0,
		AppMinor: 16,
		AppPatch: 0,
		BuildTags: []string{
			"signrpc", "walletrpc", "chainrpc", "invoicesrpc",
		},
	}

	// defaultClientPingTime is the default time we'll use for the client
	// keepalive ping time. This means the client will ping the server every
	// 10 seconds (if there is no other activity) to make sure the TCP
	// connection is still alive.
	defaultClientPingTime = 10 * time.Second
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
	sidecarAcceptor *SidecarAcceptor
	lsatStore       *lsat.FileStore
	lndServices     *lndclient.GrpcLndServices
	lndClient       lnrpc.LightningClient
	grpcServer      *grpc.Server
	restProxy       *http.Server
	grpcListener    net.Listener
	restListener    net.Listener
	restCancel      func()
	macaroonService *lndclient.MacaroonService
	macaroonDB      kvdb.Backend
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
	s.lndServices, err = getLnd(
		s.cfg.Network, s.cfg.Lnd, s.cfg.ShutdownInterceptor,
	)
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
		s.cfg.Lnd.Host, s.cfg.Lnd.TLSPath,
		path.Dir(s.cfg.Lnd.MacaroonPath), s.cfg.Network,
		lndclient.MacFilename(path.Base(s.cfg.Lnd.MacaroonPath)),
	)
	if err != nil {
		return err
	}

	// Create and start the macaroon service and let it create its default
	// macaroon in case it doesn't exist yet.
	rks, db, err := lndclient.NewBoltMacaroonStore(
		s.cfg.BaseDir, lncfg.MacaroonDBName,
		clientdb.DefaultPoolDBTimeout,
	)
	if err != nil {
		return err
	}
	shutdownFuncs["macaroondb"] = db.Close

	s.macaroonDB = db
	s.macaroonService, err = lndclient.NewMacaroonService(
		&lndclient.MacaroonServiceConfig{
			RootKeyStore:     rks,
			MacaroonLocation: poolMacaroonLocation,
			MacaroonPath:     s.cfg.MacaroonPath,
			Checkers: []macaroons.Checker{
				macaroons.IPLockChecker,
			},
			RequiredPerms: perms.RequiredPermissions,
			DBPassword:    macDbDefaultPw,
			LndClient:     &s.lndServices.LndServices,
			EphemeralKey:  lndclient.SharedKeyNUMS,
			KeyLocator:    lndclient.SharedKeyLocator,
		},
	)
	if err != nil {
		return err
	}

	if err := s.macaroonService.Start(); err != nil {
		return err
	}
	shutdownFuncs["macaroon"] = s.macaroonService.Stop

	// Setup the auctioneer client and interceptor.
	err = s.setupClient()
	if err != nil {
		return err
	}
	shutdownFuncs["clientdb"] = s.db.Close
	shutdownFuncs["auctioneer"] = s.AuctioneerClient.Stop

	// Instantiate the trader gRPC server and start it.
	s.rpcServer, err = newRPCServer(s)
	if err != nil {
		return err
	}
	err = s.rpcServer.Start()
	if err != nil {
		return err
	}
	shutdownFuncs["rpcServer"] = s.rpcServer.Stop

	// Let's create our interceptor chain, starting with the security
	// interceptors that will check macaroons for their validity.
	unaryMacIntercept, streamMacIntercept, err := s.macaroonService.Interceptors()
	if err != nil {
		return fmt.Errorf("error with macaroon interceptor: %v", err)
	}

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

		// The default JSON marshaler of the REST proxy only sets
		// OrigName to true, which instructs it to use the same field
		// names as specified in the proto file and not switch to camel
		// case. What we also want is that the marshaler prints all
		// values, even if they are falsey.
		customMarshalerOption := proxy.WithMarshalerOption(
			proxy.MIMEWildcard, &proxy.JSONPb{
				MarshalOptions: protojson.MarshalOptions{
					UseProtoNames:   true,
					EmitUnpopulated: true,
				},
			},
		)

		// We'll also create and start an accompanying proxy to serve
		// clients through REST.
		var ctx context.Context
		ctx, s.restCancel = context.WithCancel(context.Background())
		mux := proxy.NewServeMux(customMarshalerOption)
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

		s.restProxy = &http.Server{Handler: mux} //nolint:gosec
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
	lndGrpc *lndclient.GrpcLndServices, withMacaroonService bool) error {

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

	if withMacaroonService {
		// Create and start the macaroon service and let it create its
		// default macaroon in case it doesn't exist yet.
		rks, db, err := lndclient.NewBoltMacaroonStore(
			s.cfg.BaseDir, lncfg.MacaroonDBName,
			clientdb.DefaultPoolDBTimeout,
		)
		if err != nil {
			return err
		}
		shutdownFuncs["macaroondb"] = db.Close

		s.macaroonDB = db
		s.macaroonService, err = lndclient.NewMacaroonService(
			&lndclient.MacaroonServiceConfig{
				RootKeyStore:     rks,
				MacaroonLocation: poolMacaroonLocation,
				MacaroonPath:     s.cfg.MacaroonPath,
				Checkers: []macaroons.Checker{
					macaroons.IPLockChecker,
				},
				RequiredPerms: perms.RequiredPermissions,
				DBPassword:    macDbDefaultPw,
				LndClient:     &s.lndServices.LndServices,
				EphemeralKey:  lndclient.SharedKeyNUMS,
				KeyLocator:    lndclient.SharedKeyLocator,
			},
		)
		if err != nil {
			return err
		}

		if err := s.macaroonService.Start(); err != nil {
			return err
		}
		shutdownFuncs["macaroon"] = s.macaroonService.Stop
	}

	// Setup the auctioneer client and interceptor.
	err := s.setupClient()
	if err != nil {
		return err
	}
	shutdownFuncs["clientdb"] = s.db.Close
	shutdownFuncs["auctioneer"] = s.AuctioneerClient.Stop

	// Instantiate the trader gRPC server and start it.
	s.rpcServer, err = newRPCServer(s)
	if err != nil {
		return err
	}
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

	if s.macaroonService == nil {
		return fmt.Errorf("macaroon service has not been initialised")
	}

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

	// Parse our lnd node's public key.
	nodePubKey, err := btcec.ParsePubKey(s.lndServices.NodePubkey[:])
	if err != nil {
		return fmt.Errorf("unable to parse node pubkey: %v", err)
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
		defaultLsatMaxCost, s.cfg.LsatMaxRoutingFee, false,
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

	// For non-regtest networks we also want to turn on gRPC keepalive to
	// detect stale connections. We don't do this for regtest because there
	// might be older regtest-only servers out there where this would lead
	// to disconnects because the server doesn't allow pings that often
	// (since this requires a server side change to be deployed as well).
	if s.cfg.Network != "regtest" {
		s.cfg.AuctioneerDialOpts = append(
			s.cfg.AuctioneerDialOpts,
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time: defaultClientPingTime,
			}),
		)
	}

	// Create the funding manager. The RPC server is responsible for
	// starting/stopping it though as all that logic is currently there for
	// the other managers as well.
	channelAcceptor := NewChannelAcceptor(s.lndServices.Client)
	s.fundingManager = funding.NewManager(&funding.ManagerConfig{
		DB:                s.db,
		WalletKit:         s.lndServices.WalletKit,
		LightningClient:   s.lndServices.Client,
		SignerClient:      s.lndServices.Signer,
		BaseClient:        s.lndClient,
		NodePubKey:        nodePubKey,
		BatchStepTimeout:  order.DefaultBatchStepTimeout,
		NewNodesOnly:      s.cfg.NewNodesOnly,
		NotifyShimCreated: channelAcceptor.ShimRegistered,
	})

	batchVersion, err := s.determineBatchVersion()
	if err != nil {
		return err
	}

	// Create an instance of the auctioneer client library.
	clientCfg := &auctioneer.Config{
		ServerAddress: s.cfg.AuctionServer,
		ProxyAddress:  s.cfg.Proxy,
		Insecure:      s.cfg.Insecure,
		TLSPathServer: s.cfg.TLSPathAuctSrv,
		DialOpts:      s.cfg.AuctioneerDialOpts,
		Signer:        s.lndServices.Signer,
		MinBackoff:    s.cfg.MinBackoff,
		MaxBackoff:    s.cfg.MaxBackoff,
		BatchSource:   s.db,
		BatchCleaner:  s.fundingManager,
		BatchVersion:  batchVersion,
		GenUserAgent: func(ctx context.Context) string {
			return UserAgent(InitiatorFromContext(ctx))
		},
	}

	// Create the acceptors for receiving sidecar channels. We need to
	// create a copy of the auctioneer client configuration because the
	// acceptor is going to overwrite some of its values.
	clientCfgCopy := *clientCfg
	s.sidecarAcceptor = NewSidecarAcceptor(&SidecarAcceptorConfig{
		SidecarDB:      s.db,
		AcctDB:         &accountStore{DB: s.db},
		Signer:         s.lndServices.Signer,
		Wallet:         s.lndServices.WalletKit,
		BaseClient:     s.lndClient,
		Acceptor:       channelAcceptor,
		NodePubKey:     nodePubKey,
		ClientCfg:      clientCfgCopy,
		FundingManager: s.fundingManager,
		PrepareOrder: func(ctx context.Context,
			order order.Order,
			acct *account.Account,
			terms *terms.AuctioneerTerms) (*order.ServerOrderParams, error) {

			// Rather than passing in the function directly, we use
			// an intermediate closure as this pointer won't
			// existing when we initialize this config, as the rpc
			// server is created _after_ we set up the client.
			return s.rpcServer.orderManager.PrepareOrder(
				ctx, order, acct, terms,
			)
		},
		FetchSidecarBid: s.db.SidecarBidTemplate,
	})

	// Create an instance of the auctioneer client library.
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

	// The gRPC server might be nil if started as a subserver.
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}

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
	if s.macaroonService != nil {
		if err := s.macaroonService.Stop(); err != nil {
			log.Errorf("Error stopping macaroon service: %v", err)
		}
	}
	if s.macaroonDB != nil {
		if err := s.macaroonDB.Close(); err != nil {
			log.Errorf("Error closing macaroon DB: %v", err)
		}
	}

	s.lndServices.Close()
	s.wg.Wait()

	if shutdownErr != nil {
		return fmt.Errorf("error shutting down server: %v", shutdownErr)
	}
	return nil
}

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *LndConfig,
	interceptor signal.Interceptor) (*lndclient.GrpcLndServices, error) {

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
		case <-interceptor.ShutdownChannel():
			cancel()

		// The check was completed and the above defer canceled the
		// context. We can just exit the goroutine, nothing more to do.
		case <-ctxc.Done():
		}
	}()

	return lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:            cfg.Host,
		Network:               lndclient.Network(network),
		CustomMacaroonPath:    cfg.MacaroonPath,
		TLSPath:               cfg.TLSPath,
		CheckVersion:          minimalCompatibleVersion,
		BlockUntilChainSynced: true,
		BlockUntilUnlocked:    true,
		CallerCtx:             ctxc,
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

// StreamInterceptor intercepts streaming requests and appends the dummy LSAT
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

	addUpdate := func(nonce order.Nonce, state order.State) {
		noncesToUpdate = append(noncesToUpdate, nonce)
		ordersToUpdate = append(
			ordersToUpdate,
			[]order.Modifier{
				order.StateModifier(state),
			},
		)
	}
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
			switch {
			// If the server does not know about this order mark it
			// as failed and allow the client start up with the
			// rest of valid orders.
			//
			// NOTE: This should never happen under normal
			// conditions. This was added to handle a specific
			// failure that's being investigated.
			case auctioneer.IsOrderNotFoundErr(err):
				addUpdate(orderNonce, order.StateFailed)

			default:
				return fmt.Errorf("unable to fetch order(%v): "+
					"%v", orderNonce, err)
			}
			continue
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
			addUpdate(orderNonce, order.StateCanceled)
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

// determineBatchVersion determines the batch version that will be sent to the
// auctioneer to signal compatibility with the different features the trader
// client can support.
func (s *Server) determineBatchVersion() (order.BatchVersion, error) {
	configVersion := s.cfg.DebugConfig.BatchVersion

	// We set the default value of the config flag to -1, so we can
	// differentiate between no value set and the first version (0).
	if configVersion >= 0 {
		log.Infof("Using configured batch version %d for connecting "+
			"to auctioneer", configVersion)

		return order.BatchVersion(configVersion), nil
	}

	ctxt, cancel := context.WithTimeout(
		context.Background(), getInfoTimeout,
	)
	defer cancel()

	// We will try to decide what version to used based on the latest
	// batch version + what features the node supports.
	info, err := s.lndClient.GetInfo(ctxt, &lnrpc.GetInfoRequest{})
	if err != nil {
		return 0, err
	}

	baseVersion := order.UpgradeAccountTaprootBatchVersion

	// If the node supports ZeroConfChannels use that batch version.
	_, zeroConfOpt := info.Features[uint32(lnwire.ZeroConfOptional)]
	_, zeroConfReq := info.Features[uint32(lnwire.ZeroConfRequired)]
	supportsZeroConf := zeroConfOpt || zeroConfReq

	_, SCIDAliasOpt := info.Features[uint32(lnwire.ScidAliasOptional)]
	_, SCIDAliasReq := info.Features[uint32(lnwire.ScidAliasRequired)]
	supportsSCIDAlias := SCIDAliasOpt || SCIDAliasReq

	if supportsZeroConf && supportsSCIDAlias {
		baseVersion |= order.ZeroConfChannelsFlag
	}

	// We can only use the new version of the MuSig2 protocol if we have a
	// recent lnd version that added support for specifying the MuSig2
	// version in the RPC.
	currentLndVersion := s.lndServices.Version
	verErr := lndclient.AssertVersionCompatible(
		currentLndVersion, muSig2V100RC2Version,
	)
	if verErr == nil {
		baseVersion |= order.UpgradeAccountTaprootV2Flag
	}

	log.Infof("Using batch version %d for connecting to auctioneer",
		baseVersion)

	return baseVersion, nil
}
