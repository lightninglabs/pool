package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/agora/client"
	"github.com/lightningnetwork/lnd/signal"

	// Blank import to set up profiling HTTP handlers.
	_ "net/http/pprof"
)

var (
	defaultConfigFilename = "agorad.conf"
)

func main() {
	err := start()
	if err != nil {
		fmt.Println(err)
	}
}

func start() error {
	config := client.DefaultConfig

	// Parse command line flags.
	parser := flags.NewParser(&config, flags.Default)
	parser.SubcommandsOptional = true

	_, err := parser.Parse()
	if e, ok := err.(*flags.Error); ok && e.Type == flags.ErrHelp {
		return nil
	}
	if err != nil {
		return err
	}

	// Parse ini file.
	networkDir := filepath.Join(config.BaseDir, config.Network)
	if err := os.MkdirAll(networkDir, os.ModePerm); err != nil {
		return err
	}

	configFile := filepath.Join(networkDir, defaultConfigFilename)
	if err := flags.IniParse(configFile, &config); err != nil {
		// If it's a parsing related error, then we'll return
		// immediately, otherwise we can proceed as possibly the config
		// file doesn't exist which is OK.
		if _, ok := err.(*flags.IniError); ok {
			return err
		}
	}

	// Parse command line flags again to restore flags overwritten by ini
	// parse.
	_, err = parser.Parse()
	if err != nil {
		return err
	}

	// Enable http profiling and Validate profile port number if reqeusted.
	if config.Profile != "" {
		profilePort, err := strconv.Atoi(config.Profile)
		if err != nil || profilePort < 1024 || profilePort > 65535 {
			return fmt.Errorf("the profile port must be between " +
				"1024 and 65535")
		}

		go func() {
			listenAddr := net.JoinHostPort("", config.Profile)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Execute command.
	if parser.Active == nil {
		// Show the version and exit if the version flag was specified.
		appName := filepath.Base(os.Args[0])
		appName = strings.TrimSuffix(appName, filepath.Ext(appName))
		if config.ShowVersion {
			fmt.Println(appName, "version", client.Version())
			os.Exit(0)
		}

		// Special show command to list supported subsystems and exit.
		if config.DebugLevel == "show" {
			fmt.Printf("Supported subsystems: %v\n",
				client.SupportedSubsystems())
			os.Exit(0)
		}

		signal.Intercept()
		trader, err := client.NewServer(&config)
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

	return fmt.Errorf("unimplemented command %v", parser.Active.Name)
}
