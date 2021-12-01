package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/pool/auctioneerrpc"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/urfave/cli"
)

type Lease struct {
	ChannelPoint          string `json:"channel_point"`
	ChannelAmtSat         uint64 `json:"channel_amt_sat"`
	ChannelDurationBlocks uint32 `json:"channel_duration_blocks"`
	ChannelLeaseExpiry    uint32 `json:"channel_lease_expiry"`
	ChannelRemoteNodeKey  string `json:"channel_node_key"`
	ChannelNodeTier       string `json:"channel_node_tier"`
	PremiumSat            uint64 `json:"premium_sat"`
	ClearingRatePrice     uint64 `json:"clearing_rate_price"`
	OrderFixedRate        uint64 `json:"order_fixed_rate"`
	ExecutionFeeSat       uint64 `json:"execution_fee_sat"`
	ChainFeeSat           uint64 `json:"chain_fee_sat"`
	OrderNonce            string `json:"order_nonce"`
	MatchedOrderNonce     string `json:"matched_order_nonce"`
	Purchased             bool   `json:"purchased"`
	SelfChanBalance       uint64 `json:"self_chan_balance"`
	SidecarChannel        bool   `json:"sidecar_channel"`
}

// NewLeaseFromProto creates a display Lease from its proto.
func NewLeaseFromProto(a *poolrpc.Lease) *Lease {
	var opHash chainhash.Hash
	copy(opHash[:], a.ChannelPoint.Txid)

	chanPoint := fmt.Sprintf("%v:%d", opHash, a.ChannelPoint.OutputIndex)
	return &Lease{
		ChannelPoint:          chanPoint,
		ChannelAmtSat:         a.ChannelAmtSat,
		ChannelDurationBlocks: a.ChannelDurationBlocks,
		ChannelLeaseExpiry:    a.ChannelLeaseExpiry,
		ChannelRemoteNodeKey:  hex.EncodeToString(a.ChannelRemoteNodeKey),
		ChannelNodeTier:       a.ChannelNodeTier.String(),
		PremiumSat:            a.PremiumSat,
		ClearingRatePrice:     a.ClearingRatePrice,
		OrderFixedRate:        a.OrderFixedRate,
		ExecutionFeeSat:       a.ExecutionFeeSat,
		ChainFeeSat:           a.ChainFeeSat,
		OrderNonce:            hex.EncodeToString(a.OrderNonce),
		MatchedOrderNonce:     hex.EncodeToString(a.MatchedOrderNonce),
		Purchased:             a.Purchased,
		SelfChanBalance:       a.SelfChanBalance,
		SidecarChannel:        a.SidecarChannel,
	}
}

// Markets is a simple type alias to make the following Snapshot struct more
// compact.
type Markets = map[uint32]*auctioneerrpc.MatchedMarketSnapshot

// Snapshot is a copy of poolrpc.BatchSnapshotResponse that does not include the
// deprecated fields.
type Snapshot struct {
	Version                uint32  `json:"version"`
	BatchID                string  `json:"batch_id"`
	PrevBatchID            string  `json:"prev_batch_id"`
	BatchTxID              string  `json:"batch_tx_id"`
	BatchTx                string  `json:"batch_tx"`
	BatchTxFeeRateSatPerKw uint64  `json:"batch_tx_fee_rate_sat_per_kw"`
	CreationTimestampNs    uint64  `json:"creation_timestamp_ns"`
	MatchedMarkets         Markets `json:"matched_markets"`
}

// NewSnapshotsFromProto creates a display Snapshot from its proto.
func NewSnapshotsFromProto(s []*auctioneerrpc.BatchSnapshotResponse) []*Snapshot {
	result := make([]*Snapshot, len(s))
	for idx, snapshot := range s {
		result[idx] = &Snapshot{
			Version: snapshot.Version,
			BatchID: hex.EncodeToString(
				snapshot.BatchId,
			),
			PrevBatchID: hex.EncodeToString(
				snapshot.PrevBatchId,
			),
			BatchTxID: snapshot.BatchTxId,
			BatchTx: hex.EncodeToString(
				snapshot.BatchTx,
			),
			BatchTxFeeRateSatPerKw: snapshot.BatchTxFeeRateSatPerKw,
			CreationTimestampNs:    snapshot.CreationTimestampNs,
			MatchedMarkets:         snapshot.MatchedMarkets,
		}
	}

	return result
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
			leaseDurationsCommand,
			nextBatchInfoCommand,
			ratingsCommand,
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
	ArgsUsage: "batch_id",
	Description: `
		Returns information about a prior batch or batches such as the
		clearing price and the set of orders included in the batch. The
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
		cli.Uint64Flag{
			Name: "num_batches",
			Usage: "the number of batches to show, starting at " +
				"the batch_id and going back through the " +
				"history of finalized batches",
			Value: 1,
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

	var (
		batchIDStr string
		ctxb       = context.Background()
	)
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

	resp, err := client.BatchSnapshots(
		ctxb, &auctioneerrpc.BatchSnapshotsRequest{
			StartBatchId:   batchID,
			NumBatchesBack: uint32(ctx.Uint64("num_batches")),
		},
	)
	if err != nil {
		return err
	}

	// Get rid of the deprecated fields by copying the response into a new
	// struct just for displaying. Can be removed once the deprecated fields
	// are removed from the proto (alpha->beta transition?).
	// TODO(guggero): Use original proto message once deprecated fields are
	// removed.
	printJSON(NewSnapshotsFromProto(resp.Batches))

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

var leaseDurationsCommand = cli.Command{
	Name:      "leasedurations",
	ShortName: "ld",
	Usage: "returns the current active set of accepted lease " +
		"durations for orders",
	Action: leaseDurations,
}

func leaseDurations(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.LeaseDurations(
		context.Background(), &poolrpc.LeaseDurationRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var nextBatchInfoCommand = cli.Command{
	Name:      "nextbatchinfo",
	ShortName: "nbi",
	Usage:     "returns information about the next batch the auctioneer will attempt to clear",
	Action:    nextBatchInfo,
}

func nextBatchInfo(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.NextBatchInfo(
		context.Background(), &poolrpc.NextBatchInfoRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var ratingsCommand = cli.Command{
	Name:      "ratings",
	ShortName: "r",
	Usage:     "query for the current ratings information of a Lightning Node",
	Description: `
		Returns the current Node Tier of a node, along with other
		information.
		`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "node_key",
			Usage: "the node key to look up the rating for",
		},
	},
	Action: ratings,
}

func ratings(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	var nodeKey string
	switch {
	case ctx.Args().Present():
		nodeKey = ctx.Args().First()

	default:
		nodeKey = ctx.String("node_key")
	}

	if nodeKey == "" {
		return fmt.Errorf("an LN node key must be provided")
	}

	nodeKeyBytes, err := hex.DecodeString(nodeKey)
	if err != nil {
		return fmt.Errorf("unable to decode node key: %v", err)
	}

	nodeRating, err := client.NodeRatings(
		context.Background(), &poolrpc.NodeRatingRequest{
			NodePubkeys: [][]byte{nodeKeyBytes},
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(nodeRating)

	return nil
}
