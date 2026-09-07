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

package pathdb

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb/database"
)

// The types of locations where the node is found.
const (
	locDirtyCache = "dirty" // dirty cache
	locCleanCache = "clean" // clean cache
	locDiskLayer  = "disk"  // persistent state
	locDiffLayer  = "diff"  // diff layers
)

// nodeLoc is a helpful structure that contains the location where the node
// is found, as it's useful for debugging purposes.
type nodeLoc struct {
	loc   string
	depth int
}

// string returns the string representation of node location.
func (loc nodeLoc) string() string {
	return fmt.Sprintf("loc: %s, depth: %d", loc.loc, loc.depth)
}

// reader implements the database.NodeReader interface, providing the functionalities to
// retrieve trie nodes by wrapping the internal state layer.
type reader struct {
	db          *Database
	state       common.Hash
	noHashCheck bool
	layer       layer
}

// Node implements database.NodeReader interface, retrieving the node with specified
// node info. Don't modify the returned byte slice since it's not deep-copied
// and still be referenced by database.
func (r *reader) Node(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	// Fast path: consult the layer tree's node index. A hit is
	// content-verified (the hash is part of the index key); a definitive
	// miss guarantees no live diff layer holds the node, so it can only be
	// in the disk layer — skip the layer-chain walk entirely.
	if !r.noHashCheck && hash != (common.Hash{}) {
		if blob, found, definitive := r.db.tree.node(owner, path, hash); found {
			nodeIndexHitMeter.Mark(1)
			return blob, nil
		} else if definitive {
			nodeIndexMissMeter.Mark(1)
			if blob, ok := r.diskNode(owner, path, hash); ok {
				return blob, nil
			}
			// Irregularity (stale disk layer mid-read, hash mismatch) —
			// resolve through the legacy walk below, which owns fallback
			// and error reporting.
		}
	}
	return r.nodeWalk(owner, path, hash)
}

// diskNode serves a definitive node-index miss straight from the disk layer.
// ok=false on any irregularity so the caller retries via the legacy walk.
func (r *reader) diskNode(owner common.Hash, path []byte, hash common.Hash) ([]byte, bool) {
	blob, got, loc, err := r.db.tree.bottom().node(owner, path, 0)
	if err != nil {
		return nil, false
	}
	if got == hash {
		return blob, true
	}
	// Evict a potentially poisoned clean-cache entry before the legacy walk
	// retries (see the mismatch commentary in nodeWalk).
	if loc.loc == locCleanCache {
		nodeCleanFalseMeter.Mark(1)
		evictCachedNode(r.db.tree.bottom(), owner, path)
	}
	return nil, false
}

