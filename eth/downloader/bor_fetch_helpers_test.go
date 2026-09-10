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

package downloader

import (
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
)

type fakeTypedQueue struct {
	k queueKind
}

type witnessSupportPeer bool

func (w witnessSupportPeer) Head() (common.Hash, *big.Int) { return common.Hash{}, nil }
func (w witnessSupportPeer) RequestHeadersByHash(common.Hash, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (w witnessSupportPeer) RequestHeadersByNumber(uint64, int, int, bool, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (w witnessSupportPeer) RequestBodies([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (w witnessSupportPeer) RequestReceipts([]common.Hash, []uint64, []uint64, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (w witnessSupportPeer) RequestWitnesses([]common.Hash, chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (w witnessSupportPeer) SupportsWitness() bool { return bool(w) }

func (f fakeTypedQueue) kind() queueKind                                      { return f.k }
func (f fakeTypedQueue) waker() chan bool                                     { return nil }
func (f fakeTypedQueue) pending() int                                         { return 0 }
func (f fakeTypedQueue) capacity(peer *peerConnection, rtt time.Duration) int { return 0 }
func (f fakeTypedQueue) updateCapacity(peer *peerConnection, items int, elapsed time.Duration) {
}
func (f fakeTypedQueue) reserve(peer *peerConnection, items int) (*fetchRequest, bool, bool) {
	return nil, false, false
}
func (f fakeTypedQueue) unreserve(peer string) int { return 0 }
func (f fakeTypedQueue) request(peer *peerConnection, req *fetchRequest, resCh chan *eth.Response) (*eth.Request, error) {
	return nil, nil
}
func (f fakeTypedQueue) deliver(peer *peerConnection, packet *eth.Response) (int, error) {
	return 0, nil
}

func TestQueueAcceptsPeer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    queueKind
		version uint
		witness bool
		peer    Peer
		accept  bool
	}{
		{name: "header accepts any version", kind: headerQueueKind, version: eth.ETH68, accept: true},
		{name: "receipt rejects below eth/69", kind: receiptQueueKind, version: eth.ETH68, accept: false},
		{name: "receipt accepts eth/69", kind: receiptQueueKind, version: eth.ETH69, accept: true},
		{name: "witness rejects peer without support", kind: witnessQueueKind, version: eth.ETH69, peer: witnessSupportPeer(false), accept: false},
		{name: "witness accepts peer with support", kind: witnessQueueKind, version: eth.ETH69, peer: witnessSupportPeer(true), accept: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := newPeerConnection("peer", tt.version, tt.peer, log.New())
			if got := queueAcceptsPeer(fakeTypedQueue{k: tt.kind}, peer); got != tt.accept {
				t.Fatalf("acceptance mismatch: have %v, want %v", got, tt.accept)
			}
		})
	}
}

func TestCollectIdlePeersSurfacesBackoff(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet()}

	idle := newPeerConnection("idle", eth.ETH69, nil, log.New())
	benched := newPeerConnection("benched", eth.ETH69, nil, log.New())
	if err := d.peers.Register(idle); err != nil {
		t.Fatalf("register idle peer: %v", err)
	}
	if err := d.peers.Register(benched); err != nil {
		t.Fatalf("register benched peer: %v", err)
	}
	benched.backoffFor(30 * time.Second)

	queue := fakeTypedQueue{k: headerQueueKind}
	idles, _, hasBackedOff, awaitingStale, nextBackoff := d.collectIdlePeers(queue, nil, nil)

	if len(idles) != 1 || idles[0].id != "idle" {
		t.Fatalf("expected only the non-benched peer idle, got %v", idles)
	}
	if !hasBackedOff {
		t.Fatal("expected hasBackedOff to be reported")
	}
	if awaitingStale != 0 {
		t.Fatalf("no stale requests outstanding, want awaitingStale 0, have %d", awaitingStale)
	}
	if nextBackoff.IsZero() {
		t.Fatal("expected a backoff wake instant for the benched peer")
	}
	if want := benched.backoffExpiry(); !nextBackoff.Equal(want) {
		t.Fatalf("backoff wake mismatch: have %v, want %v", nextBackoff, want)
	}
}

func TestArmBackoffTimer(t *testing.T) {
	t.Parallel()

	created, ch := armBackoffTimer(nil, time.Now().Add(20*time.Millisecond))
	if created == nil || ch == nil {
		t.Fatal("expected an armed timer for a future wake instant")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("armed timer did not fire")
	}

	reused, ch := armBackoffTimer(created, time.Now().Add(20*time.Millisecond))
	if reused != created {
		t.Fatal("expected the existing timer to be reused, not reallocated, for a future wake instant")
	}
	if ch == nil {
		t.Fatal("expected an active channel when reusing a timer")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("reused timer did not fire")
	}

	retained, ch := armBackoffTimer(reused, time.Time{})
	if retained != reused {
		t.Fatal("expected the timer to be retained for reuse on a zero wake instant")
	}
	if ch != nil {
		t.Fatal("expected no active channel for a zero wake instant")
	}
	if retained.Stop() {
		t.Fatal("a retained timer for a zero wake instant should already be stopped")
	}

	fresh, ch := armBackoffTimer(nil, time.Now().Add(-time.Second))
	if fresh == nil || ch == nil {
		t.Fatal("expected an armed timer for a past wake instant")
	}
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timer for a past wake instant did not fire promptly")
	}
}

func TestRespondToPeerDropRoutesToDropPeer(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())

	d.respondToPeer(peer, peerFailureInvalidChain, errInvalidChain)

	select {
	case id := <-dropped:
		if id != "peer" {
			t.Fatalf("dropped wrong peer: have %q, want %q", id, "peer")
		}
	default:
		t.Fatal("drop reason should route to dropPeer")
	}
}

