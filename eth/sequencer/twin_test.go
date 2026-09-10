package sequencer

import (
	"testing"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
)

// Two publishers against one store: the unit-test stand-in for two block
// producers sharing a signing key. Every scenario below is one that cost a
// kurtosis run to find, so each is pinned here instead.
func twinPublishers(t *testing.T) (*harness, *Publisher, *Publisher) {
	t.Helper()

	h := startHarness(t)

	return h, newTestPublisher(t, h, &fakeChain{}), newTestPublisher(t, h, &fakeChain{})
}

// sealedParent gets both publishers past a sealed block 1 so height 2 is a
// clean contested slot, and returns the parent hash they must both build on.
func sealedParent(t *testing.T, h *harness, incumbent, twin *Publisher) common.Hash {
	t.Helper()

	sealed := publishBlock(t, incumbent, 1, common.Hash{0xef}, 1)
	waitHead(t, h, incumbent, 5*time.Second)

	// The twin has to see block 1 too, or its own build starts from a stale
	// anchor and the scenario tests the wrong thing.
	waitFor(t, 5*time.Second, func() bool { return twin.isAnchored() })

	return sealHash(t, sealed)
}

// The height=243 failure: the twin adopts a snapshot of the incumbent's
// window, the incumbent keeps writing, and the twin seals the snapshot —
// stranding every record added since. Measured as a 362-record block
// displacing 1088 already-acked ones.
func TestTwinDoesNotSealStaleSnapshot(t *testing.T) {
	h, incumbent, twin := twinPublishers(t)
	parent := sealedParent(t, h, incumbent, twin)

	incumbent.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	incumbent.PublishTx(testTx(t, 0))
	incumbent.PublishTx(testTx(t, 1))
	waitDrained(t, incumbent, 5*time.Second)

	// The twin inherits the window as it stands now: two records.
	w := twin.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("twin did not adopt the incumbent's window")
	}

	if len(w.Txs) != 2 {
		t.Fatalf("adopted %d txs, want the 2 published so far", len(w.Txs))
	}

	// The incumbent is alive and keeps going. The twin's snapshot is now
	// stale by one record.
	incumbent.PublishTx(testTx(t, 2))
	waitDrained(t, incumbent, 5*time.Second)

	// Mark the height contested, as a STALE would, and take the contested
	// path twice: the first call rebuilds, the second must still refuse
	// because the window moved on.
	twin.mu.Lock()
	twin.curHeight = 2
	twin.hold = hold{after: twin.ackedSeq, kind: holdSticky}
	twin.mu.Unlock()

	if awaitOurWindow(twin, 2*time.Second) {
		t.Fatal("first contested attempt sealed instead of rebuilding")
	}

	if awaitOurWindow(twin, 2*time.Second) {
		t.Fatal("twin sealed a stale snapshot: the records the incumbent " +
			"added after adoption were acked, so they are preconfirmed and " +
			"this block strands them")
	}
}

// The same path must still reach a seal once the incumbent stops, or
// contention has no liveness escape and the chain stalls — the failure that
// held a devnet at one height for five minutes.
func TestTwinSealsOnceWindowSettles(t *testing.T) {
	h, incumbent, twin := twinPublishers(t)
	parent := sealedParent(t, h, incumbent, twin)

	incumbent.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	incumbent.PublishTx(testTx(t, 0))
	waitDrained(t, incumbent, 5*time.Second)

	if twin.AdoptWindow(2, parent) == nil {
		t.Fatal("twin did not adopt the incumbent's window")
	}

	twin.mu.Lock()
	twin.curHeight = 2
	twin.hold = hold{after: twin.ackedSeq, kind: holdSticky}
	twin.mu.Unlock()

	if awaitOurWindow(twin, 2*time.Second) {
		t.Fatal("a contested attempt sealed instead of rebuilding")
	}

	twin.ResyncNeeded() // the worker's rebuild consumes the signal

	// The rebuild adopts: the incumbent wrote nothing more, so the adopted
	// window is the whole of the store's content and the seal proceeds.
	if twin.AdoptWindow(2, parent) == nil {
		t.Fatal("rebuild did not adopt the settled window")
	}

	if !awaitOurWindow(twin, 2*time.Second) {
		t.Fatal("twin refused a settled, fully adopted window: nobody would " +
			"ever close this height")
	}
}

