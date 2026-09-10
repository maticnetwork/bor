// Copyright 2026 The go-ethereum Authors
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
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb/database"
)

var (
	// These meters are intentionally emitted from snapshotNodeReader.Node so
	// hit/miss attribution includes every trie-node fetch SRC attempts. If this
	// shows up in CPU profiles, batch these counts per block and emit them once
	// from the import handoff instead.
	warmSnapshotAccountHitMeter  = metrics.NewRegisteredMeter("chain/imports/pipelined/warm_snapshot/account/hit", nil)
	warmSnapshotAccountMissMeter = metrics.NewRegisteredMeter("chain/imports/pipelined/warm_snapshot/account/miss", nil)
	warmSnapshotStorageHitMeter  = metrics.NewRegisteredMeter("chain/imports/pipelined/warm_snapshot/storage/hit", nil)
	warmSnapshotStorageMissMeter = metrics.NewRegisteredMeter("chain/imports/pipelined/warm_snapshot/storage/miss", nil)
)

// WarmSnapshot is an immutable, hash-verified copy of trie nodes loaded by the
// execution-side prefetcher. It is constructed in the pipelined SRC goroutine
// from a quiesced WarmSnapshotInput so SRC's NewTrieOnly reader can
// short-circuit pathdb/pebble lookups for nodes the main thread already loaded.
//
// The snapshot is read-only after construction. Concurrent readers are safe
// because a populated map is never mutated post-construction; the SRC handoff
// in persistPipelinedImport provides the happens-before edge.
type WarmSnapshot struct {
	// nodes is keyed by (owner, path, hash). Across blocks the same
	// (owner, path) can resolve to different node hashes as the trie
	// shape evolves; keying by hash too keeps every distinct warm node
	// retrievable rather than overwriting earlier entries with later
	// ones for the same path. The hash check on lookup remains the
	// authoritative correctness gate; this keying just preserves hits
	// the prefetcher actually observed.
	nodes map[warmKey][]byte
}

// warmKey identifies a trie node by its containing trie's owner (zero for
// the account trie, account hash for storage tries), its path within the
// trie, and the node's content hash — the hash disambiguates entries that
// share owner+path across different blocks/states. It is a comparable value
// type built on the stack: a string-keyed variant would allocate on every
// Lookup, since Go's string(bytes) map-index optimization does not apply to
// struct-literal keys. MPT paths are at most 64 nibbles; longer paths cannot
// be produced by the trie and are simply not indexed (Lookup misses fall
// through to the underlying reader).
type warmKey struct {
	owner   common.Hash
	hash    common.Hash
	pathLen uint8
	path    [64]byte // MPT path nibbles
}

func makeWarmKey(owner common.Hash, path []byte, hash common.Hash) (warmKey, bool) {
	if len(path) > len(warmKey{}.path) {
		return warmKey{}, false
	}
	key := warmKey{owner: owner, hash: hash, pathLen: uint8(len(path))}
	copy(key.path[:], path)
	return key, true
}

// NewWarmSnapshot constructs a snapshot from per-trie node maps already
// extracted from a quiesced prefetcher. Each entry in tries supplies a trie
// owner and a (path -> blob) map (typically the result of trie.Witness()).
// Blobs are copied; the snapshot does not retain references into the source
// maps. Empty input produces a non-nil empty snapshot — Lookup on an empty
// snapshot is a fast miss.
//
// Source maps are keyed by path only; this constructor computes each blob's
// hash and uses (owner, path, hash) as the snapshot key. If the source
// somehow contains two distinct blobs at the same (owner, path) — possible
// when the prefetcher loaded the same path at different roots within a
// single block — both are retained.
func NewWarmSnapshot(tries []TrieWarmNodes) *WarmSnapshot {
	total := 0
	for i := range tries {
		total += len(tries[i].Nodes)
	}
	s := &WarmSnapshot{nodes: make(map[warmKey][]byte, total)}
	for i := range tries {
		owner := tries[i].Owner
		for path, blob := range tries[i].Nodes {
			if len(blob) == 0 {
				continue
			}
			cp := make([]byte, len(blob))
			copy(cp, blob)
			key, ok := makeWarmKey(owner, []byte(path), crypto.Keccak256Hash(cp))
			if !ok {
				continue
			}
			s.nodes[key] = cp
		}
	}
	return s
}

