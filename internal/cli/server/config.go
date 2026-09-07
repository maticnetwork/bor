package server

import (
	"crypto/ecdsa"
	"fmt"

	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	godebug "runtime/debug"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/imdario/mergo"
	"github.com/mitchellh/go-homedir"
	gopsutil "github.com/shirou/gopsutil/mem"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/fdlimit"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/internal/cli/server/chains"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

type Config struct {
	chain *chains.Chain

	// Chain is the chain to sync with
	Chain string `hcl:"chain,optional" toml:"chain,optional"`

	// Identity of the node
	Identity string `hcl:"identity,optional" toml:"identity,optional"`

	// RequiredBlocks is a list of required (block number, hash) pairs to accept
	RequiredBlocks map[string]string `hcl:"eth.requiredblocks,optional" toml:"eth.requiredblocks,optional"`

	// Verbosity is the level of the logs to put out
	Verbosity int `hcl:"verbosity,optional" toml:"verbosity,optional"`

	// Record information useful for VM and contract debugging
	EnablePreimageRecording bool `hcl:"vmdebug,optional" toml:"vmdebug,optional"`

	// VMTrace enables live VM tracing at startup
	VMTrace string `hcl:"vmtrace,optional" toml:"vmtrace,optional"`

	// VMTraceJsonConfig is the JSON config for the VM tracer
	VMTraceJsonConfig string `hcl:"vmtrace.jsonconfig,optional" toml:"vmtrace.jsonconfig,optional"`

	// Use switch-based fast path EVM interpreter
	EnableEVMSwitchDispatch bool `hcl:"evm-switch-dispatch,optional" toml:"evm-switch-dispatch,optional"`

	// Enable state size tracking
	StateSizeTracking bool `hcl:"state.size-tracking,optional" toml:"state.size-tracking,optional"`

	// DataDir is the directory to store the state in
	DataDir string `hcl:"datadir,optional" toml:"datadir,optional"`

	// Ancient is the directory to store the state in
	Ancient string `hcl:"ancient,optional" toml:"ancient,optional"`

	// DBEngine is used to select leveldb or pebble as database
	DBEngine string `hcl:"db.engine,optional" toml:"db.engine,optional"`

	// KeyStoreDir is the directory to store keystores
	KeyStoreDir string `hcl:"keystore,optional" toml:"keystore,optional"`

	// Maximum number of requests in a batch (default=1000, use 0 for no limits)
	BatchRequestLimit int `hcl:"rpc.batch-request-limit,optional" toml:"rpc.batch-request-limit,optional"`

	// Maximum number of response bytes across all requests in a batch (default=25MB, use 0 for no limits)
	BatchResponseMaxSize int `hcl:"rpc.batch-response-max-size,optional" toml:"rpc.batch-response-max-size,optional"`

	// Maximum size (in bytes) a result of an rpc request could have (default=100000, use 0 for no limits)
	RPCReturnDataLimit uint64 `hcl:"rpc.returndatalimit,optional" toml:"rpc.returndatalimit,optional"`

	// SyncMode selects the sync protocol
	SyncMode string `hcl:"syncmode,optional" toml:"syncmode,optional"`

	// GcMode selects the garbage collection mode for the trie
	GcMode string `hcl:"gcmode,optional" toml:"gcmode,optional"`

	// state.scheme selects the Scheme to use for storing ethereum state ('hash' or 'path')
	StateScheme string `hcl:"state.scheme,optional" toml:"state.scheme,optional"`

	// Snapshot enables the snapshot database mode
	Snapshot bool `hcl:"snapshot,optional" toml:"snapshot,optional"`

	// BorLogs enables bor log retrieval
	BorLogs bool `hcl:"bor.logs,optional" toml:"bor.logs,optional"`

	// Ethstats is the address of the ethstats server to send telemetry
	Ethstats string `hcl:"ethstats,optional" toml:"ethstats,optional"`

	// DisableBlindForkValidation disables additional fork validation and accept blind forks without tracing back to last whitelisted entry
	DisableBlindForkValidation bool `hcl:"disable-blind-fork-validation,optional" toml:"disable-blind-fork-validation,optional"`

	// MaxBlindForkValidationLimit denotes the maximum number of blocks to traverse back in the database when validating blind forks
	MaxBlindForkValidationLimit uint64 `hcl:"max-blind-fork-validation-limit,optional" toml:"max-blind-fork-validation-limit,optional"`

	// Logging has the logging related settings
	Logging *LoggingConfig `hcl:"log,block" toml:"log,block"`

	// P2P has the p2p network related settings
	P2P *P2PConfig `hcl:"p2p,block" toml:"p2p,block"`

	// Heimdall has the heimdall connection related settings
	Heimdall *HeimdallConfig `hcl:"heimdall,block" toml:"heimdall,block"`

	// TxPool has the transaction pool related settings
	TxPool *TxPoolConfig `hcl:"txpool,block" toml:"txpool,block"`

	// Sealer has the validator related settings
	Sealer *SealerConfig `hcl:"miner,block" toml:"miner,block"`

	// JsonRPC has the json-rpc related settings
	JsonRPC *JsonRPCConfig `hcl:"jsonrpc,block" toml:"jsonrpc,block"`

	// Gpo has the gas price oracle related settings
	Gpo *GpoConfig `hcl:"gpo,block" toml:"gpo,block"`

	// Telemetry has the telemetry related settings
	Telemetry *TelemetryConfig `hcl:"telemetry,block" toml:"telemetry,block"`

	// Cache has the cache related settings
	Cache *CacheConfig `hcl:"cache,block" toml:"cache,block"`

	ExtraDB *ExtraDBConfig `hcl:"leveldb,block" toml:"leveldb,block"`

	// Account has the validator account related settings
	Accounts *AccountsConfig `hcl:"accounts,block" toml:"accounts,block"`

	// GRPC has the grpc server related settings
	GRPC *GRPCConfig `hcl:"grpc,block" toml:"grpc,block"`

	// Developer has the developer mode related settings
	Developer *DeveloperConfig `hcl:"developer,block" toml:"developer,block"`

	// ParallelEVM has the parallel evm related settings
	ParallelEVM *ParallelEVMConfig `hcl:"parallelevm,block" toml:"parallelevm,block"`

	// Witness has the witness related settings
	Witness *WitnessConfig `hcl:"witness,block" toml:"witness,block"`

	// Develop Fake Author mode to produce blocks without authorisation
	DevFakeAuthor bool `hcl:"devfakeauthor,optional" toml:"devfakeauthor,optional"`

	// Pprof has the pprof related settings
	Pprof *PprofConfig `hcl:"pprof,block" toml:"pprof,block"`

	// HistoryConfig has historical data retention related settings
	History *HistoryConfig `hcl:"history,block" toml:"history,block"`

	// HealthConfig has health check related settings
	Health *HealthConfig `hcl:"health,block" toml:"health,block"`

	// Relay has transaction relay related settings
	Relay *RelayConfig `hcl:"relay,block" toml:"relay,block"`

	// Pipeline has pipelined SRC settings for block import
	Pipeline *PipelineConfig `hcl:"pipeline,block" toml:"pipeline,block"`
}

type HistoryConfig struct {
	// TransactionHistory denotes the maximum number of blocks from head whose tx indices are reserved.
	TransactionHistory uint64 `hcl:"transactions,block" toml:"transactions,block"`

	// LogHistory denotes the maximum number of blocks from head where a log search index is maintained.
	LogHistory uint64 `hcl:"logs,block" toml:"logs,block"`

	// LogNoHistory denotes whether log search index is maintained or not.
	LogNoHistory bool `hcl:"logs.disable,block" toml:"logs.disable,block"`

	// StateHistory denotes number of recent blocks to retain state history for (only relevant
	// in state.scheme=path)
	StateHistory uint64 `hcl:"state,block" toml:"state,block"`
}

type HealthConfig struct {
	// MaxGoRoutineThreshold is the maximum number of goroutines before bor health check fails.
	MaxGoRoutineThreshold int `hcl:"max_goroutine_threshold,optional" toml:"max_goroutine_threshold,optional"`

	// WarnGoRoutineThreshold is the maximum number of goroutines before bor health check warns.
	WarnGoRoutineThreshold int `hcl:"warn_goroutine_threshold,optional" toml:"warn_goroutine_threshold,optional"`

	// MinPeerThreshold is the minimum number of peers before bor health check fails.
	MinPeerThreshold int `hcl:"min_peer_threshold,optional" toml:"min_peer_threshold,optional"`

	// WarnPeerThreshold is the minimum number of peers before bor health check warns.
	WarnPeerThreshold int `hcl:"warn_peer_threshold,optional" toml:"warn_peer_threshold,optional"`
}

type LoggingConfig struct {
	// Per-module verbosity: comma-separated list of <pattern>=<level> (e.g. eth/*=5,p2p=4)
	Vmodule string `hcl:"vmodule,optional" toml:"vmodule,optional"`

	// Format logs with JSON
	Json bool `hcl:"json,optional" toml:"json,optional"`

	// Request a stack trace at a specific logging statement (e.g. "block.go:271")
	Backtrace string `hcl:"backtrace,optional" toml:"backtrace,optional"`

	// Prepends log messages with call-site location (file and line number)
	Debug bool `hcl:"debug,optional" toml:"debug,optional"`

	// EnableBlockTracking allows logging of information collected while tracking block lifecycle
	EnableBlockTracking bool `hcl:"enable-block-tracking,optional" toml:"enable-block-tracking,optional"`

	// TODO - implement this
	// // Write execution trace to the given file
	// Trace string `hcl:"trace,optional" toml:"trace,optional"`
}

type PprofConfig struct {
	// Enableed enable the pprof HTTP server
	Enabled bool `hcl:"pprof,optional" toml:"pprof,optional"`

	// pprof HTTP server listening port
	Port int `hcl:"port,optional" toml:"port,optional"`

	// pprof HTTP server listening interface
	Addr string `hcl:"addr,optional" toml:"addr,optional"`

	// Turn on memory profiling with the given rate
	MemProfileRate int `hcl:"memprofilerate,optional" toml:"memprofilerate,optional"`

	// Turn on block profiling with the given rate
	BlockProfileRate int `hcl:"blockprofilerate,optional" toml:"blockprofilerate,optional"`
}

type P2PConfig struct {
	// MaxPeers sets the maximum number of connected peers
	MaxPeers uint64 `hcl:"maxpeers,optional" toml:"maxpeers,optional"`

	// MaxPendPeers sets the maximum number of pending connected peers
	MaxPendPeers uint64 `hcl:"maxpendpeers,optional" toml:"maxpendpeers,optional"`

	// Bind is the bind address
	Bind string `hcl:"bind,optional" toml:"bind,optional"`

	// Port is the port number
	Port uint64 `hcl:"port,optional" toml:"port,optional"`

	// NoDiscover is used to disable discovery
	NoDiscover bool `hcl:"nodiscover,optional" toml:"nodiscover,optional"`

	// NAT it used to set NAT options
	NAT string `hcl:"nat,optional" toml:"nat,optional"`

	// Connectivity can be restricted to certain IP networks.
	// If this option is set to a non-nil value, only hosts which match one of the
	// IP networks contained in the list are considered.
	NetRestrict string `hcl:"netrestrict,optional" toml:"netrestrict,optional"`

	// P2P node key file
	NodeKey string `hcl:"nodekey,optional" toml:"nodekey,optional"`

	// P2P node key as hex
	NodeKeyHex string `hcl:"nodekeyhex,optional" toml:"nodekeyhex,optional"`

	// Discovery has the p2p discovery related settings
	Discovery *P2PDiscovery `hcl:"discovery,block" toml:"discovery,block"`

	// TxArrivalWait sets the maximum duration the transaction fetcher will wait for
	// an announced transaction to arrive before explicitly requesting it
	TxArrivalWait    time.Duration `hcl:"-,optional" toml:"-"`
	TxArrivalWaitRaw string        `hcl:"txarrivalwait,optional" toml:"txarrivalwait,optional"`

	// TxAnnouncementOnly is used to only announce transactions to peers
	TxAnnouncementOnly bool `hcl:"txannouncementonly,optional" toml:"txannouncementonly,optional"`

	// DisableTxPropagation disables transaction broadcast and announcement completely to its peers
	DisableTxPropagation bool `hcl:"disable-tx-propagation,optional" toml:"disable-tx-propagation,optional"`

	// NoSnapServing disables serving snap sync requests to peers (snap/1 protocol is not advertised)
	NoSnapServing bool `hcl:"nosnap,optional" toml:"nosnap,optional"`
}

type P2PDiscovery struct {
	// DiscoveryV4 specifies whether V4 discovery should be started.
	DiscoveryV4 bool `hcl:"v4disc,optional" toml:"v4disc,optional"`

	// DiscoveryV5 specifies whether the new topic-discovery based V5 discovery
	// protocol should be started or not.
	DiscoveryV5 bool `hcl:"v5disc,optional" toml:"v5disc,optional"`

	// Bootnodes is the list of initial bootnodes
	Bootnodes []string `hcl:"bootnodes,optional" toml:"bootnodes,optional"`

	// BootnodesV4 is the list of initial v4 bootnodes
	BootnodesV4 []string `hcl:"bootnodesv4,optional" toml:"bootnodesv4,optional"`

	// BootnodesV5 is the list of initial v5 bootnodes
	BootnodesV5 []string `hcl:"bootnodesv5,optional" toml:"bootnodesv5,optional"`

	// StaticNodes is the list of static nodes
	StaticNodes []string `hcl:"static-nodes,optional" toml:"static-nodes,optional"`

	// TrustedNodes is the list of trusted nodes
	TrustedNodes []string `hcl:"trusted-nodes,optional" toml:"trusted-nodes,optional"`

	// DNS is the list of enrtree:// URLs which will be queried for nodes to connect to
	DNS []string `hcl:"dns,optional" toml:"dns,optional"`
}

type HeimdallConfig struct {
	// URL is the url of the heimdall server (comma-separated for failover: "url1,url2,url3")
	URL string `hcl:"url,optional" toml:"url,optional"`

	Timeout time.Duration `hcl:"timeout,optional" toml:"timeout,optional"`

	// Without is used to disable remote heimdall during testing
	Without bool `hcl:"bor.without,optional" toml:"bor.without,optional"`

	// GRPCAddress is the address of the heimdall grpc server (comma-separated for failover: "addr1,addr2")
	GRPCAddress string `hcl:"grpc-address,optional" toml:"grpc-address,optional"`

	// WSAddress is the address of the heimdall ws subscription server (comma-separated for failover: "addr1,addr2")
	WSAddress string `hcl:"ws-address,optional" toml:"ws-address,optional"`

	// RunHeimdall is used to run heimdall as a child process
	RunHeimdall bool `hcl:"bor.runheimdall,optional" toml:"bor.runheimdall,optional"`

	// RunHeimdal args are the arguments to run heimdall with
	RunHeimdallArgs string `hcl:"bor.runheimdallargs,optional" toml:"bor.runheimdallargs,optional"`

	// UseHeimdallApp is used to fetch data from heimdall app when running heimdall as a child process
	UseHeimdallApp bool `hcl:"bor.useheimdallapp,optional" toml:"bor.useheimdallapp,optional"`
}

type TxPoolConfig struct {
	// Locals are the addresses that should be treated by default as local
	Locals []string `hcl:"locals,optional" toml:"locals,optional"`

	// NoLocals enables whether local transaction handling should be disabled
	NoLocals bool `hcl:"nolocals,optional" toml:"nolocals,optional"`

	// Journal is the path to store local transactions to survive node restarts
	Journal string `hcl:"journal,optional" toml:"journal,optional"`

	// Rejournal is the time interval to regenerate the local transaction journal
	Rejournal    time.Duration `hcl:"-,optional" toml:"-"`
	RejournalRaw string        `hcl:"rejournal,optional" toml:"rejournal,optional"`

	// PriceLimit is the minimum gas price to enforce for acceptance into the pool
	PriceLimit uint64 `hcl:"pricelimit,optional" toml:"pricelimit,optional"`

	// PriceBump is the minimum price bump percentage to replace an already existing transaction (nonce)
	PriceBump uint64 `hcl:"pricebump,optional" toml:"pricebump,optional"`

	// AccountSlots is the number of executable transaction slots guaranteed per account
	AccountSlots uint64 `hcl:"accountslots,optional" toml:"accountslots,optional"`

	// GlobalSlots is the maximum number of executable transaction slots for all accounts
	GlobalSlots uint64 `hcl:"globalslots,optional" toml:"globalslots,optional"`

	// AccountQueue is the maximum number of non-executable transaction slots permitted per account
	AccountQueue uint64 `hcl:"accountqueue,optional" toml:"accountqueue,optional"`

	// GlobalQueueis the maximum number of non-executable transaction slots for all accounts
	GlobalQueue uint64 `hcl:"globalqueue,optional" toml:"globalqueue,optional"`

	// lifetime is the maximum amount of time non-executable transaction are queued
	LifeTime    time.Duration `hcl:"-,optional" toml:"-"`
	LifeTimeRaw string        `hcl:"lifetime,optional" toml:"lifetime,optional"`

	// FilteredAddressesFile is the path to newline-separated list of addresses whose transactions will be filtered
	FilteredAddressesFile string `hcl:"filtered-addresses,optional" toml:"filtered-addresses,optional"`

	// Rebroadcast enables the stuck transaction rebroadcast mechanism
	Rebroadcast bool `hcl:"rebroadcast,optional" toml:"rebroadcast,optional"`

	// RebroadcastInterval is the interval between rebroadcast checks
	RebroadcastInterval    time.Duration `hcl:"-,optional" toml:"-"`
	RebroadcastIntervalRaw string        `hcl:"rebroadcast-interval,optional" toml:"rebroadcast-interval,optional"`

	// RebroadcastMaxAge is the maximum age for rebroadcast eligibility
	RebroadcastMaxAge    time.Duration `hcl:"-,optional" toml:"-"`
	RebroadcastMaxAgeRaw string        `hcl:"rebroadcast-max-age,optional" toml:"rebroadcast-max-age,optional"`

	// RebroadcastBatchSize is the maximum number of transactions per rebroadcast cycle
	RebroadcastBatchSize int `hcl:"rebroadcast-batch-size,optional" toml:"rebroadcast-batch-size,optional"`
}

type SealerConfig struct {
	// Enabled is used to enable validator mode
	Enabled bool `hcl:"mine,optional" toml:"mine,optional"`

	// AllowGasTipOverride allows a block producer to override the miner gas tip
	AllowGasTipOverride bool `hcl:"allow-gas-tip-override,optional" toml:"allow-gas-tip-override,optional"`

	// Etherbase is the address of the validator
	Etherbase string `hcl:"etherbase,optional" toml:"etherbase,optional"`

	// ExtraData is the block extra data set by the miner
	ExtraData string `hcl:"extradata,optional" toml:"extradata,optional"`

	// GasCeil is the target gas ceiling for mined blocks.
	GasCeil uint64 `hcl:"gaslimit,optional" toml:"gaslimit,optional"`

	// Dynamic gas limit configuration
	EnableDynamicGasLimit bool   `hcl:"enableDynamicGasLimit,optional" toml:"enableDynamicGasLimit,optional"`
	GasLimitMin           uint64 `hcl:"gasLimitMin,optional" toml:"gasLimitMin,optional"`
	GasLimitMax           uint64 `hcl:"gasLimitMax,optional" toml:"gasLimitMax,optional"`
	TargetBaseFee         uint64 `hcl:"targetBaseFee,optional" toml:"targetBaseFee,optional"`
	BaseFeeBuffer         uint64 `hcl:"baseFeeBuffer,optional" toml:"baseFeeBuffer,optional"`

	// Dynamic target gas percentage configuration (post-Lisovo, mutually exclusive with EnableDynamicGasLimit)
	// Shares TargetBaseFee and BaseFeeBuffer with dynamic gas limit configuration.
	EnableDynamicTargetGas bool   `hcl:"enableDynamicTargetGas,optional" toml:"enableDynamicTargetGas,optional"`
	TargetGasMinPercentage uint64 `hcl:"targetGasMinPercentage,optional" toml:"targetGasMinPercentage,optional"`
	TargetGasMaxPercentage uint64 `hcl:"targetGasMaxPercentage,optional" toml:"targetGasMaxPercentage,optional"`

	// GasPrice is the minimum gas price for mining a transaction
	GasPrice    *big.Int `hcl:"-,optional" toml:"-"`
	GasPriceRaw string   `hcl:"gasprice,optional" toml:"gasprice,optional"`

	// The time interval for miner to re-create mining work.
	Recommit    time.Duration `hcl:"-,optional" toml:"-"`
	RecommitRaw string        `hcl:"recommit,optional" toml:"recommit,optional"`

	CommitInterruptFlag bool `hcl:"commitinterrupt,optional" toml:"commitinterrupt,optional"`

	// BlockTime is the block time defined by the miner. Needs to be larger or equal to the consensus block time. If not set (default = 0), the miner will use the consensus block time.
	BlockTime    time.Duration `hcl:"-,optional" toml:"-"`
	BlockTimeRaw string        `hcl:"blocktime,optional" toml:"blocktime,optional"`

	// TargetGasPercentage is the target gas as percentage of gas limit (1-100, default 65) for post-Lisovo blocks
	TargetGasPercentage uint64 `hcl:"target-gas-percentage,optional" toml:"target-gas-percentage,optional"`

	// BaseFeeChangeDenominator is the base fee change rate (must be >0, default 64) for post-Lisovo blocks
	BaseFeeChangeDenominator uint64 `hcl:"base-fee-change-denominator,optional" toml:"base-fee-change-denominator,optional"`

	// EnablePrefetch enables transaction prefetching from pool during block building
	EnablePrefetch bool `hcl:"prefetch,optional" toml:"prefetch,optional"`

	// PrefetchGasLimitPercent is the gas limit percentage for prefetching (e.g., 100 = 100%, 110 = 110%)
	PrefetchGasLimitPercent uint64 `hcl:"prefetch-gaslimit-percent,optional" toml:"prefetch-gaslimit-percent,optional"`

	// DisablePendingBlock disables the pending block creation loop on non block producer nodes. When
	// set, 'pending' block will be unavailable for RPC queries. This won't apply for block producer
	// nodes.
	DisablePendingBlock bool `hcl:"disable-pending-block,optional" toml:"disable-pending-block,optional"`
}

type JsonRPCConfig struct {
	// IPCDisable enables whether ipc is enabled or not
	IPCDisable bool `hcl:"ipcdisable,optional" toml:"ipcdisable,optional"`

	// IPCPath is the path of the ipc endpoint
	IPCPath string `hcl:"ipcpath,optional" toml:"ipcpath,optional"`

	// GasCap is the global gas cap for eth-call variants.
	GasCap uint64 `hcl:"gascap,optional" toml:"gascap,optional"`

	// Sets a timeout used for eth_call (0=infinite)
	RPCEVMTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	RPCEVMTimeoutRaw string        `hcl:"evmtimeout,optional" toml:"evmtimeout,optional"`

	// TxFeeCap is the global transaction fee cap for send-transaction variants
	TxFeeCap float64 `hcl:"txfeecap,optional" toml:"txfeecap,optional"`

	// LogQueryLimit is the max number of addresses or topics allowed in filter criteria for eth_getLogs.
	LogQueryLimit int `hcl:"logquerylimit,optional" toml:"logquerylimit,optional"`

	// RangeLimit is the maximum block range allowed for eth_getLogs and bor_getLogs (0 = no limit).
	RangeLimit uint64 `hcl:"rangelimit,optional" toml:"rangelimit,optional"`

	// Http has the json-rpc http related settings
	Http *APIConfig `hcl:"http,block" toml:"http,block"`

	// Ws has the json-rpc websocket related settings
	Ws *APIConfig `hcl:"ws,block" toml:"ws,block"`

	// Graphql has the json-rpc graphql related settings
	Graphql *APIConfig `hcl:"graphql,block" toml:"graphql,block"`

	// AUTH RPC related settings
	Auth *AUTHConfig `hcl:"auth,block" toml:"auth,block"`

	HttpTimeout *HttpTimeouts `hcl:"timeouts,block" toml:"timeouts,block"`

	AllowUnprotectedTxs bool `hcl:"allow-unprotected-txs,optional" toml:"allow-unprotected-txs,optional"`

	// EnablePersonal enables the deprecated personal namespace.
	EnablePersonal bool `hcl:"enabledeprecatedpersonal,optional" toml:"enabledeprecatedpersonal,optional"`

	// EnableTrace enables the Parity-compatible trace namespace (trace_block,
	// trace_transaction, trace_call, trace_callMany, trace_replayTransaction,
	// trace_replayBlockTransactions). Disabled by default.
	EnableTrace bool `hcl:"enabletrace,optional" toml:"enabletrace,optional"`

	// Default timeout for eth_sendRawTransactionSync (e.g. 2s, 500ms)
	TxSyncDefaultTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	TxSyncDefaultTimeoutRaw string        `hcl:"txsync.defaulttimeout,optional" toml:"txsync.defaulttimeout,optional"`

	// Maximum allowed timeout for eth_sendRawTransactionSync (e.g. 5m)
	TxSyncMaxTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	TxSyncMaxTimeoutRaw string        `hcl:"txsync.maxtimeout,optional" toml:"txsync.maxtimeout,optional"`

	// AcceptPreconfTx allows the RPC server to accept preconf transactions
	AcceptPreconfTx bool `hcl:"accept-preconf-tx,optional" toml:"accept-preconf-tx,optional"`

	// AcceptPrivateTx allows the RPC server to accept private transactions
	AcceptPrivateTx bool `hcl:"accept-private-tx,optional" toml:"accept-private-tx,optional"`
}

type AUTHConfig struct {
	// JWTSecret is the hex-encoded jwt secret.
	JWTSecret string `hcl:"jwtsecret,optional" toml:"jwtsecret,optional"`

	// Addr is the listening address on which authenticated APIs are provided.
	Addr string `hcl:"addr,optional" toml:"addr,optional"`

	// Port is the port number on which authenticated APIs are provided.
	Port uint64 `hcl:"port,optional" toml:"port,optional"`

	// VHosts is the list of virtual hostnames which are allowed on incoming requests
	// for the authenticated api. This is by default {'localhost'}.
	VHosts []string `hcl:"vhosts,optional" toml:"vhosts,optional"`
}

type GRPCConfig struct {
	// Addr is the bind address for the grpc rpc server
	Addr string `hcl:"addr,optional" toml:"addr,optional"`
	// Token is the bearer token required for incoming gRPC calls; empty disables auth
	Token string `hcl:"token,optional" toml:"token,optional"`
}

type APIConfig struct {
	// Enabled selects whether the api is enabled
	Enabled bool `hcl:"enabled,optional" toml:"enabled,optional"`

	// Port is the port number for this api
	Port uint64 `hcl:"port,optional" toml:"port,optional"`

	// Prefix is the http prefix to expose this api
	Prefix string `hcl:"prefix,optional" toml:"prefix,optional"`

	// Host is the address to bind the api
	Host string `hcl:"host,optional" toml:"host,optional"`

	// API is the list of enabled api modules
	API []string `hcl:"api,optional" toml:"api,optional"`

	// VHost is the list of valid virtual hosts
	VHost []string `hcl:"vhosts,optional" toml:"vhosts,optional"`

	// Cors is the list of Cors endpoints
	Cors []string `hcl:"corsdomain,optional" toml:"corsdomain,optional"`

	// Origins is the list of endpoints to accept requests from (only consumed for websockets)
	Origins []string `hcl:"origins,optional" toml:"origins,optional"`

	// ExecutionPoolSize is max size of workers to be used for rpc execution
	ExecutionPoolSize uint64 `hcl:"ep-size,optional" toml:"ep-size,optional"`

	// ExecutionPoolRequestTimeout is timeout used by execution pool for rpc execution
	ExecutionPoolRequestTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	ExecutionPoolRequestTimeoutRaw string        `hcl:"ep-requesttimeout,optional" toml:"ep-requesttimeout,optional"`
}

// Used from rpc.HTTPTimeouts
type HttpTimeouts struct {
	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	ReadTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	ReadTimeoutRaw string        `hcl:"read,optional" toml:"read,optional"`

	// ReadHeaderTimeout is the amount of time allowed to read
	// request headers.
	ReadHeaderTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	ReadHeaderTimeoutRaw string        `hcl:"readheader,optional" toml:"readheader,optional"`

	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	WriteTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	WriteTimeoutRaw string        `hcl:"write,optional" toml:"write,optional"`

	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alives are enabled. If IdleTimeout
	// is zero, the value of ReadTimeout is used. If both are
	// zero, ReadHeaderTimeout is used.
	IdleTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	IdleTimeoutRaw string        `hcl:"idle,optional" toml:"idle,optional"`
}

type GpoConfig struct {
	// Blocks is the number of blocks to track to compute the price oracle
	Blocks uint64 `hcl:"blocks,optional" toml:"blocks,optional"`

	// Percentile sets the weights to new blocks
	Percentile uint64 `hcl:"percentile,optional" toml:"percentile,optional"`

	// Maximum header history of gasprice oracle
	MaxHeaderHistory int `hcl:"maxheaderhistory,optional" toml:"maxheaderhistory,optional"`

	// Maximum block history of gasprice oracle
	MaxBlockHistory int `hcl:"maxblockhistory,optional" toml:"maxblockhistory,optional"`

	// MaxPrice is an upper bound gas price
	MaxPrice    *big.Int `hcl:"-,optional" toml:"-"`
	MaxPriceRaw string   `hcl:"maxprice,optional" toml:"maxprice,optional"`

	// IgnorePrice is a lower bound gas price
	IgnorePrice    *big.Int `hcl:"-,optional" toml:"-"`
	IgnorePriceRaw string   `hcl:"ignoreprice,optional" toml:"ignoreprice,optional"`
}

type TelemetryConfig struct {
	// Enabled enables metrics
	Enabled bool `hcl:"metrics,optional" toml:"metrics,optional"`

	// Expensive enables expensive metrics
	Expensive bool `hcl:"expensive,optional" toml:"expensive,optional"`

	// InfluxDB has the influxdb related settings
	InfluxDB *InfluxDBConfig `hcl:"influx,block" toml:"influx,block"`

	// Prometheus Address
	PrometheusAddr string `hcl:"prometheus-addr,optional" toml:"prometheus-addr,optional"`

	// Open collector endpoint
	OpenCollectorEndpoint string `hcl:"opencollector-endpoint,optional" toml:"opencollector-endpoint,optional"`
}

type InfluxDBConfig struct {
	// V1Enabled enables influx v1 mode
	V1Enabled bool `hcl:"influxdb,optional" toml:"influxdb,optional"`

	// Endpoint is the url endpoint of the influxdb service
	Endpoint string `hcl:"endpoint,optional" toml:"endpoint,optional"`

	// Database is the name of the database in Influxdb to store the metrics.
	Database string `hcl:"database,optional" toml:"database,optional"`

	// Enabled is the username to authorize access to Influxdb
	Username string `hcl:"username,optional" toml:"username,optional"`

	// Password is the password to authorize access to Influxdb
	Password string `hcl:"password,optional" toml:"password,optional"`

	// Tags are tags attaches to all generated metrics
	Tags map[string]string `hcl:"tags,optional" toml:"tags,optional"`

	// Enabled enables influx v2 mode
	V2Enabled bool `hcl:"influxdbv2,optional" toml:"influxdbv2,optional"`

	// Token is the token to authorize access to Influxdb V2.
	Token string `hcl:"token,optional" toml:"token,optional"`

	// Bucket is the bucket to store metrics in Influxdb V2.
	Bucket string `hcl:"bucket,optional" toml:"bucket,optional"`

	// Organization is the name of the organization for Influxdb V2.
	Organization string `hcl:"organization,optional" toml:"organization,optional"`
}

type CacheConfig struct {
	// Cache is the amount of cache of the node
	Cache uint64 `hcl:"cache,optional" toml:"cache,optional"`

	// PercGc is percentage of cache used for garbage collection
	PercGc uint64 `hcl:"gc,optional" toml:"gc,optional"`

	// PercSnapshot is percentage of cache used for snapshots
	PercSnapshot uint64 `hcl:"snapshot,optional" toml:"snapshot,optional"`

	// PercDatabase is percentage of cache used for the database
	PercDatabase uint64 `hcl:"database,optional" toml:"database,optional"`

	// PercTrie is percentage of cache used for the trie
	PercTrie uint64 `hcl:"trie,optional" toml:"trie,optional"`

	// NoPrefetch is used to disable prefetch of tries
	NoPrefetch bool `hcl:"noprefetch,optional" toml:"noprefetch,optional"`

	// Preimages is used to enable the track of hash preimages
	Preimages bool `hcl:"preimages,optional" toml:"preimages,optional"`

	// TxLookupLimit sets the maximum number of blocks from head whose tx indices are reserved.
	TxLookupLimit uint64 `hcl:"txlookuplimit,optional" toml:"txlookuplimit,optional"`

	// Number of block states to keep in memory (default = 128)
	TriesInMemory uint64 `hcl:"triesinmemory,optional" toml:"triesinmemory,optional"`

	// This is the number of blocks for which logs will be cached in the filter system.
	FilterLogCacheSize int `hcl:"blocklogs,optional" toml:"blocklogs,optional"`

	// Time after which the Merkle Patricia Trie is stored to disc from memory
	TrieTimeout    time.Duration `hcl:"-,optional" toml:"-"`
	TrieTimeoutRaw string        `hcl:"timeout,optional" toml:"timeout,optional"`

	// Directory path to the journal used for persisting trie data across node restarts
	TrieJournalDirectory string `hcl:"triejournaldirectory,optional" toml:"triejournaldirectory,optional"`

	// Raise the open file descriptor resource limit (default = system fd limit)
	FDLimit int `hcl:"fdlimit,optional" toml:"fdlimit,optional"`

	// Address-specific cache sizes for biased caching (format: "address=sizeMB,address=sizeMB")
	// Size is specified in MB (megabytes)
	// Example: "0x1234...=1024,0x5678...=512" (1024MB and 512MB)
	AddressCacheSizesRaw string            `hcl:"addresscachesizes,optional" toml:"addresscachesizes,optional"`
	AddressCacheSizes    map[string]string `hcl:"-,optional" toml:"-"`

	// PreloadRateLimit limits cache preload I/O in bytes per second per address.
	// This prevents preloading from overwhelming the disk during sync.
	// Accepts values like "500KB", "1MB", "0" (for unlimited). Default: 1MB/s
	PreloadRateLimit string `hcl:"preloadratelimit,optional" toml:"preloadratelimit,optional"`

	// GC settings
	// GoMemLimit sets the soft memory limit for the runtime
	GoMemLimit string `hcl:"gomemlimit,optional" toml:"gomemlimit,optional"`

	// GoGC sets the initial garbage collection target percentage
	GoGC int `hcl:"gogc,optional" toml:"gogc,optional"`

	// GoDebug sets debugging variables for the runtime
	GoDebug string `hcl:"godebug,optional" toml:"godebug,optional"`
}

type ExtraDBConfig struct {
	LevelDbCompactionTableSize           uint64  `hcl:"compactiontablesize,optional" toml:"compactiontablesize,optional"`
	LevelDbCompactionTableSizeMultiplier float64 `hcl:"compactiontablesizemultiplier,optional" toml:"compactiontablesizemultiplier,optional"`
	LevelDbCompactionTotalSize           uint64  `hcl:"compactiontotalsize,optional" toml:"compactiontotalsize,optional"`
	LevelDbCompactionTotalSizeMultiplier float64 `hcl:"compactiontotalsizemultiplier,optional" toml:"compactiontotalsizemultiplier,optional"`
}

type AccountsConfig struct {
	// Unlock is the list of addresses to unlock in the node
	Unlock []string `hcl:"unlock,optional" toml:"unlock,optional"`

	// PasswordFile is the file where the account passwords are stored
	PasswordFile string `hcl:"password,optional" toml:"password,optional"`

	// AllowInsecureUnlock allows user to unlock accounts in unsafe http environment.
	AllowInsecureUnlock bool `hcl:"allow-insecure-unlock,optional" toml:"allow-insecure-unlock,optional"`

	// UseLightweightKDF enables a faster but less secure encryption of accounts
	UseLightweightKDF bool `hcl:"lightkdf,optional" toml:"lightkdf,optional"`

	// DisableBorWallet disables the personal wallet endpoints
	DisableBorWallet bool `hcl:"disable-bor-wallet,optional" toml:"disable-bor-wallet,optional"`
}

type DeveloperConfig struct {
	// Enabled enables the developer mode
	Enabled bool `hcl:"dev,optional" toml:"dev,optional"`

	// Period is the block period to use in developer mode
	Period uint64 `hcl:"period,optional" toml:"period,optional"`

	// Initial block gas limit
	GasLimit uint64 `hcl:"gaslimit,optional" toml:"gaslimit,optional"`
}

type ParallelEVMConfig struct {
	Enable bool `hcl:"enable,optional" toml:"enable,optional"`

	SpeculativeProcesses int `hcl:"procs,optional" toml:"procs,optional"`

	Enforce bool `hcl:"enforce,optional" toml:"enforce,optional"`
}

type WitnessConfig struct {
	// Enable enables the wit protocol
	Enable bool `hcl:"enable,optional" toml:"enable,optional"`

	// SyncWithWitnesses enables syncing blocks with witnesses
	SyncWithWitnesses bool `hcl:"syncwithwitnesses,optional" toml:"syncwithwitnesses,optional"`

	// ProduceWitnesses enables producing witnesses while syncing
	ProduceWitnesses bool `hcl:"producewitnesses,optional" toml:"producewitnesses,optional"`

	// Parallel stateless import (download path) toggle
	EnableParallelStatelessImport bool `hcl:"parallel-stateless-import,optional" toml:"parallel-stateless-import,optional"`

	// Number of workers (CPUs) to use for parallel stateless import. If 0, uses GOMAXPROCS.
	ParallelStatelessImportWorkers int `hcl:"parallel-stateless-import-workers,optional" toml:"parallel-stateless-import-workers,optional"`

	// WitnessAPI enables witness API endpoints
	WitnessAPI bool `hcl:"witnessapi,optional" toml:"witnessapi,optional"`

	// FileStore enables storing witness blobs on the filesystem instead of Pebble
	FileStore bool `hcl:"filestore,optional" toml:"filestore,optional"`

	// Minimum necessary distance between local header and peer to fast forward
	FastForwardThreshold uint64 `hcl:"fastforwardthreshold,optional" toml:"fastforwardthreshold,optional"`
}

type RelayConfig struct {
	// EnablePreconfs enables relay to accept transactions for preconfs
	EnablePreconfs bool `hcl:"enable-preconfs,optional" toml:"enable-preconfs,optional"`

	// EnablePrivateTx enables relaying transactions privately to block producers
	EnablePrivateTx bool `hcl:"enable-private-tx,optional" toml:"enable-private-tx,optional"`

	// BlockProducerRpcEndpoints is a list of block producer rpc endpoints to submit transactions to
	BlockProducerRpcEndpoints []string `hcl:"bp-rpc-endpoints,optional" toml:"bp-rpc-endpoints,optional"`
}

// PipelineConfig has settings for pipelined state root computation during block import.
type PipelineConfig struct {
	// EnableImportSRC enables pipelined state root computation during block import:
	// overlap SRC(N) with tx execution of block N+1
	EnableImportSRC bool `hcl:"enable-import-src,optional" toml:"enable-import-src,optional"`

	// ImportSRCLogs enables verbose logging for pipelined import SRC
	ImportSRCLogs bool `hcl:"import-src-logs,optional" toml:"import-src-logs,optional"`

	// WarmSnapshot enables warm-cache handoff from the execution-side trie
	// prefetcher to the pipelined SRC goroutine when witnesses are produced.
	// Targets cold-cache restart/catch-up CPU; no effect on correctness or
	// witness completeness. Witness-off import ignores it.
	WarmSnapshot bool `hcl:"warm-snapshot,optional" toml:"warm-snapshot,optional"`
}

func DefaultConfig() *Config {
	return &Config{
		Chain:                       "mainnet",
		Ethstats:                    "",
		Identity:                    Hostname(),
		RequiredBlocks:              map[string]string{},
		Verbosity:                   3,
		EnablePreimageRecording:     false,
		EnableEVMSwitchDispatch:     false,
		StateSizeTracking:           ethconfig.Defaults.EnableStateSizeTracking,
		DataDir:                     DefaultDataDir(),
		Ancient:                     "",
		DBEngine:                    "pebble",
		KeyStoreDir:                 "",
		DisableBlindForkValidation:  false,
		MaxBlindForkValidationLimit: whitelist.DefaultMaxForkCorrectnessLimit,
		VMTrace:                     "",
		VMTraceJsonConfig:           "{}",
		BatchRequestLimit:           node.DefaultConfig.BatchRequestLimit,
		BatchResponseMaxSize:        node.DefaultConfig.BatchResponseMaxSize,
		RPCReturnDataLimit:          100000,
		SyncMode:                    "full",
		GcMode:                      "full",
		StateScheme:                 "path",
		Snapshot:                    true,
		BorLogs:                     false,
		DevFakeAuthor:               false,
		Logging: &LoggingConfig{
			Vmodule:             "",
			Json:                false,
			Backtrace:           "",
			Debug:               false,
			EnableBlockTracking: false,
		},
		P2P: &P2PConfig{
			MaxPeers:             50,
			MaxPendPeers:         50,
			Bind:                 "0.0.0.0",
			Port:                 30303,
			NoDiscover:           false,
			NAT:                  "any",
			NetRestrict:          "",
			TxArrivalWait:        500 * time.Millisecond,
			TxAnnouncementOnly:   false,
			DisableTxPropagation: false,
			NoSnapServing:        false,
			Discovery: &P2PDiscovery{
				DiscoveryV4:  true,
				DiscoveryV5:  true,
				Bootnodes:    []string{},
				BootnodesV4:  []string{},
				BootnodesV5:  []string{},
				StaticNodes:  []string{},
				TrustedNodes: []string{},
				DNS:          []string{},
			},
		},
		Heimdall: &HeimdallConfig{
			URL:         "http://localhost:1317",
			Timeout:     5 * time.Second,
			Without:     false,
			GRPCAddress: "",
			WSAddress:   "",
		},
		TxPool: &TxPoolConfig{
			Locals:               []string{},
			NoLocals:             false,
			Journal:              "transactions.rlp",
			Rejournal:            1 * time.Hour,
			PriceLimit:           params.BorDefaultTxPoolPriceLimit, // bor's default
			PriceBump:            10,
			AccountSlots:         16,
			GlobalSlots:          131072,
			AccountQueue:         64,
			GlobalQueue:          131072,
			LifeTime:             3 * time.Hour,
			Rebroadcast:          true,
			RebroadcastInterval:  30 * time.Second,
			RebroadcastMaxAge:    10 * time.Minute,
			RebroadcastBatchSize: 200,
		},
		Sealer: &SealerConfig{
			Enabled:                  false,
			AllowGasTipOverride:      false,
			Etherbase:                "",
			GasCeil:                  miner.DefaultConfig.GasCeil,
			EnableDynamicGasLimit:    miner.DefaultConfig.EnableDynamicGasLimit,
			GasLimitMin:              miner.DefaultConfig.GasLimitMin,
			GasLimitMax:              miner.DefaultConfig.GasLimitMax,
			TargetBaseFee:            miner.DefaultConfig.TargetBaseFee,
			BaseFeeBuffer:            miner.DefaultConfig.BaseFeeBuffer,
			EnableDynamicTargetGas:   false,
			TargetGasMinPercentage:   50,                                         // 50% floor
			TargetGasMaxPercentage:   80,                                         // 80% ceiling
			GasPrice:                 big.NewInt(params.BorDefaultMinerGasPrice), // bor's default
			ExtraData:                "",
			Recommit:                 125 * time.Second,
			CommitInterruptFlag:      true,
			BlockTime:                0,
			EnablePrefetch:           false, // Disabled by default, requires explicit opt-in
			PrefetchGasLimitPercent:  100,
			TargetGasPercentage:      0, // Initialize to 0, will be set from CLI or remain 0 (meaning use default)
			BaseFeeChangeDenominator: 0, // Initialize to 0, will be set from CLI or remain 0 (meaning use default)
			DisablePendingBlock:      false,
		},
		Gpo: &GpoConfig{
			Blocks:           20,
			Percentile:       60,
			MaxHeaderHistory: 1024,
			MaxBlockHistory:  1024,
			MaxPrice:         new(big.Int).Set(gasprice.DefaultMaxPrice),
			IgnorePrice:      new(big.Int).Set(gasprice.DefaultIgnorePrice), // bor's default
		},
		JsonRPC: &JsonRPCConfig{
			IPCDisable:           false,
			IPCPath:              "",
			GasCap:               ethconfig.Defaults.RPCGasCap,
			TxFeeCap:             ethconfig.Defaults.RPCTxFeeCap,
			LogQueryLimit:        ethconfig.Defaults.LogQueryLimit,
			RPCEVMTimeout:        ethconfig.Defaults.RPCEVMTimeout,
			AllowUnprotectedTxs:  false,
			EnablePersonal:       false,
			EnableTrace:          false,
			TxSyncDefaultTimeout: ethconfig.Defaults.TxSyncDefaultTimeout,
			TxSyncMaxTimeout:     ethconfig.Defaults.TxSyncMaxTimeout,
			AcceptPreconfTx:      false,
			AcceptPrivateTx:      false,
			Http: &APIConfig{
				Enabled:                     false,
				Port:                        8545,
				Prefix:                      "",
				Host:                        "localhost",
				API:                         []string{"eth", "net", "web3", "txpool", "bor"},
				Cors:                        []string{"localhost"},
				VHost:                       []string{"localhost"},
				ExecutionPoolSize:           40,
				ExecutionPoolRequestTimeout: 0,
			},
			Ws: &APIConfig{
				Enabled:                     false,
				Port:                        8546,
				Prefix:                      "",
				Host:                        "localhost",
				API:                         []string{"net", "web3"},
				Origins:                     []string{"localhost"},
				ExecutionPoolSize:           40,
				ExecutionPoolRequestTimeout: 0,
			},
			Graphql: &APIConfig{
				Enabled: false,
				Cors:    []string{"localhost"},
				VHost:   []string{"localhost"},
			},
			HttpTimeout: &HttpTimeouts{
				ReadTimeout:       10 * time.Second,
				ReadHeaderTimeout: 30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			},
			Auth: &AUTHConfig{
				JWTSecret: "",
				Port:      node.DefaultAuthPort,
				Addr:      node.DefaultAuthHost,
				VHosts:    node.DefaultAuthVhosts,
			},
		},
		Telemetry: &TelemetryConfig{
			Enabled:               false,
			Expensive:             false,
			PrometheusAddr:        "127.0.0.1:7071",
			OpenCollectorEndpoint: "",
			InfluxDB: &InfluxDBConfig{
				V1Enabled:    false,
				Endpoint:     "",
				Database:     "",
				Username:     "",
				Password:     "",
				Tags:         map[string]string{},
				V2Enabled:    false,
				Token:        "",
				Bucket:       "",
				Organization: "",
			},
		},
		Cache: &CacheConfig{
			Cache:                1024, // geth's default (suitable for mumbai)
			PercDatabase:         50,
			PercTrie:             15,
			PercGc:               25,
			PercSnapshot:         10,
			NoPrefetch:           false,
			Preimages:            false,
			TxLookupLimit:        2350000,
			TriesInMemory:        128,
			FilterLogCacheSize:   ethconfig.Defaults.FilterLogCacheSize,
			TrieTimeout:          60 * time.Minute,
			TrieJournalDirectory: "", // When empty, backend resolves this to DATADIR/triedb
			FDLimit:              0,
			GoMemLimit:           "",  // Empty means no limit
			GoGC:                 100, // Go default is 100%
			GoDebug:              "",  // Empty means no debug flags
		},
		ExtraDB: &ExtraDBConfig{
			// These are LevelDB defaults, specifying here for clarity in code and in logging.
			// See: https://github.com/syndtr/goleveldb/blob/126854af5e6d8295ef8e8bee3040dd8380ae72e8/leveldb/opt/options.go
			LevelDbCompactionTableSize:           2, // MiB
			LevelDbCompactionTableSizeMultiplier: 1,
			LevelDbCompactionTotalSize:           10, // MiB
			LevelDbCompactionTotalSizeMultiplier: 10,
		},
		Accounts: &AccountsConfig{
			Unlock:              []string{},
			PasswordFile:        "",
			AllowInsecureUnlock: false,
			UseLightweightKDF:   false,
			DisableBorWallet:    true,
		},
		GRPC: &GRPCConfig{
			Addr: "127.0.0.1:3131",
		},
		Developer: &DeveloperConfig{
			Enabled:  false,
			Period:   0,
			GasLimit: 11500000,
		},
		Pprof: &PprofConfig{
			Enabled:          false,
			Port:             6060,
			Addr:             "127.0.0.1",
			MemProfileRate:   512 * 1024,
			BlockProfileRate: 0,
		},
		ParallelEVM: &ParallelEVMConfig{
			Enable:               true,
			SpeculativeProcesses: 8,
			Enforce:              false,
		},
		Witness: &WitnessConfig{
			Enable:                         false,
			SyncWithWitnesses:              false,
			ProduceWitnesses:               false,
			EnableParallelStatelessImport:  false,
			ParallelStatelessImportWorkers: 0,
			WitnessAPI:                     false,
			FileStore:                      true,
			FastForwardThreshold:           6400,
		},
		History: &HistoryConfig{
			TransactionHistory: ethconfig.Defaults.TransactionHistory,
			LogHistory:         ethconfig.Defaults.LogHistory,
			LogNoHistory:       ethconfig.Defaults.LogNoHistory,
			StateHistory:       params.FullImmutabilityThreshold,
		},
		Health: &HealthConfig{
			MaxGoRoutineThreshold:  0,
			WarnGoRoutineThreshold: 0,
			MinPeerThreshold:       0,
			WarnPeerThreshold:      0,
		},
		Relay: &RelayConfig{
			EnablePreconfs:            false,
			EnablePrivateTx:           false,
			BlockProducerRpcEndpoints: []string{},
		},
		Pipeline: &PipelineConfig{
			EnableImportSRC: false,
			ImportSRCLogs:   false,
			WarmSnapshot:    true,
		},
	}
}

func (c *Config) fillBigInt() error {
	tds := []struct {
		path string
		td   **big.Int
		str  *string
	}{
		{"gpo.maxprice", &c.Gpo.MaxPrice, &c.Gpo.MaxPriceRaw},
		{"gpo.ignoreprice", &c.Gpo.IgnorePrice, &c.Gpo.IgnorePriceRaw},
		{"miner.gasprice", &c.Sealer.GasPrice, &c.Sealer.GasPriceRaw},
	}

	for _, x := range tds {
		if *x.str != "" {
			b := new(big.Int)

			var ok bool

			if strings.HasPrefix(*x.str, "0x") {
				b, ok = b.SetString((*x.str)[2:], 16)
			} else {
				b, ok = b.SetString(*x.str, 10)
			}

			if !ok {
				return fmt.Errorf("%s can't parse big int %s", x.path, *x.str)
			}

			*x.str = ""
			*x.td = b
		}
	}

	return nil
}

func (c *Config) fillTimeDurations() error {
	tds := []struct {
		path string
		td   *time.Duration
		str  *string
	}{
		{"jsonrpc.evmtimeout", &c.JsonRPC.RPCEVMTimeout, &c.JsonRPC.RPCEVMTimeoutRaw},
		{"miner.recommit", &c.Sealer.Recommit, &c.Sealer.RecommitRaw},
		{"miner.blocktime", &c.Sealer.BlockTime, &c.Sealer.BlockTimeRaw},
		{"jsonrpc.timeouts.read", &c.JsonRPC.HttpTimeout.ReadTimeout, &c.JsonRPC.HttpTimeout.ReadTimeoutRaw},
		{"jsonrpc.timeouts.readheader", &c.JsonRPC.HttpTimeout.ReadHeaderTimeout, &c.JsonRPC.HttpTimeout.ReadHeaderTimeoutRaw},
		{"jsonrpc.timeouts.write", &c.JsonRPC.HttpTimeout.WriteTimeout, &c.JsonRPC.HttpTimeout.WriteTimeoutRaw},
		{"jsonrpc.timeouts.idle", &c.JsonRPC.HttpTimeout.IdleTimeout, &c.JsonRPC.HttpTimeout.IdleTimeoutRaw},
		{"jsonrpc.ws.ep-requesttimeout", &c.JsonRPC.Ws.ExecutionPoolRequestTimeout, &c.JsonRPC.Ws.ExecutionPoolRequestTimeoutRaw},
		{"jsonrpc.http.ep-requesttimeout", &c.JsonRPC.Http.ExecutionPoolRequestTimeout, &c.JsonRPC.Http.ExecutionPoolRequestTimeoutRaw},
		{"txpool.lifetime", &c.TxPool.LifeTime, &c.TxPool.LifeTimeRaw},
		{"txpool.rejournal", &c.TxPool.Rejournal, &c.TxPool.RejournalRaw},
		{"txpool.rebroadcast-interval", &c.TxPool.RebroadcastInterval, &c.TxPool.RebroadcastIntervalRaw},
		{"txpool.rebroadcast-max-age", &c.TxPool.RebroadcastMaxAge, &c.TxPool.RebroadcastMaxAgeRaw},
		{"cache.timeout", &c.Cache.TrieTimeout, &c.Cache.TrieTimeoutRaw},
		{"p2p.txarrivalwait", &c.P2P.TxArrivalWait, &c.P2P.TxArrivalWaitRaw},
		{"rpc.txsync.defaulttimeout", &c.JsonRPC.TxSyncDefaultTimeout, &c.JsonRPC.TxSyncDefaultTimeoutRaw},
		{"rpc.txsync.maxtimeout", &c.JsonRPC.TxSyncMaxTimeout, &c.JsonRPC.TxSyncMaxTimeoutRaw},
	}

	for _, x := range tds {
		if x.td != nil && x.str != nil && *x.str != "" {
			d, err := time.ParseDuration(*x.str)
			if err != nil {
				return fmt.Errorf("%s can't parse time duration %s", x.path, *x.str)
			}

			*x.str = ""
			*x.td = d
		}
	}

	return nil
}

func readConfigFile(path string) (*Config, error) {
	ext := filepath.Ext(path)
	if ext == ".toml" {
		return readLegacyConfig(path)
	}

	config := &Config{
		TxPool: &TxPoolConfig{},
		Cache:  &CacheConfig{},
		Sealer: &SealerConfig{},
		// WarmSnapshot defaults on: witness-producing nodes measure faster
		// with it, and it is inert otherwise. The zero value would silently
		// put HCL-configured witness nodes on the slower path.
		Pipeline: &PipelineConfig{WarmSnapshot: true},
	}

	if err := hclsimple.DecodeFile(path, nil, config); err != nil {
		return nil, fmt.Errorf("failed to decode config file '%s': %v", path, err)
	}

	if err := config.fillBigInt(); err != nil {
		return nil, err
	}

	if err := config.fillTimeDurations(); err != nil {
		return nil, err
	}

	return config, nil
}

func (c *Config) loadChain() error {
	chain, err := chains.GetChain(c.Chain)
	if err != nil {
		return err
	}

	c.chain = chain

	// preload some default values that depend on the chain file
	if c.P2P.Discovery.DNS == nil {
		c.P2P.Discovery.DNS = c.chain.DNS
	}

	return nil
}

//nolint:gocognit
func (c *Config) buildEth(stack *node.Node, accountManager *accounts.Manager) (*ethconfig.Config, error) {
	dbHandles, err := MakeDatabaseHandles(c.Cache.FDLimit)
	if err != nil {
		return nil, err
	}

	n := ethconfig.Defaults

	// only update for non-developer mode as we don't yet
	// have the chain object for it.
	if !c.Developer.Enabled {
		n.NetworkId = c.chain.NetworkId
		n.Genesis = c.chain.Genesis
	}

	n.HeimdallURL = c.Heimdall.URL
	n.HeimdallTimeout = c.Heimdall.Timeout
	n.WithoutHeimdall = c.Heimdall.Without
	n.HeimdallgRPCAddress = c.Heimdall.GRPCAddress
	n.HeimdallWSAddress = c.Heimdall.WSAddress
	n.RunHeimdall = c.Heimdall.RunHeimdall
	n.RunHeimdallArgs = c.Heimdall.RunHeimdallArgs
	n.UseHeimdallApp = c.Heimdall.UseHeimdallApp

	// Developer Fake Author for producing blocks without authorisation on bor consensus
	n.DevFakeAuthor = c.DevFakeAuthor

	// Developer Fake Author for producing blocks without authorisation on bor consensus
	n.DevFakeAuthor = c.DevFakeAuthor

	// gas price oracle
	{
		n.GPO.Blocks = int(c.Gpo.Blocks)
		n.GPO.Percentile = int(c.Gpo.Percentile)
		n.GPO.MaxHeaderHistory = uint64(c.Gpo.MaxHeaderHistory)
		n.GPO.MaxBlockHistory = uint64(c.Gpo.MaxBlockHistory)
		n.GPO.MaxPrice = c.Gpo.MaxPrice
		n.GPO.IgnorePrice = c.Gpo.IgnorePrice
	}

	n.EnablePreimageRecording = c.EnablePreimageRecording
	n.EnableEVMSwitchDispatch = c.EnableEVMSwitchDispatch
	n.EnableStateSizeTracking = c.StateSizeTracking
	n.VMTrace = c.VMTrace
	n.VMTraceJsonConfig = c.VMTraceJsonConfig

	// txpool options
	{
		for _, addrStr := range c.TxPool.Locals {
			n.TxPool.Locals = append(n.TxPool.Locals, common.HexToAddress(addrStr))
		}
		n.TxPool.NoLocals = c.TxPool.NoLocals
		n.TxPool.Journal = c.TxPool.Journal
		n.TxPool.Rejournal = c.TxPool.Rejournal
		n.TxPool.PriceLimit = c.TxPool.PriceLimit
		n.TxPool.PriceBump = c.TxPool.PriceBump
		n.TxPool.AccountSlots = c.TxPool.AccountSlots
		n.TxPool.GlobalSlots = c.TxPool.GlobalSlots
		n.TxPool.AccountQueue = c.TxPool.AccountQueue
		n.TxPool.GlobalQueue = c.TxPool.GlobalQueue
		n.TxPool.Lifetime = c.TxPool.LifeTime

		// Load filtered addresses during config initialization
		if filteredAddrs, err := loadFilteredAddresses(c.TxPool.FilteredAddressesFile); err != nil {
			return nil, fmt.Errorf("failed to load filtered addresses: %v", err)
		} else {
			n.TxPool.FilteredAddresses = filteredAddrs
		}

		// Rebroadcast options
		n.TxPool.Rebroadcast = c.TxPool.Rebroadcast
		n.TxPool.RebroadcastInterval = c.TxPool.RebroadcastInterval
		n.TxPool.RebroadcastMaxAge = c.TxPool.RebroadcastMaxAge
		n.TxPool.RebroadcastBatchSize = c.TxPool.RebroadcastBatchSize
	}

	// miner options
	{
		// only allow gas tip override if mining is enabled
		n.Miner.AllowGasTipOverride = c.Sealer.AllowGasTipOverride && c.Sealer.Enabled
		n.Miner.Recommit = c.Sealer.Recommit
		n.Miner.GasPrice = c.Sealer.GasPrice
		n.Miner.GasCeil = c.Sealer.GasCeil
		n.Miner.ExtraData = []byte(c.Sealer.ExtraData)
		n.Miner.CommitInterruptFlag = c.Sealer.CommitInterruptFlag
		n.Miner.BlockTime = c.Sealer.BlockTime
		n.Miner.EnablePrefetch = c.Sealer.EnablePrefetch
		n.Miner.PrefetchGasLimitPercent = c.Sealer.PrefetchGasLimitPercent
		n.Miner.DisablePendingBlock = c.Sealer.DisablePendingBlock

		// Validate prefetch gas limit percentage
		if c.Sealer.EnablePrefetch && c.Sealer.PrefetchGasLimitPercent > 150 {
			return nil, fmt.Errorf("miner.prefetch-gaslimit-percent (%d) must not exceed 150%%", c.Sealer.PrefetchGasLimitPercent)
		}

		// Dynamic gas limit configuration
		n.Miner.EnableDynamicGasLimit = c.Sealer.EnableDynamicGasLimit
		n.Miner.GasLimitMin = c.Sealer.GasLimitMin
		n.Miner.GasLimitMax = c.Sealer.GasLimitMax
		n.Miner.TargetBaseFee = c.Sealer.TargetBaseFee
		n.Miner.BaseFeeBuffer = c.Sealer.BaseFeeBuffer

		// Enforce mutual exclusivity between dynamic gas limit and dynamic target gas
		if c.Sealer.EnableDynamicGasLimit && c.Sealer.EnableDynamicTargetGas {
			return nil, fmt.Errorf("miner.enableDynamicGasLimit and miner.enableDynamicTargetGas are mutually exclusive; only one may be enabled at a time")
		}

		// Validate dynamic gas limit configuration
		if c.Sealer.EnableDynamicGasLimit {
			if c.Sealer.GasLimitMin >= c.Sealer.GasLimitMax {
				return nil, fmt.Errorf("miner.gasLimitMin (%d) must be less than miner.gasLimitMax (%d)", c.Sealer.GasLimitMin, c.Sealer.GasLimitMax)
			}
			if c.Sealer.GasLimitMin < params.MinGasLimit {
				return nil, fmt.Errorf("miner.gasLimitMin (%d) must be at least %d (protocol minimum)", c.Sealer.GasLimitMin, params.MinGasLimit)
			}
			if c.Sealer.TargetBaseFee == 0 {
				return nil, fmt.Errorf("miner.targetBaseFee must be greater than 0 when dynamic gas limit is enabled")
			}
		}

		// Validate dynamic target gas percentage configuration
		if c.Sealer.EnableDynamicTargetGas {
			if c.Sealer.TargetGasMinPercentage == 0 || c.Sealer.TargetGasMinPercentage > 100 {
				return nil, fmt.Errorf("miner.targetGasMinPercentage (%d) must be between 1-100", c.Sealer.TargetGasMinPercentage)
			}
			if c.Sealer.TargetGasMaxPercentage == 0 || c.Sealer.TargetGasMaxPercentage > 100 {
				return nil, fmt.Errorf("miner.targetGasMaxPercentage (%d) must be between 1-100", c.Sealer.TargetGasMaxPercentage)
			}
			if c.Sealer.TargetGasMinPercentage >= c.Sealer.TargetGasMaxPercentage {
				return nil, fmt.Errorf("miner.targetGasMinPercentage (%d) must be less than miner.targetGasMaxPercentage (%d)", c.Sealer.TargetGasMinPercentage, c.Sealer.TargetGasMaxPercentage)
			}
			if c.Sealer.TargetBaseFee == 0 {
				return nil, fmt.Errorf("miner.targetBaseFee must be greater than 0 when dynamic target gas is enabled")
			}
			if c.Sealer.BaseFeeBuffer >= c.Sealer.TargetBaseFee {
				log.Warn("miner.baseFeeBuffer >= miner.targetBaseFee; lower bound will be 0 (TargetGasMinPercentage branch permanently disabled, only upward fee pressure can trigger)")
			}
			// The static fallback percentage (explicit or implicit default) must fall within [min, max].
			// When baseFee is within the buffer, GetTargetGasPercentage() is used as the neutral value;
			// it must respect the configured dynamic range.
			if c.Sealer.TargetGasPercentage > 0 {
				if c.Sealer.TargetGasPercentage <= c.Sealer.TargetGasMinPercentage || c.Sealer.TargetGasPercentage >= c.Sealer.TargetGasMaxPercentage {
					return nil, fmt.Errorf("miner.target-gas-percentage (%d) must be between miner.targetGasMinPercentage (%d) and miner.targetGasMaxPercentage (%d)",
						c.Sealer.TargetGasPercentage, c.Sealer.TargetGasMinPercentage, c.Sealer.TargetGasMaxPercentage)
				}
			} else {
				// Implicit default: TargetGasPercentagePostDandeli (65) must also be within range
				defaultPct := uint64(params.TargetGasPercentagePostDandeli)
				if defaultPct <= c.Sealer.TargetGasMinPercentage || defaultPct >= c.Sealer.TargetGasMaxPercentage {
					return nil, fmt.Errorf("default target gas percentage (%d) falls outside [miner.targetGasMinPercentage=%d, miner.targetGasMaxPercentage=%d]; set --miner.target-gas-percentage to a value within the range",
						defaultPct, c.Sealer.TargetGasMinPercentage, c.Sealer.TargetGasMaxPercentage)
				}
			}
		}

		if etherbase := c.Sealer.Etherbase; etherbase != "" {
			if !common.IsHexAddress(etherbase) {
				return nil, fmt.Errorf("etherbase is not an address: %s", etherbase)
			}

			n.Miner.Etherbase = common.HexToAddress(etherbase)
		}
	}

	// Set runtime miner gas parameters in BorConfig (if not in developer mode and Bor chain)
	if !c.Developer.Enabled && n.Genesis != nil && n.Genesis.Config != nil && n.Genesis.Config.Bor != nil {
		// Only set if non-zero (0 means not set via CLI, use defaults from consensus)
		if c.Sealer.TargetGasPercentage > 0 {
			if c.Sealer.TargetGasPercentage > 100 {
				return nil, fmt.Errorf("miner.targetGasPercentage must be between 1-100, got %d", c.Sealer.TargetGasPercentage)
			}
			n.Genesis.Config.Bor.TargetGasPercentage = &c.Sealer.TargetGasPercentage
		}
		if c.Sealer.BaseFeeChangeDenominator > 0 {
			n.Genesis.Config.Bor.BaseFeeChangeDenominator = &c.Sealer.BaseFeeChangeDenominator
		}

		// Wire dynamic target gas percentage configuration to BorConfig
		if c.Sealer.EnableDynamicTargetGas {
			n.Genesis.Config.Bor.EnableDynamicTargetGas = &c.Sealer.EnableDynamicTargetGas
			n.Genesis.Config.Bor.TargetGasMinPercentage = &c.Sealer.TargetGasMinPercentage
			n.Genesis.Config.Bor.TargetGasMaxPercentage = &c.Sealer.TargetGasMaxPercentage
			n.Genesis.Config.Bor.TargetBaseFee = &c.Sealer.TargetBaseFee
			n.Genesis.Config.Bor.BaseFeeBuffer = &c.Sealer.BaseFeeBuffer
		}
	}

	// unlock accounts
	if len(c.Accounts.Unlock) > 0 {
		if !stack.Config().InsecureUnlockAllowed && stack.Config().ExtRPCEnabled() {
			return nil, fmt.Errorf("account unlock with HTTP access is forbidden")
		}

		ks := accountManager.Backends(keystore.KeyStoreType)[0].(*keystore.KeyStore)

		passwords, err := MakePasswordListFromFile(c.Accounts.PasswordFile)
		if err != nil {
			return nil, err
		}

		if len(passwords) < len(c.Accounts.Unlock) {
			return nil, fmt.Errorf("number of passwords provided (%v) is less than number of accounts (%v) to unlock",
				len(passwords), len(c.Accounts.Unlock))
		}

		for i, account := range c.Accounts.Unlock {
			unlockAccount(ks, account, i, passwords)
		}
	}

	// update for developer mode
	if c.Developer.Enabled {
		// Get a keystore
		var ks *keystore.KeyStore
		if keystores := accountManager.Backends(keystore.KeyStoreType); len(keystores) > 0 {
			ks = keystores[0].(*keystore.KeyStore)
		}

		// Create new developer account or reuse existing one
		var (
			developer  accounts.Account
			passphrase string
			err        error
		)

		// etherbase has been set above, configuring the miner address from command line flags.
		if n.Miner.Etherbase != (common.Address{}) {
			developer = accounts.Account{Address: n.Miner.Etherbase}
		} else if accs := ks.Accounts(); len(accs) > 0 {
			developer = ks.Accounts()[0]
		} else {
			developer, err = ks.NewAccount(passphrase)
			if err != nil {
				return nil, fmt.Errorf("failed to create developer account: %v", err)
			}
		}
		if err := ks.Unlock(developer, passphrase); err != nil {
			return nil, fmt.Errorf("failed to unlock developer account: %v", err)
		}

		log.Info("Using developer account", "address", developer.Address)

		// Set the Etherbase
		c.Sealer.Etherbase = developer.Address.Hex()
		n.Miner.Etherbase = developer.Address

		// get developer mode chain config
		c.chain = chains.GetDeveloperChain(c.Developer.Period, c.Developer.GasLimit, developer.Address)

		// update the parameters
		n.NetworkId = c.chain.NetworkId
		n.Genesis = c.chain.Genesis

		// Set runtime miner gas parameters in BorConfig for developer mode
		if n.Genesis != nil && n.Genesis.Config != nil && n.Genesis.Config.Bor != nil {
			// Only set if non-zero (0 means not set via CLI, use defaults from consensus)
			if c.Sealer.TargetGasPercentage > 0 {
				if c.Sealer.TargetGasPercentage > 100 {
					return nil, fmt.Errorf("miner.targetGasPercentage must be between 1-100, got %d", c.Sealer.TargetGasPercentage)
				}
				n.Genesis.Config.Bor.TargetGasPercentage = &c.Sealer.TargetGasPercentage
			}
			if c.Sealer.BaseFeeChangeDenominator > 0 {
				n.Genesis.Config.Bor.BaseFeeChangeDenominator = &c.Sealer.BaseFeeChangeDenominator
			}
		}

		// Update cache
		c.Cache.Cache = 1024

		// Update sync mode
		c.SyncMode = "full"

		// update miner gas price
		if n.Miner.GasPrice == nil {
			n.Miner.GasPrice = big.NewInt(1)
		}
	}

	// discovery (this params should be in node.Config)
	{
		n.EthDiscoveryURLs = c.P2P.Discovery.DNS
		n.SnapDiscoveryURLs = c.P2P.Discovery.DNS
	}

	// RequiredBlocks
	{
		n.RequiredBlocks = map[uint64]common.Hash{}

		for k, v := range c.RequiredBlocks {
			number, err := strconv.ParseUint(k, 0, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid required block number %s: %v", k, err)
			}

			var hash common.Hash
			if err = hash.UnmarshalText([]byte(v)); err != nil {
				return nil, fmt.Errorf("invalid required block hash %s: %v", v, err)
			}

			n.RequiredBlocks[number] = hash
		}
	}

	// cache
	{
		cache := c.Cache.Cache
		calcPerc := func(val uint64) int {
			return int(cache * (val) / 100)
		}

		// Cap the cache allowance
		mem, err := gopsutil.VirtualMemory()
		if err == nil {
			if 32<<(^uintptr(0)>>63) == 32 && mem.Total > 2*1024*1024*1024 {
				log.Warn("Lowering memory allowance on 32bit arch", "available", mem.Total/1024/1024, "addressable", 2*1024)
				mem.Total = 2 * 1024 * 1024 * 1024
			}

			allowance := mem.Total / 1024 / 1024 / 3
			if cache > allowance {
				log.Warn("Sanitizing cache to Go's GC limits", "provided", cache, "updated", allowance)
				cache = allowance
			}
		}
		// Apply configurable garbage collection settings with validation
		if c.Cache.GoMemLimit != "" {
			if bytes, err := parseGoMemLimit(c.Cache.GoMemLimit); err != nil {
				log.Warn("Invalid GOMEMLIMIT value, skipping", "value", c.Cache.GoMemLimit, "error", err)
			} else {
				prev := godebug.SetMemoryLimit(bytes)
				log.Info("Set GOMEMLIMIT via runtime", "value", c.Cache.GoMemLimit, "bytes", bytes, "previous", prev)
			}
		}

		// Sanitize GOGC value to reasonable bounds
		sanitizedGoGC := sanitizeGoGC(c.Cache.GoGC)
		if sanitizedGoGC != c.Cache.GoGC {
			log.Warn("GOGC value sanitized", "original", c.Cache.GoGC, "sanitized", sanitizedGoGC)
		}
		if sanitizedGoGC != 100 { // Only set if different from default
			godebug.SetGCPercent(sanitizedGoGC)
			log.Info("Set GOGC", "percent", sanitizedGoGC)
		}

		if c.Cache.GoDebug != "" {
			if err := validateGoDebug(c.Cache.GoDebug); err != nil {
				log.Warn("Invalid GODEBUG value, skipping", "value", c.Cache.GoDebug, "error", err)
			} else {
				os.Setenv("GODEBUG", c.Cache.GoDebug)
				log.Info("Set GODEBUG", "value", c.Cache.GoDebug)
			}
		}

		n.DatabaseCache = calcPerc(c.Cache.PercDatabase)
		n.SnapshotCache = calcPerc(c.Cache.PercSnapshot)
		n.TrieCleanCache = calcPerc(c.Cache.PercTrie)
		n.TrieDirtyCache = calcPerc(c.Cache.PercGc)
		n.NoPrefetch = c.Cache.NoPrefetch
		n.Preimages = c.Cache.Preimages
		n.EnablePipelinedImportSRC = c.Pipeline.EnableImportSRC
		n.PipelinedImportSRCLogs = c.Pipeline.ImportSRCLogs
		n.PipelinedSRCWarmSnapshot = c.Pipeline.WarmSnapshot
		// Note that even the values set by `history.transactions` will be written in the old flag until it's removed.
		n.TransactionHistory = c.Cache.TxLookupLimit
		n.TrieTimeout = c.Cache.TrieTimeout
		n.TriesInMemory = c.Cache.TriesInMemory
		n.TrieJournalDirectory = c.Cache.TrieJournalDirectory
		if stack != nil && n.TrieJournalDirectory != "" {
			n.TrieJournalDirectory = stack.ResolvePath(n.TrieJournalDirectory)
		}
		n.FilterLogCacheSize = c.Cache.FilterLogCacheSize

		// Parse address-specific cache sizes
		if c.Cache.AddressCacheSizesRaw != "" {
			addressCacheSizes, err := parseAddressCacheSizes(c.Cache.AddressCacheSizesRaw)
			if err != nil {
				log.Warn("Failed to parse address cache sizes", "error", err)
			} else {
				n.AddressCacheSizes = addressCacheSizes
			}
		}

		// Parse preload rate limit (default: 1MB/s per address)
		if c.Cache.PreloadRateLimit != "" {
			rateLimitBytes, err := parseByteSize(c.Cache.PreloadRateLimit)
			if err != nil {
				log.Warn("Failed to parse preload rate limit, using default 1MB/s per address", "error", err)
				n.PreloadRateLimit = 1024 * 1024
			} else {
				n.PreloadRateLimit = rateLimitBytes
			}
		} else {
			// Default to 1MB/s per address if not specified
			n.PreloadRateLimit = 1024 * 1024
		}
	}

	// History
	{
		// TODO: uncomment this when txlookuplimit is completely removed
		// n.TransactionHistory = c.History.TransactionHistory
		n.LogHistory = c.History.LogHistory
		n.LogNoHistory = c.History.LogNoHistory
		n.StateHistory = c.History.StateHistory
	}

	// LevelDB
	{
		n.LevelDbCompactionTableSize = c.ExtraDB.LevelDbCompactionTableSize
		n.LevelDbCompactionTableSizeMultiplier = c.ExtraDB.LevelDbCompactionTableSizeMultiplier
		n.LevelDbCompactionTotalSize = c.ExtraDB.LevelDbCompactionTotalSize
		n.LevelDbCompactionTotalSizeMultiplier = c.ExtraDB.LevelDbCompactionTotalSizeMultiplier
	}

	n.RPCGasCap = c.JsonRPC.GasCap
	if n.RPCGasCap != 0 {
		log.Info("Set global gas cap", "cap", n.RPCGasCap)
	} else {
		log.Info("Global gas cap disabled")
	}

	n.RPCEVMTimeout = c.JsonRPC.RPCEVMTimeout
	n.RPCTxFeeCap = c.JsonRPC.TxFeeCap
	n.TxSyncDefaultTimeout = c.JsonRPC.TxSyncDefaultTimeout
	n.TxSyncMaxTimeout = c.JsonRPC.TxSyncMaxTimeout

	n.LogQueryLimit = c.JsonRPC.LogQueryLimit
	n.RPCBlockRangeLimit = c.JsonRPC.RangeLimit

	// Choose the sync mode
	switch c.SyncMode {
	case "full":
		n.SyncMode = downloader.FullSync
	case "snap":
		n.SyncMode = downloader.SnapSync
	case "stateless":
		n.SyncMode = downloader.StatelessSync
		log.Info("Using Stateless Sync mode - syncing from latest checkpoint without history")
	default:
		return nil, fmt.Errorf("sync mode '%s' not found", c.SyncMode)
	}

	// archive mode. It can either be "archive" or "full".
	switch c.GcMode {
	case "full":
		n.NoPruning = false
	case "archive":
		n.NoPruning = true
		if !n.Preimages {
			n.Preimages = true

			log.Info("Enabling recording of key preimages since archive mode is used")
		}
	default:
		return nil, fmt.Errorf("gcmode '%s' not found", c.GcMode)
	}

	// statescheme "hash" or "path"
	switch c.StateScheme {
	case "path":
		n.StateScheme = "path"
	default:
		n.StateScheme = "hash"
	}

	// snapshot disable check
	if !c.Snapshot {
		if n.SyncMode == downloader.SnapSync {
			log.Info("Snap sync requested, enabling --snapshot")
		} else {
			// disable snapshot
			n.TrieCleanCache += n.SnapshotCache
			n.SnapshotCache = 0
		}
	}

	n.BorLogs = c.BorLogs
	n.DatabaseHandles = dbHandles

	n.ParallelEVM.Enable = c.ParallelEVM.Enable
	n.ParallelEVM.SpeculativeProcesses = c.ParallelEVM.SpeculativeProcesses
	n.ParallelEVM.Enforce = c.ParallelEVM.Enforce

	n.NoSnapServing = c.P2P.NoSnapServing
	n.WitnessProtocol = c.Witness.Enable
	if c.SyncMode == "stateless" {
		if !c.Witness.Enable {
			log.Warn("Witness protocol is disabled, overriding to true for stateless sync")
		}
		n.WitnessProtocol = true
	}
	n.SyncWithWitnesses = c.Witness.SyncWithWitnesses
	n.SyncAndProduceWitnesses = c.Witness.ProduceWitnesses
	n.EnableParallelStatelessImport = c.Witness.EnableParallelStatelessImport
	n.EnableParallelStatelessImportWorkers = c.Witness.ParallelStatelessImportWorkers
	n.WitnessAPIEnabled = c.Witness.WitnessAPI
	n.WitnessFileStore = c.Witness.FileStore
	n.FastForwardThreshold = c.Witness.FastForwardThreshold
	n.RPCReturnDataLimit = c.RPCReturnDataLimit

	if c.Ancient != "" {
		n.DatabaseFreezer = c.Ancient
	}

	n.EnableBlockTracking = c.Logging.EnableBlockTracking

	// Blind fork acceptance configs
	n.DisableBlindForkValidation = c.DisableBlindForkValidation
	n.MaxBlindForkValidationLimit = c.MaxBlindForkValidationLimit

	// Set preconf / private transaction flags for relay
	n.EnablePreconfs = c.Relay.EnablePreconfs
	n.EnablePrivateTx = c.Relay.EnablePrivateTx
	n.BlockProducerRpcEndpoints = c.Relay.BlockProducerRpcEndpoints

	// Set preconf / private transaction flags for block producers
	n.AcceptPreconfTx = c.JsonRPC.AcceptPreconfTx
	n.AcceptPrivateTx = c.JsonRPC.AcceptPrivateTx

	return &n, nil
}

// parseAddressCacheSizes parses address cache sizes from a string format
// Expected format: "address1=sizeMB1,address2=sizeMB2,..."
// Sizes are specified in MB (megabytes) and converted to bytes
// Example: "0x1234...=1024,0x5678...=512" means 1024MB and 512MB
func parseAddressCacheSizes(input string) (map[common.Address]int, error) {
	result := make(map[common.Address]int)
	if input == "" {
		return result, nil
	}

	// Split by comma
	pairs := strings.Split(input, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		// Split by equals
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format for address cache size pair: %s", pair)
		}

		// Parse address
		addressStr := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(addressStr, "0x") {
			addressStr = "0x" + addressStr
		}
		address := common.HexToAddress(addressStr)

		// Parse size in MB and convert to bytes
		sizeMB, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("invalid size for address %s: %v (must be integer MB)", addressStr, err)
		}

		// Convert MB to bytes
		sizeBytes := sizeMB * 1024 * 1024
		result[address] = sizeBytes
	}

	return result, nil
}

