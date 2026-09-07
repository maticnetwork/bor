package eth

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	ethproto "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/require"
)

// decodeTestWitness decodes canonical witness bytes back into a Witness, as a
// gossip receiver would before handing it to the broadcast handler.
func decodeTestWitness(t *testing.T, body []byte) *stateless.Witness {
	t.Helper()

	var witness stateless.Witness
	require.NoError(t, rlp.DecodeBytes(body, &witness))
	return &witness
}

// registerEthWitPeer registers an eth peer with an attached wit peer sharing
// the same enode ID — as in production, where both protocols run on one
// devp2p connection. Sharing the ID is what lets ID-keyed lookups (e.g. the
// cosend recipient map) match across the two protocol wrappers. Returns the
// wit peer at the requested version and a cleanup func.
func registerEthWitPeer(t *testing.T, h *testHandler, version uint) (*wit.Peer, func()) {
	t.Helper()

	var id enode.ID
	rand.Read(id[:])

	app, net := p2p.MsgPipe()
	ethApp, ethNet := p2p.MsgPipe()
	done := make(chan struct{})
	ethDone := make(chan struct{})
	go func() {
		for {
			msg, err := app.ReadMsg()
			if err != nil {
				close(done)
				return
			}
			msg.Discard()
		}
	}()
	go func() {
		for {
			msg, err := ethApp.ReadMsg()
			if err != nil {
				close(ethDone)
				return
			}
			msg.Discard()
		}
	}()

	witPeer := wit.NewPeer(version, p2p.NewPeer(id, "test-peer", nil), net, log.New())
	// A real MsgReadWriter is required: BroadcastBlock announces via the
	// plain eth path (AsyncSendNewBlockHash) once a block's witness is
	// cached, not only once HasBlock is true, so ethPeer's broadcast
	// goroutine can fire during these tests.
	ethPeer := ethproto.NewPeer(ethproto.ETH68, p2p.NewPeer(id, "test-eth-peer", nil), ethNet, nil)
	require.NoError(t, h.handler.peers.registerPeer(ethPeer, nil, witPeer))

	cleanup := func() {
		h.handler.peers.unregisterPeer(ethPeer.ID())
		app.Close()
		ethApp.Close()
		witPeer.Close()
		ethPeer.Close()
		<-done
		<-ethDone
	}
	return witPeer, cleanup
}

// TestPeerWit2TrackerBudgetLifecycle pins the token-bucket arithmetic of the
// announce rate limiter: a fresh peer starts at the burst cap, over-budget
// packets are rejected without going negative, idle time refills tokens up to
// (and not beyond) the cap, and forget resets the peer to a full budget.
func TestPeerWit2TrackerBudgetLifecycle(t *testing.T) {
	tr := newPeerWit2Tracker()

	// Fresh peer: full burst is allowed, one more announcement is not.
	require.True(t, tr.allow("p1", wit2AnnounceBurstCap))
	require.False(t, tr.allow("p1", 1), "budget must be exhausted after consuming the full burst")

	// Idle refill: backdate the last refill and confirm tokens come back at
	// the configured rate (1s → wit2AnnounceRefillPerSecond tokens).
	tr.mu.Lock()
	tr.state["p1"].lastRefill = time.Now().Add(-time.Second)
	tr.mu.Unlock()
	require.True(t, tr.allow("p1", wit2AnnounceRefillPerSecond/2))

	// Refill clamps at the burst cap: a long idle period must not bank more
	// than one full burst.
	tr.mu.Lock()
	tr.state["p1"].lastRefill = time.Now().Add(-time.Hour)
	tr.mu.Unlock()
	require.True(t, tr.allow("p1", wit2AnnounceBurstCap))
	require.False(t, tr.allow("p1", 1), "refill must clamp at the burst cap")

	// forget resets the peer: a full burst is available again.
	tr.forget("p1")
	require.True(t, tr.allow("p1", wit2AnnounceBurstCap))
}

// TestPeerWit2TrackerStrikeWindowReset verifies that strikes outside the decay
// window do not accumulate toward a disconnect: a peer striking at a rate
// below the limit-per-window is tolerated indefinitely (stray pre-fork
// content), while sustained misbehavior inside one window trips the limit.
func TestPeerWit2TrackerStrikeWindowReset(t *testing.T) {
	tr := newPeerWit2Tracker()

	for i := 0; i < wit2MisbehaviorStrikeLimit-1; i++ {
		require.False(t, tr.strike("p1"), "strike %d must stay under the limit", i)
	}

	// Age every recorded strike out of the window: the next strike must see
	// an empty sliding window instead of tripping the limit.
	tr.mu.Lock()
	st := tr.state["p1"]
	for i := range st.strikes {
		st.strikes[i] = time.Now().Add(-2 * wit2MisbehaviorWindow)
	}
	tr.mu.Unlock()
	require.False(t, tr.strike("p1"), "strike after every prior strike aged out must not trip the limit")
}

// TestPeerWit2TrackerStrikeSlidesAcrossWindowBoundary is the regression for
// the fixed/tumbling-window finding: with a tumbling window, a peer could
// land wit2MisbehaviorStrikeLimit-1 strikes right before the window's reset
// boundary and more right after, netting up to ~2x the documented budget
// indefinitely without ever tripping the limit. A true sliding window must
// still catch that clustering: once wit2MisbehaviorStrikeLimit strikes exist
// within any trailing wit2MisbehaviorWindow, the peer trips regardless of
// where the strikes fall relative to a fixed boundary.
func TestPeerWit2TrackerStrikeSlidesAcrossWindowBoundary(t *testing.T) {
	tr := newPeerWit2Tracker()

	// One strike lands, opening what a tumbling window would treat as its
	// fixed boundary.
	require.False(t, tr.strike("p1"))

	// Backdate it to just under the window edge, then land the rest of the
	// budget "just before" the boundary and "just after" it — exactly the
	// clustering a tumbling window would reset through undetected.
	tr.mu.Lock()
	tr.state["p1"].strikes[0] = time.Now().Add(-wit2MisbehaviorWindow + 50*time.Millisecond)
	tr.mu.Unlock()

	for i := 0; i < wit2MisbehaviorStrikeLimit-2; i++ {
		require.False(t, tr.strike("p1"), "strike %d must stay under the limit", i)
	}

	// This strike is the wit2MisbehaviorStrikeLimit-th one to land within the
	// trailing window — a sliding window must trip here even though a
	// tumbling window would have already reset past the first strike's
	// nominal boundary.
	require.True(t, tr.strike("p1"), "strikes clustered across where a tumbling window would reset must still trip the sliding-window limit")
}

