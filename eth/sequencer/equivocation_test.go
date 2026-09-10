package sequencer

import (
	"testing"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/miner"
)

// Two producers sharing a signing key both seal the same height. When the
// chain picks the other block, our flush must not overwrite the winner's
// content: the store's newest generation at a height has to agree with the
// canonical chain.
func TestForeignSealAtOwnHeightNonCanonical(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}, known: map[common.Hash]*types.Header{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// The twin publishes and seals ITS block 2 into the store first.
	twin := testHeader(2, parent)
	twin.Extra = []byte("twin") // distinct hash
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, twin)

	// The chain accepts the TWIN's block 2 as canonical; ours lost.
	chain.canonical[2] = twin.Hash()

	// Now we seal our own (losing) block 2 and flush.
	ours := testHeader(2, parent)
	p.OpenBlock(2, ours.Time, parent, ours.GasLimit, ours.BaseFee)
	tx := testTx(t, 0)
	p.PublishTx(tx)

	supersedes := reconcileSupersede.Snapshot().Count()

	p.SealBlock(blockFor(ours, []*types.Transaction{tx}))
	time.Sleep(2 * time.Second) // let the flush + reconcile settle

	if got := reconcileSupersede.Snapshot().Count(); got > supersedes {
		t.Fatalf("superseded a CANONICAL foreign seal at our own height "+
			"(supersede %d -> %d): the store's newest generation at height 2 "+
			"now holds non-canonical content", supersedes, got)
	}
}

// The mirror case: the chain chose OUR block, so the foreign seal at our
// height is the loser — supersede it, as before.
func TestForeignSealAtOwnHeightWeAreCanonical(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}, known: map[common.Hash]*types.Header{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	twin := testHeader(2, parent)
	twin.Extra = []byte("twin")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, twin)

	ours := testHeader(2, parent)
	chain.canonical[2] = ours.Hash() // WE won

	p.OpenBlock(2, ours.Time, parent, ours.GasLimit, ours.BaseFee)
	tx := testTx(t, 0)
	p.PublishTx(tx)

	supersedes := reconcileSupersede.Snapshot().Count()
	yields := reconcileYield.Snapshot().Count()

	p.SealBlock(blockFor(ours, []*types.Transaction{tx}))

	// The gate runs on every seal in production, and our block being
	// canonical confirms it — which releases the flush to supersede the
	// loser's content. Until then the pre-broadcast hold keeps our window
	// off a height the store has already closed.
	if v := p.ConfirmSeal(2 * time.Second); v != miner.SealConfirmed {
		t.Fatalf("gate verdict = %v for our own canonical block", v)
	}

	waitHead(t, h, p, 10*time.Second)

	if reconcileYield.Snapshot().Count() != yields {
		t.Fatal("yielded despite being the canonical producer")
	}

	if reconcileSupersede.Snapshot().Count() != supersedes+1 {
		t.Fatal("canonical producer must supersede the losing seal")
	}
}

// The pre-seal barrier: a confirmed window clears, a contested one does
// not, and an unreachable store clears anyway so production never stalls.
func TestAwaitSequencedBarrier(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitHead(t, h, p, 5*time.Second)

	if !awaitOurWindow(p, 2*time.Second) {
		t.Fatal("a fully confirmed window must clear the barrier")
	}

	// A competitor takes the height with DIFFERENT content: its window is
	// not a prefix of ours, so there is nothing to complete in place and
	// our next write STALEs us into the contested hold.
	foreignWindow(t, h, 2, parent, testTx(t, 9))
	p.PublishTx(testTx(t, 1))

	waitFor(t, 5*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.hold.kind == holdSticky
	})

	if awaitOurWindow(p, 2*time.Second) {
		t.Fatal("a contested window must NOT clear the barrier")
	}
}

// An unreachable store must not gate block production: the barrier clears
// on its deadline rather than stalling the chain.
func TestAwaitSequencedYieldsToLiveness(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	h.stop() // store gone; entries pile up unacked

	p.OpenBlock(2, 1700000002, common.Hash{0x01}, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))

	start := time.Now()
	if !awaitOurWindow(p, 300*time.Millisecond) {
		t.Fatal("an unreachable store must not block sealing")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("barrier waited %v; it must bound the stall", elapsed)
	}
}

