package main

import (
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/command/flagset"
	"github.com/ethereum/go-ethereum/command/server"
	"github.com/ethereum/go-ethereum/command/server/proto"
	"github.com/mitchellh/cli"
	"github.com/ryanuber/columnize"
	"google.golang.org/grpc"
)

func main() {
	os.Exit(Run(os.Args[1:]))
}

func Run(args []string) int {
	commands := commands()

	cli := &cli.CLI{
		Name:     "bor",
		Args:     args,
		Commands: commands,
	}

	exitCode, err := cli.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing CLI: %s\n", err.Error())
		return 1
	}
	return exitCode
}

func commands() map[string]cli.CommandFactory {
	ui := &cli.BasicUi{
		Reader:      os.Stdin,
		Writer:      os.Stdout,
		ErrorWriter: os.Stderr,
	}

	meta := &Meta{
		UI: ui,
	}
	return map[string]cli.CommandFactory{
		"server": func() (cli.Command, error) {
			return &server.Command{
				UI: ui,
			}, nil
		},
		"version": func() (cli.Command, error) {
			return &VersionCommand{
				UI: ui,
			}, nil
		},
		"debug": func() (cli.Command, error) {
			return &DebugCommand{
				Meta: meta,
			}, nil
		},
		"account": func() (cli.Command, error) {
			return &Account{
				UI: ui,
			}, nil
		},
		"account new": func() (cli.Command, error) {
			return &AccountNewCommand{
				Meta: meta,
			}, nil
		},
		"account import": func() (cli.Command, error) {
			return &AccountImportCommand{
				Meta: meta,
			}, nil
		},
		"account list": func() (cli.Command, error) {
			return &AccountListCommand{
				Meta: meta,
			}, nil
		},
		"peers": func() (cli.Command, error) {
			return &PeersCommand{
				UI: ui,
			}, nil
		},
		"peers add": func() (cli.Command, error) {
			return &PeersAddCommand{
				Meta: meta,
			}, nil
		},
		"peers remove": func() (cli.Command, error) {
			return &PeersRemoveCommand{
				Meta: meta,
			}, nil
		},
		"peers list": func() (cli.Command, error) {
			return &PeersListCommand{
				Meta: meta,
			}, nil
		},
		"peers status": func() (cli.Command, error) {
			return &PeersStatusCommand{
				Meta: meta,
			}, nil
		},
	}
}

type Meta struct {
	UI cli.Ui

	addr string
}

func (m *Meta) NewFlagSet(n string) *flagset.Flagset {
	f := flagset.NewFlagSet(n)

	f.StringFlag(&flagset.StringFlag{
		Name:    "address",
		Value:   &m.addr,
		Usage:   "Address of the grpc endpoint",
		Default: "127.0.0.1:3131",
	})
	return f
}

func (m *Meta) Conn() (*grpc.ClientConn, error) {
	conn, err := grpc.Dial(m.addr, grpc.WithInsecure())
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %v", err)
	}
	return conn, nil
}

func (m *Meta) BorConn() (proto.BorClient, error) {
	conn, err := m.Conn()
	if err != nil {
		return nil, err
	}
	return proto.NewBorClient(conn), nil
}

func (m *Meta) AskPassword() (string, error) {
	return m.UI.AskSecret("Your new account is locked with a password. Please give a password. Do not forget this password")
}

func formatList(in []string) string {
	columnConf := columnize.DefaultConfig()
	columnConf.Empty = "<none>"
	return columnize.Format(in, columnConf)
}

func formatKV(in []string) string {
	columnConf := columnize.DefaultConfig()
	columnConf.Empty = "<none>"
	columnConf.Glue = " = "
	return columnize.Format(in, columnConf)
}
