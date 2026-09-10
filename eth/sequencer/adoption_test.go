package sequencer

import (
	"math/big"
	"testing"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// foreignWindow appends a dangling open window (open + txs records) to the
// store, as an incumbent producer's unsealed work.
func foreignWindow(t *testing.T, h *harness, number uint64, parent common.Hash, txs ...*types.Transaction) (uint64, uint64) {
	t.Helper()

	ts := 1700000000 + number
	gasLimit := uint64(30_000_000)

	entry := &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
		BlockNumber:      number,
		BlockTimestamp:   ts,
		ParentHash:       parent.Bytes(),
		GasLimit:         gasLimit,
		BaseFee:          big25gwei(),
		PrefixCommitment: h.store.Head().Bytes(),
	}}}

	if status := h.store.Append(entry); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("foreign open rejected: %v", status)
	}

	for _, tx := range txs {
		raw, err := tx.MarshalBinary()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		rec := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
			Transactions:     [][]byte{raw},
			PrefixCommitment: h.store.Head().Bytes(),
		}}}

		if status := h.store.Append(rec); status != pb.AckStatus_ACK_STATUS_OK {
			t.Fatalf("foreign record rejected: %v", status)
		}
	}

	return ts, gasLimit
}

func fee25() *big.Int {
	return new(big.Int).SetBytes(big25gwei())
}

func windowHeader(number uint64, parent common.Hash, ts uint64, gasLimit uint64) *types.Header {
	return &types.Header{
		ParentHash: parent,
		Number:     new(big.Int).SetUint64(number),
		GasLimit:   gasLimit,
		Time:       ts,
		BaseFee:    fee25(),
		Difficulty: big.NewInt(1),
	}
}

// The flagship adopt path: the build-start check finds the incumbent's
// window and adopts it — engage swallows the open, matched transactions
// publish nothing, continuations stay buffered — and the seal flush
// completes the window in place. No supersession anywhere.
func TestFollowCompletesWindowAtSeal(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx1, tx2 := testTx(t, 0), testTx(t, 1)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx1, tx2)

	adopts := reconcileAdopt.Snapshot().Count()
	supersedes := reconcileSupersede.Snapshot().Count()
	headBefore := h.store.Head()

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 2 || w.Timestamp != ts || w.GasLimit != gasLimit {
		t.Fatalf("AdoptWindow = %+v", w)
	}

	if got := reconcileAdopt.Snapshot().Count(); got != adopts+1 {
		t.Fatalf("adopt counter = %d, want %d", got, adopts+1)
	}

	// The worker builds under the adopted context, re-committing the
	// window's transactions: those are matched and swallowed, so the
	// store does not move for them.
	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0])
	p.PublishTx(w.Txs[1])

	time.Sleep(100 * time.Millisecond)

	if h.store.Head() != headBefore {
		t.Fatal("re-committed window transactions must publish nothing")
	}

	// A continuation extends the adopted window immediately — its ack is
	// what proves this node owns the height and may seal it.
	tx3 := testTx(t, 2)
	p.PublishTx(tx3)
	waitHead(t, h, p, 5*time.Second)

	// The seal completes the window in place.
	header := windowHeader(2, parent, ts, gasLimit)
	p.SealBlock(blockFor(header, []*types.Transaction{w.Txs[0], w.Txs[1], tx3}))
	waitHead(t, h, p, 5*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("adopted block missing: %v", err)
	}

	if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
		t.Fatalf("adopt completion superseded: counter %d -> %d", supersedes, got)
	}
}

// A worker that drops a window transaction (divergence) publishes nothing
// at the moment of divergence; the seal flush re-anchors — the only
// supersession — and the store converges on the sealed content.
func TestFollowDivergenceResolvesAtSeal(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx1, tx2 := testTx(t, 0), testTx(t, 1)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx1, tx2)

	headBefore := h.store.Head()

	w := p.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("no window to adopt")
	}

	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0])

	other := testTx(t, 7) // tx2 failed to apply; the worker committed another
	p.PublishTx(other)

	time.Sleep(100 * time.Millisecond)

	if h.store.Head() != headBefore {
		t.Fatal("divergence published before sealing")
	}

	// The barrier must hold the block to the executable prefix, not the
	// full incumbent window: refusing here re-adopts the same window and
	// re-diverges forever.
	blockTxs := []*types.Transaction{w.Txs[0], other}
	if !p.AwaitSequenced(50*time.Millisecond, 2, blockTxs) {
		t.Fatal("barrier refused a partial adoption")
	}

	header := windowHeader(2, parent, ts, gasLimit)
	p.SealBlock(blockFor(header, blockTxs))
	waitHead(t, h, p, 10*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("sealed block missing after divergence flush: %v", err)
	}
}

