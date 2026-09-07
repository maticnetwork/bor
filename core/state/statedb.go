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

// Package state provides a caching layer atop the Ethereum state trie.
package state

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holiman/uint256"
	"golang.org/x/sync/errgroup"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/trie/trienode"
)

// TriesInMemory represents the number of layers that are kept in RAM.
const TriesInMemory = 128

type mutationType int

const (
	update mutationType = iota
	deletion
)

type mutation struct {
	typ     mutationType
	applied bool
}

func (m *mutation) copy() *mutation {
	return &mutation{typ: m.typ, applied: m.applied}
}

func (m *mutation) isDelete() bool {
	return m.typ == deletion
}

// StateDB structs within the ethereum protocol are used to store anything
// within the merkle trie. StateDBs take care of caching and storing
// nested states. It's the general query interface to retrieve:
//
// * Contracts
// * Accounts
//
// Once the state is committed, tries cached in stateDB (including account
// trie, storage tries) will no longer be functional. A new state instance
// must be created with new root and updated database for accessing post-
// commit states.
type StateDB struct {
	db         Database
	prefetcher *triePrefetcher
	reader     Reader
	trie       Trie // it's resolved on first access

	// originalRoot is the pre-state root, before any changes were made.
	// It will be updated when the Commit is called.
	originalRoot common.Hash

	// This map holds 'live' objects, which will get modified while
	// processing a state transition.
	stateObjects map[common.Address]*stateObject

	// This map holds 'deleted' objects. An object with the same address
	// might also occur in the 'stateObjects' map due to account
	// resurrection. The account value is tracked as the original value
	// before the transition. This map is populated at the transaction
	// boundaries.
	stateObjectsDestruct map[common.Address]*stateObject

	// currentBlockDestructs holds the subset of stateObjectsDestruct that this
	// block's own execution destructed. The FlatDiff replay paths also seed
	// stateObjectsDestruct — with the *parent* block's destructs, whose
	// post-parent storage the overlay legitimately serves — so the combined map
	// can't distinguish "storage was wiped by the block being executed" from
	// "the parent wiped it and the overlay holds what replaced it". Only the
	// former may suppress an overlay read.
	currentBlockDestructs map[common.Address]struct{}

	// This map tracks the account mutations that occurred during the
	// transition. Uncommitted mutations belonging to the same account
	// can be merged into a single one which is equivalent from database's
	// perspective. This map is populated at the transaction boundaries.
	mutations map[common.Address]*mutation

	// Block-stm related fields
	mvHashmap    *blockstm.MVHashMap
	incarnation  int
	readMap      map[blockstm.Key]blockstm.ReadDescriptor
	writeMap     map[blockstm.Key]blockstm.WriteDescriptor
	revertedKeys map[blockstm.Key]struct{}
	dep          int

	skipTimers bool // skip time.Now() calls in hot paths for worker stateDBs

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be
	// returned by StateDB.Commit. Notably, this error is also shared
	// by all cached state objects in case the database failure occurs
	// when accessing state of accounts.
	dbErr error

	// The refund counter, also used by state transitioning.
	refund uint64

	// The tx context and all occurred logs in the scope of transaction.
	thash   common.Hash
	txIndex int
	logs    map[common.Hash][]*types.Log
	logSize uint

	// Preimages occurred seen by VM in the scope of block.
	preimages map[common.Hash][]byte

	// Per-transaction access list
	accessList   *accessList
	accessEvents *AccessEvents

	// Transient storage
	transientStorage transientStorage

	// Journal of state modifications. This is the backbone of
	// Snapshot and RevertToSnapshot.
	journal *journal

	// State witness if cross validation is needed
	witness      *stateless.Witness
	witnessStats *stateless.WitnessStats
	// witnessPrewalkStop stops the read-set prewalker started by
	// StartWitnessReadSetPrewalk; CollectStateWitness invokes it before
	// collecting. Idempotent. Deliberately not carried across Copy.
	witnessPrewalkStop func()

	// nonExistentReads tracks addresses that were looked up but don't exist
	// in the state trie. Under pipelined SRC, these are included in the
	// FlatDiff so the SRC goroutine can walk their trie paths and capture
	// proof-of-absence nodes for the witness. Without this, stateless
	// execution fails when it tries to prove these accounts don't exist.
	nonExistentReads map[common.Address]struct{}

	// flatDiffRef is a read-only reference to the parent block's FlatDiff,
	// consulted lazily by getStateObject and GetCommittedState before falling
	// through to the trie reader. Set by NewWithFlatBase; nil otherwise.
	flatDiffRef *FlatDiff

	// Measurements gathered during execution for debugging purposes

	AccountLoaded        int          // Number of accounts retrieved from the database during the state transition
	AccountUpdated       int          // Number of accounts updated during the state transition
	AccountDeleted       int          // Number of accounts deleted during the state transition
	StorageLoaded        int          // Number of storage slots retrieved from the database during the state transition
	StorageUpdated       atomic.Int64 // Number of storage slots updated during the state transition
	StorageDeleted       atomic.Int64 // Number of storage slots deleted during the state transition
	AccountReads         time.Duration
	AccountHashes        time.Duration
	AccountUpdates       time.Duration
	AccountCommits       time.Duration
	StorageReads         time.Duration
	StorageHashes        time.Duration
	StorageUpdates       time.Duration
	StorageCommits       time.Duration
	SnapshotAccountReads time.Duration
	SnapshotStorageReads time.Duration
	SnapshotCommits      time.Duration
	TrieDBCommits        time.Duration
	WitnessCollection    time.Duration // time spent collecting trie nodes into witness during IntermediateRoot (sequential portion only)

	// Bor metrics
	BorConsensusTime time.Duration
}

// New creates a new state from a given trie.
func New(root common.Hash, db Database) (*StateDB, error) {
	reader, err := db.Reader(root)
	if err != nil {
		return nil, err
	}
	return NewWithReader(root, db, reader)
}

// NewTrieOnly creates a new state that uses only the trie reader (no flat/snapshot
// readers). This forces all account and storage reads to walk the MPT, which is
// required for witness building — the witness captures trie nodes during the walk.
// Used by the pipelined SRC goroutine to ensure the witness is complete.
func NewTrieOnly(root common.Hash, db *CachingDB) (*StateDB, error) {
	reader, err := db.TrieOnlyReader(root)
	if err != nil {
		return nil, err
	}
	return NewWithReader(root, db, reader)
}

// NewTrieOnlyWithSnapshot is the warm-cache variant of NewTrieOnly. Trie reads
// consult a WarmSnapshot (typically captured from the execution-side trie
// prefetcher) before falling through to the regular pathdb-backed NodeReader.
// Hits with matching hash skip diff-layer/disk-layer/pebble work entirely;
// misses or hash mismatches are served by the underlying reader unchanged.
// NewTrieOnly semantics are preserved — the trie still walks, prevalueTracer
// still records, witness is still complete. The snapshot wrapper is installed
// on the StateDB database itself, not just the initial Reader, so commit-time
// OpenTrie/OpenStorageTrie calls also use the same warm handoff.
//
// A nil snapshot is equivalent to NewTrieOnly.
func NewTrieOnlyWithSnapshot(root common.Hash, db *CachingDB, snapshot *WarmSnapshot) (*StateDB, error) {
	if snapshot == nil || snapshot.Len() == 0 {
		return NewTrieOnly(root, db)
	}
	snapshotDB := newSnapshotStateDatabase(db, snapshot)
	reader, err := snapshotDB.Reader(root)
	if err != nil {
		return nil, err
	}
	return NewWithReader(root, snapshotDB, reader)
}

// NewWithReader creates a new state for the specified state root. Unlike New,
// this function accepts an additional Reader which is bound to the given root.
func NewWithReader(root common.Hash, db Database, reader Reader) (*StateDB, error) {
	sdb := &StateDB{
		db:                    db,
		originalRoot:          root,
		reader:                reader,
		stateObjects:          make(map[common.Address]*stateObject),
		revertedKeys:          make(map[blockstm.Key]struct{}),
		stateObjectsDestruct:  make(map[common.Address]*stateObject),
		currentBlockDestructs: make(map[common.Address]struct{}),
		mutations:             make(map[common.Address]*mutation),
		logs:                  make(map[common.Hash][]*types.Log),
		preimages:             make(map[common.Hash][]byte),
		journal:               newJournal(),
		accessList:            newAccessList(),
		transientStorage:      newTransientStorage(),
	}
	if db.TrieDB().IsVerkle() {
		sdb.accessEvents = NewAccessEvents()
	}

	return sdb, nil
}

func NewWithMVHashmap(root common.Hash, db Database, snaps *snapshot.Tree, mvhm *blockstm.MVHashMap) (*StateDB, error) {
	if sdb, err := New(root, db); err != nil {
		return nil, err
	} else {
		sdb.mvHashmap = mvhm
		sdb.dep = -1

		return sdb, nil
	}
}

func (s *StateDB) SetMVHashmap(mvhm *blockstm.MVHashMap) {
	s.mvHashmap = mvhm
	s.dep = -1
}

func (s *StateDB) GetMVHashmap() *blockstm.MVHashMap {
	return s.mvHashmap
}

func (s *StateDB) MVWriteList() []blockstm.WriteDescriptor {
	writes := make([]blockstm.WriteDescriptor, 0, len(s.writeMap))

	for _, v := range s.writeMap {
		if _, ok := s.revertedKeys[v.Path]; !ok {
			writes = append(writes, v)
		}
	}

	return writes
}

func (s *StateDB) MVFullWriteList() []blockstm.WriteDescriptor {
	writes := make([]blockstm.WriteDescriptor, 0, len(s.writeMap))

	for _, v := range s.writeMap {
		writes = append(writes, v)
	}

	return writes
}

func (s *StateDB) MVReadMap() map[blockstm.Key]blockstm.ReadDescriptor {
	return s.readMap
}

func (s *StateDB) MVReadList() []blockstm.ReadDescriptor {
	reads := make([]blockstm.ReadDescriptor, 0, len(s.readMap))

	for _, v := range s.MVReadMap() {
		reads = append(reads, v)
	}

	return reads
}

func (s *StateDB) ensureReadMap() {
	if s.readMap == nil {
		s.readMap = make(map[blockstm.Key]blockstm.ReadDescriptor)
	}
}

func (s *StateDB) ensureWriteMap() {
	if s.writeMap == nil {
		s.writeMap = make(map[blockstm.Key]blockstm.WriteDescriptor)
	}
}

func (s *StateDB) ClearReadMap() {
	s.readMap = make(map[blockstm.Key]blockstm.ReadDescriptor)
}

func (s *StateDB) ClearWriteMap() {
	s.writeMap = make(map[blockstm.Key]blockstm.WriteDescriptor)
}

func (s *StateDB) HadInvalidRead() bool {
	return s.dep >= 0
}

func (s *StateDB) DepTxIndex() int {
	return s.dep
}

// RecordTransfer records a transfer for deferred log creation in parallel mode.
// V2 PDB has its own RecordTransfer that captures TransferRecords; the
// serial StateDB has nothing to capture, so this is a no-op.
func (s *StateDB) RecordTransfer(sender, recipient common.Address, amount *uint256.Int) bool {
	return false
}

func (s *StateDB) SetIncarnation(inc int) {
	s.incarnation = inc
}

var (
	witnessReadSetSettleTimer = metrics.NewRegisteredTimer("chain/witness/readset/settle", nil)
	witnessReadSetSettleKeys  = metrics.NewRegisteredCounter("chain/witness/readset/settle/keys", nil)
	witnessReadSetPrewalkKeys = metrics.NewRegisteredCounter("chain/witness/readset/prewalk/keys", nil)
)

const (
	witnessReadSetWalkWorkers  = 4
	witnessReadSetPrewalkEvery = time.Millisecond
)

// CollectStateWitness adds every trie node read through this StateDB's
// reader into the supplied witness's state set. Used by V2 BlockSTM to
// pull in worker reads that finalDB.IntermediateRoot would otherwise miss
// (they touch tries shared with finalDB but never appear in
// finalDB.stateObjects). No-op when witness is nil or the reader chain
// doesn't bottom out at a *trieReader (e.g. snapshot-only readers).
func (s *StateDB) CollectStateWitness() {
	if s.witness == nil {
		return
	}
	// Stop the prewalker before collecting: an in-flight resolution landing
	// after collection would be lost. Stopping here (rather than trusting
	// every caller to do it) makes the collect safe to call on its own.
	if s.witnessPrewalkStop != nil {
		s.witnessPrewalkStop()
	}
	start := time.Now()
	collectStateWitnessFromReader(s.reader, s.witness.AddState)
	witnessReadSetSettleTimer.UpdateSince(start)
}

// StartWitnessReadSetPrewalk keeps resolving newly-cached read keys into the
// trie reader's tracers while block execution is still running, so the
// settle-time drain in CollectStateWitness only covers a small tail instead
// of the whole block's read-set. The returned stop function is idempotent
// and blocks until the walker has fully exited; CollectStateWitness also
// invokes it before collecting. No-op when no witness is being recorded or
// the reader chain has no trie reader.
func (s *StateDB) StartWitnessReadSetPrewalk() (stop func()) {
	// Stop any previously started prewalker first so repeated calls can't
	// leak its goroutine.
	if s.witnessPrewalkStop != nil {
		s.witnessPrewalkStop()
		s.witnessPrewalkStop = nil
	}
	noop := func() {}
	if s.witness == nil {
		return noop
	}
	rwc := findReaderWithCache(s.reader)
	if rwc == nil {
		return noop
	}
	tr := findTrieReader(rwc.Reader)
	if tr == nil {
		return noop
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(witnessReadSetPrewalkEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				witnessReadSetPrewalkKeys.Inc(int64(rwc.resolveCachedKeysIntoTrie(tr, witnessReadSetWalkWorkers)))
			}
		}
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			close(done)
			wg.Wait()
		})
	}
	s.witnessPrewalkStop = stop
	return stop
}

func collectStateWitnessFromReader(r any, addState func(map[string][]byte)) {
	switch v := r.(type) {
	case *reader:
		collectStateWitnessFromReader(v.StateReader, addState)
	case *readerWithCache:
		// Cached keys whose first resolution came from a flat reader never
		// reached the trie tracers; walk them through the trie before
		// collecting so the witness covers every read, not just trie misses.
		// With a prewalker running this is only the un-walked tail.
		if tr := findTrieReader(v.Reader); tr != nil {
			witnessReadSetSettleKeys.Inc(int64(v.resolveCachedKeysIntoTrie(tr, witnessReadSetWalkWorkers)))
		}
		collectStateWitnessFromReader(v.Reader, addState)
	case *readerWithCacheStats:
		collectStateWitnessFromReader(v.readerWithCache, addState)
	case *trieReader:
		v.CollectStateWitness(addState)
	case *multiStateReader:
		for _, inner := range v.readers {
			collectStateWitnessFromReader(inner, addState)
		}
	}
}

