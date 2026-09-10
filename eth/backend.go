// Copyright 2014 The go-ethereum Authors
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

// Package eth implements the Ethereum protocol.
package eth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/filtermaps"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/pruner"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/blobpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/txpool/locals"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/eth/relay"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/shutdowncheck"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/dnsdisc"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	gethversion "github.com/ethereum/go-ethereum/version"
)

var (
	MilestoneWhitelistedDelayTimer = metrics.NewRegisteredTimer("chain/milestone/whitelisteddelay", nil)
	pendingPipelinedMaskedCounter  = metrics.NewRegisteredCounter("chain/sync/pending_pipelined_masked_total", nil)
)

func hasPendingPipelinedHeadState(chain *core.BlockChain, head *types.Header) bool {
	if !chain.HasRecentPipelinedHeadState(head.Hash(), head.Root) {
		return false
	}
	pendingPipelinedMaskedCounter.Inc(1)
	return true
}

const (
	// This is the fairness knob for the discovery mixer. When looking for peers, we'll
	// wait this long for a single source of candidates before moving on and trying other
	// sources. If this timeout expires, the source will be skipped in this round, but it
	// will continue to fetch in the background and will have a chance with a new timeout
	// in the next rounds, giving it overall more time but a proportionally smaller share.
	// We expect a normal source to produce ~10 candidates per second.
	discmixTimeout = 100 * time.Millisecond

	// discoveryPrefetchBuffer is the number of peers to pre-fetch from a discovery
	// source. It is useful to avoid the negative effects of potential longer timeouts
	// in the discovery, keeping dial progress while waiting for the next batch of
	// candidates.
	discoveryPrefetchBuffer = 32

	// maxParallelENRRequests is the maximum number of parallel ENR requests that can be
	// performed by a disc/v4 source.
	maxParallelENRRequests = 16
)

// Config contains the configuration options of the ETH protocol.
// Deprecated: use ethconfig.Config instead.
type Config = ethconfig.Config

// Ethereum implements the Ethereum full node service.
type Ethereum struct {
	// core protocol objects
	config         *ethconfig.Config
	txPool         *txpool.TxPool
	blobTxPool     *blobpool.BlobPool
	localTxTracker *locals.TxTracker
	blockchain     *core.BlockChain

	handler *handler
	discmix *enode.FairMix
	dropper *dropper

	// DB interfaces
	chainDb ethdb.Database // Block chain database

	eventMux       *event.TypeMux
	engine         consensus.Engine
	accountManager *accounts.Manager
	authorized     bool // If consensus engine is authorized with keystore

	filterMaps      *filtermaps.FilterMaps
	closeFilterMaps chan chan struct{}

	APIBackend *EthAPIBackend

	miner     *miner.Miner
	gasPrice  *big.Int
	etherbase common.Address

	networkID     uint64
	netRPCService *ethapi.NetAPI

	p2pServer *p2p.Server

	lock sync.RWMutex // Protects the variadic fields (e.g. gas price and etherbase)

	closeCh chan struct{} // Channel to signal the background processes to exit

	shutdownTracker *shutdowncheck.ShutdownTracker // Tracks if and when the node has shutdown ungracefully
}