// A window whose trailing transaction fails to apply never fires the
// mismatch (nothing commits after the prefix): the adoption stays engaged
// short of a full match, and the barrier reads the executable prefix from
// it live. The seal flush supersedes the dropped tail.
func TestFollowTrailingDropSealsThrough(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx1, tx2 := testTx(t, 0), testTx(t, 1)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx1, tx2)

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 2 {
		t.Fatalf("offer: %+v", w)
	}

	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0]) // tx2 failed to apply; nothing committed after it

	blockTxs := []*types.Transaction{w.Txs[0]}
	if !p.AwaitSequenced(50*time.Millisecond, 2, blockTxs) {
		t.Fatal("barrier refused a trailing-drop adoption")
	}

	header := windowHeader(2, parent, ts, gasLimit)
	p.SealBlock(blockFor(header, blockTxs))
	waitHead(t, h, p, 10*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("sealed block missing after trailing-drop flush: %v", err)
	}
}

// A build that raced the window under its own context (the check returned
// the window but the worker opened differently) buffers silently and the
// seal flush re-anchors to the sealed truth.
func TestFollowMismatchedOpenResolvesAtSeal(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	foreignWindow(t, h, 2, parent, testTx(t, 0))

	if w := p.AdoptWindow(2, parent); w == nil {
		t.Fatal("no window offered")
	}

	supersedes := reconcileSupersede.Snapshot().Count()

	// Own context: the adopt disengages, un-adopts back to the window's
	// base, and the divergent local build supersedes the incumbent window.
	ownTs := uint64(1600000099)
	p.OpenBlock(2, ownTs, parent, 30_000_000, fee25())
	tx := testTx(t, 3)
	p.PublishTx(tx)

	header := windowHeader(2, parent, ownTs, 30_000_000)
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))
	waitHead(t, h, p, 10*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("sealed block missing after flush: %v", err)
	}

	// The supersede of the incumbent's window is counted, not silent
	// (finding: divergent takeover on the clean-append path escaped the
	// metric).
	if got := reconcileSupersede.Snapshot().Count(); got != supersedes+1 {
		t.Fatalf("divergent takeover supersede uncounted: %d -> %d", supersedes, got)
	}
}

// A window at a height the build is not at (chain moved) is not adopted:
// the build buffers and the flush repairs.
func TestCheckTailForeignHeightHolds(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	foreignWindow(t, h, 2, parent, testTx(t, 0))

	if w := p.AdoptWindow(3, common.Hash{0x99}); w != nil {
		t.Fatal("mismatched build must not receive the window")
	}

	p.mu.Lock()
	ceiling := p.hold.after
	next := p.journal.nextSeq
	p.mu.Unlock()

	if ceiling == noHold || ceiling != next-1 {
		t.Fatalf("foreign window must gate new entries (ceiling=%d next=%d)", ceiling, next)
	}
}

// A live window at our height on a different parent (reorg territory) must
// not wedge production: the build mutes, the barrier lets it seal even
// under a sticky hold the foreign window armed, and the seal flush
// supersedes the dead-parent window with sealed truth.
func TestForeignParentWindowMutesAndSealsThrough(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// A window at our height whose parent lost the reorg.
	foreignWindow(t, h, 2, common.Hash{0xaa}, testTx(t, 0))

	parent := sealHash(t, sealed)
	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatalf("a dead-parent window must not be adopted: %+v", w)
	}

	// The held watchdog can re-arm a sticky hold while the muted build
	// runs; the barrier must seal through it regardless.
	p.mu.Lock()
	p.hold = hold{after: p.ackedSeq, kind: holdSticky}
	muted := p.mode.mutedAt(2)
	p.mu.Unlock()

	if !muted {
		t.Fatal("a dead-parent window did not mute the build")
	}

	tx := testTx(t, 9)
	if !p.AwaitSequenced(50*time.Millisecond, 2, []*types.Transaction{tx}) {
		t.Fatal("barrier refused a muted build")
	}

	header := windowHeader(2, parent, sealed.Time+2, 30_000_000)
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))
	waitHead(t, h, p, 10*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("seal never superseded the dead-parent window: %v", err)
	}
}

