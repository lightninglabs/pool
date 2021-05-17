package pool

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

// Run starts the trader daemon and blocks until it's shut down again.
func Run(cfg *Config) error {
	// Hook interceptor for os signals.
	shutdownInterceptor, err := signal.Intercept()
	if err != nil {
		return err
	}

	logWriter := build.NewRotatingLogWriter()
	SetupLoggers(logWriter, shutdownInterceptor)

	// Special show command to list supported subsystems and exit.
	if cfg.DebugLevel == "show" {
		fmt.Printf("Supported subsystems: %v\n",
			logWriter.SupportedSubsystems())
		os.Exit(0)
	}

	// Initialize logging at the default logging level.
	err = logWriter.InitLogRotator(
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

	trader := NewServer(cfg)
	err = trader.Start()
	if err != nil {
		return fmt.Errorf("unable to start server: %v", err)
	}
	<-shutdownInterceptor.ShutdownChannel()
	return trader.Stop()
}
