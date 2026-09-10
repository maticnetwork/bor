package sequencer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/miner"
)

// A failed publisher ignores the whole lifecycle: nothing folds, nothing
// queues, nothing gates.
func TestLifecycleIgnoredAfterFailure(t *testing.T) {
	p := barePublisher()
	p.failed.Store(true)

	p.OpenBlock(1, 100, common.Hash{0xef}, 30_000_000, common.Big1)
	p.PublishTx(testTx(t, 0))
	p.SealBlock(types.NewBlockWithHeader(testHeader(1, common.Hash{0xef})))

	if len(p.journal.items) != 0 {
		t.Fatal("a failed publisher must not append")
	}
}

// Unacked flushes past the hot bound collapse to the pending range instead
// of holding whole blocks in memory.
func TestColdCollapseBoundsHotSeals(t *testing.T) {
	p := barePublisher()

	parent := common.Hash{0xef}
	for n := uint64(1); n <= 4; n++ {
		parent = publishBlock(t, p, n, parent, 1).Hash()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.unackedSealsLocked() > journalHotSeals {
		t.Fatalf("collapse must bound hot seals, have %d", p.unackedSealsLocked())
	}

	if p.pendingFrom != 1 || p.pendingTo == 0 {
		t.Fatalf("collapsed heights must become backfill debt: from=%d to=%d",
			p.pendingFrom, p.pendingTo)
	}
}

// The drain rebuilds what the chain still has, jumps what it pruned, and
// clears the debt it served.
func TestBackfillDrainSkipsPrunedBlocks(t *testing.T) {
	blockAt := func(n uint64, parent common.Hash) *types.Block {
		return types.NewBlockWithHeader(testHeader(n, parent))
	}

	b1 := blockAt(1, common.Hash{0xef})
	b3 := blockAt(3, common.Hash{0x03})

	p := barePublisher()
	p.chain = &fakeChain{blocks: map[uint64]*types.Block{1: b1, 3: b3}}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pendingFrom, p.pendingTo, p.pendingEntries = 1, 3, 6

	fresh := newJournal()
	p.backfillLocked(fresh, p.head)

	if p.pendingFrom != 0 || p.pendingTo != 0 || p.pendingEntries != 0 {
		t.Fatalf("a completed drain must clear the debt: %d..%d (%d)",
			p.pendingFrom, p.pendingTo, p.pendingEntries)
	}

	// Two rebuilt blocks, open+seal each (empty bodies), height 2 jumped.
	if len(fresh.items) != 4 {
		t.Fatalf("drain rebuilt %d entries, want 4", len(fresh.items))
	}
}

// Debt no drain can serve is dropped loudly, never silently wedged.
func TestBackfillDropsUndrainableDebt(t *testing.T) {
	t.Run("inverted range", func(t *testing.T) {
		p := barePublisher()
		p.chain = &fakeChain{}

		p.mu.Lock()
		defer p.mu.Unlock()

		p.pendingFrom, p.pendingTo, p.pendingEntries = 5, 3, 9
		p.backfillLocked(newJournal(), p.head)

		if p.pendingFrom != 0 || p.pendingEntries != 0 {
			t.Fatal("an inverted range must drop the debt")
		}
	})

	t.Run("no chain to read from", func(t *testing.T) {
		p := barePublisher()

		p.mu.Lock()
		defer p.mu.Unlock()

		p.pendingFrom, p.pendingTo = 1, 2
		before := reconcileForwardJump.Snapshot().Count()
		p.backfillLocked(newJournal(), p.head)

		if p.pendingFrom != 0 {
			t.Fatal("chainless debt must drop")
		}

		if reconcileForwardJump.Snapshot().Count() != before+1 {
			t.Fatal("a chainless drop is a counted forward jump")
		}
	})
}

// A refused flush is unwound: the journal cuts back to before the refused
// block so the rebuild replaces it instead of stacking on it.
func TestRefusalDropsTheRefusedFlush(t *testing.T) {
	p := barePublisher()
	publishBlock(t, p, 1, common.Hash{0xef}, 1)

	before := len(p.journal.items)
	if before == 0 {
		t.Fatal("publish must queue entries")
	}

	if v := p.refuseGated(); v != miner.SealRefused {
		t.Fatalf("refusal verdict %v", v)
	}

	if len(p.journal.items) >= before {
		t.Fatal("the refused flush must be cut from the journal")
	}
}

// Refusing the same height repeatedly caps out into a liveness broadcast:
// losing blocks forever to a wedged verdict would strand the chain.
func TestRefusalStreakBroadcastsForLiveness(t *testing.T) {
	p := barePublisher()

	verdicts := []miner.SealVerdict{}
	for i := 0; i <= maxGateRefusals; i++ {
		p.gate = sealGate{height: 7, hash: common.Hash{0x07}}
		verdicts = append(verdicts, p.refuseGated())
	}

	for i := 0; i < maxGateRefusals; i++ {
		if verdicts[i] != miner.SealRefused {
			t.Fatalf("refusal %d must refuse, got %v", i, verdicts[i])
		}
	}

	if verdicts[maxGateRefusals] != miner.SealUnknown {
		t.Fatalf("the capped refusal must broadcast for liveness, got %v",
			verdicts[maxGateRefusals])
	}
}

// An armed offer the previous cycle never consumed is re-served; a window
// that grew since the snapshot is re-read and re-adopted in full.
func TestReofferServesAnUnconsumedWindow(t *testing.T) {
	h := startHarness(t)
	parent := common.Hash{0xef}

	appendForeignOpen(t, h, 1, parent)
	appendForeignRecord(t, h, testTx(t, 0))

	p := classifierPublisher(t, h, nil)

	w := p.AdoptWindow(1, parent)
	if w == nil || len(w.Txs) != 1 {
		t.Fatalf("a live window on our parent must adopt: %+v", w)
	}

	// The build died before opening: the next cycle re-serves the offer.
	w = p.AdoptWindow(1, parent)
	if w == nil || len(w.Txs) != 1 {
		t.Fatalf("an unconsumed offer must be re-served: %+v", w)
	}

	// The window grew between cycles: the re-offer carries the growth.
	appendForeignRecord(t, h, testTx(t, 1))

	w = p.AdoptWindow(1, parent)
	if w == nil || len(w.Txs) != 2 {
		t.Fatalf("a grown window must re-adopt in full: %+v", w)
	}
}

// The gate's last-look recheck reads what actually stands at the gated
// height: our own seal confirms, foreign content refuses, an unknown
// height stays verdictless.
func TestGateRecheckReadsTheGatedHeight(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)
	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	rp := classifierPublisher(t, h, nil)

	if v := rp.gateRecheck(sealGate{height: 1, hash: sealed.Hash()}); v != miner.SealConfirmed {
		t.Fatalf("our own standing seal must confirm, got %v", v)
	}

	if v := rp.gateRecheck(sealGate{height: 1, hash: common.Hash{0xbd}}); v != miner.SealRefused {
		t.Fatalf("foreign sealed content must refuse, got %v", v)
	}

	if v := rp.gateRecheck(sealGate{height: 9, hash: sealed.Hash()}); v != miner.SealUnknown {
		t.Fatalf("an unknown height must stay verdictless, got %v", v)
	}
}

