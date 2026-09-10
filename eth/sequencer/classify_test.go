package sequencer

import (
	"context"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// lineagePublisher builds a bare publisher holding a confirmed one-block
// lineage plus an open window at height 2 with one record.
func lineagePublisher(t *testing.T, chain chainReader) (*Publisher, commitment.Head) {
	t.Helper()

	p := barePublisher()
	p.chain = chain

	header := testHeader(1, common.Hash{0xef})
	p.OpenBlock(1, header.Time, header.ParentHash, header.GasLimit, header.BaseFee)
	p.SealBlock(blockFor(header, nil))

	p.OpenBlock(2, header.Time+1, header.Hash(), header.GasLimit, header.BaseFee)
	p.PublishTx(testTx(t, 0))

	// Everything through the seal of block 1 is store-confirmed.
	items, _ := p.journal.after(0)
	sealItem := items[1]
	p.ackedSeq = sealItem.seq
	p.anchor = sealItem.post
	p.confirmed = true

	return p, sealItem.post
}

func TestApplyTailRow1Replay(t *testing.T) {
	p, anchor := lineagePublisher(t, &fakeChain{})

	if out := p.applyTail(tailInfo{s: anchor}); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.anchored || p.head == anchor {
		t.Fatalf("must anchor and keep the unconfirmed suffix (anchored=%v)", p.anchored)
	}
}

func TestApplyTailRow1FindPost(t *testing.T) {
	p, _ := lineagePublisher(t, &fakeChain{})

	// The store confirmed one entry further than we knew: the open of 2.
	items, _ := p.journal.after(p.ackedSeq)
	openItem := items[0]

	if out := p.applyTail(tailInfo{s: openItem.post}); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ackedSeq != openItem.seq || p.anchor != openItem.post {
		t.Fatalf("frontier not advanced: acked=%d", p.ackedSeq)
	}
}

// A foreign tail while our window is unsealed never writes: the publisher
// holds and the seal flush resolves the height.
func TestApplyTailForeignUnsealedHolds(t *testing.T) {
	cases := []struct {
		name string
		info tailInfo
	}{
		{name: "open window at pending", info: tailInfo{s: commitment.Head{0x99}, tipOpen: true, tipOpenHeight: 2}},
		{name: "open window above pending", info: tailInfo{s: commitment.Head{0x33}, tipOpen: true, tipOpenHeight: 5}},
		{name: "sealed past pending", info: tailInfo{s: commitment.Head{0x55}, haveSeal: true, lastSealHeight: 2, lastSealHash: common.Hash{0xcc}}},
		{name: "store behind", info: tailInfo{s: commitment.Head{0x77}, haveSeal: true, lastSealHeight: 1, lastSealHash: common.Hash{0xaa}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeChain{}
			p, _ := lineagePublisher(t, fc)

			journalBefore := len(p.journal.items)

			if out := p.applyTail(tc.info); out != recOK {
				t.Fatalf("outcome = %v", out)
			}

			p.mu.Lock()
			defer p.mu.Unlock()

			if p.hold.after != p.ackedSeq {
				t.Fatalf("unsealed window against a foreign tail must gate unacked entries (ceiling=%d acked=%d)", p.hold.after, p.ackedSeq)
			}

			if len(p.journal.items) != journalBefore {
				t.Fatal("hold must not touch the lineage")
			}
		})
	}
}

// The same foreign tails with the window sealed (flush in flight) re-anchor
// the sealed window onto the store head — supersede for contention shapes,
// forward jump for a store still behind.
func TestApplyTailSealedFlushReanchors(t *testing.T) {
	cases := []struct {
		name     string
		info     tailInfo
		wantNone bool
	}{
		{name: "open window at pending", info: tailInfo{s: commitment.Head{0x99}, tipOpen: true, tipOpenHeight: 2}},
		{name: "sealed divergent at pending", info: tailInfo{s: commitment.Head{0x55}, haveSeal: true, lastSealHeight: 2, lastSealHash: common.Hash{0xcc}}},
		{name: "parent sealed, clean tail", info: tailInfo{s: commitment.Head{0x77}, haveSeal: true, lastSealHeight: 1, lastSealHash: common.Hash{0xaa}}, wantNone: true},
		{name: "unsealed contender above flush", info: tailInfo{s: commitment.Head{0x44}, haveSeal: true, lastSealHeight: 1, lastSealHash: common.Hash{0xaa}, tipOpen: true, tipOpenHeight: 3}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeChain{}
			p, _ := lineagePublisher(t, fc)

			// Seal the pending window locally: the flush owns the height now.
			header2 := testHeader(2, common.Hash{0x01})
			sealOnChain(p, fc, header2, []*types.Transaction{testTx(t, 0)})

			// The block went out on the gate's liveness path (Unknown):
			// from here the flush is post-broadcast repair, which is the
			// behavior under test.
			p.ConfirmSeal(time.Millisecond)

			supersedes := reconcileSupersede.Snapshot().Count()
			jumps := reconcileForwardJump.Snapshot().Count()

			if out := p.applyTail(tc.info); out != recOK {
				t.Fatalf("outcome = %v", out)
			}

			p.mu.Lock()
			defer p.mu.Unlock()

			items, covered := p.journal.after(p.ackedSeq)
			if !covered || len(items) == 0 {
				t.Fatalf("re-anchored suffix missing: covered=%v len=%d", covered, len(items))
			}

			if items[0].kind != entryOpen || items[0].height != 2 {
				t.Fatalf("suffix starts with kind %d height %d", items[0].kind, items[0].height)
			}

			if got := commitment.Head(entryPrefix(items[0].entry)); got != tc.info.s {
				t.Fatalf("re-prefixed onto %x, want %x", got, tc.info.s)
			}

			if items[len(items)-1].kind != entrySeal {
				t.Fatal("the flush suffix must end with the seal")
			}

			if tc.wantNone {
				if got := reconcileSupersede.Snapshot().Count(); got != supersedes {
					t.Fatalf("re-delivery counted as supersede: %d -> %d", supersedes, got)
				}

				if got := reconcileForwardJump.Snapshot().Count(); got != jumps {
					t.Fatalf("re-delivery counted as jump: %d -> %d", jumps, got)
				}
			} else {
				if got := reconcileSupersede.Snapshot().Count(); got != supersedes+1 {
					t.Fatalf("supersede counter = %d, want %d", got, supersedes+1)
				}
			}
		})
	}
}

