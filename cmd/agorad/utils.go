package main

import (
	"github.com/lightninglabs/loop/lndclient"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *lndConfig) (*lndclient.GrpcLndServices, error) {
	return lndclient.NewLndServices(
		cfg.Host, network, cfg.MacaroonDir, cfg.TLSPath,
	)
}
