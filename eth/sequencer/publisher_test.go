package sequencer

import (
	"math/big"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	"github.com/0xPolygon/sequence-store-proto/devstore"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

const testChainID = 1337

// harness serves one devstore over TCP and can stop and resume serving on
// the same address, keeping the store's state — an outage, from the
// publisher's point of view.
type harness struct {
	t     *testing.T
	store *devstore.Store
	addr  string
	srv   *grpc.Server
}

func startHarness(t *testing.T) *harness {
	return startHarnessChain(t, testChainID)
}

func startHarnessChain(t *testing.T, chainID uint64) *harness {
	t.Helper()

	h := &harness{t: t, store: devstore.New(chainID)}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	h.addr = lis.Addr().String()
	h.serve(lis)
	t.Cleanup(func() { h.srv.Stop() })

	return h
}

func (h *harness) serve(lis net.Listener) {
	h.srv = grpc.NewServer()
	pb.RegisterPublisherServiceServer(h.srv, h.store)
	pb.RegisterConsumerServiceServer(h.srv, h.store)

	go func() { _ = h.srv.Serve(lis) }()
}

func (h *harness) stop() {
	h.srv.Stop()
}

func (h *harness) resume() {
	h.t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		lis, err := net.Listen("tcp", h.addr)
		if err == nil {
			h.serve(lis)

			return
		}

		if time.Now().After(deadline) {
			h.t.Fatalf("relisten %s: %v", h.addr, err)
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func newTestPublisher(t *testing.T, h *harness, chain chainReader) *Publisher {
	t.Helper()

	p, err := NewPublisher(h.addr, h.addr, testChainID, 0, chain, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	t.Cleanup(p.Close)

	return p
}

func testTx(t *testing.T, nonce uint64) *types.Transaction {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	tx, err := types.SignNewTx(key, types.LatestSignerForChainID(big.NewInt(testChainID)), &types.DynamicFeeTx{
		ChainID:   big.NewInt(testChainID),
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(30_000_000_000),
		Gas:       21000,
		To:        &common.Address{0x01},
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	return tx
}

func testHeader(number uint64, parent common.Hash) *types.Header {
	return &types.Header{
		ParentHash: parent,
		Number:     new(big.Int).SetUint64(number),
		GasLimit:   30_000_000,
		Time:       1700000000 + number,
		BaseFee:    big.NewInt(25_000_000_000),
		Difficulty: big.NewInt(1),
	}
}

// blockFor assembles a sealed block from a header and its transactions.
func blockFor(header *types.Header, txs []*types.Transaction) *types.Block {
	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
}

// publishBlock drives a full lifecycle through the publisher and returns the
// sealed header.
func publishBlock(t *testing.T, p *Publisher, number uint64, parent common.Hash, txs int) *types.Header {
	t.Helper()

	header := testHeader(number, parent)
	p.OpenBlock(number, header.Time, parent, header.GasLimit, header.BaseFee)

	var body []*types.Transaction

	for i := 0; i < txs; i++ {
		tx := testTx(t, uint64(i))
		body = append(body, tx)
		p.PublishTx(tx)
	}

	p.SealBlock(blockFor(header, body))

	return header
}

func waitHead(t *testing.T, h *harness, p *Publisher, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		p.mu.Lock()
		local := p.head
		p.mu.Unlock()

		if h.store.Head() == local {
			return
		}

		if time.Now().After(deadline) {
			p.mu.Lock()
			defer p.mu.Unlock()
			t.Fatalf("store head %x never reached local head %x", h.store.Head(), p.head)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// The publisher's folds must match the store's across a full lifecycle.
func TestPublisherLifecycle(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	header := publishBlock(t, p, 1, common.Hash{0xef}, 3)
	waitHead(t, h, p, 5*time.Second)

	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatalf("rlp: %v", err)
	}

	// Independently computed chain: seed → open → 3 txs → seal.
	want := commitment.Seed(testChainID)
	want, err = commitment.FoldOpen(want, commitment.OpenContext{
		Number:     1,
		Timestamp:  header.Time,
		ParentHash: common.Hash{0xef},
		GasLimit:   header.GasLimit,
		BaseFee:    header.BaseFee,
	})
	if err != nil {
		t.Fatalf("fold open: %v", err)
	}

	p.mu.Lock()
	items, _ := p.journal.after(0)
	p.mu.Unlock()

	for _, item := range items {
		if item.kind == entryRecord {
			want = commitment.FoldTxs(want, item.entry.GetRecord().GetTransactions())
		}
	}

	want = commitment.FoldSeal(want, commitment.SealedHash(raw))

	if h.store.Head() != want {
		t.Fatalf("store head %x, want %x", h.store.Head(), want)
	}
}

// A restart against a non-empty store relocates the tail from the chain's
// last block and the next open extends it in place — no fresh topic
// required and no forward jump counted (nothing abandoned).
func TestRestartWarmResume(t *testing.T) {
	h := startHarness(t)

	jumps := reconcileForwardJump.Snapshot().Count()

	first, err := NewPublisher(h.addr, h.addr, testChainID, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	sealed := publishBlock(t, first, 1, common.Hash{0xef}, 2)
	waitHead(t, h, first, 5*time.Second)
	first.Close()

	// The restart locates the store tail from the chain's last block.
	chain := &fakeChain{current: &types.Header{Number: big.NewInt(1)}}

	second, err := NewPublisher(h.addr, h.addr, testChainID, 0, chain, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	t.Cleanup(second.Close)

	publishBlock(t, second, 2, sealHash(t, sealed), 1)
	waitHead(t, h, second, 5*time.Second)

	if got := reconcileForwardJump.Snapshot().Count(); got != jumps {
		t.Fatalf("clean warm resume counted %d forward jumps", got-jumps)
	}
}

// A cold publisher against a non-empty store anchors via the ladder's probe
// and floor rungs, located from the chain's last block.
func TestStartupColdNonEmptyStore(t *testing.T) {
	h := startHarness(t)

	seed := newTestPublisher(t, h, nil)
	sealed := publishBlock(t, seed, 1, common.Hash{0xef}, 1)
	waitHead(t, h, seed, 5*time.Second)
	seed.Close()

	chain := &fakeChain{current: &types.Header{Number: big.NewInt(1)}}

	p, err := NewPublisher(h.addr, h.addr, testChainID, 0, chain, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	t.Cleanup(p.Close)

	publishBlock(t, p, 2, sealHash(t, sealed), 1)
	waitHead(t, h, p, 5*time.Second)
}

// A store outage while blocks keep being produced recovers by journal replay:
// full continuity, every entry published.
func TestOutageJournalReplay(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	h.stop()

	// Produced entirely during the outage; folded into the journal only.
	sealed2 := publishBlock(t, p, 2, sealHash(t, sealed), 2)
	publishBlock(t, p, 3, sealHash(t, sealed2), 1)

	h.resume()
	waitHead(t, h, p, 15*time.Second)

	if got := h.store.Head(); got != localHead(p) {
		t.Fatalf("replay did not converge: store %x local %x", got, localHead(p))
	}
}

// A publisher whose chain names a height the store does not have (wiped or
// far behind) falls through the ladder to the floor and re-anchors on the
// seed.
func TestRestartAgainstUnknownStore(t *testing.T) {
	h := startHarness(t)

	// Chain claims height 9, but the store is empty: probe finds nothing,
	// the floor read seeds, and publishing resumes.
	chain := &fakeChain{current: &types.Header{Number: big.NewInt(9)}}

	p, err := NewPublisher(h.addr, h.addr, testChainID, 0, chain, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	t.Cleanup(p.Close)

	waitFor(t, 5*time.Second, func() bool { return p.isAnchored() })

	publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
}

// A dial-time error must surface, not return a nil publisher.
func TestNewPublisherRejectsBadEndpoint(t *testing.T) {
	if p, err := NewPublisher("bad\x7ftarget:99:99", "bad\x7ftarget:99:99", testChainID, 0, nil, nil); err == nil {
		p.Close()
		t.Fatal("bad endpoint accepted")
	}
}

// A fold failure (nil base fee) is a publisher bug: terminal, and every
// later enqueue is a no-op.
func TestOpenBlockFoldFailureIsTerminal(t *testing.T) {
	p := barePublisher()

	p.OpenBlock(1, 1700000001, common.Hash{0x01}, 30_000_000, nil)

	if !p.failed.Load() {
		t.Fatal("fold failure not terminal")
	}

	p.PublishTx(testTx(t, 0))

	if items, _ := p.journal.after(0); len(items) != 0 {
		t.Fatalf("enqueue after failure: %d items", len(items))
	}
}

// Records and seals are dropped while awaiting the next open after a purge.
func TestAwaitOpenSuppressesRecords(t *testing.T) {
	p := barePublisher()
	p.awaitOpen = true

	p.PublishTx(testTx(t, 0))

	if items, _ := p.journal.after(0); len(items) != 0 {
		t.Fatalf("suppressed records reached the journal: %d", len(items))
	}

	// A seal is never suppressed: the flush rebuilds the window from
	// the block body.
	p.SealBlock(blockFor(testHeader(1, common.Hash{0x01}), nil))

	if items, _ := p.journal.after(0); len(items) != 2 {
		t.Fatalf("seal flush must rebuild open+seal: %d items", len(items))
	}

	header := testHeader(2, common.Hash{0x02})
	p.OpenBlock(2, header.Time, header.ParentHash, header.GasLimit, header.BaseFee)
	p.PublishTx(testTx(t, 1))

	if items, _ := p.journal.after(0); len(items) != 4 {
		t.Fatalf("open must clear the suppression: %d items", len(items))
	}
}

func TestStaleOpenSuppressesRecords(t *testing.T) {
	p := barePublisher()
	p.sealedTip = 1

	stale := testHeader(1, common.Hash{0x01})
	p.OpenBlock(1, stale.Time, stale.ParentHash, stale.GasLimit, stale.BaseFee)
	p.PublishTx(testTx(t, 0))

	if items, _ := p.journal.after(0); len(items) != 0 {
		t.Fatalf("stale lifecycle reached the journal: %d items", len(items))
	}

	fresh := testHeader(2, stale.Hash())
	p.OpenBlock(2, fresh.Time, fresh.ParentHash, fresh.GasLimit, fresh.BaseFee)
	p.PublishTx(testTx(t, 1))

	if items, _ := p.journal.after(0); len(items) != 2 {
		t.Fatalf("fresh open did not resume publishing: %d items", len(items))
	}
}

func sealHash(t *testing.T, header *types.Header) common.Hash {
	t.Helper()

	return header.Hash()
}

func localHead(p *Publisher) commitment.Head {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.head
}

// A publisher pointed at a store seeded for a different chain anchors on
// the foreign seed and publishes blindly — nothing alarms on the producer
// side (detection is the consumer's and the auditor's job).
func TestWrongChainStorePublishesBlindly(t *testing.T) {
	h := startHarnessChain(t, testChainID+1)
	p := newTestPublisher(t, h, nil)

	publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	if p.failed.Load() {
		t.Fatal("wrong-chain store must not fail the publisher (blind by design)")
	}
}

func journalEntry(kind int) *pb.Entry {
	switch kind {
	case entryOpen:
		return &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{}}}
	case entrySeal:
		return &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{}}}
	default:
		return &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{Transactions: [][]byte{{0x01}}}}}
	}
}

// appendBlock adds an open+record+seal window at height h, delivered
// (acked), so the count bound may evict it.
func appendJournalBlock(r *journal, h uint64) {
	r.append(journalEntry(entryOpen), commitment.Head{}, commitment.Head{}, entryOpen, h, r.nextSeq, nil)
	r.append(journalEntry(entryRecord), commitment.Head{}, commitment.Head{}, entryRecord, h, r.nextSeq, nil)
	r.append(journalEntry(entrySeal), commitment.Head{}, commitment.Head{}, entrySeal, h, r.nextSeq, nil)
}

func TestJournalEvictsOldestSealedOnly(t *testing.T) {
	r := newJournal()

	for h := uint64(1); h <= journalSealedBlocks+2; h++ {
		appendJournalBlock(r, h)
	}

	// Two oldest sealed blocks evicted, open-window invariant untouched.
	if r.seals != journalSealedBlocks {
		t.Fatalf("seals retained %d, want %d", r.seals, journalSealedBlocks)
	}

	if first := r.items[0]; first.height != 3 || first.kind != entryOpen {
		t.Fatalf("front is height %d kind %d, want open of height 3", first.height, first.kind)
	}
}

func TestJournalNeverEvictsOpenWindow(t *testing.T) {
	r := newJournal()

	// One open window far over the byte cap: nothing sealed to drop.
	r.append(journalEntry(entryOpen), commitment.Head{}, commitment.Head{}, entryOpen, 1, r.nextSeq, nil)

	huge := &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions: [][]byte{make([]byte, journalMaxBytes+1)},
	}}}
	r.append(huge, commitment.Head{}, commitment.Head{}, entryRecord, 1, r.nextSeq, nil)

	if len(r.items) != 2 {
		t.Fatalf("open window evicted: %d items", len(r.items))
	}
}

func TestJournalAfterGapDetection(t *testing.T) {
	r := newJournal()

	for h := uint64(1); h <= journalSealedBlocks+3; h++ {
		appendJournalBlock(r, h)
	}

	// seq 1 was evicted: the position is no longer covered.
	if _, covered := r.after(0); covered {
		t.Fatal("gap not detected after eviction")
	}

	items, covered := r.after(r.items[0].seq)
	if !covered || len(items) != len(r.items)-1 {
		t.Fatalf("covered=%v len=%d", covered, len(items))
	}
}

func TestJournalAfterEmptyBoundary(t *testing.T) {
	r := newJournal()

	// Empty journal: only the position right before nextSeq is covered.
	if _, covered := r.after(0); !covered {
		t.Fatal("fresh empty journal must cover seq 0")
	}

	appendJournalBlock(r, 1)
	r.items, r.seals = nil, 0 // simulate a full drain

	if _, covered := r.after(r.nextSeq - 1); !covered {
		t.Fatal("drained journal must cover its frontier")
	}

	if _, covered := r.after(r.nextSeq - 2); covered {
		t.Fatal("drained journal must not cover older positions")
	}
}

func TestJournalEvictOnlyBeyondSealedBound(t *testing.T) {
	r := newJournal()

	for h := uint64(1); h <= journalSealedBlocks; h++ {
		appendJournalBlock(r, h)
	}

	if r.seals != journalSealedBlocks || r.items[0].height != 1 {
		t.Fatalf("eviction fired at the bound: seals=%d front=%d", r.seals, r.items[0].height)
	}
}

// Undelivered seals are never evicted: eviction only trims delivered
// history. Older undelivered flushes leave via collapseOldestUnacked,
// which hands their heights to the chain-database backfill.
func TestJournalKeepsUndeliveredSeals(t *testing.T) {
	r := newJournal()

	for h := uint64(1); h <= journalSealedBlocks+40; h++ {
		r.append(journalEntry(entryOpen), commitment.Head{}, commitment.Head{}, entryOpen, h, 0, nil)
		r.append(journalEntry(entrySeal), commitment.Head{}, commitment.Head{}, entrySeal, h, 0, nil)
	}

	if r.seals != journalSealedBlocks+40 || r.items[0].height != 1 {
		t.Fatalf("undelivered seal evicted: seals=%d front=%d", r.seals, r.items[0].height)
	}
}

func TestJournalCollapseOldestUnacked(t *testing.T) {
	r := newJournal()

	for h := uint64(1); h <= 4; h++ {
		r.append(journalEntry(entryOpen), commitment.Head{}, commitment.Head{}, entryOpen, h, 0, nil)
		r.append(journalEntry(entrySeal), commitment.Head{}, commitment.Head{}, entrySeal, h, 0, nil)
	}

	// Block 1 delivered (acked through its seal at seq 2); 2-4 undelivered.
	// Each undelivered block is open+seal = 2 entries.
	if h, n, ok := r.collapseOldestUnacked(2); !ok || h != 2 || n != 2 {
		t.Fatalf("collapse = %d/%d/%v, want 2/2", h, n, ok)
	}

	// The delivered block dropped with it; block 3 is now the front.
	if r.items[0].height != 3 || r.seals != 2 {
		t.Fatalf("front=%d seals=%d after collapse", r.items[0].height, r.seals)
	}

	if _, _, ok := r.collapseOldestUnacked(2); !ok {
		t.Fatal("second collapse must pop block 3")
	}

	if h, n, ok := r.collapseOldestUnacked(2); !ok || h != 4 || n != 2 {
		t.Fatalf("third collapse = %d/%d/%v, want 4/2", h, n, ok)
	}

	if _, _, ok := r.collapseOldestUnacked(2); ok {
		t.Fatal("no seals left to collapse")
	}
}

func TestJournalSuffixFromHeight(t *testing.T) {
	r := newJournal()
	appendJournalBlock(r, 5)
	appendJournalBlock(r, 6)

	suffix := r.suffixFromHeight(6)
	if len(suffix) != 3 || suffix[0].kind != entryOpen || suffix[0].height != 6 {
		t.Fatalf("suffix wrong: len=%d", len(suffix))
	}

	if r.suffixFromHeight(7) != nil {
		t.Fatal("suffix beyond retained heights should be nil")
	}
}
