package sequencer

import (
	"github.com/0xPolygon/sequence-store-proto/commitment"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
)

// applyTail classifies one tail read and applies the corrective action.
// Writes are reserved for replay of our own lineage, gap
// crossings, and the seal flush: a foreign tail with our window unsealed
// is held (the flush resolves it), never superseded.
func (p *Publisher) applyTail(info tailInfo) reconcileOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.advanceStoreSealedTipLocked(info, "reconcile read")

	// Row 1: the store head is on our lineage — retire through it and let
	// the send loop replay the rest. A diverged flush branches *below* the
	// anchor (the adopt rewound to a prefix): its suffix does not extend
	// the store head and must re-anchor onto it instead.
	if info.s == p.anchor {
		if fh, ok := p.pendingFlushLocked(); ok && !p.suffixExtendsLocked(info.s) {
			return p.refoldLocked(info.s, fh, reconcileSupersede)
		}

		return p.finishRow1Locked()
	}

	if seq, ok := p.journal.findPost(info.s); ok {
		p.ackedSeq = seq
		p.anchor = info.s
		p.confirmed = true

		return p.finishRow1Locked()
	}

	return p.classifyForeignLocked(info)
}

func (p *Publisher) finishRow1Locked() reconcileOutcome {
	if items, covered := p.journal.after(p.ackedSeq); covered && p.pendingFrom == 0 {
		if len(items) > 0 {
			reconcileGapfill.Inc(1)
		}

		p.anchored, p.confirmed = true, true

		return recOK
	}

	// Eviction opened a gap behind the replayable suffix: jump, keeping
	// everything a flush still owes the store (the oldest undelivered
	// seal onward), else the current window (always retained).
	keep := p.curHeight
	if h := p.oldestPendingSealLocked(); h != 0 {
		keep = h
	}

	if keep == 0 {
		keep = keepNone
	}

	return p.refoldLocked(p.anchor, keep, reconcileForwardJump)
}

// classifyForeignLocked resolves a foreign tail against what this publisher
// holds, in rank order: a flush in flight first, then the between-blocks
// re-anchor, else hold until our seal resolves the height.
func (p *Publisher) classifyForeignLocked(info tailInfo) reconcileOutcome {
	if out, handled := p.classifyPendingFlushLocked(info); handled {
		return out
	}

	if p.curHeight == 0 {
		return p.classifyBetweenBlocksLocked(info)
	}

	// The store's window at our height may be a prefix of what we hold —
	// our own earlier generation, or an identical build. Absorb it and
	// deliver only the records it lacks. Republishing the whole window
	// instead would write a fresh generation of data the store already
	// has, which is pure duplication: every reader already has the prefix.
	if p.completeExtendedWindowLocked(info, p.curHeight) {
		return recOK
	}

	// A window standing at our own height on our own parent means another
	// producer is building this slot and reached the store first. Adopt it:
	// rebuilding onto their sequence is what keeps the two blocks from
	// diverging, and adoption extends their window rather than replacing
	// it, so nothing already published is revoked. Only this exact shape
	// arms the signal: a transient STALE, a foreign window at another
	// height, or one on a different parent must never restart a
	// legitimate build.
	if ourParent, ok := p.ourOpenParentLocked(); ok &&
		info.tipOpen && info.tipOpenHeight == p.curHeight &&
		info.tipOpenParent == ourParent {
		p.resync = true

		reconcileResync.Inc(1)
		log.Warn("Sequencer found a competing producer at our height, requesting rebuild",
			"number", p.curHeight, "parent", ourParent)
	}

	// Our window is unsealed: nothing we hold outranks the store yet.
	// Buffer everything unacked until the seal flush resolves the height;
	// a build the chain moved past heals at the next build-start check.
	return p.holdStaleLocked()
}