// findReaderWithCache unwraps a reader chain to its shared *readerWithCache,
// or nil if the chain doesn't carry one.
func findReaderWithCache(r any) *readerWithCache {
	switch v := r.(type) {
	case *readerWithCacheStats:
		return v.readerWithCache
	case *readerWithCache:
		return v
	}
	return nil
}

// findTrieReader walks a reader chain down to its backing *trieReader, or nil
// if the chain doesn't bottom out at one (e.g. snapshot-only readers).
func findTrieReader(r any) *trieReader {
	switch v := r.(type) {
	case *reader:
		return findTrieReader(v.StateReader)
	case *readerWithCache:
		return findTrieReader(v.Reader)
	case *readerWithCacheStats:
		return findTrieReader(v.readerWithCache)
	case *trieReader:
		return v
	case *multiStateReader:
		for _, inner := range v.readers {
			if tr := findTrieReader(inner); tr != nil {
				return tr
			}
		}
	}
	return nil
}

// EnableConcurrentReads makes the trie reader safe for concurrent access
// by using sync.Map for node resolution instead of in-place tree mutation.
func (s *StateDB) EnableConcurrentReads() {
	enableConcurrentOnReader(s.reader)
}

// StorageCache returns the shared trieReader storage cache (sync.Map) if available.
// This cache is populated by all readers (prefetcher, serial, V2).
func (s *StateDB) StorageCache() *sync.Map {
	return findStorageCache(s.reader)
}

// OverlayPendingStorageInto walks every live stateObject and writes its
// in-memory pending+dirty storage values into target, keyed by stateKey{addr,
// slot}. This is useful for external storage caches that were populated from
// raw trie reads before pre-block writes (EIP-4788/EIP-2935 system contracts,
// DAO fork, etc.) landed in dirty/pending storage.
func (s *StateDB) OverlayPendingStorageInto(target *sync.Map) {
	if target == nil {
		return
	}
	for addr, obj := range s.stateObjects {
		if obj == nil {
			continue
		}
		obj.storageMutex.Lock()
		for slot, val := range obj.pendingStorage {
			target.Store(stateKey{addr: addr, slot: slot}, val)
		}
		for slot, val := range obj.dirtyStorage {
			target.Store(stateKey{addr: addr, slot: slot}, val)
		}
		obj.storageMutex.Unlock()
	}
}

func findStorageCache(r any) *sync.Map {
	switch v := r.(type) {
	case *reader:
		return findStorageCache(v.StateReader)
	case *readerWithCacheStats:
		return findStorageCache(v.readerWithCache)
	case *readerWithCache:
		return findStorageCache(v.Reader)
	case *trieReader:
		return &v.storageCache
	case *multiStateReader:
		for _, inner := range v.readers {
			if c := findStorageCache(inner); c != nil {
				return c
			}
		}
	}
	return nil
}

// enableConcurrentOnReader recursively finds and enables concurrent reads on
// all trieReaders in the reader chain, regardless of wrapper types.
func enableConcurrentOnReader(r any) {
	switch v := r.(type) {
	case *reader:
		enableConcurrentOnReader(v.StateReader)
	case *readerWithCache:
		enableConcurrentOnReader(v.Reader)
	case *readerWithCacheStats:
		enableConcurrentOnReader(v.readerWithCache)
	case *trieReader:
		v.EnableConcurrentReads()
	case *multiStateReader:
		for _, inner := range v.readers {
			enableConcurrentOnReader(inner)
		}
	}
}

type StorageVal[T any] struct {
	Value *T
}

func MVRead[T any](s *StateDB, k blockstm.Key, defaultV T, readStorage func(s *StateDB) T) (v T) {
	if s.mvHashmap == nil {
		return readStorage(s)
	}

	s.ensureReadMap()

	if s.writeMap != nil {
		if _, ok := s.writeMap[k]; ok {
			return readStorage(s)
		}
	}

	if !k.IsAddress() {
		// If we are reading subpath from a deleted account, return default value instead of reading from MVHashmap
		addr := k.GetAddress()
		if s.getStateObject(addr) == nil {
			return defaultV
		}
	}

	res := s.mvHashmap.Read(k, s.txIndex)

	var rd blockstm.ReadDescriptor

	rd.V = blockstm.Version{
		TxnIndex:    res.DepIdx(),
		Incarnation: res.Incarnation(),
	}

	rd.Path = k

	switch res.Status() {
	case blockstm.MVReadResultDone:
		{
			v = readStorage(res.Value().(*StateDB))
			rd.Kind = blockstm.ReadKindMap
		}
	case blockstm.MVReadResultDependency:
		{
			s.dep = res.DepIdx()

			panic("Found dependency")
		}
	case blockstm.MVReadResultNone:
		{
			v = readStorage(s)
			rd.Kind = blockstm.ReadKindStorage
		}
	default:
		return defaultV
	}

	if prevRd, ok := s.readMap[k]; !ok {
		s.readMap[k] = rd
	} else {
		if prevRd.Kind != rd.Kind || prevRd.V.TxnIndex != rd.V.TxnIndex || prevRd.V.Incarnation != rd.V.Incarnation {
			s.dep = rd.V.TxnIndex
			panic("Read conflict detected")
		}
	}

	return
}

func MVWrite(s *StateDB, k blockstm.Key) {
	if s.mvHashmap != nil {
		s.ensureWriteMap()
		s.writeMap[k] = blockstm.WriteDescriptor{
			Path: k,
			V:    s.Version(),
			Val:  s,
		}
	}
}

func RevertWrite(s *StateDB, k blockstm.Key) {
	s.revertedKeys[k] = struct{}{}
}

func MVWritten(s *StateDB, k blockstm.Key) bool {
	if s.mvHashmap == nil || s.writeMap == nil {
		return false
	}

	_, ok := s.writeMap[k]

	return ok
}

// FlushMVWriteSet applies entries in the write set to MVHashMap. Note that this function does not clear the write set.
func (s *StateDB) FlushMVWriteSet() {
	if s.mvHashmap != nil && s.writeMap != nil {
		s.mvHashmap.FlushMVWriteSet(s.MVFullWriteList())
	}
}

// ApplyMVWriteSet applies entries in a given write set to StateDB. Note that this function does not change MVHashMap nor write set
// of the current StateDB.
func (s *StateDB) ApplyMVWriteSet(writes []blockstm.WriteDescriptor) {
	for i := range writes {
		path := writes[i].Path
		sr := writes[i].Val.(*StateDB)

		if path.IsState() {
			addr := path.GetAddress()
			stateKey := path.GetStateKey()
			state := sr.GetState(addr, stateKey)
			s.SetState(addr, stateKey, state)
		} else if path.IsAddress() {
			continue
		} else {
			addr := path.GetAddress()

			switch path.GetSubpath() {
			case BalancePath:
				s.SetBalance(addr, sr.GetBalance(addr), tracing.BalanceChangeUnspecified)
			case NoncePath:
				s.SetNonce(addr, sr.GetNonce(addr), tracing.NonceChangeUnspecified)
			case CodePath:
				s.SetCode(addr, sr.GetCode(addr), tracing.CodeChangeUnspecified)
			case SuicidePath:
				stateObject := s.getStateObject(addr)
				if stateObject != nil && sr.HasSelfDestructed(addr) {
					s.SelfDestruct(addr)
				}
			default:
				panic(fmt.Errorf("unknown key type: %d", path.GetSubpath()))
			}
		}
	}
}

type DumpStruct struct {
	TxIdx  int
	TxInc  int
	VerIdx int
	VerInc int
	Path   []byte
	Op     string
}

// GetReadMapDump gets readMap Dump of format: "TxIdx, Inc, Path, Read"
func (s *StateDB) GetReadMapDump() []DumpStruct {
	readList := s.MVReadList()
	res := make([]DumpStruct, 0, len(readList))

	for _, val := range readList {
		temp := &DumpStruct{
			TxIdx:  s.txIndex,
			TxInc:  s.incarnation,
			VerIdx: val.V.TxnIndex,
			VerInc: val.V.Incarnation,
			Path:   val.Path[:],
			Op:     "Read\n",
		}
		res = append(res, *temp)
	}

	return res
}

// GetWriteMapDump gets writeMap Dump of format: "TxIdx, Inc, Path, Write"
func (s *StateDB) GetWriteMapDump() []DumpStruct {
	writeList := s.MVWriteList()
	res := make([]DumpStruct, 0, len(writeList))

	for _, val := range writeList {
		temp := &DumpStruct{
			TxIdx:  s.txIndex,
			TxInc:  s.incarnation,
			VerIdx: val.V.TxnIndex,
			VerInc: val.V.Incarnation,
			Path:   val.Path[:],
			Op:     "Write\n",
		}
		res = append(res, *temp)
	}

	return res
}

// AddEmptyMVHashMap adds empty MVHashMap to StateDB
func (s *StateDB) AddEmptyMVHashMap() {
	mvh := blockstm.MakeMVHashMap()
	s.mvHashmap = mvh
}

func (s *StateDB) SetWitness(witness *stateless.Witness) {
	s.witness = witness
}

// StartPrefetcher initializes a new trie prefetcher to pull in nodes from the
// state trie concurrently while the state is mutated so that when we reach the
// commit phase, most of the needed data is already hot.
func (s *StateDB) StartPrefetcher(namespace string, witness *stateless.Witness, witnessStats *stateless.WitnessStats) {
	// Terminate any previously running prefetcher
	s.StopPrefetcher()

	// Enable witness collection if requested
	s.witness = witness
	s.witnessStats = witnessStats

	// With the switch to the Proof-of-Stake consensus algorithm, block production
	// rewards are now handled at the consensus layer. Consequently, a block may
	// have no state transitions if it contains no transactions and no withdrawals.
	// In such cases, the account trie won't be scheduled for prefetching, leading
	// to unnecessary error logs.
	//
	// To prevent this, the account trie is always scheduled for prefetching once
	// the prefetcher is constructed. For more details, see:
	// https://github.com/ethereum/go-ethereum/issues/29880
	s.prefetcher = newTriePrefetcher(s.db, s.originalRoot, namespace, witness == nil)
	if err := s.prefetcher.prefetch(common.Hash{}, s.originalRoot, common.Address{}, nil, nil, false); err != nil {
		log.Error("Failed to prefetch account trie", "root", s.originalRoot, "err", err)
	}
}

// StopPrefetcher terminates a running prefetcher and reports any leftover stats
// from the gathered metrics.
func (s *StateDB) StopPrefetcher() {
	if s.prefetcher != nil {
		s.prefetcher.terminate(false)
		s.prefetcher.report()
		s.prefetcher = nil
	}
}

// DetachedPrefetcher is a trie prefetcher that has been removed from its
// StateDB and handed to another owner. It is used by pipelined SRC import so
// the import thread can move on with a fresh StateDB while SRC waits for the
// previous block's prefetcher to finish.
//
// A detached prefetcher must be consumed exactly once via Stop or
// StopAndCollectWarmSnapshot. Both methods synchronously wait for all
// subfetcher goroutines to exit before reporting stats.
type DetachedPrefetcher struct {
	prefetcher *triePrefetcher
}

// DetachPrefetcher removes the current prefetcher from the StateDB without
// stopping it. The caller becomes responsible for eventually calling Stop or
// StopAndCollectWarmSnapshot on the returned handle.
func (s *StateDB) DetachPrefetcher() *DetachedPrefetcher {
	if s.prefetcher == nil {
		return nil
	}
	prefetcher := s.prefetcher
	s.prefetcher = nil
	return &DetachedPrefetcher{prefetcher: prefetcher}
}

// PrefetcherSnapshotStats describes the synchronous phases and warm-node mix
// observed while stopping and snapshotting a trie prefetcher.
type PrefetcherSnapshotStats struct {
	Drain   time.Duration
	Collect time.Duration
	Report  time.Duration

	Fetchers        int
	LoadedFetchers  int
	AccountFetchers int
	StorageFetchers int

	AccountNodes int
	StorageNodes int
	AccountBytes int
	StorageBytes int
}

// Stop synchronously drains a detached prefetcher, reports its stats, and
// discards any warm nodes it loaded. This is the wait-only pipelined SRC mode:
// it lets the execution-side prefetcher finish warming shared lower-level
// caches without installing a WarmSnapshot reader.
func (p *DetachedPrefetcher) Stop() PrefetcherSnapshotStats {
	var stats PrefetcherSnapshotStats
	if p == nil || p.prefetcher == nil {
		return stats
	}
	prefetcher := p.prefetcher
	p.prefetcher = nil

	phaseStart := time.Now()
	prefetcher.terminate(false)
	stats.Drain = time.Since(phaseStart)
	stats.Fetchers = prefetcher.fetcherCount()

	phaseStart = time.Now()
	prefetcher.report()
	stats.Report = time.Since(phaseStart)
	return stats
}

// StopAndCollectWarmSnapshot synchronously drains a detached prefetcher,
// captures the trie nodes its subfetchers loaded, reports stats, and returns a
// quiesced WarmSnapshotInput owned by the caller.
//
// This method uses full-drain termination. Execution can continue on the
// import thread while SRC waits here, so queued prefetch work is allowed to
// finish and increase the warm surface.
func (p *DetachedPrefetcher) StopAndCollectWarmSnapshot() (*WarmSnapshotInput, PrefetcherSnapshotStats) {
	var stats PrefetcherSnapshotStats
	if p == nil || p.prefetcher == nil {
		return nil, stats
	}
	prefetcher := p.prefetcher
	p.prefetcher = nil

	phaseStart := time.Now()
	prefetcher.terminate(false)
	stats.Drain = time.Since(phaseStart)

	phaseStart = time.Now()
	tries, snapshotStats := prefetcher.snapshotWarmNodes()
	stats.Collect = time.Since(phaseStart)
	stats.Fetchers = snapshotStats.Fetchers
	stats.LoadedFetchers = snapshotStats.LoadedFetchers
	stats.AccountFetchers = snapshotStats.AccountFetchers
	stats.StorageFetchers = snapshotStats.StorageFetchers
	stats.AccountNodes = snapshotStats.AccountNodes
	stats.StorageNodes = snapshotStats.StorageNodes
	stats.AccountBytes = snapshotStats.AccountBytes
	stats.StorageBytes = snapshotStats.StorageBytes

	phaseStart = time.Now()
	prefetcher.report()
	stats.Report = time.Since(phaseStart)
	if len(tries) == 0 {
		return nil, stats
	}
	return NewWarmSnapshotInput(tries), stats
}

