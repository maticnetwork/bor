// Copyright 2015 The go-ethereum Authors
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

package miner

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holiman/uint256"
	"go.opentelemetry.io/otel"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/tracing"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// minRecommitInterval is the minimal time interval to recreate the sealing block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	// In PoW chains (like pre-merge Ethereum), this is set to 7 because orphaned blocks
	// can still be included as "uncle blocks" up to 6-7 blocks deep, earning partial rewards.
	// In Bor's PoS consensus, validators take turns producing blocks deterministically,
	// so there are no competing miners and no uncle block concept. Any non-canonical block
	// is immediately stale and can be discarded, hence staleThreshold is set to 0.
	staleThreshold = 0

	// interruptBuffer is the buffer time to give some buffer for state root computation
	interruptBuffer = 100 * time.Millisecond

	// prefetchChanBufSize is the default buffer for the unified prefetcher's tx
	// stream channel. ≈ one full block's worth of 21k-gas txs at the 100M-gas
	// block limit. Sized to absorb the idle provider's per-loop burst (bounded
	// by gas budget) without ever blocking a sender; workers drain far faster
	// than the idle heap can fill in practice. Channel memory is ~33 KB.
	prefetchChanBufSize = 4096

	// prefetchIdleLoopInterval is the minimum cadence between idle-phase pool
	// snapshots in runIdleTxProvider.
	prefetchIdleLoopInterval = 100 * time.Millisecond

	// prefetchDefaultGasLimitPercent is the default percentage of header
	// gas limit used as the idle-phase prefetch budget when unconfigured.
	prefetchDefaultGasLimitPercent = 100

	// prefetchMaxGasLimitPercent caps the idle-phase prefetch gas budget to
	// guard against misconfiguration DoS.
	prefetchMaxGasLimitPercent = 150
)

var (
	errBlockInterruptedByNewHead  = errors.New("new head arrived while building block")
	errBlockInterruptedByRecommit = errors.New("recommit interrupt while building block")
	errBlockInterruptedByTimeout  = errors.New("timeout while building block")

	// metrics gauge to track total and empty blocks sealed by a miner
	sealedBlocksCounter      = metrics.NewRegisteredCounter("worker/sealedBlocks", nil)
	sealedEmptyBlocksCounter = metrics.NewRegisteredCounter("worker/sealedEmptyBlocks", nil)
	txCommitInterruptCounter = metrics.NewRegisteredCounter("worker/txCommitInterrupt", nil)

	// txHeapInitTimer measures time taken to initialise a heap of pending transactions from pool
	txHeapInitTimer = metrics.NewRegisteredTimer("worker/txheapinit", nil)
	// prepareWorkTimer measures time taken to prepare environment for block building which
	// includes the `bor.Prepare` call as well.
	prepareWorkTimer = metrics.NewRegisteredTimer("worker/prepareWork", nil)
	// pendingTimer measures time taken to fetch transactions from pool in the actual block
	// building cycle (excluding the calls made by prefetcher).
	pendingTimer = metrics.NewRegisteredTimer("worker/pending", nil)
	// commitTransactionsTimer measures time taken to execute transactions
	commitTransactionsTimer = metrics.NewRegisteredTimer("worker/commitTransactions", nil)
	// txApplyDurationTimer captures per-transaction apply latency during block building.
	// Uses a larger reservoir to preserve tail visibility on high-throughput blocks.
	txApplyDurationTimer = newRegisteredCustomTimer("worker/txApplyDuration", 8192)
	// Split variants of txApplyDuration by prefetch status. The aggregate timer
	// above stays to preserve existing Grafana dashboards.
	txApplyDurationPrefetchedTimer    = newRegisteredCustomTimer("worker/txApplyDuration/prefetched", 8192)
	txApplyDurationNotPrefetchedTimer = newRegisteredCustomTimer("worker/txApplyDuration/notPrefetched", 8192)
	// finalizeAndAssembleTimer measures time taken to finalize and assemble the block (state root calculation).
	// NOT emitted when pipelined SRC is enabled: the pipelined path uses
	// FinalizeForPipeline, which deliberately skips the inline IntermediateRoot
	// (the root comes from the background SRC goroutine instead). Closest
	// pipeline equivalents: worker/pipelineSRCTime (total SRC compute) and
	// worker/pipelineSRCWait (portion of SRC that actually blocked the caller).
	finalizeAndAssembleTimer = metrics.NewRegisteredTimer("worker/finalizeAndAssemble", nil)
	// intermediateRootTimer measures time taken to calculate intermediate root.
	// NOT emitted when pipelined SRC is enabled: there is no inline root calculation
	// under pipelining — the SRC goroutine computes it in parallel with the next
	// block's execution. Closest pipeline equivalent: worker/pipelineSRCTime (cost)
	// or worker/pipelineSRCWait (how much of the cost was hidden by the overlap).
	intermediateRootTimer = metrics.NewRegisteredTimer("worker/intermediateRoot", nil)
	// commitTimer measures total time for complete block building (tx execution + finalization + state root).
	// NOT emitted when pipelined SRC is enabled: the pipelined model has no
	// single contiguous "build" interval — speculative fill of N+1 overlaps with
	// SRC(N), so fabricating a total would be misleading. Closest pipeline signals:
	// worker/pipelineSRCWait + worker/pipelineSealDuration + worker/pipelineAnnounceEarlinessMs.
	commitTimer = metrics.NewRegisteredTimer("worker/commit", nil)
	// writeBlockAndSetHeadTimer measures total time for WriteBlockAndSetHead in the seal result loop.
	// This covers the entire gap between block sealing and event posting: witness encoding, batch write,
	// state commit, and (in hashdb mode) trie GC. Spikes here directly delay block broadcasting.
	writeBlockAndSetHeadTimer = metrics.NewRegisteredTimer("worker/writeBlockAndSetHead", nil)

	// Cache hit/miss metrics for block production (miner path)
	// These are the same meters used by the import path in blockchain.go
	accountCacheHitMeter  = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/process/hit", nil)
	accountCacheMissMeter = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/process/miss", nil)
	storageCacheHitMeter  = metrics.NewRegisteredMeter("worker/chain/storage/reads/cache/process/hit", nil)
	storageCacheMissMeter = metrics.NewRegisteredMeter("worker/chain/storage/reads/cache/process/miss", nil)

	accountCacheHitPrefetchMeter  = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/prefetch/hit", nil)
	accountCacheMissPrefetchMeter = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/prefetch/miss", nil)
	storageCacheHitPrefetchMeter  = metrics.NewRegisteredMeter("worker/chain/storage/reads/cache/prefetch/hit", nil)
	storageCacheMissPrefetchMeter = metrics.NewRegisteredMeter("worker/chain/storage/reads/cache/prefetch/miss", nil)

	// Additional prefetch attribution metrics
	accountHitFromPrefetchMeter       = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/process/hit_from_prefetch", nil)
	storageHitFromPrefetchMeter       = metrics.NewRegisteredMeter("worker/chain/storage/reads/cache/process/hit_from_prefetch", nil)
	accountInsertPrefetchMeter        = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/prefetch/insert", nil)
	storageInsertPrefetchMeter        = metrics.NewRegisteredMeter("worker/chain/storage/reads/cache/prefetch/insert", nil)
	accountHitFromPrefetchUniqueMeter = metrics.NewRegisteredMeter("worker/chain/account/reads/cache/process/prefetch_used_unique", nil)
	prefetchPanicMeter                = metrics.NewRegisteredMeter("worker/prefetch/panic", nil)

	// prefetchMissRateHistogram tracks percentage of block transactions that were NOT prefetched.
	// Values range 0-100. High percentiles indicate prefetch degradation.
	prefetchMissRateHistogram = metrics.NewRegisteredHistogram(
		"worker/prefetch/miss_rate_percent",
		nil,
		metrics.NewExpDecaySample(1028, 0.015),
	)

	// prefetchBuilderAddedHistogram tracks the percentage of block transactions that were
	// prefetched exclusively during the builder phase (i.e. would have been a miss if the
	// idle phase had been the only prefetch source). Directly measures the payoff of the
	// builder-phase prefetch over the aggregate miss rate above.
	prefetchBuilderAddedHistogram = metrics.NewRegisteredHistogram(
		"worker/prefetch/builder_added_percent",
		nil,
		metrics.NewExpDecaySample(1028, 0.015),
	)

	// Trie read/hash/execution metrics for block production (mirroring blockchain.go import path).
	// Namespaced under worker/chain/ to distinguish from import-path chain/ metrics.
	workerAccountReadTimer         = metrics.NewRegisteredResettingTimer("worker/chain/account/reads", nil)
	workerStorageReadTimer         = metrics.NewRegisteredResettingTimer("worker/chain/storage/reads", nil)
	workerSnapshotAccountReadTimer = metrics.NewRegisteredResettingTimer("worker/chain/snapshot/account/reads", nil)
	workerSnapshotStorageReadTimer = metrics.NewRegisteredResettingTimer("worker/chain/snapshot/storage/reads", nil)
	workerAccountUpdateTimer       = metrics.NewRegisteredResettingTimer("worker/chain/account/updates", nil)
	workerStorageUpdateTimer       = metrics.NewRegisteredResettingTimer("worker/chain/storage/updates", nil)
	workerAccountHashTimer         = metrics.NewRegisteredResettingTimer("worker/chain/account/hashes", nil)
	workerStorageHashTimer         = metrics.NewRegisteredTimer("worker/chain/storage/hashes", nil)
	workerBorConsensusTimer        = metrics.NewRegisteredTimer("worker/chain/bor/consensus", nil)
	workerBlockExecutionTimer      = metrics.NewRegisteredTimer("worker/chain/execution", nil)
	workerMgaspsTimer              = metrics.NewRegisteredResettingTimer("worker/chain/mgasps", nil)
	// Throughput histograms — mode-agnostic. For the pipelined path, "per-block build elapsed"
	// isn't a single contiguous interval, so mgasps is only emitted by the normal path.
	// gas_used_per_block and txs_per_block are emitted in both modes.
	workerGasUsedPerBlockHistogram = metrics.NewRegisteredHistogram("worker/chain/gas_used_per_block", nil, metrics.NewExpDecaySample(1028, 0.015))
	workerTxsPerBlockHistogram     = metrics.NewRegisteredHistogram("worker/chain/txs_per_block", nil, metrics.NewExpDecaySample(1028, 0.015))
	// End-to-end producer timer: wall clock from build begin to NewMinedBlockEvent broadcast.
	// Fires in both normal (resultLoop → mux.Post) and pipelined (inlineSealAndBroadcast → mux.Post) modes,
	// giving a directly comparable apples-to-apples A/B signal.
	workerBuildToAnnounceTimer = metrics.NewRegisteredTimer("worker/build_to_announce", nil)

	// Trie commit metrics for block production (populated after WriteBlockAndSetHead → CommitWithUpdate).
	workerAccountCommitTimer     = metrics.NewRegisteredResettingTimer("worker/chain/account/commits", nil)
	workerStorageCommitTimer     = metrics.NewRegisteredResettingTimer("worker/chain/storage/commits", nil)
	workerSnapshotCommitTimer    = metrics.NewRegisteredResettingTimer("worker/chain/snapshot/commits", nil)
	workerTriedbCommitTimer      = metrics.NewRegisteredResettingTimer("worker/chain/triedb/commits", nil)
	workerWitnessCollectionTimer = metrics.NewRegisteredTimer("worker/chain/witness/collection", nil)
)

// firstNonZeroTime returns a if non-zero, otherwise b.
func firstNonZeroTime(a, b time.Time) time.Time {
	if !a.IsZero() {
		return a
	}
	return b
}

// productionStartFrom extracts the productionStart time from genParams.
// Returns zero time if genParams is nil, matching the guarded access pattern
// already used elsewhere in commit() (e.g. the genParams != nil check at the
// prefetch coverage block).
func productionStartFrom(genParams *generateParams) time.Time {
	if genParams == nil {
		return time.Time{}
	}
	return genParams.productionStart
}

func newRegisteredCustomTimer(name string, reservoirSize int) *metrics.Timer {
	return metrics.GetOrRegister(name, func() interface{} {
		return metrics.NewCustomTimer(
			metrics.NewHistogram(metrics.NewExpDecaySample(reservoirSize, 0.015)),
			metrics.NewMeter(),
		)
	}).(*metrics.Timer)
}

// environment is the worker's current environment and holds all
// information of the sealing block generation.
type environment struct {
	signer           types.Signer
	state            *state.StateDB // apply state changes here
	tcount           int            // tx count in cycle
	size             uint64         // size of the block we are building
	stateSyncReserve uint64         // block-size budget reserved for the state-sync system tx appended in Finalize (Valencia+)
	gasPool          *core.GasPool  // available gas used to pack transactions
	coinbase         common.Address
	evm              *vm.EVM
	// buildInterrupt owns the timeout signal for this specific block-building
	// attempt. It must not be shared across overlapping sequential/speculative
	// builds, otherwise one timer can abort another build.
	buildInterrupt *buildInterruptState

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt
	sidecars []*types.BlobTxSidecar
	blobs    int

	// reservedGasUsed accumulates the actual gas used by reserved-region
	// transactions, written into BlockExtraData.ReservedGasUsed at the end of
	// the build. Quota accounting (declared gas limits) lives in sequencing.
	reservedGasUsed uint64

	witness *stateless.Witness

	// Readers with stats tracking for metrics reporting
	prefetchReader state.ReaderWithStats
	processReader  state.ReaderWithStats

	// prefetchedTxHashes is the live set written by the prefetch stream's
	// onSuccess callback. Read at tx-commit time to annotate slow-tx logs and
	// split the apply-duration histogram by prefetch status. May be nil.
	prefetchedTxHashes *sync.Map

	// Observability for pre block building phase
	makeEnvDuration    time.Duration
	makeHeaderDuration time.Duration // primarily includes call to bor.Prepare
	// Track time taken to fetch pending transactions during block building
	pendingDuration time.Duration
}

type buildInterruptState struct {
	timedOut  atomic.Bool
	flagSetAt atomic.Int64
}

func newBuildInterruptState() *buildInterruptState {
	return &buildInterruptState{}
}

func (s *buildInterruptState) timeoutFlag() *atomic.Bool {
	if s == nil {
		return nil
	}
	return &s.timedOut
}

func (s *buildInterruptState) flagSetAtPtr() *atomic.Int64 {
	if s == nil {
		return nil
	}
	return &s.flagSetAt
}

func (w *worker) interruptStateForEnv(env *environment) (*atomic.Bool, *atomic.Int64) {
	if env != nil && env.header != nil && w.isPipelineEligible(env.header.Number.Uint64()) {
		return env.buildInterrupt.timeoutFlag(), env.buildInterrupt.flagSetAtPtr()
	}
	return &w.interruptBlockBuilding, &w.interruptFlagSetAt
}

// copy creates a deep copy of environment.
func (env *environment) copy() *environment {
	cpy := &environment{
		signer:             env.signer,
		state:              env.state.Copy(),
		tcount:             env.tcount,
		stateSyncReserve:   env.stateSyncReserve,
		coinbase:           env.coinbase,
		buildInterrupt:     newBuildInterruptState(),
		header:             types.CopyHeader(env.header),
		receipts:           copyReceipts(env.receipts),
		reservedGasUsed:    env.reservedGasUsed,
		prefetchReader:     env.prefetchReader,
		processReader:      env.processReader,
		prefetchedTxHashes: env.prefetchedTxHashes,
		makeEnvDuration:    env.makeEnvDuration,
		makeHeaderDuration: env.makeHeaderDuration,
		pendingDuration:    env.pendingDuration,
	}

	if env.gasPool != nil {
		gasPool := *env.gasPool
		cpy.gasPool = &gasPool
	}
	cpy.txs = make([]*types.Transaction, len(env.txs))
	copy(cpy.txs, env.txs)

	cpy.sidecars = make([]*types.BlobTxSidecar, len(env.sidecars))
	copy(cpy.sidecars, env.sidecars)

	return cpy
}

