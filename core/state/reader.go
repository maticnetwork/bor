// Copyright 2024 The go-ethereum Authors
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

package state

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/overlay"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/trie/bintrie"
	"github.com/ethereum/go-ethereum/trie/transitiontrie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/database"
)

// ContractCodeReader defines the interface for accessing contract code.
type ContractCodeReader interface {
	// Code retrieves a particular contract's code.
	//
	// - Returns nil code along with nil error if the requested contract code
	//   doesn't exist
	// - Returns an error only if an unexpected issue occurs
	Code(addr common.Address, codeHash common.Hash) ([]byte, error)

	// CodeSize retrieves a particular contracts code's size.
	//
	// - Returns zero code size along with nil error if the requested contract code
	//   doesn't exist
	// - Returns an error only if an unexpected issue occurs
	CodeSize(addr common.Address, codeHash common.Hash) (int, error)
}

// StateReader defines the interface for accessing accounts and storage slots
// associated with a specific state.
//
// StateReader is assumed to be thread-safe and implementation must take care
// of the concurrency issue by themselves.
type StateReader interface {
	// Account retrieves the account associated with a particular address.
	//
	// - Returns a nil account if it does not exist
	// - Returns an error only if an unexpected issue occurs
	// - The returned account is safe to modify after the call
	Account(addr common.Address) (*types.StateAccount, error)

	// Storage retrieves the storage slot associated with a particular account
	// address and slot key.
	//
	// - Returns an empty slot if it does not exist
	// - Returns an error only if an unexpected issue occurs
	// - The returned storage slot is safe to modify after the call
	Storage(addr common.Address, slot common.Hash) (common.Hash, error)
}

// Reader defines the interface for accessing accounts, storage slots and contract
// code associated with a specific state.
//
// Reader is assumed to be thread-safe and implementation must take care of the
// concurrency issue by themselves.
type Reader interface {
	ContractCodeReader
	StateReader
}

// ReaderStats wraps the statistics of reader.
type ReaderStats struct {
	AccountHit  int64
	AccountMiss int64
	StorageHit  int64
	StorageMiss int64
}

// PrefetchStats exposes additional attribution stats for evaluating prefetch effectiveness.
type PrefetchStats struct {
	// Hits in PROCESS that came from PREFETCH-origin entries.
	AccountHitFromPrefetch int64
	StorageHitFromPrefetch int64
	// Unique keys PREFETCH inserted into the shared local cache.
	AccountInsert int64
	StorageInsert int64
	// Unique prefetched account keys that PROCESS actually used.
	AccountHitFromPrefetchUnique int64
}

// ReaderWithStats wraps the additional method to retrieve the reader statistics from.
type ReaderWithStats interface {
	Reader
	GetStats() ReaderStats
	GetPrefetchStats() PrefetchStats
}

// cachingCodeReader implements ContractCodeReader, accessing contract code either in
// local key-value store or the shared code cache.
//
// cachingCodeReader is safe for concurrent access.
type cachingCodeReader struct {
	db ethdb.KeyValueReader

	// These caches could be shared by multiple code reader instances,
	// they are natively thread-safe.
	codeCache     *lru.SizeConstrainedCache[common.Hash, []byte]
	codeSizeCache *lru.Cache[common.Hash, int]
}

// newCachingCodeReader constructs the code reader.
func newCachingCodeReader(db ethdb.KeyValueReader, codeCache *lru.SizeConstrainedCache[common.Hash, []byte], codeSizeCache *lru.Cache[common.Hash, int]) *cachingCodeReader {
	return &cachingCodeReader{
		db:            db,
		codeCache:     codeCache,
		codeSizeCache: codeSizeCache,
	}
}

// Code implements ContractCodeReader, retrieving a particular contract's code.
// If the contract code doesn't exist, no error will be returned.
func (r *cachingCodeReader) Code(addr common.Address, codeHash common.Hash) ([]byte, error) {
	code, _ := r.codeCache.Get(codeHash)
	if len(code) > 0 {
		return code, nil
	}
	code = rawdb.ReadCode(r.db, codeHash)
	if len(code) > 0 {
		r.codeCache.Add(codeHash, code)
		r.codeSizeCache.Add(codeHash, len(code))
	}
	return code, nil
}

