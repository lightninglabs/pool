package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/trader"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/build"
	"google.golang.org/grpc"
)

// Start runs agorad in daemon mode. It will listen for grpc connections,
// execute commands and pass back auction status information.
func Start(config *Config) error {
	// Show the version and exit if the version flag was specified.
	appName := filepath.Base(os.Args[0])
	appName = strings.TrimSuffix(appName, filepath.Ext(appName))
	if config.ShowVersion {
		fmt.Println(appName, "version", Version())
		os.Exit(0)
	}

	// Special show command to list supported subsystems and exit.
	if config.DebugLevel == "show" {
		fmt.Printf("Supported subsystems: %v\n",
			logWriter.SupportedSubsystems())
		os.Exit(0)
	}

	// Append the network type to the log directory so it is
	// "namespaced" per network in the same fashion as the data directory.
	config.LogDir = filepath.Join(config.LogDir, config.Network)

	// Initialize logging at the default logging level.
	err := logWriter.InitLogRotator(
		filepath.Join(config.LogDir, DefaultLogFilename),
		config.MaxLogFileSize, config.MaxLogFiles,
	)
	if err != nil {
		return err
	}
	err = build.ParseAndSetDebugLevels(config.DebugLevel, logWriter)
	if err != nil {
		return err
	}

	// Print the version before executing either primary directive.
	log.Infof("Version: %v", Version())

	lnd, err := getLnd(config.Network, config.Lnd)
	if err != nil {
		return err
	}
	defer lnd.Close()

	// If no auction server is specified, use the default addresses for
	// mainnet and testnet.
	if config.AuctionServer == "" && len(config.AuctioneerDialOpts) == 0 {
		switch config.Network {
		case "mainnet":
			config.AuctionServer = MainnetServer
		case "testnet":
			config.AuctionServer = TestnetServer
		default:
			return errors.New("no auction server address specified")
		}
	}

	log.Infof("Auction server address: %v", config.AuctionServer)

	// Create an instance of the auctioneer client library.
	auctioneerClient, cleanup, err := auctioneer.NewClient(
		config.AuctionServer, config.Insecure, config.TLSPathAuctSrv,
		lnd.WalletKit, config.AuctioneerDialOpts...,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	// Instantiate the agorad gRPC server.
	networkDir := filepath.Join(config.BaseDir, config.Network)
	traderServer, err := trader.NewServer(
		&lnd.LndServices, auctioneerClient, networkDir,
	)
	if err != nil {
		return err
	}

	serverOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(serverOpts...)
	clmrpc.RegisterChannelAuctioneerClientServer(grpcServer, traderServer)

	// Next, start the gRPC server listening for HTTP/2 connections.
	// If the provided grpcListener is not nil, it means agorad is being
	// used as a library and the listener might not be a real network
	// connection (but maybe a UNIX socket or bufconn). So we don't spin up
	// a REST listener in that case.
	log.Infof("Starting gRPC listener")
	var (
		wg           sync.WaitGroup
		grpcListener = config.RPCListener
		restListener net.Listener
		restProxy    *http.Server
	)
	if grpcListener == nil {
		grpcListener, err = net.Listen("tcp", config.RPCListen)
		if err != nil {
			return fmt.Errorf("RPC server unable to listen on %s",
				config.RPCListen)

		}
		defer closeOrLog(grpcListener)

		// We'll also create and start an accompanying proxy to serve
		// clients through REST.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		mux := proxy.NewServeMux()
		proxyOpts := []grpc.DialOption{grpc.WithInsecure()}
		err = clmrpc.RegisterChannelAuctioneerClientHandlerFromEndpoint(
			ctx, mux, config.RPCListen, proxyOpts,
		)
		if err != nil {
			return err
		}

		log.Infof("Starting REST proxy listener")
		restListener, err = net.Listen("tcp", config.RESTListen)
		if err != nil {
			return fmt.Errorf("REST proxy unable to listen on %s",
				config.RESTListen)
		}
		defer closeOrLog(restListener)
		restProxy = &http.Server{Handler: mux}
		wg.Add(1)
		go func() {
			defer wg.Done()

			err := restProxy.Serve(restListener)
			if err != nil && err != http.ErrServerClosed {
				log.Errorf("could not start rest listener: %v",
					err)
			}
		}()
	}

	// Start the trader server itself.
	err = traderServer.Start()
	if err != nil {
		return err
	}

	// Start the grpc server.
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Infof("RPC server listening on %s", grpcListener.Addr())
		if restListener != nil {
			log.Infof("REST proxy listening on %s",
				restListener.Addr())
		}

		err = grpcServer.Serve(grpcListener)
		if err != nil {
			log.Error(err)
		}
	}()

	// Run until the user terminates agorad.
	<-config.ShutdownChannel
	log.Info("Received shutdown signal, stopping server")
	grpcServer.GracefulStop()
	if restProxy != nil {
		err := restProxy.Shutdown(context.Background())
		if err != nil {
			log.Errorf("error shutting down REST proxy: %v", err)
		}
	}
	err = traderServer.Stop()
	if err != nil {
		log.Errorf("error shutting down server: %v", err)
	}

	wg.Wait()
	return nil
}

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *LndConfig) (*lndclient.GrpcLndServices, error) {
	return lndclient.NewLndServices(
		cfg.Host, network, cfg.MacaroonDir, cfg.TLSPath,
	)
}

func closeOrLog(c io.Closer) {
	err := c.Close()
	if err != nil {
		log.Errorf("could not close: %v", err)
	}
}
