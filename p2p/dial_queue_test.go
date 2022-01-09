// Copyright 2015 The go-ethereum Authors
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
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/internal/testlog"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"github.com/stretchr/testify/assert"
)

func newDialSchedFramework(t *testing.T, config dialConfig) *dialSchedFramework {
	d := &dialSchedFramework{
		t:        t,
		notifyCh: make(chan struct{}, 4),
		dialed:   newPeerList(),
		iter:     newDialTestIterator(),
	}
	d.sched = newDialScheduler(config, d.iter, d.dial)
	return d
}

type dialSchedFramework struct {
	t        *testing.T
	sched    *dialScheduler
	iter     *dialTestIterator
	config   dialConfig
	dialed   peerList
	notifyCh chan struct{}
}

func (d *dialSchedFramework) RunDialStep() {
	d.sched.runImpl()
}

func (d *dialSchedFramework) DelPeers(peers []*Peer) {
	for _, p := range peers {
		d.sched.peerRemoved(p)
	}
}

func (d *dialSchedFramework) AddPeers(peers []*Peer) {
	for _, p := range peers {
		d.sched.peerAdded(p, true)
	}
}

func (d *dialSchedFramework) dial(flag connFlag, n *enode.Node) error {
	fmt.Println("DIAL", n.ID())

	d.dialed.add(n.ID())

	select {
	case d.notifyCh <- struct{}{}:
	default:
	}
	return nil
}

func (d *dialSchedFramework) ExpectDials(expect []enode.ID) {
	doneCh := make(chan struct{})
	go func() {
		time.Sleep(2 * time.Second)
		close(doneCh)
	}()

	for {
		select {
		case <-d.notifyCh:
		case <-doneCh:
			d.t.Fatal("timeout")
		}

		found := true
		for _, i := range expect {
			if !d.dialed.contains(i) {
				found = false
			}
		}
		if found {
			if d.dialed.len() != len(expect) {
				panic("BAD SIZE")
			}
			d.dialed.reset()
			return
		}
	}
}

func (d *dialSchedFramework) Close() {
	d.sched.Close()
}

func (d *dialSchedFramework) Discover(mNodes []*mockNode) {
	nodes := []*enode.Node{}
	for _, i := range mNodes {
		nodes = append(nodes, i.Node())
	}
	d.iter.addNodes(nodes)
}

type mockNode struct {
	id   int
	addr string
}

func newMockNode(id int) *mockNode {
	return &mockNode{id: id, addr: "127.0.0.1:30303"}
}

func (m *mockNode) Addr(addr string) *mockNode {
	m.addr = addr
	return m
}

func (m *mockNode) Node() *enode.Node {
	return newNode(uintID(uint16(m.id)), m.addr)
}

func TestDialScheduler_DynamicDials(t *testing.T) {
	// This test checks that dynamic dials are launched from discovery results.

	sched := newDialSchedFramework(t, dialConfig{
		maxActiveDials: 5,
		maxDialPeers:   4,
	})

	sched.AddPeers([]*Peer{
		{flags: staticDialedConn, node: newMockNode(0).Node()},
		{flags: dynDialedConn, node: newMockNode(1).Node()},
		{flags: dynDialedConn, node: newMockNode(2).Node()},
	})

	sched.RunDialStep()

	// 1 stage:
	// - 3 nodes are connected of 4 max peers.
	// - 2 dial slots available. 9 peers discovered but only 2 dialed.
	disc := []*mockNode{}
	for i := 2; i < 15; i++ {
		disc = append(disc, newMockNode(i))
	}
	sched.Discover(disc)

	sched.ExpectDials([]enode.ID{
		uintID(3),
		uintID(4),
	})

	// at this point is connected to 5 peers and it has dialed 5
	assert.Equal(t, sched.sched.peers.len(), int(5))
	assert.Equal(t, sched.sched.dialPeers, int(5))

	fmt.Println("// STAGE 2")

	// 2 stage:
	// - we need to remove at least 2 nodes to have less than maxDialPeers and dial again
	// - node 5 and 6 are dialed
	sched.DelPeers([]*Peer{
		{flags: dynDialedConn, node: newMockNode(2).Node()},
		{flags: dynDialedConn, node: newMockNode(4).Node()},
	})

	sched.ExpectDials([]enode.ID{
		uintID(5),
		uintID(6),
	})

	fmt.Println("// STAGE 3")

	// 3 stage:
	// 3 peers drop of and we dial up to maxActiveDials
	sched.DelPeers([]*Peer{
		{flags: dynDialedConn, node: newMockNode(1).Node()},
		{flags: dynDialedConn, node: newMockNode(3).Node()},
		{flags: dynDialedConn, node: newMockNode(5).Node()},
		{flags: dynDialedConn, node: newMockNode(6).Node()},
	})

	sched.ExpectDials([]enode.ID{
		uintID(7),
		uintID(8),
		uintID(9),
		uintID(10),
		uintID(11),
	})
}

