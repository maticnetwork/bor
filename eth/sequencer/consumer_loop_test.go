package sequencer

import (
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// appendOK appends one entry to the harness store, failing on a rejected
// prefix, and returns the advanced head for the next entry.
func appendOK(t *testing.T, h *harness, entry *pb.Entry) commitment.Head {
	t.Helper()

	if status := h.store.Append(entry); status != pb.AckStatus_ACK_STATUS_OK {
		t.Fatalf("append rejected: %v", status)
	}

	return h.store.Head()
}

// precomputedSeal re-executes the block the way the consumer will and
// returns the sealed header a correct producer would announce.
func precomputedSeal(t *testing.T, chain *core.BlockChain, env *blockEnv) *types.Header {
	t.Helper()

	header := types.CopyHeader(env.header)
	header.Difficulty = big.NewInt(1)
	body := &types.Body{Transactions: append(types.Transactions(nil), env.txs...)}
	assembled, _, _, err := chain.Engine().FinalizeAndAssemble(chain, header, env.statedb.Copy(), body, cloneReceipts(env.receipts))
	if err != nil {
		t.Fatalf("finalize test seal: %v", err)
	}
	return assembled.Header()
}

// The full follow loop against a live store: stream, re-execute, serve the
// receipt, evict on canonical import, survive a store bounce with a warm
// resume, and keep consuming.
func TestConsumerFollowsTheStore(t *testing.T) {
	ex := startExecHarness(t)
	h := startHarness(t)
	head := ex.chain.CurrentBlock()

	// Height N+1, staged in the store before the consumer starts (the
	// replay path). The seal is precomputed by re-executing locally —
	// identical inputs, identical results.
	cur := h.store.Head()
	open1 := openOn(head, ex.config, cur)
	cur = appendOK(t, h, open1)

	tx1 := ex.transfer(t, 0)
	raw1, _ := tx1.MarshalBinary()
	cur = appendOK(t, h, recordEntry(raw1, cur))

	statedb, err := ex.chain.StateAt(head.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	env1 := newBlockEnv(ex.chain, statedb, open1.GetBlockOpen(), nil)
	if _, _, err := env1.applyRaw(raw1); err != nil {
		t.Fatalf("local re-execution: %v", err)
	}

	sealed1 := precomputedSeal(t, ex.chain, env1)
	appendOK(t, h, sealEntry(encodeHeader(t, sealed1), cur))

	consumer, err := NewConsumer(h.addr, ex.chain)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	consumer.Start()
	defer consumer.Close()

	waitFor(t, 5*time.Second, func() bool {
		receipt, _, ok := consumer.Index().Lookup(tx1.Hash())
		pending, _, _ := consumer.Pending()
		return ok && receipt.BlockHash == sealed1.Hash() && pending != nil
	})

	// Canonical import of the same height evicts the preconf receipt: the
	// chain serves it from here on.
	if _, err := ex.chain.InsertChain(types.Blocks{ex.next}, false); err != nil {
		t.Fatalf("insert next: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		_, _, ok := consumer.Index().Lookup(tx1.Hash())
		pending, _, _ := consumer.Pending()
		return !ok && pending == nil
	})

	// Store bounce: the session survives on a warm resume and keeps
	// consuming entries appended after the restart.
	h.stop()
	h.resume()

	cur = h.store.Head()
	canonical := ex.chain.CurrentBlock()
	open2 := openOn(canonical, ex.config, cur)
	cur = appendOK(t, h, open2)

	tx2 := ex.transfer(t, 0)
	raw2, _ := tx2.MarshalBinary()
	cur = appendOK(t, h, recordEntry(raw2, cur))

	state2, err := ex.chain.StateAt(canonical.Root)
	if err != nil {
		t.Fatalf("state 2: %v", err)
	}
	env2 := newBlockEnv(ex.chain, state2, open2.GetBlockOpen(), nil)
	if _, _, err := env2.applyRaw(raw2); err != nil {
		t.Fatalf("local re-execution 2: %v", err)
	}

	appendOK(t, h, sealEntry(encodeHeader(t, precomputedSeal(t, ex.chain, env2)), cur))

	waitFor(t, 10*time.Second, func() bool {
		_, _, ok := consumer.Index().Lookup(tx2.Hash())

		return ok
	})
}

func TestNewConsumerRequiresABorChain(t *testing.T) {
	ex := startExecHarnessBor(t, nil)

	if _, err := NewConsumer("127.0.0.1:0", ex.chain); err == nil {
		t.Fatal("a non-bor chain must be rejected")
	}
}

// deterministic gates the session on a reproducible execution context.
func TestConsumerDeterminismGate(t *testing.T) {
	preRio := startExecHarnessBor(t, &params.BorConfig{
		RioBlock:      big.NewInt(1_000_000),
		BurntContract: map[string]string{"0": "0x000000000000000000000000000000000000dead"},
	})

	c := &Consumer{chain: preRio.chain}
	if err := c.deterministic(); err == nil {
		t.Fatal("pre-rio head must not be deterministic")
	}

	noCoinbase := startExecHarnessBor(t, &params.BorConfig{
		RioBlock:      big.NewInt(0),
		BurntContract: map[string]string{"0": "0x000000000000000000000000000000000000dead"},
	})

	c = &Consumer{chain: noCoinbase.chain}
	if err := c.deterministic(); err == nil {
		t.Fatal("a zero coinbase map must not be deterministic")
	}

	rio := startExecHarness(t)

	c = &Consumer{chain: rio.chain}
	if err := c.deterministic(); err != nil {
		t.Fatalf("rio with a coinbase map must be deterministic: %v", err)
	}
}

// The resume ladder never asks the same anchor twice: a warm session walks
// head, block anchor, earliest; a cold one has no head rung and goes
// straight from the block anchor to the earliest retained entry.
func TestResumeRequestLadder(t *testing.T) {
	ex := startExecHarness(t)
	c := &Consumer{chain: ex.chain, index: NewIndex()}

	kind := func(r *pb.StreamRequest) string {
		switch r.After.(type) {
		case *pb.StreamRequest_Head:
			return "head"
		case *pb.StreamRequest_Block:
			return "block"
		default:
			return "earliest"
		}
	}

	warm := &session{consumer: c, seeded: true}
	for attempt, want := range []string{"head", "block", "earliest"} {
		if got := kind(c.resumeRequest(warm, attempt)); got != want {
			t.Fatalf("warm attempt %d: %s, want %s", attempt, got, want)
		}
	}

	for _, sess := range []*session{nil, {consumer: c}} {
		for attempt, want := range []string{"block", "earliest"} {
			if got := kind(c.resumeRequest(sess, attempt)); got != want {
				t.Fatalf("cold attempt %d (fresh session=%v): %s, want %s",
					attempt, sess != nil, got, want)
			}
		}
	}
}