// New creates a new Ethereum object (including the initialisation of the common Ethereum object),
// whose lifecycle will be managed by the provided node.
func New(stack *node.Node, config *ethconfig.Config) (*Ethereum, error) {
	// Ensure configuration values are compatible and sane
	if !config.SyncMode.IsValid() {
		return nil, fmt.Errorf("invalid sync mode %d", config.SyncMode)
	}
	if !config.HistoryMode.IsValid() {
		return nil, fmt.Errorf("invalid history mode %d", config.HistoryMode)
	}

	if config.Miner.GasPrice == nil {
		log.Info("Miner gas price not set, using default", "value", params.BorDefaultMinerGasPrice)
		config.Miner.GasPrice = big.NewInt(params.BorDefaultMinerGasPrice)
	}
	// PIP-35: Enforce min gas price to 25 gwei only if gas tip override is not allowed
	if !config.Miner.AllowGasTipOverride && config.Miner.GasPrice.Cmp(big.NewInt(params.BorDefaultMinerGasPrice)) != 0 {
		log.Warn("Sanitizing invalid miner gas price", "provided", config.Miner.GasPrice, "updated", ethconfig.Defaults.Miner.GasPrice)
		config.Miner.GasPrice = ethconfig.Defaults.Miner.GasPrice
	}
	if config.Miner.AllowGasTipOverride {
		log.Info("Setting miner gas price", "value", config.Miner.GasPrice)
	}

	if config.NoPruning && config.TrieDirtyCache > 0 && config.StateScheme == rawdb.HashScheme {
		if config.SnapshotCache > 0 {
			config.TrieCleanCache += config.TrieDirtyCache * 3 / 5
			config.SnapshotCache += config.TrieDirtyCache * 2 / 5
		} else {
			config.TrieCleanCache += config.TrieDirtyCache
		}
		config.TrieDirtyCache = 0
	}
	log.Info("Allocated trie memory caches", "clean", common.StorageSize(config.TrieCleanCache)*1024*1024, "dirty", common.StorageSize(config.TrieDirtyCache)*1024*1024)

	witnessPruneEnabled := false
	if config.SyncMode == downloader.StatelessSync || config.WitnessProtocol {
		witnessPruneEnabled = true
	}

	blockPruneEnabled := false
	if config.SyncMode == downloader.StatelessSync {
		blockPruneEnabled = true
	}

	dbOptions := node.DatabaseOptions{
		Cache:               config.DatabaseCache,
		Handles:             config.DatabaseHandles,
		AncientsDirectory:   config.DatabaseFreezer,
		EraDirectory:        config.DatabaseEra,
		MetricsNamespace:    "eth/db/chaindata/",
		WitnessPruneEnabled: witnessPruneEnabled,
		BlockPruneEnabled:   blockPruneEnabled,
		Stateless:           config.SyncMode == downloader.StatelessSync,
		WitnessFileStore:    config.WitnessFileStore,
	}
	chainDb, err := stack.OpenDatabaseWithOptions("chaindata", dbOptions)
	if err != nil {
		return nil, err
	}

	scheme, err := rawdb.ParseStateScheme(config.StateScheme, chainDb)
	if err != nil {
		return nil, err
	}
	// Try to recover offline state pruning only in hash-based.
	if scheme == rawdb.HashScheme {
		if err := pruner.RecoverPruning(stack.ResolvePath(""), chainDb); err != nil {
			log.Error("Failed to recover state", "error", err)
		}
	}

	// Assemble the Ethereum object.
	eth := &Ethereum{
		config:          config,
		chainDb:         chainDb,
		eventMux:        stack.EventMux(),
		accountManager:  stack.AccountManager(),
		authorized:      false,
		networkID:       config.NetworkId,
		gasPrice:        config.Miner.GasPrice,
		etherbase:       config.Miner.Etherbase,
		p2pServer:       stack.Server(),
		discmix:         enode.NewFairMix(discmixTimeout),
		shutdownTracker: shutdowncheck.NewShutdownTracker(chainDb),
		closeCh:         make(chan struct{}),
	}

	relayService := relay.Init(config.EnablePreconfs, config.EnablePrivateTx, config.AcceptPreconfTx, config.AcceptPrivateTx, config.BlockProducerRpcEndpoints)
	privateTxGetter := relayService.GetPrivateTxGetter()

	// START: Bor changes
	eth.APIBackend = &EthAPIBackend{stack.Config().ExtRPCEnabled(), stack.Config().AllowUnprotectedTxs, eth, nil, relayService}
	if eth.APIBackend.allowUnprotectedTxs {
		log.Info("Unprotected transactions allowed")
		config.TxPool.AllowUnprotectedTxs = true
	}

	// Set transaction getter for relay service to query local database
	relayService.SetTxGetter(eth.APIBackend.GetCanonicalTransaction)

	blockChainAPI := ethapi.NewBlockChainAPI(eth.APIBackend)

	// Prepare the vm config for tracing
	vmCfg := vm.Config{
		EnablePreimageRecording: config.EnablePreimageRecording,
		StatelessSelfValidation: config.StatelessSelfValidation,
		EnableWitnessStats:      config.EnableWitnessStats,
		EnableEVMSwitchDispatch: config.EnableEVMSwitchDispatch,
	}

	// Setup live tracer if requested
	if config.VMTrace != "" && config.ParallelEVM.Enable {
		log.Warn("Live tracing requested but not supported with ParallelEVM enabled. Disable ParallelEVM via `--parallelevm.enable=false` to use live tracing.")
	} else if config.VMTrace != "" {
		traceConfig := json.RawMessage("{}")
		if config.VMTraceJsonConfig != "" {
			traceConfig = json.RawMessage(config.VMTraceJsonConfig)
		}
		t, err := tracers.LiveDirectory.New(config.VMTrace, traceConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create tracer %s: %v", config.VMTrace, err)
		}
		// For tracing state-sync transactions, we need a modified tracer. We wrap the hooks
		// of the live tracer above with a state-sync aware tracer which is used across multiple
		// modules.
		if borCfg := config.Genesis.Config.Bor; borCfg != nil {
			stateReceiver := common.HexToAddress(borCfg.StateReceiverContract)
			t = tracers.WrapStateSyncHooks(t, stateReceiver)
		}
		vmCfg.Tracer = t
	}

	engine, err := ethconfig.CreateConsensusEngine(config.Genesis.Config, config, chainDb, blockChainAPI, vmCfg)
	eth.engine = engine
	if err != nil {
		return nil, err
	}
	// END: Bor changes

	bcVersion := rawdb.ReadDatabaseVersion(chainDb)
	dbVer := "<nil>"
	if bcVersion != nil {
		dbVer = fmt.Sprintf("%d", *bcVersion)
	}
	log.Info("Initialising Ethereum protocol", "network", config.NetworkId, "dbversion", dbVer)

	// Create BlockChain object.
	if !config.SkipBcVersionCheck {
		if bcVersion != nil && *bcVersion > core.BlockChainVersion {
			return nil, fmt.Errorf("database version is v%d, Geth %s only supports v%d", *bcVersion, version.WithMeta, core.BlockChainVersion)
		} else if bcVersion == nil || *bcVersion < core.BlockChainVersion {
			if bcVersion != nil { // only print warning on upgrade, not on init
				log.Warn("Upgrade blockchain database version", "from", dbVer, "to", core.BlockChainVersion)
			}
			rawdb.WriteDatabaseVersion(chainDb, core.BlockChainVersion)
		}
	}
	trieJournalDirectory := config.TrieJournalDirectory
	if trieJournalDirectory == "" {
		trieJournalDirectory = stack.ResolvePath("triedb")
	}

	var (
		options = &core.BlockChainConfig{
			TrieCleanLimit:           config.TrieCleanCache,
			NoPrefetch:               config.NoPrefetch,
			TrieDirtyLimit:           config.TrieDirtyCache,
			ArchiveMode:              config.NoPruning,
			TrieTimeLimit:            config.TrieTimeout,
			SnapshotLimit:            config.SnapshotCache,
			Preimages:                config.Preimages,
			StateHistory:             config.StateHistory,
			TrienodeHistory:          config.TrienodeHistory,
			NodeFullValueCheckpoint:  config.NodeFullValueCheckpoint,
			StateScheme:              scheme,
			TriesInMemory:            config.TriesInMemory,
			ChainHistoryMode:         config.HistoryMode,
			TxLookupLimit:            int64(min(config.TransactionHistory, math.MaxInt64)),
			AddressCacheSizes:        config.AddressCacheSizes,
			PreloadRateLimit:         config.PreloadRateLimit,
			VmConfig:                 vmCfg,
			EnablePipelinedImportSRC: config.EnablePipelinedImportSRC,
			PipelinedImportSRCLogs:   config.PipelinedImportSRCLogs,
			PipelinedSRCWarmSnapshot: config.PipelinedSRCWarmSnapshot,
			Stateless:                config.SyncMode == downloader.StatelessSync,
			// Enables file journaling for the trie database. The journal files will be stored
			// within the data directory. The corresponding paths will be either:
			// - DATADIR/triedb/merkle.journal
			// - DATADIR/triedb/verkle.journal
			TrieJournalDirectory: trieJournalDirectory,
			StateSizeTracking:    config.EnableStateSizeTracking,
		}
	)

	checker := whitelist.NewService(chainDb, config.DisableBlindForkValidation, config.MaxBlindForkValidationLimit)

	// Override the chain config with provided settings.
	var overrides core.ChainOverrides
	if config.OverrideOsaka != nil {
		overrides.OverrideOsaka = config.OverrideOsaka
	}
	if config.OverrideVerkle != nil {
		overrides.OverrideVerkle = config.OverrideVerkle
	}

	options.Overrides = &overrides
	options.Checker = checker

	// Wire MilestoneFetcher so verifyPendingHeaders queries Heimdall directly.
	if borEngine, ok := eth.engine.(*bor.Bor); ok && borEngine.HeimdallClient != nil {
		options.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			m, err := borEngine.HeimdallClient.FetchMilestone(ctx)
			if err != nil {
				return 0, err
			}
			return m.EndBlock, nil
		}
	}

	// check if Parallel EVM is enabled
	// if enabled, use parallel state processor
	if config.ParallelEVM.Enable {
		eth.blockchain, err = core.NewParallelBlockChain(chainDb, config.Genesis, eth.engine, options, config.ParallelEVM.SpeculativeProcesses, config.ParallelEVM.Enforce)
	} else {
		eth.blockchain, err = core.NewBlockChain(chainDb, config.Genesis, eth.engine, options)
	}

	// Set the chain head event subscription function for private tx store
	relayService.SetchainEventSubFn(eth.blockchain.SubscribeChainEvent)

	// Set parallel stateless import toggle on blockchain
	if err == nil && eth.blockchain != nil && config.EnableParallelStatelessImport {
		eth.blockchain.ParallelStatelessImportEnable()
		if config.EnableParallelStatelessImportWorkers > 0 {
			eth.blockchain.SetParallelStatelessImportWorkers(config.EnableParallelStatelessImportWorkers)
		}
	}

	if err != nil {
		return nil, err
	}

	// Set blockchain reference for fork detection in whitelist service
	checker.SetBlockchain(eth.blockchain)

	// Initialize filtermaps log index.
	fmConfig := filtermaps.Config{
		History:        config.LogHistory,
		Disabled:       config.LogNoHistory,
		ExportFileName: config.LogExportCheckpoints,
		HashScheme:     scheme == rawdb.HashScheme,
	}
	chainView := eth.newChainView(eth.blockchain.CurrentBlock())
	historyCutoff, _ := eth.blockchain.HistoryPruningCutoff()
	var finalBlock uint64
	if fb := eth.blockchain.CurrentFinalBlock(); fb != nil {
		finalBlock = fb.Number.Uint64()
	}
	filterMaps, err := filtermaps.NewFilterMaps(chainDb, chainView, historyCutoff, finalBlock, filtermaps.DefaultParams, fmConfig)
	if err != nil {
		return nil, err
	}
	eth.filterMaps = filterMaps
	eth.closeFilterMaps = make(chan chan struct{})

	if config.BlobPool.Datadir != "" {
		config.BlobPool.Datadir = stack.ResolvePath(config.BlobPool.Datadir)
	}

	// TxPool
	if config.TxPool.Journal != "" {
		config.TxPool.Journal = stack.ResolvePath(config.TxPool.Journal)
	}
	legacyPool := legacypool.New(config.TxPool, eth.blockchain)

	// BOR changes
	// Blob pool is removed from Subpool for Bor
	if eth.config.SyncMode != downloader.StatelessSync {
		eth.txPool, err = txpool.New(config.TxPool.PriceLimit, eth.blockchain, []txpool.SubPool{legacyPool})
		if err != nil {
			return nil, err
		}
		// The `config.TxPool.PriceLimit` used above doesn't reflect the sanitized/enforced changes
		// made in the txpool. Update the `gasTip` explicitly to reflect the enforced value.
		eth.txPool.SetGasTip(new(big.Int).SetUint64(params.BorDefaultTxPoolPriceLimit))

		// Allow private tx store to check if txs are still in the pool for cleanup
		relayService.SetTxPoolChecker(eth.txPool.Has)
	}

	if !config.TxPool.NoLocals {
		rejournal := config.TxPool.Rejournal
		if rejournal < time.Second {
			log.Warn("Sanitizing invalid txpool journal time", "provided", rejournal, "updated", time.Second)
			rejournal = time.Second
		}
		eth.localTxTracker = locals.New(config.TxPool.Journal, rejournal, eth.blockchain.Config(), eth.txPool)
		stack.RegisterLifecycle(eth.localTxTracker)
	}

	// Permit the downloader to use the trie cache allowance during fast sync
	cacheLimit := options.TrieCleanLimit + options.TrieDirtyLimit + options.SnapshotLimit
	if eth.handler, err = newHandler(&handlerConfig{
		NodeID:                  eth.p2pServer.Self().ID(),
		Database:                chainDb,
		Chain:                   eth.blockchain,
		TxPool:                  eth.txPool,
		Network:                 config.NetworkId,
		Sync:                    config.SyncMode,
		BloomCache:              uint64(cacheLimit),
		EventMux:                eth.eventMux,
		RequiredBlocks:          config.RequiredBlocks,
		EthAPI:                  blockChainAPI,
		gasCeil:                 config.Miner.GasCeil,
		checker:                 checker,
		enableBlockTracking:     eth.config.EnableBlockTracking,
		txAnnouncementOnly:      eth.p2pServer.TxAnnouncementOnly,
		disableTxPropagation:    eth.p2pServer.DisableTxPropagation,
		witnessProtocol:         eth.config.WitnessProtocol,
		syncWithWitnesses:       eth.config.SyncWithWitnesses,
		syncAndProduceWitnesses: eth.config.SyncAndProduceWitnesses,
		fastForwardThreshold:    config.FastForwardThreshold,
		p2pServer:               eth.p2pServer,
		privateTxGetter:         privateTxGetter,
	}); err != nil {
		return nil, err
	}

	eth.dropper = newDropper(eth.p2pServer.MaxDialedConns(), eth.p2pServer.MaxInboundConns())

	if config.SyncMode != downloader.StatelessSync {
		eth.miner = miner.New(eth, &config.Miner, eth.blockchain.Config(), eth.eventMux, eth.engine, eth.isLocalBlock, eth.config.WitnessProtocol)
		eth.miner.SetExtra(makeExtraData(config.Miner.ExtraData))
		eth.miner.SetPrioAddresses(config.TxPool.Locals)
	}

	// 1.14.8: NewOracle function definition was changed to accept (startPrice *big.Int) param.
	eth.APIBackend.gpo = gasprice.NewOracle(eth.APIBackend, config.GPO, config.Miner.GasPrice)
	eth.APIBackend.gpo.ProcessCache()

	// Start the RPC service
	eth.netRPCService = ethapi.NewNetAPI(eth.p2pServer, config.NetworkId)

	// Register the backend on the node
	stack.RegisterAPIs(eth.APIs())
	stack.RegisterProtocols(eth.Protocols())
	stack.RegisterLifecycle(eth)

	// Successful startup; push a marker and check previous unclean shutdowns.
	eth.shutdownTracker.MarkStartup()

	return eth, nil
}

