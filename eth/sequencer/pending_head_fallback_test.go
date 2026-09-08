package sequencer

import (
	"testing"

	"github.com/0xPolygon/sequence-store-proto/commitment"
)

// HeadPendingView is the last-resort pending view a non-mining RPC node serves
// in the window between importing a block and receiving the producer's open
// for the next height, when no preconf entry is active and there is no miner
// to fall back to. It must be an empty block on head+1 carrying the head
// state — the correct pending answer while nothing is preconfirmed. Before
// this fix such a node answered -32000 "pending state is not available" for
// ~150ms every block, breaking bind's default eth_getCode(addr,"pending").
func TestHeadPendingViewServesEmptyHeadState(t *testing.T) {
	h := startExecHarness(t)
	c := &Consumer{chain: h.chain, index: NewIndex()}
	head := h.chain.CurrentBlock()

	block, receipts, statedb, err := c.HeadPendingView()
	if err != nil {
		t.Fatalf("HeadPendingView error: %v", err)
	}
	if block == nil || statedb == nil {
		t.Fatal("head pending view unavailable: the -32000 regression")
	}
	if got, want := block.NumberU64(), head.Number.Uint64()+1; got != want {
		t.Fatalf("pending number = %d, want head+1 = %d", got, want)
	}
	if block.ParentHash() != head.Hash() {
		t.Fatalf("pending parent = %s, want head %s", block.ParentHash(), head.Hash())
	}
	if len(block.Transactions()) != 0 || len(receipts) != 0 {
		t.Fatalf("gap pending must be empty, got %d txs %d receipts",
			len(block.Transactions()), len(receipts))
	}
	if statedb.GetBalance(h.addr).IsZero() {
		t.Fatal("served state is not the head state: funded genesis account missing")
	}
}

// The head view is a pure fallback: it reflects only the head state and never
// leaks a live preconf entry's transactions. Preferring the live preconf view
// is the eth backend's job (it consults PendingSnapshot first); HeadPendingView
// stays empty so it can never shadow preconfirmed content.
func TestHeadPendingViewIgnoresActivePreconf(t *testing.T) {
	h := startExecHarness(t)
	session := h.session()
	c := session.consumer

	cur := handleOK(t, session, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x41}))
	raw, err := h.transfer(t, 0).MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	handleOK(t, session, recordEntry(raw, cur))
	publishPendingSnapshot(t, session)

	// The live view carries the preconf tx.
	live, _, _, err := c.PendingSnapshot(t.Context())
	if err != nil || live == nil || len(live.Transactions()) != 1 {
		t.Fatalf("live preconf view = %v err=%v", live, err)
	}

	// The head fallback stays empty regardless.
	fallback, _, _, err := c.HeadPendingView()
	if err != nil || fallback == nil || len(fallback.Transactions()) != 0 {
		t.Fatalf("head fallback leaked preconf content: %v err=%v", fallback, err)
	}
}
