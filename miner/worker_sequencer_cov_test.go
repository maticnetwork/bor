package miner

import (
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"time"
)

// A barrier refusal ends the cycle without sealing, logging which refusal
// shape occurred (store holds uncovered records vs a foreign owner), and
// production resumes as soon as the contest clears.
func TestBarrierRefusalDiscardsTheCycle(t *testing.T) {
	w, b, rec := newSequencerTestWorker(t)

	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())

	rec.mu.Lock()
	rec.contested = true
	rec.resyncN = 1 // first refusal reports the uncovered-records shape
	rec.mu.Unlock()

	w.start()
	defer w.stop()

	// Cycles must keep opening blocks while every one is refused pre-seal.
	waitOpens := func(n int) {
		t.Helper()

		deadline := time.Now().Add(10 * time.Second)

		for {
			opens, seals, _, _ := rec.snapshot()
			if len(seals) != 0 {
				t.Fatal("a contested window must never reach the seal hook")
			}

			if len(opens) >= n {
				return
			}

			if time.Now().After(deadline) {
				t.Fatalf("worker stopped cycling: %d opens", len(opens))
			}

			// A refused cycle seals nothing, so no chain event triggers the
			// next one; nudge the work loop the way a new task would.
			w.start()
			time.Sleep(20 * time.Millisecond)
		}
	}

	waitOpens(2) // consumes the resync-shaped refusal and a foreign-owner one

	rec.mu.Lock()
	rec.contested = false
	rec.mu.Unlock()

	deadline := time.Now().Add(10 * time.Second)

	for {
		_, seals, _, _ := rec.snapshot()
		if len(seals) > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("sealing must resume once the contest clears")
		}

		w.start()
		time.Sleep(20 * time.Millisecond)
	}
}

// The continuous-fill loop stops the moment the store elects a competing
// producer for the height: the build is abandoned for a rebuild on the
// store's sequence.
func TestFillLoopStopsForTheStoreSequence(t *testing.T) {
	t.Parallel()

	w, _, rec := newSequencerTestWorker(t)
	rec.refresh = 25 * time.Millisecond
	rec.resyncN = 1
	w.start()

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	work.header.ActualTime = time.Now().Add(2 * time.Second)

	if err := w.fillBlock(nil, work, genParams); !errors.Is(err, errRebuildForSequence) {
		t.Fatalf("fill must stop for the store's sequence, got %v", err)
	}
}

// haltFill only consults the store when sequencing is active for the
// height; a nil sequencer never signals a rebuild.
func TestHaltFillInactiveSequencer(t *testing.T) {
	t.Parallel()

	w, _, rec := newSequencerTestWorker(t)
	rec.resyncN = 1

	saved := w.sequencer
	w.sequencer = nil

	defer func() { w.sequencer = saved }()

	if err := w.haltFill(nil, big.NewInt(1)); err != nil {
		t.Fatalf("inactive sequencing must not halt the fill: %v", err)
	}
}

// A height the store elected another producer for is discarded mid-fill:
// the whole build cycle ends without sealing, and the consumed signal
// leaves the next cycle free to follow the store's sequence.
func TestStoreOwnedHeightDiscardsTheBuild(t *testing.T) {
	w, _, rec := newSequencerTestWorker(t)
	rec.refresh = 20 * time.Millisecond
	rec.resyncN = 3
	// The store owns this height: the barrier refuses every build for it,
	// which the fill-loop resync abort races to beat. Marking it contested
	// makes that refusal unconditional so a build the background loop or a
	// slow fill lets slip past the abort still never seals — otherwise the
	// assertion races the seal under constrained scheduling.
	rec.contested = true
	w.start()
	defer w.stop()

	// Read the cycle's prepared context once so the adopted window passes
	// every bound; adoption is what arms the announce deadline the
	// continuous fill loop runs against.
	probe := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(probe, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	adopt := &AdoptedWindow{
		Number:     work.header.Number.Uint64(),
		Timestamp:  work.header.Time,
		ParentHash: work.header.ParentHash,
		GasLimit:   work.header.GasLimit,
		BaseFee:    new(big.Int).Set(work.header.BaseFee),
	}
	work.discard()

	genParams := &generateParams{coinbase: testBankAddress, adoption: adopt}

	var prefetch atomic.Bool

	w.buildAndCommitBlock(nil, true, genParams, &prefetch)

	if _, seals, _, _ := rec.snapshot(); len(seals) != 0 {
		t.Fatal("a store-owned height must never reach the seal hook")
	}
}