func TestClassifyPeerStallerBacksOff(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	queue := fakeTypedQueue{k: headerQueueKind}

	sent := time.Now()
	fresh := map[string]*eth.Request{peer.id: {Sent: sent}}
	idle, _, backedOff, awaiting, wake := d.classifyPeer(queue, peer, nil, fresh)
	if idle || backedOff {
		t.Fatalf("stale peer within grace period mismatch: idle=%v backedOff=%v", idle, backedOff)
	}
	if !awaiting {
		t.Fatal("a stale peer within the grace period should be awaited for a late delivery")
	}
	if want := sent.Add(timeoutGracePeriod); !wake.Equal(want) {
		t.Fatalf("within-grace wake mismatch: have %v, want %v", wake, want)
	}
	if peer.backedOff() {
		t.Fatal("a peer within the grace period must not be backed off")
	}
	select {
	case id := <-dropped:
		t.Fatalf("peer within grace period was dropped: %q", id)
	default:
	}

	stale := map[string]*eth.Request{peer.id: {Sent: time.Now().Add(-2 * timeoutGracePeriod)}}
	idle, _, backedOff, awaiting, wake = d.classifyPeer(queue, peer, nil, stale)
	if idle {
		t.Fatal("a stalling peer must not be offered as idle")
	}
	if !backedOff {
		t.Fatal("a stalling peer past the grace period should be backed off")
	}
	if awaiting {
		t.Fatal("a stalling peer past the grace period is no longer awaited for a late delivery")
	}
	if wake.IsZero() {
		t.Fatal("a backed-off staller should schedule a wake instant")
	}
	if !peer.backedOff() {
		t.Fatal("a stalling peer past the grace period should be benched")
	}
	select {
	case id := <-dropped:
		t.Fatalf("a stalling peer should back off, not drop: %q", id)
	default:
	}
}

func TestClassifyPeerBusyPeerNotIdle(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet()}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	queue := fakeTypedQueue{k: headerQueueKind}

	pending := map[string]*eth.Request{peer.id: {Sent: time.Now()}}
	idle, capacity, backedOff, awaiting, wake := d.classifyPeer(queue, peer, pending, nil)
	if idle {
		t.Fatal("a peer with an in-flight request must not be offered as idle")
	}
	if backedOff {
		t.Fatal("a peer with an in-flight request must not be reported as backed off")
	}
	if awaiting {
		t.Fatal("a peer with a live in-flight request is not an awaited staller")
	}
	if capacity != 0 {
		t.Fatalf("busy peer capacity mismatch: have %d, want 0", capacity)
	}
	if !wake.IsZero() {
		t.Fatalf("busy peer should not schedule a backoff wake: have %v", wake)
	}
}

