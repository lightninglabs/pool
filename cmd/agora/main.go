package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightninglabs/protobuf-hex-display/proto"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

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
	jsonMarshaler := &jsonpb.Marshaler{
		EmitDefaults: true,
		Indent:       "\t", // Matches indentation of printJSON.
	}

	jsonStr, err := jsonMarshaler.MarshalToString(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(jsonStr)
}

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "[agora] %v\n", err)
	os.Exit(1)
}

func main() {
	app := cli.NewApp()

	app.Version = client.Version()
	app.Name = "agora"
	app.Usage = "control plane for your agorad"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:12010",
			Usage: "agorad daemon address host:port",
		},
	}
	app.Commands = append(app.Commands, accountsCommands...)
	app.Commands = append(app.Commands, ordersCommands...)

	err := app.Run(os.Args)
	if err != nil {
		fatal(err)
	}
}

func getClient(ctx *cli.Context) (clmrpc.TraderClient, func(),
	error) {

	rpcServer := ctx.GlobalString("rpcserver")
	conn, err := getClientConn(rpcServer)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	traderClient := clmrpc.NewTraderClient(conn)
	return traderClient, cleanup, nil
}

func parseAmt(text string) (btcutil.Amount, error) {
	amtInt64, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amt value: %v", err)
	}
	return btcutil.Amount(amtInt64), nil
}

func parseExpiry(text string) (uint32, error) {
	expiry, err := strconv.ParseInt(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid expiry value: %v", err)
	}
	return uint32(expiry), nil
}

func getClientConn(address string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}
