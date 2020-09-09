package pool

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lncfg"
	"google.golang.org/grpc"
)

var (
	// DefaultBaseDir is the default root data directory where pool will
	// store all its data. On UNIX like systems this will resolve to
	// ~/.pool. Below this directory the logs and network directory will be
	// created.
	DefaultBaseDir = btcutil.AppDataDir("pool", false)

	// DefaultNetwork is the default bitcoin network pool runs on.
	DefaultNetwork = "mainnet"

	// DefaultLogFilename is the default name that is given to the pool log
	// file.
	DefaultLogFilename = "poold.log"

	defaultLogLevel   = "info"
	defaultLogDirname = "logs"
	defaultLogDir     = filepath.Join(DefaultBaseDir, defaultLogDirname)

	defaultMaxLogFiles    = 3
	defaultMaxLogFileSize = 10

	defaultMinBackoff = 5 * time.Second
	defaultMaxBackoff = 1 * time.Minute
)

type LndConfig struct {
	Host        string `long:"host" description:"lnd instance rpc address"`
	MacaroonDir string `long:"macaroondir" description:"Path to the directory containing all the required lnd macaroons"`
	TLSPath     string `long:"tlspath" description:"Path to lnd tls certificate"`
}

type Config struct {
	ShowVersion    bool   `long:"version" description:"Display version information and exit"`
	Insecure       bool   `long:"insecure" description:"disable tls"`
	Network        string `long:"network" description:"network to run on" choice:"regtest" choice:"testnet" choice:"mainnet" choice:"simnet"`
	AuctionServer  string `long:"auctionserver" description:"auction server address host:port"`
	TLSPathAuctSrv string `long:"tlspathauctserver" description:"Path to auction server tls certificate"`
	RPCListen      string `long:"rpclisten" description:"Address to listen on for gRPC clients"`
	RESTListen     string `long:"restlisten" description:"Address to listen on for REST clients"`
	BaseDir        string `long:"basedir" description:"The base directory where pool stores all its data. If set, this option overwrites --logdir."`

	LogDir         string `long:"logdir" description:"Directory to log output."`
	MaxLogFiles    int    `long:"maxlogfiles" description:"Maximum logfiles to keep (0 for no rotation)"`
	MaxLogFileSize int    `long:"maxlogfilesize" description:"Maximum logfile size in MB"`

	MinBackoff time.Duration `long:"minbackoff" description:"Shortest backoff when reconnecting to the server. Valid time units are {s, m, h}."`
	MaxBackoff time.Duration `long:"maxbackoff" description:"Longest backoff when reconnecting to the server. Valid time units are {s, m, h}."`
	DebugLevel string        `long:"debuglevel" description:"Logging level for all subsystems {trace, debug, info, warn, error, critical} -- You may also specify <subsystem>=<level>,<subsystem2>=<level>,... to set the log level for individual subsystems -- Use show to list available subsystems"`

	NewNodesOnly bool `long:"newnodesonly" description:"Only accept channels from nodes that the connected lnd node doesn't already have open or pending channels with."`

	Profile  string `long:"profile" description:"Enable HTTP profiling on given port -- NOTE port must be between 1024 and 65535"`
	FakeAuth bool   `long:"fakeauth" description:"Disable LSAT authentication and instead use a fake LSAT ID to identify. For testing only, cannot be set on mainnet."`

	Lnd *LndConfig `group:"lnd" namespace:"lnd"`

	// RPCListener is a network listener that can be set if poold should be
	// used as a library and listen on the given listener instead of what is
	// configured in the --rpclisten parameter. Setting this will also
	// disable REST.
	RPCListener net.Listener

	// AuctioneerDialOpts is a list of dial options that should be used when
	// dialing the auctioneer server.
	AuctioneerDialOpts []grpc.DialOption
}

const (
	MainnetServer = "pool.lightning.finance:12010"
	TestnetServer = "test.auction.lightning.today:12009"

	// defaultRPCTimeout is the default number of seconds an unary RPC call
	// is allowed to take to complete.
	defaultRPCTimeout  = 30 * time.Second
	defaultLsatMaxCost = btcutil.Amount(1000)
	defaultLsatMaxFee  = btcutil.Amount(10)
)

// DefaultConfig returns the default value for the Config struct.
func DefaultConfig() Config {
	return Config{
		Network:        DefaultNetwork,
		RPCListen:      "localhost:12010",
		RESTListen:     "localhost:8281",
		Insecure:       false,
		BaseDir:        DefaultBaseDir,
		LogDir:         defaultLogDir,
		MaxLogFiles:    defaultMaxLogFiles,
		MaxLogFileSize: defaultMaxLogFileSize,
		MinBackoff:     defaultMinBackoff,
		MaxBackoff:     defaultMaxBackoff,
		DebugLevel:     defaultLogLevel,
		Lnd: &LndConfig{
			Host: "localhost:10009",
		},
	}
}

// Validate cleans up paths in the config provided and validates it.
func Validate(cfg *Config) error {
	// Cleanup any paths before we use them.
	cfg.BaseDir = lncfg.CleanAndExpandPath(cfg.BaseDir)
	cfg.LogDir = lncfg.CleanAndExpandPath(cfg.LogDir)

	// Since our pool directory overrides our log dir value, make sure that
	// they are not set when base dir is set. We hard here rather than
	// overwriting and potentially confusing the user.
	baseDirSet := cfg.BaseDir != DefaultBaseDir

	if baseDirSet {
		logDirSet := cfg.LogDir != defaultLogDir

		if logDirSet {
			return fmt.Errorf("basedir overwrites logdir, please " +
				"only set one value")
		}

		// Once we are satisfied that no other config value was set, we
		// replace them with our pool dir.
		cfg.LogDir = filepath.Join(cfg.BaseDir, defaultLogDirname)
	}

	// Append the network type to the log and base directory so it is
	// "namespaced" per network in the same fashion as the data directory.
	cfg.LogDir = filepath.Join(cfg.LogDir, cfg.Network)
	cfg.BaseDir = filepath.Join(cfg.BaseDir, cfg.Network)

	// If either of these directories do not exist, create them.
	if err := os.MkdirAll(cfg.BaseDir, os.ModePerm); err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.LogDir, os.ModePerm); err != nil {
		return err
	}

	return nil
}
