// As this file is very similar in every package, ignore the linter here.
// nolint:dupl
package main

import (
	"github.com/btcsuite/btclog"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/auctioneer"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightninglabs/agora/client/trader"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
)

const Subsystem = "AGOD"

var (
	logWriter = build.NewRotatingLogWriter()
	log       = build.NewSubLogger(Subsystem, logWriter.GenSubLogger)
)

func init() {
	setSubLogger(Subsystem, log, nil)
	addSubLogger(trader.Subsystem, trader.UseLogger)
	addSubLogger(auctioneer.Subsystem, auctioneer.UseLogger)
	addSubLogger(order.Subsystem, order.UseLogger)
	addSubLogger("LNDC", lndclient.UseLogger)
	addSubLogger("SGNL", signal.UseLogger)
	addSubLogger(account.Subsystem, account.UseLogger)
}

// addSubLogger is a helper method to conveniently create and register the
// logger of a sub system.
func addSubLogger(subsystem string, useLogger func(btclog.Logger)) {
	logger := build.NewSubLogger(subsystem, logWriter.GenSubLogger)
	setSubLogger(subsystem, logger, useLogger)
}

// setSubLogger is a helper method to conveniently register the logger of a sub
// system.
func setSubLogger(subsystem string, logger btclog.Logger,
	useLogger func(btclog.Logger)) {

	logWriter.RegisterSubLogger(subsystem, logger)
	if useLogger != nil {
		useLogger(logger)
	}
}
