package sequencer

import (
	"testing"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// A probe-inferred boundary is not a decoded seal: recording it as the
// sealed tip told every backfill the height was owed nothing, and an
// outage's partial delivery at exactly that height became a permanent hole
// (devnet height 364: partial 340/1399, skipped by every repairer).
func TestInferredBoundaryDoesNotAdvanceTheSealedTip(t *testing.T) {
	p := barePublisher()
	p.chain = &fakeChain{}

	p.applyTail(tailInfo{haveSeal: true, lastSealHeight: 9}) // no sealDecoded

	p.mu.Lock()
	tip := p.storeSealedTip
	p.mu.Unlock()

	if tip != 0 {
		t.Fatalf("storeSealedTip = %d from an inferred boundary, want 0: "+
			"only a decoded seal proves anything is sealed", tip)
	}
}

// Priming from an undecoded boundary includes the boundary height itself: it
// may be a partial delivery, and a duplicate generation is cheaper than a
// hole.
func TestPrimeIncludesAnUndecodedBoundary(t *testing.T) {
	p := barePublisher()
	p.chain = &fakeChain{}

	p.mu.Lock()
	p.primeBackfillLocked(tailInfo{haveSeal: true, lastSealHeight: 7}, 10)
	from, to := p.pendingFrom, p.pendingTo
	p.mu.Unlock()

	if from != 7 || to != 9 {
		t.Fatalf("pending = [%d,%d], want [7,9]: an inferred boundary at 7 "+
			"may be a partial window and must be re-delivered", from, to)
	}
}

// And a decoded seal at the boundary starts the range above it, as before.
func TestPrimeExcludesADecodedSeal(t *testing.T) {
	p := barePublisher()
	p.chain = &fakeChain{}

	p.mu.Lock()
	p.primeBackfillLocked(tailInfo{haveSeal: true, sealDecoded: true, lastSealHeight: 7}, 10)
	from, to := p.pendingFrom, p.pendingTo
	p.mu.Unlock()

	if from != 8 || to != 9 {
		t.Fatalf("pending = [%d,%d], want [8,9]", from, to)
	}
}

// A refold that abandons unacked sealed heights must put them back in the
// pending range: the backfill advanced past them at build time, and without
// restoration nothing remembers the debt.
func TestAbandonedBackfillHeightsReturnToPending(t *testing.T) {
	p := barePublisher()
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p.chain = chain

	parent := common.Hash{0xef}
	for n := uint64(4); n <= 6; n++ {
		header := testHeader(n, parent)
		chain.blocks[n] = blockFor(header, nil)
		parent = header.Hash()
	}

	// The journal holds rebuilt-but-unacked seals for 4..5, as after a
	// backfill batch whose delivery lost its head race. pendingFrom has
	// already advanced past them.
	p.mu.Lock()
	cur := commitment.Seed(testChainID)
	fresh := newJournal()
	for n := uint64(4); n <= 5; n++ {
		next, ok := p.appendBlockLocked(fresh, cur, chain.blocks[n])
		if !ok {
			p.mu.Unlock()
			t.Fatal("appendBlock failed")
		}
		cur = next
	}
	p.journal = fresh
	p.ackedSeq = 0
	p.pendingFrom, p.pendingTo = 6, 6

	// A forward-jump refold abandons everything (empty suffix). The restored
	// debt is drained by this same refold's backfill, so the proof is the
	// rebuilt content: the fresh journal must carry the abandoned heights
	// again, not silently forget them.
	p.refoldLocked(commitment.Head{0x99}, ^uint64(0), nil)

	rebuilt := map[uint64]bool{}
	for _, it := range p.journal.items {
		if it.kind == entrySeal {
			rebuilt[it.height] = true
		}
	}
	p.mu.Unlock()

	for _, h := range []uint64{4, 5, 6} {
		if !rebuilt[h] {
			t.Fatalf("height %d not rebuilt after its batch was abandoned: "+
				"the debt was forgotten (rebuilt: %v)", h, rebuilt)
		}
	}
}

// A partial abandonment restores only what was actually abandoned: heights
// kept by the refold's suffix are still owed by the journal itself, and
// re-adding them to pending would double-deliver every surviving flush.
func TestPartialAbandonmentRestoresOnlyTheAbandonedHeights(t *testing.T) {
	p := barePublisher()
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p.chain = chain

	parent := common.Hash{0xef}
	for n := uint64(4); n <= 6; n++ {
		header := testHeader(n, parent)
		chain.blocks[n] = blockFor(header, nil)
		parent = header.Hash()
	}

	p.mu.Lock()
	cur := commitment.Seed(testChainID)
	fresh := newJournal()
	for n := uint64(4); n <= 6; n++ {
		next, ok := p.appendBlockLocked(fresh, cur, chain.blocks[n])
		if !ok {
			p.mu.Unlock()
			t.Fatal("appendBlock failed")
		}
		cur = next
	}
	p.journal = fresh
	p.ackedSeq = 0

	// The refold keeps height 6 (a live flush) and abandons 4-5.
	p.refoldLocked(commitment.Head{0x99}, 6, nil)
	from, to := p.pendingFrom, p.pendingTo

	rebuilt := map[uint64]int{}
	for _, it := range p.journal.items {
		if it.kind == entrySeal {
			rebuilt[it.height]++
		}
	}
	p.mu.Unlock()

	// 4 and 5 return via the restored pending range (drained by this same
	// refold's backfill); 6 survives via the suffix — exactly once each.
	for _, h := range []uint64{4, 5, 6} {
		if rebuilt[h] != 1 {
			t.Fatalf("height %d appears %d times, want exactly once "+
				"(pending was [%d,%d])", h, rebuilt[h], from, to)
		}
	}
}

// A collapse during an active drain reaches heights below pendingFrom — the
// stranded batch suffix is the journal's oldest content — and must merge
// into the pending range. Clobbering pendingTo downward inverted the range,
// which the backfill read as "nothing owed" and zeroed: 25 heights of a
// 50-block outage evaporated as permanent holes exactly that way.
func TestCollapseMergesIntoThePendingRange(t *testing.T) {
	p := barePublisher()
	p.pendingFrom, p.pendingTo = 254, 270

	parent := common.Hash{0xee}

	for n := uint64(228); n <= 231; n++ {
		header := testHeader(n, parent)
		p.OpenBlock(n, header.Time, parent, header.GasLimit, header.BaseFee)
		p.SealBlock(blockFor(header, nil))
		parent = header.Hash()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.unackedSealsLocked() > journalHotSeals {
		t.Fatalf("collapse did not run: %d unacked seals", p.unackedSealsLocked())
	}

	if p.pendingFrom != 228 {
		t.Fatalf("pendingFrom = %d, want 228: the collapsed height must extend the range downward", p.pendingFrom)
	}

	if p.pendingTo != 270 {
		t.Fatalf("pendingTo = %d, want 270: the upper bound must never move down", p.pendingTo)
	}

	if p.pendingEntries == 0 {
		t.Fatal("collapsed entries were not accounted")
	}
}