// discard terminates the background prefetcher go-routine. It should
// always be called for all created environment instances otherwise
// the go-routine leak can happen.
func (env *environment) discard() {
	if env.state == nil {
		return
	}

	env.state.StopPrefetcher()
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts             []*types.Receipt
	state                *state.StateDB
	block                *types.Block
	createdAt            time.Time
	productionStart      time.Time     // wall clock at build begin — used for worker/build_to_announce (fires from resultLoop at mux.Post)
	productionElapsed    time.Duration // elapsed from after prepareWork to task submission (excludes sealing wait); used for workerMgaspsTimer and workerBlockExecutionTimer
	intermediateRootTime time.Duration // time spent in IntermediateRoot inside FinalizeAndAssemble; subtracted when computing workerBlockExecutionTimer
	// reservedTxIndexes lists positions within block.Transactions() classified
	// reserved (fee-free), strictly ascending. Persisted alongside receipts by
	// the result loop so reads report the correct effective gas price for
	// reserved transactions.
	reservedTxIndexes []uint64
	// reservedClientUsage is the block's per-client reserved-region usage,
	// re-derived from the final body with the same walk a verifier runs, so
	// the producer's chain/reserved client gauges match what every importing
	// node reports for this block.
	reservedClientUsage map[uint64]registryreader.ClientUsage

	pipelined    bool   // If true, state was already committed by SRC goroutine — skip CommitWithUpdate in writeBlockWithState
	witnessBytes []byte // RLP-encoded witness from SRC goroutine (for pipelined blocks)
}

// stateSyncReserveFor returns the block-size budget to hold back for the state-sync
// system tx that CommitStates appends at sprint start (Valencia+). Only sprint-start
// blocks carry that tx, so only they reserve; pre-Valencia and non-Bor configs
// reserve nothing.
func stateSyncReserveFor(config *params.ChainConfig, number *big.Int) uint64 {
	if config.Bor == nil || !config.Bor.IsValencia(number) {
		return 0
	}

	// Reserve only at sprint start. Fall back to reserving when the sprint length
	// is unknown, which avoids a divide-by-zero and never under-reserves.
	sprint := uint64(0)
	if len(config.Bor.Sprint) > 0 {
		sprint = config.Bor.CalculateSprint(number.Uint64())
	}
	if sprint > 0 && !bor.IsSprintStart(number.Uint64(), sprint) {
		return 0
	}

	return params.MaxStateSyncBytesPerBlock
}

// txFits reports whether the transaction fits into the block size limit.
func (env *environment) txFitsSize(tx *types.Transaction) bool {
	return env.size+tx.Size() < params.MaxBlockSize-maxBlockSizeBufferZone-env.stateSyncReserve
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
	commitInterruptTimeout
)

// Block size is capped by the protocol at params.MaxBlockSize. When producing blocks, we
// try to say below the size including a buffer zone, this is to avoid going over the
// maximum size with auxiliary data added into the block.
const maxBlockSizeBufferZone = 1_000_000

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt *atomic.Int32
	noempty   bool
	timestamp int64
}

// newPayloadResult is the result of payload generation.
type newPayloadResult struct {
	err      error
	block    *types.Block
	fees     *big.Int               // total block fees
	sidecars []*types.BlobTxSidecar // collected blobs of blob transactions
	stateDB  *state.StateDB         // StateDB after executing the transactions
	receipts []*types.Receipt       // Receipts collected during construction
	requests [][]byte               // Consensus layer requests collected during block construction
	witness  *stateless.Witness     // Witness is an optional stateless proof

}

// getWorkReq represents a request for getting a new sealing work with provided parameters.
type getWorkReq struct {
	//nolint:containedctx
	ctx    context.Context
	params *generateParams
	result chan *newPayloadResult // non-blocking channel
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config      *Config
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	eth         Backend
	chain       *core.BlockChain

	prio []common.Address // A list of senders to prioritize

	// reservedSnapshotOverride replaces the env's registry snapshot during
	// block building. Test seam only — production sequencing reads the
	// snapshot makeEnv pins on the EVM block context.
	reservedSnapshotOverride *registryreader.Snapshot

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan core.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan core.ChainHeadEvent
	chainHeadSub event.Subscription

	// Channels
	newWorkCh          chan *newWorkReq
	getWorkCh          chan *getWorkReq
	taskCh             chan *task
	resultCh           chan *consensus.NewSealedBlockEvent
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	wg         sync.WaitGroup
	prefetchWg sync.WaitGroup

	currentMu sync.RWMutex // The lock used to protect the current environment
	current   *environment // An environment for current running cycle.

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte
	tip      *uint256.Int // Minimum tip needed for non-local transaction to include them

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	// Block number which is currently being worked on (0 = none).
	// Used to prevent duplicate work.
	pendingWorkBlock atomic.Uint64

	// When set, the next sequential build is recovering a discarded
	// speculative block and should preserve its original target slot.
	nextCommitAbortRecovery atomic.Bool

	snapshotMu       sync.RWMutex // The lock used to protect the snapshots below
	snapshotBlock    *types.Block
	snapshotReceipts types.Receipts
	snapshotState    *state.StateDB

	// atomic status counters
	running atomic.Bool  // The indicator whether the consensus engine is running or not.
	newTxs  atomic.Int32 // New arrival transaction count since last sealing work submitting.
	syncing atomic.Bool  // The indicator whether the node is still syncing.

	// newpayloadTimeout is the maximum timeout allowance for creating payload.
	// The default value is 2 seconds but node operator can set it to arbitrary
	// large value. A large timeout allowance may cause Geth to fail creating
	// a non-empty payload within the specified time and eventually miss the slot
	// in case there are some computation expensive transactions in txpool.
	newpayloadTimeout time.Duration

	// recommit is the time interval to re-create sealing work or to re-build
	// payload in proof-of-stake stage.
	recommit time.Duration

	// External functions
	isLocalBlock func(header *types.Header) bool // Function used to determine whether the specified block is mined by local miner.

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.

	// Interrupt commit to stop block building on time. Develop-compatible
	// sequential builds use the worker-global flag; pipelined builds switch to
	// per-environment timeout state so overlapping builds cannot interrupt each other.
	interruptCommitFlag    bool
	interruptBlockBuilding atomic.Bool
	interruptFlagSetAt     atomic.Int64
	mockTxDelay            uint // A mock delay for transaction execution, only used in tests

	blockTime     time.Duration     // The block time defined by the miner. Needs to be larger or equal to the consensus block time. If not set (default = 0), the miner will use the consensus block time.
	slowTxTracker *slowTxTopTracker // Tracks top slow transactions for periodic reporting.

	// noempty is the flag used to control whether the feature of pre-seal empty
	// block is enabled. The default value is false(pre-seal is enabled by default).
	// But in some special scenario the consensus engine will seal blocks instantaneously,
	// in this case this feature will add all empty blocks into canonical chain
	// non-stop and no real transaction will be included.
	noempty atomic.Bool

	makeWitness bool

	// Pipelined SRC: speculative work channel for block N+1 execution
	speculativeWorkCh chan *speculativeWorkReq
}

//nolint:staticcheck
func newWorker(config *Config, chainConfig *params.ChainConfig, engine consensus.Engine, eth Backend, mux *event.TypeMux, isLocalBlock func(header *types.Header) bool, init bool, makeWitness bool) *worker {
	worker := &worker{
		config:              config,
		chainConfig:         chainConfig,
		engine:              engine,
		eth:                 eth,
		chain:               eth.BlockChain(),
		mux:                 mux,
		isLocalBlock:        isLocalBlock,
		coinbase:            config.Etherbase,
		extra:               config.ExtraData,
		tip:                 uint256.MustFromBig(config.GasPrice),
		pendingTasks:        make(map[common.Hash]*task),
		txsCh:               make(chan core.NewTxsEvent, txChanSize),
		chainHeadCh:         make(chan core.ChainHeadEvent, chainHeadChanSize),
		newWorkCh:           make(chan *newWorkReq),
		getWorkCh:           make(chan *getWorkReq),
		taskCh:              make(chan *task),
		resultCh:            make(chan *consensus.NewSealedBlockEvent, resultQueueSize),
		startCh:             make(chan struct{}, 1),
		exitCh:              make(chan struct{}),
		resubmitIntervalCh:  make(chan time.Duration),
		resubmitAdjustCh:    make(chan *intervalAdjust, resubmitAdjustChanSize),
		interruptCommitFlag: config.CommitInterruptFlag,
		blockTime:           config.BlockTime,
		slowTxTracker:       newSlowTxTopTracker(),
		makeWitness:         makeWitness,
		speculativeWorkCh:   make(chan *speculativeWorkReq, 1),
	}
	worker.noempty.Store(true)
	// Production-side pipelined SRC is intentionally disabled and no longer has
	// a miner config knob. Keep the gauge at 0; import-side pipelining has its
	// own chain/imports/pipelined/enabled gauge.
	pipelineBuildEnabledGauge.Update(0)
	// Subscribe for transaction insertion events (whether from network or resurrects)
	worker.txsSub = eth.TxPool().SubscribeTransactions(worker.txsCh, true)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)

	if !worker.interruptCommitFlag {
		worker.noempty.Store(false)
	}

	// Sanitize recommit interval if the user-specified one is too short.
	recommit := worker.config.Recommit
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	worker.recommit = recommit

	// Sanitize the timeout config for creating payload.
	newpayloadTimeout := worker.config.NewPayloadTimeout
	if newpayloadTimeout == 0 {
		log.Warn("Sanitizing new payload timeout to default", "provided", newpayloadTimeout, "updated", DefaultConfig.NewPayloadTimeout)
		newpayloadTimeout = DefaultConfig.NewPayloadTimeout
	}

	if newpayloadTimeout < time.Millisecond*100 {
		log.Warn("Low payload timeout may cause high amount of non-full blocks", "provided", newpayloadTimeout, "default", DefaultConfig.NewPayloadTimeout)
	}

	worker.newpayloadTimeout = newpayloadTimeout

	worker.wg.Add(4)

	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()

	// Submit first work to initialize pending state.
	if init {
		worker.startCh <- struct{}{}
	}

	return worker
}

// setMockTxDelay sets the delay field used for inducing delay in between
// transaction execution in tests.
func (w *worker) setMockTxDelay(mockTxDelay uint) {
	w.mockTxDelay = mockTxDelay
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// etherbase retrieves the configured etherbase address.
func (w *worker) etherbase() common.Address {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.coinbase
}

func (w *worker) setGasCeil(ceil uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config.GasCeil = ceil
}

// calculateDesiredGasLimit determines the target gas limit based on configuration.
// If dynamic gas limit is enabled, it adjusts based on the parent's base fee:
// - When base fee > target + buffer: target max gas limit (increase supply)
// - When base fee < target - buffer: target min gas limit (decrease supply)
// - When within buffer: maintain current gas limit (no change)
// If dynamic gas limit is disabled, returns the static GasCeil value.
func (w *worker) calculateDesiredGasLimit(parent *types.Header) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.calculateDesiredGasLimitLocked(parent)
}

