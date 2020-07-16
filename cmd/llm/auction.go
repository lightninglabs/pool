package main

import (
	"context"
	"encoding/hex"
	"fmt"

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
			batchSnapshotCommand,
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

var batchSnapshotCommand = cli.Command{
	Name:      "snapshot",
	ShortName: "s",
	Usage:     "return information about a prior cleared auction batch",
	Description: `
		Returns information about a prior batch such as the clearing
		price and the set of orders included in the batch. The
		prev_batch_id field can be used to explore prior batches in the
		sequence, similar to a block chain.
		`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "batch_id",
			Usage: "the target batch ID to obtain a snapshot " +
				"for, if left blank, information about the " +
				"latest batch is returned",
		},
	},
	Action: batchSnapshot,
}

func batchSnapshot(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var batchIDStr string
	switch {
	case ctx.Args().Present():
		batchIDStr = ctx.Args().First()

	case ctx.IsSet("batch_id"):
		batchIDStr = ctx.String("batch_id")
	}

	batchID, err := hex.DecodeString(batchIDStr)
	if err != nil {
		return fmt.Errorf("unable to decode batch ID: %v", err)
	}

	ctxb := context.Background()
	batchSnapshot, err := client.BatchSnapshot(
		ctxb,
		&clmrpc.BatchSnapshotRequest{
			BatchId: batchID,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(batchSnapshot)

	return nil
}
