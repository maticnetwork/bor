// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package ethconfig contains the configuration of the ETH and LES protocols.
package ethconfig

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/bor"
	borabi "github.com/ethereum/go-ethereum/consensus/bor/abi"
	"github.com/ethereum/go-ethereum/consensus/bor/contract"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall" //nolint:typecheck
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdallgrpc"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdallws"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/history"
	"github.com/ethereum/go-ethereum/core/txpool/blobpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/params"
)

// parseURLs splits a comma-separated URL string into a trimmed, non-empty slice.
func parseURLs(s string) []string {
	if s == "" {
		return nil
	}

	parts := strings.Split(s, ",")

	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}

	return out
}

// FullNodeGPO contains default gasprice oracle settings for full node.
var FullNodeGPO = gasprice.Config{
	Blocks:           20,
	Percentile:       60,
	MaxHeaderHistory: 1024,
	MaxBlockHistory:  1024,
	MaxPrice:         gasprice.DefaultMaxPrice,
	IgnorePrice:      gasprice.DefaultIgnorePrice,
}

// Defaults contains default settings for use on the Ethereum main net.
var Defaults = Config{
	SyncMode:              downloader.SnapSync,
	HistoryMode:           history.KeepAll,
	NetworkId:             0, // enable auto configuration of networkID == chainID
	TxLookupLimit:         2350000,
	TransactionHistory:    2350000, // Note: used in bor cli
	LogHistory:            2350000, // Note: used in bor cli
	StateHistory:          params.FullImmutabilityThreshold,
	DatabaseCache:         512,
	TrieCleanCache:        154,
	TrieDirtyCache:        256,
	TrieTimeout:           60 * time.Minute,
	SnapshotCache:         102,
	FilterLogCacheSize:    32,
	LogQueryLimit:         1000,
	Miner:                 miner.DefaultConfig,
	TxPool:                legacypool.DefaultConfig,
	BlobPool:              blobpool.DefaultConfig,
	RPCGasCap:             50000000,
	RPCEVMTimeout:         5 * time.Second,
	GPO:                   FullNodeGPO,
	RPCTxFeeCap:           1, // 1 ether
	FastForwardThreshold:  6400,
	WitnessPruneThreshold: 64000,
	WitnessPruneInterval:  120 * time.Second,
	WitnessAPIEnabled:     false,
	TxSyncDefaultTimeout:  20 * time.Second,
	TxSyncMaxTimeout:      1 * time.Minute,
}

//go:generate go run github.com/fjl/gencodec -type Config -formats toml -out gen_config.go

