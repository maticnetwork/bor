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
	"errors"
	"fmt"
	"runtime"
	"strings"
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
	fn        string     // filename for reporting
	db        *pebble.DB // Underlying pebble storage engine
	namespace string     // Namespace for metrics

	compTimeMeter          *metrics.Meter   // Meter for measuring the total time spent in database compaction
	compReadMeter          *metrics.Meter   // Meter for measuring the data read during compaction
	compWriteMeter         *metrics.Meter   // Meter for measuring the data written during compaction
	writeDelayNMeter       *metrics.Meter   // Meter for measuring the write delay number due to database compaction
	writeDelayMeter        *metrics.Meter   // Meter for measuring the write delay duration due to database compaction
	diskSizeGauge          *metrics.Gauge   // Gauge for tracking the size of all the levels in the database
	diskReadMeter          *metrics.Meter   // Meter for measuring the effective amount of data read
	diskWriteMeter         *metrics.Meter   // Meter for measuring the effective amount of data written
	memCompGauge           *metrics.Gauge   // Gauge for tracking the number of memory compaction
	level0CompGauge        *metrics.Gauge   // Gauge for tracking the number of table compaction in level0
	nonlevel0CompGauge     *metrics.Gauge   // Gauge for tracking the number of table compaction in non0 level
	seekCompGauge          *metrics.Gauge   // Gauge for tracking the number of table compaction caused by read opt
	manualMemAllocGauge    *metrics.Gauge   // Gauge for tracking amount of non-managed memory currently allocated
	liveMemTablesGauge     *metrics.Gauge   // Gauge for tracking the number of live memory tables
	zombieMemTablesGauge   *metrics.Gauge   // Gauge for tracking the number of zombie memory tables
	blockCacheHitGauge     *metrics.Gauge   // Gauge for tracking the number of total hit in the block cache
	blockCacheMissGauge    *metrics.Gauge   // Gauge for tracking the number of total miss in the block cache
	tableCacheHitGauge     *metrics.Gauge   // Gauge for tracking the number of total hit in the table cache
	tableCacheMissGauge    *metrics.Gauge   // Gauge for tracking the number of total miss in the table cache
	filterHitGauge         *metrics.Gauge   // Gauge for tracking the number of total hit in bloom filter
	filterMissGauge        *metrics.Gauge   // Gauge for tracking the number of total miss in bloom filter
	estimatedCompDebtGauge *metrics.Gauge   // Gauge for tracking the number of bytes that need to be compacted
	liveCompGauge          *metrics.Gauge   // Gauge for tracking the number of in-progress compactions
	liveCompSizeGauge      *metrics.Gauge   // Gauge for tracking the size of in-progress compactions
	liveIterGauge          *metrics.Gauge   // Gauge for tracking the number of live database iterators
	levelsGauge            []*metrics.Gauge // Gauge for tracking the number of tables in levels

	// Read and Write Amplification metrics
	readAmpGauge       *metrics.GaugeFloat64   // Gauge for tracking read amplification
	levelWriteAmpGauge []*metrics.GaugeFloat64 // Gauge for tracking write amplification per level
	totalWriteAmpGauge *metrics.GaugeFloat64   // Gauge for tracking total write amplification

	// Detailed I/O tracking metrics
	walBytesWrittenMeter   *metrics.Meter // Bytes written to WAL
	walFileCountGauge      *metrics.Gauge // Number of WAL files
	sstBytesReadMeter      *metrics.Meter // Bytes read from SST files (compaction input)
	sstBytesWrittenMeter   *metrics.Meter // Bytes written to SST files (compaction output)
	flushBytesWrittenMeter *metrics.Meter // Bytes written during memtable flush

	// Per-level size tracking
	levelSizeGauge  []*metrics.Gauge // Size of each level in bytes
	levelScoreGauge []*metrics.Gauge // Compaction score per level (>1 means needs compaction)

	// Detailed WAL metrics
	walSizeGauge         *metrics.Gauge // Current WAL size
	walPhysicalSizeGauge *metrics.Gauge // Physical WAL size on disk
	walObsoleteSizeGauge *metrics.Gauge // Obsolete WAL data size

	// Snapshot metrics
	snapshotCountGauge *metrics.Gauge // Number of snapshots

	// Keys metrics for understanding data distribution
	keysCountGauge []*metrics.Gauge // Number of keys per level

	// Calculated amplification metrics
	calcWriteAmpGauge   *metrics.GaugeFloat64 // Calculated write amplification: total physical writes / logical user data
	calcReadAmpGauge    *metrics.GaugeFloat64 // Calculated read amplification (same as readamp)
	calcSpaceAmpGauge   *metrics.GaugeFloat64 // Calculated space amplification: disk/size / actual data size
	walWriteAmpGauge    *metrics.GaugeFloat64 // WAL write amplification: WAL physical / logical data
	actualDataSizeGauge *metrics.Gauge        // Actual user data size (from live SST files)

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
	writeDelayReason    string       // The reason of the latest write stall
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

	// Take just the first word of the reason. These are two potential
	// reasons for the write stall:
	// - memtable count limit reached
	// - L0 file count limit exceeded
	reason := b.Reason
	if i := strings.IndexByte(reason, ' '); i != -1 {
		reason = reason[:i]
	}
	if reason == "L0" || reason == "memtable" {
		d.writeDelayReason = reason
		metrics.GetOrRegisterGauge(d.namespace+"stall/count/"+reason, nil).Inc(1)
	}
}