// CodeSize implements ContractCodeReader, retrieving a particular contracts code's size.
// If the contract code doesn't exist, no error will be returned.
func (r *cachingCodeReader) CodeSize(addr common.Address, codeHash common.Hash) (int, error) {
	if cached, ok := r.codeSizeCache.Get(codeHash); ok {
		return cached, nil
	}
	code, err := r.Code(addr, codeHash)
	if err != nil {
		return 0, err
	}
	return len(code), nil
}

// flatReader wraps a database state reader and is safe for concurrent access.
type flatReader struct {
	reader    database.StateReader
	addrCache sync.Map // common.Address → common.Hash (keccak256 cache)
}

// newFlatReader constructs a state reader with on the given state root.
func newFlatReader(reader database.StateReader) *flatReader {
	return &flatReader{reader: reader}
}

// Account implements StateReader, retrieving the account specified by the address.
//
// An error will be returned if the associated snapshot is already stale or
// the requested account is not yet covered by the snapshot.
//
// The returned account might be nil if it's not existent.
func (r *flatReader) Account(addr common.Address) (*types.StateAccount, error) {
	var addrHash common.Hash
	if v, ok := r.addrCache.Load(addr); ok {
		addrHash = v.(common.Hash)
	} else {
		addrHash = crypto.Keccak256Hash(addr.Bytes())
		r.addrCache.Store(addr, addrHash)
	}
	account, err := r.reader.Account(addrHash)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, nil
	}
	acct := &types.StateAccount{
		Nonce:    account.Nonce,
		Balance:  account.Balance,
		CodeHash: account.CodeHash,
		Root:     common.BytesToHash(account.Root),
	}
	if len(acct.CodeHash) == 0 {
		acct.CodeHash = types.EmptyCodeHash.Bytes()
	}
	if acct.Root == (common.Hash{}) {
		acct.Root = types.EmptyRootHash
	}
	return acct, nil
}

// Storage implements StateReader, retrieving the storage slot specified by the
// address and slot key.
//
// An error will be returned if the associated snapshot is already stale or
// the requested storage slot is not yet covered by the snapshot.
//
// The returned storage slot might be empty if it's not existent.
func (r *flatReader) Storage(addr common.Address, key common.Hash) (common.Hash, error) {
	var addrHash common.Hash
	if v, ok := r.addrCache.Load(addr); ok {
		addrHash = v.(common.Hash)
	} else {
		addrHash = crypto.Keccak256Hash(addr.Bytes())
		r.addrCache.Store(addr, addrHash)
	}
	slotHash := crypto.Keccak256Hash(key.Bytes())
	ret, err := r.reader.Storage(addrHash, slotHash)
	if err != nil {
		return common.Hash{}, err
	}
	if len(ret) == 0 {
		return common.Hash{}, nil
	}
	// Perform the rlp-decode as the slot value is RLP-encoded in the state
	// snapshot.
	_, content, _, err := rlp.Split(ret)
	if err != nil {
		return common.Hash{}, err
	}
	var value common.Hash
	value.SetBytes(content)
	return value, nil
}

// trieReader implements the StateReader interface, providing functions to access
// state from the referenced trie.
//
// trieReader is safe for concurrent read.
type trieReader struct {
	root common.Hash      // State root which uniquely represent a state
	db   *triedb.Database // Database for loading trie (kept for IsVerkle / Disk access)

	// nodeDB is the database used to construct sub-tries (storage tries) on
	// demand. It defaults to db, but the snapshot-aware constructor wraps db
	// so storage trie reads consult a WarmSnapshot before falling through to
	// pathdb. Keeping it as an interface lets the wrapper remain transparent.
	nodeDB database.NodeDatabase

	// Main trie, resolved in constructor. Note either the Merkle-Patricia-tree
	// or Verkle-tree is not safe for concurrent read.
	mainTrie Trie

	subRoots          sync.Map   // common.Address → common.Hash (storage roots)
	subTries          sync.Map   // common.Address → Trie (storage tries)
	lock              sync.Mutex // Lock for protecting concurrent read
	accountCache      sync.Map   // addr → *types.StateAccount (concurrent-safe cache)
	storageCache      sync.Map   // addrSlot → common.Hash (concurrent-safe cache)
	concurrentEnabled bool       // if true, skip r.lock (trie uses sync.Map resolve cache)
}

