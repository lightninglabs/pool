package main

import (
	"path/filepath"

	"github.com/btcsuite/btcutil"
)

var (
	agoraDirBase = btcutil.AppDataDir("agora", false)

	defaultLogLevel    = "info"
	defaultLogDirname  = "logs"
	defaultLogFilename = "agorad.log"
	defaultLogDir      = filepath.Join(agoraDirBase, defaultLogDirname)

	defaultMaxLogFiles    = 3
	defaultMaxLogFileSize = 10
)

type lndConfig struct {
	Host        string `long:"host" description:"lnd instance rpc address"`
	MacaroonDir string `long:"macaroondir" description:"Path to the directory containing all the required lnd macaroons"`
	TLSPath     string `long:"tlspath" description:"Path to lnd tls certificate"`
}

type viewParameters struct{}

type config struct {
	ShowVersion    bool   `short:"V" long:"version" description:"Display version information and exit"`
	Insecure       bool   `long:"insecure" description:"disable tls"`
	Network        string `long:"network" description:"network to run on" choice:"regtest" choice:"testnet" choice:"mainnet" choice:"simnet"`
	AuctionServer  string `long:"auctionserver" description:"auction server address host:port"`
	TLSPathAuctSrv string `long:"tlspathauctserver" description:"Path to auction server tls certificate"`
	RPCListen      string `long:"rpclisten" description:"Address to listen on for gRPC clients"`
	RESTListen     string `long:"restlisten" description:"Address to listen on for REST clients"`

	LogDir         string `long:"logdir" description:"Directory to log output."`
	MaxLogFiles    int    `long:"maxlogfiles" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"maxlogfilesize" description:"Maximum logfile size in MB"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	Lnd *lndConfig `group:"lnd" namespace:"lnd"`

	View viewParameters `command:"view" alias:"v" description:"View all orders in the database. This command can only be executed when agorad is not running."`
}

const (
	mainnetServer = "auction.lightning.today:12009"
	testnetServer = "test.auction.lightning.today:12009"
)

var defaultConfig = config{
	Network:        "mainnet",
	RPCListen:      "localhost:12010",
	RESTListen:     "localhost:8281",
	Insecure:       false,
	LogDir:         defaultLogDir,
	MaxLogFiles:    defaultMaxLogFiles,
	MaxLogFileSize: defaultMaxLogFileSize,
	DebugLevel:     defaultLogLevel,
	Lnd: &lndConfig{
		Host: "localhost:10009",
	},
}