// TrieWarmNodes carries one trie's contribution to a WarmSnapshot. The Owner
// is the trie's identifying hash (zero for the account trie, the account hash
// for a storage trie). Nodes maps trie-path to RLP-encoded node blob.
type TrieWarmNodes struct {
	Owner common.Hash
	Nodes map[string][]byte
}

// WarmSnapshotInput is the quiesced handoff from the execution-side
// prefetcher to SRC. It contains cloned path->blob maps returned by
// Trie.Witness() after all subfetcher goroutines have exited. The maps are
// read-only after construction and may be passed to another goroutine.
//
// Build constructs the final immutable, hash-indexed WarmSnapshot. Keeping
// this as a separate step lets the import thread stop and detach the
// prefetcher quickly while SRC pays the copy/hash/index cost in the background.
type WarmSnapshotInput struct {
	tries []TrieWarmNodes
}

// NewWarmSnapshotInput wraps quiesced trie-node maps for later WarmSnapshot
// construction. It does not copy blobs or compute hashes; callers must only
// pass maps that will not be mutated after this point.
func NewWarmSnapshotInput(tries []TrieWarmNodes) *WarmSnapshotInput {
	if len(tries) == 0 {
		return nil
	}
	return &WarmSnapshotInput{tries: tries}
}

// Build constructs the immutable WarmSnapshot from the input. The returned
// snapshot owns copies of all node blobs and does not alias the input maps.
func (in *WarmSnapshotInput) Build() *WarmSnapshot {
	if in == nil || len(in.tries) == 0 {
		return nil
	}
	return NewWarmSnapshot(in.tries)
}

// Len returns the number of nodes in the snapshot. Useful for tests and
// metrics; safe to call on a nil snapshot.
func (s *WarmSnapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.nodes)
}

// SizeBytes returns the total retained trie-node blob bytes. It intentionally
// excludes map/key overhead; use it as a stable payload-size signal rather than
// a precise heap-size estimate.
func (s *WarmSnapshot) SizeBytes() int {
	if s == nil {
		return 0
	}
	var size int
	for _, blob := range s.nodes {
		size += len(blob)
	}
	return size
}

// Lookup returns the cached trie-node blob for (owner, path, expectedHash) if
// present. A miss returns (nil, false) and the caller is expected to fall
// through to the underlying NodeReader.
//
// The map is keyed by the (owner, path, hash) triple, so a present entry
// already has a verified hash by construction (see NewWarmSnapshot); the
// expectedHash supplied by the caller becomes part of the lookup key
// itself, which means a request for a different hash at the same
// (owner, path) is a structural miss — no stored entry can satisfy it
// regardless of contents. This is the linearisation point that prevents a
// stale snapshot entry from satisfying a different state's read.
func (s *WarmSnapshot) Lookup(owner common.Hash, path []byte, expectedHash common.Hash) ([]byte, bool) {
	if s == nil || len(s.nodes) == 0 {
		return nil, false
	}
	key, ok := makeWarmKey(owner, path, expectedHash)
	if !ok {
		return nil, false
	}
	blob, ok := s.nodes[key]
	if !ok {
		return nil, false
	}
	return blob, true
}