// ResetPrefetcher cleans the prefetcher from a State, commonly used in tempStates to track witness while no impacting block building
// Do also remove mutations previously tracked to just look to the new ones
func (s *StateDB) ResetPrefetcher() {
	s.prefetcher = nil
	s.mutations = make(map[common.Address]*mutation)
	s.stateObjects = make(map[common.Address]*stateObject)
	s.stateObjectsDestruct = make(map[common.Address]*stateObject)
	s.currentBlockDestructs = make(map[common.Address]struct{})
}

// setError remembers the first non-nil error it is called with.
func (s *StateDB) setError(err error) {
	if s.dbErr == nil {
		s.dbErr = err
	}
}

// Error returns the memorized database failure occurred earlier.
func (s *StateDB) Error() error {
	return s.dbErr
}

func (s *StateDB) AddLog(log *types.Log) {
	s.journal.logChange(s.thash)

	log.TxHash = s.thash
	log.TxIndex = uint(s.txIndex)
	log.Index = s.logSize
	s.logs[s.thash] = append(s.logs[s.thash], log)
	s.logSize++
}

// GetLogs returns the logs matching the specified transaction hash, and annotates
// them with the given blockNumber and blockHash.
func (s *StateDB) GetLogs(hash common.Hash, blockNumber uint64, blockHash common.Hash, blockTime uint64) []*types.Log {
	logs := s.logs[hash]
	for _, l := range logs {
		l.BlockNumber = blockNumber
		l.BlockHash = blockHash
		l.BlockTimestamp = blockTime
	}

	return logs
}

func (s *StateDB) Logs() []*types.Log {
	logs := make([]*types.Log, 0, s.logSize)
	for _, lgs := range s.logs {
		logs = append(logs, lgs...)
	}
	return logs
}

// AddPreimage records a SHA3 preimage seen by the VM.
func (s *StateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := s.preimages[hash]; !ok {
		s.preimages[hash] = slices.Clone(preimage)
	}
}

// Preimages returns a list of SHA3 preimages that have been submitted.
func (s *StateDB) Preimages() map[common.Hash][]byte {
	return s.preimages
}

// AddRefund adds gas to the refund counter
func (s *StateDB) AddRefund(gas uint64) {
	s.journal.refundChange(s.refund)
	s.refund += gas
}

// SubRefund removes gas from the refund counter.
// This method will panic if the refund counter goes below zero
func (s *StateDB) SubRefund(gas uint64) {
	s.journal.refundChange(s.refund)
	if gas > s.refund {
		panic(fmt.Sprintf("Refund counter below zero (gas: %d > refund: %d)", gas, s.refund))
	}

	s.refund -= gas
}

// Exist reports whether the given account address exists in the state.
// Notably this also returns true for self-destructed accounts within the current transaction.
func (s *StateDB) Exist(addr common.Address) bool {
	return s.getStateObject(addr) != nil
}

// Empty returns whether the state object is either non-existent
// or empty according to the EIP161 specification (balance = nonce = code = 0)
func (s *StateDB) Empty(addr common.Address) bool {
	so := s.getStateObject(addr)
	return so == nil || so.empty()
}

// Create a unique path for special fields (e.g. balance, code) in a state object.
// func subPath(prefix []byte, s uint8) [blockstm.KeyLength]byte {
// 	path := append(prefix, common.Hash{}.Bytes()...) // append a full empty hash to avoid collision with storage state
// 	path = append(path, s)                           // append the special field identifier

// 	return path
// }

const BalancePath = 1
const NoncePath = 2
const CodePath = 3

// SuicidePath flags an account as self-destructed. V1 uses it as an MVHashMap
// key on the serial StateDB; V2 uses it as an MVStore subpath so later txs
// in the same block see the account as non-existent during parallel reads.
const SuicidePath = 4
const CreatePath = 5

// GetBalance retrieves the balance from the given address or 0 if object not found.
// Restored to the original (origin/develop) MVRead-based form. The delta-balance
// optimization that lived here previously had a fundamental inconsistency between
// the MVHashmap delta view (used for cross-tx reads) and stateObject.Balance() (used
// for self-write reads), which caused fee-transfer log divergence between V1 parallel
// and serial when coinbase accumulated tips. V2 uses MVBalanceStore (separate path)
// and is unaffected by this revert.
func (s *StateDB) GetBalance(addr common.Address) *uint256.Int {
	return MVRead(s, blockstm.NewSubpathKey(addr, BalancePath), uint256.NewInt(0), func(s *StateDB) *uint256.Int {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			return stateObject.Balance()
		}
		return uint256.NewInt(0)
	})
}

// GetNonce retrieves the nonce from the given address or 0 if object not found
func (s *StateDB) GetNonce(addr common.Address) uint64 {
	return MVRead(s, blockstm.NewSubpathKey(addr, NoncePath), 0, func(s *StateDB) uint64 {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			return stateObject.Nonce()
		}

		return 0
	})
}

// GetStorageRoot retrieves the storage root from the given address or empty
// if object not found.
func (s *StateDB) GetStorageRoot(addr common.Address) common.Hash {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.Root()
	}
	return common.Hash{}
}

// TxIndex returns the current transaction index set by SetTxContext.
func (s *StateDB) TxIndex() int {
	return s.txIndex
}

func (s *StateDB) Version() blockstm.Version {
	return blockstm.Version{
		TxnIndex:    s.txIndex,
		Incarnation: s.incarnation,
	}
}

func (s *StateDB) GetCode(addr common.Address) []byte {
	return MVRead(s, blockstm.NewSubpathKey(addr, CodePath), nil, func(s *StateDB) []byte {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			if s.witness != nil {
				s.witness.AddCode(stateObject.Code())
			}
			return stateObject.Code()
		}

		return nil
	})
}

func (s *StateDB) GetCodeSize(addr common.Address) int {
	return MVRead(s, blockstm.NewSubpathKey(addr, CodePath), 0, func(s *StateDB) int {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			if s.witness != nil {
				s.witness.AddCode(stateObject.Code())
			}
			return stateObject.CodeSize()
		}

		return 0
	})
}

func (s *StateDB) GetCodeHash(addr common.Address) common.Hash {
	return MVRead(s, blockstm.NewSubpathKey(addr, CodePath), common.Hash{}, func(s *StateDB) common.Hash {
		stateObject := s.getStateObject(addr)
		if stateObject == nil {
			return common.Hash{}
		}

		return common.BytesToHash(stateObject.CodeHash())
	})
}

// GetState retrieves the value associated with the specific key.
func (s *StateDB) GetState(addr common.Address, hash common.Hash) common.Hash {
	return MVRead(s, blockstm.NewStateKey(addr, hash), common.Hash{}, func(s *StateDB) common.Hash {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			return stateObject.GetState(hash)
		}

		return common.Hash{}
	})
}

// GetCommittedState retrieves the value associated with the specific key
// without any mutations caused in the current execution.
func (s *StateDB) GetCommittedState(addr common.Address, hash common.Hash) common.Hash {
	return MVRead(s, blockstm.NewStateKey(addr, hash), common.Hash{}, func(s *StateDB) common.Hash {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			return stateObject.GetCommittedState(hash)
		}

		return common.Hash{}
	})
}

// GetStateAndCommittedState returns the current value and the original value.
func (s *StateDB) GetStateAndCommittedState(addr common.Address, hash common.Hash) (common.Hash, common.Hash) {
	stateObject := s.getStateObject(addr)
	if stateObject != nil {
		return stateObject.getState(hash)
	}
	return common.Hash{}, common.Hash{}
}

// Database retrieves the low level database supporting the lower level trie ops.
func (s *StateDB) Database() Database {
	return s.db
}

// StorageTrie returns the storage trie of an account. The return value is a copy
// and is nil for non-existent accounts. An error will be returned if storage trie
// is existent but can't be loaded correctly.
func (s *StateDB) StorageTrie(addr common.Address) (Trie, error) {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		//nolint:nilnil
		return nil, nil
	}

	cpy := stateObject.deepCopy(s)

	if _, err := cpy.updateTrie(); err != nil {
		return nil, err
	}

	return cpy.getTrie()
}

// Reader retrieves the low level database reader supporting the
// lower level operations.
func (s *StateDB) Reader() Reader {
	return s.reader
}

func (s *StateDB) HasSelfDestructed(addr common.Address) bool {
	return MVRead(s, blockstm.NewSubpathKey(addr, SuicidePath), false, func(s *StateDB) bool {
		stateObject := s.getStateObject(addr)
		if stateObject != nil {
			return stateObject.selfDestructed
		}

		return false
	})
}

/*
 * SETTERS
 */

// AddBalance adds amount to the account associated with addr.
func (s *StateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject == nil {
		return uint256.Int{}
	}

	if s.mvHashmap != nil {
		// ensure a read balance operation is recorded in mvHashmap
		s.GetBalance(addr)
	}

	stateObject = s.mvRecordWritten(stateObject)
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
	return stateObject.AddBalance(amount)
}

// SubBalance subtracts amount from the account associated with addr.
func (s *StateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject == nil {
		return uint256.Int{}
	}

	if s.mvHashmap != nil {
		// ensure a read balance operation is recorded in mvHashmap
		s.GetBalance(addr)
	}

	stateObject = s.mvRecordWritten(stateObject)
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))

	if amount.IsZero() {
		return *(stateObject.Balance())
	}
	return stateObject.SetBalance(new(uint256.Int).Sub(stateObject.Balance(), amount))
}

// SetBalance sets amount to the account associated with addr.
func (s *StateDB) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject == nil {
		return uint256.Int{}
	}

	stateObject = s.mvRecordWritten(stateObject)
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
	return stateObject.SetBalance(amount)
}

func (s *StateDB) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		stateObject = s.mvRecordWritten(stateObject)
		stateObject.SetNonce(nonce)
		MVWrite(s, blockstm.NewSubpathKey(addr, NoncePath))
	}
}

func (s *StateDB) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) (prev []byte) {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		// TODO (@pratikspatil024) - verify this change
		stateObject = s.mvRecordWritten(stateObject)
		tempPrev := stateObject.SetCode(crypto.Keccak256Hash(code), code)
		MVWrite(s, blockstm.NewSubpathKey(addr, CodePath))
		return tempPrev
	}
	return nil
}

func (s *StateDB) SetState(addr common.Address, key, value common.Hash) common.Hash {
	stateObject := s.getOrNewStateObject(addr)
	if stateObject != nil {
		stateObject = s.mvRecordWritten(stateObject)
		MVWrite(s, blockstm.NewStateKey(addr, key))
		return stateObject.SetState(key, value)
	}
	return common.Hash{}
}

// SetStorage replaces the entire storage for the specified account with given
// storage. This function should only be used for debugging and the mutations
// must be discarded afterwards.
func (s *StateDB) SetStorage(addr common.Address, storage map[common.Hash]common.Hash) {
	// SetStorage needs to wipe the existing storage. We achieve this by marking
	// the account as self-destructed in this block. The effect is that storage
	// lookups will not hit the disk, as it is assumed that the disk data belongs
	// to a previous incarnation of the object.
	//
	// TODO (rjl493456442): This function should only be supported by 'unwritable'
	// state, and all mutations made should be discarded afterward.
	obj := s.getStateObject(addr)
	if obj != nil {
		if _, ok := s.stateObjectsDestruct[addr]; !ok {
			s.stateObjectsDestruct[addr] = obj
		}
	}
	s.currentBlockDestructs[addr] = struct{}{}
	newObj := s.createObject(addr)
	for k, v := range storage {
		newObj.SetState(k, v)
	}
	// Inherit the metadata of original object if it was existent
	if obj != nil {
		newObj.SetCode(common.BytesToHash(obj.CodeHash()), obj.code)
		newObj.SetNonce(obj.Nonce())
		newObj.SetBalance(obj.Balance())
	}
}

// SelfDestruct marks the given account as selfdestructed.
// This clears the account balance.
//
// The account's state object is still available until the state is committed,
// getStateObject will return a non-nil account after SelfDestruct.
func (s *StateDB) SelfDestruct(addr common.Address) uint256.Int {
	stateObject := s.getStateObject(addr)
	var prevBalance uint256.Int
	if stateObject == nil {
		return prevBalance
	}
	stateObject = s.mvRecordWritten(stateObject)

	prevBalance = *(stateObject.Balance())
	// Regardless of whether it is already destructed or not, we do have to
	// journal the balance-change, if we set it to zero here.
	if !stateObject.Balance().IsZero() {
		stateObject.SetBalance(new(uint256.Int))
	}
	// If it is already marked as self-destructed, we do not need to add it
	// for journalling a second time.
	if !stateObject.selfDestructed {
		s.journal.destruct(addr)
		stateObject.markSelfdestructed()
	}
	MVWrite(s, blockstm.NewSubpathKey(addr, SuicidePath))
	MVWrite(s, blockstm.NewSubpathKey(addr, BalancePath))
	return prevBalance
}

func (s *StateDB) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	stateObject := s.getStateObject(addr)
	if stateObject == nil {
		return uint256.Int{}, false
	}
	if stateObject.newContract {
		return s.SelfDestruct(addr), true
	}
	return *(stateObject.Balance()), false
}

// SetTransientState sets transient storage for a given account. It
// adds the change to the journal so that it can be rolled back
// to its previous value if there is a revert.
func (s *StateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	prev := s.GetTransientState(addr, key)
	if prev == value {
		return
	}
	s.journal.transientStateChange(addr, key, prev)
	s.setTransientState(addr, key, value)
}

// setTransientState is a lower level setter for transient storage. It
// is called during a revert to prevent modifications to the journal.
func (s *StateDB) setTransientState(addr common.Address, key, value common.Hash) {
	s.transientStorage.Set(addr, key, value)
}

// GetTransientState gets transient storage for a given account.
func (s *StateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return s.transientStorage.Get(addr, key)
}

//
// Setting, updating & deleting state object methods.
//

// updateStateObject writes the given object to the trie.
func (s *StateDB) updateStateObject(obj *stateObject) {
	// Encode the account and update the account trie
	if err := s.trie.UpdateAccount(obj.Address(), &obj.data, len(obj.code)); err != nil {
		s.setError(fmt.Errorf("updateStateObject (%x) error: %v", obj.Address(), err))
	}
	if obj.dirtyCode {
		s.trie.UpdateContractCode(obj.Address(), common.BytesToHash(obj.CodeHash()), obj.code)
	}
}

