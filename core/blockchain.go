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

// Package core implements the Ethereum consensus protocol.
package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/history"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

var (
	headBlockGauge          = metrics.NewRegisteredGauge("chain/head/block", nil)
	headHeaderGauge         = metrics.NewRegisteredGauge("chain/head/header", nil)
	headFastBlockGauge      = metrics.NewRegisteredGauge("chain/head/receipt", nil)
	headFinalizedBlockGauge = metrics.NewRegisteredGauge("chain/head/finalized", nil)
	headSafeBlockGauge      = metrics.NewRegisteredGauge("chain/head/safe", nil)

	chainInfoGauge   = metrics.NewRegisteredGaugeInfo("chain/info", nil)
	chainMgaspsMeter = metrics.NewRegisteredResettingTimer("chain/mgasps", nil)

	accountReadTimer   = metrics.NewRegisteredResettingTimer("chain/account/reads", nil)
	accountHashTimer   = metrics.NewRegisteredResettingTimer("chain/account/hashes", nil)
	accountUpdateTimer = metrics.NewRegisteredResettingTimer("chain/account/updates", nil)
	accountCommitTimer = metrics.NewRegisteredResettingTimer("chain/account/commits", nil)

	storageReadTimer   = metrics.NewRegisteredResettingTimer("chain/storage/reads", nil)
	storageHashTimer   = metrics.NewRegisteredTimer("chain/storage/hashes", nil)
	storageUpdateTimer = metrics.NewRegisteredResettingTimer("chain/storage/updates", nil)
	storageCommitTimer = metrics.NewRegisteredResettingTimer("chain/storage/commits", nil)

	accountCacheHitMeter  = metrics.NewRegisteredMeter("chain/account/reads/cache/process/hit", nil)
	accountCacheMissMeter = metrics.NewRegisteredMeter("chain/account/reads/cache/process/miss", nil)
	storageCacheHitMeter  = metrics.NewRegisteredMeter("chain/storage/reads/cache/process/hit", nil)
	storageCacheMissMeter = metrics.NewRegisteredMeter("chain/storage/reads/cache/process/miss", nil)

	accountCacheHitPrefetchMeter  = metrics.NewRegisteredMeter("chain/account/reads/cache/prefetch/hit", nil)
	accountCacheMissPrefetchMeter = metrics.NewRegisteredMeter("chain/account/reads/cache/prefetch/miss", nil)
	storageCacheHitPrefetchMeter  = metrics.NewRegisteredMeter("chain/storage/reads/cache/prefetch/hit", nil)
	storageCacheMissPrefetchMeter = metrics.NewRegisteredMeter("chain/storage/reads/cache/prefetch/miss", nil)

	// Additional prefetch attribution metrics
	accountHitFromPrefetchMeter       = metrics.NewRegisteredMeter("chain/account/reads/cache/process/hit_from_prefetch", nil)
	storageHitFromPrefetchMeter       = metrics.NewRegisteredMeter("chain/storage/reads/cache/process/hit_from_prefetch", nil)
	accountInsertPrefetchMeter        = metrics.NewRegisteredMeter("chain/account/reads/cache/prefetch/insert", nil)
	storageInsertPrefetchMeter        = metrics.NewRegisteredMeter("chain/storage/reads/cache/prefetch/insert", nil)
	accountHitFromPrefetchUniqueMeter = metrics.NewRegisteredMeter("chain/account/reads/cache/process/prefetch_used_unique", nil)

	accountReadSingleTimer   = metrics.NewRegisteredResettingTimer("chain/account/single/reads", nil) //nolint:revive,unused
	storageReadSingleTimer   = metrics.NewRegisteredResettingTimer("chain/storage/single/reads", nil) //nolint:revive,unused
	snapshotCommitTimer      = metrics.NewRegisteredResettingTimer("chain/snapshot/commits", nil)
	triedbCommitTimer        = metrics.NewRegisteredResettingTimer("chain/triedb/commits", nil)
	snapshotAccountReadTimer = metrics.NewRegisteredResettingTimer("chain/snapshot/account/reads", nil)
	snapshotStorageReadTimer = metrics.NewRegisteredResettingTimer("chain/snapshot/storage/reads", nil)

	borConsensusTime = metrics.NewRegisteredTimer("chain/bor/consensus", nil)

	blockImportTimer = metrics.NewRegisteredMeter("chain/imports", nil)

	blockInsertTimer = metrics.NewRegisteredTimer("chain/inserts", nil)
	// blockValidationTimer does NOT fire when pipelined SRC is enabled.
	// Reason: pipelined import uses ValidateStateCheap (gas + bloom + receipt
	// root only); the full root match happens later in the SRC goroutine.
	// Closest pipeline signals: chain/imports/pipelined/collect (caller's wait
	// on root verification) and chain/imports/pipelined/root_mismatch (must stay zero).
	blockValidationTimer      = metrics.NewRegisteredTimer("chain/validation", nil)
	blockCrossValidationTimer = metrics.NewRegisteredResettingTimer("chain/crossvalidation", nil) //nolint:revive,unused
	blockExecutionTimer       = metrics.NewRegisteredTimer("chain/execution", nil)
	// blockWriteTimer does NOT fire when pipelined SRC is enabled.
	// Reason: pipelined import splits "write" across two code paths — metadata/batch
	// write in writeBlockAndSetHeadPipelined and async state commit in the SRC
	// goroutine — so there is no single "write phase" number. Approximate by summing
	// chain/batch/write + chain/state/commit + chain/{account,storage}/commits.
	blockWriteTimer                    = metrics.NewRegisteredTimer("chain/write", nil)
	blockExecutionParallelCounter      = metrics.NewRegisteredCounter("chain/execution/parallel", nil)
	blockExecutionSerialCounter        = metrics.NewRegisteredCounter("chain/execution/serial", nil)
	blockExecutionParallelErrorCounter = metrics.NewRegisteredCounter("chain/execution/parallel/error", nil)
	blockExecutionParallelTimer        = metrics.NewRegisteredTimer("chain/execution/parallel/timer", nil)
	blockExecutionSerialTimer          = metrics.NewRegisteredTimer("chain/execution/serial/timer", nil)
	blockMgaspsMeter                   = metrics.NewRegisteredHistogram("chain/execution/mgasps", nil, metrics.NewUniformSample(10240))

	statelessParallelImportTimer           = metrics.NewRegisteredTimer("chain/imports/stateless/parallel", nil)
	statelessSequentialImportTimer         = metrics.NewRegisteredTimer("chain/imports/stateless/sequential", nil)
	statelessParallelImportBlocksCounter   = metrics.NewRegisteredCounter("chain/imports/stateless/parallel/blocks", nil)
	statelessSequentialImportBlocksCounter = metrics.NewRegisteredCounter("chain/imports/stateless/sequential/blocks", nil)

	blockReorgMeter     = metrics.NewRegisteredMeter("chain/reorg/executes", nil)
	blockReorgAddMeter  = metrics.NewRegisteredMeter("chain/reorg/add", nil)
	blockReorgDropMeter = metrics.NewRegisteredMeter("chain/reorg/drop", nil)

	blockPrefetchExecuteTimer     = metrics.NewRegisteredResettingTimer("chain/prefetch/executes", nil)
	blockPrefetchInterruptMeter   = metrics.NewRegisteredMeter("chain/prefetch/interrupts", nil)
	blockPrefetchTxsInvalidMeter  = metrics.NewRegisteredMeter("chain/prefetch/txs/invalid", nil)
	blockPrefetchTxsValidMeter    = metrics.NewRegisteredMeter("chain/prefetch/txs/valid", nil)
	blockPrefetchWorkerPanicMeter = metrics.NewRegisteredMeter("chain/prefetch/worker/panic", nil)

	// Witness and write-path metrics for block production observability.
	// These track the time spent in each phase of writeBlockWithState, which runs
	// on the critical path between block sealing and broadcasting. Delays here
	// (e.g. from large witness encoding, DB compaction stalls, or pathdb diff layer
	// flushes) can cause blocks to be broadcast late, triggering span rotations.
	witnessEncodeTimer     = metrics.NewRegisteredTimer("chain/witness/encode", nil)     // time to RLP-encode the witness (EncodeRLP)
	witnessDbWriteTimer    = metrics.NewRegisteredTimer("chain/witness/dbwrite", nil)    // time to write encoded witness into the DB batch (WriteWitness)
	witnessCollectionTimer = metrics.NewRegisteredTimer("chain/witness/collection", nil) // time spent collecting trie nodes into the witness during IntermediateRoot
	blockBatchWriteTimer   = metrics.NewRegisteredTimer("chain/batch/write", nil)        // time to flush the block batch to disk (blockBatch.Write) — spikes indicate DB compaction stalls
	stateCommitTimer       = metrics.NewRegisteredTimer("chain/state/commit", nil)       // time for statedb.CommitWithUpdate — in pathdb mode, spikes indicate diff layer flushes

	// Pipelined import SRC metrics
	pipelineImportBlocksCounter       = metrics.NewRegisteredCounter("chain/imports/pipelined/blocks", nil)
	pipelineImportTotalTimer          = metrics.NewRegisteredTimer("chain/imports/pipelined/total", nil)
	pipelineImportSRCTimer            = metrics.NewRegisteredTimer("chain/imports/pipelined/src", nil)
	pipelineImportCollectTimer        = metrics.NewRegisteredTimer("chain/imports/pipelined/collect", nil)
	pipelineImportFallbackCounter     = metrics.NewRegisteredCounter("chain/imports/pipelined/fallback", nil)
	pipelineImportHitCounter          = metrics.NewRegisteredCounter("chain/imports/pipelined/hit", nil)           // pending matched next block's parent — overlap achieved
	pipelineImportMissCounter         = metrics.NewRegisteredCounter("chain/imports/pipelined/miss", nil)          // pending didn't match — flushed (reorg/gap)
	pipelineImportRootMismatchCounter = metrics.NewRegisteredCounter("chain/imports/pipelined/root_mismatch", nil) // SRC goroutine returned wrong root — safety alarm, must stay zero
	// Mode gauge — 1 when pipelined SRC import is enabled on this node, 0 otherwise.
	// Dashboards can use this to distinguish "metric is zero because pipelining is off"
	// from "metric is zero because the pipelined code path bypassed its emit site".
	pipelineImportEnabledGauge = metrics.NewRegisteredGauge("chain/imports/pipelined/enabled", nil)

	// Cheap-exec timer for pipelined import. Wraps the synchronous
	// ProcessBlock call (FlatDiff overlay path). Disambiguates "cheap exec
	// is itself slow" from "main path waited on prev SRC" — chain/imports/
	// pipelined/collect covers only the wait, and the parity chain/execution
	// timer wraps the entire persistPipelinedImport (which includes that wait),
	// so neither pinpoints the cheap exec on its own.
	pipelineImportCheapExecTimer       = metrics.NewRegisteredTimer("chain/imports/pipelined/cheap_exec", nil)
	pipelineImportCheapValidationTimer = metrics.NewRegisteredTimer("chain/imports/pipelined/cheap_validation", nil)
	pipelineImportExecutionTimer       = metrics.NewRegisteredTimer("chain/imports/pipelined/execution", nil)
	// Execution time split by whether the previous block's SRC was still
	// running during this block's execution. with_overlap vs no_overlap is the
	// direct A/B the percent buckets refine — they answer "is execution slower
	// on overlapped blocks?" without relying on temporal correlation.
	pipelineImportExecWithOverlapTimer    = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/with_overlap", nil)
	pipelineImportExecNoOverlapTimer      = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/no_overlap", nil)
	pipelineImportExecOverlap0Timer       = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/overlap_0_percent", nil)
	pipelineImportExecOverlap1To25Timer   = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/overlap_1_25_percent", nil)
	pipelineImportExecOverlap25To50Timer  = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/overlap_25_50_percent", nil)
	pipelineImportExecOverlap50To75Timer  = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/overlap_50_75_percent", nil)
	pipelineImportExecOverlap75To100Timer = metrics.NewRegisteredTimer("chain/imports/pipelined/execution/overlap_75_100_percent", nil)
	pipelineImportPostExecTimer           = metrics.NewRegisteredTimer("chain/imports/pipelined/post_exec", nil)
	pipelineImportPostExecResidualTimer   = metrics.NewRegisteredTimer("chain/imports/pipelined/post_exec/residual", nil)
	pipelineImportWitnessCaptureTimer     = metrics.NewRegisteredTimer("chain/imports/pipelined/witness_capture", nil)
	pipelineImportPrefetchDetachTimer     = metrics.NewRegisteredTimer("chain/imports/pipelined/prefetch_detach", nil)
	pipelineImportPrefetchCleanupTimer    = metrics.NewRegisteredTimer("chain/imports/pipelined/prefetch_cleanup", nil)
	pipelineImportSRCPrefetchWaitTimer    = metrics.NewRegisteredTimer("chain/imports/pipelined/src/prefetch_wait", nil)
	pipelineImportSRCPrefetchReportTimer  = metrics.NewRegisteredTimer("chain/imports/pipelined/src/prefetch_report", nil)
	pipelineImportSRCOpenStateDBTimer     = metrics.NewRegisteredTimer("chain/imports/pipelined/src/open_statedb", nil)
	pipelineImportSRCApplyFlatDiffTimer   = metrics.NewRegisteredTimer("chain/imports/pipelined/src/apply_flatdiff", nil)
	pipelineImportSRCCommitTimer          = metrics.NewRegisteredTimer("chain/imports/pipelined/src/commit", nil)
	// SRC wall-clock split by whether the next block's execution overlapped it.
	// The next-block-perspective counterpart to the execution split above.
	pipelineImportSRCWithNextExecTimer     = metrics.NewRegisteredTimer("chain/imports/pipelined/src/with_next_exec_overlap", nil)
	pipelineImportSRCNoNextExecTimer       = metrics.NewRegisteredTimer("chain/imports/pipelined/src/no_next_exec_overlap", nil)
	pipelineImportOverlapExecutionTimer    = metrics.NewRegisteredTimer("chain/imports/pipelined/overlap/execution", nil)
	pipelineImportCommitSnapshotTimer      = metrics.NewRegisteredTimer("chain/imports/pipelined/commit_snapshot", nil)
	pipelineImportCollectTotalTimer        = metrics.NewRegisteredTimer("chain/imports/pipelined/collect_total", nil)
	pipelineImportStateSyncFeedTimer       = metrics.NewRegisteredTimer("chain/imports/pipelined/state_sync_feed", nil)
	pipelineImportReorgCheckTimer          = metrics.NewRegisteredTimer("chain/imports/pipelined/reorg_check", nil)
	pipelineImportSetFlatDiffTimer         = metrics.NewRegisteredTimer("chain/imports/pipelined/set_flatdiff", nil)
	pipelineImportWriteHeadTimer           = metrics.NewRegisteredTimer("chain/imports/pipelined/write_head", nil)
	pipelineImportBuildSRCBlockTimer       = metrics.NewRegisteredTimer("chain/imports/pipelined/build_src_block", nil)
	pipelineImportSpawnSRCTimer            = metrics.NewRegisteredTimer("chain/imports/pipelined/spawn_src", nil)
	pipelineImportPendingPublishTimer      = metrics.NewRegisteredTimer("chain/imports/pipelined/pending_publish", nil)
	pipelineImportWarmSnapshotCollect      = metrics.NewRegisteredTimer("chain/imports/pipelined/warm_snapshot/collect", nil)
	pipelineImportWarmSnapshotBuild        = metrics.NewRegisteredTimer("chain/imports/pipelined/warm_snapshot/build", nil)
	pipelineImportWarmSnapshotFetchers     = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/fetchers", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportOverlapBlocksCounter     = metrics.NewRegisteredCounter("chain/imports/pipelined/overlap/blocks", nil)
	pipelineImportNoOverlapBlocksCounter   = metrics.NewRegisteredCounter("chain/imports/pipelined/overlap/no_overlap", nil)
	pipelineImportOverlapExecutionPercent  = metrics.NewRegisteredHistogram("chain/imports/pipelined/overlap/execution_percent", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportSRCPrefetchSubfetchers   = metrics.NewRegisteredHistogram("chain/imports/pipelined/src/prefetch_subfetchers", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportWarmSnapshotNodes        = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/nodes", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportWarmSnapshotBytes        = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/bytes", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportWarmSnapshotAccountNodes = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/account_nodes", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportWarmSnapshotStorageNodes = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/storage_nodes", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportWarmSnapshotAccountBytes = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/account_bytes", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineImportWarmSnapshotStorageBytes = metrics.NewRegisteredHistogram("chain/imports/pipelined/warm_snapshot/storage_bytes", nil, metrics.NewExpDecaySample(1028, 0.015))

	// Normal import phase timers. These mirror the pipelined phase timers enough
	// to compare the "Imported new chain segment" elapsed breakdown between
	// develop-style import and pipelined import.
	normalImportTotalTimer      = metrics.NewRegisteredTimer("chain/imports/normal/total", nil)
	normalImportProcessTimer    = metrics.NewRegisteredTimer("chain/imports/normal/process", nil)
	normalImportValidationTimer = metrics.NewRegisteredTimer("chain/imports/normal/validation", nil)
	normalImportReorgCheckTimer = metrics.NewRegisteredTimer("chain/imports/normal/reorg_check", nil)
	normalImportWriteTimer      = metrics.NewRegisteredTimer("chain/imports/normal/write", nil)

	// Auto-collection phase timers. The auto-collection goroutine runs
	// asynchronously after persistPipelinedImport returns:
	//   WaitForSRC -> verifyImportSRCRoot -> publishImportWitness -> handleImportTrieGC
	// The main path's collect-wait (chain/imports/pipelined/collect) blocks
	// until ALL these phases finish, so a sustained main-path wait is not
	// necessarily a slow SRC compute — it could be slow witness publish or
	// trie GC. WaitForSRC duration is already covered by chain/imports/
	// pipelined/src; total covers the whole runImportAutoCollection wall
	// time so dashboards can verify verify+publish+gc sums to total minus src.
	pipelineImportAutoCollectTotalTimer   = metrics.NewRegisteredTimer("chain/imports/pipelined/auto_collect/total", nil)
	pipelineImportAutoCollectVerifyTimer  = metrics.NewRegisteredTimer("chain/imports/pipelined/auto_collect/verify", nil)
	pipelineImportAutoCollectPublishTimer = metrics.NewRegisteredTimer("chain/imports/pipelined/auto_collect/publish", nil)
	pipelineImportAutoCollectGCTimer      = metrics.NewRegisteredTimer("chain/imports/pipelined/auto_collect/gc", nil)

	// preloadFlatDiffReads instrumentation.
	pipelineSRCPreloadTimer                    = metrics.NewRegisteredTimer("chain/pipelined/src/preload", nil)
	pipelineSRCPreloadReadAccountsHistogram    = metrics.NewRegisteredHistogram("chain/pipelined/src/preload/read_accounts", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineSRCPreloadSlotsHistogram           = metrics.NewRegisteredHistogram("chain/pipelined/src/preload/slots", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineSRCPreloadDestructsHistogram       = metrics.NewRegisteredHistogram("chain/pipelined/src/preload/destructs", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineSRCPreloadNonexistentHistogram     = metrics.NewRegisteredHistogram("chain/pipelined/src/preload/nonexistent", nil, metrics.NewExpDecaySample(1028, 0.015))
	pipelineSRCPreloadSlotsPerAccountHistogram = metrics.NewRegisteredHistogram("chain/pipelined/src/preload/slots_per_account", nil, metrics.NewExpDecaySample(1028, 0.015))

	// Throughput histograms (mode-agnostic — emitted from both normal and pipelined import paths).
	gasUsedPerBlockHistogram      = metrics.NewRegisteredHistogram("chain/gas_used_per_block", nil, metrics.NewExpDecaySample(1028, 0.015))
	txsPerBlockHistogram          = metrics.NewRegisteredHistogram("chain/txs_per_block", nil, metrics.NewExpDecaySample(1028, 0.015))
	importSegmentBlocksHistogram  = metrics.NewRegisteredHistogram("chain/imports/segment/blocks", nil, metrics.NewExpDecaySample(1028, 0.015))
	importSegmentElapsedTimer     = metrics.NewRegisteredTimer("chain/imports/segment/elapsed", nil)
	importSegmentGasUsedHistogram = metrics.NewRegisteredHistogram("chain/imports/segment/gas_used", nil, metrics.NewExpDecaySample(1028, 0.015))
	importSegmentMgaspsHistogram  = metrics.NewRegisteredHistogram("chain/imports/segment/mgasps", nil, metrics.NewExpDecaySample(1028, 0.015))
	// Witness size histogram in bytes. Spikes here directly drive stateless-peer bandwidth cost.
	witnessSizeBytesHistogram = metrics.NewRegisteredHistogram("chain/witness/size_bytes", nil, metrics.NewExpDecaySample(1028, 0.015))
	// End-to-end import timer: from block processing start until the witness is
	// on disk and peer-visible (non-pipelined: end of writeBlockWithState;
	// pipelined: after WitnessReadyEvent fires in the auto-collection goroutine).
	// Apples-to-apples A/B metric between modes.
	witnessReadyEndToEndTimer = metrics.NewRegisteredTimer("chain/imports/witness_ready_end_to_end", nil)

	errInsertionInterrupted = errors.New("insertion is interrupted")
	errChainStopped         = errors.New("blockchain is stopped")
	errInvalidOldChain      = errors.New("invalid old chain")
	errInvalidNewChain      = errors.New("invalid new chain")
	// errWitnessTimeout           = errors.New("timeout waiting for witness computation")     // New error
	// errWitnessComputationFailed = errors.New("witness computation failed or was cancelled") // New error
)

const (
	bodyCacheLimit     = 256
	blockCacheLimit    = 256
	receiptsCacheLimit = 1024
	txLookupCacheLimit = 1024

	slowImportBlockThreshold    = time.Second
	slowImportPostExecThreshold = 500 * time.Millisecond
	slowImportCollectThreshold  = 100 * time.Millisecond
	slowImportSnapshotThreshold = 100 * time.Millisecond
	slowImportResidualThreshold = 100 * time.Millisecond

	// BlockChainVersion ensures that an incompatible database forces a resync from scratch.
	//
	// Changelog:
	//
	// - Version 4
	//   The following incompatible database changes were added:
	//   * the `BlockNumber`, `TxHash`, `TxIndex`, `BlockHash` and `Index` fields of log are deleted
	//   * the `Bloom` field of receipt is deleted
	//   * the `BlockIndex` and `TxIndex` fields of txlookup are deleted
	//
	// - Version 5
	//  The following incompatible database changes were added:
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are no longer stored for a receipt
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are computed by looking up the
	//      receipts' corresponding block
	//
	// - Version 6
	//  The following incompatible database changes were added:
	//    * Transaction lookup information stores the corresponding block number instead of block hash
	//
	// - Version 7
	//  The following incompatible database changes were added:
	//    * Use freezer as the ancient database to maintain all ancient data
	//
	// - Version 8
	//  The following incompatible database changes were added:
	//    * New scheme for contract code in order to separate the codes and trie nodes
	//
	// - Version 9
	//  The following incompatible database changes were added:
	//  * (not applicable for bor) Total difficulty has been removed from both the key-value store and the ancient store.
	//  * The metadata structure of freezer is changed by adding 'flushOffset'
	BlockChainVersion uint64 = 9
)

// BlockChainConfig contains the configuration of the BlockChain object.
type BlockChainConfig struct {
	// Trie database related options
	TrieCleanLimit       int           // Memory allowance (MB) to use for caching trie nodes in memory
	TrieDirtyLimit       int           // Memory limit (MB) at which to start flushing dirty trie nodes to disk
	TrieTimeLimit        time.Duration // Time limit after which to flush the current in-memory trie to disk
	TrieNoAsyncFlush     bool          // Whether the asynchronous buffer flushing is disallowed
	TrieJournalDirectory string        // Directory path to the journal used for persisting trie data across node restarts
	TriesInMemory        uint64        // Number of recent tries to keep in memory

	Preimages   bool   // Whether to store preimage of trie key to the disk
	StateScheme string // Scheme used to store ethereum states and merkle tree nodes on top
	ArchiveMode bool   // Whether to enable the archive mode

	// Number of blocks from the chain head for which state histories are retained.
	// If set to 0, all state histories across the entire chain will be retained;
	StateHistory uint64

	// Address-specific cache sizes for biased caching (pathdb only)
	// Maps account address to cache size in bytes
	AddressCacheSizes map[common.Address]int

	// PreloadRateLimit limits cache preload I/O in bytes per second per address.
	// This prevents preloading from overwhelming the disk during sync.
	// 0 = unlimited (legacy behavior), default = 1MB/s
	PreloadRateLimit int64

	// Number of blocks from the chain head for which trienode histories are retained.
	// If set to 0, all trienode histories across the entire chain will be retained;
	// If set to -1, no trienode history will be retained;
	TrienodeHistory int64

	// The frequency of full-value encoding. For example, a value of 16 means
	// that, on average, for a given trie node across its 16 consecutive historical
	// versions, only one version is stored in full format, while the others
	// are stored in diff mode for storage compression.
	NodeFullValueCheckpoint uint32

	// State snapshot related options
	SnapshotLimit   int  // Memory allowance (MB) to use for caching snapshot entries in memory
	SnapshotNoBuild bool // Whether the background generation is allowed
	SnapshotWait    bool // Wait for snapshot construction on startup. TODO(karalabe): This is a dirty hack for testing, nuke it

	// This defines the cutoff block for history expiry.
	// Blocks before this number may be unavailable in the chain database.
	ChainHistoryMode history.HistoryMode

	// Misc options
	NoPrefetch bool            // Whether to disable heuristic state prefetching when processing blocks
	Overrides  *ChainOverrides // Optional chain config overrides
	VmConfig   vm.Config       // Config options for the EVM Interpreter

	// TxLookupLimit specifies the maximum number of blocks from head for which
	// transaction hashes will be indexed.
	//
	// If the value is zero, all transactions of the entire chain will be indexed.
	// If the value is -1, indexing is disabled.
	TxLookupLimit int64

	// StateSizeTracking indicates whether the state size tracking is enabled.
	StateSizeTracking bool

	ShouldPreserve func(header *types.Header) bool
	Checker        ethereum.ChainValidator

	// This defines the cutoff block for history expiry.
	// Blocks before this number may be unavailable in the chain database.
	HistoryPruningCutoff uint64

	// Whether the node is in stateless mode or not.
	Stateless bool

	// MilestoneFetcher returns the latest milestone end block from Heimdall.
	MilestoneFetcher func(ctx context.Context) (uint64, error)

	// EnablePipelinedImportSRC enables pipelined state root computation during
	// block import: overlap SRC(N) with tx execution of block N+1.
	EnablePipelinedImportSRC bool

	// PipelinedImportSRCLogs enables verbose logging for the import pipeline.
	PipelinedImportSRCLogs bool

	// PipelinedSRCWarmSnapshot enables a warm-node handoff to the pipelined
	// SRC goroutine when witnesses are produced: persistPipelinedImport
	// captures the trie nodes the execution-side prefetcher had loaded into a
	// quiesced WarmSnapshot; SRC's NewTrieOnly reader consults it before
	// falling through to pathdb. Lookups are hash-verified and fall through
	// to pathdb on miss, so root determinism and witness completeness are
	// unaffected. Witness-off import ignores this flag — the pathdb node
	// index already serves diff-layer nodes in one probe, and A/B
	// benchmarking showed a commit-fed warm ring on top of it cost SRC-lane
	// time instead of saving it.
	PipelinedSRCWarmSnapshot bool
}

// PipelineImportOpts configures ProcessBlock for pipelined import mode.
// When non-nil, ProcessBlock opens state at CommittedParentRoot (with optional
// FlatDiff overlay) and uses ValidateStateCheap instead of full ValidateState.
type PipelineImportOpts struct {
	CommittedParentRoot common.Hash     // Last committed trie root (grandparent when FlatDiff is set)
	FlatDiff            *state.FlatDiff // Previous block's state overlay (nil for first block in pipeline)
	Mode                string          // "flatdiff" when overlaying pending SRC state, "direct" otherwise
	PendingBlock        uint64          // Pending SRC block that supplied FlatDiff, if any
	PendingHash         common.Hash     // Hash of PendingBlock, if any
	PendingCollected    bool            // Whether PendingBlock's SRC collection had already completed at selection time
	pendingSRC          *pendingSRCState
}

const (
	pipelineImportModeDirect   = "direct"
	pipelineImportModeFlatDiff = "flatdiff"
)

// DefaultConfig returns the default config.
// Note the returned object is safe to modify!
func DefaultConfig() *BlockChainConfig {
	return &BlockChainConfig{
		TrieCleanLimit:   256,
		TrieDirtyLimit:   256,
		TrieTimeLimit:    5 * time.Minute,
		TriesInMemory:    state.TriesInMemory,
		StateScheme:      rawdb.HashScheme,
		SnapshotLimit:    256,
		SnapshotWait:     true,
		ChainHistoryMode: history.KeepAll,
		// Transaction indexing is disabled by default.
		// This is appropriate for most unit tests.
		TxLookupLimit: -1,
		VmConfig:      vm.Config{},
	}
}

// WithArchive enables/disables archive mode on the config.
func (cfg BlockChainConfig) WithArchive(on bool) *BlockChainConfig {
	cfg.ArchiveMode = on
	return &cfg
}

// WithStateScheme sets the state storage scheme on the config.
func (cfg BlockChainConfig) WithStateScheme(scheme string) *BlockChainConfig {
	cfg.StateScheme = scheme
	return &cfg
}

// WithNoAsyncFlush enables/disables asynchronous buffer flushing mode on the config.
func (cfg BlockChainConfig) WithNoAsyncFlush(on bool) *BlockChainConfig {
	cfg.TrieNoAsyncFlush = on
	return &cfg
}

// GetTriesInMemory returns the safe value of tries in memory (defaults to [state.TriesInMemory])
func (cfg BlockChainConfig) GetTriesInMemory() uint64 {
	if cfg.TriesInMemory == 0 {
		return state.TriesInMemory
	}
	return cfg.TriesInMemory
}

// triedbConfig derives the configures for trie database.
func (cfg *BlockChainConfig) triedbConfig(isVerkle bool) *triedb.Config {
	config := &triedb.Config{
		Preimages: cfg.Preimages,
		IsVerkle:  isVerkle,
	}
	if cfg.StateScheme == rawdb.HashScheme {
		config.HashDB = &hashdb.Config{
			CleanCacheSize: cfg.TrieCleanLimit * 1024 * 1024,
		}
	}
	if cfg.StateScheme == rawdb.PathScheme {
		config.PathDB = &pathdb.Config{
			StateHistory:        cfg.StateHistory,
			TrienodeHistory:     cfg.TrienodeHistory,
			EnableStateIndexing: cfg.ArchiveMode,
			FullValueCheckpoint: cfg.NodeFullValueCheckpoint,
			TrieCleanSize:       cfg.TrieCleanLimit * 1024 * 1024,
			StateCleanSize:      cfg.SnapshotLimit * 1024 * 1024,
			MaxDiffLayers:       cfg.GetTriesInMemory(),
			JournalDirectory:    cfg.TrieJournalDirectory,

			// TODO(rjl493456442): The write buffer represents the memory limit used
			// for flushing both trie data and state data to disk. The config name
			// should be updated to eliminate the confusion.
			WriteBufferSize:   cfg.TrieDirtyLimit * 1024 * 1024,
			NoAsyncFlush:      cfg.TrieNoAsyncFlush,
			AddressCacheSizes: cfg.AddressCacheSizes,
			PreloadRateLimit:  cfg.PreloadRateLimit,
		}
	}
	return config
}

// txLookup is wrapper over transaction lookup along with the corresponding
// transaction object.
type txLookup struct {
	lookup      *rawdb.LegacyTxLookupEntry
	transaction *types.Transaction
}

// pendingSRCState tracks an in-flight pipelined state root computation goroutine.
// root, witness, and err are written by the goroutine before wg.Done();
// callers block on wg.Wait() and read them afterwards.
type pendingSRCState struct {
	blockHash   common.Hash
	blockNumber uint64
	wg          sync.WaitGroup
	startNanos  atomic.Int64
	doneNanos   atomic.Int64
	// Set by the next block's execution-metrics recording so SRC wall-clock can
	// be split by whether that execution overlapped this SRC. classified gates
	// the read (overlapped is only meaningful once classified is true).
	nextExecOverlapped atomic.Bool
	nextExecClassified atomic.Bool
	root               common.Hash
	witness            []byte // RLP-encoded witness built by the SRC goroutine
	err                error
}

// pendingImportSRCState stores the state of a block whose SRC goroutine has
// been spawned. Block metadata is written to DB immediately; the state commit
// runs in the background. An auto-collection goroutine waits for SRC to finish
// and immediately writes the witness + handles trie GC, so collection doesn't
// depend on the arrival of the next block.
type pendingImportSRCState struct {
	block         *types.Block
	flatDiff      *state.FlatDiff
	committedRoot common.Hash   // last committed trie root when SRC was spawned
	procTime      time.Duration // for gcproc accumulation
	blockStart    time.Time     // block processing start — used for chain/imports/witness_ready_end_to_end
	makeWitness   bool          // whether the SRC goroutine is producing a witness for this block
	src           *pendingSRCState

	// collectedCh is closed when auto-collection completes (verify root,
	// write witness, trie GC). Callers block on <-collectedCh.
	collectedCh   chan struct{}
	collectedRoot common.Hash // verified root (set before closing collectedCh)
	collectedErr  error       // non-nil if SRC failed or root mismatch
	// divergentRoot is the root SRC computed and committed when it disagreed
	// with the block header. Zero when SRC failed before committing. Set
	// before collectedCh closes, read only after.
	divergentRoot common.Hash
}

// pipelinedImportStateAvailabilityGrace bounds how long sync-mode checks may
// treat a missing head state as "currently being committed by pipelined SRC".
// It only suppresses transient false positives in the write-head -> SRC-commit
// handoff; once the window expires, missing state is reported normally.
const pipelinedImportStateAvailabilityGrace = 2 * time.Minute

// BlockChain represents the canonical chain given a database with a genesis
// block. The Blockchain manages chain imports, reverts, chain reorganisations.
//
// Importing blocks in to the block chain happens according to the set of rules
// defined by the two stage Validator. Processing of blocks is done using the
// Processor which processes the included transaction. The validation of the state
// is done in the second part of the Validator. Failing results in aborting of
// the import.
//
// The BlockChain also helps in returning blocks from **any** chain included
// in the database as well as blocks that represents the canonical chain. It's
// important to note that GetBlock can return any block and does not need to be
// included in the canonical one where as GetBlockByNumber always represents the
// canonical chain.
type BlockChain struct {
	chainConfig *params.ChainConfig // Chain & network configuration
	cfg         *BlockChainConfig   // Blockchain configuration

	db            ethdb.Database                   // Low level persistent database to store final content in
	snaps         *snapshot.Tree                   // Snapshot tree for fast trie leaf access
	triegc        *prque.Prque[int64, common.Hash] // Priority queue mapping block numbers to tries to gc
	gcproc        time.Duration                    // Accumulates canonical block processing for trie dumping
	lastWrite     uint64                           // Last block when the state was flushed
	flushInterval atomic.Int64                     // Time interval (processing time) after which to flush a state
	triedb        *triedb.Database                 // The database handler for maintaining trie nodes.
	statedb       *state.CachingDB                 // State database to reuse between imports (contains state cache)
	txIndexer     *txIndexer                       // Transaction indexer, might be nil if not enabled

	hc               *HeaderChain
	rmLogsFeed       event.Feed
	chainFeed        event.Feed
	chainHeadFeed    event.Feed
	logsFeed         event.Feed
	blockProcFeed    event.Feed
	witnessReadyFeed event.Feed
	blockProcCounter int32
	scope            event.SubscriptionScope
	genesisBlock     *types.Block

	// This mutex synchronizes chain write operations.
	// Readers don't need to take it, they can just read the database.
	chainmu *syncx.ClosableMutex

	currentBlock      atomic.Pointer[types.Header] // Current head of the chain
	currentSnapBlock  atomic.Pointer[types.Header] // Current head of snap-sync
	currentFinalBlock atomic.Pointer[types.Header] // Latest (consensus) finalized block
	currentSafeBlock  atomic.Pointer[types.Header] // Latest (consensus) safe block
	historyPrunePoint atomic.Pointer[history.PrunePoint]

	bodyCache     *lru.Cache[common.Hash, *types.Body]
	bodyRLPCache  *lru.Cache[common.Hash, rlp.RawValue]
	receiptsCache *lru.Cache[common.Hash, []*types.Receipt] // Receipts cache with all fields derived
	blockCache    *lru.Cache[common.Hash, *types.Block]
	witnessCache  *lru.Cache[common.Hash, []byte] // Witness cache for RLP-encoded witnesses
	witnessStore  rawdb.WitnessStore

	txLookupLock  sync.RWMutex
	txLookupCache *lru.Cache[common.Hash, txLookup]

	wg            sync.WaitGroup
	quit          chan struct{} // shutdown signal, closed in Stop.
	stopping      atomic.Bool   // false if chain is running, true when stopped
	procInterrupt atomic.Bool   // interrupt signaler for block processing

	engine                         consensus.Engine
	validator                      Validator // Block and state validator interface
	prefetcher                     Prefetcher
	processor                      Processor // Block transaction processor interface
	parallelProcessor              Processor // Parallel block transaction processor interface
	parallelSpeculativeProcesses   int       // Number of parallel speculative processes
	enforceParallelProcessor       bool
	parallelStatelessImportEnabled atomic.Bool // Whether parallel stateless import is enabled via config
	parallelStatelessImportWorkers int         // Number of workers to use for parallel stateless import
	forker                         *ForkChoice
	logger                         *tracing.Hooks
	stateSizer                     *state.SizeTracker // State size tracking

	// Bor related changes
	borReceiptsCache    *lru.Cache[common.Hash, *types.Receipt]   // Cache for the most recent bor receipt receipts per block
	stateSyncMu         sync.RWMutex                              // Mutex to protect the stateSyncData access
	borReceiptsRLPCache *lru.Cache[common.Hash, rlp.RawValue]     // Cache for the most recent bor receipt RLPs per block
	stateSyncData       []*types.StateSyncData                    // State sync data
	stateSyncFeed       event.Feed                                // State sync feed
	chain2HeadFeed      event.Feed                                // Reorg/NewHead/Fork data feed
	chainSideFeed       event.Feed                                // Side chain data feed (removed from geth but needed in bor)
	milestoneFetcher    func(ctx context.Context) (uint64, error) // Function to fetch the latest milestone end block from Heimdall.

	// Pipelined SRC: concurrent state root calculation.
	// pendingSRC tracks the in-flight SRC goroutine for the most recent block.
	pendingSRC   *pendingSRCState
	pendingSRCMu sync.Mutex

	// pendingImportSRC tracks a block whose SRC goroutine is in-flight during
	// pipelined import. Persists across insertChain calls.
	pendingImportSRC *pendingImportSRCState
	// srcHoldForTesting, when non-nil, is invoked at the start of each SRC
	// goroutine. Tests use it to hold the pipelined window (head advanced,
	// state root not yet committed) open deterministically.
	srcHoldForTesting func(blockNumber uint64)
	// pipelinedMakeWitness latches whether the most recent pipelined import
	// produced a witness. waitForPipelinedWitness uses it to skip the cache
	// poll on witness-off nodes, where no witness will ever appear and a
	// peer could otherwise tie the WIT handler up for the full poll timeout
	// per recent block hash.
	pipelinedMakeWitness atomic.Bool
	// pendingImportHead* covers the short gap after block metadata/head are
	// written and before pendingImportSRC is published for the same block.
	pendingImportHeadHash  common.Hash
	pendingImportHeadRoot  common.Hash
	pendingImportHeadStart time.Time
	pendingImportSRCMu     sync.Mutex

	// lastFlatDiff holds the FlatDiff from the most recently committed block.
	// The miner uses it together with the grandparent's committed root to open
	// a StateDB via NewWithFlatBase, allowing block N+1 execution to start
	// before the SRC goroutine finishes.
	lastFlatDiff           *state.FlatDiff
	lastFlatDiffBlockNum   uint64
	lastFlatDiffParentRoot common.Hash // committed root that the FlatDiff is based on
	lastFlatDiffBlockRoot  common.Hash // the block's own state root (from header)
	lastFlatDiffMu         sync.RWMutex
}

// NewBlockChain returns a fully initialised block chain using information
// available in the database. It initialises the default Ethereum Validator
// and Processor.
func NewBlockChain(db ethdb.Database, genesis *Genesis, engine consensus.Engine, cfg *BlockChainConfig) (*BlockChain, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if cfg.EnablePipelinedImportSRC {
		pipelineImportEnabledGauge.Update(1)
	} else {
		pipelineImportEnabledGauge.Update(0)
	}

	// Open trie database with provided config
	enableVerkle, err := EnableVerkleAtGenesis(db, genesis)
	if err != nil {
		return nil, err
	}
	triedb := triedb.NewDatabase(db, cfg.triedbConfig(enableVerkle))

	// Write the supplied genesis to the database if it has not been initialized
	// yet. The corresponding chain config will be returned, either from the
	// provided genesis or from the locally stored configuration if the genesis
	// has already been initialized.
	chainConfig, genesisHash, compatErr, err := SetupGenesisBlockWithOverride(db, triedb, genesis, cfg.Overrides)
	if err != nil {
		return nil, err
	}

	log.Info("")
	log.Info(strings.Repeat("-", 153))

	for _, line := range strings.Split(chainConfig.Description(), "\n") {
		log.Info(line)
	}

	log.Info(strings.Repeat("-", 153))
	log.Info("")

	bc := &BlockChain{
		chainConfig:         chainConfig,
		cfg:                 cfg,
		db:                  db,
		triedb:              triedb,
		triegc:              prque.New[int64, common.Hash](nil),
		quit:                make(chan struct{}),
		chainmu:             syncx.NewClosableMutex(),
		bodyCache:           lru.NewCache[common.Hash, *types.Body](bodyCacheLimit),
		bodyRLPCache:        lru.NewCache[common.Hash, rlp.RawValue](bodyCacheLimit),
		receiptsCache:       lru.NewCache[common.Hash, []*types.Receipt](receiptsCacheLimit),
		blockCache:          lru.NewCache[common.Hash, *types.Block](blockCacheLimit),
		txLookupCache:       lru.NewCache[common.Hash, txLookup](txLookupCacheLimit),
		witnessCache:        lru.NewCache[common.Hash, []byte](bodyCacheLimit),
		witnessStore:        rawdb.GetWitnessStore(db),
		engine:              engine,
		borReceiptsCache:    lru.NewCache[common.Hash, *types.Receipt](receiptsCacheLimit),
		borReceiptsRLPCache: lru.NewCache[common.Hash, rlp.RawValue](receiptsCacheLimit),
		logger:              cfg.VmConfig.Tracer,
		milestoneFetcher:    cfg.MilestoneFetcher,
	}

	bc.hc, err = NewHeaderChain(db, chainConfig, engine, bc.insertStopped)
	if err != nil {
		return nil, err
	}
	bc.flushInterval.Store(int64(cfg.TrieTimeLimit))
	bc.forker = NewForkChoice(bc, cfg.ShouldPreserve, cfg.Checker)

	bc.statedb = state.NewDatabase(bc.triedb, nil)
	bc.validator = NewBlockValidator(chainConfig, bc)
	bc.prefetcher = NewStatePrefetcher(chainConfig, bc.hc)
	bc.processor = NewStateProcessor(bc.hc)

	genesisHeader := bc.GetHeaderByNumber(0)
	bc.genesisBlock = types.NewBlockWithHeader(genesisHeader)
	if bc.genesisBlock == nil {
		return nil, ErrNoGenesis
	}

	bc.currentBlock.Store(nil)
	bc.currentSnapBlock.Store(nil)
	bc.currentFinalBlock.Store(nil)
	bc.currentSafeBlock.Store(nil)

	// Update chain info data metrics
	chainInfoGauge.Update(metrics.GaugeInfoValue{"chain_id": bc.chainConfig.ChainID.String()})

	// If Geth is initialized with an external ancient store, re-initialize the
	// missing chain indexes and chain flags. This procedure can survive crash
	// and can be resumed in next restart since chain flags are updated in last step.
	if bc.empty() {
		rawdb.InitDatabaseFromFreezer(bc.db)
	}
	// Load blockchain states from disk
	if err := bc.loadLastState(); err != nil {
		return nil, err
	}
	// Make sure the state associated with the block is available, or log out
	// if there is no available state, waiting for state sync.
	head := bc.CurrentBlock()
	// nolint:nestif

	// If the node is in stateless mode, it should not load the state from disk
	if !bc.HasState(head.Root) && !bc.cfg.Stateless {
		if head.Number.Uint64() == 0 {
			// The genesis state is missing, which is only possible in the path-based
			// scheme. This situation occurs when the initial state sync is not finished
			// yet, or the chain head is rewound below the pivot point. In both scenarios,
			// there is no possible recovery approach except for rerunning a snap sync.
			// Do nothing here until the state syncer picks it up.
			log.Info("Genesis state is missing, wait state sync")
		} else {
			// Head state is missing, before the state recovery, find out the disk
			// layer point of snapshot(if it's enabled). Make sure the rewound point
			// is lower than disk layer.
			//
			// Note it's unnecessary in path mode which always keep trie data and
			// state data consistent.
			var diskRoot common.Hash
			if bc.cfg.SnapshotLimit > 0 && bc.cfg.StateScheme == rawdb.HashScheme {
				diskRoot = rawdb.ReadSnapshotRoot(bc.db)
			}
			if diskRoot != (common.Hash{}) {
				log.Warn("Head state missing, repairing", "number", head.Number, "hash", head.Hash(), "snaproot", diskRoot)

				snapDisk, err := bc.setHeadBeyondRoot(head.Number.Uint64(), 0, diskRoot, true)
				if err != nil {
					return nil, err
				}
				// Chain rewound, persist old snapshot number to indicate recovery procedure
				if snapDisk != 0 {
					rawdb.WriteSnapshotRecoveryNumber(bc.db, snapDisk)
				}
			} else {
				log.Warn("Head state missing, repairing", "number", head.Number, "hash", head.Hash())
				if _, err := bc.setHeadBeyondRoot(head.Number.Uint64(), 0, common.Hash{}, true); err != nil {
					return nil, err
				}
			}
		}
	}
	// Ensure that a previous crash in SetHead doesn't leave extra ancients
	//nolint:nestif
	if frozen, err := bc.db.ItemAmountInAncient(); err == nil && frozen > 0 {
		frozen, err = bc.db.Ancients()
		if err != nil {
			return nil, err
		}
		var (
			needRewind bool
			low        uint64
		)
		// The head full block may be rolled back to a very low height due to
		// blockchain repair. If the head full block is even lower than the ancient
		// chain, truncate the ancient store.
		fullBlock := bc.CurrentBlock()
		if fullBlock != nil && fullBlock.Hash() != bc.genesisBlock.Hash() && fullBlock.Number.Uint64() < frozen-1 {
			needRewind = true
			low = fullBlock.Number.Uint64()
		}
		// In snap sync, it may happen that ancient data has been written to the
		// ancient store, but the LastFastBlock has not been updated, truncate the
		// extra data here.
		snapBlock := bc.CurrentSnapBlock()
		if snapBlock != nil && snapBlock.Number.Uint64() < frozen-1 {
			needRewind = true

			if snapBlock.Number.Uint64() < low || low == 0 {
				low = snapBlock.Number.Uint64()
			}
		}

		if needRewind {
			log.Error("Truncating ancient chain", "from", bc.CurrentHeader().Number.Uint64(), "to", low)

			if err := bc.SetHead(low); err != nil {
				return nil, err
			}
		}
	}

	// Check the current state of the block hashes and make sure that we do not have any of the bad blocks in our chain
	for hash := range BadHashes {
		if header := bc.GetHeaderByHash(hash); header != nil {
			// get the canonical block corresponding to the offending header's number
			headerByNumber := bc.GetHeaderByNumber(header.Number.Uint64())
			// make sure the headerByNumber (if present) is in our current canonical chain
			if headerByNumber != nil && headerByNumber.Hash() == header.Hash() {
				log.Error("Found bad hash, rewinding chain", "number", header.Number, "hash", header.ParentHash)

				if err := bc.SetHead(header.Number.Uint64() - 1); err != nil {
					return nil, err
				}

				log.Error("Chain rewind was successful, resuming normal operation")
			}
		}
	}

	if bc.logger != nil && bc.logger.OnBlockchainInit != nil {
		bc.logger.OnBlockchainInit(chainConfig)
	}
	if bc.logger != nil && bc.logger.OnGenesisBlock != nil {
		if block := bc.CurrentBlock(); block.Number.Uint64() == 0 {
			alloc, err := getGenesisState(bc.db, block.Hash())
			if err != nil {
				return nil, fmt.Errorf("failed to get genesis state: %w", err)
			}
			if alloc == nil {
				return nil, errors.New("live blockchain tracer requires genesis alloc to be set")
			}
			bc.logger.OnGenesisBlock(bc.genesisBlock, alloc)
		}
	}
	bc.setupSnapshot()

	// Rewind the chain in case of an incompatible config upgrade.
	if compatErr != nil {
		log.Warn("Rewinding chain to upgrade configuration", "err", compatErr)
		if compatErr.RewindToTime > 0 {
			bc.SetHeadWithTimestamp(compatErr.RewindToTime)
		} else {
			bc.SetHead(compatErr.RewindToBlock)
		}

		rawdb.WriteChainConfig(db, genesisHash, chainConfig)
	}

	// Start tx indexer if it's enabled.
	// Disable tx indexer in stateless mode to avoid potential issues with pruning in stateless mode.
	if bc.cfg.TxLookupLimit >= 0 && !bc.cfg.Stateless {
		bc.txIndexer = newTxIndexer(uint64(bc.cfg.TxLookupLimit), bc)
	}

	// Start header verification loop
	bc.startHeaderVerificationLoop()

	// Start state size tracker
	if bc.cfg.StateSizeTracking {
		stateSizer, err := state.NewSizeTracker(bc.db, bc.triedb)
		if err == nil {
			bc.stateSizer = stateSizer
			log.Info("Enabled state size metrics")
		} else {
			log.Info("Failed to setup size tracker", "err", err)
		}
	}
	return bc, nil
}

// ParallelStatelessImportEnable enables parallel stateless import.
func (bc *BlockChain) ParallelStatelessImportEnable() {
	bc.parallelStatelessImportEnabled.Store(true)
}

// SetParallelStatelessImportWorkers sets the number of workers used by parallel stateless import.
func (bc *BlockChain) SetParallelStatelessImportWorkers(n int) {
	if n > 0 {
		bc.parallelStatelessImportWorkers = n
	}
}

// IsParallelStatelessImportEnabled returns true if parallel stateless import is currently enabled.
func (bc *BlockChain) IsParallelStatelessImportEnabled() bool {
	return bc.parallelStatelessImportEnabled.Load()
}

// NewParallelBlockChain is similar to NewBlockChain and creates a new blockchain object,
// but with a parallel state processor
func NewParallelBlockChain(db ethdb.Database, genesis *Genesis, engine consensus.Engine, cfg *BlockChainConfig, numprocs int, enforce bool) (*BlockChain, error) {
	bc, err := NewBlockChain(db, genesis, engine, cfg)
	if err != nil {
		return nil, err
	}

	bc.parallelProcessor = NewV2StateProcessor(bc.hc, bc, numprocs)
	bc.parallelSpeculativeProcesses = numprocs
	bc.enforceParallelProcessor = enforce

	return bc, nil
}

// fireBlockStart emits the OnBlockStart tracing event when a tracer is set.
func (bc *BlockChain) fireBlockStart(block *types.Block) {
	if bc.logger == nil || bc.logger.OnBlockStart == nil {
		return
	}
	td := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
	bc.logger.OnBlockStart(tracing.BlockEvent{
		Block:     block,
		TD:        td,
		Finalized: bc.CurrentFinalBlock(),
		Safe:      bc.CurrentSafeBlock(),
	})
}

// setupBlockReaders builds the three StateDBs needed for parallel block
// processing: throwaway (for prefetcher), statedb (for serial processor),
// and parallelStatedb (for V2).
func (bc *BlockChain) setupBlockReaders(parent *types.Header, pipeOpts *PipelineImportOpts) (
	throwaway, statedb, parallelStatedb *state.StateDB,
	prefetch, process, parallel state.ReaderWithStats, err error,
) {
	// Under pipelined import parent.Root may not be committed yet. Open
	// trie readers against the last committed root and install the FlatDiff
	// overlay below so execution still sees the previous block's post-state.
	readerRoot := pipelineReaderRoot(parent, pipeOpts)
	prefetch, process, parallel, err = bc.statedb.ReadersWithCacheStatsTriple(readerRoot)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	if throwaway, err = state.NewWithReader(readerRoot, bc.statedb, prefetch); err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	if statedb, err = state.NewWithReader(readerRoot, bc.statedb, process); err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	if parallelStatedb, err = state.NewWithReader(readerRoot, bc.statedb, parallel); err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	applyFlatDiffOverlayToAll(pipeOpts, throwaway, statedb, parallelStatedb)
	parallelStatedb.EnableConcurrentReads()
	return throwaway, statedb, parallelStatedb, prefetch, process, parallel, nil
}

func pipelineImportMode(pipeOpts *PipelineImportOpts) string {
	if pipeOpts == nil {
		return "disabled"
	}
	if pipeOpts.Mode != "" {
		return pipeOpts.Mode
	}
	if pipeOpts.FlatDiff != nil {
		return pipelineImportModeFlatDiff
	}
	return pipelineImportModeDirect
}

func pendingImportSRCCollected(pending *pendingImportSRCState) bool {
	if pending == nil {
		return false
	}
	select {
	case <-pending.collectedCh:
		return true
	default:
		return false
	}
}

func (p *pendingSRCState) markStarted(t time.Time) {
	if p != nil {
		p.startNanos.Store(t.UnixNano())
	}
}

func (p *pendingSRCState) markDone(t time.Time) {
	if p != nil {
		p.doneNanos.Store(t.UnixNano())
	}
}

func (p *pendingSRCState) executionOverlap(execStart, execEnd time.Time) time.Duration {
	if p == nil || execStart.IsZero() || execEnd.IsZero() || !execEnd.After(execStart) {
		return 0
	}
	srcStart := p.startNanos.Load()
	if srcStart == 0 {
		return 0
	}
	overlapStart := execStart.UnixNano()
	if srcStart > overlapStart {
		overlapStart = srcStart
	}
	overlapEnd := execEnd.UnixNano()
	if srcDone := p.doneNanos.Load(); srcDone != 0 && srcDone < overlapEnd {
		overlapEnd = srcDone
	}
	if overlapEnd <= overlapStart {
		return 0
	}
	return time.Duration(overlapEnd - overlapStart)
}

func recordPipelinedImportExecutionMetrics(pipeOpts *PipelineImportOpts, execStart, execEnd time.Time) {
	if pipeOpts == nil || execStart.IsZero() || execEnd.IsZero() || !execEnd.After(execStart) {
		return
	}
	execDuration := execEnd.Sub(execStart)
	pipelineImportExecutionTimer.Update(execDuration)

	if pipeOpts.pendingSRC == nil {
		return
	}
	src := pipeOpts.pendingSRC
	overlap := src.executionOverlap(execStart, execEnd)
	pipelineImportOverlapExecutionTimer.Update(overlap)
	if overlap > 0 {
		pipelineImportOverlapBlocksCounter.Inc(1)
		pipelineImportExecWithOverlapTimer.Update(execDuration)
	} else {
		pipelineImportNoOverlapBlocksCounter.Inc(1)
		pipelineImportExecNoOverlapTimer.Update(execDuration)
	}

	var percent int64
	if execDuration > 0 {
		percent = overlap.Nanoseconds() * 100 / execDuration.Nanoseconds()
		pipelineImportOverlapExecutionPercent.Update(percent)
	}
	recordExecutionOverlapBucket(percent, execDuration)

	// Record the classification so the SRC-side split can be emitted once the
	// SRC goroutine is collected (its wall-clock isn't final yet here).
	src.nextExecOverlapped.Store(overlap > 0)
	src.nextExecClassified.Store(true)
}

// recordExecutionOverlapBucket files this block's execution time into the
// bucket matching how much of it overlapped the previous SRC. percent is
// already clamped to 0..100 (overlap can't exceed execution duration).
func recordExecutionOverlapBucket(percent int64, execDuration time.Duration) {
	switch {
	case percent <= 0:
		pipelineImportExecOverlap0Timer.Update(execDuration)
	case percent < 25:
		pipelineImportExecOverlap1To25Timer.Update(execDuration)
	case percent < 50:
		pipelineImportExecOverlap25To50Timer.Update(execDuration)
	case percent < 75:
		pipelineImportExecOverlap50To75Timer.Update(execDuration)
	default:
		pipelineImportExecOverlap75To100Timer.Update(execDuration)
	}
}

// recordPipelinedImportSRCOverlapSplit files the SRC goroutine's full
// wall-clock into the with/no-next-exec-overlap timer. Called after collection,
// when doneNanos is final and the next block's execution has classified it.
func recordPipelinedImportSRCOverlapSplit(src *pendingSRCState) {
	if src == nil || !src.nextExecClassified.Load() {
		return
	}
	start := src.startNanos.Load()
	done := src.doneNanos.Load()
	// done == start is legitimate (sub-resolution SRC); only reject an
	// uninitialized start or an inverted window.
	if start == 0 || done < start {
		return
	}
	dur := time.Duration(done - start)
	if src.nextExecOverlapped.Load() {
		pipelineImportSRCWithNextExecTimer.Update(dur)
	} else {
		pipelineImportSRCNoNextExecTimer.Update(dur)
	}
}

func flatDiffLogAttrs(diff *state.FlatDiff) []interface{} {
	attrs := []interface{}{"hasFlatDiff", diff != nil}
	if diff == nil {
		return append(attrs,
			"flatdiffAccounts", 0,
			"flatdiffStorageAccounts", 0,
			"flatdiffStorageSlots", 0,
			"flatdiffReadSet", 0,
			"flatdiffReadStorageAccounts", 0,
			"flatdiffReadStorageSlots", 0,
			"flatdiffDestructs", 0,
			"flatdiffNonExistentReads", 0,
			"flatdiffCode", 0,
		)
	}
	storageSlots := 0
	for _, slots := range diff.Storage {
		storageSlots += len(slots)
	}
	readStorageSlots := 0
	for _, slots := range diff.ReadStorage {
		readStorageSlots += len(slots)
	}
	return append(attrs,
		"flatdiffAccounts", len(diff.Accounts),
		"flatdiffStorageAccounts", len(diff.Storage),
		"flatdiffStorageSlots", storageSlots,
		"flatdiffReadSet", len(diff.ReadSet),
		"flatdiffReadStorageAccounts", len(diff.ReadStorage),
		"flatdiffReadStorageSlots", readStorageSlots,
		"flatdiffDestructs", len(diff.Destructs),
		"flatdiffNonExistentReads", len(diff.NonExistentReads),
		"flatdiffCode", len(diff.Code),
	)
}

func pipelineImportLogAttrs(parent *types.Header, pipeOpts *PipelineImportOpts) []interface{} {
	parentNumber := uint64(0)
	parentHash := common.Hash{}
	parentRoot := common.Hash{}
	if parent != nil && parent.Number != nil {
		parentNumber = parent.Number.Uint64()
	}
	if parent != nil {
		parentHash = parent.Hash()
		parentRoot = parent.Root
	}
	attrs := []interface{}{
		"pipelineMode", pipelineImportMode(pipeOpts),
		"parent", parentNumber,
		"parentHash", parentHash,
		"parentRoot", parentRoot,
	}
	if pipeOpts == nil {
		return attrs
	}
	readerRoot := pipeOpts.CommittedParentRoot
	if parent != nil {
		readerRoot = pipelineReaderRoot(parent, pipeOpts)
	}
	attrs = append(attrs,
		"readerRoot", readerRoot,
		"committedParentRoot", pipeOpts.CommittedParentRoot,
	)
	if pipeOpts.PendingBlock != 0 {
		attrs = append(attrs,
			"pendingBlock", pipeOpts.PendingBlock,
			"pendingHash", pipeOpts.PendingHash,
			"pendingCollected", pipeOpts.PendingCollected,
		)
	}
	return append(attrs, flatDiffLogAttrs(pipeOpts.FlatDiff)...)
}

// reportReaderStats marks per-block cache hit/miss meters from prefetch,
// process, and parallel readers. Intended to be called via defer at the
// end of ProcessBlock.
//
// process and parallel both use the roleProcess label internally and
// share the same underlying cache, but ReadersWithCacheStatsTriple
// returns independent ReaderWithStats wrappers, so V2's reads accumulate
// in `parallel`'s atomic counters separately from V1's `process` counters.
// We merge them into the same meter set here so the cache-hit-rate
// dashboards reflect the work the winning processor (typically V2) did,
// rather than only the losing serial path's interrupted reads.
func reportReaderStats(prefetch, process, parallel state.ReaderWithStats) {
	stats := prefetch.GetStats()
	accountCacheHitPrefetchMeter.Mark(stats.AccountHit)
	accountCacheMissPrefetchMeter.Mark(stats.AccountMiss)
	storageCacheHitPrefetchMeter.Mark(stats.StorageHit)
	storageCacheMissPrefetchMeter.Mark(stats.StorageMiss)

	procStats := process.GetStats()
	parStats := parallel.GetStats()
	accountCacheHitMeter.Mark(procStats.AccountHit + parStats.AccountHit)
	accountCacheMissMeter.Mark(procStats.AccountMiss + parStats.AccountMiss)
	storageCacheHitMeter.Mark(procStats.StorageHit + parStats.StorageHit)
	storageCacheMissMeter.Mark(procStats.StorageMiss + parStats.StorageMiss)

	prefetchStats := prefetch.GetPrefetchStats()
	accountInsertPrefetchMeter.Mark(prefetchStats.AccountInsert)
	storageInsertPrefetchMeter.Mark(prefetchStats.StorageInsert)

	procPF := process.GetPrefetchStats()
	parPF := parallel.GetPrefetchStats()
	accountHitFromPrefetchMeter.Mark(procPF.AccountHitFromPrefetch + parPF.AccountHitFromPrefetch)
	storageHitFromPrefetchMeter.Mark(procPF.StorageHitFromPrefetch + parPF.StorageHitFromPrefetch)
	accountHitFromPrefetchUniqueMeter.Mark(procPF.AccountHitFromPrefetchUnique + parPF.AccountHitFromPrefetchUnique)
}

// sharedBlockCaches holds VM-level caches that are shared between the
// prefetcher goroutine and the V2 BlockSTM workers for a single block.
type sharedBlockCaches struct {
	jumpDests vm.JumpDestCache
	keccak    *sync.Map
	ecrecover *sync.Map
}

func newSharedBlockCaches() *sharedBlockCaches {
	return &sharedBlockCaches{
		jumpDests: vm.NewSyncJumpDestCache(),
		keccak:    &sync.Map{},
		ecrecover: &sync.Map{},
	}
}

// applyTo populates a vm.Config with the shared caches.
func (c *sharedBlockCaches) applyTo(cfg *vm.Config) {
	cfg.SharedJumpDestCache = c.jumpDests
	cfg.Keccak256Cache = c.keccak
	cfg.EcrecoverCache = c.ecrecover
}

// startPrefetchGoroutine launches the throwaway-statedb prefetcher in
// the background. It runs the block with tracing disabled to warm caches
// for the real processors.
func (bc *BlockChain) startPrefetchGoroutine(block *types.Block, throwaway *state.StateDB,
	caches *sharedBlockCaches, followupInterrupt *atomic.Bool) {
	go func(start time.Time) {
		vmCfg := bc.cfg.VmConfig
		vmCfg.Tracer = nil
		caches.applyTo(&vmCfg)
		bc.prefetcher.Prefetch(block, throwaway, vmCfg, false, followupInterrupt)
		blockPrefetchExecuteTimer.Update(time.Since(start))
		if followupInterrupt.Load() {
			blockPrefetchInterruptMeter.Mark(1)
		}
	}(time.Now())
}

func (bc *BlockChain) ProcessBlock(block *types.Block, parent *types.Header, witness *stateless.Witness, followupInterrupt *atomic.Bool, pipeOpts *PipelineImportOpts) (_ types.Receipts, _ []*types.Log, _ uint64, _ *state.StateDB, vtime time.Duration, blockEndErr error) {
	// Process the block using processor and parallelProcessor at the same time, take the one which finishes first, cancel the other, and return the result
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if followupInterrupt == nil {
		followupInterrupt = &atomic.Bool{}
	}
	bc.fireBlockStart(block)
	if bc.logger != nil && bc.logger.OnBlockEnd != nil {
		defer func() { bc.logger.OnBlockEnd(blockEndErr) }()
	}

	throwaway, statedb, parallelStatedb, prefetch, process, parallel, err := bc.setupBlockReaders(parent, pipeOpts)
	if err != nil {
		return nil, nil, 0, nil, 0, err
	}
	defer reportReaderStats(prefetch, process, parallel)

	// Shared caches for this block — used by both prefetcher and V2 workers.
	sharedCaches := newSharedBlockCaches()
	bc.startPrefetchGoroutine(block, throwaway, sharedCaches, followupInterrupt)

	type Result struct {
		receipts types.Receipts
		logs     []*types.Log
		usedGas  uint64
		err      error
		statedb  *state.StateDB
		counter  *metrics.Counter
		parallel bool
		vtime    time.Duration
		execFrom time.Time
		execTo   time.Time
	}

	var resultChanLen int = 2
	if bc.enforceParallelProcessor {
		log.Debug("Processing block using Block STM only", "number", block.NumberU64())
		resultChanLen = 1
	}
	resultChan := make(chan Result, resultChanLen)

	processorCount := 0
	execStart := time.Now()

	if bc.parallelProcessor != nil {
		processorCount++

		go func() {
			pstart := time.Now()
			parallelStatedb.StartPrefetcher("chain", witness, nil)
			v2VmCfg := bc.cfg.VmConfig
			sharedCaches.applyTo(&v2VmCfg)
			res, err := bc.parallelProcessor.Process(block, parallelStatedb, v2VmCfg, nil, ctx)
			pend := time.Now()
			blockExecutionParallelTimer.Update(pend.Sub(pstart))
			var localVtime time.Duration
			if err == nil {
				vstart := time.Now()
				err = validateStateForPipeline(bc.validator, block, parallelStatedb, res, pipeOpts)
				localVtime = time.Since(vstart)
			}
			// Stop prefetcher when either (a) ctx cancelled — we lost the race,
			// or (b) V2 errored out. In case (b) the fallback in ProcessBlock
			// overwrites this Result with V1's and decrements processorCount,
			// so the final drain at "processorCount == 2" won't fire and the
			// subfetcher goroutines would leak with live trie references that
			// the about-to-be-committed pathdb layer would invalidate.
			if err != nil || ctx.Err() != nil {
				parallelStatedb.StopPrefetcher()
			}
			if res == nil {
				res = &ProcessResult{}
			}
			resultChan <- Result{
				receipts: res.Receipts,
				logs:     res.Logs,
				usedGas:  res.GasUsed,
				err:      err,
				statedb:  parallelStatedb,
				counter:  blockExecutionParallelCounter,
				parallel: true,
				vtime:    localVtime,
				execFrom: pstart,
				execTo:   pend,
			}
		}()
	}

	if bc.processor != nil && !bc.enforceParallelProcessor {
		processorCount++

		go func() {
			pstart := time.Now()
			statedb.StartPrefetcher("chain", witness, nil)
			res, err := bc.processor.Process(block, statedb, bc.cfg.VmConfig, nil, ctx)
			pend := time.Now()
			blockExecutionSerialTimer.Update(pend.Sub(pstart))
			var localVtime time.Duration
			if err == nil {
				vstart := time.Now()
				err = validateStateForPipeline(bc.validator, block, statedb, res, pipeOpts)
				localVtime = time.Since(vstart)
			}
			if err != nil || ctx.Err() != nil {
				statedb.StopPrefetcher()
			}
			if res == nil {
				res = &ProcessResult{}
			}
			resultChan <- Result{
				receipts: res.Receipts,
				logs:     res.Logs,
				usedGas:  res.GasUsed,
				err:      err,
				statedb:  statedb,
				counter:  blockExecutionSerialCounter,
				parallel: false,
				vtime:    localVtime,
				execFrom: pstart,
				execTo:   pend,
			}
		}()
	}

	result := <-resultChan

	// If V2 returned an error (panic, ApplyMessage consensus error, etc.)
	// and the serial processor is also running, fall back to the serial
	// result BEFORE cancelling — cancelling first would interrupt the
	// still-running serial processor at its next tx boundary and the
	// fallback would receive context.Canceled instead of a usable
	// recovery. The fallback IS the recovery; it must run to completion.
	if result.parallel && result.err != nil {
		attrs := []interface{}{"number", block.NumberU64(), "hash", block.Hash(), "err", result.err}
		attrs = append(attrs, pipelineImportLogAttrs(parent, pipeOpts)...)
		log.Warn("Parallel state processor failed", attrs...)
		blockExecutionParallelErrorCounter.Inc(1)
		// Stop the failed V2 statedb's prefetcher before discarding the
		// result. The V2 goroutine only stops it on ctx cancellation, so a
		// V2-only error path (panic, ApplyMessage error, validate mismatch)
		// would otherwise leave trie prefetch work running across the
		// caller's commit — exactly the stale-layer scenario this code is
		// trying to avoid. Applies in both fallback (processorCount==2) and
		// enforce (processorCount==1) modes.
		result.statedb.StopPrefetcher()
		// If the parallel processor failed, we will fallback to the serial processor if enabled
		if processorCount == 2 {
			result = <-resultChan
			result.statedb.StopPrefetcher()
			processorCount--
		}
	}
	if result.err == nil {
		recordPipelinedImportExecutionMetrics(pipeOpts, result.execFrom, result.execTo)
	}

	// With the result we plan to keep in hand, cancel the shared context
	// so the loser (if any) stops at its next tx boundary, and signal the
	// throwaway prefetcher to stop. This must happen BEFORE ProcessBlock
	// returns, because the caller will commit the block (advancing the
	// pathdb layer), which would invalidate any trie references still
	// held by the loser's prefetcher.
	cancel()
	followupInterrupt.Store(true)

	result.counter.Inc(1)

	// Report per-block mgasps for the winning processor.
	// Value is scaled by 1000 (stored as µgasps) to preserve 3 decimal places,
	// e.g. 210.357 mgasps → 210357. Divide by 1000 when reading.
	// Exclude sprint-end blocks (with state sync tx) — their Finalize overhead
	// (Heimdall state sync ~164ms) distorts the execution throughput metric.
	hasStateSync := false
	if txs := block.Transactions(); len(txs) > 0 {
		hasStateSync = txs[len(txs)-1].Type() == types.StateSyncTxType
	}
	if elapsed := time.Since(execStart); elapsed > 0 && result.usedGas > 0 && !hasStateSync && result.err == nil {
		mgasps := int64(float64(result.usedGas) * 1e6 / float64(elapsed)) // µgasps (mgasps * 1000)
		blockMgaspsMeter.Update(mgasps)
	}

	// Wait for the losing processor to finish and stop its prefetcher.
	// Must be synchronous: the caller will commit the block (advancing the
	// pathdb layer), which invalidates trie references held by the loser's
	// prefetcher subfetchers. The context is already cancelled and both V1
	// and V2 honour it at task-boundary level (V1 in its task loop; V2 in
	// the executor's dispatcher and validation loop), so the loser stops
	// promptly — typically within one tx execution.
	if processorCount == 2 {
		second_result := <-resultChan
		second_result.statedb.StopPrefetcher()
	}

	return result.receipts, result.logs, result.usedGas, result.statedb, result.vtime, result.err
}

func (bc *BlockChain) setupSnapshot() {
	// Short circuit if the chain is established with path scheme, as the
	// state snapshot has been integrated into path database natively.
	if bc.cfg.StateScheme == rawdb.PathScheme {
		return
	}
	// Load any existing snapshot, regenerating it if loading failed
	if bc.cfg.SnapshotLimit > 0 {
		// If the chain was rewound past the snapshot persistent layer (causing
		// a recovery block number to be persisted to disk), check if we're still
		// in recovery mode and in that case, don't invalidate the snapshot on a
		// head mismatch.
		var recover bool
		head := bc.CurrentBlock()
		if layer := rawdb.ReadSnapshotRecoveryNumber(bc.db); layer != nil && *layer >= head.Number.Uint64() {
			log.Warn("Enabling snapshot recovery", "chainhead", head.Number, "diskbase", *layer)
			recover = true
		}
		snapconfig := snapshot.Config{
			CacheSize:  bc.cfg.SnapshotLimit,
			Recovery:   recover,
			NoBuild:    bc.cfg.SnapshotNoBuild,
			AsyncBuild: !bc.cfg.SnapshotWait,
		}
		bc.snaps, _ = snapshot.New(snapconfig, bc.db, bc.triedb, head.Root)

		// Re-initialize the state database with snapshot
		bc.statedb = state.NewDatabase(bc.triedb, bc.snaps)
	}
}

// empty returns an indicator whether the blockchain is empty.
// Note, it's a special case that we connect a non-empty ancient
// database with an empty node, so that we can plugin the ancient
// into node seamlessly.
func (bc *BlockChain) empty() bool {
	genesis := bc.genesisBlock.Hash()
	for _, hash := range []common.Hash{rawdb.ReadHeadBlockHash(bc.db), rawdb.ReadHeadHeaderHash(bc.db), rawdb.ReadHeadFastBlockHash(bc.db)} {
		if hash != genesis {
			return false
		}
	}

	return true
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
func (bc *BlockChain) loadLastState() error {
	// Restore the last known head block
	head := rawdb.ReadHeadBlockHash(bc.db)
	if head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		log.Warn("Empty database, resetting chain")
		return bc.Reset()
	}
	headHeader := bc.GetHeaderByHash(head)
	if headHeader == nil {
		// Corrupt or empty database, init from scratch
		log.Warn("Head header missing, resetting chain", "hash", head)
		return bc.Reset()
	}

	var headBlock *types.Block
	if cmp := headHeader.Number.Cmp(new(big.Int)); cmp == 1 {
		// Make sure the entire head block is available.
		headBlock = bc.GetBlockByHash(head)
	} else if cmp == 0 {
		// On a pruned node the block body might not be available. But a pruned
		// block should never be the head block. The only exception is when, as
		// a last resort, chain is reset to genesis.
		headBlock = bc.genesisBlock
	}
	if headBlock == nil {
		// Corrupt or empty database, init from scratch
		log.Warn("Head block missing, resetting chain", "hash", head)
		return bc.Reset()
	}
	// Everything seems to be fine, set as the head block
	bc.currentBlock.Store(headHeader)
	headBlockGauge.Update(int64(headBlock.NumberU64()))

	// Restore the last known head header
	if head := rawdb.ReadHeadHeaderHash(bc.db); head != (common.Hash{}) {
		if header := bc.GetHeaderByHash(head); header != nil {
			headHeader = header
		}
	}

	bc.hc.SetCurrentHeader(headHeader)

	// Initialize history pruning.
	latest := max(headBlock.NumberU64(), headHeader.Number.Uint64())
	if err := bc.initializeHistoryPruning(latest); err != nil {
		return err
	}

	// Restore the last known head snap block
	bc.currentSnapBlock.Store(headBlock.Header())
	headFastBlockGauge.Update(int64(headBlock.NumberU64()))

	if head := rawdb.ReadHeadFastBlockHash(bc.db); head != (common.Hash{}) {
		if block := bc.GetBlockByHash(head); block != nil {
			bc.currentSnapBlock.Store(block.Header())
			headFastBlockGauge.Update(int64(block.NumberU64()))
		}
	}

	// Restore the last known finalized block and safe block
	// Note: the safe block is not stored on disk and it is set to the last
	// known finalized block on startup
	if head := rawdb.ReadFinalizedBlockHash(bc.db); head != (common.Hash{}) {
		if block := bc.GetBlockByHash(head); block != nil {
			bc.currentFinalBlock.Store(block.Header())
			headFinalizedBlockGauge.Update(int64(block.NumberU64()))
			bc.currentSafeBlock.Store(block.Header())
			headSafeBlockGauge.Update(int64(block.NumberU64()))
		}
	}

	// Issue a status log for the user
	var (
		currentSnapBlock  = bc.CurrentSnapBlock()
		currentFinalBlock = bc.CurrentFinalBlock()

		headerTd = bc.GetTd(headHeader.Hash(), headHeader.Number.Uint64())
		blockTd  = bc.GetTd(headBlock.Hash(), headBlock.NumberU64())
	)

	if headHeader.Hash() != headBlock.Hash() {
		log.Info("Loaded most recent local header", "number", headHeader.Number, "hash", headHeader.Hash(), "td", headerTd, "age", common.PrettyAge(time.Unix(int64(headHeader.Time), 0)))
	}
	log.Info("Loaded most recent local block", "number", headBlock.Number(), "hash", headBlock.Hash(), "td", blockTd, "age", common.PrettyAge(time.Unix(int64(headBlock.Time()), 0)))
	if headBlock.Hash() != currentSnapBlock.Hash() {
		snapTd := bc.GetTd(currentSnapBlock.Hash(), currentSnapBlock.Number.Uint64())
		log.Info("Loaded most recent local snap block", "number", currentSnapBlock.Number, "hash", currentSnapBlock.Hash(), "td", snapTd, "age", common.PrettyAge(time.Unix(int64(currentSnapBlock.Time), 0)))
	}

	if currentFinalBlock != nil {
		finalTd := bc.GetTd(currentFinalBlock.Hash(), currentFinalBlock.Number.Uint64())
		log.Info("Loaded most recent local finalized block", "number", currentFinalBlock.Number, "hash", currentFinalBlock.Hash(), "td", finalTd, "age", common.PrettyAge(time.Unix(int64(currentFinalBlock.Time), 0)))
	}

	if pivot := rawdb.ReadLastPivotNumber(bc.db); pivot != nil {
		log.Info("Loaded last snap-sync pivot marker", "number", *pivot)
	}
	if pruning := bc.historyPrunePoint.Load(); pruning != nil {
		log.Info("Chain history is pruned", "earliest", pruning.BlockNumber, "hash", pruning.BlockHash)
	}
	return nil
}

// initializeHistoryPruning sets bc.historyPrunePoint.
func (bc *BlockChain) initializeHistoryPruning(latest uint64) error {
	freezerTail, _ := bc.db.Tail()

	switch bc.cfg.ChainHistoryMode {
	case history.KeepAll:
		if freezerTail == 0 {
			return nil
		}
		// The database was pruned somehow, so we need to figure out if it's a known
		// configuration or an error.
		predefinedPoint := history.PrunePoints[bc.genesisBlock.Hash()]
		if predefinedPoint == nil || freezerTail != predefinedPoint.BlockNumber {
			log.Error("Chain history database is pruned with unknown configuration", "tail", freezerTail)
			return errors.New("unexpected database tail")
		}
		bc.historyPrunePoint.Store(predefinedPoint)
		return nil

	// nolint:staticcheck
	case history.KeepPostMerge:
		if freezerTail == 0 && latest != 0 {
			// This is the case where a user is trying to run with --history.chain
			// postmerge directly on an existing DB. We could just trigger the pruning
			// here, but it'd be a bit dangerous since they may not have intended this
			// action to happen. So just tell them how to do it.
			log.Error(fmt.Sprintf("Chain history mode is configured as %q, but database is not pruned.", bc.cfg.ChainHistoryMode.String()))
			log.Error(fmt.Sprintf("Run 'geth prune-history' to prune pre-merge history."))
			return errors.New("history pruning requested via configuration")
		}
		predefinedPoint := history.PrunePoints[bc.genesisBlock.Hash()]
		if predefinedPoint == nil {
			log.Error("Chain history pruning is not supported for this network", "genesis", bc.genesisBlock.Hash())
			return errors.New("history pruning requested for unknown network")
		} else if freezerTail > 0 && freezerTail != predefinedPoint.BlockNumber {
			log.Error("Chain history database is pruned to unknown block", "tail", freezerTail)
			return errors.New("unexpected database tail")
		}
		bc.historyPrunePoint.Store(predefinedPoint)
		return nil

	default:
		return fmt.Errorf("invalid history mode: %d", bc.cfg.ChainHistoryMode)
	}
}

// SetHead rewinds the local chain to a new head. Depending on whether the node
// was snap synced or full synced and in which state, the method will try to
// delete minimal data from disk whilst retaining chain consistency.
func (bc *BlockChain) SetHead(head uint64) error {
	if _, err := bc.setHeadBeyondRoot(head, 0, common.Hash{}, false); err != nil {
		return err
	}
	// Send chain head event to update the transaction pool
	header := bc.CurrentBlock()
	if block := bc.GetBlock(header.Hash(), header.Number.Uint64()); block == nil {
		// In a pruned node the genesis block will not exist in the freezer.
		// It should not happen that we set head to any other pruned block.
		if header.Number.Uint64() > 0 {
			// This should never happen. In practice, previously currentBlock
			// contained the entire block whereas now only a "marker", so there
			// is an ever so slight chance for a race we should handle.
			log.Error("Current block not found in database", "block", header.Number, "hash", header.Hash())
			return fmt.Errorf("current block missing: #%d [%x..]", header.Number, header.Hash().Bytes()[:4])
		}
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: header})
	return nil
}

// SetHeadWithTimestamp rewinds the local chain to a new head that has at max
// the given timestamp. Depending on whether the node was snap synced or full
// synced and in which state, the method will try to delete minimal data from
// disk whilst retaining chain consistency.
func (bc *BlockChain) SetHeadWithTimestamp(timestamp uint64) error {
	if _, err := bc.setHeadBeyondRoot(0, timestamp, common.Hash{}, false); err != nil {
		return err
	}
	// Send chain head event to update the transaction pool
	header := bc.CurrentBlock()
	if block := bc.GetBlock(header.Hash(), header.Number.Uint64()); block == nil {
		// In a pruned node the genesis block will not exist in the freezer.
		// It should not happen that we set head to any other pruned block.
		if header.Number.Uint64() > 0 {
			// This should never happen. In practice, previously currentBlock
			// contained the entire block whereas now only a "marker", so there
			// is an ever so slight chance for a race we should handle.
			log.Error("Current block not found in database", "block", header.Number, "hash", header.Hash())
			return fmt.Errorf("current block missing: #%d [%x..]", header.Number, header.Hash().Bytes()[:4])
		}
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: header})
	return nil
}

// SetFinalized sets the finalized block.
func (bc *BlockChain) SetFinalized(header *types.Header) {
	bc.currentFinalBlock.Store(header)

	if header != nil {
		rawdb.WriteFinalizedBlockHash(bc.db, header.Hash())
		headFinalizedBlockGauge.Update(int64(header.Number.Uint64()))
	} else {
		rawdb.WriteFinalizedBlockHash(bc.db, common.Hash{})
		headFinalizedBlockGauge.Update(0)
	}
}

// SetSafe sets the safe block.
func (bc *BlockChain) SetSafe(header *types.Header) {
	bc.currentSafeBlock.Store(header)

	if header != nil {
		headSafeBlockGauge.Update(int64(header.Number.Uint64()))
	} else {
		headSafeBlockGauge.Update(0)
	}
}

// rewindHashHead implements the logic of rewindHead in the context of hash scheme.
func (bc *BlockChain) rewindHashHead(head *types.Header, root common.Hash) (*types.Header, uint64) {
	var (
		limit      uint64                             // The oldest block that will be searched for this rewinding
		beyondRoot = root == common.Hash{}            // Flag whether we're beyond the requested root (no root, always true)
		pivot      = rawdb.ReadLastPivotNumber(bc.db) // Associated block number of pivot point state
		rootNumber uint64                             // Associated block number of requested root

		start  = time.Now() // Timestamp the rewinding is restarted
		logged = time.Now() // Timestamp last progress log was printed
	)
	// The oldest block to be searched is determined by the pivot block or a constant
	// searching threshold. The rationale behind this is as follows:
	//
	// - Snap sync is selected if the pivot block is available. The earliest available
	//   state is the pivot block itself, so there is no sense in going further back.
	//
	// - Full sync is selected if the pivot block does not exist. The hash database
	//   periodically flushes the state to disk, and the used searching threshold is
	//   considered sufficient to find a persistent state, even for the testnet. It
	//   might be not enough for a chain that is nearly empty. In the worst case,
	//   the entire chain is reset to genesis, and snap sync is re-enabled on top,
	//   which is still acceptable.
	if pivot != nil {
		limit = *pivot
	} else if head.Number.Uint64() > params.FullImmutabilityThreshold {
		limit = head.Number.Uint64() - params.FullImmutabilityThreshold
	}
	for {
		logger := log.Trace
		if time.Since(logged) > time.Second*8 {
			logged = time.Now()
			logger = log.Info
		}
		logger("Block state missing, rewinding further", "number", head.Number, "hash", head.Hash(), "elapsed", common.PrettyDuration(time.Since(start)))

		// If a root threshold was requested but not yet crossed, check
		if !beyondRoot && head.Root == root {
			beyondRoot, rootNumber = true, head.Number.Uint64()
		}
		// If search limit is reached, return the genesis block as the
		// new chain head.
		if head.Number.Uint64() < limit {
			log.Info("Rewinding limit reached, resetting to genesis", "number", head.Number, "hash", head.Hash(), "limit", limit)
			return bc.genesisBlock.Header(), rootNumber
		}
		// If the associated state is not reachable, continue searching
		// backwards until an available state is found.
		if !bc.HasState(head.Root) {
			// If the chain is gapped in the middle, return the genesis
			// block as the new chain head.
			parent := bc.GetHeader(head.ParentHash, head.Number.Uint64()-1)
			if parent == nil {
				log.Error("Missing block in the middle, resetting to genesis", "number", head.Number.Uint64()-1, "hash", head.ParentHash)
				return bc.genesisBlock.Header(), rootNumber
			}
			head = parent

			// If the genesis block is reached, stop searching.
			if head.Number.Uint64() == 0 {
				log.Info("Genesis block reached", "number", head.Number, "hash", head.Hash())
				return head, rootNumber
			}
			continue // keep rewinding
		}
		// Once the available state is found, ensure that the requested root
		// has already been crossed. If not, continue rewinding.
		if beyondRoot || head.Number.Uint64() == 0 {
			log.Info("Rewound to block with state", "number", head.Number, "hash", head.Hash())
			return head, rootNumber
		}
		log.Debug("Skipping block with threshold state", "number", head.Number, "hash", head.Hash(), "root", head.Root)
		head = bc.GetHeader(head.ParentHash, head.Number.Uint64()-1) // Keep rewinding
	}
}

// rewindPathHead implements the logic of rewindHead in the context of path scheme.
func (bc *BlockChain) rewindPathHead(head *types.Header, root common.Hash) (*types.Header, uint64) {
	var (
		pivot      = rawdb.ReadLastPivotNumber(bc.db) // Associated block number of pivot block
		rootNumber uint64                             // Associated block number of requested root

		// BeyondRoot represents whether the requested root is already
		// crossed. The flag value is set to true if the root is empty.
		beyondRoot = root == common.Hash{}

		// noState represents if the target state requested for search
		// is unavailable and impossible to be recovered.
		noState = !bc.HasState(root) && !bc.stateRecoverable(root)

		start  = time.Now() // Timestamp the rewinding is restarted
		logged = time.Now() // Timestamp last progress log was printed
	)
	// Rewind the head block tag until an available state is found.
	for {
		logger := log.Trace
		if time.Since(logged) > time.Second*8 {
			logged = time.Now()
			logger = log.Info
		}
		logger("Block state missing, rewinding further", "number", head.Number, "hash", head.Hash(), "elapsed", common.PrettyDuration(time.Since(start)))

		// If a root threshold was requested but not yet crossed, check
		if !beyondRoot && head.Root == root {
			beyondRoot, rootNumber = true, head.Number.Uint64()
		}
		// If the root threshold hasn't been crossed but the available
		// state is reached, quickly determine if the target state is
		// possible to be reached or not.
		if !beyondRoot && noState && bc.HasState(head.Root) {
			beyondRoot = true
			log.Info("Disable the search for unattainable state", "root", root)
		}
		// Check if the associated state is available or recoverable if
		// the requested root has already been crossed.
		if beyondRoot && (bc.HasState(head.Root) || bc.stateRecoverable(head.Root)) {
			break
		}
		// If pivot block is reached, return the genesis block as the
		// new chain head. Theoretically there must be a persistent
		// state before or at the pivot block, prevent endless rewinding
		// towards the genesis just in case.
		if pivot != nil && *pivot >= head.Number.Uint64() {
			log.Info("Pivot block reached, resetting to genesis", "number", head.Number, "hash", head.Hash())
			return bc.genesisBlock.Header(), rootNumber
		}
		// If the chain is gapped in the middle, return the genesis
		// block as the new chain head
		parent := bc.GetHeader(head.ParentHash, head.Number.Uint64()-1) // Keep rewinding
		if parent == nil {
			log.Error("Missing block in the middle, resetting to genesis", "number", head.Number.Uint64()-1, "hash", head.ParentHash)
			return bc.genesisBlock.Header(), rootNumber
		}
		head = parent

		// If the genesis block is reached, stop searching.
		if head.Number.Uint64() == 0 {
			log.Info("Genesis block reached", "number", head.Number, "hash", head.Hash())
			return head, rootNumber
		}
	}
	// Recover if the target state if it's not available yet.
	if !bc.HasState(head.Root) {
		if err := bc.triedb.Recover(head.Root); err != nil {
			log.Crit("Failed to rollback state", "err", err)
		}
	}
	log.Info("Rewound to block with state", "number", head.Number, "hash", head.Hash())
	return head, rootNumber
}

// rewindHead searches the available states in the database and returns the associated
// block as the new head block.
//
// If the given root is not empty, then the rewind should attempt to pass the specified
// state root and return the associated block number as well. If the root, typically
// representing the state corresponding to snapshot disk layer, is deemed impassable,
// then block number zero is returned, indicating that snapshot recovery is disabled
// and the whole snapshot should be auto-generated in case of head mismatch.
func (bc *BlockChain) rewindHead(head *types.Header, root common.Hash) (*types.Header, uint64) {
	if bc.triedb.Scheme() == rawdb.PathScheme {
		return bc.rewindPathHead(head, root)
	}
	return bc.rewindHashHead(head, root)
}

// setHeadBeyondRoot rewinds the local chain to a new head with the extra condition
// that the rewind must pass the specified state root. This method is meant to be
// used when rewinding with snapshots enabled to ensure that we go back further than
// persistent disk layer. Depending on whether the node was snap synced or full, and
// in which state, the method will try to delete minimal data from disk whilst
// retaining chain consistency.
//
// The method also works in timestamp mode if `head == 0` but `time != 0`. In that
// case blocks are rolled back until the new head becomes older or equal to the
// requested time. If both `head` and `time` is 0, the chain is rewound to genesis.
//
// The method returns the block number where the requested root cap was found.
// nolint:gocognit
func (bc *BlockChain) setHeadBeyondRoot(head uint64, time uint64, root common.Hash, repair bool) (uint64, error) {
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	var (
		// Track the block number of the requested root hash
		rootNumber uint64 // (no root == always 0)

		// Retrieve the last pivot block to short circuit rollbacks beyond it
		// and the current freezer limit to start nuking it's underflown.
		pivot = rawdb.ReadLastPivotNumber(bc.db)
	)
	updateFn := func(db ethdb.KeyValueWriter, header *types.Header) (*types.Header, bool) {
		// Rewind the blockchain, ensuring we don't end up with a stateless head
		// block. Note, depth equality is permitted to allow using SetHead as a
		// chain reparation mechanism without deleting any data!
		// nolint:nestif
		if currentBlock := bc.CurrentBlock(); currentBlock != nil && header.Number.Uint64() <= currentBlock.Number.Uint64() {
			var newHeadBlock *types.Header
			if !bc.cfg.Stateless {
				newHeadBlock, rootNumber = bc.rewindHead(header, root)
			} else {
				newHeadBlock = header
				rootNumber = header.Number.Uint64()
			}
			rawdb.WriteHeadBlockHash(db, newHeadBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe to update in-memory markers directly.
			bc.currentBlock.Store(newHeadBlock)
			headBlockGauge.Update(int64(newHeadBlock.Number.Uint64()))

			// The head state is missing, which is only possible in the path-based
			// scheme. This situation occurs when the chain head is rewound below
			// the pivot point. In this scenario, there is no possible recovery
			// approach except for rerunning a snap sync. Do nothing here until the
			// state syncer picks it up.
			// Skip state checking for stateless nodes
			if !bc.cfg.Stateless && !bc.HasState(newHeadBlock.Root) {
				if newHeadBlock.Number.Uint64() != 0 {
					log.Crit("Chain is stateless at a non-genesis block")
				}
				log.Info("Chain is stateless, wait state sync", "number", newHeadBlock.Number, "hash", newHeadBlock.Hash())
			}
		}
		// Rewind the snap block in a simpleton way to the target head
		if currentSnapBlock := bc.CurrentSnapBlock(); currentSnapBlock != nil && header.Number.Uint64() < currentSnapBlock.Number.Uint64() {
			newHeadSnapBlock := bc.GetBlock(header.Hash(), header.Number.Uint64())
			// If either blocks reached nil, reset to the genesis state
			if newHeadSnapBlock == nil {
				newHeadSnapBlock = bc.genesisBlock
			}

			rawdb.WriteHeadFastBlockHash(db, newHeadSnapBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory, we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe the update in-memory markers directly.
			bc.currentSnapBlock.Store(newHeadSnapBlock.Header())
			headFastBlockGauge.Update(int64(newHeadSnapBlock.NumberU64()))
		}

		var (
			headHeader = bc.CurrentBlock()
			headNumber = headHeader.Number.Uint64()
		)
		// If setHead underflown the freezer threshold and the block processing
		// intent afterwards is full block importing, delete the chain segment
		// between the stateful-block and the sethead target.
		var wipe bool
		frozen, _ := bc.db.Ancients()
		if headNumber+1 < frozen {
			wipe = pivot == nil || headNumber >= *pivot
		}

		return headHeader, wipe // Only force wipe if full synced
	}
	// Rewind the header chain, deleting all block bodies until then
	delFn := func(db ethdb.KeyValueWriter, hash common.Hash, num uint64) {
		// Ignore the error here since light client won't hit this path
		frozen, _ := bc.db.Ancients()
		if num+1 <= frozen {
			// Truncate all relative data(header, total difficulty, body, receipt
			// and canonical hash) from ancient store.
			if _, err := bc.db.TruncateHead(num); err != nil {
				log.Crit("Failed to truncate ancient data", "number", num, "err", err)
			}
			// Remove the hash <-> number mapping from the active store.
			rawdb.DeleteHeaderNumber(db, hash)
		} else {
			// Remove the associated body and receipts from the key-value store.
			// The header, hash-to-number mapping, and canonical hash will be
			// removed by the hc.SetHead function.
			rawdb.DeleteBody(db, hash, num)
			rawdb.DeleteReceipts(db, hash, num)
			rawdb.DeleteBorReceipt(db, hash, num)
			rawdb.DeleteBorTxLookupEntry(db, hash, num)
		}
		// Todo(rjl493456442) txlookup, log index, etc
	}
	// If SetHead was only called as a chain reparation method, try to skip
	// touching the header chain altogether, unless the freezer is broken
	if repair {
		if target, force := updateFn(bc.db, bc.CurrentBlock()); force {
			bc.hc.SetHead(target.Number.Uint64(), nil, delFn)
		}
	} else {
		// Rewind the chain to the requested head and keep going backwards until a
		// block with a state is found or snap sync pivot is passed
		if time > 0 {
			log.Warn("Rewinding blockchain to timestamp", "target", time)
			bc.hc.SetHeadWithTimestamp(time, updateFn, delFn)
		} else {
			log.Warn("Rewinding blockchain to block", "target", head)
			bc.hc.SetHead(head, updateFn, delFn)
		}
	}
	// Clear out any stale content from the caches
	bc.bodyCache.Purge()
	bc.bodyRLPCache.Purge()
	bc.receiptsCache.Purge()
	bc.blockCache.Purge()
	bc.txLookupCache.Purge()
	bc.witnessCache.Purge()
	bc.borReceiptsCache.Purge()

	// Clear safe block, finalized block if needed
	if safe := bc.CurrentSafeBlock(); safe != nil && head < safe.Number.Uint64() {
		log.Warn("SetHead invalidated safe block")
		bc.SetSafe(nil)
	}

	if finalized := bc.CurrentFinalBlock(); finalized != nil && head < finalized.Number.Uint64() {
		log.Error("SetHead invalidated finalized block")
		bc.SetFinalized(nil)
	}

	return rootNumber, bc.loadLastState()
}

// SnapSyncCommitHead sets the current head block to the one defined by the hash
// irrelevant what the chain contents were prior.
func (bc *BlockChain) SnapSyncCommitHead(hash common.Hash) error {
	// Make sure that both the block as well at its state trie exists
	block := bc.GetBlockByHash(hash)
	if block == nil {
		return fmt.Errorf("non existent block [%x..]", hash[:4])
	}
	// Reset the trie database with the fresh snap synced state.
	root := block.Root()
	if bc.triedb.Scheme() == rawdb.PathScheme {
		if err := bc.triedb.Enable(root); err != nil {
			return err
		}
	}
	if !bc.HasState(root) {
		return fmt.Errorf("non existent state [%x..]", root[:4])
	}
	// If all checks out, manually set the head block.
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}

	bc.currentBlock.Store(block.Header())
	headBlockGauge.Update(int64(block.NumberU64()))
	bc.chainmu.Unlock()

	// Destroy any existing state snapshot and regenerate it in the background,
	// also resuming the normal maintenance of any previously paused snapshot.
	if bc.snaps != nil {
		bc.snaps.Rebuild(root)
	}

	log.Info("Committed new head block", "number", block.Number(), "hash", hash)

	return nil
}

// Reset purges the entire blockchain, restoring it to its genesis state.
func (bc *BlockChain) Reset() error {
	return bc.ResetWithGenesisBlock(bc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
func (bc *BlockChain) ResetWithGenesisBlock(genesis *types.Block) error {
	// Dump the entire block chain and purge the caches
	if err := bc.SetHead(0); err != nil {
		return err
	}

	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	batch := bc.db.NewBatch()
	rawdb.WriteTd(batch, genesis.Hash(), genesis.NumberU64(), genesis.Difficulty())
	rawdb.WriteBlock(batch, genesis)

	if err := batch.Write(); err != nil {
		log.Crit("Failed to write genesis block", "err", err)
	}

	bc.writeHeadBlock(genesis)

	// Last update all in-memory chain markers
	bc.genesisBlock = genesis
	bc.currentBlock.Store(bc.genesisBlock.Header())
	headBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	bc.hc.SetGenesis(bc.genesisBlock.Header())
	bc.hc.SetCurrentHeader(bc.genesisBlock.Header())
	bc.currentSnapBlock.Store(bc.genesisBlock.Header())
	headFastBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))

	// Reset history pruning status.
	return bc.initializeHistoryPruning(0)
}

// Export writes the active chain to the given writer.
func (bc *BlockChain) Export(w io.Writer) error {
	return bc.ExportN(w, uint64(0), bc.CurrentBlock().Number.Uint64())
}

// ExportN writes a subset of the active chain to the given writer.
func (bc *BlockChain) ExportN(w io.Writer, first uint64, last uint64) error {
	if first > last {
		return fmt.Errorf("export failed: first (%d) is greater than last (%d)", first, last)
	}

	log.Info("Exporting batch of blocks", "count", last-first+1)

	var (
		parentHash common.Hash
		start      = time.Now()
		reported   = time.Now()
	)

	for nr := first; nr <= last; nr++ {
		block := bc.GetBlockByNumber(nr)
		if block == nil {
			return fmt.Errorf("export failed on #%d: not found", nr)
		}

		if nr > first && block.ParentHash() != parentHash {
			return errors.New("export failed: chain reorg during export")
		}

		parentHash = block.Hash()

		if err := block.EncodeRLP(w); err != nil {
			return err
		}

		if time.Since(reported) >= statsReportLimit {
			log.Info("Exporting blocks", "exported", block.NumberU64()-first, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}

	return nil
}

// writeHeadBlock injects a new head block into the current block chain. This method
// assumes that the block is indeed a true head. It will also reset the head
// header and the head snap sync block to this very same block if they are older
// or if they are on a different side chain.
//
// Note, this function assumes that the `mu` mutex is held!
func (bc *BlockChain) writeHeadBlock(block *types.Block) {
	// Add the block to the canonical chain number scheme and mark as the head
	batch := bc.db.NewBatch()
	rawdb.WriteHeadHeaderHash(batch, block.Hash())
	rawdb.WriteHeadFastBlockHash(batch, block.Hash())
	rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, block)
	rawdb.WriteHeadBlockHash(batch, block.Hash())

	// Flush the whole batch into the disk, exit the node if failed
	if err := batch.Write(); err != nil {
		log.Crit("Failed to update chain indexes and markers", "err", err)
	}
	// Update all in-memory chain markers in the last step
	bc.hc.SetCurrentHeader(block.Header())

	bc.currentSnapBlock.Store(block.Header())
	headFastBlockGauge.Update(int64(block.NumberU64()))

	bc.currentBlock.Store(block.Header())
	headBlockGauge.Update(int64(block.NumberU64()))
}

// stopWithoutSaving stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt. This method stops all running
// goroutines, but does not do all the post-stop work of persisting data.
// OBS! It is generally recommended to use the Stop method!
// This method has been exposed to allow tests to stop the blockchain while simulating
// a crash.
func (bc *BlockChain) stopWithoutSaving() {
	if !bc.stopping.CompareAndSwap(false, true) {
		return
	}
	// Signal shutdown tx indexer.
	if bc.txIndexer != nil {
		bc.txIndexer.close()
	}
	// Unsubscribe all subscriptions registered from blockchain.
	bc.scope.Close()

	close(bc.quit)

	// Signal shutdown to all goroutines.
	bc.InterruptInsert(true)

	// Stop state size tracker
	if bc.stateSizer != nil {
		bc.stateSizer.Stop()
	}
	// Flush any pending import SRC before waiting for goroutines. No rollback
	// here — this path doesn't hold chainmu, and the startup rewind moves the
	// head off an unverified block.
	if err := bc.flushPendingImportSRC(false); err != nil {
		log.Error("Failed to flush pending import SRC during shutdown", "err", err)
	}

	// Now wait for all chain modifications to end and persistent goroutines to exit.
	//
	// Note: Close waits for the mutex to become available, i.e. any running chain
	// modification will have exited when Close returns. Since we also called StopInsert,
	// the mutex should become available quickly. It cannot be taken again after Close has
	// returned.
	bc.chainmu.Close()
	bc.wg.Wait()
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
func (bc *BlockChain) Stop() {
	bc.stopWithoutSaving()

	// Ensure that the entirety of the state snapshot is journaled to disk.
	var snapBase common.Hash

	if bc.snaps != nil {
		var err error
		if snapBase, err = bc.snaps.Journal(bc.CurrentBlock().Root); err != nil {
			log.Error("Failed to journal state snapshot", "err", err)
		}
		bc.snaps.Release()
	}
	if bc.triedb.Scheme() == rawdb.PathScheme {
		// Ensure that the in-memory trie nodes are journaled to disk properly.
		if !bc.cfg.Stateless {
			if err := bc.triedb.Journal(bc.CurrentBlock().Root); err != nil {
				log.Info("Failed to journal in-memory trie nodes", "err", err)
			}
		}
	} else {
		// Ensure the state of a recent block is also stored to disk before exiting.
		// We're writing three different states to catch different restart scenarios:
		//  - HEAD:     So we don't need to reprocess any blocks in the general case
		//  - HEAD-1:   So we don't do large reorgs if our HEAD becomes an uncle
		//  - HEAD-127: So we have a hard limit on the number of blocks reexecuted
		if !bc.cfg.ArchiveMode {
			triedb := bc.triedb

			triesInMemory := bc.cfg.GetTriesInMemory()
			for _, offset := range []uint64{0, 1, triesInMemory - 1} {
				if number := bc.CurrentBlock().Number.Uint64(); number > offset {
					recent := bc.GetBlockByNumber(number - offset)

					log.Info("Writing cached state to disk", "block", recent.Number(), "hash", recent.Hash(), "root", recent.Root())
					if err := triedb.Commit(recent.Root(), true); err != nil {
						log.Error("Failed to commit recent state trie", "err", err)
					}
				}
			}
			if snapBase != (common.Hash{}) {
				log.Info("Writing snapshot state to disk", "root", snapBase)
				if err := triedb.Commit(snapBase, true); err != nil {
					log.Error("Failed to commit recent state trie", "err", err)
				}
			}
			for !bc.triegc.Empty() {
				triedb.Dereference(bc.triegc.PopItem())
			}
			if _, nodes, _ := triedb.Size(); nodes != 0 { // all memory is contained within the nodes return for hashdb
				log.Error("Dangling trie nodes after full cleanup")
			}
		}
	}
	// Allow tracers to clean-up and release resources.
	if bc.logger != nil && bc.logger.OnClose != nil {
		bc.logger.OnClose()
	}
	// Close the trie database, release all the held resources as the last step.
	if err := bc.triedb.Close(); err != nil {
		log.Error("Failed to close trie database", "err", err)
	}

	log.Info("Blockchain stopped")
}

// InterruptInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible, or resume the chain insertion
// if required.
func (bc *BlockChain) InterruptInsert(on bool) {
	if on {
		bc.procInterrupt.Store(true)
	} else {
		bc.procInterrupt.Store(false)
	}
}

// StopInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible. Insertion is permanently disabled after
// calling this method.
func (bc *BlockChain) StopInsert() {
	bc.procInterrupt.Store(true)
}

// insertStopped returns true after StopInsert has been called.
func (bc *BlockChain) insertStopped() bool {
	return bc.procInterrupt.Load()
}

// WriteStatus status of write
type WriteStatus byte

const (
	NonStatTy WriteStatus = iota
	CanonStatTy
	SideStatTy
)

// getReceiptFields given a list of normal receipts returns the tx index, the log index
// and cumulative gas used for populating the bor receipt.
func getReceiptFields(receipts []*types.ReceiptForStorage) (int, int, uint64) {
	if len(receipts) == 0 {
		return 0, 0, 0
	}

	logs := 0
	for _, receipt := range receipts {
		logs += len(receipt.Logs)
	}

	cumulativeGasUsed := receipts[len(receipts)-1].CumulativeGasUsed

	return len(receipts), logs, cumulativeGasUsed
}

// isStateSyncReceiptPresent checks if a state-sync receipt is present in the list of
// receipts or not.
func isStateSyncReceiptPresent(decoded []*types.ReceiptForStorage) bool {
	if len(decoded) == 0 {
		return false
	}

	// The state-sync receipt can either have a 0 cumulative gas used (this depends on the remote peer) or
	// have the same cumulative gas used as the previous receipt as state-sync transactions uses 0 gas and
	// hence they don't contribute to the cumulative gas used value.
	if decoded[len(decoded)-1].CumulativeGasUsed == 0 {
		return true
	}

	if len(decoded) >= 2 && decoded[len(decoded)-1].CumulativeGasUsed == decoded[len(decoded)-2].CumulativeGasUsed {
		return true
	}

	return false
}

// splitReceiptsAndDeriveFields separates out the state-sync receipt from the whole receipt list
// of a block and returns the encoded lists back separately. If a state-sync receipt is found, it
// derives the necessary fields and populates them. For empty / nil input the normal-receipts
// return is the canonical RLP empty list and the bor-receipts return is nil (the bor-receipt
// slot stores a single struct, so "no entry" is the correct representation there).
func splitReceiptsAndDeriveFields(receipts rlp.RawValue, number uint64, hash common.Hash, borCfg *params.BorConfig) (rlp.RawValue, rlp.RawValue) {
	// If there are no receipts, normalise the receipt entry to the canonical RLP
	// empty list to match with the on-disk shape under blockReceiptsKey.
	if len(receipts) == 0 {
		return rlp.EmptyList, nil
	}

	// After the Madhugiri HF, no need to split receipts as all receipts for a block
	// are stored together (i.e. under same key).
	if borCfg.IsMadhugiri(big.NewInt(int64(number))) {
		return receipts, nil
	}

	// Bor receipts can only exist on sprint end blocks. Avoid decoding if possible.
	if !types.IsSprintEndBlock(borCfg, number) {
		return receipts, nil
	}

	var decoded []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(receipts, &decoded); err != nil {
		log.Warn("Failed to decode block receipts", "number", number, "hash", hash, "err", err)
		return receipts, nil
	}

	// Split receipts only if there's a state-sync receipt present
	if isStateSyncReceiptPresent(decoded) {
		borReceipt := decoded[len(decoded)-1]

		// Derive rest of fields for bor receipts before encoding back
		txIndex, logIndex, cumulativeGasUsed := getReceiptFields(decoded[:len(decoded)-1])
		types.DeriveFieldsForBorLogs(borReceipt.Logs, hash, number, uint(txIndex), uint(logIndex))
		borReceipt.Status = types.ReceiptStatusSuccessful
		borReceipt.CumulativeGasUsed = cumulativeGasUsed

		// Encode the state-sync transaction receipt separately
		encodedStateSyncReceipt, err := rlp.EncodeToBytes(borReceipt)
		if err != nil {
			log.Warn("Failed to encode state-sync receipt", "number", number, "hash", hash, "err", err)
			return receipts, nil
		}

		// If no normal receipts remain after extracting the state-sync
		// receipt, return the canonical RLP empty list rather than nil so
		// the on-disk shape under blockReceiptsKey is consistent.
		if len(decoded[:len(decoded)-1]) == 0 {
			return rlp.EmptyList, encodedStateSyncReceipt
		}

		// Encode back the normal (non state-sync) receipts and return
		encodedReceipts, err := rlp.EncodeToBytes(decoded[:len(decoded)-1])
		if err != nil {
			log.Warn("Failed to encode remaining receipts after excluding state-sync receipt", "number", number, "hash", hash, "err", err)
			return receipts, encodedStateSyncReceipt
		}
		return encodedReceipts, encodedStateSyncReceipt
	}

	return receipts, nil
}

// InsertReceiptChain attempts to complete an already existing header chain with
// transaction and receipt data.
func (bc *BlockChain) InsertReceiptChain(blockChain types.Blocks, receiptChain []rlp.RawValue, ancientLimit uint64) (int, error) {
	// We don't require the chainMu here since we want to maximize the
	// concurrency of header insertion and receipt insertion.
	bc.wg.Add(1)
	defer bc.wg.Done()

	var (
		ancientBlocks, liveBlocks     types.Blocks
		ancientReceipts, liveReceipts []rlp.RawValue
	)
	// Do a sanity check that the provided chain is actually ordered and linked
	for i, block := range blockChain {
		if i != 0 {
			prev := blockChain[i-1]
			if block.NumberU64() != prev.NumberU64()+1 || block.ParentHash() != prev.Hash() {
				log.Error("Non contiguous receipt insert",
					"number", block.Number(), "hash", block.Hash(), "parent", block.ParentHash(),
					"prevnumber", prev.Number(), "prevhash", prev.Hash())
				return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])",
					i-1, prev.NumberU64(), prev.Hash().Bytes()[:4],
					i, block.NumberU64(), block.Hash().Bytes()[:4], block.ParentHash().Bytes()[:4])
			}
		}
		if block.NumberU64() <= ancientLimit {
			ancientBlocks, ancientReceipts = append(ancientBlocks, block), append(ancientReceipts, receiptChain[i])
		} else {
			liveBlocks, liveReceipts = append(liveBlocks, block), append(liveReceipts, receiptChain[i])
		}

		// Here we also validate that blob transactions in the block do not contain a sidecar.
		// While the sidecar does not affect the block hash / tx hash, sending blobs within a block is not allowed.
		for txIndex, tx := range block.Transactions() {
			if tx.Type() == types.BlobTxType && tx.BlobTxSidecar() != nil {
				return 0, fmt.Errorf("block #%d contains unexpected blob sidecar in tx at index %d", block.NumberU64(), txIndex)
			}
		}
	}

	var (
		stats = struct{ processed, ignored int32 }{}
		start = time.Now()
		size  = int64(0)
	)

	// updateHead updates the head snap sync block if the inserted blocks are better
	// and returns an indicator whether the inserted blocks are canonical.
	updateHead := func(head *types.Block, headers []*types.Header) bool {
		if !bc.chainmu.TryLock() {
			return false
		}
		defer bc.chainmu.Unlock()

		// Rewind may have occurred, skip in that case.
		if bc.CurrentHeader().Number.Cmp(head.Number()) >= 0 {
			reorg, err := bc.forker.ReorgNeeded(bc.CurrentSnapBlock(), head.Header())
			if err != nil {
				log.Warn("Reorg failed", "err", err)
				return false
			} else if !reorg {
				return false
			}

			isValid, err := bc.forker.ValidateReorg(bc.CurrentSnapBlock(), headers)
			if err != nil {
				log.Warn("Reorg failed", "err", err)
				return false
			} else if !isValid {
				return false
			}

			rawdb.WriteHeadFastBlockHash(bc.db, head.Hash())
			bc.currentSnapBlock.Store(head.Header())
			headFastBlockGauge.Update(int64(head.NumberU64()))

			return true
		}

		return false
	}
	// writeAncient writes blockchain and corresponding receipt chain into ancient store.
	//
	// this function only accepts canonical chain data. All side chain will be reverted
	// eventually.
	writeAncient := func(blockChain types.Blocks, receiptChain []rlp.RawValue) (int, error) {
		first := blockChain[0]
		last := blockChain[len(blockChain)-1]

		// Ensure genesis is in ancients.
		if first.NumberU64() == 1 {
			if frozen, _ := bc.db.Ancients(); frozen == 0 {
				td := bc.genesisBlock.Difficulty()
				writeSize, err := rawdb.WriteAncientBlocks(bc.db, []*types.Block{bc.genesisBlock}, []rlp.RawValue{rlp.EmptyList}, []rlp.RawValue{rlp.EmptyList}, td)
				if err != nil {
					log.Error("Error writing genesis to ancients", "err", err)
					return 0, err
				}

				size += writeSize

				log.Info("Wrote genesis to ancients")
			}
		}

		// Separate out bor receipts (i.e. receipts of state-sync transactions)
		borReceipts := make([]rlp.RawValue, len(receiptChain))
		for i, receipts := range receiptChain {
			receiptChain[i], borReceipts[i] = splitReceiptsAndDeriveFields(receipts, blockChain[i].NumberU64(), blockChain[i].Hash(), bc.chainConfig.Bor)
		}

		var headers []*types.Header
		for _, block := range blockChain {
			headers = append(headers, block.Header())
		}

		// Write all chain data to ancients.
		td := bc.GetTd(first.Hash(), first.NumberU64())
		writeSize, err := rawdb.WriteAncientBlocks(bc.db, blockChain, receiptChain, borReceipts, td)
		if err != nil {
			log.Error("Error importing chain data to ancients", "err", err)
			return 0, err
		}
		size += writeSize

		// Write tx indices if any condition is satisfied:
		// * If user requires to reserve all tx indices(txlookuplimit=0)
		// * If all ancient tx indices are required to be reserved(txlookuplimit is even higher than ancientlimit)
		// * If block number is large enough to be regarded as a recent block
		// It means blocks below the ancientLimit-txlookupLimit won't be indexed.
		//
		// But if the `TxIndexTail` is not nil, e.g. Geth is initialized with
		// an external ancient database, during the setup, blockchain will start
		// a background routine to re-indexed all indices in [ancients - txlookupLimit, ancients)
		// range. In this case, all tx indices of newly imported blocks should be
		// generated.
		batch := bc.db.NewBatch()
		for i, block := range blockChain {
			if bc.txIndexer == nil || bc.txIndexer.limit == 0 || ancientLimit <= bc.txIndexer.limit || block.NumberU64() >= ancientLimit-bc.txIndexer.limit {
				rawdb.WriteTxLookupEntriesByBlock(batch, block)
				if len(borReceipts[i]) > 0 {
					rawdb.WriteBorTxLookupEntry(batch, block.Hash(), block.NumberU64())
				}
			} else if rawdb.ReadTxIndexTail(bc.db) != nil {
				rawdb.WriteTxLookupEntriesByBlock(batch, block)
				if len(borReceipts[i]) > 0 {
					rawdb.WriteBorTxLookupEntry(batch, block.Hash(), block.NumberU64())
				}
			}

			stats.processed++

			if batch.ValueSize() > ethdb.IdealBatchSize || i == len(blockChain)-1 {
				size += int64(batch.ValueSize())

				if err = batch.Write(); err != nil {
					snapBlock := bc.CurrentSnapBlock().Number.Uint64()
					if _, err := bc.db.TruncateHead(snapBlock + 1); err != nil {
						log.Error("Can't truncate ancient store after failed insert", "err", err)
					}

					return 0, err
				}

				batch.Reset()
			}
		}

		// Sync the ancient store explicitly to ensure all data has been flushed to disk.
		if err := bc.db.SyncAncient(); err != nil {
			return 0, err
		}
		// Update the current snap block because all block data is now present in DB.
		previousSnapBlock := bc.CurrentSnapBlock().Number.Uint64()
		if !updateHead(blockChain[len(blockChain)-1], headers) {
			// We end up here if the header chain has reorg'ed, and the blocks/receipts
			// don't match the canonical chain.
			if _, err := bc.db.TruncateHead(previousSnapBlock + 1); err != nil {
				log.Error("Can't truncate ancient store after failed insert", "err", err)
			}

			return 0, errSideChainReceipts
		}

		// Delete block data from the main database.

		canonHashes := make(map[common.Hash]struct{}, len(blockChain))

		batch = bc.db.NewBatch()
		for _, block := range blockChain {
			canonHashes[block.Hash()] = struct{}{}

			if block.NumberU64() == 0 {
				continue
			}

			rawdb.DeleteCanonicalHash(batch, block.NumberU64())
			rawdb.DeleteBlockWithoutNumber(batch, block.Hash(), block.NumberU64())
		}
		// Delete side chain hash-to-number mappings.
		for _, nh := range rawdb.ReadAllHashesInRange(bc.db, first.NumberU64(), last.NumberU64()) {
			if _, canon := canonHashes[nh.Hash]; !canon {
				rawdb.DeleteHeader(batch, nh.Hash, nh.Number)
			}
		}

		if err := batch.Write(); err != nil {
			return 0, err
		}
		stats.processed += int32(len(blockChain))
		return 0, nil
	}

	// writeLive writes blockchain and corresponding receipt chain into active store.
	writeLive := func(blockChain types.Blocks, receiptChain []rlp.RawValue) (int, error) {
		headers := make([]*types.Header, 0, len(blockChain))
		var (
			skipPresenceCheck = false
			batch             = bc.db.NewBatch()
		)
		for i, block := range blockChain {
			// Update the headers for bor specific reorg check
			headers = append(headers, block.Header())

			// Short circuit insertion if shutting down or processing failed
			if bc.insertStopped() {
				return 0, errInsertionInterrupted
			}
			// Short circuit if the owner header is unknown
			if !bc.HasHeader(block.Hash(), block.NumberU64()) {
				return i, fmt.Errorf("containing header #%d [%x..] unknown", block.Number(), block.Hash().Bytes()[:4])
			}

			if !skipPresenceCheck {
				// Ignore if the entire data is already known
				if bc.HasBlock(block.Hash(), block.NumberU64()) {
					stats.ignored++
					continue
				} else {
					// If block N is not present, neither are the later blocks.
					// This should be true, but if we are mistaken, the shortcut
					// here will only cause overwriting of some existing data
					skipPresenceCheck = true
				}
			}

			// Separate out bor receipts (i.e. receipts of state-sync transactions)
			var borReceiptRaw rlp.RawValue
			receiptChain[i], borReceiptRaw = splitReceiptsAndDeriveFields(receiptChain[i], block.NumberU64(), block.Hash(), bc.chainConfig.Bor)

			// Write all the data out into the database
			rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
			rawdb.WriteBlock(batch, block)
			rawdb.WriteRawReceipts(batch, block.Hash(), block.NumberU64(), receiptChain[i])

			var borReceipt types.ReceiptForStorage
			if len(borReceiptRaw) > 0 {
				if err := rlp.DecodeBytes(borReceiptRaw, &borReceipt); err == nil {
					rawdb.WriteBorReceipt(batch, block.Hash(), block.NumberU64(), &borReceipt)
					rawdb.WriteBorTxLookupEntry(batch, block.Hash(), block.NumberU64())
				}
			}

			// Write everything belongs to the blocks into the database. So that
			// we can ensure all components of body is completed(body, receipts)
			// except transaction indexes(will be created once sync is finished).
			if batch.ValueSize() >= ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					return 0, err
				}

				size += int64(batch.ValueSize())
				batch.Reset()
			}

			stats.processed++
		}
		// Write everything belongs to the blocks into the database. So that
		// we can ensure all components of body is completed(body, receipts,
		// tx indexes)
		if batch.ValueSize() > 0 {
			size += int64(batch.ValueSize())

			if err := batch.Write(); err != nil {
				return 0, err
			}
		}

		updateHead(blockChain[len(blockChain)-1], headers)

		return 0, nil
	}

	// Write downloaded chain data and corresponding receipt chain data
	if len(ancientBlocks) > 0 {
		if n, err := writeAncient(ancientBlocks, ancientReceipts); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}

			return n, err
		}
	}
	if len(liveBlocks) > 0 {
		if n, err := writeLive(liveBlocks, liveReceipts); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}

			return n, err
		}
	}
	var (
		head = blockChain[len(blockChain)-1]

		context = []interface{}{
			"count", stats.processed, "elapsed", common.PrettyDuration(time.Since(start)),
			"number", head.Number(), "hash", head.Hash(), "age", common.PrettyAge(time.Unix(int64(head.Time()), 0)),
			"size", common.StorageSize(size),
		}
	)
	if stats.ignored > 0 {
		context = append(context, []interface{}{"ignored", stats.ignored}...)
	}

	log.Debug("Imported new block receipts", context...)

	return 0, nil
}