// parseByteSize parses a byte size string like "5MB", "10MB", "1GB", or "0" (for unlimited)
// Returns the size in bytes. Supported suffixes: B, KB, MB, GB (case insensitive)
func parseByteSize(input string) (int64, error) {
	input = strings.TrimSpace(input)
	if input == "" || input == "0" {
		return 0, nil
	}

	input = strings.ToUpper(input)

	var multiplier int64 = 1
	var numStr string

	switch {
	case strings.HasSuffix(input, "GB"):
		multiplier = 1024 * 1024 * 1024
		numStr = strings.TrimSuffix(input, "GB")
	case strings.HasSuffix(input, "MB"):
		multiplier = 1024 * 1024
		numStr = strings.TrimSuffix(input, "MB")
	case strings.HasSuffix(input, "KB"):
		multiplier = 1024
		numStr = strings.TrimSuffix(input, "KB")
	case strings.HasSuffix(input, "B"):
		multiplier = 1
		numStr = strings.TrimSuffix(input, "B")
	default:
		// Assume bytes if no suffix
		numStr = input
	}

	numStr = strings.TrimSpace(numStr)
	num, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size: %s", input)
	}

	return num * multiplier, nil
}

var (
	clientIdentifier = "bor"
	gitCommit        = "" // Git SHA1 commit hash of the release (set via linker flags)
	gitDate          = "" // Git commit date YYYYMMDD of the release (set via linker flags)
)

