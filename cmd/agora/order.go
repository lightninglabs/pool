package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/urfave/cli"
)

const (
	defaultFundingFeeRate = chainfee.FeePerKwFloor
	defaultAskRate        = 5
	defaultAskMaxDuration = 210000
	defaultBidRate        = 5
	defaultBidMinDuration = 144
)

var ordersCommands = []cli.Command{
	{
		Name:     "orders",
		Aliases:  []string{"o"},
		Usage:    "Submit/interact with orders.",
		Category: "Orders",
		Subcommands: []cli.Command{
			ordersListCommand,
			ordersCancelCommand,
			{
				Name:    "submit",
				Aliases: []string{"s"},
				Usage:   "submit an order",
				Subcommands: []cli.Command{
					ordersSubmitAskCommand,
					ordersSubmitBidCommand,
				},
			},
		},
	},
}

var sharedFlags = []cli.Flag{
	cli.Uint64Flag{
		Name: "funding_fee_rate",
		Usage: "the fee rate (sat/vByte) to use to publish " +
			"the funding transaction",
		Value: uint64(defaultFundingFeeRate),
	},
}

// parseCommonParams tries to read the common order parameters from the command
// line positional arguments and/or flags and parses them based on their
// destination data type. No formal in-depth validation is performed as the
// server will do that on the RPC level anyway.
func parseCommonParams(ctx *cli.Context) (*clmrpc.Order, error) {
	var (
		err    error
		amt    btcutil.Amount
		args   = ctx.Args()
		params = &clmrpc.Order{}
	)

	switch {
	case ctx.IsSet("amt"):
		params.Amt = ctx.Uint64("amt")
	case args.Present():
		amt, err = parseAmt(args.First())
		params.Amt = uint64(amt)
		args = args.Tail()
	}
	if err != nil {
		return nil, fmt.Errorf("unable to decode amount: %v", err)
	}

	params.TraderKey, err = parseAccountKey(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("unable to parse acct_key: %v", err)
	}

	params.FundingFeeRate = ctx.Uint64("funding_fee_rate")
	params.RateFixed = uint32(ctx.Uint64("rate_fixed"))

	return params, nil
}

// parseAccountKey tries to read the account key parameter from the command
// line positional arguments and/or flags.
func parseAccountKey(ctx *cli.Context, args cli.Args) ([]byte, error) {
	var (
		acctKeyStr string
	)
	switch {
	case ctx.IsSet("acct_key"):
		acctKeyStr = ctx.String("acct_key")
	case args.Present():
		acctKeyStr = args.First()
		args = args.Tail()
	default:
		return nil, fmt.Errorf("acct_key argument missing")
	}
	if len(acctKeyStr) != hex.EncodedLen(33) {
		return nil, fmt.Errorf("acct_key in invalid format. " +
			"must be hex encoded 33 byte public key")
	}
	return hex.DecodeString(acctKeyStr)
}

var ordersSubmitAskCommand = cli.Command{
	Name:  "ask",
	Usage: "offer channel liquidity",
	ArgsUsage: "amt acct_key [--rate_fixed=R] [--funding_fee_rate=F] " +
		"[--max_duration_blocks=M]",
	Description: `
	Create an offer to provide inbound liquidity to an auction participant
	by opening a channel to them for a certain time.`,
	Flags: append([]cli.Flag{
		cli.Uint64Flag{
			Name: "rate_fixed",
			Usage: "the rate at which the funds will be lent out " +
				"in parts per million",
			Value: defaultAskRate,
		},
		cli.Uint64Flag{
			Name: "amt",
			Usage: "the amount to offer for channel creation in" +
				"satoshis",
		},
		cli.StringFlag{
			Name: "acct_key",
			Usage: "the account key to use to offer " +
				"liquidity from",
		},
		cli.Uint64Flag{
			Name: "max_duration_blocks",
			Usage: "the maximum number of blocks that the " +
				"liquidity should be offered for",
			Value: defaultAskMaxDuration,
		},
	}, sharedFlags...),
	Action: ordersSubmitAsk,
}