// snapshotStateDatabase wraps CachingDB for a single SRC StateDB so every trie
// opening path can consult the same WarmSnapshot. The plain snapshot reader
// wrapper is enough for StateDB.reader reads, but CommitWithUpdate also opens
// tries through StateDB.db.OpenTrie/OpenStorageTrie. If those methods keep
// using the unwrapped CachingDB, the commit and witness-collection walks miss
// the warm handoff entirely.
//
// The wrapper preserves NewTrieOnly semantics: account and storage reads still
// walk MPT tries, and trie resolveAndTrack still records proof nodes. The only
// change is the NodeDatabase behind those tries: snapshot hits return an
// already-loaded RLP node blob, while misses fall through to the underlying
// triedb/pathdb chain.
type snapshotStateDatabase struct {
	*CachingDB

	nodeDB   database.NodeDatabase
	snapshot *WarmSnapshot
}

func newSnapshotStateDatabase(inner *CachingDB, snapshot *WarmSnapshot) *snapshotStateDatabase {
	return &snapshotStateDatabase{
		CachingDB: inner,
		nodeDB:    newSnapshotNodeDatabase(inner.triedb, snapshot),
		snapshot:  snapshot,
	}
}

// Reader intentionally returns a trie-only snapshot-aware reader, not
// CachingDB.Reader's multi-reader. This wrapper is meant for short-lived SRC
// StateDB instances that are discarded after CommitWithUpdate; do not reuse it
// for long-lived StateDBs that expect flat/snapshot reader semantics after
// commit-time reader refreshes.
func (db *snapshotStateDatabase) Reader(stateRoot common.Hash) (Reader, error) {
	tr, err := newTrieReaderWithSnapshot(stateRoot, db.triedb, db.nodeDB)
	if err != nil {
		return nil, err
	}
	return newReader(newCachingCodeReader(db.disk, db.codeCache, db.codeSizeCache), tr), nil
}

func (db *snapshotStateDatabase) OpenTrie(root common.Hash) (Trie, error) {
	if db.triedb.IsVerkle() {
		return db.CachingDB.OpenTrie(root)
	}
	tr, err := trie.NewStateTrie(trie.StateTrieID(root), db.nodeDB)
	if err != nil {
		return nil, err
	}
	return tr, nil
}

