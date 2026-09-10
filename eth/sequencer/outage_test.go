package sequencer

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// outageChain builds a hash-linked chain the publisher can backfill from: the
// chain database is the archive during an outage, so the fake must serve
// complete blocks by number.
func outageChain(t *testing.T, txsPer int, through uint64) (*fakeChain, []*types.Block) {
	t.Helper()

	chain := &fakeChain{
		canonical: map[uint64]common.Hash{},
		known:     map[common.Hash]*types.Header{},
		blocks:    map[uint64]*types.Block{},
	}

	parent := common.Hash{0xef}
	blocks := make([]*types.Block, 0, through)

	for n := uint64(1); n <= through; n++ {
		header := testHeader(n, parent)

		var txs []*types.Transaction
		for i := 0; i < txsPer; i++ {
			txs = append(txs, testTx(t, uint64(i)))
		}

		block := blockFor(header, txs)
		chain.blocks[n] = block
		chain.canonical[n] = block.Hash()
		chain.known[block.Hash()] = block.Header()
		chain.current = block.Header()
		blocks = append(blocks, block)
		parent = block.Hash()
	}

	return chain, blocks
}

// sealThrough runs the producer's publishing lifecycle over the given blocks:
// open, records, seal — as the worker would during the outage.
func sealThrough(p *Publisher, blocks []*types.Block) {
	for _, b := range blocks {
		h := b.Header()
		p.OpenBlock(h.Number.Uint64(), h.Time, h.ParentHash, h.GasLimit, h.BaseFee)

		for _, tx := range b.Transactions() {
			p.PublishTx(tx)
		}

		p.SealBlock(b)
	}
}

// The outage contract: the chain never stops for the store, and when the
// store returns, the producer backfills every missing block — oldest first,
// so a reader never sees the sealed tip jump a gap it would then refuse to
// fill. Nothing during the outage was acked, so nothing was promised, and
// the audit must come back clean.
func TestOutageBackfillsGaplessInOrder(t *testing.T) {
	h := startHarness(t)

	const through = 12

	chain, blocks := outageChain(t, 2, through)
	p := newTestPublisher(t, h, chain)

	// Block 1 lands normally; the store then goes down.
	sealThrough(p, blocks[:1])
	waitHead(t, h, p, 5*time.Second)

	h.stop()

	// The chain keeps producing through the outage. No ack can arrive, so
	// no preconfirmation is issued for any of this.
	sealThrough(p, blocks[1:])

	h.resume()

	// The store converges to the tip: every block sealed, oldest first.
	waitFor(t, 20*time.Second, func() bool {
		sealed := 0

		for _, g := range readAllGenerations(t, h) {
			if g.sealed {
				sealed++
			}
		}

		return sealed >= through
	})

	gens := readAllGenerations(t, h)

	last := uint64(0)
	for _, g := range gens {
		if g.height < last {
			t.Fatalf("backfill wrote height %d after height %d: out of order, "+
				"the sealed tip jumped a gap", g.height, last)
		}

		last = g.height
	}

	canonical := map[uint64][]common.Hash{}
	for _, b := range blocks {
		hashes := []common.Hash{}
		for _, tx := range b.Transactions() {
			hashes = append(hashes, tx.Hash())
		}

		canonical[b.NumberU64()] = hashes
	}

	audit := auditStore(t, h, canonical)
	if !audit.clean() {
		t.Fatalf("outage recovery broke promises: %+v", audit)
	}
}

// After catch-up the live stream resumes: a new block opened at the tip
// publishes normally, gated behind nothing.
func TestLiveStreamResumesAfterCatchUp(t *testing.T) {
	h := startHarness(t)

	const through = 6

	chain, blocks := outageChain(t, 1, through)
	p := newTestPublisher(t, h, chain)

	sealThrough(p, blocks[:1])
	waitHead(t, h, p, 5*time.Second)

	h.stop()
	sealThrough(p, blocks[1:])
	h.resume()

	waitFor(t, 20*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.pendingFrom == 0 && p.unackedLocked() == 0
	})

	// The next build finds a clean boundary at the tip and streams live.
	tip := blocks[len(blocks)-1]
	if w := p.AdoptWindow(through+1, tip.Hash()); w != nil {
		t.Fatalf("clean boundary after catch-up offered an adoption: %+v", w)
	}

	p.OpenBlock(through+1, tip.Header().Time+2, tip.Hash(), tip.GasLimit(), fee25())
	p.PublishTx(testTx(t, 0))
	waitDrained(t, p, 10*time.Second)
}

// During the outage the barrier must not gate production: the store is
// unreachable, nothing acks, and the chain seals anyway.
func TestBarrierNeverGatesDuringOutage(t *testing.T) {
	h := startHarness(t)

	chain, blocks := outageChain(t, 1, 3)
	p := newTestPublisher(t, h, chain)

	sealThrough(p, blocks[:1])
	waitHead(t, h, p, 5*time.Second)

	h.stop()

	head := blocks[0]
	p.OpenBlock(2, head.Header().Time+2, head.Hash(), head.GasLimit(), fee25())
	p.PublishTx(testTx(t, 0))

	start := time.Now()
	if !awaitOurWindow(p, 300*time.Millisecond) {
		t.Fatal("an unreachable store gated a seal")
	}

	if time.Since(start) > 2*time.Second {
		t.Fatal("the liveness override took too long")
	}
}