// Flushes stacked behind a reconcile backoff all re-anchor — oldest
// window first — so no sealed-on-chain height is left unsealed in the
// store (the quad-strand shape).
func TestStackedFlushesAllDeliver(t *testing.T) {
	fc2 := &fakeChain{}
	p, _ := lineagePublisher(t, fc2)

	// Two blocks seal while nothing delivers: the journal carries two
	// complete flush windows beyond the acked prefix.
	header2 := testHeader(2, common.Hash{0x01})
	sealOnChain(p, fc2, header2, []*types.Transaction{testTx(t, 0)})
	p.OpenBlock(3, header2.Time+1, header2.Hash(), header2.GasLimit, header2.BaseFee)
	tx := testTx(t, 1)
	p.PublishTx(tx)
	sealOnChain(p, fc2, testHeader(3, header2.Hash()), []*types.Transaction{tx})

	// The next build has already opened on top of the stack — the flush
	// must stay visible through the trailing open (back-to-back builds
	// leave almost no trailing-seal moments for a reconcile to land in).
	header3 := testHeader(3, common.Hash{0x02})
	p.OpenBlock(4, header3.Time+1, header3.Hash(), header3.GasLimit, header3.BaseFee)

	// A foreign contender window stands above both flushes.
	foreign := commitment.Head{0x44}
	info := tailInfo{s: foreign, haveSeal: true, lastSealHeight: 1, lastSealHash: common.Hash{0xaa}, tipOpen: true, tipOpenHeight: 3}

	if out := p.applyTail(info); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	items, covered := p.journal.after(p.ackedSeq)
	if !covered || len(items) == 0 {
		t.Fatalf("re-anchored suffix missing: covered=%v len=%d", covered, len(items))
	}

	if items[0].kind != entryOpen || items[0].height != 2 {
		t.Fatalf("suffix starts with kind %d height %d, want the oldest flush window", items[0].kind, items[0].height)
	}

	if got := commitment.Head(entryPrefix(items[0].entry)); got != foreign {
		t.Fatalf("re-prefixed onto %x, want %x", got, foreign)
	}

	seals := 0

	for _, it := range items {
		if it.kind == entrySeal {
			seals++
		}
	}

	if seals != 2 {
		t.Fatalf("re-anchored suffix carries %d seals, want both", seals)
	}
}

