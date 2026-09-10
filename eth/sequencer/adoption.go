package sequencer

import (
	"context"
	"math/big"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
)

// checkTailTimeout bounds the build-start tail read: past it
// the block is built from the pool with the publisher buffering silently.
var checkTailTimeout = 250 * time.Millisecond

// noHold disables the send loop's send ceiling.
const noHold = ^uint64(0)

// buildMode is the build treatment for the current height, decided once by
// the build-start classification and consumed by OpenBlock/PublishTx (mute)
// and SealBlock (gate arming). It replaces three formerly loose flags whose
// legal combinations were enforced only by discipline.
type buildMode struct {
	kind   int
	height uint64 // the height the treatment was decided for (modeOpen: unused)
}

const (
	modeOpen       = iota // publish normally
	modeMuted             // build at/behind a sealed height: publish nothing
	modeSealedWait        // muted over a store-closed height whose block is still owed
	modeRecover           // rebuilding a sealed generation the chain never received
	// modeOverSealed is muted at a height the store sealed but recovery
	// declined: the liveness fallback, whose broadcast the store's standing
	// seal must not refuse — refusing every build here would halt the chain
	// at this block forever.
	modeOverSealed
)

// mutedAt reports whether the classification muted the build at this height.
// A muted build publishes nothing, so the seal barrier has no window to
// mirror and no hold can apply to it — the seal flush (or the gate, for
// modeSealedWait) resolves the height instead.
func (m buildMode) mutedAt(number uint64) bool {
	return m.height == number &&
		(m.kind == modeMuted || m.kind == modeSealedWait || m.kind == modeOverSealed)
}

// divergence records a partial adoption: the worker proved the adopted
// window's transaction at index matched unexecutable on canonical state,
// dropped it, and built on. No block at this height can honor the window
// past that point, so the seal barrier holds the block to window[:matched]
// and the seal flush supersedes the remainder. The zero value records
// nothing (heights start at 1).
type divergence struct {
	height  uint64
	matched int
}

// adoptedPrefix reports the executable-prefix length of a partially adopted
// window at this height: a recorded divergence (a mid-window drop), or a
// still-engaged adoption short of a full match (a trailing drop — nothing
// committed past the prefix, so no mismatch ever fired). ok is false when
// the build did not partially adopt, and the full-window mirror applies.
func (p *Publisher) adoptedPrefix(height uint64) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.diverged.height == height {
		return p.diverged.matched, true
	}

	if a := p.adopt; a != nil && a.engaged && a.number == height && a.idx < len(a.txs) {
		return a.idx, true
	}

	return 0, false
}

// keepNone is the keepFrom value for a refold that keeps no suffix: every
// unacked entry is abandoned to supersession convergence.
const keepNone = ^uint64(0)

// hold is the send loop's send ceiling: entries with seq above after stay
// buffered in the journal. A build-start hold gates only the new build's
// entries (after = last existing seq), so a prior seal flush always
// finishes draining; a mid-block STALE hold gates everything unacked
// (after = ackedSeq). kind decides the release: holdBuild lifts the moment
// the flush it ordered behind is home — batching a window until its own
// seal would defeat mid-block streaming — while holdSticky persists until
// a seal or reconcile resolves the lineage. The cleared state pairs
// noHold with holdNone; use clearedHold(), never the zero value.
type hold struct {
	after uint64
	kind  int
}

func clearedHold() hold { return hold{after: noHold, kind: holdNone} }

func (h hold) active() bool { return h.kind != holdNone }

func (h hold) gates(seq uint64) bool { return seq > h.after }

// Send-ceiling kinds. Only the self-release behavior is branched on:
// a build-start hold lifts when the flush it ordered behind drains, while a
// sticky hold (foreign tail, adopt, or mid-block STALE) persists until a
// seal or reconciliation resolves the lineage.
const (
	holdNone = iota
	holdBuild
	holdSticky
)

