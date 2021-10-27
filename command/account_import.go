package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/ethereum/go-ethereum/command/flagset"
	"github.com/ethereum/go-ethereum/command/server/proto"
	"github.com/ethereum/go-ethereum/crypto"
)

type AccountImportCommand struct {
	*Meta
}

// Help implements the cli.Command interface
func (a *AccountImportCommand) Help() string {
	return `Usage: bor account import

  Import a private key into a new account.

  Import an account:

    $ bor account import key.json

  ` + a.Flags().Help()
}

func (a *AccountImportCommand) Flags() *flagset.Flagset {
	return a.NewFlagSet("account import")
}

// Synopsis implements the cli.Command interface
func (a *AccountImportCommand) Synopsis() string {
	return "Import a private key into a new account"
}

// Run implements the cli.Command interface
func (a *AccountImportCommand) Run(args []string) int {
	flags := a.Flags()
	if err := flags.Parse(args); err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		a.UI.Error("Expected one argument")
		return 1
	}
	// make sure it is a valid ecdsa key
	key, err := crypto.LoadECDSA(args[0])
	if err != nil {
		a.UI.Error(fmt.Sprintf("Failed to load the private key '%s': %v", args[0], err))
		return 1
	}
	password, err := a.AskPassword()
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	borClt, err := a.BorConn()
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	req := &proto.AccountsImportRequest{
		Key:      hex.EncodeToString(crypto.FromECDSA(key)),
		Password: password,
	}
	resp, err := borClt.AccountsImport(context.Background(), req)
	if err != nil {
		a.UI.Error(err.Error())
		return 1
	}

	a.UI.Output(fmt.Sprintf("Account created: %s", resp.Account.Address))
	return 0
}
