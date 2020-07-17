package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/llm/clmrpc"
	"github.com/lightninglabs/llm/order"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/urfave/cli"
)

const (
	defaultFundingFeeRate = chainfee.FeePerKwFloor
	defaultAskMaxDuration = 210000
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

// promptForConfirmation continuously prompts the user for the message until
// receiving a response of "yes" or "no" and returns their answer as a bool.
func promptForConfirmation(msg string) bool {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print(msg)

		answer, err := reader.ReadString('\n')
		if err != nil {
			return false
		}

		answer = strings.ToLower(strings.TrimSpace(answer))

		switch {
		case answer == "yes":
			return true
		case answer == "no":
			return false
		default:
			continue
		}
	}
}

// parseCommonParams tries to read the common order parameters from the command
// line positional arguments and/or flags and parses them based on their
// destination data type. No formal in-depth validation is performed as the
// server will do that on the RPC level anyway.
func parseCommonParams(ctx *cli.Context, blockDuration uint32) (*clmrpc.Order, error) {
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

	// We'll map the interest rate specified on the command line to our
	// internal "rate_fixed" unit.
	//
	// rate = % / 100
	// rate = rateFixed / totalParts
	// rateFixed = rate * totalParts
	interestPercent := ctx.Float64("interest_rate_percent")
	interestRate := interestPercent / 100
	rateFixedFloat := interestRate * order.FeeRateTotalParts

	// We then take this rate fixed, and divide it by the number of blocks
	// as the user wants this rate to be the final lump sum they pay.
	rateFixed := uint32(rateFixedFloat / float64(blockDuration))

	// At this point, if this value is less than 1, then we aren't able to
	// express it given the current precision allowed by our fixed point.
	if rateFixed < 1 {
		return nil, fmt.Errorf("fixed rate of %v is too small "+
			"(%v%% over %v blocks), min is 1 (%v%%)", rateFixed,
			interestPercent, blockDuration,
			float64(1)/order.FeeRateTotalParts)
	}

	params.RateFixed = rateFixed

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
		cli.Float64Flag{
			Name: "interest_rate_percent",
			Usage: "the total percent one is willing to pay or " +
				"accept as yield for the specified interval",
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

	ask := &clmrpc.Ask{
		MaxDurationBlocks: uint32(ctx.Uint64("max_duration_blocks")),
		Version:           uint32(order.VersionDefault),
	}

	params, err := parseCommonParams(ctx, ask.MaxDurationBlocks)
	if err != nil {
		return fmt.Errorf("unable to parse order params: %v", err)
	}

	ask.Details = params

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// If the user didn't opt to force submit this order, then we'll show a
	// break down of the final order details and request a confirmation
	// before we submit.
	if !ctx.Bool("force") {
		if err := printOrderDetails(
			client, btcutil.Amount(ask.Details.Amt),
			order.FixedRatePremium(ask.Details.RateFixed),
			ask.MaxDurationBlocks, true,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v", err)
		}

		if !promptForConfirmation("Confirm order (yes/no): ") {
			fmt.Println("Cancelling order...")
			return nil
		}
	}

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

func printOrderDetails(client clmrpc.TraderClient, amt btcutil.Amount,
	rate order.FixedRatePremium, leaseDuration uint32, isAsk bool) error {

	auctionFee, err := client.AuctionFee(
		context.Background(), &clmrpc.AuctionFeeRequest{},
	)
	if err != nil {
		return err
	}

	feeSchedule := order.NewLinearFeeSchedule(
		btcutil.Amount(auctionFee.ExecutionFee.BaseFee),
		btcutil.Amount(auctionFee.ExecutionFee.FeeRate),
	)
	exeFee := feeSchedule.BaseFee() + feeSchedule.ExecutionFee(amt)

	orderType := "Bid"
	premiumDescription := "paid to maker"
	if isAsk {
		orderType = "Ask"
		premiumDescription = "yield from taker"
	}
	ratePerMil := float64(rate) / order.FeeRateTotalParts

	premium := rate.LumpSumPremium(amt, leaseDuration)

	fmt.Println("-- Order Details --")
	fmt.Printf("%v Amount: %v\n", orderType, amt)
	fmt.Printf("%v Duration: %v\n", orderType, leaseDuration)
	fmt.Printf("Total Premium (%v): %v \n", premiumDescription, premium)
	fmt.Printf("Rate Fixed: %v\n", rate)
	fmt.Printf("Rate Per Block: %.9f (%.7f%%)\n", ratePerMil, ratePerMil*100)
	fmt.Println("Execution Fee: ", exeFee)

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
		cli.Float64Flag{
			Name: "interest_rate_percent",
			Usage: "the total percent one is willing to pay or " +
				"accept as yield for the specified interval",
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
		cli.BoolFlag{
			Name:  "force",
			Usage: "skip order placement confirmation",
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

	bid := &clmrpc.Bid{
		MinDurationBlocks: uint32(ctx.Uint64("min_duration_blocks")),
		Version:           uint32(order.VersionDefault),
	}

	params, err := parseCommonParams(ctx, bid.MinDurationBlocks)
	if err != nil {
		return fmt.Errorf("unable to parse order params: %v", err)
	}

	bid.Details = params

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// If the user didn't opt to force submit this order, then we'll show a
	// break down of the final order details and request a confirmation
	// before we submit.
	if !ctx.Bool("force") {
		if err := printOrderDetails(
			client, btcutil.Amount(bid.Details.Amt),
			order.FixedRatePremium(bid.Details.RateFixed),
			bid.MinDurationBlocks, false,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v", err)
		}

		if !promptForConfirmation("Confirm order (yes/no): ") {
			fmt.Println("Cancelling order...")
			return nil
		}
	}

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