// TestWitnessWaiterRegistryCapsAndExpiry covers the waiter registry's bounds:
// nil peers are ignored, expired waiters stop counting (and get GC'd), and
// both the distinct-hash cap and the per-hash peer cap refuse new entries
// rather than evicting live ones.
func TestWitnessWaiterRegistryCapsAndExpiry(t *testing.T) {
	r := newWitnessWaiterRegistry()
	hash := common.HexToHash("0x01")

	r.record(hash, nil)
	require.False(t, r.has(hash), "nil peer must not be recorded")
	require.Nil(t, r.take(hash), "take on an empty registry must return nil")

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	r.record(hash, peer)
	require.True(t, r.has(hash))

	// Expire the waiter: has() must turn false and take() must skip it.
	r.mu.Lock()
	r.waiters[hash][peer.ID()].at = time.Now().Add(-2 * witnessWaiterTTL)
	r.mu.Unlock()
	require.False(t, r.has(hash), "expired waiter must not count as live")
	require.Empty(t, r.take(hash), "expired waiter must not be returned")

	// Distinct-hash cap: once the registry is full of live hashes, recording
	// a waiter for a new hash is skipped (the peer keeps polling instead).
	for i := 0; i < witnessWaiterHashCap; i++ {
		r.record(common.BytesToHash([]byte(fmt.Sprintf("filler-%d", i))), peer)
	}
	overflow := common.HexToHash("0xfeed")
	r.record(overflow, peer)
	require.False(t, r.has(overflow), "hash over the registry cap must not be recorded")

	// Per-hash peer cap: a hash already at its waiter limit refuses new
	// peers but keeps serving the recorded ones.
	target := common.BytesToHash([]byte("filler-0"))
	r.mu.Lock()
	for i := 0; len(r.waiters[target]) < witnessWaiterPerHashCap; i++ {
		r.waiters[target][fmt.Sprintf("synthetic-%d", i)] = &witnessWaiter{peer: peer, at: time.Now()}
	}
	r.mu.Unlock()

	extra, cleanupExtra := newTestWit2PeerWithReader()
	defer cleanupExtra()
	r.record(target, extra)

	r.mu.Lock()
	_, recorded := r.waiters[target][extra.ID()]
	r.mu.Unlock()
	require.False(t, recorded, "peer over the per-hash cap must not be recorded")
}

// TestWaiterPushGuards covers the safety rails around the waiter push: nil
// witnesses and oversized bodies are never pushed (the latter falls back to
// the paged pull path), already-delivered waiters are skipped, a flush with
// no stored body is a no-op, and undecodable bytes are dropped without
// consuming the waiters.
func TestWaiterPushGuards(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	header := &types.Header{Number: big.NewInt(515)}
	hash := header.Hash()

	// Nil witness: nothing happens, waiter stays.
	h.handler.witnessWaiters.record(hash, peer)
	h.handler.pushWitnessToWaiters(hash, nil, 0)
	require.True(t, h.handler.witnessWaiters.has(hash))

	// Oversized witness: push is skipped, waiters stay on the pull path
	// (entry is NOT consumed by the size guard).
	witness, err := stateless.NewWitness(header, nil)
	require.NoError(t, err)
	h.handler.pushWitnessToWaiters(hash, witness, witnessPushMaxSize+1)
	require.True(t, h.handler.witnessWaiters.has(hash), "oversize guard must not consume waiters")

	// Waiter already knows the body: take() consumes the entry but the send
	// is skipped.
	peer.AddKnownWitness(hash)
	h.handler.pushWitnessToWaiters(hash, witness, 1024)
	require.False(t, h.handler.witnessWaiters.has(hash), "push must consume the waiter entry")

	// flush with no body in chain storage: no-op, waiter preserved.
	h.handler.witnessWaiters.record(hash, peer)
	h.handler.flushWitnessWaitersForImported(hash)
	require.True(t, h.handler.witnessWaiters.has(hash), "flush without a stored body must keep the waiter")

	// Undecodable bytes: decode fails, waiters preserved for the pull path.
	h.handler.pushWitnessBytesToWaiters(hash, []byte{0xde, 0xad, 0xbe, 0xef})
	require.True(t, h.handler.witnessWaiters.has(hash), "decode failure must not consume waiters")

	// Empty bytes / no waiter recorded: early returns.
	h.handler.pushWitnessBytesToWaiters(hash, nil)
	h.handler.pushWitnessBytesToWaiters(common.HexToHash("0x9999"), []byte{0x01})

	// Serving cache not wired (nil): cacheVerifiedWitnessForServing is a no-op.
	saved := h.handler.pendingWitnessBodies
	h.handler.pendingWitnessBodies = nil
	h.handler.cacheVerifiedWitnessForServing(hash, []byte{0x01}, common.Hash{})
	h.handler.pendingWitnessBodies = saved
}