// writeBlockWithoutState writes only the block and its metadata to the database,
// but does not write any state. This is used to construct competing side forks
// up to the point where they exceed the canonical total difficulty.
func (bc *BlockChain) writeBlockWithoutState(block *types.Block, td *big.Int) (err error) {
	if bc.insertStopped() {
		return errInsertionInterrupted
	}
	batch := bc.db.NewBatch()
	rawdb.WriteTd(batch, block.Hash(), block.NumberU64(), td)
	rawdb.WriteBlock(batch, block)

	if err := batch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}

	return nil
}

// writeKnownBlock updates the head block flag with a known block
// and introduces chain reorg if necessary.
func (bc *BlockChain) writeKnownBlock(block *types.Block) error {
	current := bc.CurrentBlock()
	if block.ParentHash() != current.Hash() {
		if err := bc.reorg(current, block.Header()); err != nil {
			return err
		}
	}

	bc.writeHeadBlock(block)

	return nil
}

// writeBlockWithState writes block, metadata and corresponding state data to the
// database.
func (bc *BlockChain) writeBlockWithState(block *types.Block, receipts []*types.Receipt, logs []*types.Log, statedb *state.StateDB) ([]*types.Log, error) {
	// Calculate the total difficulty of the block
	ptd := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
	var externTd *big.Int
	if ptd == nil {
		return []*types.Log{}, consensus.ErrUnknownAncestor
	}

	// Make sure no inconsistent state is leaked during insertion
	externTd = new(big.Int).Add(block.Difficulty(), ptd)

	// Irrelevant of the canonical status, write the block itself to the database.
	//
	// Note all the components of block(td, hash->number map, header, body, receipts)
	// should be written atomically. BlockBatch is used for containing all components.
	blockBatch := bc.db.NewBatch()
	rawdb.WriteTd(blockBatch, block.Hash(), block.NumberU64(), externTd)
	rawdb.WriteBlock(blockBatch, block)
	rawdb.WriteReceipts(blockBatch, block.Hash(), block.NumberU64(), receipts)

	// Bor state-sync logs: system calls append state-sync logs into state, so
	// state.Logs() may exceed the transaction-produced logs. Pre-Madhugiri we
	// write a synthetic bor receipt + tx lookup entry for those.
	stateSyncLogs := bc.writeBorStateSyncLogs(blockBatch, block, receipts, logs, statedb)

	rawdb.WritePreimages(blockBatch, statedb.Preimages())

	if statedb.Witness() != nil {
		encStart := time.Now()

		var witBuf bytes.Buffer
		if err := statedb.Witness().EncodeRLP(&witBuf); err != nil {
			log.Error("error in witness encoding", "caughterr", err)
		}

		encodeDuration := time.Since(encStart)
		witnessEncodeTimer.Update(encodeDuration)

		witnessBytes := witBuf.Bytes()

		writeStart := time.Now()
		log.Debug("Writing witness", "block", block.NumberU64(), "hash", block.Hash(), "header", statedb.Witness().Header())
		bc.WriteWitness(block.Hash(), witnessBytes)
		dbWriteDuration := time.Since(writeStart)
		witnessDbWriteTimer.Update(dbWriteDuration)
		witnessSizeBytesHistogram.Update(int64(len(witnessBytes)))

		if encodeDuration > 100*time.Millisecond {
			log.Warn("Slow witness encoding", "block", block.NumberU64(), "elapsed", common.PrettyDuration(encodeDuration), "size", common.StorageSize(len(witnessBytes)))
		}
		if dbWriteDuration > 100*time.Millisecond {
			log.Warn("Slow witness DB write", "block", block.NumberU64(), "elapsed", common.PrettyDuration(dbWriteDuration), "size", common.StorageSize(len(witnessBytes)))
		}
	} else {
		log.Debug("No witness to write", "block", block.NumberU64())
	}

	batchStart := time.Now()
	if err := blockBatch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	batchFlushDuration := time.Since(batchStart)
	blockBatchWriteTimer.Update(batchFlushDuration)
	if batchFlushDuration > 100*time.Millisecond {
		log.Warn("Slow block batch flush", "block", block.NumberU64(), "elapsed", common.PrettyDuration(batchFlushDuration))
	}

	// Commit all cached state changes into underlying memory database.
	commitStart := time.Now()
	root, stateUpdate, err := statedb.CommitWithUpdate(block.NumberU64(), bc.chainConfig.IsEIP158(block.Number()), bc.chainConfig.IsCancun(block.Number()))
	commitDuration := time.Since(commitStart)
	stateCommitTimer.Update(commitDuration)
	if commitDuration > 100*time.Millisecond {
		log.Warn("Slow state commit", "block", block.NumberU64(), "elapsed", common.PrettyDuration(commitDuration))
	}
	if err != nil {
		return []*types.Log{}, err
	}

	rawdb.WriteBytecodeSyncLastBlock(bc.db, block.NumberU64())

	// Emit the state update to the state sizestats if it's active
	if bc.stateSizer != nil {
		bc.stateSizer.Notify(stateUpdate)
	}
	// If node is running in path mode, skip explicit gc operation
	// which is unnecessary in this mode.
	if bc.triedb.Scheme() == rawdb.PathScheme {
		return []*types.Log{}, nil
	}
	// If we're running an archive node, always flush
	if bc.cfg.ArchiveMode {
		return []*types.Log{}, bc.triedb.Commit(root, false)
	}
	// Full but not archive node, do proper garbage collection
	bc.triedb.Reference(root, common.Hash{}) // metadata reference to keep trie alive
	bc.triegc.Push(root, -int64(block.NumberU64()))

	// Flush limits are not considered for the first TriesInMemory blocks.
	current := block.NumberU64()
	triesInMemory := bc.cfg.GetTriesInMemory()
	if current <= triesInMemory {
		return []*types.Log{}, nil
	}
	// If we exceeded our memory allowance, flush matured singleton nodes to disk
	var (
		_, nodes, imgs = bc.triedb.Size() // all memory is contained within the nodes return for hashdb
		limit          = common.StorageSize(bc.cfg.TrieDirtyLimit) * 1024 * 1024
	)

	if nodes > limit || imgs > 4*1024*1024 {
		_ = bc.triedb.Cap(limit - ethdb.IdealBatchSize)
	}
	// Find the next state trie we need to commit
	chosen := current - triesInMemory
	flushInterval := time.Duration(bc.flushInterval.Load())
	// If we exceeded time allowance, flush an entire trie to disk
	if bc.gcproc > flushInterval {
		// If the header is missing (canonical chain behind), we're reorging a low
		// diff sidechain. Suspend committing until this operation is completed.
		header := bc.GetHeaderByNumber(chosen)
		if header == nil {
			log.Warn("Reorg in progress, trie commit postponed", "number", chosen)
		} else {
			// If we're exceeding limits but haven't reached a large enough memory gap,
			// warn the user that the system is becoming unstable.
			if chosen < bc.lastWrite+triesInMemory && bc.gcproc >= 2*flushInterval {
				log.Info("State in memory for too long, committing", "time", bc.gcproc, "allowance", flushInterval, "optimum", float64(chosen-bc.lastWrite)/float64(triesInMemory))
			}
			// Flush an entire trie and restart the counters
			_ = bc.triedb.Commit(header.Root, true)
			bc.lastWrite = chosen
			bc.gcproc = 0
		}
	}
	// Garbage collect anything below our required write retention
	for !bc.triegc.Empty() {
		root, number := bc.triegc.Pop()
		if uint64(-number) > chosen {
			bc.triegc.Push(root, number)
			break
		}

		bc.triedb.Dereference(root)
	}

	return stateSyncLogs, nil
}

