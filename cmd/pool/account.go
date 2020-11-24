package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/urfave/cli"
)

var accountsCommands = []cli.Command{
	{
		Name:      "accounts",
		ShortName: "a",
		Usage:     "Interact with trader accounts.",
		Category:  "Accounts",
		Subcommands: []cli.Command{
			newAccountCommand,
			listAccountsCommand,
			depositAccountCommand,
			withdrawAccountCommand,
			closeAccountCommand,
			bumpAccountFeeCommand,
			recoverAccountsCommand,
		},
	},
}

type Account struct {
	TraderKey        string `json:"trader_key"`
	OutPoint         string `json:"outpoint"`
	Value            uint64 `json:"value"`
	AvailableBalance uint64 `json:"available_balance"`
	ExpirationHeight uint32 `json:"expiration_height"`
	State            string `json:"state"`
	LatestTxid       string `json:"latest_txid"`
}

// NewAccountFromProto creates a display Account from its proto.
func NewAccountFromProto(a *poolrpc.Account) *Account {
	var opHash, latestTxHash chainhash.Hash
	copy(opHash[:], a.Outpoint.Txid)
	copy(latestTxHash[:], a.LatestTxid)

	return &Account{
		TraderKey:        hex.EncodeToString(a.TraderKey),
		OutPoint:         fmt.Sprintf("%v:%d", opHash, a.Outpoint.OutputIndex),
		Value:            a.Value,
		AvailableBalance: a.AvailableBalance,
		ExpirationHeight: a.ExpirationHeight,
		State:            a.State.String(),
		LatestTxid:       latestTxHash.String(),
	}
}

const (
	accountExpiryAbsolute = "expiry_height"

	accountExpiryRelative = "expiry_blocks"

	defaultFundingConfTarget = 6
)

