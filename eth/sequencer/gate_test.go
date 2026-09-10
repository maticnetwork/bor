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
	"github.com/ethereum/go-ethereum/rlp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const sealGateBudget = contestedGateTimeout

// errUnauthorizedTestSigner stands in for the engine's "signer is not a part
// of the producer set" rejection in gate tests.
var errUnauthorizedTestSigner = errors.New("signer is not a part of the producer set")

// gateBlock builds and seals a block through the publisher, returning it for
// ConfirmSeal. The window drains first, as the pre-seal barrier guarantees
// in production.
func gateBlock(t *testing.T, p *Publisher, h *harness, number uint64, parent common.Hash) *types.Block {
	t.Helper()

	header := testHeader(number, parent)
	p.OpenBlock(number, header.Time, parent, header.GasLimit, header.BaseFee)

	tx := testTx(t, 0)
	p.PublishTx(tx)
	waitDrained(t, p, 5*time.Second)

	block := blockFor(header, []*types.Transaction{tx})
	p.SealBlock(block)

	return block
}

// The sole producer's seal acks and the gate confirms in milliseconds — the
// path every block takes when nothing is wrong.
func TestSealGateConfirmsSoleProducer(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	gateBlock(t, p, h, 2, sealHash(t, sealed))

	if v := p.ConfirmSeal(2 * time.Second); v != miner.SealConfirmed {
		t.Fatalf("sole producer's seal verdict = %v, want Confirmed", v)
	}
}

// The loser learns it lost from its own chain: the winner's block importing
// at our height is the rejection notice, no store read required.
func TestSealGateRefusesWhenRivalBlockImports(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// Our window drains normally while we build...
	header := testHeader(2, parent)
	p.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)
	tx := testTx(t, 0)
	p.PublishTx(tx)
	waitDrained(t, p, 5*time.Second)

	// ...the rival's seal lands in the store as we go to seal...
	twin := testHeader(2, parent)
	twin.Extra = []byte("twin")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, twin)

	// ...and its block becomes our chain's block 2.
	chain.canonical[2] = twin.Hash()

	p.SealBlock(blockFor(header, []*types.Transaction{tx}))

	if v := p.ConfirmSeal(2 * time.Second); v != miner.SealRefused {
		t.Fatalf("verdict = %v, want Refused: broadcasting would fork an "+
			"already-decided height", v)
	}

	// The refused flush must not have stomped the winner's seal: one sealed
	// generation at 2, the rival's.
	sealedGens := 0
	for _, g := range readAllGenerations(t, h) {
		if g.height == 2 && g.sealed {
			sealedGens++
		}
	}

	if sealedGens != 1 {
		t.Fatalf("store holds %d sealed generations at height 2, want the "+
			"winner's alone", sealedGens)
	}
}

// A same-key twin that broadcast our exact block is a confirmation, not a
// refusal: the block on chain IS ours, and re-broadcasting is harmless.
func TestSealGateConfirmsWhenTwinShippedOurBlock(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	block := gateBlock(t, p, h, 2, sealHash(t, sealed))
	chain.canonical[2] = block.Hash()

	if v := p.ConfirmSeal(2 * time.Second); v != miner.SealConfirmed {
		t.Fatalf("verdict = %v, want Confirmed for our own canonical block", v)
	}
}

// A foreign generation that sealed our height rejects our own seal, and the
// rejection is the answer: this height belongs to someone else, and the next
// build recovers its content. Broadcasting anyway put two blocks at one
// height on a devnet, and the milestone vote then displaced 38 preconfirmed
// transactions into the following block. The held flush must also not have
// superseded the winner meanwhile.
func TestSealGateRefusesWhenOurSealIsRejected(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	header := testHeader(2, parent)
	p.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)
	tx := testTx(t, 0)
	p.PublishTx(tx)
	waitDrained(t, p, 5*time.Second)

	twin := testHeader(2, parent)
	twin.Extra = []byte("twin")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, twin)

	// No canonical block at 2: the chain has not decided.
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))

	start := time.Now()
	if v := p.ConfirmSeal(sealGateBudget); v != miner.SealRefused {
		t.Fatalf("verdict = %v, want Refused: the store rejected our seal "+
			"for this height", v)
	}

	if time.Since(start) > 2*time.Second {
		t.Fatal("the gate overstayed its budget")
	}

	sealedGens := 0
	for _, g := range readAllGenerations(t, h) {
		if g.height == 2 && g.sealed {
			sealedGens++
		}
	}

	if sealedGens != 1 {
		t.Fatalf("the unbroadcast flush superseded a possible winner: %d "+
			"sealed generations at height 2", sealedGens)
	}
}

