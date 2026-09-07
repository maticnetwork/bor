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
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/database"
)

// stubNodeReader records every call made to it and returns a configured blob
// per (owner, path) pair. It lets us assert exactly when the snapshot wrapper
// falls through to the underlying NodeReader.
type stubNodeReader struct {
	calls []stubCall
	nodes map[stubKey]stubNode
}

type stubCall struct {
	owner common.Hash
	path  []byte
	hash  common.Hash
}

type stubKey struct {
	owner common.Hash
	path  string
}

type stubNode struct {
	blob []byte
	err  error
}

func newStubNodeReader() *stubNodeReader {
	return &stubNodeReader{nodes: make(map[stubKey]stubNode)}
}

func (s *stubNodeReader) set(owner common.Hash, path []byte, blob []byte) {
	s.nodes[stubKey{owner: owner, path: string(path)}] = stubNode{blob: blob}
}

func (s *stubNodeReader) Node(owner common.Hash, path []byte, hash common.Hash) ([]byte, error) {
	s.calls = append(s.calls, stubCall{owner: owner, path: append([]byte(nil), path...), hash: hash})
	n, ok := s.nodes[stubKey{owner: owner, path: string(path)}]
	if !ok {
		return nil, errors.New("stub: not found")
	}
	if n.err != nil {
		return nil, n.err
	}
	return n.blob, nil
}

// stubNodeDB satisfies database.NodeDatabase by returning a fixed
// stubNodeReader regardless of state root. It exists only to drive
// snapshotNodeDatabase in tests.
type stubNodeDB struct {
	reader *stubNodeReader
}

func (s *stubNodeDB) NodeReader(stateRoot common.Hash) (database.NodeReader, error) {
	return s.reader, nil
}

type errorNodeDB struct {
	err error
}

func (s *errorNodeDB) NodeReader(common.Hash) (database.NodeReader, error) {
	return nil, s.err
}

type preimageNodeDB struct {
	*stubNodeDB
	preimages map[common.Hash][]byte
	enabled   bool
}

func (db *preimageNodeDB) Preimage(hash common.Hash) []byte {
	return db.preimages[hash]
}

func (db *preimageNodeDB) InsertPreimage(preimages map[common.Hash][]byte) {
	for hash, preimage := range preimages {
		db.preimages[hash] = preimage
	}
}

func (db *preimageNodeDB) PreimageEnabled() bool {
	return db.enabled
}

// TestWarmSnapshot_HashMismatchFallsThrough is the consensus-critical safety
// test for the snapshot reader. The snapshot is keyed by (owner, path, hash);
// the caller-supplied expectedHash participates in the lookup key, so a
// request for a different hash at the same (owner, path) is a structural
// miss and the reader must fall through to the authoritative pathdb-backed
// reader rather than serve a blob whose hash does not match what the caller
// expects.
//
// Failing this test means a stale snapshot entry could satisfy a current
// trie read with the wrong blob — a silent state-corruption / consensus
// risk. The (owner, path, hash) keying is the structural guarantee that
// prevents that, and the underlying-reader fallthrough is its observable
// consequence.
func TestWarmSnapshot_HashMismatchFallsThrough(t *testing.T) {
	owner := common.HexToHash("0x01")
	path := []byte{0xab, 0xcd}

	correctBlob := []byte("trie-node-correct")
	staleBlob := []byte("trie-node-stale-from-old-state")

	correctHash := crypto.Keccak256Hash(correctBlob)
	staleHash := crypto.Keccak256Hash(staleBlob)
	require.NotEqual(t, correctHash, staleHash, "test setup: stale and correct blobs must hash differently")

	// Snapshot contains the stale blob keyed by (owner, path).
	snap := NewWarmSnapshot([]TrieWarmNodes{{
		Owner: owner,
		Nodes: map[string][]byte{string(path): staleBlob},
	}})
	require.Equal(t, 1, snap.Len())

	// Underlying reader has the correct blob.
	underlying := newStubNodeReader()
	underlying.set(owner, path, correctBlob)

	wrappedDB := newSnapshotNodeDatabase(&stubNodeDB{reader: underlying}, snap)
	reader, err := wrappedDB.NodeReader(common.Hash{}) // root is irrelevant for stub
	require.NoError(t, err)

	// Caller asks for the CORRECT hash. The snapshot's only entry is keyed
	// by (owner, path, staleHash), so a lookup with (owner, path,
	// correctHash) is a structural miss and the wrapper must fall through
	// to the underlying reader.
	got, err := reader.Node(owner, path, correctHash)
	require.NoError(t, err)
	require.Equal(t, correctBlob, got, "must serve from underlying reader, not the stale snapshot blob")
	require.Len(t, underlying.calls, 1, "underlying reader must be invoked exactly once on hash-mismatch fallthrough")

	// Sanity: when the caller asks for staleHash, the lookup key matches
	// the stored entry exactly and the snapshot serves without consulting
	// the underlying reader. This shows the hash component of the key is
	// what distinguishes hit from miss at the same (owner, path).
	got, err = reader.Node(owner, path, staleHash)
	require.NoError(t, err)
	require.Equal(t, staleBlob, got, "snapshot must serve when expectedHash matches stored hash")
	require.Len(t, underlying.calls, 1, "underlying reader must NOT be invoked on a snapshot hit")
}