// classifyPendingFlushLocked handles a flush in flight: a sealed trailing
// window with undelivered entries — the sealed, broadcast block overrides
// whatever the store shows at its height. handled=false means either no
// flush is pending, or a canonical seal past the flush outranks it and
// classification continues as if no flush were pending.
func (p *Publisher) classifyPendingFlushLocked(info tailInfo) (reconcileOutcome, bool) {
	flushHeight, ok := p.pendingFlushLocked()
	if !ok {
		return recOK, false
	}

	// The store's standing window may be OUR window extended by records
	// that were still in flight when we read it (a dying producer's drain,
	// an adoption snapshot racing the stream). When it is a strict prefix
	// of what this flush carries, complete it in place — absorbing the
	// store's copy and delivering only the remainder — instead of
	// superseding content that matches ours.
	if p.completeExtendedWindowLocked(info, flushHeight) {
		return recOK, true
	}

	// A seal standing at our own flush height is another producer claiming
	// the same slot (duplicate signing keys), or our own copy already
	// delivered. Superseding it is only right when the chain chose OUR
	// block: if the store's seal is the canonical one, overwriting it
	// would leave the store's newest generation disagreeing with the
	// chain. We lost — yield instead.
	if info.haveSeal && info.lastSealHeight == flushHeight &&
		p.chain != nil && p.foreignSealCanonicalLocked(info) {
		return p.yieldFlushLocked(info), true
	}

	// A foreign seal already stands at our flush height and our own block
	// is still unbroadcast: the store says this height is closed, and the
	// head we would chain onto encodes that seal — we cannot claim not to
	// know. Publishing our window and seal over it would put a second
	// sealed generation at a height the store already closed, for a block
	// that may never exist on any chain.
	//
	// Unconditional while the gate is pending: the chain's opinion is not
	// required, and waiting for it is what let a refold slip through in
	// the ~140ms before the winner's block imported. Once the gate clears
	// (broadcast, or the liveness timeout) this height becomes ordinary
	// repair, where canonicality decides supersede versus yield. A decoded
	// seal also settles the gate itself: this hold is the verdict, not a
	// wait for one.
	if info.haveSeal && info.lastSealHeight == flushHeight &&
		p.gatePendingLocked(flushHeight) {
		p.resolveGateFromSealLocked(info)

		return p.holdStaleLocked(), true
	}

	// Only a sealed height past the flush outranks it. A foreign unsealed
	// open above the flush height is a contender that already lost to this
	// seal — stranding the flush on it would leave the height sealed on
	// chain but forever open in the store.
	if !info.haveSeal || info.lastSealHeight <= flushHeight {
		if !p.mayDisplaceWindowLocked(info, flushHeight) {
			return p.holdStaleLocked(), true
		}

		return p.reanchorFlushLocked(info), true
	}

	// A foreign seal stands above our pending flush. Dropping our seal for
	// it is right only when that lineage is the canonical chain — we were
	// genuinely reorged past. If the chain has not accepted it (a reorg
	// loser, or a lineage not yet imported), the store tail is not
	// authoritative: hold our seal and let our next flush re-anchor onto
	// the canonical chain. Never drop a canonical seal for a non-canonical
	// store tail.
	if !p.foreignSealCanonicalLocked(info) {
		return p.holdStaleLocked(), true
	}

	return recOK, false // canonical seal past the flush outranks it
}