// tries unlocking the specified account a few times.
func unlockAccount(ks *keystore.KeyStore, address string, i int, passwords []string) (accounts.Account, string) {
	account, err := utils.MakeAddress(ks, address)

	if err != nil {
		utils.Fatalf("Could not list accounts: %v", err)
	}

	for trials := 0; trials < 3; trials++ {
		prompt := fmt.Sprintf("Unlocking account %s | Attempt %d/%d", address, trials+1, 3)
		password := utils.GetPassPhraseWithList(prompt, false, i, passwords)
		err = ks.Unlock(account, password)

		if err == nil {
			log.Info("Unlocked account", "address", account.Address.Hex())
			return account, password
		}

		if err, ok := err.(*keystore.AmbiguousAddrError); ok {
			log.Info("Unlocked account", "address", account.Address.Hex())
			return ambiguousAddrRecovery(ks, err, password), password
		}

		if err != keystore.ErrDecrypt {
			// No need to prompt again if the error is not decryption-related.
			break
		}
	}
	// All trials expended to unlock account, bail out
	utils.Fatalf("Failed to unlock account %s (%v)", address, err)

	return accounts.Account{}, ""
}

func ambiguousAddrRecovery(ks *keystore.KeyStore, err *keystore.AmbiguousAddrError, auth string) accounts.Account {
	log.Warn("Multiple key files exist for", "address", err.Addr)

	for _, a := range err.Matches {
		log.Info("Multiple keys", "file", a.URL.String())
	}

	log.Info("Testing your password against all of them...")

	var match *accounts.Account

	for _, a := range err.Matches {
		if err := ks.Unlock(a, auth); err == nil {
			// nolint:gosec, exportloopref
			match = &a
			break
		}
	}

	if match == nil {
		utils.Fatalf("None of the listed files could be unlocked.")
	}

	log.Info("Your password unlocked", "key", match.URL.String())
	log.Warn("In order to avoid this warning, you need to remove the following duplicate key files:")

	for _, a := range err.Matches {
		if a != *match {
			log.Warn("Duplicate", "key", a.URL.String())
		}
	}

	return *match
}

