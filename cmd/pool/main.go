package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon.v2"
)

var (
	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)

	// defaultMacaroonTimeout is the default macaroon timeout in seconds
	// that we set when sending it over the line.
	defaultMacaroonTimeout int64 = 60

	baseDirFlag = cli.StringFlag{
		Name:  "basedir",
		Value: pool.DefaultBaseDir,
		Usage: "path to pool's base directory",
	}
	networkFlag = cli.StringFlag{
		Name: "network, n",
		Usage: "the network pool is running on e.g. mainnet, " +
			"testnet, etc.",
		Value: pool.DefaultNetwork,
	}
	tlsCertFlag = cli.StringFlag{
		Name:  "tlscertpath",
		Usage: "path to pool's TLS certificate",
		Value: pool.DefaultTLSCertPath,
	}
	macaroonPathFlag = cli.StringFlag{
		Name:  "macaroonpath",
		Usage: "path to macaroon file",
		Value: pool.DefaultMacaroonPath,
	}
)

const (
	// defaultInitiator is the name of the pool CLI binary that is sent as
	// the initiator field in some of the calls. The initiator identifies
	// the software that initiated a certain RPC call and is appended to the
	// static user agent string of the daemon binary.
	defaultInitiator = "pool-cli"
)

type invalidUsageError struct {
	ctx     *cli.Context
	command string
}

func (e *invalidUsageError) Error() string {
	return fmt.Sprintf("invalid usage of command %s", e.command)
}

func printJSON(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	_ = json.Indent(&out, b, "", "\t")
	out.WriteString("\n")
	_, _ = out.WriteTo(os.Stdout)
}

func printRespJSON(resp proto.Message) { // nolint
	jsonBytes, err := lnrpc.ProtoJSONMarshalOpts.Marshal(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(string(jsonBytes))
}

func fatal(err error) {
	var e *invalidUsageError
	if errors.As(err, &e) {
		_ = cli.ShowCommandHelp(e.ctx, e.command)
	} else {
		_, _ = fmt.Fprintf(os.Stderr, "[pool] %v\n", err)
	}
	os.Exit(1)
}

func main() {
	app := cli.NewApp()

	app.Version = pool.Version()
	app.Name = "pool"
	app.Usage = "control plane for your poold"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:12010",
			Usage: "poold daemon address host:port",
		},
		networkFlag,
		baseDirFlag,
		tlsCertFlag,
		macaroonPathFlag,
	}
	app.Commands = append(app.Commands, accountsCommands...)
	app.Commands = append(app.Commands, ordersCommands...)
	app.Commands = append(app.Commands, sidecarCommands...)
	app.Commands = append(app.Commands, auctionCommands...)
	app.Commands = append(app.Commands, listAuthCommand)
	app.Commands = append(app.Commands, getInfoCommand)
	app.Commands = append(app.Commands, debugCommands...)
	app.Commands = append(app.Commands, stopDaemonCommand)

	err := app.Run(os.Args)
	if err != nil {
		fatal(err)
	}
}

var stopDaemonCommand = cli.Command{
	Name:  "stop",
	Usage: "gracefully shut down the daemon",
	Description: "Sends the stop command to the Pool trader daemon to " +
		"initiate a graceful shutdown",
	Action: stop,
}

func stop(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	_, err = client.StopDaemon(
		context.Background(), &poolrpc.StopDaemonRequest{},
	)
	return err
}

func getClient(ctx *cli.Context) (poolrpc.TraderClient, func(),
	error) {

	rpcServer := ctx.GlobalString("rpcserver")
	tlsCertPath, macaroonPath, err := extractPathArgs(ctx)
	if err != nil {
		return nil, nil, err
	}
	conn, err := getClientConn(rpcServer, tlsCertPath, macaroonPath)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = conn.Close() }

	traderClient := poolrpc.NewTraderClient(conn)
	return traderClient, cleanup, nil
}

func parseAmt(text string) (btcutil.Amount, error) {
	amtInt64, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amt value: %v", err)
	}
	return btcutil.Amount(amtInt64), nil
}