// calculateDesiredGasLimitLocked requires w.mu to be held for reading.
func (w *worker) calculateDesiredGasLimitLocked(parent *types.Header) uint64 {
	// If dynamic gas limit is not enabled, use the static GasCeil
	if !w.config.EnableDynamicGasLimit {
		return w.config.GasCeil
	}

	// Pre-London blocks don't have base fee, use static GasCeil
	if parent.BaseFee == nil {
		return w.config.GasCeil
	}

	parentBaseFee := parent.BaseFee.Uint64()
	targetBaseFee := w.config.TargetBaseFee
	buffer := w.config.BaseFeeBuffer

	// Calculate bounds
	upperBound := targetBaseFee + buffer
	var lowerBound uint64
	if buffer < targetBaseFee {
		lowerBound = targetBaseFee - buffer
	} else {
		lowerBound = 0 // Prevent underflow
	}

	// Determine desired gas limit based on base fee position
	if parentBaseFee > upperBound {
		// Base fee is too high, increase gas limit to max to reduce fee pressure
		return w.config.GasLimitMax
	} else if parentBaseFee < lowerBound {
		// Base fee is too low, decrease gas limit to min to increase fee pressure
		return w.config.GasLimitMin
	}

	// Within buffer zone, maintain current gas limit
	return parent.GasLimit
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setGasTip sets the minimum miner tip needed to include a non-local transaction.
func (w *worker) setGasTip(tip *big.Int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tip = uint256.MustFromBig(tip)
}

// setPrio sets the list of addresses to prioritize for transaction inclusion.
func (w *worker) setPrio(prio []common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prio = prio
}

// getCurrent returns the current environment safely for testing.
func (w *worker) getCurrent() *environment {
	w.currentMu.RLock()
	defer w.currentMu.RUnlock()
	return w.current
}

// setReservedSnapshot installs a fixed reserved-registry snapshot the
// sequencing pass classifies against, bypassing the env's snapshot. Test seam
// only.
func (w *worker) setReservedSnapshot(snap *registryreader.Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reservedSnapshotOverride = snap
}

// resolveReservedSnapshot returns the reserved-registry snapshot for building
// header: a test/injection override (setReservedSnapshot) if set, else the
// chain's registry read at the parent state. A post-fork read failure is fatal
// — the producer must not build a block it can't classify (mirrors the
// verifier's hard error).
//
// Called only from makeEnv, which runs under prepareWork's w.mu.RLock, so the
// override is read directly: taking the read lock again would deadlock against a
// concurrent writer (e.g. setExtra's w.mu.Lock) arriving between the two RLocks,
// since Go's RWMutex blocks the second RLock to avoid writer starvation.
func (w *worker) resolveReservedSnapshot(state *state.StateDB, header *types.Header) (*registryreader.Snapshot, error) {
	if override := w.reservedSnapshotOverride; override != nil {
		return override, nil
	}
	return core.ReservedSnapshotForBlock(w.chain, state, header)
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	select {
	case w.resubmitIntervalCh <- interval:
	case <-w.exitCh:
	}
}

// pending returns the pending state and corresponding block. The returned
// values can be nil in case the pending block is not initialized.
func (w *worker) pending() (*types.Block, types.Receipts, *state.StateDB) {
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()

	if w.snapshotState == nil {
		return nil, nil, nil
	}

	return w.snapshotBlock, w.snapshotReceipts, w.snapshotState.Copy()
}

// pendingBlock returns pending block. The returned block can be nil in case the
// pending block is not initialized.
func (w *worker) pendingBlock() *types.Block {
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()

	return w.snapshotBlock
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	w.running.Store(true)
	w.startCh <- struct{}{}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	w.running.Store(false)
}

// IsRunning returns an indicator whether worker is running or not.
func (w *worker) IsRunning() bool {
	return w.running.Load()
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	w.running.Store(false)
	close(w.exitCh)
	w.wg.Wait()
	w.prefetchWg.Wait()
}

// recalcRecommit recalculates the resubmitting interval upon feedback.
func recalcRecommit(minRecommit, prev time.Duration, target float64, inc bool) time.Duration {
	//var (
	//	prevF = float64(prev.Nanoseconds())
	//	next  float64
	//)
	//if inc {
	//	next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
	//	max := float64(maxRecommitInterval.Nanoseconds())
	//	if next > max {
	//		next = max
	//	}
	//} else {
	//	next = prevF*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
	//	min := float64(minRecommit.Nanoseconds())
	//	if next < min {
	//		next = min
	//	}
	//}
	return prev
}

// veblopFallbackDecision is the action the veblop stall-fallback takes when its
// timer fires. See decideVeblopFallback.
type veblopFallbackDecision int

const (
	// veblopWait: the next block is being built normally (chain not yet stale)
	// or work is otherwise progressing — just rearm the timer.
	veblopWait veblopFallbackDecision = iota
	// veblopSkip: a sealing task is genuinely in flight for the next block —
	// nothing to do.
	veblopSkip
	// veblopRecommit: the chain is stale and there is no sealing task in flight,
	// so the producer must (re)submit work to make progress.
	veblopRecommit
)

// decideVeblopFallback decides what the veblop stall-fallback should do when its
// timer fires. It is a pure function so the decision can be unit-tested without
// driving the whole newWorkLoop.
//
// The critical case is `pendingWorkBlock == nextBlock && !hasPendingTasks`: a
// previous build pinned pendingWorkBlock (commitWork only clears it via a
// deferred Store(0) that runs on return) but produced no sealing task. This can
// mean either (a) a build is legitimately in progress and just hasn't registered
// its task yet, or (b) the build wedged inside commitWork and never will — the
// mainnet stall. The old logic skipped recovery on `pendingWorkBlock == nextBlock`
// alone, so case (b) never recovered without a process restart.
//
// These two cases are distinguished ONLY by how long the build has been
// outstanding (pendingWorkAge = time since the build was submitted), NOT by
// block-timestamp age (chainAge is meaningless when the head carries an old
// timestamp, e.g. at genesis). We recover only once the outstanding build clearly
// exceeds a normal build duration (stallThreshold), so a slow-but-live build is
// never interrupted.
//
// All durations are compared at their native resolution: pendingWorkAge is real
// elapsed wall-clock (sub-second precise), so the threshold stays accurate at the
// sub-second block times mainnet runs (miner block time is 1.5s). chainAge is the
// exception — it derives from integer block timestamps, so it is inherently
// second-granular regardless of representation.
func decideVeblopFallback(pendingWorkBlock, nextBlock uint64, hasPendingTasks bool, chainAge, veblopTimeout, pendingWorkAge, stallThreshold time.Duration) veblopFallbackDecision {
	// A sealing task is in flight; never interrupt it.
	if hasPendingTasks {
		return veblopSkip
	}
	if pendingWorkBlock == nextBlock {
		// Build outstanding for the next block. Recover only if it has been
		// outstanding long enough to be considered wedged rather than in-progress.
		if pendingWorkAge >= stallThreshold {
			return veblopRecommit
		}
		return veblopWait
	}
	// Nothing is claimed for the next block — resubmit once the chain is stale.
	if chainAge >= veblopTimeout {
		return veblopRecommit
	}
	return veblopWait
}

// newWorkLoop is a standalone goroutine to submit new sealing work upon received events.
//
//nolint:gocognit
func (w *worker) newWorkLoop(recommit time.Duration) {
	defer w.wg.Done()

	_, isBor := w.engine.(*bor.Bor)

	var (
		interrupt   *atomic.Int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of sealing.
		// Stall-detection state for veblopTimer: tracks the last time we
		// emitted a stall warning so the log isn't flooded while the
		// producer is stuck.
		lastStallWarnAt time.Time
		// pendingWorkSubmittedAt records when we last submitted a sealing build
		// for the next block. The veblop fallback uses time-since-submit (not
		// block-timestamp age) to tell a wedged build apart from one that is
		// simply still in progress.
		pendingWorkSubmittedAt time.Time
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	<-timer.C // discard the initial tick

	veblopTimeout := time.Duration(w.chainConfig.Bor.CalculatePeriod(w.chain.CurrentBlock().Number.Uint64())) * time.Second
	if veblopTimeout < w.blockTime {
		veblopTimeout = w.blockTime
	}
	veblopTimer := time.NewTimer(veblopTimeout)
	defer veblopTimer.Stop()

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	// It returns true if the new work request was submitted to mainLoop.
	//
	// When nonblocking is true the request is sent with a non-blocking send: if
	// mainLoop is not currently ready to receive (e.g. it is wedged inside
	// commitWork — exactly the state that pins pendingWorkBlock with no pending
	// task), the request is dropped and commit returns false instead of blocking
	// newWorkLoop on the unbuffered newWorkCh. This keeps newWorkLoop alive so it
	// keeps emitting the stall warning and retrying every tick.
	commit := func(noempty bool, s int32, nonblocking bool) bool {
		if interrupt != nil {
			interrupt.Store(s)
		}

		newInterrupt := new(atomic.Int32)
		req := &newWorkReq{interrupt: newInterrupt, timestamp: timestamp, noempty: noempty}
		if nonblocking {
			select {
			case w.newWorkCh <- req:
			case <-w.exitCh:
				return false
			default:
				// mainLoop not ready (busy or wedged); don't block.
				return false
			}
		} else {
			select {
			case w.newWorkCh <- req:
			case <-w.exitCh:
				return false
			}
		}
		interrupt = newInterrupt
		timer.Reset(recommit)
		veblopTimeout = time.Duration(w.chainConfig.Bor.CalculatePeriod(w.chain.CurrentBlock().Number.Uint64())) * time.Second
		if veblopTimeout < w.blockTime {
			veblopTimeout = w.blockTime
		}
		veblopTimer.Reset(veblopTimeout)
		w.newTxs.Store(0)
		return true
	}

	for {
		select {
		case <-w.startCh:
			w.clearPending(w.chain.CurrentBlock().Number.Uint64())

			timestamp = time.Now().Unix()
			w.pendingWorkBlock.Store(w.chain.CurrentBlock().Number.Uint64() + 1)
			pendingWorkSubmittedAt = time.Now()
			commit(false, commitInterruptNewHead, false)

		case head := <-w.chainHeadCh:
			w.clearPending(head.Header.Number.Uint64())

			pendingWorkBlock := w.pendingWorkBlock.Load()
			if pendingWorkBlock == head.Header.Number.Uint64()+1 {
				// Next block is already being worked on, skip the commit.
				continue
			}

			timestamp = time.Now().Unix()
			w.pendingWorkBlock.Store(head.Header.Number.Uint64() + 1)
			pendingWorkSubmittedAt = time.Now()
			commit(false, commitInterruptNewHead, false)

		case <-veblopTimer.C:
			currentBlock := w.chain.CurrentBlock()

			veblopTimeout = time.Duration(w.chainConfig.Bor.CalculatePeriod(currentBlock.Number.Uint64())) * time.Second
			if veblopTimeout < w.blockTime {
				veblopTimeout = w.blockTime
			}

			// Veblop fallback fires for any Bor chain — pre-Rio needs the
			// retry to recover after a transient peer outage on real
			// network nodes, since startCh fires only once on startup.
			if !isBor || w.chainConfig.Bor == nil {
				veblopTimer.Reset(veblopTimeout)
				continue
			}

			w.pendingMu.RLock()
			pendingTasksCount := len(w.pendingTasks)
			w.pendingMu.RUnlock()
			hasPendingTasks := pendingTasksCount > 0

			pendingWorkBlock := w.pendingWorkBlock.Load()
			chainAgeSec := time.Now().Unix() - int64(currentBlock.Time)
			lastStallWarnAt = w.warnIfStalled(currentBlock, chainAgeSec, veblopTimeout, pendingWorkBlock, pendingTasksCount, lastStallWarnAt)

			// veblopTimeout is already floored to the miner block time by commit();
			// guard only the degenerate non-positive case so the threshold can't
			// collapse to zero and interrupt builds instantly. Do NOT round up to a
			// whole second — that would break sub-second block times.
			effTimeout := veblopTimeout
			if effTimeout <= 0 {
				effTimeout = time.Second
			}
			// A build is treated as wedged only after it has been outstanding for
			// noticeably longer than a normal build (~3x the block period, the same
			// staleness multiple warnIfStalled uses), so a slow-but-live build is
			// never interrupted. Measured from submit time, not block-timestamp age,
			// at full sub-second resolution.
			stallThreshold := 3 * effTimeout
			var pendingWorkAge time.Duration
			if !pendingWorkSubmittedAt.IsZero() {
				pendingWorkAge = time.Since(pendingWorkSubmittedAt)
			}

			switch decideVeblopFallback(
				pendingWorkBlock,
				currentBlock.Number.Uint64()+1,
				hasPendingTasks,
				time.Duration(chainAgeSec)*time.Second,
				effTimeout,
				pendingWorkAge,
				stallThreshold,
			) {
			case veblopRecommit:
				// No sealing task is in flight and the producer is not making
				// progress — either nothing is claimed for the next block, or a
				// build wedged inside commitWork and left pendingWorkBlock pinned.
				// Resubmit with a non-blocking send so newWorkLoop never wedges on
				// the unbuffered newWorkCh if mainLoop is itself blocked; only claim
				// pendingWorkBlock once the request is actually accepted.
				timestamp = time.Now().Unix()
				if commit(false, commitInterruptNewHead, true) {
					w.pendingWorkBlock.Store(currentBlock.Number.Uint64() + 1)
					pendingWorkSubmittedAt = time.Now()
				} else {
					// mainLoop not ready; retry on the next tick.
					veblopTimer.Reset(veblopTimeout)
				}
				// On success commit() already reset veblopTimer.
			default:
				// veblopSkip (task genuinely in flight) or veblopWait (chain not
				// yet stale) — nothing to submit, just rearm the timer.
				veblopTimer.Reset(veblopTimeout)
			}

		case <-timer.C:
			// Recommit disabled due to the current low block period (no need to capture more txs on the block already built)
			continue

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}

			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				target := float64(recommit.Nanoseconds()) / adjust.ratio
				recommit = recalcRecommit(minRecommit, recommit, target, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recommit = recalcRecommit(minRecommit, recommit, float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// schedulePipelineRetry re-enters block building through the normal newWorkCh
// path after the pipeline exits or aborts. A short delay lets the latest head
// become visible first, so the retry builds on the correct parent instead of
// recursively calling commitWork from inside the speculative-work handler.
// handleSpeculativeWork runs a pipelined speculative-work request and, when
// shouldRetry=true, requeues a normal commitWork via the newWorkCh path.
// Extracted from mainLoop so the main dispatch select stays compact.
// Requeueing instead of recursing avoids building on a stale parent and is
// deliberately skipped when commitSpeculativeWork fell back to sequential
// (fallbackToSequential already sealed block N via taskCh, and retrying would
// loop-restart Seal() with fresh timestamps).
func (w *worker) handleSpeculativeWork(req *speculativeWorkReq) {
	shouldRetry, abortRecovery := w.commitSpeculativeWork(req)
	if !shouldRetry {
		return
	}
	if abortRecovery {
		w.nextCommitAbortRecovery.Store(true)
	}
	w.schedulePipelineRetry()
}

func (w *worker) schedulePipelineRetry() {
	go func() {
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-w.exitCh:
			return
		}

		current := w.chain.CurrentBlock()
		if current == nil {
			return
		}

		target := current.Number.Uint64() + 1
		if w.pendingWorkBlock.Load() >= target {
			return
		}
		w.pendingWorkBlock.Store(target)

		select {
		case w.newWorkCh <- &newWorkReq{timestamp: time.Now().Unix()}:
		case <-w.exitCh:
		}
	}()
}

// mainLoop is responsible for generating and submitting sealing work based on
// the received event. It can support two modes: automatically generate task and
// submit it or return task according to given parameters for various proposes.
// nolint:gocognit, contextcheck
func (w *worker) mainLoop() {
	defer w.wg.Done()
	defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	slowTxWindowTicker := time.NewTicker(slowTxWindowPeriod)
	defer slowTxWindowTicker.Stop()
	defer func() {
		w.currentMu.Lock()
		if w.current != nil {
			w.current.discard()
		}
		w.currentMu.Unlock()
	}()

	for {
		select {
		case req := <-w.newWorkCh:
			// When DisablePendingBlock is set and the worker is not actively producing
			// blocks (non-validator), skip commitWork entirely — its only purpose in
			// that case is to maintain the pending block snapshot for RPC.
			if w.config.DisablePendingBlock && !w.IsRunning() {
				w.pendingWorkBlock.Store(0)
				continue
			}

			abortRecovery := w.nextCommitAbortRecovery.Swap(false)
			//nolint:contextcheck
			w.commitWork(req.interrupt, req.noempty, req.timestamp, abortRecovery)
		case req := <-w.speculativeWorkCh:
			w.handleSpeculativeWork(req)

		case req := <-w.getWorkCh:
			req.result <- w.generateWork(req.params, false)

		case ev := <-w.txsCh:
			// Apply transactions to the pending state if we're not sealing
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current sealing block. These transactions will
			// be automatically eliminated.
			// nolint : nestif
			if !w.IsRunning() && !w.config.DisablePendingBlock && w.current != nil {
				// If block is already full, abort
				if gp := w.current.gasPool; gp != nil && gp.Gas() < params.TxGas {
					continue
				}
				// If we don't have time to execute (i.e. we're past header timestamp), abort
				delay := time.Until(time.Unix(int64(w.current.header.Time), 0))
				if delay <= 0 {
					continue
				}
				txs := make(map[common.Address][]*txpool.LazyTransaction, len(ev.Txs))
				for _, tx := range ev.Txs {
					acc, _ := types.Sender(w.current.signer, tx)
					txs[acc] = append(txs[acc], &txpool.LazyTransaction{
						Pool:      w.eth.TxPool(), // We don't know where this came from, yolo resolve from everywhere
						Hash:      tx.Hash(),
						Tx:        nil, // Do *not* set this! We need to resolve it later to pull blobs in
						Time:      tx.Time(),
						GasFeeCap: uint256.MustFromBig(tx.GasFeeCap()),
						GasTipCap: uint256.MustFromBig(tx.GasTipCap()),
						Gas:       tx.Gas(),
						BlobGas:   tx.BlobGas(),
					})
				}

				stopFn := func() {}
				if w.interruptCommitFlag {
					// Production-side pipelining is disabled, so the interrupt
					// timer should use the regular block-time boundary. If the
					// production pipeline is re-enabled, this was previously
					// wired to the miner pipeline enable flag.
					timeoutInterrupt, timeoutFlagSetAt := w.interruptStateForEnv(w.current)
					stopFn = createInterruptTimer(
						w.current.header.Number.Uint64(),
						w.current.header.GetActualTime(),
						timeoutInterrupt,
						timeoutFlagSetAt,
						w.isPipelineEligible(w.current.header.Number.Uint64()),
					)
				}

				timeoutInterrupt, _ := w.interruptStateForEnv(w.current)
				plainTxs := newTransactionsByPriceAndNonce(w.current.signer, txs, w.current.header.BaseFee, timeoutInterrupt) // Mixed bag of everrything, yolo
				blobTxs := newTransactionsByPriceAndNonce(w.current.signer, nil, w.current.header.BaseFee, timeoutInterrupt)  // Empty bag, don't bother optimising

				tcount := w.current.tcount

				// This branch only refreshes the RPC pending-block preview (guarded
				// by !w.IsRunning() above); it never seals a consensus block —
				// production goes through fillTransactions, which builds a fresh env
				// and a single walk. A fresh walk here (quota reset) is therefore
				// fine: at worst the pending preview over-classifies reserved gas,
				// with no consensus effect.
				w.commitTransactions(w.current, plainTxs, blobTxs, nil, nil, registryreader.NewReservedWalk(w.current.evm.Context.ReservedSnapshot))
				stopFn()

				// Only update the snapshot if any new transactons were added
				// to the pending block
				if tcount != w.current.tcount {
					w.updateSnapshot(w.current)
				}
			} else {
				// Special case, if the consensus engine is 0 period clique(dev mode),
				// submit sealing work here since all empty submission will be rejected
				// by clique. Of course the advance sealing(empty submission) is disabled.
				if w.chainConfig.Clique != nil && w.chainConfig.Clique.Period == 0 {
					w.commitWork(nil, true, time.Now().Unix(), false)
				}
			}

			w.newTxs.Add(int32(len(ev.Txs)))

		case tickAt := <-slowTxWindowTicker.C:
			if w.IsRunning() {
				w.flushSlowTxWindow(tickAt)
			} else {
				// Avoid carrying stale data across non-producer windows.
				w.slowTxTracker.Reset()
			}

		// System stopped
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		case <-w.chainHeadSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	defer w.wg.Done()

	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// pendingTasks cleanup for stop-branch exits is handled by the
	// SealWithStopHook onStopExit callback below — doing it here would
	// race with the success branch and drop validly-sealed blocks.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}

	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()

			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}

			w.pendingMu.Lock()
			w.pendingTasks[sealHash] = task
			w.pendingMu.Unlock()

			// Cleanup runs only on stop-branch exits; success deliveries
			// remain available for resultLoop.
			sealHashCapture := sealHash
			onStopExit := func() {
				if w.deletePendingTask(sealHashCapture) {
					log.Warn("Cleaned leaked pendingTasks entry on Seal stop-exit", "sealhash", sealHashCapture)
				}
			}
			var sealErr error
			if borEngine, ok := w.engine.(*bor.Bor); ok {
				sealErr = borEngine.SealWithStopHook(w.chain, task.block, task.state.Witness(), w.resultCh, stopCh, onStopExit)
			} else {
				sealErr = w.engine.Seal(w.chain, task.block, task.state.Witness(), w.resultCh, stopCh)
			}
			if err := sealErr; err != nil {
				switch err.(type) {
				case *bor.UnauthorizedSignerError:
					log.Debug("Block sealing skipped (not in validator set)", "err", err)
				default:
					log.Warn("Block sealing failed", "err", err)
				}
				w.pendingMu.Lock()
				delete(w.pendingTasks, sealHash)
				w.pendingMu.Unlock()
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	defer w.wg.Done()

	for {
		select {
		case newSealedBlockEvent := <-w.resultCh:

			// Short circuit when receiving empty result.
			if newSealedBlockEvent == nil {
				continue
			}
			block := newSealedBlockEvent.Block
			witness := newSealedBlockEvent.Witness
			if block == nil {
				continue
			}

			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}

			// Skip if the sealed block is behind current head (stale block from before reorg)
			currentBlock := w.chain.CurrentBlock()
			if currentBlock != nil && block.NumberU64() <= currentBlock.Number.Uint64() {
				log.Info("Skipping stale sealed block", "sealed", block.NumberU64(), "current", currentBlock.Number.Uint64())
				continue
			}

			oldBlock := w.chain.GetBlockByNumber(block.NumberU64())
			if oldBlock != nil {
				oldBlockAuthor, _ := w.chain.Engine().Author(oldBlock.Header())
				newBlockAuthor, _ := w.chain.Engine().Author(block.Header())

				if oldBlockAuthor == newBlockAuthor {
					log.Info("same block ", "height", block.NumberU64())
					continue
				}
			}

			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)

			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()

			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			// Different block could share same sealhash, deep copy here to prevent write-write conflict.
			var (
				receipts = make([]*types.Receipt, len(task.receipts))
				logs     []*types.Log
				err      error
			)

			for i, taskReceipt := range task.receipts {
				receipt := new(types.Receipt)
				receipts[i] = receipt
				*receipt = *taskReceipt

				// add block location fields
				receipt.BlockHash = hash
				receipt.BlockNumber = block.Number()
				receipt.TransactionIndex = uint(i)

				// Update the block hash in all logs since it is now available and not when the
				// receipt/log of individual transactions were created.
				receipt.Logs = make([]*types.Log, len(taskReceipt.Logs))

				for i, taskLog := range taskReceipt.Logs {
					log := new(types.Log)
					receipt.Logs[i] = log
					*log = *taskLog
					log.BlockHash = hash
				}

				logs = append(logs, receipt.Logs...)
			}

			if witness != nil {
				witness.SetHeader(block.Header())
			}

			emitExecutionMetrics(task)

			writeStart := time.Now()
			_, err = w.writeTaskBlock(task, block, receipts, logs)
			writeElapsed := time.Since(writeStart)
			writeBlockAndSetHeadTimer.Update(writeElapsed)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				w.pendingMu.Lock()
				delete(w.pendingTasks, sealhash)
				w.pendingMu.Unlock()
				continue
			}

			emitCommitMetrics(task, block, writeElapsed)

			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(task.createdAt)))

			announceTaskBlock(w.mux, task, block, witness)

			sealedBlocksCounter.Inc(1)

			if block.Transactions().Len() == 0 {
				sealedEmptyBlocksCounter.Inc(1)
			}

			// Clear all pending tasks for blocks at or below the sealed block number.
			// These tasks are now obsolete since the chain has progressed past them.
			w.clearPending(block.NumberU64())

		case <-w.exitCh:
			return
		}
	}
}

// emitExecutionMetrics reports the task's pre-write statedb timers + execution
// time. Matches the import path which emits read/update/hash/execution/bor
// metrics before writeBlockAndSetHead so observations aren't lost on write
// failure. No-op when metrics are disabled.
func emitExecutionMetrics(task *task) {
	if !metrics.Enabled() {
		return
	}
	workerAccountReadTimer.Update(task.state.AccountReads)
	workerStorageReadTimer.Update(task.state.StorageReads)
	workerSnapshotAccountReadTimer.Update(task.state.SnapshotAccountReads)
	workerSnapshotStorageReadTimer.Update(task.state.SnapshotStorageReads)
	workerAccountUpdateTimer.Update(task.state.AccountUpdates)
	workerStorageUpdateTimer.Update(task.state.StorageUpdates)
	workerAccountHashTimer.Update(task.state.AccountHashes)
	workerStorageHashTimer.Update(task.state.StorageHashes)
	workerBorConsensusTimer.Update(task.state.BorConsensusTime)
	trieRead := task.state.SnapshotAccountReads + task.state.AccountReads +
		task.state.SnapshotStorageReads + task.state.StorageReads
	// productionElapsed covers fillTx + FinalizeAndAssemble; subtract trie reads,
	// Bor consensus, and IntermediateRoot time to isolate pure EVM execution.
	// Mirrors blockchain.go's ptime = productionElapsed - trieRead (clamped
	// to zero for measurement jitter).
	execTime := task.productionElapsed - trieRead - task.state.BorConsensusTime - task.intermediateRootTime
	if execTime < 0 {
		execTime = 0
	}
	workerBlockExecutionTimer.Update(execTime)
}

// writeTaskBlock commits the sealed block + state to disk. Pipelined tasks go
// through WriteBlockAndSetHeadPipelined so the SRC goroutine's earlier
// CommitWithUpdate isn't duplicated; normal tasks go through the standard
// path. Returns the write status for parity with the original inline call.
func (w *worker) writeTaskBlock(task *task, block *types.Block, receipts []*types.Receipt, logs []*types.Log) (core.WriteStatus, error) {
	if task.pipelined {
		return w.chain.WriteBlockAndSetHeadPipelined(block, receipts, logs, task.state, task.reservedTxIndexes, task.reservedClientUsage, true, task.witnessBytes)
	}
	return w.chain.WriteBlockAndSetHead(block, receipts, logs, task.state, task.reservedTxIndexes, task.reservedClientUsage, true)
}

// emitCommitMetrics reports the task's post-write statedb timers, mgas/s,
// and per-block throughput histograms. Must run only after a successful
// write — the commit fields are populated by CommitWithUpdate inside
// WriteBlockAndSetHead. No-op when metrics are disabled.
func emitCommitMetrics(task *task, block *types.Block, writeElapsed time.Duration) {
	if !metrics.Enabled() {
		return
	}
	workerAccountCommitTimer.Update(task.state.AccountCommits)
	workerStorageCommitTimer.Update(task.state.StorageCommits)
	workerSnapshotCommitTimer.Update(task.state.SnapshotCommits)
	workerTriedbCommitTimer.Update(task.state.TrieDBCommits)
	workerWitnessCollectionTimer.Update(task.state.WitnessCollection)
	// MGas/s: denominator is production + write (matches blockchain.go's
	// elapsed, measured after writeBlockAndSetHead returns). Duration stores
	// milli-gas/ns = MGas/s.
	if total := task.productionElapsed + writeElapsed; total > 0 {
		workerMgaspsTimer.Update(time.Duration(float64(block.GasUsed()) * 1000 / float64(total)))
	}
	workerGasUsedPerBlockHistogram.Update(int64(block.GasUsed()))
	workerTxsPerBlockHistogram.Update(int64(block.Transactions().Len()))
}

// announceTaskBlock broadcasts the sealed block to peers and updates the
// build-to-announce + PIP-66 earliness + committed metrics for pipelined
// tasks sealed via taskCh (last-of-pipeline, eligibility-fail, or fallback).
// inlineSealAndBroadcast emits the same signals on the inline path.
func announceTaskBlock(mux *event.TypeMux, task *task, block *types.Block, witness *stateless.Witness) {
	announceAt := time.Now()
	if !task.productionStart.IsZero() {
		workerBuildToAnnounceTimer.UpdateSince(task.productionStart)
	}
	if task.pipelined {
		earlyMs := block.Header().GetActualTime().Sub(announceAt).Milliseconds()
		pipelineAnnounceEarlinessMs.Update(earlyMs)
		pipelineSpeculativeCommittedCounter.Inc(1)
	}
	mux.Post(core.NewMinedBlockEvent{Block: block, Witness: witness, SealedAt: announceAt})
}

// resolveStateFor returns the caller-supplied statedb if any (from commitWork's
// dual-reader path), otherwise opens one at the parent's root. Kept as a helper
// so makeEnv itself fits within the function-size budget.
func (w *worker) resolveStateFor(header *types.Header, genParams *generateParams) (*state.StateDB, error) {
	if genParams.statedb != nil {
		return genParams.statedb, nil
	}
	parent := w.chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return nil, fmt.Errorf("parent block not found")
	}
	// Overlay-free for the same reason as commitWork: this state seeds a
	// block whose root will be sealed into a header.
	return w.chain.SealingStateAt(parent.Root)
}

// makeEnv creates a new environment for the sealing block.
func (w *worker) makeEnv(header *types.Header, coinbase common.Address, witness bool, genParams *generateParams) (*environment, error) {
	state, err := w.resolveStateFor(header, genParams)
	if err != nil {
		return nil, err
	}

	if witness {
		bundle, err := stateless.NewWitness(header, w.chain)
		if err != nil {
			return nil, err
		}
		state.StartPrefetcher("miner", bundle, nil)
	} else {
		// todo: @anshalshukla - check if witness is required
		state.StartPrefetcher("miner", nil, nil)
	}

	// Build the block context once and attach the parent-state reserved snapshot
	// so the producer classifies reserved senders the same way verifiers do. The
	// reserved set is filled incrementally as reserved batches commit (see
	// commitTransactions), so overflow diverted to the normal pass pays normal
	// fees. A verifier rederives the identical set from the ordered body via
	// registryreader.ClassifyReserved.
	blockCtx := core.NewEVMBlockContext(header, w.chain, &coinbase)
	reservedSnapshot, err := w.resolveReservedSnapshot(state, header)
	if err != nil {
		return nil, err
	}
	blockCtx.ReservedSnapshot = reservedSnapshot
	blockCtx.ReservedTxs = make(map[registryreader.ReservedKey]struct{})

	// Note the passed coinbase may be different with header.Coinbase.
	env := &environment{
		signer:             types.MakeSigner(w.chainConfig, header.Number, header.Time),
		state:              state,
		size:               uint64(header.Size()),
		coinbase:           coinbase,
		buildInterrupt:     newBuildInterruptState(),
		header:             header,
		witness:            state.Witness(),
		evm:                vm.NewEVM(blockCtx, state, w.chainConfig, w.vmConfig()),
		prefetchReader:     genParams.prefetchReader,
		processReader:      genParams.processReader,
		prefetchedTxHashes: genParams.prefetchedTxHashes,
	}
	timeoutInterrupt, _ := w.interruptStateForEnv(env)
	env.evm.SetInterrupt(timeoutInterrupt)
	env.stateSyncReserve = stateSyncReserveFor(w.chainConfig, header.Number)

	// Keep track of transactions which return errors so they can be removed
	env.tcount = 0

	return env, nil
}

// updateSnapshot updates pending snapshot block, receipts and state.
func (w *worker) updateSnapshot(env *environment) {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.snapshotBlock = types.NewBlock(
		env.header,
		&types.Body{
			Transactions: env.txs,
		},
		env.receipts,
		trie.NewStackTrie(nil),
	)

	w.snapshotReceipts = copyReceipts(env.receipts)
	w.snapshotState = env.state.Copy()
}

func (w *worker) commitTransaction(env *environment, tx *types.Transaction) ([]*types.Log, error) {
	var (
		snap = env.state.Snapshot()
		gp   = env.gasPool.Gas()
	)

	receipt, err := core.ApplyTransaction(env.evm, env.gasPool, env.state, env.header, tx, &env.header.GasUsed)
	if err != nil {
		env.state.RevertToSnapshot(snap)
		env.gasPool.SetGas(gp)

		return nil, err
	}
	env.txs = append(env.txs, tx)
	env.receipts = append(env.receipts, receipt)
	env.tcount++
	env.size += tx.Size()

	return receipt.Logs, nil
}

// markReservedTentative classifies tx against the reserved per-client quota walk
// before execution, so state_transition sees the fee-free decision via the block
// context set. It only Peeks — the walk advances on a successful commit — so a
// skipped tx never consumes quota; a verifier rederives the identical set by
// running the same walk over the final block (registryreader.ClassifyReserved),
// independent of which batch placed the tx.
func markReservedTentative(env *environment, walk *registryreader.ReservedWalk, from common.Address, tx *types.Transaction) (bool, registryreader.ReservedKey) {
	reserved := walk.Peek(from, tx.Gas())
	key := registryreader.ReservedKey{From: from, Nonce: tx.Nonce()}
	if reserved && env.evm.Context.ReservedTxs != nil {
		env.evm.Context.ReservedTxs[key] = struct{}{}
	}
	return reserved, key
}

// commitReserved advances the reserved walk for a just-committed tx. Reserved
// txs count toward the header's ReservedGasUsed by actual gas used; the verifier
// recomputes the same sum from the block (validateReservedFields).
func commitReserved(env *environment, walk *registryreader.ReservedWalk, from common.Address, tx *types.Transaction, reserved bool) {
	walk.Commit(from, tx.Gas(), reserved)
	if !reserved {
		return
	}
	if n := len(env.receipts); n > 0 {
		env.reservedGasUsed += env.receipts[n-1].GasUsed
	}
}

// rollbackReservedIfSkipped undoes a tx's tentative reserved mark when it was
// skipped (err != nil): a skipped tx is not in the block and its walk was never
// advanced, so the producer's set stays equal to ClassifyReserved(block).
func rollbackReservedIfSkipped(env *environment, err error, reserved bool, key registryreader.ReservedKey) {
	if err != nil && reserved && env.evm.Context.ReservedTxs != nil {
		delete(env.evm.Context.ReservedTxs, key)
	}
}

func (w *worker) commitTransactions(env *environment, plainTxs, blobTxs *transactionsByPriceAndNonce, interrupt *atomic.Int32, builderGasFreedCh chan<- uint64, reservedWalk *registryreader.ReservedWalk) error {
	defer func(t0 time.Time) {
		commitTransactionsTimer.Update(time.Since(t0))
	}(time.Now())

	gasLimit := env.header.GasLimit
	if env.gasPool == nil {
		env.gasPool = new(core.GasPool).AddGas(gasLimit)
	}

	var coalescedLogs []*types.Log

	var lastTxHash common.Hash

	var (
		lastCommitStart        time.Time      // start of the most recent commitTransaction call
		lastTxIndex            int            // index of the last attempted tx (for interrupt context)
		lastTxSender           common.Address // sender of the last attempted tx (for interrupt context)
		flagToTxInterruptDelay time.Duration  // delay from setting interrupt flag to tx interruption
		hasTxInterruptDelay    bool
	)
	lastTxIndex = -1

mainloop:
	for {
		// Check interruption signal and abort building if it's fired.
		if interrupt != nil {
			if signal := interrupt.Load(); signal != commitInterruptNone {
				return signalToErr(signal)
			}
		}

		// Check for the flag to interrupt block building on timeout.
		// The worker-global interrupt is only a manual/test override; the real
		// timeout state is owned by this build attempt.
		if w.interruptBlockBuilding.Load() || (env.buildInterrupt != nil && env.buildInterrupt.timedOut.Load()) {
			txCommitInterruptCounter.Inc(1)
			logCtx := []interface{}{
				"number", env.header.Number.Uint64(),
				"headerTime", common.PrettyTime(time.Unix(int64(env.header.Time), 0)),
			}
			flagSetAt := w.interruptFlagSetAt.Load()
			if flagSetAt == 0 && env.buildInterrupt != nil {
				flagSetAt = env.buildInterrupt.flagSetAt.Load()
			}
			if flagSetAt > 0 {
				flagSetTime := time.Unix(0, flagSetAt)
				logCtx = append(logCtx, "flagSetAt", common.PrettyTime(flagSetTime))
				logCtx = append(logCtx, "flagToAbortDelay", common.PrettyDuration(time.Since(flagSetTime)))
			}
			if hasTxInterruptDelay {
				logCtx = append(logCtx, "flagToTxInterruptDelay", common.PrettyDuration(flagToTxInterruptDelay))
			}
			if !lastCommitStart.IsZero() {
				logCtx = append(logCtx, "txHash", lastTxHash.Hex())
				logCtx = append(logCtx, "txIndex", lastTxIndex)
				logCtx = append(logCtx, "sender", lastTxSender)
				logCtx = append(logCtx, "txElapsed", common.PrettyDuration(time.Since(lastCommitStart)))
			}

			if w.IsRunning() {
				log.Info("Block building interrupted due to timeout, aborting new transaction commits", logCtx...)
			} else {
				log.Debug("Block building interrupted due to timeout, aborting new transaction commits", logCtx...)
			}

			break mainloop
		}

		// If we don't have enough gas for any further transactions then we're done.
		if env.gasPool.Gas() < params.TxGas {
			log.Trace("Not enough gas for further transactions", "have", env.gasPool, "want", params.TxGas)
			break
		}
		// If we don't have enough blob space for any further blob transactions,
		// skip that list altogether
		if !blobTxs.Empty() && env.blobs >= eip4844.MaxBlobsPerBlock(w.chainConfig, env.header.Time) {
			log.Trace("Not enough blob space for further blob transactions")
			blobTxs.Clear()
			// Fall though to pick up any plain txs
		}
		// Retrieve the next transaction and abort if all done.

		var (
			ltx *txpool.LazyTransaction
			txs *transactionsByPriceAndNonce
		)
		pltx, ptip := plainTxs.Peek()
		bltx, btip := blobTxs.Peek()

		switch {
		case pltx == nil:
			txs, ltx = blobTxs, bltx
		case bltx == nil:
			txs, ltx = plainTxs, pltx
		default:
			if ptip.Lt(btip) {
				txs, ltx = blobTxs, bltx
			} else {
				txs, ltx = plainTxs, pltx
			}
		}
		if ltx == nil {
			break
		}
		// If we don't have enough space for the next transaction, skip the account.
		if env.gasPool.Gas() < ltx.Gas {
			log.Trace("Not enough gas left for transaction", "hash", ltx.Hash, "left", env.gasPool.Gas(), "needed", ltx.Gas)
			txs.Pop()
			continue
		}

		// Transaction seems to fit, pull it up from the pool
		tx := ltx.Resolve()
		if tx == nil {
			log.Trace("Ignoring evicted transaction", "hash", ltx.Hash)
			txs.Pop()
			continue
		}

		// Make sure all transactions after osaka have cell proofs
		if w.chainConfig.IsOsaka(env.header.Number) {
			if sidecar := tx.BlobTxSidecar(); sidecar != nil {
				if sidecar.Version == 0 {
					log.Info("Including blob tx with v0 sidecar, recomputing proofs", "hash", ltx.Hash)
					sidecar.Proofs = make([]kzg4844.Proof, 0, len(sidecar.Blobs)*kzg4844.CellProofsPerBlob)
					for _, blob := range sidecar.Blobs {
						cellProofs, err := kzg4844.ComputeCellProofs(&blob)
						if err != nil {
							panic(err)
						}
						sidecar.Proofs = append(sidecar.Proofs, cellProofs...)
					}
				}
			}
		}
		// if inclusion of the transaction would put the block size over the
		// maximum we allow, don't add any more txs to the payload.
		if !env.txFitsSize(tx) {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance in the transaction pool.
		from, _ := types.Sender(env.signer, tx)

		// not prioritising conditional transaction, yet.
		//nolint:nestif
		if options := tx.GetOptions(); options != nil {
			if err := env.header.ValidateBlockNumberOptionsPIP15(options.BlockNumberMin, options.BlockNumberMax); err != nil {
				log.Trace("Dropping conditional transaction", "from", from, "hash", tx.Hash(), "reason", err)
				txs.Pop()

				continue
			}

			if err := env.header.ValidateTimestampOptionsPIP15(options.TimestampMin, options.TimestampMax); err != nil {
				log.Trace("Dropping conditional transaction", "from", from, "hash", tx.Hash(), "reason", err)
				txs.Pop()

				continue
			}

			if err := env.state.ValidateKnownAccounts(options.KnownAccounts); err != nil {
				log.Trace("Dropping conditional transaction", "from", from, "hash", tx.Hash(), "reason", err)
				txs.Pop()

				continue
			}
		}

		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.chainConfig.IsEIP155(env.header.Number) {
			log.Trace("Ignoring replay protected transaction", "hash", ltx.Hash, "eip155", w.chainConfig.EIP155Block)
			txs.Pop()
			continue
		}
		reserved, reservedKey := markReservedTentative(env, reservedWalk, from, tx)

		// Start executing the transaction
		lastCommitStart = time.Now()
		lastTxHash = tx.Hash()
		lastTxIndex = env.tcount
		lastTxSender = from
		env.state.SetTxContext(tx.Hash(), env.tcount)

		// Capture gas pool before execution so we can compute freed gas afterwards.
		gasPoolBefore := env.gasPool.Gas()
		logs, err := w.commitTransaction(env, tx)
		txDuration := time.Since(lastCommitStart)

		// Set mock delay (if any) between transactions for tests
		time.Sleep(time.Duration(w.mockTxDelay) * time.Millisecond)

		switch {
		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "hash", ltx.Hash, "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			commitReserved(env, reservedWalk, from, tx, reserved)
			prefetched := false
			if env.prefetchedTxHashes != nil {
				_, prefetched = env.prefetchedTxHashes.Load(tx.Hash())
			}
			if metrics.Enabled() {
				txApplyDurationTimer.Update(txDuration)
				if prefetched {
					txApplyDurationPrefetchedTimer.Update(txDuration)
				} else {
					txApplyDurationNotPrefetchedTimer.Update(txDuration)
				}
			}
			if w.IsRunning() {
				var gasUsed uint64
				if n := len(env.receipts); n > 0 {
					gasUsed = env.receipts[n-1].GasUsed
				}
				w.slowTxTracker.Add(txTimingEntry{
					hash:       tx.Hash(),
					duration:   txDuration,
					gasUsed:    gasUsed,
					prefetched: prefetched,
				})
			}

			txs.Shift()

			// Report freed gas to the prefetcher so it can predict overflow txs.
			// freed = declared gas limit − actual gas used; non-zero means the block
			// has more capacity than the plan assumed, enabling extra txs to fit.
			if builderGasFreedCh != nil && ltx.Gas > 0 {
				actualUsed := gasPoolBefore - env.gasPool.Gas()
				if freed := ltx.Gas - actualUsed; freed > 0 {
					select {
					case builderGasFreedCh <- freed:
					default:
					}
				}
			}

		case errors.Is(err, vm.ErrInterrupt):
			// Timeout interrupt surfaced from EVM execution for this tx.
			if !hasTxInterruptDelay {
				flagSetAt := w.interruptFlagSetAt.Load()
				if flagSetAt == 0 && env.buildInterrupt != nil {
					flagSetAt = env.buildInterrupt.flagSetAt.Load()
				}
				if flagSetAt > 0 {
					flagToTxInterruptDelay = time.Since(time.Unix(0, flagSetAt))
					hasTxInterruptDelay = true
				}
			}
			log.Debug("Transaction interrupted due to timeout", "hash", ltx.Hash, "err", err)
			txs.Pop()

		default:
			// Transaction is regarded as invalid, drop all consecutive transactions from
			// the same sender because of `nonce-too-high` clause.
			log.Debug("Transaction failed, account skipped", "hash", ltx.Hash, "err", err)
			txs.Pop()
		}

		rollbackReservedIfSkipped(env, err, reserved, reservedKey)
	}

	if !w.IsRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are sealing. The reason is that
		// when we are sealing, the worker will regenerate a sealing block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.
		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}

		w.pendingLogsFeed.Send(cpy)
	}

	return nil
}