// getNodeKey creates a node key from set command line flags, either loading it
// from a file or as a specified hex value. If neither flags were provided, this
// method returns nil and an ephemeral key is to be generated.
func getNodeKey(hex string, file string) *ecdsa.PrivateKey {
	var (
		key *ecdsa.PrivateKey
		err error
	)

	switch {
	case file != "" && hex != "":
		utils.Fatalf("Options %q and %q are mutually exclusive", file, hex)
	case file != "":
		if key, err = crypto.LoadECDSA(file); err != nil {
			utils.Fatalf("Option %q: %v", file, err)
		}

		return key
	case hex != "":
		if key, err = crypto.HexToECDSA(hex); err != nil {
			utils.Fatalf("Option %q: %v", hex, err)
		}

		return key
	}

	return nil
}

func (c *Config) buildNode() (*node.Config, error) {
	ipcPath := ""
	if !c.JsonRPC.IPCDisable {
		ipcPath = clientIdentifier + ".ipc"
		if c.JsonRPC.IPCPath != "" {
			ipcPath = c.JsonRPC.IPCPath
		}
	}

	cfg := &node.Config{
		Name:                  clientIdentifier,
		UserIdent:             c.Identity,
		DataDir:               c.DataDir,
		DBEngine:              c.DBEngine,
		KeyStoreDir:           c.KeyStoreDir,
		UseLightweightKDF:     c.Accounts.UseLightweightKDF,
		InsecureUnlockAllowed: c.Accounts.AllowInsecureUnlock,
		Version:               params.VersionWithCommit(gitCommit, gitDate),
		IPCPath:               ipcPath,
		AllowUnprotectedTxs:   c.JsonRPC.AllowUnprotectedTxs,
		EnablePersonal:        c.JsonRPC.EnablePersonal,
		P2P: p2p.Config{
			MaxPeers:             int(c.P2P.MaxPeers),
			MaxPendingPeers:      int(c.P2P.MaxPendPeers),
			ListenAddr:           c.P2P.Bind + ":" + strconv.Itoa(int(c.P2P.Port)),
			DiscoveryV4:          c.P2P.Discovery.DiscoveryV4,
			DiscoveryV5:          c.P2P.Discovery.DiscoveryV5,
			TxArrivalWait:        c.P2P.TxArrivalWait,
			TxAnnouncementOnly:   c.P2P.TxAnnouncementOnly,
			DisableTxPropagation: c.P2P.DisableTxPropagation,
		},
		HTTPModules:         c.JsonRPC.Http.API,
		HTTPCors:            c.JsonRPC.Http.Cors,
		HTTPVirtualHosts:    c.JsonRPC.Http.VHost,
		HTTPPathPrefix:      c.JsonRPC.Http.Prefix,
		WSModules:           c.JsonRPC.Ws.API,
		WSOrigins:           c.JsonRPC.Ws.Origins,
		WSPathPrefix:        c.JsonRPC.Ws.Prefix,
		GraphQLCors:         c.JsonRPC.Graphql.Cors,
		GraphQLVirtualHosts: c.JsonRPC.Graphql.VHost,
		HTTPTimeouts: rpc.HTTPTimeouts{
			ReadTimeout:       c.JsonRPC.HttpTimeout.ReadTimeout,
			ReadHeaderTimeout: c.JsonRPC.HttpTimeout.ReadHeaderTimeout,
			WriteTimeout:      c.JsonRPC.HttpTimeout.WriteTimeout,
			IdleTimeout:       c.JsonRPC.HttpTimeout.IdleTimeout,
		},
		JWTSecret:                              c.JsonRPC.Auth.JWTSecret,
		AuthPort:                               int(c.JsonRPC.Auth.Port),
		AuthAddr:                               c.JsonRPC.Auth.Addr,
		AuthVirtualHosts:                       c.JsonRPC.Auth.VHosts,
		BatchRequestLimit:                      c.BatchRequestLimit,
		BatchResponseMaxSize:                   c.BatchResponseMaxSize,
		WSJsonRPCExecutionPoolSize:             c.JsonRPC.Ws.ExecutionPoolSize,
		WSJsonRPCExecutionPoolRequestTimeout:   c.JsonRPC.Ws.ExecutionPoolRequestTimeout,
		HTTPJsonRPCExecutionPoolSize:           c.JsonRPC.Http.ExecutionPoolSize,
		HTTPJsonRPCExecutionPoolRequestTimeout: c.JsonRPC.Http.ExecutionPoolRequestTimeout,
	}

	if c.P2P.NetRestrict != "" {
		list, err := netutil.ParseNetlist(c.P2P.NetRestrict)
		if err != nil {
			utils.Fatalf("Option %q: %v", c.P2P.NetRestrict, err)
		}

		cfg.P2P.NetRestrict = list
	}

	key := getNodeKey(c.P2P.NodeKeyHex, c.P2P.NodeKey)
	if key != nil {
		cfg.P2P.PrivateKey = key
	}

	// dev mode
	if c.Developer.Enabled {
		cfg.UseLightweightKDF = true

		// disable p2p networking
		c.P2P.NoDiscover = true
		cfg.P2P.ListenAddr = ""
		cfg.P2P.NoDial = true
		cfg.P2P.DiscoveryV5 = false
	}

	// enable jsonrpc endpoints
	{
		if c.JsonRPC.Http.Enabled {
			cfg.HTTPHost = c.JsonRPC.Http.Host
			cfg.HTTPPort = int(c.JsonRPC.Http.Port)
		}

		if c.JsonRPC.Ws.Enabled {
			cfg.WSHost = c.JsonRPC.Ws.Host
			cfg.WSPort = int(c.JsonRPC.Ws.Port)
		}
	}

	natif, err := nat.Parse(c.P2P.NAT)
	if err != nil {
		return nil, fmt.Errorf("wrong 'nat' flag: %v", err)
	}

	cfg.P2P.NAT = natif

	// only check for non-developer modes
	if !c.Developer.Enabled {
		// Discovery
		// Append the bootnodes defined with those hardcoded in the config file
		bootnodes := c.P2P.Discovery.Bootnodes
		if c.chain != nil {
			bootnodes = append(bootnodes, c.chain.Bootnodes...)
		}

		if cfg.P2P.BootstrapNodes, err = parseBootnodes(bootnodes); err != nil {
			return nil, err
		}

		if cfg.P2P.BootstrapNodesV5, err = parseBootnodes(c.P2P.Discovery.BootnodesV5); err != nil {
			return nil, err
		}

		if cfg.P2P.StaticNodes, err = parseBootnodes(c.P2P.Discovery.StaticNodes); err != nil {
			return nil, err
		}

		if len(cfg.P2P.StaticNodes) == 0 {
			cfg.P2P.StaticNodes = cfg.StaticNodes()
		}

		if cfg.P2P.TrustedNodes, err = parseBootnodes(c.P2P.Discovery.TrustedNodes); err != nil {
			return nil, err
		}

		if len(cfg.P2P.TrustedNodes) == 0 {
			cfg.P2P.TrustedNodes = cfg.TrustedNodes()
		}
	}

	if c.P2P.NoDiscover {
		// Disable peer discovery
		cfg.P2P.NoDiscovery = true
	}

	return cfg, nil
}