/*
// This test checks that dynamic dials are launched from discovery results.
func TestDialSchedDynDial(t *testing.T) {
	t.Parallel()

	config := dialConfig{
		maxActiveDials: 5,
		maxDialPeers:   4,
	}
	runDialTest(t, config, []dialTestRound{
		// 3 out of 4 peers are connected, leaving 2 dial slots.
		// 9 nodes are discovered, but only 2 are dialed.
		{
			peersAdded: []*Peer{
				{flags: staticDialedConn, node: newNode(uintID(0x00), "")},
				{flags: dynDialedConn, node: newNode(uintID(0x01), "")},
				{flags: dynDialedConn, node: newNode(uintID(0x02), "")},
			},
			discovered: []*enode.Node{
				newNode(uintID(0x00), "127.0.0.1:30303"), // not dialed because already connected as static peer
				newNode(uintID(0x02), "127.0.0.1:30303"), // ...
				newNode(uintID(0x03), "127.0.0.1:30303"),
				newNode(uintID(0x04), "127.0.0.1:30303"),
				newNode(uintID(0x05), "127.0.0.1:30303"), // not dialed because there are only two slots
				newNode(uintID(0x06), "127.0.0.1:30303"), // ...
				newNode(uintID(0x07), "127.0.0.1:30303"), // ...
				newNode(uintID(0x08), "127.0.0.1:30303"), // ...
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x03), "127.0.0.1:30303"),
				newNode(uintID(0x04), "127.0.0.1:30303"),
			},
		},

		// One dial completes, freeing one dial slot.
		{
			failed: []enode.ID{
				uintID(0x04),
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x05), "127.0.0.1:30303"),
			},
		},

		// Dial to 0x03 completes, filling the last remaining peer slot.
		{
			succeeded: []enode.ID{
				uintID(0x03),
			},
			failed: []enode.ID{
				uintID(0x05),
			},
			discovered: []*enode.Node{
				newNode(uintID(0x09), "127.0.0.1:30303"), // not dialed because there are no free slots
			},
		},

		// 3 peers drop off, creating 6 dial slots. Check that 5 of those slots
		// (i.e. up to maxActiveDialTasks) are used.
		{
			peersRemoved: []enode.ID{
				uintID(0x00),
				uintID(0x01),
				uintID(0x02),
			},
			discovered: []*enode.Node{
				newNode(uintID(0x0a), "127.0.0.1:30303"),
				newNode(uintID(0x0b), "127.0.0.1:30303"),
				newNode(uintID(0x0c), "127.0.0.1:30303"),
				newNode(uintID(0x0d), "127.0.0.1:30303"),
				newNode(uintID(0x0f), "127.0.0.1:30303"),
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x06), "127.0.0.1:30303"),
				newNode(uintID(0x07), "127.0.0.1:30303"),
				newNode(uintID(0x08), "127.0.0.1:30303"),
				newNode(uintID(0x09), "127.0.0.1:30303"),
				newNode(uintID(0x0a), "127.0.0.1:30303"),
			},
		},
	})
}
*/