// mayDisplaceWindowLocked decides whether this flush may overwrite a live
// foreign window, having looked at what that window holds.
//
// A re-anchor rebuilds our own entries onto the store head; it never ingests
// what stands behind that head. So displacing a live window silently drops
// every record it holds that our block does not carry — records the store
// already acked, which is to say preconfirmations. That is only defensible
// when our block is the one the chain kept: then the store must end up
// holding our content or it disagrees with the chain. When our block did not
// win, displacing is pure destruction, and holding lets the winner's flush
// resolve the height.
func (p *Publisher) mayDisplaceWindowLocked(info tailInfo, flushHeight uint64) bool {
	// A foreign live window at the flush height is content this flush would
	// overwrite; one above it is re-anchored past just as surely — a silent
	// fold-past exactly there is how two full acked generations ended up
	// buried under an empty sealed block. Either shape needs proof, and a
	// flush the chain ratified still proceeds: the re-anchor only moves our
	// lineage, so a window above stays the newest generation at its own
	// height, where the next build's boundary read adopts it.
	foreignWindow := info.tipOpen && info.tipOpenHeight >= flushHeight &&
		p.relateWindowLocked(info, info.tipOpenHeight) == windowForeign

	sealCandidate := info.sealDecoded && info.lastSealHeight == flushHeight

	if !foreignWindow && !sealCandidate {
		return true // nothing foreign standing here to displace
	}

	// A sealed foreign generation at our height is a stronger claim than a
	// live window, and it gets the same treatment: chaining our flush past
	// it reads the head's bytes without understanding what they encode — a
	// closed height. Only a decoded seal counts, and our own seal already
	// standing there is re-delivery, not displacement.
	ours, sealedHere := p.sealedHashAtLocked(flushHeight)
	foreignSealed := sealCandidate && !(sealedHere && info.lastSealHash == ours)

	if !foreignWindow && !foreignSealed {
		return true // the standing seal is our own re-delivery
	}

	if p.chain == nil {
		return true
	}

	// Only affirmative proof that the chain kept our block licenses a
	// displacement. "Not decided yet" is not proof — on a devnet the
	// winner's block imported 134ms after a displacement made on exactly
	// that reasoning, and the records it destroyed were already acked.
	if !sealedHere || p.chain.GetCanonicalHash(flushHeight) != ours {
		// With the gate still pending this withhold is the verdict: an
		// unbroadcast block can never become canonical, so waiting for the
		// proof would deadlock against producing it. Refuse the broadcast
		// and the rebuild adopts what stands here instead. The liveness
		// fallback's tolerance covers exactly the store's seal — a foreign
		// window still refuses it.
		if p.gatePendingLocked(flushHeight) &&
			(foreignWindow || !p.gate.tolerateSealed) {
			p.gate.verdict = gateLost
		}

		log.Warn("Sequencer withholding flush: the chain has not kept this block",
			"number", flushHeight, "ours", ours)

		return false
	}

	// Only what this block does not carry is actually orphaned, and only a
	// window standing at the flush height itself can be counted truthfully:
	// a displaced foreign seal's generation is not in this read, and the
	// trailing window of a higher height compared against this height's
	// block once reported 1,960 phantom orphans for a displacement that
	// destroyed nothing.
	if foreignWindow && info.tipOpenHeight == flushHeight {
		if orphans := p.orphanedByDisplacementLocked(info, flushHeight); orphans > 0 {
			windowDisplacedRecords.Inc(int64(orphans))
			log.Warn("Sequencer flush displacing acked records this block does not carry",
				"number", flushHeight, "orphaned", orphans)
		}
	}

	return true
}

// orphanedByDisplacementLocked counts the displaced window's transactions
// that our block does not deliver at this height. The caller has already
// established that the canonical block at this height is ours, so the chain
// holds the authoritative copy of what we delivered — the journal has moved
// on by seal time and no longer describes the window.
func (p *Publisher) orphanedByDisplacementLocked(info tailInfo, height uint64) int {
	theirs, ok := windowTxHashes(info.window)
	if !ok || p.chain == nil {
		return 0
	}

	block := p.chain.GetBlockByNumber(height)
	if block == nil {
		return 0
	}

	ours := make(map[common.Hash]struct{}, block.Transactions().Len())
	for _, tx := range block.Transactions() {
		ours[tx.Hash()] = struct{}{}
	}

	orphans := 0

	for _, h := range theirs {
		if _, carried := ours[h]; !carried {
			orphans++
		}
	}

	return orphans
}

// sealedHashAtLocked returns the block hash our undelivered seal carries for
// a height.
func (p *Publisher) sealedHashAtLocked(height uint64) (common.Hash, bool) {
	for i := len(p.journal.items) - 1; i >= 0; i-- {
		it := p.journal.items[i]
		if it.kind != entrySeal || it.height != height {
			continue
		}

		header, err := decodeSealHeader(it.entry.GetBlockSeal().GetHeader())
		if err != nil {
			return common.Hash{}, false
		}

		return header.Hash(), true
	}

	return common.Hash{}, false
}