func TestFetchStallError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		stalled       bool
		idles         int
		capablePeers  int
		awaitingStale int
		hasBackedOff  bool
		want          error
	}{
		{name: "not stalled never errors", stalled: false, idles: 0, capablePeers: 2, awaitingStale: 0, hasBackedOff: true, want: nil},
		{name: "all capable peers idle but unusable", stalled: true, idles: 2, capablePeers: 2, awaitingStale: 0, hasBackedOff: false, want: ErrPeersUnavailable},
		{name: "all capable peers backed off past grace", stalled: true, idles: 0, capablePeers: 2, awaitingStale: 0, hasBackedOff: true, want: ErrPeerBackedOff},
		{name: "awaiting a late delivery keeps waiting", stalled: true, idles: 0, capablePeers: 2, awaitingStale: 1, hasBackedOff: true, want: nil},
		{name: "no backed-off peer keeps waiting", stalled: true, idles: 1, capablePeers: 2, awaitingStale: 0, hasBackedOff: false, want: nil},
		{name: "idle-equals-capable wins over backed-off", stalled: true, idles: 2, capablePeers: 2, awaitingStale: 0, hasBackedOff: true, want: ErrPeersUnavailable},
		{name: "more idles than capable does not over-trigger", stalled: true, idles: 3, capablePeers: 2, awaitingStale: 0, hasBackedOff: true, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fetchStallError(tt.stalled, tt.idles, tt.capablePeers, tt.awaitingStale, tt.hasBackedOff); got != tt.want {
				t.Fatalf("fetchStallError mismatch: have %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCollectIdlePeersAllStalePastGraceUnblocksExit(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 4)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	queue := fakeTypedQueue{k: headerQueueKind}

	ids := []string{"peer-a", "peer-b"}
	for _, id := range ids {
		pc := newPeerConnection(id, eth.ETH69, nil, log.New("peer", id))
		if err := d.peers.Register(pc); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}

	stales := make(map[string]*eth.Request)
	for _, id := range ids {
		stales[id] = &eth.Request{Peer: id, Sent: time.Now().Add(-2 * timeoutGracePeriod)}
	}

	idles, _, hasBackedOff, awaitingStale, nextBackoff := d.collectIdlePeers(queue, nil, stales)
	if len(idles) != 0 {
		t.Fatalf("stalling peers must not be offered as idle, got %v", idles)
	}
	if !hasBackedOff {
		t.Fatal("stalling peers past grace should be reported as backed off")
	}
	if awaitingStale != 0 {
		t.Fatalf("no stale peer is within grace, want awaitingStale 0, have %d", awaitingStale)
	}
	if nextBackoff.IsZero() {
		t.Fatal("a backed-off staller should schedule a wake instant")
	}
	for _, id := range ids {
		if !d.peers.Peer(id).backedOff() {
			t.Fatalf("peer %s should be benched, not dropped", id)
		}
	}
	select {
	case id := <-dropped:
		t.Fatalf("stalling peers should back off, not drop: %q", id)
	default:
	}

	if err := fetchStallError(true, len(idles), len(ids), awaitingStale, hasBackedOff); err != ErrPeerBackedOff {
		t.Fatalf("all-stale-past-grace round should return ErrPeerBackedOff, have %v", err)
	}
}

func TestCollectIdlePeersWithinGraceKeepsWaiting(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet(), dropPeer: func(string) {}}
	queue := fakeTypedQueue{k: headerQueueKind}

	pc := newPeerConnection("peer", eth.ETH69, nil, log.New("peer", "peer"))
	if err := d.peers.Register(pc); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	stales := map[string]*eth.Request{"peer": {Peer: "peer", Sent: time.Now()}}
	idles, _, hasBackedOff, awaitingStale, nextBackoff := d.collectIdlePeers(queue, nil, stales)
	if len(idles) != 0 {
		t.Fatalf("a stalling peer must not be idle, got %v", idles)
	}
	if hasBackedOff {
		t.Fatal("a peer within the grace period must not be backed off")
	}
	if awaitingStale != 1 {
		t.Fatalf("the within-grace staller should be awaited, want 1, have %d", awaitingStale)
	}
	if nextBackoff.IsZero() {
		t.Fatal("a within-grace staller should still schedule a grace-expiry wake")
	}
	if err := fetchStallError(true, len(idles), 1, awaitingStale, hasBackedOff); err != nil {
		t.Fatalf("a round still awaiting a late delivery must keep waiting, have %v", err)
	}
}

func TestGradeStalePeerClearsEntryAndStrikesOnce(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet(), dropPeer: func(string) {}}
	queue := fakeTypedQueue{k: headerQueueKind}

	pc := newPeerConnection("peer", eth.ETH69, nil, log.New("peer", "peer"))
	if err := d.peers.Register(pc); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	stales := map[string]*eth.Request{"peer": {Peer: "peer", Sent: time.Now().Add(-2 * timeoutGracePeriod)}}

	d.classifyPeer(queue, pc, nil, stales)
	if _, ok := stales["peer"]; ok {
		t.Fatal("a graded stale entry must be removed so an expired backoff does not re-grade the same timeout")
	}
	if got := softStrikeTally(d.peers, "peer"); got != 1 {
		t.Fatalf("grading a stale entry once should record exactly one soft strike, have %d", got)
	}
	if !pc.backedOff() {
		t.Fatal("a graded staller should be benched")
	}

	pc.lock.Lock()
	pc.backoff = time.Time{}
	pc.lock.Unlock()

	d.classifyPeer(queue, pc, nil, stales)
	if got := softStrikeTally(d.peers, "peer"); got != 1 {
		t.Fatalf("a peer with no outstanding stale entry must not accrue a second strike, have %d", got)
	}
}

func softStrikeTally(ps *peerSet, id string) int {
	ps.lock.RLock()
	defer ps.lock.RUnlock()

	return ps.softStrikes[id].count
}
