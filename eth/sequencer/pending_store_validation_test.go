package sequencer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

func publishSizedPrefix(t *testing.T, prefixCount, totalCount int) (*PendingStore, *types.Block) {
	t.Helper()
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	txs := make(types.Transactions, totalCount)
	receipts := make(types.Receipts, prefixCount)
	for index := range txs {
		txs[index] = types.NewTransaction(uint64(index), common.Address{0xee}, big.NewInt(1), 21_000, big.NewInt(1), nil)
		if index < prefixCount {
			receipts[index] = &types.Receipt{
				TxHash:            txs[index].Hash(),
				BlockNumber:       big.NewInt(3),
				TransactionIndex:  uint(index),
				Status:            types.ReceiptStatusSuccessful,
				GasUsed:           21_000,
				CumulativeGasUsed: uint64(index+1) * 21_000,
			}
		}
	}
	header := fixture.block.Header()
	header.GasLimit = 300_000_000
	header.GasUsed = receipts[len(receipts)-1].CumulativeGasUsed
	prefix := types.NewBlock(header, &types.Body{Transactions: txs[:prefixCount]}, receipts, trie.NewStackTrie(nil))
	candidate := types.NewBlock(types.CopyHeader(header), &types.Body{Transactions: txs}, nil, trie.NewStackTrie(nil))
	store := NewPendingStore(nil)
	generation := store.begin(prefix.NumberU64(), prefix.ParentHash(), true)
	if !store.publish(prefix, receipts, fixture.state, nil, generation) {
		t.Fatal("prefix publish failed")
	}
	return store, candidate
}

func TestPendingStoreBoundsSerialPrefixCatchup(t *testing.T) {
	t.Run("80 of 100 reuses", func(t *testing.T) {
		store, candidate := publishSizedPrefix(t, 80, 100)
		prefix, ok := store.claimPreconfPrefix(candidate)
		if !ok || len(prefix.Transactions) != 80 {
			t.Fatalf("prefix = %+v, ok = %v", prefix, ok)
		}
	})

	t.Run("800 of 1000 reuses", func(t *testing.T) {
		store, candidate := publishSizedPrefix(t, 800, 1000)
		prefix, ok := store.claimPreconfPrefix(candidate)
		if !ok || len(prefix.Transactions) != 800 {
			t.Fatalf("prefix = %+v, ok = %v", prefix, ok)
		}
	})

	t.Run("16 of 2000 uses normal import", func(t *testing.T) {
		store, candidate := publishSizedPrefix(t, 16, 2000)
		if prefix, ok := store.claimPreconfPrefix(candidate); ok || prefix != nil {
			t.Fatalf("oversized serial catch-up = %+v, %v", prefix, ok)
		}
		key := pendingKey{number: candidate.NumberU64(), parent: candidate.ParentHash()}
		store.mu.RLock()
		phase := store.entries[key].phase
		store.mu.RUnlock()
		if phase != PendingBuilding {
			t.Fatalf("rejected catch-up changed phase to %v", phase)
		}
	})
}

func TestPendingStoreInvalidOperationsLeaveNoState(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	if store.publish(nil, nil, fixture.state, nil, 0) {
		t.Fatal("nil block was published")
	}
	if store.publish(fixture.block, nil, nil, nil, 0) {
		t.Fatal("nil state was published")
	}
	if execution, ok := store.ClaimPreconf(nil); ok || execution != nil {
		t.Fatalf("nil block claim = %+v, %v", execution, ok)
	}
	if prefix, ok := store.claimPreconfPrefix(nil); ok || prefix != nil {
		t.Fatalf("nil prefix claim = %+v, %v", prefix, ok)
	}
	if reason, invalid := store.CheckPreconfInvalidation(nil, nil); invalid || reason != "" {
		t.Fatalf("nil invalidation = %q, %v", reason, invalid)
	}
	if logs := store.RejectClaimedPreconf(nil); len(logs) != 0 {
		t.Fatalf("nil rejection logs = %+v", logs)
	}
	if logs := store.CompletePreconf(nil, false); len(logs) != 0 {
		t.Fatalf("nil completion logs = %+v", logs)
	}
	if entryMatchesCanonical(new(pendingEntry), fixture.block, nil) {
		t.Fatal("entry without an RPC view matched canonical data")
	}
	if block, payload, ok := preparePending(&blockEnv{statedb: fixture.state}, nil, common.Hash{}, nil); ok || block != nil || payload.view != nil {
		t.Fatalf("pending data without a header = %v, %+v, %v", block, payload, ok)
	}

	store.mu.Lock()
	store.removeLocked(pendingKey{number: 99})
	store.mu.Unlock()
	if len(store.entries) != 0 || store.hasActive {
		t.Fatal("invalid operations mutated the pending store")
	}
}

