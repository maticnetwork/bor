// Copyright 2020 The go-ethereum Authors
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

// This file contains some shares testing functionality, common to  multiple
// different files and modules being tested.

package eth

import (
	"crypto/rand"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// testPeer is a simulated peer to allow testing direct network calls.
type testPeer struct {
	*Peer

	net p2p.MsgReadWriter // Network layer reader/writer to simulate remote messaging
	app *p2p.MsgPipeRW    // Application layer reader/writer to simulate the local side
}

// newTestPeer creates a new peer registered at the given data backend.
func newTestPeer(name string, version uint, backend Backend) (*testPeer, <-chan error) {
	// Create a message pipe to communicate through
	app, net := p2p.MsgPipe()

	// Start the peer on a new thread
	var id enode.ID

	rand.Read(id[:])

	peer := NewPeer(version, p2p.NewPeer(id, name, nil), net, backend.TxPool(), backend.Chain().Config())
	errc := make(chan error, 1)

	go func() {
		defer app.Close()

		errc <- backend.RunPeer(peer, func(peer *Peer) error {
			return Handle(backend, peer)
		})
	}()

	return &testPeer{app: app, net: net, Peer: peer}, errc
}

// close terminates the local side of the peer, notifying the remote protocol
// manager of termination.
func (p *testPeer) close() {
	p.Peer.Close()
	p.app.Close()
}

func TestPeerSet(t *testing.T) {
	size := 5
	s := newKnownCache(size)

	// add 10 items
	for i := 0; i < size*2; i++ {
		s.Add(common.Hash{byte(i)})
	}

	if s.Cardinality() != size {
		t.Fatalf("wrong size, expected %d but found %d", size, s.Cardinality())
	}

	vals := []common.Hash{}
	for i := 10; i < 20; i++ {
		vals = append(vals, common.Hash{byte(i)})
	}

	// add item in batch
	s.Add(vals...)

	if s.Cardinality() < size {
		t.Fatalf("bad size")
	}
}

func TestKnownCacheRemove(t *testing.T) {
	size := 10
	s := newKnownCache(size)

	// Add some items
	hashes := make([]common.Hash, 5)
	for i := 0; i < 5; i++ {
		hashes[i] = common.Hash{byte(i)}
		s.Add(hashes[i])
	}

	if s.Cardinality() != 5 {
		t.Fatalf("wrong size after add, expected 5 but found %d", s.Cardinality())
	}

	// Remove some items
	s.Remove(hashes[0], hashes[2], hashes[4])

	if s.Cardinality() != 2 {
		t.Fatalf("wrong size after remove, expected 2 but found %d", s.Cardinality())
	}

	// Verify the correct items were removed
	if s.Contains(hashes[0]) {
		t.Error("hash[0] should have been removed")
	}
	if !s.Contains(hashes[1]) {
		t.Error("hash[1] should still be present")
	}
	if s.Contains(hashes[2]) {
		t.Error("hash[2] should have been removed")
	}
	if !s.Contains(hashes[3]) {
		t.Error("hash[3] should still be present")
	}
	if s.Contains(hashes[4]) {
		t.Error("hash[4] should have been removed")
	}
}

func TestPeerForgetTransactions(t *testing.T) {
	// Create a peer with a known tx cache
	app, _ := p2p.MsgPipe()
	defer app.Close()

	var id enode.ID
	rand.Read(id[:])

	peer := NewPeer(ETH68, p2p.NewPeer(id, "test", nil), app, nil, nil)
	defer peer.Close()

	// Add some transaction hashes to the known set
	hashes := make([]common.Hash, 5)
	for i := 0; i < 5; i++ {
		hashes[i] = common.Hash{byte(i + 100)}
		peer.knownTxs.Add(hashes[i])
	}

	if peer.knownTxs.Cardinality() != 5 {
		t.Fatalf("wrong size after add, expected 5 but found %d", peer.knownTxs.Cardinality())
	}

	// Forget some transactions
	peer.ForgetTransactions([]common.Hash{hashes[0], hashes[2], hashes[4]})

	if peer.knownTxs.Cardinality() != 2 {
		t.Fatalf("wrong size after forget, expected 2 but found %d", peer.knownTxs.Cardinality())
	}

	// Verify the transactions were forgotten
	if peer.knownTxs.Contains(hashes[0]) {
		t.Error("hash[0] should have been forgotten")
	}
	if !peer.knownTxs.Contains(hashes[1]) {
		t.Error("hash[1] should still be known")
	}
}