func TestDialScheduler_NetRestrict(t *testing.T) {
	// If net restrict is enabled we only dial nodes that match
	// the CIDR mask

	sched := newDialSchedFramework(t, dialConfig{
		netRestrict:    new(netutil.Netlist),
		maxActiveDials: 10,
		maxDialPeers:   10,
	})
	sched.config.netRestrict.Add("127.0.2.0/24")
	defer sched.Close()

	sched.Discover([]*mockNode{
		// localhost nodes do not match CIDR
		newMockNode(1),
		newMockNode(2),
		newMockNode(3),
		newMockNode(4),

		// nodes match CIDR
		newMockNode(5).Addr("127.0.2.5:30303"),
		newMockNode(6).Addr("127.0.2.6:30303"),
		newMockNode(7).Addr("127.0.2.7:30303"),
		newMockNode(8).Addr("127.0.2.8:30303"),
	})

	sched.ExpectDials([]enode.ID{
		uintID(5),
		uintID(6),
		uintID(7),
		uintID(8),
	})
}

/*
// This test checks that candidates that do not match the netrestrict list are not dialed.
func TestDialSchedNetRestrict(t *testing.T) {
	t.Parallel()

	nodes := []*enode.Node{
		newNode(uintID(0x01), "127.0.0.1:30303"),
		newNode(uintID(0x02), "127.0.0.2:30303"),
		newNode(uintID(0x03), "127.0.0.3:30303"),
		newNode(uintID(0x04), "127.0.0.4:30303"),
		newNode(uintID(0x05), "127.0.2.5:30303"),
		newNode(uintID(0x06), "127.0.2.6:30303"),
		newNode(uintID(0x07), "127.0.2.7:30303"),
		newNode(uintID(0x08), "127.0.2.8:30303"),
	}
	config := dialConfig{
		netRestrict:    new(netutil.Netlist),
		maxActiveDials: 10,
		maxDialPeers:   10,
	}
	config.netRestrict.Add("127.0.2.0/24")
	runDialTest(t, config, []dialTestRound{
		{
			discovered:   nodes,
			wantNewDials: nodes[4:8],
		},
		{
			succeeded: []enode.ID{
				nodes[4].ID(),
				nodes[5].ID(),
				nodes[6].ID(),
				nodes[7].ID(),
			},
		},
	})
}
*/

func TestDialScheduler_StaticDial(t *testing.T) {
	// This test checks that static dials work and obey the limits.

	sched := newDialSchedFramework(t, dialConfig{
		maxActiveDials: 5,
		maxDialPeers:   4,
	})

	// node 1 to 4 are static peers
	for i := 1; i < 10; i++ {
		node := newMockNode(i).Node()
		if i <= 3 {
			sched.sched.addStatic(node)
		} else {
			sched.iter.addNodes([]*enode.Node{node})
		}
	}

	sched.RunDialStep()

	// 1. It should dial the 3 static nodes (1,2,3)
	// and other 2 from the discovery table
	sched.ExpectDials([]enode.ID{
		uintID(1),
		uintID(2),
		uintID(3),
		uintID(4),
		uintID(5),
	})

	fmt.Println("==> STAGE 2")

	// 2. add more static nodes. Any new opened slots should be filled first
	// by this static peer
	sched.sched.addStatic(newMockNode(10).Node())

	sched.DelPeers([]*Peer{
		{flags: dynDialedConn, node: newMockNode(4).Node()},
		{flags: dynDialedConn, node: newMockNode(5).Node()},
	})

	sched.RunDialStep()

	// it tries both the static and one from discovery
	sched.ExpectDials([]enode.ID{
		uintID(10),
		uintID(6),
	})

	fmt.Println("==> STAGE 3")

	// 3. disconnect a static node.
	// it should connect inmediately to it again.

	sched.DelPeers([]*Peer{
		{flags: dynDialedConn, node: newMockNode(1).Node()},
		{flags: dynDialedConn, node: newMockNode(6).Node()},
	})

	sched.RunDialStep()

	sched.ExpectDials([]enode.ID{
		uintID(1),
		uintID(8),
	})
}