// Config contains configuration options for ETH and LES protocols.
type Config struct {
	// The genesis block, which is inserted if the database is empty.
	// If nil, the Ethereum main net block is used.
	Genesis *core.Genesis `toml:",omitempty"`

	// Network ID separates blockchains on the peer-to-peer networking level. When left
	// zero, the chain ID is used as network ID.
	NetworkId uint64
	SyncMode  downloader.SyncMode

	// HistoryMode configures chain history retention.
	HistoryMode history.HistoryMode

	// This can be set to list of enrtree:// URLs which will be queried for
	// nodes to connect to.
	EthDiscoveryURLs  []string
	SnapDiscoveryURLs []string

	// State options.
	NoPruning  bool // Whether to disable pruning and flush everything to disk
	NoPrefetch bool // Whether to disable prefetching and only load state on demand

	// Pipelined import SRC: overlap SRC(N) with tx execution of block N+1 during import
	EnablePipelinedImportSRC bool
	PipelinedImportSRCLogs   bool

	// PipelinedSRCWarmSnapshot enables warm-cache handoff from the
	// execution-side trie prefetcher to the pipelined SRC goroutine. Trie
	// reads in SRC consult a hash-verified snapshot before falling through
	// to pathdb. Targets cold-cache restart/catch-up CPU. NewTrieOnly
	// semantics, witness completeness, and root determinism are unaffected.
	PipelinedSRCWarmSnapshot bool

	// Deprecated: use 'TransactionHistory' instead.
	TxLookupLimit uint64 `toml:",omitempty"` // The maximum number of blocks from head whose tx indices are reserved.

	TransactionHistory   uint64 `toml:",omitempty"` // The maximum number of blocks from head whose tx indices are reserved.
	LogHistory           uint64 `toml:",omitempty"` // The maximum number of blocks from head where a log search index is maintained.
	LogNoHistory         bool   `toml:",omitempty"` // No log search index is maintained.
	LogExportCheckpoints string // export log index checkpoints to file
	StateHistory         uint64 `toml:",omitempty"` // The maximum number of blocks from head whose state histories are reserved.

	// State scheme represents the scheme used to store ethereum states and trie
	// nodes on top. It can be 'hash', 'path', or none which means use the scheme
	// consistent with persistent state.
	StateScheme string `toml:",omitempty"`

	// RequiredBlocks is a set of block number -> hash mappings which must be in the
	// canonical chain of all remote peers. Setting the option makes geth verify the
	// presence of these blocks for every new peer connection.
	RequiredBlocks map[uint64]common.Hash `toml:"-"`

	// Database options
	SkipBcVersionCheck bool `toml:"-"`
	DatabaseHandles    int  `toml:"-"`
	DatabaseCache      int
	DatabaseFreezer    string
	DatabaseEra        string

	// Database - LevelDB options
	LevelDbCompactionTableSize           uint64
	LevelDbCompactionTableSizeMultiplier float64
	LevelDbCompactionTotalSize           uint64
	LevelDbCompactionTotalSizeMultiplier float64

	TrieCleanCache int
	TrieDirtyCache int
	TrieTimeout    time.Duration
	SnapshotCache  int
	Preimages      bool
	TriesInMemory  uint64
	// Directory path to the journal used for persisting trie data across node restarts.
	TrieJournalDirectory string

	// This is the number of blocks for which logs will be cached in the filter system.
	FilterLogCacheSize int

	// This is the maximum number of addresses or topics allowed in filter criteria
	// for eth_getLogs.
	LogQueryLimit int

	// Address-specific cache sizes for biased caching (pathdb only)
	// Maps account address to cache size in bytes
	AddressCacheSizes map[common.Address]int

	// PreloadRateLimit limits cache preload I/O in bytes per second per address.
	// This prevents preloading from overwhelming the disk during sync.
	// 0 = unlimited (legacy behavior), default = 1MB/s
	PreloadRateLimit int64

	// Mining options
	Miner miner.Config

	// Transaction pool options
	TxPool   legacypool.Config
	BlobPool blobpool.Config

	// Gas Price Oracle options
	GPO gasprice.Config

	// Enables tracking of SHA3 preimages in the VM
	EnablePreimageRecording bool

	// Enables collection of witness trie access statistics
	EnableWitnessStats bool

	// Generate execution witnesses and self-check against them (testing purpose)
	StatelessSelfValidation bool

	// Use switch-based fast path interpreter
	EnableEVMSwitchDispatch bool

	// Enables tracking of state size
	EnableStateSizeTracking bool

	// Enables VM tracing
	VMTrace           string
	VMTraceJsonConfig string

	// RPCGasCap is the global gas cap for eth-call variants.
	RPCGasCap uint64

	// Maximum size (in bytes) a result of an rpc request could have
	RPCReturnDataLimit uint64

	// RPCEVMTimeout is the global timeout for eth-call.
	RPCEVMTimeout time.Duration

	// RPCTxFeeCap is the global transaction fee (price * gas limit) cap for
	// send-transaction variants. The unit is ether.
	RPCTxFeeCap float64

	// RPCBlockRangeLimit is the maximum block range allowed in eth_getLogs / bor_getLogs (0 = unlimited)
	RPCBlockRangeLimit uint64

	// URL to connect to Heimdall node (comma-separated for failover: "url1,url2,url3")
	HeimdallURL string

	// timeout in heimdall requests
	HeimdallTimeout time.Duration

	// No heimdall service
	WithoutHeimdall bool

	// Address to connect to Heimdall gRPC server (comma-separated for failover: "addr1,addr2")
	HeimdallgRPCAddress string

	// Address to connect to Heimdall WS subscription server (comma-separated for failover: "addr1,addr2")
	HeimdallWSAddress string

	// Run heimdall service as a child process
	RunHeimdall bool

	// Arguments to pass to heimdall service
	RunHeimdallArgs string

	// Use child heimdall process to fetch data, Only works when RunHeimdall is true
	UseHeimdallApp bool

	// OverrideHeimdallClient allows injecting a mock HeimdallClient for testing.
	// When set, this client is used instead of creating one from HeimdallURL/HeimdallgRPCAddress.
	OverrideHeimdallClient bor.IHeimdallClient `toml:"-"`

	// Bor logs flag
	BorLogs bool

	// Parallel EVM (Block-STM) related config
	ParallelEVM core.ParallelEVMConfig `toml:",omitempty"`

	// WitnessProtocol enabless the wit protocol
	WitnessProtocol bool

	// NoSnapServing disables the snap/1 sub-protocol, preventing this node from
	// serving snap sync state to peers. Useful for block producers that should
	// not spend resources on snap sync serving.
	NoSnapServing bool

	// SyncWithWitnesses enables syncing blocks with witnesses
	SyncWithWitnesses bool

	// SyncAndProduceWitnesses enables producing witnesses while syncing
	SyncAndProduceWitnesses bool

	// Develop Fake Author mode to produce blocks without authorisation
	DevFakeAuthor bool `hcl:"devfakeauthor,optional" toml:"devfakeauthor,optional"`

	// OverrideOsaka (TODO: remove after the fork)
	OverrideOsaka *big.Int `toml:",omitempty"`

	// OverrideVerkle (TODO: remove after the fork)
	OverrideVerkle *big.Int `toml:",omitempty"`

	// EnableBlockTracking allows logging of information collected while tracking block lifecycle
	EnableBlockTracking bool

	// Minimum necessary distance between local header and peer to fast forward
	FastForwardThreshold uint64

	// Minimum necessary distance between local header and latest non pruned witness
	WitnessPruneThreshold uint64

	// The time interval between each witness prune routine
	WitnessPruneInterval time.Duration

	// EnableParallelStatelessImport toggles parallel stateless block import (download path)
	EnableParallelStatelessImport bool

	// EnableParallelStatelessImportWorkers sets the number of workers (CPUs) used for parallel stateless import.
	// If 0, defaults to GOMAXPROCS.
	EnableParallelStatelessImportWorkers int

	// WitnessAPIEnabled enables witness API endpoints
	WitnessAPIEnabled bool

	// WitnessFileStore enables storing witness blobs on the filesystem
	// instead of in the key-value database. Reduces DB write amplification.
	WitnessFileStore bool

	// DisableBlindForkValidation disables additional fork validation and accept blind forks without tracing back to last whitelisted entry
	DisableBlindForkValidation bool

	// MaxBlindForkValidationLimit denotes the maximum number of blocks to traverse back in the database when validating blind forks
	MaxBlindForkValidationLimit uint64

	// EIP-7966: eth_sendRawTransactionSync timeouts
	TxSyncDefaultTimeout time.Duration `toml:",omitempty"`
	TxSyncMaxTimeout     time.Duration `toml:",omitempty"`

	// Preconf / Private transaction relay related settings
	EnablePreconfs            bool
	EnablePrivateTx           bool
	BlockProducerRpcEndpoints []string

	// Preconf / Private transaction related settings for block producers
	AcceptPreconfTx bool
	AcceptPrivateTx bool
}

