package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/llm/clmrpc"
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
			recoverAccountsCommand,
		},
	},
}

type Account struct {
	TraderKey        string `json:"trader_key"`
	OutPoint         string `json:"outpoint"`
	Value            uint64 `json:"value"`
	ExpirationHeight uint32 `json:"expiration_height"`
	State            string `json:"state"`
	CloseTxid        string `json:"close_txid"`
}

// NewAccountFromProto creates a display Account from its proto.
func NewAccountFromProto(a *clmrpc.Account) *Account {
	var opHash, closeTxHash chainhash.Hash
	copy(opHash[:], a.Outpoint.Txid)
	copy(closeTxHash[:], a.CloseTxid)

	return &Account{
		TraderKey:        hex.EncodeToString(a.TraderKey),
		OutPoint:         fmt.Sprintf("%v:%d", opHash, a.Outpoint.OutputIndex),
		Value:            a.Value,
		ExpirationHeight: a.ExpirationHeight,
		State:            a.State.String(),
		CloseTxid:        closeTxHash.String(),
	}
}

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
			Name: "expiry",
			Usage: "the block height at which this account should " +
				"expire at",
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
	expiry, err := parseUint64(ctx, 1, "expiry", cmd)
	if err != nil {
		return err
	}

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.InitAccount(context.Background(),
		&clmrpc.InitAccountRequest{
			AccountValue:  amt,
			AccountExpiry: uint32(expiry),
		},
	)
	if err != nil {
		return err
	}

	printJSON(NewAccountFromProto(resp))

	return nil
}

var listAccountsCommand = cli.Command{
	Name:        "list",
	ShortName:   "l",
	Usage:       "list all existing accounts",
	Description: `List all existing accounts.`,
	Action:      listAccounts,
}

func listAccounts(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.ListAccounts(
		context.Background(), &clmrpc.ListAccountsRequest{},
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

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.DepositAccount(
		context.Background(), &clmrpc.DepositAccountRequest{
			TraderKey:   traderKey,
			AmountSat:   amt,
			SatPerVbyte: uint32(satPerVByte),
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

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.WithdrawAccount(
		context.Background(), &clmrpc.WithdrawAccountRequest{
			TraderKey: traderKey,
			Outputs: []*clmrpc.Output{
				{
					ValueSat: amt,
					Address:  addr,
				},
			},
			SatPerVbyte: satPerVByte,
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
	Name:        "close",
	ShortName:   "c",
	Usage:       "close an existing account",
	Description: `Close an existing accounts`,
	ArgsUsage:   "trader_key",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "trader_key",
			Usage: "the trader key associated with the account",
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

	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.CloseAccount(
		context.Background(), &clmrpc.CloseAccountRequest{
			TraderKey: traderKey,
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
		context.Background(), &clmrpc.RecoverAccountsRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