// generateParams wraps various of settings for generating sealing task.
type generateParams struct {
	timestamp                 uint64                  // The timestamp for sealing task
	forceTime                 bool                    // Flag whether the given timestamp is immutable or not
	parentHash                common.Hash             // Parent block hash, empty means the latest chain head
	coinbase                  common.Address          // The fee recipient address for including transaction
	abortRecovery             bool                    // Flag that this build is rebuilding a discarded speculative block
	random                    common.Hash             // The randomness generated by beacon chain, empty before the merge
	withdrawals               types.Withdrawals       // List of withdrawals to include in block.
	beaconRoot                *common.Hash            // The beacon root (cancun field).
	noTxs                     bool                    // Flag whether an empty block without any transaction is expected
	statedb                   *state.StateDB          // The statedb to use for block generation
	prefetchReader            state.ReaderWithStats   // The prefetch reader to use for statistics
	processReader             state.ReaderWithStats   // The process reader to use for statistics
	prefetchedTxHashes        *sync.Map               // Map of successfully prefetched transaction hashes
	builderPrefetchedTxHashes *sync.Map               // Subset of prefetchedTxHashes populated only during the builder phase; used to measure builder-phase contribution
	productionStart           time.Time               // Start of full-block building (after optional empty pre-seal); used for productionElapsed
	preBuildDuration          time.Duration           // Duration of pre block build phase
	builderStarted            *atomic.Bool            // Set when block building begins; immediately interrupts the idle Prefetch() call and triggers builder-mode prefetching
	builderPlanCh             chan *types.Transaction // Builder sends each validated tx here before execution; prefetcher reads and warms state concurrently
	builderGasFreedCh         chan uint64             // Builder sends (declared−actual) gas after each successful tx; prefetcher uses it to predict overflow txs
	planWg                    sync.WaitGroup          // Tracks sendPlan goroutines; must reach zero before builderPlanCh is closed
}