// newTrieReader constructs a trie reader of the specific state. An error will be
// returned if the associated trie specified by root is not existent.
func newTrieReader(root common.Hash, db *triedb.Database) (*trieReader, error) {
	var (
		tr  Trie
		err error
	)
	if !db.IsVerkle() {
		tr, err = trie.NewStateTrie(trie.StateTrieID(root), db)
	} else {
		// When IsVerkle() is true, create a BinaryTrie wrapped in TransitionTrie
		binTrie, binErr := bintrie.NewBinaryTrie(root, db)
		if binErr != nil {
			return nil, binErr
		}

		// Based on the transition status, determine if the overlay
		// tree needs to be created, or if a single, target tree is
		// to be picked.
		ts := overlay.LoadTransitionState(db.Disk(), root, true)
		if ts.InTransition() {
			mpt, err := trie.NewStateTrie(trie.StateTrieID(ts.BaseRoot), db)
			if err != nil {
				return nil, err
			}
			tr = transitiontrie.NewTransitionTrie(mpt, binTrie, false)
		} else {
			// HACK: Use TransitionTrie with nil base as a wrapper to make BinaryTrie
			// satisfy the Trie interface. This works around the import cycle between
			// trie and trie/bintrie packages.
			//
			// TODO: In future PRs, refactor the package structure to avoid this hack:
			// - Option 1: Move common interfaces (Trie, NodeIterator) to a separate
			//   package that both trie and trie/bintrie can import
			// - Option 2: Create a factory function in the trie package that returns
			//   BinaryTrie as a Trie interface without direct import
			// - Option 3: Move BinaryTrie to the main trie package
			//
			// The current approach works but adds unnecessary overhead and complexity
			// by using TransitionTrie when there's no actual transition happening.
			tr = transitiontrie.NewTransitionTrie(nil, binTrie, false)
		}
	}
	if err != nil {
		return nil, err
	}
	return &trieReader{
		root:     root,
		db:       db,
		nodeDB:   db,
		mainTrie: tr,
	}, nil
}

// newTrieReaderWithSnapshot mirrors newTrieReader but receives a snapshot-aware
// NodeDatabase so trie reads can consult a WarmSnapshot before falling through
// to pathdb/pebble. The snapshot is hash-verified by the supplied NodeDatabase;
// misses or hash mismatches transparently fall through. The trie itself is
// unchanged (NewStateTrie sees a wrapped NodeDatabase only) — its
// resolveAndTrack path and prevalueTracer recording fire identically whether
// the served node came from the snapshot or pathdb, so witness completeness
// under NewTrieOnly semantics is preserved.
//
// Verkle is not supported by this path: pipelined SRC is MPT-only and the
// snapshot is constructed from MPT trie nodes. Callers that need verkle
// readers must use newTrieReader.
func newTrieReaderWithSnapshot(root common.Hash, db *triedb.Database, nodeDB database.NodeDatabase) (*trieReader, error) {
	if db.IsVerkle() {
		return nil, errors.New("warm snapshot reader: verkle scheme is not supported")
	}
	tr, err := trie.NewStateTrie(trie.StateTrieID(root), nodeDB)
	if err != nil {
		return nil, err
	}
	return &trieReader{
		root:     root,
		db:       db,
		nodeDB:   nodeDB,
		mainTrie: tr,
	}, nil
}

// account is the inner version of Account and assumes the r.lock is already held.
func (r *trieReader) account(addr common.Address) (*types.StateAccount, error) {
	account, err := r.mainTrie.GetAccount(addr)
	if err != nil {
		return nil, err
	}
	if account == nil {
		r.subRoots.Store(addr, types.EmptyRootHash)
	} else {
		r.subRoots.Store(addr, account.Root)
	}
	return account, nil
}

// Account implements StateReader, retrieving the account specified by the address.
//
// An error will be returned if the trie state is corrupted. An nil account
// will be returned if it's not existent in the trie.
func (r *trieReader) Account(addr common.Address) (*types.StateAccount, error) {
	// Fast path: check concurrent-safe cache before acquiring lock.
	if cached, ok := r.accountCache.Load(addr); ok {
		return cached.(*types.StateAccount), nil
	}

	if r.concurrentEnabled {
		// Trie has sync.Map resolve cache — no lock needed.
		acct, err := r.account(addr)
		if err == nil {
			r.accountCache.Store(addr, acct)
		}
		return acct, err
	}

	r.lock.Lock()
	acct, err := r.account(addr)
	r.lock.Unlock()

	if err == nil {
		r.accountCache.Store(addr, acct)
	}
	return acct, err
}