// WriteBlockAndSetHead writes the given block and all associated state to the database,
// and applies the block as the new chain head.
func (bc *BlockChain) WriteBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool) (status WriteStatus, err error) {
	if !bc.chainmu.TryLock() {
		return NonStatTy, errChainStopped
	}
	defer bc.chainmu.Unlock()

	return bc.writeBlockAndSetHead(block, receipts, logs, state, emitHeadEvent, false)
}

// writeBlockAndSetHead is the internal implementation of WriteBlockAndSetHead.
// This function expects the chain mutex to be held.
func (bc *BlockChain) writeBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool, stateless bool) (status WriteStatus, err error) {
	stateSyncLogs, err := bc.writeBlockWithState(block, receipts, logs, state)
	if err != nil {
		return NonStatTy, err
	}
	status, err = bc.resolvePostWriteStatus(block, stateless)
	if err != nil {
		return NonStatTy, err
	}
	bc.emitPostWriteEvents(block, receipts, logs, stateSyncLogs, status, emitHeadEvent)
	return status, nil
}

// InsertChain attempts to insert the given batch of blocks in to the canonical
// chain or, otherwise, create a fork. If an error is returned it will return
// the index number of the failing block as well an error describing what went
// wrong. After insertion is done, all accumulated events will be fired.
func (bc *BlockChain) InsertChain(chain types.Blocks, makeWitnesses bool) (int, error) {
	return bc.InsertChainWithWitnesses(chain, makeWitnesses, nil)
}