// With no seal at or below the chain's head anchor, the consumer's resume
// ladder falls back to the earliest retained entry and skips what it
// cannot apply.
func TestConsumerFallsBackToTheEarliestEntry(t *testing.T) {
	ex := startExecHarness(t)
	h := startHarness(t)

	appendForeignOpen(t, h, 10, common.Hash{0x0a})
	orphan := testTx(t, 0)
	appendForeignRecord(t, h, orphan)

	consumer, err := NewConsumer(h.addr, ex.chain)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	consumer.Start()
	defer consumer.Close()

	time.Sleep(400 * time.Millisecond)

	if _, _, ok := consumer.Index().Lookup(orphan.Hash()); ok {
		t.Fatal("an unappliable window must be skipped, not indexed")
	}
}

// The recheck also reads live windows: records standing at the height that
// the block does not carry refuse the broadcast; a window our block leads
// stays verdictless.
func TestGateRecheckReadsLiveWindows(t *testing.T) {
	h := startHarness(t)
	parent := common.Hash{0xef}

	appendForeignOpen(t, h, 1, parent)
	tx := testTx(t, 0)
	appendForeignRecord(t, h, tx)

	rp := classifierPublisher(t, h, nil)

	uncovered := sealGate{height: 1, hash: common.Hash{0x01}}
	if v := rp.gateRecheck(uncovered); v != miner.SealRefused {
		t.Fatalf("acked records the block does not carry must refuse, got %v", v)
	}

	covering := sealGate{height: 1, hash: common.Hash{0x01}, txs: []common.Hash{tx.Hash()}}
	if v := rp.gateRecheck(covering); v != miner.SealUnknown {
		t.Fatalf("a window our block leads stays verdictless, got %v", v)
	}
}