// A record-less window (open only) is adopted with an empty seed; the
// first pool transaction buffers as a continuation and the flush completes
// the window.
func TestFollowEmptyWindow(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	ts, gasLimit := foreignWindow(t, h, 2, parent) // zero records

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 0 {
		t.Fatalf("AdoptWindow = %+v, want empty window", w)
	}

	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	tx := testTx(t, 0)
	p.PublishTx(tx)

	header := windowHeader(2, parent, ts, gasLimit)
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))
	waitHead(t, h, p, 5*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("adopted block missing: %v", err)
	}
}

// A clean tail (no window) publishes normally — the incumbent path.
func TestCheckTailCleanPublishes(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)

	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatalf("clean tail returned a window: %+v", w)
	}

	publishBlock(t, p, 2, parent, 1)
	waitHead(t, h, p, 5*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("incumbent block missing: %v", err)
	}
}

// A seal arriving while an adopted window stands but was never engaged
// (the build sealed under its own steam) is never dropped: the flush
// rebuilds the window from the block and the store converges.
func TestSealNeverDropped(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	foreignWindow(t, h, 2, parent, testTx(t, 0))

	if w := p.AdoptWindow(2, parent); w == nil {
		t.Fatal("no window offered")
	}

	// No OpenBlock at all — the straggler seal arrives first.
	header := testHeader(2, parent)
	tx := testTx(t, 5)
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))
	waitHead(t, h, p, 10*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("straggler seal never landed: %v", err)
	}
}

// A build-start check must never strangle the previous block's draining
// seal flush: the send ceiling gates only the new build's entries, so a
// flush buffered during an outage still delivers once the store returns —
// even though the next build's check ran (and held) in between.
func TestBuildStartHoldDoesNotBlockFlush(t *testing.T) {
	restore := checkTailTimeout
	checkTailTimeout = 100 * time.Millisecond

	t.Cleanup(func() { checkTailTimeout = restore })

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// Store goes down; block 2 is built and sealed entirely offline — the
	// flush entries sit in the journal, undeliverable.
	h.stop()

	parent := sealHash(t, sealed)
	sealed2 := publishBlock(t, p, 2, parent, 2)

	// The next build's check runs while the flush is still pending (store
	// unreachable): it must gate only future entries.
	if w := p.AdoptWindow(3, sealed2.Hash()); w != nil {
		t.Fatalf("unreachable store returned a window: %+v", w)
	}

	// Store returns: the pending flush must drain without any further
	// publisher calls.
	h.resume()
	waitHead(t, h, p, 15*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("flush strangled by build-start hold: %v", err)
	}
}

// An offer whose build dies before opening (rotation churn) is re-served
// at the next work cycle: the absorbed window must be resumed, never
// mistaken for a clean tail and replaced by a fresh generation.
func TestUnconsumedOfferReoffered(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx := testTx(t, 0)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx)

	// First work cycle takes the offer and dies before OpenBlock.
	if w := p.AdoptWindow(2, parent); w == nil {
		t.Fatal("no window offered")
	}

	supersedes := reconcileSupersede.Snapshot().Count()

	// The replacement build for the same height must get the offer again.
	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 1 || w.Timestamp != ts {
		t.Fatalf("unconsumed offer not re-served: %+v", w)
	}

	// This time the build engages and completes the window in place.
	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0])
	p.SealBlock(blockFor(windowHeader(2, parent, ts, gasLimit), []*types.Transaction{w.Txs[0]}))
	waitHead(t, h, p, 10*time.Second)

	if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
		t.Fatalf("resumed window superseded: %d -> %d", supersedes, got)
	}
}