func makeExtraData(extra []byte) []byte {
	if len(extra) == 0 {
		// create default extradata
		extra, _ = rlp.EncodeToBytes([]interface{}{
			uint(gethversion.Major<<16 | gethversion.Minor<<8 | gethversion.Patch),
			"bor",
			runtime.Version(),
			runtime.GOOS,
		})
	}

	if uint64(len(extra)) > params.MaximumExtraDataSize {
		log.Warn("Miner extra data exceed limit", "extra", hexutil.Bytes(extra), "limit", params.MaximumExtraDataSize)
		extra = nil
	}

	return extra
}

// PeerCount returns the number of connected peers.
func (s *Ethereum) PeerCount() int {
	return s.p2pServer.PeerCount()
}

// APIs return the collection of RPC services the ethereum package offers.
// NOTE, some of these services probably need to be moved to somewhere else.
func (s *Ethereum) APIs() []rpc.API {
	apis := ethapi.GetAPIs(s.APIBackend)

	// Append any APIs exposed explicitly by the consensus engine
	apis = append(apis, s.engine.APIs(s.BlockChain())...)

	// BOR change starts
	filterSystem := filters.NewFilterSystem(s.APIBackend, filters.Config{
		LogCacheSize:  s.config.FilterLogCacheSize,
		LogQueryLimit: s.config.LogQueryLimit,
		RangeLimit:    s.config.RPCBlockRangeLimit,
	})
	// set genesis to public filter api
	publicFilterAPI := filters.NewFilterAPI(filterSystem, s.config.BorLogs)
	// avoiding constructor changed by introducing new method to set genesis
	publicFilterAPI.SetChainConfig(s.blockchain.Config())
	// BOR change ends

	// Append all the local APIs and return
	return append(apis, []rpc.API{
		{
			Namespace: "miner",
			Service:   NewMinerAPI(s),
		}, {
			Namespace: "eth",
			Service:   publicFilterAPI, // BOR related change
		}, {
			Namespace: "admin",
			Service:   NewAdminAPI(s),
		}, {
			Namespace: "debug",
			Service:   NewDebugAPI(s),
		}, {
			Namespace: "net",
			Service:   s.netRPCService,
		},
	}...)
}