// A store that is down cannot delay production more than the budget.
func TestSealGateUnknownWhenStoreDown(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	h.stop()

	header := testHeader(2, parent)
	p.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)
	p.PublishTx(testTx(t, 0))
	p.SealBlock(blockFor(header, []*types.Transaction{testTx(t, 0)}))

	start := time.Now()
	if v := p.ConfirmSeal(300 * time.Millisecond); v != miner.SealUnknown {
		t.Fatalf("verdict = %v, want Unknown with the store down", v)
	}

	if time.Since(start) > 2*time.Second {
		t.Fatal("a dead store gated production past the budget")
	}
}

// A height sealed in the store whose block the chain already has is an
// ordinary loss: the build mutes. Should the worker seal there anyway (a
// build already in flight when the winner landed), the flush must not stomp
// the store's standing seal.
func TestSealGateRefusesALostHeightAndStompsNothing(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	twin := testHeader(2, parent)
	twin.Extra = []byte("twin")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, twin)
	chain.canonical[2] = twin.Hash()

	if p.AdoptWindow(2, parent) != nil {
		t.Fatal("a sealed height the chain already has offered an adoption")
	}

	header := testHeader(2, parent)
	header.Extra = []byte("ours")
	p.SealBlock(blockFor(header, []*types.Transaction{testTx(t, 0)}))

	if v := p.ConfirmSeal(300 * time.Millisecond); v != miner.SealRefused {
		t.Fatalf("verdict = %v, want Refused: the chain already holds "+
			"another block at this height", v)
	}

	sealedGens := 0
	for _, g := range readAllGenerations(t, h) {
		if g.height == 2 && g.sealed {
			sealedGens++
		}
	}

	if sealedGens != 1 {
		t.Fatalf("the held flush superseded the standing seal: %d sealed "+
			"generations at height 2", sealedGens)
	}
}

// The upgraded twin property: with the gate, one height gets ONE broadcast.
// The winner confirms; the loser is refused the moment the winner's block
// is on its chain.
func TestTwinsNeverBothBroadcast(t *testing.T) {
	h, a, b := twinPublishers(t)
	chainA := &fakeChain{canonical: map[uint64]common.Hash{}}
	chainB := &fakeChain{canonical: map[uint64]common.Hash{}}

	a.mu.Lock()
	a.chain = chainA
	a.mu.Unlock()
	b.mu.Lock()
	b.chain = chainB
	b.mu.Unlock()

	parent := sealedParent(t, h, a, b)

	blockA := gateBlock(t, a, h, 2, parent)

	// B builds divergent content for the same height and seals it too.
	headerB := testHeader(2, parent)
	headerB.Extra = []byte("b")
	b.OpenBlock(2, headerB.Time, parent, headerB.GasLimit, headerB.BaseFee)
	b.PublishTx(testTx(t, 7))
	blockB := blockFor(headerB, []*types.Transaction{testTx(t, 7)})
	b.SealBlock(blockB)

	// Consensus: A's block wins on both chains.
	chainA.canonical[2] = blockA.Hash()
	chainB.canonical[2] = blockA.Hash()

	va := a.ConfirmSeal(2 * time.Second)
	vb := b.ConfirmSeal(2 * time.Second)

	broadcasts := 0
	if va != miner.SealRefused {
		broadcasts++
	}

	if vb != miner.SealRefused {
		broadcasts++
	}

	if broadcasts != 1 {
		t.Fatalf("verdicts A=%v B=%v: exactly one twin may broadcast", va, vb)
	}
}