// An armed offer whose window kept growing after the snapshot (the
// incumbent streamed more records before dying) is re-read from its base
// and re-adopted in full — sealing the stale snapshot would supersede
// the extra records at the flush.
func TestGrownWindowReadoptedNotStale(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx1 := testTx(t, 0)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx1)

	// Snapshot taken at one record; the build dies before opening.
	if w := p.AdoptWindow(2, parent); w == nil || len(w.Txs) != 1 {
		t.Fatalf("first offer wrong: %+v", w)
	}

	// The incumbent streams one more record before dying.
	tx2 := testTx(t, 1)
	raw, err := tx2.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{raw},
		PrefixCommitment: h.store.Head().Bytes(),
	}}}
	if status := h.store.Append(rec); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("grown record rejected: %v", status)
	}

	supersedes := reconcileSupersede.Snapshot().Count()

	// The replacement build must receive the FULL grown window.
	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 2 {
		t.Fatalf("grown window not re-adopted: %+v", w)
	}

	// Engage and complete in place: both records already in the store.
	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0])
	p.PublishTx(w.Txs[1])
	p.SealBlock(blockFor(windowHeader(2, parent, ts, gasLimit), []*types.Transaction{w.Txs[0], w.Txs[1]}))
	waitHead(t, h, p, 10*time.Second)

	if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
		t.Fatalf("grown-window resume superseded: %d -> %d", supersedes, got)
	}
}

// A producer restarted mid-window resumes it (self-adoption): with no
// persisted position, the fresh publisher relocates the store tail from
// the chain's last imported block, the startup reconcile anchors at the
// window's base, the first build-start check collects and adopts it, and
// the seal completes the same generation — its own preconfs never revoked.
func TestRestartResumesOwnWindow(t *testing.T) {
	h := startHarness(t)

	first, err := NewPublisher(h.addr, h.addr, testChainID, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	sealed := publishBlock(t, first, 1, common.Hash{0xef}, 1)
	waitHead(t, h, first, 5*time.Second)

	// Mid-window death: open 2 and stream one record, then die unsealed.
	parent := sealHash(t, sealed)
	tx := testTx(t, 0)
	first.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	first.PublishTx(tx)
	waitHead(t, h, first, 5*time.Second)
	first.Close()

	// A fresh publisher with block 1 as its chain head: the restart probe
	// locates the store tail from that height — no local state carried over.
	chain := &fakeChain{current: &types.Header{Number: big.NewInt(1)}}

	p, err := NewPublisher(h.addr, h.addr, testChainID, 0, chain, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	t.Cleanup(p.Close)
	waitFor(t, 5*time.Second, func() bool { return p.isAnchored() })

	supersedes := reconcileSupersede.Snapshot().Count()

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 1 || w.Txs[0].Hash() != tx.Hash() {
		t.Fatalf("restart did not resume own window: %+v", w)
	}

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(w.Txs[0])
	p.SealBlock(blockFor(windowHeader(2, parent, 1700000002, 30_000_000), []*types.Transaction{w.Txs[0]}))
	waitHead(t, h, p, 10*time.Second)

	if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
		t.Fatalf("self-resume superseded own window: %d -> %d", supersedes, got)
	}
}

// A build-start hold lifts the moment the flush it ordered behind is
// drained: the gated window then streams mid-block instead of batching
// until its own seal — under continuous load a seal-persistent hold
// would ratchet (each flush still draining at the next build's check)
// and turn preconf streaming into at-seal batching.
func TestBuildStartHoldReleasesOnDrain(t *testing.T) {
	restore := checkTailTimeout
	checkTailTimeout = 100 * time.Millisecond

	t.Cleanup(func() { checkTailTimeout = restore })

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// Block 2 seals while the store is down: its flush sits in the journal.
	h.stop()

	parent := sealHash(t, sealed)
	sealed2 := publishBlock(t, p, 2, parent, 1)

	// Build 3 starts against the unreachable store: its window is gated
	// behind the pending flush.
	if w := p.AdoptWindow(3, sealed2.Hash()); w != nil {
		t.Fatalf("unreachable store returned a window: %+v", w)
	}

	p.OpenBlock(3, sealed2.Time+2, sealed2.Hash(), sealed2.GasLimit, fee25())
	p.PublishTx(testTx(t, 3))

	// The store returns: the flush drains, the hold lifts, and block 3's
	// window streams to the store with no SealBlock in sight.
	h.resume()
	waitHead(t, h, p, 15*time.Second)

	p.mu.Lock()
	tail := p.journal.items[len(p.journal.items)-1]
	held := p.hold.after != noHold
	p.mu.Unlock()

	if tail.kind != entryRecord || tail.height != 3 || held {
		t.Fatalf("mid-block window not streaming: tail kind=%d height=%d held=%v", tail.kind, tail.height, held)
	}
}

// A build racing its own chain-head update (its height already sealed and
// flushed) is muted: no phantom open dangles on the sealed tip, and the
// next legitimate build publishes normally.
func TestStaleBuildPublishesNothing(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	headBefore := h.store.Head()
	muted := publishMutedCount.Snapshot().Count()

	// The worker re-prepares height 1 before the chain head catches up.
	if w := p.AdoptWindow(1, common.Hash{0xef}); w != nil {
		t.Fatalf("stale build received a window: %+v", w)
	}

	if got := publishMutedCount.Snapshot().Count(); got != muted+1 {
		t.Fatalf("muted counter = %d, want %d", got, muted+1)
	}

	p.OpenBlock(1, sealed.Time, common.Hash{0xef}, sealed.GasLimit, fee25())
	p.PublishTx(testTx(t, 7))

	p.mu.Lock()
	pending := p.journal.nextSeq - 1 - p.ackedSeq
	p.mu.Unlock()

	if pending != 0 {
		t.Fatalf("muted build appended %d entries", pending)
	}

	if h.store.Head() != headBefore {
		t.Fatal("muted build reached the store")
	}

	// The interrupted build gives way to the real next height.
	parent := sealHash(t, sealed)
	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatalf("clean tail returned a window: %+v", w)
	}

	publishBlock(t, p, 2, parent, 1)
	waitHead(t, h, p, 5*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("post-mute block missing: %v", err)
	}
}

