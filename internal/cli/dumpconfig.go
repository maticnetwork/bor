package cli

import (
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum/internal/cli/server"
	"github.com/naoina/toml"
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
	config := server.DefaultConfig()

	// Overwrite user provided values on defaults (as done in ./server/command.go's Run() function)
	if err := config.Merge(userConfig); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	// Currently, the config is exported into `toml` file format.
	out, err := tomlSettings.Marshal(&config)
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	c.UI.Output(string(out))

	return 0
}
