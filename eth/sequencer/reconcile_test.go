package sequencer

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

type fakeChain struct {
	canonical map[uint64]common.Hash
	known     map[common.Hash]*types.Header
	current   *types.Header
	blocks    map[uint64]*types.Block
}

func (f *fakeChain) GetCanonicalHash(number uint64) common.Hash {
	return f.canonical[number]
}

func (f *fakeChain) GetHeaderByHash(hash common.Hash) *types.Header {
	return f.known[hash]
}

func (f *fakeChain) CurrentBlock() *types.Header {
	return f.current
}

func (f *fakeChain) GetBlockByNumber(number uint64) *types.Block {
	return f.blocks[number]
}

// appendForeignOpen appends an open at the store head, as a competing
// publisher would.
func appendForeignOpen(t *testing.T, h *harness, number uint64, parent common.Hash) {
	t.Helper()
	appendForeignOpenAt(t, h, number, parent, 1700000000+number)
}

func appendForeignOpenAt(t *testing.T, h *harness, number uint64, parent common.Hash, ts uint64) {
	t.Helper()

	entry := &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
		BlockNumber:      number,
		BlockTimestamp:   ts,
		ParentHash:       parent.Bytes(),
		GasLimit:         30_000_000,
		BaseFee:          big25gwei(),
		PrefixCommitment: h.store.Head().Bytes(),
	}}}

	if status := h.store.Append(entry); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("foreign open rejected: %v", status)
	}
}

func appendForeignSeal(t *testing.T, h *harness, header *types.Header) {
	t.Helper()

	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatalf("rlp: %v", err)
	}

	entry := &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{
		Header:           raw,
		PrefixCommitment: h.store.Head().Bytes(),
	}}}

	if status := h.store.Append(entry); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("foreign seal rejected: %v", status)
	}
}

func big25gwei() []byte {
	return testHeader(0, common.Hash{}).BaseFee.Bytes()
}

// A foreign open landing mid-block holds our publishing (no re-anchor over
// unsealed work); our seal flush then overrides it — the only supersede.
func TestSealFlushOverridesForeignWindow(t *testing.T) {
	h := startHarness(t)
	fc := &fakeChain{}
	p := newTestPublisher(t, h, fc)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// Our window for block 2, confirmed.
	header2 := testHeader(2, sealHash(t, sealed))
	p.OpenBlock(2, header2.Time, header2.ParentHash, header2.GasLimit, header2.BaseFee)
	tx0 := testTx(t, 0)
	p.PublishTx(tx0)
	waitHead(t, h, p, 5*time.Second)

	// A competing publisher supersedes our window with its own block 2.
	appendForeignOpen(t, h, 2, common.Hash{0xaa})
	foreignHead := h.store.Head()

	// Our next record STALEs; the publisher holds instead of re-anchoring.
	tx1 := testTx(t, 1)
	p.PublishTx(tx1)

	waitFor(t, 5*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.hold.after != noHold
	})

	if h.store.Head() != foreignHead {
		t.Fatal("held publisher wrote to the store")
	}

	// The seal flush re-anchors: the sealed block overrides the window.
	sealOnChain(p, fc, header2, []*types.Transaction{tx0, tx1})
	waitHead(t, h, p, 10*time.Second)

	resp, err := h.store.GetBlock(context.Background(), &pb.GetBlockRequest{BlockNumber: 2})
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}

	open := resp.GetEntries()[0].GetBlockOpen()
	if got := common.BytesToHash(open.GetParentHash()); got != sealHash(t, sealed) {
		t.Fatalf("latest generation parent %x, want ours %x", got, sealHash(t, sealed))
	}

	if last := resp.GetEntries()[len(resp.GetEntries())-1]; last.GetBlockSeal() == nil {
		t.Fatal("flushed generation is not sealed")
	}
}

