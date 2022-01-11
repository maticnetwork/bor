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

package p2p

import (
	"testing"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/assert"
)

func TestServer_Listen(t *testing.T) {
	// TODO: See original test
}

func TestServer_Dial(t *testing.T) {
	// TODO: See original test
}

func TestServer_RemovePeerDisconnect(t *testing.T) {
	// TODO: This test checks that RemovePeer disconnects the peer if it is connected.
}

func TestServer_AtCap(t *testing.T) {
	// TODO: This test checks that connections are disconnected just after the encryption handshake
	// when the server is at capacity. Trusted connections should still be accepted.
	// Use a real rlpx connection.
}

func TestServer_InboundThrottle(t *testing.T) {
	// TODO: This test checks that inbound connections are throttled by IP.
}

func TestServer_PeerAddDrop(t *testing.T) {
	// Test whether we can add and drop peers from the server and there are no locks
	sched := newDialScheduler(dialConfig{}, newDialTestIterator(), func(connFlag, *enode.Node) error {
		return nil
	})

	srv := &Server{
		log:       log.Root(),
		peers:     map[enode.ID]*Peer{},
		dialsched: sched,
	}

	peer0 := &Peer{
		node: newNode(uintID(0x1), "127.0.0.1:30303"),
	}
	peer1 := &Peer{
		node: newNode(uintID(0x2), "127.0.0.1:30303"),
	}

	srv.addPeer(peer0)
	srv.addPeer(peer1)

	assert.Equal(t, srv.lenPeers(), 2)

	srv.Disconnected(peerDisconnected{
		Id:    peer0.node.ID(),
		Error: DiscIncompatibleVersion,
	})
}