// yieldFlushLocked abandons our sealed window because the chain chose
// another producer's block at that height. Our content is non-canonical;
// republishing it would overwrite the winner's correct content. Counted
// apart from supersede — here we are the loser, not the winner.
func (p *Publisher) yieldFlushLocked(info tailInfo) reconcileOutcome {
	log.Warn("Sequencer yielding to canonical seal at our own height",
		"height", info.lastSealHeight, "canonical", info.lastSealHash)
	reconcileYield.Inc(1)

	return p.refoldLocked(info.s, keepNone, nil)
}

// reanchorFlushLocked re-anchors every undelivered seal onto the store
// head, oldest window first — flushes stack behind a reconcile backoff,
// and a skipped one would leave its height unsealed in the store forever.
func (p *Publisher) reanchorFlushLocked(info tailInfo) reconcileOutcome {
	keepFrom := p.oldestPendingSealLocked()

	var counter *metrics.Counter

	switch {
	case (info.tipOpen && info.tipOpenHeight >= keepFrom) ||
		(info.haveSeal && info.lastSealHeight >= keepFrom):
		counter = reconcileSupersede // foreign content on re-anchored ground is overridden
	case info.haveSeal && info.lastSealHeight+1 == keepFrom:
		counter = nil // contiguous re-delivery: nothing overridden or abandoned
	default:
		counter = reconcileForwardJump // sealed ground missing below the flush
	}

	return p.refoldLocked(info.s, keepFrom, counter)
}

// classifyBetweenBlocksLocked resolves a foreign tail with no build in
// progress. A tail ending in an adoptable unsealed window is not history
// to rebase past — it is what the next build-start check adopts (a
// restarted producer resumes its own window this way): anchor at the
// window's base, keeping the window ahead of the anchor where the
// build-start read can collect it. Otherwise rebase onto the store head
// and let the next open extend it; abandoning unconfirmed history here is
// the forward-jump case (counted only when entries are actually left
// behind).
func (p *Publisher) classifyBetweenBlocksLocked(info tailInfo) reconcileOutcome {
	if len(info.window) > 0 && p.unackedLocked() == 0 {
		base := commitment.Head(info.window[0].GetBlockOpen().GetPrefixCommitment())
		if base != p.anchor {
			p.rebaseLocked(base)
		}

		p.anchored = true

		return recOK
	}

	log.Info("Sequencer rebasing onto store head", "head", info.s)

	return p.refoldLocked(info.s, keepNone, reconcileForwardJump)
}

// holdStaleLocked buffers everything unacked until a seal flush resolves
// the height.
func (p *Publisher) holdStaleLocked() reconcileOutcome {
	p.hold = hold{after: p.ackedSeq, kind: holdSticky}
	p.anchored = true

	return recOK
}

// foreignSealCanonicalLocked reports whether the store's trailing seal is
// the block the local chain considers canonical at that height. A nil
// chain (test publishers) preserves the pre-check behavior.
func (p *Publisher) foreignSealCanonicalLocked(info tailInfo) bool {
	if !info.haveSeal {
		return false
	}

	if p.chain == nil {
		return true
	}

	return p.chain.GetCanonicalHash(info.lastSealHeight) == info.lastSealHash
}

// pendingFlushLocked reports whether the lineage carries any undelivered
// seal — a flush in flight, possibly stacked behind retry pacing and
// possibly with the next build's window already opened on top — and the
// height of the newest one. Back-to-back builds leave only a sliver of
// time where a seal is the trailing item, so keying on the trailing item
// alone would hide the stack from nearly every reconcile.
func (p *Publisher) pendingFlushLocked() (uint64, bool) {
	for i := len(p.journal.items) - 1; i >= 0; i-- {
		it := p.journal.items[i]
		if it.seq <= p.ackedSeq {
			return 0, false
		}

		if it.kind == entrySeal {
			return it.height, true
		}
	}

	return 0, false
}

