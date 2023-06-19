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

	logWriter = build.NewRotatingLogWriter()
	SetupLoggers(logWriter, cfg.ShutdownInterceptor)

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