func (c *Config) Merge(cc ...*Config) error {
	for _, elem := range cc {
		if err := mergo.Merge(c, elem, mergo.WithOverwriteWithEmptyValue); err != nil {
			return fmt.Errorf("failed to merge configurations: %v", err)
		}
	}

	return nil
}

func MakeDatabaseHandles(max int) (int, error) {
	limit, err := fdlimit.Maximum()
	if err != nil {
		return -1, err
	}

	switch {
	case max == 0:
		// User didn't specify a meaningful value, use system limits
	case max < 128:
		// User specified something unhealthy, just use system defaults
		log.Error("File descriptor limit invalid (<128)", "had", max, "updated", limit)
	case max > limit:
		// User requested more than the OS allows, notify that we can't allocate it
		log.Warn("Requested file descriptors denied by OS", "req", max, "limit", limit)
	default:
		// User limit is meaningful and within allowed range, use that
		limit = max
	}

	raised, err := fdlimit.Raise(uint64(limit))
	if err != nil {
		return -1, err
	}

	return int(raised / 2), nil // Leave half for networking and other stuff
}

func parseBootnodes(urls []string) ([]*enode.Node, error) {
	dst := []*enode.Node{}

	for _, url := range urls {
		if url != "" {
			node, err := enode.Parse(enode.ValidSchemes, url)
			if err != nil {
				return nil, fmt.Errorf("invalid bootstrap url '%s': %v", url, err)
			}

			dst = append(dst, node)
		}
	}

	return dst, nil
}