// oldestPendingSealLocked returns the height of the first undelivered
// seal — the start of a possibly stacked flush suffix.
func (p *Publisher) oldestPendingSealLocked() uint64 {
	for _, it := range p.journal.items {
		if it.seq > p.ackedSeq && it.kind == entrySeal {
			return it.height
		}
	}

	return 0
}

// suffixExtendsLocked reports whether the first undelivered journal item
// folds directly onto s — the send loop can replay it verbatim.
func (p *Publisher) suffixExtendsLocked(s commitment.Head) bool {
	items, covered := p.journal.after(p.ackedSeq)
	if !covered || len(items) == 0 {
		return true
	}

	return items[0].pre == s
}

// completeExtendedWindowLocked completes a window in place when the store's
// standing window is a prefix of what we hold: the store's copy is absorbed
// as acked and only the records it lacks (plus the seal, once sealed) fold
// onto its head. Nothing already published is re-sent and no new generation
// is written — readers already have the prefix.
func (p *Publisher) completeExtendedWindowLocked(info tailInfo, flushHeight uint64) bool {
	ours, items, cut, ok := p.matchExtendedWindowLocked(info, flushHeight)
	if !ok {
		return false
	}

	p.absorbExtendedWindowLocked(info, ours, items, cut, flushHeight)

	return true
}

// matchExtendedWindowLocked reports whether the store's standing window is
// a strict prefix of the flush at flushHeight (same open, records a
// leading byte-subsequence of ours, same order), returning our flush
// suffix and the store's window parsed as journal items.
func (p *Publisher) matchExtendedWindowLocked(info tailInfo, flushHeight uint64) (ours, items []journalItem, cut int, ok bool) {
	if !info.tipOpen || info.tipOpenHeight != flushHeight || len(info.window) == 0 {
		return nil, nil, 0, false
	}

	// The window may still be building (no seal yet): a mid-build
	// re-anchor completes in place just as a flush does.
	ours = p.journal.suffixFromHeight(flushHeight)
	if len(ours) == 0 || ours[0].kind != entryOpen {
		return nil, nil, 0, false
	}
	if !contentEqual(info.window[0], ours[0].entry) {
		return nil, nil, 0, false
	}

	storedHashes, ok := windowTxHashes(info.window)
	if !ok || !windowLeadsHashes(storedHashes, journalRecordHashes(ours[1:])) {
		return nil, nil, 0, false
	}

	stored, items, ok := parseWindow(info)
	if !ok || stored.number != flushHeight {
		return nil, nil, 0, false
	}
	cut, ok = journalRecordCut(ours, len(storedHashes))
	if !ok {
		return nil, nil, 0, false
	}

	return ours, items, cut, true
}

// absorbExtendedWindowLocked absorbs the store's copy as confirmed lineage
// and re-folds the remainder (records past the stored prefix, and the
// seal) onto its head for the send loop to deliver. A refold failure
// latches fail(); nothing else to classify.
func (p *Publisher) absorbExtendedWindowLocked(info tailInfo, ours, items []journalItem, cut int, flushHeight uint64) {
	// Older stacked flushes below this window are abandoned by the swap:
	// their heights are already represented byte-identically in the store
	// lineage this window folds on (the open's parent hash pins it), but
	// the entries themselves never deliver — count them.
	if abandoned := p.unackedLocked() - len(ours); abandoned > 0 {
		publishDropMeter.Mark(int64(abandoned))
	}

	fresh := newJournal()
	fresh.nextSeq = p.journal.nextSeq

	for _, it := range items {
		fresh.append(it.entry, it.pre, it.post, it.kind, it.height, fresh.nextSeq, it.txHashes)
	}

	p.ackedSeq = fresh.nextSeq - 1
	cur := info.s

	for _, item := range ours[cut:] {
		entry, next, err := refoldEntry(cur, item)
		if err != nil {
			p.fail("refold flush remainder", "err", err)

			return
		}

		fresh.append(entry, cur, next, item.kind, item.height, 0, item.txHashes)
		cur = next
	}

	log.Info("Sequencer completing extended window in place", "number", flushHeight,
		"stored", len(info.window), "delivering", len(ours)-len(info.window))
	p.installSwapLocked(fresh, cur, info.s)
}