func (d *Database) onWriteStallEnd() {
	d.writeDelayTime.Add(int64(time.Since(d.writeDelayStartTime)))
	d.writeStalled.Store(false)

	if d.writeDelayReason != "" {
		metrics.GetOrRegisterResettingTimer(d.namespace+"stall/time/"+d.writeDelayReason, nil).UpdateSince(d.writeDelayStartTime)
		d.writeDelayReason = ""
	}
	d.writeDelayStartTime = time.Time{}
}

// Track SST file operations
func (d *Database) onTableCreated(info pebble.TableCreateInfo) {
	metrics.GetOrRegisterMeter(d.namespace+"file/sst/created", nil).Mark(1)
	d.log.Debug("SST file created", "reason", info.Reason, "fileNum", info.FileNum)
}

func (d *Database) onTableDeleted(info pebble.TableDeleteInfo) {
	metrics.GetOrRegisterMeter(d.namespace+"file/sst/deleted", nil).Mark(1)
}

// Track WAL (.log) file operations
func (d *Database) onWALCreated(info pebble.WALCreateInfo) {
	metrics.GetOrRegisterMeter(d.namespace+"file/wal/created", nil).Mark(1)
	d.log.Debug("WAL file created", "fileNum", info.FileNum, "recycled", info.RecycledFileNum)
}