func TestPendingStoreFailedCompletionPreservesUnclaimedEntry(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
		t.Fatal("publish failed")
	}
	if logs := store.CompletePreconf(fixture.block, false); len(logs) != 0 {
		t.Fatalf("unclaimed completion logs = %+v", logs)
	}
	if pending := store.PendingBlock(); pending != fixture.block {
		t.Fatalf("unclaimed entry was removed: %v", pending)
	}
}

func TestConsumerRejectsUnavailablePrefixClaims(t *testing.T) {
	h := startExecHarness(t)

	if prefix, ok := (&Consumer{}).ClaimPreconfPrefix(nil); ok || prefix != nil {
		t.Fatalf("nil block prefix = %+v, %v", prefix, ok)
	}
	sprintBlock := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(16)})
	consumer := &Consumer{chain: h.chain, index: NewIndex()}
	if prefix, ok := consumer.ClaimPreconfPrefix(sprintBlock); ok || prefix != nil {
		t.Fatalf("sprint-boundary prefix = %+v, %v", prefix, ok)
	}
	stateSyncBlock := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(3)}).WithBody(types.Body{
		Transactions: types.Transactions{types.NewTx(&types.StateSyncTx{})},
	})
	if prefix, ok := consumer.ClaimPreconfPrefix(stateSyncBlock); ok || prefix != nil {
		t.Fatalf("state-sync prefix = %+v, %v", prefix, ok)
	}
	ordinary := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(3)}).WithBody(types.Body{
		Transactions: types.Transactions{h.transfer(t, 0)},
	})
	if prefix, ok := consumer.ClaimPreconfPrefix(ordinary); ok || prefix != nil {
		t.Fatalf("missing prefix = %+v, %v", prefix, ok)
	}
}

func TestConsumerReleasesPrefixWhenWorkerIsUnavailable(t *testing.T) {
	h := startExecHarness(t)
	store := NewPendingStore(nil)
	_, candidate := publishPrefixWithTail(t, store)
	consumer := &Consumer{chain: h.chain, index: NewIndex(), store: store}
	consumer.BeginPreconfImport(candidate)
	if execution, ok := consumer.ClaimPreconfPrefix(candidate); ok || execution != nil {
		t.Fatalf("prefix without worker = %+v, %v", execution, ok)
	}
	if !consumer.canonicalHandoffMatches(candidate.Hash(), candidate.NumberU64()+1) {
		t.Fatal("failed cache claim cleared the canonical import handoff")
	}
	if pending := store.PendingBlock(); pending != nil {
		t.Fatalf("released prefix remained pending: %v", pending)
	}
	consumer.CompletePreconf(candidate, nil, false)
}

func TestConsumerRejectsClaimsFromStaleHead(t *testing.T) {
	h := startExecHarness(t)
	consumer := &Consumer{chain: h.chain, index: NewIndex()}
	consumer.reconciled.Store(&types.Header{Number: big.NewInt(99)})
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(3)})
	if execution, ok := consumer.ClaimPreconf(block); ok || execution != nil {
		t.Fatalf("stale-head claim = %+v, %v", execution, ok)
	}
}