func journalRecordCut(items []journalItem, transactions int) (int, bool) {
	matched := 0
	for i := 1; i < len(items); i++ {
		if items[i].kind != entryRecord {
			return i, matched == transactions
		}
		matched += len(items[i].txHashes)
		if matched == transactions {
			return i + 1, true
		}
		if matched > transactions {
			return 0, false
		}
	}
	return len(items), matched == transactions
}

// refoldLocked swaps the lineage: journal items from the first window at or
// above keepFrom are re-prefixed onto s (mid-window republish);
// everything earlier is abandoned to supersession convergence.
func (p *Publisher) refoldLocked(s commitment.Head, keepFrom uint64, counter *metrics.Counter) reconcileOutcome {
	suffix := p.journal.suffixFromHeight(keepFrom)

	// Entries behind the kept suffix are abandoned to supersession
	// convergence; count the unconfirmed ones as dropped. Collapsed
	// entries are excluded — the backfill accounts for them (rebuilt or
	// skipped) itself. A re-anchor that abandons nothing (a fresh process
	// resuming at the store tail) is not a forward jump — only count the
	// jump when entries are left behind.
	abandoned := p.unackedLocked() - len(suffix) - p.pendingEntries
	if abandoned > 0 {
		publishDropMeter.Mark(int64(abandoned))
		p.restoreAbandonedDebtLocked(suffix)
	} else if counter == reconcileForwardJump {
		counter = nil
	}

	fresh := newJournal()
	fresh.nextSeq = p.journal.nextSeq
	p.ackedSeq = fresh.nextSeq - 1

	cur := p.backfillLocked(fresh, s)

	// Everything appended so far is the backfill batch; the suffix follows.
	batchCeiling := fresh.nextSeq - 1

	for _, item := range suffix {
		entry, next, err := refoldEntry(cur, item)
		if err != nil {
			p.fail("refold entry", "err", err)

			return recTerminal
		}

		// Refolded entries are all undelivered: watermark 0 keeps a
		// stacked re-anchor from evicting its own seals.
		fresh.append(entry, cur, next, item.kind, item.height, 0, item.txHashes)
		cur = next
	}

	p.installSwapLocked(fresh, cur, s)

	// An unfinished drain gates the suffix behind the batch: a suffix seal
	// landing out of order would advance the store's sealed tip past the
	// gap, and the floor above would then skip the middle forever. The
	// drain cycle re-arms this ceiling each batch and lifts it with the
	// last one.
	if p.pendingFrom != 0 {
		p.hold = hold{after: batchCeiling, kind: holdBuild}
	}

	if counter != nil {
		counter.Inc(1)
	}

	return recOK
}

// installSwapLocked commits a rebuilt lineage: it points the journal, head and
// anchor at the swap result and resets every field a lineage swap must clear
// together — the confirmed-anchor latches, any adopted window (folded on
// state that no longer exists), and the send hold — then re-derives the open
// window and wakes the transport. Centralizing this keeps the "reset as a
// unit" invariant in one place across the refold and complete-in-place paths.
func (p *Publisher) installSwapLocked(fresh *journal, head, anchor commitment.Head) {
	p.journal = fresh
	p.head = head
	p.anchor = anchor
	p.anchored, p.confirmed = true, true
	p.adopt = nil
	p.hold = clearedHold()
	p.syncWindowLocked()
	publishQueueGauge.Update(int64(p.unackedLocked()))
	p.signalWake()
}

// syncWindowLocked recomputes the open-window bookkeeping after a lineage
// swap. A worker mid-window whose entries were all dropped must not have its
// remaining records published without their open (awaitOpen).
func (p *Publisher) syncWindowLocked() {
	if idx := p.journal.openStart(); idx >= 0 {
		p.curHeight = p.journal.items[idx].height
		p.awaitOpen = false

		return
	}

	if p.curHeight != 0 {
		p.awaitOpen = true
	}

	p.curHeight = 0
}