func DefaultDataDir() string {
	// Try to place the data folder in the user's home dir
	home, _ := homedir.Dir()
	if home == "" {
		// we cannot guess a stable location
		return ""
	}

	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Bor")
	case "windows":
		appdata := os.Getenv("LOCALAPPDATA")
		if appdata == "" {
			// Windows XP and below don't have LocalAppData.
			panic("environment variable LocalAppData is undefined")
		}

		return filepath.Join(appdata, "Bor")
	default:
		return filepath.Join(home, ".bor")
	}
}

func Hostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "bor"
	}

	return hostname
}

func MakePasswordListFromFile(path string) ([]string, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read password file: %v", err)
	}

	lines := strings.Split(string(text), "\n")

	// Sanitise DOS line endings.
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}

	return lines, nil
}

// parseGoMemLimit parses a GOMEMLIMIT string (e.g. "100GB", "1024MB", "off")
// into bytes. Returns math.MaxInt64 for "off" (no limit).
func parseGoMemLimit(value string) (int64, error) {
	if value == "off" {
		return math.MaxInt64, nil
	}

	// Split numeric prefix from unit suffix
	i := 0
	for i < len(value) && (value[i] >= '0' && value[i] <= '9' || value[i] == '.') {
		i++
	}

	if i == 0 {
		return 0, fmt.Errorf("invalid GOMEMLIMIT: no numeric value in %q", value)
	}

	numStr := value[:i]
	unit := value[i:]

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid GOMEMLIMIT number %q: %v", numStr, err)
	}

	var multiplier float64

	switch unit {
	case "", "B":
		multiplier = 1
	case "KB", "KiB":
		multiplier = 1024
	case "MB", "MiB":
		multiplier = 1024 * 1024
	case "GB", "GiB":
		multiplier = 1024 * 1024 * 1024
	case "TB", "TiB":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("invalid GOMEMLIMIT unit %q (expected B, KB, MB, GB, TB)", unit)
	}

	return int64(num * multiplier), nil
}

