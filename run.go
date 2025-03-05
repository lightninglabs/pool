package pool

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

// Run starts the trader daemon and blocks until it's shut down again.
func Run(cfg *Config) error {
	// Hook interceptor for os signals.
	var err error
	cfg.ShutdownInterceptor, err = signal.Intercept()
	if err != nil {
		return err
	}
	cfg.RequestShutdown = cfg.ShutdownInterceptor.RequestShutdown

	sugLogMgr := build.NewSubLoggerManager(
		build.NewDefaultLogHandlers(cfg.Logging, logWriter)...,
	)
	SetupLoggers(sugLogMgr, cfg.ShutdownInterceptor)

	// Special show command to list supported subsystems and exit.
	if cfg.DebugLevel == "show" {
		fmt.Printf("Supported subsystems: %v\n",
			sugLogMgr.SupportedSubsystems())
		os.Exit(0)
	}

	if cfg.MaxLogFiles != 0 {
		if cfg.Logging.File.MaxLogFiles !=
			build.DefaultMaxLogFiles {

			return fmt.Errorf("cannot set both maxlogfiles and "+
				"logging.file.max-files: %w", err)
		}

		cfg.Logging.File.MaxLogFiles = cfg.MaxLogFiles
	}
	if cfg.MaxLogFileSize != 0 {
		if cfg.Logging.File.MaxLogFileSize !=
			build.DefaultMaxLogFileSize {

			return fmt.Errorf("cannot set both maxlogfilesize and "+
				"logging.file.max-file-size: %w", err)
		}

		cfg.Logging.File.MaxLogFileSize = cfg.MaxLogFileSize
	}

	err = logWriter.InitLogRotator(
		cfg.Logging.File, filepath.Join(cfg.LogDir, DefaultLogFilename),
	)
	if err != nil {
		return err
	}

	err = build.ParseAndSetDebugLevels(cfg.DebugLevel, sugLogMgr)
	if err != nil {
		return err
	}

	if cfg.Profile != "" {
		go func() {
			log.Infof("Pprof listening on %v", cfg.Profile)
			profileRedirect := http.RedirectHandler(
				"/debug/pprof", http.StatusSeeOther,
			)
			http.Handle("/", profileRedirect)
			//nolint:gosec
			err := http.ListenAndServe(cfg.Profile, nil)
			if err != nil {
				log.Errorf("Unable to run profiler: %v", err)
			}
		}()
	}

	trader := NewServer(cfg)
	err = trader.Start()
	if err != nil {
		log.Errorf("Error starting server: %v", err)
		return fmt.Errorf("unable to start server: %v", err)
	}
	<-cfg.ShutdownInterceptor.ShutdownChannel()

	err = trader.Stop()
	if err != nil {
		log.Errorf("Error stopping server: %v", err)
		return err
	}

	return nil
}