// adoption tracks a store window being adopted — a three-state machine:
// p.adopt == nil (none) -> armed (returned by AdoptWindow, awaiting the
// engaging open; awaitOpen is true exactly then) -> engaged (idx tracks
// the next PublishTx match). It ends by full match, divergence rewind,
// unadopt, or a seal flush.
type adoption struct {
	number    uint64
	timestamp uint64
	parent    common.Hash
	gasLimit  uint64
	baseFee   *big.Int
	base      commitment.Head // store position the window folds on (its open's prefix)

	txs     []*types.Transaction // the window's transactions, decoded from store bytes
	idx     int                  // next expected PublishTx match
	engaged bool
}

// buildStartState is the store's relationship to the height about to be
// built, resolved from the parent's seal boundary.
// AdoptWindow is the worker's build-start check, and the whole of Rule 1:
// an open may be published only at a seal boundary. The store elects the
// owner of every height — writes are a CAS on its head — and this read is
// how a build respects the election before its open is even attempted:
//
//	sealed at/past this height  -> mute (the height is closed)
//	live window on our parent   -> adopt (the store has an owner; extend it)
//	clean boundary              -> open (we are the owner-elect; the CAS
//	                               settles the simultaneous-open race)
//	store behind our parent     -> hold + prime the backfill (outage)
//	store unreadable            -> hold (build and buffer; the flush repairs)
func (p *Publisher) AdoptWindow(number uint64, parent common.Hash) *miner.AdoptedWindow {
	if p.failed.Load() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTailTimeout)
	defer cancel()

	info, state := p.buildStartRead(ctx, number)

	// An armed offer the previous work cycle never consumed (its build died
	// before opening) stays servable — without it this read would mistake
	// our own absorbed window for a clean boundary.
	if w, handled := p.reoffer(ctx, number, parent, info); handled {
		return w
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Every non-reoffer classification starts disarmed and unmuted, and a
	// resync signal armed during the previous cycle dies here: this fresh
	// read supersedes whatever that signal saw, so it must not abort the
	// build it just classified.
	p.adopt = nil
	p.mode = buildMode{}
	p.resync = false
	p.diverged = divergence{}

	p.advanceStoreSealedTipLocked(info, "build-start read")

	// Lost head race, or a reorg rebuilding sealed ground — no phantom open
	// on our own sealed tip. If it seals after all, the flush rebuild
	// publishes the complete window.
	if number <= p.sealedTip {
		return p.muteLocked(number)
	}

	// A draining flush is not a foreign tail — gate only the new build's
	// entries so the flush finishes delivering first.
	if _, flushing := p.pendingFlushLocked(); flushing {
		p.holdNewLocked(holdBuild)

		return nil
	}

	if w, handled := p.stateWindowLocked(info, state, number, parent); handled {
		return w
	}

	// buildAtBoundary: info is exactly this height's live state.
	return p.boundaryWindowLocked(info, number, parent)
}

// stateWindowLocked resolves every build-start state short of the boundary
// case; handled reports whether the state was one of them.
func (p *Publisher) stateWindowLocked(info tailInfo, state buildStartState,
	number uint64, parent common.Hash,
) (*miner.AdoptedWindow, bool) {
	switch state {
	case buildUnknown:
		// Blind at build start: build and buffer; the transport goroutine
		// keeps probing and the seal flush repairs.
		log.Warn("Sequencer could not read the store at build start", "number", number)
		p.holdNewLocked(holdBuild)

		return nil, true

	case buildSealedPast:
		w := p.muteLocked(number)
		// Recovery declined this store-sealed height (the chain already has
		// the block, or its generation cannot be fetched). A build that
		// still seals here is the liveness fallback the gate must let
		// through.
		p.mode.kind = modeOverSealed

		return w, true

	case buildSealedWait:
		// The store closed this height and its block is still owed to us.
		// Refusing the broadcast is safe only while the rebuild is still
		// coming: once the grace elapses this height recovers properly, so
		// the refusal cannot outlive it and strand the chain here.
		w := p.muteLocked(number)
		p.mode.kind = modeSealedWait

		return w, true

	case buildRecover:
		// Rebuild the dead producer's block exactly: its transactions were
		// acked, so they are promised at this height. The store already
		// holds the whole generation, so publish nothing — mute — and hand
		// the content to the worker to seal and broadcast.
		a, _, ok := parseWindow(info)
		if !ok || a.number != number || a.parent != parent {
			return p.muteLocked(number), true
		}

		p.mode = buildMode{kind: modeRecover, height: number}

		return offerLocked(a), true

	case buildBehind:
		// The store is owed every block from its seal edge to our parent.
		// Build and buffer: the backfill drains oldest-first, and the live
		// window follows only once the store reaches our boundary.
		p.primeBackfillLocked(info, number)
		p.holdNewLocked(holdBuild)

		return nil, true
	}

	return nil, false
}

