package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/sidecar"
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
		},
	},
}

var sidecarOfferCommand = cli.Command{
	Name:      "offer",
	Aliases:   []string{"o"},
	Usage:     "offer a sidecar channel",
	ArgsUsage: "capacity self_chan_balance lease_duration_blocks",
	Description: `
	Creates an offer for providing a sidecar channel to another node.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "capacity",
			Usage: "the total channel capacity of the sidecar " +
				"channel to offer",
		},
		cli.Uint64Flag{
			Name: "self_chan_balance",
			Usage: "the number of satoshis that should be pushed " +
				"to the recipient of the sidecar channel as " +
				"initial outbound channel balance; amount " +
				"will be deducted from account that submits " +
				"bid order, reimbursement must happen out of " +
				"band, not part of the sidecar protocol",
		},
		cli.Uint64Flag{
			Name: "lease_duration_blocks",
			Usage: "the number of blocks the resulting leased " +
				"channel should be open for",
			Value: uint64(order.LegacyLeaseDurationBucket),
		},
	},
	Action: sidecarOffer,
}

func sidecarOffer(ctx *cli.Context) error {
	// Show help if no arguments or flags are provided.
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		_ = cli.ShowCommandHelp(ctx, "offer")
		return nil
	}

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

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.OfferSidecar(
		context.Background(), &poolrpc.OfferSidecarRequest{
			ChannelCapacitySat:  capacity,
			SelfChanBalance:     pushAmt,
			LeaseDurationBlocks: duration,
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
