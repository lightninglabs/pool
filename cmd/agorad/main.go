package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/agora/client"
	"github.com/lightningnetwork/lnd/signal"
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

	// Execute command.
	if parser.Active == nil {
		signal.Intercept()
		config.ShutdownChannel = signal.ShutdownChannel()
		return client.Start(&config)
	}

	return fmt.Errorf("unimplemented command %v", parser.Active.Name)
}