// makeHeader creates a new block header for sealing. The caller must hold w.mu
// for reading because the header includes mutable worker configuration.
func (w *worker) makeHeader(genParams *generateParams, waitOnPrepare bool) (*types.Header, common.Address, error) {
	// Find the parent block for sealing task
	parent := w.chain.CurrentBlock()

	if genParams.parentHash != (common.Hash{}) {
		block := w.chain.GetBlockByHash(genParams.parentHash)
		if block == nil {
			return nil, common.Address{}, fmt.Errorf("missing parent")
		}

		parent = block.Header()
	}
	// Sanity check the timestamp correctness, recap the timestamp
	// to parent+1 if the mutation is allowed.
	timestamp := genParams.timestamp
	if parent.Time >= timestamp {
		if genParams.forceTime {
			return nil, common.Address{}, fmt.Errorf("invalid timestamp, parent %d given %d", parent.Time, timestamp)
		}

		timestamp = parent.Time + 1
	}

	newBlockNumber := new(big.Int).Add(parent.Number, common.Big1)
	coinbase := w.resolveCoinbase(newBlockNumber.Uint64(), genParams.coinbase)

	// Calculate desired gas limit (may be dynamically adjusted based on base fee)
	desiredGasLimit := w.calculateDesiredGasLimitLocked(parent)

	// Construct the sealing block header.
	header := &types.Header{
		ParentHash: parent.Hash(),
		Number:     newBlockNumber,
		GasLimit:   core.CalcGasLimit(parent.GasLimit, desiredGasLimit),
		Time:       timestamp,
		Coinbase:   coinbase,
	}
	header.AbortRecovery = genParams.abortRecovery
	// Set the extra field.
	if len(w.extra) != 0 {
		header.Extra = w.extra
	}
	// Set the randomness field from the beacon chain if it's available.
	if genParams.random != (common.Hash{}) {
		header.MixDigest = genParams.random
	}
	// Set baseFee and GasLimit if we are on an EIP-1559 chain
	if w.chainConfig.IsLondon(header.Number) {
		header.BaseFee = eip1559.CalcBaseFee(w.chainConfig, parent)
		if !w.chainConfig.IsLondon(parent.Number) {
			parentGasLimit := parent.GasLimit * w.chainConfig.ElasticityMultiplier()
			header.GasLimit = core.CalcGasLimit(parentGasLimit, desiredGasLimit)
		}
	}

	header.BlobGasUsed = nil
	header.ExcessBlobGas = nil
	header.ParentBeaconRoot = nil

	// Run the consensus preparation with the default or customized consensus engine.
	if err := w.engine.Prepare(w.chain, header, waitOnPrepare); err != nil {
		switch err.(type) {
		case *bor.UnauthorizedSignerError:
			log.Debug("Failed to prepare header for sealing", "err", err)
		default:
			log.Error("Failed to prepare header for sealing", "err", err)
		}

		return nil, common.Address{}, err
	}

	return header, coinbase, nil
}

// prepareWork constructs the sealing task according to the given parameters,
// either based on the last chain head or specified parent. In this function
// the pending transactions are not filled yet, only the empty task returned.
func (w *worker) prepareWork(genParams *generateParams, witness bool) (*environment, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	makeHeaderStart := time.Now()
	header, coinbase, err := w.makeHeader(genParams, true)
	if err != nil {
		return nil, err
	}
	makeHeaderDuration := time.Since(makeHeaderStart)

	// Could potentially happen if starting to mine in an odd state.
	// Note genParams.coinbase can be different with header.Coinbase
	// since clique algorithm can modify the coinbase field in header.
	makeEnvStart := time.Now()
	env, err := w.makeEnv(header, coinbase, witness, genParams)
	if err != nil {
		log.Error("Failed to create sealing context", "err", err)
		return nil, err
	}
	env.makeEnvDuration = time.Since(makeEnvStart)
	env.makeHeaderDuration = makeHeaderDuration
	if header.ParentBeaconRoot != nil {
		context := core.NewEVMBlockContext(header, w.chain, nil)
		vmenv := vm.NewEVM(context, env.state, w.chainConfig, w.vmConfig())
		core.ProcessBeaconBlockRoot(*header.ParentBeaconRoot, vmenv)
	}
	if w.chainConfig.IsPrague(header.Number) {
		// EIP-2935
		context := core.NewEVMBlockContext(header, w.chain, nil)
		vmenv := vm.NewEVM(context, env.state, w.chainConfig, w.vmConfig())
		core.ProcessParentBlockHash(header.ParentHash, vmenv)
	}
	return env, nil
}

// buildDefaultFilter creates a pending transaction filter based on chain configuration
// and current tip/base fee settings.
func (w *worker) buildDefaultFilter(BaseFee *big.Int, Number *big.Int) txpool.PendingFilter {
	w.mu.RLock()
	tip := w.tip
	w.mu.RUnlock()

	// Retrieve the pending transactions pre-filtered by the 1559/4844 dynamic fees
	filter := txpool.PendingFilter{
		MinTip: uint256.MustFromBig(tip.ToBig()),
	}

	if BaseFee != nil {
		filter.BaseFee = uint256.MustFromBig(BaseFee)
	}

	isOsaka := w.chainConfig.IsOsaka(Number)
	isMadhugiri := w.chainConfig.Bor != nil && w.chainConfig.Bor.IsMadhugiri(Number)
	// Verify tx gas limit does not exceed EIP-7825 cap.
	if isOsaka || isMadhugiri {
		filter.GasLimitCap = params.MaxTxGas
	}

	return filter
}

// buildTxPlan greedily scans h (which is consumed) and returns the ordered list of
// transactions the builder is predicted to include, using declared gas limits as the
// budget. Already-prefetched transactions are excluded from the list but still counted
// against the gas budget so the estimate stays accurate.
//
// Callers that need the original heap unmodified must pass a clone: h.clone().
//
// The plan is a conservative lower bound: freed gas (actual < declared) means some
// bonus txs may fit that are absent from the plan; those are covered by the per-tx
// channel sends inside commitTransactions.
func buildTxPlan(h *transactionsByPriceAndNonce, gasLimit uint64, prefetchedHashes *sync.Map) []*types.Transaction {
	var plan []*types.Transaction
	remaining := gasLimit
	for {
		ltx, _ := h.Peek()
		if ltx == nil {
			break
		}
		if ltx.Gas > remaining {
			h.Pop() // Too large for remaining space; abandon account (mirrors commitTransactions)
			continue
		}
		// Already warmed during idle prefetch — count against gas budget but skip the send.
		// Deliberate: the tx is still bound for the block, so its gas is consumed here.
		if prefetchedHashes != nil {
			if _, done := prefetchedHashes.Load(ltx.Hash); done {
				remaining -= ltx.Gas
				h.Shift()
				continue
			}
		}
		tx := ltx.Resolve()
		if tx == nil {
			// Resolve failed (tx evicted from the pool between listing and here);
			// don't consume budget for a tx that won't make the block.
			h.Pop()
			continue
		}
		remaining -= ltx.Gas
		plan = append(plan, tx)
		h.Shift()
	}
	return plan
}

// sendPlan forwards the residual (not-yet-prefetched) transactions to the prefetcher
// plan channel before execution begins. The heap clone is made synchronously — it
// must happen before commitTransactions consumes the heap. The scan and channel sends
// run in a goroutine so the builder's critical path is not blocked.
//
// Already-prefetched txs are excluded from the send but still counted in the gas
// budget so the estimate stays accurate. Bonus txs that fit due to freed gas are
// covered by the prefetcher's own overflow heap driven by builderGasFreedCh.
func sendPlan(builderPlanCh chan<- *types.Transaction, genParams *generateParams, plainTxs *transactionsByPriceAndNonce, gasLimit uint64) {
	if builderPlanCh == nil || genParams == nil || plainTxs == nil {
		return
	}
	// Clone is O(N) pointer copies — done synchronously before the heap is consumed.
	clone := plainTxs.clone()
	prefetchedHashes := genParams.prefetchedTxHashes
	genParams.planWg.Add(1)
	go func() {
		defer genParams.planWg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Error("sendPlan goroutine panicked", "err", r, "stack", string(debug.Stack()))
				prefetchPanicMeter.Mark(1)
			}
		}()
		for _, tx := range buildTxPlan(clone, gasLimit, prefetchedHashes) {
			select {
			case builderPlanCh <- tx:
			default:
			}
		}
	}()
}