func (db *snapshotStateDatabase) OpenStorageTrie(stateRoot common.Hash, address common.Address, root common.Hash, self Trie) (Trie, error) {
	if db.triedb.IsVerkle() {
		return self, nil
	}
	tr, err := trie.NewStateTrie(trie.StorageTrieID(stateRoot, crypto.Keccak256Hash(address.Bytes()), root), db.nodeDB)
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// preimageForwarder is the interface trie.NewStateTrie checks at construction
// time to decide whether the trie has a backing preimage store (see
// trie/secure_trie.go's preimageStore type assertion). Mirroring it here
// lets snapshotNodeDatabase satisfy the same shape so wrapped tries don't
// silently lose preimage support.
//
// We declare it locally rather than importing trie's unexported preimageStore
// because: (a) trie's interface is unexported, and (b) Go's structural typing
// makes a type-equivalent local declaration sufficient — the type assertion
// inside trie.NewStateTrie will succeed against any value that has these
// three methods.
type preimageForwarder interface {
	Preimage(hash common.Hash) []byte
	InsertPreimage(preimages map[common.Hash][]byte)
	PreimageEnabled() bool
}

// snapshotNodeDatabase wraps a database.NodeDatabase so that NodeReaders
// returned from it consult a WarmSnapshot before falling through to the
// underlying reader. It is the boundary at which the SRC goroutine's trie
// reads bypass pathdb/pebble for warm nodes.
//
// trie.Reader.Node(path, hash) calls the wrapped NodeReader once per node
// fetch, supplying owner via its internal field and path/hash from the
// caller. The wrapper consults the snapshot using exactly that triple. On
// miss or hash mismatch, the underlying NodeReader is invoked unchanged, so
// trie.Trie.resolveAndTrack and the trie's prevalueTracer record the served
// node regardless of whether it came from the snapshot or pathdb. Witness
// completeness under NewTrieOnly semantics is therefore preserved.
//
// snapshotNodeDatabase also forwards preimage methods (Preimage,
// InsertPreimage, PreimageEnabled) when the inner database supports them.
// trie.NewStateTrie type-asserts the supplied NodeDatabase to detect a
// preimage store; without forwarding, wrapped tries would silently lose
// preimage recording even though the underlying *triedb.Database supports it.
type snapshotNodeDatabase struct {
	inner    database.NodeDatabase
	snapshot *WarmSnapshot

	// preimages is the inner database's preimage interface, captured at
	// construction iff the inner database implements it. Nil when the
	// underlying database does not record preimages, which preserves the
	// "preimages disabled" branch in trie.NewStateTrie's type assertion.
	preimages preimageForwarder
}

// newSnapshotNodeDatabase wraps inner with the given snapshot. If snapshot is
// nil or empty, returns inner unchanged so callers can pass through without
// allocating a wrapper.
func newSnapshotNodeDatabase(inner database.NodeDatabase, snapshot *WarmSnapshot) database.NodeDatabase {
	if snapshot == nil || snapshot.Len() == 0 {
		return inner
	}
	wrapped := &snapshotNodeDatabase{inner: inner, snapshot: snapshot}
	if pi, ok := inner.(preimageForwarder); ok {
		wrapped.preimages = pi
	}
	return wrapped
}

func (db *snapshotNodeDatabase) NodeReader(stateRoot common.Hash) (database.NodeReader, error) {
	r, err := db.inner.NodeReader(stateRoot)
	if err != nil {
		return nil, err
	}
	return &snapshotNodeReader{inner: r, snapshot: db.snapshot}, nil
}

// Preimage forwards to the inner preimage store. Trie callers reach this via
// the preimageStore type assertion inside trie.NewStateTrie; if the inner
// database had no preimage support, this method returns nil (the trie's
// PreimageEnabled() check below returns false, so the trie won't install us
// as its preimage store and these methods will not be invoked).
func (db *snapshotNodeDatabase) Preimage(hash common.Hash) []byte {
	if db.preimages == nil {
		return nil
	}
	return db.preimages.Preimage(hash)
}

// InsertPreimage forwards a preimage batch to the inner store. Called by
// trie.StateTrie.Commit when secKeyCache is non-empty and PreimageEnabled
// returned true at construction time.
func (db *snapshotNodeDatabase) InsertPreimage(preimages map[common.Hash][]byte) {
	if db.preimages == nil {
		return
	}
	db.preimages.InsertPreimage(preimages)
}

// PreimageEnabled reports whether the underlying database has preimage
// recording enabled. Returning false makes trie.NewStateTrie skip the
// preimage-store linkage for the wrapped trie — same outcome as if the
// caller had passed an unwrapped *triedb.Database with preimages off.
func (db *snapshotNodeDatabase) PreimageEnabled() bool {
	if db.preimages == nil {
		return false
	}
	return db.preimages.PreimageEnabled()
}

// snapshotNodeReader is the per-state-root NodeReader that consults the
// snapshot first. Hits avoid pathdb diff-layer walks and pebble I/O entirely;
// misses fall through to the underlying reader without modification.
type snapshotNodeReader struct {
	inner    database.NodeReader
	snapshot *WarmSnapshot
}

func (r *snapshotNodeReader) Node(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	if blob, ok := r.snapshot.Lookup(owner, path, hash); ok {
		if owner == (common.Hash{}) {
			warmSnapshotAccountHitMeter.Mark(1)
		} else {
			warmSnapshotStorageHitMeter.Mark(1)
		}
		return blob, nil
	}
	if owner == (common.Hash{}) {
		warmSnapshotAccountMissMeter.Mark(1)
	} else {
		warmSnapshotStorageMissMeter.Mark(1)
	}
	return r.inner.Node(owner, path, hash)
}