func ordersSubmitAsk(ctx *cli.Context) error { // nolint: dupl
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "ask")
		return nil
	}

	params, err := parseCommonParams(ctx)
	if err != nil {
		return fmt.Errorf("unable to parse order params: %v", err)
	}
	ask := &clmrpc.Ask{
		Details:           params,
		MaxDurationBlocks: uint32(ctx.Uint64("max_duration_blocks")),
		Version:           uint32(order.VersionDefault),
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.SubmitOrder(
		context.Background(), &clmrpc.SubmitOrderRequest{
			Details: &clmrpc.SubmitOrderRequest_Ask{
				Ask: ask,
			},
		},
	)
	if err != nil {
		return err
	}
	printRespJSON(resp)

	return nil
}

var ordersSubmitBidCommand = cli.Command{
	Name:  "bid",
	Usage: "obtain channel liquidity",
	ArgsUsage: "amt acct_key [--rate_fixed=R] [--funding_fee_rate=F] " +
		"[--min_duration_blocks=M]",
	Description: `
	Place an offer for acquiring inbound liquidity by lending
	funding capacity from another participant in the order book.`,
	Flags: append([]cli.Flag{
		cli.Uint64Flag{
			Name: "rate_fixed",
			Usage: "the rate in parts per million that is " +
				"acceptable to be paid to the offering " +
				"participant for lending the funds out",
			Value: defaultBidRate,
		},
		cli.Uint64Flag{
			Name: "amt",
			Usage: "the amount of inbound liquidity in satoshis " +
				"to request",
		},
		cli.StringFlag{
			Name: "acct_key",
			Usage: "the account key to use to pay the order " +
				"fees with",
		},
		cli.Uint64Flag{
			Name: "min_duration_blocks",
			Usage: "the minimum number of blocks that the " +
				"liquidity should be provided for",
			Value: defaultBidMinDuration,
		},
	}, sharedFlags...),
	Action: ordersSubmitBid,
}

func ordersSubmitBid(ctx *cli.Context) error { // nolint: dupl
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "bid")
		return nil
	}

	params, err := parseCommonParams(ctx)
	if err != nil {
		return fmt.Errorf("unable to parse order params: %v", err)
	}
	bid := &clmrpc.Bid{
		Details:           params,
		MinDurationBlocks: uint32(ctx.Uint64("min_duration_blocks")),
		Version:           uint32(order.VersionDefault),
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.SubmitOrder(
		context.Background(), &clmrpc.SubmitOrderRequest{
			Details: &clmrpc.SubmitOrderRequest_Bid{
				Bid: bid,
			},
		},
	)
	if err != nil {
		return err
	}
	printRespJSON(resp)

	return nil
}

var ordersListCommand = cli.Command{
	Name:    "list",
	Aliases: []string{"l"},
	Usage:   "list all existing orders",
	Description: `
	List all orders that are stored in the local order database`,
	Flags:  []cli.Flag{},
	Action: ordersList,
}

func ordersList(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListOrders(
		context.Background(), &clmrpc.ListOrdersRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var ordersCancelCommand = cli.Command{
	Name:      "cancel",
	Aliases:   []string{"c"},
	Usage:     "remove an order from the order book by canceling it",
	ArgsUsage: "order_nonce",
	Description: `
	Remove a pending offer from the order book.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "order_nonce",
			Usage: "the order nonce of the order to cancel",
		},
	},
	Action: ordersCancel,
}

func ordersCancel(ctx *cli.Context) error { // nolint: dupl
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "cancel")
		return nil
	}

	var (
		nonceHex string
		args     = ctx.Args()
	)
	switch {
	case ctx.IsSet("order_nonce"):
		nonceHex = ctx.String("order_nonce")
	case args.Present():
		nonceHex = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("order_nonce argument missing")
	}
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil {
		return fmt.Errorf("cannot hex decode order nonce: %v", err)
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.CancelOrder(
		context.Background(), &clmrpc.CancelOrderRequest{
			OrderNonce: nonce,
		},
	)
	if err != nil {
		return err
	}
	printRespJSON(resp)
	return nil
}