// scanOverflow greedily pops transactions from the heap that fit within the freed-gas
// budget, returning new txs to prefetch and the remaining budget. Already-prefetched
// txs are skipped entirely (not counted against the budget) because the freed gas
// represents extra block capacity beyond the existing plan — plan txs' gas is already
// committed to the main gas pool and should not be double-counted here.
func scanOverflow(
	h *transactionsByPriceAndNonce,
	budget uint64,
	prefetchedHashes *sync.Map,
	inFlightHashes map[common.Hash]struct{},
) ([]*types.Transaction, uint64) {
	var bonus []*types.Transaction
	remaining := budget
	for {
		ltx, _ := h.Peek()
		if ltx == nil {
			break
		}
		// Skip already-prefetched (planned) txs without consuming freed budget:
		// their gas is already accounted for in the main block gas pool.
		if prefetchedHashes != nil {
			if _, done := prefetchedHashes.Load(ltx.Hash); done {
				h.Shift()
				continue
			}
		}
		// Skip txs still in-flight from an earlier plan-batch forward. They
		// aren't in prefetchedHashes yet (onSuccess hasn't fired), but a worker
		// is already executing them — emitting again just burns a second worker
		// on the same tx.
		if _, inflight := inFlightHashes[ltx.Hash]; inflight {
			h.Shift()
			continue
		}
		if ltx.Gas > remaining {
			// Don't pop: extendedBudget accumulates across iterations, so an
			// account too large for this window may fit in a later one. Popping
			// would permanently evict price-leading accounts that the builder
			// is most likely to include.
			break
		}
		tx := ltx.Resolve()
		if tx == nil {
			h.Pop()
			continue
		}
		remaining -= ltx.Gas
		bonus = append(bonus, tx)
		h.Shift()
	}
	return bonus, remaining
}

// fillTransactions retrieves the pending transactions from the txpool,
// sequences the plain ones into ordered groups (priority, reserved
// per-client, then normal) via sequenceTxs, and commits each group in turn.
// Blob transactions are never reserved-classified; they commit after the
// plain groups, operator-priority senders first, so the reserved walk's
// per-client accounting over the plain body is unaffected.
//
// Zero-fee reserved transactions flow through this path end-to-end: the txpool
// waives the minimum-tip floor for reserved senders and execution waives the
// base-fee floor, so the reserved pass commits them fee-free.
func (w *worker) fillTransactions(interrupt *atomic.Int32, env *environment, genParams *generateParams) error {
	if w.interruptBlockBuilding.Load() {
		return nil
	}

	pendingStart := time.Now()

	filter := w.buildDefaultFilter(env.header.BaseFee, env.header.Number)
	timeoutInterrupt, _ := w.interruptStateForEnv(env)

	filter.BlobTxs = false
	pendingPlainTxs := w.eth.TxPool().Pending(filter, timeoutInterrupt)

	filter.BlobTxs = true
	if w.chainConfig.IsOsaka(env.header.Number) {
		filter.BlobVersion = types.BlobSidecarVersion1
	} else {
		filter.BlobVersion = types.BlobSidecarVersion0
	}
	pendingBlobTxs := w.eth.TxPool().Pending(filter, timeoutInterrupt)

	env.pendingDuration = time.Since(pendingStart)
	pendingTimer.Update(env.pendingDuration)

	// One registry snapshot serves the whole build — the same parent-state
	// snapshot makeEnv pinned for execution — so client order, quotas and
	// membership stay consistent across batches and match what the executor
	// classifies. A future multi-pass (mini-block) builder inherits this for
	// free: the snapshot lives on the env, outside any pass loop.
	snap := w.sequencingSnapshot(env)

	// Order plain transactions based on priority into 3 buckets - priority,
	// reserved and normal transactions. sequenceTxs consumes w.prio itself,
	// excluding registered senders for produce/verify parity.
	txBatches := w.sequenceTxs(env, snap, pendingPlainTxs)

	// Blob transactions keep the operator-priority split of the plain path:
	// priority senders' blobs commit before the rest.
	w.mu.RLock()
	prio := w.prio
	w.mu.RUnlock()

	prioBlobTxs := make(map[common.Address][]*txpool.LazyTransaction)
	for _, account := range prio {
		if txs := pendingBlobTxs[account]; len(txs) > 0 {
			delete(pendingBlobTxs, account)
			prioBlobTxs[account] = txs
		}
	}

	// One reserved-classification walk for the whole build, advanced tx-by-tx as
	// each batch commits, so per-client quota accounting is continuous across
	// batches and matches ClassifyReserved(final block) on the verify side.
	reservedWalk := registryreader.NewReservedWalk(snap)

	// Shared channels used during builder mode. Both are nil when there is no prefetcher.
	var builderPlanCh chan<- *types.Transaction
	var builderGasFreedCh chan<- uint64
	if genParams != nil && genParams.builderPlanCh != nil {
		builderPlanCh = genParams.builderPlanCh
		if genParams.builderGasFreedCh != nil {
			builderGasFreedCh = genParams.builderGasFreedCh
		}
	}

	emptyBlobTxs := newTransactionsByPriceAndNonce(env.signer, nil, env.header.BaseFee, timeoutInterrupt)

	var fillErr error

	for _, txs := range txBatches {
		sendPlan(builderPlanCh, genParams, txs, remainingGas(env))
		if fillErr = w.commitTransactions(env, txs, emptyBlobTxs, interrupt, builderGasFreedCh, reservedWalk); fillErr != nil {
			break
		}
		// The block-building time budget makes commitTransactions stop early
		// with a nil error. Stop handing it further batches in that case —
		// they would immediately no-op.
		if timeoutInterrupt.Load() {
			break
		}
	}

	if fillErr == nil && !timeoutInterrupt.Load() {
		emptyPlainTxs := newTransactionsByPriceAndNonce(env.signer, nil, env.header.BaseFee, timeoutInterrupt)

		for _, blobs := range []map[common.Address][]*txpool.LazyTransaction{prioBlobTxs, pendingBlobTxs} {
			if len(blobs) == 0 {
				continue
			}

			blobHeap := newTransactionsByPriceAndNonce(env.signer, blobs, env.header.BaseFee, timeoutInterrupt)
			if fillErr = w.commitTransactions(env, emptyPlainTxs, blobHeap, interrupt, builderGasFreedCh, reservedWalk); fillErr != nil {
				break
			}

			if timeoutInterrupt.Load() {
				break
			}
		}
	}

	// Record reserved gas even when a commit pass was interrupted: callers
	// seal the partial block, and its header must account for the reserved
	// transactions that did commit.
	if err := w.writeReservedFields(env); err != nil {
		return err
	}

	return fillErr
}

// remainingGas returns the block gas still available for the next
// commitTransactions pass. Before the first pass env.gasPool is nil, so it
// falls back to the full header limit.
func remainingGas(env *environment) uint64 {
	if env.gasPool == nil {
		return env.header.GasLimit
	}
	return env.gasPool.Gas()
}

// sequencingSnapshot returns the reserved-registry snapshot the sequencing
// pass classifies against: the one makeEnv pinned on the EVM block context,
// i.e. the same snapshot execution classifies with, so the sequencer and the
// executor can never disagree within one build. Nil (pre-fork, or no registry
// configured) disables the reserved pass.
func (w *worker) sequencingSnapshot(env *environment) *registryreader.Snapshot {
	w.mu.RLock()
	override := w.reservedSnapshotOverride
	w.mu.RUnlock()

	if override != nil {
		return override
	}
	if env.evm == nil {
		return nil
	}

	return env.evm.Context.ReservedSnapshot
}

// writeReservedFields records the build's reserved-region gas total and the
// sequencing snapshot's effective capacity in the header's BlockExtraData,
// overwriting the placeholder zeros that Prepare stamped. Post-fork headers
// must carry both fields (verifyReservedFields), so this runs on every
// post-fork build — including interrupted ones, whose partial blocks still
// seal. The capacity comes from the same snapshot sequencing and execution
// classified with, so a producer can never stamp a value execution disagrees
// with.
func (w *worker) writeReservedFields(env *environment) error {
	if w.chainConfig.Bor == nil || !w.chainConfig.Bor.IsReservedBlockspace(env.header.Number) {
		return nil
	}

	capacity := w.sequencingSnapshot(env).EffectiveCapacity()
	reservedGasUsedGauge.Update(int64(env.reservedGasUsed))
	reservedCapacityGauge.Update(int64(capacity))

	if err := env.header.SetReservedFields(w.chainConfig, env.reservedGasUsed, capacity); err != nil {
		log.Error("error while writing reserved fields into block extra data", "err", err)
		return err
	}

	return nil
}

// generateWork generates a sealing block based on the given parameters.
func (w *worker) generateWork(params *generateParams, witness bool) *newPayloadResult {
	work, err := w.prepareWork(params, witness)
	if err != nil {
		return &newPayloadResult{err: err}
	}
	defer work.discard()

	if !params.noTxs {
		interrupt := new(atomic.Int32)

		timer := time.AfterFunc(w.newpayloadTimeout, func() {
			interrupt.Store(commitInterruptTimeout)
		})
		defer timer.Stop()

		err := w.fillTransactions(interrupt, work, nil)
		if errors.Is(err, errBlockInterruptedByTimeout) {
			log.Warn("Block building is interrupted", "allowance", common.PrettyDuration(w.newpayloadTimeout))
		}
	} else {
		// fillTransactions (which stamps the reserved header fields at the
		// end of a normal build) is skipped for a no-tx payload build, so
		// without this the header would keep Prepare's placeholder
		// ReservedCapacity — wrong as soon as the registry carves out any
		// capacity, even for a genuinely empty block.
		_ = w.writeReservedFields(work)
	}

	body := types.Body{Transactions: work.txs, Withdrawals: params.withdrawals}
	allLogs := make([]*types.Log, 0)
	for _, r := range work.receipts {
		allLogs = append(allLogs, r.Logs...)
	}

	// Polygon/bor: EIP-6110, EIP-7002, and EIP-7251 are not supported
	// Collect consensus-layer requests if Prague is enabled and bor consensus is not active.
	var requests [][]byte
	if w.chainConfig.IsPrague(work.header.Number) && w.chainConfig.Bor == nil {
		// EIP-6110 deposits
		err := core.ParseDepositLogs(&requests, allLogs, w.chainConfig)
		if err != nil {
			return &newPayloadResult{err: err}
		}
		// create EVM for system calls
		blockContext := core.NewEVMBlockContext(work.header, w.chain, &work.header.Coinbase)
		vmenv := vm.NewEVM(blockContext, work.state, w.chainConfig, w.vmConfig())
		// EIP-7002 withdrawals
		core.ProcessWithdrawalQueue(&requests, vmenv)
		// EIP-7251 consolidations
		core.ProcessConsolidationQueue(&requests, vmenv)
	}
	if requests != nil {
		reqHash := types.CalcRequestsHash(requests)
		work.header.RequestsHash = &reqHash
	}

	var block *types.Block
	block, work.receipts, _, err = w.engine.FinalizeAndAssemble(w.chain, work.header, work.state, &body, work.receipts)

	if err != nil {
		return &newPayloadResult{err: err}
	}
	return &newPayloadResult{
		block:    block,
		fees:     totalFees(block, work.receipts),
		sidecars: work.sidecars,
		stateDB:  work.state,
		receipts: work.receipts,
		requests: requests,
	}
}

// commitWork generates several new sealing tasks based on the parent block
// and submit them to the sealer.
func (w *worker) commitWork(interrupt *atomic.Int32, noempty bool, timestamp int64, abortRecovery bool) {
	// Clear pendingWorkBlock on every exit, including early returns, so the
	// veblop fallback doesn't short-circuit on a stale signal. When production
	// pipelining is re-enabled this needs to become pipeline-aware: a pipelined
	// build advances pendingWorkBlock past head+1 to mark N+1 in flight, and an
	// unconditional reset here would wipe that signal (see git history:
	// clearPendingWorkOnExit).
	defer func() {
		w.pendingWorkBlock.Store(0)
	}()

	// Abort committing if node is still syncing
	if w.syncing.Load() {
		return
	}
	buildStart := time.Now()

	// Set the coinbase if the worker is running or it's required
	var coinbase common.Address
	if w.IsRunning() {
		coinbase = w.etherbase()
		if coinbase == (common.Address{}) {
			log.Error("Refusing to mine without etherbase")
			return
		}
	}

	parent := w.chain.CurrentBlock()
	// Sealing must build on committed state: the FlatDiff overlay that
	// StateAtWithReaders serves during the pipelined window is rooted at the
	// grandparent, and a root computed over it omits the parent's writes —
	// producing a header every importer rejects. SealingStateAt* waits for
	// the pending SRC (bounded) and never serves the overlay.
	state, throwaway, prefetchReader, processReader, err := w.chain.SealingStateAtWithReaders(parent.Root)
	if err != nil {
		log.Warn("Sealing state unavailable, skipping work round", "parent", parent.Number, "root", parent.Root, "err", err)
		return
	}
	// The head may have been rolled back while waiting (failed SRC): the
	// state we hold belongs to the old parent. Skip; the corrective
	// ChainHeadEvent retriggers commitWork on the rolled-back head.
	if cur := w.chain.CurrentBlock(); cur.Hash() != parent.Hash() {
		log.Warn("Chain head moved while acquiring sealing state, skipping work round",
			"was", parent.Number, "now", cur.Number)
		return
	}

	genParams := generateParams{
		timestamp:          uint64(timestamp),
		coinbase:           coinbase,
		abortRecovery:      abortRecovery,
		parentHash:         parent.Hash(),
		statedb:            state,
		prefetchReader:     prefetchReader,
		processReader:      processReader,
		prefetchedTxHashes: &sync.Map{},
		preBuildDuration:   time.Since(buildStart),
	}

	var interruptPrefetch atomic.Bool
	w.maybeStartPrefetch(parent, throwaway, &genParams, &interruptPrefetch)
	w.buildAndCommitBlock(interrupt, noempty, &genParams, &interruptPrefetch)
}

// maybeStartPrefetch launches the tx-prefetch goroutine when enabled AND
// the next block is Giugliano-activated. Giugliano gating prevents pre-fork
// blocks from triggering speculative prefetch, which can read storage slots
// the current block's EVM hasn't touched yet and cause cache thrash.
func (w *worker) maybeStartPrefetch(parent *types.Header, throwaway *state.StateDB, genParams *generateParams, interruptPrefetch *atomic.Bool) {
	newBlockNumber := new(big.Int).Add(parent.Number, common.Big1)
	if !w.config.EnablePrefetch || w.chainConfig.Bor == nil || !w.chainConfig.Bor.IsGiugliano(newBlockNumber) {
		return
	}
	// Only allocate the builder-mode signal when a prefetcher will consume it.
	// Downstream paths gate planning work on builderStarted != nil, so leaving
	// it nil means zero overhead when prefetch is disabled.
	genParams.builderStarted = new(atomic.Bool)
	genParams.builderPrefetchedTxHashes = &sync.Map{}
	w.prefetchWg.Add(1)
	go func() {
		defer w.prefetchWg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Error("Prefetch goroutine panicked", "err", r, "stack", string(debug.Stack()))
				prefetchPanicMeter.Mark(1)
			}
		}()
		// Go's GC keeps throwaway alive while this goroutine references it;
		// released when the goroutine exits after prefetch completes.
		w.runPrefetcher(parent, throwaway, genParams, interruptPrefetch)
	}()
}

// buildAndCommitBlock prepares work, fills transactions, and commits the block for sealing.
// submitForSealing dispatches the built block to either the pipelined path
// (overlap SRC for N with N+1's execution) or the sequential path. The
// pendingWorkBlock bump is what de-duplicates ChainHeadEvent-triggered
// commitWork in newWorkLoop when the pipeline is already handling N+1.
func (w *worker) submitForSealing(work *environment, start time.Time, genParams *generateParams) {
	if w.isPipelineEligible(work.header.Number.Uint64()) {
		w.pendingWorkBlock.Store(work.header.Number.Uint64() + 1)
		_ = w.commitPipelined(work, start)
		return
	}
	_ = w.commit(work.copy(), w.fullTaskHook, true, start, genParams, reservedTxsOf(work), reservedSnapshotOf(work))
}