// boundaryWindowLocked resolves a build start whose height holds live
// state: a clean boundary opens, our parent's live window is adopted, and
// anything else builds locally without publishing.
func (p *Publisher) boundaryWindowLocked(info tailInfo, number uint64, parent common.Hash) *miner.AdoptedWindow {
	p.discardLostLocked(info)

	if len(info.window) == 0 {
		// The parent's seal is the store head: a clean boundary, and the
		// open publishes. Sync our lineage to the head when nothing of ours
		// is pending, so the open folds onto the true tip.
		if p.head == p.anchor && info.s != p.anchor {
			p.rebaseLocked(info.s)
		}

		p.hold = clearedHold()

		return nil
	}

	// A live window at this height: the store already has an owner.
	a, items, ok := parseWindow(info)
	if ok && a.number == number && a.parent == parent {
		return p.adoptWindowLocked(info, a, items)
	}

	// Unparseable, or on a different parent (reorg territory): the window
	// extends a branch our build parent has displaced, so nothing built on
	// it can seal here. A hold has nothing to wait for — every rebuild
	// re-reads the same dead window — so mute instead: build locally,
	// publish nothing, and the seal flush supersedes the window with
	// sealed truth.
	return p.muteLocked(number)
}

// primeBackfillLocked registers the store's gap [edge+1, parent] as owed
// from the chain database. The collapse machinery already tracks blocks
// sealed while the store was down; this covers a process that restarted
// mid-outage, whose collapsed range died with its journal.
func (p *Publisher) primeBackfillLocked(info tailInfo, number uint64) {
	if number < 2 || p.chain == nil {
		return
	}

	// The range is based on what this read decoded, never the accumulated
	// storeSealedTip: the tip has carried inferred values, and even a real
	// tip proves nothing about heights below it. An undecoded boundary
	// (probe inference) includes the boundary height itself — it may be a
	// partial delivery, and a duplicate generation is cheaper than a hole.
	var from uint64

	switch {
	case info.sealDecoded:
		from = info.lastSealHeight + 1
	case info.haveSeal:
		from = info.lastSealHeight
	}

	if from <= 1 {
		// The store's seal edge is unknown; priming from genesis would
		// republish the world. Leave it to the collapse machinery.
		return
	}

	to := number - 1

	if p.pendingFrom != 0 {
		if p.pendingFrom < from {
			from = p.pendingFrom
		}

		if p.pendingTo > to {
			to = p.pendingTo
		}
	}

	if from > to {
		return
	}

	p.pendingFrom, p.pendingTo = from, to
}

// holdNewLocked gates entries appended from here on while letting
// everything already in the journal (a draining flush included) deliver.
func (p *Publisher) holdNewLocked(kind int) {
	p.hold = hold{after: p.journal.nextSeq - 1, kind: kind}
}

// muteLocked silences the coming build: OpenBlock and PublishTx append
// nothing, so the doomed lineage never reaches the journal or the store.
func (p *Publisher) muteLocked(number uint64) *miner.AdoptedWindow {
	p.mode = buildMode{kind: modeMuted, height: number}

	publishMutedCount.Inc(1)
	log.Debug("Sequencer muting build", "number", number, "sealedTip", p.sealedTip)

	return nil
}

