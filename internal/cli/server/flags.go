package server

import (
	"github.com/ethereum/go-ethereum/internal/cli/flagset"
)

func (c *Command) Flags(config *Config) *flagset.Flagset {
	if config != nil {
		c.cliConfig = config
	} else {
		c.cliConfig = DefaultConfig()
	}

	f := flagset.NewFlagSet("server")

	f.StringFlag(&flagset.StringFlag{
		Name:    "chain",
		Usage:   "Name of the chain to sync ('amoy', 'mumbai', 'mainnet') or path to a genesis file",
		Value:   &c.cliConfig.Chain,
		Default: c.cliConfig.Chain,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:               "identity",
		Usage:              "Name/Identity of the node",
		Value:              &c.cliConfig.Identity,
		Default:            c.cliConfig.Identity,
		HideDefaultFromDoc: true,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "verbosity",
		Usage:   "Logging verbosity for the server (5=trace|4=debug|3=info|2=warn|1=error|0=crit)",
		Value:   &c.cliConfig.Verbosity,
		Default: c.cliConfig.Verbosity,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:               "datadir",
		Usage:              "Path of the data directory to store information",
		Value:              &c.cliConfig.DataDir,
		Default:            c.cliConfig.DataDir,
		HideDefaultFromDoc: true,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "vmdebug",
		Usage:   "Record information useful for VM and contract debugging",
		Value:   &c.cliConfig.EnablePreimageRecording,
		Default: c.cliConfig.EnablePreimageRecording,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "evm-switch-dispatch",
		Usage:   "Enable switch-based fast path EVM interpreter",
		Value:   &c.cliConfig.EnableEVMSwitchDispatch,
		Default: c.cliConfig.EnableEVMSwitchDispatch,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "vmtrace",
		Usage:   "Name of a tracer to record internal VM operations during blockchain synchronization (costly) (e.g. 'json')",
		Value:   &c.cliConfig.VMTrace,
		Default: c.cliConfig.VMTrace,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "vmtrace.jsonconfig",
		Usage:   "Tracer configuration (JSON)",
		Value:   &c.cliConfig.VMTraceJsonConfig,
		Default: c.cliConfig.VMTraceJsonConfig,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "datadir.ancient",
		Usage:   "Data directory for ancient chain segments (default = inside chaindata)",
		Value:   &c.cliConfig.Ancient,
		Default: c.cliConfig.Ancient,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "db.engine",
		Usage:   "Backing database implementation to use ('leveldb' or 'pebble')",
		Value:   &c.cliConfig.DBEngine,
		Default: c.cliConfig.DBEngine,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "keystore",
		Usage:   "Path of the directory where keystores are located",
		Value:   &c.cliConfig.KeyStoreDir,
		Default: c.cliConfig.KeyStoreDir,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "rpc.batch-request-limit",
		Usage:   "Maximum number of requests in a batch (use 0 for no limits)",
		Value:   &c.cliConfig.BatchRequestLimit,
		Default: c.cliConfig.BatchRequestLimit,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "rpc.batch-response-max-size",
		Usage:   "Maximum number of response bytes across all requests in a batch (use 0 for no limits)",
		Value:   &c.cliConfig.BatchResponseMaxSize,
		Default: c.cliConfig.BatchResponseMaxSize,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "rpc.returndatalimit",
		Usage:   "Maximum size (in bytes) a result of an rpc request could have (use 0 for no limits)",
		Value:   &c.cliConfig.RPCReturnDataLimit,
		Default: c.cliConfig.RPCReturnDataLimit,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:  "config",
		Usage: "Path to the TOML configuration file",
		Value: &c.configFile,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "syncmode",
		Usage:   `Blockchain sync mode ("full", "snap" or "stateless")`,
		Value:   &c.cliConfig.SyncMode,
		Default: c.cliConfig.SyncMode,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "gcmode",
		Usage:   `Blockchain garbage collection mode ("full", "archive")`,
		Value:   &c.cliConfig.GcMode,
		Default: c.cliConfig.GcMode,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "state.scheme",
		Usage:   "Scheme to use for storing ethereum state ('hash' or 'path')",
		Value:   &c.cliConfig.StateScheme,
		Default: c.cliConfig.StateScheme,
	})
	f.MapStringFlag(&flagset.MapStringFlag{
		Name:    "eth.requiredblocks",
		Usage:   "Comma separated block number-to-hash mappings to require for peering (<number>=<hash>)",
		Value:   &c.cliConfig.RequiredBlocks,
		Default: c.cliConfig.RequiredBlocks,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "snapshot",
		Usage:   `Enables the snapshot-database mode`,
		Value:   &c.cliConfig.Snapshot,
		Default: c.cliConfig.Snapshot,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "bor.logs",
		Usage:   `Enables bor log retrieval`,
		Value:   &c.cliConfig.BorLogs,
		Default: c.cliConfig.BorLogs,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "disable-blind-fork-validation",
		Usage:   `Disable additional fork validation and accept blind forks without tracing back to last whitelisted entry`,
		Value:   &c.cliConfig.DisableBlindForkValidation,
		Default: c.cliConfig.DisableBlindForkValidation,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "max-blind-fork-validation-limit",
		Usage:   `Maximum number of blocks to traverse back in the database when validating blind forks`,
		Value:   &c.cliConfig.MaxBlindForkValidationLimit,
		Default: c.cliConfig.MaxBlindForkValidationLimit,
	})

	// logging related flags
	f.StringFlag(&flagset.StringFlag{
		Name:    "vmodule",
		Usage:   "Per-module verbosity: comma-separated list of <pattern>=<level> (e.g. eth/*=5,p2p=4)",
		Value:   &c.cliConfig.Logging.Vmodule,
		Default: c.cliConfig.Logging.Vmodule,
		Group:   "Logging",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "log.json",
		Usage:   "Format logs with JSON",
		Value:   &c.cliConfig.Logging.Json,
		Default: c.cliConfig.Logging.Json,
		Group:   "Logging",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "log.backtrace",
		Usage:   "Request a stack trace at a specific logging statement (e.g. 'block.go:271')",
		Value:   &c.cliConfig.Logging.Backtrace,
		Default: c.cliConfig.Logging.Backtrace,
		Group:   "Logging",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "log.debug",
		Usage:   "Prepends log messages with call-site location (file and line number)",
		Value:   &c.cliConfig.Logging.Debug,
		Default: c.cliConfig.Logging.Debug,
		Group:   "Logging",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "log.enable-block-tracking",
		Usage:   "Enables additional logging of information collected while tracking block lifecycle",
		Value:   &c.cliConfig.Logging.EnableBlockTracking,
		Default: c.cliConfig.Logging.EnableBlockTracking,
		Group:   "Logging",
	})

	// heimdall
	f.StringFlag(&flagset.StringFlag{
		Name:    "bor.heimdall",
		Usage:   "URL of Heimdall service (comma-separated for failover: \"url1,url2\")",
		Value:   &c.cliConfig.Heimdall.URL,
		Default: c.cliConfig.Heimdall.URL,
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "bor.heimdalltimeout",
		Usage:   "Timeout period for bor's outgoing requests to heimdall",
		Value:   &c.cliConfig.Heimdall.Timeout,
		Default: c.cliConfig.Heimdall.Timeout,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "bor.withoutheimdall",
		Usage:   "Run without Heimdall service (for testing purpose)",
		Value:   &c.cliConfig.Heimdall.Without,
		Default: c.cliConfig.Heimdall.Without,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "bor.devfakeauthor",
		Usage:   "Run miner without validator set authorization [dev mode] : Use with '--bor.withoutheimdall'",
		Value:   &c.cliConfig.DevFakeAuthor,
		Default: c.cliConfig.DevFakeAuthor,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "bor.heimdallgRPC",
		Usage:   "Address of Heimdall gRPC service (comma-separated for failover: \"addr1,addr2\")",
		Value:   &c.cliConfig.Heimdall.GRPCAddress,
		Default: c.cliConfig.Heimdall.GRPCAddress,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "bor.heimdallWS",
		Usage:   "Address of Heimdall WS subscription service (comma-separated for failover: \"addr1,addr2\")",
		Value:   &c.cliConfig.Heimdall.WSAddress,
		Default: c.cliConfig.Heimdall.WSAddress,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "bor.runheimdall",
		Usage:   "Run Heimdall service as a child process",
		Value:   &c.cliConfig.Heimdall.RunHeimdall,
		Default: c.cliConfig.Heimdall.RunHeimdall,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "bor.runheimdallargs",
		Usage:   "Arguments to pass to Heimdall service",
		Value:   &c.cliConfig.Heimdall.RunHeimdallArgs,
		Default: c.cliConfig.Heimdall.RunHeimdallArgs,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "bor.useheimdallapp",
		Usage:   "Use child heimdall process to fetch data, Only works when bor.runheimdall is true",
		Value:   &c.cliConfig.Heimdall.UseHeimdallApp,
		Default: c.cliConfig.Heimdall.UseHeimdallApp,
	})

	// txpool options
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "txpool.locals",
		Usage:   "Comma separated accounts to treat as locals (no flush, priority inclusion)",
		Value:   &c.cliConfig.TxPool.Locals,
		Default: c.cliConfig.TxPool.Locals,
		Group:   "Transaction Pool",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "txpool.nolocals",
		Usage:   "Disables price exemptions for locally submitted transactions",
		Value:   &c.cliConfig.TxPool.NoLocals,
		Default: c.cliConfig.TxPool.NoLocals,
		Group:   "Transaction Pool",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "txpool.journal",
		Usage:   "Disk journal for local transaction to survive node restarts",
		Value:   &c.cliConfig.TxPool.Journal,
		Default: c.cliConfig.TxPool.Journal,
		Group:   "Transaction Pool",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "txpool.rejournal",
		Usage:   "Time interval to regenerate the local transaction journal",
		Value:   &c.cliConfig.TxPool.Rejournal,
		Default: c.cliConfig.TxPool.Rejournal,
		Group:   "Transaction Pool",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txpool.pricelimit",
		Usage:   "Minimum gas price limit to enforce for acceptance into the pool",
		Value:   &c.cliConfig.TxPool.PriceLimit,
		Default: c.cliConfig.TxPool.PriceLimit,
		Group:   "Transaction Pool",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txpool.pricebump",
		Usage:   "Price bump percentage to replace an already existing transaction",
		Value:   &c.cliConfig.TxPool.PriceBump,
		Default: c.cliConfig.TxPool.PriceBump,
		Group:   "Transaction Pool",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txpool.accountslots",
		Usage:   "Minimum number of executable transaction slots guaranteed per account",
		Value:   &c.cliConfig.TxPool.AccountSlots,
		Default: c.cliConfig.TxPool.AccountSlots,
		Group:   "Transaction Pool",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txpool.globalslots",
		Usage:   "Maximum number of executable transaction slots for all accounts",
		Value:   &c.cliConfig.TxPool.GlobalSlots,
		Default: c.cliConfig.TxPool.GlobalSlots,
		Group:   "Transaction Pool",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txpool.accountqueue",
		Usage:   "Maximum number of non-executable transaction slots permitted per account",
		Value:   &c.cliConfig.TxPool.AccountQueue,
		Default: c.cliConfig.TxPool.AccountQueue,
		Group:   "Transaction Pool",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txpool.globalqueue",
		Usage:   "Maximum number of non-executable transaction slots for all accounts",
		Value:   &c.cliConfig.TxPool.GlobalQueue,
		Default: c.cliConfig.TxPool.GlobalQueue,
		Group:   "Transaction Pool",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "txpool.lifetime",
		Usage:   "Maximum amount of time non-executable transaction are queued",
		Value:   &c.cliConfig.TxPool.LifeTime,
		Default: c.cliConfig.TxPool.LifeTime,
		Group:   "Transaction Pool",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "txpool.filtered-addresses",
		Usage:   "Path to the file containing a newline-separated list of addresses whose transactions will be filtered",
		Value:   &c.cliConfig.TxPool.FilteredAddressesFile,
		Default: c.cliConfig.TxPool.FilteredAddressesFile,
		Group:   "Transaction Pool",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "txpool.rebroadcast",
		Usage:   "Enable stuck transaction rebroadcast mechanism",
		Value:   &c.cliConfig.TxPool.Rebroadcast,
		Default: c.cliConfig.TxPool.Rebroadcast,
		Group:   "Transaction Pool",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "txpool.rebroadcast-interval",
		Usage:   "Interval between rebroadcast checks for stuck transactions",
		Value:   &c.cliConfig.TxPool.RebroadcastInterval,
		Default: c.cliConfig.TxPool.RebroadcastInterval,
		Group:   "Transaction Pool",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "txpool.rebroadcast-max-age",
		Usage:   "Maximum age for a transaction to be eligible for rebroadcast",
		Value:   &c.cliConfig.TxPool.RebroadcastMaxAge,
		Default: c.cliConfig.TxPool.RebroadcastMaxAge,
		Group:   "Transaction Pool",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "txpool.rebroadcast-batch-size",
		Usage:   "Maximum number of transactions to rebroadcast per cycle",
		Value:   &c.cliConfig.TxPool.RebroadcastBatchSize,
		Default: c.cliConfig.TxPool.RebroadcastBatchSize,
		Group:   "Transaction Pool",
	})

	// sealer options
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "mine",
		Usage:   "Enable mining",
		Value:   &c.cliConfig.Sealer.Enabled,
		Default: c.cliConfig.Sealer.Enabled,
		Group:   "Sealer",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "allow-gas-tip-override",
		Usage:   "Allows block producers to override the mining gas tip",
		Value:   &c.cliConfig.Sealer.AllowGasTipOverride,
		Default: c.cliConfig.Sealer.AllowGasTipOverride,
		Group:   "Sealer",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "miner.etherbase",
		Usage:   "Public address for block mining rewards",
		Value:   &c.cliConfig.Sealer.Etherbase,
		Default: c.cliConfig.Sealer.Etherbase,
		Group:   "Sealer",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "miner.extradata",
		Usage:   "Block extra data set by the miner (default = client version)",
		Value:   &c.cliConfig.Sealer.ExtraData,
		Default: c.cliConfig.Sealer.ExtraData,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.gaslimit",
		Usage:   "Target gas ceiling (gas limit) for mined blocks",
		Value:   &c.cliConfig.Sealer.GasCeil,
		Default: c.cliConfig.Sealer.GasCeil,
		Group:   "Sealer",
	})
	f.BigIntFlag(&flagset.BigIntFlag{
		Name:    "miner.gasprice",
		Usage:   "Minimum gas price for mining a transaction",
		Value:   c.cliConfig.Sealer.GasPrice,
		Group:   "Sealer",
		Default: c.cliConfig.Sealer.GasPrice,
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "miner.recommit",
		Usage:   "The time interval for miner to re-create mining work",
		Value:   &c.cliConfig.Sealer.Recommit,
		Default: c.cliConfig.Sealer.Recommit,
		Group:   "Sealer",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "miner.interruptcommit",
		Usage:   "Interrupt block commit when block creation time is passed",
		Value:   &c.cliConfig.Sealer.CommitInterruptFlag,
		Default: c.cliConfig.Sealer.CommitInterruptFlag,
		Group:   "Sealer",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "miner.blocktime",
		Usage:   "The block time defined by the miner. Needs to be larger or equal to the consensus block time. If not set (default = 0), the miner will use the consensus block time.",
		Value:   &c.cliConfig.Sealer.BlockTime,
		Default: c.cliConfig.Sealer.BlockTime,
		Group:   "Sealer",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "miner.prefetch",
		Usage:   "Enable transaction prefetching from the pool during block building",
		Value:   &c.cliConfig.Sealer.EnablePrefetch,
		Default: c.cliConfig.Sealer.EnablePrefetch,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.prefetch.gaslimit.percent",
		Usage:   "Gas limit percentage for prefetching (e.g., 100 = 100%, 110 = 110%)",
		Value:   &c.cliConfig.Sealer.PrefetchGasLimitPercent,
		Default: c.cliConfig.Sealer.PrefetchGasLimitPercent,
		Group:   "Sealer",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "miner.enableDynamicGasLimit",
		Usage:   "Enable dynamic gas limit adjustment based on base fee",
		Value:   &c.cliConfig.Sealer.EnableDynamicGasLimit,
		Default: c.cliConfig.Sealer.EnableDynamicGasLimit,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.gasLimitMin",
		Usage:   "Minimum gas limit when dynamic gas limit is enabled",
		Value:   &c.cliConfig.Sealer.GasLimitMin,
		Default: c.cliConfig.Sealer.GasLimitMin,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.gasLimitMax",
		Usage:   "Maximum gas limit when dynamic gas limit is enabled",
		Value:   &c.cliConfig.Sealer.GasLimitMax,
		Default: c.cliConfig.Sealer.GasLimitMax,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.targetBaseFee",
		Usage:   "Target base fee in wei for dynamic gas limit (e.g., 30000000000 for 30 gwei)",
		Value:   &c.cliConfig.Sealer.TargetBaseFee,
		Default: c.cliConfig.Sealer.TargetBaseFee,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.baseFeeBuffer",
		Usage:   "Buffer around target base fee in wei (no adjustment when within buffer)",
		Value:   &c.cliConfig.Sealer.BaseFeeBuffer,
		Default: c.cliConfig.Sealer.BaseFeeBuffer,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.targetGasPercentage",
		Usage:   "Target gas as percentage of gas limit (1-100, default 65) for post-Lisovo blocks",
		Value:   &c.cliConfig.Sealer.TargetGasPercentage,
		Default: c.cliConfig.Sealer.TargetGasPercentage,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.baseFeeChangeDenominator",
		Usage:   "Base fee change rate denominator (must be >0, default 64) for post-Lisovo blocks",
		Value:   &c.cliConfig.Sealer.BaseFeeChangeDenominator,
		Default: c.cliConfig.Sealer.BaseFeeChangeDenominator,
		Group:   "Sealer",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "miner.enableDynamicTargetGas",
		Usage:   "Enable dynamic EIP-1559 target gas percentage adjustment based on base fee (post-Lisovo, mutually exclusive with enableDynamicGasLimit)",
		Value:   &c.cliConfig.Sealer.EnableDynamicTargetGas,
		Default: c.cliConfig.Sealer.EnableDynamicTargetGas,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.targetGasMinPercentage",
		Usage:   "Minimum target gas percentage (1-100) when dynamic target gas is enabled",
		Value:   &c.cliConfig.Sealer.TargetGasMinPercentage,
		Default: c.cliConfig.Sealer.TargetGasMinPercentage,
		Group:   "Sealer",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "miner.targetGasMaxPercentage",
		Usage:   "Maximum target gas percentage (1-100) when dynamic target gas is enabled",
		Value:   &c.cliConfig.Sealer.TargetGasMaxPercentage,
		Default: c.cliConfig.Sealer.TargetGasMaxPercentage,
		Group:   "Sealer",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "miner.disable-pending-block",
		Usage:   "Disable the pending block creation loop on non block producer nodes. When set, 'pending' block will be unavailable for RPC queries.",
		Value:   &c.cliConfig.Sealer.DisablePendingBlock,
		Default: c.cliConfig.Sealer.DisablePendingBlock,
		Group:   "Sealer",
	})

	// ethstats
	f.StringFlag(&flagset.StringFlag{
		Name:    "ethstats",
		Usage:   "Reporting URL of a ethstats service (nodename:secret@host:port)",
		Value:   &c.cliConfig.Ethstats,
		Default: c.cliConfig.Ethstats,
	})

	// gas price oracle
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "gpo.blocks",
		Usage:   "Number of recent blocks to check for gas prices",
		Value:   &c.cliConfig.Gpo.Blocks,
		Default: c.cliConfig.Gpo.Blocks,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "gpo.percentile",
		Usage:   "Suggested gas price is the given percentile of a set of recent transaction gas prices",
		Value:   &c.cliConfig.Gpo.Percentile,
		Default: c.cliConfig.Gpo.Percentile,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "gpo.maxheaderhistory",
		Usage:   "Maximum header history of gasprice oracle",
		Value:   &c.cliConfig.Gpo.MaxHeaderHistory,
		Default: c.cliConfig.Gpo.MaxHeaderHistory,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "gpo.maxblockhistory",
		Usage:   "Maximum block history of gasprice oracle",
		Value:   &c.cliConfig.Gpo.MaxBlockHistory,
		Default: c.cliConfig.Gpo.MaxBlockHistory,
	})
	f.BigIntFlag(&flagset.BigIntFlag{
		Name:    "gpo.maxprice",
		Usage:   "Maximum gas price will be recommended by gpo",
		Value:   c.cliConfig.Gpo.MaxPrice,
		Default: c.cliConfig.Gpo.MaxPrice,
	})
	f.BigIntFlag(&flagset.BigIntFlag{
		Name:    "gpo.ignoreprice",
		Usage:   "Gas price below which gpo will ignore transactions",
		Value:   c.cliConfig.Gpo.IgnorePrice,
		Default: c.cliConfig.Gpo.IgnorePrice,
	})

	// cache options
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "cache",
		Usage:   "Megabytes of memory allocated to internal caching",
		Value:   &c.cliConfig.Cache.Cache,
		Default: c.cliConfig.Cache.Cache,
		Group:   "Cache",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "cache.database",
		Usage:   "Percentage of cache memory allowance to use for database io",
		Value:   &c.cliConfig.Cache.PercDatabase,
		Default: c.cliConfig.Cache.PercDatabase,
		Group:   "Cache",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "cache.trie",
		Usage:   "Percentage of cache memory allowance to use for trie caching",
		Value:   &c.cliConfig.Cache.PercTrie,
		Default: c.cliConfig.Cache.PercTrie,
		Group:   "Cache",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "cache.gc",
		Usage:   "Percentage of cache memory allowance to use for trie pruning",
		Value:   &c.cliConfig.Cache.PercGc,
		Default: c.cliConfig.Cache.PercGc,
		Group:   "Cache",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "cache.snapshot",
		Usage:   "Percentage of cache memory allowance to use for snapshot caching",
		Value:   &c.cliConfig.Cache.PercSnapshot,
		Default: c.cliConfig.Cache.PercSnapshot,
		Group:   "Cache",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "cache.noprefetch",
		Usage:   "Disable heuristic state prefetch during block import (less CPU and disk IO, more time waiting for data)",
		Value:   &c.cliConfig.Cache.NoPrefetch,
		Default: c.cliConfig.Cache.NoPrefetch,
		Group:   "Cache",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "cache.preimages",
		Usage:   "Enable recording the SHA3/keccak preimages of trie keys",
		Value:   &c.cliConfig.Cache.Preimages,
		Default: c.cliConfig.Cache.Preimages,
		Group:   "Cache",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "cache.addresscachesizes",
		Usage:   "Address-specific cache sizes for biased caching in MB (format: address=sizeMB,address=sizeMB, e.g. 0x1234...=1024,0x5678...=512)",
		Value:   &c.cliConfig.Cache.AddressCacheSizesRaw,
		Default: c.cliConfig.Cache.AddressCacheSizesRaw,
		Group:   "Cache",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "cache.preloadratelimit",
		Usage:   "Rate limit per address for cache preloading (e.g. 500KB, 1MB, 0 for unlimited). Limits I/O during sync. Default: 1MB",
		Value:   &c.cliConfig.Cache.PreloadRateLimit,
		Default: c.cliConfig.Cache.PreloadRateLimit,
		Group:   "Cache",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "cache.triesinmemory",
		Usage:   "Number of block states (tries) to keep in memory",
		Value:   &c.cliConfig.Cache.TriesInMemory,
		Default: c.cliConfig.Cache.TriesInMemory,
		Group:   "Cache",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "cache.blocklogs",
		Usage:   "Size (in number of blocks) of the log cache for filtering",
		Value:   &c.cliConfig.Cache.FilterLogCacheSize,
		Default: c.cliConfig.Cache.FilterLogCacheSize,
		Group:   "Cache",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "txlookuplimit",
		Usage:   "Number of recent blocks to maintain transactions index for (soon to be deprecated, use history.transactions instead)",
		Value:   &c.cliConfig.Cache.TxLookupLimit,
		Default: c.cliConfig.Cache.TxLookupLimit,
		Group:   "Cache",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "pipeline.enable-import-src",
		Usage:   "Enable pipelined state root computation during block import: overlap SRC(N) with block N+1 tx execution",
		Value:   &c.cliConfig.Pipeline.EnableImportSRC,
		Default: c.cliConfig.Pipeline.EnableImportSRC,
		Group:   "Pipeline",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "pipeline.import-src-logs",
		Usage:   "Enable verbose logging for pipelined import SRC",
		Value:   &c.cliConfig.Pipeline.ImportSRCLogs,
		Default: c.cliConfig.Pipeline.ImportSRCLogs,
		Group:   "Pipeline",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "pipeline.warm-snapshot",
		Usage:   "Enable warm-node handoff from the execution-side trie prefetcher to the pipelined SRC when witnesses are produced; no effect when import SRC is disabled or witnesses are off",
		Value:   &c.cliConfig.Pipeline.WarmSnapshot,
		Default: c.cliConfig.Pipeline.WarmSnapshot,
		Group:   "Pipeline",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "fdlimit",
		Usage:   "Raise the open file descriptor resource limit (default = system fd limit)",
		Value:   &c.cliConfig.Cache.FDLimit,
		Default: c.cliConfig.Cache.FDLimit,
		Group:   "Cache",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "cache.gomemlimit",
		Usage:   "Set GOMEMLIMIT for the runtime (e.g. '34GB', '34359738368'). Empty means no limit",
		Value:   &c.cliConfig.Cache.GoMemLimit,
		Default: c.cliConfig.Cache.GoMemLimit,
		Group:   "Cache",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "cache.gogc",
		Usage:   "Set GOGC percentage for garbage collection trigger (default: 100)",
		Value:   &c.cliConfig.Cache.GoGC,
		Default: c.cliConfig.Cache.GoGC,
		Group:   "Cache",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "cache.godebug",
		Usage:   "Set GODEBUG variables for runtime debugging (e.g. 'gctrace=1,gcpacertrace=1')",
		Value:   &c.cliConfig.Cache.GoDebug,
		Default: c.cliConfig.Cache.GoDebug,
		Group:   "Cache",
	})

	// LevelDB options
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "leveldb.compaction.table.size",
		Usage:   "LevelDB SSTable/file size in mebibytes",
		Value:   &c.cliConfig.ExtraDB.LevelDbCompactionTableSize,
		Default: c.cliConfig.ExtraDB.LevelDbCompactionTableSize,
		Group:   "ExtraDB",
	})
	f.Float64Flag(&flagset.Float64Flag{
		Name:    "leveldb.compaction.table.size.multiplier",
		Usage:   "Multiplier on LevelDB SSTable/file size. Size for a level is determined by: `leveldb.compaction.table.size * (leveldb.compaction.table.size.multiplier ^ Level)`",
		Value:   &c.cliConfig.ExtraDB.LevelDbCompactionTableSizeMultiplier,
		Default: c.cliConfig.ExtraDB.LevelDbCompactionTableSizeMultiplier,
		Group:   "ExtraDB",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "leveldb.compaction.total.size",
		Usage:   "Total size in mebibytes of SSTables in a given LevelDB level. Size for a level is determined by: `leveldb.compaction.total.size * (leveldb.compaction.total.size.multiplier ^ Level)`",
		Value:   &c.cliConfig.ExtraDB.LevelDbCompactionTotalSize,
		Default: c.cliConfig.ExtraDB.LevelDbCompactionTotalSize,
		Group:   "ExtraDB",
	})
	f.Float64Flag(&flagset.Float64Flag{
		Name:    "leveldb.compaction.total.size.multiplier",
		Usage:   "Multiplier on level size on LevelDB levels. Size for a level is determined by: `leveldb.compaction.total.size * (leveldb.compaction.total.size.multiplier ^ Level)`",
		Value:   &c.cliConfig.ExtraDB.LevelDbCompactionTotalSizeMultiplier,
		Default: c.cliConfig.ExtraDB.LevelDbCompactionTotalSizeMultiplier,
		Group:   "ExtraDB",
	})

	// rpc options
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "rpc.gascap",
		Usage:   "Sets a cap on gas that can be used in eth_call/estimateGas (0=infinite)",
		Value:   &c.cliConfig.JsonRPC.GasCap,
		Default: c.cliConfig.JsonRPC.GasCap,
		Group:   "JsonRPC",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "rpc.evmtimeout",
		Usage:   "Sets a timeout used for eth_call (0=infinite)",
		Value:   &c.cliConfig.JsonRPC.RPCEVMTimeout,
		Default: c.cliConfig.JsonRPC.RPCEVMTimeout,
		Group:   "JsonRPC",
	})
	f.Float64Flag(&flagset.Float64Flag{
		Name:    "rpc.txfeecap",
		Usage:   "Sets a cap on transaction fee (in ether) that can be sent via the RPC APIs (0 = no cap)",
		Value:   &c.cliConfig.JsonRPC.TxFeeCap,
		Default: c.cliConfig.JsonRPC.TxFeeCap,
		Group:   "JsonRPC",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "rpc.logquerylimit",
		Usage:   "Maximum number of alternative addresses or topics allowed per search position in eth_getLogs filter criteria (0 = no cap)",
		Value:   &c.cliConfig.JsonRPC.LogQueryLimit,
		Default: c.cliConfig.JsonRPC.LogQueryLimit,
		Group:   "JsonRPC",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "rpc.rangelimit",
		Usage:   "Maximum block range allowed for eth_getLogs and bor_getLogs (0 = no limit)",
		Value:   &c.cliConfig.JsonRPC.RangeLimit,
		Default: c.cliConfig.JsonRPC.RangeLimit,
		Group:   "JsonRPC",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "rpc.allow-unprotected-txs",
		Usage:   "Allow for unprotected (non EIP155 signed) transactions to be submitted via RPC",
		Value:   &c.cliConfig.JsonRPC.AllowUnprotectedTxs,
		Default: c.cliConfig.JsonRPC.AllowUnprotectedTxs,
		Group:   "JsonRPC",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "rpc.enabledeprecatedpersonal",
		Usage:   "Enables the (deprecated) personal namespace",
		Value:   &c.cliConfig.JsonRPC.EnablePersonal,
		Default: c.cliConfig.JsonRPC.EnablePersonal,
		Group:   "JsonRPC",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "rpc.enabletrace",
		Usage:   "Enables the Parity-compatible trace namespace (trace_block, trace_transaction, trace_call, trace_callMany, trace_replayTransaction, trace_replayBlockTransactions)",
		Value:   &c.cliConfig.JsonRPC.EnableTrace,
		Default: c.cliConfig.JsonRPC.EnableTrace,
		Group:   "JsonRPC",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "rpc.txsync.defaulttimeout",
		Usage:   "Default timeout for eth_sendRawTransactionSync (e.g. 2s, 500ms)",
		Value:   &c.cliConfig.JsonRPC.TxSyncDefaultTimeout,
		Default: c.cliConfig.JsonRPC.TxSyncDefaultTimeout,
		Group:   "JsonRPC",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "rpc.txsync.maxtimeout",
		Usage:   "Maximum allowed timeout for eth_sendRawTransactionSync (e.g. 5m)",
		Value:   &c.cliConfig.JsonRPC.TxSyncMaxTimeout,
		Default: c.cliConfig.JsonRPC.TxSyncMaxTimeout,
		Group:   "JsonRPC",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "ipcdisable",
		Usage:   "Disable the IPC-RPC server",
		Value:   &c.cliConfig.JsonRPC.IPCDisable,
		Default: c.cliConfig.JsonRPC.IPCDisable,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "ipcpath",
		Usage:   "Filename for IPC socket/pipe within the datadir (explicit paths escape it)",
		Value:   &c.cliConfig.JsonRPC.IPCPath,
		Default: c.cliConfig.JsonRPC.IPCPath,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "authrpc.jwtsecret",
		Usage:   "Path to a JWT secret to use for authenticated RPC endpoints",
		Value:   &c.cliConfig.JsonRPC.Auth.JWTSecret,
		Default: c.cliConfig.JsonRPC.Auth.JWTSecret,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "authrpc.addr",
		Usage:   "Listening address for authenticated APIs",
		Value:   &c.cliConfig.JsonRPC.Auth.Addr,
		Default: c.cliConfig.JsonRPC.Auth.Addr,
		Group:   "JsonRPC",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "authrpc.port",
		Usage:   "Listening port for authenticated APIs",
		Value:   &c.cliConfig.JsonRPC.Auth.Port,
		Default: c.cliConfig.JsonRPC.Auth.Port,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "authrpc.vhosts",
		Usage:   "Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard.",
		Value:   &c.cliConfig.JsonRPC.Auth.VHosts,
		Default: c.cliConfig.JsonRPC.Auth.VHosts,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "http.corsdomain",
		Usage:   "Comma separated list of domains from which to accept cross origin requests (browser enforced)",
		Value:   &c.cliConfig.JsonRPC.Http.Cors,
		Default: c.cliConfig.JsonRPC.Http.Cors,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "http.vhosts",
		Usage:   "Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard.",
		Value:   &c.cliConfig.JsonRPC.Http.VHost,
		Default: c.cliConfig.JsonRPC.Http.VHost,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "ws.origins",
		Usage:   "Origins from which to accept websockets requests",
		Value:   &c.cliConfig.JsonRPC.Ws.Origins,
		Default: c.cliConfig.JsonRPC.Ws.Origins,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "graphql.corsdomain",
		Usage:   "Comma separated list of domains from which to accept cross origin requests (browser enforced)",
		Value:   &c.cliConfig.JsonRPC.Graphql.Cors,
		Default: c.cliConfig.JsonRPC.Graphql.Cors,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "graphql.vhosts",
		Usage:   "Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard.",
		Value:   &c.cliConfig.JsonRPC.Graphql.VHost,
		Default: c.cliConfig.JsonRPC.Graphql.VHost,
		Group:   "JsonRPC",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "accept-preconf-tx",
		Usage:   "Allows the RPC server to accept transactions for preconfirmation",
		Value:   &c.cliConfig.JsonRPC.AcceptPreconfTx,
		Default: c.cliConfig.JsonRPC.AcceptPreconfTx,
		Group:   "JsonRPC",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "accept-private-tx",
		Usage:   "Allows the RPC server to accept private transactions",
		Value:   &c.cliConfig.JsonRPC.AcceptPrivateTx,
		Default: c.cliConfig.JsonRPC.AcceptPrivateTx,
		Group:   "JsonRPC",
	})

	// http options
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "http",
		Usage:   "Enable the HTTP-RPC server",
		Value:   &c.cliConfig.JsonRPC.Http.Enabled,
		Default: c.cliConfig.JsonRPC.Http.Enabled,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "http.addr",
		Usage:   "HTTP-RPC server listening interface",
		Value:   &c.cliConfig.JsonRPC.Http.Host,
		Default: c.cliConfig.JsonRPC.Http.Host,
		Group:   "JsonRPC",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "http.port",
		Usage:   "HTTP-RPC server listening port",
		Value:   &c.cliConfig.JsonRPC.Http.Port,
		Default: c.cliConfig.JsonRPC.Http.Port,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "http.rpcprefix",
		Usage:   "HTTP path path prefix on which JSON-RPC is served. Use '/' to serve on all paths.",
		Value:   &c.cliConfig.JsonRPC.Http.Prefix,
		Default: c.cliConfig.JsonRPC.Http.Prefix,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "http.api",
		Usage:   "API's offered over the HTTP-RPC interface",
		Value:   &c.cliConfig.JsonRPC.Http.API,
		Default: c.cliConfig.JsonRPC.Http.API,
		Group:   "JsonRPC",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "http.ep-size",
		Usage:   "Maximum size of workers to run in rpc execution pool for HTTP requests",
		Value:   &c.cliConfig.JsonRPC.Http.ExecutionPoolSize,
		Default: c.cliConfig.JsonRPC.Http.ExecutionPoolSize,
		Group:   "JsonRPC",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "http.ep-requesttimeout",
		Usage:   "Request Timeout for rpc execution pool for HTTP requests",
		Value:   &c.cliConfig.JsonRPC.Http.ExecutionPoolRequestTimeout,
		Default: c.cliConfig.JsonRPC.Http.ExecutionPoolRequestTimeout,
		Group:   "JsonRPC",
	})

	// ws options
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "ws",
		Usage:   "Enable the WS-RPC server",
		Value:   &c.cliConfig.JsonRPC.Ws.Enabled,
		Default: c.cliConfig.JsonRPC.Ws.Enabled,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "ws.addr",
		Usage:   "WS-RPC server listening interface",
		Value:   &c.cliConfig.JsonRPC.Ws.Host,
		Default: c.cliConfig.JsonRPC.Ws.Host,
		Group:   "JsonRPC",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "ws.port",
		Usage:   "WS-RPC server listening port",
		Value:   &c.cliConfig.JsonRPC.Ws.Port,
		Default: c.cliConfig.JsonRPC.Ws.Port,
		Group:   "JsonRPC",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "ws.rpcprefix",
		Usage:   "HTTP path prefix on which JSON-RPC is served. Use '/' to serve on all paths.",
		Value:   &c.cliConfig.JsonRPC.Ws.Prefix,
		Default: c.cliConfig.JsonRPC.Ws.Prefix,
		Group:   "JsonRPC",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "ws.api",
		Usage:   "API's offered over the WS-RPC interface",
		Value:   &c.cliConfig.JsonRPC.Ws.API,
		Default: c.cliConfig.JsonRPC.Ws.API,
		Group:   "JsonRPC",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "ws.ep-size",
		Usage:   "Maximum size of workers to run in rpc execution pool for WS requests",
		Value:   &c.cliConfig.JsonRPC.Ws.ExecutionPoolSize,
		Default: c.cliConfig.JsonRPC.Ws.ExecutionPoolSize,
		Group:   "JsonRPC",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "ws.ep-requesttimeout",
		Usage:   "Request Timeout for rpc execution pool for WS requests",
		Value:   &c.cliConfig.JsonRPC.Ws.ExecutionPoolRequestTimeout,
		Default: c.cliConfig.JsonRPC.Ws.ExecutionPoolRequestTimeout,
		Group:   "JsonRPC",
	})

	// graphql options
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "graphql",
		Usage:   "Enable GraphQL on the HTTP-RPC server. Note that GraphQL can only be started if an HTTP server is started as well.",
		Value:   &c.cliConfig.JsonRPC.Graphql.Enabled,
		Default: c.cliConfig.JsonRPC.Graphql.Enabled,
		Group:   "JsonRPC",
	})

	// p2p options
	f.StringFlag(&flagset.StringFlag{
		Name:    "bind",
		Usage:   "Network binding address",
		Value:   &c.cliConfig.P2P.Bind,
		Default: c.cliConfig.P2P.Bind,
		Group:   "P2P",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "port",
		Usage:   "Network listening port",
		Value:   &c.cliConfig.P2P.Port,
		Default: c.cliConfig.P2P.Port,
		Group:   "P2P",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "bootnodes",
		Usage:   "Comma separated enode URLs for P2P discovery bootstrap",
		Value:   &c.cliConfig.P2P.Discovery.Bootnodes,
		Default: c.cliConfig.P2P.Discovery.Bootnodes,
		Group:   "P2P",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "maxpeers",
		Usage:   "Maximum number of network peers (network disabled if set to 0)",
		Value:   &c.cliConfig.P2P.MaxPeers,
		Default: c.cliConfig.P2P.MaxPeers,
		Group:   "P2P",
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "maxpendpeers",
		Usage:   "Maximum number of pending connection attempts",
		Value:   &c.cliConfig.P2P.MaxPendPeers,
		Default: c.cliConfig.P2P.MaxPendPeers,
		Group:   "P2P",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "nat",
		Usage:   "NAT port mapping mechanism (any|none|upnp|pmp|extip:<IP>)",
		Value:   &c.cliConfig.P2P.NAT,
		Default: c.cliConfig.P2P.NAT,
		Group:   "P2P",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "netrestrict",
		Usage:   "Restricts network communication to the given IP networks (CIDR masks)",
		Value:   &c.cliConfig.P2P.NetRestrict,
		Default: c.cliConfig.P2P.NetRestrict,
		Group:   "P2P",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "nodekey",
		Usage:   " P2P node key file",
		Value:   &c.cliConfig.P2P.NodeKey,
		Default: c.cliConfig.P2P.NodeKey,
		Group:   "P2P",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "nodekeyhex",
		Usage:   "P2P node key as hex",
		Value:   &c.cliConfig.P2P.NodeKeyHex,
		Default: c.cliConfig.P2P.NodeKeyHex,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "nodiscover",
		Usage:   "Disables the peer discovery mechanism (manual peer addition)",
		Value:   &c.cliConfig.P2P.NoDiscover,
		Default: c.cliConfig.P2P.NoDiscover,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "v4disc",
		Usage:   "Enables the V4 discovery mechanism",
		Value:   &c.cliConfig.P2P.Discovery.DiscoveryV4,
		Default: c.cliConfig.P2P.Discovery.DiscoveryV4,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "v5disc",
		Usage:   "Enables the V5 discovery mechanism",
		Value:   &c.cliConfig.P2P.Discovery.DiscoveryV5,
		Default: c.cliConfig.P2P.Discovery.DiscoveryV5,
		Group:   "P2P",
	})
	f.DurationFlag(&flagset.DurationFlag{
		Name:    "txarrivalwait",
		Usage:   "Maximum duration to wait for a transaction before explicitly requesting it",
		Value:   &c.cliConfig.P2P.TxArrivalWait,
		Default: c.cliConfig.P2P.TxArrivalWait,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "txannouncementonly",
		Usage:   "Whether to only announce transactions to peers",
		Value:   &c.cliConfig.P2P.TxAnnouncementOnly,
		Default: c.cliConfig.P2P.TxAnnouncementOnly,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "disable-tx-propagation",
		Usage:   "Disable transaction broadcast and announcements to all peers",
		Value:   &c.cliConfig.P2P.DisableTxPropagation,
		Default: c.cliConfig.P2P.DisableTxPropagation,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "p2p.nosnap",
		Usage:   "Disable serving snap sync requests to peers (snap/1 protocol is not advertised)",
		Value:   &c.cliConfig.P2P.NoSnapServing,
		Default: c.cliConfig.P2P.NoSnapServing,
		Group:   "P2P",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "discovery.dns",
		Usage:   "Comma separated list of enrtree:// URLs which will be queried for nodes to connect to",
		Value:   &c.cliConfig.P2P.Discovery.DNS,
		Default: c.cliConfig.P2P.Discovery.DNS,
		Group:   "P2P",
	})

	// metrics
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "metrics",
		Usage:   "Enable metrics collection and reporting",
		Value:   &c.cliConfig.Telemetry.Enabled,
		Default: c.cliConfig.Telemetry.Enabled,
		Group:   "Telemetry",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "metrics.expensive",
		Usage:   "Enable expensive metrics collection and reporting",
		Value:   &c.cliConfig.Telemetry.Expensive,
		Default: c.cliConfig.Telemetry.Expensive,
		Group:   "Telemetry",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "metrics.influxdb",
		Usage:   "Enable metrics export/push to an external InfluxDB database (v1)",
		Value:   &c.cliConfig.Telemetry.InfluxDB.V1Enabled,
		Default: c.cliConfig.Telemetry.InfluxDB.V1Enabled,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.endpoint",
		Usage:   "InfluxDB API endpoint to report metrics to",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Endpoint,
		Default: c.cliConfig.Telemetry.InfluxDB.Endpoint,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.database",
		Usage:   "InfluxDB database name to push reported metrics to",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Database,
		Default: c.cliConfig.Telemetry.InfluxDB.Database,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.username",
		Usage:   "Username to authorize access to the database",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Username,
		Default: c.cliConfig.Telemetry.InfluxDB.Username,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.password",
		Usage:   "Password to authorize access to the database",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Password,
		Default: c.cliConfig.Telemetry.InfluxDB.Password,
		Group:   "Telemetry",
	})
	f.MapStringFlag(&flagset.MapStringFlag{
		Name:    "metrics.influxdb.tags",
		Usage:   "Comma-separated InfluxDB tags (key/values) attached to all measurements",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Tags,
		Group:   "Telemetry",
		Default: c.cliConfig.Telemetry.InfluxDB.Tags,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.prometheus-addr",
		Usage:   "Address for Prometheus Server",
		Value:   &c.cliConfig.Telemetry.PrometheusAddr,
		Default: c.cliConfig.Telemetry.PrometheusAddr,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.opencollector-endpoint",
		Usage:   "OpenCollector Endpoint (host:port)",
		Value:   &c.cliConfig.Telemetry.OpenCollectorEndpoint,
		Default: c.cliConfig.Telemetry.OpenCollectorEndpoint,
		Group:   "Telemetry",
	})
	// influx db v2
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "metrics.influxdbv2",
		Usage:   "Enable metrics export/push to an external InfluxDB v2 database",
		Value:   &c.cliConfig.Telemetry.InfluxDB.V2Enabled,
		Default: c.cliConfig.Telemetry.InfluxDB.V2Enabled,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.token",
		Usage:   "Token to authorize access to the database (v2 only)",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Token,
		Default: c.cliConfig.Telemetry.InfluxDB.Token,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.bucket",
		Usage:   "InfluxDB bucket name to push reported metrics to (v2 only)",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Bucket,
		Default: c.cliConfig.Telemetry.InfluxDB.Bucket,
		Group:   "Telemetry",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "metrics.influxdb.organization",
		Usage:   "InfluxDB organization name (v2 only)",
		Value:   &c.cliConfig.Telemetry.InfluxDB.Organization,
		Default: c.cliConfig.Telemetry.InfluxDB.Organization,
		Group:   "Telemetry",
	})

	// account
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "unlock",
		Usage:   "Comma separated list of accounts to unlock",
		Value:   &c.cliConfig.Accounts.Unlock,
		Default: c.cliConfig.Accounts.Unlock,
		Group:   "Account Management",
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "password",
		Usage:   "Password file to use for non-interactive password input",
		Value:   &c.cliConfig.Accounts.PasswordFile,
		Default: c.cliConfig.Accounts.PasswordFile,
		Group:   "Account Management",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "allow-insecure-unlock",
		Usage:   "Allow insecure account unlocking when account-related RPCs are exposed by http",
		Value:   &c.cliConfig.Accounts.AllowInsecureUnlock,
		Default: c.cliConfig.Accounts.AllowInsecureUnlock,
		Group:   "Account Management",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "lightkdf",
		Usage:   "Reduce key-derivation RAM & CPU usage at some expense of KDF strength",
		Value:   &c.cliConfig.Accounts.UseLightweightKDF,
		Default: c.cliConfig.Accounts.UseLightweightKDF,
		Group:   "Account Management",
	})
	f.BoolFlag((&flagset.BoolFlag{
		Name:    "disable-bor-wallet",
		Usage:   "Disable the personal wallet endpoints",
		Value:   &c.cliConfig.Accounts.DisableBorWallet,
		Default: c.cliConfig.Accounts.DisableBorWallet,
	}))

	// grpc
	f.StringFlag(&flagset.StringFlag{
		Name:    "grpc.addr",
		Usage:   "Address and port to bind the GRPC server. Empty disables the server. Non-loopback binds without --grpc.token log a startup warning.",
		Value:   &c.cliConfig.GRPC.Addr,
		Default: c.cliConfig.GRPC.Addr,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "grpc.token",
		Usage:   "Raw token expected in the `authorization: Bearer <token>` header of incoming gRPC calls (empty disables auth; the `Bearer ` prefix is stripped before comparison). Prefer the BOR_GRPC_TOKEN environment variable over this flag.",
		Value:   &c.cliConfig.GRPC.Token,
		Default: c.cliConfig.GRPC.Token,
	})

	// developer
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "dev",
		Usage:   "Enable developer mode with ephemeral proof-of-authority network and a pre-funded developer account, mining enabled",
		Value:   &c.cliConfig.Developer.Enabled,
		Default: c.cliConfig.Developer.Enabled,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "dev.period",
		Usage:   "Block period to use in developer mode (0 = mine only if transaction pending)",
		Value:   &c.cliConfig.Developer.Period,
		Default: c.cliConfig.Developer.Period,
	})

	// parallelevm
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "parallelevm.enable",
		Usage:   "Enable Block STM",
		Value:   &c.cliConfig.ParallelEVM.Enable,
		Default: c.cliConfig.ParallelEVM.Enable,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "parallelevm.procs",
		Usage:   "Number of speculative processes (cores) in Block STM",
		Value:   &c.cliConfig.ParallelEVM.SpeculativeProcesses,
		Default: c.cliConfig.ParallelEVM.SpeculativeProcesses,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "parallelevm.enforce",
		Usage:   "Enforce block processing via Block STM",
		Value:   &c.cliConfig.ParallelEVM.Enforce,
		Default: c.cliConfig.ParallelEVM.Enforce,
	})

	// Witness Protocol Flags
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "witness.enable",
		Usage:   "Enable witness protocol",
		Value:   &c.cliConfig.Witness.Enable,
		Default: c.cliConfig.Witness.Enable,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "witness.syncwithwitnesses",
		Usage:   "Sync blocks with witnesses",
		Value:   &c.cliConfig.Witness.SyncWithWitnesses,
		Default: c.cliConfig.Witness.SyncWithWitnesses,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "witness.producewitnesses",
		Usage:   "Produce witnesses while syncing",
		Value:   &c.cliConfig.Witness.ProduceWitnesses,
		Default: c.cliConfig.Witness.ProduceWitnesses,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "witness.parallelstatelessimport",
		Usage:   "Enable parallel stateless block import",
		Value:   &c.cliConfig.Witness.EnableParallelStatelessImport,
		Default: c.cliConfig.Witness.EnableParallelStatelessImport,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "witness.parallelstatelessimportworkers",
		Usage:   "Number of workers to use for parallel stateless import (0 = GOMAXPROCS)",
		Value:   &c.cliConfig.Witness.ParallelStatelessImportWorkers,
		Default: c.cliConfig.Witness.ParallelStatelessImportWorkers,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "witness.witnessapi",
		Usage:   "Enable witness API endpoints (by default disabled)",
		Value:   &c.cliConfig.Witness.WitnessAPI,
		Default: c.cliConfig.Witness.WitnessAPI,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "witness.filestore",
		Usage:   "Store witness blobs on the filesystem instead of the key-value database",
		Value:   &c.cliConfig.Witness.FileStore,
		Default: c.cliConfig.Witness.FileStore,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "witness.fastforwardthreshold",
		Usage:   "Minimum necessary distance between local header and chain tip to trigger fast forward",
		Value:   &c.cliConfig.Witness.FastForwardThreshold,
		Default: c.cliConfig.Witness.FastForwardThreshold,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "dev.gaslimit",
		Usage:   "Initial block gas limit",
		Value:   &c.cliConfig.Developer.GasLimit,
		Default: c.cliConfig.Developer.GasLimit,
	})

	// pprof
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "pprof",
		Usage:   "Enable the pprof HTTP server",
		Value:   &c.cliConfig.Pprof.Enabled,
		Default: c.cliConfig.Pprof.Enabled,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "pprof.port",
		Usage:   "pprof HTTP server listening port",
		Value:   &c.cliConfig.Pprof.Port,
		Default: c.cliConfig.Pprof.Port,
	})
	f.StringFlag(&flagset.StringFlag{
		Name:    "pprof.addr",
		Usage:   "pprof HTTP server listening interface",
		Value:   &c.cliConfig.Pprof.Addr,
		Default: c.cliConfig.Pprof.Addr,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "pprof.memprofilerate",
		Usage:   "Turn on memory profiling with the given rate",
		Value:   &c.cliConfig.Pprof.MemProfileRate,
		Default: c.cliConfig.Pprof.MemProfileRate,
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "pprof.blockprofilerate",
		Usage:   "Turn on block profiling with the given rate",
		Value:   &c.cliConfig.Pprof.BlockProfileRate,
		Default: c.cliConfig.Pprof.BlockProfileRate,
	})
	// Historical data retention related flags
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "history.transactions",
		Usage:   "Number of recent blocks to maintain transactions index for (default = about 2 months, 0 = entire chain)",
		Value:   &c.cliConfig.History.TransactionHistory,
		Default: c.cliConfig.History.TransactionHistory,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "history.logs",
		Usage:   "Number of recent blocks to maintain log search index for (default = about 2 months, 0 = entire chain)",
		Value:   &c.cliConfig.History.LogHistory,
		Default: c.cliConfig.History.LogHistory,
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "history.logs.disable",
		Usage:   "Do not maintain log search index",
		Value:   &c.cliConfig.History.LogNoHistory,
		Default: c.cliConfig.History.LogNoHistory,
	})
	f.Uint64Flag(&flagset.Uint64Flag{
		Name:    "history.state",
		Usage:   "Number of recent blocks to retain state history for, only relevant in state.scheme=path (default = 90,000 blocks, 0 = entire chain)",
		Value:   &c.cliConfig.History.StateHistory,
		Default: c.cliConfig.History.StateHistory,
	})

	// Health check related flags
	f.IntFlag(&flagset.IntFlag{
		Name:    "health.max-goroutine-threshold",
		Usage:   "Maximum number of goroutines before health check fails (0 = disabled)",
		Value:   &c.cliConfig.Health.MaxGoRoutineThreshold,
		Default: c.cliConfig.Health.MaxGoRoutineThreshold,
		Group:   "Health",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "health.warn-goroutine-threshold",
		Usage:   "Maximum number of goroutines before health check warns (0 = disabled)",
		Value:   &c.cliConfig.Health.WarnGoRoutineThreshold,
		Default: c.cliConfig.Health.WarnGoRoutineThreshold,
		Group:   "Health",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "health.min-peer-threshold",
		Usage:   "Minimum number of peers before health check fails (0 = disabled)",
		Value:   &c.cliConfig.Health.MinPeerThreshold,
		Default: c.cliConfig.Health.MinPeerThreshold,
		Group:   "Health",
	})
	f.IntFlag(&flagset.IntFlag{
		Name:    "health.warn-peer-threshold",
		Usage:   "Minimum number of peers before health check warns (0 = disabled)",
		Value:   &c.cliConfig.Health.WarnPeerThreshold,
		Default: c.cliConfig.Health.WarnPeerThreshold,
		Group:   "Health",
	})

	// Relay related flags
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "relay.enable-preconfs",
		Usage:   "Enable transaction preconfirmations",
		Value:   &c.cliConfig.Relay.EnablePreconfs,
		Default: c.cliConfig.Relay.EnablePreconfs,
		Group:   "P2P",
	})
	f.BoolFlag(&flagset.BoolFlag{
		Name:    "relay.enable-private-tx",
		Usage:   "Enable private transaction submission",
		Value:   &c.cliConfig.Relay.EnablePrivateTx,
		Default: c.cliConfig.Relay.EnablePrivateTx,
		Group:   "P2P",
	})
	f.SliceStringFlag(&flagset.SliceStringFlag{
		Name:    "relay.bp-rpc-endpoints",
		Usage:   "Comma separated rpc endpoints of all block producers",
		Value:   &c.cliConfig.Relay.BlockProducerRpcEndpoints,
		Default: c.cliConfig.Relay.BlockProducerRpcEndpoints,
		Group:   "P2P",
	})

	return f
}