// nodeWalk resolves a trie node by walking the reader's layer chain. It is
// the pre-index resolution path, kept as the authority for noHashCheck
// readers, non-definitive index misses, and all irregular cases (stale
// layers, hash mismatches), owning the fallback and error-reporting logic.
func (r *reader) nodeWalk(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	blob, got, loc, err := r.layer.node(owner, path, 0)
	if err != nil {
		// If the diff layer chain walks into a stale disk layer (marked stale
		// by concurrent cap()/persist() during pipelined SRC), fall back to
		// the current base disk layer — same strategy as accountFallback and
		// storageFallback.
		if errors.Is(err, errSnapshotStale) {
			blob, got, loc, err = r.nodeFallback(owner, path)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if r.noHashCheck || got == hash {
		return blob, nil
	}
	// Hash mismatch path. The clean-cache writer (the biased preloader's
	// async Has→Set) is racy: a flusher's Set(newer) can land between the
	// preloader's Has(absent) and Set(older), poisoning the cache with a
	// stale blob. Evict the offending cache entry and retry from disk so
	// the cache self-heals instead of returning the same stale blob until
	// natural eviction.
	if loc.loc == locCleanCache {
		nodeCleanFalseMeter.Mark(1)
		evictCachedNode(r.layer, owner, path)
		blob, got, loc, err = r.layer.node(owner, path, 0)
		if err != nil {
			return nil, err
		}
		if got == hash {
			return blob, nil
		}
		// Still wrong after eviction → backing store is corrupt or the
		// caller asked for a node that doesn't exist at this state.
	}
	switch loc.loc {
	case locCleanCache:
		nodeCleanFalseMeter.Mark(1)
	case locDirtyCache:
		nodeDirtyFalseMeter.Mark(1)
	case locDiffLayer:
		nodeDiffFalseMeter.Mark(1)
	case locDiskLayer:
		nodeDiskFalseMeter.Mark(1)
	}
	blobHex := "nil"
	if len(blob) > 0 {
		blobHex = hexutil.Encode(blob)
	}
	log.Error("Unexpected trie node", "location", loc.loc, "owner", owner.Hex(), "path", path, "expect", hash.Hex(), "got", got.Hex(), "blob", blobHex)
	return nil, fmt.Errorf("unexpected node: (%x %v), %x!=%x, %s, blob: %s", owner, path, hash, got, loc.string(), blobHex)
}

// evictCachedNode walks the parentLayer chain to find the disk layer and
// removes the given (owner, path) entry from its clean cache. Used by
// reader.Node's hash-mismatch retry path.
func evictCachedNode(l layer, owner common.Hash, path []byte) {
	for l != nil {
		if dl, ok := l.(*diskLayer); ok {
			if dl.nodes != nil {
				dl.nodes.Del(nodeCacheKey(owner, path))
			}
			return
		}
		l = l.parentLayer()
	}
}

// nodeFallback retrieves a trie node when the normal diff layer walk fails
// due to concurrent layer flattening (cap).
//
// During pipelined SRC, the background SRC goroutine's CommitWithUpdate can
// trigger cap() which flattens bottom diff layers into a new disk layer,
// marking the old disk layer as stale. Concurrently, the prefetcher's trie
// walk may reach this stale disk layer and get errSnapshotStale.
//
// Unlike accountFallback/storageFallback — whose primary attempt is the
// lookup-index path, making an entry-layer retry a genuinely different
// second strategy — nodeWalk's primary attempt already IS the entry-layer
// walk, so retrying r.layer.node would deterministically hit the same stale
// disk layer. Go straight to tree.bottom(), the current base disk layer,
// which holds the flattened data and is guaranteed non-stale.
func (r *reader) nodeFallback(owner common.Hash, path []byte) ([]byte, common.Hash, nodeLoc, error) {
	return r.db.tree.bottom().node(owner, path, 0)
}

// AccountRLP directly retrieves the account associated with a particular hash.
// An error will be returned if the read operation exits abnormally. Specifically,
// if the layer is already stale.
//
// Note:
// - the returned account data is not a copy, please don't modify it
// - no error will be returned if the requested account is not found in database
func (r *reader) AccountRLP(hash common.Hash) ([]byte, error) {
	l, err := r.db.tree.lookupAccount(hash, r.state)
	if err != nil {
		// A lookup rejection is authoritative: accountTip only fails when the
		// reader's state is neither the disk layer nor a descendant of it, so
		// the disk layer's flat data does not correspond to the requested
		// state. Surface the staleness rather than serving another state's
		// values — flat reads carry no hash to catch the substitution.
		return nil, err
	}
	// If the located layer is stale, fall back to the slow path to retrieve
	// the account data. This is an edge case where the located layer is the
	// disk layer (e.g., the requested account was not changed in all the diff
	// layers), and it becomes stale within a very short time window.
	//
	// This fallback mechanism is essential, because the traversal starts from
	// the entry point layer and goes down, the staleness of the disk layer does
	// not affect the result unless the entry point layer is also stale.
	blob, err := l.account(hash, 0)
	if errors.Is(err, errSnapshotStale) {
		return r.accountFallback(hash)
	}
	return blob, err
}

// accountFallback retrieves account data when the located layer goes stale
// between the lookup and the read — the disk layer was the lookup tip and
// concurrent flattening (cap/persist) replaced it. It tries the reader's
// entry-point layer first (still in memory), then the current base disk layer.
// The base fallback is needed because persist() creates intermediate disk
// layers that are marked stale during recursive flattening — only the final
// base layer is guaranteed non-stale.
//
// The base is used only when it is an ancestor of (or equal to) the reader's
// state. The lookup already proved no diff layer between the disk layer and
// the reader's state touched this account, so an ancestor base holds the same
// value the reader's state would; a base that has advanced past the reader's
// state may have absorbed a newer write, and returning that would be silently
// wrong — flat reads have no hash check to catch it.
func (r *reader) accountFallback(hash common.Hash) ([]byte, error) {
	blob, err := r.layer.account(hash, 0)
	if errors.Is(err, errSnapshotStale) {
		base := r.db.tree.bottomIfAncestorOf(r.state)
		if base == nil {
			return nil, fmt.Errorf("[%#x] %w", r.state, errSnapshotStale)
		}
		return base.account(hash, 0)
	}
	return blob, err
}

// Account directly retrieves the account associated with a particular hash in
// the slim data format. An error will be returned if the read operation exits
// abnormally. Specifically, if the layer is already stale.
//
// Note:
// - the returned account object is safe to modify
// - no error will be returned if the requested account is not found in database
func (r *reader) Account(hash common.Hash) (*types.SlimAccount, error) {
	blob, err := r.AccountRLP(hash)
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, nil
	}
	account := new(types.SlimAccount)
	if err := rlp.DecodeBytes(blob, account); err != nil {
		panic(err)
	}
	return account, nil
}

// Storage directly retrieves the storage data associated with a particular hash,
// within a particular account. An error will be returned if the read operation
// exits abnormally. Specifically, if the layer is already stale.
//
// Note:
// - the returned storage data is not a copy, please don't modify it
// - no error will be returned if the requested slot is not found in database
func (r *reader) Storage(accountHash, storageHash common.Hash) ([]byte, error) {
	l, err := r.db.tree.lookupStorage(accountHash, storageHash, r.state)
	if err != nil {
		// Authoritative rejection — see the equivalent comment in AccountRLP.
		return nil, err
	}
	// If the located layer is stale, fall back to the slow path to retrieve
	// the storage data. This is an edge case where the located layer is the
	// disk layer (e.g., the requested account was not changed in all the diff
	// layers), and it becomes stale within a very short time window.
	//
	// This fallback mechanism is essential, because the traversal starts from
	// the entry point layer and goes down, the staleness of the disk layer does
	// not affect the result unless the entry point layer is also stale.
	blob, err := l.storage(accountHash, storageHash, 0)
	if errors.Is(err, errSnapshotStale) {
		return r.storageFallback(accountHash, storageHash)
	}
	return blob, err
}

// storageFallback is the storage counterpart of accountFallback, including the
// ancestor check on the base disk layer.
func (r *reader) storageFallback(accountHash, storageHash common.Hash) ([]byte, error) {
	blob, err := r.layer.storage(accountHash, storageHash, 0)
	if errors.Is(err, errSnapshotStale) {
		base := r.db.tree.bottomIfAncestorOf(r.state)
		if base == nil {
			return nil, fmt.Errorf("[%#x] %w", r.state, errSnapshotStale)
		}
		return base.storage(accountHash, storageHash, 0)
	}
	return blob, err
}

// NodeReader retrieves a layer belonging to the given state root.
func (db *Database) NodeReader(root common.Hash) (database.NodeReader, error) {
	layer := db.tree.get(root)
	if layer == nil {
		return nil, fmt.Errorf("state %#x is not available", root)
	}
	return &reader{
		db:          db,
		state:       root,
		noHashCheck: db.isVerkle,
		layer:       layer,
	}, nil
}

// StateReader returns a reader that allows access to the state data associated
// with the specified state.
func (db *Database) StateReader(root common.Hash) (database.StateReader, error) {
	layer := db.tree.get(root)
	if layer == nil {
		return nil, fmt.Errorf("state %#x is not available", root)
	}
	return &reader{
		db:    db,
		state: root,
		layer: layer,
	}, nil
}

// HistoricalStateReader is a wrapper over history reader, providing access to
// historical state.
type HistoricalStateReader struct {
	db     *Database
	reader *stateHistoryReader
	id     uint64
}

// HistoricReader constructs a reader for accessing the requested historic state.
func (db *Database) HistoricReader(root common.Hash) (*HistoricalStateReader, error) {
	// Bail out if the state history hasn't been fully indexed
	if db.stateIndexer == nil || db.stateFreezer == nil {
		return nil, fmt.Errorf("historical state %x is not available", root)
	}
	if !db.stateIndexer.inited() {
		return nil, errors.New("state histories haven't been fully indexed yet")
	}
	// - States at the current disk layer or above are directly accessible
	//   via `db.StateReader`.
	//
	// - States older than the current disk layer (including the disk layer
	//   itself) are available via `db.HistoricReader`.
	id := rawdb.ReadStateID(db.diskdb, root)
	if id == nil {
		return nil, fmt.Errorf("state %#x is not available", root)
	}
	// Ensure the requested state is canonical, historical states on side chain
	// are not accessible.
	meta, err := readStateHistoryMeta(db.stateFreezer, *id+1)
	if err != nil {
		return nil, err // e.g., the referred state history has been pruned
	}
	if meta.parent != root {
		return nil, fmt.Errorf("state %#x is not canonincal", root)
	}
	return &HistoricalStateReader{
		id:     *id,
		db:     db,
		reader: newStateHistoryReader(db.diskdb, db.stateFreezer),
	}, nil
}

// AccountRLP directly retrieves the account RLP associated with a particular
// address in the slim data format. An error will be returned if the read
// operation exits abnormally. Specifically, if the layer is already stale.
//
// Note:
// - the returned account is not a copy, please don't modify it.
// - no error will be returned if the requested account is not found in database.
func (r *HistoricalStateReader) AccountRLP(address common.Address) ([]byte, error) {
	defer func(start time.Time) {
		historicalAccountReadTimer.UpdateSince(start)
	}(time.Now())

	// TODO(rjl493456442): Theoretically, the obtained disk layer could become stale
	// within a very short time window.
	//
	// While reading the account data while holding `db.tree.lock` can resolve
	// this issue, but it will introduce a heavy contention over the lock.
	//
	// Let's optimistically assume the situation is very unlikely to happen,
	// and try to define a low granularity lock if the current approach doesn't
	// work later.
	dl := r.db.tree.bottom()
	hash := crypto.Keccak256Hash(address.Bytes())
	latest, err := dl.account(hash, 0)
	if err != nil {
		return nil, err
	}
	return r.reader.read(newAccountIdentQuery(address, hash), r.id, dl.stateID(), latest)
}

// Account directly retrieves the account associated with a particular address in
// the slim data format. An error will be returned if the read operation exits
// abnormally. Specifically, if the layer is already stale.
//
// No error will be returned if the requested account is not found in database
func (r *HistoricalStateReader) Account(address common.Address) (*types.SlimAccount, error) {
	blob, err := r.AccountRLP(address)
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, nil
	}
	account := new(types.SlimAccount)
	if err := rlp.DecodeBytes(blob, account); err != nil {
		panic(err)
	}
	return account, nil
}