func (w *worker) buildAndCommitBlock(interrupt *atomic.Int32, noempty bool, genParams *generateParams, interruptPrefetch *atomic.Bool) {
	// Must be the first defer so the prefetcher goroutine is signaled to exit
	// on every return path — including the early return below when prepareWork
	// fails. Otherwise runIdleTxProvider loops until gas exhaustion, burning
	// CPU on throwaway EVM work while the block build is already aborted.
	defer interruptPrefetch.Store(true)

	prepareWorkStart := time.Now()
	work, err := w.prepareWork(genParams, w.makeWitness)
	if err != nil {
		return
	}
	prepareWorkDuration := time.Since(prepareWorkStart)
	prepareWorkTimer.Update(prepareWorkDuration)

	// Starts accounting time after prepareWork. Slot timing is handled in Seal
	// for sequential paths and explicitly in the pipeline path.
	start := time.Now()

	// Create the builder plan channel before signalling builder mode so the prefetcher goroutine
	// always finds a valid channel when it transitions. The buffer covers a full block's worth
	// of transactions with room to spare; the builder never blocks on a full buffer because
	// all sends are non-blocking.
	if genParams.builderStarted != nil {
		genParams.builderPlanCh = make(chan *types.Transaction, prefetchChanBufSize)
		genParams.builderGasFreedCh = make(chan uint64, 256)
		genParams.builderStarted.Store(true) // immediately interrupts idle Prefetch() + mode switch
	}

	stopFn := func() {}
	defer func() {
		stopFn()
	}()

	if w.IsRunning() {
		timeUntilInterrupt := time.Until(work.header.GetActualTime())
		if timeUntilInterrupt > time.Second {
			timeUntilInterrupt -= interruptBuffer
		}
		parent := w.chain.CurrentBlock()
		log.Info("Starting to build block", "number", work.header.Number.Uint64(),
			"buildStart", prepareWorkStart.UTC().Format(time.RFC3339Nano),
			"preBuild", common.PrettyDuration(genParams.preBuildDuration), // time spent before `buildAndCommitBlock` is called
			"prepareWork", common.PrettyDuration(prepareWorkDuration), // total time spent in prepare work
			"makeEnv", common.PrettyDuration(work.makeEnvDuration), // total time spent in `makeEnv` inside prepare work
			"makeHeader", common.PrettyDuration(work.makeHeaderDuration), // total time spent in `makeHeader` inside prepare work includes bor.Prepare call
			"parentTime", time.Unix(int64(parent.Time), 0).UTC().Format(time.RFC3339Nano),
			"parentActualTime", parent.GetActualTime().UTC().Format(time.RFC3339Nano),
			"headerTime", time.Unix(int64(work.header.Time), 0).UTC().Format(time.RFC3339Nano),
			"headerActualTime", work.header.GetActualTime().UTC().Format(time.RFC3339Nano),
			"timeUntilInterrupt", common.PrettyDuration(timeUntilInterrupt), // time left before block building will be interrupted
		)
	}

	if !noempty && w.interruptCommitFlag {
		// Start the timer for block building
		// Production-side pipelining is disabled, so the interrupt timer should
		// use the regular block-time boundary. If the production pipeline is
		// re-enabled, this was previously wired to the miner pipeline enable flag.
		timeoutInterrupt, timeoutFlagSetAt := w.interruptStateForEnv(work)
		stopFn = createInterruptTimer(
			work.header.Number.Uint64(),
			work.header.GetActualTime(),
			timeoutInterrupt,
			timeoutFlagSetAt,
			w.isPipelineEligible(work.header.Number.Uint64()),
		)
	}

	// Create an empty block based on temporary copied state for
	// sealing in advance without waiting block execution finished.
	// If the block is a veblop block, we will never try to create a commit for an empty block.
	var isRio bool
	if w.chainConfig.Bor != nil {
		isRio = w.chainConfig.Bor.IsRio(work.header.Number)
	}
	if !noempty && !w.noempty.Load() && !isRio {
		emptyWork := work.copy()
		emptyWork.state.ResetPrefetcher()
		// This copy is taken before fillTransactions runs, so it never gets
		// fillTransactions's end-of-build reserved-field write; stamp them
		// here too. The placeholder ReservedCapacity Prepare set is only
		// correct while the registry carves out zero capacity. The reserved
		// set is read from the pre-copy env: copy() does not carry evm over.
		_ = w.writeReservedFields(emptyWork)
		_ = w.commit(emptyWork, nil, false, start, genParams, reservedTxsOf(work), reservedSnapshotOf(work))
	}
	// Mark the start of full-block building. Set after the optional empty pre-seal commit so that
	// productionElapsed for the full block does not include empty-block overhead.
	genParams.productionStart = time.Now()
	// Fill pending transactions from the txpool into the block.
	err = w.fillTransactions(interrupt, work, genParams)
	// Wait for any sendPlan goroutines to finish before closing the channel.
	// These goroutines do only non-blocking sends so they complete in microseconds.
	// Waiting here ensures no goroutine sends to a closed channel.
	genParams.planWg.Wait()
	// Close gas freed channel first so the prefetcher sees it as exhausted before
	// the plan channel closes — the prefetcher exits on plan channel close.
	if genParams.builderGasFreedCh != nil {
		close(genParams.builderGasFreedCh)
	}
	// Signal the prefetcher that no more transactions will be sent. The prefetcher drains
	// any remaining channel entries and then exits naturally.
	if genParams.builderPlanCh != nil {
		close(genParams.builderPlanCh)
	}

	switch {
	case err == nil:
		// The entire block is filled, decrease resubmit interval in case
		// of current interval is larger than the user-specified one.
		w.adjustResubmitInterval(&intervalAdjust{inc: false})

	case errors.Is(err, errBlockInterruptedByRecommit):
		// Notify resubmit loop to increase resubmitting interval if the
		// interruption is due to frequent commits.
		gaslimit := work.header.GasLimit

		ratio := float64(gaslimit-work.gasPool.Gas()) / float64(gaslimit)
		if ratio < 0.1 {
			ratio = 0.1
		}
		w.adjustResubmitInterval(&intervalAdjust{
			ratio: ratio,
			inc:   true,
		})

	case errors.Is(err, errBlockInterruptedByNewHead):
		// If the block building is interrupted by newhead event, discard it
		// totally. Committing the interrupted block introduces unnecessary
		// delay, and possibly causes miner to mine on the previous head,
		// which could result in higher uncle rate.
		work.discard()
		return
	}
	// Submit the generated block for consensus sealing.
	w.submitForSealing(work, start, genParams)

	// Swap out the old work with the new one, terminating any leftover
	// prefetcher processes in the mean time and starting a new one.
	w.currentMu.Lock()
	if w.current != nil {
		w.current.discard()
	}
	w.current = work
	w.currentMu.Unlock()
}

// runPrefetcher owns the lifecycle of the unified prefetcher stream for one block.
// It starts a single long-lived worker pool (via PrefetchStream), runs the idle tx
// provider until the builder flips, executes the idle→builder handoff, and then
// runs the builder tx provider until block building completes.
//
// The handoff between phases uses the prefetcher's soft-interrupt (evmAbort) to
// abort any in-flight idle tx execution and drain buffered idle txs from the
// stream channel, so only builder txs reach the worker pool from that point on.
// hardKill is the permanent stream-exit signal (set by buildAndCommitBlock on exit).
func (w *worker) runPrefetcher(parent *types.Header, throwaway *state.StateDB, genParams *generateParams, hardKill *atomic.Bool) {
	w.mu.RLock()
	header, _, err := w.makeHeader(genParams, false)
	w.mu.RUnlock()
	if err != nil {
		return
	}

	prefetcher := core.NewStatePrefetcher(w.chainConfig, w.chain.HeaderChain())
	txsCh := make(chan *types.Transaction, prefetchChanBufSize)
	evmAbort := new(atomic.Bool)
	// inBuilderPhase gates builder-phase attribution. Flipped to true only
	// after the idle→builder handoff completes (evmAbort drain + reset), so
	// any onSuccess call firing after that point is known to come from
	// post-handoff work. Using genParams.builderStarted directly would open a
	// small attribution race: buildAndCommitBlock sets builderStarted=true
	// before runPrefetcher reaches the handoff, and an idle-phase tx whose
	// EVM work finishes between those two moments would otherwise be
	// miscounted as builder.
	//
	// Residual edge case: a worker that finished ApplyMessage but is still
	// inside IntermediateRoot(true) (not interruptible by evmAbort) when the
	// handoff completes could still reach onSuccess after inBuilderPhase=true,
	// inflating builder attribution by at most one tx. Handoff is sub-
	// millisecond in practice while IntermediateRoot spans microseconds to
	// low milliseconds, so the window is tiny but not zero.
	inBuilderPhase := new(atomic.Bool)

	onSuccess := func(hash common.Hash, _ uint64) {
		if genParams.prefetchedTxHashes != nil {
			genParams.prefetchedTxHashes.Store(hash, struct{}{})
		}
		if inBuilderPhase.Load() && genParams.builderPrefetchedTxHashes != nil {
			genParams.builderPrefetchedTxHashes.Store(hash, struct{}{})
		}
	}

	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		// intermediateRootPrefetch=false: benchmarks (state_prefetcher_intermediate_root_test.go)
		// show the per-tx IntermediateRoot adds ~80–130% prefetch wall time for ≤10%
		// commit speedup (≈0.1 ms). With snapshots active, the warming target is
		// pebble's block cache, which under realistic clean-cache sizes is already
		// resident. Upstream go-ethereum's prefetcher does not compute intermediate
		// roots either.
		prefetcher.PrefetchStream(header, throwaway, w.vmConfig(), false,
			hardKill, evmAbort, txsCh, onSuccess)
	}()

	// Defer the shutdown so a panic in either provider still releases the
	// workers. Without this, range-over-channel blocks forever (hardKill is
	// checked only after dequeue) and N+1 goroutines leak per panicking block.
	// sync.Once protects against the normal-exit close() racing with this
	// deferred close — the normal path does it explicitly below for deterministic
	// ordering with <-streamDone.
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			evmAbort.Store(true)
			close(txsCh)
		})
	}
	defer shutdown()

	// Phase 1: idle tx provider — streams pool txs until builder flips or hardKill fires.
	w.runIdleTxProvider(txsCh, header, genParams, hardKill)

	// Phase 2: builder tx provider, if we actually switched modes.
	if genParams.builderStarted != nil && genParams.builderStarted.Load() && !hardKill.Load() {
		// Handoff: abort in-flight idle work and drain buffered idle txs so only
		// builder txs reach the pool from here on. Then clear abort and run builder.
		// Any in-flight idle EVM execution aborts via evmAbort; workers finish their
		// current tx quickly (IntermediateRoot is the only non-interruptible work)
		// and move on. Workers that pick up a drained-but-not-gone tx see evmAbort=true
		// and skip it.
		evmAbort.Store(true)
		drainTxChan(txsCh)
		evmAbort.Store(false)
		// Flip phase attribution only after the handoff is complete. From here
		// on, every successful prefetch is genuinely builder-phase work.
		inBuilderPhase.Store(true)

		w.runBuilderTxProvider(txsCh, header, genParams, hardKill)
	}

	// Normal shutdown: close first, then wait for the stream to drain. The
	// defer above is a panic safety net; on the happy path we want the wait
	// ordered with the close rather than after the wrapping goroutine's recover.
	shutdown()
	<-streamDone
}

// drainTxChan removes all currently-buffered entries from the channel without blocking.
// Safe to call while other goroutines are reading from ch (reads consume; drain stops
// when the channel is empty from the drainer's perspective).
func drainTxChan(ch <-chan *types.Transaction) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// runIdleTxProvider speculatively streams transactions from the txpool into the
// prefetcher. It loops on a ~100ms cadence, bounded by a configurable gas budget
// (PrefetchGasLimitPercent of header.GasLimit, defaulting to 100%). Returns when
// the budget is exhausted, the builder flips, or hardKill fires.
//
// Gas accounting uses declared tx gas (not actual execution gas) — close enough
// since the budget only bounds speculative work, not correctness.
func (w *worker) runIdleTxProvider(txsCh chan<- *types.Transaction, header *types.Header, genParams *generateParams, interrupt *atomic.Bool) {
	signer := types.MakeSigner(w.chainConfig, header.Number, header.Time)
	filter := w.buildDefaultFilter(header.BaseFee, header.Number)
	filter.BlobTxs = false

	totalGasPool := new(core.GasPool).AddGas(header.GasLimit * idleGasLimitPercent(w.config) / 100)
	localPrefetched := make(map[common.Hash]struct{})

	shouldExit := func() bool {
		return interrupt.Load() ||
			(genParams.builderStarted != nil && genParams.builderStarted.Load()) ||
			totalGasPool.Gas() == 0
	}

	for !shouldExit() {
		loopStart := time.Now()

		pendingTxs := w.eth.TxPool().Pending(filter, interrupt)
		txs := newTransactionsByPriceAndNonce(signer, pendingTxs, header.BaseFee, interrupt)
		w.streamIdleBatch(txsCh, txs, totalGasPool, localPrefetched, header.GasLimit)

		waitUntilNextLoop(loopStart, prefetchIdleLoopInterval, shouldExit)
	}
}

// idleGasLimitPercent returns the configured prefetch gas budget percent, capped
// defensively at prefetchMaxGasLimitPercent and defaulted to
// prefetchDefaultGasLimitPercent when unset.
func idleGasLimitPercent(cfg *Config) uint64 {
	pct := cfg.PrefetchGasLimitPercent
	if pct == 0 {
		return prefetchDefaultGasLimitPercent
	}
	if pct > prefetchMaxGasLimitPercent {
		log.Warn("Prefetch gas limit percent exceeds maximum, capping",
			"configured", pct, "max", prefetchMaxGasLimitPercent)
		return prefetchMaxGasLimitPercent
	}
	return pct
}

// streamIdleBatch walks the price-nonce heap and non-blockingly forwards
// un-prefetched transactions to txsCh until the per-loop gas cap is exhausted,
// the heap is drained, or the channel fills. Returning on a full channel
// avoids spinning through the rest of the heap doing Peek/Shift work that
// would drop every tx: the outer loop will re-snapshot the pool on its next
// iteration (~100ms later), by which time workers have drained the channel.
func (w *worker) streamIdleBatch(
	txsCh chan<- *types.Transaction,
	txs *transactionsByPriceAndNonce,
	totalGasPool *core.GasPool,
	localPrefetched map[common.Hash]struct{},
	headerGasLimit uint64,
) {
	loopGasLimit := totalGasPool.Gas()
	if loopGasLimit > headerGasLimit {
		loopGasLimit = headerGasLimit
	}
	gaspool := new(core.GasPool).AddGas(loopGasLimit)

	for {
		ltx, tx := nextViableIdleTx(txs, gaspool, localPrefetched)
		if ltx == nil {
			return
		}
		select {
		case txsCh <- tx:
			localPrefetched[ltx.Hash] = struct{}{}
			gaspool.SubGas(ltx.Gas)
			totalGasPool.SubGas(ltx.Gas)
		default:
			// Channel full — stop this batch. The tx we failed to send will
			// reappear in the next iteration's pool snapshot.
			return
		}
		txs.Shift()
	}
}

// nextViableIdleTx advances the heap past txs that are too large for the loop
// budget, already warmed, or fail to resolve, and returns the next tx worth
// sending. Returns (nil, nil) when the heap is drained.
func nextViableIdleTx(
	txs *transactionsByPriceAndNonce,
	gaspool *core.GasPool,
	localPrefetched map[common.Hash]struct{},
) (*txpool.LazyTransaction, *types.Transaction) {
	for {
		ltx, _ := txs.Peek()
		if ltx == nil {
			return nil, nil
		}
		if gaspool.Gas() < ltx.Gas {
			txs.Pop()
			continue
		}
		if _, seen := localPrefetched[ltx.Hash]; seen {
			txs.Shift()
			continue
		}
		tx := ltx.Resolve()
		if tx == nil {
			txs.Pop()
			continue
		}
		return ltx, tx
	}
}