// sanitizeGoGC clamps GOGC values to reasonable bounds
func sanitizeGoGC(value int) int {
	const (
		minGoGC = 10
		maxGoGC = 2000
	)

	if value < minGoGC {
		return minGoGC
	}
	if value > maxGoGC {
		return maxGoGC
	}
	return value
}

// validateGoDebug validates GODEBUG values for known debug variables
func validateGoDebug(value string) error {
	// Known GODEBUG variables (not exhaustive, but covers common ones)
	knownVars := map[string]bool{
		"gctrace":            true, // GC trace
		"gcpacertrace":       true, // GC pacer trace
		"madvdontneed":       true, // Memory management
		"scavenge":           true, // Scavenger debug
		"asyncpreempt":       true, // Async preemption
		"cgocheck":           true, // CGO check
		"schedtrace":         true, // Scheduler trace
		"scheddetail":        true, // Scheduler detail
		"tracebackancestors": true, // Traceback ancestors
		"httpmuxgo121":       true, // HTTP mux
		"netdns":             true, // DNS resolution
		"tls13":              true, // TLS 1.3
		"panicnil":           true, // Panic on nil
		"invalidptr":         true, // Invalid pointer checking
		"sbrk":               true, // Memory allocation
		"efence":             true, // Electric fence
		"inittrace":          true, // Init trace
		"cpu.all":            true, // CPU features
	}

	// Split by comma and validate each key=value pair
	pairs := strings.Split(value, ",")
	for _, pair := range pairs {
		if strings.TrimSpace(pair) == "" {
			continue
		}

		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid GODEBUG format: '%s', expected key=value", pair)
		}

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		if key == "" {
			return fmt.Errorf("empty GODEBUG variable name in: '%s'", pair)
		}

		// Validate value (most GODEBUG vars use 0/1 or numeric values)
		if val == "" {
			return fmt.Errorf("empty GODEBUG value for '%s'", key)
		}

		// Warn about unknown variables but don't fail (Go may add new ones)
		if !knownVars[key] {
			log.Warn("Unknown GODEBUG variable", "var", key, "value", val)
		}

		// Validate numeric values where appropriate
		if key == "gctrace" || key == "gcpacertrace" || key == "schedtrace" || key == "cgocheck" {
			if _, err := strconv.Atoi(val); err != nil {
				return fmt.Errorf("GODEBUG variable '%s' expects numeric value, got '%s'", key, val)
			}
		}
	}

	return nil
}

// loadFilteredAddresses loads newline-separated addresses to filter from the specified file.
func loadFilteredAddresses(filePath string) (map[common.Address]struct{}, error) {
	if filePath == "" {
		return make(map[common.Address]struct{}), nil
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warn("Filtered addresses file not found", "file", filePath)
			return make(map[common.Address]struct{}), nil
		}
		return nil, fmt.Errorf("failed to read filtered addresses file: %v", err)
	}

	filteredAddrs := make(map[common.Address]struct{})
	content := strings.TrimSpace(string(data))
	if content == "" {
		log.Info("Empty filtered addresses file", "file", filePath)
		return filteredAddrs, nil
	}

	addresses := strings.Split(content, "\n")
	for i, addrStr := range addresses {
		addrStr = strings.TrimSpace(addrStr)
		if addrStr == "" {
			continue
		}
		if !common.IsHexAddress(addrStr) {
			log.Warn("Invalid address in filtered addresses file", "file", filePath, "position", i+1, "address", addrStr)
			continue
		}
		addr := common.HexToAddress(addrStr)
		filteredAddrs[addr] = struct{}{}
	}

	log.Info("Loaded filtered addresses", "count", len(filteredAddrs), "file", filePath)
	return filteredAddrs, nil
}
