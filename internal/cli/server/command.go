package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mitchellh/cli"
	"github.com/pelletier/go-toml"

	"github.com/ethereum/go-ethereum/log"

	"github.com/0xPolygon/heimdall-v2/app"
	heimdalld "github.com/0xPolygon/heimdall-v2/cmd/heimdalld/cmd"
	svrcmd "github.com/cosmos/cosmos-sdk/server/cmd"
)

// Command is the command to start the sever
type Command struct {
	UI cli.Ui

	// cli configuration
	cliConfig *Config

	// final configuration
	config *Config

	configFile string

	srv *Server
}

// MarkDown implements cli.MarkDown interface
func (c *Command) MarkDown() string {
	items := []string{
		"# Server",
		"The ```bor server``` command runs the Bor client.",
		c.Flags(nil).MarkDown(),
	}

	return strings.Join(items, "\n\n")
}

// Help implements the cli.Command interface
func (c *Command) Help() string {
	return `Usage: bor [options]

	Run the Bor server.
  ` + c.Flags(nil).Help()
}

// Synopsis implements the cli.Command interface
func (c *Command) Synopsis() string {
	return "Run the Bor server"
}

// checkConfigFlag checks if the config flag is set or not. If set,
// it returns the value else an empty string.
func checkConfigFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Check for single or double dashes
		if strings.HasPrefix(arg, "-config") || strings.HasPrefix(arg, "--config") {
			parts := strings.SplitN(arg, "=", 2)
			if len(parts) == 2 {
				return parts[1]
			}

			// If there's no equal sign, check the next argument
			if i+1 < len(args) {
				return args[i+1]
			}
		}
	}

	return ""
}

func (c *Command) extractFlags(args []string) error {
	// Check if config file is provided or not
	configFilePath := checkConfigFlag(args)

	if configFilePath != "" {
		log.Info("Reading config file", "path", configFilePath)

		// Parse the config file
		cfg, err := readConfigFile(configFilePath)
		if err != nil {
			c.UI.Error(err.Error())

			return err
		}

		log.Warn("Config set via config file will be overridden by cli flags")

		// Initialise a flagset based on the config created above
		flags := c.Flags(cfg)

		// Check for explicit cli args
		cmd := Command{} // use a new variable to keep the original config intact

		cliFlags := cmd.Flags(nil)
		if err := cliFlags.Parse(args); err != nil {
			c.UI.Error(err.Error())

			return err
		}

		// Get the list of flags set explicitly
		names, values := cliFlags.Visit()

		// Set these flags using the flagset created earlier
		flags.UpdateValue(names, values)
	} else {
		flags := c.Flags(nil)

		if err := flags.Parse(args); err != nil {
			c.UI.Error(err.Error())

			return err
		}
	}

	var tomlConfig *toml.Tree

	// nolint: nestif
	// check for log-level and verbosity here
	if configFilePath != "" {
		tomlConfig, _ = toml.LoadFile(configFilePath)
		if tomlConfig.Has("verbosity") && tomlConfig.Has("log-level") {
			log.Warn("Config contains both, verbosity and log-level, log-level will be deprecated soon. Use verbosity only.", "using", tomlConfig.Get("verbosity"))
		} else if !tomlConfig.Has("verbosity") && tomlConfig.Has("log-level") {
			log.Warn("Config contains log-level only, note that log-level will be deprecated soon. Use verbosity instead.", "using", tomlConfig.Get("log-level"))
			c.cliConfig.Verbosity = VerbosityStringToInt(strings.ToLower(tomlConfig.Get("log-level").(string)))
		}
	} else {
		tempFlag := 0

		for _, val := range args {
			if (strings.HasPrefix(val, "-verbosity") || strings.HasPrefix(val, "--verbosity")) && c.cliConfig.LogLevel != "" {
				tempFlag = 1
				break
			}
		}

		if tempFlag == 1 {
			log.Warn("Both, verbosity and log-level flags are provided, log-level will be deprecated soon. Use verbosity only.", "using", c.cliConfig.Verbosity)
		} else if tempFlag == 0 && c.cliConfig.LogLevel != "" {
			log.Warn("Only log-level flag is provided, note that log-level will be deprecated soon. Use verbosity instead.", "using", c.cliConfig.LogLevel)
			c.cliConfig.Verbosity = VerbosityStringToInt(strings.ToLower(c.cliConfig.LogLevel))
		}
	}

	// Handle multiple flags for tx lookup limit
	c.cliConfig.Cache.TxLookupLimit = handleTxLookupLimitFlag(tomlConfig, args, c.cliConfig)

	c.config = c.cliConfig

	return nil
}