/*
// This test checks that static dials work and obey the limits.
func TestDialSchedStaticDial(t *testing.T) {
	t.Parallel()

	config := dialConfig{
		maxActiveDials: 5,
		maxDialPeers:   4,
	}
	runDialTest(t, config, []dialTestRound{
		// Static dials are launched for the nodes that
		// aren't yet connected.
		{
			peersAdded: []*Peer{
				{flags: dynDialedConn, node: newNode(uintID(0x01), "127.0.0.1:30303")},
				{flags: dynDialedConn, node: newNode(uintID(0x02), "127.0.0.2:30303")},
			},
			update: func(d *dialScheduler) {
				// These two are not dialed because they're already connected
				// as dynamic peers.
				d.addStatic(newNode(uintID(0x01), "127.0.0.1:30303"))
				d.addStatic(newNode(uintID(0x02), "127.0.0.2:30303"))
				// These nodes will be dialed:
				d.addStatic(newNode(uintID(0x03), "127.0.0.3:30303"))
				d.addStatic(newNode(uintID(0x04), "127.0.0.4:30303"))
				d.addStatic(newNode(uintID(0x05), "127.0.0.5:30303"))
				d.addStatic(newNode(uintID(0x06), "127.0.0.6:30303"))
				d.addStatic(newNode(uintID(0x07), "127.0.0.7:30303"))
				d.addStatic(newNode(uintID(0x08), "127.0.0.8:30303"))
				d.addStatic(newNode(uintID(0x09), "127.0.0.9:30303"))
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x03), "127.0.0.3:30303"),
				newNode(uintID(0x04), "127.0.0.4:30303"),
				newNode(uintID(0x05), "127.0.0.5:30303"),
				newNode(uintID(0x06), "127.0.0.6:30303"),
			},
		},
		// Dial to 0x03 completes, filling a peer slot. One slot remains,
		// two dials are launched to attempt to fill it.
		{
			succeeded: []enode.ID{
				uintID(0x03),
			},
			failed: []enode.ID{
				uintID(0x04),
				uintID(0x05),
				uintID(0x06),
			},
			wantResolves: map[enode.ID]*enode.Node{
				uintID(0x04): nil,
				uintID(0x05): nil,
				uintID(0x06): nil,
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x08), "127.0.0.8:30303"),
				newNode(uintID(0x09), "127.0.0.9:30303"),
			},
		},
		// Peer 0x01 drops and 0x07 connects as inbound peer.
		// Only 0x01 is dialed.
		{
			peersAdded: []*Peer{
				{flags: inboundConn, node: newNode(uintID(0x07), "127.0.0.7:30303")},
			},
			peersRemoved: []enode.ID{
				uintID(0x01),
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x01), "127.0.0.1:30303"),
			},
		},
	})
}
*/

func TestDialScheduler_RemoveStatic(t *testing.T) {
	// This test checks that removing static nodes stops connecting to them.
	sched := newDialSchedFramework(t, dialConfig{
		maxActiveDials: 1,
		maxDialPeers:   1,
	})

	// node 1 to 4 are static peers
	for i := 1; i < 4; i++ {
		sched.sched.addStatic(newMockNode(i).Node())
	}

	sched.RunDialStep()

	// 1. It should dial the 3 static nodes (1,2,3)
	// and other 2 from the discovery table
	sched.ExpectDials([]enode.ID{
		uintID(1),
	})

	// stage 2
	// Remove node 1 and tries it again
	sched.DelPeers([]*Peer{
		{flags: dynDialedConn, node: newMockNode(1).Node()},
	})

	sched.RunDialStep()

	sched.ExpectDials([]enode.ID{
		uintID(2),
	})

	// stage 3 remove all static
	fmt.Println("// STAGE 3")

	for i := 1; i < 4; i++ {
		sched.sched.removeStatic(newMockNode(i).Node())
	}

	sched.DelPeers([]*Peer{
		{flags: dynDialedConn, node: newMockNode(2).Node()},
	})

	// If we do not run Close, RunDialStep will wait forever
	// until new tasks are available, but no more tasks are available
	// because we have removed the static peers
	go func() {
		time.After(1 * time.Second)
		sched.Close()
	}()
	sched.RunDialStep()

	fmt.Println(sched.dialed.len(), 0)
}

