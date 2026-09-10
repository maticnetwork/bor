package sequencer

import (
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/miner"
)

// The barrier's job is to compare the block against the store, not the
// journal against the store. A block built before an adoption leaves the
// journal perfectly in sync while the block itself carries different
// transactions — position agreement says nothing about what we are about to
// broadcast, and on a devnet that gap put two blocks at one height and
// orphaned the loser's acked records into the next block.
func TestBarrierRefusesABlockMissingThePromisedSequence(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// Another producer owns the window at height 2 and has three records
	// acked there.
	t0, t1, t2 := testTx(t, 0), testTx(t, 1), testTx(t, 2)
	appendForeignOpen(t, h, 2, parent)

	for _, tx := range []*types.Transaction{t0, t1, t2} {
		appendForeignRecord(t, h, tx)
	}

	// Our block carries only two of them: t1 was promised at this height
	// and this block does not deliver it.
	block := []*types.Transaction{t0, t2}

	if p.AwaitSequenced(time.Second, 2, block) {
		t.Fatal("barrier passed a block missing a transaction the store " +
			"already acked at this height")
	}

	if !p.ResyncNeeded() {
		t.Fatal("barrier refused without asking for a rebuild: the slot is " +
			"dropped instead of corrected")
	}
}

// The same read, with a block that does deliver the promised sequence, must
// pass — a barrier that refuses the correct block stalls the chain.
func TestBarrierPassesABlockCarryingThePromisedSequence(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	t0, t1 := testTx(t, 0), testTx(t, 1)
	appendForeignOpen(t, h, 2, parent)

	for _, tx := range []*types.Transaction{t0, t1} {
		appendForeignRecord(t, h, tx)
	}

	if !p.AwaitSequenced(time.Second, 2, []*types.Transaction{t0, t1}) {
		t.Fatal("barrier refused a block that is exactly the store's window")
	}
}

// A block may still be filling past what the store has acked; only dropping
// or reordering the acked prefix is a violation.
func TestBarrierPassesABlockExtendingThePromisedSequence(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	t0 := testTx(t, 0)
	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, t0)

	if !p.AwaitSequenced(time.Second, 2, []*types.Transaction{t0, testTx(t, 5)}) {
		t.Fatal("barrier refused a block that carries the promised prefix " +
			"and one more transaction of its own")
	}
}

// Reordering the acked prefix breaks the promise as surely as dropping it:
// a preconfirmation names a position, not just membership.
func TestBarrierRefusesAReorderedPromisedSequence(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	t0, t1 := testTx(t, 0), testTx(t, 1)
	appendForeignOpen(t, h, 2, parent)

	for _, tx := range []*types.Transaction{t0, t1} {
		appendForeignRecord(t, h, tx)
	}

	if p.AwaitSequenced(time.Second, 2, []*types.Transaction{t1, t0}) {
		t.Fatal("barrier passed a block that reordered the acked prefix")
	}
}

// An unreadable store must not gate production: liveness outranks a check we
// cannot perform.
func TestBarrierPassesWhenTheStoreCannotBeRead(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 0))

	gate := &blockableConsumer{}

	p.mu.Lock()
	gate.ConsumerServiceClient = p.read.cons
	p.read.cons = gate
	p.mu.Unlock()
	gate.block(true)

	if !p.AwaitSequenced(time.Second, 2, []*types.Transaction{testTx(t, 9)}) {
		t.Fatal("an unreadable store blocked production")
	}
}

// A re-anchor rebuilds our own entries onto the store head without ever
// ingesting what stands behind it, so displacing a live foreign window
// silently drops every record it holds that our block lacks. That is only
// defensible when our block is the one the chain kept; when it lost, the
// flush must wait rather than destroy the winner's sequence.
func TestFlushWithholdsDisplacementWhenOurBlockLost(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// We build and seal height 2 locally.
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	// Another producer's window takes the head at the same height, with
	// records of its own, before our seal goes out.
	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 7))

	before := len(readAllGenerations(t, h))

	ours := testHeader(2, parent)
	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 0)}))

	// The chain kept that producer's block, not ours.
	chain.canonical[2] = common.Hash{0xbb}

	info, _ := p.readTail(t.Context())

	p.mu.Lock()
	out, handled := p.classifyPendingFlushLocked(info)
	p.mu.Unlock()

	if !handled || out != recOK {
		t.Fatalf("flush classification: out=%v handled=%v", out, handled)
	}

	time.Sleep(300 * time.Millisecond)

	if got := len(readAllGenerations(t, h)); got != before {
		t.Fatalf("flush displaced a live window for a block the chain did "+
			"not keep: %d generations, want %d", got, before)
	}
}

