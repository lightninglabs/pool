package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/urfave/cli"
)

const (
	defaultAskMaxDuration = 2016
	defaultBidMinDuration = 2016

	channelTypePeerDependent  = "legacy"
	channelTypeScriptEnforced = "script-enforced"

	auctionTypeInboundLiquidity  = "inbound"
	auctionTypeOutboundLiquidity = "outbound"

	defaultMaxBatchFeeRateSatPerVByte = 100

	defaultConfirmationConstraints = 1

	defaultAuctionType = auctionTypeInboundLiquidity

	submitOrderDescription = `
        Submit a new order in the given market. Currently, Pool supports two
        types of orders: asks, and bids. In Pool, market participants buy/sell 
        liquidity in _units_. A unit is 100,000 satoshis.

        In the *inbound* market, the bidder pays the asker a premium for a
        channel with 50% to 100% of inbound liquidity. The premium amount is 
        calculated using the funding amount of the asker.

        In the outbound market, the bidder pays the asker a premium for  
        a channel with outbound liquidity. 
        To participate in the outbound market the asker needs to create an order
        with '--amt=100000' (one unit) and the '--min_chan_amt' equals to 
        the minimum channel size that is willing to accept.
        The bidder needs to create an order with '--amt=100000' (one unit) 
        and '--self_chan_balance' equals to the funds that is willing to commit 
        in a channel. The premium amount is calculated using the funding amount 
        of the bidder ('--self_chan_balance').
        `
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
				Name:        "submit",
				Aliases:     []string{"s"},
				Usage:       "submit an order",
				Description: submitOrderDescription,
				Subcommands: []cli.Command{
					ordersSubmitAskCommand,
					ordersSubmitBidCommand,
				},
			},
		},
	},
}

// baseBidFlags is the set of flags that are common to any command that may
// need to accept a bid such as the main bid submission method as when a user
// attempts to offer a sidecar ticket.
var baseBidFlags = []cli.Flag{
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
	cli.BoolFlag{
		Name: "unannounced_channel",
		Usage: "flag used to signal that this bid is interested only " +
			"in unannounced channels. If this flag is not set, " +
			"the channels resulting from matching this order " +
			"will be announced to the network",
	},
	cli.BoolFlag{
		Name: "zero_conf_channel",
		Usage: "flag used to signal that this bid is only interested " +
			"in zero conf channels",
	},
}