// TestDeferredAnnounceCacheLifecycle covers the multi-candidate deferred-announce
// cache: one candidate per (blockHash, peerID) refreshed in place, multiple
// peers' candidates coexisting for the same block, the per-block / per-peer /
// global caps, and take/peekPeer/has treating TTL-expired entries as absent.
func TestDeferredAnnounceCacheLifecycle(t *testing.T) {
	annHash := func(block, witness byte) wit.SignedWitnessAnnouncement {
		return wit.SignedWitnessAnnouncement{
			BlockHash:   common.BytesToHash([]byte{block}),
			BlockNumber: uint64(block),
			WitnessHash: common.BytesToHash([]byte{0xc0, witness}),
			Signature:   make([]byte, wit.SignatureLength),
		}
	}
	ann := func(b byte) wit.SignedWitnessAnnouncement { return annHash(b, b) }

	// Tiny capacity still yields a usable per-peer share of 1.
	tiny := newDeferredAnnounceCache(1)
	require.Equal(t, 1, tiny.perPeerCap)

	c := newDeferredAnnounceCache(64)

	// Re-put from the same peer for the same block refreshes in place (no new
	// slot); a second peer's announce for the same block coexists as a distinct
	// candidate rather than evicting the first.
	c.put(ann(1), "peer-a")
	c.put(annHash(1, 0xaa), "peer-a") // same peer, revised hash: in place
	c.put(ann(1), "peer-b")           // different peer: second candidate
	c.mu.Lock()
	require.Len(t, c.entries[ann(1).BlockHash], 2, "two distinct peers must yield two candidates")
	require.Equal(t, 1, c.perPeer["peer-a"], "same-peer re-put must not consume a second slot")
	require.Equal(t, 1, c.perPeer["peer-b"])
	c.mu.Unlock()
	peerID, ok := c.peekPeer(ann(1).BlockHash, nil)
	require.True(t, ok)
	require.Equal(t, "peer-b", peerID, "peekPeer returns the freshest candidate's relayer")

	// Per-block cap: distinct peers beyond the cap are dropped, not evicting
	// the candidates already present.
	pb := newDeferredAnnounceCache(256)
	for i := range deferredAnnounceMaxCandidatesPerBlock + 5 {
		pb.put(ann(1), fmt.Sprintf("pb-peer-%d", i))
	}
	pb.mu.Lock()
	require.Len(t, pb.entries[ann(1).BlockHash], deferredAnnounceMaxCandidatesPerBlock,
		"per-block candidate count must be capped")
	pb.mu.Unlock()

	// Per-peer cap: one peer cannot occupy more than its share across blocks.
	for i := byte(10); i < 40; i++ {
		c.put(ann(i), "hog")
	}
	c.mu.Lock()
	hogCount := c.perPeer["hog"]
	c.mu.Unlock()
	require.LessOrEqual(t, hogCount, c.perPeerCap, "per-peer cap must bound a single peer's share")

	// Global cap: filling from distinct peers evicts the oldest entry.
	full := newDeferredAnnounceCache(2)
	full.put(ann(1), "p1")
	full.mu.Lock()
	full.entries[ann(1).BlockHash][0].receivedAt = time.Now().Add(-10 * time.Second)
	full.mu.Unlock()
	full.put(ann(2), "p2")
	full.put(ann(3), "p3")
	require.False(t, full.has(ann(1).BlockHash), "oldest entry must be evicted at capacity")
	require.True(t, full.has(ann(2).BlockHash))
	require.True(t, full.has(ann(3).BlockHash))

	// Expiry: take/peekPeer/has all treat a TTL-expired entry as gone.
	exp := newDeferredAnnounceCache(4)
	exp.put(ann(7), "p7")
	exp.mu.Lock()
	exp.entries[ann(7).BlockHash][0].receivedAt = time.Now().Add(-2 * wit2AnnounceTTL)
	exp.mu.Unlock()
	require.False(t, exp.has(ann(7).BlockHash))
	_, ok = exp.peekPeer(ann(7).BlockHash, nil)
	require.False(t, ok)
	_, ok = exp.take(ann(7).BlockHash)
	require.False(t, ok, "expired entry must not be returned by take")

	// Miss paths.
	_, ok = exp.take(common.HexToHash("0xabsent"))
	require.False(t, ok)
	_, ok = exp.peekPeer(common.HexToHash("0xabsent"), nil)
	require.False(t, ok)
}

// TestPeekPeerSkipsDeadCandidates covers the multi-candidate fetch fallback:
// when the freshest deferred candidate's relayer has disconnected, peekPeer
// must skip it and return an older candidate whose peer is still live, rather
// than surfacing the dead peer and stranding the consumer. Deferred candidates
// are deliberately retained across disconnect, so without the liveness filter
// the dead-peer entry would keep winning by freshness until the TTL.
func TestPeekPeerSkipsDeadCandidates(t *testing.T) {
	ann := func(b byte) wit.SignedWitnessAnnouncement {
		return wit.SignedWitnessAnnouncement{
			BlockHash:   common.BytesToHash([]byte{b}),
			BlockNumber: uint64(b),
			WitnessHash: common.BytesToHash([]byte{0xc0, b}),
			Signature:   make([]byte, wit.SignatureLength),
		}
	}

	c := newDeferredAnnounceCache(64)
	hash := ann(1).BlockHash
	c.put(ann(1), "live-old")
	c.put(ann(1), "dead-fresh")

	// Make "dead-fresh" strictly the freshest candidate so it would win a
	// freshness-only scan.
	c.mu.Lock()
	for _, e := range c.entries[hash] {
		if e.peerID == "live-old" {
			e.receivedAt = time.Now().Add(-5 * time.Second)
		}
	}
	c.mu.Unlock()

	live := func(id string) bool { return id != "dead-fresh" }

	peerID, ok := c.peekPeer(hash, live)
	require.True(t, ok, "an older live candidate must still be reachable")
	require.Equal(t, "live-old", peerID, "peekPeer must skip the freshest dead peer for a live candidate")

	// With every candidate dead, there is no target.
	_, ok = c.peekPeer(hash, func(string) bool { return false })
	require.False(t, ok, "no live candidate means no pull target")

	// A nil predicate keeps the original freshest-wins behavior.
	peerID, ok = c.peekPeer(hash, nil)
	require.True(t, ok)
	require.Equal(t, "dead-fresh", peerID, "nil predicate treats every candidate as eligible")
}

// TestVerifySignedAnnouncementRejectsBadRecoveryID covers the ecrecover
// failure branch: a signature of the right length whose recovery byte is out
// of range must be rejected (not panic, not recover a garbage address).
func TestVerifySignedAnnouncementRejectsBadRecoveryID(t *testing.T) {
	sig := make([]byte, wit.SignatureLength)
	sig[wit.SignatureLength-1] = 99 // invalid recovery id

	_, err := verifySignedAnnouncement(wit.SignedWitnessAnnouncement{
		BlockHash:   common.HexToHash("0x01"),
		BlockNumber: 1,
		WitnessHash: common.HexToHash("0x02"),
		Signature:   sig,
	})
	require.Error(t, err)
}