func (bc *BlockChain) InsertChainWithWitnesses(chain types.Blocks, makeWitness bool, witnesses []*stateless.Witness) (int, error) {
	// Sanity check that we have something meaningful to import
	if len(chain) == 0 {
		return 0, nil
	}

	// Do a sanity check that the provided chain is actually ordered and linked.
	for i := 1; i < len(chain); i++ {
		block, prev := chain[i], chain[i-1]
		if block.NumberU64() != prev.NumberU64()+1 || block.ParentHash() != prev.Hash() {
			log.Error("Non contiguous block insert",
				"number", block.Number(),
				"hash", block.Hash(),
				"parent", block.ParentHash(),
				"prevnumber", prev.Number(),
				"prevhash", prev.Hash(),
			)

			return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, prev.NumberU64(),
				prev.Hash().Bytes()[:4], i, block.NumberU64(), block.Hash().Bytes()[:4], block.ParentHash().Bytes()[:4])
		}
	}
	// Pre-checks passed, start the full block imports
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	_, n, err := bc.insertChainWithWitnesses(chain, true, makeWitness, witnesses)
	return n, err
}

// verifyContiguousBlocks checks that the provided blocks are ordered and linked.
func verifyContiguousBlocks(chain types.Blocks) error {
	for i := 1; i < len(chain); i++ {
		block, prev := chain[i], chain[i-1]
		if block.NumberU64() != prev.NumberU64()+1 || block.ParentHash() != prev.Hash() {
			log.Error("Non contiguous block insert",
				"number", block.Number(),
				"hash", block.Hash(),
				"parent", block.ParentHash(),
				"prevnumber", prev.Number(),
				"prevhash", prev.Hash(),
			)
			return fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, prev.NumberU64(),
				prev.Hash().Bytes()[:4], i, block.NumberU64(), block.Hash().Bytes()[:4], block.ParentHash().Bytes()[:4])
		}
	}
	return nil
}

// prepareHeaderVerification starts the parallel header verifier and returns a stopper and per-header error channels.
func (bc *BlockChain) prepareHeaderVerification(headers []*types.Header) (stop func(), errChans []chan error) {
	abort, results := bc.engine.VerifyHeaders(bc, headers)
	var abortOnce sync.Once
	stop = func() { abortOnce.Do(func() { close(abort) }) }

	errChans = make([]chan error, len(headers))
	for i := range errChans {
		errChans[i] = make(chan error, 1)
	}
	go func() {
		for i := 0; i < len(headers); i++ {
			err := <-results
			errChans[i] <- err
		}
		for i := range errChans {
			close(errChans[i])
		}
	}()
	return stop, errChans
}

func (bc *BlockChain) handleHeaderVerificationError(block *types.Block, index int, hErr error) error {
	if hErr == consensus.ErrUnknownAncestor {
		parentNum := block.NumberU64() - 1
		existingBlock := bc.GetBlockByNumber(parentNum)
		if existingBlock != nil && existingBlock.Hash() != block.ParentHash() {
			log.Info("Conflicting block detected in stateless sync",
				"blockNum", block.NumberU64(),
				"parentNum", parentNum,
				"existingParent", existingBlock.Hash(),
				"expectedParent", block.ParentHash())
			existingHeader := existingBlock.Header()
			verifyErr := bc.engine.VerifyHeader(bc, existingHeader)
			if verifyErr == nil {
				log.Info("Existing parent block is valid, rejecting new fork",
					"existingParent", existingBlock.Hash(),
					"rejectedParent", block.ParentHash())
				return fmt.Errorf("rejecting block %d: existing parent %s is valid", block.NumberU64(), existingBlock.Hash())
			}
			log.Info("Existing parent block is invalid, accepting reorg",
				"existingParent", existingBlock.Hash(),
				"newParent", block.ParentHash(),
				"verifyErr", verifyErr)
			if err := bc.SetHead(parentNum - 1); err != nil {
				return fmt.Errorf("failed to rewind for reorg: %w", err)
			}
			return fmt.Errorf("reorg detected, rewound to block %d", parentNum-1)
		}
		if index != 0 {
			return hErr
		}
		return nil
	}
	return hErr
}