// A builder whose published window lost to a foreign seal discards the
// dead buffer at the next work cycle instead of re-STALEing it
// forever, and its next open folds onto the foreign store head.
func TestBuildStartDiscardsSupersededBuffer(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// Our window for 2 folds locally but the store never sees it (the
	// foreign producer owns the height); its content stays unacked.
	parent := sealHash(t, sealed)
	h.stop()
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))

	// The foreign producer's block 2 lands in the store, sealed.
	h.resume()
	ts, gasLimit := foreignWindow(t, h, 2, parent, testTx(t, 1))
	appendForeignSeal(t, h, windowHeader(2, parent, ts, gasLimit))

	p.mu.Lock()
	buffered := p.journal.nextSeq - 1 - p.ackedSeq
	p.mu.Unlock()

	if buffered == 0 {
		t.Fatal("test setup: nothing buffered")
	}

	// Next work cycle: the read shows the foreign sealed tip covering our
	// whole buffer — it is discarded and the lineage re-anchors.
	if w := p.AdoptWindow(3, common.Hash{0x33}); w != nil {
		t.Fatalf("AdoptWindow = %+v", w)
	}

	p.mu.Lock()
	left := p.journal.nextSeq - 1 - p.ackedSeq
	rebased := p.head == h.store.Head() && p.anchor == p.head
	p.mu.Unlock()

	if left != 0 || !rebased {
		t.Fatalf("buffer not discarded: unacked=%d rebased=%v", left, rebased)
	}

	// The new build publishes cleanly onto the foreign head.
	publishBlock(t, p, 3, common.Hash{0x33}, 1)
	waitHead(t, h, p, 5*time.Second)
}

// A muted build that seals after all (a reorg win) loses nothing: the
// flush rebuilds the complete window from the block — the designed
// supersede-by-seal.
func TestStaleBuildSealFlushRebuilds(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	if w := p.AdoptWindow(1, common.Hash{0xef}); w != nil {
		t.Fatalf("stale build received a window: %+v", w)
	}

	tx := testTx(t, 9)
	p.OpenBlock(1, sealed.Time+2, common.Hash{0xef}, sealed.GasLimit, fee25())
	p.PublishTx(tx)

	header := testHeader(1, common.Hash{0xef})
	header.Time = sealed.Time + 2
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))
	waitHead(t, h, p, 10*time.Second)

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 1}); err != nil {
		t.Fatalf("reorg seal never landed: %v", err)
	}
}