// The devnet case: our seal is rejected and our flush reaches the store in
// the same millisecond, 134ms before the winner's block imports. The chain
// has no opinion yet at that instant, and treating "undecided" as permission
// is what displaced a live window on behalf of a block that was never
// broadcast.
func TestFlushWithholdsDisplacementWhileChainUndecided(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 7))

	before := len(readAllGenerations(t, h))

	ours := testHeader(2, parent)
	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 0)}))

	// The chain holds nothing at this height: the winner's block is still
	// in flight. Our block is not known to have been kept.
	info, _ := p.readTail(t.Context())

	p.mu.Lock()
	out, handled := p.classifyPendingFlushLocked(info)
	p.mu.Unlock()

	if !handled || out != recOK {
		t.Fatalf("flush classification: out=%v handled=%v", out, handled)
	}

	time.Sleep(300 * time.Millisecond)

	if got := len(readAllGenerations(t, h)); got != before {
		t.Fatalf("displaced a live window while the chain had not kept our "+
			"block: %d generations, want %d", got, before)
	}
}

func mustReadTail(t *testing.T, p *Publisher) tailInfo {
	t.Helper()

	info, out := p.readTail(t.Context())
	if out != recOK {
		t.Fatalf("tail read: %v", out)
	}

	return info
}

// The displacement counter has to name what was actually lost. Counting the
// whole displaced window reports thousands of orphaned records when the
// block delivers every one of them, which makes the metric useless for
// telling a harmless supersede from a damaging one.
func TestDisplacementCountsOnlyWhatTheBlockDropped(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// Our window: two transactions, published and acked.
	shared, alsoShared := testTx(t, 0), testTx(t, 1)
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(shared)
	p.PublishTx(alsoShared)
	waitDrained(t, p, 5*time.Second)

	// The window we would displace holds both of ours plus one we lack.
	dropped := testTx(t, 7)
	appendForeignOpen(t, h, 2, parent)

	for _, tx := range []*types.Transaction{shared, alsoShared, dropped} {
		appendForeignRecord(t, h, tx)
	}

	// Our block seals with our two, and the chain keeps it, so the flush is
	// allowed to displace — the counter must then name the one record lost.
	ours := testHeader(2, parent)
	p.SealBlock(blockFor(ours, []*types.Transaction{shared, alsoShared}))
	chain.canonical[2] = ours.Hash()
	chain.blocks = map[uint64]*types.Block{
		2: blockFor(ours, []*types.Transaction{shared, alsoShared}),
	}

	info := mustReadTail(t, p)

	p.mu.Lock()
	orphans := p.orphanedByDisplacementLocked(info, 2)
	may := p.mayDisplaceWindowLocked(info, 2)
	p.mu.Unlock()

	if !may {
		t.Fatal("withheld a displacement for a block the chain kept")
	}

	if orphans != 1 {
		t.Fatalf("counted %d orphaned records, want 1: only the transaction "+
			"this block does not carry is lost", orphans)
	}
}

// Under load the drain does not finish inside the barrier's budget, and the
// old deadline path surrendered — returning "seal it" without ever comparing
// content. That is how a block with none of a 9523-record window reached the
// chain while every one of those records was already acked. The check has to
// survive the deadline; a still-draining window is a prefix of our own block,
// so the comparison is valid mid-drain.
func TestBarrierChecksContentEvenWhenTheDrainOvershoots(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// Another producer's window stands at our height with records acked.
	promised := []*types.Transaction{testTx(t, 0), testTx(t, 1), testTx(t, 2)}
	appendForeignOpen(t, h, 2, parent)

	for _, tx := range promised {
		appendForeignRecord(t, h, tx)
	}

	// Hold our own writes so the journal keeps entries in flight: the
	// barrier can never reach unacked == 0 and must take the deadline path.
	// A build hold, not a sticky one — sticky is the contended case and
	// returns before the deadline is ever reached.
	p.mu.Lock()
	p.hold = hold{after: p.ackedSeq, kind: holdBuild}
	p.mu.Unlock()

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 8))

	p.mu.Lock()
	draining, sticky := p.unackedLocked() > 0, p.hold.kind == holdSticky
	p.mu.Unlock()

	if !draining || sticky {
		t.Fatalf("setup did not reach the deadline path: draining=%v sticky=%v",
			draining, sticky)
	}

	// An empty block, exactly the shape that reached the chain at 915.
	if p.AwaitSequenced(50*time.Millisecond, 2, nil) {
		t.Fatal("barrier surrendered on its deadline and passed a block " +
			"carrying none of the promised sequence")
	}
}