var sharedFlags = []cli.Flag{
	cli.Uint64Flag{
		Name: "max_batch_fee_rate",
		Usage: "the maximum fee rate (sat/vByte) to use to for " +
			"the batch transaction",
		Value: defaultMaxBatchFeeRateSatPerVByte,
	},
	cli.StringFlag{
		Name: "channel_type",
		Usage: fmt.Sprintf("the type of channel resulting from the "+
			"order being matched (%q, %q)",
			channelTypePeerDependent,
			channelTypeScriptEnforced),
	},
	cli.StringFlag{
		Name: "auction_type",
		Usage: fmt.Sprintf("the auction market where this offer "+
			"must be considered in during matching (%q, %q)",
			auctionTypeInboundLiquidity,
			auctionTypeOutboundLiquidity),
		Value: defaultAuctionType,
	},
	cli.StringSliceFlag{
		Name: "allowed_node_id",
		Usage: "the list of nodes this order is allowed to match " +
			"with; if empty, the order will be able to match " +
			"with any node unless not_allowed_node_id is set. " +
			"Can be specified multiple times",
	},
	cli.StringSliceFlag{
		Name: "not_allowed_node_id",
		Usage: "the list of nodes this order is not allowed to match " +
			"with; if empty, the order will be able to match " +
			"with any node unless allowed_node_id is set. Can be " +
			"specified multiple times",
	},
	cli.BoolFlag{
		Name: "public",
		Usage: "flag used to signal that this order's details can be " +
			"shared in public market places.",
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

// isBidOrder returns true for the Bid type.
func isBidOrder(orderType order.Type) bool {
	return orderType == order.TypeBid
}

// isInboundLiquidityOrder returns true when the `auction_type` param is set
// to the inbound liquidity market.
func isInboundLiquidityOrder(ctx *cli.Context) bool {
	return ctx.String("auction_type") == auctionTypeInboundLiquidity
}

// isOutboundLiquidityOrder returns true when the `auction_type` param is set
// to the outbound liquidity market.
func isOutboundLiquidityOrder(ctx *cli.Context) bool {
	return ctx.String("auction_type") == auctionTypeOutboundLiquidity
}

// validateOrderAmount checks that the order amount is valid for the given
// market.
func validateOrderAmount(ctx *cli.Context, orderAmt uint64) error {
	baseSupply := uint64(order.BaseSupplyUnit)
	if isOutboundLiquidityOrder(ctx) && orderAmt != baseSupply {
		return fmt.Errorf("the order amount must be exactly the base "+
			"supply amount (%v sats)", baseSupply)
	}

	return nil
}

// getMinChanAmount returns the minChanAmount for an order taking into account
// the `min_chan_amt` parameter and the target market.
func getMinChanAmount(ctx *cli.Context, orderAmt uint64,
	orderType order.Type) btcutil.Amount {

	// If a `min_chan_amt` parameter was set by the user use its value.
	if ctx.IsSet("min_chan_amt") {
		return btcutil.Amount(ctx.Uint64("min_chan_amt"))
	}

	// If the `min_chan_amt` parameter was not set we use its default value
	// depending on auction/order type and order amount.
	switch {
	// If the minimum channel amount flag wasn't provided, use a
	// default of 10% and round to the nearest unit.
	case isInboundLiquidityOrder(ctx):
		minChanAmt := order.RoundToNextSupplyUnit(
			btcutil.Amount(orderAmt) / 10,
		).ToSatoshis()
		return minChanAmt

	// The outbound liquidity market only has default values for
	// bid orders.
	case isOutboundLiquidityOrder(ctx) && isBidOrder(orderType):
		return order.BaseSupplyUnit
	}

	return 0
}

// validateMinChanAmount checks that the minimum channel amount parameter
// has been properly set.
func validateMinChanAmount(ctx *cli.Context,
	orderAmt, minChanAmt btcutil.Amount, orderType order.Type) error {

	baseSupply := uint64(order.BaseSupplyUnit)
	switch {
	// minChanAmt will be internally expressed as supply units so it must
	// be a multiple of the BaseSupplyUnit.
	case minChanAmt%order.BaseSupplyUnit != 0:
		return fmt.Errorf("minimum channel amount %v must be "+
			"a multiple of %v", minChanAmt, baseSupply)

	// We cannot match less than one unit.
	case minChanAmt < order.BaseSupplyUnit:
		return fmt.Errorf("minimum channel amount %v is less than "+
			"acceptable lower bound (%v)", minChanAmt, baseSupply)

	// Validation checks that only apply to the inbound liquidity market.
	case isInboundLiquidityOrder(ctx):
		if minChanAmt > orderAmt {
			return fmt.Errorf("minimum channel amount %v is "+
				"above order amount %v", minChanAmt,
				orderAmt)
		}

	// Validation checks that only apply to the outbound liquidity market.
	//
	// NOTE: ask orders do not have any extra constraints in this market.
	case isOutboundLiquidityOrder(ctx) && isBidOrder(orderType):
		if minChanAmt != order.BaseSupplyUnit {
			return fmt.Errorf("the minimum channel amount must be "+
				"exactly equal to the base supply unit "+
				"(%v sats))", baseSupply)
		}
	}

	return nil
}

// validateOrderSelfBalance checks that the self balance amount is valid for
// the given market.
func validateOrderSelfBalance(ctx *cli.Context, bid *poolrpc.Bid) error {
	bidAmt := btcutil.Amount(bid.Details.Amt)
	bidUnits := order.NewSupplyFromSats(bidAmt)

	switch {
	case isInboundLiquidityOrder(ctx):
		// Make sure the self channel balance is within reasonable
		// limits (currently max the same of the total order amount)
		// and that the min units matched is also set to 100% of the
		// order amount.
		if bid.Details.MinUnitsMatch != uint32(bidUnits) {
			return fmt.Errorf("when using " +
				"self_chan_balance the min_chan_amt must be " +
				"set to the same value as amt")
		}

	case isOutboundLiquidityOrder(ctx):
		if bid.SelfChanBalance < uint64(order.BaseSupplyUnit) {
			return fmt.Errorf("the self balance amount must be at "+
				"least the base supply amount (%v sats)",
				order.BaseSupplyUnit)
		}
	}

	// Default checks.
	return order.CheckOfferParams(
		order.AuctionType(bid.Details.AuctionType), bidAmt,
		btcutil.Amount(bid.SelfChanBalance),
		order.BaseSupplyUnit,
	)
}

// parseCommonParams tries to read the common order parameters from the command
// line positional arguments and/or flags and parses them based on their
// destination data type. No formal in-depth validation is performed as the
// server will do that on the RPC level anyway.
func parseCommonParams(ctx *cli.Context, blockDuration uint32) (*poolrpc.Order,
	error) {

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
			return nil, fmt.Errorf("unable to decode amount: %v",
				err)
		}
		params.Amt = uint64(amt)
		args = args.Tail()
	}
	err := validateOrderAmount(ctx, params.Amt)
	if err != nil {
		return nil, fmt.Errorf("invalid order amount: %v", err)
	}

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

	// Determine the appropriate channel type that should be opened upon an
	// order match.
	channelType := ctx.String("channel_type")
	switch channelType {
	// No values means that the unknown type will be set, which means the
	// sever will select a type based on the version of the connected node.
	case "":
		break
	case channelTypePeerDependent:
		params.ChannelType = auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_PEER_DEPENDENT

	case channelTypeScriptEnforced:
		params.ChannelType = auctioneerrpc.OrderChannelType_ORDER_CHANNEL_TYPE_SCRIPT_ENFORCED

	default:
		return nil, fmt.Errorf("unknown channel type %q", channelType)
	}

	auctionType := ctx.String("auction_type")
	switch auctionType {
	case auctionTypeInboundLiquidity:
		params.AuctionType = auctioneerrpc.AuctionType_AUCTION_TYPE_BTC_INBOUND_LIQUIDITY

	case auctionTypeOutboundLiquidity:
		params.AuctionType = auctioneerrpc.AuctionType_AUCTION_TYPE_BTC_OUTBOUND_LIQUIDITY

	default:
		return nil, fmt.Errorf("unknown auction type %q", channelType)
	}

	// Get the list of node ids this order is allowed/not allowed to match
	// with.
	allowedNodeIDs, err := parseNodePubKeySlice(ctx, "allowed_node_id")
	if err != nil {
		return nil, fmt.Errorf("unable to parse allowed_node_id: %v",
			err)
	}

	notAllowedNodeIDs, err := parseNodePubKeySlice(
		ctx, "not_allowed_node_id",
	)
	if err != nil {
		return nil, fmt.Errorf("unable to parse "+
			"not_allowed_node_id: %v", err)
	}

	// By default the order is able to match with all the orders unless
	// one of this fields is specified. They are incompatible.
	if len(allowedNodeIDs) > 0 && len(notAllowedNodeIDs) > 0 {
		return nil, fmt.Errorf("allowed_node_id and " +
			"not_allowed_node_id cannot be set together")
	}

	params.AllowedNodeIds = allowedNodeIDs
	params.NotAllowedNodeIds = notAllowedNodeIDs

	params.IsPublic = true
	if ctx.IsSet("public") {
		params.IsPublic = ctx.Bool("public")
	}

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

// parseNodePubKeySlice parses the list of node ids in the paramater matching
// the given `key`.
//
// NOTE: the parameter must contain a string slice. The strings are hex decoded
// but not parsed as a btcec.PublicKey.
func parseNodePubKeySlice(ctx *cli.Context, key string) ([][]byte, error) {
	hexNodeIDs := ctx.StringSlice(key)
	nodeIDs := make([][]byte, 0, len(hexNodeIDs))
	for _, hexNodeID := range hexNodeIDs {
		nodeID, err := hex.DecodeString(hexNodeID)
		if err != nil {
			return nil, fmt.Errorf("invalid node ID %v: %v",
				hexNodeID, err)
		}
		nodeIDs = append(nodeIDs, nodeID)
	}

	return nodeIDs, nil
}

var ordersSubmitAskCommand = cli.Command{
	Name:  "ask",
	Usage: "offer channel liquidity",
	ArgsUsage: "amt acct_key [--interest_rate_percent=R] " +
		"[--max_batch_fee_rate=F] [--lease_duration_blocks=M]",
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
		cli.Uint64Flag{
			Name: "announcement_constraints",
			Usage: "specifies if the liquidity must be sold in " +
				"announced or unannounced channels. Set to 0 " +
				"to express no preference, set to 1 for only " +
				"announced channels and 2 for only " +
				"unannounced ones. The default value is \"no " +
				"preference\"",
		},
		cli.Uint64Flag{
			Name: "confirmation_constraints",
			Usage: "specifies if the liquidity must be sold in " +
				"zero conf or confirmed channels. Set to 0 " +
				"to express no preference, set to 1 for only " +
				"confirmed channels and 2 for only zero conf " +
				"ones. The default value is \"only confirmed\"",
			Value: defaultConfirmationConstraints,
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

	announcement := auctioneerrpc.ChannelAnnouncementConstraints(
		ctx.Uint64("announcement_constraints"),
	)

	confirmations := auctioneerrpc.ChannelConfirmationConstraints(
		ctx.Uint64("confirmation_constraints"),
	)

	ask := &poolrpc.Ask{
		LeaseDurationBlocks: uint32(
			ctx.Uint64("lease_duration_blocks"),
		),
		Version:                 uint32(order.VersionChannelType),
		AnnouncementConstraints: announcement,
		ConfirmationConstraints: confirmations,
	}

	params, err := parseCommonParams(ctx, ask.LeaseDurationBlocks)
	if err != nil {
		return fmt.Errorf("unable to parse order params: %v", err)
	}

	minChanAmt := getMinChanAmount(ctx, params.Amt, order.TypeAsk)
	err = validateMinChanAmount(
		ctx, btcutil.Amount(params.Amt), minChanAmt, order.TypeAsk,
	)
	if err != nil {
		return fmt.Errorf("invalid min chan amount: %v", err)
	}
	params.MinUnitsMatch = uint32(minChanAmt / order.BaseSupplyUnit)

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
			order.SupplyUnit(ask.Details.MinUnitsMatch),
			0, order.FixedRatePremium(ask.Details.RateFixed),
			ask.LeaseDurationBlocks,
			chainfee.SatPerKWeight(
				ask.Details.MaxBatchFeeRateSatPerKw,
			), true, ask.Details.IsPublic, nil,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v",
				err)
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

func printOrderDetails(client poolrpc.TraderClient, amt btcutil.Amount,
	minUnitsMatch order.SupplyUnit, selfChanBalance btcutil.Amount,
	rate order.FixedRatePremium, leaseDuration uint32,
	maxBatchFeeRate chainfee.SatPerKWeight, isAsk, isPublic bool,
	sidecarTicket *sidecar.Ticket) error {

	quote, err := client.QuoteOrder(
		context.Background(), &poolrpc.QuoteOrderRequest{
			Amt:                     uint64(amt),
			RateFixed:               uint32(rate),
			LeaseDurationBlocks:     leaseDuration,
			MaxBatchFeeRateSatPerKw: uint64(maxBatchFeeRate),
			MinUnitsMatch:           uint32(minUnitsMatch),
		},
	)
	if err != nil {
		return err
	}

	orderType := "Bid"
	premiumDescription := "paid to maker"
	if isAsk {
		orderType = "Ask"
		premiumDescription = "yield from taker"
	}

	fmt.Println("-- Order Details --")
	fmt.Printf("%v Amount: %v\n", orderType, amt)
	fmt.Printf("%v Duration: %v\n", orderType, leaseDuration)
	fmt.Printf("Total Premium (%v): %v \n", premiumDescription,
		btcutil.Amount(quote.TotalPremiumSat))
	fmt.Printf("Rate Fixed: %v\n", rate)
	fmt.Printf("Rate Per Block: %.9f (%.7f%%)\n", quote.RatePerBlock,
		quote.RatePercent)
	fmt.Println("Execution Fee: ",
		btcutil.Amount(quote.TotalExecutionFeeSat))
	fmt.Printf("Max batch fee rate: %d sat/vByte\n",
		maxBatchFeeRate.FeePerKVByte()/1000)
	fmt.Println("Max chain fee:",
		btcutil.Amount(quote.WorstCaseChainFeeSat))

	if selfChanBalance > 0 {
		fmt.Printf("Self channel balance: %v\n", selfChanBalance)
	}

	fmt.Printf("Is public: %t\n", isPublic)

	if sidecarTicket != nil {
		fmt.Println("Sidecar order: ")
		fmt.Printf("  Recipient node: %x\n",
			sidecarTicket.Recipient.NodePubKey.SerializeCompressed())
	}

	return nil
}

var ordersSubmitBidCommand = cli.Command{
	Name:  "bid",
	Usage: "obtain channel liquidity",
	ArgsUsage: "amt acct_key [--interest_rate_percent=R]" +
		"[--max_batch_fee_rate=F] [--lease_duration_blocks=M]",
	Description: `
	Place an offer for acquiring inbound liquidity by lending
	funding capacity from another participant in the order book.`,
	Flags: append(
		append(
			baseBidFlags,
			cli.StringFlag{
				Name: "sidecar_ticket",
				Usage: "instead of leasing a channel for the node " +
					"connected to this pool instance, lease a " +
					"channel for another node; use the " +
					"information within the ticket to identify " +
					"the receiver of the sidecar channel; using " +
					"a sidecar ticket will also overwrite the " +
					"amt, min_chan_amt, lease_duration_blocks " +
					"and self_chan_balance fields",
			},
		), sharedFlags...,
	),
	Action: ordersSubmitBid,
}

func parseBaseBid(ctx *cli.Context) (*poolrpc.Bid, *sidecar.Ticket, error) {
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
		return nil, nil, err
	}

	bid := &poolrpc.Bid{
		LeaseDurationBlocks: uint32(
			ctx.Uint64("lease_duration_blocks"),
		),
		Version:            uint32(order.VersionChannelType),
		MinNodeTier:        nodeTier,
		UnannouncedChannel: ctx.Bool("unannounced_channel"),
		ZeroConfChannel:    ctx.Bool("zero_conf_channel"),
	}

	// Let's find out if this is an order for a sidecar channel because if
	// it is, we can take some of the information out of the ticket and
	// don't require the user to enter them manually again.
	var ticket *sidecar.Ticket
	if ctx.IsSet("sidecar_ticket") {
		// The ticket is expected in the string encoded version which
		// has a prefix and a checksum. We're supposed to send it to
		// the daemon in its raw format though. So let's decode and
		// check it in the process.
		ticket, err = sidecar.DecodeString(ctx.String("sidecar_ticket"))
		if err != nil {
			return nil, nil, fmt.Errorf("unable to parse sidecar "+
				"ticket: %v", err)
		}

		// Let's make sure the ticket is in the correct state. This will
		// be checked by the server as well but we want to make sure we
		// don't run into a nil reference when printing the order
		// details below.
		if ticket.State != sidecar.StateRegistered ||
			ticket.Recipient == nil {

			return nil, nil, fmt.Errorf("unexpected sidecar "+
				"ticket state %d, possibly not registered "+
				"with recipient node yet", ticket.State)
		}

		// With the ticket parsed and formally checked, we can now pre-
		// fill the amount and min channel amount. Those values must
		// match the offered capacity, otherwise the push amount won't
		// work as expected and the protocol would get more complex as
		// well.
		amtStr := fmt.Sprintf("%d", ticket.Offer.Capacity)
		pushAmtStr := fmt.Sprintf("%d", ticket.Offer.PushAmt)
		_ = ctx.Set("amt", amtStr)
		_ = ctx.Set("min_chan_amt", amtStr)
		_ = ctx.Set("self_chan_balance", pushAmtStr)
		bid.LeaseDurationBlocks = ticket.Offer.LeaseDurationBlocks

		// Looks good so far. The rest will be checked server side. For
		// now we can just add the ticket to the order.
		bid.SidecarTicket = ctx.String("sidecar_ticket")
	}

	params, err := parseCommonParams(ctx, bid.LeaseDurationBlocks)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to parse order "+
			"params: %v", err)
	}

	minChanAmt := getMinChanAmount(ctx, params.Amt, order.TypeBid)
	err = validateMinChanAmount(
		ctx, btcutil.Amount(params.Amt), minChanAmt, order.TypeBid,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid min chan amount: %v", err)
	}
	params.MinUnitsMatch = uint32(minChanAmt / order.BaseSupplyUnit)

	bid.Details = params

	hasSelfChanBalance := ctx.IsSet("self_chan_balance")
	switch {
	case !hasSelfChanBalance && isOutboundLiquidityOrder(ctx):
		return nil, nil, fmt.Errorf("the `self_chan_balance` "+
			"parameter must be set to at least %v sats",
			order.BaseSupplyUnit)

	case hasSelfChanBalance:
		bid.SelfChanBalance = ctx.Uint64("self_chan_balance")
		if err = validateOrderSelfBalance(ctx, bid); err != nil {
			return nil, nil, fmt.Errorf("invalid self balance "+
				"amount: %v", err)
		}
	}

	return bid, ticket, nil
}

func ordersSubmitBid(ctx *cli.Context) error { // nolint: dupl
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "bid")
		return nil
	}

	bid, ticket, err := parseBaseBid(ctx)
	if err != nil {
		return err
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
			order.SupplyUnit(bid.Details.MinUnitsMatch),
			btcutil.Amount(bid.SelfChanBalance),
			order.FixedRatePremium(bid.Details.RateFixed),
			bid.LeaseDurationBlocks,
			chainfee.SatPerKWeight(
				bid.Details.MaxBatchFeeRateSatPerKw,
			), false, bid.Details.IsPublic, ticket,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v",
				err)
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
