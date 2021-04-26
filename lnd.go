// +build !js

package pool

import (
	"context"
	"path"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// getLnd returns an instance of the lnd services proxy.
func getLnd(network string, cfg *LndConfig) (*lndclient.GrpcLndServices,
	lnrpc.LightningClient, error) {

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

	services, err := lndclient.NewLndServices(&lndclient.LndServicesConfig{
		LndAddress:            cfg.Host,
		Network:               lndclient.Network(network),
		CustomMacaroonPath:    cfg.MacaroonPath,
		TLSPath:               cfg.TLSPath,
		CheckVersion:          minimalCompatibleVersion,
		BlockUntilChainSynced: true,
		BlockUntilUnlocked:    true,
		CallerCtx:             ctxc,
	})
	if err != nil {
		return nil, nil, err
	}

	basicClient, err := lndclient.NewBasicClient(
		cfg.Host, cfg.TLSPath, path.Dir(cfg.MacaroonPath), network,
		lndclient.MacFilename(path.Base(cfg.MacaroonPath)),
	)
	if err != nil {
		return nil, nil, err
	}

	return services, basicClient, nil
}