// deleteStateObject removes the given object from the state trie.
func (s *StateDB) deleteStateObject(addr common.Address) {
	// Track the amount of time wasted on deleting the account from the trie
	if metrics.Enabled() && !s.skipTimers {
		defer func(start time.Time) { s.AccountUpdates += time.Since(start) }(time.Now())
	}

	// Delete the account from the trie
	if err := s.trie.DeleteAccount(addr); err != nil {
		s.setError(fmt.Errorf("deleteStateObject (%x) error: %v", addr[:], err))
	}
}

func (s *StateDB) getStateObject(addr common.Address) *stateObject {
	return MVRead(s, blockstm.NewAddressKey(addr), nil, func(s *StateDB) *stateObject {
		// FlatDiff is part of this StateDB's logical base. Let it mask stale
		// cached objects loaded from committedParentRoot, unless the current
		// execution has already dirtied the account.
		if s.flatDiffRef != nil && !s.hasAccountMutation(addr) {
			if acct, exists, covered := s.flatDiffRef.accountOverlay(addr); covered {
				if !exists {
					return nil
				}
				if obj := s.stateObjects[addr]; obj != nil && obj.fromFlatDiff {
					return obj
				}
				return s.flatDiffStateObject(addr, acct)
			}
		}
		// Prefer live objects if any is available
		if obj := s.stateObjects[addr]; obj != nil {
			return obj
		}
		// Short circuit if the account is already destructed in this block.
		if _, ok := s.stateObjectsDestruct[addr]; ok {
			return nil
		}
		s.AccountLoaded++

		var start time.Time
		if !s.skipTimers {
			start = time.Now()
		}
		acct, err := s.reader.Account(addr)
		if err != nil {
			s.setError(fmt.Errorf("getStateObject (%x) error: %w", addr.Bytes(), err))
			return nil
		}
		if !s.skipTimers {
			s.AccountReads += time.Since(start)
		}
		// Independent of where we loaded the data from, add it to the prefetcher.
		// Whilst this would be a bit weird if snapshots are disabled, but we still
		// want the trie nodes to end up in the prefetcher too, so just push through.
		if s.prefetcher != nil {
			if err = s.prefetcher.prefetch(common.Hash{}, s.originalRoot, common.Address{}, []common.Address{addr}, nil, true); err != nil {
				log.Error("Failed to prefetch account", "addr", addr, "err", err)
			}
		}
		// Short circuit if the account is not found
		if acct == nil {
			// Track the address so the pipelined SRC goroutine can walk
			// the trie path and capture proof-of-absence nodes for the
			// witness. Without this, stateless execution can't verify
			// non-existent accounts.
			if s.nonExistentReads == nil {
				s.nonExistentReads = make(map[common.Address]struct{})
			}
			s.nonExistentReads[addr] = struct{}{}
			return nil
		}
		// Insert into the live set
		obj := newObject(s, addr, acct)
		s.setStateObject(obj)
		s.AccountLoaded++
		return obj
	})
}

func (s *StateDB) setStateObject(object *stateObject) {
	s.stateObjects[object.Address()] = object
}

// hasAccountMutation reports whether current execution already owns the
// account value. FlatDiff still defines the parent base, but it must not
// replace a live object that this block has dirtied.
func (s *StateDB) hasAccountMutation(addr common.Address) bool {
	if _, ok := s.journal.dirties[addr]; ok {
		return true
	}
	if _, ok := s.mutations[addr]; ok {
		return true
	}
	return false
}

func (s *StateDB) flatDiffStateObject(addr common.Address, acct types.StateAccount) *stateObject {
	flatDiffAccountHitsMeter.Mark(1)
	acctCopy := acct
	obj := newObject(s, addr, &acctCopy)
	obj.fromFlatDiff = true
	if code, ok := s.flatDiffRef.Code[common.BytesToHash(acctCopy.CodeHash)]; ok {
		obj.code = code
	}
	// Resolve the committed storage root for prefetcher consistency.
	//
	// The FlatDiff account's Root is block N's post-state storage root,
	// but the prefetcher's NodeReader is opened at committedParentRoot
	// (the grandparent). These are inconsistent — the reader can only
	// resolve trie nodes for the grandparent's storage root. Without
	// this, the prefetcher hits "Unexpected trie node" hash mismatches
	// on every storage trie root resolution for FlatDiff accounts.
	//
	// We read the account from the committed state (flat reader, in-
	// memory snapshot) to get the grandparent's storage root. This is
	// the root that the prefetcher's reader can actually resolve.
	if acctCopy.Root != types.EmptyRootHash {
		if committedAcct, err := s.reader.Account(addr); err == nil && committedAcct != nil {
			obj.prefetchRoot = committedAcct.Root
		} else {
			obj.prefetchRoot = types.EmptyRootHash
		}
		// If the account doesn't exist in the committed state (new in
		// block N), prefetchRoot is set to the empty storage root so the
		// storage prefetcher skips it; the trie didn't exist at
		// committedParentRoot and block N's post-state root would be
		// inconsistent with this reader.
	}
	s.setStateObject(obj)
	return obj
}

// Exporting so that it can be used by simulated backend for test cases
func (s *StateDB) GetOrNewStateObject(addr common.Address) *stateObject {
	return s.getOrNewStateObject(addr)
}

// getOrNewStateObject retrieves a state object or create a new state object if nil.
func (s *StateDB) getOrNewStateObject(addr common.Address) *stateObject {
	obj := s.getStateObject(addr)
	if obj == nil {
		obj = s.createObject(addr)
	}
	return obj
}

// mvRecordWritten checks whether a state object is already present in the current MV writeMap.
// If yes, it returns the object directly.
// If not, it clones the object and inserts it into the writeMap before returning it.
func (s *StateDB) mvRecordWritten(object *stateObject) *stateObject {
	if s.mvHashmap == nil {
		return object
	}

	addrKey := blockstm.NewAddressKey(object.Address())

	if MVWritten(s, addrKey) {
		return object
	}

	// Deepcopy is needed to ensure that objects are not written by multiple transactions at the same time, because
	// the input state object can come from a different transaction.
	s.setStateObject(object.deepCopy(s))
	MVWrite(s, addrKey)

	return s.stateObjects[object.Address()]
}

// createObject creates a new state object. The assumption is held there is no
// existing account with the given address, otherwise it will be silently overwritten.
func (s *StateDB) createObject(addr common.Address) *stateObject {
	obj := newObject(s, addr, nil)
	s.journal.createObject(addr)
	s.setStateObject(obj)
	MVWrite(s, blockstm.NewAddressKey(addr))
	return obj
}

// CreateAccount explicitly creates a new state object, assuming that the
// account did not previously exist in the state. If the account already
// exists, this function will silently overwrite it which might lead to a
// consensus bug eventually.
func (s *StateDB) CreateAccount(addr common.Address) {
	s.createObject(addr)
}

// CreateContract is used whenever a contract is created. This may be preceded
// by CreateAccount, but that is not required if it already existed in the
// state due to funds sent beforehand.
// This operation sets the 'newContract'-flag, which is required in order to
// correctly handle EIP-6780 'delete-in-same-transaction' logic.
func (s *StateDB) CreateContract(addr common.Address) {
	obj := s.getStateObject(addr)
	if obj != nil {
		obj = s.mvRecordWritten(obj)
	}
	if !obj.newContract {
		obj.newContract = true
		s.journal.createContract(addr)
	}

	MVWrite(s, blockstm.NewAddressKey(addr))
}

// Copy creates a deep, independent copy of the state.
// Snapshots of the copied state cannot be applied to the copy.
func (s *StateDB) Copy() *StateDB {
	// Copy all the basic fields, initialize the memory ones
	state := &StateDB{
		db:                    s.db,
		reader:                s.reader,
		originalRoot:          s.originalRoot,
		stateObjects:          make(map[common.Address]*stateObject, len(s.stateObjects)),
		stateObjectsDestruct:  make(map[common.Address]*stateObject, len(s.stateObjectsDestruct)),
		currentBlockDestructs: make(map[common.Address]struct{}, len(s.currentBlockDestructs)),
		revertedKeys:          make(map[blockstm.Key]struct{}),
		mutations:             make(map[common.Address]*mutation, len(s.mutations)),
		dbErr:                 s.dbErr,
		refund:                s.refund,
		thash:                 s.thash,
		txIndex:               s.txIndex,
		logs:                  make(map[common.Hash][]*types.Log, len(s.logs)),
		logSize:               s.logSize,
		preimages:             maps.Clone(s.preimages),

		// Timing fields — must be carried over so metrics in resultLoop see
		// the values accumulated during fillTransactions, not zero.
		AccountReads:         s.AccountReads,
		StorageReads:         s.StorageReads,
		SnapshotAccountReads: s.SnapshotAccountReads,
		SnapshotStorageReads: s.SnapshotStorageReads,
		BorConsensusTime:     s.BorConsensusTime,

		// Do we need to copy the access list and transient storage?
		// In practice: No. At the start of a transaction, these two lists are empty.
		// In practice, we only ever copy state _between_ transactions/blocks, never
		// in the middle of a transaction. However, it doesn't cost us much to copy
		// empty lists, so we do it anyway to not blow up if we ever decide copy them
		// in the middle of a transaction.
		accessList:       s.accessList.Copy(),
		transientStorage: s.transientStorage.Copy(),
		journal:          s.journal.copy(),
	}
	state.flatDiffRef = s.flatDiffRef // read-only, safe to share
	if s.trie != nil {
		state.trie = mustCopyTrie(s.trie)
	}
	if s.witness != nil {
		state.witness = s.witness.Copy()
	}
	if s.accessEvents != nil {
		state.accessEvents = s.accessEvents.Copy()
	}
	// Deep copy cached state objects.
	for addr, obj := range s.stateObjects {
		state.stateObjects[addr] = obj.deepCopy(state)
	}
	// Deep copy destructed state objects.
	for addr := range s.currentBlockDestructs {
		state.currentBlockDestructs[addr] = struct{}{}
	}
	for addr, obj := range s.stateObjectsDestruct {
		state.stateObjectsDestruct[addr] = obj.deepCopy(state)
	}
	// Deep copy the object state markers.
	for addr, op := range s.mutations {
		state.mutations[addr] = op.copy()
	}
	// Deep copy the logs occurred in the scope of block
	for hash, logs := range s.logs {
		cpy := make([]*types.Log, len(logs))
		for i, l := range logs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}

		state.logs[hash] = cpy
	}
	// Do we need to copy the access list and transient storage?
	// In practice: No. At the start of a transaction, these two lists are empty.
	// In practice, we only ever copy state _between_ transactions/blocks, never
	// in the middle of a transaction. However, it doesn't cost us much to copy
	// empty lists, so we do it anyway to not blow up if we ever decide copy them
	// in the middle of a transaction.
	state.accessList = s.accessList.Copy()
	state.transientStorage = s.transientStorage.Copy()

	if s.prefetcher != nil {
		state.prefetcher = s.prefetcher
	}

	if s.mvHashmap != nil {
		state.mvHashmap = s.mvHashmap
	}

	if len(s.nonExistentReads) > 0 {
		state.nonExistentReads = maps.Clone(s.nonExistentReads)
	}

	return state
}

// Snapshot returns an identifier for the current revision of the state.
func (s *StateDB) Snapshot() int {
	return s.journal.snapshot()
}

// RevertToSnapshot reverts all state changes made since the given revision.
func (s *StateDB) RevertToSnapshot(revid int) {
	s.journal.revertToSnapshot(revid, s)
}

// GetRefund returns the current value of the refund counter.
func (s *StateDB) GetRefund() uint64 {
	return s.refund
}

// Finalise finalises the state by removing the destructed objects and clears
// the journal as well as the refunds. Finalise, however, will not push any updates
// into the tries just yet. Only IntermediateRoot or Commit will do that.
func (s *StateDB) Finalise(deleteEmptyObjects bool) {
	addressesToPrefetch := make([]common.Address, 0, len(s.journal.dirties))
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist {
			// ripeMD is 'touched' at block 1714175, in tx 0x1237f737031e40bcde4a8b7e717b2d15e3ecadfe49bb1bbc71ee9deb09c6fcf2
			// That tx goes out of gas, and although the notion of 'touched' does not exist there, the
			// touch-event will still be recorded in the journal. Since ripeMD is a special snowflake,
			// it will persist in the journal even though the journal is reverted. In this special circumstance,
			// it may exist in `s.journal.dirties` but not in `s.stateObjects`.
			// Thus, we can safely ignore it here
			continue
		}

		if obj.selfDestructed || (deleteEmptyObjects && obj.empty()) {
			delete(s.stateObjects, obj.address)
			s.markDelete(addr)
			// We need to maintain account deletions explicitly (will remain
			// set indefinitely). Note only the first occurred self-destruct
			// event is tracked.
			if _, ok := s.stateObjectsDestruct[obj.address]; !ok {
				s.stateObjectsDestruct[obj.address] = obj
			}
			s.currentBlockDestructs[obj.address] = struct{}{}
		} else {
			obj.finalise()
			s.markUpdate(addr)
		}
		// At this point, also ship the address off to the precacher. The precacher
		// will start loading tries, and when the change is eventually committed,
		// the commit-phase will be a lot faster
		addressesToPrefetch = append(addressesToPrefetch, addr) // Copy needed for closure
	}

	if s.prefetcher != nil && len(addressesToPrefetch) > 0 {
		if err := s.prefetcher.prefetch(common.Hash{}, s.originalRoot, common.Address{}, addressesToPrefetch, nil, false); err != nil {
			if !errors.Is(err, errTerminated) {
				log.Error("Failed to prefetch addresses", "addresses", len(addressesToPrefetch), "err", err)
			}
		}
	}
	// Invalidate journal because reverting across transactions is not allowed.
	s.clearJournalAndRefund()
}

// addWitnessNodes adds storage-trie nodes to the block witness and, when
// per-account stats are being tracked, attributes them to the account.
func (s *StateDB) addWitnessNodes(nodes map[string][]byte, addrHash common.Hash) {
	s.witness.AddState(nodes)
	if s.witnessStats != nil {
		s.witnessStats.Add(nodes, addrHash)
	}
}

// addObjectWitness pulls the object's storage-trie witness into the block
// witness, preferring the prefetched trie, then the object's own trie. With
// neither available and no prefetcher running, storage reads went through the
// reader (separate trie / prevalueTracer), so the intermediate proof-path
// nodes are missing — open the storage trie and re-read the accessed slots to
// capture them.
func (s *StateDB) addObjectWitness(obj *stateObject) {
	if trie := obj.getPrefetchedTrie(); trie != nil {
		s.addWitnessNodes(trie.Witness(), obj.addrHash())
	} else if obj.trie != nil {
		s.addWitnessNodes(obj.trie.Witness(), obj.addrHash())
	} else if s.prefetcher == nil {
		if tr, err := obj.getTrie(); err == nil {
			for key := range obj.originStorage {
				tr.GetStorage(obj.address, key[:])
			}
			s.addWitnessNodes(tr.Witness(), obj.addrHash())
		}
	}
}

