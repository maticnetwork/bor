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

package snap

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
)

// serveCodesFromCorpus returns a codeRequestHandler that answers with the
// requested blobs it has in corpus, in request order, omitting any it lacks —
// the well-behaving snap-server behaviour.
func serveCodesFromCorpus(corpus map[common.Hash][]byte) codeHandlerFunc {
	return func(tp *testPeer, id uint64, hashes []common.Hash, _ uint64) error {
		var out [][]byte
		for _, h := range hashes {
			if code, ok := corpus[h]; ok {
				out = append(out, code)
			}
		}
		return tp.remote.OnByteCodes(tp, id, out)
	}
}

func TestFetchByteCodesOnDemand(t *testing.T) {
	codeA := []byte{0x60, 0x00, 0x60, 0x00, 0xf3}
	codeB := []byte{0xfe, 0x00, 0x01}
	hashA := crypto.Keccak256Hash(codeA)
	hashB := crypto.Keccak256Hash(codeB)
	unknown := crypto.Keccak256Hash([]byte("no peer has this"))

	syncer := NewSyncer(rawdb.NewMemoryDatabase(), rawdb.HashScheme)
	peer := newTestPeer("server", t, func() { t.Helper(); t.Fatal("peer terminated") })
	peer.remote = syncer
	peer.codeRequestHandler = serveCodesFromCorpus(map[common.Hash][]byte{hashA: codeA, hashB: codeB})
	if err := syncer.Register(peer); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	// Both present → both returned and byte-exact.
	got, err := syncer.FetchByteCodes(context.Background(), []common.Hash{hashA, hashB})
	if err != nil {
		t.Fatalf("FetchByteCodes: %v", err)
	}
	if !bytes.Equal(got[hashA], codeA) || !bytes.Equal(got[hashB], codeB) {
		t.Fatalf("wrong codes: got[A]=%x got[B]=%x", got[hashA], got[hashB])
	}

	// A hash no peer serves must not appear in the result (and must not be
	// fabricated).
	got, _ = syncer.FetchByteCodes(context.Background(), []common.Hash{unknown})
	if _, ok := got[unknown]; ok {
		t.Fatalf("unknown hash was served a value: %x", got[unknown])
	}
}

// TestFetchByteCodesFailsOverAndVerifies proves the fetch rejects a peer that
// returns bytes not matching the requested hash (content-addressed check) and
// fails over to a peer that serves the correct blob.
func TestFetchByteCodesFailsOverAndVerifies(t *testing.T) {
	code := []byte{0x60, 0x2a, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}
	hash := crypto.Keccak256Hash(code)

	syncer := NewSyncer(rawdb.NewMemoryDatabase(), rawdb.HashScheme)

	liar := newTestPeer("liar", t, func() {})
	liar.remote = syncer
	liar.codeRequestHandler = func(tp *testPeer, id uint64, hashes []common.Hash, _ uint64) error {
		// Serve the wrong bytes for every requested hash.
		out := make([][]byte, len(hashes))
		for i := range out {
			out[i] = []byte{0xde, 0xad, 0xbe, 0xef}
		}
		return tp.remote.OnByteCodes(tp, id, out)
	}
	honest := newTestPeer("honest", t, func() {})
	honest.remote = syncer
	honest.codeRequestHandler = serveCodesFromCorpus(map[common.Hash][]byte{hash: code})

	if err := syncer.Register(liar); err != nil {
		t.Fatalf("register liar: %v", err)
	}
	if err := syncer.Register(honest); err != nil {
		t.Fatalf("register honest: %v", err)
	}

	got, err := syncer.FetchByteCodes(context.Background(), []common.Hash{hash})
	if err != nil {
		t.Fatalf("FetchByteCodes: %v", err)
	}
	if !bytes.Equal(got[hash], code) {
		t.Fatalf("failover did not yield the verified code: got %x want %x", got[hash], code)
	}
}