func (s *Ethereum) ResetWithGenesisBlock(gb *types.Block) {
	s.blockchain.ResetWithGenesisBlock(gb)
}

func (s *Ethereum) PublicBlockChainAPI() *ethapi.BlockChainAPI {
	return s.handler.ethAPI
}

func (s *Ethereum) Etherbase() (eb common.Address, err error) {
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if etherbase != (common.Address{}) {
		return etherbase, nil
	}
	return common.Address{}, errors.New("etherbase must be explicitly specified")
}

// isLocalBlock checks whether the specified block is mined
// by local miner accounts.
//
// We regard two types of accounts as local miner account: etherbase
// and accounts specified via `txpool.locals` flag.
func (s *Ethereum) isLocalBlock(header *types.Header) bool {
	author, err := s.engine.Author(header)
	if err != nil {
		log.Warn("Failed to retrieve block author", "number", header.Number.Uint64(), "hash", header.Hash(), "err", err)
		return false
	}
	// Check whether the given address is etherbase.
	s.lock.RLock()
	etherbase := s.etherbase
	s.lock.RUnlock()

	if author == etherbase {
		return true
	}
	// Check whether the given address is specified by `txpool.local`
	// CLI flag.
	for _, account := range s.config.TxPool.Locals {
		if account == author {
			return true
		}
	}

	return false
}