// The commitment hash is how anyone learns exactly what the store holds: to
// publish at all, a producer must chain onto the current head, and that head
// encodes every seal before it. So a flush that would land after a foreign
// seal at its own height cannot claim ignorance of it — and must not publish
// a second sealed generation there while its own block is unbroadcast.
//
// This is the 764 defect: the losing twin reconciled onto a head that already
// carried the winner's seal, then republished its own window and seal on top,
// leaving the store's newest view of the height pointing at a block that
// never existed on any chain.
func TestFlushNeverRepublishesOverAKnownSeal(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// We build height 2 while it is genuinely clean, as the loser did.
	header := testHeader(2, parent)
	p.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)
	tx := testTx(t, 0)
	p.PublishTx(tx)
	waitDrained(t, p, 5*time.Second)

	// The rival's complete generation lands first, closing the height.
	twin := testHeader(2, parent)
	twin.Extra = []byte("twin")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, twin)

	// Our flush composes now, and the chain has not decided yet — the exact
	// window in which the old canonicality-conditioned guard let a refold
	// through.
	p.SealBlock(blockFor(header, []*types.Transaction{tx}))

	if v := p.ConfirmSeal(500 * time.Millisecond); v == miner.SealConfirmed {
		t.Fatalf("verdict = %v: the store had already closed this height", v)
	}

	// Give any reconcile a chance to act before auditing.
	time.Sleep(500 * time.Millisecond)

	sealedGens := 0
	for _, g := range readAllGenerations(t, h) {
		if g.height == 2 && g.sealed {
			sealedGens++
		}
	}

	if sealedGens != 1 {
		t.Fatalf("store holds %d sealed generations at height 2: a producer "+
			"that could see the seal published over it anyway", sealedGens)
	}
}

// A producer that got its seal acked and then died leaves the height closed
// in the store and empty on the chain. Muting there strands the height
// forever; building fresh content there would orphan every record the dead
// producer already had acked. Recovery does neither: it rebuilds that exact
// prefix and publishes nothing, because the store already holds the whole
// generation.
func TestPhantomSealRecoversTheExactPrefix(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// A producer sealed height 2 in the store with three records...
	t0, t1, t2 := testTx(t, 0), testTx(t, 1), testTx(t, 2)
	appendForeignOpen(t, h, 2, parent)

	for _, tx := range []*types.Transaction{t0, t1, t2} {
		appendForeignRecord(t, h, tx)
	}

	dead := testHeader(2, parent)
	appendForeignSeal(t, h, dead)

	// ...and died: the chain has no block at 2.
	entriesBefore := len(readAllGenerations(t, h))

	w := p.AdoptWindow(2, parent)
	if w == nil {
		t.Fatal("a height sealed in the store but absent from the chain was " +
			"muted: nobody rebuilds it and it is stranded forever")
	}

	if len(w.Txs) != 3 {
		t.Fatalf("recovered %d txs, want the dead producer's 3", len(w.Txs))
	}

	for i, want := range []*types.Transaction{t0, t1, t2} {
		if w.Txs[i].Hash() != want.Hash() {
			t.Fatalf("recovered tx %d differs: rebuilding here would orphan "+
				"the records the dead producer already had acked", i)
		}
	}

	if w.Timestamp != dead.Time || w.ParentHash != parent {
		t.Fatal("recovery did not inherit the sealed open context")
	}

	// The store already holds the generation: the rebuild must add nothing.
	p.OpenBlock(2, w.Timestamp, parent, w.GasLimit, w.BaseFee)
	p.PublishTx(t0)
	time.Sleep(300 * time.Millisecond)

	if got := len(readAllGenerations(t, h)); got != entriesBefore {
		t.Fatalf("recovery republished into the store: %d generations, want %d",
			got, entriesBefore)
	}

	// Nor at the seal: the store already holds this generation's seal, so a
	// second one would open a second generation over a sealed height.
	p.SealBlock(types.NewBlockWithHeader(dead))
	time.Sleep(300 * time.Millisecond)

	if got := len(readAllGenerations(t, h)); got != entriesBefore {
		t.Fatalf("recovery re-sealed into the store: %d generations, want %d",
			got, entriesBefore)
	}

	// And the block must reach the chain: a gate refusal here would strand
	// the height a second time.
	if v := p.ConfirmSeal(200 * time.Millisecond); v == miner.SealRefused {
		t.Fatal("the recovery block was refused: the height stays stranded")
	}
}