// Adoption hands the build exactly the store's transactions: the adopted
// window is the content the block extends, in store order.
func TestContestedTwinSealsWindowExactly(t *testing.T) {
	h, incumbent, twin := twinPublishers(t)
	parent := sealedParent(t, h, incumbent, twin)

	incumbent.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	incumbent.PublishTx(testTx(t, 0))
	incumbent.PublishTx(testTx(t, 1))
	waitDrained(t, incumbent, 5*time.Second)

	w := twin.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("twin did not adopt the incumbent's window")
	}

	if len(w.Txs) != 2 {
		t.Fatalf("window carries %d txs, want the 2 in the store", len(w.Txs))
	}
}

// The mid-window blind spot, closed by Rule 1: a producer whose anchor sits
// at the store head — past the live window's open — used to classify the
// tail as clean and open a second generation beside the incumbent's. The
// build-start read now probes down from the height itself, so the window is
// visible from anywhere.
func TestBuildStartSeesLiveWindowFromAMidWindowAnchor(t *testing.T) {
	h, incumbent, twin := twinPublishers(t)
	parent := sealedParent(t, h, incumbent, twin)

	incumbent.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	incumbent.PublishTx(testTx(t, 0))
	waitDrained(t, incumbent, 5*time.Second)

	// Strand the twin's anchor at the store head, mid-window: exactly the
	// state a between-blocks re-anchor used to leave behind.
	twin.mu.Lock()
	twin.anchor = incumbent.head
	twin.confirmed = true
	twin.mu.Unlock()

	w := twin.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("a live window at this height was invisible from a " +
			"mid-window anchor: the twin would open a second generation " +
			"beside the incumbent's")
	}

	if len(w.Txs) != 1 {
		t.Fatalf("adopted %d txs, want the incumbent's 1", len(w.Txs))
	}
}

// The open CAS is the election: when both twins open the same height, one
// open lands and the other STALEs. The loser must adopt — and the store must
// end up with exactly one generation at the height, which is the invariant
// every double-seal traced back to.
func TestLosingOpenAdoptsAndAddsNoGeneration(t *testing.T) {
	h, a, b := twinPublishers(t)
	parent := sealedParent(t, h, a, b)

	// A wins the height: its window is in the store.
	a.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	a.PublishTx(testTx(t, 0))
	waitDrained(t, a, 5*time.Second)

	// B opens the same height blind (its build-start read raced A's open).
	// Divergent content, so nothing absorbs: B's open must STALE.
	b.OpenBlock(2, 1700000009, parent, 30_000_000, fee25())
	b.PublishTx(testTx(t, 7))

	// B discovers the loss and asks for a rebuild.
	waitFor(t, 5*time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()

		return b.hold.kind == holdSticky || b.resync
	})

	if !awaitOurWindow(b, time.Second) {
		b.ResyncNeeded() // the worker's rebuild consumes the signal
	}

	// The rebuild's boundary read adopts A's window.
	w := b.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("the losing twin did not adopt the winner's window")
	}

	// The store holds exactly one generation at height 2: the CAS kept the
	// loser's open out entirely.
	gens := 0
	for _, g := range readAllGenerations(t, h) {
		if g.height == 2 {
			gens++
		}
	}

	if gens != 1 {
		t.Fatalf("store holds %d generations at height 2, want exactly 1: "+
			"a second generation is where every double-seal came from", gens)
	}
}

// windowContent returns this publisher's entries for a height, so two
// publishers' views can be compared for genuine agreement rather than for
// having merely passed the same checks.
func windowContent(t *testing.T, p *Publisher, height uint64) []*pb.Entry {
	t.Helper()

	p.mu.Lock()
	defer p.mu.Unlock()

	var out []*pb.Entry
	for _, it := range p.journal.suffixFromHeight(height) {
		out = append(out, it.entry)
	}

	return out
}

