package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/urfave/cli"
)

var sidecarCommands = []cli.Command{
	{
		Name:     "sidecar",
		Aliases:  []string{"s"},
		Usage:    "Manage sidecar channels.",
		Category: "Orders",
		Subcommands: []cli.Command{
			sidecarOfferCommand,
			sidecarPrintTicketCommand,
			sidecarRegisterCommand,
			sidecarExpectChannelCommand,
		},
	},
}

var sidecarOfferCommand = cli.Command{
	Name:      "offer",
	Aliases:   []string{"o"},
	Usage:     "offer a sidecar channel",
	ArgsUsage: "[<full bid args> --auto] | capacity self_chan_balance lease_duration_blocks",
	Description: `
	Creates an offer for providing a sidecar channel to another node.
	If the auto flag is specified, then all bid information needs to be 
	specified as normal. If the auto flag isn't specified, then only 
	capacity, self_chan_balance and lease_duration_blocks needs to set.`,
	Flags: append(
		append(baseBidFlags, sharedFlags...),
		cli.BoolFlag{
			Name: "auto",
			Usage: "if true, then the full bid information needs to " +
				"be specified as automated negotiation will be " +
				"attempted",
		},
	),
	Action: sidecarOffer,
}

func sidecarOffer(ctx *cli.Context) error {
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "offer")
		return nil
	}

	var (
		bid *poolrpc.Bid
		err error
	)

	// If auto isn't set, then we'll only need to parse out a hand full of
	// fields to submit a valid ticket.
	if !ctx.Bool("auto") {
		var (
			args              = ctx.Args()
			capacity, pushAmt uint64
			duration          uint32
		)

		switch {
		case ctx.IsSet("capacity"):
			capacity = ctx.Uint64("capacity")
		case args.Present():
			parsed, err := parseAmt(args.First())
			if err != nil {
				return fmt.Errorf("unable to decode capacity: %v", err)
			}
			capacity = uint64(parsed)
			args = args.Tail()
		}

		switch {
		case ctx.IsSet("self_chan_balance"):
			pushAmt = ctx.Uint64("self_chan_balance")
		case args.Present():
			parsed, err := parseAmt(args.First())
			if err != nil {
				return fmt.Errorf("unable to decode self channel "+
					"balance: %v", err)
			}
			pushAmt = uint64(parsed)
			args = args.Tail()
		}

		switch {
		case ctx.IsSet("lease_duration_blocks"):
			duration = uint32(ctx.Uint64("lease_duration_blocks"))
		case args.Present():
			duration64, err := strconv.ParseInt(args.First(), 10, 32)
			if err != nil {
				return fmt.Errorf("unable to parse lease duration "+
					"blocks: %v", err)
			}
			duration = uint32(duration64)
			args = args.Tail()
		}

		bid = &poolrpc.Bid{
			Details: &poolrpc.Order{
				Amt: capacity,
			},
			SelfChanBalance:     pushAmt,
			LeaseDurationBlocks: duration,
		}

	} else {
		// Otherwise, will parse out the full bid as normal.
		bid, _, err = parseBaseBid(ctx)
		if err != nil {
			return err

		}
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}

	defer cleanup()

	// Give the user a chance to confirm the order details as this is
	// binding once submitted, but only if the entire bid was specified.
	if !ctx.Bool("force") && ctx.Bool("auto") {
		if err := printOrderDetails(
			client, btcutil.Amount(bid.Details.Amt),
			order.SupplyUnit(bid.Details.MinUnitsMatch),
			btcutil.Amount(bid.SelfChanBalance),
			order.FixedRatePremium(bid.Details.RateFixed),
			bid.LeaseDurationBlocks,
			chainfee.SatPerKWeight(
				bid.Details.MaxBatchFeeRateSatPerKw,
			), false, nil,
		); err != nil {
			return fmt.Errorf("unable to print order details: %v", err)
		}

		if !promptForConfirmation("Confirm order (yes/no): ") {
			fmt.Println("Cancelling order...")
			return nil
		}
	}

	resp, err := client.OfferSidecar(
		context.Background(), &poolrpc.OfferSidecarRequest{
			AutoNegotiate: ctx.Bool("auto"),
			Bid:           bid,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

type jsonTicket struct {
	ID                        string
	Version                   uint8
	State                     string
	Capacity                  uint64
	PushAmount                uint64
	LeaseDurationBlocks       uint32
	OfferSigningPubKey        string
	RecipientNodePubKey       string
	RecipientMultiSigPubKey   string
	RecipientMultiSigKeyIndex uint32
	OrderNonce                string
}

var sidecarPrintTicketCommand = cli.Command{
	Name:      "printticket",
	Aliases:   []string{"p"},
	Usage:     "decode and print the content of a sidecar ticket",
	ArgsUsage: "ticket",
	Description: `
	Tries to decode the given ticket from the human readable (prefixed)
	base64 encoded version.`,
	Action: sidecarPrintTicket,
}

func sidecarPrintTicket(ctx *cli.Context) error {
	// Show help if no arguments or flags are provided.
	if ctx.NArg() != 1 || ctx.NumFlags() != 0 {
		_ = cli.ShowCommandHelp(ctx, "printticket")
		return nil
	}

	ticketStr := ctx.Args().First()

	ticket, err := sidecar.DecodeString(ticketStr)
	if err != nil {
		return fmt.Errorf("error decoding base64 ticket: %v", err)
	}

	jsonTicket := &jsonTicket{
		ID:                  hex.EncodeToString(ticket.ID[:]),
		Version:             uint8(ticket.Version),
		State:               ticket.State.String(),
		Capacity:            uint64(ticket.Offer.Capacity),
		PushAmount:          uint64(ticket.Offer.PushAmt),
		LeaseDurationBlocks: ticket.Offer.LeaseDurationBlocks,
	}

	if ticket.Offer.SignPubKey != nil {
		jsonTicket.OfferSigningPubKey = hex.EncodeToString(
			ticket.Offer.SignPubKey.SerializeCompressed(),
		)
	}
	if ticket.Recipient != nil {
		if ticket.Recipient.NodePubKey != nil {
			jsonTicket.RecipientNodePubKey = hex.EncodeToString(
				ticket.Recipient.NodePubKey.SerializeCompressed(),
			)
		}
		if ticket.Recipient.MultiSigPubKey != nil {
			jsonTicket.RecipientMultiSigPubKey = hex.EncodeToString(
				ticket.Recipient.MultiSigPubKey.SerializeCompressed(),
			)
		}
		jsonTicket.RecipientMultiSigKeyIndex = ticket.Recipient.MultiSigKeyIndex
	}
	if ticket.Order != nil {
		jsonTicket.OrderNonce = hex.EncodeToString(
			ticket.Order.BidNonce[:],
		)
	}

	printJSON(jsonTicket)

	return nil
}

var sidecarRegisterCommand = cli.Command{
	Name:    "register",
	Aliases: []string{"r"},
	Usage: "register an incoming sidecar channel and add node info to " +
		"ticket",
	ArgsUsage: "ticket",
	Description: `
	Registers a sidecar ticket for an incoming sidecar channel with the node
	and adds its recipient information to it, resulting in an updated ticket
	that needs to be handed back to the provider.`,
	Action: sidecarRegister,
}

func sidecarRegister(ctx *cli.Context) error {
	// Show help if no arguments or flags are provided.
	if ctx.NArg() != 1 || ctx.NumFlags() != 0 {
		_ = cli.ShowCommandHelp(ctx, "register")
		return nil
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.RegisterSidecar(
		context.Background(), &poolrpc.RegisterSidecarRequest{
			Ticket: ctx.Args().First(),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var sidecarExpectChannelCommand = cli.Command{
	Name:      "expectchannel",
	Aliases:   []string{"e"},
	Usage:     "start waiting for sidecar channel",
	ArgsUsage: "ticket",
	Description: `
	Connect to the auctioneer and wait for a sidecar order to be matched and
	a channel being opened to us.`,
	Action: sidecarExpectChannel,
}

func sidecarExpectChannel(ctx *cli.Context) error {
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "expectchannel")
		return nil
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ExpectSidecarChannel(
		context.Background(), &poolrpc.ExpectSidecarChannelRequest{
			Ticket: ctx.Args().First(),
		})
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