// parallelStatelessImport processes a batch of blocks in parallel in stateless mode.
func (bc *BlockChain) insertChainStatelessParallel(chain types.Blocks, witnesses []*stateless.Witness, errChans []chan error, stats *insertStats, stopHeaders func()) (int, error) {
	log.Debug("Performing parallel stateless import", "chain length", len(chain))
	start := time.Now()
	defer func() { statelessParallelImportTimer.UpdateSince(start) }()
	statelessParallelImportBlocksCounter.Inc(int64(len(chain)))

	// Parallel stateless execution with a worker pool
	type execResult struct {
		sdb        *state.StateDB
		err        error
		needsRetry bool
		gasUsed    uint64
	}
	results := make([]execResult, len(chain))
	defer func() {
		for i := range results {
			if results[i].sdb != nil {
				results[i].sdb = nil
			}
		}
	}()

	workCh := make(chan int, len(chain))
	var snapDiffItems, snapBufItems common.StorageSize
	var wg sync.WaitGroup
	numWorkers := runtime.GOMAXPROCS(0)
	if bc.parallelStatelessImportEnabled.Load() && bc.parallelStatelessImportWorkers > 0 {
		numWorkers = bc.parallelStatelessImportWorkers
	}

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range workCh {
				blk := chain[idx]
				// Known block: skip execution
				if bc.HasBlock(blk.Hash(), blk.NumberU64()) {
					continue
				}
				if bc.insertStopped() {
					results[idx].err = errInsertionInterrupted
					continue
				}
				var witness *stateless.Witness
				if idx < len(witnesses) {
					witness = witnesses[idx]
				}
				sdb, res, perr := bc.ProcessBlockWithWitnesses(blk, witness)
				if perr != nil {
					sdb = nil
					// Defer execution errors to the sequential writer for retry
					log.Info("Deferring execution retry to writer stage", "block", blk.NumberU64(), "hash", blk.Hash(), "err", perr)
					results[idx].needsRetry = true
					continue
				}

				// If StateDB captured a database error during execution, defer to writer
				if sdb != nil && sdb.Error() != nil {
					err := sdb.Error()
					log.Info("Deferring due to StateDB error", "block", blk.NumberU64(), "hash", blk.Hash(), "err", err)
					sdb = nil
					results[idx].needsRetry = true
					continue
				}
				if witness != nil {
					sdb.SetWitness(witness)
				}
				results[idx].sdb = sdb
				results[idx].gasUsed = res.GasUsed
			}
		}()
	}

	for i := range chain {
		workCh <- i
	}
	close(workCh)
	wg.Wait()

	// Sequentially verify headers and write blocks
	var processed atomic.Int32
	for i, block := range chain {
		if bc.HasBlock(block.Hash(), block.NumberU64()) {
			processed.Add(1)
			log.Trace("Skipping known block in InsertChainStateless", "number", block.NumberU64(), "hash", block.Hash())
			if i < len(errChans) {
				if err := <-errChans[i]; err != nil {
					stopHeaders()
					return int(processed.Load() - 1), fmt.Errorf("header verification failed for known block %d (%s): %w", block.NumberU64(), block.Hash(), err)
				}
			}
			stats.processed = int(processed.Load())
			stats.usedGas += block.GasUsed()

			if bc.snaps != nil {
				snapDiffItems, snapBufItems = bc.snaps.Size()
			}
			trieDiffNodes, trieBufNodes, _ := bc.triedb.Size()
			stats.report(chain, i, snapDiffItems, snapBufItems, trieDiffNodes, trieBufNodes, true, true)
			continue
		}

		if resErr := results[i].err; resErr != nil {
			stopHeaders()
			return int(processed.Load()), resErr
		}

		var hErr error
		if i < len(errChans) {
			hErr = <-errChans[i]
		}
		if hErr != nil {
			if err := bc.handleHeaderVerificationError(block, i, hErr); err != nil {
				stopHeaders()
				return int(processed.Load()), err
			}
		}

		// Validate witness pre-state for this block (if present) before writing
		if i < len(witnesses) && witnesses[i] != nil {
			var headerReader stateless.HeaderReader = bc
			if witnesses[i].HeaderReader() != nil {
				headerReader = witnesses[i].HeaderReader()
			}
			if err := stateless.ValidateWitnessPreState(witnesses[i], headerReader, block.Header()); err != nil {
				stopHeaders()
				return int(processed.Load()), fmt.Errorf("post-import witness validation failed for block %d: %w", block.NumberU64(), err)
			}
		}

		// Only commit blocks that don't need retry
		if !results[i].needsRetry {
			if _, werr := bc.writeBlockAndSetHead(block, nil, nil, results[i].sdb, true, true); werr != nil {
				stopHeaders()
				return int(processed.Load()), werr
			}
			results[i].sdb = nil
		} else {
			// Handle deferred retry for validation errors
			log.Info("Retrying deferred validation", "block", block.NumberU64(), "hash", block.Hash())
			var witness *stateless.Witness
			if i < len(witnesses) {
				witness = witnesses[i]
			}
			sdb, res, perr := bc.ProcessBlockWithWitnesses(block, witness)
			if perr != nil {
				log.Error("Deferred validation failed", "block", block.NumberU64(), "hash", block.Hash(), "err", perr)
				stopHeaders()
				return int(processed.Load()), perr
			}
			if sdb != nil && sdb.Error() != nil {
				retryErr := sdb.Error()
				log.Error("Deferred validation captured StateDB error", "block", block.NumberU64(), "hash", block.Hash(), "err", retryErr)
				stopHeaders()
				return int(processed.Load()), retryErr
			}
			if witness != nil {
				sdb.SetWitness(witness)
			}
			results[i].gasUsed = res.GasUsed

			// Commit the block after successful retry
			if _, werr := bc.writeBlockAndSetHead(block, nil, nil, sdb, true, true); werr != nil {
				stopHeaders()
				return int(processed.Load()), werr
			}
		}

		processed.Add(1)
		stats.processed = int(processed.Load())
		stats.usedGas += results[i].gasUsed
		if bc.snaps != nil {
			snapDiffItems, snapBufItems = bc.snaps.Size()
		}
		trieDiffNodes, trieBufNodes, _ := bc.triedb.Size()
		stats.report(chain, i, snapDiffItems, snapBufItems, trieDiffNodes, trieBufNodes, true, true)
	}

	return int(processed.Load()), nil
}

func (bc *BlockChain) InsertChainStateless(chain types.Blocks, witnesses []*stateless.Witness) (int, error) {
	// Sanity check that we have something meaningful to import
	if len(chain) == 0 {
		return 0, nil
	}

	bc.blockProcFeed.Send(true)
	defer bc.blockProcFeed.Send(false)

	stats := insertStats{startTime: mclock.Now()}

	// Ensure blocks are ordered and linked
	if err := verifyContiguousBlocks(chain); err != nil {
		return 0, err
	}

	// Pre-checks passed, start the full block imports
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Prepare headers slice
	headers := make([]*types.Header, len(chain))
	for i, block := range chain {
		headers[i] = block.Header()
	}

	// Start header verification
	stopHeaders, errChans := bc.prepareHeaderVerification(headers)
	defer stopHeaders()

	// Check the validity of incoming chain
	isValid, err := bc.forker.ValidateReorg(bc.CurrentBlock(), headers)
	if err != nil {
		return 0, err
	}
	if !isValid {
		return 0, whitelist.ErrMismatch
	}

	if !bc.parallelStatelessImportEnabled.Load() {
		return bc.insertChainStatelessSequential(chain, witnesses, errChans, &stats)
	}

	return bc.insertChainStatelessParallel(chain, witnesses, errChans, &stats, stopHeaders)
}

// insertChainStatelessSequential imports a small batch of blocks sequentially in stateless mode.
func (bc *BlockChain) insertChainStatelessSequential(chain types.Blocks, witnesses []*stateless.Witness, errChans []chan error, stats *insertStats) (int, error) {
	log.Debug("Performing sequential stateless import", "chain length", len(chain))
	start := time.Now()
	defer func() { statelessSequentialImportTimer.UpdateSince(start) }()
	statelessSequentialImportBlocksCounter.Inc(int64(len(chain)))
	var processed atomic.Int32
	for i, block := range chain {
		// Known block short-circuit
		if bc.HasBlock(block.Hash(), block.NumberU64()) {
			processed.Add(1)
			log.Trace("Skipping known block in InsertChainStateless", "number", block.NumberU64(), "hash", block.Hash())
			if err := <-errChans[i]; err != nil {
				// If header verification failed for this known block (shouldn't happen often),
				// it might indicate a deeper issue, but we can't proceed with the chain.
				log.Warn("Header verification failed for known block", "number", block.NumberU64(), "hash", block.Hash(), "err", err)
				return int(processed.Load() - 1), fmt.Errorf("header verification failed for known block %d (%s): %w", block.NumberU64(), block.Hash(), err)
			}
			continue
		}
		if bc.insertStopped() {
			return int(processed.Load()), errInsertionInterrupted
		}
		var witness *stateless.Witness
		if i < len(witnesses) {
			witness = witnesses[i]
		}
		statedb, res, perr := bc.ProcessBlockWithWitnesses(block, witness)
		if perr != nil {
			return int(processed.Load()), perr
		}
		statedb.SetWitness(witness)
		hErr := <-errChans[i]
		if hErr != nil {
			if err := bc.handleHeaderVerificationError(block, i, hErr); err != nil {
				return int(processed.Load()), err
			}
		}

		if _, werr := bc.writeBlockAndSetHead(block, nil, nil, statedb, true, true); werr != nil {
			return int(processed.Load()), werr
		}
		processed.Add(1)
		stats.processed = int(processed.Load())
		stats.usedGas += res.GasUsed
		var snapDiffItems, snapBufItems common.StorageSize
		if bc.snaps != nil {
			snapDiffItems, snapBufItems = bc.snaps.Size()
		}
		trieDiffNodes, trieBufNodes, _ := bc.triedb.Size()
		stats.report(chain, i, snapDiffItems, snapBufItems, trieDiffNodes, trieBufNodes, true, true)
	}
	// End-of-batch witness validation
	for i, block := range chain {
		if i < len(witnesses) && witnesses[i] != nil {
			var headerReader stateless.HeaderReader = bc
			if witnesses[i].HeaderReader() != nil {
				headerReader = witnesses[i].HeaderReader()
			}
			if err := stateless.ValidateWitnessPreState(witnesses[i], headerReader, block.Header()); err != nil {
				return int(processed.Load()), fmt.Errorf("post-import witness validation failed for block %d: %w", block.NumberU64(), err)
			}
		}
	}
	return int(processed.Load()), nil
}

// insertChain is the internal implementation of InsertChain, which assumes that
// 1) chains are contiguous, and 2) The chain mutex is held.
//
// This method is split out so that import batches that require re-injecting
// historical blocks can do so without releasing the lock, which could lead to
// racey behaviour. If a sidechain import is in progress, and the historic state
// is imported, but then new canon-head is added before the actual sidechain
// completes, then the historic state could be pruned again
func (bc *BlockChain) insertChain(chain types.Blocks, setHead bool, makeWitness bool) (*stateless.Witness, int, error) {
	return bc.insertChainWithWitnesses(chain, setHead, makeWitness, nil)
}

func (bc *BlockChain) insertChainWithWitnesses(chain types.Blocks, setHead bool, makeWitness bool, witnesses []*stateless.Witness) (*stateless.Witness, int, error) {
	// If the chain is terminating, don't even bother starting up.
	if bc.insertStopped() {
		return nil, 0, nil
	}

	if atomic.AddInt32(&bc.blockProcCounter, 1) == 1 {
		bc.blockProcFeed.Send(true)
	}
	defer func() {
		if atomic.AddInt32(&bc.blockProcCounter, -1) == 0 {
			bc.blockProcFeed.Send(false)
		}
	}()

	// Start a parallel signature recovery (signer will fluke on fork transition, minimal perf loss)
	SenderCacher().RecoverFromBlocks(types.MakeSigner(bc.chainConfig, chain[0].Number(), chain[0].Time()), chain)

	var (
		stats     = insertStats{startTime: mclock.Now()}
		lastCanon *types.Block
	)
	// Fire a single chain head event if we've progressed the chain
	defer func() {
		if lastCanon != nil && bc.CurrentBlock().Hash() == lastCanon.Hash() {
			bc.chainHeadFeed.Send(ChainHeadEvent{Header: lastCanon.Header()})
		}
	}()
	// Start the parallel header verifier
	headers := make([]*types.Header, len(chain))
	for i, block := range chain {
		headers[i] = block.Header()
	}
	abort, results := bc.engine.VerifyHeaders(bc, headers)
	defer close(abort)

	// Peek the error for the first block to decide the directing import logic
	it := newInsertIterator(chain, results, bc.validator)
	block, err := it.next()

	// Update the block import meter; it will just record chains we've received
	// from other peers. (Note that the actual chain which gets imported would be
	// quite low).
	blockImportTimer.Mark(int64(len(headers)))

	// Check the validity of incoming chain
	isValid, err1 := bc.forker.ValidateReorg(bc.CurrentBlock(), headers)
	if err1 != nil {
		return nil, it.index, err1
	}

	if !isValid {
		// The chain to be imported is invalid as the blocks doesn't match with
		// the whitelisted block number.
		return nil, it.index, whitelist.ErrMismatch
	}

	// Left-trim all the known blocks that don't need to build snapshot
	if bc.skipBlock(err, it) {
		// First block (and state) is known
		//   1. We did a roll-back, and should now do a re-import
		//   2. The block is stored as a sidechain, and is lying about it's stateroot, and passes a stateroot
		//      from the canonical chain, which has not been verified.
		// Skip all known blocks that are behind us.
		var (
			reorg   bool
			current = bc.CurrentBlock()
		)

		for block != nil && bc.skipBlock(err, it) {
			reorg, err = bc.forker.ReorgNeeded(current, block.Header())
			if err != nil {
				return nil, it.index, err
			}

			if reorg {
				// Switch to import mode if the forker says the reorg is necessary
				// and also the block is not on the canonical chain.
				// In eth2 the forker always returns true for reorg decision (blindly trusting
				// the external consensus engine), but in order to prevent the unnecessary
				// reorgs when importing known blocks, the special case is handled here.
				if block.NumberU64() > current.Number.Uint64() || bc.GetCanonicalHash(block.NumberU64()) != block.Hash() {
					break
				}
			}

			log.Debug("Ignoring already known block", "number", block.Number(), "hash", block.Hash())

			stats.ignored++

			block, err = it.next()
		}
		// The remaining blocks are still known blocks, the only scenario here is:
		// During the snap sync, the pivot point is already submitted but rollback
		// happens. Then node resets the head full block to a lower height via `rollback`
		// and leaves a few known blocks in the database.
		//
		// When node runs a snap sync again, it can re-import a batch of known blocks via
		// `insertChain` while a part of them have higher total difficulty than current
		// head full block(new pivot point).
		for block != nil && bc.skipBlock(err, it) {
			log.Debug("Writing previously known block", "number", block.Number(), "hash", block.Hash())

			if err := bc.writeKnownBlock(block); err != nil {
				return nil, it.index, err
			}

			lastCanon = block

			block, err = it.next()
		}
		// Falls through to the block import
	}

	switch {
	// First block is pruned
	case errors.Is(err, consensus.ErrPrunedAncestor):
		if setHead {
			// First block is pruned, insert as sidechain and reorg only if TD grows enough
			log.Debug("Pruned ancestor, inserting as sidechain", "number", block.Number(), "hash", block.Hash())
			return bc.insertSideChain(block, it, makeWitness)
		} else {
			// We're post-merge and the parent is pruned, try to recover the parent state
			log.Debug("Pruned ancestor", "number", block.Number(), "hash", block.Hash())
			_, err := bc.recoverAncestors(block, makeWitness)
			return nil, it.index, err
		}

	// Some other error(except ErrKnownBlock) occurred, abort.
	// ErrKnownBlock is allowed here since some known blocks
	// still need re-execution to generate snapshots that are missing
	case err != nil && !errors.Is(err, ErrKnownBlock):
		stats.ignored += len(it.chain)

		bc.reportBlock(block, nil, err)
		return nil, it.index, err
	}

	// No validation errors for the first block (or chain prefix skipped)
	var activeState *state.StateDB
	defer func() {
		// The chain importer is starting and stopping trie prefetchers. If a bad
		// block or other error is hit however, an early return may not properly
		// terminate the background threads. This defer ensures that we clean up
		// and dangling prefetcher, without deferring each and holding on live refs.
		if activeState != nil {
			activeState.StopPrefetcher()
		}
	}()

	// Track the singleton witness from this chain insertion (if any)
	var witness *stateless.Witness

	// accumulator for canonical blocks
	var canonAccum []*types.Block

	emitAccum := func() {
		size := len(canonAccum)
		if size == 0 || size > 5 {
			// avoid reporting events for large sync events
			return
		}

		headers := make([]*types.Header, size)
		for i, block := range canonAccum {
			headers[i] = block.Header()
		}
		bc.chain2HeadFeed.Send(Chain2HeadEvent{
			Type:     Chain2HeadCanonicalEvent,
			NewChain: headers,
		})

		canonAccum = canonAccum[:0]
	}

	for ; block != nil && err == nil || errors.Is(err, ErrKnownBlock); block, err = it.next() {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during block processing")
			break
		}

		// If the header is a banned one, straight out abort
		if BadHashes[block.Hash()] {
			bc.reportBlock(block, nil, ErrBannedHash)
			return nil, it.index, ErrBannedHash
		}

		// If the block is known (in the middle of the chain), it's a special case for
		// Clique blocks where they can share state among each other, so importing an
		// older block might complete the state of the subsequent one. In this case,
		// just skip the block (we already validated it once fully (and crashed), since
		// its header and body was already in the database). But if the corresponding
		// snapshot layer is missing, forcibly rerun the execution to build it.
		if bc.skipBlock(err, it) {
			logger := log.Debug
			if bc.chainConfig.Clique == nil {
				logger = log.Warn
			}

			logger("Inserted known block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"root", block.Root())

			// Special case. Commit the empty receipt slice if we meet the known
			// block in the middle. It can only happen in the clique chain. Whenever
			// we insert blocks via `insertSideChain`, we only commit `td`, `header`
			// and `body` if it's non-existent. Since we don't have receipts without
			// reexecution, so nothing to commit. But if the sidechain will be adopted
			// as the canonical chain eventually, it needs to be reexecuted for missing
			// state, but if it's this special case here(skip reexecution) we will lose
			// the empty receipt entry.
			if len(block.Transactions()) == 0 {
				rawdb.WriteReceipts(bc.db, block.Hash(), block.NumberU64(), nil)
			} else {
				log.Error("Please file an issue, skip known block execution without receipt",
					"hash", block.Hash(), "number", block.NumberU64())
			}

			if err := bc.writeKnownBlock(block); err != nil {
				return nil, it.index, err
			}

			stats.processed++
			if bc.logger != nil && bc.logger.OnSkippedBlock != nil {
				bc.logger.OnSkippedBlock(tracing.BlockEvent{
					Block:     block,
					TD:        bc.GetTd(block.ParentHash(), block.NumberU64()-1),
					Finalized: bc.CurrentFinalBlock(),
					Safe:      bc.CurrentSafeBlock(),
				})
			}
			// We can assume that logs are empty here, since the only way for consecutive
			// Clique blocks to have the same state is if there are no transactions.
			lastCanon = block

			continue
		}
		// Retrieve the parent block and it's state to execute on top
		start := time.Now()

		parent := it.previous()
		if parent == nil {
			parent = bc.GetHeader(block.ParentHash(), block.NumberU64()-1)
		}

		// --- Pipelined import: check for pending SRC from previous block ---
		//
		// A supplied witness is incompatible with pipelining: serving this
		// block from a witness swaps the process-wide trie read backend for a
		// memdb holding only this block's nodes, which both starves an SRC
		// goroutine still resolving the previous block's trie and denies this
		// block's own execution the parent-root nodes the FlatDiff mode reads
		// from. Fall back to the serial path for such blocks, and collect any
		// in-flight SRC before the backend is swapped.
		witnessFed := witnesses != nil && len(witnesses) > it.processed()-1 && witnesses[it.processed()-1] != nil
		pipelineActive := bc.cfg.EnablePipelinedImportSRC && setHead && !bc.cfg.Stateless && !witnessFed
		var pipeOpts *PipelineImportOpts
		if pipelineActive {
			pipeOpts = bc.buildPipelineImportOpts(block, parent)
		} else if witnessFed {
			// Nothing is in flight for this block yet, so no prefetch to
			// interrupt — a failed flush rolls back the rejected pipelined
			// block and aborts the insert.
			if err := bc.flushPendingImportSRC(true); err != nil {
				return nil, it.index, err
			}
		}

		// Note: ProcessBlock opens its own statedbs internally. The statedb
		// created here in the original code was only used for activeState tracking.
		// With pipelined import, ProcessBlock handles all state opening.

		// If we are past Byzantium, enable prefetching to pull in trie node paths
		// while processing transactions. Before Byzantium the prefetcher is mostly
		// useless due to the intermediate root hashing after each transaction.
		if bc.chainConfig.IsByzantium(block.Number()) {
			// Generate witnesses either if we're self-testing, or if it's the
			// only block being inserted. A bit crude, but witnesses are huge,
			// so we refuse to make an entire chain of them.
			if bc.cfg.VmConfig.StatelessSelfValidation || (makeWitness && len(chain) == 1) {
				witness, err = stateless.NewWitness(block.Header(), bc)
				if err != nil {
					return nil, it.index, err
				}
			}
		}

		var followupInterrupt atomic.Bool

		// Process block using the parent state as reference point
		pstart := time.Now()

		computeWitness := makeWitness

		if witnessFed {
			// 1. Validate the witness.
			var headerReader stateless.HeaderReader = bc
			if witnesses[it.processed()-1].HeaderReader() != nil {
				headerReader = witnesses[it.processed()-1].HeaderReader()
			}
			if err := stateless.ValidateWitnessPreState(witnesses[it.processed()-1], headerReader, block.Header()); err != nil {
				log.Error("Witness validation failed during chain insertion", "blockNumber", block.Number(), "blockHash", block.Hash(), "err", err)
				bc.reportBlock(block, &ProcessResult{}, err)
				followupInterrupt.Store(true)
				return nil, it.index, fmt.Errorf("witness validation failed: %w", err)
			}

			// 2. Set the witness to the statedb.
			memdb := witnesses[it.processed()-1].MakeHashDB(bc.statedb.TrieDB().Disk())
			bc.statedb.TrieDB().SetReadBackend(hashdb.New(memdb, triedb.HashDefaults.HashDB))
			computeWitness = false
			bc.statedb.DisableSnapInReader()
		}

		if computeWitness {
			witness, err = stateless.NewWitness(block.Header(), bc)
			if err != nil {
				log.Error("Error in witness generation", "err", err)
			}
		}

		cheapExecStart := time.Now()
		receipts, logs, usedGas, statedb, vtime, err := bc.ProcessBlock(block, parent, witness, &followupInterrupt, pipeOpts)
		cheapExecElapsed := time.Since(cheapExecStart)
		if pipelineActive {
			pipelineImportCheapExecTimer.Update(cheapExecElapsed)
			pipelineImportCheapValidationTimer.Update(vtime)
		} else {
			normalImportProcessTimer.Update(cheapExecElapsed)
			normalImportValidationTimer.Update(vtime)
		}
		bc.statedb.TrieDB().SetReadBackend(nil)
		bc.statedb.EnableSnapInReader()
		activeState = statedb

		if err != nil {
			// A block is only "bad" if it is invalid. Missing local state —
			// the parent's trie pruned or capped away between validation and
			// the reader open — says nothing about the block, and recording it
			// as bad both poisons the bad-block DB and misreports the peer that
			// delivered a perfectly valid block. The pre-pipeline code had a
			// state.New pre-check that returned this class early; ProcessBlock
			// now opens its own readers, so filter it here instead.
			if isLocalStateUnavailable(err) {
				log.Warn("Skipping block, parent state unavailable locally",
					"number", block.NumberU64(), "hash", block.Hash(), "err", err)
			} else {
				bc.reportBlock(block, &ProcessResult{Receipts: receipts}, err)
			}
			followupInterrupt.Store(true)
			// Flush any pending import SRC before returning on error. Log any
			// flush error (e.g., previous block's root mismatch) — the outer
			// err takes precedence for the caller, but a silent flush failure
			// would mask real corruption from the prior pipelined block.
			if pipelineActive {
				if flushErr := bc.flushPendingImportSRC(true); flushErr != nil {
					attrs := []interface{}{"block", block.NumberU64(), "flushErr", flushErr, "processErr", err}
					attrs = append(attrs, pipelineImportLogAttrs(parent, pipeOpts)...)
					log.Error("Pipelined import: flush failed after ProcessBlock error", attrs...)
				}
			}
			return nil, it.index, err
		}

		// --- Pipelined import: extract FlatDiff, collect previous SRC, write metadata, spawn SRC ---
		if pipelineActive {
			adjustBack, err := bc.persistPipelinedImport(block, parent, statedb, receipts, logs, start, cheapExecElapsed, vtime, computeWitness)
			if err != nil {
				followupInterrupt.Store(true)
				// adjustBack attributes the failure to the previous block,
				// whose SRC this insert collected. When the collecting block
				// is the first of the batch there is no previous index to
				// point at — the pipeline deliberately leaves an SRC in
				// flight across insertChain calls, so that is the normal
				// cross-batch shape, not an edge case. Return -1 then: both
				// consumers treat out-of-band indices as "failure outside
				// this batch" and only log, whereas clamping to 0 would
				// blame (and bad-block report) this batch's first block for
				// a previous batch's failure.
				idx := it.index
				if adjustBack {
					idx--
				}
				return nil, idx, err
			}
			followupInterrupt.Store(true)
			stats.processed++
			stats.usedGas += usedGas
			lastCanon = block
			var snapDiffItems, snapBufItems common.StorageSize
			if bc.snaps != nil {
				snapDiffItems, snapBufItems = bc.snaps.Size()
			}
			trieDiffNodes, trieBufNodes, _ := bc.triedb.Size()
			stats.report(chain, it.index, snapDiffItems, snapBufItems, trieDiffNodes, trieBufNodes, setHead, false)
			emitPipelinedImportParityMetrics(statedb, start, pstart, vtime, block)
			continue
		}

		// --- Normal (non-pipelined) write path ---

		// BOR state sync feed related changes
		bc.stateSyncMu.RLock()
		for _, data := range bc.getStateSyncLocked() {
			bc.stateSyncFeed.Send(StateSyncEvent{Data: data})
		}
		bc.stateSyncMu.RUnlock()
		// BOR
		ptime := time.Since(pstart) - vtime - statedb.BorConsensusTime

		proctime := time.Since(start) // processing + validation

		// Update the metrics touched during block processing and validation
		accountReadTimer.Update(statedb.AccountReads)                   // Account reads are complete(in processing)
		storageReadTimer.Update(statedb.StorageReads)                   // Storage reads are complete(in processing)
		snapshotAccountReadTimer.Update(statedb.SnapshotAccountReads)   // Account reads are complete(in processing)
		snapshotStorageReadTimer.Update(statedb.SnapshotStorageReads)   // Storage reads are complete(in processing)
		accountUpdateTimer.Update(statedb.AccountUpdates)               // Account updates are complete(in validation)
		storageUpdateTimer.Update(statedb.StorageUpdates)               // Storage updates are complete(in validation)
		accountHashTimer.Update(statedb.AccountHashes)                  // Account hashes are complete(in validation)
		storageHashTimer.Update(statedb.StorageHashes)                  // Storage hashes are complete(in validation)
		triehash := statedb.AccountHashes + statedb.StorageHashes       // The time spent on tries hashing
		trieUpdate := statedb.AccountUpdates + statedb.StorageUpdates   // The time spent on tries update
		trieRead := statedb.SnapshotAccountReads + statedb.AccountReads // The time spent on account read
		trieRead += statedb.SnapshotStorageReads + statedb.StorageReads // The time spent on storage read
		blockExecutionTimer.Update(ptime - trieRead)                    // The time spent on EVM processing
		blockValidationTimer.Update(vtime - (triehash + trieUpdate))    // The time spent on block validation
		borConsensusTime.Update(statedb.BorConsensusTime)               // The time spent on bor consensus (span + state sync)
		// Write the block to the chain and get the status.
		var (
			wstart = time.Now()
			status WriteStatus
		)

		// Before the actual db insertion happens, verify the block against the whitelisted
		// milestone and checkpoint. This is to prevent a race condition where a milestone
		// or checkpoint was whitelisted while the block execution happened (and wasn't
		// available sometime before) and the block turns out to be invalid (i.e. not
		// honouring the milestone or checkpoint). Use the block itself as current block
		// so that it's considered as a `past` chain and the validation doesn't get bypassed.
		reorgCheckStart := time.Now()
		isValid, err = bc.forker.ValidateReorg(block.Header(), []*types.Header{block.Header()})
		reorgCheckElapsed := time.Since(reorgCheckStart)
		normalImportReorgCheckTimer.Update(reorgCheckElapsed)
		if err != nil {
			return nil, it.index, err
		}

		if !isValid {
			return nil, it.index, whitelist.ErrMismatch
		}

		if !setHead {
			// Don't set the head, only insert the block
			_, err = bc.writeBlockWithState(block, receipts, logs, statedb)
		} else {
			status, err = bc.writeBlockAndSetHead(block, receipts, logs, statedb, false, false)
		}
		writeElapsed := time.Since(wstart)
		normalImportWriteTimer.Update(writeElapsed)

		followupInterrupt.Store(true)

		if err != nil {
			return nil, it.index, err
		}

		// Update the metrics touched during block commit
		accountCommitTimer.Update(statedb.AccountCommits)   // Account commits are complete, we can mark them
		storageCommitTimer.Update(statedb.StorageCommits)   // Storage commits are complete, we can mark them
		snapshotCommitTimer.Update(statedb.SnapshotCommits) // Snapshot commits are complete, we can mark them
		triedbCommitTimer.Update(statedb.TrieDBCommits)     // Trie database commits are complete, we can mark them
		witnessCollectionTimer.Update(statedb.WitnessCollection)

		blockWriteTimer.Update(time.Since(wstart) - statedb.AccountCommits - statedb.StorageCommits - statedb.SnapshotCommits - statedb.TrieDBCommits)
		elapsedNormal := time.Since(start)
		blockInsertTimer.Update(elapsedNormal)
		normalImportTotalTimer.Update(elapsedNormal)
		bc.logSlowNormalImport(block, cheapExecElapsed, vtime, reorgCheckElapsed, writeElapsed, elapsedNormal, statedb)
		gasUsedPerBlockHistogram.Update(int64(block.GasUsed()))
		txsPerBlockHistogram.Update(int64(len(block.Transactions())))
		if elapsedNormal > 0 {
			chainMgaspsMeter.Update(time.Duration(float64(block.GasUsed()) * 1000 / float64(elapsedNormal)))
		}
		// Witness has already been written inside writeBlockWithState by this point,
		// so "witness ready" == "import complete" in the non-pipelined case.
		witnessReadyEndToEndTimer.Update(elapsedNormal)

		// Report the import stats before returning the various results
		stats.processed++
		stats.usedGas += usedGas

		var snapDiffItems, snapBufItems common.StorageSize
		if bc.snaps != nil {
			snapDiffItems, snapBufItems = bc.snaps.Size()
		}
		trieDiffNodes, trieBufNodes, _ := bc.triedb.Size()
		stats.report(chain, it.index, snapDiffItems, snapBufItems, trieDiffNodes, trieBufNodes, setHead, false)

		/*
			// Print confirmation that a future fork is scheduled, but not yet active.
			bc.logForkReadiness(block)
		*/

		if !setHead {
			// After merge we expect few side chains. Simply count
			// all blocks the CL gives us for GC processing time
			bc.gcproc += proctime
			return witness, it.index, nil // Direct block insertion of a single block
		}

		// BOR
		if status == CanonStatTy {
			canonAccum = append(canonAccum, block)
		} else {
			emitAccum()
		}
		// BOR

		switch status {
		case CanonStatTy:
			log.Debug("Inserted new block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"elapsed", common.PrettyDuration(time.Since(start)),
				"root", block.Root())

			lastCanon = block

			// Only count canonical blocks for GC processing time
			bc.gcproc += proctime

		case SideStatTy:
			log.Debug("Inserted forked block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())

		default:
			// This in theory is impossible, but lets be nice to our future selves and leave
			// a log, instead of trying to track down blocks imports that don't emit logs.
			log.Warn("Inserted block with unknown status", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
	}

	// BOR
	emitAccum()
	// BOR

	stats.ignored += it.remaining()
	return witness, it.index, err
}

// blockProcessingResult is a summary of block processing
// used for updating the stats.
// nolint : unused
type blockProcessingResult struct {
	usedGas  uint64
	procTime time.Duration
	status   WriteStatus
	witness  *stateless.Witness
}

//nolint:unused
func (bpr *blockProcessingResult) Witness() *stateless.Witness {
	return bpr.witness
}

// ProcessBlock executes and validates the given block. If there was no error
// it writes the block and associated state to database.
// nolint : unused
func (bc *BlockChain) processBlock(block *types.Block, statedb *state.StateDB, start time.Time, setHead bool, diskdb ethdb.Database) (_ *blockProcessingResult, blockEndErr error) {
	startTime := time.Now()
	if bc.logger != nil && bc.logger.OnBlockStart != nil {
		td := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
		bc.logger.OnBlockStart(tracing.BlockEvent{
			Block:     block,
			TD:        td,
			Finalized: bc.CurrentFinalBlock(),
			Safe:      bc.CurrentSafeBlock(),
		})
	}
	if bc.logger != nil && bc.logger.OnBlockEnd != nil {
		defer func() {
			bc.logger.OnBlockEnd(blockEndErr)
		}()
	}

	// Process block using the parent state as reference point
	pstart := time.Now()
	res, err := bc.processor.Process(block, statedb, bc.cfg.VmConfig, nil, context.Background())
	if err != nil {
		bc.reportBlock(block, res, err)
		return nil, err
	}
	ptime := time.Since(pstart)

	vstart := time.Now()
	if err := bc.validator.ValidateState(block, statedb, res, false); err != nil {
		bc.reportBlock(block, res, err)
		return nil, err
	}
	vtime := time.Since(vstart)

	var witness *stateless.Witness
	var witnessStats *stateless.WitnessStats

	// If witnesses was generated and stateless self-validation requested, do
	// that now. Self validation should *never* run in production, it's more of
	// a tight integration to enable running *all* consensus tests through the
	// witness builder/runner, which would otherwise be impossible due to the
	// various invalid chain states/behaviors being contained in those tests.
	xvstart := time.Now()
	if witness = statedb.Witness(); witness != nil && bc.cfg.VmConfig.StatelessSelfValidation {
		log.Warn("Running stateless self-validation", "block", block.Number(), "hash", block.Hash())

		if bc.cfg.VmConfig.EnableWitnessStats {
			witnessStats = stateless.NewWitnessStats()
		}

		// Remove critical computed fields from the block to force true recalculation
		context := block.Header()
		context.Root = common.Hash{}
		context.ReceiptHash = common.Hash{}

		task := types.NewBlockWithHeader(context).WithBody(*block.Body())
		author := NewEVMBlockContext(block.Header(), bc.hc, nil).Coinbase

		// Run the stateless self-cross-validation
		crossStateRoot, crossReceiptRoot, _, _, err := ExecuteStateless(bc.chainConfig, bc.cfg.VmConfig, task, witness, &author, bc.engine, diskdb)
		if err != nil {
			return nil, fmt.Errorf("stateless self-validation failed: %v", err)
		}
		if crossStateRoot != block.Root() {
			return nil, fmt.Errorf("stateless self-validation root mismatch (cross: %x local: %x)", crossStateRoot, block.Root())
		}
		if crossReceiptRoot != block.ReceiptHash() {
			return nil, fmt.Errorf("stateless self-validation receipt root mismatch (cross: %x local: %x)", crossReceiptRoot, block.ReceiptHash())
		}
	}

	xvtime := time.Since(xvstart)
	proctime := time.Since(startTime) // processing + validation + cross validation

	// Update the metrics touched during block processing and validation
	accountReadTimer.Update(statedb.AccountReads) // Account reads are complete(in processing)
	storageReadTimer.Update(statedb.StorageReads) // Storage reads are complete(in processing)
	if statedb.AccountLoaded != 0 {
		accountReadSingleTimer.Update(statedb.AccountReads / time.Duration(statedb.AccountLoaded))
	}
	if statedb.StorageLoaded != 0 {
		storageReadSingleTimer.Update(statedb.StorageReads / time.Duration(statedb.StorageLoaded))
	}
	accountUpdateTimer.Update(statedb.AccountUpdates)                                 // Account updates are complete(in validation)
	storageUpdateTimer.Update(statedb.StorageUpdates)                                 // Storage updates are complete(in validation)
	accountHashTimer.Update(statedb.AccountHashes)                                    // Account hashes are complete(in validation)
	triehash := statedb.AccountHashes                                                 // The time spent on tries hashing
	trieUpdate := statedb.AccountUpdates + statedb.StorageUpdates                     // The time spent on tries update
	blockExecutionTimer.Update(ptime - (statedb.AccountReads + statedb.StorageReads)) // The time spent on EVM processing
	blockValidationTimer.Update(vtime - (triehash + trieUpdate))                      // The time spent on block validation
	blockCrossValidationTimer.Update(xvtime)                                          // The time spent on stateless cross validation

	// Write the block to the chain and get the status.
	var (
		wstart = time.Now()
		status WriteStatus
	)
	if !setHead {
		// Don't set the head, only insert the block
		_, err = bc.writeBlockWithState(block, res.Receipts, res.Logs, statedb)
	} else {
		status, err = bc.writeBlockAndSetHead(block, res.Receipts, res.Logs, statedb, false, false)
	}
	if err != nil {
		return nil, err
	}
	// Report the collected witness statistics
	if witnessStats != nil {
		witnessStats.ReportMetrics(block.NumberU64())
	}

	// Update the metrics touched during block commit
	accountCommitTimer.Update(statedb.AccountCommits)   // Account commits are complete, we can mark them
	storageCommitTimer.Update(statedb.StorageCommits)   // Storage commits are complete, we can mark them
	snapshotCommitTimer.Update(statedb.SnapshotCommits) // Snapshot commits are complete, we can mark them
	triedbCommitTimer.Update(statedb.TrieDBCommits)     // Trie database commits are complete, we can mark them
	witnessCollectionTimer.Update(statedb.WitnessCollection)

	blockWriteTimer.Update(time.Since(wstart) - max(statedb.AccountCommits, statedb.StorageCommits) /* concurrent */ - statedb.SnapshotCommits - statedb.TrieDBCommits)
	elapsed := time.Since(startTime) + 1 // prevent zero division
	blockInsertTimer.Update(elapsed)

	// TODO(rjl493456442) generalize the ResettingTimer
	mgasps := float64(res.GasUsed) * 1000 / float64(elapsed)
	chainMgaspsMeter.Update(time.Duration(mgasps))

	return &blockProcessingResult{
		usedGas:  res.GasUsed,
		procTime: proctime,
		status:   status,
		witness:  witness,
	}, nil
}

// insertSideChain is called when an import batch hits upon a pruned ancestor
// error, which happens when a sidechain with a sufficiently old fork-block is
// found.
//
// The method writes all (header-and-body-valid) blocks to disk, then tries to
// switch over to the new chain if the TD exceeded the current chain.
// insertSideChain is only used pre-merge.
func (bc *BlockChain) insertSideChain(block *types.Block, it *insertIterator, makeWitness bool) (*stateless.Witness, int, error) {
	var (
		lastBlock = block
		current   = bc.CurrentBlock()
		headers   []*types.Header
		externTd  *big.Int
	)

	// The first sidechain block error is already verified to be ErrPrunedAncestor.
	// Since we don't import them here, we expect ErrUnknownAncestor for the remaining
	// ones. Any other errors means that the block is invalid, and should not be written
	// to disk.
	err := consensus.ErrPrunedAncestor
	for ; block != nil && errors.Is(err, consensus.ErrPrunedAncestor); block, err = it.next() {
		headers = append(headers, block.Header())
		// Check the canonical state root for that number
		if number := block.NumberU64(); current.Number.Uint64() >= number {
			canonical := bc.GetBlockByNumber(number)
			if canonical != nil && canonical.Hash() == block.Hash() {
				// Not a sidechain block, this is a re-import of a canon block which has it's state pruned

				// Collect the TD of the block. Since we know it's a canon one,
				// we can get it directly, and not (like further below) use
				// the parent and then add the block on top
				externTd = bc.GetTd(block.Hash(), block.NumberU64())
				continue
			}

			if canonical != nil && canonical.Root() == block.Root() {
				// This is most likely a shadow-state attack. When a fork is imported into the
				// database, and it eventually reaches a block height which is not pruned, we
				// just found that the state already exist! This means that the sidechain block
				// refers to a state which already exists in our canon chain.
				//
				// If left unchecked, we would now proceed importing the blocks, without actually
				// having verified the state of the previous blocks.
				log.Warn("Sidechain ghost-state attack detected", "number", block.NumberU64(), "sideroot", block.Root(), "canonroot", canonical.Root())

				// If someone legitimately side-mines blocks, they would still be imported as usual. However,
				// we cannot risk writing unverified blocks to disk when they obviously target the pruning
				// mechanism.
				return nil, it.index, errors.New("sidechain ghost-state attack")
			}
		}
		if externTd == nil {
			externTd = bc.GetTd(block.ParentHash(), block.NumberU64()-1)
		}
		externTd = new(big.Int).Add(externTd, block.Difficulty())

		if !bc.HasBlock(block.Hash(), block.NumberU64()) {
			start := time.Now()
			if err := bc.writeBlockWithoutState(block, externTd); err != nil {
				return nil, it.index, err
			}

			log.Debug("Injected sidechain block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}

		lastBlock = block
	}
	// At this point, we've written all sidechain blocks to database. Loop ended
	// either on some other error or all were processed. If there was some other
	// error, we can ignore the rest of those blocks.
	//
	// If the externTd was larger than our local TD, we now need to reimport the previous
	// blocks to regenerate the required state
	reorg, err := bc.forker.ReorgNeeded(current, lastBlock.Header())
	if err != nil {
		return nil, it.index, err
	}

	isValid, err := bc.forker.ValidateReorg(current, headers)
	if err != nil {
		return nil, it.index, err
	}

	if !reorg || !isValid {
		localTd := bc.GetTd(current.Hash(), current.Number.Uint64())
		log.Info("Sidechain written to disk", "start", it.first().NumberU64(), "end", it.previous().Number, "sidetd", externTd, "localtd", localTd)

		return nil, it.index, err
	}
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
	)

	parent := it.previous()
	for parent != nil && !bc.HasState(parent.Root) {
		if bc.stateRecoverable(parent.Root) {
			if err := bc.triedb.Recover(parent.Root); err != nil {
				return nil, 0, err
			}
			break
		}
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.Number.Uint64())

		parent = bc.GetHeader(parent.ParentHash, parent.Number.Uint64()-1)
	}

	if parent == nil {
		return nil, it.index, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	var (
		blocks []*types.Block
		memory uint64
	)

	for i := len(hashes) - 1; i >= 0; i-- {
		// Append the next block to our batch
		block := bc.GetBlock(hashes[i], numbers[i])

		blocks = append(blocks, block)
		memory += block.Size()

		// If memory use grew too large, import and continue. Sadly we need to discard
		// all raised events and logs from notifications since we're too heavy on the
		// memory here.
		if len(blocks) >= 2048 || memory > 64*1024*1024 {
			log.Info("Importing heavy sidechain segment", "blocks", len(blocks), "start", blocks[0].NumberU64(), "end", block.NumberU64())
			if _, _, err := bc.insertChain(blocks, true, false); err != nil {
				return nil, 0, err
			}

			blocks, memory = blocks[:0], 0

			// If the chain is terminating, stop processing blocks
			if bc.insertStopped() {
				log.Debug("Abort during blocks processing")
				return nil, 0, nil
			}
		}
	}

	if len(blocks) > 0 {
		log.Info("Importing sidechain segment", "start", blocks[0].NumberU64(), "end", blocks[len(blocks)-1].NumberU64())
		return bc.insertChain(blocks, true, makeWitness)
	}
	return nil, 0, nil
}

// recoverAncestors finds the closest ancestor with available state and re-execute
// all the ancestor blocks since that.
// recoverAncestors is only used post-merge.
// We return the hash of the latest block that we could correctly validate.
func (bc *BlockChain) recoverAncestors(block *types.Block, makeWitness bool) (common.Hash, error) {
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
		parent  = block
	)

	for parent != nil && !bc.HasState(parent.Root()) {
		if bc.stateRecoverable(parent.Root()) {
			if err := bc.triedb.Recover(parent.Root()); err != nil {
				return common.Hash{}, err
			}
			break
		}
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.NumberU64())
		parent = bc.GetBlock(parent.ParentHash(), parent.NumberU64()-1)

		// If the chain is terminating, stop iteration
		if bc.insertStopped() {
			log.Debug("Abort during blocks iteration")
			return common.Hash{}, errInsertionInterrupted
		}
	}

	if parent == nil {
		return common.Hash{}, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	for i := len(hashes) - 1; i >= 0; i-- {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during blocks processing")
			return common.Hash{}, errInsertionInterrupted
		}

		var b *types.Block
		if i == 0 {
			b = block
		} else {
			b = bc.GetBlock(hashes[i], numbers[i])
		}
		if _, _, err := bc.insertChain(types.Blocks{b}, false, makeWitness && i == 0); err != nil {
			return b.ParentHash(), err
		}
	}

	return block.Hash(), nil
}

// collectLogs collects the logs that were generated or removed during the
// processing of a block. These logs are later announced as deleted or reborn.
func (bc *BlockChain) collectLogs(b *types.Block, removed bool) []*types.Log {
	_, logs := bc.collectReceiptsAndLogs(b, removed)
	return logs
}

// collectReceiptsAndLogs retrieves receipts from the database and returns both receipts and logs.
// This avoids duplicate database reads when both are needed.
func (bc *BlockChain) collectReceiptsAndLogs(b *types.Block, removed bool) ([]*types.Receipt, []*types.Log) {
	var blobGasPrice *big.Int
	if b.ExcessBlobGas() != nil && bc.chainConfig.BlobScheduleConfig != nil {
		blobGasPrice = eip4844.CalcBlobFee(bc.chainConfig, b.Header())
	}
	receipts := rawdb.ReadRawReceipts(bc.db, b.Hash(), b.NumberU64())

	// Append bor receipt
	borReceipt := rawdb.ReadBorReceipt(bc.db, b.Hash(), b.NumberU64(), bc.chainConfig)
	if borReceipt != nil {
		receipts = append(receipts, borReceipt)
	}

	if err := receipts.DeriveFields(bc.chainConfig, b.Hash(), b.NumberU64(), b.Time(), b.BaseFee(), blobGasPrice, b.Transactions()); err != nil {
		log.Error("Failed to derive block receipts fields", "hash", b.Hash(), "number", b.NumberU64(), "err", err)
	}
	var logs []*types.Log

	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if removed {
				log.Removed = true
			}
			logs = append(logs, log)
		}
	}
	return receipts, logs
}