// The drain takes at least one block per cycle but stops at the byte
// budget, leaving the remainder as pending debt for the next re-anchor.
func TestBackfillDrainStopsAtTheByteBudget(t *testing.T) {
	huge := make([]byte, backfillBatchBytes)

	bigBlock := func(n uint64, parent common.Hash) *types.Block {
		header := testHeader(n, parent)
		tx := types.NewTx(&types.LegacyTx{Nonce: n, Gas: 21000, GasPrice: common.Big1, Data: huge})

		return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{tx}})
	}

	p := barePublisher()
	p.chain = &fakeChain{blocks: map[uint64]*types.Block{
		1: bigBlock(1, common.Hash{0xef}),
		2: bigBlock(2, common.Hash{0x01}),
	}}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pendingFrom, p.pendingTo, p.pendingEntries = 1, 2, 6

	fresh := newJournal()
	p.backfillLocked(fresh, p.head)

	if p.pendingFrom != 2 || p.pendingTo != 2 {
		t.Fatalf("the over-budget remainder must stay pending: %d..%d", p.pendingFrom, p.pendingTo)
	}

	// One block rebuilt: open, its record, seal.
	if len(fresh.items) != 3 {
		t.Fatalf("one block per over-budget batch, rebuilt %d entries", len(fresh.items))
	}
}

// A canonical whitelisted milestone floors the backfill: heights at or
// below it are final and permanently served by the canonical chain, so
// the drain starts past the milestone and the skipped prefix becomes a
// counted forward jump.
func TestBackfillFloorsAtTheMilestone(t *testing.T) {
	blocks := map[uint64]*types.Block{}
	parent := common.Hash{0xef}

	for n := uint64(1); n <= 6; n++ {
		b := types.NewBlockWithHeader(testHeader(n, parent))
		blocks[n] = b
		parent = b.Hash()
	}

	p := barePublisher()
	p.chain = &fakeChain{
		blocks:    blocks,
		canonical: map[uint64]common.Hash{4: blocks[4].Hash()},
		current:   blocks[6].Header(),
	}
	p.finality = func() (bool, uint64, common.Hash) { return true, 4, blocks[4].Hash() }

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pendingFrom, p.pendingTo = 1, 6

	fresh := newJournal()
	p.backfillLocked(fresh, p.head)

	if p.pendingFrom != 0 || p.pendingTo != 0 {
		t.Fatalf("floored drain must settle the debt: %d..%d", p.pendingFrom, p.pendingTo)
	}

	// Two empty blocks past the milestone: an open and a seal each.
	if len(fresh.items) != 4 {
		t.Fatalf("two blocks past the milestone rebuild 4 entries, got %d", len(fresh.items))
	}

	if fresh.items[0].height != 5 {
		t.Fatalf("drain must start past the milestone: first height %d", fresh.items[0].height)
	}
}

// Without a usable milestone the depth cap floors the drain: at most
// backfillDepthCap blocks below the tip rebuild, the rest jump.
func TestBackfillDepthCapWithoutMilestone(t *testing.T) {
	blocks := map[uint64]*types.Block{}
	parent := common.Hash{0xef}
	tip := uint64(backfillDepthCap + 10)

	for n := uint64(1); n <= tip; n++ {
		b := types.NewBlockWithHeader(testHeader(n, parent))
		blocks[n] = b
		parent = b.Hash()
	}

	p := barePublisher()
	p.chain = &fakeChain{blocks: blocks, current: blocks[tip].Header()}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pendingFrom, p.pendingTo = 1, tip

	fresh := newJournal()
	p.backfillLocked(fresh, p.head)

	if len(fresh.items) == 0 {
		t.Fatal("capped drain rebuilt nothing")
	}

	if want := tip - backfillDepthCap + 1; fresh.items[0].height != want {
		t.Fatalf("drain must start at the depth cap %d, first height %d", want, fresh.items[0].height)
	}
}

