// Copyright 2023 The go-ethereum Authors
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

// Package pebble implements the key-value database layer based on pebble.
package pebble

import (
	"bytes"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

// numLevels is the number of levels in PebbleDB's LSM tree.
// This matches the internal constant manifest.NumLevels = 7 from
// github.com/cockroachdb/pebble/internal/manifest/version.go
const numLevels = 7

const (
	// minCache is the minimum amount of memory in megabytes to allocate to pebble
	// read and write caching, split half and half.
	minCache = 16

	// minHandles is the minimum number of files handles to allocate to the open
	// database files.
	minHandles = 16

	// metricsGatheringInterval specifies the interval to retrieve pebble database
	// compaction, io and pause stats to report to the user.
	metricsGatheringInterval = 3 * time.Second

	// degradationWarnInterval specifies how often warning should be printed if the
	// leveldb database cannot keep up with requested writes.
	degradationWarnInterval = time.Minute
)

// Database is a persistent key-value store based on the pebble storage engine.
// Apart from basic data storage functionality it also supports batch writes and
// iterating over the keyspace in binary-alphabetical order.
type Database struct {
	fn string     // filename for reporting
	db *pebble.DB // Underlying pebble storage engine

	compTimeMeter       *metrics.Meter // Meter for measuring the total time spent in database compaction
	compReadMeter       *metrics.Meter // Meter for measuring the data read during compaction
	compWriteMeter      *metrics.Meter // Meter for measuring the data written during compaction
	writeDelayNMeter    *metrics.Meter // Meter for measuring the write delay number due to database compaction
	writeDelayMeter     *metrics.Meter // Meter for measuring the write delay duration due to database compaction
	diskSizeGauge       *metrics.Gauge // Gauge for tracking the size of all the levels in the database
	diskReadMeter       *metrics.Meter // Meter for measuring the effective amount of data read
	diskWriteMeter      *metrics.Meter // Meter for measuring the effective amount of data written
	memCompGauge        *metrics.Gauge // Gauge for tracking the number of memory compaction
	level0CompGauge     *metrics.Gauge // Gauge for tracking the number of table compaction in level0
	nonlevel0CompGauge  *metrics.Gauge // Gauge for tracking the number of table compaction in non0 level
	seekCompGauge       *metrics.Gauge // Gauge for tracking the number of table compaction caused by read opt
	manualMemAllocGauge *metrics.Gauge // Gauge for tracking amount of non-managed memory currently allocated

	levelsGauge []*metrics.Gauge // Gauge for tracking the number of tables in levels

	// PebbleDB native metrics with pebblenative namespace
	pebbleNativeBlockCacheSizeGauge   *metrics.Gauge // Gauge for tracking block cache size in bytes
	pebbleNativeBlockCacheCountGauge  *metrics.Gauge // Gauge for tracking number of cached blocks
	pebbleNativeBlockCacheHitsMeter   *metrics.Meter // Meter for tracking block cache hits
	pebbleNativeBlockCacheMissesMeter *metrics.Meter // Meter for tracking block cache misses

	pebbleNativeTableCacheSizeGauge   *metrics.Gauge // Gauge for tracking block cache size in bytes
	pebbleNativeTableCacheCountGauge  *metrics.Gauge // Gauge for tracking number of cached blocks
	pebbleNativeTableCacheHitsMeter   *metrics.Meter // Meter for tracking block cache hits
	pebbleNativeTableCacheMissesMeter *metrics.Meter // Meter for tracking block cache misses

	// PebbleDB native compact metrics with pebblenative namespace
	pebbleNativeCompactCountMeter             *metrics.Meter // Meter for tracking total number of compactions
	pebbleNativeCompactDefaultCountMeter      *metrics.Meter // Meter for tracking default compaction count
	pebbleNativeCompactDeleteOnlyCountMeter   *metrics.Meter // Meter for tracking delete-only compaction count
	pebbleNativeCompactElisionOnlyCountMeter  *metrics.Meter // Meter for tracking elision-only compaction count
	pebbleNativeCompactMoveCountMeter         *metrics.Meter // Meter for tracking move compaction count
	pebbleNativeCompactReadCountMeter         *metrics.Meter // Meter for tracking read compaction count
	pebbleNativeCompactRewriteCountMeter      *metrics.Meter // Meter for tracking rewrite compaction count
	pebbleNativeCompactMultiLevelCountMeter   *metrics.Meter // Meter for tracking multi-level compaction count
	pebbleNativeCompactCounterLevelCountMeter *metrics.Meter // Meter for tracking counter-level compaction count
	pebbleNativeCompactEstimatedDebtGauge     *metrics.Gauge // Gauge for tracking estimated compaction debt in bytes
	pebbleNativeCompactInProgressBytesGauge   *metrics.Gauge // Gauge for tracking bytes in in-progress compactions
	pebbleNativeCompactNumInProgressGauge     *metrics.Gauge // Gauge for tracking number of in-progress compactions
	pebbleNativeCompactMarkedFilesGauge       *metrics.Gauge // Gauge for tracking number of files marked for compaction
	pebbleNativeCompactDurationMeter          *metrics.Meter // Meter for tracking cumulative compaction duration

	// PebbleDB native ingest metrics with pebblenative namespace
	pebbleNativeIngestCountMeter *metrics.Meter // Meter for tracking total number of ingestions

	// PebbleDB native flush metrics with pebblenative namespace
	pebbleNativeFlushCountMeter                   *metrics.Meter // Meter for tracking total number of flushes
	pebbleNativeFlushNumInProgressGauge           *metrics.Gauge // Gauge for tracking number of in-progress flushes
	pebbleNativeFlushAsIngestCountMeter           *metrics.Meter // Meter for tracking flush operations handling ingested tables
	pebbleNativeFlushAsIngestTableCountMeter      *metrics.Meter // Meter for tracking tables ingested as flushables
	pebbleNativeFlushAsIngestBytesMeter           *metrics.Meter // Meter for tracking bytes flushed for flushables from ingestion
	pebbleNativeFlushWriteThroughputBytesMeter    *metrics.Meter // Meter for tracking flush write throughput bytes
	pebbleNativeFlushWriteThroughputDurationMeter *metrics.Meter // Meter for tracking flush write throughput work duration
	pebbleNativeFlushWriteThroughputIdleMeter     *metrics.Meter // Meter for tracking flush write throughput idle duration

	// PebbleDB native filter metrics with pebblenative namespace
	pebbleNativeFilterHitsMeter   *metrics.Meter // Meter for tracking filter hits
	pebbleNativeFilterMissesMeter *metrics.Meter // Meter for tracking filter misses

	// PebbleDB native memtable metrics with pebblenative namespace
	pebbleNativeMemTableSizeGauge        *metrics.Gauge // Gauge for tracking memtable size in bytes
	pebbleNativeMemTableCountGauge       *metrics.Gauge // Gauge for tracking number of memtables
	pebbleNativeMemTableZombieSizeGauge  *metrics.Gauge // Gauge for tracking zombie memtable size in bytes
	pebbleNativeMemTableZombieCountGauge *metrics.Gauge // Gauge for tracking number of zombie memtables

	// PebbleDB native keys metrics with pebblenative namespace
	pebbleNativeKeysRangeKeySetsCountGauge       *metrics.Gauge // Gauge for tracking range key sets count
	pebbleNativeKeysTombstoneCountGauge          *metrics.Gauge // Gauge for tracking tombstone count
	pebbleNativeKeysMissizedTombstonesCountMeter *metrics.Meter // Meter for tracking cumulative missized tombstones count

	// PebbleDB native snapshots metrics with pebblenative namespace
	pebbleNativeSnapshotsCountGauge          *metrics.Gauge // Gauge for tracking number of open snapshots
	pebbleNativeSnapshotsEarliestSeqNumGauge *metrics.Gauge // Gauge for tracking earliest snapshot sequence number
	pebbleNativeSnapshotsPinnedKeysMeter     *metrics.Meter // Meter for tracking cumulative pinned keys count
	pebbleNativeSnapshotsPinnedSizeMeter     *metrics.Meter // Meter for tracking cumulative pinned size

	// PebbleDB native table and uptime metrics with pebblenative namespace
	pebbleNativeTableItersGauge *metrics.Gauge // Gauge for tracking number of open sstable iterators
	pebbleNativeUptimeGauge     *metrics.Gauge // Gauge for tracking database uptime

	// PebbleDB native WAL metrics with pebblenative namespace
	pebbleNativeWALFilesGauge                *metrics.Gauge // Gauge for tracking number of live WAL files
	pebbleNativeWALObsoleteFilesGauge        *metrics.Gauge // Gauge for tracking number of obsolete WAL files
	pebbleNativeWALObsoletePhysicalSizeGauge *metrics.Gauge // Gauge for tracking physical size of obsolete WAL files
	pebbleNativeWALSizeGauge                 *metrics.Gauge // Gauge for tracking size of live data in WAL files
	pebbleNativeWALPhysicalSizeGauge         *metrics.Gauge // Gauge for tracking physical size of WAL files on-disk
	pebbleNativeWALBytesInMeter              *metrics.Meter // Meter for tracking logical bytes written to WAL
	pebbleNativeWALBytesWrittenMeter         *metrics.Meter // Meter for tracking bytes written to WAL

	// PebbleDB native level metrics with pebblenative namespace (numLevels levels: L0-L6)
	pebbleNativeLevelSublevelsGauge               [numLevels]*metrics.Gauge // Gauge for tracking sublevels for each level
	pebbleNativeLevelNumFilesGauge                [numLevels]*metrics.Gauge // Gauge for tracking number of files for each level
	pebbleNativeLevelNumVirtualFilesGauge         [numLevels]*metrics.Gauge // Gauge for tracking number of virtual files for each level
	pebbleNativeLevelSizeGauge                    [numLevels]*metrics.Gauge // Gauge for tracking size in bytes for each level
	pebbleNativeLevelVirtualSizeGauge             [numLevels]*metrics.Gauge // Gauge for tracking virtual size for each level
	pebbleNativeLevelScoreGauge                   [numLevels]*metrics.Gauge // Gauge for tracking compaction score for each level
	pebbleNativeLevelBytesInMeter                 [numLevels]*metrics.Meter // Meter for tracking bytes in for each level
	pebbleNativeLevelBytesIngestedMeter           [numLevels]*metrics.Meter // Meter for tracking bytes ingested for each level
	pebbleNativeLevelBytesMovedMeter              [numLevels]*metrics.Meter // Meter for tracking bytes moved for each level
	pebbleNativeLevelBytesReadMeter               [numLevels]*metrics.Meter // Meter for tracking bytes read for each level
	pebbleNativeLevelBytesCompactedMeter          [numLevels]*metrics.Meter // Meter for tracking bytes compacted for each level
	pebbleNativeLevelBytesFlushedMeter            [numLevels]*metrics.Meter // Meter for tracking bytes flushed for each level
	pebbleNativeLevelTablesCompactedMeter         [numLevels]*metrics.Meter // Meter for tracking tables compacted for each level
	pebbleNativeLevelTablesFlushedMeter           [numLevels]*metrics.Meter // Meter for tracking tables flushed for each level
	pebbleNativeLevelTablesIngestedMeter          [numLevels]*metrics.Meter // Meter for tracking tables ingested for each level
	pebbleNativeLevelTablesMovedMeter             [numLevels]*metrics.Meter // Meter for tracking tables moved for each level
	pebbleNativeLevelMultiLevelBytesInTopMeter    [numLevels]*metrics.Meter // Meter for tracking multi-level bytes in top for each level
	pebbleNativeLevelMultiLevelBytesInMeter       [numLevels]*metrics.Meter // Meter for tracking multi-level bytes in for each level
	pebbleNativeLevelMultiLevelBytesReadMeter     [numLevels]*metrics.Meter // Meter for tracking multi-level bytes read for each level
	pebbleNativeLevelValueBlocksSizeGauge         [numLevels]*metrics.Gauge // Gauge for tracking value blocks size for each level
	pebbleNativeLevelBytesWrittenDataBlocksMeter  [numLevels]*metrics.Meter // Meter for tracking bytes written to data blocks for each level
	pebbleNativeLevelBytesWrittenValueBlocksMeter [numLevels]*metrics.Meter // Meter for tracking bytes written to value blocks for each level

	// Operation timing histograms with pebblenative namespace
	pebbleNativeGetTimeHistogram metrics.Histogram // Histogram for tracking Get operation time
	pebbleNativePutTimeHistogram metrics.Histogram // Histogram for tracking Put operation time

	quitLock sync.RWMutex    // Mutex protecting the quit channel and the closed flag
	quitChan chan chan error // Quit channel to stop the metrics collection before closing the database
	closed   bool            // keep track of whether we're Closed

	log log.Logger // Contextual logger tracking the database path

	activeComp    int           // Current number of active compactions
	compStartTime time.Time     // The start time of the earliest currently-active compaction
	compTime      atomic.Int64  // Total time spent in compaction in ns
	level0Comp    atomic.Uint32 // Total number of level-zero compactions
	nonLevel0Comp atomic.Uint32 // Total number of non level-zero compactions

	writeStalled        atomic.Bool  // Flag whether the write is stalled
	writeDelayStartTime time.Time    // The start time of the latest write stall
	writeDelayCount     atomic.Int64 // Total number of write stall counts
	writeDelayTime      atomic.Int64 // Total time spent in write stalls

	writeOptions *pebble.WriteOptions
}

func (d *Database) onCompactionBegin(info pebble.CompactionInfo) {
	if d.activeComp == 0 {
		d.compStartTime = time.Now()
	}

	l0 := info.Input[0]

	if l0.Level == 0 {
		d.level0Comp.Add(1)
	} else {
		d.nonLevel0Comp.Add(1)
	}

	d.activeComp++
}

func (d *Database) onCompactionEnd(info pebble.CompactionInfo) {
	if d.activeComp == 1 {
		d.compTime.Add(int64(time.Since(d.compStartTime)))
	} else if d.activeComp == 0 {
		panic("should not happen")
	}

	d.activeComp--
}

func (d *Database) onWriteStallBegin(b pebble.WriteStallBeginInfo) {
	d.writeDelayStartTime = time.Now()
	d.writeDelayCount.Add(1)
	d.writeStalled.Store(true)
}

func (d *Database) onWriteStallEnd() {
	d.writeDelayTime.Add(int64(time.Since(d.writeDelayStartTime)))
	d.writeStalled.Store(false)
}

// panicLogger is just a noop logger to disable Pebble's internal logger.
//
// TODO(karalabe): Remove when Pebble sets this as the default.
type panicLogger struct{}

func (l panicLogger) Infof(format string, args ...interface{}) {
}

func (l panicLogger) Errorf(format string, args ...interface{}) {
}

func (l panicLogger) Fatalf(format string, args ...interface{}) {
	panic(fmt.Errorf("fatal: "+format, args...))
}

// New returns a wrapped pebble DB object. The namespace is the prefix that the
// metrics reporting should use for surfacing internal stats.
func New(file string, cache int, handles int, namespace string, readonly bool, ephemeral bool) (*Database, error) {
	// Ensure we have some minimal caching and file guarantees
	if cache < minCache {
		cache = minCache
	}

	if handles < minHandles {
		handles = minHandles
	}

	logger := log.New("database", file)
	logger.Info("Allocated cache and file handles", "cache", common.StorageSize(cache*1024*1024), "handles", handles)

	// The max memtable size is limited by the uint32 offsets stored in
	// internal/arenaskl.node, DeferredBatchOp, and flushableBatchEntry.
	//
	// - MaxUint32 on 64-bit platforms;
	// - MaxInt on 32-bit platforms.
	//
	// It is used when slices are limited to Uint32 on 64-bit platforms (the
	// length limit for slices is naturally MaxInt on 32-bit platforms).
	//
	// Taken from https://github.com/cockroachdb/pebble/blob/master/internal/constants/constants.go
	maxMemTableSize := (1<<31)<<(^uint(0)>>63) - 1

	// Two memory tables is configured which is identical to leveldb,
	// including a frozen memory table and another live one.
	memTableLimit := 2
	memTableSize := cache * 1024 * 1024 / 2 / memTableLimit

	// The memory table size is currently capped at maxMemTableSize-1 due to a
	// known bug in the pebble where maxMemTableSize is not recognized as a
	// valid size.
	//
	// TODO use the maxMemTableSize as the maximum table size once the issue
	// in pebble is fixed.
	if memTableSize >= maxMemTableSize {
		memTableSize = maxMemTableSize - 1
	}

	db := &Database{
		fn:           file,
		log:          logger,
		quitChan:     make(chan chan error),
		writeOptions: &pebble.WriteOptions{Sync: !ephemeral},
	}
	opt := &pebble.Options{
		// Pebble has a single combined cache area and the write
		// buffers are taken from this too. Assign all available
		// memory allowance for cache.
		Cache:        pebble.NewCache(int64(cache * 1024 * 1024)),
		MaxOpenFiles: handles,

		// The size of memory table(as well as the write buffer).
		// Note, there may have more than two memory tables in the system.
		MemTableSize: uint64(memTableSize),

		// MemTableStopWritesThreshold places a hard limit on the size
		// of the existent MemTables(including the frozen one).
		// Note, this must be the number of tables not the size of all memtables
		// according to https://github.com/cockroachdb/pebble/blob/master/options.go#L738-L742
		// and to https://github.com/cockroachdb/pebble/blob/master/db.go#L1892-L1903.
		MemTableStopWritesThreshold: memTableLimit,

		// The default compaction concurrency(1 thread),
		// Here use all available CPUs for faster compaction.
		MaxConcurrentCompactions: runtime.NumCPU,

		// Per-level options. Options for at least one level must be specified. The
		// options for the last level are used for all subsequent levels.
		Levels: []pebble.LevelOptions{
			{TargetFileSize: 2 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 4 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 8 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 16 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 32 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 64 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
			{TargetFileSize: 128 * 1024 * 1024, FilterPolicy: bloom.FilterPolicy(10)},
		},
		ReadOnly: readonly,
		EventListener: &pebble.EventListener{
			CompactionBegin: db.onCompactionBegin,
			CompactionEnd:   db.onCompactionEnd,
			WriteStallBegin: db.onWriteStallBegin,
			WriteStallEnd:   db.onWriteStallEnd,
		},
		Logger: panicLogger{}, // TODO(karalabe): Delete when this is upstreamed in Pebble
	}
	// Disable seek compaction explicitly. Check https://github.com/ethereum/go-ethereum/pull/20130
	// for more details.
	opt.Experimental.ReadSamplingMultiplier = -1

	// Open the db and recover any potential corruptions
	innerDB, err := pebble.Open(file, opt)
	if err != nil {
		return nil, err
	}

	db.db = innerDB

	db.compTimeMeter = metrics.GetOrRegisterMeter(namespace+"compact/time", nil)
	db.compReadMeter = metrics.GetOrRegisterMeter(namespace+"compact/input", nil)
	db.compWriteMeter = metrics.GetOrRegisterMeter(namespace+"compact/output", nil)
	db.diskSizeGauge = metrics.GetOrRegisterGauge(namespace+"disk/size", nil)
	db.diskReadMeter = metrics.GetOrRegisterMeter(namespace+"disk/read", nil)
	db.diskWriteMeter = metrics.GetOrRegisterMeter(namespace+"disk/write", nil)
	db.writeDelayMeter = metrics.GetOrRegisterMeter(namespace+"compact/writedelay/duration", nil)
	db.writeDelayNMeter = metrics.GetOrRegisterMeter(namespace+"compact/writedelay/counter", nil)
	db.memCompGauge = metrics.GetOrRegisterGauge(namespace+"compact/memory", nil)
	db.level0CompGauge = metrics.GetOrRegisterGauge(namespace+"compact/level0", nil)
	db.nonlevel0CompGauge = metrics.GetOrRegisterGauge(namespace+"compact/nonlevel0", nil)
	db.seekCompGauge = metrics.GetOrRegisterGauge(namespace+"compact/seek", nil)
	db.manualMemAllocGauge = metrics.GetOrRegisterGauge(namespace+"memory/manualalloc", nil)

	// PebbleDB native metrics with pebblenative namespace
	db.pebbleNativeBlockCacheSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/blockcache/size", nil)
	db.pebbleNativeBlockCacheCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/blockcache/count", nil)
	db.pebbleNativeBlockCacheHitsMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/blockcache/hits", nil)
	db.pebbleNativeBlockCacheMissesMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/blockcache/misses", nil)

	db.pebbleNativeTableCacheSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/tablecache/size", nil)
	db.pebbleNativeTableCacheCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/tablecache/count", nil)
	db.pebbleNativeTableCacheHitsMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/tablecache/hits", nil)
	db.pebbleNativeTableCacheMissesMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/tablecache/misses", nil)

	// PebbleDB native compact metrics with pebblenative namespace
	db.pebbleNativeCompactCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/count", nil)
	db.pebbleNativeCompactDefaultCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/defaultcount", nil)
	db.pebbleNativeCompactDeleteOnlyCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/deleteonlycount", nil)
	db.pebbleNativeCompactElisionOnlyCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/elisiononlycount", nil)
	db.pebbleNativeCompactMoveCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/movecount", nil)
	db.pebbleNativeCompactReadCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/readcount", nil)
	db.pebbleNativeCompactRewriteCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/rewritecount", nil)
	db.pebbleNativeCompactMultiLevelCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/multilevelcount", nil)
	db.pebbleNativeCompactCounterLevelCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/counterlevelcount", nil)
	db.pebbleNativeCompactEstimatedDebtGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/compact/estimateddebt", nil)
	db.pebbleNativeCompactInProgressBytesGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/compact/inprogressbytes", nil)
	db.pebbleNativeCompactNumInProgressGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/compact/numinprogress", nil)
	db.pebbleNativeCompactMarkedFilesGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/compact/markedfiles", nil)
	db.pebbleNativeCompactDurationMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/compact/duration", nil)

	// PebbleDB native ingest metrics with pebblenative namespace
	db.pebbleNativeIngestCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/ingest/count", nil)

	// PebbleDB native flush metrics with pebblenative namespace
	db.pebbleNativeFlushCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/count", nil)
	db.pebbleNativeFlushNumInProgressGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/flush/numinprogress", nil)
	db.pebbleNativeFlushAsIngestCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/asingestcount", nil)
	db.pebbleNativeFlushAsIngestTableCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/asingesttablecount", nil)
	db.pebbleNativeFlushAsIngestBytesMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/asingestbytes", nil)
	db.pebbleNativeFlushWriteThroughputBytesMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/writethroughput/bytes", nil)
	db.pebbleNativeFlushWriteThroughputDurationMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/writethroughput/duration", nil)
	db.pebbleNativeFlushWriteThroughputIdleMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/flush/writethroughput/idle", nil)

	// PebbleDB native filter metrics with pebblenative namespace
	db.pebbleNativeFilterHitsMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/filter/hits", nil)
	db.pebbleNativeFilterMissesMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/filter/misses", nil)

	// PebbleDB native memtable metrics with pebblenative namespace
	db.pebbleNativeMemTableSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/memtable/size", nil)
	db.pebbleNativeMemTableCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/memtable/count", nil)
	db.pebbleNativeMemTableZombieSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/memtable/zombiesize", nil)
	db.pebbleNativeMemTableZombieCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/memtable/zombiecount", nil)

	// PebbleDB native keys metrics with pebblenative namespace
	db.pebbleNativeKeysRangeKeySetsCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/keys/rangekeysetscount", nil)
	db.pebbleNativeKeysTombstoneCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/keys/tombstonecount", nil)
	db.pebbleNativeKeysMissizedTombstonesCountMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/keys/missizedtombstonescount", nil)

	// PebbleDB native snapshots metrics with pebblenative namespace
	db.pebbleNativeSnapshotsCountGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/snapshots/count", nil)
	db.pebbleNativeSnapshotsEarliestSeqNumGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/snapshots/earliestseqnum", nil)
	db.pebbleNativeSnapshotsPinnedKeysMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/snapshots/pinnedkeys", nil)
	db.pebbleNativeSnapshotsPinnedSizeMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/snapshots/pinnedsize", nil)

	// PebbleDB native table and uptime metrics with pebblenative namespace
	db.pebbleNativeTableItersGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/tableiters", nil)
	db.pebbleNativeUptimeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/uptime", nil)

	// PebbleDB native WAL metrics with pebblenative namespace
	db.pebbleNativeWALFilesGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/wal/files", nil)
	db.pebbleNativeWALObsoleteFilesGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/wal/obsoletefiles", nil)
	db.pebbleNativeWALObsoletePhysicalSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/wal/obsoletephysicalsize", nil)
	db.pebbleNativeWALSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/wal/size", nil)
	db.pebbleNativeWALPhysicalSizeGauge = metrics.GetOrRegisterGauge(namespace+"pebblenative/wal/physicalsize", nil)
	db.pebbleNativeWALBytesInMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/wal/bytesin", nil)
	db.pebbleNativeWALBytesWrittenMeter = metrics.GetOrRegisterMeter(namespace+"pebblenative/wal/byteswritten", nil)

	// PebbleDB native level metrics with pebblenative namespace (register for all levels)
	for level := 0; level < numLevels; level++ {
		levelStr := fmt.Sprintf("%d", level)

		db.pebbleNativeLevelSublevelsGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/sublevels", nil)
		db.pebbleNativeLevelNumFilesGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/numfiles", nil)
		db.pebbleNativeLevelNumVirtualFilesGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/numvirtualfiles", nil)
		db.pebbleNativeLevelSizeGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/size", nil)
		db.pebbleNativeLevelVirtualSizeGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/virtualsize", nil)
		db.pebbleNativeLevelScoreGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/score", nil)
		db.pebbleNativeLevelBytesInMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/bytesin", nil)
		db.pebbleNativeLevelBytesIngestedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/bytesingested", nil)
		db.pebbleNativeLevelBytesMovedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/bytesmoved", nil)
		db.pebbleNativeLevelBytesReadMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/bytesread", nil)
		db.pebbleNativeLevelBytesCompactedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/bytescompacted", nil)
		db.pebbleNativeLevelBytesFlushedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/bytesflushed", nil)
		db.pebbleNativeLevelTablesCompactedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/tablescompacted", nil)
		db.pebbleNativeLevelTablesFlushedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/tablesflushed", nil)
		db.pebbleNativeLevelTablesIngestedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/tablesingested", nil)
		db.pebbleNativeLevelTablesMovedMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/tablesmoved", nil)
		db.pebbleNativeLevelMultiLevelBytesInTopMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/multilevel/bytesintop", nil)
		db.pebbleNativeLevelMultiLevelBytesInMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/multilevel/bytesin", nil)
		db.pebbleNativeLevelMultiLevelBytesReadMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/multilevel/bytesread", nil)
		db.pebbleNativeLevelValueBlocksSizeGauge[level] = metrics.GetOrRegisterGauge(namespace+"pebblenative/level/"+levelStr+"/valueblockssize", nil)
		db.pebbleNativeLevelBytesWrittenDataBlocksMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/byteswrittendatablocks", nil)
		db.pebbleNativeLevelBytesWrittenValueBlocksMeter[level] = metrics.GetOrRegisterMeter(namespace+"pebblenative/level/"+levelStr+"/byteswrittenvalueblocks", nil)
	}

	// Operation timing histograms with pebblenative namespace
	db.pebbleNativeGetTimeHistogram = metrics.GetOrRegisterHistogram(namespace+"pebblenative/operation/get/time", nil, metrics.NewExpDecaySample(1028, 0.015))
	db.pebbleNativePutTimeHistogram = metrics.GetOrRegisterHistogram(namespace+"pebblenative/operation/put/time", nil, metrics.NewExpDecaySample(1028, 0.015))
	//

	// Start up the metrics gathering and return
	go db.meter(metricsGatheringInterval, namespace)
	return db, nil
}

// Close stops the metrics collection, flushes any pending data to disk and closes
// all io accesses to the underlying key-value store.
func (d *Database) Close() error {
	d.quitLock.Lock()
	defer d.quitLock.Unlock()
	// Allow double closing, simplifies things
	if d.closed {
		return nil
	}
	d.closed = true
	if d.quitChan != nil {
		errc := make(chan error)
		d.quitChan <- errc

		if err := <-errc; err != nil {
			d.log.Error("Metrics collection failed", "err", err)
		}

		d.quitChan = nil
	}

	return d.db.Close()
}

// Has retrieves if a key is present in the key-value store.
func (d *Database) Has(key []byte) (bool, error) {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()
	if d.closed {
		return false, pebble.ErrClosed
	}
	_, closer, err := d.db.Get(key)
	if err == pebble.ErrNotFound {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err = closer.Close(); err != nil {
		return false, err
	}
	return true, nil
}

// Get retrieves the given key if it's present in the key-value store.
func (d *Database) Get(key []byte) ([]byte, error) {
	start := time.Now()
	defer func() {
		d.pebbleNativeGetTimeHistogram.Update(time.Since(start).Nanoseconds())
	}()

	d.quitLock.RLock()
	defer d.quitLock.RUnlock()
	if d.closed {
		return nil, pebble.ErrClosed
	}
	dat, closer, err := d.db.Get(key)
	if err != nil {
		return nil, err
	}

	ret := make([]byte, len(dat))
	copy(ret, dat)
	if err = closer.Close(); err != nil {
		return nil, err
	}
	return ret, nil
}

// Put inserts the given value into the key-value store.
func (d *Database) Put(key []byte, value []byte) error {
	start := time.Now()
	defer func() {
		d.pebbleNativePutTimeHistogram.Update(time.Since(start).Nanoseconds())
	}()

	d.quitLock.RLock()
	defer d.quitLock.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.Set(key, value, d.writeOptions)
}

// Delete removes the key from the key-value store.
func (d *Database) Delete(key []byte) error {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.Delete(key, d.writeOptions)
}

// DeleteRange deletes all of the keys (and values) in the range [start,end)
// (inclusive on start, exclusive on end).
func (d *Database) DeleteRange(start, end []byte) error {
	d.quitLock.RLock()
	defer d.quitLock.RUnlock()
	if d.closed {
		return pebble.ErrClosed
	}
	return d.db.DeleteRange(start, end, d.writeOptions)
}

// NewBatch creates a write-only key-value store that buffers changes to its host
// database until a final write is called.
func (d *Database) NewBatch() ethdb.Batch {
	return &batch{
		b:  d.db.NewBatch(),
		db: d,
	}
}

// NewBatchWithSize creates a write-only database batch with pre-allocated buffer.
func (d *Database) NewBatchWithSize(size int) ethdb.Batch {
	return &batch{
		b:  d.db.NewBatchWithSize(size),
		db: d,
	}
}

// upperBound returns the upper bound for the given prefix
func upperBound(prefix []byte) (limit []byte) {
	for i := len(prefix) - 1; i >= 0; i-- {
		c := prefix[i]
		if c == 0xff {
			continue
		}

		limit = make([]byte, i+1)
		copy(limit, prefix)
		limit[i] = c + 1

		break
	}

	return limit
}

// Stat returns the internal metrics of Pebble in a text format. It's a developer
// method to read everything there is to read, independent of Pebble version.
func (d *Database) Stat() (string, error) {
	return d.db.Metrics().String(), nil
}

// Compact flattens the underlying data store for the given key range. In essence,
// deleted and overwritten versions are discarded, and the data is rearranged to
// reduce the cost of operations needed to access them.
//
// A nil start is treated as a key before all keys in the data store; a nil limit
// is treated as a key after all keys in the data store. If both is nil then it
// will compact entire data store.
func (d *Database) Compact(start []byte, limit []byte) error {
	// There is no special flag to represent the end of key range
	// in pebble(nil in leveldb). Use an ugly hack to construct a
	// large key to represent it.
	// Note any prefixed database entry will be smaller than this
	// flag, as for trie nodes we need the 32 byte 0xff because
	// there might be a shared prefix starting with a number of
	// 0xff-s, so 32 ensures than only a hash collision could touch it.
	// https://github.com/cockroachdb/pebble/issues/2359#issuecomment-1443995833
	if limit == nil {
		limit = bytes.Repeat([]byte{0xff}, 32)
	}

	return d.db.Compact(start, limit, true) // Parallelization is preferred
}

// Path returns the path to the database directory.
func (d *Database) Path() string {
	return d.fn
}

// meter periodically retrieves internal pebble counters and reports them to
// the metrics subsystem.
func (d *Database) meter(refresh time.Duration, namespace string) {
	var errc chan error

	timer := time.NewTimer(refresh)

	defer timer.Stop()

	// Create storage and warning log tracer for write delay.
	var (
		compTimes  [2]int64
		compWrites [2]int64
		compReads  [2]int64

		nWrites [2]int64

		writeDelayTimes      [2]int64
		writeDelayCounts     [2]int64
		lastWriteStallReport time.Time
	)

	// Iterate ad infinitum and collect the stats
	for i := 1; errc == nil; i++ {
		var (
			compWrite int64
			compRead  int64
			nWrite    int64

			stats              = d.db.Metrics()
			compTime           = d.compTime.Load()
			writeDelayCount    = d.writeDelayCount.Load()
			writeDelayTime     = d.writeDelayTime.Load()
			nonLevel0CompCount = int64(d.nonLevel0Comp.Load())
			level0CompCount    = int64(d.level0Comp.Load())
		)

		writeDelayTimes[i%2] = writeDelayTime
		writeDelayCounts[i%2] = writeDelayCount
		compTimes[i%2] = compTime

		for _, levelMetrics := range stats.Levels {
			nWrite += int64(levelMetrics.BytesCompacted)
			nWrite += int64(levelMetrics.BytesFlushed)
			compWrite += int64(levelMetrics.BytesCompacted)
			compRead += int64(levelMetrics.BytesRead)
		}

		nWrite += int64(stats.WAL.BytesWritten)

		compWrites[i%2] = compWrite
		compReads[i%2] = compRead
		nWrites[i%2] = nWrite

		d.writeDelayNMeter.Mark(writeDelayCounts[i%2] - writeDelayCounts[(i-1)%2])
		d.writeDelayMeter.Mark(writeDelayTimes[i%2] - writeDelayTimes[(i-1)%2])
		// Print a warning log if writing has been stalled for a while. The log will
		// be printed per minute to avoid overwhelming users.
		if d.writeStalled.Load() && writeDelayCounts[i%2] == writeDelayCounts[(i-1)%2] &&
			time.Now().After(lastWriteStallReport.Add(degradationWarnInterval)) {
			d.log.Warn("Database compacting, degraded performance")
			lastWriteStallReport = time.Now()
		}
		d.compTimeMeter.Mark(compTimes[i%2] - compTimes[(i-1)%2])
		d.compReadMeter.Mark(compReads[i%2] - compReads[(i-1)%2])
		d.compWriteMeter.Mark(compWrites[i%2] - compWrites[(i-1)%2])
		d.diskSizeGauge.Update(int64(stats.DiskSpaceUsage()))
		d.diskReadMeter.Mark(0) // pebble doesn't track non-compaction reads
		d.diskWriteMeter.Mark(nWrites[i%2] - nWrites[(i-1)%2])

		// See https://github.com/cockroachdb/pebble/pull/1628#pullrequestreview-1026664054
		manuallyAllocated := stats.BlockCache.Size + int64(stats.MemTable.Size) + int64(stats.MemTable.ZombieSize)
		d.manualMemAllocGauge.Update(manuallyAllocated)
		d.memCompGauge.Update(stats.Flush.Count)
		d.nonlevel0CompGauge.Update(nonLevel0CompCount)
		d.level0CompGauge.Update(level0CompCount)
		d.seekCompGauge.Update(stats.Compact.ReadCount)

		for i, level := range stats.Levels {
			// Append metrics for additional layers
			if i >= len(d.levelsGauge) {
				d.levelsGauge = append(d.levelsGauge, metrics.GetOrRegisterGauge(namespace+fmt.Sprintf("tables/level%v", i), nil))
			}
			d.levelsGauge[i].Update(level.NumFiles)
		}

		// Update PebbleDB native block cache metrics
		d.pebbleNativeBlockCacheSizeGauge.Update(stats.BlockCache.Size)
		d.pebbleNativeBlockCacheCountGauge.Update(stats.BlockCache.Count)
		d.pebbleNativeBlockCacheHitsMeter.Mark(stats.BlockCache.Hits)
		d.pebbleNativeBlockCacheMissesMeter.Mark(stats.BlockCache.Misses)

		d.pebbleNativeTableCacheSizeGauge.Update(stats.TableCache.Size)
		d.pebbleNativeTableCacheCountGauge.Update(stats.TableCache.Count)
		d.pebbleNativeTableCacheHitsMeter.Mark(stats.TableCache.Hits)
		d.pebbleNativeTableCacheMissesMeter.Mark(stats.TableCache.Misses)

		// Update PebbleDB native compact metrics
		d.pebbleNativeCompactCountMeter.Mark(stats.Compact.Count)
		d.pebbleNativeCompactDefaultCountMeter.Mark(stats.Compact.DefaultCount)
		d.pebbleNativeCompactDeleteOnlyCountMeter.Mark(stats.Compact.DeleteOnlyCount)
		d.pebbleNativeCompactElisionOnlyCountMeter.Mark(stats.Compact.ElisionOnlyCount)
		d.pebbleNativeCompactMoveCountMeter.Mark(stats.Compact.MoveCount)
		d.pebbleNativeCompactReadCountMeter.Mark(stats.Compact.ReadCount)
		d.pebbleNativeCompactRewriteCountMeter.Mark(stats.Compact.RewriteCount)
		d.pebbleNativeCompactMultiLevelCountMeter.Mark(stats.Compact.MultiLevelCount)
		d.pebbleNativeCompactCounterLevelCountMeter.Mark(stats.Compact.CounterLevelCount)
		d.pebbleNativeCompactEstimatedDebtGauge.Update(int64(stats.Compact.EstimatedDebt))
		d.pebbleNativeCompactInProgressBytesGauge.Update(stats.Compact.InProgressBytes)
		d.pebbleNativeCompactNumInProgressGauge.Update(stats.Compact.NumInProgress)
		d.pebbleNativeCompactMarkedFilesGauge.Update(int64(stats.Compact.MarkedFiles))
		d.pebbleNativeCompactDurationMeter.Mark(int64(stats.Compact.Duration))

		// Update PebbleDB native ingest metrics
		d.pebbleNativeIngestCountMeter.Mark(int64(stats.Ingest.Count))

		// Update PebbleDB native flush metrics
		d.pebbleNativeFlushCountMeter.Mark(stats.Flush.Count)
		d.pebbleNativeFlushNumInProgressGauge.Update(stats.Flush.NumInProgress)
		d.pebbleNativeFlushAsIngestCountMeter.Mark(int64(stats.Flush.AsIngestCount))
		d.pebbleNativeFlushAsIngestTableCountMeter.Mark(int64(stats.Flush.AsIngestTableCount))
		d.pebbleNativeFlushAsIngestBytesMeter.Mark(int64(stats.Flush.AsIngestBytes))
		d.pebbleNativeFlushWriteThroughputBytesMeter.Mark(int64(stats.Flush.WriteThroughput.Bytes))
		d.pebbleNativeFlushWriteThroughputDurationMeter.Mark(int64(stats.Flush.WriteThroughput.WorkDuration))
		d.pebbleNativeFlushWriteThroughputIdleMeter.Mark(int64(stats.Flush.WriteThroughput.IdleDuration))

		// Update PebbleDB native filter metrics
		d.pebbleNativeFilterHitsMeter.Mark(stats.Filter.Hits)
		d.pebbleNativeFilterMissesMeter.Mark(stats.Filter.Misses)

		// Update PebbleDB native memtable metrics
		d.pebbleNativeMemTableSizeGauge.Update(int64(stats.MemTable.Size))
		d.pebbleNativeMemTableCountGauge.Update(stats.MemTable.Count)
		d.pebbleNativeMemTableZombieSizeGauge.Update(int64(stats.MemTable.ZombieSize))
		d.pebbleNativeMemTableZombieCountGauge.Update(stats.MemTable.ZombieCount)

		// Update PebbleDB native keys metrics
		d.pebbleNativeKeysRangeKeySetsCountGauge.Update(int64(stats.Keys.RangeKeySetsCount))
		d.pebbleNativeKeysTombstoneCountGauge.Update(int64(stats.Keys.TombstoneCount))
		d.pebbleNativeKeysMissizedTombstonesCountMeter.Mark(int64(stats.Keys.MissizedTombstonesCount))

		// Update PebbleDB native snapshots metrics
		d.pebbleNativeSnapshotsCountGauge.Update(int64(stats.Snapshots.Count))
		d.pebbleNativeSnapshotsEarliestSeqNumGauge.Update(int64(stats.Snapshots.EarliestSeqNum))
		d.pebbleNativeSnapshotsPinnedKeysMeter.Mark(int64(stats.Snapshots.PinnedKeys))
		d.pebbleNativeSnapshotsPinnedSizeMeter.Mark(int64(stats.Snapshots.PinnedSize))

		// Update PebbleDB native table and uptime metrics
		d.pebbleNativeTableItersGauge.Update(stats.TableIters)
		d.pebbleNativeUptimeGauge.Update(int64(stats.Uptime))

		// Update PebbleDB native WAL metrics
		d.pebbleNativeWALFilesGauge.Update(stats.WAL.Files)
		d.pebbleNativeWALObsoleteFilesGauge.Update(stats.WAL.ObsoleteFiles)
		d.pebbleNativeWALObsoletePhysicalSizeGauge.Update(int64(stats.WAL.ObsoletePhysicalSize))
		d.pebbleNativeWALSizeGauge.Update(int64(stats.WAL.Size))
		d.pebbleNativeWALPhysicalSizeGauge.Update(int64(stats.WAL.PhysicalSize))
		d.pebbleNativeWALBytesInMeter.Mark(int64(stats.WAL.BytesIn))
		d.pebbleNativeWALBytesWrittenMeter.Mark(int64(stats.WAL.BytesWritten))

		// Update PebbleDB native level metrics for all levels
		// Assert that PebbleDB stats has the expected number of levels
		if len(stats.Levels) != numLevels {
			d.log.Error("PebbleDB level count mismatch", "expected", numLevels, "actual", len(stats.Levels))
			panic(fmt.Sprintf("PebbleDB level count mismatch: expected %d levels, got %d", numLevels, len(stats.Levels)))
		}

		for level := 0; level < numLevels; level++ {
			levelMetrics := &stats.Levels[level]

			d.pebbleNativeLevelSublevelsGauge[level].Update(int64(levelMetrics.Sublevels))
			d.pebbleNativeLevelNumFilesGauge[level].Update(levelMetrics.NumFiles)
			d.pebbleNativeLevelNumVirtualFilesGauge[level].Update(int64(levelMetrics.NumVirtualFiles))
			d.pebbleNativeLevelSizeGauge[level].Update(levelMetrics.Size)
			d.pebbleNativeLevelVirtualSizeGauge[level].Update(int64(levelMetrics.VirtualSize))
			d.pebbleNativeLevelScoreGauge[level].Update(int64(levelMetrics.Score * 1000)) // Scale float64 to int64 by 1000 for precision
			d.pebbleNativeLevelBytesInMeter[level].Mark(int64(levelMetrics.BytesIn))
			d.pebbleNativeLevelBytesIngestedMeter[level].Mark(int64(levelMetrics.BytesIngested))
			d.pebbleNativeLevelBytesMovedMeter[level].Mark(int64(levelMetrics.BytesMoved))
			d.pebbleNativeLevelBytesReadMeter[level].Mark(int64(levelMetrics.BytesRead))
			d.pebbleNativeLevelBytesCompactedMeter[level].Mark(int64(levelMetrics.BytesCompacted))
			d.pebbleNativeLevelBytesFlushedMeter[level].Mark(int64(levelMetrics.BytesFlushed))
			d.pebbleNativeLevelTablesCompactedMeter[level].Mark(int64(levelMetrics.TablesCompacted))
			d.pebbleNativeLevelTablesFlushedMeter[level].Mark(int64(levelMetrics.TablesFlushed))
			d.pebbleNativeLevelTablesIngestedMeter[level].Mark(int64(levelMetrics.TablesIngested))
			d.pebbleNativeLevelTablesMovedMeter[level].Mark(int64(levelMetrics.TablesMoved))
			d.pebbleNativeLevelMultiLevelBytesInTopMeter[level].Mark(int64(levelMetrics.MultiLevel.BytesInTop))
			d.pebbleNativeLevelMultiLevelBytesInMeter[level].Mark(int64(levelMetrics.MultiLevel.BytesIn))
			d.pebbleNativeLevelMultiLevelBytesReadMeter[level].Mark(int64(levelMetrics.MultiLevel.BytesRead))
			d.pebbleNativeLevelValueBlocksSizeGauge[level].Update(int64(levelMetrics.Additional.ValueBlocksSize))
			d.pebbleNativeLevelBytesWrittenDataBlocksMeter[level].Mark(int64(levelMetrics.Additional.BytesWrittenDataBlocks))
			d.pebbleNativeLevelBytesWrittenValueBlocksMeter[level].Mark(int64(levelMetrics.Additional.BytesWrittenValueBlocks))
		}

		// Sleep a bit, then repeat the stats collection
		select {
		case errc = <-d.quitChan:
			// Quit requesting, stop hammering the database
		case <-timer.C:
			// Timeout, gather a new set of stats
			timer.Reset(refresh)
		}
	}
	errc <- nil
}

// batch is a write-only batch that commits changes to its host database
// when Write is called. A batch cannot be used concurrently.
type batch struct {
	b    *pebble.Batch
	db   *Database
	size int
}

// Put inserts the given value into the batch for later committing.
func (b *batch) Put(key, value []byte) error {
	if err := b.b.Set(key, value, nil); err != nil {
		return err
	}
	b.size += len(key) + len(value)

	return nil
}

// Delete inserts the key removal into the batch for later committing.
func (b *batch) Delete(key []byte) error {
	if err := b.b.Delete(key, nil); err != nil {
		return err
	}
	b.size += len(key)

	return nil
}

// ValueSize retrieves the amount of data queued up for writing.
func (b *batch) ValueSize() int {
	return b.size
}

// Write flushes any accumulated data to disk.
func (b *batch) Write() error {
	b.db.quitLock.RLock()
	defer b.db.quitLock.RUnlock()
	if b.db.closed {
		return pebble.ErrClosed
	}
	return b.b.Commit(b.db.writeOptions)
}

// Reset resets the batch for reuse.
func (b *batch) Reset() {
	b.b.Reset()
	b.size = 0
}

// Replay replays the batch contents.
func (b *batch) Replay(w ethdb.KeyValueWriter) error {
	reader := b.b.Reader()

	for {
		kind, k, v, ok, err := reader.Next()
		if !ok || err != nil {
			return err
		}
		// The (k,v) slices might be overwritten if the batch is reset/reused,
		// and the receiver should copy them if they are to be retained long-term.
		if kind == pebble.InternalKeyKindSet {
			if err = w.Put(k, v); err != nil {
				return err
			}
		} else if kind == pebble.InternalKeyKindDelete {
			if err = w.Delete(k); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unhandled operation, keytype: %v", kind)
		}
	}
}

// pebbleIterator is a wrapper of underlying iterator in storage engine.
// The purpose of this structure is to implement the missing APIs.
//
// The pebble iterator is not thread-safe.
type pebbleIterator struct {
	iter     *pebble.Iterator
	moved    bool
	released bool
}

// NewIterator creates a binary-alphabetical iterator over a subset
// of database content with a particular key prefix, starting at a particular
// initial key (or after, if it does not exist).
func (d *Database) NewIterator(prefix []byte, start []byte) ethdb.Iterator {
	iter, _ := d.db.NewIter(&pebble.IterOptions{
		LowerBound: append(prefix, start...),
		UpperBound: upperBound(prefix),
	})
	iter.First()
	return &pebbleIterator{iter: iter, moved: true, released: false}
}

// Next moves the iterator to the next key/value pair. It returns whether the
// iterator is exhausted.
func (iter *pebbleIterator) Next() bool {
	if iter.moved {
		iter.moved = false
		return iter.iter.Valid()
	}

	return iter.iter.Next()
}

// Error returns any accumulated error. Exhausting all the key/value pairs
// is not considered to be an error.
func (iter *pebbleIterator) Error() error {
	return iter.iter.Error()
}

// Key returns the key of the current key/value pair, or nil if done. The caller
// should not modify the contents of the returned slice, and its contents may
// change on the next call to Next.
func (iter *pebbleIterator) Key() []byte {
	return iter.iter.Key()
}

// Value returns the value of the current key/value pair, or nil if done. The
// caller should not modify the contents of the returned slice, and its contents
// may change on the next call to Next.
func (iter *pebbleIterator) Value() []byte {
	return iter.iter.Value()
}

// Release releases associated resources. Release should always succeed and can
// be called multiple times without causing error.
func (iter *pebbleIterator) Release() {
	if !iter.released {
		iter.iter.Close()
		iter.released = true
	}
}
