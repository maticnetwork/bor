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

// The preconfirmation failures storeprobe measures on a devnet. Asserting
// them here closes the gap that let a 40x regression pass a green suite: the
// other tests check what the publisher decides, these check what a consumer
// of the store would actually experience.
type storeAudit struct {
	// Revoked: a record the store acked — so it was preconfirmed — that
	// never landed in any canonical block. The worst outcome; the promise
	// was simply broken.
	Revoked []common.Hash

	// Displaced: acked at one height, landed at another. The transaction
	// executes, but not where it was promised.
	Displaced []common.Hash

	// Reordered: landed at the promised height in a different order.
	// Position within a block is part of the promise for anything
	// order-sensitive.
	Reordered int

	// Mismatch: the store's newest generation at a height disagrees with the
	// canonical block. A consumer reading the store's latest view of that
	// height gets the wrong answer.
	Mismatch []uint64
}

func (a storeAudit) clean() bool {
	return len(a.Revoked) == 0 && len(a.Displaced) == 0 &&
		a.Reordered == 0 && len(a.Mismatch) == 0
}

// generation is one open..seal span at a height, in store order.
type generation struct {
	height uint64
	txs    []common.Hash
	sealed bool
}

// readAllGenerations replays the whole log and groups it the way the store's
// readers see it: an open starts a generation at its height, records extend
// the current one, and later generations at a height supersede earlier ones.
func readAllGenerations(t *testing.T, h *harness) []generation {
	t.Helper()

	seed := commitment.Seed(testChainID)

	var (
		gens []generation
		cur  *generation
		next = &pb.RangeRequest{
			After: &pb.RangeRequest_Head{Head: seed.Bytes()},
			Limit: 512,
		}
	)

	for {
		resp, err := h.store.Range(context.Background(), next)
		if err != nil {
			t.Fatalf("range: %v", err)
		}

		for _, e := range resp.GetEntries() {
			switch {
			case e.GetBlockOpen() != nil:
				gens = append(gens, generation{height: e.GetBlockOpen().GetBlockNumber()})
				cur = &gens[len(gens)-1]
			case e.GetRecord() != nil && cur != nil:
				for _, raw := range e.GetRecord().GetTransactions() {
					var tx types.Transaction
					if err := tx.UnmarshalBinary(raw); err != nil {
						t.Fatalf("decode record tx: %v", err)
					}

					cur.txs = append(cur.txs, tx.Hash())
				}
			case e.GetBlockSeal() != nil && cur != nil:
				cur.sealed = true
			}
		}

		if resp.GetLive() {
			return gens
		}

		next = &pb.RangeRequest{
			After: &pb.RangeRequest_Head{Head: resp.GetNext()},
			Limit: 512,
		}
	}
}

// auditStore compares everything the store acked against the canonical
// chain the test declares (height -> ordered transaction hashes).
func auditStore(t *testing.T, h *harness, canonical map[uint64][]common.Hash) storeAudit {
	t.Helper()

	gens := readAllGenerations(t, h)

	landedAt := map[common.Hash]uint64{}
	indexIn := map[common.Hash]int{}

	for height, txs := range canonical {
		for i, tx := range txs {
			landedAt[tx] = height
			indexIn[tx] = i
		}
	}

	var audit storeAudit

	for _, g := range gens {
		for _, tx := range g.txs {
			at, ok := landedAt[tx]
			switch {
			case !ok:
				audit.Revoked = append(audit.Revoked, tx)
			case at != g.height:
				audit.Displaced = append(audit.Displaced, tx)
			}
		}
	}

	// The newest generation at a height is what a reader takes as the truth
	// for that height, so that is what has to agree with the block.
	newest := map[uint64]generation{}
	for _, g := range gens {
		newest[g.height] = g
	}

	for height, g := range newest {
		block, ok := canonical[height]
		if !ok {
			continue // no block at this height: nothing to disagree with
		}

		if len(g.txs) != len(block) {
			audit.Mismatch = append(audit.Mismatch, height)

			continue
		}

		for i := range g.txs {
			if g.txs[i] != block[i] {
				audit.Mismatch = append(audit.Mismatch, height)

				break
			}
		}

		// Order within the promised height, counted pairwise as storeprobe
		// does: two records promised in one order must not land reversed.
		for i := 0; i < len(g.txs); i++ {
			for j := i + 1; j < len(g.txs); j++ {
				a, b := g.txs[i], g.txs[j]
				if landedAt[a] == height && landedAt[b] == height &&
					indexIn[a] > indexIn[b] {
					audit.Reordered++
				}
			}
		}
	}

	return audit
}