// Storage directly retrieves the storage data associated with a particular key,
// within a particular account. An error will be returned if the read operation
// exits abnormally. Specifically, if the layer is already stale.
//
// Note:
// - the returned storage data is not a copy, please don't modify it.
// - no error will be returned if the requested slot is not found in database.
func (r *HistoricalStateReader) Storage(address common.Address, key common.Hash) ([]byte, error) {
	defer func(start time.Time) {
		historicalStorageReadTimer.UpdateSince(start)
	}(time.Now())

	// TODO(rjl493456442): Theoretically, the obtained disk layer could become stale
	// within a very short time window.
	//
	// While reading the account data while holding `db.tree.lock` can resolve
	// this issue, but it will introduce a heavy contention over the lock.
	//
	// Let's optimistically assume the situation is very unlikely to happen,
	// and try to define a low granularity lock if the current approach doesn't
	// work later.
	dl := r.db.tree.bottom()
	addrHash := crypto.Keccak256Hash(address.Bytes())
	keyHash := crypto.Keccak256Hash(key.Bytes())
	latest, err := dl.storage(addrHash, keyHash, 0)
	if err != nil {
		return nil, err
	}
	return r.reader.read(newStorageIdentQuery(address, addrHash, key, keyHash), r.id, dl.stateID(), latest)
}