// Mid-drain, the anchor walk starts past the window's open and returns only
// our trailing records: no window to reconstruct (storeWindow reads zero), a
// head past our anchor, and a tip that is not "open at this height" as far as
// the read can tell. Treating that as foreign content refused the producer's
// own block, adopted its own window minus the in-flight tail, and cost the
// height a rebuild cycle or two — the 2-3s blocks a devnet saw on every
// ack-lag spike. The matcher already proved the suffix ours; the mirror must
// use that and compare the block against the journal.
func TestBarrierPassesMidDrainWhenStoreTailIsOurOwnWindow(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	t0, t1 := testTx(t, 0), testTx(t, 1)
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(t0)
	p.PublishTx(t1)
	waitDrained(t, p, 5*time.Second)

	// Rewind the ack bookkeeping to just after the open: the records are
	// committed and visible in the store, their acks not yet processed —
	// the read lands mid-drain.
	p.mu.Lock()
	window := p.journal.suffixFromHeight(2)
	if len(window) != 3 || window[0].kind != entryOpen {
		p.mu.Unlock()
		t.Fatalf("setup: journal window is not open+2 records: %d items", len(window))
	}
	p.ackedSeq, p.anchor = window[0].seq, window[0].post
	p.mu.Unlock()

	if !p.AwaitSequenced(150*time.Millisecond, 2, []*types.Transaction{t0, t1}) {
		t.Fatal("barrier refused the producer's own block over its own " +
			"still-draining window")
	}

	if p.ResyncNeeded() {
		t.Fatal("a mid-drain read of our own window armed a rebuild")
	}
}

// The same mid-drain shape with a record we do not hold is a real extension:
// the store promises a transaction the block lacks, and the refusal stands.
func TestBarrierStillRefusesWhenStoreExtendsPastOurJournal(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	t0, t1 := testTx(t, 0), testTx(t, 1)
	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(t0)
	p.PublishTx(t1)
	waitDrained(t, p, 5*time.Second)

	// A record past everything we hold: adopted-then-extended, or a rival's
	// append onto our window's head.
	appendForeignRecord(t, h, testTx(t, 7))

	p.mu.Lock()
	window := p.journal.suffixFromHeight(2)
	p.ackedSeq, p.anchor = window[0].seq, window[0].post
	p.mu.Unlock()

	if p.AwaitSequenced(150*time.Millisecond, 2, []*types.Transaction{t0, t1}) {
		t.Fatal("barrier passed a block missing a record the store holds " +
			"past our journal")
	}

	if !p.ResyncNeeded() {
		t.Fatal("a real extension refusal did not ask for a rebuild")
	}
}

// The same deadline path must still pass a block that does carry the
// promised prefix, or a slow drain would stall production outright.
func TestBarrierDeadlineStillPassesACoveringBlock(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	t0 := testTx(t, 0)
	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, t0)

	// Our block carries the promised record plus more still in flight.
	if !p.AwaitSequenced(50*time.Millisecond, 2,
		[]*types.Transaction{t0, testTx(t, 1), testTx(t, 2)}) {
		t.Fatal("barrier refused a block that carries the promised prefix " +
			"and is still filling")
	}
}

// Every per-block wait on the store is pointless once the transport is
// failing, and paying them all pushes blocks past their slot — which is what
// arms bor's span-check path and turns a store outage into a chain
// slowdown. A devnet outage cost 22s per block this way.
func TestUnreachableStoreCostsNoWaiting(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.unreachable.Store(true)

	header := testHeader(2, parent)
	p.SealBlock(blockFor(header, nil))

	start := time.Now()

	if v := p.ConfirmSeal(4 * time.Second); v != miner.SealUnknown {
		t.Fatalf("verdict = %v, want Unknown with the store unreachable", v)
	}

	if !p.AwaitSequenced(4*time.Second, 3, nil) {
		t.Fatal("the barrier gated production on an unreachable store")
	}

	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("spent %v waiting on a store known to be down", elapsed)
	}
}

// And the moment a read succeeds the publisher stops treating the store as
// down: a latched flag would keep holding builds after recovery.
func TestSuccessfulReadClearsUnreachable(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	p.unreachable.Store(true)
	p.AdoptWindow(2, sealHash(t, sealed))

	if p.unreachable.Load() {
		t.Fatal("a successful build-start read left the store marked down")
	}
}

// While the backfill drains, the store is behind us by construction: there
// is nothing worth comparing against and nothing worth waiting for. Making
// sealing wait through the drain is what stalled a devnet for 44 seconds on
// the cycle after a store outage.
func TestCatchUpDoesNotGateSealing(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// A foreign window stands at our height that our block does not carry —
	// normally grounds for a rebuild. Draining a backfill outranks it.
	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 0))

	p.mu.Lock()
	p.pendingFrom, p.pendingTo = 1, 1
	p.mu.Unlock()

	start := time.Now()

	if !p.AwaitSequenced(2*time.Second, 2, nil) {
		t.Fatal("sealing was gated while the backfill was still draining")
	}

	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("waited %v during a drain that makes the comparison "+
			"meaningless anyway", elapsed)
	}
}