// The ordinary loss must not be mistaken for a phantom: when the chain does
// hold a block at the height, there is nothing to recover and the build mutes.
func TestSealedHeightWithChainBlockStillMutes(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 0))

	winner := testHeader(2, parent)
	appendForeignSeal(t, h, winner)

	// The winner's block is on our chain: an ordinary loss.
	chain.canonical[2] = winner.Hash()

	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatalf("recovered a height the chain already has: %+v", w)
	}
}

// A seal only moments old may simply be in flight. Rebuilding for it that
// early races the real broadcast, so the grace has to elapse first — and
// until it does, a build carrying different content must not go out.
func TestFreshSealIsWaitedForNotRebuilt(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	now := uint64(time.Now().Unix())
	appendForeignOpenAt(t, h, 2, parent, now)
	appendForeignRecord(t, h, testTx(t, 0))

	fresh := testHeader(2, parent)
	fresh.Time = now
	appendForeignSeal(t, h, fresh)

	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatal("rebuilt a seal that is still within its broadcast grace: " +
			"this races the block already on its way")
	}

	// The build proceeds anyway (mute does not stop the miner), so the gate
	// is the last line: divergent content over a standing seal must not
	// broadcast just because the height is undecided.
	ours := testHeader(2, parent)
	ours.Extra = []byte("ours")
	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 9)}))

	if v := p.ConfirmSeal(300 * time.Millisecond); v != miner.SealRefused {
		t.Fatalf("verdict = %v, want Refused: broadcasting here forks the "+
			"height away from the sequenced content", v)
	}

	// And it published nothing: the store's generation stands alone.
	sealedGens := 0

	for _, g := range readAllGenerations(t, h) {
		if g.height == 2 && g.sealed {
			sealedGens++
		}
	}

	if sealedGens != 1 {
		t.Fatalf("%d sealed generations at height 2, want the store's one",
			sealedGens)
	}
}

// Once the grace has passed with no block, the same height is rebuilt from
// the store rather than left stranded.
func TestStaleSealIsRebuiltAfterGrace(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	past := uint64(time.Now().Add(-2 * recoverGrace).Unix())
	appendForeignOpenAt(t, h, 2, parent, past)
	appendForeignRecord(t, h, testTx(t, 0))

	dead := testHeader(2, parent)
	dead.Time = past
	appendForeignSeal(t, h, dead)

	if p.AdoptWindow(2, parent) == nil {
		t.Fatal("a seal past its grace with no block was not rebuilt")
	}
}

// A consumer whose per-block fetch fails while the tail still reads: the
// store says the height is sealed, but its content cannot be recovered.
type unfetchableConsumer struct {
	pb.ConsumerServiceClient
}

func (c *unfetchableConsumer) GetBlock(ctx context.Context, req *pb.GetBlockRequest,
	opts ...grpc.CallOption,
) (*pb.GetBlockResponse, error) {
	return nil, status.Error(codes.Unavailable, "block fetch unavailable")
}

// Refusing a height is only safe while the rebuild that resolves it is still
// coming. If recovery can never reconstruct the height, refusing every build
// there would halt the chain at that block forever — so an unrecoverable
// sealed height falls back to liveness.
func TestUnrecoverableSealedHeightStillBroadcasts(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	past := uint64(time.Now().Add(-2 * recoverGrace).Unix())
	appendForeignOpenAt(t, h, 2, parent, past)
	appendForeignRecord(t, h, testTx(t, 0))

	lost := testHeader(2, parent)
	lost.Time = past
	appendForeignSeal(t, h, lost)

	p.mu.Lock()
	p.read.cons = &unfetchableConsumer{ConsumerServiceClient: p.read.cons}
	p.mu.Unlock()

	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatal("recovered a height whose content could not be fetched")
	}

	ours := testHeader(2, parent)
	ours.Extra = []byte("ours")
	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 9)}))

	if v := p.ConfirmSeal(300 * time.Millisecond); v == miner.SealRefused {
		t.Fatal("refused an unrecoverable height: every build here refuses " +
			"the same way and the chain never gets past this block")
	}
}

