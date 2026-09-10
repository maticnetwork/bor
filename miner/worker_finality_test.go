package miner

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum/go-ethereum/common"
)

// These share the package-level finalityGrace, so they must not run in
// parallel with each other.

// A restarting producer must not build until finality has spoken about the
// chain it holds. The failure this prevents: a same-key node came back from
// a restart, synced a head, and mined a four-block private fork before its
// milestone view caught up — every peer refused the whole lineage with a
// whitelist mismatch, and the blocks were reorged away.
func TestFinalityGateWaitsForAMilestoneAfterRestart(t *testing.T) {
	w, b, _ := newSequencerTestWorker(t)

	if w.finalityConfirmed() {
		t.Fatal("a fresh producer with no milestone built immediately: this " +
			"is the window where a restart mines onto a fork finality has " +
			"already rejected")
	}

	// A milestone naming our own chain confirms the fork.
	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())

	if !w.finalityConfirmed() {
		t.Fatal("a milestone matching our canonical chain must release the gate")
	}
}

// Confirmation latches: steady-state production must not re-check, or a
// producer would stall every time the milestone view lagged its own head.
func TestFinalityGateLatchesOnceConfirmed(t *testing.T) {
	w, b, _ := newSequencerTestWorker(t)

	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())

	if !w.finalityConfirmed() {
		t.Fatal("gate did not open on a matching milestone")
	}

	// A milestone that no longer matches (our head has moved on, as it
	// always does) must not re-close the gate.
	b.setMilestone(head.Number.Uint64()+5, common.Hash{0x99})

	if !w.finalityConfirmed() {
		t.Fatal("gate re-closed after confirming: a producer would stall " +
			"whenever finality lagged its own head, which is always")
	}
}

// A milestone that names a block we do not have is proof we are on a
// rejected fork. Refuse for as long as that holds — the grace window is for
// ambiguity, not for contradiction.
func TestFinalityGateRefusesAConflictingChainPastGrace(t *testing.T) {
	w, _, _ := newSequencerTestWorker(t)

	restore := finalityGrace
	finalityGrace = 0 // grace already expired
	t.Cleanup(func() { finalityGrace = restore })

	w.eth.(*testWorkerBackend).setMilestone(1, common.Hash{0xbe, 0xef})

	if w.finalityConfirmed() {
		t.Fatal("built on a chain that provably conflicts with the " +
			"whitelisted milestone: every block extends ground that is " +
			"already reorged away")
	}
}

// Liveness: with no milestone at all — a fresh chain, or Heimdall down — the
// grace expires and production proceeds. A producer that never hears from
// Heimdall must pause, not halt.
func TestFinalityGateOpensAfterGraceWithoutMilestone(t *testing.T) {
	w, _, _ := newSequencerTestWorker(t)

	restore := finalityGrace
	finalityGrace = 0
	t.Cleanup(func() { finalityGrace = restore })

	if !w.finalityConfirmed() {
		t.Fatal("a producer with no milestone never started: a Heimdall " +
			"outage must cost a pause, not the chain")
	}
}

// The grace measures eligibility, not uptime. A restart's resync never
// consults the gate (commitWork returns on syncing first), so a resync
// that outlives the grace would otherwise consume it silently and open
// the gate the instant sync completes — the exact window the gate covers.
func TestFinalityGraceRearmsWhenSyncCompletes(t *testing.T) {
	w, _, _ := newSequencerTestWorker(t)

	// Age the anchor past the grace, as a long resync does.
	w.eligibleSince.Store(time.Now().Add(-2 * finalityGrace).UnixNano())

	if !w.finalityConfirmed() {
		t.Fatal("sanity: an expired grace with no milestone opens the gate")
	}

	// Sync completion re-arms (miner's DoneEvent/FailedEvent handling).
	w.rearmFinalityGrace()

	if w.finalityConfirmed() {
		t.Fatal("the gate opened with zero effective wait after sync: the " +
			"grace must restart when the node becomes eligible to build")
	}
}

// The gate must actually stop production, not merely report. Driven through
// the running worker's own loops — commitWork is the single path every build
// takes, and this asserts the wiring, not the predicate.
func TestFinalityGateBlocksProduction(t *testing.T) {
	w, b, rec := newSequencerTestWorker(t)

	b.txPool.Add([]*types.Transaction{b.newRandomTxWithNonce(false, 0)}, true)

	// No milestone, grace running: the gate is shut.
	w.start()
	defer w.stop()

	time.Sleep(1500 * time.Millisecond)

	if opens, _, _, _ := rec.snapshot(); len(opens) != 0 {
		t.Fatalf("produced %d blocks while finality had not confirmed our "+
			"chain: this is the restart window that mined a doomed fork",
			len(opens))
	}

	// Confirm our chain, then retrigger. In production the veblop fallback
	// does this every block period for a stalled producer; the clique test
	// engine skips that path, so poke startCh directly.
	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())
	w.start()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if opens, _, _, _ := rec.snapshot(); len(opens) > 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("no production after finality confirmed the chain: the " +
				"gate never reopens and the producer is stuck")
		}

		time.Sleep(50 * time.Millisecond)
	}
}
