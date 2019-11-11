package main

import (
	"os"
	"path/filepath"

	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/loop/lndclient"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *lndConfig) (*lndclient.GrpcLndServices, error) {
	return lndclient.NewLndServices(
		cfg.Host, "client", network, cfg.MacaroonDir, cfg.TLSPath,
	)
}

// getAuctioneerClient returns an instance of the auction client.
func getAuctioneerClient(network, auctionServer string, insecure bool,
	tlsPathServer string, lnd *lndclient.LndServices) (*auctioneer.Client,
	func(), error) {

	storeDir, err := getStoreDir(network)
	if err != nil {
		return nil, nil, err
	}

	auctioneerClient, cleanUp, err := auctioneer.NewClient(
		storeDir, auctionServer, insecure, tlsPathServer, lnd,
	)
	if err != nil {
		return nil, nil, err
	}

	return auctioneerClient, cleanUp, nil
}

func getStoreDir(network string) (string, error) {
	dir := filepath.Join(agoraDirBase, network)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return "", err
	}

	return dir, nil
}
