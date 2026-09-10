package sequencer

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
)

func NewPendingStore(db ethdb.Database) *PendingStore {
	return &PendingStore{entries: make(map[pendingKey]*pendingEntry), db: db}
}

func (c *Consumer) pendingStore() *PendingStore {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	if c.store == nil {
		var db ethdb.Database
		if c.chain != nil {
			db = c.chain.DB()
		}
		c.store = NewPendingStore(db)
	}
	return c.store
}

func (c *Consumer) BeginPreconfImport(block *types.Block) {
	c.markCanonicalHandoff(block)
}

func (c *Consumer) ClaimPreconf(block *types.Block) (*core.PreconfExecution, bool) {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	if !c.pendingHeadCurrent() {
		return nil, false
	}

	execution, ok := c.pendingStore().ClaimPreconf(block)
	return execution, ok
}

func (c *Consumer) ClaimPreconfPrefix(block *types.Block) (*core.PreconfExecution, bool) {
	if block == nil {
		return nil, false
	}
	sprint := c.chain.Config().Bor.CalculateSprint(block.NumberU64())
	if sprint == 0 || block.NumberU64()%sprint == 0 {
		return nil, false
	}
	for _, tx := range block.Transactions() {
		if tx.Type() == types.StateSyncTxType {
			return nil, false
		}
	}
	c.publishMu.Lock()
	if !c.pendingHeadCurrent() {
		c.publishMu.Unlock()
		return nil, false
	}
	prefix, ok := c.pendingStore().claimPreconfPrefix(block)
	worker := c.worker.Load()
	c.publishMu.Unlock()
	if !ok {
		return nil, false
	}
	if !c.interruptPreconfWorker(worker, prefix.Generation) {
		c.releasePreconfClaim(block)
		return nil, false
	}
	if prefix.State == nil {
		c.releasePreconfClaim(block)
		return nil, false
	}
	statedb, err := prefix.State.NewStateDB()
	if err != nil {
		c.releasePreconfClaim(block)
		return nil, false
	}
	prefix.State = nil
	prefix.StateDB = statedb
	execution, err := c.completePreconfPrefix(block, prefix)
	if err != nil {
		c.RejectClaimedPreconf(block)
		return nil, false
	}
	return execution, true
}

func (c *Consumer) releasePreconfClaim(block *types.Block) {
	c.publishMu.Lock()
	logs, invalidations, removed, _ := c.pendingStore().completePreconf(block, nil, false)
	if removed && block != nil && c.index != nil {
		c.index.ClearFrom(block.NumberU64())
	}
	c.enqueuePendingLogs(logs)
	c.publishMu.Unlock()
	c.pendingStore().writeInvalidations(invalidations)
}

func (c *Consumer) RejectClaimedPreconf(block *types.Block) {
	c.publishMu.Lock()
	logs, invalidations, removed := c.pendingStore().rejectClaimedPreconf(block)
	if removed && block != nil && c.index != nil {
		c.index.ClearFrom(block.NumberU64())
	}
	c.enqueuePendingLogs(logs)
	c.publishMu.Unlock()
	c.pendingStore().writeInvalidations(invalidations)
}

func (c *Consumer) CompletePreconf(block *types.Block, receipts types.Receipts, committed bool) string {
	c.publishMu.Lock()
	if committed {
		c.markCanonicalHandoff(block)
	} else {
		c.clearCanonicalHandoff(block)
	}
	logs, invalidations, removed, matched := c.pendingStore().completePreconf(block, receipts, committed)
	if committed && block != nil {
		if c.index != nil {
			preconfCanonicalReceipts.Inc(int64(c.index.CountCanonical(block)))
			c.index.EvictThrough(block.NumberU64())
		}
		if matched {
			c.reconciled.Store(types.CopyHeader(block.Header()))
		}
	} else if removed && block != nil && c.index != nil {
		c.index.ClearFrom(block.NumberU64())
	}
	c.enqueuePendingLogs(logs)
	c.publishMu.Unlock()
	if committed && len(invalidations) > 0 {
		return invalidations[0].reason
	}
	c.pendingStore().writeInvalidations(invalidations)
	return ""
}

func (c *Consumer) markCanonicalHandoff(block *types.Block) {
	if block != nil {
		c.handoff.Store(block.Header())
	}
}

func (c *Consumer) clearCanonicalHandoff(block *types.Block) {
	if block == nil {
		return
	}
	header := c.handoff.Load()
	if header != nil && header.Hash() == block.Hash() {
		c.handoff.CompareAndSwap(header, nil)
	}
}

func (c *Consumer) clearCanonicalHandoffThrough(head *types.Header) {
	if head == nil || head.Number == nil {
		return
	}
	header := c.handoff.Load()
	if header != nil && header.Number != nil && (header.Number.Cmp(head.Number) < 0 ||
		header.Number.Cmp(head.Number) == 0 && header.Hash() == head.Hash()) {
		c.handoff.CompareAndSwap(header, nil)
	}
}

