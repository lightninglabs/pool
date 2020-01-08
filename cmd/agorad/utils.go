package main

import (
	"os"
	"path/filepath"

	"github.com/lightninglabs/loop/lndclient"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *lndConfig) (*lndclient.GrpcLndServices, error) {
	return lndclient.NewLndServices(
		cfg.Host, network, cfg.MacaroonDir, cfg.TLSPath,
	)
}

func getStoreDir(network string) (string, error) {
	dir := filepath.Join(agoraDirBase, network)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return "", err
	}

	return dir, nil
}