// reorg takes two blocks, an old chain and a new chain and will reconstruct the
// blocks and inserts them to be part of the new canonical chain and accumulates
// potential missing transactions and post an event about them.
//
// Note the new head block won't be processed here, callers need to handle it
// externally.
func (bc *BlockChain) reorg(oldHead *types.Header, newHead *types.Header) error {
	var (
		newChain    []*types.Header
		oldChain    []*types.Header
		commonBlock *types.Header
	)
	// Reduce the longer chain to the same number as the shorter one
	if oldHead.Number.Uint64() > newHead.Number.Uint64() {
		// Old chain is longer, gather all transactions and logs as deleted ones
		for ; oldHead != nil && oldHead.Number.Uint64() != newHead.Number.Uint64(); oldHead = bc.GetHeader(oldHead.ParentHash, oldHead.Number.Uint64()-1) {
			oldChain = append(oldChain, oldHead)
		}
	} else {
		// New chain is longer, stash all blocks away for subsequent insertion
		for ; newHead != nil && newHead.Number.Uint64() != oldHead.Number.Uint64(); newHead = bc.GetHeader(newHead.ParentHash, newHead.Number.Uint64()-1) {
			newChain = append(newChain, newHead)
		}
	}
	if oldHead == nil {
		return errInvalidOldChain
	}
	if newHead == nil {
		return errInvalidNewChain
	}
	// Both sides of the reorg are at the same number, reduce both until the common
	// ancestor is found
	for {
		// If the common ancestor was found, bail out
		if oldHead.Hash() == newHead.Hash() {
			commonBlock = oldHead
			break
		}
		// Remove an old block as well as stash away a new block
		oldChain = append(oldChain, oldHead)
		newChain = append(newChain, newHead)

		// Step back with both chains
		oldHead = bc.GetHeader(oldHead.ParentHash, oldHead.Number.Uint64()-1)
		if oldHead == nil {
			return errInvalidOldChain
		}
		newHead = bc.GetHeader(newHead.ParentHash, newHead.Number.Uint64()-1)
		if newHead == nil {
			return errInvalidNewChain
		}
	}

	// Ensure the user sees large reorgs
	if len(oldChain) == 0 && len(newChain) == 0 {
		// No actual reorg, same block
		log.Info("No reorg needed; old and new head are identical", "number", oldHead.Number, "hash", oldHead.Hash())
		return nil
	}

	if len(oldChain) > 0 && len(newChain) > 0 {
		bc.chain2HeadFeed.Send(Chain2HeadEvent{
			Type:     Chain2HeadReorgEvent,
			NewChain: newChain,
			OldChain: oldChain,
		})

		logFn := log.Info

		msg := "Chain reorg detected"
		if len(oldChain) > 63 {
			msg = "Large chain reorg detected"
			logFn = log.Warn
		}
		logFn(msg, "number", commonBlock.Number, "hash", commonBlock.Hash(),
			"drop", len(oldChain), "dropfrom", oldChain[0].Hash(), "add", len(newChain), "addfrom", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
		blockReorgDropMeter.Mark(int64(len(oldChain)))
		blockReorgMeter.Mark(1)
	} else if len(newChain) > 0 {
		// Special case happens in the post merge stage that current head is
		// the ancestor of new head while these two blocks are not consecutive
		log.Info("Extend chain", "add", len(newChain), "number", newChain[0].Number, "hash", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
	} else {
		// len(newChain) == 0 && len(oldChain) > 0
		// rewind the canonical chain to a lower point.
		log.Error("Impossible reorg, please file an issue", "oldnum", oldHead.Number, "oldhash", oldHead.Hash(), "oldblocks", len(oldChain), "newnum", newHead.Number, "newhash", newHead.Hash(), "newblocks", len(newChain))
	}
	// Acquire the tx-lookup lock before mutation. This step is essential
	// as the txlookups should be changed atomically, and all subsequent
	// reads should be blocked until the mutation is complete.
	bc.txLookupLock.Lock()

	// Reorg can be executed, start reducing the chain's old blocks and appending
	// the new blocks
	var (
		deletedTxs []common.Hash
		rebirthTxs []common.Hash

		deletedLogs []*types.Log
		rebirthLogs []*types.Log
	)
	// Deleted log emission on the API uses forward order, which is borked, but
	// we'll leave it in for legacy reasons.
	//
	// TODO(karalabe): This should be nuked out, no idea how, deprecate some APIs?
	{
		for i := len(oldChain) - 1; i >= 0; i-- {
			// Also send event for blocks removed from the canon chain. Note: Geth has removed
			// the concept of side chains but we need them in bor.
			bc.chainSideFeed.Send(ChainSideEvent{Header: oldChain[i]})

			block := bc.GetBlock(oldChain[i].Hash(), oldChain[i].Number.Uint64())
			if block == nil {
				return errInvalidOldChain // Corrupt database, mostly here to avoid weird panics
			}
			if logs := bc.collectLogs(block, true); len(logs) > 0 {
				deletedLogs = append(deletedLogs, logs...)
			}
			if len(deletedLogs) > 512 {
				bc.rmLogsFeed.Send(RemovedLogsEvent{deletedLogs})
				deletedLogs = nil
			}
		}
		if len(deletedLogs) > 0 {
			bc.rmLogsFeed.Send(RemovedLogsEvent{deletedLogs})
		}
	}
	// Undo old blocks in reverse order
	for i := 0; i < len(oldChain); i++ {
		// Collect all the deleted transactions
		block := bc.GetBlock(oldChain[i].Hash(), oldChain[i].Number.Uint64())
		if block == nil {
			return errInvalidOldChain // Corrupt database, mostly here to avoid weird panics
		}
		for _, tx := range block.Transactions() {
			deletedTxs = append(deletedTxs, tx.Hash())
		}
		// Collect deleted logs and emit them for new integrations
		if logs := bc.collectLogs(block, true); len(logs) > 0 {
			// Emit revertals latest first, older then
			slices.Reverse(logs)

			// TODO(karalabe): Hook into the reverse emission part
		}
	}
	// Apply new blocks in forward order
	for i := len(newChain) - 1; i >= 1; i-- {
		// Collect all the included transactions
		block := bc.GetBlock(newChain[i].Hash(), newChain[i].Number.Uint64())
		if block == nil {
			return errInvalidNewChain // Corrupt database, mostly here to avoid weird panics
		}
		for _, tx := range block.Transactions() {
			rebirthTxs = append(rebirthTxs, tx.Hash())
		}
		// Collect inserted logs and emit them
		if logs := bc.collectLogs(block, false); len(logs) > 0 {
			rebirthLogs = append(rebirthLogs, logs...)
		}
		if len(rebirthLogs) > 512 {
			bc.logsFeed.Send(rebirthLogs)
			rebirthLogs = nil
		}
		// Update the head block
		bc.writeHeadBlock(block)
	}
	if len(rebirthLogs) > 0 {
		bc.logsFeed.Send(rebirthLogs)
	}
	// Delete useless indexes right now which includes the non-canonical
	// transaction indexes, canonical chain indexes which above the head.
	batch := bc.db.NewBatch()
	for _, tx := range types.HashDifference(deletedTxs, rebirthTxs) {
		rawdb.DeleteTxLookupEntry(batch, tx)
	}
	// Delete all hash markers that are not part of the new canonical chain.
	// Because the reorg function does not handle new chain head, all hash
	// markers greater than or equal to new chain head should be deleted.
	number := commonBlock.Number
	if len(newChain) > 1 {
		number = newChain[1].Number
	}
	for i := number.Uint64() + 1; ; i++ {
		hash := rawdb.ReadCanonicalHash(bc.db, i)
		if hash == (common.Hash{}) {
			break
		}
		rawdb.DeleteCanonicalHash(batch, i)
	}
	if err := batch.Write(); err != nil {
		log.Crit("Failed to delete useless indexes", "err", err)
	}
	// Reset the tx lookup cache to clear stale txlookup cache.
	bc.txLookupCache.Purge()

	// Release the tx-lookup lock after mutation.
	bc.txLookupLock.Unlock()

	return nil
}

// InsertBlockWithoutSetHead executes the block, runs the necessary verification
// upon it and then persist the block and the associate state into the database.
// The key difference between the InsertChain is it won't do the canonical chain
// updating. It relies on the additional SetCanonical call to finalize the entire
// procedure.
func (bc *BlockChain) InsertBlockWithoutSetHead(block *types.Block, makeWitness bool) (*stateless.Witness, error) {
	if !bc.chainmu.TryLock() {
		return nil, errChainStopped
	}
	defer bc.chainmu.Unlock()

	witness, _, err := bc.insertChain(types.Blocks{block}, false, makeWitness)
	return witness, err
}

// SetCanonical rewinds the chain to set the new head block as the specified
// block. It's possible that the state of the new head is missing, and it will
// be recovered in this function as well.
func (bc *BlockChain) SetCanonical(head *types.Block) (common.Hash, error) {
	if !bc.chainmu.TryLock() {
		return common.Hash{}, errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Re-execute the reorged chain in case the head state is missing.
	if !bc.HasState(head.Root()) {
		if latestValidHash, err := bc.recoverAncestors(head, false); err != nil {
			return latestValidHash, err
		}

		log.Info("Recovered head state", "number", head.Number(), "hash", head.Hash())
	}
	// Run the reorg if necessary and set the given block as new head.
	start := time.Now()

	if head.ParentHash() != bc.CurrentBlock().Hash() {
		if err := bc.reorg(bc.CurrentBlock(), head.Header()); err != nil {
			return common.Hash{}, err
		}
	}

	bc.writeHeadBlock(head)

	// Emit events
	receipts, logs := bc.collectReceiptsAndLogs(head, false)

	bc.chainFeed.Send(ChainEvent{
		Header:       head.Header(),
		Receipts:     receipts,
		Transactions: head.Transactions(),
	})

	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: head.Header()})

	context := []interface{}{
		"number", head.Number(),
		"hash", head.Hash(),
		"root", head.Root(),
		"elapsed", time.Since(start),
	}
	if timestamp := time.Unix(int64(head.Time()), 0); time.Since(timestamp) > time.Minute {
		context = append(context, []interface{}{"age", common.PrettyAge(timestamp)}...)
	}

	log.Info("Chain head was updated", context...)

	return head.Hash(), nil
}

// skipBlock returns 'true', if the block being imported can be skipped over, meaning
// that the block does not need to be processed but can be considered already fully 'done'.
func (bc *BlockChain) skipBlock(err error, it *insertIterator) bool {
	// We can only ever bypass processing if the only error returned by the validator
	// is ErrKnownBlock, which means all checks passed, but we already have the block
	// and state.
	if !errors.Is(err, ErrKnownBlock) {
		return false
	}
	// If we're not using snapshots, we can skip this, since we have both block
	// and (trie-) state
	if bc.snaps == nil {
		return true
	}

	var (
		header     = it.current() // header can't be nil
		parentRoot common.Hash
	)
	// If we also have the snapshot-state, we can skip the processing.
	if bc.snaps.Snapshot(header.Root) != nil {
		return true
	}
	// In this case, we have the trie-state but not snapshot-state. If the parent
	// snapshot-state exists, we need to process this in order to not get a gap
	// in the snapshot layers.
	// Resolve parent block
	if parent := it.previous(); parent != nil {
		parentRoot = parent.Root
	} else if parent = bc.GetHeaderByHash(header.ParentHash); parent != nil {
		parentRoot = parent.Root
	}

	if parentRoot == (common.Hash{}) {
		return false // Theoretically impossible case
	}
	// Parent is also missing snapshot: we can skip this. Otherwise process.
	if bc.snaps.Snapshot(parentRoot) == nil {
		return true
	}

	return false
}

// reportBlock logs a bad block error.
// isLocalStateUnavailable reports whether err means the node cannot reach the
// state it needs, as opposed to the block being invalid. Both shapes occur:
// pathdb/hashdb report an unavailable state root, the trie reports a missing
// node once a walk descends into pruned data.
func isLocalStateUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var missing *trie.MissingNodeError
	if errors.As(err, &missing) {
		return true
	}
	return strings.Contains(err.Error(), "is not available")
}

func (bc *BlockChain) reportBlock(block *types.Block, res *ProcessResult, err error) {
	var receipts types.Receipts
	if res != nil {
		receipts = res.Receipts
	}
	rawdb.WriteBadBlock(bc.db, block)
	log.Error(summarizeBadBlock(block, receipts, bc.Config(), err))
}

/*
// logForkReadiness will write a log when a future fork is scheduled, but not
// active. This is useful so operators know their client is ready for the fork.
func (bc *BlockChain) logForkReadiness(block *types.Block) {
	current := bc.Config().LatestFork(block.Time())

	// Short circuit if the timestamp of the last fork is undefined.
	t := bc.Config().Timestamp(current + 1)
	if t == nil {
		return
	}
	at := time.Unix(int64(*t), 0)

	// Only log if:
	// - Current time is before the fork activation time
	// - Enough time has passed since last alert
	now := time.Now()
	if now.Before(at) && now.After(bc.lastForkReadyAlert.Add(forkReadyInterval)) {
		log.Info("Ready for fork activation", "fork", current+1, "date", at.Format(time.RFC822),
			"remaining", time.Until(at).Round(time.Second), "timestamp", at.Unix())
		bc.lastForkReadyAlert = time.Now()
	}
}
*/

// summarizeBadBlock returns a string summarizing the bad block and other
// relevant information.
func summarizeBadBlock(block *types.Block, receipts []*types.Receipt, config *params.ChainConfig, err error) string {
	var receiptString string
	for i, receipt := range receipts {
		receiptString += fmt.Sprintf("\n  %d: cumulative: %v gas: %v contract: %v status: %v tx: %v logs: %v bloom: %x state: %x",
			i, receipt.CumulativeGasUsed, receipt.GasUsed, receipt.ContractAddress.Hex(),
			receipt.Status, receipt.TxHash.Hex(), receipt.Logs, receipt.Bloom, receipt.PostState)
	}

	version, vcs := version.Info()
	platform := fmt.Sprintf("%s %s %s %s", version, runtime.Version(), runtime.GOARCH, runtime.GOOS)

	if vcs != "" {
		vcs = fmt.Sprintf("\nVCS: %s", vcs)
	}

	return fmt.Sprintf(`
########## BAD BLOCK #########
Block: %v (%#x)
Error: %v
Platform: %v%v
Chain config: %#v
Receipts: %v
##############################
`, block.Number(), block.Hash(), err, platform, vcs, config, receiptString)
}

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
func (bc *BlockChain) InsertHeaderChain(chain []*types.Header) (int, error) {
	if len(chain) == 0 {
		return 0, nil
	}

	start := time.Now()
	if i, err := bc.hc.ValidateHeaderChain(chain); err != nil {
		return i, err
	}

	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()
	_, err := bc.hc.InsertHeaderChain(chain, start, bc.forker)
	return 0, err
}

func (bc *BlockChain) InsertHeaderChainWithoutValidation(chain []*types.Header) (int, error) {
	if len(chain) == 0 {
		return 0, nil
	}

	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	count, err := bc.hc.WriteHeaders(chain)
	return count, err
}

func (bc *BlockChain) GetChainConfig() *params.ChainConfig {
	return bc.chainConfig
}

// SetBlockValidatorAndProcessorForTesting sets the current validator and processor.
// This method can be used to force an invalid blockchain to be verified for tests.
// This method is unsafe and should only be used before block import starts.
func (bc *BlockChain) SetBlockValidatorAndProcessorForTesting(v Validator, p Processor) {
	bc.validator = v
	bc.processor = p
}

// SetTrieFlushInterval configures how often in-memory tries are persisted to disk.
// The interval is in terms of block processing time, not wall clock.
// It is thread-safe and can be called repeatedly without side effects.
func (bc *BlockChain) SetTrieFlushInterval(interval time.Duration) {
	bc.flushInterval.Store(int64(interval))
}

// GetTrieFlushInterval gets the in-memory tries flushAlloc interval
func (bc *BlockChain) GetTrieFlushInterval() time.Duration {
	return time.Duration(bc.flushInterval.Load())
}

func (bc *BlockChain) SubscribeChain2HeadEvent(ch chan<- Chain2HeadEvent) event.Subscription {
	return bc.scope.Track(bc.chain2HeadFeed.Subscribe(ch))
}

// WriteBlockAndSetHeadPipelined writes block data (header, body, receipts) to
// the database and sets it as the chain head, WITHOUT committing trie state.
// The state commit is handled separately by the SRC goroutine that already
// called CommitWithUpdate. This avoids the "layer stale" error that occurs
// when two CommitWithUpdate calls diverge from the same parent root.
// WriteBlockAndSetHeadPipelined is the public variant that acquires the chain mutex.
// Used by the miner pipeline (resultLoop) where the mutex is not already held.
func (bc *BlockChain) WriteBlockAndSetHeadPipelined(block *types.Block, receipts []*types.Receipt, logs []*types.Log, statedb *state.StateDB, emitHeadEvent bool, witnessBytes []byte) (WriteStatus, error) {
	if !bc.chainmu.TryLock() {
		return NonStatTy, errChainStopped
	}
	defer bc.chainmu.Unlock()

	return bc.writeBlockAndSetHeadPipelined(block, receipts, logs, statedb, emitHeadEvent, witnessBytes)
}

// writeBlockAndSetHeadPipelined is the internal implementation. It writes block
// data (header, body, receipts) to the database and sets it as the chain head,
// WITHOUT committing trie state. The state commit is handled by the SRC goroutine.
// This function does NOT acquire the chain mutex — the caller must ensure
// proper synchronization (e.g., called from insertChainWithWitnesses).
func (bc *BlockChain) writeBlockAndSetHeadPipelined(block *types.Block, receipts []*types.Receipt, logs []*types.Log, statedb *state.StateDB, emitHeadEvent bool, witnessBytes []byte) (WriteStatus, error) {
	status, stateSyncLogs, err := bc.writePipelinedBlockAndResolveStatus(block, receipts, logs, statedb, witnessBytes)
	if err != nil {
		return NonStatTy, err
	}
	bc.emitPostWriteEvents(block, receipts, logs, stateSyncLogs, status, emitHeadEvent)
	return status, nil
}

func (bc *BlockChain) writePipelinedBlockAndResolveStatus(block *types.Block, receipts []*types.Receipt, logs []*types.Log, statedb *state.StateDB, witnessBytes []byte) (WriteStatus, []*types.Log, error) {
	ptd := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
	if ptd == nil {
		return NonStatTy, nil, consensus.ErrUnknownAncestor
	}
	stateSyncLogs, err := bc.writePipelinedBlockBatch(block, receipts, logs, statedb, witnessBytes, new(big.Int).Add(block.Difficulty(), ptd))
	if err != nil {
		return NonStatTy, nil, err
	}
	status, err := bc.resolvePostWriteStatus(block, false)
	if err != nil {
		return NonStatTy, nil, err
	}
	return status, stateSyncLogs, nil
}

// writePipelinedBlockBatch assembles one atomic batch with the block, its
// receipts, bor state-sync logs (pre-Madhugiri only), preimages, the SRC
// goroutine's witness, and total difficulty — then flushes it. Returns the
// stateSyncLogs slice so the caller can emit them on the logs feed.
// The SRC witness replaces the execution-side witness because FlatDiff
// overlay accounts bypass the trie during speculative execution, so their
// MPT proof nodes are only captured during SRC's CommitWithUpdate.
func (bc *BlockChain) writePipelinedBlockBatch(block *types.Block, receipts []*types.Receipt, logs []*types.Log, statedb *state.StateDB, witnessBytes []byte, externTd *big.Int) ([]*types.Log, error) {
	blockBatch := bc.db.NewBatch()
	rawdb.WriteTd(blockBatch, block.Hash(), block.NumberU64(), externTd)
	rawdb.WriteBlock(blockBatch, block)
	rawdb.WriteReceipts(blockBatch, block.Hash(), block.NumberU64(), receipts)
	stateSyncLogs := bc.writeBorStateSyncLogs(blockBatch, block, receipts, logs, statedb)
	rawdb.WritePreimages(blockBatch, statedb.Preimages())
	if len(witnessBytes) > 0 {
		witWriteStart := time.Now()
		bc.WriteWitness(block.Hash(), witnessBytes)
		witnessDbWriteTimer.UpdateSince(witWriteStart)
		witnessSizeBytesHistogram.Update(int64(len(witnessBytes)))
	}
	batchStart := time.Now()
	if err := blockBatch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	blockBatchWriteTimer.UpdateSince(batchStart)
	rawdb.WriteBytecodeSyncLastBlock(bc.db, block.NumberU64())
	return stateSyncLogs, nil
}

