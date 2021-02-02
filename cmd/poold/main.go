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
	"github.com/lightninglabs/pool"

	// Blank import to set up profiling HTTP handlers.
	_ "net/http/pprof"
)

var (
	defaultConfigFilename = "poold.conf"
)

func main() {
	err := start()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func start() error {
	config := pool.DefaultConfig()

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
	poolDir := filepath.Join(config.BaseDir, config.Network)
	configFile := filepath.Join(poolDir, defaultConfigFilename)

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

	// Make sure the passed configuration is valid.
	if err := pool.Validate(&config); err != nil {
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
			fmt.Println(appName, "version", pool.Version())
			os.Exit(0)
		}

		// Special show command to list supported subsystems and exit.
		if config.DebugLevel == "show" {
			fmt.Printf("Supported subsystems: %v\n",
				pool.SupportedSubsystems())
			os.Exit(0)
		}

		return pool.Run(&config)
	}

	return fmt.Errorf("unimplemented command %v", parser.Active.Name)
}
