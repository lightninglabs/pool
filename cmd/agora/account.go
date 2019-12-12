package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/agora/client/clmrpc"
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
		},
	},
}

type Account struct {
	TraderKey        string `json:"trader_key"`
	OutPoint         string `json:"outpoint"`
	Value            uint32 `json:"value"`
	ExpirationHeight uint32 `json:"expiration_height"`
	State            string `json:"state"`
}

// NewAccountFromProto creates a display Account from its proto.
func NewAccountFromProto(a *clmrpc.Account) *Account {
	var opHash chainhash.Hash
	copy(opHash[:], a.Outpoint.Txid)
	return &Account{
		TraderKey:        hex.EncodeToString(a.TraderKey),
		OutPoint:         fmt.Sprintf("%v:%d", opHash, a.Outpoint.OutputIndex),
		Value:            a.Value,
		ExpirationHeight: a.ExpirationHeight,
		State:            a.State.String(),
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
	args := ctx.Args()

	var amtStr string
	switch {
	case ctx.IsSet("amt"):
		amtStr = ctx.String("amt")
	case args.Present():
		amtStr = args.First()
		args = args.Tail()
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "new")
	}

	amt, err := parseAmt(amtStr)
	if err != nil {
		return err
	}

	var expiryStr string
	switch {
	case ctx.IsSet("expiry"):
		expiryStr = ctx.String("expiry")
	case args.Present():
		expiryStr = args.First()
		args = args.Tail()
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "new")
	}

	expiry, err := parseExpiry(expiryStr)
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
			AccountValue:  uint32(amt),
			AccountExpiry: expiry,
		},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

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