// TestCosendWitnessAnnouncementVersionSplit verifies the per-peer protocol
// split on the block-propagation cosend: WIT2 recipients get the signed
// announcement, WIT1 recipients get the unsigned hash announce, and the whole
// cosend is skipped when the local node does not hold the witness.
func TestCosendWitnessAnnouncementVersionSplit(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	wit2Peer, cleanup2 := registerEthWitPeer(t, h, wit.WIT2)
	defer cleanup2()
	wit1Peer, cleanup1 := registerEthWitPeer(t, h, wit.WIT1)
	defer cleanup1()

	header := &types.Header{Number: big.NewInt(616)}
	hash := header.Hash()
	transfer := []*ethPeer{
		h.handler.peers.peer(wit2Peer.ID()),
		h.handler.peers.peer(wit1Peer.ID()),
	}
	require.NotNil(t, transfer[0])
	require.NotNil(t, transfer[1])

	// Witness not held locally: cosend must be a no-op for everyone.
	h.handler.cosendWitnessAnnouncement(hash, header.Number.Uint64(), transfer, nil)
	require.False(t, wit2Peer.KnownAnnounceContainsHash(hash))
	require.False(t, wit1Peer.KnownWitnessContainsHash(hash))

	// Store the witness and a signed announcement (as if relayed to us), then
	// cosend: the WIT2 peer gets the signed announce, the WIT1 peer the
	// unsigned hash announce.
	rawdb.WriteWitness(h.chain.DB(), hash, []byte{0x01, 0x02, 0x03})
	h.handler.signedWitnesses.putIfNewer(wit.SignedWitnessAnnouncement{
		BlockHash:   hash,
		BlockNumber: header.Number.Uint64(),
		WitnessHash: common.HexToHash("0xc0de"),
		Signature:   make([]byte, wit.SignatureLength),
	})

	h.handler.cosendWitnessAnnouncement(hash, header.Number.Uint64(), transfer, nil)
	require.True(t, wit2Peer.KnownAnnounceContainsHash(hash), "WIT2 peer must receive the signed announcement")
	require.False(t, wit2Peer.KnownWitnessContainsHash(hash), "signed announce must not mark the peer as a body-holder")
	require.True(t, wit1Peer.KnownWitnessContainsHash(hash), "WIT1 peer must receive the unsigned hash announce")

	// lookupSignedWitnessHash round-trip: hit for the cached announce, miss
	// for an unknown hash.
	got, ok := h.handler.lookupSignedWitnessHash(hash)
	require.True(t, ok)
	require.Equal(t, common.HexToHash("0xc0de"), got)
	_, ok = h.handler.lookupSignedWitnessHash(common.HexToHash("0xabsent"))
	require.False(t, ok)

	// Re-cosend via the static/trusted list: both peers now know the witness,
	// so they are absent from the recipient map and skipped gracefully.
	h.handler.cosendWitnessAnnouncement(hash, header.Number.Uint64(), nil, transfer)
}

// TestSignLocalWitnessAnnouncementFallbacks pins the non-producer behavior of
// the announce signing path: a cached announcement (ours or a relayed
// producer's) is returned without re-signing, and absent both a cache entry
// and a bor engine the function reports no signature — the caller then falls
// back to the truthful unsigned WIT1 announce.
func TestSignLocalWitnessAnnouncementFallbacks(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	header := &types.Header{Number: big.NewInt(717)}
	hash := header.Hash()

	// No cache entry, non-bor engine: no signature available.
	_, ok := h.handler.signLocalWitnessAnnouncement(hash, header.Number.Uint64())
	require.False(t, ok, "non-bor engine without a cached announce must not produce a signature")

	// Cached announcement: returned as-is, no engine interaction.
	cached := wit.SignedWitnessAnnouncement{
		BlockHash:   hash,
		BlockNumber: header.Number.Uint64(),
		WitnessHash: common.HexToHash("0xbeef"),
		Signature:   make([]byte, wit.SignatureLength),
	}
	h.handler.signedWitnesses.putIfNewer(cached)
	got, ok := h.handler.signLocalWitnessAnnouncement(hash, header.Number.Uint64())
	require.True(t, ok)
	require.Equal(t, cached.WitnessHash, got.WitnessHash)
}

// TestBroadcastBlockWitnessAnnounceVersionSplit covers the post-import
// announce fanout in BroadcastBlock: with a witness in storage, WIT2 peers
// receive the signed announcement when one is available while WIT1 peers
// receive the unsigned hash announce — and with no signature available,
// everyone receives the truthful unsigned announce.
func TestBroadcastBlockWitnessAnnounceVersionSplit(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	wit2Peer, cleanup2 := registerEthWitPeer(t, h, wit.WIT2)
	defer cleanup2()
	wit1Peer, cleanup1 := registerEthWitPeer(t, h, wit.WIT1)
	defer cleanup1()

	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(818)})
	hash := block.Hash()
	rawdb.WriteWitness(h.chain.DB(), hash, []byte{0x0a, 0x0b})

	// No signature available (non-bor engine, nothing cached): both peers get
	// the unsigned WIT1-style hash announce.
	h.handler.BroadcastBlock(block, nil, false)
	require.True(t, wit2Peer.KnownWitnessContainsHash(hash), "WIT2 peer must get the unsigned announce when no signature exists")
	require.True(t, wit1Peer.KnownWitnessContainsHash(hash))

	// With a signed announcement cached: the WIT2 peer (not yet aware of the
	// announce) receives the signed variant.
	block2 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(819)})
	hash2 := block2.Hash()
	rawdb.WriteWitness(h.chain.DB(), hash2, []byte{0x0c, 0x0d})
	h.handler.signedWitnesses.putIfNewer(wit.SignedWitnessAnnouncement{
		BlockHash:   hash2,
		BlockNumber: block2.NumberU64(),
		WitnessHash: common.HexToHash("0xf00d"),
		Signature:   make([]byte, wit.SignatureLength),
	})

	h.handler.BroadcastBlock(block2, nil, false)
	require.True(t, wit2Peer.KnownAnnounceContainsHash(hash2), "WIT2 peer must get the signed announce")
	require.True(t, wit1Peer.KnownWitnessContainsHash(hash2), "WIT1 peer must get the unsigned announce")
}

// signTestAnnouncement produces a structurally valid BP signature over the
// announcement triple with a throwaway key.
func signTestAnnouncement(t *testing.T, ann *wit.SignedWitnessAnnouncement) {
	t.Helper()

	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	digest := wit.WitnessAnnouncementSigningHash(ann.BlockHash, ann.BlockNumber, ann.WitnessHash)
	sig, err := crypto.Sign(digest.Bytes(), key)
	require.NoError(t, err)
	ann.Signature = sig
}

