package sequencer

import (
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// The backfill debt: heights the store is owed from the chain database.
// Debt accrues when sealed blocks could not be delivered (an outage
// collapse, a refold abandoning acked-pending seals) and drains
// oldest-first on re-anchors so the store's sealed tip never advances past
// a gap it would then refuse to fill.

// restoreAbandonedDebtLocked folds the sealed heights a refold is about to
// abandon back into the pending range. The backfill advances pendingFrom at
// build time, before delivery is confirmed; when the batch then loses its
// head race and the next classification abandons it, nothing else remembers
// those heights are owed — a devnet pinned an outage height as a permanent
// hole exactly this way. Restoring the debt costs at most a duplicate
// delivery.
func (p *Publisher) restoreAbandonedDebtLocked(suffix []journalItem) {
	kept := uint64(0)
	if len(suffix) > 0 {
		kept = suffix[0].seq
	}

	lo, hi := p.abandonedSealRangeLocked(kept)
	if lo == 0 {
		return
	}

	if p.pendingFrom == 0 || lo < p.pendingFrom {
		p.pendingFrom = lo
	}

	if hi > p.pendingTo {
		p.pendingTo = hi
	}

	log.Warn("Sequencer restoring abandoned sealed heights to the backfill",
		"from", lo, "to", hi)
}

// abandonedSealRangeLocked returns the height range of unconfirmed sealed
// entries behind the kept suffix — the seals a refold abandons. A zero lo
// means there are none.
func (p *Publisher) abandonedSealRangeLocked(kept uint64) (lo, hi uint64) {
	for _, it := range p.journal.items {
		if it.seq <= p.ackedSeq || it.kind != entrySeal {
			continue
		}

		if kept != 0 && it.seq >= kept {
			break
		}

		if lo == 0 || it.height < lo {
			lo = it.height
		}

		if it.height > hi {
			hi = it.height
		}
	}

	return lo, hi
}

// backfillLocked drains the oldest owed blocks from the chain database onto
// cur, appending to fresh as undelivered entries, and returns the new fold
// head. One call rebuilds at most a journal byte budget's worth; the
// remainder stays pending, and the drain resumes on the next re-anchor —
// oldest first, so the store's sealed tip never advances past a gap it
// would then refuse to fill. Blocks the store already sealed are not owed;
// blocks missing from the database (pruned) are the one gap backfill cannot
// close, skipped as a counted forward jump.
func (p *Publisher) backfillLocked(fresh *journal, cur commitment.Head) commitment.Head {
	if p.pendingFrom == 0 {
		return cur
	}

	// The drain never trusts storeSealedTip as a floor. The tip is the
	// newest seal, not proof of anything below it: live flushes seal
	// heights above the gap while it waits, and a store restart can shed
	// acked writes — "the store has these" was twice false on a devnet,
	// and pending heights skipped on that belief stayed holes forever.
	// The only floor is our own finality view (backfillFloorLocked):
	// a fact about the chain, not a belief about the store. Below it the
	// hole is intentional — the jump open crosses it, and re-delivering a
	// height the store does have only adds an identical duplicate
	// generation, which costs churn, not correctness.
	lo, hi := p.pendingFrom, p.pendingTo

	if floor := p.backfillFloorLocked(); floor > lo {
		reconcileForwardJump.Inc(1)
		log.Warn("Sequencer backfill jumping finalized heights",
			"from", lo, "through", min(floor-1, hi), "floor", floor)

		if floor > hi {
			p.pendingFrom, p.pendingTo, p.pendingEntries = 0, 0, 0

			return cur
		}

		lo, p.pendingFrom = floor, floor
	}

	log.Info("Sequencer backfill starting", "from", lo, "to", hi,
		"storeSealedTip", p.storeSealedTip)

	if lo > hi || p.chain == nil {
		p.dropUndrainableDebtLocked(lo, hi)

		return cur
	}

	return p.drainDebtLocked(fresh, cur, lo, hi)
}

// backfillFloorLocked returns the lowest height still worth delivering.
// Heights at or below a canonical whitelisted milestone are final:
// immutable and permanently served by the canonical chain, so their store
// copies serve no consumer — a 1-hour outage owes seconds of blocks, not
// the hour. Without a usable milestone (Heimdall down alongside the
// store, or a milestone naming a chain we don't hold), backfillDepthCap
// below the tip bounds the drain instead. Zero means no floor.
func (p *Publisher) backfillFloorLocked() uint64 {
	var floor uint64

	if p.finality != nil && p.chain != nil {
		if exists, number, hash := p.finality(); exists && p.chain.GetCanonicalHash(number) == hash {
			floor = number + 1
		}
	}

	if p.chain != nil {
		if head := p.chain.CurrentBlock(); head != nil {
			if tip := head.Number.Uint64(); tip > backfillDepthCap {
				floor = max(floor, tip-backfillDepthCap+1)
			}
		}
	}

	return floor
}

// dropUndrainableDebtLocked zeroes debt no drain can serve: no chain to
// read from, or an inverted range.
func (p *Publisher) dropUndrainableDebtLocked(lo, hi uint64) {
	if p.chain == nil && hi >= lo {
		reconcileForwardJump.Inc(1)
	}

	// An inverted range cannot arise anymore — the collapse merges
	// rather than clobbers — so hitting one means new accounting is
	// broken somewhere. Dropping the debt is still the only safe move
	// (a wedged drain is worse), but never a silent one again.
	if lo > hi {
		log.Warn("Sequencer backfill pending range inverted, dropping the debt",
			"from", lo, "to", hi)
	}

	p.pendingFrom, p.pendingTo, p.pendingEntries = 0, 0, 0
}

// drainDebtLocked rebuilds [lo, hi] from the chain database onto cur, one
// byte budget per call, and settles the pending accounting for whatever
// the batch reached.
func (p *Publisher) drainDebtLocked(fresh *journal, cur commitment.Head, lo, hi uint64) commitment.Head {
	var (
		budget, rebuilt int
		jumped          bool
		n               = lo
		started         = time.Now()
	)

	for ; n <= hi; n++ {
		block := p.chain.GetBlockByNumber(n)
		if block == nil {
			jumped = true // pruned: unfillable

			continue
		}

		// Always take at least one block, or a block larger than the
		// budget would wedge the drain forever.
		if budget += int(block.Size()); budget > backfillBatchBytes && rebuilt > 0 {
			break
		}

		before := len(fresh.items)

		next, ok := p.appendBlockLocked(fresh, cur, block)
		if !ok {
			return cur // fail() latched
		}

		rebuilt += len(fresh.items) - before
		cur = next
	}

	if n > hi {
		p.pendingFrom, p.pendingTo, p.pendingEntries = 0, 0, 0
	} else {
		p.pendingFrom = n
		if p.pendingEntries > rebuilt {
			p.pendingEntries -= rebuilt
		} else {
			p.pendingEntries = 0
		}

		log.Info("Sequencer backfill batch, remainder pending",
			"rebuilt-through", n-1, "pending", n, "to", hi,
			"batch", time.Since(started))

		backfillBatchTimer.UpdateSince(started)
	}

	if jumped {
		reconcileForwardJump.Inc(1)
		log.Warn("Sequencer backfill skipped pruned blocks", "pending", lo, "through", hi)
	}

	return cur
}

// appendBlockLocked rebuilds one chain block as journal entries folded onto
// cur — the same encodings the live build publishes, so a rebuilt block is
// byte-identical to the original.
func (p *Publisher) appendBlockLocked(fresh *journal, cur commitment.Head, block *types.Block) (commitment.Head, bool) {
	header := block.Header()
	n := header.Number.Uint64()

	open := openEntry(commitment.OpenContext{
		Number:     n,
		Timestamp:  header.Time,
		ParentHash: header.ParentHash,
		GasLimit:   header.GasLimit,
		BaseFee:    header.BaseFee,
	}, cur)

	next, err := foldEntry(cur, open)
	if err != nil {
		p.fail("backfill fold open", "number", n, "err", err)

		return cur, false
	}

	fresh.append(open, cur, next, entryOpen, n, 0, nil)
	cur = next

	for _, tx := range block.Transactions() {
		raw, err := tx.MarshalBinary()
		if err != nil {
			p.fail("backfill encode transaction", "hash", tx.Hash(), "err", err)

			return cur, false
		}

		rec := recordEntry(raw, cur)
		next = commitment.FoldTxs(cur, [][]byte{raw})
		fresh.append(rec, cur, next, entryRecord, n, 0, []common.Hash{tx.Hash()})
		cur = next
	}

	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		p.fail("backfill encode header", "number", n, "err", err)

		return cur, false
	}

	seal := sealEntry(raw, cur)
	next = commitment.FoldSeal(cur, commitment.SealedHash(raw))
	fresh.append(seal, cur, next, entrySeal, n, 0, nil)

	return next, true
}
