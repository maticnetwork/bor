package main

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/command/flagset"
	"github.com/ethereum/go-ethereum/command/server/proto"
)

type AccountNewCommand struct {
	*Meta
}

// Help implements the cli.Command interface
func (a *AccountNewCommand) Help() string {
	return `Usage: bor account new

  Create a new local account.

  ` + a.Flags().Help()
}

func (a *AccountNewCommand) Flags() *flagset.Flagset {
	return a.NewFlagSet("account new")
}

// Synopsis implements the cli.Command interface
func (a *AccountNewCommand) Synopsis() string {
	return "Create a new local account"
}

// Run implements the cli.Command interface
func (a *AccountNewCommand) Run(args []string) int {
	flags := a.Flags()
	if err := flags.Parse(args); err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	borClt, err := a.BorConn()
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	password, err := a.AskPassword()
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	req := &proto.AccountsNewRequest{
		Password: password,
	}
	resp, err := borClt.AccountsNew(context.Background(), req)
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	a.UI.Output("\nYour new key was generated")
	a.UI.Output(fmt.Sprintf("Public address of the key:   %s", resp.Account.Address))
	a.UI.Output(fmt.Sprintf("Path of the secret key file: %s", resp.Account.Url))

	return 0
}