// A floor inside the pending range drops only the finalized prefix: the
// drain rebuilds from the floor up and the debt settles completely.
func TestBackfillDrainsAboveTheFloorOnly(t *testing.T) {
	blocks := map[uint64]*types.Block{}
	parent := common.Hash{0xef}

	for n := uint64(1); n <= 6; n++ {
		b := types.NewBlockWithHeader(testHeader(n, parent))
		blocks[n] = b
		parent = b.Hash()
	}

	p := barePublisher()
	p.chain = &fakeChain{
		blocks:    blocks,
		canonical: map[uint64]common.Hash{3: blocks[3].Hash()},
		current:   blocks[6].Header(),
	}
	p.finality = func() (bool, uint64, common.Hash) { return true, 3, blocks[3].Hash() }

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pendingFrom, p.pendingTo = 1, 6

	fresh := newJournal()
	p.backfillLocked(fresh, p.head)

	if p.pendingFrom != 0 || p.pendingTo != 0 {
		t.Fatalf("drain above the floor must settle the debt: %d..%d", p.pendingFrom, p.pendingTo)
	}

	// Three empty blocks above the floor: an open and a seal each.
	if len(fresh.items) != 6 {
		t.Fatalf("blocks 4..6 rebuild 6 entries, got %d", len(fresh.items))
	}

	if fresh.items[0].height != 4 {
		t.Fatalf("drain must start at the floor: first height %d", fresh.items[0].height)
	}
}

// Debt entirely at or below the milestone drops whole: the jump open
// crosses it and nothing rebuilds.
func TestBackfillDropsDebtBelowTheMilestone(t *testing.T) {
	milestone := common.Hash{0x4d}
	p := barePublisher()
	p.chain = &fakeChain{canonical: map[uint64]common.Hash{9: milestone}}
	p.finality = func() (bool, uint64, common.Hash) { return true, 9, milestone }

	p.mu.Lock()
	defer p.mu.Unlock()

	p.pendingFrom, p.pendingTo, p.pendingEntries = 3, 8, 12

	fresh := newJournal()
	p.backfillLocked(fresh, p.head)

	if len(fresh.items) != 0 || p.pendingFrom != 0 || p.pendingTo != 0 || p.pendingEntries != 0 {
		t.Fatalf("finalized debt must drop whole: %d entries, pending %d..%d",
			len(fresh.items), p.pendingFrom, p.pendingTo)
	}
}

// A milestone naming a chain we do not hold is no floor at all — the
// depth cap (when the tip allows one) is the only bound then.
func TestBackfillIgnoresANonCanonicalMilestone(t *testing.T) {
	p := barePublisher()
	p.chain = &fakeChain{canonical: map[uint64]common.Hash{9: {0x0c}}}
	p.finality = func() (bool, uint64, common.Hash) { return true, 9, common.Hash{0xbe, 0xef} }

	p.mu.Lock()
	defer p.mu.Unlock()

	if f := p.backfillFloorLocked(); f != 0 {
		t.Fatalf("a non-canonical milestone must not floor the drain, got %d", f)
	}
}

// primeBackfill's bounds: an unknown seal edge primes nothing, standing
// debt widens rather than narrows, and an empty gap stays empty.
func TestPrimeBackfillBounds(t *testing.T) {
	p := barePublisher()
	p.chain = &fakeChain{}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Seal edge at 1: priming from genesis would republish the world.
	p.primeBackfillLocked(tailInfo{haveSeal: true, lastSealHeight: 1}, 3)

	if p.pendingFrom != 0 {
		t.Fatal("an unknown seal edge must prime nothing")
	}

	// A decoded seal primes from the next height; standing debt only widens.
	p.pendingFrom, p.pendingTo = 2, 9
	p.primeBackfillLocked(tailInfo{sealDecoded: true, haveSeal: true, lastSealHeight: 3}, 5)

	if p.pendingFrom != 2 || p.pendingTo != 9 {
		t.Fatalf("standing debt must widen, not narrow: %d..%d", p.pendingFrom, p.pendingTo)
	}

	// A seal edge already past the parent leaves no gap to prime.
	p.pendingFrom, p.pendingTo = 0, 0
	p.primeBackfillLocked(tailInfo{sealDecoded: true, haveSeal: true, lastSealHeight: 5}, 5)

	if p.pendingFrom != 0 {
		t.Fatal("no gap must prime nothing")
	}
}

