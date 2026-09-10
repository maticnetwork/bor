package sequencer

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// coverageSkip records a seal allowed through without a proven-complete
// window. Each reason is a different way this block can be sealed short of
// what the store holds, so they are worth telling apart rather than
// collapsing into one silent "true".
func coverageSkip(height uint64, reason string, ctx ...any) {
	log.Info("Coverage check skipped",
		append([]any{"number", height, "reason", reason}, ctx...)...)
}

// AwaitSequenced blocks until the block about to be sealed is provably the
// store's sequence at this height. The rule is a mirror: seal only a block
// whose content is exactly the store's live window.
//
// The store elects the owner of every height: writes are a compare-and-swap
// on its head, so exactly one producer's open lands and everyone else
// STALEs. The owner's window is the store, its mirror check passes, and it
// seals — liveness by construction, no tie-break needed. A producer that
// finds itself behind adopts the store's window and rebuilds; false from
// here means exactly that (the resync signal is armed).
//
// A store that is merely unreachable, slow, or catching up after an outage
// returns true on the deadline: block production never waits on the store.
// A build the classification muted published nothing, so it seals without
// a mirror; its seal flush reconciles the store afterward.
func (p *Publisher) AwaitSequenced(timeout time.Duration, number uint64, txs []*types.Transaction) bool {
	deadline := time.Now().Add(timeout)

	for {
		if p.failed.Load() {
			return true // publishing is off; it must not gate production
		}

		p.mu.Lock()
		unacked := p.unackedLocked()
		sticky := p.hold.kind == holdSticky
		catchingUp := p.pendingFrom != 0
		muted := p.mode.mutedAt(number)
		p.mu.Unlock()

		switch {
		case muted:
			// The build-start read already resolved this height (sealed
			// ground, or a window on a displaced parent) and silenced the
			// build: nothing was published, so there is no window to
			// mirror, and a hold — the foreign window's watchdog re-arms
			// one throughout the build — gates entries this build never
			// wrote. Refusing here is what wedged production on a reorg.
			coverageSkip(number, "build muted at classification")

			return true
		case sticky:
			// A STALE armed this hold. Usually another producer owns the
			// height and the rebuild adopts their window — but a takeover
			// publisher's own stale-prefix entries STALE the same way with
			// nobody owning anything, and refusing then starves the very
			// seal flush the transport is holding for (the devnet
			// seventeen-second takeover stall). The store arbitrates: a
			// tail sealed exactly through this block's parent with nothing
			// live past it proves the height unowned, and the seal passes;
			// the simultaneous-open race stays with the CAS and the gate.
			if p.sealedThroughParent(number) {
				coverageSkip(number, "store sealed through parent, nothing owed here")

				return true
			}

			p.armResync()

			return p.barrierVerdict(number, false)
		case catchingUp:
			// The store is behind us by construction while the backfill
			// drains, so there is nothing here worth comparing against and
			// nothing worth waiting for. Seal now; the mirror check resumes
			// once the drain finishes.
			publishCatchupSkip.Inc(1)

			return true
		case unacked == 0:
			return p.barrierVerdict(number, p.sealMirror(number, txs))
		case time.Now().After(deadline):
			publishBarrierTimeout.Inc(1)

			// The drain did not finish in budget, but that says nothing
			// about whether this block carries what the store promised —
			// and surrendering here drops the content check exactly when
			// load makes it matter. A window still draining is a prefix of
			// our own block, so the comparison holds mid-drain; it was a
			// block with none of a 9523-record window that rode this exit
			// out to a broadcast.
			return p.barrierVerdict(number, p.sealMirror(number, txs))
		}

		time.Sleep(2 * time.Millisecond)
	}
}

// sealedThroughParent reports the clean-boundary proof: nothing stands in
// the store at or past this height, so a seal here can revoke nothing. One
// walk anchored at the parent answers it — an unsealed parent is served
// from its open (a non-empty walk), a sealed one resumes just past its
// seal, and an unknown one reads NOT_FOUND with nothing standing anywhere.
// Any entry the walk returns — a live window, a later generation, a
// re-anchor — is content this block cannot vouch for, and the refusal
// stands, as it does for anything unreadable.
func (p *Publisher) sealedThroughParent(height uint64) bool {
	if height < 2 || p.unreachable.Load() || p.read == nil || p.read.cons == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), tailReadTimeout)
	defer cancel()

	info, out, done := p.tryWalk(ctx, blockReq(height-1), false)

	return done && out == recOK && !info.tipOpen && !info.haveSeal && len(info.window) == 0
}

// maxBarrierRefusals bounds consecutive pre-seal refusals at one height —
// the barrier's analog of the gate's maxGateRefusals. Sized past every
// legitimate convergence (adoption is one rebuild; recovery's grace is a
// few slots) and below heimdall's producer-ineffectiveness threshold, so a
// store answering inconsistently across reads costs bounded slots and the
// node seals for liveness before consensus rotates it away as dead.
const maxBarrierRefusals = 8

// barrierVerdict funnels every pre-seal pass/refuse through the per-height
// refusal cap: a pass resets the streak, a refusal past the cap becomes a
// pass — refuse → rebuild is convergent only when the store eventually
// shows one consistent shape, and a Byzantine or split-view store (a load
// balancer over replicas at different lags) never does. Past the cap the
// post-seal gate and consensus arbitrate.
func (p *Publisher) barrierVerdict(height uint64, ok bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if ok {
		p.barrierRefusals = refusalStreak{}

		return true
	}

	if p.barrierRefusals.height != height {
		p.barrierRefusals = refusalStreak{height: height}
	}

	p.barrierRefusals.count++

	if p.barrierRefusals.count > maxBarrierRefusals {
		log.Warn("Sequencer barrier refused this height repeatedly, sealing for liveness",
			"number", height, "refusals", p.barrierRefusals.count)

		return true
	}

	return false
}

