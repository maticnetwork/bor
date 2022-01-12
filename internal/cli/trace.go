package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ethereum/go-ethereum/command/flagset"
	"github.com/ethereum/go-ethereum/command/server/proto"
)

// TraceCommand is the command to group the peers commands
type TraceCommand struct {
	*Meta2

	yes bool
}

// Help implements the cli.Command interface
func (c *TraceCommand) Help() string {
	return `Usage: bor chain sethead <number> [--yes]

  This command sets the current chain to a certain block`
}

func (c *TraceCommand) Flags() *flagset.Flagset {
	flags := c.NewFlagSet("trace")

	flags.BoolFlag(&flagset.BoolFlag{
		Name:    "yes",
		Usage:   "Force set head",
		Default: false,
		Value:   &c.yes,
	})
	return flags
}

// Synopsis implements the cli.Command interface
func (c *TraceCommand) Synopsis() string {
	return "Set the new head of the chain"
}

// Run implements the cli.Command interface
func (c *TraceCommand) Run(args []string) int {
	flags := c.Flags()
	if err := flags.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	args = flags.Args()
	if len(args) != 1 {
		c.UI.Error("No number provided")
		return 1
	}

	borClt, err := c.BorConn()
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	arg := args[0]
	fmt.Println(arg)

	fmt.Println(borClt)

	num, err := strconv.Atoi(arg)
	if err != nil {
		panic(err)
	}

	borClt.TraceBlock(context.Background(), &proto.TraceRequest{Number: int64(num)})

	c.UI.Output("Done!")
	return 0
}