// Takeover: the incumbent died, we adopt its window. We are the ONLY
// producer, so the barrier must let us seal.
func TestBarrierAllowsAdoptedTakeover(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// A dead producer's dangling window at height 2.
	foreignWindow(t, h, 2, parent, testTx(t, 0))

	w := p.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("window not adopted")
	}

	if !awaitOurWindow(p, 2*time.Second) {
		t.Fatal("barrier blocked a takeover seal: the taker is the only " +
			"producer, so refusing to seal would stall the chain")
	}
}

// A re-anchor must not re-post data the store already holds. With the
// store carrying a strict prefix of our window, completion absorbs that
// prefix as confirmed and leaves only the missing records to deliver —
// republishing the whole window would duplicate entries every reader has.
func TestReanchorPublishesOnlyTheDelta(t *testing.T) {
	p := barePublisher()

	parent := common.Hash{0x01}
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	p.PublishTx(testTx(t, 1))
	p.PublishTx(testTx(t, 2))

	items, _ := p.journal.after(0)
	if len(items) != 4 {
		t.Fatalf("expected open+3 records, got %d", len(items))
	}

	// The store holds a strict prefix: the open and the first record.
	info := tailInfo{
		s:             items[1].post,
		tipOpen:       true,
		tipOpenHeight: 2,
		tipOpenParent: parent,
		window:        []*pb.Entry{items[0].entry, items[1].entry},
	}

	p.mu.Lock()
	p.curHeight = 2
	completed := p.completeExtendedWindowLocked(info, 2)
	unacked := p.unackedLocked()
	total := len(p.journal.items)
	p.mu.Unlock()

	if !completed {
		t.Fatal("a store prefix of our window must complete in place")
	}

	// Only the two records the store lacks remain to send; the absorbed
	// prefix is seated as already-confirmed, not queued for re-posting.
	if unacked != 2 {
		t.Fatalf("unacked = %d, want 2 (the delta only)", unacked)
	}

	if total != 4 {
		t.Fatalf("journal holds %d entries, want 4 — the window was duplicated", total)
	}
}