// A seal flush whose window does not mirror the sealed block rebuilds the
// window — without erasing older flushes still undelivered behind it.
func TestSealRebuildPreservesStackedFlushes(t *testing.T) {
	p, _ := lineagePublisher(t, &fakeChain{})

	// First flush: seals the standing window for height 2, undelivered.
	header2 := testHeader(2, common.Hash{0x01})
	p.SealBlock(blockFor(header2, []*types.Transaction{testTx(t, 0)}))

	// Second build diverges from what seals (its window will not mirror
	// the block), forcing the rebuild path.
	p.OpenBlock(3, header2.Time+1, header2.Hash(), header2.GasLimit, header2.BaseFee)
	p.PublishTx(testTx(t, 1))

	sealedTx := testTx(t, 2)
	p.SealBlock(blockFor(testHeader(3, header2.Hash()), []*types.Transaction{sealedTx}))

	p.mu.Lock()
	defer p.mu.Unlock()

	heights := map[uint64]int{}

	for _, it := range p.journal.items {
		if it.kind == entrySeal && it.seq > p.ackedSeq {
			heights[it.height]++
		}
	}

	if heights[2] != 1 || heights[3] != 1 {
		t.Fatalf("stacked flushes lost in rebuild: undelivered seals per height = %v", heights)
	}
}

func TestApplyTailBetweenBlocksRebases(t *testing.T) {
	p := barePublisher()

	// The store's sealed lineage is far past our stale flush and is the
	// canonical chain locally: the chain moved on, so a rebase is right
	// (a canonical sealed height past the flush outranks it).
	p.chain = &fakeChain{canonical: map[uint64]common.Hash{8: {0xbb}}}

	header := testHeader(1, common.Hash{0xef})
	p.OpenBlock(1, header.Time, header.ParentHash, header.GasLimit, header.BaseFee)
	p.SealBlock(blockFor(header, nil))
	p.curHeight = 0 // between blocks

	foreign := commitment.Head{0x22}
	info := tailInfo{s: foreign, haveSeal: true, lastSealHeight: 8, lastSealHash: common.Hash{0xbb}, tipOpen: true, tipOpenHeight: 9}

	if out := p.applyTail(info); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.head != foreign || p.anchor != foreign {
		t.Fatalf("head/anchor not rebased: %x/%x", p.head, p.anchor)
	}

	if items, _ := p.journal.after(p.ackedSeq); len(items) != 0 {
		t.Fatal("rebase must purge the lineage")
	}
}