// adoptWindowLocked absorbs an unsealed tail window as confirmed lineage: its
// entries enter the journal as published-and-acked, the head moves to S, and
// the window is handed to the miner. Continuations then publish normally,
// so their acks (or STALEs) settle who owns the height.
func (p *Publisher) adoptWindowLocked(info tailInfo, a *adoption, items []journalItem) *miner.AdoptedWindow {
	if abandoned := p.unackedLocked(); abandoned > 0 {
		publishDropMeter.Mark(int64(abandoned))
	}

	fresh := newJournal()
	fresh.nextSeq = p.journal.nextSeq

	for _, it := range items {
		fresh.append(it.entry, it.pre, it.post, it.kind, it.height, fresh.nextSeq, it.txHashes)
	}

	p.journal = fresh
	p.ackedSeq = fresh.nextSeq - 1
	p.head = info.s
	p.anchor = info.s
	p.anchored, p.confirmed = true, true
	p.curHeight = a.number
	p.awaitOpen = true
	p.adopt = a

	// No hold: continuations publish immediately. Extending the adopted
	// window is how ownership is discovered — an ack means we own this
	// height and may seal, a STALE means another producer is still
	// writing it and the pre-seal barrier must stop us. Holding here
	// would hide both answers and strand a legitimate takeover.
	p.hold = clearedHold()

	publishQueueGauge.Update(int64(p.unackedLocked()))
	reconcileAdopt.Inc(1)
	log.Info("Sequencer adopting store window", "number", a.number, "txs", len(a.txs))

	return offerLocked(a)
}

// reoffer re-serves or refreshes an armed, unengaged offer for the same
// build. handled=false falls through to normal classification, disarmed.
func (p *Publisher) reoffer(ctx context.Context, number uint64, parent common.Hash, info tailInfo) (*miner.AdoptedWindow, bool) {
	p.mu.Lock()

	a := p.adopt
	if a == nil || a.engaged || a.number != number || a.parent != parent {
		p.mu.Unlock()

		return nil, false
	}

	if info.s == p.head {
		defer p.mu.Unlock()

		// The offer is re-served exactly as a fresh adoption would be: any
		// hold armed since (a STALE from the death throes of the previous
		// cycle) is residue of a lineage this adoption replaced, and keeping
		// it would gate the continuations that discover ownership.
		p.hold = clearedHold()

		return offerLocked(a), true // window unchanged since absorption
	}

	p.mu.Unlock()

	return p.readoptGrown(ctx, a, number, parent)
}