// TestWarmSnapshot_NilAndEmpty exercises the no-op paths: a nil snapshot or
// an empty snapshot must always return a miss and let every read fall through
// to the underlying reader. Constructing the snapshot wrapper should be free
// in those cases.
func TestWarmSnapshot_NilAndEmpty(t *testing.T) {
	owner := common.HexToHash("0x02")
	path := []byte{0x10}
	blob := []byte("real-node")
	hash := crypto.Keccak256Hash(blob)

	underlying := newStubNodeReader()
	underlying.set(owner, path, blob)

	innerDB := &stubNodeDB{reader: underlying}

	// Nil snapshot: wrapper short-circuits, returns inner DB unchanged.
	require.Same(t, innerDB, newSnapshotNodeDatabase(innerDB, nil))

	// Empty snapshot: same short-circuit.
	empty := NewWarmSnapshot(nil)
	require.Equal(t, 0, empty.Len())
	require.Same(t, innerDB, newSnapshotNodeDatabase(innerDB, empty))

	// Direct Lookup on nil snapshot is a miss.
	var nilSnap *WarmSnapshot
	_, ok := nilSnap.Lookup(owner, path, hash)
	require.False(t, ok)

	// Direct Lookup on empty snapshot is a miss.
	_, ok = empty.Lookup(owner, path, hash)
	require.False(t, ok)
}

// TestNewTrieOnlyWithSnapshotInstallsStateDBWrapper verifies that the snapshot
// handoff is installed on StateDB.db, not only on the initial Reader. Commit
// paths call StateDB.db.OpenTrie/OpenStorageTrie directly; if db remained the
// plain CachingDB those calls would bypass WarmSnapshot.
func TestNewTrieOnlyWithSnapshotInstallsStateDBWrapper(t *testing.T) {
	cdb := NewDatabaseForTesting()
	snap := NewWarmSnapshot([]TrieWarmNodes{{
		Owner: common.Hash{},
		Nodes: map[string][]byte{"warm": []byte("warm-node")},
	}})
	require.Equal(t, 1, snap.Len())

	sdb, err := NewTrieOnlyWithSnapshot(types.EmptyRootHash, cdb, snap)
	require.NoError(t, err)
	_, ok := sdb.db.(*snapshotStateDatabase)
	require.True(t, ok, "StateDB.db must be snapshot-aware so commit-time trie opens use the warm handoff")

	sdb, err = NewTrieOnlyWithSnapshot(types.EmptyRootHash, cdb, nil)
	require.NoError(t, err)
	_, ok = sdb.db.(*snapshotStateDatabase)
	require.False(t, ok, "nil snapshot must preserve the plain trie-only database path")
}