// EnableConcurrentReads enables concurrent trie reads by switching the
// trie to use a sync.Map resolve cache instead of in-place mutation.
// After calling this, Account() and Storage() can be called concurrently
// without the r.lock (using only the sync.Map caches).
func (r *trieReader) EnableConcurrentReads() {
	if st, ok := r.mainTrie.(*trie.StateTrie); ok {
		st.EnableConcurrentReads()
	}
	r.concurrentEnabled = true
}

// CollectStateWitness adds every trie node that has been read through this
// reader into the supplied witness. Called by V2 BlockSTM at end-of-block
// because finalDB.IntermediateRoot's per-stateObject witness loop only
// covers addresses present in finalDB.stateObjects — addresses that were
// only READ (never written) by V2 workers don't reach finalDB at all.
//
// The reader's mainTrie and per-address sub-tries are SHARED with all
// pool copies and finalDB (StateDB.Copy() shares reader by reference), so
// every worker read accumulates in the same set of trie tracers. Walking
// reader.subTries here picks up exactly the worker-only-read tries that
// finalDB doesn't know about.
func (r *trieReader) CollectStateWitness(addState func(map[string][]byte)) {
	if r.mainTrie != nil {
		addState(r.mainTrie.Witness())
	}
	r.subTries.Range(func(_, v any) bool {
		if t, ok := v.(interface{ Witness() map[string][]byte }); ok {
			addState(t.Witness())
		}
		return true
	})
}

// Storage implements StateReader, retrieving the storage slot specified by the
// address and slot key.
//
// An error will be returned if the trie state is corrupted. An empty storage
// slot will be returned if it's not existent in the trie.
// storageCacheKey is a composite key for the trieReader storage cache.
// Uses the same layout as stateKey so the maps are compatible.
type storageCacheKey = stateKey

func (r *trieReader) Storage(addr common.Address, key common.Hash) (common.Hash, error) {
	cacheKey := storageCacheKey{addr, key}
	if cached, ok := r.storageCache.Load(cacheKey); ok {
		return cached.(common.Hash), nil
	}
	tr, err := r.subTrieFor(addr)
	if err != nil {
		return common.Hash{}, err
	}
	ret, err := tr.GetStorage(addr, key.Bytes())
	if err != nil {
		return common.Hash{}, err
	}
	var value common.Hash
	value.SetBytes(ret)
	r.storageCache.Store(cacheKey, value)
	return value, nil
}

