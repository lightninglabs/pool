package llm

import (
	"fmt"
	"path/filepath"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

// Run starts the trader daemon and blocks until it's shut down again.
func Run(cfg *Config) error {
	// Append the network type to the log directory so it is
	// "namespaced" per network in the same fashion as the data directory.
	cfg.LogDir = filepath.Join(cfg.LogDir, cfg.Network)

	// Initialize logging at the default logging level.
	err := logWriter.InitLogRotator(
		filepath.Join(cfg.LogDir, DefaultLogFilename),
		cfg.MaxLogFileSize, cfg.MaxLogFiles,
	)
	if err != nil {
		return err
	}
	err = build.ParseAndSetDebugLevels(cfg.DebugLevel, logWriter)
	if err != nil {
		return err
	}

	signal.Intercept()
	trader, err := NewServer(cfg)
	if err != nil {
		return fmt.Errorf("unable to create server: %v", err)
	}
	err = trader.Start()
	if err != nil {
		return fmt.Errorf("unable to start server: %v", err)
	}
	<-signal.ShutdownChannel()
	return trader.Stop()
}