func TestProbeFindsStoreEdge(t *testing.T) {
	h := startHarness(t)

	seed := newTestPublisher(t, h, nil)
	sealed := publishBlock(t, seed, 1, common.Hash{0xef}, 1)
	sealed2 := publishBlock(t, seed, 2, sealed.Hash(), 1)
	publishBlock(t, seed, 3, sealed2.Hash(), 1)
	waitHead(t, h, seed, 5*time.Second)
	seed.Close()

	p := newTestPublisher(t, h, nil)
	waitFor(t, 5*time.Second, func() bool { return p.isAnchored() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	edge, found, err := p.read.probeDown(ctx, 10)
	if err != nil || !found || edge != 3 {
		t.Fatalf("probeDown = %d/%v/%v, want 3", edge, found, err)
	}

	up, err := p.read.probeUp(ctx, 1)
	if err != nil || up != 3 {
		t.Fatalf("probeUp = %d/%v, want 3", up, err)
	}
}

// A lagging ack from a lineage an adoption has since replaced must not
// regress the frontier — the phantom eviction gap it faked used to force
// a spurious forward-jump superseding a window under silent adopt.
func TestLaggingAckAfterLineageSwapIgnored(t *testing.T) {
	p, _ := lineagePublisher(t, &fakeChain{})

	inflight, _ := p.journal.after(p.ackedSeq)
	openItem := inflight[0]

	var win []*pb.Entry
	for _, it := range inflight {
		win = append(win, it.entry)
	}

	p.mu.Lock()
	info := tailInfo{s: p.head, tipOpen: true, tipOpenHeight: 2, window: win}

	a, items, ok := parseWindow(info)
	if !ok {
		p.mu.Unlock()
		t.Fatal("window unparseable")
	}

	p.adoptWindowLocked(info, a, items)
	acked := p.ackedSeq
	p.mu.Unlock()

	jumps := reconcileForwardJump.Snapshot().Count()

	// The transport's lagging ack for the pre-swap open arrives.
	p.retire(openItem, time.Now())

	p.mu.Lock()
	after, anchor := p.ackedSeq, p.anchor
	p.mu.Unlock()

	if after != acked {
		t.Fatalf("stale ack regressed ackedSeq: %d -> %d", acked, after)
	}

	if out := p.applyTail(tailInfo{s: anchor}); out != recOK {
		t.Fatalf("row-1 outcome: %v", out)
	}

	if got := reconcileForwardJump.Snapshot().Count(); got != jumps {
		t.Fatalf("spurious forward-jump: %d -> %d", jumps, got)
	}
}

// completeExtendedWindowLocked abandons older stacked flushes — their
// content is pinned byte-identically in the store lineage the window
// folds on — but must count them as drops.
func TestExtendedCompletionCountsAbandonedStack(t *testing.T) {
	p, _ := lineagePublisher(t, &fakeChain{})

	header2 := testHeader(2, common.Hash{0x01})
	p.SealBlock(blockFor(header2, []*types.Transaction{testTx(t, 0)}))

	p.OpenBlock(3, header2.Time+1, header2.Hash(), header2.GasLimit, header2.BaseFee)
	tx := testTx(t, 1)
	p.PublishTx(tx)
	p.SealBlock(blockFor(testHeader(3, header2.Hash()), []*types.Transaction{tx}))

	p.mu.Lock()
	defer p.mu.Unlock()

	ours := p.journal.suffixFromHeight(3)
	win := []*pb.Entry{ours[0].entry, ours[1].entry}
	info := tailInfo{s: ours[1].post, tipOpen: true, tipOpenHeight: 3, window: win}

	drops := publishDropMeter.Snapshot().Count()

	if !p.completeExtendedWindowLocked(info, 3) {
		t.Fatal("completion did not fire")
	}

	if got := publishDropMeter.Snapshot().Count(); got <= drops {
		t.Fatalf("abandoned stack uncounted: %d -> %d", drops, got)
	}
}

// A foreign seal above our pending flush that the local chain does NOT
// consider canonical (a reorg loser, or a lineage not yet imported) must
// not drop our canonical seal: hold and let our next flush re-anchor.
func TestForeignSealAboveNonCanonicalHolds(t *testing.T) {
	p, _ := lineagePublisher(t, &fakeChain{}) // nil canonical map: seal not canonical

	header2 := testHeader(2, common.Hash{0x01})
	p.SealBlock(blockFor(header2, []*types.Transaction{testTx(t, 0)}))

	forwardBefore := reconcileForwardJump.Snapshot().Count()

	// A foreign seal at height 3 stands above our pending flush for 2, but
	// GetCanonicalHash(3) is zero (not this hash) → not canonical.
	info := tailInfo{s: commitment.Head{0x22}, haveSeal: true, lastSealHeight: 3, lastSealHash: common.Hash{0xbb}}

	if out := p.applyTail(info); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Our pending seal must survive (held, not dropped) and no jump counted.
	items, covered := p.journal.after(p.ackedSeq)
	if !covered || len(items) == 0 || items[len(items)-1].kind != entrySeal {
		t.Fatalf("pending seal dropped for a non-canonical foreign tail: covered=%v len=%d", covered, len(items))
	}

	if p.hold.kind != holdSticky {
		t.Fatalf("expected holdSticky, got holdKind=%d", p.hold.kind)
	}

	if got := reconcileForwardJump.Snapshot().Count(); got != forwardBefore {
		t.Fatalf("dropped a canonical seal (forward jump %d -> %d)", forwardBefore, got)
	}
}

// A foreign seal above our pending flush that IS canonical locally means
// we were genuinely reorged past: drop the stale seal and rebase.
func TestForeignSealAboveCanonicalRebases(t *testing.T) {
	p, _ := lineagePublisher(t, &fakeChain{canonical: map[uint64]common.Hash{3: {0xbb}}})

	header2 := testHeader(2, common.Hash{0x01})
	p.SealBlock(blockFor(header2, []*types.Transaction{testTx(t, 0)}))

	info := tailInfo{s: commitment.Head{0x22}, haveSeal: true, lastSealHeight: 3, lastSealHash: common.Hash{0xbb}}

	if out := p.applyTail(info); out != recOK {
		t.Fatalf("outcome = %v", out)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.anchor != (commitment.Head{0x22}) {
		t.Fatalf("canonical reorg-past must rebase: anchor=%x", p.anchor)
	}
}