// IntermediateRoot computes the current root hash of the state trie.
// It is called in between transactions to get the root hash that
// goes into transaction receipts.
func (s *StateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash {
	// Finalise all the dirty storage states and write them into the tries
	s.Finalise(deleteEmptyObjects)

	// Initialize the trie if it's not constructed yet. If the prefetch
	// is enabled, the trie constructed below will be replaced by the
	// prefetched one.
	//
	// This operation must be done before state object storage hashing,
	// as it assumes the main trie is already loaded.
	if s.trie == nil {
		tr, err := s.db.OpenTrie(s.originalRoot)
		if err != nil {
			s.setError(err)
			return common.Hash{}
		}
		s.trie = tr
	}
	// If there was a trie prefetcher operating, terminate it async so that the
	// individual storage tries can be updated as soon as the disk load finishes.
	if s.prefetcher != nil {
		s.prefetcher.terminate(true)
		defer func() {
			s.prefetcher.report()
			s.prefetcher = nil // Pre-byzantium, unset any used up prefetcher
		}()
	}
	// Process all storage updates concurrently. The state object update root
	// method will internally call a blocking trie fetch from the prefetcher,
	// so there's no need to explicitly wait for the prefetchers to finish.
	var (
		start   time.Time
		workers errgroup.Group
	)
	if !s.skipTimers {
		start = time.Now()
	}
	if s.db.TrieDB().IsVerkle() {
		// Whilst MPT storage tries are independent, Verkle has one single trie
		// for all the accounts and all the storage slots merged together. The
		// former can thus be simply parallelized, but updating the latter will
		// need concurrency support within the trie itself. That's a TODO for a
		// later time.
		workers.SetLimit(1)
	}
	for addr, op := range s.mutations {
		if op.applied || op.isDelete() {
			continue
		}
		obj := s.stateObjects[addr] // closure for the task runner below
		workers.Go(func() error {
			if s.db.TrieDB().IsVerkle() {
				obj.updateTrie()
			} else {
				obj.updateRoot()

				// If witness building is enabled and the state object has a trie,
				// gather the witnesses for its specific storage trie
				if s.witness != nil && obj.trie != nil {
					s.witness.AddState(obj.trie.Witness())
				}
			}
			return nil
		})
	}
	// If witness building is enabled, gather all the read-only accesses.
	// Skip witness collection in Verkle mode, they will be gathered
	// together at the end.
	if s.witness != nil && !s.db.TrieDB().IsVerkle() {
		witStart := time.Now()
		// Pull in anything that has been accessed before destruction
		for _, obj := range s.stateObjectsDestruct {
			// Skip any objects that haven't touched their storage
			if len(obj.originStorage) == 0 {
				continue
			}
			s.addObjectWitness(obj)
		}
		// Pull in only-read and non-destructed trie witnesses
		for _, obj := range s.stateObjects {
			// Skip any objects that have been updated
			if _, ok := s.mutations[obj.address]; ok {
				continue
			}
			// Skip any objects that haven't touched their storage
			if len(obj.originStorage) == 0 {
				continue
			}
			s.addObjectWitness(obj)
		}
		s.WitnessCollection += time.Since(witStart)
	}
	workers.Wait()
	if !s.skipTimers {
		s.StorageUpdates += time.Since(start)
	}

	// Now we're about to start to write changes to the trie. The trie is so far
	// _untouched_. We can check with the prefetcher, if it can give us a trie
	// which has the same root, but also has some content loaded into it.
	//
	// Don't check prefetcher if verkle trie has been used. In the context of verkle,
	// only a single trie is used for state hashing. Replacing a non-nil verkle tree
	// here could result in losing uncommitted changes from storage.
	if !s.skipTimers {
		start = time.Now()
	}
	if s.prefetcher != nil {
		if trie := s.prefetcher.trie(common.Hash{}, s.originalRoot); trie == nil {
			log.Error("Failed to retrieve account pre-fetcher trie")
		} else {
			s.trie = trie
		}
	}
	// Perform updates before deletions.  This prevents resolution of unnecessary trie nodes
	// in circumstances similar to the following:
	//
	// Consider nodes `A` and `B` who share the same full node parent `P` and have no other siblings.
	// During the execution of a block:
	// - `A` self-destructs,
	// - `C` is created, and also shares the parent `P`.
	// If the self-destruct is handled first, then `P` would be left with only one child, thus collapsed
	// into a shortnode. This requires `B` to be resolved from disk.
	// Whereas if the created node is handled first, then the collapse is avoided, and `B` is not resolved.
	var (
		usedAddrs    []common.Address
		deletedAddrs []common.Address
	)
	for addr, op := range s.mutations {
		if op.applied {
			continue
		}
		op.applied = true

		if op.isDelete() {
			deletedAddrs = append(deletedAddrs, addr)
		} else {
			s.updateStateObject(s.stateObjects[addr])
			s.AccountUpdated += 1
		}
		usedAddrs = append(usedAddrs, addr) // Copy needed for closure
	}
	for _, deletedAddr := range deletedAddrs {
		s.deleteStateObject(deletedAddr)
		s.AccountDeleted += 1
	}
	if !s.skipTimers {
		s.AccountUpdates += time.Since(start)
	}

	if s.prefetcher != nil {
		s.prefetcher.used(common.Hash{}, s.originalRoot, usedAddrs, nil)
	}
	// When there is no prefetcher and witness building is enabled, account
	// reads went through the reader (a separate trie with its own
	// prevalueTracer), so s.trie lacks intermediate nodes for read-only
	// accounts. Walk them through s.trie now to capture proof-path nodes
	// that will be included in the witness.
	if s.witness != nil && s.prefetcher == nil && !s.db.TrieDB().IsVerkle() {
		for _, obj := range s.stateObjects {
			if _, ok := s.mutations[obj.address]; ok {
				continue
			}
			s.trie.GetAccount(obj.address)
		}
		// Walk proof-of-absence paths for non-existent accounts. Even
		// though these accounts don't exist, the trie traversal captures
		// intermediate nodes that stateless execution needs to verify
		// the accounts' non-existence.
		for addr := range s.nonExistentReads {
			s.trie.GetAccount(addr)
		}
	}
	// Track the amount of time wasted on hashing the account trie
	if !s.skipTimers {
		defer func(start time.Time) { s.AccountHashes += time.Since(start) }(time.Now())
	}

	hash := s.trie.Hash()

	// If witness building is enabled, gather the account trie witness
	if s.witness != nil {
		witStart := time.Now()
		witness := s.trie.Witness()
		s.witness.AddState(witness)
		if s.witnessStats != nil {
			s.witnessStats.Add(witness, common.Hash{})
		}
		s.WitnessCollection += time.Since(witStart)
	}
	return hash
}

// SetTxContext sets the current transaction hash and index which are
// used when the EVM emits new state logs. It should be invoked before
// transaction execution.
func (s *StateDB) SetTxContext(thash common.Hash, ti int) {
	s.thash = thash
	s.txIndex = ti
}

func (s *StateDB) clearJournalAndRefund() {
	s.journal.reset()
	s.refund = 0
}

// fastDeleteStorage is the function that efficiently deletes the storage trie
// of a specific account. It leverages the associated state snapshot for fast
// storage iteration and constructs trie node deletion markers by creating
// stack trie with iterated slots.
func (s *StateDB) fastDeleteStorage(snaps *snapshot.Tree, addrHash common.Hash, root common.Hash) (map[common.Hash][]byte, map[common.Hash][]byte, *trienode.NodeSet, error) {
	iter, err := snaps.StorageIterator(s.originalRoot, addrHash, common.Hash{})
	if err != nil {
		return nil, nil, nil, err
	}
	defer iter.Release()

	var (
		nodes          = trienode.NewNodeSet(addrHash) // the set for trie node mutations (value is nil)
		storages       = make(map[common.Hash][]byte)  // the set for storage mutations (value is nil)
		storageOrigins = make(map[common.Hash][]byte)  // the set for tracking the original value of slot
	)
	stack := trie.NewStackTrie(func(path []byte, hash common.Hash, blob []byte) {
		nodes.AddNode(path, trienode.NewDeletedWithPrev(blob))
	})
	for iter.Next() {
		slot := common.CopyBytes(iter.Slot())
		if err := iter.Error(); err != nil { // error might occur after Slot function
			return nil, nil, nil, err
		}
		key := iter.Hash()
		storages[key] = nil
		storageOrigins[key] = slot

		if err := stack.Update(key.Bytes(), slot); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := iter.Error(); err != nil { // error might occur during iteration
		return nil, nil, nil, err
	}
	if stack.Hash() != root {
		return nil, nil, nil, fmt.Errorf("snapshot is not matched, exp %x, got %x", root, stack.Hash())
	}
	return storages, storageOrigins, nodes, nil
}

// slowDeleteStorage serves as a less-efficient alternative to "fastDeleteStorage,"
// employed when the associated state snapshot is not available. It iterates the
// storage slots along with all internal trie nodes via trie directly.
func (s *StateDB) slowDeleteStorage(addr common.Address, addrHash common.Hash, root common.Hash) (map[common.Hash][]byte, map[common.Hash][]byte, *trienode.NodeSet, error) {
	tr, err := s.db.OpenStorageTrie(s.originalRoot, addr, root, s.trie)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open storage trie, err: %w", err)
	}
	it, err := tr.NodeIterator(nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open storage iterator, err: %w", err)
	}
	var (
		nodes          = trienode.NewNodeSet(addrHash) // the set for trie node mutations (value is nil)
		storages       = make(map[common.Hash][]byte)  // the set for storage mutations (value is nil)
		storageOrigins = make(map[common.Hash][]byte)  // the set for tracking the original value of slot
	)
	for it.Next(true) {
		if it.Leaf() {
			key := common.BytesToHash(it.LeafKey())
			storages[key] = nil
			storageOrigins[key] = common.CopyBytes(it.LeafBlob())
			continue
		}
		if it.Hash() == (common.Hash{}) {
			continue
		}
		nodes.AddNode(it.Path(), trienode.NewDeletedWithPrev(it.NodeBlob()))
	}
	if err := it.Error(); err != nil {
		return nil, nil, nil, err
	}
	return storages, storageOrigins, nodes, nil
}

// deleteStorage is designed to delete the storage trie of a designated account.
// The function will make an attempt to utilize an efficient strategy if the
// associated state snapshot is reachable; otherwise, it will resort to a less
// efficient approach.
func (s *StateDB) deleteStorage(addr common.Address, addrHash common.Hash, root common.Hash) (map[common.Hash][]byte, map[common.Hash][]byte, *trienode.NodeSet, error) {
	var (
		err            error
		nodes          *trienode.NodeSet      // the set for trie node mutations (value is nil)
		storages       map[common.Hash][]byte // the set for storage mutations (value is nil)
		storageOrigins map[common.Hash][]byte // the set for tracking the original value of slot
	)
	// The fast approach can be failed if the snapshot is not fully
	// generated, or it's internally corrupted. Fallback to the slow
	// one just in case.
	snaps := s.db.Snapshot()
	if snaps != nil {
		storages, storageOrigins, nodes, err = s.fastDeleteStorage(snaps, addrHash, root)
	}
	if snaps == nil || err != nil {
		storages, storageOrigins, nodes, err = s.slowDeleteStorage(addr, addrHash, root)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return storages, storageOrigins, nodes, nil
}

// handleDestruction processes all destruction markers and deletes the account
// and associated storage slots if necessary. There are four potential scenarios
// as following:
//
//	(a) the account was not existent and be marked as destructed
//	(b) the account was not existent and be marked as destructed,
//	    however, it's resurrected later in the same block.
//	(c) the account was existent and be marked as destructed
//	(d) the account was existent and be marked as destructed,
//	    however it's resurrected later in the same block.
//
// In case (a), nothing needs be deleted, nil to nil transition can be ignored.
// In case (b), nothing needs be deleted, nil is used as the original value for
// newly created account and storages
// In case (c), **original** account along with its storages should be deleted,
// with their values be tracked as original value.
// In case (d), **original** account along with its storages should be deleted,
// with their values be tracked as original value.
func (s *StateDB) handleDestruction(noStorageWiping bool) (map[common.Hash]*accountDelete, []*trienode.NodeSet, error) {
	var (
		nodes   []*trienode.NodeSet
		deletes = make(map[common.Hash]*accountDelete)
	)
	for addr, prevObj := range s.stateObjectsDestruct {
		prev := prevObj.origin

		// The account was non-existent, and it's marked as destructed in the scope
		// of block. It can be either case (a) or (b) and will be interpreted as
		// null->null state transition.
		// - for (a), skip it without doing anything
		// - for (b), the resurrected account with nil as original will be handled afterwards
		if prev == nil {
			continue
		}
		// The account was existent, it can be either case (c) or (d).
		addrHash := crypto.Keccak256Hash(addr.Bytes())
		op := &accountDelete{
			address: addr,
			origin:  types.SlimAccountRLP(*prev),
		}
		deletes[addrHash] = op

		// Short circuit if the origin storage was empty.
		if prev.Root == types.EmptyRootHash || s.db.TrieDB().IsVerkle() {
			continue
		}
		if noStorageWiping {
			return nil, nil, fmt.Errorf("unexpected storage wiping, %x", addr)
		}
		// Remove storage slots belonging to the account.
		storages, storagesOrigin, set, err := s.deleteStorage(addr, addrHash, prev.Root)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to delete storage, err: %w", err)
		}
		op.storages = storages
		op.storagesOrigin = storagesOrigin

		// Aggregate the associated trie node changes.
		nodes = append(nodes, set)
	}
	return deletes, nodes, nil
}

// GetTrie returns the account trie.
func (s *StateDB) GetTrie() Trie {
	return s.trie
}

// commit gathers the state mutations accumulated along with the associated
// trie changes, resetting all internal flags with the new state as the base.
func (s *StateDB) commit(deleteEmptyObjects bool, noStorageWiping bool, blockNumber uint64) (*stateUpdate, error) {
	// Short circuit in case any database failure occurred earlier.
	if s.dbErr != nil {
		return nil, fmt.Errorf("commit aborted due to earlier error: %v", s.dbErr)
	}
	// Finalize any pending changes and merge everything into the tries
	s.IntermediateRoot(deleteEmptyObjects)

	// Short circuit if any error occurs within the IntermediateRoot.
	if s.dbErr != nil {
		return nil, fmt.Errorf("commit aborted due to database error: %v", s.dbErr)
	}
	// Commit objects to the trie, measuring the elapsed time
	var (
		accountTrieNodesUpdated int
		accountTrieNodesDeleted int
		storageTrieNodesUpdated int
		storageTrieNodesDeleted int

		lock    sync.Mutex                                               // protect two maps below
		nodes   = trienode.NewMergedNodeSet()                            // aggregated trie nodes
		updates = make(map[common.Hash]*accountUpdate, len(s.mutations)) // aggregated account updates

		// merge aggregates the dirty trie nodes into the global set.
		//
		// Given that some accounts may be destroyed and then recreated within
		// the same block, it's possible that a node set with the same owner
		// may already exist. In such cases, these two sets are combined, with
		// the later one overwriting the previous one if any nodes are modified
		// or deleted in both sets.
		//
		// merge run concurrently across  all the state objects and account trie.
		merge = func(set *trienode.NodeSet) error {
			if set == nil {
				return nil
			}
			lock.Lock()
			defer lock.Unlock()

			updates, deletes := set.Size()
			if set.Owner == (common.Hash{}) {
				accountTrieNodesUpdated += updates
				accountTrieNodesDeleted += deletes
			} else {
				storageTrieNodesUpdated += updates
				storageTrieNodesDeleted += deletes
			}
			return nodes.Merge(set)
		}
	)
	// Given that some accounts could be destroyed and then recreated within
	// the same block, account deletions must be processed first. This ensures
	// that the storage trie nodes deleted during destruction and recreated
	// during subsequent resurrection can be combined correctly.
	deletes, delNodes, err := s.handleDestruction(noStorageWiping)
	if err != nil {
		return nil, err
	}
	for _, set := range delNodes {
		if err := merge(set); err != nil {
			return nil, err
		}
	}
	// Handle all state updates afterwards, concurrently to one another to shave
	// off some milliseconds from the commit operation. Also accumulate the code
	// writes to run in parallel with the computations.
	var (
		start   = time.Now()
		root    common.Hash
		workers errgroup.Group
	)
	// Schedule the account trie first since that will be the biggest, so give
	// it the most time to crunch.
	//
	// TODO(karalabe): This account trie commit is *very* heavy. 5-6ms at chain
	// heads, which seems excessive given that it doesn't do hashing, it just
	// shuffles some data. For comparison, the *hashing* at chain head is 2-3ms.
	// We need to investigate what's happening as it seems something's wonky.
	// Obviously it's not an end of the world issue, just something the original
	// code didn't anticipate for.
	workers.Go(func() error {
		// Write the account trie changes, measuring the amount of wasted time
		newroot, set := s.trie.Commit(true)
		root = newroot

		if err := merge(set); err != nil {
			return err
		}
		s.AccountCommits = time.Since(start)
		return nil
	})
	// Schedule each of the storage tries that need to be updated, so they can
	// run concurrently to one another.
	//
	// TODO(karalabe): Experimentally, the account commit takes approximately the
	// same time as all the storage commits combined, so we could maybe only have
	// 2 threads in total. But that kind of depends on the account commit being
	// more expensive than it should be, so let's fix that and revisit this todo.
	for addr, op := range s.mutations {
		if op.isDelete() {
			continue
		}
		// Write any contract code associated with the state object
		obj := s.stateObjects[addr]
		if obj == nil {
			return nil, errors.New("missing state object")
		}
		// Run the storage updates concurrently to one another
		workers.Go(func() error {
			// Write any storage changes in the state object to its storage trie
			update, set, err := obj.commit()
			if err != nil {
				return err
			}
			if err := merge(set); err != nil {
				return err
			}
			lock.Lock()
			updates[obj.addrHash()] = update
			s.StorageCommits = time.Since(start) // overwrite with the longest storage commit runtime
			lock.Unlock()
			return nil
		})
	}
	// Wait for everything to finish and update the metrics
	if err := workers.Wait(); err != nil {
		return nil, err
	}
	accountReadMeters.Mark(int64(s.AccountLoaded))
	storageReadMeters.Mark(int64(s.StorageLoaded))
	accountUpdatedMeter.Mark(int64(s.AccountUpdated))
	storageUpdatedMeter.Mark(s.StorageUpdated.Load())
	accountDeletedMeter.Mark(int64(s.AccountDeleted))
	storageDeletedMeter.Mark(s.StorageDeleted.Load())
	accountTrieUpdatedMeter.Mark(int64(accountTrieNodesUpdated))
	accountTrieDeletedMeter.Mark(int64(accountTrieNodesDeleted))
	storageTriesUpdatedMeter.Mark(int64(storageTrieNodesUpdated))
	storageTriesDeletedMeter.Mark(int64(storageTrieNodesDeleted))

	// Clear the metric markers
	s.AccountLoaded, s.AccountUpdated, s.AccountDeleted = 0, 0, 0
	s.StorageLoaded = 0
	s.StorageUpdated.Store(0)
	s.StorageDeleted.Store(0)

	// Clear all internal flags and update state root at the end.
	s.mutations = make(map[common.Address]*mutation)
	s.stateObjectsDestruct = make(map[common.Address]*stateObject)
	s.currentBlockDestructs = make(map[common.Address]struct{})

	origin := s.originalRoot
	s.originalRoot = root

	return newStateUpdate(noStorageWiping, origin, root, blockNumber, deletes, updates, nodes), nil
}

// commitAndFlush is a wrapper of commit which also commits the state mutations
// to the configured data stores.
func (s *StateDB) commitAndFlush(block uint64, deleteEmptyObjects bool, noStorageWiping bool) (*stateUpdate, error) {
	ret, err := s.commit(deleteEmptyObjects, noStorageWiping, block)
	if err != nil {
		return nil, err
	}
	// Commit dirty contract code if any exists
	if db := s.db.TrieDB().Disk(); db != nil && len(ret.codes) > 0 {
		batch := db.NewBatch()
		for _, code := range ret.codes {
			rawdb.WriteCode(batch, code.hash, code.blob)
		}
		if err := batch.Write(); err != nil {
			return nil, err
		}
	}
	if !ret.empty() {
		// If snapshotting is enabled, update the snapshot tree with this new version
		if snap := s.db.Snapshot(); snap != nil && snap.Snapshot(ret.originRoot) != nil {
			start := time.Now()
			if err := snap.Update(ret.root, ret.originRoot, ret.accounts, ret.storages); err != nil {
				log.Warn("Failed to update snapshot tree", "from", ret.originRoot, "to", ret.root, "err", err)
			}
			// Keep 128 diff layers in the memory, persistent layer is 129th.
			// - head layer is paired with HEAD state
			// - head-1 layer is paired with HEAD-1 state
			// - head-127 layer(bottom-most diff layer) is paired with HEAD-127 state
			if err := snap.Cap(ret.root, TriesInMemory); err != nil {
				log.Warn("Failed to cap snapshot tree", "root", ret.root, "layers", TriesInMemory, "err", err)
			}
			s.SnapshotCommits += time.Since(start)
		}
		// If trie database is enabled, commit the state update as a new layer
		if db := s.db.TrieDB(); db != nil {
			start := time.Now()
			if err := db.Update(ret.root, ret.originRoot, block, ret.nodes, ret.stateSet()); err != nil {
				return nil, err
			}
			s.TrieDBCommits += time.Since(start)
		}
	}
	s.reader, _ = s.db.Reader(s.originalRoot)
	return ret, err
}

// Commit writes the state mutations into the configured data stores.
//
// Once the state is committed, tries cached in stateDB (including account
// trie, storage tries) will no longer be functional. A new state instance
// must be created with new root and updated database for accessing post-
// commit states.
//
// The associated block number of the state transition is also provided
// for more chain context.
//
// noStorageWiping is a flag indicating whether storage wiping is permitted.
// Since self-destruction was deprecated with the Cancun fork and there are
// no empty accounts left that could be deleted by EIP-158, storage wiping
// should not occur.
func (s *StateDB) Commit(block uint64, deleteEmptyObjects bool, noStorageWiping bool) (common.Hash, error) {
	ret, err := s.commitAndFlush(block, deleteEmptyObjects, noStorageWiping)
	if err != nil {
		return common.Hash{}, err
	}
	return ret.root, nil
}

// CommitWithUpdate writes the state mutations and returns both the root hash and the state update.
// This is useful for tracking state changes at the blockchain level.
func (s *StateDB) CommitWithUpdate(block uint64, deleteEmptyObjects bool, noStorageWiping bool) (common.Hash, *stateUpdate, error) {
	ret, err := s.commitAndFlush(block, deleteEmptyObjects, noStorageWiping)
	if err != nil {
		return common.Hash{}, nil, err
	}
	return ret.root, ret, nil
}

// FlatDiff is a flat snapshot of all account and storage mutations from one
// block's execution. It is extracted cheaply (~1ms) via CommitSnapshot without
// requiring MPT hashing. A goroutine then applies the FlatDiff to a fresh
// StateDB to compute the actual state root concurrently with the next block.
type FlatDiff struct {
	Accounts  map[common.Address]types.StateAccount          // post-state of each modified account
	Storage   map[common.Address]map[common.Hash]common.Hash // post-state storage slots
	Destructs map[common.Address]struct{}                    // self-destructed accounts
	Code      map[common.Hash][]byte                         // newly deployed code

	// ReadSet and ReadStorage list accounts and storage slots that were read
	// (but not mutated) during block execution. The pipelined SRC goroutine loads
	// these from the root_{N-1} trie so their MPT proof nodes are captured in
	// the witness for stateless execution.
	ReadSet     []common.Address
	ReadStorage map[common.Address][]common.Hash

	// NonExistentReads lists addresses that were looked up during execution
	// but don't exist in the state trie. The SRC goroutine walks these paths
	// to capture proof-of-absence trie nodes for the witness, enabling
	// stateless execution to verify these accounts don't exist.
	NonExistentReads []common.Address

	// trackOverlayReads enables recording of overlay-served reads while this
	// FlatDiff acts as the next block's read overlay. Overlay hits never
	// reach the shared reader, and under BlockSTM v2 they may materialize
	// only on discarded pool copies — this tracker is the only place they
	// are reliably observed. Set by CommitSnapshot when a witness is being
	// produced; left off otherwise so witness-off imports pay nothing.
	trackOverlayReads bool

	// overlayAccountReads / overlayStorageReads accumulate the keys the NEXT
	// block read from this overlay (concurrent: workers and finalDB record
	// into the shared FlatDiff). Keys: common.Address / stateKey.
	overlayAccountReads sync.Map
	overlayStorageReads sync.Map
}

// recordOverlayAccountRead notes an overlay-served account read. Load-first
// keeps the hit path to one lock-free map probe after the first record.
func (diff *FlatDiff) recordOverlayAccountRead(addr common.Address) {
	if !diff.trackOverlayReads {
		return
	}
	if _, ok := diff.overlayAccountReads.Load(addr); !ok {
		diff.overlayAccountReads.Store(addr, struct{}{})
	}
}

// recordOverlayStorageRead notes an overlay-served storage read.
func (diff *FlatDiff) recordOverlayStorageRead(addr common.Address, slot common.Hash) {
	if !diff.trackOverlayReads {
		return
	}
	key := stateKey{addr: addr, slot: slot}
	if _, ok := diff.overlayStorageReads.Load(key); !ok {
		diff.overlayStorageReads.Store(key, struct{}{})
	}
}

// collectOverlayReads hands every recorded overlay-served read to the
// callbacks. Account existence is resolved against this FlatDiff itself:
// an overlay-covered account either exists in Accounts (written or
// resurrected by the overlay's block) or was destructed by it.
func (diff *FlatDiff) collectOverlayReads(onAccount func(common.Address, bool), onStorage func(common.Address, common.Hash)) {
	diff.overlayAccountReads.Range(func(k, _ any) bool {
		addr := k.(common.Address)
		_, exists := diff.Accounts[addr]
		onAccount(addr, exists)
		return true
	})
	diff.overlayStorageReads.Range(func(k, _ any) bool {
		key := k.(stateKey)
		onStorage(key.addr, key.slot)
		return true
	})
}

// storageOverlay returns a FlatDiff-covered storage value. A destructed
// account covers every slot: rewritten slots come from Storage, all other
// pre-destruction slots read as zero. The third return value is true when
// the value came from an explicit Storage entry rather than the destruct mask.
func (diff *FlatDiff) storageOverlay(addr common.Address, key common.Hash) (common.Hash, bool, bool) {
	if diff == nil {
		return common.Hash{}, false, false
	}
	if slots, ok := diff.Storage[addr]; ok {
		if value, ok := slots[key]; ok {
			diff.recordOverlayStorageRead(addr, key)
			return value, true, true
		}
	}
	if _, destructed := diff.Destructs[addr]; destructed {
		diff.recordOverlayStorageRead(addr, key)
		return common.Hash{}, true, false
	}
	return common.Hash{}, false, false
}

// accountOverlay returns FlatDiff-covered account data. Accounts wins over
// Destructs to represent destruct-and-resurrect in the same parent block.
func (diff *FlatDiff) accountOverlay(addr common.Address) (types.StateAccount, bool, bool) {
	if diff == nil {
		return types.StateAccount{}, false, false
	}
	if acct, ok := diff.Accounts[addr]; ok {
		diff.recordOverlayAccountRead(addr)
		return acct, true, true
	}
	if _, destructed := diff.Destructs[addr]; destructed {
		diff.recordOverlayAccountRead(addr)
		return types.StateAccount{}, false, true
	}
	return types.StateAccount{}, false, false
}

// TouchAllAddresses performs read-only accesses on dst for every address and
// storage slot recorded in the FlatDiff. This ensures dst tracks these
// addresses in its stateObjects so they later appear in dst's own FlatDiff
// (via CommitSnapshot). Unlike ApplyFlatDiff, it does NOT overwrite any
// account data — it only forces dst to load the accounts from its own trie.
func (diff *FlatDiff) TouchAllAddresses(dst *StateDB) {
	for addr := range diff.Accounts {
		touchAddressAndStorage(dst, addr, diff.mutatedStorageKeys(addr))
	}
	for _, addr := range diff.ReadSet {
		touchAddressAndStorage(dst, addr, diff.ReadStorage[addr])
	}
	for addr := range diff.Destructs {
		dst.GetBalance(addr)
	}
	// Touch non-existent addresses so dst tracks them (via its own
	// nonExistentReads) and the SRC goroutine can capture their
	// proof-of-absence trie nodes for the witness.
	for _, addr := range diff.NonExistentReads {
		dst.GetBalance(addr)
	}
}

// touchAddressAndStorage calls GetBalance on addr and GetCommittedState on
// each provided slot so the destination statedb tracks the reads (and the
// background SRC walks those trie nodes for the witness).
func touchAddressAndStorage(dst *StateDB, addr common.Address, slots []common.Hash) {
	dst.GetBalance(addr)
	for _, slot := range slots {
		dst.GetCommittedState(addr, slot)
	}
}

// mutatedStorageKeys returns the keys of diff.Storage[addr] as a slice so
// TouchAllAddresses can route both mutated and read-only accounts through
// touchAddressAndStorage without branching on map vs slice.
func (diff *FlatDiff) mutatedStorageKeys(addr common.Address) []common.Hash {
	slots, ok := diff.Storage[addr]
	if !ok {
		return nil
	}
	keys := make([]common.Hash, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	return keys
}

// CommitSnapshot finalises the StateDB and returns a FlatDiff capturing all
// mutations without performing any MPT hashing (~1ms). After this call the
// StateDB should no longer be used by the caller.
func (s *StateDB) CommitSnapshot(deleteEmptyObjects bool) *FlatDiff {
	s.Finalise(deleteEmptyObjects)

	diff := &FlatDiff{
		Accounts:    make(map[common.Address]types.StateAccount),
		Storage:     make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:   make(map[common.Address]struct{}),
		Code:        make(map[common.Hash][]byte),
		ReadStorage: make(map[common.Address][]common.Hash),
	}
	for addr := range s.stateObjectsDestruct {
		diff.Destructs[addr] = struct{}{}
	}
	for addr, op := range s.mutations {
		s.captureMutation(diff, addr, op)
	}
	// Read-only accounts: accessed during execution but not mutated. The
	// pipelined SRC goroutine loads their root_{N-1} trie nodes into the
	// witness so stateless nodes can execute against root_{N-1}.
	for addr, obj := range s.stateObjects {
		s.captureReadOnlyAccount(diff, addr, obj)
	}
	// Non-existent account reads: looked-up addresses that don't exist in
	// the state trie. The SRC goroutine needs these to walk proof-of-absence
	// paths and capture trie nodes for the witness.
	for addr := range s.nonExistentReads {
		s.captureNonExistentRead(diff, addr)
	}
	// The stateObjects walk above only sees reads that materialized on THIS
	// statedb. Under BlockSTM v2 worker reads live on discarded pool copies,
	// and reads served by the parent FlatDiff overlay never reach the shared
	// reader at all — both leave the read-set short, the SRC preload re-reads
	// too little at the parent root, and the completed witness lacks
	// current-generation proof nodes for the missing keys. Drain the two
	// shared, attribution-free read records instead. Only witness production
	// consumes the read surface, so witness-off imports skip the cost.
	if s.witness != nil {
		s.drainExternalReadsIntoDiff(diff)
		diff.trackOverlayReads = true
	}
	return diff
}

// drainExternalReadsIntoDiff merges read keys observed outside this StateDB's
// stateObjects into the diff's read surface: the shared reader cache (every
// read that reached the reader, from any worker or statedb) and the parent
// FlatDiff's overlay-read tracker (reads the overlay served without touching
// the reader). Keys already covered by the diff's mutations or existing read
// lists are skipped. This may include speculative prefetcher reads — a
// superset only ever costs witness size, never correctness.
func (s *StateDB) drainExternalReadsIntoDiff(diff *FlatDiff) {
	seenAccounts := make(map[common.Address]struct{}, len(diff.ReadSet)+len(diff.NonExistentReads))
	for _, addr := range diff.ReadSet {
		seenAccounts[addr] = struct{}{}
	}
	for _, addr := range diff.NonExistentReads {
		seenAccounts[addr] = struct{}{}
	}
	seenSlots := make(map[stateKey]struct{})
	for addr, slots := range diff.ReadStorage {
		for _, slot := range slots {
			seenSlots[stateKey{addr: addr, slot: slot}] = struct{}{}
		}
	}
	onAccount := func(addr common.Address, exists bool) {
		if _, ok := seenAccounts[addr]; ok {
			return
		}
		// Mutated and destructed accounts get their trie paths walked by
		// ApplyFlatDiffForCommit/CommitWithUpdate and the destruct preload.
		if _, ok := diff.Accounts[addr]; ok {
			return
		}
		if _, ok := diff.Destructs[addr]; ok {
			return
		}
		seenAccounts[addr] = struct{}{}
		if exists {
			diff.ReadSet = append(diff.ReadSet, addr)
		} else {
			diff.NonExistentReads = append(diff.NonExistentReads, addr)
		}
	}
	onStorage := func(addr common.Address, slot common.Hash) {
		key := stateKey{addr: addr, slot: slot}
		if _, ok := seenSlots[key]; ok {
			return
		}
		if slots, ok := diff.Storage[addr]; ok {
			if _, written := slots[slot]; written {
				return
			}
		}
		seenSlots[key] = struct{}{}
		diff.ReadStorage[addr] = append(diff.ReadStorage[addr], slot)
	}
	if rwc := findReaderWithCache(s.reader); rwc != nil {
		rwc.CollectReadSet(onAccount, onStorage)
	}
	if s.flatDiffRef != nil {
		s.flatDiffRef.collectOverlayReads(onAccount, onStorage)
	}
}

// captureMutation records a single mutated/destructed account into the
// FlatDiff. Destructs take both the explicit delete path and the pending
// Destructs set; live mutations copy account data, dirty code, and both
// pending and read-only storage so the SRC goroutine can later walk every
// trie node the block touched.
func (s *StateDB) captureMutation(diff *FlatDiff, addr common.Address, op *mutation) {
	if op.isDelete() {
		diff.Destructs[addr] = struct{}{}
		return
	}
	obj, ok := s.stateObjects[addr]
	if !ok {
		return
	}
	diff.Accounts[addr] = obj.data
	if obj.dirtyCode {
		diff.Code[common.BytesToHash(obj.CodeHash())] = obj.code
	}
	captureObjectStorage(diff, addr, obj)
}

// captureObjectStorage copies pending (post-Finalise) storage mutations and
// any read-only slots that weren't overwritten. Read-only slots matter
// because the SRC goroutine needs to load their trie nodes into the witness
// (e.g., span commits read validator-contract slots they don't write).
func captureObjectStorage(diff *FlatDiff, addr common.Address, obj *stateObject) {
	if len(obj.pendingStorage) > 0 {
		slots := make(map[common.Hash]common.Hash, len(obj.pendingStorage))
		for k, v := range obj.pendingStorage {
			slots[k] = v
		}
		diff.Storage[addr] = slots
	}
	if len(obj.originStorage) == 0 {
		return
	}
	var readSlots []common.Hash
	for slot := range obj.originStorage {
		if _, dirty := obj.pendingStorage[slot]; !dirty {
			readSlots = append(readSlots, slot)
		}
	}
	if len(readSlots) > 0 {
		diff.ReadStorage[addr] = readSlots
	}
}

// captureReadOnlyAccount adds an account to ReadSet (and its originStorage
// to ReadStorage) if it was accessed but neither mutated nor destructed in
// this block. Mutated/destructed accounts are already handled by
// captureMutation.
func (s *StateDB) captureReadOnlyAccount(diff *FlatDiff, addr common.Address, obj *stateObject) {
	if _, isMutation := s.mutations[addr]; isMutation {
		return
	}
	if _, isDestruct := s.stateObjectsDestruct[addr]; isDestruct {
		return
	}
	diff.ReadSet = append(diff.ReadSet, addr)
	if len(obj.originStorage) == 0 {
		return
	}
	slots := make([]common.Hash, 0, len(obj.originStorage))
	for slot := range obj.originStorage {
		slots = append(slots, slot)
	}
	diff.ReadStorage[addr] = slots
}

// captureNonExistentRead records proof-of-absence address reads. Skips
// addresses that ended up existing (e.g., created later in the block) since
// captureMutation/captureReadOnlyAccount already handled them.
func (s *StateDB) captureNonExistentRead(diff *FlatDiff, addr common.Address) {
	if _, isMutation := s.mutations[addr]; isMutation {
		return
	}
	if _, ok := s.stateObjects[addr]; ok {
		return
	}
	diff.NonExistentReads = append(diff.NonExistentReads, addr)
}

// ApplyFlatDiff installs the previous block's mutations as pre-loaded (but not
// dirty) state objects, giving the current block immediate read access to the
// previous block's post-state without waiting for the background goroutine to
// commit the trie.
//
// Accounts are inserted directly into s.stateObjects — bypassing the journal —
// so Finalise/CommitSnapshot only captures accounts the CURRENT block actually
// modifies. Without this, every account touched in block N would be re-captured
// in block N+1's FlatDiff and cascade indefinitely.
//
// Newly deployed contract code (dirtyCode in the previous block) is carried
// in-memory because the background goroutine may not have written it to the
// key-value store yet.
func (s *StateDB) ApplyFlatDiff(diff *FlatDiff) {
	// Register self-destructed accounts so getStateObject returns nil for them,
	// preventing a stale trie read while the background goroutine's deletion
	// has not yet been committed.
	for addr := range diff.Destructs {
		if _, already := s.stateObjectsDestruct[addr]; !already {
			s.stateObjectsDestruct[addr] = newObject(s, addr, nil)
		}
	}
	for addr, acct := range diff.Accounts {
		s.applyFlatAccountOverlay(diff, addr, acct)
	}
}

// applyFlatAccountOverlay installs a FlatDiff account into stateObjects as a
// read-only overlay: no journal entries, no dirty bits. Newly-deployed code
// is carried in memory because the background goroutine may not have
// persisted it yet; pre-existing contracts resolve via stateObject.Code().
// Pending storage from the previous block is loaded as originStorage so
// CommitSnapshot only re-captures slots that THIS block writes.
func (s *StateDB) applyFlatAccountOverlay(diff *FlatDiff, addr common.Address, acct types.StateAccount) {
	acctCopy := acct
	obj := newObject(s, addr, &acctCopy)
	if code, ok := diff.Code[common.BytesToHash(acctCopy.CodeHash)]; ok {
		obj.code = code
		// dirtyCode intentionally left false: code was deployed in the
		// previous block, not this one.
	}
	if slots, ok := diff.Storage[addr]; ok {
		for k, v := range slots {
			obj.originStorage[k] = v
		}
	}
	s.stateObjects[addr] = obj
}

// ApplyFlatDiffForCommit marks all mutations in diff as dirty via the normal
// Set* mutation path, so that a subsequent CommitWithUpdate produces the
// correct state root for the block. Unlike ApplyFlatDiff, which installs
// accounts as a read-only overlay, this method ensures every change is
// journalled so Finalise and commit pick them up.
//
// Use this only in the background goroutine that computes a block's actual
// state root; it is not suitable for execution state objects (it would cause
// mutations to cascade into subsequent FlatDiffs).
func (s *StateDB) ApplyFlatDiffForCommit(diff *FlatDiff) {
	// Handle self-destructs. Pure destructs (not resurrected) go through
	// SelfDestruct, which loads the original from the trie and marks it for
	// deletion. Resurrected accounts (present in both Destructs and Accounts)
	// are set up inside applyFlatMutation so the subsequent Set* calls create
	// a fresh object via getOrNewStateObject.
	for addr := range diff.Destructs {
		if _, resurrected := diff.Accounts[addr]; resurrected {
			continue
		}
		s.SelfDestruct(addr)
	}
	for addr, acct := range diff.Accounts {
		s.applyFlatMutation(diff, addr, acct)
	}
}

// ApplyFlatDiffForCommitFast marks all mutations in diff as dirty without
// constructing journal revert entries. It is intended for the witness-off SRC
// goroutine only: the FlatDiff replay is irrevocable, so Snapshot/RevertToSnapshot
// support would only add allocation and setter overhead before CommitWithUpdate.
//
// Witness-producing SRC still uses ApplyFlatDiffForCommit because the normal
// setters/read path is the conservative path for collecting every proof node.
func (s *StateDB) ApplyFlatDiffForCommitFast(diff *FlatDiff) {
	for addr := range diff.Destructs {
		if _, resurrected := diff.Accounts[addr]; resurrected {
			continue
		}
		s.applyFlatPureDestructFast(addr)
	}
	for addr, acct := range diff.Accounts {
		s.applyFlatMutationFast(diff, addr, acct)
	}
}

// applyFlatMutation commits one FlatDiff account mutation onto the statedb
// via the journalled Set* path so Finalise / commit pick it up. Handles
// resurrection by seeding stateObjectsDestruct with the pre-block original
// (needed by handleDestruction to delete the original storage trie).
func (s *StateDB) applyFlatMutation(diff *FlatDiff, addr common.Address, acct types.StateAccount) {
	if _, destructed := diff.Destructs[addr]; destructed {
		if _, already := s.stateObjectsDestruct[addr]; !already {
			if prev := s.getStateObject(addr); prev != nil {
				s.stateObjectsDestruct[addr] = prev
			}
		}
		delete(s.stateObjects, addr)
	}
	if code, ok := diff.Code[common.BytesToHash(acct.CodeHash)]; ok {
		s.SetCode(addr, code, tracing.CodeChangeUnspecified)
	}
	// SetState reads the pre-block origin from the storage trie, populating
	// uncommittedStorage so updateTrie correctly writes or deletes each slot
	// (including zero-value deletions).
	if slots, ok := diff.Storage[addr]; ok {
		for k, v := range slots {
			s.SetState(addr, k, v)
		}
	}
	// Set* ensures the account appears in journal.dirties so Finalise emits
	// a markUpdate, even when only storage or code changed.
	s.SetNonce(addr, acct.Nonce, tracing.NonceChangeUnspecified)
	s.SetBalance(addr, acct.Balance, tracing.BalanceChangeUnspecified)
}

func (s *StateDB) applyFlatPureDestructFast(addr common.Address) {
	obj := s.getStateObject(addr)
	if obj == nil {
		return
	}
	if !obj.Balance().IsZero() {
		obj.setBalance(new(uint256.Int))
	}
	obj.markSelfdestructed()
	s.journal.dirty(addr)
}

// resolveFlatMutationObject returns the state object a flat mutation for addr
// applies to, recreating the object from scratch when the diff destructed the
// account (preserving the pre-destruct object for witness collection).
func (s *StateDB) resolveFlatMutationObject(diff *FlatDiff, addr common.Address) *stateObject {
	if _, destructed := diff.Destructs[addr]; destructed {
		if _, already := s.stateObjectsDestruct[addr]; !already {
			if prev := s.getStateObject(addr); prev != nil {
				s.stateObjectsDestruct[addr] = prev
			}
		}
		delete(s.stateObjects, addr)
		delete(s.nonExistentReads, addr)
		obj := newObject(s, addr, nil)
		s.setStateObject(obj)
		return obj
	}
	obj := s.getStateObject(addr)
	if obj == nil {
		delete(s.nonExistentReads, addr)
		obj = newObject(s, addr, nil)
		s.setStateObject(obj)
	}
	return obj
}

func (s *StateDB) applyFlatMutationFast(diff *FlatDiff, addr common.Address, acct types.StateAccount) {
	obj := s.resolveFlatMutationObject(diff, addr)
	if obj == nil {
		return
	}
	obj.data.Nonce = acct.Nonce
	if acct.Balance != nil {
		obj.data.Balance = new(uint256.Int).Set(acct.Balance)
	} else {
		obj.data.Balance = new(uint256.Int)
	}
	codeHash := common.BytesToHash(acct.CodeHash)
	if code, ok := diff.Code[codeHash]; ok {
		obj.setCode(codeHash, code)
	}
	if slots, ok := diff.Storage[addr]; ok {
		for key, value := range slots {
			obj.dirtyStorage[key] = value
		}
	}
	s.journal.dirty(addr)
}

// NewWithFlatBase creates a StateDB at parentCommittedRoot (the last root
// committed to the trie database) with a FlatDiff overlay so that reads see
// the post-state of the block that produced flatDiff, without waiting for
// that block's state root to be computed.
//
// This is used during pipelined SRC: while a background goroutine computes
// root_N from (root_{N-1}, FlatDiff_N), the next block N+1 can already be
// executed using NewWithFlatBase(root_{N-1}, db, FlatDiff_N).
func NewWithFlatBase(parentCommittedRoot common.Hash, db Database, flatDiff *FlatDiff) (*StateDB, error) {
	sdb, err := New(parentCommittedRoot, db)
	if err != nil {
		return nil, err
	}
	if flatDiff != nil {
		sdb.flatDiffRef = flatDiff
	}
	return sdb, nil
}

// SetFlatDiffRef sets the read-only FlatDiff reference for lazy lookups.
func (s *StateDB) SetFlatDiffRef(diff *FlatDiff) {
	s.flatDiffRef = diff
}

// WasStorageSlotRead returns true if the given address+slot was accessed
// (read) during this block's execution. Used by pipelined SRC to detect
// whether any transaction read the EIP-2935 history storage slot that
// contains stale data during speculative execution.
func (s *StateDB) WasStorageSlotRead(addr common.Address, slot common.Hash) bool {
	obj, exists := s.stateObjects[addr]
	if !exists {
		return false
	}
	_, accessed := obj.originStorage[slot]
	return accessed
}

// Prepare handles the preparatory steps for executing a state transition with.
// This method must be invoked before state transition.
//
// Berlin fork:
// - Add sender to access list (2929)
// - Add destination to access list (2929)
// - Add precompiles to access list (2929)
// - Add the contents of the optional tx access list (2930)
//
// Potential EIPs:
// - Reset access list (Berlin)
// - Add coinbase to access list (EIP-3651)
// - Reset transient storage (EIP-1153)
func (s *StateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList) {
	if rules.IsEIP2929 && rules.IsEIP4762 {
		panic("eip2929 and eip4762 are both activated")
	}
	if rules.IsEIP2929 {
		// Clear out any leftover from previous executions
		al := newAccessList()
		s.accessList = al

		al.AddAddress(sender)

		if dst != nil {
			// If it's a create-tx, the destination will be added inside evm.create
			al.AddAddress(*dst)
		}

		for _, addr := range precompiles {
			al.AddAddress(addr)
		}

		for _, el := range list {
			al.AddAddress(el.Address)

			for _, key := range el.StorageKeys {
				al.AddSlot(el.Address, key)
			}
		}
		if rules.IsShanghai { // EIP-3651: warm coinbase
			al.AddAddress(coinbase)
		}
	}
	// Reset transient storage at the beginning of transaction execution
	s.transientStorage = newTransientStorage()
}

// AddAddressToAccessList adds the given address to the access list
func (s *StateDB) AddAddressToAccessList(addr common.Address) {
	if s.accessList.AddAddress(addr) {
		s.journal.accessListAddAccount(addr)
	}
}

// AddSlotToAccessList adds the given (address, slot)-tuple to the access list
func (s *StateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	addrMod, slotMod := s.accessList.AddSlot(addr, slot)
	if addrMod {
		// In practice, this should not happen, since there is no way to enter the
		// scope of 'address' without having the 'address' become already added
		// to the access list (via call-variant, create, etc).
		// Better safe than sorry, though
		s.journal.accessListAddAccount(addr)
	}

	if slotMod {
		s.journal.accessListAddSlot(addr, slot)
	}
}

// AddressInAccessList returns true if the given address is in the access list.
func (s *StateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.ContainsAddress(addr)
}

// SlotInAccessList returns true if the given (address, slot)-tuple is in the access list.
func (s *StateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressPresent bool, slotPresent bool) {
	return s.accessList.Contains(addr, slot)
}

func (s *StateDB) ValidateKnownAccounts(knownAccounts types.KnownAccounts) error {
	if knownAccounts == nil {
		return nil
	}

	for k, v := range knownAccounts {
		// check if the value is hex string or an object
		switch {
		case v.IsSingle():
			trie, _ := s.StorageTrie(k)
			if trie != nil {
				actualRootHash := trie.Hash()
				if *v.Single != actualRootHash {
					return fmt.Errorf("invalid root hash for: %v root hash: %v actual root hash: %v", k, v.Single, actualRootHash)
				}
			} else {
				return fmt.Errorf("Storage Trie is nil for: %v", k)
			}
		case v.IsStorage():
			for slot, value := range v.Storage {
				actualValue := s.GetState(k, slot)
				if value != actualValue {
					return fmt.Errorf("invalid slot value at address: %v slot: %v value: %v actual value: %v", k, slot, value, actualValue)
				}
			}
		default:
			return fmt.Errorf("impossible to validate known accounts: %v", k)
		}
	}

	return nil
}

// markDelete is invoked when an account is deleted but the deletion is
// not yet committed. The pending mutation is cached and will be applied
// all together
func (s *StateDB) markDelete(addr common.Address) {
	if _, ok := s.mutations[addr]; !ok {
		s.mutations[addr] = &mutation{}
	}
	s.mutations[addr].applied = false
	s.mutations[addr].typ = deletion
}

func (s *StateDB) markUpdate(addr common.Address) {
	if _, ok := s.mutations[addr]; !ok {
		s.mutations[addr] = &mutation{}
	}
	s.mutations[addr].applied = false
	s.mutations[addr].typ = update
}

// ---------------------------------------------------------------------------
// Fast settlement methods — bypass journal for irrevocable operations.
// Used by V2 BlockSTM settlement where reverts never happen.
// ---------------------------------------------------------------------------

// SetStorageDirectWithOrigins writes storage slots along with their committed
// (origin) values without creating a journal revert entry; the caller owns
// rollback semantics (V2 settlement never reverts). The account is still
// marked dirty so Finalise/Commit pick up the change. Providing origins up
// front avoids expensive trie reads during FinaliseFast.
func (s *StateDB) SetStorageDirectWithOrigins(addr common.Address, slots map[common.Hash]common.Hash, origins map[common.Hash]common.Hash) {
	if len(slots) == 0 {
		return
	}
	obj := s.getOrNewStateObject(addr)
	if obj == nil {
		return
	}
	s.journal.dirty(addr)
	s.markUpdate(addr)
	for key, value := range slots {
		obj.dirtyStorage[key] = value
		if _, cached := obj.originStorage[key]; !cached {
			if origin, ok := origins[key]; ok {
				obj.originStorage[key] = origin
			}
		}
	}
}

// SetNonceDirect writes a nonce without creating a journal revert entry; the
// account is still marked dirty so Finalise/Commit pick up the change. Used
// by V2 settlement, which never reverts.
func (s *StateDB) SetNonceDirect(addr common.Address, nonce uint64) {
	obj := s.getOrNewStateObject(addr)
	if obj == nil {
		return
	}
	s.journal.dirty(addr)
	s.markUpdate(addr)
	obj.data.Nonce = nonce
}

// AddBalanceDirect adds balance without journaling or reading the old balance.
func (s *StateDB) AddBalanceDirect(addr common.Address, amount *uint256.Int) {
	obj := s.getOrNewStateObject(addr)
	if obj == nil {
		return
	}
	// EIP-161: zero-amount add to empty account must trigger touch for cleanup.
	if amount.IsZero() {
		if obj.empty() {
			obj.touch()
		}
		return
	}
	s.journal.dirty(addr)
	s.markUpdate(addr)
	obj.setBalance(new(uint256.Int).Add(obj.Balance(), amount))
}

// SubBalanceDirect subtracts balance without journaling.
//
// uint256.Int.Sub wraps on underflow — same modular-arithmetic semantics
// as the journaled SubBalance path (statedb.go:922-940). TestDirectSetter
// Parity_SubBalance pins byte-equality between the two, so this MUST stay
// consistent with that behaviour. The EVM's CALL/transfer pre-checks
// guarantee amount ≤ balance for any code path that reaches a settle.
func (s *StateDB) SubBalanceDirect(addr common.Address, amount *uint256.Int) {
	obj := s.getOrNewStateObject(addr)
	if obj == nil {
		return
	}
	s.journal.dirty(addr)
	s.markUpdate(addr)
	obj.setBalance(new(uint256.Int).Sub(obj.Balance(), amount))
}

// FinaliseFastWithPrefetch is FinaliseFast plus prefetcher triggering for
// storage tries — matching serial Finalise's prefetch behavior.
func (s *StateDB) FinaliseFastWithPrefetch(deleteEmptyObjects bool) {
	// Snapshot dirty storage slots BEFORE FinaliseFast moves them to pending,
	// then prefetch their tries so the GetCommittedState calls inside
	// FinaliseFast hit cached data instead of going to Pebble.
	if s.prefetcher != nil {
		for _, as := range s.snapshotDirtyStorageSlots() {
			obj := s.stateObjects[as.addr]
			if obj == nil {
				continue
			}
			_ = s.prefetcher.prefetch(obj.addrHash(), as.root, as.addr, nil, as.slots, false)
		}
	}
	s.FinaliseFast(deleteEmptyObjects)
}

type addrDirtySlots struct {
	addr  common.Address
	root  common.Hash
	slots []common.Hash
}

// snapshotDirtyStorageSlots returns per-address dirty slot lists for every
// dirty journal entry whose state object has a non-empty root and dirty
// storage. Used to scope prefetching to only what FinaliseFast will touch.
func (s *StateDB) snapshotDirtyStorageSlots() []addrDirtySlots {
	var out []addrDirtySlots
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist || len(obj.dirtyStorage) == 0 {
			continue
		}
		root := obj.getPrefetchRoot()
		if root == types.EmptyRootHash && !s.db.TrieDB().IsVerkle() {
			continue
		}
		slots := make([]common.Hash, 0, len(obj.dirtyStorage))
		for key := range obj.dirtyStorage {
			slots = append(slots, key)
		}
		out = append(out, addrDirtySlots{addr: addr, root: root, slots: slots})
	}
	return out
}

// FinaliseFast is a V2-optimized Finalise that skips GetCommittedState calls
// when origin values are cached, and triggers prefetcher in the background.
// Used during pipelined settlement where incremental commit tracking is
// not required — the final Finalise before IntermediateRoot handles that.
func (s *StateDB) FinaliseFast(deleteEmptyObjects bool) {
	var addressesToPrefetch []common.Address
	for addr := range s.journal.dirties {
		obj, exist := s.stateObjects[addr]
		if !exist {
			continue
		}
		if obj.selfDestructed || (deleteEmptyObjects && obj.empty()) {
			s.finaliseDelete(addr, obj)
		} else {
			s.finalisePromote(addr, obj)
		}
		addressesToPrefetch = append(addressesToPrefetch, addr)
	}
	if s.prefetcher != nil && len(addressesToPrefetch) > 0 {
		// Pre-load storage tries in the background while later txs settle;
		// errors mean the prefetcher already terminated and are safe to drop.
		_ = s.prefetcher.prefetch(common.Hash{}, s.originalRoot, common.Address{}, addressesToPrefetch, nil, false)
	}
	s.clearJournalAndRefund()
}

// finaliseDelete tears down a self-destructed or empty object during
// FinaliseFast — moves it to the destruct map and marks it deleted.
func (s *StateDB) finaliseDelete(addr common.Address, obj *stateObject) {
	delete(s.stateObjects, obj.address)
	s.markDelete(addr)
	if _, ok := s.stateObjectsDestruct[obj.address]; !ok {
		s.stateObjectsDestruct[obj.address] = obj
	}
	s.currentBlockDestructs[obj.address] = struct{}{}
}

// finalisePromote moves dirty storage to pending, capturing origin values
// (cached when possible) into uncommittedStorage for later commit tracking.
func (s *StateDB) finalisePromote(addr common.Address, obj *stateObject) {
	for key, value := range obj.dirtyStorage {
		if _, exists := obj.uncommittedStorage[key]; !exists {
			if origin, cached := obj.originStorage[key]; cached {
				obj.uncommittedStorage[key] = origin
			} else {
				obj.uncommittedStorage[key] = obj.GetCommittedState(key)
			}
		}
		obj.pendingStorage[key] = value
	}
	if len(obj.dirtyStorage) > 0 {
		obj.dirtyStorage = make(Storage)
	}
	obj.newContract = false
	s.markUpdate(addr)
}

// SkipTimers disables time.Now() calls in hot paths (account reads, storage reads).
// Used by V2 parallel execution where per-operation timing is not needed.
func (s *StateDB) SkipTimers() {
	s.skipTimers = true
}

// Witness retrieves the current state witness being collected.
func (s *StateDB) Witness() *stateless.Witness {
	return s.witness
}

func (s *StateDB) AccessEvents() *AccessEvents {
	return s.accessEvents
}

// Inner receives the underlying state db
func (s *StateDB) Inner() *StateDB {
	return s
}

// PropagateReadsTo touches all addresses and storage slots accessed in s on
// the destination StateDB. This ensures the destination tracks them in its
// stateObjects (and later in its FlatDiff ReadSet) so the pipelined SRC
// goroutine captures their trie proof nodes in the witness.
//
// Use this when a temporary copy of the state is used for EVM calls (e.g.,
// CommitStates → LastStateId) and the accessed addresses must be visible
// in the original state for witness generation.
func (s *StateDB) PropagateReadsTo(dst *StateDB) {
	for addr, obj := range s.stateObjects {
		dst.GetBalance(addr)
		for slot := range obj.originStorage {
			dst.GetState(addr, slot)
		}
	}
}
