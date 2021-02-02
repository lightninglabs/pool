package main

import (
	"context"

	"github.com/lightninglabs/pool/poolrpc"
	"github.com/urfave/cli"
)

var getInfoCommand = cli.Command{
	Name:  "getinfo",
	Usage: "show info about the daemon's current state",
	Description: "Displays basic info about the current state of the Pool " +
		"trader daemon",
	Action: getInfo,
}

func getInfo(ctx *cli.Context) error {
	client, cleanup, err := getClient(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	resp, err := client.GetInfo(
		context.Background(), &poolrpc.GetInfoRequest{},
	)
	if err != nil {
		return err
	}

	printRespJSON(resp)

	return nil
}