// The store rejecting our seal is a verdict, not slowness: another
// producer's window owns the height. Riding the liveness timeout out to a
// broadcast after that is the double broadcast the gate exists to prevent.
func TestRejectedSealRefusesInsteadOfTimingOutToBroadcast(t *testing.T) {
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
	p.SealBlock(blockFor(ours, []*types.Transaction{testTx(t, 0)}))

	// Entries that are not this block's seal must not decide the gate: a
	// refusal has to name our seal at our height, or an unrelated rejection
	// would withhold a block nobody else is producing.
	p.markGateLost(journalItem{kind: entryRecord, height: 2})
	p.markGateLost(journalItem{kind: entrySeal, height: 3})

	p.mu.Lock()
	spurious := p.gate.verdict == gateLost
	p.mu.Unlock()

	if spurious {
		t.Fatal("an unrelated rejection lost the gate")
	}

	p.markGateLost(journalItem{kind: entrySeal, height: 2})

	start := time.Now()

	if v := p.ConfirmSeal(sealGateBudget); v != miner.SealRefused {
		t.Fatalf("verdict = %v, want Refused: the store rejected this "+
			"block's seal", v)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v to refuse a seal the store already rejected: a "+
			"known verdict must not wait out the liveness budget", elapsed)
	}
}

// A refused block and its rebuild share a height, and the gate must confirm
// only on the gated block's own seal. Two layers enforce it: retire's
// lineage guard (a different block folds differently, so a stale ack fails
// the byte match) and the gate's hash key. Each is exercised here through a
// journal the item genuinely stands in.
func TestLateAckForAnotherSealDoesNotConfirmTheGate(t *testing.T) {
	first := testHeader(2, common.Hash{0xaa})
	rebuild := testHeader(2, common.Hash{0xaa})
	rebuild.Extra = []byte("rebuild")

	seal := func(h *types.Header) ([]byte, *pb.Entry) {
		raw, err := rlp.EncodeToBytes(h)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		return raw, sealEntry(raw, commitment.Head{})
	}

	ack := func(sealed *types.Header, gated common.Hash) gateVerdict {
		p := barePublisher()

		_, entry := seal(sealed)
		post := commitment.Head{0x99}

		p.mu.Lock()
		p.journal.append(entry, commitment.Head{}, post, entrySeal, 2, 0, nil)
		item := p.journal.items[len(p.journal.items)-1]
		p.gate = sealGate{height: 2, hash: gated}
		p.mu.Unlock()

		p.retire(item, time.Now())

		p.mu.Lock()
		defer p.mu.Unlock()

		return p.gate.verdict
	}

	if v := ack(first, rebuild.Hash()); v == gateConfirmed {
		t.Fatal("a late ack for a different block's seal confirmed the gate")
	}

	if v := ack(rebuild, rebuild.Hash()); v != gateConfirmed {
		t.Fatalf("the gated block's own seal ack did not confirm (verdict %d)", v)
	}
}

// A flush withheld while its gate is pending is the verdict, not a wait for
// one: the block is unbroadcast, so the canonical proof the withhold wants
// can never arrive. The refusal must also unwedge the rebuild — dead flush
// dropped, sealed tip rolled back, hold cleared — or every later build at
// this height orders itself behind a flush that cannot resolve. The
// liveness fallback's tolerance covers only the store's seal, so a foreign
// live window refuses a tolerant gate all the same.
func TestWithheldFlushRefusesThePendingGate(t *testing.T) {
	for _, tolerate := range []bool{false, true} {
		name := "plain gate"
		if tolerate {
			name = "tolerant gate"
		}

		t.Run(name, func(t *testing.T) {
			fc := &fakeChain{}
			p, _ := lineagePublisher(t, fc)

			h1 := testHeader(1, common.Hash{0xef})
			header := testHeader(2, h1.Hash())
			p.SealBlock(blockFor(header, []*types.Transaction{testTx(t, 1)}))

			p.mu.Lock()
			p.gate.tolerateSealed = tolerate
			p.mu.Unlock()

			foreign := openEntry(commitment.OpenContext{
				Number:     2,
				Timestamp:  header.Time + 7,
				ParentHash: h1.Hash(),
				GasLimit:   header.GasLimit,
				BaseFee:    header.BaseFee,
			}, commitment.Head{0x66})

			info := tailInfo{
				s:             commitment.Head{0x66},
				tipOpen:       true,
				tipOpenHeight: 2,
				window:        []*pb.Entry{foreign},
			}

			if out := p.applyTail(info); out != recOK {
				t.Fatalf("outcome = %v", out)
			}

			if v := p.ConfirmSeal(sealGateBudget); v != miner.SealRefused {
				t.Fatalf("verdict = %v, want refused", v)
			}

			p.mu.Lock()
			defer p.mu.Unlock()

			if items, covered := p.journal.after(p.ackedSeq); !covered || len(items) != 0 {
				t.Fatalf("the dead flush survived the refusal (covered=%v len=%d)", covered, len(items))
			}

			if p.sealedTip != 1 {
				t.Fatalf("sealedTip = %d, want 1: the rebuild must adopt here, not mute", p.sealedTip)
			}

			if p.hold.active() {
				t.Fatal("the withheld flush's hold outlived the flush")
			}
		})
	}
}

