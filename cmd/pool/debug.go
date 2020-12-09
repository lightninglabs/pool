package main

import (
	"bytes"
	"fmt"
	"path"
	"path/filepath"

	"github.com/lightninglabs/pool"
	"github.com/lightninglabs/pool/auctioneer"
	"github.com/lightninglabs/pool/clientdb"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/urfave/cli"
)

var debugCommands = []cli.Command{
	{
		Name:      "debug",
		ShortName: "d",
		Usage:     "Debug the local trader state.",
		Hidden:    true,
		Subcommands: []cli.Command{
			dumpAccountsCommand,
			dumpOrdersCommand,
			dumpPendingBatcheCommand,
			removePendingBatchCommand,
		},
	},
}

var dumpAccountsCommand = cli.Command{
	Name:      "dumpaccounts",
	ShortName: "da",
	Usage:     "dump all accounts contained in the local database",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "db",
			Usage: "the specific pool database to use instead " +
				"of the default one on ~/.pool/<network>/" +
				"pool.db",
		},
	},
	Action: dumpAccounts,
}

func dumpAccounts(ctx *cli.Context) error {
	db, err := getPoolDB(ctx)
	if err != nil {
		return fmt.Errorf("error loading DB: %v", err)
	}

	accounts, err := db.Accounts()
	if err != nil {
		return fmt.Errorf("error loading accounts: %v", err)
	}

	rpcAccounts := make([]*poolrpc.Account, len(accounts))
	for idx, acct := range accounts {
		rpcAcct, err := pool.MarshallAccount(acct)
		if err != nil {
			return fmt.Errorf("error marshalling account: %v", err)
		}
		rpcAccounts[idx] = rpcAcct
	}

	printRespJSON(&poolrpc.ListAccountsResponse{Accounts: rpcAccounts})

	return nil
}

var dumpOrdersCommand = cli.Command{
	Name:      "dumporders",
	ShortName: "do",
	Usage:     "dump all orders contained in the local database",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "db",
			Usage: "the specific pool database to use instead " +
				"of the default one on ~/.pool/<network>/" +
				"pool.db",
		},
	},
	Action: dumpOrders,
}

func dumpOrders(ctx *cli.Context) error {
	db, err := getPoolDB(ctx)
	if err != nil {
		return fmt.Errorf("error loading DB: %v", err)
	}

	orders, err := db.GetOrders()
	if err != nil {
		return fmt.Errorf("error loading orders: %v", err)
	}

	asks := make([]*poolrpc.Ask, 0, len(orders))
	bids := make([]*poolrpc.Bid, 0, len(orders))
	for _, dbOrder := range orders {
		nonce := dbOrder.Nonce()
		dbDetails := dbOrder.Details()

		orderState, err := pool.DBOrderStateToRPCState(dbDetails.State)
		if err != nil {
			return err
		}

		details := &poolrpc.Order{
			TraderKey: dbDetails.AcctKey[:],
			RateFixed: dbDetails.FixedRate,
			Amt:       uint64(dbDetails.Amt),
			MaxBatchFeeRateSatPerKw: uint64(
				dbDetails.MaxBatchFeeRate,
			),
			OrderNonce:       nonce[:],
			State:            orderState,
			Units:            uint32(dbDetails.Units),
			UnitsUnfulfilled: uint32(dbDetails.UnitsUnfulfilled),
			ReservedValueSat: uint64(dbOrder.ReservedValue(
				terms.NewLinearFeeSchedule(0, 0),
			)),
			CreationTimestampNs: uint64(0),
			MinUnitsMatch: uint32(
				dbOrder.Details().MinUnitsMatch,
			),
		}

		switch o := dbOrder.(type) {
		case *order.Ask:
			rpcAsk := &poolrpc.Ask{
				Details:             details,
				LeaseDurationBlocks: dbDetails.LeaseDuration,
				Version:             uint32(o.Version),
			}
			asks = append(asks, rpcAsk)

		case *order.Bid:
			nodeTier, err := auctioneer.MarshallNodeTier(
				o.MinNodeTier,
			)
			if err != nil {
				return err
			}

			rpcBid := &poolrpc.Bid{
				Details:             details,
				LeaseDurationBlocks: dbDetails.LeaseDuration,
				Version:             uint32(o.Version),
				MinNodeTier:         nodeTier,
			}
			bids = append(bids, rpcBid)

		default:
			return fmt.Errorf("unknown order type: %v", o)
		}
	}

	printRespJSON(&poolrpc.ListOrdersResponse{Asks: asks, Bids: bids})

	return nil
}

var dumpPendingBatcheCommand = cli.Command{
	Name:      "dumppendingbatch",
	ShortName: "dpb",
	Usage: "dump the current pending batch contained in the local " +
		"database",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "db",
			Usage: "the specific pool database to use instead " +
				"of the default one on ~/.pool/<network>/" +
				"pool.db",
		},
	},
	Action: dumpPendingBatch,
}

func dumpPendingBatch(ctx *cli.Context) error {
	db, err := getPoolDB(ctx)
	if err != nil {
		return fmt.Errorf("error loading DB: %v", err)
	}

	snapshot, err := db.PendingBatchSnapshot()
	if err != nil {
		return fmt.Errorf("error getting pending batch: %v", err)
	}

	fmt.Printf("Batch ID:\t%x\n", snapshot.BatchID[:])
	fmt.Printf("TXID:\t\t%s\n", snapshot.BatchTX.TxHash())

	var buf bytes.Buffer
	err = snapshot.BatchTX.Serialize(&buf)
	if err != nil {
		return fmt.Errorf("error serializing TX: %v", err)
	}
	fmt.Printf("Raw TX:\t\t%x\n", buf.Bytes())

	return nil
}

var removePendingBatchCommand = cli.Command{
	Name:      "removependingbatch",
	ShortName: "rpb",
	Usage:     "remove the current pending batch from the local database",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "db",
			Usage: "the specific pool database to use instead " +
				"of the default one on ~/.pool/<network>/" +
				"pool.db",
		},
	},
	Action: removePendingBatch,
}

func removePendingBatch(ctx *cli.Context) error {
	db, err := getPoolDB(ctx)
	if err != nil {
		return fmt.Errorf("error loading DB: %v", err)
	}

	return db.DeletePendingBatch()
}

func getPoolDB(ctx *cli.Context) (*clientdb.DB, error) {
	fullDbPath := filepath.Join(
		pool.DefaultBaseDir, ctx.GlobalString("network"),
		clientdb.DBFilename,
	)
	if ctx.IsSet("db") {
		fullDbPath = lncfg.CleanAndExpandPath(ctx.String("db"))
	}

	db, err := clientdb.New(path.Dir(fullDbPath), path.Base(fullDbPath))
	if err != nil {
		return nil, fmt.Errorf("error opening DB at %v: %v", fullDbPath,
			err)
	}

	return db, nil
}