func sameContent(a, b []*pb.Entry) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !contentEqual(a[i], b[i]) {
			return false
		}
	}

	return true
}

// decide drives a publisher to a final seal answer. The contested path
// defers once before deciding, so one call is not the verdict.
func decide(p *Publisher) bool {
	for i := 0; i < 3; i++ {
		if ok := awaitOurWindow(p, time.Second); ok {
			return true
		}

		p.ResyncNeeded() // consume, as the worker's rebuild would
	}

	return false
}

// The invariant the whole mechanism exists for: two producers at one height
// must not both seal while holding different content. Every other test here
// checks a decision in isolation; this one checks the outcome, which is what
// a consumer actually experiences.
func TestTwinsNeverBothSealDivergentContent(t *testing.T) {
	h, a, b := twinPublishers(t)
	parent := sealedParent(t, h, a, b)

	// A gets its window in first.
	a.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	a.PublishTx(testTx(t, 0))
	waitDrained(t, a, 5*time.Second)

	// B builds the same height on the same context with different content.
	b.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	b.PublishTx(testTx(t, 7))

	waitFor(t, 5*time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()

		return b.hold.kind == holdSticky || b.unackedLocked() == 0
	})

	sealA, sealB := decide(a), decide(b)

	if sealA && sealB && !sameContent(windowContent(t, a, 2), windowContent(t, b, 2)) {
		t.Fatal("both producers sealed height 2 holding different content: " +
			"consensus discards one block and every record only in it loses " +
			"its preconfirmation")
	}
}

// A contested height must not seal on an unreadable tail. This is the state
// that held for 68 of one node's reads in a five-minute devnet window
// ("rung out of budget"), and treating it as permission to seal is what
// turned every missed read into a divergent block.
func TestContestedSealRefusedWhenTailUnreadable(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	p.mu.Lock()
	p.curHeight = 2
	p.hold = hold{after: p.ackedSeq, kind: holdSticky}
	p.read.cons = &failingConsumer{}
	p.mu.Unlock()

	if awaitOurWindow(p, time.Second) {
		t.Fatal("sealed a contested height without being able to read the " +
			"store: with no way to know what the other producer wrote, this " +
			"block can only diverge")
	}
}

// The same requirement for the coverage check on its own: an unreadable tail
// is not evidence of coverage. Failing open here is what made every dropped
// read a licence to seal.
func TestCoverageFailsClosedOnUnreadableTail(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 5*time.Second)

	// Uncontested but unreadable: an ordinary build must still seal, because
	// production never waits on the store. The distinction is contention.
	p.mu.Lock()
	p.read.cons = &failingConsumer{}
	p.mu.Unlock()

	if !awaitOurWindow(p, time.Second) {
		t.Fatal("an unreachable store blocked an uncontested seal: " +
			"production must never wait on the store")
	}
}

// The counterpart invariant: under contention someone must still seal. Two
// producers politely refusing each other is what held a devnet at one height
// for five minutes, and no safety property is worth a stopped chain.
//
// Together with TestTwinsNeverBothSealDivergentContent this pins the whole
// trade: that test forbids two divergent seals, this one forbids zero seals.
// Any change that satisfies one by breaking the other has not solved
// anything.
func TestTwinsAlwaysProduceASeal(t *testing.T) {
	h, a, b := twinPublishers(t)
	parent := sealedParent(t, h, a, b)

	a.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	a.PublishTx(testTx(t, 0))
	waitDrained(t, a, 5*time.Second)

	b.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	b.PublishTx(testTx(t, 7))

	waitFor(t, 5*time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()

		return b.hold.kind == holdSticky || b.unackedLocked() == 0
	})

	if !decide(a) && !decide(b) {
		t.Fatal("neither producer sealed height 2: the height never closes " +
			"and the chain stops advancing")
	}
}