// TestHandleSignedWitnessAnnouncementsAcceptCacheRelayAndDedup drives the full
// receive path on a non-bor test chain (producer binding reduces to
// header-number matching): a valid announce is accepted, cached, credited to
// the sender, and relayed to other WIT2 peers but not WIT1 peers; an
// immediate duplicate is suppressed.
func TestHandleSignedWitnessAnnouncementsAcceptCacheRelayAndDedup(t *testing.T) {
	h := newTestHandler()
	defer h.close()
	witH := (*witHandler)(h.handler)

	sender, cleanupS := registerEthWitPeer(t, h, wit.WIT2)
	defer cleanupS()
	relayTarget, cleanupR := registerEthWitPeer(t, h, wit.WIT2)
	defer cleanupR()
	wit1Peer, cleanup1 := registerEthWitPeer(t, h, wit.WIT1)
	defer cleanup1()

	header := &types.Header{Number: big.NewInt(919)}
	hash := header.Hash()
	rawdb.WriteHeader(h.chain.DB(), header)

	ann := wit.SignedWitnessAnnouncement{
		BlockHash:   hash,
		BlockNumber: header.Number.Uint64(),
		WitnessHash: common.HexToHash("0xab"),
	}
	signTestAnnouncement(t, &ann)

	require.NoError(t, witH.handleSignedWitnessAnnouncements(sender, []wit.SignedWitnessAnnouncement{ann}))

	require.True(t, sender.KnownAnnounceContainsHash(hash), "sender must be credited as announce-known")
	_, cached := h.handler.signedWitnesses.get(hash)
	require.True(t, cached, "accepted announce must be cached")
	require.True(t, relayTarget.KnownAnnounceContainsHash(hash), "announce must relay to other WIT2 peers")
	require.False(t, wit1Peer.KnownAnnounceContainsHash(hash), "announce must not relay to WIT1 peers")

	// Re-delivery inside the relay window: dedup path, no error.
	require.NoError(t, witH.handleSignedWitnessAnnouncements(sender, []wit.SignedWitnessAnnouncement{ann}))
}

// TestHandleSignedWitnessAnnouncementsRateLimitDrop verifies that a peer over
// its announce budget has the whole packet dropped without verification and
// without strikes — rate limiting is back-pressure, not misbehavior.
func TestHandleSignedWitnessAnnouncementsRateLimitDrop(t *testing.T) {
	h := newTestHandler()
	defer h.close()
	witH := (*witHandler)(h.handler)

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	// Exhaust the budget out-of-band.
	require.True(t, h.handler.wit2PeerTracker.allow(peer.ID(), wit2AnnounceBurstCap))

	ann := wit.SignedWitnessAnnouncement{
		BlockHash:   common.HexToHash("0x77"),
		BlockNumber: 77,
		WitnessHash: common.HexToHash("0x78"),
		Signature:   make([]byte, wit.SignatureLength),
	}
	require.NoError(t, witH.handleSignedWitnessAnnouncements(peer, []wit.SignedWitnessAnnouncement{ann}))

	_, cached := h.handler.signedWitnesses.get(ann.BlockHash)
	require.False(t, cached, "rate-limited packet must not be processed")

	h.handler.wit2PeerTracker.mu.Lock()
	strikes := len(h.handler.wit2PeerTracker.state[peer.ID()].strikes)
	h.handler.wit2PeerTracker.mu.Unlock()
	require.Zero(t, strikes, "rate limiting must not strike the peer")
}

// TestAcceptSignedAnnouncementStrikesOnNumberMismatch covers the confirmed-
// misbehavior branch with a locally known header: the announce's blockNumber
// contradicts the header it names, so the relayer is struck (no deferral —
// the header IS available).
func TestAcceptSignedAnnouncementStrikesOnNumberMismatch(t *testing.T) {
	h := newTestHandler()
	defer h.close()
	witH := (*witHandler)(h.handler)

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	header := &types.Header{Number: big.NewInt(303)}
	rawdb.WriteHeader(h.chain.DB(), header)

	ann := wit.SignedWitnessAnnouncement{
		BlockHash:   header.Hash(),
		BlockNumber: header.Number.Uint64() + 1, // contradicts the local header
		WitnessHash: common.HexToHash("0xcc"),
	}
	signTestAnnouncement(t, &ann)

	require.False(t, witH.acceptSignedAnnouncement(peer, ann))
	require.False(t, h.handler.deferredAnnounces.has(ann.BlockHash), "known-header mismatch must not defer")

	h.handler.wit2PeerTracker.mu.Lock()
	strikes := len(h.handler.wit2PeerTracker.state[peer.ID()].strikes)
	h.handler.wit2PeerTracker.mu.Unlock()
	require.Equal(t, 1, strikes, "confirmed mis-binding must strike the relayer")
}

// TestStrikeWit2PeerDisconnectsAtLimit drives the strike accumulator to the
// disconnect threshold and confirms the tracker state is cleaned up via the
// removePeer → forget path (the peer is not in the peer set; removal must
// still be graceful).
func TestStrikeWit2PeerDisconnectsAtLimit(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	for i := 0; i < wit2MisbehaviorStrikeLimit; i++ {
		h.handler.strikeWit2Peer(peer)
	}

	h.handler.wit2PeerTracker.mu.Lock()
	_, tracked := h.handler.wit2PeerTracker.state[peer.ID()]
	h.handler.wit2PeerTracker.mu.Unlock()
	require.False(t, tracked, "disconnect must forget the peer's tracker state")
}