var newAccountCommand = cli.Command{
	Name:      "new",
	ShortName: "n",
	Usage:     "create an account",
	ArgsUsage: "amt expiry",
	Description: `
		Send the amount in satoshis specified by the amt argument to a
		new account.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "amt",
			Usage: "the amount in satoshis to create account for",
		},
		cli.Uint64Flag{
			Name: accountExpiryAbsolute,
			Usage: "the block height at which this account should " +
				"expire at",
		},
		cli.Uint64Flag{
			Name: accountExpiryRelative,
			Usage: "the relative height (from the current chain " +
				"height) that the account should expire at",
		},
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the target number of blocks the on-chain " +
				"account funding transaction should confirm " +
				"within",
			Value: defaultFundingConfTarget,
		},
		cli.BoolFlag{
			Name:  "force",
			Usage: "skip account fee confirmation",
		},
	},
	Action: newAccount,
}

func newAccount(ctx *cli.Context) error {
	cmd := "new"

	amt, err := parseUint64(ctx, 0, "amt", cmd)
	if err != nil {
		return err
	}

	req := &poolrpc.InitAccountRequest{
		AccountValue: amt,
		Fees: &poolrpc.InitAccountRequest_ConfTarget{
			ConfTarget: uint32(ctx.Uint64("conf_target")),
		},
	}

	if ctx.IsSet(accountExpiryAbsolute) && ctx.IsSet(accountExpiryRelative) {
		return fmt.Errorf("only %v or %v should be set, but not both",
			accountExpiryAbsolute, accountExpiryRelative)
	}

	if height, err := parseUint64(ctx, 1, accountExpiryAbsolute, cmd); err == nil {
		req.AccountExpiry = &poolrpc.InitAccountRequest_AbsoluteHeight{
			AbsoluteHeight: uint32(height),
		}
	} else if height, err := parseUint64(ctx, 1, accountExpiryRelative, cmd); err == nil {
		req.AccountExpiry = &poolrpc.InitAccountRequest_RelativeHeight{
			RelativeHeight: uint32(height),
		}
	} else {
		return fmt.Errorf("either a relative or absolute height must " +
			"be specified")
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// In interactive mode, get a quote for the on-chain fees first and
	// present it to the user.
	if !ctx.Bool("force") {
		if err := printAccountFees(
			client, btcutil.Amount(amt),
			uint32(ctx.Uint64("conf_target")),
		); err != nil {
			return err
		}

		if !promptForConfirmation("Confirm account (yes/no): ") {
			fmt.Println("Cancelling account...")
			return nil
		}
	}

	resp, err := client.InitAccount(context.Background(), req)
	if err != nil {
		return err
	}

	printJSON(NewAccountFromProto(resp))

	return nil
}

func printAccountFees(client poolrpc.TraderClient, amt btcutil.Amount,
	confTarget uint32) error {

	req := &poolrpc.QuoteAccountRequest{
		AccountValue: uint64(amt),
		Fees: &poolrpc.QuoteAccountRequest_ConfTarget{
			ConfTarget: confTarget,
		},
	}
	resp, err := client.QuoteAccount(context.Background(), req)
	if err != nil {
		return fmt.Errorf("unable to estimate on-chain fees: "+
			"%v", err)
	}

	feeRate := chainfee.SatPerKWeight(resp.MinerFeeRateSatPerKw)
	satPerVByte := float64(feeRate.FeePerKVByte()) / 1000

	fmt.Println("-- Account Funding Details --")
	fmt.Printf("Amount: %v\n", amt)
	fmt.Printf("Confirmation target: %v blocks\n", confTarget)
	fmt.Printf("Fee rate (estimated): %.1f sat/vByte\n", satPerVByte)
	fmt.Printf("Total miner fee (estimated): %v\n",
		btcutil.Amount(resp.MinerFeeTotal))

	return nil
}

var listAccountsCommand = cli.Command{
	Name:        "list",
	ShortName:   "l",
	Usage:       "list all existing accounts",
	Description: `List all existing accounts.`,
	Action:      listAccounts,
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "show_archived",
			Usage: "include accounts that are no longer active",
		},
	},
}

func listAccounts(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	// Default to only showing active accounts.
	activeOnly := true
	if ctx.Bool("show_archived") {
		activeOnly = false
	}

	resp, err := client.ListAccounts(
		context.Background(), &poolrpc.ListAccountsRequest{
			ActiveOnly: activeOnly,
		},
	)
	if err != nil {
		return err
	}

	var listAccountsResp = struct {
		Accounts []*Account `json:"accounts"`
	}{
		Accounts: make([]*Account, 0, len(resp.Accounts)),
	}
	for _, protoAccount := range resp.Accounts {
		a := NewAccountFromProto(protoAccount)
		listAccountsResp.Accounts = append(listAccountsResp.Accounts, a)
	}

	printJSON(listAccountsResp)

	return nil
}

var depositAccountCommand = cli.Command{
	Name:      "deposit",
	ShortName: "d",
	Usage:     "deposit funds into an existing account",
	Description: `
	Deposit funds into an existing account.
	`,
	ArgsUsage: "trader_key amt sat_per_vbyte",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "trader_key",
			Usage: "the hex-encoded trader key of the account to " +
				"deposit funds into",
		},
		cli.Uint64Flag{
			Name:  "amt",
			Usage: "the amount to deposit into the account",
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "the fee rate expressed in sat/vbyte that " +
				"should be used for the withdrawal",
		},
	},
	Action: depositAccount,
}

func depositAccount(ctx *cli.Context) error {
	cmd := "deposit"
	traderKey, err := parseHexStr(ctx, 0, "trader_key", cmd)
	if err != nil {
		return err
	}
	amt, err := parseUint64(ctx, 1, "amt", cmd)
	if err != nil {
		return err
	}
	satPerVByte, err := parseUint64(ctx, 2, "sat_per_vbyte", cmd)
	if err != nil {
		return err
	}

	// Enforce a minimum fee rate of 253 sat/kw by rounding up if 1
	// sat/byte is used.
	feeRate := chainfee.SatPerKVByte(satPerVByte * 1000).FeePerKWeight()
	if feeRate < chainfee.FeePerKwFloor {
		feeRate = chainfee.FeePerKwFloor
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.DepositAccount(
		context.Background(), &poolrpc.DepositAccountRequest{
			TraderKey:       traderKey,
			AmountSat:       amt,
			FeeRateSatPerKw: uint64(feeRate),
		},
	)
	if err != nil {
		return err
	}

	var depositTxid chainhash.Hash
	copy(depositTxid[:], resp.DepositTxid)

	var depositAccountResp = struct {
		Account     *Account `json:"account"`
		DepositTxid string   `json:"deposit_txid"`
	}{
		Account:     NewAccountFromProto(resp.Account),
		DepositTxid: depositTxid.String(),
	}

	printJSON(depositAccountResp)

	return nil
}

var withdrawAccountCommand = cli.Command{
	Name:      "withdraw",
	ShortName: "w",
	Usage:     "withdraw funds from an existing account",
	Description: `
	Withdraw funds from an existing account to a supported address.
	`,
	ArgsUsage: "trader_key amt addr sat_per_vbyte",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "trader_key",
			Usage: "the hex-encoded trader key of the account to " +
				"withdraw funds from",
		},
		cli.StringFlag{
			Name:  "addr",
			Usage: "the address the withdrawn funds should go to",
		},
		cli.Uint64Flag{
			Name: "amt",
			Usage: "the amount that will be sent to the address " +
				"and withdrawn from the account",
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "the fee rate expressed in sat/vbyte that " +
				"should be used for the withdrawal",
		},
	},
	Action: withdrawAccount,
}

func withdrawAccount(ctx *cli.Context) error {
	cmd := "withdraw"
	traderKey, err := parseHexStr(ctx, 0, "trader_key", cmd)
	if err != nil {
		return err
	}
	amt, err := parseUint64(ctx, 1, "amt", cmd)
	if err != nil {
		return err
	}
	addr, err := parseStr(ctx, 2, "addr", cmd)
	if err != nil {
		return err
	}
	satPerVByte, err := parseUint64(ctx, 3, "sat_per_vbyte", cmd)
	if err != nil {
		return err
	}

	// Enforce a minimum fee rate of 253 sat/kw by rounding up if 1
	// sat/byte is used.
	feeRate := chainfee.SatPerKVByte(satPerVByte * 1000).FeePerKWeight()
	if feeRate < chainfee.FeePerKwFloor {
		feeRate = chainfee.FeePerKwFloor
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.WithdrawAccount(
		context.Background(), &poolrpc.WithdrawAccountRequest{
			TraderKey: traderKey,
			Outputs: []*poolrpc.Output{
				{
					ValueSat: amt,
					Address:  addr,
				},
			},
			FeeRateSatPerKw: uint64(feeRate),
		},
	)
	if err != nil {
		return err
	}

	var withdrawTxid chainhash.Hash
	copy(withdrawTxid[:], resp.WithdrawTxid)

	var withdrawAccountResp = struct {
		Account      *Account `json:"account"`
		WithdrawTxid string   `json:"withdraw_txid"`
	}{
		Account:      NewAccountFromProto(resp.Account),
		WithdrawTxid: withdrawTxid.String(),
	}

	printJSON(withdrawAccountResp)

	return nil
}

var closeAccountCommand = cli.Command{
	Name:      "close",
	ShortName: "c",
	Usage:     "close an existing account",
	Description: `
	Close an existing account. An optional address can be provided which the
	funds of the account to close will be sent to, otherwise they are sent
	to an address under control of the connected lnd node.
	`,
	ArgsUsage: "trader_key sat_per_vbyte",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "trader_key",
			Usage: "the trader key associated with the account " +
				"to close",
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "the fee rate expressed in sat/vbyte that " +
				"should be used for the closing transaction",
		},
		cli.StringFlag{
			Name: "addr",
			Usage: "an optional address which the funds of the " +
				"account to close will be sent to",
		},
	},
	Action: closeAccount,
}

func closeAccount(ctx *cli.Context) error {
	cmd := "close"
	traderKey, err := parseHexStr(ctx, 0, "trader_key", cmd)
	if err != nil {
		return err
	}
	satPerVByte, err := parseUint64(ctx, 1, "sat_per_vbyte", cmd)
	if err != nil {
		return err
	}
	satPerKw := chainfee.SatPerKVByte(satPerVByte * 1000).FeePerKWeight()
	if satPerKw < chainfee.FeePerKwFloor {
		satPerKw = chainfee.FeePerKwFloor
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.CloseAccount(
		context.Background(), &poolrpc.CloseAccountRequest{
			TraderKey: traderKey,
			FundsDestination: &poolrpc.CloseAccountRequest_OutputWithFee{
				OutputWithFee: &poolrpc.OutputWithFee{
					Address: ctx.String("addr"),
					Fees: &poolrpc.OutputWithFee_FeeRateSatPerKw{
						FeeRateSatPerKw: uint64(satPerKw),
					},
				},
			},
		},
	)
	if err != nil {
		return err
	}

	var closeTxid chainhash.Hash
	copy(closeTxid[:], resp.CloseTxid)

	closeAccountResp := struct {
		CloseTxid string `json:"close_txid"`
	}{
		CloseTxid: closeTxid.String(),
	}

	printJSON(closeAccountResp)

	return nil
}

var bumpAccountFeeCommand = cli.Command{
	Name:      "bumpfee",
	ShortName: "b",
	Usage:     "bump the fee of an account in a pending state",
	Description: `
	This command allows users to bump the fee of an account's unconfirmed
	transaction through child-pays-for-parent (CPFP). Since the CPFP is
	performed through the backing lnd node, the account transaction must
	contain an output under its control for a successful bump. If a CPFP has
	already been performed for an account, and this RPC is invoked again,
	then a replacing transaction (RBF) of the child will be broadcast.
	`,
	ArgsUsage: "trader_key sat_per_vbyte",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "trader_key",
			Usage: "the trader key associated with the account to " +
				"bump the fee of",
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "the fee rate expressed in sat/vbyte that " +
				"should be used for the CPFP",
		},
	},
	Action: bumpAccountFee,
}

func bumpAccountFee(ctx *cli.Context) error {
	cmd := "bumpfee"
	traderKey, err := parseHexStr(ctx, 0, "trader_key", cmd)
	if err != nil {
		return err
	}
	satPerVbyte, err := parseUint64(ctx, 1, "sat_per_vbyte", cmd)
	if err != nil {
		return err
	}
	satPerKw := chainfee.SatPerKVByte(satPerVbyte * 1000).FeePerKWeight()

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.BumpAccountFee(
		context.Background(), &poolrpc.BumpAccountFeeRequest{
			TraderKey:       traderKey,
			FeeRateSatPerKw: uint64(satPerKw),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}

var recoverAccountsCommand = cli.Command{
	Name:      "recover",
	ShortName: "r",
	Usage: "recover accounts after data loss with the help of the " +
		"auctioneer",
	Description: `
	In case the data directory of the trader was corrupted or lost, this
	command can be used to ask the auction server to send back its view of
	the trader's accounts. This is possible as long as the connected lnd
	node is running with the same seed as when the accounts to recover were
	first created.

	NOTE: This command should only be used after data loss as it will fail
	if there already are open accounts in the trader's database.
	All open or pending orders of any recovered account will be canceled on
	the auctioneer's side and won't be restored in the trader's database.
	`,
	Action: recoverAccounts,
}

func recoverAccounts(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.RecoverAccounts(
		context.Background(), &poolrpc.RecoverAccountsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
