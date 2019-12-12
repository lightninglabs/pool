package main

import (
	"context"

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
		},
	},
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