// TestUnregisterPeerForgetsWit2TrackerState is the regression for the
// peerWit2Tracker leak (review finding C-1): forget() was wired only to
// removePeer (proactive drops), not to unregisterPeer — the universal teardown
// that runs on every clean/remote-initiated disconnect. A peer that connects,
// sends one announce (seeding tracker state), then disconnects on its own thus
// leaked a per-peer entry forever, with no GC. The fix forgets in unregisterPeer.
func TestUnregisterPeerForgetsWit2TrackerState(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	const peerID = "wit2-leaker"
	// Seed tracker state exactly as an inbound announce does (allow() on first
	// contact creates the per-peer entry).
	require.True(t, h.handler.wit2PeerTracker.allow(peerID, 1))
	h.handler.wit2PeerTracker.mu.Lock()
	_, seeded := h.handler.wit2PeerTracker.state[peerID]
	h.handler.wit2PeerTracker.mu.Unlock()
	require.True(t, seeded, "precondition: announce must seed tracker state")

	// The universal teardown path for a clean / remote-initiated disconnect.
	h.handler.unregisterPeer(peerID)

	h.handler.wit2PeerTracker.mu.Lock()
	_, tracked := h.handler.wit2PeerTracker.state[peerID]
	h.handler.wit2PeerTracker.mu.Unlock()
	require.False(t, tracked, "clean disconnect (unregisterPeer) must forget wit2 tracker state; otherwise it leaks one entry per peer with no GC")
}

// TestUnregisterPeerForgetsWitnessWaiters is the regression for the witness-waiter
// registry leak (review finding H2.1): witnessWaiters retained a *wit.Peer keyed
// by hash->peerID with no cleanup on disconnect, unlike wit2PeerTracker which is
// forgotten in unregisterPeer. A peer that asked for a not-yet-available witness
// then disconnected left its peer pointer (and the per-peer caches it pins)
// recorded until the 30s TTL. The fix forgets the peer from every waiter bucket
// on the universal teardown path, symmetric with the C-1 tracker fix.
func TestUnregisterPeerForgetsWitnessWaiters(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	header := &types.Header{Number: big.NewInt(701)}
	hash := header.Hash()

	// A peer asks for a witness we don't yet hold: record() is the same call
	// handleGetWitness makes when a signed announce is on file but the body has
	// not arrived.
	h.handler.witnessWaiters.record(hash, peer)
	require.True(t, h.handler.witnessWaiters.has(hash), "precondition: peer must be recorded as a waiter")

	// The universal teardown path for a clean / remote-initiated disconnect.
	h.handler.unregisterPeer(peer.ID())

	require.False(t, h.handler.witnessWaiters.has(hash),
		"clean disconnect (unregisterPeer) must forget witness-waiter state; otherwise a departed peer's *wit.Peer is retained until the 30s TTL")
}

// TestOnBlockImportedDropsPendingWitnessBody is the regression for the pre-import
// body cache leak (review finding H3.1): pendingWitnessBodyCache.drop() had no
// production caller, so bytes cached for pre-import serving were never released on
// import — they lingered until capacity eviction or the 30s TTL, holding up to
// ~500MB longer than the "dropped after the body is written to chain storage"
// contract the cache documents. The fix drops the entry from the per-import
// chain-head hook the moment the body is in chain storage (servable from rawdb).
func TestOnBlockImportedDropsPendingWitnessBody(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	header := &types.Header{Number: big.NewInt(702)}
	hash := header.Hash()
	body := []byte{0x01, 0x02, 0x03, 0x04}

	h.handler.pendingWitnessBodies.put(hash, body, crypto.Keccak256Hash(body))
	if _, _, ok := h.handler.pendingWitnessBodies.get(hash); !ok {
		t.Fatal("precondition: body must be cached pre-import")
	}

	// Simulate the block's header becoming local — the chain-head loop runs this
	// per imported block.
	h.handler.onBlockImported(hash)

	if _, _, ok := h.handler.pendingWitnessBodies.get(hash); ok {
		t.Fatal("import must drop the redundant pre-import body (now servable from chain storage); leaving it pins up to ~500MB past the TTL")
	}
}

// TestStrikeWit2PeerJailsAtLimit is the regression for the strike-discipline gap
// (review finding D-1): crossing the wit2 strike limit only disconnected the peer
// (removePeer) and wiped its strike state (forget), so it could re-dial with a
// clean ledger immediately. The byte-verification violation path already jails;
// the announce-strike path must too, so a forger is held off for the jail period.
func TestStrikeWit2PeerJailsAtLimit(t *testing.T) {
	setup := setupJailPeerTest(t, true)
	h := setup.handler

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()
	nodeID, err := enode.ParseID(peer.ID())
	require.NoError(t, err)

	for range wit2MisbehaviorStrikeLimit - 1 {
		h.strikeWit2Peer(peer)
	}
	require.False(t, wit2PeerIsJailed(t, setup.p2pServer, nodeID), "must not jail before crossing the strike limit")

	h.strikeWit2Peer(peer) // crosses the threshold
	require.True(t, wit2PeerIsJailed(t, setup.p2pServer, nodeID), "crossing the wit2 strike limit must jail the peer, not just disconnect it")
}

// TestStrikeWit2PeerByIDJailsAtLimit confirms the handler-side target of the
// witness-manager byte-mismatch striker (E-1/E-2 wiring): repeated byte-serving
// strikes share the announce strike budget and disconnect+jail at the threshold.
func TestStrikeWit2PeerByIDJailsAtLimit(t *testing.T) {
	setup := setupJailPeerTest(t, true)
	h := setup.handler

	var nodeID enode.ID
	rand.Read(nodeID[:])
	peerID := nodeID.String()

	for range wit2MisbehaviorStrikeLimit - 1 {
		h.strikeWit2PeerByID(peerID)
	}
	require.False(t, wit2PeerIsJailed(t, setup.p2pServer, nodeID), "must not jail before crossing the strike limit")

	h.strikeWit2PeerByID(peerID) // crosses the threshold
	require.True(t, wit2PeerIsJailed(t, setup.p2pServer, nodeID), "byte-serving strikes must disconnect+jail at the limit")
}

// TestHandleWitnessBroadcastSignedMatchCachesAndServes covers the WIT2 accept
// path of the unsolicited-body broadcast: bytes matching the BP-signed
// commitment are cached for pre-import serving and the sender is marked as a
// body-holder.
func TestHandleWitnessBroadcastSignedMatchCachesAndServes(t *testing.T) {
	h := newTestHandler()
	defer h.close()
	witH := (*witHandler)(h.handler)

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	hash, bodyBytes, _ := persistedSignedWitness(t, h, 1021, 0)

	witness := decodeTestWitness(t, bodyBytes)
	require.NoError(t, witH.handleWitnessBroadcast(peer, witness))

	require.True(t, peer.KnownWitnessContainsHash(hash), "matching broadcast must mark the sender as a body-holder")
	cachedBytes, _, ok := h.handler.pendingWitnessBodies.get(hash)
	require.True(t, ok, "matching broadcast must populate the pre-import serving cache")
	require.Equal(t, bodyBytes, cachedBytes)
}

