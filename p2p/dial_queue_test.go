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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"github.com/stretchr/testify/assert"
)

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

func newDialSchedFramework(t *testing.T, config dialConfig) *dialSchedFramework {
	d := &dialSchedFramework{
		t:        t,
		notifyCh: make(chan struct{}, 4),
		dialed:   newPeerList(),
		iter:     newDialTestIterator(),
		config:   config,
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
		d.sched.peerAdded(p)
	}
}

func (d *dialSchedFramework) dial(flag connFlag, n *enode.Node) error {
	fmt.Println("DIAL", n.ID())

	d.dialed.add(n.ID())

	// in this framework we assume that all the dials are correct
	d.sched.peerAdded(&Peer{node: n, flags: flag})

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

	// 1 stage:
	// - 3 nodes are connected of 4 max peers.
	// - 2 dial slots available. 9 peers discovered but only 2 dialed.
	fmt.Println("STAGE 1")
	{
		disc := []*mockNode{}
		for i := 2; i < 15; i++ {
			disc = append(disc, newMockNode(i))
		}
		sched.Discover(disc)

		sched.RunDialStep()

		sched.ExpectDials([]enode.ID{
			uintID(3),
			uintID(4),
		})

		// at this point is connected to 5 peers and it has dialed 5
		assert.Equal(t, int(5), sched.sched.peers.len())
		assert.Equal(t, int(5), sched.sched.dialPeers)
	}

	// 2 stage:
	// - we need to remove at least 2 nodes to have less than maxDialPeers and dial again
	// - node 5 and 6 are dialed
	fmt.Println("STAGE 2")
	{
		sched.DelPeers([]*Peer{
			{flags: dynDialedConn, node: newMockNode(2).Node()},
			{flags: dynDialedConn, node: newMockNode(4).Node()},
		})

		sched.RunDialStep()

		sched.ExpectDials([]enode.ID{
			uintID(5),
			uintID(6),
		})
	}

	// 3 stage:
	// 3 peers drop of and we dial up to maxActiveDials
	fmt.Println("STAGE 3")
	{
		sched.DelPeers([]*Peer{
			{flags: dynDialedConn, node: newMockNode(1).Node()},
			{flags: dynDialedConn, node: newMockNode(3).Node()},
			{flags: dynDialedConn, node: newMockNode(5).Node()},
			{flags: dynDialedConn, node: newMockNode(6).Node()},
		})

		sched.RunDialStep()

		sched.ExpectDials([]enode.ID{
			uintID(7),
			uintID(8),
			uintID(9),
			uintID(10),
			uintID(11),
		})
	}
}

func TestDialScheduler_NetRestrict(t *testing.T) {
	// If net restrict is enabled we only dial nodes that match
	// the CIDR mask

	sched := newDialSchedFramework(t, dialConfig{
		netRestrict:    new(netutil.Netlist),
		maxActiveDials: 4,
		maxDialPeers:   4,
	})
	sched.config.netRestrict.Add("127.0.2.0/24")

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

	sched.RunDialStep()

	sched.ExpectDials([]enode.ID{
		uintID(5),
		uintID(6),
		uintID(7),
		uintID(8),
	})
}

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
		uintID(7),
	})
}

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

func TestDialScheduler_ManyStaticNodes(t *testing.T) {
	// This test checks that static dials are selected at random.
	// Not sure how easy (and important) is to implement since we fully delegate
	// to the dial queue to order the dials given the priority
	t.Skip("not implemented")
}

func TestDialScheduler_History(t *testing.T) {
	// TODO
	// This test checks that past dials are not retried for some time.
}

func TestDialScheduler_Resolve(t *testing.T) {
	// TODO

	/*
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
	*/
}