// The auditor has to fail before it is worth trusting: each failure class is
// constructed deliberately and must be reported. An auditor that only ever
// says "clean" is what a green suite looked like while a devnet burned.
func TestAuditorDetectsEachFailureClass(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	// Three records acked at height 2, in this order.
	t0, t1, t2 := testTx(t, 0), testTx(t, 1), testTx(t, 2)
	foreignWindow(t, h, 2, parent, t0, t1, t2)

	block1 := []common.Hash{}
	for _, tx := range sealed1Txs(t, h) {
		block1 = append(block1, tx)
	}

	cases := []struct {
		name      string
		canonical map[uint64][]common.Hash
		want      func(storeAudit) bool
		reason    string
	}{
		{
			name:      "clean",
			canonical: map[uint64][]common.Hash{1: block1, 2: {t0.Hash(), t1.Hash(), t2.Hash()}},
			want:      func(a storeAudit) bool { return a.clean() },
			reason:    "an exact match must audit clean",
		},
		{
			name:      "revoked",
			canonical: map[uint64][]common.Hash{1: block1, 2: {t0.Hash(), t1.Hash()}},
			want:      func(a storeAudit) bool { return len(a.Revoked) == 1 },
			reason:    "a preconfirmed record in no block is a revocation",
		},
		{
			name:      "displaced",
			canonical: map[uint64][]common.Hash{1: block1, 2: {t0.Hash(), t1.Hash()}, 3: {t2.Hash()}},
			want:      func(a storeAudit) bool { return len(a.Displaced) == 1 },
			reason:    "acked at 2 but landed at 3 is a displacement",
		},
		{
			name:      "reordered",
			canonical: map[uint64][]common.Hash{1: block1, 2: {t0.Hash(), t2.Hash(), t1.Hash()}},
			want:      func(a storeAudit) bool { return a.Reordered > 0 },
			reason:    "same height, swapped order, is a reorder",
		},
		{
			name:      "mismatch",
			canonical: map[uint64][]common.Hash{1: block1, 2: {t0.Hash()}},
			want:      func(a storeAudit) bool { return len(a.Mismatch) == 1 },
			reason:    "the newest generation must equal the block at that height",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := auditStore(t, h, tc.canonical)
			if !tc.want(got) {
				t.Fatalf("%s: audit=%+v", tc.reason, got)
			}
		})
	}
}

// sealed1Txs returns the transactions the store holds at height 1, so a test
// can declare a canonical chain that agrees with it.
func sealed1Txs(t *testing.T, h *harness) []common.Hash {
	t.Helper()

	for _, g := range readAllGenerations(t, h) {
		if g.height == 1 {
			return g.txs
		}
	}

	return nil
}

// canonicalFrom builds the canonical view a test declares by treating the
// blocks a publisher actually sealed as the chain, which is what happens when
// only one producer closes a height.
func canonicalFrom(blocks map[uint64][]*types.Transaction) map[uint64][]common.Hash {
	out := map[uint64][]common.Hash{}

	for height, txs := range blocks {
		hashes := make([]common.Hash, 0, len(txs))
		for _, tx := range txs {
			hashes = append(hashes, tx.Hash())
		}

		out[height] = hashes
	}

	return out
}

// The end-to-end property, stated in the terms a consumer cares about: a
// single producer's published window must audit clean against the block it
// seals. No revocation, no displacement, no reorder, no mismatch.
func TestSoleProducerAuditsClean(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	txs := []*types.Transaction{testTx(t, 0), testTx(t, 1), testTx(t, 2)}

	header := testHeader(1, common.Hash{0xef})
	p.OpenBlock(1, header.Time, common.Hash{0xef}, header.GasLimit, header.BaseFee)

	for _, tx := range txs {
		p.PublishTx(tx)
	}

	p.SealBlock(blockFor(header, txs))
	waitHead(t, h, p, 5*time.Second)

	audit := auditStore(t, h, canonicalFrom(map[uint64][]*types.Transaction{1: txs}))
	if !audit.clean() {
		t.Fatalf("a sole producer broke its own preconfirmations: %+v", audit)
	}
}