// TestWitHandlerDispatchesSignedAnnouncementPacket pins the Handle() routing
// for the WIT2 message type so a wire-decoded packet reaches the signed-
// announcement handler (an empty packet is a no-op, not an error).
func TestWitHandlerDispatchesSignedAnnouncementPacket(t *testing.T) {
	h := newTestHandler()
	defer h.close()
	witH := (*witHandler)(h.handler)

	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()

	require.NoError(t, witH.Handle(peer, &wit.SignedNewWitnessHashesPacket{}))
}

// TestDrainDeferredAnnouncesLifecycle drives the chain-head drain through its
// four outcomes: header still unknown (re-stash for the next head event),
// confirmed mis-binding (drop, no cache), success (cache + credit the
// original sender + relay), and duplicate (suppressed by the relay-window
// dedup). Uses the non-bor test chain, where producer binding reduces to
// header-number matching.
func TestDrainDeferredAnnouncesLifecycle(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	sender, cleanupS := registerEthWitPeer(t, h, wit.WIT2)
	defer cleanupS()
	relayTarget, cleanupR := registerEthWitPeer(t, h, wit.WIT2)
	defer cleanupR()

	// Empty queue: no-op.
	h.handler.drainDeferredAnnouncesFor(common.HexToHash("0x01"))

	// Header still unknown at drain time: entry must be re-stashed.
	unknown := &types.Header{Number: big.NewInt(2222)}
	annU := wit.SignedWitnessAnnouncement{BlockHash: unknown.Hash(), BlockNumber: 2222, WitnessHash: common.HexToHash("0xaa")}
	signTestAnnouncement(t, &annU)
	h.handler.deferredAnnounces.put(annU, sender.ID())
	h.handler.drainDeferredAnnouncesFor(annU.BlockHash)
	require.True(t, h.handler.deferredAnnounces.has(annU.BlockHash), "header-unknown drain must re-stash the entry")

	// Confirmed mis-binding (known header, contradicting number): dropped.
	hdrM := &types.Header{Number: big.NewInt(3333)}
	rawdb.WriteHeader(h.chain.DB(), hdrM)
	annM := wit.SignedWitnessAnnouncement{BlockHash: hdrM.Hash(), BlockNumber: 3334, WitnessHash: common.HexToHash("0xbb")}
	signTestAnnouncement(t, &annM)
	h.handler.deferredAnnounces.put(annM, sender.ID())
	h.handler.drainDeferredAnnouncesFor(annM.BlockHash)
	require.False(t, h.handler.deferredAnnounces.has(annM.BlockHash), "mis-bound announce must be dropped")
	_, cached := h.handler.signedWitnesses.get(annM.BlockHash)
	require.False(t, cached, "mis-bound announce must not be cached")

	// Success: cached, original sender credited, relayed to other WIT2 peers.
	hdrS := &types.Header{Number: big.NewInt(4444)}
	rawdb.WriteHeader(h.chain.DB(), hdrS)
	annS := wit.SignedWitnessAnnouncement{BlockHash: hdrS.Hash(), BlockNumber: 4444, WitnessHash: common.HexToHash("0xcc")}
	signTestAnnouncement(t, &annS)
	h.handler.deferredAnnounces.put(annS, sender.ID())
	h.handler.drainDeferredAnnouncesFor(annS.BlockHash)
	_, cached = h.handler.signedWitnesses.get(annS.BlockHash)
	require.True(t, cached, "verified announce must be cached at drain")
	require.True(t, sender.KnownAnnounceContainsHash(annS.BlockHash), "drain must credit the original sender")
	require.True(t, relayTarget.KnownAnnounceContainsHash(annS.BlockHash), "drain must relay to other WIT2 peers")

	// Duplicate: a re-deferred copy of an already-cached hash is suppressed.
	h.handler.deferredAnnounces.put(annS, sender.ID())
	h.handler.drainDeferredAnnouncesFor(annS.BlockHash)
}

// TestPendingWitnessBodyCachePutOverwriteKeepsCapacity guards against the
// eviction-on-overwrite bug: at capacity, a second put for a hash already in
// the cache must overwrite in place without evicting an unrelated live entry.
func TestPendingWitnessBodyCachePutOverwriteKeepsCapacity(t *testing.T) {
	const capacity = 4
	bodies := newPendingWitnessBodyCache(capacity)
	hashes := make([]common.Hash, capacity)
	for i := range capacity {
		hashes[i] = common.BytesToHash([]byte{byte(i + 1)})
		bodies.put(hashes[i], []byte{byte(i + 1)}, common.BytesToHash([]byte{byte(0xa0 + i)}))
	}
	require.Len(t, bodies.entries, capacity, "cache should be full")

	// Overwrite an existing key while at capacity (the fetch + broadcast race).
	bodies.put(hashes[0], []byte{0xff}, common.BytesToHash([]byte{0xff}))

	require.Len(t, bodies.entries, capacity, "overwrite at capacity must not shrink the cache")
	for _, h := range hashes {
		_, _, ok := bodies.get(h)
		require.True(t, ok, "no unrelated entry should have been evicted by an overwrite: %s", h.Hex())
	}
}