// A adopt snapshot raced by the dying producer's draining stream: the
// store window gains records after our absorption, so the flush STALEs —
// but the store's copy is a strict prefix of the sealed content, and the
// flush completes it in place. Same generation, nothing revoked.
func TestFlushCompletesExtendedWindowInPlace(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx1, tx2 := testTx(t, 0), testTx(t, 1)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx1)

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 1 {
		t.Fatalf("offer: %+v", w)
	}

	// The dying producer's last in-flight record lands after our snapshot.
	raw, err := tx2.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{raw},
		PrefixCommitment: h.store.Head().Bytes(),
	}}}
	if st := h.store.Append(rec); st != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("extension record rejected: %v", st)
	}

	supersedes := reconcileSupersede.Snapshot().Count()

	// The build seeds the snapshot, then commits the extension tx (it is
	// in our pool too) and a continuation, and seals all three.
	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0])
	p.PublishTx(tx2)

	tx3 := testTx(t, 2)
	p.PublishTx(tx3)
	p.SealBlock(blockFor(windowHeader(2, parent, ts, gasLimit), []*types.Transaction{w.Txs[0], tx2, tx3}))
	waitHead(t, h, p, 10*time.Second)

	if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
		t.Fatalf("extended-window completion superseded: %d -> %d", supersedes, got)
	}

	if _, err := h.store.GetBlock(t.Context(), &pb.GetBlockRequest{BlockNumber: 2}); err != nil {
		t.Fatalf("completed block missing: %v", err)
	}
}

// A adopt inside a live send loop session must not re-send the absorbed
// window: those entries are already in the store, and re-sending them
// guarantees a STALE plus a spurious gap-fill on every rotation adopt.
func TestFollowDoesNotResendAbsorbed(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx1 := testTx(t, 0)
	ts, gasLimit := foreignWindow(t, h, 2, parent, tx1)

	stales := publishStaleCount.Snapshot().Count()
	gapfills := reconcileGapfill.Snapshot().Count()

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 1 {
		t.Fatalf("offer: %+v", w)
	}

	p.OpenBlock(2, ts, parent, gasLimit, fee25())
	p.PublishTx(w.Txs[0])

	tx2 := testTx(t, 1)
	p.PublishTx(tx2)
	p.SealBlock(blockFor(windowHeader(2, parent, ts, gasLimit), []*types.Transaction{w.Txs[0], tx2}))
	waitHead(t, h, p, 10*time.Second)

	if got := publishStaleCount.Snapshot().Count(); got != stales {
		t.Fatalf("adopt re-sent absorbed entries: stale %d -> %d", stales, got)
	}

	if got := reconcileGapfill.Snapshot().Count(); got != gapfills {
		t.Fatalf("adopt caused spurious gapfill: %d -> %d", gapfills, got)
	}
}

// A parked adopter (its mid-block build died; no seal ever comes) must
// not starve the idle catch-up: the held dead buffer is refolded away and
// the anchor tracks the store tip.
func TestParkedAdopterIdleRecovers(t *testing.T) {
	restore := idleReconcileInterval
	idleReconcileInterval = 80 * time.Millisecond

	t.Cleanup(func() { idleReconcileInterval = restore })

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// Foreign content wins the height mid-build: our window STALEs into a
	// stale-hold with no seal ever coming (the parked-adopter shape).
	parent := sealHash(t, sealed)
	ts, gasLimit := foreignWindow(t, h, 2, parent, testTx(t, 0))
	appendForeignSeal(t, h, windowHeader(2, parent, ts, gasLimit))

	p.OpenBlock(2, ts+9, parent, gasLimit, fee25())
	p.PublishTx(testTx(t, 5))

	base := h.store.Head()

	waitFor(t, 10*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.anchor == base && p.unackedLocked() == 0
	})
}

