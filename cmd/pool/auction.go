package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/lightninglabs/pool/poolrpc"
	"github.com/urfave/cli"
)

type Lease struct {
	ChannelPoint          string `json:"channel_point"`
	ChannelAmtSat         uint64 `json:"channel_amt_sat"`
	ChannelDurationBlocks uint32 `json:"channel_duration_blocks"`
	PremiumSat            uint64 `json:"premium_sat"`
	ExecutionFeeSat       uint64 `json:"execution_fee_sat"`
	ChainFeeSat           uint64 `json:"chain_fee_sat"`
	OrderNonce            string `json:"order_nonce"`
	Purchased             bool   `json:"purchased"`
}

// NewLeaseFromProto creates a display Lease from its proto.
func NewLeaseFromProto(a *poolrpc.Lease) *Lease {
	chanPoint := fmt.Sprintf("%x:%v", a.ChannelPoint.Txid,
		a.ChannelPoint.OutputIndex)

	return &Lease{
		ChannelPoint:          chanPoint,
		ChannelAmtSat:         a.ChannelAmtSat,
		ChannelDurationBlocks: a.ChannelDurationBlocks,
		PremiumSat:            a.PremiumSat,
		ExecutionFeeSat:       a.ExecutionFeeSat,
		ChainFeeSat:           a.ChainFeeSat,
		OrderNonce:            hex.EncodeToString(a.OrderNonce),
		Purchased:             a.Purchased,
	}
}

var auctionCommands = []cli.Command{
	{
		Name:      "auction",
		ShortName: "auc",
		Usage:     "Interact with the auction itself.",
		Category:  "Auction",
		Subcommands: []cli.Command{
			auctionFeeCommand,
			batchSnapshotCommand,
			leasesCommand,
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
		context.Background(), &poolrpc.AuctionFeeRequest{},
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
		&poolrpc.BatchSnapshotRequest{
			BatchId: batchID,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(batchSnapshot)

	return nil
}

var leasesCommand = cli.Command{
	Name:      "leases",
	ShortName: "l",
	Usage:     "returns the list of leases purchased/sold within the auction",
	Description: `
	Returns the list of leases (i.e., channels) that were either purchased
	or sold by the trader within the auction. An optional list of batch IDs
	and accounts can be specified to filter the leases returned.
	`,
	Flags: []cli.Flag{
		cli.StringSliceFlag{
			Name: "batch_ids",
			Usage: "the target batch IDs to obtain leases for, " +
				"if left blank, leases from all batches are " +
				"returned",
		},
		cli.StringSliceFlag{
			Name: "accounts",
			Usage: "the target accounts to obtain leases for, if " +
				"left blank, leases from all accounts are " +
				"returned",
		},
	},
	Action: leases,
}

func leases(ctx *cli.Context) error {
	hexBatchIDs := ctx.StringSlice("batch_ids")
	batchIDs := make([][]byte, 0, len(hexBatchIDs))
	for _, hexBatchID := range hexBatchIDs {
		batchID, err := hex.DecodeString(hexBatchID)
		if err != nil {
			return fmt.Errorf("invalid batch ID %v: %v", hexBatchID,
				err)
		}
		batchIDs = append(batchIDs, batchID)
	}

	hexAccountKeys := ctx.StringSlice("accounts")
	accounts := make([][]byte, 0, len(hexAccountKeys))
	for _, hexAccountKey := range hexAccountKeys {
		accountKey, err := hex.DecodeString(hexAccountKey)
		if err != nil {
			return fmt.Errorf("invalid account key %v: %v",
				hexAccountKey, err)
		}
		accounts = append(accounts, accountKey)
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.Leases(context.Background(), &poolrpc.LeasesRequest{
		BatchIds: batchIDs,
		Accounts: accounts,
	})
	if err != nil {
		return err
	}

	displayLeases := make([]*Lease, 0, len(resp.Leases))
	for _, lease := range resp.Leases {
		displayLeases = append(displayLeases, NewLeaseFromProto(lease))
	}

	leasesResp := struct {
		Leases            []*Lease `json:"leases"`
		TotalAmtEarnedSat uint64   `json:"total_amt_earned_sat"`
		TotalAmtPaidSat   uint64   `json:"total_amt_paid_sat"`
	}{
		Leases:            displayLeases,
		TotalAmtEarnedSat: resp.TotalAmtEarnedSat,
		TotalAmtPaidSat:   resp.TotalAmtPaidSat,
	}

	printJSON(leasesResp)

	return nil
}