func (d *Database) onWALDeleted(info pebble.WALDeleteInfo) {
	metrics.GetOrRegisterMeter(d.namespace+"file/wal/deleted", nil).Mark(1)
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
func New(file string, cache int, handles int, namespace string, readonly bool) (*Database, error) {
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

	// Four memory tables are configured, each with a default size of 256 MB.
	// Having multiple smaller memory tables while keeping the total memory
	// limit unchanged allows writes to be flushed more smoothly. This helps
	// avoid compaction spikes and mitigates write stalls caused by heavy
	// compaction workloads.
	memTableNumber := 4
	memTableSize := cache * 1024 * 1024 / 2 / memTableNumber

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
		fn:        file,
		log:       logger,
		quitChan:  make(chan chan error),
		namespace: namespace,

		// Use asynchronous write mode by default. Otherwise, the overhead of frequent fsync
		// operations can be significant, especially on platforms with slow fsync performance
		// (e.g., macOS) or less capable SSDs.
		//
		// Note that enabling async writes means recent data may be lost in the event of an
		// application-level panic (writes will also be lost on a machine-level failure,
		// of course). Geth is expected to handle recovery from an unclean shutdown.
		writeOptions: pebble.NoSync,
	}
	opt := &pebble.Options{
		// Pebble has a single combined cache area and the write
		// buffers are taken from this too. Assign all available
		// memory allowance for cache.
		Cache:        pebble.NewCache(int64(cache * 1024 * 1024)),
		MaxOpenFiles: handles,
		// BytesPerSync was 512 KB as implicit default. Increasing it will provide fewer but larger syncs during compaction
		BytesPerSync: 1 * 1024 * 1024,

		// The size of memory table(as well as the write buffer).
		// Note, there may have more than two memory tables in the system.
		MemTableSize: uint64(memTableSize),

		// MemTableStopWritesThreshold places a hard limit on the number
		// of the existent MemTables(including the frozen one).
		//
		// Note, this must be the number of tables not the size of all memtables
		// according to https://github.com/cockroachdb/pebble/blob/master/options.go#L738-L742
		// and to https://github.com/cockroachdb/pebble/blob/master/db.go#L1892-L1903.
		//
		// MemTableStopWritesThreshold is set to twice the maximum number of
		// allowed memtables to accommodate temporary spikes.
		MemTableStopWritesThreshold: memTableNumber * 2,

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

			// Pebble doesn't use the Bloom filter at level6 for read efficiency.
			{TargetFileSize: 128 * 1024 * 1024},
		},
		ReadOnly: readonly,
		EventListener: &pebble.EventListener{
			CompactionBegin: db.onCompactionBegin,
			CompactionEnd:   db.onCompactionEnd,
			WriteStallBegin: db.onWriteStallBegin,
			WriteStallEnd:   db.onWriteStallEnd,
			TableCreated:    db.onTableCreated,
			TableDeleted:    db.onTableDeleted,
			WALCreated:      db.onWALCreated,
			WALDeleted:      db.onWALDeleted,
		},
		Logger: panicLogger{}, // TODO(karalabe): Delete when this is upstreamed in Pebble

		// Pebble is configured to use asynchronous write mode, meaning write operations
		// return as soon as the data is cached in memory, without waiting for the WAL
		// to be written. This mode offers better write performance but risks losing
		// recent writes if the application crashes or a power failure/system crash occurs.
		//
		// By setting the WALBytesPerSync, the cached WAL writes will be periodically
		// flushed at the background if the accumulated size exceeds this threshold.
		WALBytesPerSync: 5 * ethdb.IdealBatchSize,

		// L0CompactionThreshold specifies the number of L0 read-amplification
		// necessary to trigger an L0 compaction. It essentially refers to the
		// number of sub-levels at the L0. For each sub-level, it contains several
		// L0 files which are non-overlapping with each other, typically produced
		// by a single memory-table flush.
		//
		// The default value in Pebble is 4, which is a bit too large to have
		// the compaction debt as around 10GB. By reducing it to 2, the compaction
		// debt will be less than 1GB, but with more frequent compactions scheduled.
		L0CompactionThreshold: 2,
	}
	// Disable seek compaction explicitly. Check https://github.com/ethereum/go-ethereum/pull/20130
	// for more details.
	opt.Experimental.ReadSamplingMultiplier = -1

	// These two settings define the conditions under which compaction concurrency
	// is increased. Specifically, one additional compaction job will be enabled when:
	// - there is one more overlapping sub-level0;
	// - there is an additional 256 MB of compaction debt;
	//
	// The maximum concurrency is still capped by MaxConcurrentCompactions, but with
	// these settings compactions can scale up more readily.
	opt.Experimental.L0CompactionConcurrency = 1
	opt.Experimental.CompactionDebtConcurrency = 1 << 28 // 256MB

	// Adaptive compaction scales the workers based on the load instead of always using all CPUs
	// L0CompactionConcurrency compaction worker per overlapping sublevel
	opt.Experimental.L0CompactionConcurrency = 1
	// CompactionDebtConcurrency worker per 256 MB of compaction debt
	opt.Experimental.CompactionDebtConcurrency = 1 << 28

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
	db.liveMemTablesGauge = metrics.GetOrRegisterGauge(namespace+"table/live", nil)
	db.zombieMemTablesGauge = metrics.GetOrRegisterGauge(namespace+"table/zombie", nil)
	db.blockCacheHitGauge = metrics.GetOrRegisterGauge(namespace+"cache/block/hit", nil)
	db.blockCacheMissGauge = metrics.GetOrRegisterGauge(namespace+"cache/block/miss", nil)
	db.tableCacheHitGauge = metrics.GetOrRegisterGauge(namespace+"cache/table/hit", nil)
	db.tableCacheMissGauge = metrics.GetOrRegisterGauge(namespace+"cache/table/miss", nil)
	db.filterHitGauge = metrics.GetOrRegisterGauge(namespace+"filter/hit", nil)
	db.filterMissGauge = metrics.GetOrRegisterGauge(namespace+"filter/miss", nil)
	db.estimatedCompDebtGauge = metrics.GetOrRegisterGauge(namespace+"compact/estimateDebt", nil)
	db.liveCompGauge = metrics.GetOrRegisterGauge(namespace+"compact/live/count", nil)
	db.liveCompSizeGauge = metrics.GetOrRegisterGauge(namespace+"compact/live/size", nil)
	db.liveIterGauge = metrics.GetOrRegisterGauge(namespace+"iter/count", nil)

	// Register read and write amplification metrics
	db.readAmpGauge = metrics.GetOrRegisterGaugeFloat64(namespace+"readamp", nil)
	db.totalWriteAmpGauge = metrics.GetOrRegisterGaugeFloat64(namespace+"writeamp/total", nil)

	// Register detailed I/O tracking metrics
	db.walBytesWrittenMeter = metrics.GetOrRegisterMeter(namespace+"wal/bytes", nil)
	db.walFileCountGauge = metrics.GetOrRegisterGauge(namespace+"wal/files", nil)
	db.sstBytesReadMeter = metrics.GetOrRegisterMeter(namespace+"sst/read", nil)
	db.sstBytesWrittenMeter = metrics.GetOrRegisterMeter(namespace+"sst/written", nil)
	db.flushBytesWrittenMeter = metrics.GetOrRegisterMeter(namespace+"flush/bytes", nil)

	// WAL size metrics
	db.walSizeGauge = metrics.GetOrRegisterGauge(namespace+"wal/size", nil)
	db.walPhysicalSizeGauge = metrics.GetOrRegisterGauge(namespace+"wal/physicalsize", nil)
	db.walObsoleteSizeGauge = metrics.GetOrRegisterGauge(namespace+"wal/obsoletesize", nil)

	// Snapshot metrics
	db.snapshotCountGauge = metrics.GetOrRegisterGauge(namespace+"snapshots/count", nil)

	// Calculated amplification metrics
	db.calcWriteAmpGauge = metrics.GetOrRegisterGaugeFloat64(namespace+"amplification/write/calculated", nil)
	db.calcReadAmpGauge = metrics.GetOrRegisterGaugeFloat64(namespace+"amplification/read/calculated", nil)
	db.calcSpaceAmpGauge = metrics.GetOrRegisterGaugeFloat64(namespace+"amplification/space/calculated", nil)
	db.walWriteAmpGauge = metrics.GetOrRegisterGaugeFloat64(namespace+"amplification/wal", nil)
	db.actualDataSizeGauge = metrics.GetOrRegisterGauge(namespace+"disk/actualsize", nil)

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
	// There is no special flag to represent the end of key range
	// in pebble(nil in leveldb). Use an ugly hack to construct a
	// large key to represent it.
	if end == nil {
		end = ethdb.MaximumKey
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
		limit = ethdb.MaximumKey
	}

	return d.db.Compact(start, limit, true) // Parallelization is preferred
}