// A seal result racing the mute-clear between a build's check and its
// open must not publish an open at or behind the sealed tip.
func TestOpenBlockRefusesSealedHeight(t *testing.T) {
	p := barePublisher()

	header := testHeader(1, common.Hash{0xef})
	p.OpenBlock(1, header.Time, header.ParentHash, header.GasLimit, header.BaseFee)
	p.SealBlock(blockFor(header, nil))

	before := len(p.journal.items)
	p.OpenBlock(1, header.Time+4, header.ParentHash, header.GasLimit, header.BaseFee)

	if len(p.journal.items) != before {
		t.Fatal("open published at a sealed height")
	}
}

// The 30 s idle ticker landing on a healthy producer's drained-open window
// (all records acked, seal pending — unacked==0 with a hold of holdNone)
// must NOT zero curHeight: doing so tagged later records height=0 and
// misclassified the live window as between-blocks. Only a held/parked
// window is cleaned.
func TestIdleTickPreservesLiveWindow(t *testing.T) {
	restore := idleReconcileInterval
	idleReconcileInterval = 40 * time.Millisecond

	t.Cleanup(func() { idleReconcileInterval = restore })

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// Open the next window and stream a record; wait until it drains
	// (open+record acked) — a healthy incumbent between txs.
	parent := sealHash(t, sealed)
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	tx := testTx(t, 0)
	p.PublishTx(tx)
	waitHead(t, h, p, 5*time.Second)

	p.mu.Lock()
	drained := p.unackedLocked() == 0 && p.journal.openStart() >= 0 && p.hold.kind == holdNone
	p.mu.Unlock()

	if !drained {
		t.Fatal("test setup: window not in the drained-open state")
	}

	// Let several idle ticks fire across the drained-open window.
	time.Sleep(200 * time.Millisecond)

	p.mu.Lock()
	curHeight := p.curHeight
	var zeroHeightRecords int
	for _, it := range p.journal.items {
		if it.kind == entryRecord && it.height == 0 {
			zeroHeightRecords++
		}
	}
	p.mu.Unlock()

	if curHeight != 2 {
		t.Fatalf("idle tick zeroed a live window: curHeight=%d, want 2", curHeight)
	}

	// A continuation after the ticks must still tag the live height.
	p.PublishTx(testTx(t, 1))

	p.mu.Lock()
	for _, it := range p.journal.items {
		if it.kind == entryRecord && it.height == 0 {
			zeroHeightRecords++
		}
	}
	p.mu.Unlock()

	if zeroHeightRecords != 0 {
		t.Fatalf("records tagged height=0 after idle tick: %d", zeroHeightRecords)
	}

	// The window still seals in place — no supersession.
	supersedes := reconcileSupersede.Snapshot().Count()
	p.SealBlock(blockFor(windowHeader(2, parent, 1700000002, 30_000_000), []*types.Transaction{tx, testTx(t, 1)}))
	waitHead(t, h, p, 10*time.Second)

	if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
		t.Fatalf("live window superseded after idle tick: %d -> %d", supersedes, got)
	}
}