// extractPathArgs parses the TLS certificate and macaroon paths from the
// command.
func extractPathArgs(ctx *cli.Context) (string, string, error) {
	// We'll start off by parsing the network. This is needed to determine
	// the correct path to the TLS certificate and macaroon when not
	// specified.
	networkStr := strings.ToLower(ctx.GlobalString("network"))
	_, err := lndclient.Network(networkStr).ChainParams()
	if err != nil {
		return "", "", err
	}

	// We'll now fetch the basedir so we can make a decision on how to
	// properly read the cert and macaroon. This will either be the default,
	// or will have been overwritten by the end user.
	baseDir := lncfg.CleanAndExpandPath(ctx.GlobalString(baseDirFlag.Name))
	tlsCertPath := lncfg.CleanAndExpandPath(ctx.GlobalString(
		tlsCertFlag.Name,
	))
	macPath := lncfg.CleanAndExpandPath(ctx.GlobalString(
		macaroonPathFlag.Name,
	))

	// If a custom base directory was set, we'll also check if custom paths
	// for the TLS cert and macaroon file were set as well. If not, we'll
	// override their paths so they can be found within the custom base
	// directory set. This allows us to set a custom base directory, along
	// with custom paths to the TLS cert and macaroon file.
	if baseDir != pool.DefaultBaseDir || networkStr != pool.DefaultNetwork {
		tlsCertPath = filepath.Join(
			baseDir, networkStr, pool.DefaultTLSCertFilename,
		)
		macPath = filepath.Join(
			baseDir, networkStr, pool.DefaultMacaroonFilename,
		)
	}

	return tlsCertPath, macPath, nil
}

func getClientConn(address, tlsCertPath, macaroonPath string) (*grpc.ClientConn,
	error) {

	// We always need to send a macaroon.
	macOption, err := readMacaroon(macaroonPath)
	if err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
		macOption,
	}

	// TLS cannot be disabled, we'll always have a cert file to read.
	creds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		fatal(err)
	}

	opts = append(opts, grpc.WithTransportCredentials(creds))

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

func parseStr(ctx *cli.Context, argIdx int, flag, cmd string) (string, error) {
	var str string
	switch {
	case ctx.IsSet(flag):
		str = ctx.String(flag)
	case ctx.Args().Get(argIdx) != "":
		str = ctx.Args().Get(argIdx)
	default:
		return "", &invalidUsageError{ctx, cmd}
	}
	return str, nil
}

func parseHexStr(ctx *cli.Context, argIdx int, flag, cmd string) ([]byte, error) { // nolint:unparam
	hexStr, err := parseStr(ctx, argIdx, flag, cmd)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(hexStr)
}

func parseUint64(ctx *cli.Context, argIdx int, flag, cmd string) (uint64, error) {
	str, err := parseStr(ctx, argIdx, flag, cmd)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(str, 10, 64)
}

// readMacaroon tries to read the macaroon file at the specified path and create
// gRPC dial options from it.
func readMacaroon(macPath string) (grpc.DialOption, error) {
	// Load the specified macaroon file.
	macBytes, err := ioutil.ReadFile(macPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read macaroon path : %v", err)
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %v", err)
	}

	macConstraints := []macaroons.Constraint{
		// We add a time-based constraint to prevent replay of the
		// macaroon. It's good for 60 seconds by default to make up for
		// any discrepancy between client and server clocks, but leaking
		// the macaroon before it becomes invalid makes it possible for
		// an attacker to reuse the macaroon. In addition, the validity
		// time of the macaroon is extended by the time the server clock
		// is behind the client clock, or shortened by the time the
		// server clock is ahead of the client clock (or invalid
		// altogether if, in the latter case, this time is more than 60
		// seconds).
		macaroons.TimeoutConstraint(defaultMacaroonTimeout),
	}

	// Apply constraints to the macaroon.
	constrainedMac, err := macaroons.AddConstraints(mac, macConstraints...)
	if err != nil {
		return nil, err
	}

	// Now we append the macaroon credentials to the dial options.
	cred, err := macaroons.NewMacaroonCredential(constrainedMac)
	if err != nil {
		return nil, fmt.Errorf("error creating macaroon credential: %v",
			err)
	}
	return grpc.WithPerRPCCredentials(cred), nil
}
