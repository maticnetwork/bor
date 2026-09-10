package sequencer

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
)

func (c *Consumer) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	block, receipts, statedb, _ := c.PendingSnapshot(context.Background())
	return block, receipts, statedb
}

func (c *Consumer) PendingState(ctx context.Context) (*types.Block, *state.StateDB, error) {
	block, _, statedb, err := c.PendingSnapshot(ctx)
	return block, statedb, err
}

func (s *PendingStore) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	block, receipts, statedb, _ := s.PendingSnapshot(context.Background())
	return block, receipts, statedb
}

func (s *PendingStore) PendingState(ctx context.Context) (*types.Block, *state.StateDB, error) {
	block, _, statedb, err := s.PendingSnapshot(ctx)
	return block, statedb, err
}

func (s *PendingStore) publish(block *types.Block, receipts types.Receipts, statedb *state.StateDB, sealed *ReusableExecution, generation uint64) bool {
	payload, ok := makePendingPayload(block, receipts, statedb, sealed)
	if !ok {
		return false
	}
	return s.publishPayload(block, payload, generation)
}

func (s *PendingStore) reconcileThrough(number uint64, canonical func(uint64) *types.Block, canonicalReceipts func(common.Hash) types.Receipts) []*types.Log {
	logs, invalidations := s.reconcileThroughMemory(number, canonical, canonicalReceipts)
	s.writeInvalidations(invalidations)
	return logs
}

func (s *PendingStore) CheckPreconfInvalidation(block *types.Block, receipts types.Receipts) (string, bool) {
	if block == nil {
		return "", false
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	s.mu.RLock()
	entry := s.entries[key]
	mismatch := entry != nil && entry.RPCView != nil && !entryMatchesCanonical(entry, block, receipts)
	s.mu.RUnlock()
	if !mismatch {
		return "", false
	}
	return "canonical_mismatch", true
}
