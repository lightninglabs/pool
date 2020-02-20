package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/lsat"
	"github.com/lightningnetwork/lnd/build"
	"google.golang.org/grpc"
)

// Server is the main agora trader server.
type Server struct {
	cfg              *Config
	lndServices      *lndclient.GrpcLndServices
	auctioneerClient *auctioneer.Client
	traderServer     *rpcServer
	grpcServer       *grpc.Server
	restProxy        *http.Server
	grpcListener     net.Listener
	restListener     net.Listener
	wg               sync.WaitGroup
}

// NewServer creates a new trader server.
func NewServer(cfg *Config) (*Server, error) {
	// Append the network type to the log directory so it is
	// "namespaced" per network in the same fashion as the data directory.
	cfg.LogDir = filepath.Join(cfg.LogDir, cfg.Network)

	// Initialize logging at the default logging level.
	err := logWriter.InitLogRotator(
		filepath.Join(cfg.LogDir, DefaultLogFilename),
		cfg.MaxLogFileSize, cfg.MaxLogFiles,
	)
	if err != nil {
		return nil, err
	}
	err = build.ParseAndSetDebugLevels(cfg.DebugLevel, logWriter)
	if err != nil {
		return nil, err
	}

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

	// Setup the LSAT interceptor for the client.
	networkDir := filepath.Join(cfg.BaseDir, cfg.Network)
	fileStore, err := lsat.NewFileStore(networkDir)
	if err != nil {
		return nil, err
	}
	interceptor := lsat.NewInterceptor(
		&lndServices.LndServices, fileStore, defaultRPCTimeout,
		defaultLsatMaxCost, defaultLsatMaxFee,
	)
	cfg.AuctioneerDialOpts = append(
		cfg.AuctioneerDialOpts,
		grpc.WithUnaryInterceptor(interceptor.UnaryInterceptor),
		grpc.WithStreamInterceptor(interceptor.StreamInterceptor),
	)

	// Create an instance of the auctioneer client library.
	auctioneerClient, err := auctioneer.NewClient(
		cfg.AuctionServer, cfg.Insecure, cfg.TLSPathAuctSrv,
		lndServices.WalletKit, cfg.AuctioneerDialOpts...,
	)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:              cfg,
		lndServices:      lndServices,
		auctioneerClient: auctioneerClient,
	}, nil
}

// Start runs agorad in daemon mode. It will listen for grpc connections,
// execute commands and pass back auction status information.
func (s *Server) Start() error {
	var err error

	// Instantiate the agorad gRPC server.
	networkDir := filepath.Join(s.cfg.BaseDir, s.cfg.Network)
	s.traderServer, err = newRPCServer(
		&s.lndServices.LndServices, s.auctioneerClient, networkDir,
	)
	if err != nil {
		return err
	}

	serverOpts := []grpc.ServerOption{}
	s.grpcServer = grpc.NewServer(serverOpts...)
	clmrpc.RegisterTraderServer(s.grpcServer, s.traderServer)

	// Next, start the gRPC server listening for HTTP/2 connections.
	// If the provided grpcListener is not nil, it means agorad is being
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
				log.Errorf("could not start rest listener: %v",
					err)
			}
		}()
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
			log.Error(err)
		}
	}()

	return nil
}

// Stop shuts down the server, including the auction server connection, all
// client connections and network listeners.
func (s *Server) Stop() error {
	log.Info("Received shutdown signal, stopping server")
	err := s.traderServer.Stop()
	if err != nil {
		return fmt.Errorf("error shutting down server: %v", err)
	}
	s.grpcServer.GracefulStop()
	err = s.grpcListener.Close()
	if err != nil {
		return fmt.Errorf("error closing gRPC listener: %v", err)
	}
	if s.restProxy != nil {
		err := s.restProxy.Shutdown(context.Background())
		if err != nil {
			return fmt.Errorf("error shutting down REST proxy: %v",
				err)
		}
		err = s.restListener.Close()
		if err != nil {
			return fmt.Errorf("error shutting down REST listener: "+
				"%v", err)
		}
	}
	s.lndServices.Close()

	s.wg.Wait()
	return nil
}

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *LndConfig) (*lndclient.GrpcLndServices, error) {
	return lndclient.NewLndServices(
		cfg.Host, network, cfg.MacaroonDir, cfg.TLSPath,
	)
}