// TestWarmSnapshot_RetainsDistinctHashesAtSamePath verifies that two warm
// nodes with the same (owner, path) but different hashes are both retained.
// Across blocks, root churn means the same trie position can resolve to
// different node hashes; if the snapshot collapsed entries at the (owner,
// path) level, an entry with a different hash would silently overwrite an
// earlier one and the SRC would miss when its expected hash matched the
// dropped entry. Triple keying by (owner, path, hash) prevents that
// loss-of-hits scenario.
func TestWarmSnapshot_RetainsDistinctHashesAtSamePath(t *testing.T) {
	owner := common.HexToHash("0xa1")
	path := []byte{0x42}

	blobA := []byte("trie-node-A")
	blobB := []byte("trie-node-B")
	hashA := crypto.Keccak256Hash(blobA)
	hashB := crypto.Keccak256Hash(blobB)
	require.NotEqual(t, hashA, hashB)

	// Same (owner, path), two different blobs (different hashes).
	// Source map keyed by path only, so we have to materialise both as
	// separate TrieWarmNodes entries: each call to NewWarmSnapshot inserts
	// every (owner, path, hash) it sees — duplicates only collapse if the
	// triple is identical, not just (owner, path).
	snap := NewWarmSnapshot([]TrieWarmNodes{
		{Owner: owner, Nodes: map[string][]byte{string(path): blobA}},
		{Owner: owner, Nodes: map[string][]byte{string(path): blobB}},
	})
	require.Equal(t, 2, snap.Len(), "both entries must be retained when hashes differ at same (owner, path)")

	gotA, ok := snap.Lookup(owner, path, hashA)
	require.True(t, ok, "lookup with hashA must hit blobA")
	require.Equal(t, blobA, gotA)

	gotB, ok := snap.Lookup(owner, path, hashB)
	require.True(t, ok, "lookup with hashB must hit blobB")
	require.Equal(t, blobB, gotB)

	// A third hash that nobody supplied: structural miss.
	other := crypto.Keccak256Hash([]byte("never-seen"))
	_, ok = snap.Lookup(owner, path, other)
	require.False(t, ok)
}

// TestWarmSnapshot_OwnerScoped verifies that the (owner, path) keying
// distinguishes account-trie nodes from storage-trie nodes that may share a
// path. Without owner scoping a storage-trie lookup could be satisfied by an
// account-trie node at the same path, which would still pass the hash check
// only if the blobs collided — but the keying-level isolation is the
// structural guarantee.
func TestWarmSnapshot_OwnerScoped(t *testing.T) {
	accountOwner := common.Hash{}
	storageOwner := common.HexToHash("0xfeedface")
	path := []byte{0x07}

	accountBlob := []byte("account-trie-node")
	storageBlob := []byte("storage-trie-node-different-content")

	snap := NewWarmSnapshot([]TrieWarmNodes{
		{Owner: accountOwner, Nodes: map[string][]byte{string(path): accountBlob}},
		{Owner: storageOwner, Nodes: map[string][]byte{string(path): storageBlob}},
	})
	require.Equal(t, 2, snap.Len())

	// Account-trie lookup serves the account blob.
	got, ok := snap.Lookup(accountOwner, path, crypto.Keccak256Hash(accountBlob))
	require.True(t, ok)
	require.Equal(t, accountBlob, got)

	// Storage-trie lookup at the same path serves the storage blob.
	got, ok = snap.Lookup(storageOwner, path, crypto.Keccak256Hash(storageBlob))
	require.True(t, ok)
	require.Equal(t, storageBlob, got)

	// Cross-owner lookup with the wrong-owner-but-matching-path is a miss.
	_, ok = snap.Lookup(storageOwner, path, crypto.Keccak256Hash(accountBlob))
	require.False(t, ok, "must not serve account blob to storage owner even when that blob's hash matches expectedHash")
}

func TestWarmSnapshotInputAndBounds(t *testing.T) {
	t.Parallel()

	owner := common.HexToHash("0x44")
	path := []byte{0x01, 0x02}
	blob := []byte("warm-node")
	tooLong := make([]byte, 65)

	key, ok := makeWarmKey(owner, path, crypto.Keccak256Hash(blob))
	require.True(t, ok)
	require.Equal(t, uint8(len(path)), key.pathLen)
	_, ok = makeWarmKey(owner, tooLong, common.Hash{})
	require.False(t, ok)

	require.Nil(t, NewWarmSnapshotInput(nil))
	var nilInput *WarmSnapshotInput
	require.Nil(t, nilInput.Build())

	input := NewWarmSnapshotInput([]TrieWarmNodes{{
		Owner: owner,
		Nodes: map[string][]byte{
			string(path):    blob,
			"empty":         nil,
			string(tooLong): []byte("ignored"),
		},
	}})
	snapshot := input.Build()
	require.NotNil(t, snapshot)
	require.Equal(t, 1, snapshot.Len())
	require.Equal(t, len(blob), snapshot.SizeBytes())

	got, ok := snapshot.Lookup(owner, path, crypto.Keccak256Hash(blob))
	require.True(t, ok)
	require.Equal(t, blob, got)
	blob[0] = 'X'
	require.Equal(t, []byte("warm-node"), got)
	_, ok = snapshot.Lookup(owner, tooLong, common.Hash{})
	require.False(t, ok)

	var nilSnapshot *WarmSnapshot
	require.Zero(t, nilSnapshot.Len())
	require.Zero(t, nilSnapshot.SizeBytes())
}