// subTrieFor returns a Trie suitable for reading storage of addr. It
// dispatches to the concurrent or locked path based on configuration.
func (r *trieReader) subTrieFor(addr common.Address) (Trie, error) {
	if r.concurrentEnabled {
		return r.subTrieConcurrent(addr)
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	return r.subTrieLocked(addr)
}

// subTrieConcurrent is the V2 lock-free path: uses sync.Map for the trie
// cache and EnableConcurrentReads() on freshly opened tries so multiple
// goroutines can read different addresses without cross-address contention.
func (r *trieReader) subTrieConcurrent(addr common.Address) (Trie, error) {
	if v, ok := r.subTries.Load(addr); ok {
		return v.(Trie), nil
	}
	root, err := r.resolveSubRoot(addr)
	if err != nil {
		return nil, err
	}
	newTr, err := trie.NewStateTrie(trie.StorageTrieID(r.root, crypto.Keccak256Hash(addr.Bytes()), root), r.nodeDB)
	if err != nil {
		return nil, err
	}
	newTr.EnableConcurrentReads()
	// First-writer-wins: another goroutine may have created it.
	if existing, loaded := r.subTries.LoadOrStore(addr, newTr); loaded {
		return existing.(Trie), nil
	}
	return newTr, nil
}

// subTrieLocked is the legacy mutex-protected path. Verkle uses the
// merged main trie; MPT uses per-address sub tries cached in subTries.
func (r *trieReader) subTrieLocked(addr common.Address) (Trie, error) {
	if r.db.IsVerkle() {
		return r.mainTrie, nil
	}
	if v, ok := r.subTries.Load(addr); ok {
		return v.(Trie), nil
	}
	root, err := r.resolveSubRoot(addr)
	if err != nil {
		return nil, err
	}
	tr, err := trie.NewStateTrie(trie.StorageTrieID(r.root, crypto.Keccak256Hash(addr.Bytes()), root), r.nodeDB)
	if err != nil {
		return nil, err
	}
	r.subTries.Store(addr, tr)
	return tr, nil
}

// resolveSubRoot returns the storage root for addr, resolving the account
// first if necessary so the cache is populated.
func (r *trieReader) resolveSubRoot(addr common.Address) (common.Hash, error) {
	if v, ok := r.subRoots.Load(addr); ok {
		return v.(common.Hash), nil
	}
	if _, err := r.account(addr); err != nil {
		return common.Hash{}, err
	}
	if v, ok := r.subRoots.Load(addr); ok {
		return v.(common.Hash), nil
	}
	return common.Hash{}, nil
}

// multiStateReader is the aggregation of a list of StateReader interface,
// providing state access by leveraging all readers. The checking priority
// is determined by the position in the reader list.
//
// multiStateReader is safe for concurrent read and assumes all underlying
// readers are thread-safe as well.
type multiStateReader struct {
	readers []StateReader // List of state readers, sorted by checking priority
}

// newMultiStateReader constructs a multiStateReader instance with the given
// readers. The priority among readers is assumed to be sorted. Note, it must
// contain at least one reader for constructing a multiStateReader.
func newMultiStateReader(readers ...StateReader) (*multiStateReader, error) {
	if len(readers) == 0 {
		return nil, errors.New("empty reader set")
	}
	return &multiStateReader{
		readers: readers,
	}, nil
}

// Account implementing StateReader interface, retrieving the account associated
// with a particular address.
//
// - Returns a nil account if it does not exist
// - Returns an error only if an unexpected issue occurs
// - The returned account is safe to modify after the call
func (r *multiStateReader) Account(addr common.Address) (*types.StateAccount, error) {
	var errs []error
	for _, reader := range r.readers {
		acct, err := reader.Account(addr)
		if err == nil {
			return acct, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

// Storage implementing StateReader interface, retrieving the storage slot
// associated with a particular account address and slot key.
//
// - Returns an empty slot if it does not exist
// - Returns an error only if an unexpected issue occurs
// - The returned storage slot is safe to modify after the call
func (r *multiStateReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	var errs []error
	for _, reader := range r.readers {
		slot, err := reader.Storage(addr, slot)
		if err == nil {
			return slot, nil
		}
		errs = append(errs, err)
	}
	return common.Hash{}, errors.Join(errs...)
}

// reader is the wrapper of ContractCodeReader and StateReader interface.
type reader struct {
	ContractCodeReader
	StateReader
}

// newReader constructs a reader with the supplied code reader and state reader.
func newReader(codeReader ContractCodeReader, stateReader StateReader) *reader {
	return &reader{
		ContractCodeReader: codeReader,
		StateReader:        stateReader,
	}
}

// readerRole identifies the "writer" responsible for warming the shared local cache.
// It is used purely for attribution in metrics (prefetch vs process).
type readerRole uint8

const (
	roleUnknown  readerRole = 0
	rolePrefetch readerRole = 1
	roleProcess  readerRole = 2
)

// accountCacheEntry is the cached account plus attribution metadata.
type accountCacheEntry struct {
	acct *types.StateAccount
	// origin is who first inserted this entry into the local cache (prefetch/process).
	origin readerRole
	// usedByProcess is flipped exactly once when the PROCESS reader consumes an entry
	// that was prefetched. Used to compute unique-usage/precision.
	usedByProcess uint32
	// walked is set once the key's trie path has been resolved into the trie
	// reader's tracers for witness recording.
	walked atomic.Bool
}

// storageCacheEntry is the cached storage slot plus attribution metadata.
type storageCacheEntry struct {
	value  common.Hash
	origin readerRole
	// walked is set once the key's trie path has been resolved into the trie
	// reader's tracers for witness recording.
	walked atomic.Bool
}

// storageKey is the composite key for the readerWithCache storage sync.Map.
// Uses the same layout as stateKey so SafeBase can share the map.
type storageKey = stateKey

// readerWithCache is a wrapper around Reader that maintains additional state caches
// to support concurrent state access. Both caches use sync.Map for lock-free reads —
// the dominant access pattern is many concurrent readers with infrequent first-writes.
type readerWithCache struct {
	Reader // safe for concurrent read

	// Previously resolved account entries. Key: common.Address, Value: *accountCacheEntry.
	accounts sync.Map

	// Previously resolved storage entries. Key: storageKey, Value: *storageCacheEntry.
	storageCache sync.Map

	// insertGen counts first-inserts into either cache. The witness walker
	// compares it against sweptGen (its last-swept generation) to skip
	// Ranging the maps when nothing new was cached; only the (I/O-bound)
	// miss path pays it.
	insertGen atomic.Uint64
	sweptGen  atomic.Uint64
}

// newReaderWithCache constructs the reader with local cache.
func newReaderWithCache(reader Reader) *readerWithCache {
	return &readerWithCache{
		Reader: reader,
	}
}

// account retrieves the account specified by the address along with a flag
// indicating whether it's found in the cache or not. The returned account
// might be nil if it's not existent.
//
// An error will be returned if the state is corrupted in the underlying reader.
//
// It also returns the cache entry (for provenance/unique-usage accounting)
// and whether this call inserted a new entry (first-writer-wins).
func (r *readerWithCache) account(addr common.Address, caller readerRole) (*types.StateAccount, bool, *accountCacheEntry, bool, error) {
	// Try to resolve the requested account in the local cache (lock-free read).
	if v, ok := r.accounts.Load(addr); ok {
		ent := v.(*accountCacheEntry)
		return ent.acct, true, ent, false, nil
	}
	// Cache miss — resolve from the underlying reader (may involve pebble I/O).
	acct, err := r.Reader.Account(addr)
	if err != nil {
		return nil, false, nil, false, err
	}
	// First-writer-wins: LoadOrStore inserts only if key is absent.
	newEnt := &accountCacheEntry{acct: acct, origin: caller}
	if existing, loaded := r.accounts.LoadOrStore(addr, newEnt); loaded {
		ent := existing.(*accountCacheEntry)
		// Another goroutine inserted while we fetched from the backing reader.
		// Report incache=false so miss counters reflect backing-read cost.
		return ent.acct, false, ent, false, nil
	}
	r.insertGen.Add(1)
	return acct, false, newEnt, true, nil
}

// Account implements StateReader, retrieving the account specified by the address.
// The returned account might be nil if it's not existent.
//
// An error will be returned if the state is corrupted in the underlying reader.
func (r *readerWithCache) Account(addr common.Address) (*types.StateAccount, error) {
	account, _, _, _, err := r.account(addr, roleUnknown)
	return account, err
}

// CollectReadSet enumerates every account and storage key resolved through
// this cache during the block, regardless of which statedb issued the read
// or which backing layer served it. The cache is shared across the reader
// triple, finalDB, and every BlockSTM pool copy, so this is a complete
// record of reader-reaching reads — the counterpart, for the FlatDiff
// read-set, of what resolveCachedKeysIntoTrie is for witness trie nodes.
func (r *readerWithCache) CollectReadSet(onAccount func(common.Address, bool), onStorage func(common.Address, common.Hash)) {
	r.accounts.Range(func(k, v any) bool {
		ent := v.(*accountCacheEntry)
		onAccount(k.(common.Address), ent.acct != nil)
		return true
	})
	r.storageCache.Range(func(k, _ any) bool {
		key := k.(storageKey)
		onStorage(key.addr, key.slot)
		return true
	})
}

// storage retrieves the storage slot specified by the address and slot key, along
// with a flag indicating whether it's found in the cache or not. The returned
// storage slot might be empty if it's not existent.
//
// It also returns the cache entry (for provenance/unique-usage accounting)
// and whether this call inserted a new entry (first-writer-wins).
func (r *readerWithCache) storage(addr common.Address, slot common.Hash, caller readerRole) (common.Hash, bool, *storageCacheEntry, bool, error) {
	key := storageKey{addr: addr, slot: slot}

	// Try to resolve the requested storage slot in the local cache (lock-free read).
	if v, ok := r.storageCache.Load(key); ok {
		ent := v.(*storageCacheEntry)
		return ent.value, true, ent, false, nil
	}
	// Cache miss — resolve from the underlying reader (may involve pebble I/O).
	value, err := r.Reader.Storage(addr, slot)
	if err != nil {
		return common.Hash{}, false, nil, false, err
	}
	// First-writer-wins: LoadOrStore inserts only if key is absent.
	newEnt := &storageCacheEntry{value: value, origin: caller}
	if existing, loaded := r.storageCache.LoadOrStore(key, newEnt); loaded {
		ent := existing.(*storageCacheEntry)
		// Another goroutine inserted while we fetched from the backing reader.
		// Report incache=false so miss counters reflect backing-read cost.
		return ent.value, false, ent, false, nil
	}
	r.insertGen.Add(1)
	return value, false, newEnt, true, nil
}

// Storage implements StateReader, retrieving the storage slot specified by the
// address and slot key. The returned storage slot might be empty if it's not
// existent.
//
// An error will be returned if the state is corrupted in the underlying reader.
func (r *readerWithCache) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	value, _, _, _, err := r.storage(addr, slot, roleUnknown)
	return value, err
}

type readerWithCacheStats struct {
	*readerWithCache
	role readerRole

	accountHit  atomic.Int64
	accountMiss atomic.Int64
	storageHit  atomic.Int64
	storageMiss atomic.Int64

	// attribute PROCESS hits that were served by PREFETCH-origin entries.
	accountHitFromPrefetch atomic.Int64
	storageHitFromPrefetch atomic.Int64

	// count unique inserts by PREFETCH (how much it warmed).
	accountInsert atomic.Int64
	storageInsert atomic.Int64

	// count unique prefetched keys that PROCESS actually used (precision) for accounts only.
	accountHitFromPrefetchUnique atomic.Int64
}

// newReaderWithCacheStats constructs the reader with additional statistics tracked.
func newReaderWithCacheStats(reader *readerWithCache, role readerRole) *readerWithCacheStats {
	return &readerWithCacheStats{
		readerWithCache: reader,
		role:            role,
	}
}

// Account implements StateReader, retrieving the account specified by the address.
// The returned account might be nil if it's not existent.
//
// An error will be returned if the state is corrupted in the underlying reader.
func (r *readerWithCacheStats) Account(addr common.Address) (*types.StateAccount, error) {
	account, incache, ent, inserted, err := r.readerWithCache.account(addr, r.role)
	if err != nil {
		return nil, err
	}
	if incache {
		r.accountHit.Add(1)
		// Attribute hits in PROCESS that came from PREFETCH-origin entries.
		if r.role == roleProcess && ent != nil && ent.origin == rolePrefetch {
			r.accountHitFromPrefetch.Add(1)
			// Flip usedByProcess only once per entry.
			if atomic.CompareAndSwapUint32(&ent.usedByProcess, 0, 1) {
				r.accountHitFromPrefetchUnique.Add(1)
			}
		}
	} else {
		r.accountMiss.Add(1)
		// Count unique inserts done by PREFETCH (first-writer-wins).
		if r.role == rolePrefetch && inserted {
			r.accountInsert.Add(1)
		}
	}
	return account, nil
}

// Storage implements StateReader, retrieving the storage slot specified by the
// address and slot key. The returned storage slot might be empty if it's not
// existent.
//
// An error will be returned if the state is corrupted in the underlying reader.
func (r *readerWithCacheStats) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	value, incache, entCopy, inserted, err := r.readerWithCache.storage(addr, slot, r.role)
	if err != nil {
		return common.Hash{}, err
	}
	if incache {
		r.storageHit.Add(1)
		// Attribute hits in PROCESS that came from PREFETCH-origin entries.
		// NOTE: No write-lock marking (Option C). We only track hit attribution.
		if r.role == roleProcess && entCopy != nil && entCopy.origin == rolePrefetch {
			r.storageHitFromPrefetch.Add(1)
		}
	} else {
		r.storageMiss.Add(1)
		// Count unique inserts done by PREFETCH (first-writer-wins).
		// This comes "for free" on the miss/insert path (no extra locking).
		if r.role == rolePrefetch && inserted {
			r.storageInsert.Add(1)
		}
	}
	return value, nil
}

// GetStats implements ReaderWithStats, returning the statistics of state reader.
func (r *readerWithCacheStats) GetStats() ReaderStats {
	return ReaderStats{
		AccountHit:  r.accountHit.Load(),
		AccountMiss: r.accountMiss.Load(),
		StorageHit:  r.storageHit.Load(),
		StorageMiss: r.storageMiss.Load(),
	}
}

// GetPrefetchStats returns attribution statistics for evaluating prefetch effectiveness.
func (r *readerWithCacheStats) GetPrefetchStats() PrefetchStats {
	return PrefetchStats{
		AccountHitFromPrefetch:       r.accountHitFromPrefetch.Load(),
		StorageHitFromPrefetch:       r.storageHitFromPrefetch.Load(),
		AccountInsert:                r.accountInsert.Load(),
		StorageInsert:                r.storageInsert.Load(),
		AccountHitFromPrefetchUnique: r.accountHitFromPrefetchUnique.Load(),
	}
}
