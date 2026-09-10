package cli

import (
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ethereum/go-ethereum/internal/cli/server"
)

// DumpconfigCommand is for exporting user provided flags into a config file
type DumpconfigCommand struct {
	*Meta2
}

// MarkDown implements cli.MarkDown interface
func (c *DumpconfigCommand) MarkDown() string {
	items := []string{
		"# Dumpconfig",
		"The ```bor dumpconfig <your-favourite-flags>``` command will export the user provided flags into a configuration file",
	}

	return strings.Join(items, "\n\n")
}

// Help implements the cli.Command interface
func (c *DumpconfigCommand) Help() string {
	return `Usage: bor dumpconfig <your-favourite-flags>

  This command will export the user provided flags into a configuration file`
}

// Synopsis implements the cli.Command interface
func (c *DumpconfigCommand) Synopsis() string {
	return "Export configuration file"
}

// Run implements the cli.Command interface
func (c *DumpconfigCommand) Run(args []string) int {
	// Initialize an empty command instance to get the flags.
	command := server.Command{}
	flags := command.Flags(nil)

	if err := flags.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	userConfig := command.GetConfig()

	// Keep generated default config environment-neutral.
	userConfig.Chain = ""
	userConfig.Identity = ""
	userConfig.DataDir = "/var/lib/bor"

	// Convert the big.Int and time.Duration fields to their corresponding Raw fields
	userConfig.JsonRPC.RPCEVMTimeoutRaw = userConfig.JsonRPC.RPCEVMTimeout.String()
	userConfig.JsonRPC.HttpTimeout.ReadTimeoutRaw = userConfig.JsonRPC.HttpTimeout.ReadTimeout.String()
	userConfig.JsonRPC.HttpTimeout.ReadHeaderTimeoutRaw = userConfig.JsonRPC.HttpTimeout.ReadHeaderTimeout.String()
	userConfig.JsonRPC.HttpTimeout.WriteTimeoutRaw = userConfig.JsonRPC.HttpTimeout.WriteTimeout.String()
	userConfig.JsonRPC.HttpTimeout.IdleTimeoutRaw = userConfig.JsonRPC.HttpTimeout.IdleTimeout.String()
	userConfig.JsonRPC.Http.ExecutionPoolRequestTimeoutRaw = userConfig.JsonRPC.Http.ExecutionPoolRequestTimeout.String()
	userConfig.JsonRPC.Ws.ExecutionPoolRequestTimeoutRaw = userConfig.JsonRPC.Ws.ExecutionPoolRequestTimeout.String()
	userConfig.JsonRPC.TxSyncDefaultTimeoutRaw = userConfig.JsonRPC.TxSyncDefaultTimeout.String()
	userConfig.JsonRPC.TxSyncMaxTimeoutRaw = userConfig.JsonRPC.TxSyncMaxTimeout.String()
	userConfig.TxPool.RejournalRaw = userConfig.TxPool.Rejournal.String()
	userConfig.TxPool.LifeTimeRaw = userConfig.TxPool.LifeTime.String()
	userConfig.TxPool.RebroadcastIntervalRaw = userConfig.TxPool.RebroadcastInterval.String()
	userConfig.TxPool.RebroadcastMaxAgeRaw = userConfig.TxPool.RebroadcastMaxAge.String()
	userConfig.Sealer.GasPriceRaw = userConfig.Sealer.GasPrice.String()
	userConfig.Sealer.RecommitRaw = userConfig.Sealer.Recommit.String()
	userConfig.Sealer.BlockTimeRaw = userConfig.Sealer.BlockTime.String()
	userConfig.Gpo.MaxPriceRaw = userConfig.Gpo.MaxPrice.String()
	userConfig.Gpo.IgnorePriceRaw = userConfig.Gpo.IgnorePrice.String()
	userConfig.Cache.TrieTimeoutRaw = userConfig.Cache.TrieTimeout.String()
	userConfig.P2P.TxArrivalWaitRaw = userConfig.P2P.TxArrivalWait.String()
	userConfig.Sequencer.PollRaw = userConfig.Sequencer.Poll.String()

	if err := toml.NewEncoder(os.Stdout).Encode(userConfig); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	return 0
}