// The devnet takeover-stall shape: another producer's lineage sealed cleanly
// through our parent while this publisher idled muted, so its anchor is
// still the seed. The store owes nothing at the new height (decoded seal at
// exactly the parent, no live window), so the barrier must pass within the
// slot — refusing wedged a fresh producer for 17 seconds (the reconcile held
// every rebuild's open awaiting the very seal flush the barrier was
// refusing) until heimdall rotated production away as ineffective.
func TestTakeoverSealsOverCleanForeignHistory(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	f1 := testHeader(1, common.Hash{0xef})
	appendForeignOpen(t, h, 1, common.Hash{0xef})
	appendForeignSeal(t, h, f1)

	f2 := testHeader(2, f1.Hash())
	appendForeignOpen(t, h, 2, f1.Hash())
	appendForeignSeal(t, h, f2)

	// Attempt one mirrors the race the devnet producer lost: it opened
	// before any build-start read could rebase its anchor, so the journal
	// now holds unacked entries that block every later rebase. Whatever
	// this attempt's barrier says, the state it leaves is the wedge.
	header := testHeader(3, f2.Hash())
	tx := testTx(t, 0)
	p.OpenBlock(3, header.Time, f2.Hash(), header.GasLimit, header.BaseFee)
	p.PublishTx(tx)
	p.AwaitSequenced(300*time.Millisecond, 3, []*types.Transaction{tx})

	// Attempt two is the miner's normal rebuild cycle. The store owes
	// nothing at 3 (decoded seal exactly at the parent, no live window),
	// so the barrier must pass instead of wedging takeover production
	// behind an anchor rebase the held opens forbid.
	if w := p.AdoptWindow(3, f2.Hash()); w != nil {
		t.Fatalf("nothing to adopt over sealed history, got window at %d", w.Number)
	}

	p.OpenBlock(3, header.Time, f2.Hash(), header.GasLimit, header.BaseFee)
	p.PublishTx(tx)

	if !p.AwaitSequenced(300*time.Millisecond, 3, []*types.Transaction{tx}) {
		t.Fatal("barrier refused a takeover seal over clean foreign history")
	}

	// The seal flush's STALE rebases the stale anchor and redelivers: the
	// store converges to our generation at 3 without operator intervention.
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))

	deadline := time.Now().Add(5 * time.Second)

	for {
		sealed := false

		for _, g := range readAllGenerations(t, h) {
			if g.height == 3 && g.sealed {
				sealed = true
			}
		}

		if sealed {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("takeover flush never converged: no sealed generation at 3")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// The takeover escape must stay exact: a live window standing at the new
// height is promised content, and a block that does not carry it keeps its
// refusal — the empty-window proof is what lets the clean takeover through,
// never a stale anchor alone.
func TestBarrierStillRefusesPromisedWindowAtTakeover(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	f1 := testHeader(1, common.Hash{0xef})
	appendForeignOpen(t, h, 1, common.Hash{0xef})
	appendForeignSeal(t, h, f1)

	f2 := testHeader(2, f1.Hash())
	appendForeignOpen(t, h, 2, f1.Hash())
	appendForeignSeal(t, h, f2)

	// A dying producer opened 3 and promised one transaction.
	appendForeignOpen(t, h, 3, f2.Hash())

	promised := testTx(t, 7)
	raw, err := promised.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{raw},
		PrefixCommitment: h.store.Head().Bytes(),
	}}}
	if status := h.store.Append(rec); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("promised record rejected: %v", status)
	}

	// A takeover that never adopts the promised window must not seal —
	// the barrier's mirror refuses the block, and the clean-takeover
	// escape must not fire while a live window stands at the height.
	header := testHeader(3, f2.Hash())
	p.OpenBlock(3, header.Time, f2.Hash(), header.GasLimit, header.BaseFee)
	ours := testTx(t, 0)
	p.PublishTx(ours)

	if p.AwaitSequenced(300*time.Millisecond, 3, []*types.Transaction{ours}) {
		t.Fatal("barrier passed a block that drops the promised window")
	}
}

// A store that never shows a consistent, adoptable shape must cost bounded
// slots, not the height. The trigger class is a split-view store — a load
// balancer over replicas at different lags, or a Byzantine store — where
// every read refuses and none converges; the gate has a refusal cap for
// exactly this, and the barrier needs its own. Past the cap the barrier
// seals for liveness and leaves the verdict to the gate and consensus.
func TestBarrierRefusalCapSealsForLiveness(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	f1 := testHeader(1, common.Hash{0xef})
	appendForeignOpen(t, h, 1, common.Hash{0xef})
	appendForeignSeal(t, h, f1)

	// A promised window at our height that this build never adopts: the
	// mirror refuses every attempt, and no rebuild in this test converges.
	appendForeignOpen(t, h, 2, f1.Hash())

	promised := testTx(t, 7)
	raw, err := promised.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{raw},
		PrefixCommitment: h.store.Head().Bytes(),
	}}}
	if status := h.store.Append(rec); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("promised record rejected: %v", status)
	}

	header := testHeader(2, f1.Hash())
	ours := testTx(t, 0)
	p.OpenBlock(2, header.Time, f1.Hash(), header.GasLimit, header.BaseFee)
	p.PublishTx(ours)

	refusals := 0

	for range maxBarrierRefusals + 2 {
		if p.AwaitSequenced(50*time.Millisecond, 2, []*types.Transaction{ours}) {
			break
		}

		refusals++
	}

	if refusals != maxBarrierRefusals {
		t.Fatalf("barrier allowed the seal after %d refusals, want exactly %d "+
			"before liveness wins", refusals, maxBarrierRefusals)
	}
}