func (c *Consumer) canonicalHandoffMatches(parent common.Hash, number uint64) bool {
	header := c.handoff.Load()
	return number > 0 && header != nil && header.Number != nil && header.Hash() == parent && header.Number.Uint64() == number-1
}

func (c *Consumer) canonicalHandoffAt(number uint64) bool {
	header := c.handoff.Load()
	return header != nil && header.Number != nil && header.Number.Uint64() == number
}

func (c *Consumer) invalidatePendingFrom(number uint64) {
	c.invalidatePendingFromReason(number, "skipped")
}

func (c *Consumer) invalidatePendingFromReason(number uint64, reason string) {
	c.publishMu.Lock()
	if c.index != nil {
		c.index.ClearFrom(number)
	}
	logs, invalidations := c.pendingStore().invalidateFromMemory(number, reason)
	c.enqueuePendingLogs(logs)
	c.publishMu.Unlock()
	c.pendingStore().writeInvalidations(invalidations)
}

func preparePending(env *blockEnv, header *types.Header, blockHash common.Hash, reusable *ReusableExecution) (*types.Block, pendingPayload, bool) {
	block, err := blockFromExecution(header, env.txs, env.receipts)
	if err != nil {
		return nil, pendingPayload{}, false
	}
	payload, ok := preparePendingPayload(env, block, blockHash, reusable)
	return block, payload, ok
}

func preparePendingPayload(env *blockEnv, block *types.Block, blockHash common.Hash, reusable *ReusableExecution) (pendingPayload, bool) {
	payload, ok := makePendingPayloadWithPrevious(block, env.receipts, env.statedb, reusable, env.rpcView, blockHash)
	if !ok {
		return pendingPayload{}, false
	}
	payload.finalized = blockHash != (common.Hash{})
	env.rpcView = payload.view
	return payload, ok
}

func (c *Consumer) publishPending(block *types.Block, payload pendingPayload, generation uint64) bool {
	if !c.pendingHeadCurrent() {
		return false
	}
	return c.pendingStore().publishPayload(block, payload, generation)
}

func (s *PendingStore) begin(number uint64, parent common.Hash, canonicalBase bool) uint64 {
	key := pendingKey{number: number, parent: parent}
	var superseded []uint64
	s.mu.Lock()
	if entry := s.importingAtHeightLocked(number); entry != nil {
		generation := entry.generation
		s.mu.Unlock()
		return generation
	}
	for otherKey, entry := range s.entries {
		if otherKey.number == number && otherKey != key {
			superseded = append(superseded, entry.Number)
			s.removeLocked(otherKey)
		}
	}
	entry := s.entries[key]
	if entry == nil && len(s.entries) >= pendingEntryLimit {
		s.mu.Unlock()
		for _, height := range superseded {
			s.writeInvalidation(height, "superseded")
		}
		return 0
	}
	s.generation++
	if entry != nil {
		superseded = append(superseded, entry.Number)
	}
	entry = &pendingEntry{generation: s.generation, phase: PendingBuilding, canonicalBase: canonicalBase, Number: number, ParentHash: parent}
	s.entries[key] = entry
	preconfPendingEntries.Update(int64(len(s.entries)))
	s.active = key
	s.hasActive = true
	generation := entry.generation
	s.mu.Unlock()
	for _, height := range superseded {
		s.writeInvalidation(height, "superseded")
	}
	return generation
}

func (s *PendingStore) publishPayload(block *types.Block, payload pendingPayload, generation uint64) bool {
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	s.mu.Lock()
	if s.importingAtHeightLocked(key.number) != nil {
		s.mu.Unlock()
		return false
	}
	entry, ok := s.entryForPublishLocked(key, generation)
	if !ok {
		s.mu.Unlock()
		return false
	}
	superseded := s.removeCompetingLocked(key)
	s.setEntryLocked(entry, payload)
	s.active = key
	s.hasActive = true
	s.mu.Unlock()
	for _, number := range superseded {
		s.writeInvalidation(number, "superseded")
	}
	return true
}

func (s *PendingStore) acceptExecutedRecord(number uint64, parent common.Hash, generation uint64, start int, txs types.Transactions, receipts types.Receipts) bool {
	if len(txs) == 0 || len(txs) != len(receipts) {
		return false
	}
	for index, tx := range txs {
		receipt := receipts[index]
		if tx == nil || receipt == nil || receipt.BlockNumber == nil || receipt.BlockNumber.Uint64() != number {
			return false
		}
	}
	key := pendingKey{number: number, parent: parent}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil || entry.phase != PendingBuilding || entry.generation != generation ||
		entry.claimedHash != (common.Hash{}) || entry.rejected || len(entry.executedTransactions) != start {
		return false
	}
	entry.executedTransactions = append(entry.executedTransactions, txs...)
	entry.executedReceipts = append(entry.executedReceipts, receipts...)
	return true
}