/*
// This test checks that removing static nodes stops connecting to them.
func TestDialSchedRemoveStatic(t *testing.T) {
	t.Parallel()

	config := dialConfig{
		maxActiveDials: 1,
		maxDialPeers:   1,
	}
	runDialTest(t, config, []dialTestRound{
		// Add static nodes.
		{
			update: func(d *dialScheduler) {
				d.addStatic(newNode(uintID(0x01), "127.0.0.1:30303"))
				d.addStatic(newNode(uintID(0x02), "127.0.0.2:30303"))
				d.addStatic(newNode(uintID(0x03), "127.0.0.3:30303"))
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x01), "127.0.0.1:30303"),
			},
		},
		// Dial to 0x01 fails.
		{
			failed: []enode.ID{
				uintID(0x01),
			},
			wantResolves: map[enode.ID]*enode.Node{
				uintID(0x01): nil,
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x02), "127.0.0.2:30303"),
			},
		},
		// All static nodes are removed. 0x01 is in history, 0x02 is being
		// dialed, 0x03 is in staticPool.
		{
			update: func(d *dialScheduler) {
				d.removeStatic(newNode(uintID(0x01), "127.0.0.1:30303"))
				d.removeStatic(newNode(uintID(0x02), "127.0.0.2:30303"))
				d.removeStatic(newNode(uintID(0x03), "127.0.0.3:30303"))
			},
			failed: []enode.ID{
				uintID(0x02),
			},
			wantResolves: map[enode.ID]*enode.Node{
				uintID(0x02): nil,
			},
		},
		// Since all static nodes are removed, they should not be dialed again.
		{}, {}, {},
	})
}
*/

func TestDialScheduler_ManyStaticNodes(t *testing.T) {
	// This test checks that static dials are selected at random.
	// Not sure how easy (and important) is to implement since we fully delegate
	// to the dial queue to order the dials given the priority
	t.Skip("not implemented")
}

/*
// This test checks that static dials are selected at random.
func TestDialSchedManyStaticNodes(t *testing.T) {
	t.Parallel()

	config := dialConfig{maxDialPeers: 2}
	runDialTest(t, config, []dialTestRound{
		{
			peersAdded: []*Peer{
				{flags: dynDialedConn, node: newNode(uintID(0xFFFE), "")},
				{flags: dynDialedConn, node: newNode(uintID(0xFFFF), "")},
			},
			update: func(d *dialScheduler) {
				for id := uint16(0); id < 2000; id++ {
					n := newNode(uintID(id), "127.0.0.1:30303")
					d.addStatic(n)
				}
			},
		},
		{
			peersRemoved: []enode.ID{
				uintID(0xFFFE),
				uintID(0xFFFF),
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x0085), "127.0.0.1:30303"),
				newNode(uintID(0x02dc), "127.0.0.1:30303"),
				newNode(uintID(0x0285), "127.0.0.1:30303"),
				newNode(uintID(0x00cb), "127.0.0.1:30303"),
			},
		},
	})
}
*/

func TestDialScheduler_History(t *testing.T) {
	// This test checks that past dials are not retried for some time.

}

/*
// This test checks that past dials are not retried for some time.
func TestDialSchedHistory(t *testing.T) {
	t.Parallel()

	config := dialConfig{
		maxActiveDials: 3,
		maxDialPeers:   3,
	}
	runDialTest(t, config, []dialTestRound{
		{
			update: func(d *dialScheduler) {
				d.addStatic(newNode(uintID(0x01), "127.0.0.1:30303"))
				d.addStatic(newNode(uintID(0x02), "127.0.0.2:30303"))
				d.addStatic(newNode(uintID(0x03), "127.0.0.3:30303"))
			},
			wantNewDials: []*enode.Node{
				newNode(uintID(0x01), "127.0.0.1:30303"),
				newNode(uintID(0x02), "127.0.0.2:30303"),
				newNode(uintID(0x03), "127.0.0.3:30303"),
			},
		},
		// No new tasks are launched in this round because all static
		// nodes are either connected or still being dialed.
		{
			succeeded: []enode.ID{
				uintID(0x01),
				uintID(0x02),
			},
			failed: []enode.ID{
				uintID(0x03),
			},
			wantResolves: map[enode.ID]*enode.Node{
				uintID(0x03): nil,
			},
		},
		// Nothing happens in this round because we're waiting for
		// node 0x3's history entry to expire.
		{},
		// The cache entry for node 0x03 has expired and is retried.
		{
			wantNewDials: []*enode.Node{
				newNode(uintID(0x03), "127.0.0.3:30303"),
			},
		},
	})
}
*/

