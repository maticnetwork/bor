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

package downloader

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/log"
)

// fakeCodePeer is a minimal snap.SyncPeer that answers bytecode requests from a
// fixed corpus (well-behaving: request order, omitting hashes it lacks) and
// no-ops every other request kind.
type fakeCodePeer struct {
	id     string
	syncer *snap.Syncer
	corpus map[common.Hash][]byte
}

func (p *fakeCodePeer) ID() string      { return p.id }
func (p *fakeCodePeer) Log() log.Logger { return log.New("id", p.id) }

func (p *fakeCodePeer) RequestAccountRange(id uint64, root, origin, limit common.Hash, bytes uint64) error {
	return nil
}

func (p *fakeCodePeer) RequestStorageRanges(id uint64, root common.Hash, accounts []common.Hash, origin, limit []byte, bytes uint64) error {
	return nil
}

func (p *fakeCodePeer) RequestTrieNodes(id uint64, root common.Hash, paths []snap.TrieNodePathSet, bytes uint64) error {
	return nil
}

func (p *fakeCodePeer) RequestByteCodes(id uint64, hashes []common.Hash, bytes uint64) error {
	var out [][]byte
	for _, h := range hashes {
		if code, ok := p.corpus[h]; ok {
			out = append(out, code)
		}
	}
	go p.syncer.OnByteCodes(p, id, out)
	return nil
}

// TestRecoverMissingStatelessCode exercises the downloader self-heal hook:
// a *state.MissingCodeError is healed by fetching the blob from a snap peer and
// persisting it to the chain db; any other failure, or an unservable blob, is
// left untouched for the caller's safe failure path.
func TestRecoverMissingStatelessCode(t *testing.T) {
	code := []byte{0x60, 0x2a, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3}
	hash := crypto.Keccak256Hash(code)

	chaindb := rawdb.NewMemoryDatabase()
	syncer := snap.NewSyncer(rawdb.NewMemoryDatabase(), rawdb.HashScheme)
	peer := &fakeCodePeer{id: "p1", syncer: syncer, corpus: map[common.Hash][]byte{hash: code}}
	if err := syncer.Register(peer); err != nil {
		t.Fatalf("register peer: %v", err)
	}
	d := &Downloader{stateDB: chaindb, SnapSyncer: syncer}

	// A non-code failure must not be treated as recoverable, and must write nothing.
	if d.recoverMissingStatelessCode(errors.New("gas limit reached")) {
		t.Fatal("recovered a non-MissingCodeError")
	}

	// A missing code a peer can serve is fetched, verified and persisted.
	if !d.recoverMissingStatelessCode(&state.MissingCodeError{Hash: hash}) {
		t.Fatal("did not recover a servable missing code")
	}
	if got := rawdb.ReadCode(chaindb, hash); !bytes.Equal(got, code) {
		t.Fatalf("healed code not persisted to chain db: got %x want %x", got, code)
	}

	// A missing code no peer serves is not recoverable (caller falls back to safe stop).
	unservable := crypto.Keccak256Hash([]byte("no peer has this"))
	if d.recoverMissingStatelessCode(&state.MissingCodeError{Hash: unservable}) {
		t.Fatal("reported recovery for an unservable code")
	}
	if rawdb.HasCode(chaindb, unservable) {
		t.Fatal("unservable code was somehow persisted")
	}
}
