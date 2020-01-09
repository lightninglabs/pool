package client

import (
	"net"
	"path/filepath"

	"github.com/btcsuite/btcutil"
	"google.golang.org/grpc"
)

var (
	// DefaultBaseDir is the default root data directory where agora will
	// store all its data. On UNIX like systems this will resolve to
	// ~/.agora. Below this directory the logs and network directory will be
	// created.
	DefaultBaseDir = btcutil.AppDataDir("agora", false)

	// DefaultLogFilename is the default name that is given to the agora log
	// file.
	DefaultLogFilename = "agorad.log"

	defaultLogLevel   = "info"
	defaultLogDirname = "logs"
	defaultLogDir     = filepath.Join(DefaultBaseDir, defaultLogDirname)

	defaultMaxLogFiles    = 3
	defaultMaxLogFileSize = 10
)

type LndConfig struct {
	Host        string `long:"host" description:"lnd instance rpc address"`
	MacaroonDir string `long:"macaroondir" description:"Path to the directory containing all the required lnd macaroons"`
	TLSPath     string `long:"tlspath" description:"Path to lnd tls certificate"`
}

type Config struct {
	ShowVersion    bool   `short:"V" long:"version" description:"Display version information and exit"`
	Insecure       bool   `long:"insecure" description:"disable tls"`
	Network        string `long:"network" description:"network to run on" choice:"regtest" choice:"testnet" choice:"mainnet" choice:"simnet"`
	AuctionServer  string `long:"auctionserver" description:"auction server address host:port"`
	TLSPathAuctSrv string `long:"tlspathauctserver" description:"Path to auction server tls certificate"`
	RPCListen      string `long:"rpclisten" description:"Address to listen on for gRPC clients"`
	RESTListen     string `long:"restlisten" description:"Address to listen on for REST clients"`
	BaseDir        string `long:"basedir" description:"The base directory where agora stores all its data"`

	LogDir         string `long:"logdir" description:"Directory to log output."`
	MaxLogFiles    int    `long:"maxlogfiles" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"maxlogfilesize" description:"Maximum logfile size in MB"`

	DebugLevel string `short:"d" long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	Lnd *LndConfig `group:"lnd" namespace:"lnd"`

	// RPCListener is a network listener that can be set if agorad should be
	// used as a library and listen on the given listener instead of what is
	// configured in the --rpclisten parameter. Setting this will also
	// disable REST.
	RPCListener net.Listener

	// AuctioneerDialOpts is a list of dial options that should be used when
	// dialing the auctioneer server.
	AuctioneerDialOpts []grpc.DialOption

	// ShutdownChannel is the channel that must be provided where agorad
	// listens for a shutdown signal.
	ShutdownChannel <-chan struct{}
}

const (
	MainnetServer = "auction.lightning.today:12009"
	TestnetServer = "test.auction.lightning.today:12009"
)

var DefaultConfig = Config{
	Network:        "mainnet",
	RPCListen:      "localhost:12010",
	RESTListen:     "localhost:8281",
	Insecure:       false,
	BaseDir:        DefaultBaseDir,
	LogDir:         defaultLogDir,
	MaxLogFiles:    defaultMaxLogFiles,
	MaxLogFileSize: defaultMaxLogFileSize,
	DebugLevel:     defaultLogLevel,
	Lnd: &LndConfig{
		Host: "localhost:10009",
	},
}