// SetEtherbase sets the mining reward address.
func (s *Ethereum) SetEtherbase(etherbase common.Address) {
	s.lock.Lock()
	s.etherbase = etherbase
	s.lock.Unlock()

	s.miner.SetEtherbase(etherbase)
}

// StartMining starts the miner with the given number of CPU threads. If mining
// is already running, this method adjust the number of threads allowed to use
// and updates the minimum price required by the transaction pool.
func (s *Ethereum) StartMining() error {
	// If the miner was not running, initialize it
	if !s.IsMining() {
		// Propagate the initial price point to the transaction pool
		s.lock.RLock()
		price := s.gasPrice
		s.lock.RUnlock()
		s.txPool.SetGasTip(price)

		// Configure the local mining address
		eb, err := s.Etherbase()
		if err != nil {
			log.Error("Cannot start mining without etherbase", "err", err)
			return fmt.Errorf("etherbase missing: %v", err)
		}
		// If personal endpoints are disabled, the server creating
		// this Ethereum instance has already Authorized consensus.
		if !s.authorized {
			var cli *clique.Clique
			if c, ok := s.engine.(*clique.Clique); ok {
				cli = c
			} else if cl, ok := s.engine.(*beacon.Beacon); ok {
				if c, ok := cl.InnerEngine().(*clique.Clique); ok {
					cli = c
				}
			}

			if cli != nil {
				wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
				if wallet == nil || err != nil {
					log.Error("Etherbase account unavailable locally", "err", err)
					return fmt.Errorf("signer missing: %v", err)
				}

				cli.Authorize(eb, wallet.SignData)
			}

			if bor, ok := s.engine.(*bor.Bor); ok {
				wallet, err := s.accountManager.Find(accounts.Account{Address: eb})
				if wallet == nil || err != nil {
					log.Error("Etherbase account unavailable locally", "err", err)

					return fmt.Errorf("signer missing: %v", err)
				}

				bor.Authorize(eb, wallet.SignData)
			}
		}

		// If mining is started, we can disable the transaction rejection mechanism
		// introduced to speed sync times.
		s.handler.enableSyncedFeatures()

		go s.miner.Start()
	}

	return nil
}

// StopMining terminates the miner, both at the consensus engine level as well as
// at the block creation level.
func (s *Ethereum) StopMining() {
	// Update the thread count within the consensus engine
	type threaded interface {
		SetThreads(threads int)
	}

	if th, ok := s.engine.(threaded); ok {
		th.SetThreads(-1)
	}
	// Stop the block creating itself
	ch := make(chan struct{})
	s.miner.Stop(ch)
}

