package sequencer

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

func entryMatchesCanonical(entry *pendingEntry, block *types.Block, receipts types.Receipts) bool {
	if entry.RPCView == nil {
		return false
	}
	if entry.phase == PendingBuilding || entry.phase == PendingImporting && entry.Sealed == nil {
		if len(entry.executedTransactions) > 0 {
			return sameExecutionContext(entry.RPCView.Header, block.Header()) &&
				sameTransactionSlicePrefix(entry.executedTransactions, block.Transactions()) &&
				(receipts == nil || sameReceiptSlicePrefix(entry.executedReceipts, receipts))
		}
		return sameExecutionContext(entry.RPCView.Header, block.Header()) &&
			sameTransactionPrefix(entry.RPCView.Block, block) &&
			(receipts == nil || sameReceiptPrefix(entry.RPCView.Block, entry.RPCView, receipts))
	}
	return entry.RPCView.Block.Hash() == block.Hash() && sameTransactions(entry.RPCView.Block, block) &&
		(receipts == nil || sameReceipts(entry.RPCView.Block, entry.RPCView, receipts))
}

func (s *PendingStore) RejectClaimedPreconf(block *types.Block) []*types.Log {
	logs, invalidations, _ := s.rejectClaimedPreconf(block)
	s.writeInvalidations(invalidations)
	return logs
}

func (s *PendingStore) rejectClaimedPreconf(block *types.Block) ([]*types.Log, []pendingInvalidation, bool) {
	if block == nil {
		return nil, nil, false
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	var logs []*types.Log
	s.mu.Lock()
	if entry := s.entries[key]; entry != nil && entry.phase == PendingImporting && entry.claimedHash == block.Hash() {
		if entry.partialClaim {
			if entry.deferredInvalidation != "" {
				logs = removedEntryLogs(entry)
				invalidation := pendingInvalidation{number: block.NumberU64(), reason: entry.deferredInvalidation}
				s.removeLocked(key)
				s.mu.Unlock()
				return logs, []pendingInvalidation{invalidation}, true
			}
			s.generation++
			entry.generation = s.generation
			entry.phase = PendingBuilding
			entry.claimedHash = common.Hash{}
			entry.partialClaim = false
			entry.rejected = true
			entry.Sealed = nil
			s.mu.Unlock()
			return nil, nil, false
		}
		if entry.deferredInvalidation != "" {
			logs = removedEntryLogs(entry)
			invalidation := pendingInvalidation{number: block.NumberU64(), reason: entry.deferredInvalidation}
			s.removeLocked(key)
			s.mu.Unlock()
			return logs, []pendingInvalidation{invalidation}, true
		}
		entry.rejected = true
		entry.phase = PendingBuilding
		if entry.Sealed != nil {
			entry.phase = PendingSealed
		}
		entry.claimedHash = common.Hash{}
		entry.Sealed = nil
	}
	s.mu.Unlock()
	return logs, nil, false
}

func (s *PendingStore) CompletePreconf(block *types.Block, committed bool) []*types.Log {
	logs, invalidations, _, _ := s.completePreconf(block, nil, committed)
	s.writeInvalidations(invalidations)
	return logs
}

func (s *PendingStore) completePreconf(block *types.Block, receipts types.Receipts, committed bool) ([]*types.Log, []pendingInvalidation, bool, bool) {
	if block == nil {
		return nil, nil, false, false
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	s.mu.Lock()
	entry := s.entries[key]
	if entry == nil {
		s.mu.Unlock()
		return nil, nil, false, false
	}
	logs, reason, removed, matched := s.completeEntryLocked(key, entry, block, receipts, committed)
	s.mu.Unlock()
	var invalidations []pendingInvalidation
	if reason != "" {
		invalidations = append(invalidations, pendingInvalidation{number: block.NumberU64(), reason: reason})
	}
	return logs, invalidations, removed, matched
}

func (s *PendingStore) completeEntryLocked(key pendingKey, entry *pendingEntry, block *types.Block, receipts types.Receipts, committed bool) ([]*types.Log, string, bool, bool) {
	if committed {
		var logs []*types.Log
		var reason string
		matched := entry.RPCView != nil && entryMatchesCanonical(entry, block, receipts)
		if !matched {
			logs = removedEntryLogs(entry)
			reason = "canonical_mismatch"
		}
		s.removeLocked(key)
		return logs, reason, true, matched
	}
	if entry.phase != PendingImporting || entry.claimedHash != block.Hash() {
		return nil, "", false, false
	}
	if entry.partialClaim || entry.deferredInvalidation != "" {
		logs := removedEntryLogs(entry)
		reason := entry.deferredInvalidation
		s.removeLocked(key)
		return logs, reason, true, false
	}
	entry.phase = PendingBuilding
	if entry.Sealed != nil {
		entry.phase = PendingSealed
	}
	entry.claimedHash = common.Hash{}
	return nil, "", false, false
}

func (s *PendingStore) invalidateFrom(number uint64, reason string) []*types.Log {
	logs, invalidations := s.invalidateFromMemory(number, reason)
	s.writeInvalidations(invalidations)
	return logs
}

func (s *PendingStore) invalidateFromMemory(number uint64, reason string) ([]*types.Log, []pendingInvalidation) {
	var removed []*types.Log
	var invalidations []pendingInvalidation
	s.mu.Lock()
	for key, entry := range s.entries {
		if key.number < number {
			continue
		}
		if entry.phase == PendingImporting {
			// Canonical import owns a claimed entry until CompletePreconf resolves it.
			if entry.deferredInvalidation == "" {
				entry.deferredInvalidation = reason
			}
			continue
		}
		removed = append(removed, removedEntryLogs(entry)...)
		invalidations = append(invalidations, pendingInvalidation{number: key.number, reason: reason})
		s.removeLocked(key)
	}
	s.mu.Unlock()
	return removed, invalidations
}

func (s *PendingStore) writeInvalidation(number uint64, reason string) {
	if s.db == nil {
		return
	}
	if err := rawdb.WriteInvalidPreconf(s.db, number, reason); err != nil {
		log.Warn("Failed to write preconfirmation invalidation", "number", number, "reason", reason, "err", err)
	}
}

func (s *PendingStore) writeInvalidations(invalidations []pendingInvalidation) {
	for _, invalidation := range invalidations {
		s.writeInvalidation(invalidation.number, invalidation.reason)
	}
}

func (s *PendingStore) removeLocked(key pendingKey) {
	entry := s.entries[key]
	if entry == nil {
		return
	}
	delete(s.entries, key)
	preconfPendingEntries.Update(int64(len(s.entries)))
	if s.hasActive && s.active == key {
		s.hasActive = false
	}
}