// completeExtendedWindow only absorbs a store window that is a strict
// content prefix of ours: anything longer, divergent, or unparseable
// declines, and the journal stays untouched.
func TestCompleteExtendedWindowGuards(t *testing.T) {
	build := func(t *testing.T) *Publisher {
		t.Helper()

		p := barePublisher()
		p.OpenBlock(2, 1700000002, common.Hash{0x01}, 30_000_000, fee25())
		p.PublishTx(testTx(t, 0))

		return p
	}

	t.Run("window longer than ours", func(t *testing.T) {
		p := build(t)
		items, _ := p.journal.after(0)

		extra := recordEntry([]byte{0xaa}, items[1].post)
		info := tailInfo{
			s: items[1].post, tipOpen: true, tipOpenHeight: 2, tipOpenParent: common.Hash{0x01},
			window: []*pb.Entry{items[0].entry, items[1].entry, extra},
		}

		p.mu.Lock()
		defer p.mu.Unlock()

		p.curHeight = 2
		if p.completeExtendedWindowLocked(info, 2) {
			t.Fatal("a window longer than ours is not our prefix")
		}
	})

	t.Run("divergent content", func(t *testing.T) {
		p := build(t)
		items, _ := p.journal.after(0)

		foreign := recordEntry([]byte{0xbb}, items[0].post)
		info := tailInfo{
			s: items[1].post, tipOpen: true, tipOpenHeight: 2, tipOpenParent: common.Hash{0x01},
			window: []*pb.Entry{items[0].entry, foreign},
		}

		p.mu.Lock()
		defer p.mu.Unlock()

		p.curHeight = 2
		if p.completeExtendedWindowLocked(info, 2) {
			t.Fatal("divergent records are not our prefix")
		}
	})

	t.Run("no window of our own", func(t *testing.T) {
		p := barePublisher()
		info := tailInfo{s: commitment.Head{0x02}, tipOpen: true, tipOpenHeight: 2}

		p.mu.Lock()
		defer p.mu.Unlock()

		p.curHeight = 2
		if p.completeExtendedWindowLocked(info, 2) {
			t.Fatal("nothing local to complete against")
		}
	})
}

// An absorb hook error ends the walk with it; a base-less walk still folds
// the tail and derives the head from its first open.
func TestWalkAbsorbAndBases(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)
	publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	r := classifierPublisher(t, h, nil).read
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	boom := errors.New("absorb rejected")
	if _, err := r.walk(ctx, &pb.RangeRequest{}, func(*pb.Entry) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("the absorb error must end the walk, got %v", err)
	}

	info, err := r.walk(ctx, &pb.RangeRequest{}, nil)
	if err != nil || !info.explained {
		t.Fatalf("a floor walk over opens must explain the head: %v %v", info.explained, err)
	}
}

// The seal policy in one place: our hash confirms, foreign content refuses
// unless the build ran gate-tolerated over a sealed height.
func TestVerdictForSeal(t *testing.T) {
	ours := common.Hash{0x07}
	g := sealGate{hash: ours}

	if g.verdictForSeal(ours) != miner.SealConfirmed {
		t.Fatal("our own seal must confirm")
	}

	if g.verdictForSeal(common.Hash{0x08}) != miner.SealRefused {
		t.Fatal("foreign content must refuse")
	}

	g.tolerateSealed = true
	if g.verdictForSeal(common.Hash{0x08}) != miner.SealUnknown {
		t.Fatal("a tolerated gate must stay verdictless over foreign content")
	}
}

// An empty store serves a live frame straight away; a store restart ends
// the session with a transport error and the consumer survives to retry.
func TestConsumerIdlesLiveOnAnEmptyStore(t *testing.T) {
	ex := startExecHarness(t)
	h := startHarness(t)

	consumer, err := NewConsumer(h.addr, ex.chain)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	consumer.Start()
	defer consumer.Close()

	time.Sleep(200 * time.Millisecond) // reach the live frame

	h.stop()
	time.Sleep(100 * time.Millisecond) // absorb the transport error
	h.resume()

	if _, _, ok := consumer.Index().Lookup(common.Hash{0x01}); ok {
		t.Fatal("an empty store must index nothing")
	}
}