func (s *Ethereum) IsMining() bool      { return s.miner.Mining() }
func (s *Ethereum) Miner() *miner.Miner { return s.miner }

func (s *Ethereum) AccountManager() *accounts.Manager  { return s.accountManager }
func (s *Ethereum) BlockChain() *core.BlockChain       { return s.blockchain }
func (s *Ethereum) TxPool() *txpool.TxPool             { return s.txPool }
func (s *Ethereum) BlobTxPool() *blobpool.BlobPool     { return s.blobTxPool }
func (s *Ethereum) Engine() consensus.Engine           { return s.engine }
func (s *Ethereum) ChainDb() ethdb.Database            { return s.chainDb }
func (s *Ethereum) IsListening() bool                  { return true } // Always listening
func (s *Ethereum) Downloader() *downloader.Downloader { return s.handler.downloader }
func (s *Ethereum) Synced() bool                       { return s.handler.synced.Load() }
func (s *Ethereum) SetSynced()                         { s.handler.enableSyncedFeatures() }
func (s *Ethereum) ArchiveMode() bool                  { return s.config.NoPruning }

// SetAuthorized sets the authorized bool variable
// denoting that consensus has been authorized while creation
func (s *Ethereum) SetAuthorized(authorized bool) {
	s.lock.Lock()
	s.authorized = authorized
	s.lock.Unlock()
}

// Protocols returns all the currently configured
// network protocols to start.
func (s *Ethereum) Protocols() []p2p.Protocol {
	protos := eth.MakeProtocols((*ethHandler)(s.handler), s.networkID, s.discmix)
	// snap/1 is only registered when the snapshot cache is enabled and NoSnapServing is false.
	// Setting NoSnapServing=true disables snap/1 entirely: the node will neither serve snap sync
	// requests nor be able to sync state via snap/1 itself. The in-memory snapshot tree (used for
	// fast local state reads) remains active regardless. Nodes that need to snap sync from peers
	// must leave NoSnapServing=false (the default).
	if s.config.SnapshotCache > 0 && !s.config.NoSnapServing {
		protos = append(protos, snap.MakeProtocols((*snapHandler)(s.handler))...)
	}
	if s.config.WitnessProtocol {
		protos = append(protos, wit.MakeProtocols((*witHandler)(s.handler), s.networkID)...)
	}

	return protos
}

// Start implements node.Lifecycle, starting all internal goroutines needed by the
// Ethereum protocol implementation.
func (s *Ethereum) Start() error {
	if err := s.setupDiscovery(); err != nil {
		return err
	}

	// Regularly update shutdown marker
	s.shutdownTracker.Start()

	// Start the networking layer
	s.handler.Start(s.p2pServer.MaxPeers)

	// Start the connection manager
	s.dropper.Start(s.p2pServer, func() bool { return !s.Synced() })

	go s.startCheckpointWhitelistService()
	go s.startMilestoneWhitelistService()

	// start log indexer
	s.filterMaps.Start()
	go s.updateFilterMapsHeads()

	return nil
}

var (
	ErrNotBorConsensus             = errors.New("not bor consensus was given")
	ErrBorConsensusWithoutHeimdall = errors.New("bor consensus without heimdall")
)

const (
	whitelistTimeout = 30 * time.Second
)

// StartCheckpointWhitelistService starts the goroutine to fetch checkpoints and update the
// checkpoint whitelist map.
func (s *Ethereum) startCheckpointWhitelistService() {
	const (
		tickerDuration = 100 * time.Second
		fnName         = "whitelist checkpoint"
	)

	s.retryHeimdallHandler(s.fetchAndHandleWhitelistCheckpoint, tickerDuration, whitelistTimeout)
}

// startMilestoneWhitelistService starts the goroutine that updates the milestone whitelist map.
// It subscribes to milestone events from heimdall via websocket when available, and falls back
// to polling otherwise.
func (s *Ethereum) startMilestoneWhitelistService() {
	ethHandler, bor, _ := s.getHandler()

	const (
		tickerDuration = 2 * time.Second
	)

	// If heimdall WS is available, use WS subscription for new milestone events instead of polling
	if bor != nil && bor.HeimdallWSClient != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-s.closeCh
			cancel()
		}()

		for {
			if err := s.subscribeAndHandleMilestone(ctx, ethHandler, bor); err != nil {
				log.Error("Error subscribing to milestone events", "err", err)
			}

			// If we fail to subscribe, retry after a delay
			select {
			case <-s.closeCh:
				return
			case <-time.After(tickerDuration):
				// Continue to retry subscribing to milestone events
			}
		}
	}

	s.retryHeimdallHandler(s.fetchAndHandleMilestone, tickerDuration, whitelistTimeout)
}

func (s *Ethereum) retryHeimdallHandler(fn heimdallHandler, tickerDuration time.Duration, timeout time.Duration) {
	retryHeimdallHandler(fn, tickerDuration, timeout, s.closeCh, s.getHandler)
}