// The unreachable flag must be set by the transport layer itself when the
// store goes away — the no-wait short-circuits are worthless if nothing
// arms them.
func TestTransportFailureMarksTheStoreUnreachable(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	h.stop()
	p.OpenBlock(2, 1700000002, common.Hash{0xaa}, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))

	waitFor(t, 10*time.Second, func() bool { return p.unreachable.Load() })
}

// A sealed foreign generation at the flush height is a stronger claim than a
// live window, and the flush must not chain past it on the head's bytes
// alone: with consensus undecided, superseding it is how a twin race minted
// generation after generation at one height. Only affirmative proof that
// the chain kept our block licenses the displacement — and our own seal
// already standing there is re-delivery, not displacement.
func TestFlushWithholdsOverAForeignSealUntilCanonical(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	// The twin's generation seals first; our own block for 2 exists too.
	twin := testHeader(2, parent)
	twin.Extra = []byte("twin")
	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 7))
	appendForeignSeal(t, h, twin)

	ours := testHeader(2, parent)
	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 0)}))

	info := mustReadTail(t, p)

	if !info.sealDecoded || info.lastSealHeight != 2 {
		t.Fatalf("setup: read did not decode the foreign seal (%+v)", info)
	}

	p.mu.Lock()
	undecided := p.mayDisplaceWindowLocked(info, 2)
	chain.canonical[2] = ours.Hash()
	won := p.mayDisplaceWindowLocked(info, 2)
	chain.canonical[2] = twin.Hash()
	lost := p.mayDisplaceWindowLocked(info, 2)
	p.mu.Unlock()

	if undecided {
		t.Fatal("flush chained past a foreign seal with consensus undecided")
	}

	if !won {
		t.Fatal("flush withheld even though the chain kept our block")
	}

	if lost {
		t.Fatal("flush displaced the seal of the block the chain kept")
	}
}

// The store's standing seal being our own block is the duplicate-delivery
// case, not a displacement: it must not be withheld, or a resumed flush
// could never finish.
func TestFlushProceedsOverOurOwnStandingSeal(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	ours := testHeader(2, parent)
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, ours) // the standing seal IS our block

	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 0)}))

	info := mustReadTail(t, p)

	p.mu.Lock()
	may := p.mayDisplaceWindowLocked(info, 2)
	p.mu.Unlock()

	if !may {
		t.Fatal("our own standing seal was treated as a foreign displacement")
	}
}

// A displaced foreign seal has no window in the read to count, and the
// trailing window of a higher height must not be counted against this
// height's block: that mispairing once reported 1,960 phantom orphans for
// a displacement that destroyed nothing.
func TestSealDisplacementCountsNoPhantomOrphans(t *testing.T) {
	fc := &fakeChain{canonical: map[uint64]common.Hash{}}
	p, _ := lineagePublisher(t, fc)

	h1 := testHeader(1, common.Hash{0xef})
	header := testHeader(2, h1.Hash())
	tx := testTx(t, 1)
	sealOnChain(p, fc, header, []*types.Transaction{tx})
	fc.blocks = map[uint64]*types.Block{2: blockFor(header, []*types.Transaction{tx})}

	stranger := testTx(t, 2)
	raw, err := stranger.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	open3 := openEntry(commitment.OpenContext{
		Number:     3,
		Timestamp:  header.Time + 4,
		ParentHash: header.Hash(),
		GasLimit:   header.GasLimit,
		BaseFee:    header.BaseFee,
	}, commitment.Head{0x77})

	info := tailInfo{
		s:              commitment.Head{0x79},
		tipOpen:        true,
		tipOpenHeight:  3,
		window:         []*pb.Entry{open3, recordEntry(raw, commitment.Head{0x78})},
		haveSeal:       true,
		sealDecoded:    true,
		lastSealHeight: 2,
		lastSealHash:   common.Hash{0xdd},
	}

	before := windowDisplacedRecords.Snapshot().Count()

	p.mu.Lock()
	may := p.mayDisplaceWindowLocked(info, 2)
	p.mu.Unlock()

	if !may {
		t.Fatal("withheld a canonical-proven displacement")
	}

	if got := windowDisplacedRecords.Snapshot().Count() - before; got != 0 {
		t.Fatalf("counted %d orphans from a higher height's window", got)
	}
}
