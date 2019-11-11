package main

import (
	"context"
	"fmt"

	"github.com/lightninglabs/agora/client/clmrpc"
	"github.com/urfave/cli"
)

var initAccountCommand = cli.Command{
	Name:      "initaccount",
	Usage:     "create an account",
	ArgsUsage: "amt",
	Description: `
		Send the amount in satoshis specified by the amt argument to a
		new account.`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "amt",
			Usage: "the amount in satoshis to create account for",
		},
	},
	Action: initAccount,
}

func initAccount(ctx *cli.Context) error {
	args := ctx.Args()

	var amtStr string
	switch {
	case ctx.IsSet("amt"):
		amtStr = ctx.String("amt")
	case ctx.NArg() > 0:
		amtStr = args[0]
		args = args.Tail()
	default:
		// Show command help if no arguments and flags were provided.
		return cli.ShowCommandHelp(ctx, "initaccount")
	}

	amt, err := parseAmt(amtStr)
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
			AccountValue:    int64(amt),
			AccountDuration: 0,
		},
	)
	if err != nil {
		return err
	}

	fmt.Printf("Account created.\n")
	printRespJSON(resp)

	return nil
}