// HistoricalNodeReader is a wrapper over history reader, providing access to
// historical trie node data.
type HistoricalNodeReader struct {
	db     *Database
	reader *trienodeReader
	id     uint64
}

// HistoricNodeReader constructs a reader for accessing the requested historic state.
func (db *Database) HistoricNodeReader(root common.Hash) (*HistoricalNodeReader, error) {
	// Bail out if the state history hasn't been fully indexed
	if db.trienodeIndexer == nil || db.trienodeFreezer == nil {
		return nil, fmt.Errorf("historical state %x is not available", root)
	}
	if !db.trienodeIndexer.inited() {
		return nil, errors.New("trienode histories haven't been fully indexed yet")
	}
	// - States at the current disk layer or above are directly accessible
	//   via `db.NodeReader`.
	//
	// - States older than the current disk layer (including the disk layer
	//   itself) are available via `db.HistoricalNodeReader`.
	id := rawdb.ReadStateID(db.diskdb, root)
	if id == nil {
		return nil, fmt.Errorf("state %#x is not available", root)
	}
	// Ensure the requested trienode history is canonical, states on side chain
	// are not accessible.
	meta, err := readTrienodeMetadata(db.trienodeFreezer, *id+1)
	if err != nil {
		return nil, err // e.g., the referred trienode history has been pruned
	}
	if meta.parent != root {
		return nil, fmt.Errorf("state %#x is not canonincal", root)
	}
	return &HistoricalNodeReader{
		id:     *id,
		db:     db,
		reader: newTrienodeReader(db.diskdb, db.trienodeFreezer, int(db.config.FullValueCheckpoint)),
	}, nil
}