// waitDrained blocks until every journal entry is store-confirmed, which is
// the state the coverage branch of the barrier runs in.
func waitDrained(t *testing.T, p *Publisher, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		p.mu.Lock()
		unacked := p.unackedLocked()
		p.mu.Unlock()

		if unacked == 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("journal still holds %d unacked entries", unacked)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// appendForeignRecord writes one record onto the store's current chain, as a
// second producer building the same height would.
func appendForeignRecord(t *testing.T, h *harness, tx *types.Transaction) {
	t.Helper()

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

// Every entry of ours acking proves the store took them — not that they are
// all the store has. A second producer's records at the same height were
// acked too, so they carry the same preconfirmation; sealing a block that
// omits them strands the promise. The barrier asks for a rebuild instead.
func TestBarrierRebuildsWhenStoreHoldsMore(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	appendForeignRecord(t, h, testTx(t, 7))

	if awaitOurWindow(p, 2*time.Second) {
		t.Fatal("sealed a block omitting records the store already holds")
	}

	if !p.ResyncNeeded() {
		t.Fatal("coverage failure did not arm a rebuild")
	}
}

// The uncontested case must stay cheap and permissive: when the store holds
// exactly our window, the coverage read confirms it and the seal proceeds.
func TestBarrierSealsWhenStoreMatches(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	if !awaitOurWindow(p, 2*time.Second) {
		t.Fatal("barrier blocked a seal whose window is exactly the store's")
	}
}

// A competing generation at our height refuses the seal and adopts. The
// old tie-break sealed here — "the later opener owns the height" — and two
// producers whose reads raced could each believe themselves later, which is
// where divergent double-seals came from. Under the adopt rule the refusal
// is unconditional and the rebuild converges on the store's window.
func TestForeignGenerationRefusedAndAdopted(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	p.PublishTx(testTx(t, 1))
	waitDrained(t, p, 5*time.Second)

	// A second producer opens its own generation at the same height.
	foreignWindow(t, h, 2, parent, testTx(t, 9))

	p.mu.Lock()
	p.resync = false
	p.mu.Unlock()

	if awaitOurWindow(p, 2*time.Second) {
		t.Fatal("sealed beside a competing generation: two blocks holding " +
			"different content at one height")
	}

	if !p.ResyncNeeded() {
		t.Fatal("the refusal must arm the rebuild that adopts the competing window")
	}
}

// A seal below the height being built is ordinary history, not evidence the
// height moved on. Bailing on it made the coverage check report "covered"
// for almost every block, because the walk from our anchor routinely crosses
// the previous block's seal.
func TestCoverageIgnoresSealBelowOurHeight(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// Block 1's seal now sits between the anchor and anything appended next.
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	appendForeignRecord(t, h, testTx(t, 7))

	if awaitOurWindow(p, 2*time.Second) {
		t.Fatal("a seal at a lower height masked a real coverage gap")
	}
}

// Takeover must stay fast: the incumbent is gone, its window is not
// growing, so the adopter covers it by definition and seals at once.
// Refusing here would stall the chain on every producer failure.
func TestAdoptedDeadWindowStillSeals(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	foreignWindow(t, h, 2, parent, testTx(t, 0))

	if p.AdoptWindow(2, parent) == nil {
		t.Fatal("window not adopted")
	}

	if !awaitOurWindow(p, 2*time.Second) {
		t.Fatal("barrier blocked a takeover of a dead window: every " +
			"producer failure would stall the chain")
	}
}

// The mirror: the incumbent is alive and its window grew past the prefix we
// adopted. Sealing now would strand every record it added.
func TestAdoptedLiveWindowDoesNotSeal(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	foreignWindow(t, h, 2, parent, testTx(t, 0))

	if p.AdoptWindow(2, parent) == nil {
		t.Fatal("window not adopted")
	}

	// The incumbent turns out to be alive and keeps writing.
	appendForeignRecord(t, h, testTx(t, 7))

	if awaitOurWindow(p, 2*time.Second) {
		t.Fatal("sealed a prefix of a window the incumbent is still growing")
	}

	if !p.ResyncNeeded() {
		t.Fatal("a grown window must arm the rebuild that covers it")
	}
}

// A read that falls off the anchor rung restarts at a block boundary and
// walks over our own open, so an open at our height is routinely ours. The
// check used to read that as a rival and pass every block it saw — a lone
// producer logged 36 "second generation" skips with no competitor running.
func TestCoverageRecognisesOurOwnWindow(t *testing.T) {
	p := barePublisher()

	parent := common.Hash{0x01}
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	p.PublishTx(testTx(t, 1))

	items, _ := p.journal.after(0)
	ourWindow := []*pb.Entry{items[0].entry, items[1].entry, items[2].entry}

	p.mu.Lock()
	defer p.mu.Unlock()

	// The whole window, as a fallback read would return it.
	whole := tailInfo{tipOpen: true, tipOpenHeight: 2, window: ourWindow}
	if got := p.relateWindowLocked(whole, 2); got != windowOurs {
		t.Fatalf("our own window read as %v, want windowOurs", got)
	}

	// A prefix of it — records still in flight.
	prefix := tailInfo{tipOpen: true, tipOpenHeight: 2, window: ourWindow[:2]}
	if got := p.relateWindowLocked(prefix, 2); got != windowOurs {
		t.Fatalf("a prefix of our window read as %v, want windowOurs", got)
	}

	// Our window with a record appended by someone else: the real gap.
	grown := tailInfo{
		tipOpen: true, tipOpenHeight: 2,
		window: append(append([]*pb.Entry{}, ourWindow...), txRecord(t, testTx(t, 9))),
	}
	if got := p.relateWindowLocked(grown, 2); got != windowExtendsOurs {
		t.Fatalf("an extended window read as %v, want windowExtendsOurs", got)
	}

	// A genuinely different lineage.
	foreign := []*pb.Entry{items[0].entry, txRecord(t, testTx(t, 9))}
	if got := p.relateWindowLocked(tailInfo{tipOpen: true, tipOpenHeight: 2, window: foreign}, 2); got != windowForeign {
		t.Fatalf("a rival window read as %v, want windowForeign", got)
	}
}

// txRecord builds a bare record entry; contentEqual compares payloads, not
// prefix commitments, so no fold is needed.
func txRecord(t *testing.T, tx *types.Transaction) *pb.Entry {
	t.Helper()

	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{Transactions: [][]byte{raw}}}}
}

// Contention refuses, always: a sticky hold is the store's own verdict that
// another producer owns the height, and the only path to a seal is adopting
// their window. The refusal arms that rebuild.
func TestContestedRefusesAndArmsAdopt(t *testing.T) {
	p := barePublisher()

	p.mu.Lock()
	p.curHeight = 7
	p.hold = hold{after: 0, kind: holdSticky}
	p.mu.Unlock()

	for i := 0; i < 3; i++ {
		if awaitOurWindow(p, time.Second) {
			t.Fatal("a contested height sealed: two producers sealing one " +
				"height with different content is the revocation machine")
		}
	}

	if !p.ResyncNeeded() {
		t.Fatal("the refusal must arm the rebuild that adopts the owner's window")
	}
}