// A decoded store seal at the gated height settles the gate from
// classification: ours confirms (a twin delivered our copy), foreign
// refuses and drops the flush.
func TestStoreSealAtGateHeightSettlesTheGate(t *testing.T) {
	h1 := testHeader(1, common.Hash{0xef})

	cases := []struct {
		name    string
		sealed  func(ours common.Hash) common.Hash
		want    miner.SealVerdict
		dropped bool
	}{
		{
			name:    "foreign seal refuses",
			sealed:  func(common.Hash) common.Hash { return common.Hash{0xdd} },
			want:    miner.SealRefused,
			dropped: true,
		},
		{
			name:   "our own seal confirms",
			sealed: func(ours common.Hash) common.Hash { return ours },
			want:   miner.SealConfirmed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeChain{}
			p, _ := lineagePublisher(t, fc)

			header := testHeader(2, h1.Hash())
			p.SealBlock(blockFor(header, nil))

			info := tailInfo{
				s:              commitment.Head{0x55},
				haveSeal:       true,
				sealDecoded:    true,
				lastSealHeight: 2,
				lastSealHash:   tc.sealed(header.Hash()),
			}

			if out := p.applyTail(info); out != recOK {
				t.Fatalf("outcome = %v", out)
			}

			if v := p.ConfirmSeal(sealGateBudget); v != tc.want {
				t.Fatalf("verdict = %v, want %v", v, tc.want)
			}

			p.mu.Lock()
			defer p.mu.Unlock()

			items, covered := p.journal.after(p.ackedSeq)
			if gone := covered && len(items) == 0; gone != tc.dropped {
				t.Fatalf("flush dropped = %v, want %v", gone, tc.dropped)
			}
		})
	}
}

