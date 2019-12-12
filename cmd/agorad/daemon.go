package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/trader"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
)

// daemon runs agorad in daemon mode. It will listen for grpc connections,
// execute commands and pass back auction status information.
func daemon(config *config) error {
	lnd, err := getLnd(config.Network, config.Lnd)
	if err != nil {
		return err
	}
	defer lnd.Close()

	// If no auction server is specified, use the default addresses for
	// mainnet and testnet.
	if config.AuctionServer == "" {
		switch config.Network {
		case "mainnet":
			config.AuctionServer = mainnetServer
		case "testnet":
			config.AuctionServer = testnetServer
		default:
			return errors.New("no auction server address specified")
		}
	}

	log.Infof("Auction server address: %v", config.AuctionServer)

	// Create an instance of the auctioneer client library.
	auctioneerClient, cleanup, err := auctioneer.NewClient(
		config.AuctionServer, config.Insecure, config.TLSPathAuctSrv,
		lnd.WalletKit,
	)
	if err != nil {
		return err
	}
	defer cleanup()

	// Instantiate the agorad gRPC server.
	dbDir, err := getStoreDir(config.Network)
	if err != nil {
		return err
	}
	traderServer, err := trader.NewServer(
		&lnd.LndServices, auctioneerClient, dbDir,
	)
	if err != nil {
		return err
	}

	serverOpts := []grpc.ServerOption{}
	grpcServer := grpc.NewServer(serverOpts...)
	clmrpc.RegisterChannelAuctioneerClientServer(grpcServer, traderServer)

	// Next, start the gRPC server listening for HTTP/2 connections.
	log.Infof("Starting gRPC listener")
	grpcListener, err := net.Listen("tcp", config.RPCListen)
	if err != nil {
		return fmt.Errorf("RPC server unable to listen on %s",
			config.RPCListen)

	}
	defer grpcListener.Close()

	// We'll also create and start an accompanying proxy to serve clients
	// through REST.
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
	restListener, err := net.Listen("tcp", config.RESTListen)
	if err != nil {
		return fmt.Errorf("REST proxy unable to listen on %s",
			config.RESTListen)
	}
	defer restListener.Close()
	restProxy := &http.Server{Handler: mux}
	go func() {
		err := restProxy.Serve(restListener)
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("could not start rest listener: %v", err)
		}
	}()

	// Start the trader server itself.
	err = traderServer.Start()
	if err != nil {
		return err
	}

	// Start the grpc server.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Infof("RPC server listening on %s", grpcListener.Addr())
		log.Infof("REST proxy listening on %s", restListener.Addr())

		err = grpcServer.Serve(grpcListener)
		if err != nil {
			log.Error(err)
		}
	}()

	// Run until the user terminates agorad.
	signal.Intercept()
	<-signal.ShutdownChannel()
	log.Info("Received shutdown signal, stopping server")
	grpcServer.GracefulStop()
	err = restProxy.Shutdown(context.Background())
	if err != nil {
		log.Errorf("error shutting down REST proxy: %v", err)
	}
	err = traderServer.Stop()
	if err != nil {
		log.Errorf("error shutting down server: %v", err)
	}

	wg.Wait()
	return nil
}
