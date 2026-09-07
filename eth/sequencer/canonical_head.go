package sequencer

import (
	"context"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
)

// evictLoop drops preconf receipts for heights the canonical chain has
// imported — the normal receipt path serves them from there on.
func (c *Consumer) evictLoop(ctx context.Context) {
	heads := make(chan core.ChainHeadEvent, 16)
	sub := c.chain.SubscribeChainHeadEvent(heads)

	defer sub.Unsubscribe()
	c.handleCanonicalHead()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-heads:
			if !ok {
				return
			}
			c.handleCanonicalHead()
		case <-sub.Err():
			return
		}
	}
}

func (c *Consumer) handleCanonicalHead() {
	c.publishMu.Lock()
	invalidations := c.reconcileCanonicalHeadLocked()
	c.publishMu.Unlock()
	c.pendingStore().writeInvalidations(invalidations)
	c.markCanonicalHeadAudited()
}

// markCanonicalHeadAudited advances the audit watermark for a height this node
// reconciled while following the store tip.
//
// Two conditions gate it. watching: a session that dropped, or one still
// replaying history, leaves heights nobody compared. Contiguity: the mark may
// only step to the next height, because a jump would carry it over a window
// the session never compared — the catch-up backlog it dropped before
// reaching the tip. A gap instead asks for an audit pass, which walks the
// window properly and lets contiguous stepping resume.
//
// Heights the store never held do advance. Nothing was promised at those
// heights, so there is no preconfirmation to invalidate — the same reasoning
// the store's own outage contract uses, and it holds for the entries a
// producer backfills long after the block went canonical.
func (c *Consumer) markCanonicalHeadAudited() {
	if !c.watching.Load() {
		return
	}
	head := c.chain.CurrentBlock()
	if head == nil || head.Number == nil {
		return
	}

	watermark, stored := rawdb.ReadPreconfAuditedThrough(c.chain.DB())
	if !stored {
		// The audit seeds the first watermark; until it does there is no
		// position to step from.
		c.requestAudit()

		return
	}

	number := head.Number.Uint64()
	if number <= watermark {
		return
	}
	if number > watermark+1 {
		c.requestAudit()

		return
	}

	c.advanceAudited(number)
}

func (c *Consumer) reconcileCanonicalHeadLocked() []pendingInvalidation {
	head := c.chain.CurrentBlock()
	number := head.Number.Uint64()
	c.index.EvictThrough(number)
	logs, invalidations := c.pendingStore().reconcileThroughMemory(number, c.chain.GetBlockByNumber, c.chain.GetReceiptsByHash)
	var clearFrom *uint64
	for _, invalidation := range invalidations {
		if invalidation.number <= number || (clearFrom != nil && invalidation.number >= *clearFrom) {
			continue
		}
		height := invalidation.number
		clearFrom = &height
	}
	if clearFrom != nil {
		c.index.ClearFrom(*clearFrom)
	}
	c.reconciled.Store(head)
	c.clearCanonicalHandoffThrough(head)
	c.enqueuePendingLogs(logs)
	return invalidations
}

func (c *Consumer) ensureCanonicalHeadReconciled() bool {
	c.publishMu.Lock()
	head := c.chain.CurrentBlock()
	if head == nil || head.Number == nil {
		c.publishMu.Unlock()
		return false
	}
	handoff := c.handoff.Load()
	if handoff != nil && handoff.Number != nil && handoff.Number.Cmp(head.Number) == 0 && handoff.Hash() != head.Hash() {
		c.publishMu.Unlock()
		return false
	}
	marker := c.reconciled.Load()
	if marker != nil {
		if marker.Hash() == head.Hash() {
			c.clearCanonicalHandoffThrough(head)
			c.publishMu.Unlock()
			return true
		}
		if marker.Number != nil && marker.Number.Cmp(head.Number) > 0 {
			c.publishMu.Unlock()
			return false
		}
	}
	invalidations := c.reconcileCanonicalHeadLocked()
	marker = c.reconciled.Load()
	head = c.chain.CurrentBlock()
	ready := marker != nil && head != nil && marker.Hash() == head.Hash()
	c.publishMu.Unlock()
	c.pendingStore().writeInvalidations(invalidations)
	return ready
}
