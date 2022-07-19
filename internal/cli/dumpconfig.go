package cli

import (
	"math/big"
	"reflect"
	"strings"
	"time"

	"github.com/naoina/toml"

	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/internal/cli/server"
	"github.com/ethereum/go-ethereum/params"
)

// These settings ensure that TOML keys use the same names as Go struct fields.
var tomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string {
		return key
	},
	FieldToKey: func(rt reflect.Type, field string) string {
		return field
	},
}

// DumpconfigCommand is for exporting user provided flags into a config file
type DumpconfigCommand struct {
	*Meta2
}

// MarkDown implements cli.MarkDown interface
func (p *DumpconfigCommand) MarkDown() string {
	items := []string{
		"# Dumpconfig",
		"The ```bor dumpconfig <your-favourite-flags>``` command will export the user provided flags into a configuration file",
	}

	return strings.Join(items, "\n\n")
}

// Help implements the cli.Command interface
func (c *DumpconfigCommand) Help() string {
	return `Usage: bor dumpconfig <your-favourite-flags>

  This command will will export the user provided flags into a configuration file`
}

// Synopsis implements the cli.Command interface
func (c *DumpconfigCommand) Synopsis() string {
	return "Export configuration file"
}

// TODO: add flags for file location and format (toml, json, hcl) of the configuration file.

// Run implements the cli.Command interface
func (c *DumpconfigCommand) Run(args []string) int {
	// Initialize an empty command instance to get flags
	command := server.Command{}
	flags := command.Flags()

	if err := flags.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	userConfig := command.GetConfig()

	// setting the default values to the raw fields
	userConfig.TxPool.RejournalRaw = (1 * time.Hour).String()
	userConfig.TxPool.LifeTimeRaw = (3 * time.Hour).String()
	userConfig.Sealer.GasPriceRaw = big.NewInt(30 * params.GWei).String()
	userConfig.Gpo.MaxPriceRaw = gasprice.DefaultMaxPrice.String()
	userConfig.Gpo.IgnorePriceRaw = gasprice.DefaultIgnorePrice.String()
	userConfig.Cache.RejournalRaw = (60 * time.Minute).String()

	// Currently, the configurations (userConfig) is exported into `toml` file format.
	out, err := tomlSettings.Marshal(&userConfig)
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	c.UI.Output(string(out))

	return 0
}
