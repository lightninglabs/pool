package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/urfave/cli"
)

const (
	defaultAskMaxDuration = 2016
	defaultBidMinDuration = 2016
)

// Default max batch fee rate to 100 sat/vByte.
const defaultMaxBatchFeeRateSatPerVByte = 100

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
		Name: "max_batch_fee_rate",
		Usage: "the maximum fee rate (sat/vByte) to use to for " +
			"the batch transaction",
		Value: defaultMaxBatchFeeRateSatPerVByte,
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
func parseCommonParams(ctx *cli.Context, blockDuration uint32) (*poolrpc.Order, error) {
	var (
		args   = ctx.Args()
		params = &poolrpc.Order{}
	)

	switch {
	case ctx.IsSet("amt"):
		params.Amt = ctx.Uint64("amt")
	case args.Present():
		amt, err := parseAmt(args.First())
		if err != nil {
			return nil, fmt.Errorf("unable to decode amount: %v", err)
		}
		params.Amt = uint64(amt)
		args = args.Tail()
	}

	// If the minimum channel amount flag wasn't provided, use a default of
	// 10% and round to the nearest unit.
	minChanAmt := btcutil.Amount(ctx.Uint64("min_chan_amt"))
	if minChanAmt == 0 {
		minChanAmt = order.RoundToNextSupplyUnit(
			btcutil.Amount(params.Amt) / 10,
		).ToSatoshis()
	}

	// Verify the minimum channel amount flag has been properly set.
	switch {
	case minChanAmt%order.BaseSupplyUnit != 0:
		return nil, fmt.Errorf("minimum channel amount %v must be "+
			"a multiple of %v", minChanAmt, order.BaseSupplyUnit)

	case minChanAmt < order.BaseSupplyUnit:
		return nil, fmt.Errorf("minimum channel amount %v is below "+
			"required value of %v", minChanAmt, order.BaseSupplyUnit)

	case minChanAmt > btcutil.Amount(params.Amt):
		return nil, fmt.Errorf("minimum channel amount %v is above "+
			"order amount %v", minChanAmt, btcutil.Amount(params.Amt))
	}
	params.MinUnitsMatch = uint32(minChanAmt / order.BaseSupplyUnit)

	var err error
	params.TraderKey, err = parseAccountKey(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("unable to parse acct_key: %v", err)
	}

	// Convert the cmd line flag from sat/vByte to sat/kw which is used
	// internally.
	satPerByte := ctx.Uint64("max_batch_fee_rate")
	if satPerByte == 0 {
		return nil, fmt.Errorf("max batch fee rate must be at " +
			"least 1 sat/vByte")
	}

	satPerKw := chainfee.SatPerKVByte(satPerByte * 1000).FeePerKWeight()

	// Because of rounding, we ensure the set rate is at least our fee
	// floor.
	if satPerKw < chainfee.FeePerKwFloor {
		satPerKw = chainfee.FeePerKwFloor
	}

	params.MaxBatchFeeRateSatPerKw = uint64(satPerKw)

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
	ArgsUsage: "amt acct_key [--rate_fixed=R] [--max_batch_fee_rate=F] " +
		"[--lease_duration_blocks=M]",
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
			Usage: "the amount to offer for channel creation in " +
				"satoshis",
		},
		cli.StringFlag{
			Name: "acct_key",
			Usage: "the account key to use to offer " +
				"liquidity from",
		},
		cli.Uint64Flag{
			Name: "lease_duration_blocks",
			Usage: "the number of blocks that the " +
				"liquidity should be offered for",
			Value: defaultAskMaxDuration,
		},
		cli.Uint64Flag{
			Name: "min_chan_amt",
			Usage: "the minimum amount of satoshis that a " +
				"resulting channel from this order must have",
		},
		cli.BoolFlag{
			Name:  "force",
			Usage: "skip order placement confirmation",
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

	ask := &poolrpc.Ask{
		LeaseDurationBlocks: uint32(ctx.Uint64("lease_duration_blocks")),
		Version:             uint32(order.VersionSelfChanBalance),
	}

	params, err := parseCommonParams(ctx, ask.LeaseDurationBlocks)
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
			order.SupplyUnit(ask.Details.MinUnitsMatch).ToSatoshis(),
			0, order.FixedRatePremium(ask.Details.RateFixed),
			ask.LeaseDurationBlocks,
			chainfee.SatPerKWeight(
				ask.Details.MaxBatchFeeRateSatPerKw,
			), true,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v", err)
		}

		if !promptForConfirmation("Confirm order (yes/no): ") {
			fmt.Println("Cancelling order...")
			return nil
		}
	}

	resp, err := client.SubmitOrder(
		context.Background(), &poolrpc.SubmitOrderRequest{
			Details: &poolrpc.SubmitOrderRequest_Ask{
				Ask: ask,
			},
			Initiator: defaultInitiator,
		},
	)
	if err != nil {
		return err
	}
	printRespJSON(resp)

	return nil
}