// TestCacheGCSweepsExpiredEntries drives the TTL gc branch of each wit2
// cache: an entry older than the TTL must be dropped by the next write,
// including the relayer-credit refund in the deferred cache and the
// emptied-hash map cleanup in the waiter registry.
func TestCacheGCSweepsExpiredEntries(t *testing.T) {
	stale := time.Now().Add(-2 * wit2AnnounceTTL)
	hashA := common.HexToHash("0x0a")
	hashB := common.HexToHash("0x0b")

	// pendingWitnessBodyCache: gc fires on put.
	bodies := newPendingWitnessBodyCache(4)
	bodies.put(hashA, []byte{0x01}, common.HexToHash("0xa1"))
	bodies.mu.Lock()
	bodies.entries[hashA].receivedAt = stale
	bodies.mu.Unlock()
	bodies.put(hashB, []byte{0x02}, common.HexToHash("0xb1"))
	bodies.mu.Lock()
	_, sweptBody := bodies.entries[hashA]
	bodies.mu.Unlock()
	require.False(t, sweptBody, "expired pending body must be swept on the next put")

	// witnessWaiterRegistry: gc fires on record; sweeping the expired waiter
	// must also drop the now-empty per-hash map.
	peer, cleanup := newTestWit2PeerWithReader()
	defer cleanup()
	reg := newWitnessWaiterRegistry()
	reg.record(hashA, peer)
	reg.mu.Lock()
	reg.waiters[hashA][peer.ID()].at = stale
	reg.mu.Unlock()
	reg.record(hashB, peer)
	reg.mu.Lock()
	_, sweptWaiter := reg.waiters[hashA]
	reg.mu.Unlock()
	require.False(t, sweptWaiter, "expired waiter hash must be swept on the next record")

	// deferredAnnounceCache: gc fires on put and must refund the relayer's
	// per-peer credit.
	deferred := newDeferredAnnounceCache(8)
	deferred.put(wit.SignedWitnessAnnouncement{BlockHash: hashA, Signature: make([]byte, wit.SignatureLength)}, "relayer")
	deferred.mu.Lock()
	deferred.entries[hashA][0].receivedAt = stale
	deferred.mu.Unlock()
	deferred.put(wit.SignedWitnessAnnouncement{BlockHash: hashB, Signature: make([]byte, wit.SignatureLength)}, "other")
	deferred.mu.Lock()
	_, sweptDeferred := deferred.entries[hashA]
	credit := deferred.perPeer["relayer"]
	deferred.mu.Unlock()
	require.False(t, sweptDeferred, "expired deferred announce must be swept on the next put")
	require.Zero(t, credit, "swept deferred announce must refund its relayer credit")

	// signedWitnessCache: gc fires on putIfNewer.
	signed := newSignedWitnessCache()
	signed.putIfNewer(wit.SignedWitnessAnnouncement{BlockHash: hashA, Signature: make([]byte, wit.SignatureLength)})
	signed.mu.Lock()
	signed.entries[hashA].receivedAt = stale
	signed.mu.Unlock()
	signed.putIfNewer(wit.SignedWitnessAnnouncement{BlockHash: hashB, Signature: make([]byte, wit.SignatureLength)})
	signed.mu.Lock()
	_, sweptSigned := signed.entries[hashA]
	signed.mu.Unlock()
	require.False(t, sweptSigned, "expired signed announce must be swept on the next putIfNewer")
}

// TestSignedWitnessCachePutIfNewer covers putIfNewer's admission rules: a fresh
// hash is stored (relay), a conflicting WitnessHash for a live entry is rejected
// outright (anti cache-poisoning — the first valid signed commitment wins), and
// a duplicate of the same commitment within the relay window is suppressed.
func TestSignedWitnessCachePutIfNewer(t *testing.T) {
	var (
		blockHash = common.HexToHash("0xb10c")
		hashOne   = common.HexToHash("0xc0mm1")
		hashTwo   = common.HexToHash("0xc0mm2")
		sig       = make([]byte, wit.SignatureLength)
	)
	c := newSignedWitnessCache()

	if !c.putIfNewer(wit.SignedWitnessAnnouncement{BlockHash: blockHash, WitnessHash: hashOne, Signature: sig}) {
		t.Fatal("first announce for a block must be admitted (relay)")
	}
	// Conflicting WitnessHash for the same block: rejected, and the original entry
	// must survive unchanged — an attacker with a second valid signature must not
	// be able to displace the honest commitment mid-fetch.
	if c.putIfNewer(wit.SignedWitnessAnnouncement{BlockHash: blockHash, WitnessHash: hashTwo, Signature: sig}) {
		t.Fatal("a conflicting WitnessHash for a live entry must be rejected")
	}
	if got, ok := c.get(blockHash); !ok || got.WitnessHash != hashOne {
		t.Fatalf("conflicting announce must not overwrite the first commitment, got %x (ok=%v)", got.WitnessHash, ok)
	}
	// Same commitment again within the relay window: duplicate, suppress relay.
	if c.putIfNewer(wit.SignedWitnessAnnouncement{BlockHash: blockHash, WitnessHash: hashOne, Signature: sig}) {
		t.Fatal("a duplicate announce within the relay window must be suppressed")
	}
}

// TestDrainDeferredAnnouncesGuards covers the drain entry guards: a handler
// wired without wit2 state must no-op rather than panic, and a stashed
// announcement that fails the signature re-check (in principle unreachable —
// the same bytes passed verification before deferral) is dropped without
// being cached or relayed.
func TestDrainDeferredAnnouncesGuards(t *testing.T) {
	(&handler{}).drainDeferredAnnouncesFor(common.HexToHash("0x01"))

	h := newTestHandler()
	defer h.close()

	bad := wit.SignedWitnessAnnouncement{
		BlockHash:   common.HexToHash("0x0bad"),
		BlockNumber: 1,
		WitnessHash: common.HexToHash("0x0bb1"),
		Signature:   make([]byte, wit.SignatureLength), // all-zero: recovery fails
	}
	h.handler.deferredAnnounces.put(bad, "relayer")
	h.handler.drainDeferredAnnouncesFor(bad.BlockHash)

	_, promoted := h.handler.signedWitnesses.get(bad.BlockHash)
	require.False(t, promoted, "announce failing the sig re-check must not be promoted")
	require.False(t, h.handler.deferredAnnounces.has(bad.BlockHash), "failed drain must consume the deferred entry")
}

// TestCanonicalWitnessHashStorageGate pins the chain-storage gate: no stored
// witness means no commitment (and thus nothing to sign), while stored bytes
// hash to the canonical commitment directly.
func TestCanonicalWitnessHashStorageGate(t *testing.T) {
	h := newTestHandler()
	defer h.close()

	hash := common.HexToHash("0x4242")
	_, ok := h.handler.canonicalWitnessHash(hash)
	require.False(t, ok, "absent witness must yield no commitment")

	body := []byte{0x01, 0x02, 0x03}
	rawdb.WriteWitness(h.chain.DB(), hash, body)
	got, ok := h.handler.canonicalWitnessHash(hash)
	require.True(t, ok)
	require.Equal(t, stateless.WitnessCommitHash(body), got)
}
