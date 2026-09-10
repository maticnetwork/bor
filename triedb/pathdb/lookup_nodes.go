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

package pathdb

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/trie/trienode"
)

// nodeLookupMaxPath bounds the path length representable in a nodeLookupKey.
// MPT paths are at most 64 nibbles (32-byte keys); a longer path can never be
// produced by the trie, but if one ever appeared the index would flag itself
// non-definitive rather than give wrong miss guarantees.
const nodeLookupMaxPath = 64

var (
	nodeIndexHitMeter   = metrics.NewRegisteredMeter("pathdb/nodeindex/hit", nil)
	nodeIndexMissMeter  = metrics.NewRegisteredMeter("pathdb/nodeindex/miss", nil)
	nodeIndexCountGauge = metrics.NewRegisteredGauge("pathdb/nodeindex/count", nil)
	nodeIndexBytesGauge = metrics.NewRegisteredGauge("pathdb/nodeindex/bytes", nil)
)

// nodeLookupKey identifies a trie node by owner, path and content hash. It is
// a comparable value type built on the stack, so index probes do not allocate.
type nodeLookupKey struct {
	owner   common.Hash
	hash    common.Hash
	pathLen uint8
	path    [nodeLookupMaxPath]byte
}

func makeNodeLookupKey(owner common.Hash, path []byte, hash common.Hash) (nodeLookupKey, bool) {
	if len(path) > nodeLookupMaxPath {
		return nodeLookupKey{}, false
	}
	key := nodeLookupKey{owner: owner, hash: hash, pathLen: uint8(len(path))}
	copy(key.path[:], path)
	return key, true
}

// makeNodeLookupKeyString is the string-path variant used on the mutation
// side, where paths arrive as map keys.
func makeNodeLookupKeyString(owner common.Hash, path string, hash common.Hash) (nodeLookupKey, bool) {
	if len(path) > nodeLookupMaxPath {
		return nodeLookupKey{}, false
	}
	key := nodeLookupKey{owner: owner, hash: hash, pathLen: uint8(len(path))}
	copy(key.path[:], path)
	return key, true
}

// nodeLookupEntry pairs a node blob with a reference count: the same
// (owner, path, hash) triple can be supplied by several live layers (sibling
// forks, or a path rewritten to identical content), and the entry must
// survive until the last of them is unlinked.
type nodeLookupEntry struct {
	blob []byte
	refs uint32
}

// nodeLookup is the trie-node counterpart of lookup. It maps every live trie
// node held by the diff layers, keyed by (owner, path, hash), to its blob.
//
// A node request always carries the expected hash — parent trie nodes embed
// their child hashes — which makes the triple content-addressed: a hit is
// correct regardless of which layer or fork supplied the entry, and a miss
// guarantees that no live diff layer holds the requested node, so readers can
// go straight to the disk layer instead of walking the layer chain probing
// every diff layer's maps.
//
// Deletion markers (empty blobs) are not indexed: a deleted node is never
// referenced by post-deletion parents, so no reader at a newer root asks for
// it, and readers at older roots want the pre-deletion version, which either
// lives in an older diff layer (indexed) or on disk (the miss path).
//
// Mutations happen under the layerTree lock, mirroring lookup.
type nodeLookup struct {
	nodes map[nodeLookupKey]nodeLookupEntry
	bytes int64

	// overflow is set if a node with an unrepresentable path was ever seen;
	// from then on misses are reported as non-definitive and readers use the
	// legacy layer walk. Cannot happen with well-formed MPT paths.
	overflow bool
}

// newNodeLookup indexes every diff layer reachable from head. Order does not
// matter: entries are content-addressed and reference-counted.
func newNodeLookup(head layer) *nodeLookup {
	l := &nodeLookup{nodes: make(map[nodeLookupKey]nodeLookupEntry)}
	for current := head; current != nil; current = current.parentLayer() {
		if diff, ok := current.(*diffLayer); ok {
			l.addLayer(diff)
		}
	}
	return l
}

// get returns the blob for the requested node if any live diff layer holds
// it. When found is false, definitive reports whether that miss is a
// guarantee (the node is absent from every live diff layer) or whether the
// caller must fall back to the layer walk.
func (l *nodeLookup) get(owner common.Hash, path []byte, hash common.Hash) (blob []byte, found bool, definitive bool) {
	key, ok := makeNodeLookupKey(owner, path, hash)
	if !ok {
		return nil, false, false
	}
	entry, ok := l.nodes[key]
	if !ok {
		return nil, false, !l.overflow
	}
	return entry.blob, true, true
}

// addLayer indexes all live nodes of the given diff layer.
func (l *nodeLookup) addLayer(dl *diffLayer) {
	for path, n := range dl.nodes.accountNodes {
		l.insert(common.Hash{}, path, n)
	}
	for owner, subset := range dl.nodes.storageNodes {
		for path, n := range subset {
			l.insert(owner, path, n)
		}
	}
	l.report()
}

// removeLayer unlinks all live nodes of the given diff layer, dropping
// entries whose last reference is gone.
func (l *nodeLookup) removeLayer(dl *diffLayer) {
	for path, n := range dl.nodes.accountNodes {
		l.remove(common.Hash{}, path, n)
	}
	for owner, subset := range dl.nodes.storageNodes {
		for path, n := range subset {
			l.remove(owner, path, n)
		}
	}
	l.report()
}

func (l *nodeLookup) insert(owner common.Hash, path string, n *trienode.Node) {
	if n == nil || len(n.Blob) == 0 {
		return // deletion marker
	}
	key, ok := makeNodeLookupKeyString(owner, path, n.Hash)
	if !ok {
		l.overflow = true
		return
	}
	entry, exists := l.nodes[key]
	if exists {
		entry.refs++
		l.nodes[key] = entry
		return
	}
	l.nodes[key] = nodeLookupEntry{blob: n.Blob, refs: 1}
	l.bytes += int64(len(n.Blob))
}

func (l *nodeLookup) remove(owner common.Hash, path string, n *trienode.Node) {
	if n == nil || len(n.Blob) == 0 {
		return
	}
	key, ok := makeNodeLookupKeyString(owner, path, n.Hash)
	if !ok {
		return
	}
	entry, exists := l.nodes[key]
	if !exists {
		return
	}
	if entry.refs > 1 {
		entry.refs--
		l.nodes[key] = entry
		return
	}
	l.bytes -= int64(len(entry.blob))
	delete(l.nodes, key)
}

func (l *nodeLookup) report() {
	nodeIndexCountGauge.Update(int64(len(l.nodes)))
	nodeIndexBytesGauge.Update(l.bytes)
}