// Path returns the path to the database directory.
func (d *Database) Path() string {
	return d.fn
}

// SyncKeyValue flushes all pending writes in the write-ahead-log to disk,
// ensuring data durability up to that point.
func (d *Database) SyncKeyValue() error {
	// The entry (value=nil) is not written to the database; it is only
	// added to the WAL. Writing this special log entry in sync mode
	// automatically flushes all previous writes, ensuring database
	// durability up to this point.
	b := d.db.NewBatch()
	b.LogData(nil, nil)
	return d.db.Apply(b, pebble.Sync)
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

		nWrites    [2]int64
		flushBytes [2]int64 // Add tracking for flush bytes
		walWrites  [2]int64 // Track WAL writes separately

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

		var totalFlushBytes int64
		for _, levelMetrics := range stats.Levels {
			// Don't add to nWrite yet - we'll calculate physical writes separately
			compWrite += int64(levelMetrics.BytesCompacted)
			compRead += int64(levelMetrics.BytesRead)
			totalFlushBytes += int64(levelMetrics.BytesFlushed)
		}

		// Track both logical and physical WAL metrics
		walLogicalWrites := int64(stats.WAL.BytesWritten)
		walPhysicalSize := int64(stats.WAL.PhysicalSize)

		// Calculate physical writes including WAL overhead
		// For nWrite, we need to account for physical WAL overhead
		// Use the ratio of physical/logical for current WAL as a multiplier
		walOverheadRatio := 1.0
		if stats.WAL.BytesWritten > 0 {
			walOverheadRatio = float64(walPhysicalSize) / float64(stats.WAL.BytesWritten)
		}

		// Estimate physical writes as: SST writes + (logical WAL * overhead ratio)
		// This gives us a better approximation of actual disk I/O
		nWrite = compWrite + totalFlushBytes + int64(float64(walLogicalWrites)*walOverheadRatio)

		compWrites[i%2] = compWrite
		compReads[i%2] = compRead
		nWrites[i%2] = nWrite
		walWrites[i%2] = walLogicalWrites
		flushBytes[i%2] = totalFlushBytes

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
		d.liveCompGauge.Update(stats.Compact.NumInProgress)
		d.liveCompSizeGauge.Update(stats.Compact.InProgressBytes)
		d.liveIterGauge.Update(stats.TableIters)

		d.liveMemTablesGauge.Update(stats.MemTable.Count)
		d.zombieMemTablesGauge.Update(stats.MemTable.ZombieCount)
		d.estimatedCompDebtGauge.Update(int64(stats.Compact.EstimatedDebt))
		d.tableCacheHitGauge.Update(stats.TableCache.Hits)
		d.tableCacheMissGauge.Update(stats.TableCache.Misses)
		d.blockCacheHitGauge.Update(stats.BlockCache.Hits)
		d.blockCacheMissGauge.Update(stats.BlockCache.Misses)
		d.filterHitGauge.Update(stats.Filter.Hits)
		d.filterMissGauge.Update(stats.Filter.Misses)

		// Update read amplification metric
		// ReadAmp returns the current read amplification of the database
		d.readAmpGauge.Update(float64(stats.ReadAmp()))

		// Track detailed I/O metrics
		var (
			totalSSTBytesRead    int64
			totalSSTBytesWritten int64
		)

		// Calculate and update write amplification metrics per level
		for i, level := range stats.Levels {
			// Append metrics for additional layers
			if i >= len(d.levelsGauge) {
				d.levelsGauge = append(d.levelsGauge, metrics.GetOrRegisterGauge(namespace+fmt.Sprintf("tables/level%v", i), nil))
				d.levelWriteAmpGauge = append(d.levelWriteAmpGauge, metrics.GetOrRegisterGaugeFloat64(namespace+fmt.Sprintf("writeamp/level%v", i), nil))
				d.levelSizeGauge = append(d.levelSizeGauge, metrics.GetOrRegisterGauge(namespace+fmt.Sprintf("size/level%v", i), nil))
				d.levelScoreGauge = append(d.levelScoreGauge, metrics.GetOrRegisterGauge(namespace+fmt.Sprintf("score/level%v", i), nil))
				d.keysCountGauge = append(d.keysCountGauge, metrics.GetOrRegisterGauge(namespace+fmt.Sprintf("keys/level%v", i), nil))
			}
			d.levelsGauge[i].Update(level.NumFiles)

			// Update write amplification for this level
			writeAmp := level.WriteAmp()
			d.levelWriteAmpGauge[i].Update(writeAmp)

			// Update level size
			d.levelSizeGauge[i].Update(level.Size)

			// Update compaction score (>1.0 means level needs compaction)
			d.levelScoreGauge[i].Update(int64(level.Score * 1000)) // Multiply by 1000 for precision

			// Update keys count per level
			d.keysCountGauge[i].Update(level.NumFiles) // Approximate by file count

			// Accumulate I/O stats (these are cumulative from Pebble)
			totalSSTBytesRead += int64(level.BytesRead)
			totalSSTBytesWritten += int64(level.BytesCompacted)
		}
		// Update I/O meters (mark only the delta since last measurement)
		if i > 1 {
			deltaRead := totalSSTBytesRead - compReads[(i-1)%2]
			deltaWrite := totalSSTBytesWritten - compWrites[(i-1)%2]
			deltaWAL := walWrites[i%2] - walWrites[(i-1)%2]
			deltaFlush := flushBytes[i%2] - flushBytes[(i-1)%2]

			// Only mark positive deltas to avoid negative values
			if deltaRead > 0 {
				d.sstBytesReadMeter.Mark(deltaRead)
			}
			if deltaWrite > 0 {
				d.sstBytesWrittenMeter.Mark(deltaWrite)
			}
			// Track WAL logical writes (the actual application data)
			if deltaWAL > 0 {
				d.walBytesWrittenMeter.Mark(deltaWAL)
			}
			if deltaFlush > 0 {
				d.flushBytesWrittenMeter.Mark(deltaFlush)
			}
		}

		// Calculate total write amplification using Pebble's built-in method
		totalMetrics := stats.Total()
		totalWriteAmp := totalMetrics.WriteAmp()
		d.totalWriteAmpGauge.Update(totalWriteAmp)

		// Update WAL metrics
		d.walFileCountGauge.Update(stats.WAL.Files)
		d.walSizeGauge.Update(int64(stats.WAL.Size))
		d.walPhysicalSizeGauge.Update(int64(stats.WAL.PhysicalSize))
		d.walObsoleteSizeGauge.Update(int64(stats.WAL.ObsoletePhysicalSize))

		// Update snapshot count
		d.snapshotCountGauge.Update(int64(stats.Snapshots.Count))

		// Calculate and update custom amplification metrics
		d.updateCalculatedAmplifications(stats)

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

// DeleteRange removes all keys in the range [start, end) from the batch for
// later committing, inclusive on start, exclusive on end.
func (b *batch) DeleteRange(start, end []byte) error {
	// There is no special flag to represent the end of key range
	// in pebble(nil in leveldb). Use an ugly hack to construct a
	// large key to represent it.
	if end == nil {
		end = ethdb.MaximumKey
	}
	if err := b.b.DeleteRange(start, end, nil); err != nil {
		return err
	}
	// Approximate size impact - just the keys
	b.size += len(start) + len(end)
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
		} else if kind == pebble.InternalKeyKindRangeDelete {
			// For range deletion, k is the start key and v is the end key
			if rangeDeleter, ok := w.(ethdb.KeyValueRangeDeleter); ok {
				if err = rangeDeleter.DeleteRange(k, v); err != nil {
					return err
				}
			} else {
				return errors.New("ethdb.KeyValueWriter does not implement DeleteRange")
			}
		} else {
			return fmt.Errorf("unhandled operation, keytype: %v", kind)
		}
	}
}