func printOrderDetails(client poolrpc.TraderClient, amt,
	minChanAmt, selfChanBalance btcutil.Amount, rate order.FixedRatePremium,
	leaseDuration uint32, maxBatchFeeRate chainfee.SatPerKWeight,
	isAsk bool) error {

	auctionFee, err := client.AuctionFee(
		context.Background(), &poolrpc.AuctionFeeRequest{},
	)
	if err != nil {
		return err
	}

	feeSchedule := terms.NewLinearFeeSchedule(
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

	maxNumMatches := amt / minChanAmt
	chainFee := maxNumMatches * order.EstimateTraderFee(1, maxBatchFeeRate)

	fmt.Println("-- Order Details --")
	fmt.Printf("%v Amount: %v\n", orderType, amt)
	fmt.Printf("%v Duration: %v\n", orderType, leaseDuration)
	fmt.Printf("Total Premium (%v): %v \n", premiumDescription, premium)
	fmt.Printf("Rate Fixed: %v\n", rate)
	fmt.Printf("Rate Per Block: %.9f (%.7f%%)\n", ratePerMil, ratePerMil*100)
	fmt.Println("Execution Fee: ", exeFee)
	fmt.Printf("Max batch fee rate: %d sat/vByte\n",
		maxBatchFeeRate.FeePerKVByte()/1000)
	fmt.Println("Max chain fee:", chainFee)

	if selfChanBalance > 0 {
		fmt.Printf("Self channel balance: %v\n", selfChanBalance)
	}

	return nil
}

var ordersSubmitBidCommand = cli.Command{
	Name:  "bid",
	Usage: "obtain channel liquidity",
	ArgsUsage: "amt acct_key [--rate_fixed=R] [--max_batch_fee_rate=F] " +
		"[--lease_duration_blocks=M]",
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
			Name: "lease_duration_blocks",
			Usage: "the number of blocks that the " +
				"liquidity should be provided for",
			Value: defaultBidMinDuration,
		},
		cli.Uint64Flag{
			Name: "min_node_tier",
			Usage: "the min node tier this bid should be matched " +
				"with, tier 1 nodes are considered 'good', if " +
				"set to tier 0, then all nodes will be " +
				"considered regardless of 'quality'",
			Value: uint64(order.NodeTierDefault),
		},
		cli.Uint64Flag{
			Name: "min_chan_amt",
			Usage: "the minimum amount of satoshis that a " +
				"resulting channel from this order must have",
		},
		cli.BoolFlag{
			Name:  "force",
			Usage: "skip order placement confirmation",
		},
		cli.Uint64Flag{
			Name: "self_chan_balance",
			Usage: "give the channel leased by this bid order an " +
				"initial balance by adding additional funds " +
				"from our account into the channel; can be " +
				"used to create up to 50/50 balanced channels",
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

	// The node tier values are a bit un-intuitive. We need to convert
	// between the human interpretation of "tier 1" (value 1) to the
	// internal representation of "tier 1" (value order.NodeTier1=2).
	cliNodeTier := order.NodeTierDefault
	if ctx.IsSet("min_node_tier") {
		if ctx.Uint64("min_node_tier") == 0 {
			cliNodeTier = order.NodeTier0
		}
		if ctx.Uint64("min_node_tier") == 1 {
			cliNodeTier = order.NodeTier1
		}
	}

	nodeTier, err := auctioneer.MarshallNodeTier(cliNodeTier)
	if err != nil {
		return nil
	}

	bid := &poolrpc.Bid{
		LeaseDurationBlocks: uint32(ctx.Uint64("lease_duration_blocks")),
		Version:             uint32(order.VersionSelfChanBalance),
		MinNodeTier:         nodeTier,
	}

	params, err := parseCommonParams(ctx, bid.LeaseDurationBlocks)
	if err != nil {
		return fmt.Errorf("unable to parse order params: %v", err)
	}

	bid.Details = params

	// Make sure the self channel balance is within reasonable limits
	// (currently max the same of the total order amount) and that the min
	// units matched is also set to 100% of the order amount.
	if ctx.IsSet("self_chan_balance") {
		bid.SelfChanBalance = ctx.Uint64("self_chan_balance")
		bidAmt := btcutil.Amount(bid.Details.Amt)
		err := sidecar.CheckOfferParams(
			bidAmt, btcutil.Amount(bid.SelfChanBalance),
			order.BaseSupplyUnit,
		)
		if err != nil {
			return err
		}

		bidUnits := order.NewSupplyFromSats(bidAmt)
		if bid.Details.MinUnitsMatch != uint32(bidUnits) {
			return fmt.Errorf("when using self_chan_balance the " +
				"min_chan_amt must be set to the same value " +
				"as amt")
		}
	}

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
			order.SupplyUnit(bid.Details.MinUnitsMatch).ToSatoshis(),
			btcutil.Amount(bid.SelfChanBalance),
			order.FixedRatePremium(bid.Details.RateFixed),
			bid.LeaseDurationBlocks,
			chainfee.SatPerKWeight(
				bid.Details.MaxBatchFeeRateSatPerKw,
			), false,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v", err)
		}

		if !promptForConfirmation("Confirm order (yes/no): ") {
			fmt.Println("Cancelling order...")
			return nil
		}
	}

	resp, err := client.SubmitOrder(
		context.Background(), &poolrpc.SubmitOrderRequest{
			Details: &poolrpc.SubmitOrderRequest_Bid{
				Bid: bid,
			},
			Initiator: defaultInitiator,
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
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose",
			Usage: "show verbose output including events",
		},
		cli.BoolFlag{
			Name:  "show_archived",
			Usage: "include orders no longer active",
		},
	},
	Action: ordersList,
}

func ordersList(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Default to only showing active orders.
	activeOnly := true
	if ctx.Bool("show_archived") {
		activeOnly = false
	}

	resp, err := client.ListOrders(
		context.Background(), &poolrpc.ListOrdersRequest{
			Verbose:    ctx.Bool("verbose"),
			ActiveOnly: activeOnly,
		},
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
		context.Background(), &poolrpc.CancelOrderRequest{
			OrderNonce: nonce,
		},
	)
	if err != nil {
		return err
	}
	printRespJSON(resp)
	return nil
}
