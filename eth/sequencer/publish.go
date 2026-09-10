package sequencer

import (
	"math/big"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// OpenBlock publishes block context the moment the producer opens it. A
// re-open of the same height (work-cycle restart) or an open on a different
// parent (reorg mid-build) supersedes the in-progress window downstream.
func (p *Publisher) OpenBlock(number uint64, timestamp uint64, parent common.Hash, gasLimit uint64, baseFee *big.Int) {
	if p.failed.Load() {
		return
	}

	open := commitment.OpenContext{
		Number:     number,
		Timestamp:  timestamp,
		ParentHash: parent,
		GasLimit:   gasLimit,
		BaseFee:    baseFee,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mode.kind != modeOpen {
		return
	}

	if number != 0 && number <= p.sealedTip {
		// A seal result raced this build's open: never open at or behind
		// a sealed height.
		p.awaitOpen = true
		p.curHeight = 0

		return
	}

	if p.adoptOpenLocked(number, timestamp, parent, gasLimit, baseFee) {
		return // engaged an adopted window already in the store
	}

	next, err := commitment.FoldOpen(p.head, open)
	if err != nil {
		p.fail("fold open context", "err", err)

		return
	}

	p.awaitOpen = false
	p.curHeight = number
	p.appendLocked(openEntry(open, p.head), next, entryOpen, number, nil)
}

// PublishTx publishes one committed transaction.
func (p *Publisher) PublishTx(tx *types.Transaction) {
	if p.failed.Load() {
		return
	}

	raw, err := tx.MarshalBinary()
	if err != nil {
		p.fail("encode transaction", "hash", tx.Hash(), "err", err)

		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mode.kind != modeOpen || p.awaitOpen || p.curHeight == 0 {
		return
	}

	if p.adoptTxLocked(tx.Hash()) {
		return // matched: the record is already in the store
	}

	p.appendLocked(recordEntry(raw, p.head), commitment.FoldTx(p.head, raw), entryRecord, p.curHeight, []common.Hash{tx.Hash()})
}

// SealBlock is the seal flush: the block sealed and is being
// broadcast, so the store must come to match it. The lineage's current
// window either already mirrors the sealed content (the journal was built
// alongside the block) or is rebuilt from the block's body; the seal
// closes it and the hold lifts — buffered entries and the seal go out
// together. A seal is never dropped. STALEs on the released entries land
// in reconciliation, which completes (byte-match) or re-anchors
// (the only supersede).
func (p *Publisher) SealBlock(block *types.Block) {
	if p.failed.Load() {
		return
	}

	header := block.Header()

	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		p.fail("encode sealed header", "number", header.Number, "err", err)

		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.adopt = nil

	mode := p.mode
	p.mode = buildMode{}
	p.diverged = divergence{}

	n := header.Number.Uint64()
	prevTip := p.sealedTip

	if n > p.sealedTip {
		p.sealedTip = n
	}

	recovered := mode.kind == modeRecover && mode.height == n
	if recovered || mode.kind == modeSealedWait {
		p.sealWithoutPublishLocked(recovered, n, block.Hash())

		return
	}

	if !p.windowMirrorsLocked(block) {
		// The journal's window does not describe the sealed block (a adopt
		// that raced a straggler seal, a purge, a divergent rebuild):
		// rebuild it from the body so the flush carries the sealed truth.
		if !p.rebuildWindowLocked(block) {
			return // fail() already recorded the cause
		}
	}

	// appendLocked already signaled the send loop; the hold clears after so the
	// released entries and the seal go out together on that wake.
	p.appendLocked(sealEntry(raw, p.head), commitment.FoldSeal(p.head, commitment.SealedHash(raw)), entrySeal, n, nil)

	// Arm the broadcast gate: ConfirmSeal resolves it from the seal's ack,
	// the chain, or its deadline.
	p.gate = sealGate{
		height: n, hash: block.Hash(), published: true, prevTip: prevTip,
		txs:            txHashes(block.Transactions()),
		tolerateSealed: mode.kind == modeOverSealed && mode.height == n,
	}

	p.curHeight = 0
	p.awaitOpen = false
	p.hold = clearedHold()
	p.collapseColdLocked()
}

// sealWithoutPublishLocked closes the build state for a block that is a
// rebuild of a generation the store already holds, seal included.
// Republishing it would open a second generation over a sealed height —
// the displacement this whole design exists to prevent. Nothing publishes;
// the block only needs to reach the chain.
func (p *Publisher) sealWithoutPublishLocked(recovered bool, n uint64, hash common.Hash) {
	p.curHeight = 0
	p.awaitOpen = false
	p.hold = clearedHold()

	if recovered {
		p.gate = sealGate{}

		return
	}

	// Not a recovery: the store closed this height with content this
	// block does not carry, and the winner's block has not arrived yet.
	// Broadcasting divergent content here is the displacement the gate
	// exists to stop, so an undecided height refuses. The next build
	// recovers the height properly once the grace has elapsed.
	p.gate = sealGate{height: n, hash: hash, refuseOnTimeout: true}
}

// collapseColdLocked bounds the undelivered flushes held in memory: past
// journalHotSeals, the oldest collapse to the pending height range and are
// rebuilt from the chain database when a re-anchor can deliver them. The
// send cursor detects the gap and routes through reconciliation.
func (p *Publisher) collapseColdLocked() {
	for p.unackedSealsLocked() > journalHotSeals {
		h, removed, ok := p.journal.collapseOldestUnacked(p.ackedSeq)
		if !ok {
			return
		}

		// Merge into the pending range, never clobber it: during an active
		// drain the collapse reaches heights BELOW pendingFrom (a stranded
		// batch suffix is the journal's oldest content), and overwriting
		// pendingTo downward inverted the range — which the backfill then
		// read as "nothing owed" and zeroed. That silent evaporation
		// stranded 25 heights of a 50-block outage as permanent holes.
		if p.pendingFrom == 0 || h < p.pendingFrom {
			p.pendingFrom = h
		}

		if h > p.pendingTo {
			p.pendingTo = h
		}

		p.pendingEntries += removed
	}
}

// unackedSealsLocked counts sealed-but-undelivered flushes in the journal.
func (p *Publisher) unackedSealsLocked() int {
	n := 0

	for _, it := range p.journal.items {
		if it.kind == entrySeal && it.seq > p.ackedSeq {
			n++
		}
	}

	return n
}

// windowMirrorsLocked reports whether the journal's trailing window is an
// open at the block's height whose records are exactly the block's
// transactions, in order.
func (p *Publisher) windowMirrorsLocked(block *types.Block) bool {
	if p.awaitOpen {
		return false
	}

	start := p.journal.openStart()
	if start < 0 || p.journal.items[start].height != block.NumberU64() {
		return false
	}

	// The window's open context must equal the sealed header: consumers
	// pinned it at open and void the window on any mismatch, so a context
	// drift means rebuild, not complete-in-place.
	if !openMatchesHeader(p.journal.items[start].entry.GetBlockOpen(), block.Header()) {
		return false
	}

	return recordsMirrorTxs(p.journal.items[start+1:], block.Transactions())
}

// rebuildWindowLocked replaces the lineage's trailing window with one built
// from the sealed block's body, folded onto the confirmed prefix.
func (p *Publisher) rebuildWindowLocked(block *types.Block) bool {
	// Drop the trailing undelivered open window; the sealed block replaces
	// it (an adopted window is acked and stays; the rebuild folds on top).
	if cut := p.journal.rebuildCut(p.ackedSeq); cut < len(p.journal.items) {
		p.rewindJournalLocked(cut)
	}

	header := block.Header()
	open := openEntry(commitment.OpenContext{
		Number:     header.Number.Uint64(),
		Timestamp:  header.Time,
		ParentHash: header.ParentHash,
		GasLimit:   header.GasLimit,
		BaseFee:    header.BaseFee,
	}, p.head)

	next, err := foldEntry(p.head, open)
	if err != nil {
		p.fail("fold flush open", "number", header.Number, "err", err)

		return false
	}

	p.appendLocked(open, next, entryOpen, header.Number.Uint64(), nil)

	for _, tx := range block.Transactions() {
		rawTx, err := tx.MarshalBinary()
		if err != nil {
			p.fail("encode flush transaction", "hash", tx.Hash(), "err", err)

			return false
		}

		p.appendLocked(recordEntry(rawTx, p.head), commitment.FoldTx(p.head, rawTx), entryRecord, header.Number.Uint64(), []common.Hash{tx.Hash()})
	}

	return true
}

// rewindJournalLocked truncates the journal to its first n items and returns the
// fold head to the truncation point.
func (p *Publisher) rewindJournalLocked(n int) {
	p.journal.truncate(n)

	if n > 0 {
		last := p.journal.items[n-1]
		p.head = last.post

		if p.ackedSeq > last.seq {
			p.ackedSeq = last.seq
		}
	} else {
		p.head = p.anchor
	}
}

// appendLocked folds one entry into the lineage and wakes the transport.
func (p *Publisher) appendLocked(entry *pb.Entry, next commitment.Head, kind int, height uint64, txHashes []common.Hash) {
	p.journal.append(entry, p.head, next, kind, height, p.ackedSeq, txHashes)
	p.head = next
	publishQueueGauge.Update(int64(p.unackedLocked()))
	p.signalWake()
}

// baseFeeBytes encodes a base fee for the wire: a nil or zero fee is the
// empty slice, which is what the commitment fold hashes — the two encodings
// must agree or a published open stops matching its fold head.
func baseFeeBytes(f *big.Int) []byte {
	if f == nil {
		return []byte{}
	}

	return f.Bytes()
}