func TestSnapshotStateDatabaseTrieOperations(t *testing.T) {
	cdb := NewDatabaseForTesting()
	snapshot := NewWarmSnapshot([]TrieWarmNodes{{
		Owner: common.Hash{},
		Nodes: map[string][]byte{"warm": []byte("node")},
	}})
	db := newSnapshotStateDatabase(cdb, snapshot)

	_, err := db.Reader(common.HexToHash("0xdead"))
	require.Error(t, err)

	accountTrie, err := db.OpenTrie(types.EmptyRootHash)
	require.NoError(t, err)
	require.NotNil(t, accountTrie)
	accountTrie, err = db.OpenTrie(common.HexToHash("0xdead"))
	require.Error(t, err)
	require.Nil(t, accountTrie)

	storageTrie, err := db.OpenStorageTrie(types.EmptyRootHash, common.HexToAddress("0x1"), types.EmptyRootHash, nil)
	require.NoError(t, err)
	require.NotNil(t, storageTrie)
	storageTrie, err = db.OpenStorageTrie(types.EmptyRootHash, common.HexToAddress("0x1"), common.HexToHash("0xdead"), nil)
	require.Error(t, err)
	require.Nil(t, storageTrie)
}

func TestSnapshotStateDatabaseVerkleTrieOperations(t *testing.T) {
	cdb := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), triedb.VerkleDefaults), nil)
	db := newSnapshotStateDatabase(cdb, NewWarmSnapshot(nil))

	accountTrie, err := db.OpenTrie(types.EmptyRootHash)
	require.NoError(t, err)
	require.NotNil(t, accountTrie)

	self := accountTrie
	storageTrie, err := db.OpenStorageTrie(types.EmptyRootHash, common.HexToAddress("0x1"), types.EmptyRootHash, self)
	require.NoError(t, err)
	require.Same(t, self, storageTrie)
}

func TestSnapshotNodeDatabaseForwardingAndErrors(t *testing.T) {
	snapshot := NewWarmSnapshot([]TrieWarmNodes{{
		Owner: common.Hash{},
		Nodes: map[string][]byte{"warm": []byte("node")},
	}})
	expected := errors.New("reader failed")
	wrapped := newSnapshotNodeDatabase(&errorNodeDB{err: expected}, snapshot)
	_, err := wrapped.NodeReader(common.Hash{})
	require.ErrorIs(t, err, expected)

	withoutPreimages := wrapped.(*snapshotNodeDatabase)
	require.Nil(t, withoutPreimages.Preimage(common.Hash{}))
	withoutPreimages.InsertPreimage(map[common.Hash][]byte{common.HexToHash("0x1"): []byte("ignored")})
	require.False(t, withoutPreimages.PreimageEnabled())

	inner := &preimageNodeDB{
		stubNodeDB: &stubNodeDB{reader: newStubNodeReader()},
		preimages:  make(map[common.Hash][]byte),
		enabled:    true,
	}
	withPreimages := newSnapshotNodeDatabase(inner, snapshot).(*snapshotNodeDatabase)
	hash := common.HexToHash("0x2")
	withPreimages.InsertPreimage(map[common.Hash][]byte{hash: []byte("preimage")})
	require.Equal(t, []byte("preimage"), withPreimages.Preimage(hash))
	require.True(t, withPreimages.PreimageEnabled())
}

func TestSnapshotNodeReaderAccountMetricsPaths(t *testing.T) {
	path := []byte{0x1}
	blob := []byte("account-node")
	hash := crypto.Keccak256Hash(blob)
	snapshot := NewWarmSnapshot([]TrieWarmNodes{{
		Owner: common.Hash{},
		Nodes: map[string][]byte{string(path): blob},
	}})
	underlying := newStubNodeReader()
	fallback := []byte("fallback")
	underlying.set(common.Hash{}, path, fallback)
	reader := &snapshotNodeReader{inner: underlying, snapshot: snapshot}

	got, err := reader.Node(common.Hash{}, path, hash)
	require.NoError(t, err)
	require.Equal(t, blob, got)

	got, err = reader.Node(common.Hash{}, path, common.HexToHash("0xdead"))
	require.NoError(t, err)
	require.Equal(t, fallback, got)
}
