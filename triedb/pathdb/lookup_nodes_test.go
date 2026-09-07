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
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie/trienode"
)

func nodeLookupTestLayer(nodes map[common.Hash]map[string]*trienode.Node) *diffLayer {
	return newDiffLayer(emptyLayer(), common.Hash{}, 0, 0, NewNodeSetWithOrigin(nodes, nil), NewStateSetWithOrigin(nil, nil, nil, nil, false))
}

func testNode(blob []byte) *trienode.Node {
	return trienode.New(crypto.Keccak256Hash(blob), blob)
}

func TestNodeLookup_AddGetRemove(t *testing.T) {
	owner := common.HexToHash("0xaa")
	acctNode := testNode([]byte("account-node"))
	storNode := testNode([]byte("storage-node"))

	layer := nodeLookupTestLayer(map[common.Hash]map[string]*trienode.Node{
		{}:    {"\x01\x02": acctNode},
		owner: {"\x03": storNode},
	})
	l := &nodeLookup{nodes: make(map[nodeLookupKey]nodeLookupEntry)}
	l.addLayer(layer)

	blob, found, definitive := l.get(common.Hash{}, []byte{0x01, 0x02}, acctNode.Hash)
	if !found || !definitive || string(blob) != "account-node" {
		t.Fatalf("account node not served: found=%v definitive=%v blob=%q", found, definitive, blob)
	}
	blob, found, _ = l.get(owner, []byte{0x03}, storNode.Hash)
	if !found || string(blob) != "storage-node" {
		t.Fatalf("storage node not served: found=%v blob=%q", found, blob)
	}
	// Wrong hash at a live path is a definitive miss.
	_, found, definitive = l.get(owner, []byte{0x03}, common.HexToHash("0xdead"))
	if found || !definitive {
		t.Fatalf("wrong-hash lookup: found=%v definitive=%v, want miss+definitive", found, definitive)
	}

	l.removeLayer(layer)
	if len(l.nodes) != 0 || l.bytes != 0 {
		t.Fatalf("index not empty after removal: entries=%d bytes=%d", len(l.nodes), l.bytes)
	}
}

func TestNodeLookup_RefcountAcrossForks(t *testing.T) {
	shared := testNode([]byte("shared-fork-node"))
	mk := func() *diffLayer {
		return nodeLookupTestLayer(map[common.Hash]map[string]*trienode.Node{
			{}: {"\x07": shared},
		})
	}
	forkA, forkB := mk(), mk()

	l := &nodeLookup{nodes: make(map[nodeLookupKey]nodeLookupEntry)}
	l.addLayer(forkA)
	l.addLayer(forkB)

	// Removing one fork must not drop the entry the sibling still owns.
	l.removeLayer(forkA)
	_, found, _ := l.get(common.Hash{}, []byte{0x07}, shared.Hash)
	if !found {
		t.Fatal("shared entry dropped while a sibling fork still holds it")
	}
	l.removeLayer(forkB)
	if _, found, _ := l.get(common.Hash{}, []byte{0x07}, shared.Hash); found {
		t.Fatal("entry survived removal of every owning layer")
	}
}

func TestNodeLookup_DeletionMarkersSkipped(t *testing.T) {
	deleted := trienode.NewDeleted()
	layer := nodeLookupTestLayer(map[common.Hash]map[string]*trienode.Node{
		{}: {"\x09": deleted},
	})
	l := &nodeLookup{nodes: make(map[nodeLookupKey]nodeLookupEntry)}
	l.addLayer(layer)
	if len(l.nodes) != 0 {
		t.Fatalf("deletion marker indexed: entries=%d", len(l.nodes))
	}
	// And removal of the same layer is a no-op rather than a corruption.
	l.removeLayer(layer)
	if l.bytes != 0 {
		t.Fatalf("byte accounting corrupted by deletion markers: %d", l.bytes)
	}
}

func TestNodeLookup_OverflowDisablesDefinitiveMiss(t *testing.T) {
	longPath := string(make([]byte, nodeLookupMaxPath+1))
	layer := nodeLookupTestLayer(map[common.Hash]map[string]*trienode.Node{
		{}: {longPath: testNode([]byte("oversized"))},
	})
	l := &nodeLookup{nodes: make(map[nodeLookupKey]nodeLookupEntry)}
	l.addLayer(layer)

	_, found, definitive := l.get(common.Hash{}, []byte{0x01}, common.HexToHash("0x01"))
	if found {
		t.Fatal("unexpected hit")
	}
	if definitive {
		t.Fatal("miss reported definitive after an unindexable path was seen")
	}
}

func TestNodeLookup_UnindexableAndMissingRemovalBranches(t *testing.T) {
	owner := common.HexToHash("0x1")
	node := testNode([]byte("node"))
	longPath := make([]byte, nodeLookupMaxPath+1)
	l := &nodeLookup{nodes: make(map[nodeLookupKey]nodeLookupEntry)}

	if _, found, definitive := l.get(owner, longPath, node.Hash); found || definitive {
		t.Fatalf("long-path lookup = found %v definitive %v", found, definitive)
	}

	l.remove(owner, string(longPath), node)
	l.remove(owner, "\x01", node)
	if len(l.nodes) != 0 {
		t.Fatalf("unexpected entries after no-op removals: %d", len(l.nodes))
	}

	key, ok := makeNodeLookupKey(owner, []byte{1}, node.Hash)
	if !ok {
		t.Fatal("short node key rejected")
	}
	l.nodes[key] = nodeLookupEntry{blob: node.Blob, refs: 2}
	l.bytes = int64(len(node.Blob))
	l.remove(owner, "\x01", node)
	if got := l.nodes[key].refs; got != 1 {
		t.Fatalf("refcount after shared removal = %d, want 1", got)
	}
}
