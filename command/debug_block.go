package main

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/command/flagset"
	"github.com/ethereum/go-ethereum/command/server/proto"
)

// DebugBlockCommand is the command to group the peers commands
type DebugBlockCommand struct {
	*Meta2

	output string
}

// Help implements the cli.Command interface
func (c *DebugBlockCommand) Help() string {
	return `Usage: bor chain sethead <number> [--yes]

  This command sets the current chain to a certain block`
}

func (c *DebugBlockCommand) Flags() *flagset.Flagset {
	flags := c.NewFlagSet("trace")

	flags.StringFlag(&flagset.StringFlag{
		Name:  "output",
		Value: &c.output,
		Usage: "Output directory",
	})

	return flags
}

// Synopsis implements the cli.Command interface
func (c *DebugBlockCommand) Synopsis() string {
	return "Set the new head of the chain"
}

// Run implements the cli.Command interface
func (c *DebugBlockCommand) Run(args []string) int {
	flags := c.Flags()
	if err := flags.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	borClt, err := c.BorConn()
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	dEnv := &debugEnv{
		output: c.output,
		prefix: "bor-block-trace-",
	}
	if err := dEnv.init(); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	c.UI.Output("Starting block tracer...")
	c.UI.Output("")

	stream, err := borClt.DebugBlock(context.Background(), &proto.DebugBlockRequest{})
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	if err := dEnv.writeFromStream("block.json", stream); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	if err := dEnv.finish(); err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	if c.output != "" {
		c.UI.Output(fmt.Sprintf("Created debug directory: %s", dEnv.dst))
	} else {
		c.UI.Output(fmt.Sprintf("Created block trace archive: %s", dEnv.tarName()))
	}
	return 0
}