// Close closes the batch and releases all associated resources. After it is
// closed, any subsequent operations on this batch are undefined.
func (b *batch) Close() {
	b.b.Close()
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

// updateCalculatedAmplifications calculates and updates custom amplification metrics
func (d *Database) updateCalculatedAmplifications(stats *pebble.Metrics) {
	// Calculate Write Amplification for the database
	calcWriteAmp := d.calculateDatabaseWriteAmp(stats)
	if calcWriteAmp >= 0 {
		d.calcWriteAmpGauge.Update(calcWriteAmp)
	}

	// Calculate WAL Write Amplification
	walWriteAmp := d.calculateWALWriteAmp(stats)
	if walWriteAmp >= 0 {
		d.walWriteAmpGauge.Update(walWriteAmp)
	}

	// Calculate Read Amplification (same as Pebble's built-in metric)
	// This represents how many levels/sublevels need to be checked for a read
	readAmp := float64(stats.ReadAmp())
	d.calcReadAmpGauge.Update(readAmp)

	// Calculate Space Amplification: Total disk space / Actual user data size
	// This represents how much extra space is used compared to the logical data size
	diskSpaceUsed := int64(stats.DiskSpaceUsage())

	// Calculate actual user data size (sum of all live SST file sizes)
	// This excludes obsolete files, WAL files, and internal metadata
	// level.Size is the CURRENT live size, which already accounts for deleted files
	var actualDataSize int64
	for _, level := range stats.Levels {
		actualDataSize += level.Size
	}

	d.actualDataSizeGauge.Update(actualDataSize)

	if actualDataSize > 0 {
		// Space Amp = Total disk usage / Live data size
		// A value of 1.0 means no amplification (ideal)
		// A value of 2.0 means using 2x the space of actual data
		spaceAmp := float64(diskSpaceUsed) / float64(actualDataSize)
		d.calcSpaceAmpGauge.Update(spaceAmp)
	}
}

// calculateDatabaseWriteAmp calculates the write amplification for database writes.
func (d *Database) calculateDatabaseWriteAmp(stats *pebble.Metrics) float64 {
	var totalBytesIn uint64
	for _, level := range stats.Levels {
		totalBytesIn += level.BytesIn
	}

	if totalBytesIn == 0 {
		return -1
	}

	// Calculate SST writes (cumulative)
	var totalSSTWrites uint64
	for _, level := range stats.Levels {
		totalSSTWrites += level.BytesFlushed + level.BytesCompacted
	}

	// WAL.BytesWritten is the cumulative physical bytes written to .log files
	// This already includes:
	// - Record headers and checksums
	// - Batching overhead
	// - fsync/sync overhead
	//
	// But it does NOT include:
	// - Block alignment padding
	// - Pre-allocated space
	// - Recycled file space
	//
	// The ratio BytesWritten/BytesIn gives us the WAL encoding overhead
	walBytesWritten := stats.WAL.BytesWritten

	// Calculate total physical writes
	totalPhysicalWrites := walBytesWritten + totalSSTWrites

	// Write Amplification = Total physical writes / Logical user input
	writeAmp := float64(totalPhysicalWrites) / float64(totalBytesIn)

	return writeAmp
}

// calculateWALWriteAmp calculates WAL-specific write amplification
func (d *Database) calculateWALWriteAmp(stats *pebble.Metrics) float64 {
	if stats.WAL.BytesIn == 0 {
		return -1
	}

	// WAL write amplification = Physical writes / Logical application writes
	// This captures the overhead from:
	// - WAL record format (headers, checksums)
	// - Batching (multiple logical writes in one physical write)
	// - Sync overhead
	walWriteAmp := float64(stats.WAL.BytesWritten) / float64(stats.WAL.BytesIn)

	return walWriteAmp
}