// CreateConsensusEngine creates a consensus engine for the given chain configuration.
func CreateConsensusEngine(chainConfig *params.ChainConfig, ethConfig *Config, db ethdb.Database, blockchainAPI *ethapi.BlockChainAPI, vmConfig vm.Config) (consensus.Engine, error) {
	// nolint:nestif
	if chainConfig.Clique != nil {
		return beacon.New(clique.New(chainConfig.Clique, db)), nil
	} else if chainConfig.Bor != nil && chainConfig.Bor.ValidatorContract != "" {
		// If Matic bor consensus is requested, set it up
		// In order to pass the ethereum transaction tests, we need to set the burn contract which is in the bor config
		// Then, bor != nil will also be enabled for ethash and clique. Only enable Bor for real if there is a validator contract present.
		genesisContractsClient := contract.NewGenesisContractsClient(chainConfig, chainConfig.Bor.ValidatorContract, chainConfig.Bor.StateReceiverContract, blockchainAPI)
		spanner := span.NewChainSpanner(blockchainAPI, borabi.ValidatorSet(), chainConfig, common.HexToAddress(chainConfig.Bor.ValidatorContract))

		log.Info("Creating consensus engine", "withoutHeimdall", ethConfig.WithoutHeimdall)
		log.Info("Using custom miner block time", "blockTime", ethConfig.Miner.BlockTime)

		if ethConfig.WithoutHeimdall {
			return bor.New(chainConfig, db, blockchainAPI, spanner, nil, nil, genesisContractsClient, ethConfig.DevFakeAuthor, ethConfig.Miner.BlockTime, vmConfig), nil
		} else {
			if ethConfig.DevFakeAuthor {
				log.Warn("Sanitizing DevFakeAuthor", "Use DevFakeAuthor with", "--bor.withoutheimdall")
			}

			var heimdallClient bor.IHeimdallClient
			// Use override client if provided (for testing)
			if ethConfig.OverrideHeimdallClient != nil {
				heimdallClient = ethConfig.OverrideHeimdallClient
			} else if ethConfig.RunHeimdall && ethConfig.UseHeimdallApp {
				// TODO: Running heimdall from bor is not tested yet.
				// heimdallClient = heimdallapp.NewHeimdallAppClient()
				panic("Running heimdall from bor is not implemented yet. Please use heimdall gRPC or HTTP client instead.")
			} else {
				httpURLs := parseURLs(ethConfig.HeimdallURL)
				grpcAddrs := parseURLs(ethConfig.HeimdallgRPCAddress)

				// Build one client per endpoint.
				// gRPC takes priority where configured; falls back to HTTP.
				var heimdallClients []heimdall.Endpoint

				n := max(len(httpURLs), len(grpcAddrs))
				for i := 0; i < n; i++ {
					if i < len(grpcAddrs) && grpcAddrs[i] != "" {
						var httpURL string
						if len(httpURLs) > 0 {
							httpURL = httpURLs[min(i, len(httpURLs)-1)]
						}

						grpcClient, err := heimdallgrpc.NewHeimdallGRPCClient(grpcAddrs[i], httpURL, ethConfig.HeimdallTimeout)
						if err != nil {
							log.Error("Failed to initialize Heimdall gRPC client; falling back to HTTP",
								"index", i, "grpc", grpcAddrs[i], "err", err)

							if i < len(httpURLs) {
								httpClient, httpErr := heimdall.NewHeimdallClient(httpURLs[i], ethConfig.HeimdallTimeout)
								if httpErr != nil {
									return nil, fmt.Errorf("failed to initialize Heimdall HTTP client for %q: %w", httpURLs[i], httpErr)
								}
								heimdallClients = append(heimdallClients, httpClient)
							}

							continue
						}

						heimdallClients = append(heimdallClients, grpcClient)
					} else if i < len(httpURLs) {
						httpClient, err := heimdall.NewHeimdallClient(httpURLs[i], ethConfig.HeimdallTimeout)
						if err != nil {
							return nil, fmt.Errorf("failed to initialize Heimdall HTTP client for %q: %w", httpURLs[i], err)
						}
						heimdallClients = append(heimdallClients, httpClient)
					}
				}

				if len(heimdallClients) == 0 {
					httpClient, err := heimdall.NewHeimdallClient(ethConfig.HeimdallURL, ethConfig.HeimdallTimeout)
					if err != nil {
						return nil, fmt.Errorf("failed to initialize Heimdall HTTP client for %q: %w", ethConfig.HeimdallURL, err)
					}
					heimdallClient = httpClient
				} else if len(heimdallClients) == 1 {
					heimdallClient = heimdallClients[0]
				} else {
					multiClient, err := heimdall.NewMultiHeimdallClient(heimdallClients...)
					if err != nil {
						return nil, fmt.Errorf("failed to create heimdall failover client: %w", err)
					}

					heimdallClient = multiClient
					log.Info("Heimdall failover enabled with multiple endpoints", "endpoints", len(heimdallClients))
				}
			}

			// WS client
			wsAddrs := parseURLs(ethConfig.HeimdallWSAddress)

			var heimdallWSClient bor.IHeimdallWSClient
			var err error

			if len(wsAddrs) > 0 {
				heimdallWSClient, err = heimdallws.NewHeimdallWSClient(wsAddrs...)
				if err != nil {
					return nil, err
				}

				if len(wsAddrs) > 1 {
					log.Info("Heimdall WS failover enabled with multiple endpoints", "endpoints", len(wsAddrs))
				}
			}

			return bor.New(chainConfig, db, blockchainAPI, spanner, heimdallClient, heimdallWSClient, genesisContractsClient, false, ethConfig.Miner.BlockTime, vmConfig), nil
		}
	}
	return beacon.New(ethash.NewFaker()), nil
}
