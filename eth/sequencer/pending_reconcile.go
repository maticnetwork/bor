package sequencer

import (
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func (s *PendingStore) reconcileThroughMemory(number uint64, canonical func(uint64) *types.Block, canonicalReceipts func(common.Hash) types.Receipts) ([]*types.Log, []pendingInvalidation) {
	s.mu.Lock()
	removed, invalidations := s.reconcileCanonicalLocked(number, canonical, canonicalReceipts)
	futureLogs, futureInvalidations := s.reconcileFutureLocked(number, canonical(number))
	s.mu.Unlock()
	return append(removed, futureLogs...), append(invalidations, futureInvalidations...)
}

func (s *PendingStore) reconcileCanonicalLocked(number uint64, canonical func(uint64) *types.Block, canonicalReceipts func(common.Hash) types.Receipts) ([]*types.Log, []pendingInvalidation) {
	var removed []*types.Log
	var invalidations []pendingInvalidation
	for key, entry := range s.entries {
		if key.number > number {
			continue
		}
		reason := reconcileCanonicalEntry(key, entry, canonical, canonicalReceipts)
		if entry.phase == PendingImporting {
			if reason != "" && entry.deferredInvalidation == "" {
				entry.deferredInvalidation = reason
			}
			continue
		}
		if reason != "" {
			removed = append(removed, removedEntryLogs(entry)...)
			invalidations = append(invalidations, pendingInvalidation{number: key.number, reason: reason})
		}
		s.removeLocked(key)
	}
	return removed, invalidations
}

func (s *PendingStore) reconcileFutureLocked(number uint64, anchor *types.Block) ([]*types.Log, []pendingInvalidation) {
	keys := s.futureKeysLocked(number)
	var removed []*types.Log
	var invalidations []pendingInvalidation
	expectedNumber := number + 1
	expectedParent, broken := futureAnchor(anchor)
	for _, key := range keys {
		entry := s.entries[key]
		if entry == nil {
			continue
		}
		viewBlock := pendingViewBlock(entry)
		if !broken {
			broken = futureLinkBroken(key, viewBlock, expectedNumber, expectedParent)
		}
		if !broken {
			expectedNumber++
			expectedParent = viewBlock.Hash()
			continue
		}
		entryLogs, invalidation := s.reconcileBrokenFutureLocked(key, entry)
		removed = append(removed, entryLogs...)
		if invalidation != nil {
			invalidations = append(invalidations, *invalidation)
		}
	}
	return removed, invalidations
}

func (s *PendingStore) futureKeysLocked(number uint64) []pendingKey {
	keys := make([]pendingKey, 0, len(s.entries))
	for key := range s.entries {
		if key.number > number {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].number != keys[j].number {
			return keys[i].number < keys[j].number
		}
		return keys[i].parent.Cmp(keys[j].parent) < 0
	})
	return keys
}

func futureAnchor(anchor *types.Block) (common.Hash, bool) {
	if anchor == nil {
		return common.Hash{}, true
	}
	return anchor.Hash(), false
}

func pendingViewBlock(entry *pendingEntry) *types.Block {
	if entry.RPCView == nil {
		return nil
	}
	return entry.RPCView.Block
}

func futureLinkBroken(key pendingKey, block *types.Block, expectedNumber uint64, expectedParent common.Hash) bool {
	return key.number != expectedNumber || key.parent != expectedParent || block == nil ||
		block.NumberU64() != key.number || block.ParentHash() != key.parent
}

func (s *PendingStore) reconcileBrokenFutureLocked(key pendingKey, entry *pendingEntry) ([]*types.Log, *pendingInvalidation) {
	if entry.phase == PendingImporting {
		if entry.deferredInvalidation == "" {
			entry.deferredInvalidation = "reorged"
		}
		return nil, nil
	}
	logs := removedEntryLogs(entry)
	invalidation := &pendingInvalidation{number: key.number, reason: "reorged"}
	s.removeLocked(key)
	return logs, invalidation
}

func reconcileCanonicalEntry(key pendingKey, entry *pendingEntry, canonical func(uint64) *types.Block, canonicalReceipts func(common.Hash) types.Receipts) string {
	block := canonical(key.number)
	match := block != nil && entry.RPCView != nil && entryMatchesCanonical(entry, block, canonicalReceipts(block.Hash()))
	if match {
		return ""
	}
	return "canonical_mismatch"
}