func makePendingPayload(block *types.Block, receipts types.Receipts, statedb *state.StateDB, sealed *ReusableExecution) (pendingPayload, bool) {
	return makePendingPayloadWithPrevious(block, receipts, statedb, sealed, nil, common.Hash{})
}

func makePendingPayloadWithPrevious(block *types.Block, receipts types.Receipts, statedb *state.StateDB, sealed *ReusableExecution, previous *PendingRPCView, blockHash common.Hash) (pendingPayload, bool) {
	if block == nil || statedb == nil {
		return pendingPayload{}, false
	}
	return pendingPayload{view: buildRPCView(block, receipts, statedb, previous, blockHash), sealed: sealed, finalized: sealed != nil}, true
}

func (s *PendingStore) importingAtHeightLocked(number uint64) *pendingEntry {
	for key, entry := range s.entries {
		if key.number == number && entry.phase == PendingImporting {
			return entry
		}
	}
	return nil
}

func (s *PendingStore) removeCompetingLocked(key pendingKey) []uint64 {
	var superseded []uint64
	for otherKey, entry := range s.entries {
		if otherKey.number == key.number && otherKey != key {
			superseded = append(superseded, entry.Number)
			s.removeLocked(otherKey)
		}
	}
	return superseded
}

func (s *PendingStore) entryForPublishLocked(key pendingKey, generation uint64) (*pendingEntry, bool) {
	entry := s.entries[key]
	if entry == nil {
		if generation != 0 || len(s.entries) >= pendingEntryLimit {
			return nil, false
		}
		s.generation++
		entry = &pendingEntry{generation: s.generation, Number: key.number, ParentHash: key.parent}
		s.entries[key] = entry
		preconfPendingEntries.Update(int64(len(s.entries)))
		return entry, true
	}
	if entry.phase == PendingImporting || generation != 0 && entry.generation != generation {
		return nil, false
	}
	return entry, true
}

func (s *PendingStore) setEntryLocked(entry *pendingEntry, payload pendingPayload) {
	entry.RPCView = payload.view
	entry.Sealed = nil
	if entry.canonicalBase {
		entry.Sealed = payload.sealed
	}
	entry.partialClaim = false
	entry.rejected = false
	entry.deferredInvalidation = ""
	entry.phase = PendingBuilding
	if payload.finalized {
		entry.phase = PendingSealed
	}
}

func (s *PendingStore) ClaimPreconf(block *types.Block) (*core.PreconfExecution, bool) {
	if block == nil {
		return nil, false
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil || entry.phase != PendingSealed || !entry.canonicalBase || entry.Sealed == nil || entry.claimedHash != (common.Hash{}) {
		return nil, false
	}
	sealed := entry.Sealed
	if sealed.HeaderHash != block.Hash() || sealed.TxRoot != block.TxHash() || !sameTransactions(entry.RPCView.Block, block) {
		return nil, false
	}
	s.generation++
	entry.generation = s.generation
	entry.phase = PendingImporting
	entry.claimedHash = block.Hash()
	entry.partialClaim = false
	return &core.PreconfExecution{
		StateDB: sealed.StateDB.Copy(),
		Result:  cloneProcessResult(sealed.Result),
	}, true
}

func (s *PendingStore) claimPreconfPrefix(block *types.Block) (*pendingPrefix, bool) {
	if block == nil {
		return nil, false
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	s.mu.Lock()
	entry := s.entries[key]
	if !prefixEntryMatches(entry, block) {
		s.mu.Unlock()
		return nil, false
	}
	prefixTxs := entry.RPCView.Block.Transactions()
	remaining := len(block.Transactions()) - len(prefixTxs)
	if remaining > maxPreconfPrefixCatchupTransactions {
		s.mu.Unlock()
		return nil, false
	}
	result, ok := prefixProcessResult(entry.RPCView)
	if !ok || result.GasUsed > block.GasLimit() {
		s.mu.Unlock()
		return nil, false
	}
	view := entry.RPCView
	txs := append(types.Transactions(nil), prefixTxs...)
	entry.phase = PendingImporting
	entry.claimedHash = block.Hash()
	entry.partialClaim = true
	generation := entry.generation
	s.mu.Unlock()
	return &pendingPrefix{Transactions: txs, State: view.State, Result: result, Generation: generation}, true
}

func prefixEntryMatches(entry *pendingEntry, block *types.Block) bool {
	if entry == nil || entry.phase != PendingBuilding || !entry.canonicalBase || entry.rejected ||
		entry.claimedHash != (common.Hash{}) || entry.RPCView == nil || entry.RPCView.Block == nil {
		return false
	}
	txs := entry.RPCView.Block.Transactions()
	if len(txs) == 0 || !sameExecutionContext(entry.RPCView.Header, block.Header()) ||
		!sameTransactionPrefix(entry.RPCView.Block, block) {
		return false
	}
	for _, tx := range txs {
		if tx.Type() == types.StateSyncTxType {
			return false
		}
	}
	return true
}