// A STALE while the gate is pending marks it contested, and a contested
// gate outlives the uncontested budget: the reconcile loop is producing the
// verdict, and cutting the wait short is what broadcast an empty block
// three seconds before its refusal would have arrived.
func TestContestedGateWaitsPastTheUncontestedBudget(t *testing.T) {
	fc := &fakeChain{}
	p, _ := lineagePublisher(t, fc)

	h1 := testHeader(1, common.Hash{0xef})
	p.SealBlock(blockFor(testHeader(2, h1.Hash()), nil))

	// A record of the flush STALEd: contest, not verdict.
	p.markGateLost(journalItem{kind: entryRecord, height: 2})

	verdicts := make(chan miner.SealVerdict, 1)

	go func() { verdicts <- p.ConfirmSeal(30 * time.Millisecond) }()

	select {
	case v := <-verdicts:
		t.Fatalf("gate resolved %v inside the uncontested budget; the contest must extend the wait", v)
	case <-time.After(150 * time.Millisecond):
	}

	// The seal's own STALE is the verdict.
	p.markGateLost(journalItem{kind: entrySeal, height: 2})

	select {
	case v := <-verdicts:
		if v != miner.SealRefused {
			t.Fatalf("verdict = %v, want refused", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("contested gate never consumed the late verdict")
	}
}

// The last look before a verdictless broadcast: silence from the gate is
// not license to bury what the store accepted while we waited.
func TestGateTimeoutRecheck(t *testing.T) {
	cases := []struct {
		name     string
		tolerate bool
		ownSeal  bool
		record   bool
		covered  bool
		want     miner.SealVerdict
	}{
		{name: "foreign seal refuses", want: miner.SealRefused},
		{name: "our own seal in the store confirms", ownSeal: true, want: miner.SealConfirmed},
		{name: "acked records the block lacks refuse", record: true, want: miner.SealRefused},
		{name: "a window the block carries broadcasts", record: true, covered: true, want: miner.SealUnknown},
		{name: "the liveness fallback tolerates the foreign seal", tolerate: true, want: miner.SealUnknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			p := newTestPublisher(t, h, nil)

			sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
			waitHead(t, h, p, 5*time.Second)
			parent := sealHash(t, sealed)

			gated := testHeader(2, parent)
			tx := testTx(t, 3)

			appendForeignOpen(t, h, 2, parent)

			switch {
			case tc.record:
				appendForeignRecord(t, h, tx)
			case tc.ownSeal:
				appendForeignSeal(t, h, gated)
			default:
				lost := testHeader(2, parent)
				lost.Extra = []byte("foreign")
				appendForeignSeal(t, h, lost)
			}

			var txs []common.Hash
			if tc.covered {
				txs = []common.Hash{tx.Hash()}
			}

			p.mu.Lock()
			p.gate = sealGate{height: 2, hash: gated.Hash(), txs: txs, tolerateSealed: tc.tolerate}
			p.mu.Unlock()

			if v := p.ConfirmSeal(30 * time.Millisecond); v != tc.want {
				t.Fatalf("verdict = %v, want %v", v, tc.want)
			}
		})
	}
}

// A foreign live window above an unproven flush withholds the re-anchor:
// folding past it is how two full acked generations were buried under an
// empty sealed block. Only the chain ratifying our flush licenses the move,
// and while the gate is pending the withhold doubles as the refusal.
func TestFlushWithholdsUnderAForeignWindowAbove(t *testing.T) {
	fc := &fakeChain{}
	p, _ := lineagePublisher(t, fc)

	h1 := testHeader(1, common.Hash{0xef})
	header := testHeader(2, h1.Hash())
	p.SealBlock(blockFor(header, []*types.Transaction{testTx(t, 1)}))

	foreign := openEntry(commitment.OpenContext{
		Number:     3,
		Timestamp:  header.Time + 9,
		ParentHash: common.Hash{0x77},
		GasLimit:   header.GasLimit,
		BaseFee:    header.BaseFee,
	}, commitment.Head{0x88})

	info := tailInfo{
		s:              commitment.Head{0x88},
		tipOpen:        true,
		tipOpenHeight:  3,
		window:         []*pb.Entry{foreign},
		haveSeal:       true,
		lastSealHeight: 1,
		lastSealHash:   common.Hash{0xaa},
	}

	journalBefore := len(p.journal.items)

	if out := p.applyTail(info); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.hold.kind != holdSticky {
		t.Fatal("flush over a foreign window above was not withheld")
	}

	if len(p.journal.items) != journalBefore {
		t.Fatal("withhold must not touch the lineage")
	}

	if p.gate.verdict != gateLost {
		t.Fatalf("gate verdict = %d, want lost: the withhold is the verdict", p.gate.verdict)
	}
}

// A foreign seal standing in the store must not refuse the elected producer
// when consensus rejects that seal's signer. The devnet incident shape: a
// producer rotated out mid-span kept sealing, its network-rejected block was
// flushed to the store as sealed truth, and the rotated-in producer's valid
// block was then discarded against it while the chain sat frozen a height
// below. With the engine's verdict wired in, the gate treats such a seal as
// noise and broadcasts.
func TestSealGateBroadcastsPastConsensusInvalidSeal(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// The rotated-out producer's generation: sealed in the store, rejected
	// by every chain ("signer is not a part of the producer set").
	foreign := testHeader(2, parent)
	foreign.Extra = []byte("rotated-out producer")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, foreign)

	rejected := foreign.Hash()
	p.SetSealVerifier(func(header *types.Header) error {
		if header.Hash() == rejected {
			return errUnauthorizedTestSigner
		}

		return nil
	})

	// The elected producer sealed blind: the incident's build-start read had
	// failed, so nothing was published and the gate holds an unpublished
	// seal awaiting its verdict.
	tx := testTx(t, 0)
	ours := blockFor(testHeader(2, parent), []*types.Transaction{tx})

	p.mu.Lock()
	p.gate = sealGate{height: 2, hash: ours.Hash(), txs: []common.Hash{tx.Hash()}}
	p.mu.Unlock()

	if v := p.ConfirmSeal(300 * time.Millisecond); v != miner.SealUnknown {
		t.Fatalf("verdict = %v, want Unknown (broadcast): the store's seal is "+
			"consensus-invalid and can never become canonical", v)
	}
}

