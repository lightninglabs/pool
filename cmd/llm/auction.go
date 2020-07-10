package main

import (
	"context"

	"github.com/lightninglabs/llm/clmrpc"
	"github.com/urfave/cli"
)

var auctionCommands = []cli.Command{
	{
		Name:      "auction",
		ShortName: "auc",
		Usage:     "Interact with the auction itself.",
		Category:  "Auction",
		Subcommands: []cli.Command{
			auctionFeeCommand,
		},
	},
}

var auctionFeeCommand = cli.Command{
	Name:      "fee",
	ShortName: "f",
	Usage:     "query for the current auction execution fee",
	Description: `
		Returns the current auction execution fee. The fee is paid for
		each order matched and scales with the size of the order.
		`,
	Action: auctionFee,
}

func auctionFee(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	auctionFee, err := client.AuctionFee(
		context.Background(), &clmrpc.AuctionFeeRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(auctionFee)

	return nil
}
