package eth

import (
	"crypto/rand"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestPeerSetExtensions(t *testing.T) {
	t.Run("snap", func(t *testing.T) {
		caps := []p2p.Cap{{Name: eth.ProtocolName, Version: eth.ETH69}, {Name: snap.ProtocolName, Version: snap.SNAP1}}
		testPeerSetExtension(t, caps, false, errSnapWithoutEth, newPeerSetSnapPeer, (*peerSet).registerSnapExtension, (*peerSet).waitSnapExtension)
	})
	t.Run("wit", func(t *testing.T) {
		caps := []p2p.Cap{{Name: eth.ProtocolName, Version: eth.ETH69}, {Name: wit.ProtocolName, Version: wit.WIT2}}
		testPeerSetExtension(t, caps, true, errWitWithoutEth, newPeerSetWitPeer, (*peerSet).registerWitExtension, (*peerSet).waitWitExtension)
	})
}

func TestPeerSetRevision(t *testing.T) {
	ps := newPeerSet()
	peer := newPeerSetEthPeer(t, enode.ID{1}, nil)

	if err := ps.unregisterPeer(peer.ID()); !errors.Is(err, errPeerNotRegistered) {
		t.Fatalf("unexpected unregister error: %v", err)
	}
	if revision := ps.currentRevision(); revision != 0 {
		t.Fatalf("failed unregister changed revision to %d", revision)
	}
	if err := ps.registerPeer(peer, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ps.registerPeer(peer, nil, nil); !errors.Is(err, errPeerAlreadyRegistered) {
		t.Fatalf("unexpected duplicate registration error: %v", err)
	}
	if revision := ps.currentRevision(); revision != 1 {
		t.Fatalf("registration changed revision to %d", revision)
	}
	if err := ps.unregisterPeer(peer.ID()); err != nil {
		t.Fatal(err)
	}
	if revision := ps.currentRevision(); revision != 2 {
		t.Fatalf("unregister changed revision to %d", revision)
	}
	ps.close()
	if err := ps.registerPeer(peer, nil, nil); !errors.Is(err, errPeerSetClosed) {
		t.Fatalf("unexpected closed set error: %v", err)
	}
	if revision := ps.currentRevision(); revision != 2 {
		t.Fatalf("failed registration changed revision to %d", revision)
	}
}

type extensionResult[T comparable] struct {
	peer T
	err  error
}

func testPeerSetExtension[T comparable](
	t *testing.T,
	caps []p2p.Cap,
	witness bool,
	incompatibleError error,
	newExtension func(*testing.T, enode.ID, []p2p.Cap) T,
	register func(*peerSet, T) error,
	wait func(*peerSet, *eth.Peer) (T, error),
) {
	t.Helper()

	t.Run("extension first", func(t *testing.T) {
		ps := newPeerSet()
		defer ps.close()

		id := enode.ID{1}
		main := newPeerSetEthPeer(t, id, caps)
		ext := newExtension(t, id, caps)
		if err := register(ps, ext); err != nil {
			t.Fatal(err)
		}
		if err := register(ps, ext); !errors.Is(err, errPeerAlreadyRegistered) {
			t.Fatalf("unexpected duplicate registration error: %v", err)
		}
		got, err := wait(ps, main)
		if err != nil || got != ext {
			t.Fatalf("extension mismatch: got %v, want %v, err %v", got, ext, err)
		}
	})

	t.Run("main first", func(t *testing.T) {
		ps := newPeerSet()
		defer ps.close()

		id := enode.ID{2}
		main := newPeerSetEthPeer(t, id, caps)
		ext := newExtension(t, id, caps)
		result := make(chan extensionResult[T], 1)
		go func() {
			peer, err := wait(ps, main)
			result <- extensionResult[T]{peer, err}
		}()
		waitForPeerSetWaiter(t, ps, id.String(), witness)
		if err := register(ps, ext); err != nil {
			t.Fatal(err)
		}
		if got := <-result; got.err != nil || got.peer != ext {
			t.Fatalf("extension mismatch: got %v, want %v, err %v", got.peer, ext, got.err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		ps := newPeerSet()
		id := enode.ID{3}
		main := newPeerSetEthPeer(t, id, caps)
		result := make(chan extensionResult[T], 1)
		go func() {
			peer, err := wait(ps, main)
			result <- extensionResult[T]{peer, err}
		}()
		waitForPeerSetWaiter(t, ps, id.String(), witness)
		ps.close()
		if got := <-result; !errors.Is(got.err, errPeerSetClosed) {
			t.Fatalf("unexpected close error: %v", got.err)
		}
	})

	t.Run("registered", func(t *testing.T) {
		ps := newPeerSet()
		defer ps.close()

		id := enode.ID{4}
		main := newPeerSetEthPeer(t, id, caps)
		ext := newExtension(t, id, caps)
		if err := ps.registerPeer(main, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := register(ps, ext); !errors.Is(err, errPeerAlreadyRegistered) {
			t.Fatalf("unexpected extension registration error: %v", err)
		}
		if _, err := wait(ps, main); !errors.Is(err, errPeerAlreadyRegistered) {
			t.Fatalf("unexpected extension wait error: %v", err)
		}
	})

	t.Run("incompatible", func(t *testing.T) {
		ps := newPeerSet()
		defer ps.close()

		id := enode.ID{5}
		ext := newExtension(t, id, caps[1:])
		if err := register(ps, ext); !errors.Is(err, incompatibleError) {
			t.Fatalf("unexpected extension registration error: %v", err)
		}
		main := newPeerSetEthPeer(t, id, caps[:1])
		var zero T
		if got, err := wait(ps, main); err != nil || got != zero {
			t.Fatalf("unexpected extension: got %v, err %v", got, err)
		}
	})
}

func newPeerSetEthPeer(t *testing.T, id enode.ID, caps []p2p.Cap) *eth.Peer {
	t.Helper()
	peer, rw := newPeerSetProtocolPeer(t, id, caps)
	result := eth.NewPeer(eth.ETH69, peer, rw, nil)
	t.Cleanup(result.Close)
	return result
}

func newPeerSetSnapPeer(t *testing.T, id enode.ID, caps []p2p.Cap) *snap.Peer {
	t.Helper()
	peer, rw := newPeerSetProtocolPeer(t, id, caps)
	return snap.NewPeer(snap.SNAP1, peer, rw)
}

func newPeerSetWitPeer(t *testing.T, id enode.ID, caps []p2p.Cap) *wit.Peer {
	t.Helper()
	peer, rw := newPeerSetProtocolPeer(t, id, caps)
	result := wit.NewPeer(wit.WIT2, peer, rw, log.New())
	t.Cleanup(result.Close)
	return result
}

func newPeerSetProtocolPeer(t *testing.T, id enode.ID, caps []p2p.Cap) (*p2p.Peer, p2p.MsgReadWriter) {
	t.Helper()
	app, net := p2p.MsgPipe()
	t.Cleanup(func() {
		app.Close()
		net.Close()
	})
	return p2p.NewPeer(id, "test", caps), net
}

func waitForPeerSetWaiter(t *testing.T, ps *peerSet, id string, witness bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		ps.lock.RLock()
		_, snapReady := ps.snapWait[id]
		_, witReady := ps.witWait[id]
		ps.lock.RUnlock()
		ready := snapReady
		if witness {
			ready = witReady
		}
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("extension waiter was not registered")
}

func TestPeerSetForgetTransactions(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	defer ps.close()

	// Create multiple test peers
	apps := make([]*p2p.MsgPipeRW, 3)

	for i := 0; i < 3; i++ {
		app, net := p2p.MsgPipe()
		apps[i] = app

		var id enode.ID
		rand.Read(id[:])

		peer := eth.NewPeer(eth.ETH68, p2p.NewPeer(id, "test", nil), net, nil)

		// Register the peer
		if err := ps.registerPeer(peer, nil, nil); err != nil {
			t.Fatalf("failed to register peer %d: %v", i, err)
		}
	}

	// Clean up
	defer func() {
		for _, app := range apps {
			app.Close()
		}
	}()

	// Verify we have 3 peers
	if ps.len() != 3 {
		t.Fatalf("expected 3 peers, got %d", ps.len())
	}

	// ForgetTransactions should not panic with registered peers
	// (the actual forgetting logic is tested in eth/protocols/eth/peer_test.go)
	hashes := []common.Hash{{1}, {2}, {3}}
	ps.ForgetTransactions(hashes)
}

func TestPeerSetForgetTransactionsEmpty(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	defer ps.close()

	// ForgetTransactions should not panic with no peers
	ps.ForgetTransactions([]common.Hash{{1}, {2}, {3}})
}

// TestGetOnePeerWithWitnessPrefersBodyOverAnnounce locks in the WIT2 fast-path
// invariant: when at least one peer has the body (knownWitnesses) and another
// has only seen the signed announce (knownAnnounces), body-known wins. If a
// future change inverts this, fetchers will silently prefer slower sources.
func TestGetOnePeerWithWitnessPrefersBodyOverAnnounce(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	defer ps.close()

	hash := common.HexToHash("0xabc")

	bodyPeer := newRegisteredPeerForTest(t, ps)
	announcePeer := newRegisteredPeerForTest(t, ps)

	bodyPeer.witPeer.Peer.AddKnownWitness(hash)
	announcePeer.witPeer.Peer.(*wit.Peer).AddKnownAnnounce(hash)

	got := ps.getOnePeerWithWitness(hash)
	if got == nil {
		t.Fatal("expected a candidate; got nil")
	}
	if got.ID() != bodyPeer.ID() {
		t.Fatalf("body-known peer must win over announce-only: got %s want %s",
			got.ID(), bodyPeer.ID())
	}
}

// TestGetOnePeerWithWitnessFallsBackToAnnounce locks in the fix for the
// fast-path regression: when no peer has the body yet, the announce-known
// fallback IS selectable. Without this, a hop-2 stateless validator with a
// verified signed announce would have nothing to fetch from until the body
// broadcast finally arrived — eliminating the WIT2 latency win.
func TestGetOnePeerWithWitnessFallsBackToAnnounce(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	defer ps.close()

	hash := common.HexToHash("0xdef")

	announcePeer := newRegisteredPeerForTest(t, ps)
	announcePeer.witPeer.Peer.(*wit.Peer).AddKnownAnnounce(hash)

	got := ps.getOnePeerWithWitness(hash)
	if got == nil {
		t.Fatal("announce-only peer must be a fetch candidate after the WIT2 fast-path fix")
	}
	if got.ID() != announcePeer.ID() {
		t.Fatalf("expected announce-only peer; got %s", got.ID())
	}
}

func newRegisteredPeerForTest(t *testing.T, ps *peerSet) *ethPeer {
	t.Helper()
	var id enode.ID
	rand.Read(id[:])
	_, net := p2p.MsgPipe()
	t.Cleanup(func() { net.Close() })

	p2pPeer := p2p.NewPeer(id, "fast-path-peer", nil)
	ethP := eth.NewPeer(eth.ETH68, p2pPeer, net, nil)
	witP := wit.NewPeer(wit.WIT2, p2pPeer, net, log.New())

	if err := ps.registerPeer(ethP, nil, witP); err != nil {
		t.Fatalf("register peer: %v", err)
	}
	return ps.peer(ethP.ID())
}

func TestPeerWithHighestTDSkipsBackedOffPeers(t *testing.T) {
	ps := newPeerSet()
	defer ps.close()

	low := registerPeerWithTD(t, ps, 10)
	high := registerPeerWithTD(t, ps, 20)

	best, retry := ps.peerWithHighestTD(func(id string) time.Duration {
		if id == high.ID() {
			return 30 * time.Second
		}
		return 0
	})
	if best == nil || best.ID() != low.ID() {
		t.Fatalf("best peer mismatch: have %v, want %v", best, low.ID())
	}
	if retry != 30*time.Second {
		t.Fatalf("retry delay mismatch: have %v, want %v", retry, 30*time.Second)
	}

	best, retry = ps.peerWithHighestTD(func(id string) time.Duration {
		if id == low.ID() {
			return 10 * time.Second
		}
		return time.Minute
	})
	if best != nil {
		t.Fatalf("unexpected peer selected while all peers were backed off: %v", best.ID())
	}
	if retry != 10*time.Second {
		t.Fatalf("retry delay mismatch: have %v, want %v", retry, 10*time.Second)
	}
}

func registerPeerWithTD(t *testing.T, ps *peerSet, td int64) *eth.Peer {
	t.Helper()

	app, net := p2p.MsgPipe()
	t.Cleanup(func() {
		app.Close()
		net.Close()
	})

	var id enode.ID
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatalf("failed to create peer id: %v", err)
	}

	peer := eth.NewPeer(eth.ETH68, p2p.NewPeer(id, "test", nil), net, nil)
	peer.SetHead(common.Hash{byte(td)}, big.NewInt(td))
	t.Cleanup(peer.Close)

	if err := ps.registerPeer(peer, nil, nil); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	return peer
}