// The consensus check must not weaken the honest race: a foreign store seal
// the engine accepts keeps its refusal — the winner's block may simply be in
// flight, and broadcasting beside it is the double-broadcast the gate exists
// to prevent. A publisher with no verifier wired behaves the same.
func TestSealGateStillRefusesConsensusValidSeal(t *testing.T) {
	cases := []struct {
		name     string
		verifier func(*types.Header) error
	}{
		{name: "engine accepts the seal", verifier: func(*types.Header) error { return nil }},
		{name: "no verifier wired", verifier: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			chain := &fakeChain{canonical: map[uint64]common.Hash{}}
			p := newTestPublisher(t, h, chain)

			sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
			waitHead(t, h, p, 5*time.Second)
			parent := sealHash(t, sealed)

			foreign := testHeader(2, parent)
			foreign.Extra = []byte("in-flight winner")
			appendForeignOpen(t, h, 2, parent)
			appendForeignSeal(t, h, foreign)

			if tc.verifier != nil {
				p.SetSealVerifier(tc.verifier)
			}

			tx := testTx(t, 0)
			ours := blockFor(testHeader(2, parent), []*types.Transaction{tx})

			p.mu.Lock()
			p.gate = sealGate{height: 2, hash: ours.Hash(), txs: []common.Hash{tx.Hash()}}
			p.mu.Unlock()

			if v := p.ConfirmSeal(300 * time.Millisecond); v != miner.SealRefused {
				t.Fatalf("verdict = %v, want Refused: the winner's block may still arrive", v)
			}
		})
	}
}

// The gateLost path — a tail read resolving the gate from a decoded foreign
// seal — honors the same consensus verdict: the exact flow of
// TestSealGateRefusesWhenOurSealIsRejected, but the store's seal is from a
// signer the engine rejects, so our block broadcasts instead.
func TestSealGateLostVerdictIgnoresConsensusInvalidSeal(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	header := testHeader(2, parent)
	p.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)
	tx := testTx(t, 0)
	p.PublishTx(tx)
	waitDrained(t, p, 5*time.Second)

	rotated := testHeader(2, parent)
	rotated.Extra = []byte("rotated-out producer")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, rotated)

	rejected := rotated.Hash()
	p.SetSealVerifier(func(h *types.Header) error {
		if h.Hash() == rejected {
			return errUnauthorizedTestSigner
		}

		return nil
	})

	p.SealBlock(blockFor(header, []*types.Transaction{tx}))

	if v := p.ConfirmSeal(sealGateBudget); v == miner.SealRefused {
		t.Fatal("verdict = Refused: a consensus-invalid store seal must not " +
			"refuse the elected producer's block")
	}
}

// The engine's seal verification waits on heimdall availability with no
// deadline of its own, and the gate runs on the miner's result loop — a
// verifier that never returns must cost the gate at most its bounded
// timeout, and an unverifiable seal keeps its refusal. Without the bound,
// a heimdall outage coinciding with a lost gate froze block import.
func TestSealGateBoundsBlockedSealVerifier(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	foreign := testHeader(2, parent)
	foreign.Extra = []byte("unverifiable while heimdall is down")
	appendForeignOpen(t, h, 2, parent)
	appendForeignSeal(t, h, foreign)

	// Heimdall down: verification never returns.
	p.SetSealVerifier(func(*types.Header) error {
		select {} // blocks forever
	})

	tx := testTx(t, 0)
	ours := blockFor(testHeader(2, parent), []*types.Transaction{tx})

	p.mu.Lock()
	p.gate = sealGate{height: 2, hash: ours.Hash(), txs: []common.Hash{tx.Hash()}}
	p.mu.Unlock()

	start := time.Now()

	if v := p.ConfirmSeal(300 * time.Millisecond); v != miner.SealRefused {
		t.Fatalf("verdict = %v, want Refused: an unverifiable seal keeps its refusal", v)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("gate took %v against a blocked verifier: the result loop would freeze", elapsed)
	}
}