func retryHeimdallHandler(fn heimdallHandler, tickerDuration time.Duration, timeout time.Duration, closeCh chan struct{}, getHandler func() (*ethHandler, *bor.Bor, error)) {
	// a shortcut helps with tests and early exit
	select {
	case <-closeCh:
		return
	default:
	}

	ethHandler, bor, err := getHandler()
	if err != nil {
		log.Error("error while getting the ethHandler", "err", err)
		return
	}

	// first run
	firstCtx, cancel := context.WithTimeout(context.Background(), timeout)
	_ = fn(firstCtx, ethHandler, bor)

	cancel()

	ticker := time.NewTicker(tickerDuration)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), timeout)

			// Skip any error reporting here as it's handled in respective functions
			_ = fn(ctx, ethHandler, bor)

			cancel()
		case <-closeCh:
			return
		}
	}
}

// fetchAndHandleWhitelistCheckpoint handles the checkpoint whitelist mechanism.
func (s *Ethereum) fetchAndHandleWhitelistCheckpoint(ctx context.Context, ethHandler *ethHandler, bor *bor.Bor) error {
	// Create a new bor verifier, which will be used to verify checkpoints and milestones
	verifier := newBorVerifier()

	checkpoint, err := ethHandler.fetchWhitelistCheckpoint(ctx, bor)
	// If the array is empty, we're bound to receive an error. Non-nill error and non-empty array
	// means that array has partial elements and it failed for some block. We'll add those partial
	// elements anyway.
	if err != nil {
		return err
	}

	_, err = ethHandler.handleWhitelistCheckpoint(ctx, checkpoint, s, verifier, true)
	return err
}

type heimdallHandler func(ctx context.Context, ethHandler *ethHandler, bor *bor.Bor) error

// fetchAndHandleMilestone handles the milestone mechanism.
func (s *Ethereum) fetchAndHandleMilestone(ctx context.Context, ethHandler *ethHandler, bor *bor.Bor) error {
	// Create a new bor verifier, which will be used to verify checkpoints and milestones
	verifier := newBorVerifier()
	milestone, err := ethHandler.fetchWhitelistMilestone(ctx, bor)
	if err != nil {
		return err
	}

	return ethHandler.handleMilestone(ctx, s, milestone, verifier)
}

func (s *Ethereum) subscribeAndHandleMilestone(ctx context.Context, ethHandler *ethHandler, bor *bor.Bor) error {
	milestoneEvents := bor.HeimdallWSClient.SubscribeMilestoneEvents(ctx)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var milestone *milestone.Milestone

	for {
		select {
		case m, ok := <-milestoneEvents:
			if !ok {
				return nil
			}

			milestone = m

			err := ethHandler.handleMilestone(ctx, s, m, newBorVerifier())
			if err != nil {
				log.Error("error handling milestone ws event", "err", err)
			}

		// Re-process the milestone periodically in case a fork is imported right after the previous milestone
		case <-ticker.C:
			if milestone == nil {
				continue
			}

			err := ethHandler.handleMilestone(ctx, s, milestone, newBorVerifier())
			if err != nil {
				log.Error("error handling milestone ws event", "err", err)
			}

		case <-ctx.Done():
			return nil
		}
	}
}

func (s *Ethereum) newChainView(head *types.Header) *filtermaps.ChainView {
	if head == nil {
		return nil
	}
	return filtermaps.NewChainView(s.blockchain, head.Number.Uint64(), head.Hash())
}

func (s *Ethereum) updateFilterMapsHeads() {
	headEventCh := make(chan core.ChainEvent, 10)
	blockProcCh := make(chan bool, 10)
	sub := s.blockchain.SubscribeChainEvent(headEventCh)
	sub2 := s.blockchain.SubscribeBlockProcessingEvent(blockProcCh)
	defer func() {
		sub.Unsubscribe()
		sub2.Unsubscribe()
		for {
			select {
			case <-headEventCh:
			case <-blockProcCh:
			default:
				return
			}
		}
	}()

	var head *types.Header
	setHead := func(newHead *types.Header) {
		if newHead == nil {
			return
		}
		if head == nil || newHead.Hash() != head.Hash() {
			head = newHead
			chainView := s.newChainView(head)
			if chainView == nil {
				return
			}
			historyCutoff, _ := s.blockchain.HistoryPruningCutoff()
			var finalBlock uint64
			if fb := s.blockchain.CurrentFinalBlock(); fb != nil {
				finalBlock = fb.Number.Uint64()
			}
			s.filterMaps.SetTarget(chainView, historyCutoff, finalBlock)
		}
	}
	setHead(s.blockchain.CurrentBlock())

	for {
		select {
		case ev := <-headEventCh:
			setHead(ev.Header)
		case blockProc := <-blockProcCh:
			s.filterMaps.SetBlockProcessing(blockProc)
		case <-time.After(time.Second * 10):
			setHead(s.blockchain.CurrentBlock())
		case ch := <-s.closeFilterMaps:
			close(ch)
			return
		}
	}
}

