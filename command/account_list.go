package main

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/command/flagset"
	"github.com/ethereum/go-ethereum/command/server/proto"
)

type AccountListCommand struct {
	*Meta
}

// Help implements the cli.Command interface
func (a *AccountListCommand) Help() string {
	return `Usage: bor account list

  List the local accounts.

  ` + a.Flags().Help()
}

func (a *AccountListCommand) Flags() *flagset.Flagset {
	return a.NewFlagSet("account list")
}

// Synopsis implements the cli.Command interface
func (a *AccountListCommand) Synopsis() string {
	return "List the local accounts"
}

// Run implements the cli.Command interface
func (a *AccountListCommand) Run(args []string) int {
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

	resp, err := borClt.AccountsList(context.Background(), &proto.AccountsListRequest{})
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	a.UI.Output(formatAccounts(resp.Accounts))
	return 0
}

func formatAccounts(accts []*proto.Account) string {
	if len(accts) == 0 {
		return "No accounts found"
	}

	rows := make([]string, len(accts)+1)
	rows[0] = "Index|Address"
	for i, d := range accts {
		rows[i+1] = fmt.Sprintf("%d|%s",
			i,
			d.Address)
	}
	return formatList(rows)
}