// readoptGrown re-reads a window that kept growing after our snapshot from
// its base and re-adopts it in full — sealing the stale snapshot would
// supersede the extra records at the flush. Any failure (read miss, the
// lineage moved while unlocked, the window is no longer ours) falls
// through to normal classification, disarmed. Reading a.base unlocked is
// race-free: it is written once, before p.adopt publishes it.
func (p *Publisher) readoptGrown(ctx context.Context, a *adoption, number uint64, parent common.Hash) (*miner.AdoptedWindow, bool) {
	fresh, out, done := p.tryWalk(ctx, headReq(a.base), false)
	if !done || out != recOK {
		return nil, false
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Bail if the lineage moved while unlocked (a seal flush landing).
	if p.adopt != a || a.engaged {
		return nil, false
	}

	if a2, items, ok := parseWindow(fresh); ok && a2.number == number && a2.parent == parent {
		return p.adoptWindowLocked(fresh, a2, items), true
	}

	return nil, false
}

// offerLocked builds the miner's view of an adopted window.
func offerLocked(a *adoption) *miner.AdoptedWindow {
	return &miner.AdoptedWindow{
		Number:     a.number,
		Timestamp:  a.timestamp,
		ParentHash: a.parent,
		GasLimit:   a.gasLimit,
		BaseFee:    new(big.Int).Set(a.baseFee),
		Txs:        a.txs,
	}
}

// discardLostLocked drops the unacked suffix when a foreign seal has
// covered all of it, re-anchoring on the store head. Entries above the
// sealed tip (an outage buffer awaiting journal replay) are never touched.
func (p *Publisher) discardLostLocked(info tailInfo) {
	lost := p.unackedLocked()
	if lost == 0 || !info.haveSeal {
		return
	}

	for _, it := range p.journal.items {
		if it.seq > p.ackedSeq && it.height > info.lastSealHeight {
			return
		}
	}

	publishDropMeter.Mark(int64(lost))
	p.rebaseLocked(info.s)
	publishQueueGauge.Update(0)
	log.Debug("Sequencer discarded superseded buffer", "entries", lost, "sealedTip", info.lastSealHeight)
}

// unadoptLocked drops an absorbed-but-abandoned adopt back to the
// window's base: the dead window leaves the journal and the next open folds
// onto the base, so a divergent local build supersedes the incumbent
// window through the normal STALE→reconcile path.
func (p *Publisher) unadoptLocked(a *adoption) {
	p.rebaseLocked(a.base)
	p.adopt = nil
	p.awaitOpen = false
	p.curHeight = 0
	p.hold = clearedHold()
}

// rebaseLocked re-anchors an idle lineage (nothing unconfirmed) onto a
// fresh store head, so the next open extends the true tip. It is the
// primitive under three resets of increasing scope — callers own what it
// deliberately does not touch (adopt, hold, curHeight/awaitOpen):
//
//	rebaseLocked            journal/head/anchor only (clean tail; between-blocks anchor)
//	discardLostLocked       + drop metric; onto the store head; hold set by the caller after
//	unadoptLocked           + clears adopt, awaitOpen, curHeight, hold; onto the WINDOW BASE,
//	                          so the divergent open STALEs onto the counted reconcile path
//	installSwapLocked       (classify.go) full swap with refolded content; re-derives the window
//	dropRefusedFlushLocked  (publisher.go) the rewind-side sibling: truncates a refused
//	                          flush off the tail instead of re-anchoring under it
func (p *Publisher) rebaseLocked(s commitment.Head) {
	fresh := newJournal()
	fresh.nextSeq = p.journal.nextSeq

	p.journal = fresh
	p.ackedSeq = fresh.nextSeq - 1
	p.head = s
	p.anchor = s
	p.anchored, p.confirmed = true, true
}

// parseWindow decodes a collected tail window into an adoption candidate and
// the journal items representing its entries, verifying the fold chain ends
// at the store head.
func parseWindow(info tailInfo) (*adoption, []journalItem, bool) {
	if len(info.window) == 0 {
		return nil, nil, false
	}

	open := info.window[0].GetBlockOpen()
	if open == nil {
		return nil, nil, false
	}

	a := &adoption{
		number:    open.GetBlockNumber(),
		timestamp: open.GetBlockTimestamp(),
		parent:    common.BytesToHash(open.GetParentHash()),
		gasLimit:  open.GetGasLimit(),
		baseFee:   new(big.Int).SetBytes(open.GetBaseFee()),
		base:      commitment.Head(open.GetPrefixCommitment()),
	}

	items := make([]journalItem, 0, len(info.window))
	cur := commitment.Head(info.window[0].GetBlockOpen().GetPrefixCommitment())

	for _, entry := range info.window {
		next, err := foldEntry(cur, entry)
		if err != nil {
			return nil, nil, false
		}

		kind := entryOpen

		var hashes []common.Hash

		if rec := entry.GetRecord(); rec != nil {
			kind = entryRecord

			for _, raw := range rec.GetTransactions() {
				tx := new(types.Transaction)
				if err := tx.UnmarshalBinary(raw); err != nil {
					return nil, nil, false
				}

				a.txs = append(a.txs, tx)
				hashes = append(hashes, tx.Hash())
			}
		}

		items = append(items, journalItem{entry: entry, pre: cur, post: next, kind: kind, height: a.number, txHashes: hashes})
		cur = next
	}

	if cur != info.s {
		return nil, nil, false
	}

	return a, items, true
}

// matchesOpen reports whether the worker's open reproduces the adopted
// window's context exactly — any mismatch means the worker rejected the
// inherited context.
func (a *adoption) matchesOpen(number, timestamp uint64, parent common.Hash, gasLimit uint64, baseFee *big.Int) bool {
	return a.number == number && a.timestamp == timestamp &&
		a.parent == parent && a.gasLimit == gasLimit &&
		baseFee != nil && a.baseFee.Cmp(baseFee) == 0
}

// adoptOpenLocked resolves an adopted window against the worker's
// OpenBlock. swallow=true means the open engaged the window and must not
// be appended; false drops the adopt and the open proceeds (buffered:
// hold stays until the seal flush).
func (p *Publisher) adoptOpenLocked(number uint64, timestamp uint64, parent common.Hash, gasLimit uint64, baseFee *big.Int) (swallow bool) {
	a := p.adopt
	if a == nil {
		return false
	}

	if a.engaged || !a.matchesOpen(number, timestamp, parent, gasLimit, baseFee) {
		// The build diverged from the adopted window — the worker
		// rejected the inherited context (a bogus or version-skewed
		// incumbent window; honest producers derive the same context).
		// Undo the absorption back to the window's base so the local open
		// folds there and STALEs against the store's incumbent window:
		// the supersede then lands on the counted reconcile path
		// instead of appending a silent second generation on the store
		// head that no reconcile ever sees.
		p.unadoptLocked(a)

		return false
	}

	a.engaged = true
	p.awaitOpen = false
	p.curHeight = number

	return true
}

// adoptTxLocked matches one committed transaction against the adopted
// window. swallow=true: the transaction is already in the store. A
// mismatch (the worker dropped or replaced a window transaction) rewinds
// the lineage to the matched prefix — the dead window tail leaves the
// journal, the head returns to the prefix, and subsequent commits fold onto
// it, buffered; the seal flush resolves the divergence. Nothing is
// published here.
func (p *Publisher) adoptTxLocked(hash common.Hash) (swallow bool) {
	a := p.adopt
	if a == nil || !a.engaged {
		return false
	}

	// Only reachable for an empty adopted window: a full match clears
	// p.adopt eagerly below.
	if a.idx >= len(a.txs) {
		p.adopt = nil

		return false
	}

	if a.txs[a.idx].Hash() == hash {
		a.idx++
		if a.idx == len(a.txs) {
			p.adopt = nil // fully adopted; continuations buffer normally
		}

		return true
	}

	log.Warn("Sequencer adopted window diverged, deferring to the seal flush",
		"number", a.number, "matched", a.idx, "of", len(a.txs))

	p.rewindToPrefixLocked(a.idx)
	p.diverged = divergence{height: a.number, matched: a.idx}
	p.adopt = nil

	return false // the divergent tx folds onto the rewound prefix, buffered
}

// rewindToPrefixLocked drops the adopted window's unmatched tail from the
// journal and returns the fold head to the end of the matched prefix. At
// call time the journal is exactly the absorbed window (matches append
// nothing; a re-open while engaged unadopts first), so the scan finds the
// window's open first.
func (p *Publisher) rewindToPrefixLocked(matched int) {
	cut := -1
	seen := 0

	for i, it := range p.journal.items {
		if it.kind == entryOpen && it.height == p.curHeight {
			cut = i // at minimum, keep the window's open
		}

		if it.kind != entryRecord {
			continue
		}

		seen += len(it.entry.GetRecord().GetTransactions())
		if seen >= matched && matched > 0 {
			cut = i

			break
		}
	}

	if cut < 0 {
		return
	}

	p.rewindJournalLocked(cut + 1)
	p.ackedSeq = p.journal.items[cut].seq
}