func (s *Ethereum) setupDiscovery() error {
	eth.StartENRUpdater(s.blockchain, s.p2pServer.LocalNode())

	// Add eth nodes from DNS.
	dnsclient := dnsdisc.NewClient(dnsdisc.Config{})
	if len(s.config.EthDiscoveryURLs) > 0 {
		iter, err := dnsclient.NewIterator(s.config.EthDiscoveryURLs...)
		if err != nil {
			return err
		}
		s.discmix.AddSource(iter)
	}

	// Add snap nodes from DNS.
	if len(s.config.SnapDiscoveryURLs) > 0 {
		iter, err := dnsclient.NewIterator(s.config.SnapDiscoveryURLs...)
		if err != nil {
			return err
		}
		s.discmix.AddSource(iter)
	}

	// Add DHT nodes from discv4.
	if s.p2pServer.DiscoveryV4() != nil {
		iter := s.p2pServer.DiscoveryV4().RandomNodes()
		resolverFunc := func(ctx context.Context, enr *enode.Node) *enode.Node {
			// RequestENR does not yet support context. It will simply time out.
			// If the ENR can't be resolved, RequestENR will return nil. We don't
			// care about the specific error here, so we ignore it.
			nn, _ := s.p2pServer.DiscoveryV4().RequestENR(enr)
			return nn
		}
		iter = enode.AsyncFilter(iter, resolverFunc, maxParallelENRRequests)
		iter = enode.Filter(iter, eth.NewNodeFilter(s.blockchain))
		iter = enode.NewBufferIter(iter, discoveryPrefetchBuffer)
		s.discmix.AddSource(iter)
	}

	// Add DHT nodes from discv5.
	if s.p2pServer.DiscoveryV5() != nil {
		filter := eth.NewNodeFilter(s.blockchain)
		iter := enode.Filter(s.p2pServer.DiscoveryV5().RandomNodes(), filter)
		iter = enode.NewBufferIter(iter, discoveryPrefetchBuffer)
		s.discmix.AddSource(iter)
	}

	return nil
}

func (s *Ethereum) getHandler() (*ethHandler, *bor.Bor, error) {
	ethHandler := (*ethHandler)(s.handler)

	bor, ok := ethHandler.chain.Engine().(*bor.Bor)
	if !ok {
		return nil, nil, ErrNotBorConsensus
	}

	if bor.HeimdallClient == nil {
		return nil, nil, ErrBorConsensusWithoutHeimdall
	}

	return ethHandler, bor, nil
}

// Stop implements node.Lifecycle, terminating all internal goroutines used by the
// Ethereum protocol.
func (s *Ethereum) Stop() error {
	// Stop all the peer-related stuff first.
	s.discmix.Close()

	// Close the tx relay service if enabled
	if s.APIBackend.relay != nil {
		s.APIBackend.relay.Close()
	}

	// Close the engine before handler else it may cause a deadlock where
	// the heimdall is unresponsive and the syncing loop keeps waiting
	// for a response and is unable to proceed to exit `Finalize` during
	// block processing.
	s.engine.Close()
	s.dropper.Stop()

	// Stop the dial scheduler to suppress "Looking for peers" during shutdown.
	s.p2pServer.StopDialing()
	s.handler.Stop()

	// Then stop everything else.
	// Close all bg processes
	close(s.closeCh)

	ch := make(chan struct{})
	s.closeFilterMaps <- ch
	<-ch
	s.filterMaps.Stop()
	s.txPool.Close()
	if s.miner != nil {
		s.miner.Close()
	}
	s.blockchain.Stop()
	s.engine.Close()

	// Clean shutdown marker as the last thing before closing db
	s.shutdownTracker.Stop()

	s.chainDb.Close()
	s.eventMux.Stop()

	return nil
}

//
// Bor related methods
//

// SetBlockchain set blockchain while testing
func (s *Ethereum) SetBlockchain(blockchain *core.BlockChain) {
	s.blockchain = blockchain
}

// SyncMode retrieves the current sync mode, either explicitly set, or derived
// from the chain status.
func (s *Ethereum) SyncMode() downloader.SyncMode {
	// If we're in snap sync mode, return that directly
	if s.handler.snapSync.Load() {
		return downloader.SnapSync
	}
	// We are probably in full sync, but we might have rewound to before the
	// snap sync pivot, check if we should re-enable snap sync.
	head := s.blockchain.CurrentBlock()
	if pivot := rawdb.ReadLastPivotNumber(s.chainDb); pivot != nil {
		if head.Number.Uint64() < *pivot {
			return downloader.SnapSync
		}
	}
	// We are in a full sync, but the associated head state is missing. To complete
	// the head state, forcefully rerun the snap sync. Note it doesn't mean the
	// persistent state is corrupted, just mismatch with the head block.
	if !s.blockchain.HasCommittedState(head.Root) && !s.handler.statelessSync.Load() {
		// Pipelined import can briefly expose a head whose SRC commit is still
		// in flight. Report full sync during that bounded handoff only; once it
		// expires, this remains a real missing-state signal.
		if hasPendingPipelinedHeadState(s.blockchain, head) {
			return downloader.FullSync
		}
		log.Info("Reenabled snap sync as chain is stateless")
		return downloader.SnapSync
	}
	// Nope, we're really full syncing
	if s.handler.statelessSync.Load() {
		return downloader.StatelessSync
	} else {
		return downloader.FullSync
	}
}