// armResync requests a rebuild that follows the store's sequence. Idempotent;
// the worker consumes it via ResyncNeeded.
func (p *Publisher) armResync() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.resync {
		p.resync = true
		reconcileResync.Inc(1)
	}
}

// sealMirror reports whether the block about to be sealed is the store's
// sequence at this height.
//
// The comparison is against the block's own transactions, not against our
// journal position. A block built before an adoption leaves the journal
// perfectly in sync with the store while the block itself carries different
// content — position agreement says nothing about the transactions we are
// about to broadcast, and those are what the store promised.
func (p *Publisher) sealMirror(height uint64, txs []*types.Transaction) bool {
	if p.unreachable.Load() {
		coverageSkip(height, "store unreachable")

		return true // production never waits on a store we cannot reach
	}

	ctx, cancel := context.WithTimeout(context.Background(), tailReadTimeout)
	defer cancel()

	info, out := p.readTail(ctx)
	if out != recOK {
		// A window we cannot read is not a window we can prove divergent,
		// and production never waits on the store.
		coverageSkip(height, "tail unreadable", "outcome", int(out))

		return true
	}

	if info.haveSeal && info.lastSealHeight >= height {
		coverageSkip(height, "height already sealed in the store")

		return true // the height moved on without us: the next build mutes
	}

	// The store's live window at our height is the sequence this block owes
	// its consumers. It must appear in the block, in order, from the front:
	// anything else means transactions were promised at this height that
	// the block does not deliver there.
	if info.tipOpen && info.tipOpenHeight == height {
		if storeTxs, ok := windowTxHashes(info.window); ok {
			return p.mirrorVerdict(height, storeTxs, txs, len(storeTxs))
		}
	}

	if ok, decided := p.journalMirror(info, height, txs); decided {
		return ok
	}

	// A tail ending in a decoded seal exactly at this block's parent, with
	// nothing live past it, proves the store owes nothing at this height —
	// a broadcast can revoke nothing, whatever our anchor thinks. This is
	// the takeover shape: this publisher idled muted while another
	// produced, its anchor is behind on sealed history it already imported
	// as blocks, and the seal flush's STALE rebases it right after.
	// Refusing here wedged a fresh producer for 17 seconds on a devnet —
	// the reconcile held every rebuild's open awaiting a seal flush this
	// refusal was preventing — until heimdall rotated production away as
	// ineffective.
	if p.sealedThroughParent(height) {
		coverageSkip(height, "store sealed through parent, nothing owed here")

		return true
	}

	// The store holds content this block does not — a record extension of
	// our window, or a generation we have not absorbed. Either way the
	// answer is the same: adopt the store's sequence and rebuild on it.
	// Never seal beside it, never republish over it.
	p.mu.Lock()
	p.resync = true
	p.hold = hold{after: p.ackedSeq, kind: holdSticky}
	reconcileResync.Inc(1)
	ours := len(p.journal.suffixFromHeight(height))
	p.mu.Unlock()

	log.Warn("Store holds content this block does not cover, adopting",
		"number", height, "ours", ours, "storeWindow", len(info.window))

	return false
}

// journalMirror compares the block against this publisher's own published
// window when the read proves the store holds nothing else: the tail ends at
// our anchor, or the walked suffix is a verified prefix of our unacked
// journal (the mid-drain read — acks lag the commits, the walk starts past
// the window's open, and the matcher proved every returned record ours).
// Either way the store's window at this height is the journal's, so the
// journal comparison is the store comparison. Undecided when the tail holds
// content we cannot account for.
func (p *Publisher) journalMirror(info tailInfo, height uint64, txs []*types.Transaction) (ok, decided bool) {
	p.mu.Lock()
	ourTxs, atHead := p.journalWindowLocked(height), info.s == p.anchor
	p.mu.Unlock()

	if !atHead && !info.suffixOurs {
		return false, false
	}

	if !atHead {
		publishMidDrainMirror.Inc(1)
	}

	return p.mirrorVerdict(height, ourTxs, txs, len(ourTxs)), true
}

// ResyncNeeded reports whether a competing producer holds the height this
// node is building, meaning this build must stop — the next work cycle's
// build-start read follows the store's sequence. Reading consumes the
// signal; it fires at most once per height.
func (p *Publisher) ResyncNeeded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.resync {
		return false
	}

	p.resync = false

	return true
}

// mirrorVerdict passes a block that delivers the promised sequence and arms
// a rebuild for one that does not.
func (p *Publisher) mirrorVerdict(height uint64, promised []common.Hash,
	txs []*types.Transaction, window int,
) bool {
	// A partial adoption holds the block to the window's executable prefix:
	// the worker proved the entry past it unexecutable on canonical state,
	// so no block at this height can carry it, and refusing would re-adopt
	// the same window and re-diverge forever. The seal flush supersedes the
	// remainder. A build that never adopted keeps the full mirror — a
	// dropped executable transaction must still refuse.
	if matched, partial := p.adoptedPrefix(height); partial && matched <= len(promised) {
		promised = promised[:matched]
	}

	if windowLeadsHashes(promised, txHashes(txs)) {
		return true
	}

	barrierDivergedCount.Inc(1)
	log.Warn("Sequencer block does not carry the sequence promised at this height, rebuilding",
		"number", height, "promised", window, "block", len(txs))

	p.mu.Lock()
	p.resync = true
	p.hold = hold{after: p.ackedSeq, kind: holdSticky}
	reconcileResync.Inc(1)
	p.mu.Unlock()

	return false
}