// waitUntilNextLoop sleeps up to (window - elapsed since loopStart) in small
// increments so shouldExit can be re-checked for fast shutdown.
func waitUntilNextLoop(loopStart time.Time, window time.Duration, shouldExit func() bool) {
	const checkInterval = 10 * time.Millisecond
	for remaining := window - time.Since(loopStart); remaining > 0; remaining = window - time.Since(loopStart) {
		if shouldExit() {
			return
		}
		sleep := checkInterval
		if remaining < checkInterval {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

// runBuilderTxProvider streams the builder's plan + freed-gas overflow into the
// prefetcher. Each 2ms window it collects plan txs and freed-gas signals via
// collectPlanBatch, scans the overflow heap for any bonus txs that fit in the
// accumulated freed budget, and streams everything to txsCh. Exits when the plan
// channel closes or hardKill fires.
func (w *worker) runBuilderTxProvider(txsCh chan<- *types.Transaction, header *types.Header, genParams *generateParams, interrupt *atomic.Bool) {
	const batchWindow = 2 * time.Millisecond

	planCh := genParams.builderPlanCh
	if planCh == nil {
		return
	}

	overflowHeap := w.buildOverflowHeap(header, interrupt)

	var extendedBudget uint64
	var gasFreedCh <-chan uint64 = genParams.builderGasFreedCh

	// inFlightHashes tracks hashes already forwarded on txsCh within the builder
	// phase. genParams.prefetchedTxHashes is only written after onSuccess fires,
	// which trails the EVM execution window; a plan tx still in-flight could
	// otherwise be re-emitted by scanOverflow from a fresh pool snapshot, wasting
	// a second worker on the same tx. Local map is safe because this provider
	// runs single-threaded.
	inFlightHashes := make(map[common.Hash]struct{})

	for {
		batch, newGasFreedCh, delta, builderDone := collectPlanBatch(
			planCh, gasFreedCh, batchWindow, genParams.prefetchedTxHashes, inFlightHashes,
		)
		gasFreedCh = newGasFreedCh
		extendedBudget += delta

		if extendedBudget > 0 {
			// Mark the plan batch as in-flight before the overflow scan so
			// scanOverflow won't re-emit the same tx within this iteration
			// (collectPlanBatch returns before forwardTxs records hashes).
			for _, tx := range batch {
				inFlightHashes[tx.Hash()] = struct{}{}
			}
			var bonus []*types.Transaction
			bonus, extendedBudget = scanOverflow(overflowHeap, extendedBudget, genParams.prefetchedTxHashes, inFlightHashes)
			batch = append(batch, bonus...)
		}

		forwardTxs(txsCh, batch, inFlightHashes)

		if builderDone || interrupt.Load() {
			return
		}
	}
}

// forwardTxs does a non-blocking send of each tx to ch. Drops silently if the
// buffer is full — prefetch is best-effort. Tracks each forwarded hash in
// inFlightHashes so follow-up overflow scans don't re-emit in-flight txs.
func forwardTxs(ch chan<- *types.Transaction, txs []*types.Transaction, inFlightHashes map[common.Hash]struct{}) {
	for _, tx := range txs {
		select {
		case ch <- tx:
			if inFlightHashes != nil {
				inFlightHashes[tx.Hash()] = struct{}{}
			}
		default:
		}
	}
}

// buildOverflowHeap takes a snapshot of the pending plain-tx pool ordered by gas price.
// The prefetcher uses it to warm bonus txs that fit in the block due to freed gas
// (declared > actual usage). It reuses the same filter as fillTransactions so the
// view is consistent with what the builder sees.
func (w *worker) buildOverflowHeap(header *types.Header, interrupt *atomic.Bool) *transactionsByPriceAndNonce {
	filter := w.buildDefaultFilter(header.BaseFee, header.Number)
	filter.BlobTxs = false
	signer := types.MakeSigner(w.chainConfig, header.Number, header.Time)
	pending := w.eth.TxPool().Pending(filter, interrupt)
	return newTransactionsByPriceAndNonce(signer, pending, header.BaseFee, interrupt)
}

// collectPlanBatch runs a single batch-collection window. It reads from the plan
// channel into batch (skipping already-prefetched txs and any tx already forwarded
// earlier in this builder phase), accumulates freed-gas signals into budgetDelta,
// and returns when the window timer fires or the plan channel closes. When
// gasFreedCh closes, it is disabled by returning a nil newGasFreedCh so the
// caller can stop selecting on it in subsequent calls.
//
// inFlightHashes closes the scanOverflow→plan cross-iteration edge of the dedup
// matrix: a tx emitted by scanOverflow in an earlier iteration and still
// executing on a worker is absent from prefetchedHashes (onSuccess trails
// multi-ms EVM) but present in inFlightHashes — without this check, a buffered
// copy of the same tx in planCh would get forwarded a second time.
func collectPlanBatch(
	planCh <-chan *types.Transaction,
	gasFreedCh <-chan uint64,
	window time.Duration,
	prefetchedHashes *sync.Map,
	inFlightHashes map[common.Hash]struct{},
) (batch []*types.Transaction, newGasFreedCh <-chan uint64, budgetDelta uint64, builderDone bool) {
	timer := time.NewTimer(window)
	defer timer.Stop()
	newGasFreedCh = gasFreedCh
	for {
		select {
		case tx, ok := <-planCh:
			if !ok {
				builderDone = true
				return
			}
			if prefetchedHashes != nil {
				if _, done := prefetchedHashes.Load(tx.Hash()); done {
					continue
				}
			}
			if _, inflight := inFlightHashes[tx.Hash()]; inflight {
				continue
			}
			batch = append(batch, tx)
		case freed, ok := <-newGasFreedCh:
			if !ok {
				newGasFreedCh = nil
			} else {
				budgetDelta += freed
			}
		case <-timer.C:
			return
		}
	}
}

// createInterruptTimer creates and starts a timer based on the header's timestamp.
func createInterruptTimer(number uint64, actualTimestamp time.Time, interruptBlockBuilding *atomic.Bool, interruptFlagSetAt *atomic.Int64, pipelinedSRC bool) func() {
	if interruptBlockBuilding == nil {
		return func() {}
	}

	delay := time.Until(actualTimestamp)

	// Reserve buffer for state root computation unless pipelined SRC is enabled,
	// in which case SRC runs in the background and fillTransactions gets the full block time.
	if !pipelinedSRC && delay > 1*time.Second {
		delay -= interruptBuffer
	}

	interruptCtx, cancel := context.WithTimeout(context.Background(), delay)

	// Reset the flag when timer starts for building a new block.
	interruptBlockBuilding.Store(false)
	if interruptFlagSetAt != nil {
		interruptFlagSetAt.Store(0)
	}

	go func() {
		// Wait for timeout
		<-interruptCtx.Done()

		// Toggle the flag to indicate commit transactions loop and EVM interpreter loop
		// to stop block building.
		if interruptCtx.Err() != context.Canceled {
			if interruptFlagSetAt != nil {
				interruptFlagSetAt.Store(time.Now().UnixNano())
			}
			interruptBlockBuilding.Store(true)
			cancel()
		}
	}()

	return cancel
}

// reservedTxsOf reads the reserved-region tx set off env's evm block context,
// nil-safe for an environment that never had one attached (e.g. pre-fork, or
// a shape that was never wired one up).
func reservedTxsOf(env *environment) map[registryreader.ReservedKey]struct{} {
	if env.evm == nil {
		return nil
	}
	return env.evm.Context.ReservedTxs
}

// reservedSnapshotOf returns the env's registry snapshot, or nil when the
// build has no reserved-aware EVM context.
func reservedSnapshotOf(env *environment) *registryreader.Snapshot {
	if env.evm == nil {
		return nil
	}
	return env.evm.Context.ReservedSnapshot
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
// Note the assumption is held that the mutation is allowed to the passed env, do
// the deep copy first.
//
// reservedTxs is the build's reserved-region tx set (keyed by sender+nonce)
// and reservedSnap the registry snapshot that classified it, both read by the
// caller off the pre-copy environment's evm.Context: env.copy() (below, and
// at both call sites before this function even sees env) does not carry the
// evm field over, so neither is recoverable from env itself here.
func (w *worker) commit(env *environment, interval func(), update bool, start time.Time, genParams *generateParams, reservedTxs map[registryreader.ReservedKey]struct{}, reservedSnap *registryreader.Snapshot) error {
	// Track total block building time and report metrics at the end of the commit cycle.
	defer func() {
		// Update total commit timer (matches the "elapsed" time in log)
		commitTimer.Update(time.Since(start))

		// Report cache hit/miss metrics (matches behavior in blockchain.go for import path)
		if metrics.Enabled() && env.prefetchReader != nil && env.processReader != nil {
			// Report prefetch reader stats
			prefetchStats := env.prefetchReader.GetStats()
			accountCacheHitPrefetchMeter.Mark(prefetchStats.AccountHit)
			accountCacheMissPrefetchMeter.Mark(prefetchStats.AccountMiss)
			storageCacheHitPrefetchMeter.Mark(prefetchStats.StorageHit)
			storageCacheMissPrefetchMeter.Mark(prefetchStats.StorageMiss)

			// Report process reader stats
			processStats := env.processReader.GetStats()
			accountCacheHitMeter.Mark(processStats.AccountHit)
			accountCacheMissMeter.Mark(processStats.AccountMiss)
			storageCacheHitMeter.Mark(processStats.StorageHit)
			storageCacheMissMeter.Mark(processStats.StorageMiss)

			// Report additional prefetch attribution metrics
			prefetchAttribStats := env.prefetchReader.GetPrefetchStats()
			accountInsertPrefetchMeter.Mark(prefetchAttribStats.AccountInsert)
			storageInsertPrefetchMeter.Mark(prefetchAttribStats.StorageInsert)

			processAttribStats := env.processReader.GetPrefetchStats()
			accountHitFromPrefetchMeter.Mark(processAttribStats.AccountHitFromPrefetch)
			storageHitFromPrefetchMeter.Mark(processAttribStats.StorageHitFromPrefetch)
			accountHitFromPrefetchUniqueMeter.Mark(processAttribStats.AccountHitFromPrefetchUnique)

			// Report prefetch coverage percentage
			if len(env.txs) > 0 && genParams != nil && genParams.prefetchedTxHashes != nil {
				prefetchedCount := 0
				builderAddedCount := 0

				for _, tx := range env.txs {
					if _, ok := genParams.prefetchedTxHashes.Load(tx.Hash()); ok {
						prefetchedCount++
					}
					if genParams.builderPrefetchedTxHashes != nil {
						if _, ok := genParams.builderPrefetchedTxHashes.Load(tx.Hash()); ok {
							builderAddedCount++
						}
					}
				}

				// Miss rate (0-100, higher = worse).
				missRate := int64((len(env.txs) - prefetchedCount) * 100 / len(env.txs))
				prefetchMissRateHistogram.Update(missRate)

				// Builder-added share (0-100): block txs the builder phase prefetched on
				// its own. Only emitted when the builder phase actually ran.
				if genParams.builderPrefetchedTxHashes != nil {
					builderAdded := int64(builderAddedCount * 100 / len(env.txs))
					prefetchBuilderAddedHistogram.Update(builderAdded)
				}
			}
		}
	}()

	if w.IsRunning() {
		if interval != nil {
			interval()
		}
		// Create a local environment copy, avoid the data race with snapshot state.
		// https://github.com/ethereum/go-ethereum/issues/24299
		env := env.copy()
		// Withdrawals are set to nil here, because this is only called in PoW.
		var block *types.Block
		var err error

		// Track time for FinalizeAndAssemble (state root calculation + block assembly)
		finalizeStart := time.Now()
		var commitTime time.Duration
		block, env.receipts, commitTime, err = w.engine.FinalizeAndAssemble(w.chain, env.header, env.state, &types.Body{
			Transactions: env.txs,
		}, env.receipts)
		finalizeDuration := time.Since(finalizeStart)
		finalizeAndAssembleTimer.Update(finalizeDuration)
		intermediateRootTimer.Update(commitTime)

		if err != nil {
			return err
		}

		reservedTxIndexes := core.ReservedTxIndexes(block.Transactions(), env.signer, reservedTxs)
		// Per-client usage is re-derived from the final body with the same
		// walk a verifier runs, so the producer's chain/reserved client
		// gauges report the identical numbers every importing node derives.
		_, reservedClientUsage := registryreader.ClassifyReserved(block.Transactions(), env.signer, reservedSnap)

		select {
		case w.taskCh <- &task{receipts: env.receipts, state: env.state, block: block, createdAt: time.Now(), productionStart: firstNonZeroTime(productionStartFrom(genParams), start), productionElapsed: time.Since(firstNonZeroTime(productionStartFrom(genParams), start)), intermediateRootTime: commitTime, reservedTxIndexes: reservedTxIndexes, reservedClientUsage: reservedClientUsage}:
			fees := totalFees(block, env.receipts)
			feesInEther := new(big.Float).Quo(new(big.Float).SetInt(fees), big.NewFloat(params.Ether))
			log.Info("Commit new sealing work",
				"number", block.Number(), "sealhash", w.engine.SealHash(block.Header()),
				"txs", env.tcount, "gas", block.GasUsed(), "fees", feesInEther,
				"elapsed", common.PrettyDuration(time.Since(start)),
				"pending", common.PrettyDuration(env.pendingDuration),
				"finalize", common.PrettyDuration(finalizeDuration),
			)

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}

	if update {
		w.updateSnapshot(env)
	}

	return nil
}

// getSealingBlock generates the sealing block based on the given parameters.
// The generation result will be passed back via the given channel no matter
// the generation itself succeeds or not.
func (w *worker) getSealingBlock(params *generateParams) *newPayloadResult {
	ctx := tracing.WithTracer(context.Background(), otel.GetTracerProvider().Tracer("getSealingBlock"))

	req := &getWorkReq{
		params: params,
		result: make(chan *newPayloadResult, 1),
		ctx:    ctx,
	}
	select {
	case w.getWorkCh <- req:
		return <-req.result
	case <-w.exitCh:
		return &newPayloadResult{err: errors.New("miner closed")}
	}
}

// adjustResubmitInterval adjusts the resubmit interval.
func (w *worker) adjustResubmitInterval(message *intervalAdjust) {
	select {
	case w.resubmitAdjustCh <- message:
	default:
		log.Warn("the resubmitAdjustCh is full, discard the message")
	}
}

// clearPending cleans the stale pending tasks.
func (w *worker) clearPending(number uint64) {
	w.pendingMu.Lock()
	for h, t := range w.pendingTasks {
		if t.block.NumberU64()+staleThreshold <= number {
			delete(w.pendingTasks, h)
		}
	}
	w.pendingMu.Unlock()
}

// warnIfStalled emits a single WARN per 30s when the chain has been stale
// for >3x the block time AND the veblop fallback can't make progress
// (either pendingWorkBlock thinks work is in flight, or pendingTasks is
// non-empty). Returns the new last-warn timestamp. pendingTasksCount must
// be captured under pendingMu by the caller — reading it here unguarded
// would race with taskLoop / resultLoop.
func (w *worker) warnIfStalled(currentBlock *types.Header, chainAgeSec int64, veblopTimeout time.Duration, pendingWorkBlock uint64, pendingTasksCount int, lastWarnAt time.Time) time.Time {
	// Compare in time.Duration so the 3x staleness threshold honors sub-second
	// block times (mainnet runs a 1.5s block time) — the same 3x multiple the
	// recovery path in decideVeblopFallback uses. chainAge itself is inherently
	// second-granular (it derives from integer block timestamps).
	if time.Duration(chainAgeSec)*time.Second <= 3*veblopTimeout {
		return lastWarnAt
	}
	if pendingWorkBlock != currentBlock.Number.Uint64()+1 && pendingTasksCount == 0 {
		return lastWarnAt
	}
	if time.Since(lastWarnAt) <= 30*time.Second {
		return lastWarnAt
	}
	log.Warn("Possible producer stall: veblop fallback skipping while chain is stale",
		"currentBlock", currentBlock.Number.Uint64(),
		"chainAgeSec", chainAgeSec,
		"veblopTimeout", veblopTimeout,
		"pendingWorkBlock", pendingWorkBlock,
		"pendingTasksCount", pendingTasksCount,
		"peerCount", w.eth.PeerCount())
	return time.Now()
}

// deletePendingTask removes a single pendingTasks entry by sealhash and
// returns true if the entry existed. The zero hash is a no-op. Called
// from the per-task onStopExit closure passed to Bor.SealWithStopHook,
// which fires on stop-branch exits where resultLoop would never reach
// the entry.
func (w *worker) deletePendingTask(sealHash common.Hash) bool {
	if sealHash == (common.Hash{}) {
		return false
	}
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	_, existed := w.pendingTasks[sealHash]
	delete(w.pendingTasks, sealHash)
	return existed
}

// vmConfig returns the VM config.
func (w *worker) vmConfig() vm.Config {
	cfg := *w.chain.GetVMConfig()
	// The miner copies its vm.Config from the chain instance, which may include
	// a vm.Config.Tracer intended only for live tracing, not for mining. Clear
	// the tracer here to prevent the miner from tracing block production and
	// conflicting with live tracing.
	cfg.Tracer = nil

	return cfg
}

// copyReceipts makes a deep copy of the given receipts.
func copyReceipts(receipts []*types.Receipt) []*types.Receipt {
	result := make([]*types.Receipt, len(receipts))

	for i, l := range receipts {
		cpy := *l
		result[i] = &cpy
	}

	return result
}

// totalFees computes total consumed miner fees in Wei. Block transactions and receipts have to have the same order.
func totalFees(block *types.Block, receipts []*types.Receipt) *big.Int {
	feesWei := new(big.Int)

	for i, tx := range block.Transactions() {
		minerFee, _ := tx.EffectiveGasTip(block.BaseFee())
		feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), minerFee))
	}

	return feesWei
}

// signalToErr converts the interruption signal to a concrete error type for return.
// The given signal must be a valid interruption signal.
func signalToErr(signal int32) error {
	switch signal {
	case commitInterruptNewHead:
		return errBlockInterruptedByNewHead
	case commitInterruptResubmit:
		return errBlockInterruptedByRecommit
	case commitInterruptTimeout:
		return errBlockInterruptedByTimeout
	default:
		panic(fmt.Errorf("undefined signal %d", signal))
	}
}