// A canonically sealed foreign block at our pending height holds our stale
// build; the next build-start check rebases onto the store head and
// publishing resumes cleanly.
func TestForeignSealedHoldsThenRebases(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{canonical: map[uint64]common.Hash{}, known: map[common.Hash]*types.Header{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	p.OpenBlock(2, 1700000002, sealHash(t, sealed), 30_000_000, testHeader(2, common.Hash{}).BaseFee)
	waitHead(t, h, p, 5*time.Second)

	// The real producer's block 2 lands in the store and on our chain.
	foreign := testHeader(2, common.Hash{0xbb})
	appendForeignOpen(t, h, 2, common.Hash{0xbb})
	appendForeignSeal(t, h, foreign)
	chain.canonical[2] = foreign.Hash()

	// Our stale-window record STALEs; the publisher holds.
	p.PublishTx(testTx(t, 0))

	waitFor(t, 10*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.hold.after != noHold
	})

	// Next build: the check finds a clean (sealed) tail and rebases.
	if w := p.AdoptWindow(3, foreign.Hash()); w != nil {
		t.Fatalf("clean tail returned a window: %+v", w)
	}

	publishBlock(t, p, 3, foreign.Hash(), 1)
	waitHead(t, h, p, 5*time.Second)
}

// publishRecorded drives one block through the publisher and registers it
// with the fake chain, as block import would.
func publishRecorded(t *testing.T, p *Publisher, chain *fakeChain, number uint64, parent common.Hash, txs int) *types.Header {
	t.Helper()

	header := testHeader(number, parent)
	p.OpenBlock(number, header.Time, parent, header.GasLimit, header.BaseFee)

	var body []*types.Transaction

	for i := 0; i < txs; i++ {
		tx := testTx(t, uint64(i))
		body = append(body, tx)
		p.PublishTx(tx)
	}

	block := blockFor(header, body)
	chain.blocks[number] = block
	p.SealBlock(block)

	return header
}

// An outage that stacks past the hot-flush bound collapses older blocks out
// of the journal; on recovery they are rebuilt from the chain database and
// every height still delivers to the store.
func TestOutageBackfillsFromDB(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishRecorded(t, p, chain, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	h.stop()

	last := uint64(journalHotSeals + 10)
	parent := sealHash(t, sealed)
	for n := uint64(2); n <= last; n++ {
		parent = sealHash(t, publishRecorded(t, p, chain, n, parent, 2))
	}

	p.mu.Lock()
	collapsed := p.pendingFrom != 0
	p.mu.Unlock()

	if !collapsed {
		t.Fatal("outage past journalHotSeals must collapse to the pending range")
	}

	h.resume()
	waitHead(t, h, p, 15*time.Second)

	publishRecorded(t, p, chain, last+1, parent, 1)
	waitHead(t, h, p, 15*time.Second)

	// Everything delivered: the collapsed blocks came back from the DB.
	for n := uint64(2); n <= last+1; n++ {
		if _, err := h.store.GetBlock(context.Background(), &pb.GetBlockRequest{BlockNumber: n}); err != nil {
			t.Fatalf("block %d missing after backfill: %v", n, err)
		}
	}
}

// The same outage without chain access (no DB to rebuild from) skips the
// collapsed range as a counted forward jump and resumes at the tip.
func TestOutageChainlessJumps(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	jumps := reconcileForwardJump.Snapshot().Count()

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	h.stop()

	last := uint64(journalHotSeals + 6)
	parent := sealHash(t, sealed)
	for n := uint64(2); n <= last; n++ {
		parent = sealHash(t, publishBlock(t, p, n, parent, 1))
	}

	h.resume()
	waitHead(t, h, p, 15*time.Second)

	publishBlock(t, p, last+1, parent, 1)
	waitHead(t, h, p, 15*time.Second)

	// The collapsed front is gone (no DB to rebuild from) and counted.
	if _, err := h.store.GetBlock(context.Background(), &pb.GetBlockRequest{BlockNumber: 2}); err == nil {
		t.Fatal("collapsed block 2 unexpectedly published without a chain")
	}

	for _, kept := range []uint64{last, last + 1} {
		if _, err := h.store.GetBlock(context.Background(), &pb.GetBlockRequest{BlockNumber: kept}); err != nil {
			t.Fatalf("block %d missing after recovery: %v", kept, err)
		}
	}

	if reconcileForwardJump.Snapshot().Count() == jumps {
		t.Fatal("chainless backfill skip must count a forward jump")
	}
}

// The matcher flags a byte-identical entry folding from a different prefix —
// version skew, which is terminal.
func TestMatcherFoldDivergence(t *testing.T) {
	entry := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{{0x01}},
		PrefixCommitment: commitment.Head{0xaa}.Bytes(),
	}}}

	other := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     [][]byte{{0x01}},
		PrefixCommitment: commitment.Head{0xbb}.Bytes(),
	}}}

	m := &matcher{snap: []journalItem{{entry: entry}}, on: true}
	if err := m.absorb(other); err == nil {
		t.Fatal("divergence not detected")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// A block rebuilt from the chain database folds to exactly the head the
// live build produced — the backfill's byte-fidelity contract.
func TestBackfillByteExact(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishRecorded(t, p, chain, 1, common.Hash{0xef}, 3)
	waitHead(t, h, p, 5*time.Second)

	liveHead := localHead(p)

	// Rebuild block 1 from the DB onto the same base the live build used.
	fresh := newJournal()

	p.mu.Lock()
	cur, ok := p.appendBlockLocked(fresh, commitment.Seed(testChainID), chain.blocks[1])
	p.mu.Unlock()

	if !ok {
		t.Fatal("rebuild failed")
	}

	if cur != liveHead {
		t.Fatalf("rebuilt fold %x, live fold %x", cur, liveHead)
	}

	_ = sealed
}

// The backfill drains oldest-first and completely: a store that was down
// owes its readers every block, in order, so nothing is abandoned for
// freshness — the sealed tip must never advance past a gap it would then
// refuse to fill.
func TestBackfillDrainsOldestFirstAndCompletely(t *testing.T) {
	p := barePublisher()
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p.chain = chain

	parent := common.Hash{0xef}
	for n := uint64(1); n <= 50; n++ {
		header := testHeader(n, parent)
		chain.blocks[n] = blockFor(header, nil)
		parent = header.Hash()
	}

	p.pendingFrom, p.pendingTo = 1, 50
	p.pendingEntries = 100 // open+seal per empty block

	jumps := reconcileForwardJump.Snapshot().Count()
	drops := publishDropMeter.Snapshot().Count()

	fresh := newJournal()

	p.mu.Lock()
	p.backfillLocked(fresh, commitment.Seed(testChainID))
	pendingFrom := p.pendingFrom
	p.mu.Unlock()

	if got := fresh.seals; got != 50 {
		t.Fatalf("rebuilt %d blocks, want all 50", got)
	}

	if fresh.items[0].height != 1 {
		t.Fatalf("rebuild starts at %d, want 1 (oldest first)", fresh.items[0].height)
	}

	if pendingFrom != 0 {
		t.Fatalf("pending not cleared after a full drain: from=%d", pendingFrom)
	}

	if reconcileForwardJump.Snapshot().Count() != jumps {
		t.Fatal("a complete drain is not a forward jump")
	}

	if got := publishDropMeter.Snapshot().Count() - drops; got != 0 {
		t.Fatalf("a complete drain dropped %d entries", got)
	}
}

// A range wider than one journal budget drains in batches: the batch takes
// the oldest blocks that fit, the remainder stays pending, and the next
// call resumes where it stopped — nothing is abandoned in between.
func TestBackfillResumesAcrossBatches(t *testing.T) {
	p := barePublisher()
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p.chain = chain

	// Each block carries ~12MiB of calldata, so a 32MiB budget takes two
	// blocks and change per batch.
	payload := make([]byte, 12<<20)
	parent := common.Hash{0xef}

	for n := uint64(1); n <= 5; n++ {
		header := testHeader(n, parent)
		chain.blocks[n] = blockFor(header, []*types.Transaction{bigTx(t, payload)})
		parent = header.Hash()
	}

	p.pendingFrom, p.pendingTo = 1, 5
	p.pendingEntries = 15

	fresh := newJournal()

	p.mu.Lock()
	cur := p.backfillLocked(fresh, commitment.Seed(testChainID))
	from1, to1 := p.pendingFrom, p.pendingTo
	p.mu.Unlock()

	if fresh.seals == 0 || fresh.seals >= 5 {
		t.Fatalf("first batch rebuilt %d blocks, want a strict subset", fresh.seals)
	}

	if fresh.items[0].height != 1 {
		t.Fatalf("first batch starts at %d, want 1 (oldest first)", fresh.items[0].height)
	}

	if from1 != uint64(fresh.seals)+1 || to1 != 5 {
		t.Fatalf("remainder not preserved: pending=[%d,%d] after %d rebuilt",
			from1, to1, fresh.seals)
	}

	// The next call picks up exactly where the last stopped.
	next := newJournal()

	p.mu.Lock()
	p.backfillLocked(next, cur)
	p.mu.Unlock()

	if next.items[0].height != from1 {
		t.Fatalf("second batch starts at %d, want %d", next.items[0].height, from1)
	}
}

// The whole pending range drains, storeSealedTip notwithstanding. The tip
// is the newest seal, not proof of anything below it: live flushes seal
// heights above the gap while it waits, and a store restart can shed acked
// writes — pending heights skipped on the tip stayed holes in the store
// forever on a devnet. A duplicate generation for a height the store does
// have costs churn; a hole costs the height.
func TestBackfillDrainsBelowTheSealedTip(t *testing.T) {
	p := barePublisher()
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p.chain = chain

	parent := common.Hash{0xef}
	for n := uint64(1); n <= 6; n++ {
		header := testHeader(n, parent)
		chain.blocks[n] = blockFor(header, nil)
		parent = header.Hash()
	}

	p.pendingFrom, p.pendingTo = 1, 6
	p.storeSealedTip = 4

	fresh := newJournal()

	p.mu.Lock()
	p.backfillLocked(fresh, commitment.Seed(testChainID))
	p.mu.Unlock()

	if fresh.seals != 6 || fresh.items[0].height != 1 {
		t.Fatalf("rebuilt seals=%d from=%d, want all 6 from 1: heights "+
			"skipped on the tip are never revisited", fresh.seals, fresh.items[0].height)
	}
}

// Mining continues while the backfill recovers an outage: with the DB as
// the archive nothing is displaced — every height delivers.
func TestMiningDuringBackfillLosesNothing(t *testing.T) {
	h := startHarness(t)
	chain := &fakeChain{blocks: map[uint64]*types.Block{}}
	p := newTestPublisher(t, h, chain)

	sealed := publishRecorded(t, p, chain, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	h.stop()

	parent := sealHash(t, sealed)
	for n := uint64(2); n <= 20; n++ {
		parent = sealHash(t, publishRecorded(t, p, chain, n, parent, 5))
	}

	h.resume()

	// New blocks keep sealing while the backfill drains.
	for n := uint64(21); n <= 24; n++ {
		parent = sealHash(t, publishRecorded(t, p, chain, n, parent, 5))
		time.Sleep(50 * time.Millisecond)
	}

	waitHead(t, h, p, 30*time.Second)

	for n := uint64(2); n <= 24; n++ {
		if _, err := h.store.GetBlock(context.Background(), &pb.GetBlockRequest{BlockNumber: n}); err != nil {
			t.Fatalf("block %d missing: mining-during-backfill displaced it", n)
		}
	}
}

// bigTx pads a transaction with calldata so a block's size is dominated by
// it — the batching tests size blocks against the journal byte budget.
func bigTx(t *testing.T, payload []byte) *types.Transaction {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(testChainID)), &types.DynamicFeeTx{
		ChainID:   big.NewInt(testChainID),
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(30_000_000_000),
		Gas:       21000,
		To:        &common.Address{0x01},
		Data:      payload,
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return tx
}