func TestDialSchedResolve(t *testing.T) {
	t.Parallel()

	config := dialConfig{
		maxActiveDials: 1,
		maxDialPeers:   1,
	}
	node := newNode(uintID(0x01), "")
	resolved := newNode(uintID(0x01), "127.0.0.1:30303")
	resolved2 := newNode(uintID(0x01), "127.0.0.55:30303")
	runDialTest(t, config, []dialTestRound{
		{
			update: func(d *dialScheduler) {
				d.addStatic(node)
			},
			wantResolves: map[enode.ID]*enode.Node{
				uintID(0x01): resolved,
			},
			wantNewDials: []*enode.Node{
				resolved,
			},
		},
		{
			failed: []enode.ID{
				uintID(0x01),
			},
			wantResolves: map[enode.ID]*enode.Node{
				uintID(0x01): resolved2,
			},
			wantNewDials: []*enode.Node{
				resolved2,
			},
		},
	})
}

// -------
// Code below here is the framework for the tests above.

type dialTestRound struct {
	peersAdded   []*Peer
	peersRemoved []enode.ID
	update       func(*dialScheduler) // called at beginning of round
	discovered   []*enode.Node        // newly discovered nodes
	succeeded    []enode.ID           // dials which succeed this round
	failed       []enode.ID           // dials which fail this round
	wantResolves map[enode.ID]*enode.Node
	wantNewDials []*enode.Node // dials that should be launched in this round
}

func runDialTest(t *testing.T, config dialConfig, rounds []dialTestRound) {
	var (
		clock    = new(mclock.Simulated)
		iterator = newDialTestIterator()
		dialer   = newDialTestDialer()
		resolver = new(dialTestResolver)
		peers    = make(map[enode.ID]*Peer)
		setupCh  = make(chan *Peer)
	)

	// Override config.
	config.clock = clock
	//config.dialer = dialer
	config.resolver = resolver
	config.log = testlog.Logger(t, log.LvlTrace)
	config.rand = rand.New(rand.NewSource(0x1111))

	// Set up the dialer. The setup function below runs on the dialTask
	// goroutine and adds the peer.
	var dialsched *dialScheduler
	setup := func(f connFlag, node *enode.Node) error {
		conn := &Peer{flags: f, node: node}
		dialsched.peerAdded(conn, true)
		setupCh <- conn
		return nil
	}
	dialsched = newDialScheduler(config, iterator, setup)
	defer dialsched.Close()

	for i, round := range rounds {
		// Apply peer set updates.
		for _, c := range round.peersAdded {
			if peers[c.node.ID()] != nil {
				t.Fatalf("round %d: peer %v already connected", i, c.node.ID())
			}
			dialsched.peerAdded(c, true)
			peers[c.node.ID()] = c
		}
		for _, id := range round.peersRemoved {
			c := peers[id]
			if c == nil {
				t.Fatalf("round %d: can't remove non-existent peer %v", i, id)
			}
			dialsched.peerRemoved(c)
		}

		// Init round.
		t.Logf("round %d (%d peers)", i, len(peers))
		resolver.setAnswers(round.wantResolves)
		if round.update != nil {
			round.update(dialsched)
		}
		iterator.addNodes(round.discovered)

		// Unblock dialTask goroutines.
		if err := dialer.completeDials(round.succeeded, nil); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		for range round.succeeded {
			conn := <-setupCh
			peers[conn.node.ID()] = conn
		}
		if err := dialer.completeDials(round.failed, errors.New("oops")); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}

		// Wait for new tasks.
		if err := dialer.waitForDials(round.wantNewDials); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		if !resolver.checkCalls() {
			t.Fatalf("unexpected calls to Resolve: %v", resolver.calls)
		}

		clock.Run(16 * time.Second)
	}
}

// dialTestIterator is the input iterator for dialer tests. This works a bit like a channel
// with infinite buffer: nodes are added to the buffer with addNodes, which unblocks Next
// and returns them from the iterator.
type dialTestIterator struct {
	cur *enode.Node

	mu     sync.Mutex
	buf    []*enode.Node
	cond   *sync.Cond
	closed bool
}

func newDialTestIterator() *dialTestIterator {
	it := &dialTestIterator{}
	it.cond = sync.NewCond(&it.mu)
	return it
}