// Node directly retrieves the trie node data associated with a particular path,
// within a particular account. An error will be returned if the read operation
// exits abnormally. Specifically, if the layer is already stale.
//
// Note:
// - the returned trie node data is not a copy, please don't modify it.
// - an error will be returned if the requested trie node is not found in database.
func (r *HistoricalNodeReader) Node(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	defer func(start time.Time) {
		historicalTrienodeReadTimer.UpdateSince(start)
	}(time.Now())

	// TODO(rjl493456442): Theoretically, the obtained disk layer could become stale
	// within a very short time window.
	//
	// While reading the account data while holding `db.tree.lock` can resolve
	// this issue, but it will introduce a heavy contention over the lock.
	//
	// Let's optimistically assume the situation is very unlikely to happen,
	// and try to define a low granularity lock if the current approach doesn't
	// work later.
	dl := r.db.tree.bottom()
	latest, h, _, err := dl.node(owner, path, 0)
	if err != nil {
		return nil, err
	}
	if h == hash {
		return latest, nil
	}
	blob, err := r.reader.read(newTrienodeIdent(owner, string(path)), r.id, dl.stateID(), latest)
	if err != nil {
		return nil, err
	}
	// Error out if the local one is inconsistent with the target.
	if crypto.Keccak256Hash(blob) != hash {
		blobHex := "nil"
		if len(blob) > 0 {
			blobHex = hexutil.Encode(blob)
		}
		log.Error("Unexpected historical trie node", "owner", owner.Hex(), "path", path, "blob", blobHex)
		return nil, fmt.Errorf("unexpected historical trie node: (%x %v), blob: %s", owner, path, blobHex)
	}
	return blob, nil
}
