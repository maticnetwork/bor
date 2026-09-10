# Server

The ```bor server``` command runs the Bor client.

## Options

- ```bor.devfakeauthor```: Run miner without validator set authorization [dev mode] : Use with '--bor.withoutheimdall' (default: false)

- ```bor.heimdall```: URL of Heimdall service (comma-separated for failover: "url1,url2") (default: http://localhost:1317)

- ```bor.heimdallWS```: Address of Heimdall WS subscription service (comma-separated for failover: "addr1,addr2")

- ```bor.heimdallgRPC```: Address of Heimdall gRPC service (comma-separated for failover: "addr1,addr2")

- ```bor.heimdalltimeout```: Timeout period for bor's outgoing requests to heimdall (default: 5s)

- ```bor.logs```: Enables bor log retrieval (default: false)

- ```bor.runheimdall```: Run Heimdall service as a child process (default: false)

- ```bor.runheimdallargs```: Arguments to pass to Heimdall service

- ```bor.useheimdallapp```: Use child heimdall process to fetch data, Only works when bor.runheimdall is true (default: false)

- ```bor.withoutheimdall```: Run without Heimdall service (for testing purpose) (default: false)

- ```chain```: Name of the chain to sync ('amoy', 'mumbai', 'mainnet') or path to a genesis file (default: mainnet)

- ```config```: Path to the TOML configuration file

- ```datadir```: Path of the data directory to store information

- ```datadir.ancient```: Data directory for ancient chain segments (default = inside chaindata)

- ```db.engine```: Backing database implementation to use ('leveldb' or 'pebble') (default: pebble)

- ```dev```: Enable developer mode with ephemeral proof-of-authority network and a pre-funded developer account, mining enabled (default: false)

- ```dev.gaslimit```: Initial block gas limit (default: 11500000)

- ```dev.period```: Block period to use in developer mode (0 = mine only if transaction pending) (default: 0)

- ```disable-blind-fork-validation```: Disable additional fork validation and accept blind forks without tracing back to last whitelisted entry (default: false)

- ```disable-bor-wallet```: Disable the personal wallet endpoints (default: true)

- ```eth.requiredblocks```: Comma separated block number-to-hash mappings to require for peering (<number>=<hash>)

- ```ethstats```: Reporting URL of a ethstats service (nodename:secret@host:port)

- ```evm-switch-dispatch```: Enable switch-based fast path EVM interpreter (default: false)

- ```gcmode```: Blockchain garbage collection mode ("full", "archive") (default: full)

- ```gpo.blocks```: Number of recent blocks to check for gas prices (default: 20)

- ```gpo.ignoreprice```: Gas price below which gpo will ignore transactions (default: 25000000000)

- ```gpo.maxblockhistory```: Maximum block history of gasprice oracle (default: 1024)

- ```gpo.maxheaderhistory```: Maximum header history of gasprice oracle (default: 1024)

- ```gpo.maxprice```: Maximum gas price will be recommended by gpo (default: 500000000000)

- ```gpo.percentile```: Suggested gas price is the given percentile of a set of recent transaction gas prices (default: 60)

- ```grpc.addr```: Address and port to bind the GRPC server. Empty disables the server. Non-loopback binds without --grpc.token log a startup warning. (default: 127.0.0.1:3131)

- ```grpc.token```: Raw token expected in the `authorization: Bearer <token>` header of incoming gRPC calls (empty disables auth; the `Bearer ` prefix is stripped before comparison). Prefer the BOR_GRPC_TOKEN environment variable over this flag.

- ```history.logs```: Number of recent blocks to maintain log search index for (default = about 2 months, 0 = entire chain) (default: 2350000)

- ```history.logs.disable```: Do not maintain log search index (default: false)

- ```history.state```: Number of recent blocks to retain state history for, only relevant in state.scheme=path (default = 90,000 blocks, 0 = entire chain) (default: 90000)

- ```history.transactions```: Number of recent blocks to maintain transactions index for (default = about 2 months, 0 = entire chain) (default: 2350000)

- ```identity```: Name/Identity of the node

- ```keystore```: Path of the directory where keystores are located

- ```max-blind-fork-validation-limit```: Maximum number of blocks to traverse back in the database when validating blind forks (default: 256)

- ```parallelevm.enable```: Enable Block STM (default: true)

- ```parallelevm.enforce```: Enforce block processing via Block STM (default: false)

- ```parallelevm.procs```: Number of speculative processes (cores) in Block STM (default: 8)

- ```pprof```: Enable the pprof HTTP server (default: false)

- ```pprof.addr```: pprof HTTP server listening interface (default: 127.0.0.1)

- ```pprof.blockprofilerate```: Turn on block profiling with the given rate (default: 0)

- ```pprof.memprofilerate```: Turn on memory profiling with the given rate (default: 524288)

- ```pprof.port```: pprof HTTP server listening port (default: 6060)

- ```rpc.batch-request-limit```: Maximum number of requests in a batch (use 0 for no limits) (default: 1000)

- ```rpc.batch-response-max-size```: Maximum number of response bytes across all requests in a batch (use 0 for no limits) (default: 25000000)

- ```rpc.returndatalimit```: Maximum size (in bytes) a result of an rpc request could have (use 0 for no limits) (default: 100000)

- ```snapshot```: Enables the snapshot-database mode (default: true)

- ```state.scheme```: Scheme to use for storing ethereum state ('hash' or 'path') (default: path)

- ```syncmode```: Blockchain sync mode ("full", "snap" or "stateless") (default: full)

- ```verbosity```: Logging verbosity for the server (5=trace|4=debug|3=info|2=warn|1=error|0=crit) (default: 3)

- ```vmdebug```: Record information useful for VM and contract debugging (default: false)

- ```vmtrace```: Name of a tracer to record internal VM operations during blockchain synchronization (costly) (e.g. 'json')

- ```vmtrace.jsonconfig```: Tracer configuration (JSON) (default: {})

- ```witness.enable```: Enable witness protocol (default: false)

- ```witness.fastforwardthreshold```: Minimum necessary distance between local header and chain tip to trigger fast forward (default: 6400)

- ```witness.filestore```: Store witness blobs on the filesystem instead of the key-value database (default: true)

- ```witness.parallelstatelessimport```: Enable parallel stateless block import (default: false)

- ```witness.parallelstatelessimportworkers```: Number of workers to use for parallel stateless import (0 = GOMAXPROCS) (default: 0)

- ```witness.producewitnesses```: Produce witnesses while syncing (default: false)

- ```witness.syncwithwitnesses```: Sync blocks with witnesses (default: false)

- ```witness.witnessapi```: Enable witness API endpoints (by default disabled) (default: false)

### Account Management Options

- ```allow-insecure-unlock```: Allow insecure account unlocking when account-related RPCs are exposed by http (default: false)

- ```lightkdf```: Reduce key-derivation RAM & CPU usage at some expense of KDF strength (default: false)

- ```password```: Password file to use for non-interactive password input

- ```unlock```: Comma separated list of accounts to unlock

### Cache Options

- ```cache```: Megabytes of memory allocated to internal caching (default: 1024)

- ```cache.addresscachesizes```: Address-specific cache sizes for biased caching in MB (format: address=sizeMB,address=sizeMB, e.g. 0x1234...=1024,0x5678...=512)

- ```cache.blocklogs```: Size (in number of blocks) of the log cache for filtering (default: 32)

- ```cache.database```: Percentage of cache memory allowance to use for database io (default: 50)

- ```cache.gc```: Percentage of cache memory allowance to use for trie pruning (default: 25)

- ```cache.godebug```: Set GODEBUG variables for runtime debugging (e.g. 'gctrace=1,gcpacertrace=1')

- ```cache.gogc```: Set GOGC percentage for garbage collection trigger (default: 100) (default: 100)

- ```cache.gomemlimit```: Set GOMEMLIMIT for the runtime (e.g. '34GB', '34359738368'). Empty means no limit

- ```cache.noprefetch```: Disable heuristic state prefetch during block import (less CPU and disk IO, more time waiting for data) (default: false)

- ```cache.preimages```: Enable recording the SHA3/keccak preimages of trie keys (default: false)

- ```cache.preloadratelimit```: Rate limit per address for cache preloading (e.g. 500KB, 1MB, 0 for unlimited). Limits I/O during sync. Default: 1MB

- ```cache.snapshot```: Percentage of cache memory allowance to use for snapshot caching (default: 10)

- ```cache.trie```: Percentage of cache memory allowance to use for trie caching (default: 15)

- ```cache.triesinmemory```: Number of block states (tries) to keep in memory (default: 128)

- ```fdlimit```: Raise the open file descriptor resource limit (default = system fd limit) (default: 0)

- ```txlookuplimit```: Number of recent blocks to maintain transactions index for (soon to be deprecated, use history.transactions instead) (default: 2350000)

### ExtraDB Options

- ```leveldb.compaction.table.size```: LevelDB SSTable/file size in mebibytes (default: 2)

- ```leveldb.compaction.table.size.multiplier```: Multiplier on LevelDB SSTable/file size. Size for a level is determined by: `leveldb.compaction.table.size * (leveldb.compaction.table.size.multiplier ^ Level)` (default: 1)

- ```leveldb.compaction.total.size```: Total size in mebibytes of SSTables in a given LevelDB level. Size for a level is determined by: `leveldb.compaction.total.size * (leveldb.compaction.total.size.multiplier ^ Level)` (default: 10)

- ```leveldb.compaction.total.size.multiplier```: Multiplier on level size on LevelDB levels. Size for a level is determined by: `leveldb.compaction.total.size * (leveldb.compaction.total.size.multiplier ^ Level)` (default: 10)

### Health Options

- ```health.max-goroutine-threshold```: Maximum number of goroutines before health check fails (0 = disabled) (default: 0)

- ```health.min-peer-threshold```: Minimum number of peers before health check fails (0 = disabled) (default: 0)

- ```health.warn-goroutine-threshold```: Maximum number of goroutines before health check warns (0 = disabled) (default: 0)

- ```health.warn-peer-threshold```: Minimum number of peers before health check warns (0 = disabled) (default: 0)

### JsonRPC Options

- ```accept-preconf-tx```: Allows the RPC server to accept transactions for preconfirmation (default: false)

- ```accept-private-tx```: Allows the RPC server to accept private transactions (default: false)

- ```authrpc.addr```: Listening address for authenticated APIs (default: localhost)

- ```authrpc.jwtsecret```: Path to a JWT secret to use for authenticated RPC endpoints

- ```authrpc.port```: Listening port for authenticated APIs (default: 8551)

- ```authrpc.vhosts```: Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard. (default: localhost)

- ```graphql```: Enable GraphQL on the HTTP-RPC server. Note that GraphQL can only be started if an HTTP server is started as well. (default: false)

- ```graphql.corsdomain```: Comma separated list of domains from which to accept cross origin requests (browser enforced) (default: localhost)

- ```graphql.vhosts```: Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard. (default: localhost)

- ```http```: Enable the HTTP-RPC server (default: false)

- ```http.addr```: HTTP-RPC server listening interface (default: localhost)

- ```http.api```: API's offered over the HTTP-RPC interface (default: eth,net,web3,txpool,bor)

- ```http.corsdomain```: Comma separated list of domains from which to accept cross origin requests (browser enforced) (default: localhost)

- ```http.ep-requesttimeout```: Request Timeout for rpc execution pool for HTTP requests (default: 0s)

- ```http.ep-size```: Maximum size of workers to run in rpc execution pool for HTTP requests (default: 40)

- ```http.port```: HTTP-RPC server listening port (default: 8545)

- ```http.rpcprefix```: HTTP path path prefix on which JSON-RPC is served. Use '/' to serve on all paths.

- ```http.vhosts```: Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard. (default: localhost)

- ```ipcdisable```: Disable the IPC-RPC server (default: false)

- ```ipcpath```: Filename for IPC socket/pipe within the datadir (explicit paths escape it)

- ```rpc.allow-unprotected-txs```: Allow for unprotected (non EIP155 signed) transactions to be submitted via RPC (default: false)

- ```rpc.enabledeprecatedpersonal```: Enables the (deprecated) personal namespace (default: false)

- ```rpc.enabletrace```: Enables the Parity-compatible trace namespace (trace_block, trace_transaction, trace_call, trace_callMany, trace_replayTransaction, trace_replayBlockTransactions) (default: false)

- ```rpc.evmtimeout```: Sets a timeout used for eth_call (0=infinite) (default: 5s)

- ```rpc.gascap```: Sets a cap on gas that can be used in eth_call/estimateGas (0=infinite) (default: 50000000)

- ```rpc.logquerylimit```: Maximum number of alternative addresses or topics allowed per search position in eth_getLogs filter criteria (0 = no cap) (default: 1000)

- ```rpc.rangelimit```: Maximum block range allowed for eth_getLogs and bor_getLogs (0 = no limit) (default: 0)

- ```rpc.txfeecap```: Sets a cap on transaction fee (in ether) that can be sent via the RPC APIs (0 = no cap) (default: 1)

- ```rpc.txsync.defaulttimeout```: Default timeout for eth_sendRawTransactionSync (e.g. 2s, 500ms) (default: 20s)

- ```rpc.txsync.maxtimeout```: Maximum allowed timeout for eth_sendRawTransactionSync (e.g. 5m) (default: 1m0s)

- ```ws```: Enable the WS-RPC server (default: false)

- ```ws.addr```: WS-RPC server listening interface (default: localhost)

- ```ws.api```: API's offered over the WS-RPC interface (default: net,web3)

- ```ws.ep-requesttimeout```: Request Timeout for rpc execution pool for WS requests (default: 0s)

- ```ws.ep-size```: Maximum size of workers to run in rpc execution pool for WS requests (default: 40)

- ```ws.origins```: Origins from which to accept websockets requests (default: localhost)

- ```ws.port```: WS-RPC server listening port (default: 8546)

- ```ws.rpcprefix```: HTTP path prefix on which JSON-RPC is served. Use '/' to serve on all paths.

### Logging Options

- ```log.backtrace```: Request a stack trace at a specific logging statement (e.g. 'block.go:271')

- ```log.debug```: Prepends log messages with call-site location (file and line number) (default: false)

- ```log.enable-block-tracking```: Enables additional logging of information collected while tracking block lifecycle (default: false)

- ```log.json```: Format logs with JSON (default: false)

- ```vmodule```: Per-module verbosity: comma-separated list of <pattern>=<level> (e.g. eth/*=5,p2p=4)

### P2P Options

- ```bind```: Network binding address (default: 0.0.0.0)

- ```bootnodes```: Comma separated enode URLs for P2P discovery bootstrap

- ```disable-tx-propagation```: Disable transaction broadcast and announcements to all peers (default: false)

- ```discovery.dns```: Comma separated list of enrtree:// URLs which will be queried for nodes to connect to

- ```maxpeers```: Maximum number of network peers (network disabled if set to 0) (default: 50)

- ```maxpendpeers```: Maximum number of pending connection attempts (default: 50)

- ```nat```: NAT port mapping mechanism (any|none|upnp|pmp|extip:<IP>) (default: any)

- ```netrestrict```: Restricts network communication to the given IP networks (CIDR masks)

- ```nodekey```:  P2P node key file

- ```nodekeyhex```: P2P node key as hex

- ```nodiscover```: Disables the peer discovery mechanism (manual peer addition) (default: false)

- ```p2p.nosnap```: Disable serving snap sync requests to peers (snap/1 protocol is not advertised) (default: false)

- ```port```: Network listening port (default: 30303)

- ```relay.bp-rpc-endpoints```: Comma separated rpc endpoints of all block producers

- ```relay.enable-preconfs```: Enable transaction preconfirmations (default: false)

- ```relay.enable-private-tx```: Enable private transaction submission (default: false)

- ```txannouncementonly```: Whether to only announce transactions to peers (default: false)

- ```txarrivalwait```: Maximum duration to wait for a transaction before explicitly requesting it (default: 500ms)

- ```v4disc```: Enables the V4 discovery mechanism (default: true)

- ```v5disc```: Enables the V5 discovery mechanism (default: true)

### Pipeline Options

- ```pipeline.enable-import-src```: Enable pipelined state root computation during block import: overlap SRC(N) with block N+1 tx execution (default: false)

- ```pipeline.import-src-logs```: Enable verbose logging for pipelined import SRC (default: false)

- ```pipeline.warm-snapshot```: Enable warm-node handoff from the execution-side trie prefetcher to the pipelined SRC when witnesses are produced; no effect when import SRC is disabled or witnesses are off (default: true)

### Sealer Options

- ```allow-gas-tip-override```: Allows block producers to override the mining gas tip (default: false)

- ```mine```: Enable mining (default: false)

- ```miner.baseFeeBuffer```: Buffer around target base fee in wei (no adjustment when within buffer) (default: 300000000000)

- ```miner.baseFeeChangeDenominator```: Base fee change rate denominator (must be >0, default 64) for post-Lisovo blocks (default: 0)

- ```miner.blocktime```: The block time defined by the miner. Needs to be larger or equal to the consensus block time. If not set (default = 0), the miner will use the consensus block time. (default: 0s)

- ```miner.disable-pending-block```: Disable the pending block creation loop on non block producer nodes. When set, 'pending' block will be unavailable for RPC queries. (default: false)

- ```miner.enableDynamicGasLimit```: Enable dynamic gas limit adjustment based on base fee (default: false)

- ```miner.enableDynamicTargetGas```: Enable dynamic EIP-1559 target gas percentage adjustment based on base fee (post-Lisovo, mutually exclusive with enableDynamicGasLimit) (default: false)

- ```miner.etherbase```: Public address for block mining rewards

- ```miner.extradata```: Block extra data set by the miner (default = client version)

- ```miner.gasLimitMax```: Maximum gas limit when dynamic gas limit is enabled (default: 65000000)

- ```miner.gasLimitMin```: Minimum gas limit when dynamic gas limit is enabled (default: 50000000)

- ```miner.gaslimit```: Target gas ceiling (gas limit) for mined blocks (default: 45000000)

- ```miner.gasprice```: Minimum gas price for mining a transaction (default: 25000000000)

- ```miner.interruptcommit```: Interrupt block commit when block creation time is passed (default: true)

- ```miner.prefetch```: Enable transaction prefetching from the pool during block building (default: false)

- ```miner.prefetch.gaslimit.percent```: Gas limit percentage for prefetching (e.g., 100 = 100%, 110 = 110%) (default: 100)

- ```miner.recommit```: The time interval for miner to re-create mining work (default: 2m5s)

- ```miner.targetBaseFee```: Target base fee in wei for dynamic gas limit (e.g., 30000000000 for 30 gwei) (default: 500000000000)

- ```miner.targetGasMaxPercentage```: Maximum target gas percentage (1-100) when dynamic target gas is enabled (default: 80)

- ```miner.targetGasMinPercentage```: Minimum target gas percentage (1-100) when dynamic target gas is enabled (default: 50)

- ```miner.targetGasPercentage```: Target gas as percentage of gas limit (1-100, default 65) for post-Lisovo blocks (default: 0)

### Telemetry Options

- ```metrics```: Enable metrics collection and reporting (default: false)

- ```metrics.expensive```: Enable expensive metrics collection and reporting (default: false)

- ```metrics.influxdb```: Enable metrics export/push to an external InfluxDB database (v1) (default: false)

- ```metrics.influxdb.bucket```: InfluxDB bucket name to push reported metrics to (v2 only)

- ```metrics.influxdb.database```: InfluxDB database name to push reported metrics to

- ```metrics.influxdb.endpoint```: InfluxDB API endpoint to report metrics to

- ```metrics.influxdb.organization```: InfluxDB organization name (v2 only)

- ```metrics.influxdb.password```: Password to authorize access to the database

- ```metrics.influxdb.tags```: Comma-separated InfluxDB tags (key/values) attached to all measurements

- ```metrics.influxdb.token```: Token to authorize access to the database (v2 only)

- ```metrics.influxdb.username```: Username to authorize access to the database

- ```metrics.influxdbv2```: Enable metrics export/push to an external InfluxDB v2 database (default: false)

- ```metrics.opencollector-endpoint```: OpenCollector Endpoint (host:port)

- ```metrics.prometheus-addr```: Address for Prometheus Server (default: 127.0.0.1:7071)

### Transaction Pool Options

- ```txpool.accountqueue```: Maximum number of non-executable transaction slots permitted per account (default: 64)

- ```txpool.accountslots```: Minimum number of executable transaction slots guaranteed per account (default: 16)

- ```txpool.filtered-addresses```: Path to the file containing a newline-separated list of addresses whose transactions will be filtered

- ```txpool.globalqueue```: Maximum number of non-executable transaction slots for all accounts (default: 131072)

- ```txpool.globalslots```: Maximum number of executable transaction slots for all accounts (default: 131072)

- ```txpool.journal```: Disk journal for local transaction to survive node restarts (default: transactions.rlp)

- ```txpool.lifetime```: Maximum amount of time non-executable transaction are queued (default: 3h0m0s)

- ```txpool.locals```: Comma separated accounts to treat as locals (no flush, priority inclusion)

- ```txpool.nolocals```: Disables price exemptions for locally submitted transactions (default: false)

- ```txpool.pricebump```: Price bump percentage to replace an already existing transaction (default: 10)

- ```txpool.pricelimit```: Minimum gas price limit to enforce for acceptance into the pool (default: 25000000000)

- ```txpool.rebroadcast```: Enable stuck transaction rebroadcast mechanism (default: true)

- ```txpool.rebroadcast-batch-size```: Maximum number of transactions to rebroadcast per cycle (default: 200)

- ```txpool.rebroadcast-interval```: Interval between rebroadcast checks for stuck transactions (default: 30s)

- ```txpool.rebroadcast-max-age```: Maximum age for a transaction to be eligible for rebroadcast (default: 10m0s)

- ```txpool.rejournal```: Time interval to regenerate the local transaction journal (default: 1h0m0s)