// writeBorStateSyncLogs emits a synthetic bor receipt + tx lookup entry for
// state-sync logs (logs the node observed from Heimdall but no EVM tx
// produced). Madhugiri replaces this with native receipt encoding and the
// legacy path is skipped there. Returns the state-sync logs slice so the
// caller can forward them on the logs feed.
func (bc *BlockChain) writeBorStateSyncLogs(batch ethdb.Batch, block *types.Block, receipts []*types.Receipt, logs []*types.Log, statedb *state.StateDB) []*types.Log {
	blockLogs := statedb.Logs()
	if len(blockLogs) == 0 {
		return nil
	}
	if bc.chainConfig.Bor != nil && bc.chainConfig.Bor.IsMadhugiri(block.Number()) {
		return nil
	}
	if len(blockLogs) <= len(logs) {
		return nil
	}
	sort.SliceStable(blockLogs, func(i, j int) bool {
		return blockLogs[i].Index < blockLogs[j].Index
	})
	stateSyncLogs := blockLogs[len(logs):]
	types.DeriveFieldsForBorLogs(stateSyncLogs, block.Hash(), block.NumberU64(), uint(len(receipts)), uint(len(logs)))
	var cumulativeGasUsed uint64
	if len(receipts) > 0 {
		cumulativeGasUsed = receipts[len(receipts)-1].CumulativeGasUsed
	}
	rawdb.WriteBorReceipt(batch, block.Hash(), block.NumberU64(), &types.ReceiptForStorage{
		Status:            types.ReceiptStatusSuccessful,
		Logs:              stateSyncLogs,
		CumulativeGasUsed: cumulativeGasUsed,
	})
	rawdb.WriteBorTxLookupEntry(batch, block.Hash(), block.NumberU64())
	return stateSyncLogs
}

// resolvePostWriteStatus decides CanonStatTy vs SideStatTy for a freshly
// written block and performs a reorg when needed. Shared by the standard
// and pipelined write paths — non-deterministic tie-breaking here would
// cause consensus splits between nodes. The stateless flag relaxes
// errInvalidNewChain during fast-forward reorgs for stateless sync.
func (bc *BlockChain) resolvePostWriteStatus(block *types.Block, stateless bool) (WriteStatus, error) {
	currentBlock := bc.CurrentBlock()
	reorg, err := bc.forker.ReorgNeeded(currentBlock, block.Header())
	if err != nil {
		return NonStatTy, err
	}
	if !reorg {
		return SideStatTy, nil
	}
	if block.ParentHash() != currentBlock.Hash() {
		if err := bc.reorg(currentBlock, block.Header()); err != nil {
			if !(stateless && err == errInvalidNewChain) {
				return NonStatTy, err
			}
		}
	}
	return CanonStatTy, nil
}

// emitPostWriteEvents publishes the correct event set for the resolved
// write status. For CanonStatTy: writeHeadBlock + ChainEvent + (optional)
// ChainHeadEvent + any state-sync events. For SideStatTy: chainSideFeed +
// chain2HeadFeed. Shared by the standard and pipelined write paths.
func (bc *BlockChain) emitPostWriteEvents(block *types.Block, receipts []*types.Receipt, logs, stateSyncLogs []*types.Log, status WriteStatus, emitHeadEvent bool) {
	if status != CanonStatTy {
		bc.chainSideFeed.Send(ChainSideEvent{Header: block.Header()})
		bc.chain2HeadFeed.Send(Chain2HeadEvent{
			Type:     Chain2HeadForkEvent,
			NewChain: []*types.Header{block.Header()},
		})
		return
	}
	bc.writeHeadBlock(block)
	bc.chainFeed.Send(ChainEvent{
		Header:       block.Header(),
		Receipts:     receipts,
		Transactions: block.Transactions(),
	})
	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	if len(stateSyncLogs) > 0 {
		bc.logsFeed.Send(stateSyncLogs)
	}
	if !emitHeadEvent {
		return
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: block.Header()})
	bc.stateSyncMu.RLock()
	for _, data := range bc.getStateSyncLocked() {
		bc.stateSyncFeed.Send(StateSyncEvent{Data: data})
	}
	bc.stateSyncMu.RUnlock()
}

// --- Pipelined SRC methods ---

// PostExecState returns a StateDB representing the post-execution state
// of the given block header. Under pipelined SRC, if the FlatDiff for this block
// is still cached (i.e. this is the chain head), it returns a non-blocking
// overlay state via NewWithFlatBase. Otherwise it falls back to resolving the
// actual state root via StateAt.
//
// This is used by the txpool and RPC layer to get correct state when the chain
// head was produced via the pipeline (where the committed trie root may lag
// behind the actual post-execution state).
func (bc *BlockChain) PostExecState(header *types.Header) (*state.StateDB, error) {
	// Fast path: if we have the FlatDiff for this block, use it as an overlay.
	// Matching by block number plus state root rather than hash because the
	// hash may not be final at the time SetLastFlatDiff is called (the seal
	// signature is added later). The miner's speculative path records a zero
	// root (the root isn't computed yet when its FlatDiff is captured), so a
	// zero stored root falls back to the number-only match; the import path
	// always records the real root, which guards against serving a stale
	// overlay for a different same-height block after a reorg.
	bc.lastFlatDiffMu.RLock()
	flatDiff := bc.lastFlatDiff
	flatDiffBlockNum := bc.lastFlatDiffBlockNum
	flatDiffParentRoot := bc.lastFlatDiffParentRoot
	flatDiffBlockRoot := bc.lastFlatDiffBlockRoot
	bc.lastFlatDiffMu.RUnlock()

	if flatDiff != nil && flatDiffBlockNum == header.Number.Uint64() &&
		(flatDiffBlockRoot == (common.Hash{}) || flatDiffBlockRoot == header.Root) {
		// Open at the parent's committed root (which IS in the trie DB) and
		// overlay the FlatDiff. We cannot use header.Root because it may not
		// be committed yet (pipelined import SRC still running).
		return state.NewWithFlatBase(flatDiffParentRoot, bc.statedb, flatDiff)
	}

	// Slow path: use the committed state root directly.
	return bc.StateAt(header.Root)
}

// SpawnSRCGoroutine launches a background goroutine that computes the actual
// state root for block by replaying flatDiff on top of parentRoot. When
// makeWitness is true, the goroutine also completes (or, for legacy call
// sites, produces) a stateless witness; when false, witness work, FlatDiff
// read-surface preload, and witness encoding are all skipped — only deferred
// state-root validation runs. The result is stored in pending.root;
// pending.wg is decremented when finished.
//
// Witness ownership (when makeWitness=true) follows the LINEAR OWNERSHIP
// INVARIANT documented at runSRCCompute. The import path passes execWitness =
// the witness already populated by EVM execution (AddCode + AddBlockHash)
// with allowOwnWitness=false; SRC then completes that single witness with
// trie-proof nodes during ApplyFlatDiffForCommit + CommitWithUpdate. Call
// sites with no execution witness in scope set execWitness=nil and
// allowOwnWitness=true to explicitly permit SRC to create its own witness.
// detachedPrefetcher is an optional execution-side prefetcher handoff. SRC
// owns it after SpawnSRCGoroutine returns: it waits for the prefetcher to
// finish and either discards the warm nodes (wait-only mode) or builds a
// WarmSnapshot from them (useWarmSnapshot=true). This keeps the import thread
// from blocking on prefetch completion while still letting SRC benefit from
// the execution-side warmup.
func (bc *BlockChain) SpawnSRCGoroutine(block *types.Block, parentRoot common.Hash, flatDiff *state.FlatDiff, makeWitness bool, execWitness *stateless.Witness, allowOwnWitness bool, detachedPrefetcher *state.DetachedPrefetcher, useWarmSnapshot bool) *pendingSRCState {
	return bc.spawnSRCGoroutine(block, parentRoot, flatDiff, makeWitness, execWitness, allowOwnWitness, detachedPrefetcher, useWarmSnapshot, true)
}

func (bc *BlockChain) spawnSRCGoroutine(block *types.Block, parentRoot common.Hash, flatDiff *state.FlatDiff, makeWitness bool, execWitness *stateless.Witness, allowOwnWitness bool, detachedPrefetcher *state.DetachedPrefetcher, useWarmSnapshot bool, publishGlobal bool) *pendingSRCState {
	pending := &pendingSRCState{
		blockHash:   block.Hash(),
		blockNumber: block.NumberU64(),
	}
	if publishGlobal {
		bc.pendingSRCMu.Lock()
		bc.pendingSRC = pending
		bc.pendingSRCMu.Unlock()
	}

	pending.wg.Add(1)
	bc.wg.Add(1)
	go bc.runSRCCompute(pending, block, parentRoot, flatDiff, makeWitness, execWitness, allowOwnWitness, detachedPrefetcher, useWarmSnapshot)
	return pending
}

func recordDetachedPrefetchStats(stats state.PrefetcherSnapshotStats, useWarmSnapshot bool) {
	pipelineImportSRCPrefetchWaitTimer.Update(stats.Drain)
	pipelineImportSRCPrefetchReportTimer.Update(stats.Report)
	pipelineImportSRCPrefetchSubfetchers.Update(int64(stats.Fetchers))
	if !useWarmSnapshot {
		return
	}
	pipelineImportWarmSnapshotCollect.Update(stats.Collect)
	pipelineImportWarmSnapshotFetchers.Update(int64(stats.LoadedFetchers))
	pipelineImportWarmSnapshotAccountNodes.Update(int64(stats.AccountNodes))
	pipelineImportWarmSnapshotStorageNodes.Update(int64(stats.StorageNodes))
	pipelineImportWarmSnapshotAccountBytes.Update(int64(stats.AccountBytes))
	pipelineImportWarmSnapshotStorageBytes.Update(int64(stats.StorageBytes))
	pipelineImportWarmSnapshotNodes.Update(int64(stats.AccountNodes + stats.StorageNodes))
	pipelineImportWarmSnapshotBytes.Update(int64(stats.AccountBytes + stats.StorageBytes))
}

func finishDetachedPrefetcher(detachedPrefetcher *state.DetachedPrefetcher, useWarmSnapshot bool) *state.WarmSnapshot {
	if detachedPrefetcher == nil {
		return nil
	}
	if !useWarmSnapshot {
		stats := detachedPrefetcher.Stop()
		recordDetachedPrefetchStats(stats, false)
		return nil
	}
	warmSnapshotInput, stats := detachedPrefetcher.StopAndCollectWarmSnapshot()
	recordDetachedPrefetchStats(stats, true)
	if warmSnapshotInput == nil {
		return nil
	}
	buildStart := time.Now()
	warmSnapshot := warmSnapshotInput.Build()
	pipelineImportWarmSnapshotBuild.UpdateSince(buildStart)
	return warmSnapshot
}

// runSRCCompute is the SRC goroutine body. It opens a StateDB at the
// committed parent root, replays the FlatDiff, and commits to produce block
// N's state root. Witness-producing SRC uses a trie-only reader and the
// journaled FlatDiff replay so proof-path nodes are captured. Witness-off SRC
// uses state.New plus the direct FlatDiff replay to avoid unnecessary journal
// overhead. When makeWitness is true, SRC also preloads the FlatDiff read
// surface so the witness covers proof-path nodes the speculative execution
// skipped, and encodes + caches the resulting witness. When false, preload
// and witness encoding are skipped — only deferred root validation runs. All
// observable side effects (pending.root, pending.err, pending.witness,
// witness cache) happen here before wg.Done().
//
// LINEAR OWNERSHIP INVARIANT for the execution witness W:
//
//  1. The main thread writes to W during ProcessBlock:
//     - AddCode via statedb.go (GetCode/GetCodeSize on contract calls)
//     - AddBlockHash via vm/instructions.go (BLOCKHASH opcode)
//     - AddState via statedb.go Finalise/IntermediateRoot reads
//  2. The trie prefetcher does not write to W — subfetcher.loop only
//     populates trie-local prevalueTracer state. The import path detaches the
//     prefetcher before this goroutine is spawned; this goroutine then stops
//     the detached prefetcher synchronously before converting any warm nodes
//     into a WarmSnapshot. That stop has writers-exited semantics
//     (trie_prefetcher.go: <-sf.term gated on loop's `defer close(sf.term)`),
//     which provides the ordering guarantee should the prefetcher ever gain a
//     witness write path.
//  3. The main thread hands W to this goroutine via SpawnSRCGoroutine and
//     never touches W again — it moves on to the next block with a fresh
//     witness.
//  4. This goroutine writes to W during ApplyFlatDiffForCommit +
//     CommitWithUpdate, then encodes via encodeAndCachePendingWitness. The
//     cached bytes are immutable thereafter.
//
// The invariant requires:
//   - No AddState / AddCode / AddBlockHash call site reachable from a
//     prefetcher-spawned goroutine.
//   - No terminate(true) (async) call on the SRC handoff path.
//   - No reuse of W on the main thread after SpawnSRCGoroutine.
func (bc *BlockChain) runSRCCompute(pending *pendingSRCState, block *types.Block, parentRoot common.Hash, flatDiff *state.FlatDiff, makeWitness bool, execWitness *stateless.Witness, allowOwnWitness bool, detachedPrefetcher *state.DetachedPrefetcher, useWarmSnapshot bool) {
	defer bc.wg.Done()
	defer pending.wg.Done()
	pending.markStarted(time.Now())
	defer func() {
		pending.markDone(time.Now())
	}()
	defer func() {
		if r := recover(); r != nil {
			log.Error("Pipelined SRC: panic in SRC goroutine", "block", block.NumberU64(), "err", r)
			pending.err = fmt.Errorf("SRC goroutine panicked: %v", r)
		}
	}()
	if bc.srcHoldForTesting != nil {
		bc.srcHoldForTesting(block.NumberU64())
	}

	// Hard-fail when a caller asked for a witness but did not hand one in.
	// allowOwnWitness=true is the explicit opt-in for call sites that want
	// SRC to create its own witness. Without it, the caller's contract is
	// that the published witness is the same object EVM execution populated,
	// preserving execution-time entries such as BLOCKHASH headers. BorWitness
	// encoding intentionally excludes Codes (see core/stateless/encoding.go),
	// so bytecode entries collected on the in-memory witness are not part of
	// the canonical Bor witness wire format.
	if makeWitness && execWitness == nil && !allowOwnWitness {
		finishDetachedPrefetcher(detachedPrefetcher, false)
		pending.err = fmt.Errorf(
			"pipelined SRC witness requested without execution witness: block=%d hash=%s allowOwnWitness=false",
			block.NumberU64(), block.Hash(),
		)
		return
	}

	warmSnapshot := finishDetachedPrefetcher(detachedPrefetcher, useWarmSnapshot)
	openStart := time.Now()
	tmpDB, witness, err := bc.openSRCStateDB(parentRoot, block, makeWitness, execWitness, warmSnapshot)
	pipelineImportSRCOpenStateDBTimer.UpdateSince(openStart)
	if err != nil {
		pending.err = err
		return
	}
	applyStart := time.Now()
	if makeWitness {
		tmpDB.ApplyFlatDiffForCommit(flatDiff)
	} else {
		tmpDB.ApplyFlatDiffForCommitFast(flatDiff)
	}
	pipelineImportSRCApplyFlatDiffTimer.UpdateSince(applyStart)

	// Preload + read-surface histograms only fire when the witness is being
	// produced — preloadFlatDiffReads exists solely to populate the witness
	// with proof-path trie nodes, and the histograms describe its work.
	if makeWitness {
		recordAndPreloadSRCWitnessReads(tmpDB, flatDiff)
		// Preload (and ApplyFlatDiffForCommit's origin lookups) walk tries
		// through tmpDB's READER, whose tracers no IntermediateRoot loop
		// harvests: the mutated-account loop only collects obj.trie (write
		// paths) and the read-only loop skips mutated objects entirely. Drain
		// the reader tracers here or read-only slots of accounts this block
		// also wrote lose their current-generation proof nodes.
		tmpDB.CollectStateWitness()
	}

	deleteEmptyObjects := bc.chainConfig.IsEIP158(block.Number())
	commitStart := time.Now()
	root, stateUpdate, err := tmpDB.CommitWithUpdate(block.NumberU64(), deleteEmptyObjects, bc.chainConfig.IsCancun(block.Number()))
	commitElapsed := time.Since(commitStart)
	pipelineImportSRCCommitTimer.Update(commitElapsed)
	stateCommitTimer.Update(commitElapsed)
	if err != nil {
		log.Error("Pipelined SRC: CommitWithUpdate failed", "block", block.NumberU64(), "err", err)
		pending.err = err
		return
	}
	emitSRCStateDBMetrics(tmpDB)
	if bc.stateSizer != nil {
		bc.stateSizer.Notify(stateUpdate)
	}
	if makeWitness {
		bc.encodeAndCachePendingWitness(pending, witness, block)
	}
	pending.root = root
}

// recordAndPreloadSRCWitnessReads measures the preload step's wall-time and
// the size of the FlatDiff read surface, then re-reads that surface through
// tmpDB so the witness captures proof-path trie nodes the speculative
// execution skipped. ReadStorage is iterated directly (not via ReadSet)
// because it also contains read-only slots on mutated accounts — answers
// "how is storage-read load distributed", which is what shapes any future
// parallelisation. Fires for both import and miner SRC since runSRCCompute
// is shared.
func recordAndPreloadSRCWitnessReads(tmpDB *state.StateDB, flatDiff *state.FlatDiff) {
	preloadSlots := 0
	for _, slots := range flatDiff.ReadStorage {
		preloadSlots += len(slots)
		pipelineSRCPreloadSlotsPerAccountHistogram.Update(int64(len(slots)))
	}
	pipelineSRCPreloadReadAccountsHistogram.Update(int64(len(flatDiff.ReadSet)))
	pipelineSRCPreloadSlotsHistogram.Update(int64(preloadSlots))
	pipelineSRCPreloadDestructsHistogram.Update(int64(len(flatDiff.Destructs)))
	pipelineSRCPreloadNonexistentHistogram.Update(int64(len(flatDiff.NonExistentReads)))

	preloadStart := time.Now()
	preloadFlatDiffReads(tmpDB, flatDiff)
	pipelineSRCPreloadTimer.UpdateSince(preloadStart)
}

// openSRCStateDB opens a StateDB at parentRoot for the pipelined SRC goroutine.
// Reader choice depends on makeWitness:
//
//   - makeWitness=true:  NewTrieOnly. Every read walks the MPT, which is what
//     lets the witness capture proof-path nodes for FlatDiff overlay accounts
//     whose trie nodes weren't touched during speculative execution. Flat
//     readers would short-circuit the trie and leave the witness incomplete.
//   - makeWitness=false: state.New (multi-reader). Pre-state reads performed
//     by ApplyFlatDiffForCommitFast (original account/storage lookups for
//     existing objects and pure self-destructs) can hit a flat reader (pathdb
//     StateReader in path mode, snapshot in hash mode) instead of the MPT.
//     state.New falls back to the trie reader when no flat reader is
//     installed or StateReader errors, so correctness does not depend on the
//     flat reader being present. Root-consistency between readers at an
//     in-memory committed root is validated by the parity tests.
//     CommitWithUpdate's MPT walk resolves recently rewritten paths through
//     the pathdb node index (one probe per diff-layer node) rather than any
//     SRC-local warm cache.
//
// Witness ownership when makeWitness=true:
//
//   - execWitness != nil: caller hands in the witness already populated by
//     EVM execution (AddCode + AddBlockHash + execution-time AddState). SRC
//     reuses it by attaching it to tmpDB so subsequent AddState calls during
//     ApplyFlatDiffForCommit and CommitWithUpdate land in the same object.
//   - execWitness == nil: only legal for call sites that opted in via
//     allowOwnWitness=true at the SpawnSRCGoroutine call site. SRC creates
//     its own witness, which contains only entries collected by the SRC path.
//     Callers that require execution-time witness entries must pass
//     execWitness != nil (enforced at the top of runSRCCompute).
//
// BorWitness serialises Headers and State only. Codes collected on the
// in-memory witness are not part of the canonical Bor witness wire format.
//
// CommitWithUpdate walks the MPT for hashing regardless of reader choice, so
// the state-root computation cost is unaffected; only the pre-state reads
// avoid cold trie traversals.
func (bc *BlockChain) openSRCStateDB(parentRoot common.Hash, block *types.Block, makeWitness bool, execWitness *stateless.Witness, warmSnapshot *state.WarmSnapshot) (*state.StateDB, *stateless.Witness, error) {
	if !makeWitness {
		// Witness-off path keeps the multi-reader: flat readers short-circuit
		// value reads, and commit-time trie node reads are served by the
		// pathdb node index.
		tmpDB, err := state.New(parentRoot, bc.statedb)
		if err != nil {
			log.Error("Pipelined SRC: failed to open tmpDB", "parentRoot", parentRoot, "err", err)
			return nil, nil, err
		}
		return tmpDB, nil, nil
	}
	// Witness-on path uses NewTrieOnly so every read walks the MPT and the
	// witness captures proof-path nodes. When a warm snapshot is supplied,
	// install a snapshot-aware reader: trie reads consult the snapshot
	// (hash-verified) before falling through to pathdb. NewTrieOnly
	// semantics, prevalueTracer recording, and witness completeness are
	// unaffected — the snapshot only short-circuits the underlying
	// NodeReader fetch.
	tmpDB, err := state.NewTrieOnlyWithSnapshot(parentRoot, bc.statedb, warmSnapshot)
	if err != nil {
		log.Error("Pipelined SRC: failed to open tmpDB", "parentRoot", parentRoot, "err", err)
		return nil, nil, err
	}
	witness := execWitness
	if witness == nil {
		// Miner / legacy fallback only; runSRCCompute already rejected this
		// branch for the import path via the allowOwnWitness check.
		newWitness, witnessErr := stateless.NewWitness(block.Header(), bc)
		if witnessErr != nil {
			log.Warn("Pipelined SRC: failed to create witness", "block", block.NumberU64(), "err", witnessErr)
			return tmpDB, nil, nil
		}
		witness = newWitness
	}
	tmpDB.SetWitness(witness)
	return tmpDB, witness, nil
}

// preloadFlatDiffReads touches every address/slot in the FlatDiff's read
// surface so the witness sees the proof-path trie nodes even when the
// speculative execution used the flat overlay. Covers:
// - ReadSet accounts (+ their ReadStorage slots)
// - Read-only storage for mutated accounts (ReadStorage)
// - Pure-destruct accounts (no resurrection)
// - Non-existent address reads (proof-of-absence)
func preloadFlatDiffReads(tmpDB *state.StateDB, flatDiff *state.FlatDiff) {
	for _, addr := range flatDiff.ReadSet {
		tmpDB.GetBalance(addr)
	}
	// Iterate ReadStorage directly rather than via ReadSet/Accounts: it can
	// carry slots for addresses in neither list (e.g. drained reader-cache
	// reads), and repeated account touches are cached no-ops anyway.
	for addr, slots := range flatDiff.ReadStorage {
		tmpDB.GetBalance(addr)
		for _, slot := range slots {
			tmpDB.GetState(addr, slot)
		}
	}
	for addr := range flatDiff.Destructs {
		if _, resurrected := flatDiff.Accounts[addr]; !resurrected {
			tmpDB.GetBalance(addr)
		}
	}
	for _, addr := range flatDiff.NonExistentReads {
		tmpDB.GetBalance(addr)
	}
}

// emitSRCStateDBMetrics reports the hash/update/commit timers from the
// trie-only statedb. These mirror the import-path names in both modes so
// dashboards work whether pipelining is on or off.
func emitSRCStateDBMetrics(tmpDB *state.StateDB) {
	accountHashTimer.Update(tmpDB.AccountHashes)
	storageHashTimer.Update(tmpDB.StorageHashes)
	accountUpdateTimer.Update(tmpDB.AccountUpdates)
	storageUpdateTimer.Update(tmpDB.StorageUpdates)
	accountCommitTimer.Update(tmpDB.AccountCommits)
	storageCommitTimer.Update(tmpDB.StorageCommits)
	snapshotCommitTimer.Update(tmpDB.SnapshotCommits)
	triedbCommitTimer.Update(tmpDB.TrieDBCommits)
	witnessCollectionTimer.Update(tmpDB.WitnessCollection)
}

// encodeAndCachePendingWitness RLP-encodes the witness (complete only after
// CommitWithUpdate has run) and pushes it into the pending state + cache.
// For imported blocks the hash is already final; for mined blocks the real
// hash isn't known until Seal() finalises Extra, so the caller retrieves
// the bytes via WaitForSRC and writes to DB under the sealed hash in
// resultLoop.
func (bc *BlockChain) encodeAndCachePendingWitness(pending *pendingSRCState, witness *stateless.Witness, block *types.Block) {
	if witness == nil {
		return
	}
	var witBuf bytes.Buffer
	encodeStart := time.Now()
	if err := witness.EncodeRLP(&witBuf); err != nil {
		log.Error("Pipelined SRC: failed to encode witness", "block", block.NumberU64(), "err", err)
		return
	}
	witnessEncodeTimer.UpdateSince(encodeStart)
	pending.witness = witBuf.Bytes()
	bc.witnessCache.Add(block.Hash(), pending.witness)
}

// WaitForSRC blocks until the pending SRC goroutine completes and returns the
// computed state root and RLP-encoded witness. The witness may be nil if witness
// creation failed or was not applicable. Returns an error if the goroutine
// failed or no SRC is pending.
func (bc *BlockChain) WaitForSRC() (common.Hash, []byte, error) {
	bc.pendingSRCMu.Lock()
	pending := bc.pendingSRC
	bc.pendingSRCMu.Unlock()

	return waitForSRCState(pending)
}

func waitForSRCState(pending *pendingSRCState) (common.Hash, []byte, error) {
	if pending == nil {
		return common.Hash{}, nil, errors.New("no pending SRC goroutine")
	}

	pending.wg.Wait()
	if pending.err != nil {
		return common.Hash{}, nil, pending.err
	}
	return pending.root, pending.witness, nil
}

// flushPendingImportSRC waits for the auto-collection goroutine to finish
// and clears the pending state. Called on shutdown and when an incoming block
// doesn't follow the pending one (reorg/gap).
//
// rollback controls what happens when the collection failed (SRC error or
// root mismatch): the pending block is already exposed as head with durable
// canonical markers, so every caller that holds chainmu must pass true and
// have the failed block rolled back — otherwise the rejected block keeps its
// canonical hash, tx lookups and subscriber view. Only the shutdown path
// passes false: it cannot take chainmu, and the startup rewind moves the head
// off the unverified block anyway.
func (bc *BlockChain) flushPendingImportSRC(rollback bool) error {
	bc.pendingImportSRCMu.Lock()
	pending := bc.pendingImportSRC
	bc.pendingImportSRC = nil
	bc.pendingImportSRCMu.Unlock()

	if pending == nil {
		return nil
	}

	pipelineImportFallbackCounter.Inc(1)

	// Wait for auto-collection to finish (it handles verify, witness, trie GC)
	<-pending.collectedCh
	if pending.collectedErr != nil && rollback {
		bc.recoverFailedPipelinedImport(pending)
	}
	return pending.collectedErr
}

// collectPendingImportSRC collects the pending import SRC goroutine, writes
// the previous block, and returns the new committed root. Unlike flush, this
// does NOT clear pendingImportSRC (the caller replaces it with the new block).
// collectPendingImportSRC waits for the auto-collection goroutine to finish
// and returns the committed root. The actual work (verify root, write witness,
// trie GC) is done by the auto-collection goroutine spawned alongside the SRC.
func (bc *BlockChain) collectPendingImportSRC() (common.Hash, error) {
	bc.pendingImportSRCMu.Lock()
	pending := bc.pendingImportSRC
	bc.pendingImportSRCMu.Unlock()

	if pending == nil {
		return common.Hash{}, errors.New("no pending import SRC")
	}

	// Wait for auto-collection goroutine to finish
	<-pending.collectedCh

	if pending.collectedErr != nil {
		// The collector deliberately leaves the rollback to us: this thread
		// holds chainmu for the whole batch, so the head rewrite has to happen
		// here rather than in the goroutine we just waited on.
		bc.recoverFailedPipelinedImport(pending)
		return common.Hash{}, pending.collectedErr
	}
	return pending.collectedRoot, nil
}

func (bc *BlockChain) markPendingImportHeadState(block *types.Block) {
	bc.pendingImportSRCMu.Lock()
	defer bc.pendingImportSRCMu.Unlock()

	// writeBlockAndSetHeadPipelined exposes the new head before the SRC
	// goroutine is registered below. Mark it so runtime sync-mode probes don't
	// misclassify the handoff as a real missing-state condition.
	bc.pendingImportHeadHash = block.Hash()
	bc.pendingImportHeadRoot = block.Root()
	bc.pendingImportHeadStart = time.Now()
}

func (bc *BlockChain) clearPendingImportHeadState(block *types.Block) {
	bc.pendingImportSRCMu.Lock()
	defer bc.pendingImportSRCMu.Unlock()

	if bc.pendingImportHeadHash != block.Hash() || bc.pendingImportHeadRoot != block.Root() {
		return
	}
	// Once pendingImportSRC is visible, the normal pending-SRC check owns the
	// handoff state. Clear this temporary marker so stale heads don't mask
	// unrelated missing-state failures.
	bc.pendingImportHeadHash = common.Hash{}
	bc.pendingImportHeadRoot = common.Hash{}
	bc.pendingImportHeadStart = time.Time{}
}

// handleImportTrieGC performs trie garbage collection after a pipelined import
// SRC has committed the state. Replicates writeBlockWithState's GC logic.
func (bc *BlockChain) handleImportTrieGC(root common.Hash, blockNum uint64, procTime time.Duration) {
	bc.gcproc += procTime
	if bc.triedb.Scheme() == rawdb.PathScheme {
		return
	}
	if bc.cfg.ArchiveMode {
		_ = bc.triedb.Commit(root, false)
		return
	}
	bc.triedb.Reference(root, common.Hash{})
	bc.triegc.Push(root, -int64(blockNum))

	triesInMemory := bc.cfg.GetTriesInMemory()
	if blockNum <= triesInMemory {
		return
	}
	bc.capTrieIfDirty()
	chosen := blockNum - triesInMemory
	bc.maybeFlushChosen(chosen, triesInMemory)
	bc.dereferenceUpTo(chosen)
}

// capTrieIfDirty flushes dirty trie nodes to disk when either node memory
// or preimages exceed their configured limits. Uses IdealBatchSize as a
// margin so the cap leaves room for further inserts before the next check.
func (bc *BlockChain) capTrieIfDirty() {
	_, nodes, imgs := bc.triedb.Size()
	limit := common.StorageSize(bc.cfg.TrieDirtyLimit) * 1024 * 1024
	if nodes > limit || imgs > 4*1024*1024 {
		_ = bc.triedb.Cap(limit - ethdb.IdealBatchSize)
	}
}

// maybeFlushChosen commits state at block `chosen` when accumulated
// processing time has crossed the flush interval. Skips on reorg (chosen
// header missing); logs a warning when we're overdue vs. the optimum ratio.
func (bc *BlockChain) maybeFlushChosen(chosen, triesInMemory uint64) {
	flushInterval := time.Duration(bc.flushInterval.Load())
	if bc.gcproc <= flushInterval {
		return
	}
	header := bc.GetHeaderByNumber(chosen)
	if header == nil {
		log.Warn("Reorg in progress, trie commit postponed", "number", chosen)
		return
	}
	if chosen < bc.lastWrite+triesInMemory && bc.gcproc >= 2*flushInterval {
		log.Info("State in memory for too long, committing",
			"time", bc.gcproc, "allowance", flushInterval,
			"optimum", float64(chosen-bc.lastWrite)/float64(triesInMemory))
	}
	_ = bc.triedb.Commit(header.Root, true)
	bc.lastWrite = chosen
	bc.gcproc = 0
}

// dereferenceUpTo drops GC references for every cached trie root at or
// below `chosen`, freeing the memory held for reorg-safety. Roots above
// `chosen` are pushed back so we stop at the first still-in-memory entry.
func (bc *BlockChain) dereferenceUpTo(chosen uint64) {
	for !bc.triegc.Empty() {
		r, number := bc.triegc.Pop()
		if uint64(-number) > chosen {
			bc.triegc.Push(r, number)
			return
		}
		bc.triedb.Dereference(r)
	}
}

// pipelineReaderRoot returns the trie root to open state readers against
// during pipelined import. The block's parent.Root may not be committed
// yet (the SRC goroutine for the parent is still running), so we fall back
// to the last-committed root stored on the PipelineImportOpts. Callers
// combine this with applyFlatDiffOverlayToAll to see post-execution state.
func pipelineReaderRoot(parent *types.Header, pipeOpts *PipelineImportOpts) common.Hash {
	if pipeOpts != nil {
		return pipeOpts.CommittedParentRoot
	}
	return parent.Root
}

// applyFlatDiffOverlayToAll attaches the pipelined FlatDiff to every
// statedb so reads see the previous block's post-execution values without
// waiting for the SRC goroutine to commit the trie. No-op when pipelining
// is off or the overlay is absent.
func applyFlatDiffOverlayToAll(pipeOpts *PipelineImportOpts, dbs ...*state.StateDB) {
	if pipeOpts == nil || pipeOpts.FlatDiff == nil {
		return
	}
	for _, db := range dbs {
		db.SetFlatDiffRef(pipeOpts.FlatDiff)
	}
}

// validateStateForPipeline dispatches to the cheap validator under
// pipelined import (gas + bloom + receipt root only; the full root match
// happens later in the SRC goroutine) and to the full validator otherwise.
// Centralising this keeps ProcessBlock's parallel/serial branches symmetric.
func validateStateForPipeline(validator Validator, block *types.Block, statedb *state.StateDB, res *ProcessResult, pipeOpts *PipelineImportOpts) error {
	if pipeOpts != nil {
		return validator.ValidateStateCheap(block, statedb, res)
	}
	return validator.ValidateState(block, statedb, res, false)
}

// pipelinedImportPersistTimings captures the synchronous post-execution phases
// that are included in the "Imported new chain segment" elapsed time but are
// not part of ProcessBlock itself.
type pipelinedImportPersistTimings struct {
	witnessCapture  time.Duration
	prefetchDetach  time.Duration
	prefetchCleanup time.Duration
	commitSnapshot  time.Duration
	collect         time.Duration
	collectTotal    time.Duration
	stateSyncFeed   time.Duration
	reorgCheck      time.Duration
	setFlatDiff     time.Duration
	writeHead       time.Duration
	buildSRCBlock   time.Duration
	spawnSRC        time.Duration
	pendingPublish  time.Duration
	residual        time.Duration
	total           time.Duration
}

func (t pipelinedImportPersistTimings) accounted() time.Duration {
	return t.witnessCapture +
		t.commitSnapshot +
		t.prefetchDetach +
		t.prefetchCleanup +
		t.collectTotal +
		t.stateSyncFeed +
		t.reorgCheck +
		t.setFlatDiff +
		t.writeHead +
		t.buildSRCBlock +
		t.spawnSRC +
		t.pendingPublish
}