func handleTxLookupLimitFlag(config *toml.Tree, args []string, cliConfig *Config) uint64 {
	var (
		oldSet bool
		newSet bool
		value  int64
	)

	for _, val := range args {
		if strings.HasPrefix(val, "-txlookuplimit") || strings.HasPrefix(val, "--txlookuplimit") {
			oldSet = true
		}
		if strings.HasPrefix(val, "-history.transactions") || strings.HasPrefix(val, "--history.transactions") {
			newSet = true
		}
	}

	if oldSet && newSet {
		log.Warn("Both, txlookuplimit and history.transactions flags are provided. txlookuplimit will be deprecated soon, using history.transactions", "value", cliConfig.History.TransactionHistory)
		return cliConfig.History.TransactionHistory
	}

	if oldSet && !newSet {
		log.Warn("The flag txlookuplimit will be deprecated soon, please use history.transactions instead")
		return cliConfig.Cache.TxLookupLimit
	}

	if newSet && !oldSet {
		return cliConfig.History.TransactionHistory
	}

	if config == nil {
		return cliConfig.History.TransactionHistory
	}

	oldSet = config.Has("cache.txlookuplimit")
	newSet = config.Has("history.transactions")

	if oldSet && newSet {
		value = config.Get("history.transactions").(int64)
		log.Warn("Config contains both, txlookuplimit and history.transactions. txlookuplimit will be deprecated soon, using history.transactions", "value", value)
		return uint64(value)
	}

	if oldSet && !newSet {
		value = config.Get("cache.txlookuplimit").(int64)
		log.Warn("The flag txlookuplimit will be deprecated soon, please use history.transactions instead")
		return uint64(value)
	}

	if newSet && !oldSet {
		value = config.Get("history.transactions").(int64)
		return uint64(value)
	}

	// User hasn't set any of these flags explcitly, use default value of new flag
	return cliConfig.History.TransactionHistory
}

// Run implements the cli.Command interface
func (c *Command) Run(args []string) int {
	err := c.extractFlags(args)
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	if c.config.Heimdall.RunHeimdall {
		// TODO HV2: Find a way to pass the shutdown ctx to heimdall process
		_, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		go func() {
			rootCmd := heimdalld.NewRootCmd()
			if err := svrcmd.Execute(rootCmd, "HD", app.DefaultNodeHome); err != nil {
				_, _ = fmt.Fprintln(rootCmd.OutOrStderr(), err)
				os.Exit(1)
			}
		}()
	}

	srv, err := NewServer(c.config, WithGRPCAddress())
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	c.srv = srv

	return c.handleSignals()
}

func (c *Command) handleSignals() int {
	signalCh := make(chan os.Signal, 4)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	sig := <-signalCh

	c.UI.Output(fmt.Sprintf("Caught signal: %v", sig))
	c.UI.Output("Gracefully shutting down agent...")

	gracefulCh := make(chan struct{})
	go func() {
		c.srv.Stop()
		close(gracefulCh)
	}()

	for i := 10; i > 0; i-- {
		select {
		case <-signalCh:
			log.Warn("Already shutting down, interrupt more force stop.", "times", i-1)
		case <-gracefulCh:
			return 0
		}
	}

	return 1
}

// GetConfig returns the user specified config
func (c *Command) GetConfig() *Config {
	return c.cliConfig
}