// Refusal buys agreement: whichever producer closes the height, the store's
// newest generation there must equal the block. That is the mismatch class,
// and it is what a consumer reading the store's latest view depends on.
func TestRefusalPreventsMismatch(t *testing.T) {
	h, a, b := twinPublishers(t)
	parent := sealedParent(t, h, a, b)

	header := testHeader(2, parent)

	// Distinct nonces throughout: height 1 already holds nonce 0.
	aTxs := []*types.Transaction{testTx(t, 1), testTx(t, 2)}
	bTxs := []*types.Transaction{testTx(t, 7)}

	a.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)

	for _, tx := range aTxs {
		a.PublishTx(tx)
	}

	waitDrained(t, a, 5*time.Second)

	b.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)

	for _, tx := range bTxs {
		b.PublishTx(tx)
	}

	waitFor(t, 5*time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()

		return b.hold.kind == holdSticky || b.unackedLocked() == 0
	})

	sealA, sealB := decide(a), decide(b)
	if sealA && sealB {
		t.Fatal("both sealed: divergent blocks at one height")
	}

	if !sealA && !sealB {
		t.Fatal("neither sealed: the height never closes")
	}

	winner := bTxs
	if sealA {
		winner = aTxs
	}

	canonical := canonicalFrom(map[uint64][]*types.Transaction{2: winner})
	canonical[1] = sealed1Txs(t, h)

	audit := auditStore(t, h, canonical)

	if len(audit.Mismatch) > 0 {
		t.Fatalf("the store's newest generation disagrees with the sealed "+
			"block at %v: a consumer reading the store gets the wrong "+
			"answer for that height", audit.Mismatch)
	}
}

// What refusal does NOT buy, stated plainly so nobody expects it to. Once a
// rival's records are acked they are preconfirmed, and they cannot also be in
// a block built from different content. They land later or not at all.
//
// Note the escape the publisher gets for free in the common case: a second
// producer appending to the same generation STALEs and never acks, so it
// never promises anything. This test uses a rival that opens its own
// generation, which the store accepts by design — the one shape where both
// sides really do hold promises. Nothing available to bor prevents it; only
// the store declining a second open at a live height removes the class.
func TestRivalGenerationStrandsItsOwnRecords(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	header := testHeader(2, parent)
	ours := []*types.Transaction{testTx(t, 1), testTx(t, 2)}

	p.OpenBlock(2, header.Time, parent, header.GasLimit, header.BaseFee)

	for _, tx := range ours {
		p.PublishTx(tx)
	}

	waitDrained(t, p, 5*time.Second)

	// A rival opens its own generation on the current head, so its record is
	// accepted and therefore preconfirmed.
	rival := testTx(t, 9)
	foreignWindow(t, h, 2, parent, rival)

	// Only one block exists at height 2, and it holds our content.
	canonical := canonicalFrom(map[uint64][]*types.Transaction{2: ours})
	canonical[1] = sealed1Txs(t, h)

	audit := auditStore(t, h, canonical)

	stranded := len(audit.Revoked) + len(audit.Displaced)
	if stranded != 1 {
		t.Fatalf("expected exactly the rival's record stranded, got %d "+
			"(revoked=%d displaced=%d): more than that means contention is "+
			"costing the winner's promises too",
			stranded, len(audit.Revoked), len(audit.Displaced))
	}

	// And the mismatch class is the one bor can still control: the store's
	// newest generation at this height is the rival's, so a reader gets the
	// wrong answer until the winner's flush corrects it.
	if len(audit.Mismatch) != 1 || audit.Mismatch[0] != 2 {
		t.Fatalf("expected the rival generation to leave height 2 mismatched, got %v",
			audit.Mismatch)
	}
}