// persistPipelinedImport handles the post-ProcessBlock work for a pipelined
// imported block: extract FlatDiff, collect any still-pending SRC from the
// previous block, publish the state-sync feed, write block metadata
// immediately (so sync protocol sees it), spawn a new SRC goroutine, and
// start auto-collection. adjustBack=true signals the caller to decrement
// it.index when returning the error (because the failure belongs to the
// previously pending block, not the current one).
func (bc *BlockChain) persistPipelinedImport(block *types.Block, parent *types.Header, statedb *state.StateDB, receipts []*types.Receipt, logs []*types.Log, start time.Time, cheapExec, validation time.Duration, makeWitness bool) (adjustBack bool, err error) {
	persistStart := time.Now()
	timings := pipelinedImportPersistTimings{}
	defer func() {
		timings.total = time.Since(persistStart)
		if accounted := timings.accounted(); timings.total > accounted {
			timings.residual = timings.total - accounted
		}
		pipelineImportPostExecTimer.Update(timings.total)
		pipelineImportPostExecResidualTimer.Update(timings.residual)
		bc.logSlowPipelinedImport(block, time.Since(start), cheapExec, validation, timings, statedb)
	}()
	// Capture the execution witness so SRC can complete it. The trie
	// prefetcher does not write to this witness — subfetcher.loop only
	// populates trie-local prevalueTracer state — so the witness can be handed
	// to SRC independently of the detached prefetcher. See LINEAR OWNERSHIP
	// INVARIANT at runSRCCompute.
	var execWitness *stateless.Witness
	phaseStart := time.Now()
	bc.pipelinedMakeWitness.Store(makeWitness)
	if makeWitness {
		execWitness = statedb.Witness()
	}
	timings.witnessCapture = time.Since(phaseStart)
	pipelineImportWitnessCaptureTimer.Update(timings.witnessCapture)

	phaseStart = time.Now()
	flatDiff := statedb.CommitSnapshot(bc.chainConfig.IsEIP158(block.Number()))
	timings.commitSnapshot = time.Since(phaseStart)
	pipelineImportCommitSnapshotTimer.Update(timings.commitSnapshot)

	// The pipelined path doesn't commit this StateDB; SRC opens its own tmpDB.
	// Detach the execution prefetcher after CommitSnapshot so Finalise can
	// still enqueue the dirty-object prefetch work it normally would. The
	// import thread does not wait here: SRC owns the returned handle and will
	// synchronously stop/report it before computing the root. If an error
	// occurs before the handle is handed to SRC, the defer consumes it to avoid
	// leaking prefetcher goroutines.
	var detachedPrefetcher *state.DetachedPrefetcher
	phaseStart = time.Now()
	detachedPrefetcher = statedb.DetachPrefetcher()
	timings.prefetchDetach = time.Since(phaseStart)
	pipelineImportPrefetchDetachTimer.Update(timings.prefetchDetach)
	defer func() {
		if detachedPrefetcher != nil {
			cleanupStart := time.Now()
			finishDetachedPrefetcher(detachedPrefetcher, false)
			timings.prefetchCleanup = time.Since(cleanupStart)
			pipelineImportPrefetchCleanupTimer.Update(timings.prefetchCleanup)
		}
	}()

	phaseStart = time.Now()
	committedRoot, collectElapsed, err := bc.collectPrevImportSRCIfAny(block, parent)
	timings.collectTotal = time.Since(phaseStart)
	pipelineImportCollectTotalTimer.Update(timings.collectTotal)
	timings.collect = collectElapsed
	if err != nil {
		return true, err
	}
	phaseStart = time.Now()
	bc.emitStateSyncFeed()
	timings.stateSyncFeed = time.Since(phaseStart)
	pipelineImportStateSyncFeedTimer.Update(timings.stateSyncFeed)

	// Verify the block against the whitelisted milestone/checkpoint. Mirrors
	// the non-pipelined path's per-block check — guards the race where Heimdall
	// whitelists a milestone AFTER the upfront check at the start of insertChain
	// but BEFORE this block is written. The block itself is passed as the
	// current head so the validation treats it as a `past` chain.
	phaseStart = time.Now()
	isValid, err := bc.forker.ValidateReorg(block.Header(), []*types.Header{block.Header()})
	timings.reorgCheck = time.Since(phaseStart)
	pipelineImportReorgCheckTimer.Update(timings.reorgCheck)
	if err != nil {
		return false, err
	}
	if !isValid {
		return false, whitelist.ErrMismatch
	}

	// Store FlatDiff BEFORE writing metadata. writeBlockAndSetHeadPipelined
	// emits ChainEvent which triggers subscribers that read state; FlatDiff
	// must be available so PostExecState works for those reads.
	phaseStart = time.Now()
	bc.SetLastFlatDiff(flatDiff, block.NumberU64(), committedRoot, block.Root())
	timings.setFlatDiff = time.Since(phaseStart)
	pipelineImportSetFlatDiffTimer.Update(timings.setFlatDiff)
	// State commit is deferred to the SRC goroutine. Split the write path so
	// the durable block batch and reorg/status checks complete before SRC
	// starts, then let SRC overlap the synchronous head/event publication tail.
	writeHeadStart := time.Now()
	bc.markPendingImportHeadState(block)
	status, stateSyncLogs, err := bc.writePipelinedBlockAndResolveStatus(block, receipts, logs, statedb, nil)
	writePrepareElapsed := time.Since(writeHeadStart)
	if err != nil {
		bc.clearPendingImportHeadState(block)
		timings.writeHead = writePrepareElapsed
		pipelineImportWriteHeadTimer.Update(timings.writeHead)
		return false, err
	}

	phaseStart = time.Now()
	tmpBlock := types.NewBlockWithHeader(block.Header()).WithBody(*block.Body())
	timings.buildSRCBlock = time.Since(phaseStart)
	pipelineImportBuildSRCBlockTimer.Update(timings.buildSRCBlock)
	// Import passes execWitness from execution and requires SRC to publish
	// that same witness object. runSRCCompute hard-fails on a nil witness
	// when allowOwnWitness=false. The detached prefetcher is always passed in
	// if present; the warm-snapshot flag only controls whether SRC converts
	// the finished prefetcher into a WarmSnapshot or simply waits/reports and
	// discards it.
	phaseStart = time.Now()
	useWarmSnapshot := makeWitness && bc.cfg.PipelinedSRCWarmSnapshot
	src := bc.spawnSRCGoroutine(tmpBlock, committedRoot, flatDiff, makeWitness, execWitness, false, detachedPrefetcher, useWarmSnapshot, false)
	detachedPrefetcher = nil
	timings.spawnSRC = time.Since(phaseStart)
	pipelineImportSpawnSRCTimer.Update(timings.spawnSRC)

	publishStart := time.Now()
	bc.emitPostWriteEvents(block, receipts, logs, stateSyncLogs, status, false)
	timings.writeHead = writePrepareElapsed + time.Since(publishStart)
	pipelineImportWriteHeadTimer.Update(timings.writeHead)

	phaseStart = time.Now()
	newPending := &pendingImportSRCState{
		block:         block,
		flatDiff:      flatDiff,
		committedRoot: committedRoot,
		procTime:      time.Since(start),
		blockStart:    start,
		makeWitness:   makeWitness,
		src:           src,
		collectedCh:   make(chan struct{}),
	}
	bc.pendingImportSRCMu.Lock()
	bc.pendingImportSRC = newPending
	bc.pendingImportSRCMu.Unlock()
	bc.clearPendingImportHeadState(block)
	bc.wg.Add(1)
	go bc.runImportAutoCollection(newPending)
	if bc.cfg.PipelinedImportSRCLogs {
		log.Info("Pipelined import: spawned SRC",
			"block", block.NumberU64(), "committedRoot", committedRoot,
			"txs", len(block.Transactions()))
	}
	timings.pendingPublish = time.Since(phaseStart)
	pipelineImportPendingPublishTimer.Update(timings.pendingPublish)
	return false, nil
}

func (bc *BlockChain) logSlowPipelinedImport(block *types.Block, total, cheapExec, validation time.Duration, timings pipelinedImportPersistTimings, statedb *state.StateDB) {
	if total < slowImportBlockThreshold &&
		timings.total < slowImportPostExecThreshold &&
		timings.collect < slowImportCollectThreshold &&
		timings.prefetchDetach < slowImportSnapshotThreshold &&
		timings.residual < slowImportResidualThreshold {
		return
	}
	log.Warn("Slow pipelined import phase",
		"block", block.NumberU64(),
		"txs", len(block.Transactions()),
		"mgas", float64(block.GasUsed())/1_000_000,
		"total", common.PrettyDuration(total),
		"cheapExec", common.PrettyDuration(cheapExec),
		"validation", common.PrettyDuration(validation),
		"postExec", common.PrettyDuration(timings.total),
		"postExecAccounted", common.PrettyDuration(timings.accounted()),
		"postExecResidual", common.PrettyDuration(timings.residual),
		"witnessCapture", common.PrettyDuration(timings.witnessCapture),
		"prefetchDetach", common.PrettyDuration(timings.prefetchDetach),
		"prefetchCleanup", common.PrettyDuration(timings.prefetchCleanup),
		"commitSnapshot", common.PrettyDuration(timings.commitSnapshot),
		"collect", common.PrettyDuration(timings.collect),
		"collectTotal", common.PrettyDuration(timings.collectTotal),
		"stateSyncFeed", common.PrettyDuration(timings.stateSyncFeed),
		"reorgCheck", common.PrettyDuration(timings.reorgCheck),
		"setFlatDiff", common.PrettyDuration(timings.setFlatDiff),
		"writeHead", common.PrettyDuration(timings.writeHead),
		"buildSRCBlock", common.PrettyDuration(timings.buildSRCBlock),
		"spawnSRC", common.PrettyDuration(timings.spawnSRC),
		"pendingPublish", common.PrettyDuration(timings.pendingPublish),
		"accountReads", common.PrettyDuration(statedb.AccountReads),
		"storageReads", common.PrettyDuration(statedb.StorageReads),
		"snapshotAccountReads", common.PrettyDuration(statedb.SnapshotAccountReads),
		"snapshotStorageReads", common.PrettyDuration(statedb.SnapshotStorageReads),
		"accountUpdates", common.PrettyDuration(statedb.AccountUpdates),
		"storageUpdates", common.PrettyDuration(statedb.StorageUpdates),
		"accountHashes", common.PrettyDuration(statedb.AccountHashes),
		"storageHashes", common.PrettyDuration(statedb.StorageHashes),
		"witnessCollection", common.PrettyDuration(statedb.WitnessCollection))
}

func (bc *BlockChain) logSlowNormalImport(block *types.Block, process, validation, reorgCheck, write, total time.Duration, statedb *state.StateDB) {
	if total < slowImportBlockThreshold && write < slowImportPostExecThreshold {
		return
	}
	log.Warn("Slow normal import phase",
		"block", block.NumberU64(),
		"txs", len(block.Transactions()),
		"mgas", float64(block.GasUsed())/1_000_000,
		"total", common.PrettyDuration(total),
		"process", common.PrettyDuration(process),
		"validation", common.PrettyDuration(validation),
		"reorgCheck", common.PrettyDuration(reorgCheck),
		"write", common.PrettyDuration(write),
		"accountReads", common.PrettyDuration(statedb.AccountReads),
		"storageReads", common.PrettyDuration(statedb.StorageReads),
		"accountUpdates", common.PrettyDuration(statedb.AccountUpdates),
		"storageUpdates", common.PrettyDuration(statedb.StorageUpdates),
		"accountHashes", common.PrettyDuration(statedb.AccountHashes),
		"storageHashes", common.PrettyDuration(statedb.StorageHashes),
		"accountCommits", common.PrettyDuration(statedb.AccountCommits),
		"storageCommits", common.PrettyDuration(statedb.StorageCommits),
		"snapshotCommits", common.PrettyDuration(statedb.SnapshotCommits),
		"trieDBCommits", common.PrettyDuration(statedb.TrieDBCommits),
		"witnessCollection", common.PrettyDuration(statedb.WitnessCollection))
}

// collectPrevImportSRCIfAny blocks on the auto-collection channel of the
// previous pending SRC (if any) and returns its committed root. If no SRC
// is pending (first block of the insertChain call), parent.Root is the
// committed root. Errors propagate as "this block belongs to the previous
// pending one" — caller returns it.index - 1.
func (bc *BlockChain) collectPrevImportSRCIfAny(block *types.Block, parent *types.Header) (common.Hash, time.Duration, error) {
	bc.pendingImportSRCMu.Lock()
	pending := bc.pendingImportSRC
	bc.pendingImportSRCMu.Unlock()
	if pending == nil {
		return parent.Root, 0, nil
	}
	if bc.cfg.PipelinedImportSRCLogs {
		log.Info("Pipelined import: collecting previous SRC",
			"block", block.NumberU64(), "pendingBlock", pending.block.NumberU64())
	}
	collectStart := time.Now()
	committedRoot, err := bc.collectPendingImportSRC()
	elapsed := time.Since(collectStart)
	pipelineImportCollectTimer.Update(elapsed)
	// SRC wall-clock is final now (collection joined the goroutine); emit the
	// next-exec-overlap split using the classification this block's execution set.
	recordPipelinedImportSRCOverlapSplit(pending.src)
	return committedRoot, elapsed, err
}

// emitStateSyncFeed publishes any queued state-sync events under the
// stateSyncMu read lock. Kept separate from writeBlockAndSetHeadPipelined
// so the import path can control when subscribers see them (before the
// FlatDiff is published, so PostExecState overlays work).
func (bc *BlockChain) emitStateSyncFeed() {
	bc.stateSyncMu.RLock()
	defer bc.stateSyncMu.RUnlock()
	for _, data := range bc.getStateSyncLocked() {
		bc.stateSyncFeed.Send(StateSyncEvent{Data: data})
	}
}

// buildPipelineImportOpts inspects the current pending SRC state and returns
// the PipelineImportOpts the next ProcessBlock should use. If the pending
// block is block.Parent, the next block can overlay the FlatDiff (true
// cross-call overlap). Otherwise the pending state is flushed (reorg/gap)
// and the block enters the pipeline fresh against parent.Root.
func (bc *BlockChain) buildPipelineImportOpts(block *types.Block, parent *types.Header) *PipelineImportOpts {
	bc.pendingImportSRCMu.Lock()
	pending := bc.pendingImportSRC
	bc.pendingImportSRCMu.Unlock()
	var opts *PipelineImportOpts
	if pending != nil {
		if block.ParentHash() == pending.block.Hash() {
			pipelineImportHitCounter.Inc(1)
			opts = &PipelineImportOpts{
				CommittedParentRoot: pending.committedRoot,
				FlatDiff:            pending.flatDiff,
				Mode:                pipelineImportModeFlatDiff,
				PendingBlock:        pending.block.NumberU64(),
				PendingHash:         pending.block.Hash(),
				PendingCollected:    pendingImportSRCCollected(pending),
				pendingSRC:          pending.src,
			}
			if bc.cfg.PipelinedImportSRCLogs {
				attrs := []interface{}{"block", block.NumberU64(), "txs", len(block.Transactions())}
				attrs = append(attrs, pipelineImportLogAttrs(parent, opts)...)
				log.Info("Pipelined import: started processing block", attrs...)
			}
			return opts
		}
		pipelineImportMissCounter.Inc(1)
		// The import continues on the incoming (reorg/gap) block after this,
		// so a failed collection must roll the rejected pending block back
		// here — otherwise it keeps its canonical markers while the chain
		// moves on around it.
		if err := bc.flushPendingImportSRC(true); err != nil {
			log.Error("Pipelined import: flush failed on mismatch", "err", err)
		}
	}
	// First block in pipeline — still enter it so the SRC goroutine persists
	// for the next insertChain call, enabling cross-call overlap.
	opts = &PipelineImportOpts{
		CommittedParentRoot: parent.Root,
		Mode:                pipelineImportModeDirect,
	}
	if bc.cfg.PipelinedImportSRCLogs {
		attrs := []interface{}{"block", block.NumberU64(), "txs", len(block.Transactions())}
		attrs = append(attrs, pipelineImportLogAttrs(parent, opts)...)
		log.Info("Pipelined import: started processing block", attrs...)
	}
	return opts
}

// runImportAutoCollection waits for a pending import SRC to finish, verifies
// the computed state root, writes the witness and emits WitnessReadyEvent,
// then does trie GC. Any failure is captured on p so flushPendingImportSRC/
// collectPendingImportSRC can surface it synchronously.
func (bc *BlockChain) runImportAutoCollection(p *pendingImportSRCState) {
	defer bc.wg.Done()
	autoCollectStart := time.Now()
	// Defer order is LIFO: this runs before bc.wg.Done above, matching the
	// original behaviour where close(p.collectedCh) happens before wg.Done.
	// The total timer wraps the full goroutine wall time so the main path's
	// collect-wait can be reconciled against (src + verify + publish + gc).
	defer func() {
		pipelineImportAutoCollectTotalTimer.UpdateSince(autoCollectStart)
		close(p.collectedCh)
	}()
	srcStart := time.Now()
	root, witnessBytes, err := waitForSRCState(p.src)
	pipelineImportSRCTimer.UpdateSince(srcStart)
	if err != nil {
		log.Error("Pipelined import: SRC goroutine failed", "block", p.block.NumberU64(), "err", err)
		p.collectedErr = err
		return
	}
	verifyStart := time.Now()
	verifyOk := bc.verifyImportSRCRoot(p, root)
	pipelineImportAutoCollectVerifyTimer.UpdateSince(verifyStart)
	if !verifyOk {
		return
	}
	p.collectedRoot = root
	if bc.cfg.PipelinedImportSRCLogs {
		log.Info("Pipelined import: SRC verified", "block", p.block.NumberU64(), "root", root)
	}
	publishStart := time.Now()
	bc.publishImportWitness(p, witnessBytes)
	pipelineImportAutoCollectPublishTimer.UpdateSince(publishStart)
	if !p.blockStart.IsZero() {
		witnessReadyEndToEndTimer.UpdateSince(p.blockStart)
	}
	gcStart := time.Now()
	bc.handleImportTrieGC(root, p.block.NumberU64(), p.procTime)
	pipelineImportAutoCollectGCTimer.UpdateSince(gcStart)
	pipelineImportBlocksCounter.Inc(1)
}

// verifyImportSRCRoot compares the SRC-computed root with the imported
// block's root. On mismatch (should never happen — a mismatch means SRC
// diverged from the block the peer sent), reverts the chain head to the
// parent and surfaces the error on p. Returns false on mismatch.
// verifyImportSRCRoot compares the SRC-computed root with the imported block's
// root. On mismatch it records the failure on p; the rollback itself is left to
// whoever collects p. This goroutine must not touch chainmu: the collecting
// thread holds it for the whole insertChain batch while blocked on
// p.collectedCh, and syncx.ClosableMutex.TryLock is a blocking receive that
// only reports failure once the mutex is closed — acquiring it here would
// deadlock both sides permanently.
func (bc *BlockChain) verifyImportSRCRoot(p *pendingImportSRCState, root common.Hash) bool {
	if root == p.block.Root() {
		return true
	}
	pipelineImportRootMismatchCounter.Inc(1)
	p.divergentRoot = root
	p.collectedErr = fmt.Errorf("pipelined import: root mismatch (expected: %x got: %x) block: %d",
		p.block.Root(), root, p.block.NumberU64())
	log.Error("Pipelined import: root mismatch, chain head will be rolled back",
		"block", p.block.NumberU64(), "expected", p.block.Root(), "got", root)
	bc.reportBlock(p.block, nil, p.collectedErr)
	return false
}

// recoverFailedPipelinedImport rolls the chain back to the parent of a
// pipelined block whose SRC failed or produced a divergent root. Because the
// pipeline publishes the head before the root is verified, the rollback has to
// undo more than the head pointer: the durable canonical/tx-lookup markers, the
// FlatDiff overlay that keeps serving the rejected block's post-state, the
// pending entry (otherwise every later insert re-collects the same failure and
// the node stops following the chain), and the subscriber view.
//
// Must be called with chainmu held, from the thread collecting p.
func (bc *BlockChain) recoverFailedPipelinedImport(p *pendingImportSRCState) {
	// Drop the pending entry first: leaving it in place is what turns a
	// single failed block into a permanent import wedge.
	bc.pendingImportSRCMu.Lock()
	if bc.pendingImportSRC == p {
		bc.pendingImportSRC = nil
	}
	bc.pendingImportSRCMu.Unlock()

	// Stop serving the rejected block's post-state via StateAt/PostExecState.
	bc.SetLastFlatDiff(nil, 0, common.Hash{}, common.Hash{})

	bad := p.block
	parent := bc.GetBlock(bad.ParentHash(), bad.NumberU64()-1)
	if parent == nil {
		log.Error("Pipelined import: cannot roll back rejected block, parent missing",
			"block", bad.NumberU64(), "parent", bad.ParentHash())
		return
	}

	// The rejected block's logs were announced with Removed=false when the
	// pipeline exposed it as head. Collect them before its indexes go away so
	// filter/subscription clients get the same retraction a reorg would send.
	deletedLogs := bc.collectLogs(bad, true)

	// writeHeadBlock only rewrites the markers of the block handed to it, and
	// SetCurrentHeader deletes nothing, so the rejected block's canonical hash
	// and transaction lookups would keep resolving through RPC after the head
	// moved back. Remove them explicitly — and not just for the rejected block:
	// SRC verification trails the insert, so a batch can publish descendants of
	// the rejected block before its failure surfaces. Their markers must go
	// too, or number-based lookups keep resolving blocks above the head.
	batch := bc.db.NewBatch()
	for n := bad.NumberU64(); ; n++ {
		hash := rawdb.ReadCanonicalHash(bc.db, n)
		if hash == (common.Hash{}) {
			break
		}
		rawdb.DeleteCanonicalHash(batch, n)
		if blk := bc.GetBlock(hash, n); blk != nil {
			for _, tx := range blk.Transactions() {
				rawdb.DeleteTxLookupEntry(batch, tx.Hash())
			}
		}
	}
	if err := batch.Write(); err != nil {
		log.Crit("Failed to remove rejected pipelined block indexes", "err", err)
	}
	bc.writeHeadBlock(parent)

	if len(deletedLogs) > 0 {
		bc.rmLogsFeed.Send(RemovedLogsEvent{deletedLogs})
	}

	// SRC committed its own (divergent) root before the mismatch was noticed.
	// Nothing references it, but in hash mode it holds trie nodes alive until
	// dereferenced. Path mode has no per-root release here; the layer is not
	// reachable from any canonical head and is dropped on restart.
	if p.divergentRoot != (common.Hash{}) && bc.triedb.Scheme() == rawdb.HashScheme {
		bc.triedb.Dereference(p.divergentRoot)
	}

	// Subscribers were told the rejected block was head (ChainEvent, logs and
	// ChainHeadEvent all fired before verification). Announce the parent so the
	// txpool re-resets against committed state instead of the rejected root.
	bc.chainHeadFeed.Send(ChainHeadEvent{Header: parent.Header()})

	log.Warn("Pipelined import: rolled back rejected block",
		"block", bad.NumberU64(), "hash", bad.Hash(), "head", parent.NumberU64())
}

// publishImportWitness persists the SRC-computed witness bytes to the
// witness store and notifies WIT peers via the witness-ready feed.
func (bc *BlockChain) publishImportWitness(p *pendingImportSRCState, witnessBytes []byte) {
	if len(witnessBytes) == 0 {
		return
	}
	bc.WriteWitness(p.block.Hash(), witnessBytes)
	witnessSizeBytesHistogram.Update(int64(len(witnessBytes)))
	bc.witnessReadyFeed.Send(WitnessReadyEvent{
		BlockHash:   p.block.Hash(),
		BlockNumber: p.block.NumberU64(),
	})
}

// emitPipelinedImportParityMetrics emits the read-side, execution,
// bor-consensus, and throughput timers under the same metric names the
// non-pipelined path uses, so dashboards work identically regardless of
// whether the chain is in pipelined mode. Hash/update/commit/stateCommit
// timers fire from the SRC goroutine's tmpDB in runSRCCompute.
func emitPipelinedImportParityMetrics(statedb *state.StateDB, start, pstart time.Time, vtime time.Duration, block *types.Block) {
	ptimePipelined := time.Since(pstart) - vtime - statedb.BorConsensusTime
	trieReadPipelined := statedb.SnapshotAccountReads + statedb.AccountReads + statedb.SnapshotStorageReads + statedb.StorageReads
	accountReadTimer.Update(statedb.AccountReads)
	storageReadTimer.Update(statedb.StorageReads)
	snapshotAccountReadTimer.Update(statedb.SnapshotAccountReads)
	snapshotStorageReadTimer.Update(statedb.SnapshotStorageReads)
	blockExecutionTimer.Update(ptimePipelined - trieReadPipelined)
	borConsensusTime.Update(statedb.BorConsensusTime)
	elapsedPipelined := time.Since(start)
	blockInsertTimer.Update(elapsedPipelined)
	pipelineImportTotalTimer.Update(elapsedPipelined)
	gasUsedPerBlockHistogram.Update(int64(block.GasUsed()))
	txsPerBlockHistogram.Update(int64(len(block.Transactions())))
	if elapsedPipelined > 0 {
		chainMgaspsMeter.Update(time.Duration(float64(block.GasUsed()) * 1000 / float64(elapsedPipelined)))
	}
}

// GetLastFlatDiff returns the FlatDiff captured from the most recently committed
// block. The miner uses this to open a NewWithFlatBase StateDB without waiting
// for the current SRC goroutine to finish.
func (bc *BlockChain) GetLastFlatDiff() *state.FlatDiff {
	bc.lastFlatDiffMu.RLock()
	defer bc.lastFlatDiffMu.RUnlock()
	return bc.lastFlatDiff
}

// SetLastFlatDiff stores the FlatDiff and the block number it belongs to.
// The block number is used by PostExecState to match the FlatDiff
// to the correct block (hash matching is unreliable because Root and seal
// signature are not available when FlatDiff is captured).
func (bc *BlockChain) SetLastFlatDiff(diff *state.FlatDiff, blockNum uint64, parentRoot common.Hash, blockRoot common.Hash) {
	bc.lastFlatDiffMu.Lock()
	bc.lastFlatDiff = diff
	bc.lastFlatDiffBlockNum = blockNum
	bc.lastFlatDiffParentRoot = parentRoot
	bc.lastFlatDiffBlockRoot = blockRoot
	bc.lastFlatDiffMu.Unlock()
}

// pipelinedStateCommitWaitCap bounds WaitForPipelinedStateCommit. SRC settles
// in single-digit milliseconds; the cap only guards a pathological stall, in
// which case the caller proceeds and fails on the trie open as it would have
// without the wait.
const pipelinedStateCommitWaitCap = 5 * time.Second

// WaitForPipelinedStateCommit blocks until the in-flight pipelined import SRC
// for the block with the given state root (if any) has committed. It is a
// no-op for any other root. RPC handlers that open tries directly by root
// (eth_getProof, debug_storageRangeAt) call this so a query at the chain head
// waits out the brief window between head advancement and state root commit
// instead of failing transiently. The FlatDiff overlay cannot serve those
// handlers: a proof is made of trie nodes, which do not exist until SRC
// builds them.
func (bc *BlockChain) WaitForPipelinedStateCommit(ctx context.Context, root common.Hash) error {
	bc.pendingImportSRCMu.Lock()
	p := bc.pendingImportSRC
	bc.pendingImportSRCMu.Unlock()
	if p == nil || p.block.Root() != root {
		return nil
	}
	timer := time.NewTimer(pipelinedStateCommitWaitCap)
	defer timer.Stop()
	select {
	case <-p.collectedCh:
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StateAtWithFlatDiff opens a StateDB at baseRoot with flatDiff as an in-memory
// overlay, allowing reads to see the post-state of the block that produced
// flatDiff without waiting for its state root to be committed to the trie DB.
func (bc *BlockChain) StateAtWithFlatDiff(baseRoot common.Hash, flatDiff *state.FlatDiff) (*state.StateDB, error) {
	return state.NewWithFlatBase(baseRoot, bc.statedb, flatDiff)
}

// ProcessBlockWithWitnesses processes a block in stateless mode using the provided witnesses.
func (bc *BlockChain) ProcessBlockWithWitnesses(block *types.Block, witness *stateless.Witness) (*state.StateDB, *ProcessResult, error) {
	if witness == nil {
		return nil, nil, errors.New("nil witness")
	}

	// Validate witness.
	// During parallel import, defer pre-state validation to the end of the batch.
	if !bc.parallelStatelessImportEnabled.Load() {
		var headerReader stateless.HeaderReader
		if witness.HeaderReader() != nil {
			headerReader = witness.HeaderReader()
		} else {
			headerReader = bc
		}
		if err := stateless.ValidateWitnessPreState(witness, headerReader, block.Header()); err != nil {
			log.Error("Witness validation failed during stateless processing", "blockNumber", block.Number(), "blockHash", block.Hash(), "err", err)
			return nil, nil, fmt.Errorf("witness validation failed: %w", err)
		}
	}

	// Remove critical computed fields from the block to force true recalculation
	context := block.Header()
	context.Root = common.Hash{}
	context.ReceiptHash = common.Hash{}

	task := types.NewBlockWithHeader(context).WithBody(*block.Body())

	// Bor: Calculate EvmBlockContext with Root and ReceiptHash to properly get the author
	author := NewEVMBlockContext(block.Header(), bc.hc, nil).Coinbase

	crossStateRoot, crossReceiptRoot, statedb, res, err := ExecuteStateless(bc.chainConfig, bc.cfg.VmConfig, task, witness, &author, bc.engine, bc.statedb.TrieDB().Disk())
	// Currently, we don't return the error, because we don't have a way to handle Span update statelessly
	// TODO: Return the error once we have a way to handle Span update
	if err != nil {
		log.Error("Stateless self-validation failed", "block", block.Number(), "hash", block.Hash(), "error", err)
		return nil, nil, err
	}
	if crossStateRoot != block.Root() {
		log.Error("Stateless self-validation root mismatch", "block", block.Number(), "hash", block.Hash(), "cross", crossStateRoot, "local", block.Root())
		err = fmt.Errorf("%w: remote %x != local %x", ErrStatelessStateRootMismatch, block.Root(), crossStateRoot)
		return nil, nil, err
	}
	if crossReceiptRoot != block.ReceiptHash() {
		log.Error("Stateless self-validation receipt root mismatch", "block", block.Number(), "hash", block.Hash(), "cross", crossReceiptRoot, "local", block.ReceiptHash())
		err = fmt.Errorf("stateless self-validation receipt root mismatch: remote %x != local %x", block.ReceiptHash(), crossReceiptRoot)
		return nil, nil, err
	}
	return statedb, res, nil
}

// startHeaderVerificationLoop starts a background goroutine that periodically
// verifies headers after the latest finalized block and rewinds the chain if
// invalid headers are detected.
func (bc *BlockChain) startHeaderVerificationLoop() {
	if bc.milestoneFetcher == nil {
		log.Warn("milestone fetcher is not set, skipping header verification loop")
		return
	}

	bc.wg.Add(1)
	go func() {
		defer bc.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		log.Info("Starting header verification loop")

		for {
			select {
			case <-bc.quit:
				log.Info("Stopping header verification loop")
				return
			case <-ticker.C:
				bc.verifyPendingHeaders()
			}
		}
	}()
}

// verifyPendingHeaders fetches the latest milestone from Heimdall and verifies
// all headers between that milestone's end block and the current chain head. If an invalid
// header is found, the chain is rewound to the last valid block.
func (bc *BlockChain) verifyPendingHeaders() {
	currentHead := bc.CurrentBlock()

	chainConfig := bc.Config()
	if chainConfig.Bor == nil || !chainConfig.Bor.IsRio(currentHead.Number) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	milestoneEndBlock, err := bc.milestoneFetcher(ctx)
	if err != nil {
		log.Error("Failed to fetch milestone end block from Heimdall for header verification", "err", err)
		return
	}

	headNumber := currentHead.Number.Uint64()
	if milestoneEndBlock >= headNumber {
		return // Still syncing or synced to the milestone end block, nothing to verify.
	}

	startBlock := milestoneEndBlock + 1

	// Collect headers from startBlock to current head.
	headers := make([]*types.Header, 0, headNumber-startBlock+1)
	for i := startBlock; i <= headNumber; i++ {
		header := bc.GetHeaderByNumber(i)
		if header == nil {
			log.Debug("Missing header during verification", "number", i)
			return
		}
		headers = append(headers, header)
	}

	if len(headers) == 0 {
		log.Debug("No headers to verify")
		return
	}

	log.Debug("Verifying pending headers",
		"from", headers[0].Number.Uint64(), "to", headers[len(headers)-1].Number.Uint64(), "count", len(headers))

	abort, results := bc.engine.VerifyHeaders(bc, headers)
	defer close(abort)

	// Check results and find the last valid header.
	lastValidNumber := milestoneEndBlock
	for _, header := range headers {
		select {
		case <-bc.quit:
			return
		case err := <-results:
			if err != nil {
				log.Warn("Invalid header detected during background verification",
					"number", header.Number.Uint64(), "hash", header.Hash(), "err", err)

				if lastValidNumber < headNumber {
					dropCount := int64(headNumber - lastValidNumber)

					log.Warn("Rewinding chain due to an invalid header",
						"from", headNumber, "to", lastValidNumber, "drop", dropCount)

					if err := bc.SetHead(lastValidNumber); err != nil {
						log.Error("Failed to rewind chain to the last valid header", "err", err)
					} else {
						blockReorgMeter.Mark(1)
						blockReorgDropMeter.Mark(dropCount)
					}
				}
				return
			}
			lastValidNumber = header.Number.Uint64()
		}
	}
}

// StateSizer returns the state size tracker, or nil if it's not initialized
func (bc *BlockChain) StateSizer() *state.SizeTracker {
	return bc.stateSizer
}
