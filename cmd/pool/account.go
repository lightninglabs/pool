package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/pool/auctioneer"
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
			renewAccountCommand,
			closeAccountCommand,
			listAccountFeesCommand,
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
	Version          string `json:"version"`
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
		Version:          a.Version.String(),
	}
}

const (
	accountExpiryAbsolute = "expiry_height"

	accountExpiryRelative = "expiry_blocks"

	// Equivalent to approx. 30d in blocks.
	defaultExpiryRelative = 30 * 144

	defaultFundingConfTarget = 6

	// defaultAccountTarget is the default number of accounts that will try
	// to recreate in the account recovery process. We set it to the same
	// number that is used to differentiate between a "hole" in the keys
	// resulting from failed attempts and there not being any keys at all.
	// This means, if a user requires more than 50 attempts before the first
	// account is created, they will need to override this value in the
	// client. But we need a safeguard here to not bombard our server with
	// attempts.
	defaultAccountTarget = auctioneer.MaxUnusedAccountKeyLookup
)

var (
	expiryAbsoluteFlag = cli.Uint64Flag{
		Name: accountExpiryAbsolute,
		Usage: "the new block height which this account " +
			"should expire at",
	}
)

var newAccountCommand = cli.Command{
	Name:      "new",
	ShortName: "n",
	Usage:     "create an account",
	ArgsUsage: "amt [--expiry_height | --expiry_blocks]",
	Description: `
		Send the amount in satoshis specified by the amt argument to a
		new account.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "amt",
			Usage: "the amount in satoshis to create account for",
		},
		expiryAbsoluteFlag,
		cli.Uint64Flag{
			Name: accountExpiryRelative,
			Usage: "the relative height (from the current chain " +
				"height) that the account should expire at " +
				"(default of 30 days equivalent in blocks)",
			Value: defaultExpiryRelative,
		},
		cli.Uint64Flag{
			Name: "conf_target",
			Usage: "the target number of blocks the on-chain " +
				"account funding transaction should confirm " +
				"within",
			Value: defaultFundingConfTarget,
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "the fee rate expressed in sat/vbyte that " +
				"should be used for the account creation " +
				"transaction",
		},
		cli.BoolFlag{
			Name:  "force",
			Usage: "skip account fee confirmation",
		},
		cli.Uint64Flag{
			Name: "version",
			Usage: "the account version to use; version 0 means " +
				"auto-decide dependent on lnd version; " +
				"version 1 is a p2wsh account, version 2 is " +
				"a p2tr account, version 3 is a p2tr " +
				"account with MuSig2 v1.0.0-rc2 support; " +
				"version 2 requires lnd 0.15 or " +
				"later, version 3 requires lnd 0.16 or later",
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
		Initiator:    defaultInitiator,
		Version:      poolrpc.AccountVersion(ctx.Uint64("version")),
	}

	satPerVByte := ctx.Uint64("sat_per_vbyte")
	confTarget := ctx.Uint64("conf_target")

	switch {
	case satPerVByte > 0:
		// If the user specified `sat_per_vbyte` we do not need to use the
		// default confTarget.
		confTarget = 0

		// Enforce a minimum fee rate of 253 sat/kw by rounding up if 1
		// sat/byte is used.
		feeRate := chainfee.SatPerKVByte(satPerVByte * 1000).FeePerKWeight()
		if feeRate < chainfee.FeePerKwFloor {
			feeRate = chainfee.FeePerKwFloor
		}
		req.Fees = &poolrpc.InitAccountRequest_FeeRateSatPerKw{
			FeeRateSatPerKw: uint64(feeRate),
		}

	case ctx.IsSet("conf_target"):
		if confTarget == 0 {
			return fmt.Errorf("specified confirmation target must be " +
				"greater than 0")
		}

		req.Fees = &poolrpc.InitAccountRequest_ConfTarget{
			ConfTarget: uint32(ctx.Uint64("conf_target")),
		}

	default:
		return fmt.Errorf("either sat/vbyte or confirmation target " +
			"must be specified")
	}

	// Parse the expiry in either of its forms. We'll always prefer the
	// absolute expiry over the relative as the relative has a default value
	// present.
	switch {
	case ctx.Uint64(accountExpiryAbsolute) != 0:
		req.AccountExpiry = &poolrpc.InitAccountRequest_AbsoluteHeight{
			AbsoluteHeight: uint32(ctx.Uint64(accountExpiryAbsolute)),
		}

	default:
		req.AccountExpiry = &poolrpc.InitAccountRequest_RelativeHeight{
			RelativeHeight: uint32(ctx.Uint64(accountExpiryRelative)),
		}
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
			client,
			btcutil.Amount(amt),
			satPerVByte,
			uint32(confTarget),
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
	satPerVByte uint64, confTarget uint32) error {

	if confTarget == 0 {
		fmt.Println("-- Account Funding Details --")
		fmt.Printf("Amount: %v\n", amt)
		fmt.Printf("Fee rate (estimated): %d sat/vByte\n", satPerVByte)

		return nil
	}

	resp, err := client.QuoteAccount(
		context.Background(),
		&poolrpc.QuoteAccountRequest{
			AccountValue: uint64(amt),
			Fees: &poolrpc.QuoteAccountRequest_ConfTarget{
				ConfTarget: confTarget,
			},
		},
	)
	if err != nil {
		return fmt.Errorf("unable to estimate on-chain fees: "+
			"%v", err)
	}

	feeRate := chainfee.SatPerKWeight(resp.MinerFeeRateSatPerKw)
	feePerVByte := float64(feeRate.FeePerKVByte()) / 1000
	fmt.Println("-- Account Funding Details --")
	fmt.Printf("Amount: %v\n", amt)
	fmt.Printf("Confirmation target: %v blocks\n", confTarget)
	fmt.Printf("Fee rate (estimated): %.1f sat/vByte\n", feePerVByte)
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

var renewAccountCommand = cli.Command{
	Name:      "renew",
	ShortName: "r",
	Usage:     "renew the expiration of an account",
	Description: `
	Renews the expiration of an account. This will broadcast a chain
	transaction as the expiry is enforced within the account output script.
	The fee rate of said transaction must be specified.`,
	ArgsUsage: "trader_key sat_per_vbyte [--expiry_height | --expiry_blocks]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "trader_key",
			Usage: "the hex-encoded trader key of the account to " +
				"renew",
		},
		cli.Uint64Flag{
			Name: "sat_per_vbyte",
			Usage: "the fee rate expressed in sat/vbyte that " +
				"should be used for the renewal transaction",
		},
		expiryAbsoluteFlag,
		cli.Uint64Flag{
			Name: accountExpiryRelative,
			Usage: "the new relative height (from the current " +
				"chain height) that the account should expire " +
				"at (default of 30 days equivalent in blocks)",
			Value: defaultExpiryRelative,
		},
		cli.Uint64Flag{
			Name: "new_version",
			Usage: "the new account version to use; version 0 " +
				"means auto-decide dependent on lnd version; " +
				"version 1 is a p2wsh account while version " +
				"2 is a p2tr account; version 2 requires lnd " +
				"0.15 or later",
		},
	},
	Action: renewAccount,
}

func renewAccount(ctx *cli.Context) error {
	cmd := "renew"
	traderKey, err := parseHexStr(ctx, 0, "trader_key", cmd)
	if err != nil {
		return err
	}
	satPerVByte, err := parseUint64(ctx, 1, "sat_per_vbyte", cmd)
	if err != nil {
		return err
	}

	// Enforce a minimum fee rate of 253 sat/kw by rounding up if 1
	// sat/byte is used.
	feeRate := chainfee.SatPerKVByte(satPerVByte * 1000).FeePerKWeight()
	if feeRate < chainfee.FeePerKwFloor {
		feeRate = chainfee.FeePerKwFloor
	}

	// Parse the expiry in either of its forms. We'll always prefer the
	// absolute expiry over the relative as the relative has a default value
	// present.
	req := &poolrpc.RenewAccountRequest{
		AccountKey:      traderKey,
		FeeRateSatPerKw: uint64(feeRate),
		NewVersion: poolrpc.AccountVersion(
			ctx.Int64("new_version"),
		),
	}
	switch {
	case ctx.Uint64(accountExpiryAbsolute) != 0:
		req.AccountExpiry = &poolrpc.RenewAccountRequest_AbsoluteExpiry{
			AbsoluteExpiry: uint32(ctx.Uint64(accountExpiryAbsolute)),
		}

	default:
		req.AccountExpiry = &poolrpc.RenewAccountRequest_RelativeExpiry{
			RelativeExpiry: uint32(ctx.Uint64(accountExpiryRelative)),
		}
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.RenewAccount(context.Background(), req)
	if err != nil {
		return err
	}

	var renewalTxid chainhash.Hash
	copy(renewalTxid[:], resp.RenewalTxid)

	var renewResp = struct {
		Account     *Account `json:"account"`
		RenewalTxid string   `json:"renewal_txid"`
	}{
		Account:     NewAccountFromProto(resp.Account),
		RenewalTxid: renewalTxid.String(),
	}

	printJSON(renewResp)

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
				"should be used for the deposit",
		},
		expiryAbsoluteFlag,
		cli.Uint64Flag{
			Name: accountExpiryRelative,
			Usage: "the new relative height (from the current " +
				"chain height) that the account should expire " +
				"at",
		},
		cli.Uint64Flag{
			Name: "new_version",
			Usage: "the new account version to use; version 0 " +
				"means auto-decide dependent on lnd version; " +
				"version 1 is a p2wsh account while version " +
				"2 is a p2tr account; version 2 requires lnd " +
				"0.15 or later",
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

	req := &poolrpc.DepositAccountRequest{
		TraderKey:       traderKey,
		AmountSat:       amt,
		FeeRateSatPerKw: uint64(feeRate),
		NewVersion: poolrpc.AccountVersion(
			ctx.Int64("new_version"),
		),
	}

	absoluteExpiry := ctx.Uint64(accountExpiryAbsolute)
	relativeExpiry := ctx.Uint64(accountExpiryRelative)
	switch {
	case absoluteExpiry != 0 && relativeExpiry != 0:
		return errors.New("relative and absolute height cannot be " +
			"set in the same request")

	case absoluteExpiry != 0:
		req.AccountExpiry = &poolrpc.DepositAccountRequest_AbsoluteExpiry{
			AbsoluteExpiry: uint32(absoluteExpiry),
		}

	case relativeExpiry != 0:
		req.AccountExpiry = &poolrpc.DepositAccountRequest_RelativeExpiry{
			RelativeExpiry: uint32(relativeExpiry),
		}
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.DepositAccount(context.Background(), req)
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
		expiryAbsoluteFlag,
		cli.Uint64Flag{
			Name: accountExpiryRelative,
			Usage: "the new relative height (from the current " +
				"chain height) that the account should expire " +
				"at",
		},
		cli.Uint64Flag{
			Name: "new_version",
			Usage: "the new account version to use; version 0 " +
				"means auto-decide dependent on lnd version; " +
				"version 1 is a p2wsh account while version " +
				"2 is a p2tr account; version 2 requires lnd " +
				"0.15 or later",
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

	req := &poolrpc.WithdrawAccountRequest{
		TraderKey: traderKey,
		Outputs: []*poolrpc.Output{
			{
				ValueSat: amt,
				Address:  addr,
			},
		},
		FeeRateSatPerKw: uint64(feeRate),
		NewVersion: poolrpc.AccountVersion(
			ctx.Int64("new_version"),
		),
	}

	absoluteExpiry := ctx.Uint64(accountExpiryAbsolute)
	relativeExpiry := ctx.Uint64(accountExpiryRelative)
	switch {
	case absoluteExpiry != 0 && relativeExpiry != 0:
		return errors.New("relative and absolute height cannot be " +
			"set in the same request")

	case absoluteExpiry != 0:
		req.AccountExpiry = &poolrpc.WithdrawAccountRequest_AbsoluteExpiry{
			AbsoluteExpiry: uint32(absoluteExpiry),
		}

	case relativeExpiry != 0:
		req.AccountExpiry = &poolrpc.WithdrawAccountRequest_RelativeExpiry{
			RelativeExpiry: uint32(relativeExpiry),
		}
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.WithdrawAccount(context.Background(), req)
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

var listAccountFeesCommand = cli.Command{
	Name:      "listfees",
	ShortName: "f",
	Usage:     "list the account modification transaction fees",
	Description: `
	This command prints a map from account key to an ordered list of account
	modification transaction fees.
	`,
	Action: listAccountFees,
}

func listAccountFees(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.AccountModificationFees(
		context.Background(), &poolrpc.AccountModificationFeesRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var recoverAccountsCommand = cli.Command{
	Name: "recover",
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
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name: "full_client",
			Usage: "fetch the latest account state from the " +
				"server. If false, the full process will" +
				"run locally, but it could take hours.",
		},
		cli.Uint64Flag{
			Name: "account_target",
			Usage: fmt.Sprintf("number of accounts that we are "+
				"looking to recover, set higher if there were "+
				"more than %d accounts or attempts at "+
				"creating accounts", defaultAccountTarget),
			Value: defaultAccountTarget,
		},
		cli.StringFlag{
			Name: "auctioneer_key",
			Usage: "Auctioneer's public key. Only used during " +
				"`full_client` recoveries",
		},
		cli.Uint64Flag{
			Name: "height_hint",
			Usage: "initial block height. Only used during " +
				"`full_client` recoveries",
		},
		cli.StringFlag{
			Name: "bitcoin_host",
			Usage: "bitcoind/btcd instance address. Only used " +
				"during `full_client` recoveries",
		},
		cli.StringFlag{
			Name: "bitcoin_user",
			Usage: "bitcoind/btcd user name. Only used during " +
				"`full_client` recoveries",
		},
		cli.StringFlag{
			Name: "bitcoin_password",
			Usage: "bitcoind/btcd password. Only used during " +
				"`full_client` recoveries",
		},
		cli.BoolFlag{
			Name: "bitcoin_httppostmode",
			Usage: "Use HTTP POST mode? bitcoind only supports " +
				"this mode. Only used during `full_client` " +
				"recoveries",
		},
		cli.BoolFlag{
			Name: "bitcoin_usetls",
			Usage: "Use TLS to connect? bitcoind only supports  " +
				"non-TLS connections. Only used during " +
				"`full_client` recoveries",
		},
		cli.StringFlag{
			Name: "bitcoin_tlspath",
			Usage: "Path to btcd's TLS certificate, if TLS is " +
				"enabled. Only used during `full_client` " +
				"recoveries",
		},
	},
	Action: recoverAccounts,
}

func recoverAccounts(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.RecoverAccounts(
		context.Background(), &poolrpc.RecoverAccountsRequest{
			FullClient:    ctx.Bool("full_client"),
			AccountTarget: uint32(ctx.Uint64("account_target")),
			AuctioneerKey: ctx.String("auctioneer_key"),
			HeightHint:    uint32(ctx.Uint64("height_hint")),

			// bitcoind/btcd configuration, ignored unless
			// full_client is true.
			BitcoinHost:         ctx.String("bitcoin_host"),
			BitcoinUser:         ctx.String("bitcoin_user"),
			BitcoinPassword:     ctx.String("bitcoin_password"),
			BitcoinHttppostmode: ctx.Bool("bitcoin_httppostmode"),
			BitcoinUsetls:       ctx.Bool("bitcoin_usetls"),
			BitcoinTlspath:      ctx.String("bitcoin_tlspath"),
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