// addNodes adds nodes to the iterator buffer and unblocks Next.
func (it *dialTestIterator) addNodes(nodes []*enode.Node) {
	it.mu.Lock()
	defer it.mu.Unlock()

	it.buf = append(it.buf, nodes...)
	it.cond.Signal()
}

// Node returns the current node.
func (it *dialTestIterator) Node() *enode.Node {
	return it.cur
}

// Next moves to the next node.
func (it *dialTestIterator) Next() bool {
	it.mu.Lock()
	defer it.mu.Unlock()

	it.cur = nil
	for len(it.buf) == 0 && !it.closed {
		it.cond.Wait()
	}
	if it.closed {
		return false
	}
	it.cur = it.buf[0]
	copy(it.buf[:], it.buf[1:])
	it.buf = it.buf[:len(it.buf)-1]
	return true
}

// Close ends the iterator, unblocking Next.
func (it *dialTestIterator) Close() {
	it.mu.Lock()
	defer it.mu.Unlock()

	it.closed = true
	it.buf = nil
	it.cond.Signal()
}

// dialTestDialer is the NodeDialer used by runDialTest.
type dialTestDialer struct {
	init    chan *dialTestReq
	blocked map[enode.ID]*dialTestReq
}

type dialTestReq struct {
	n       *enode.Node
	unblock chan error
}

func newDialTestDialer() *dialTestDialer {
	return &dialTestDialer{
		init:    make(chan *dialTestReq),
		blocked: make(map[enode.ID]*dialTestReq),
	}
}

// Dial implements NodeDialer.
func (d *dialTestDialer) Dial(ctx context.Context, n *enode.Node) (net.Conn, error) {
	req := &dialTestReq{n: n, unblock: make(chan error, 1)}
	select {
	case d.init <- req:
		select {
		case err := <-req.unblock:
			pipe, _ := net.Pipe()
			return pipe, err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waitForDials waits for calls to Dial with the given nodes as argument.
// Those calls will be held blocking until completeDials is called with the same nodes.
func (d *dialTestDialer) waitForDials(nodes []*enode.Node) error {
	waitset := make(map[enode.ID]*enode.Node)
	for _, n := range nodes {
		waitset[n.ID()] = n
	}
	timeout := time.NewTimer(1 * time.Second)
	defer timeout.Stop()

	for len(waitset) > 0 {
		select {
		case req := <-d.init:
			want, ok := waitset[req.n.ID()]
			if !ok {
				return fmt.Errorf("attempt to dial unexpected node %v", req.n.ID())
			}
			if !reflect.DeepEqual(req.n, want) {
				return fmt.Errorf("ENR of dialed node %v does not match test", req.n.ID())
			}
			delete(waitset, req.n.ID())
			d.blocked[req.n.ID()] = req
		case <-timeout.C:
			var waitlist []enode.ID
			for id := range waitset {
				waitlist = append(waitlist, id)
			}
			return fmt.Errorf("timed out waiting for dials to %v", waitlist)
		}
	}

	return d.checkUnexpectedDial()
}

func (d *dialTestDialer) checkUnexpectedDial() error {
	select {
	case req := <-d.init:
		return fmt.Errorf("attempt to dial unexpected node %v", req.n.ID())
	case <-time.After(150 * time.Millisecond):
		return nil
	}
}

// completeDials unblocks calls to Dial for the given nodes.
func (d *dialTestDialer) completeDials(ids []enode.ID, err error) error {
	for _, id := range ids {
		req := d.blocked[id]
		if req == nil {
			return fmt.Errorf("can't complete dial to %v", id)
		}
		req.unblock <- err
	}
	return nil
}

// dialTestResolver tracks calls to resolve.
type dialTestResolver struct {
	mu      sync.Mutex
	calls   []enode.ID
	answers map[enode.ID]*enode.Node
}

func (t *dialTestResolver) setAnswers(m map[enode.ID]*enode.Node) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.answers = m
	t.calls = nil
}

func (t *dialTestResolver) checkCalls() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, id := range t.calls {
		if _, ok := t.answers[id]; !ok {
			return false
		}
	}
	return true
}

func (t *dialTestResolver) Resolve(n *enode.Node) *enode.Node {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.calls = append(t.calls, n.ID())
	return t.answers[n.ID()]
}
