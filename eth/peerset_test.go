package eth

import (
	"crypto/rand"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

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
