package sequencer

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

func TestPendingStoreRetainsEntriesAndInvalidatesReorg(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	store := NewPendingStore(db)
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	parent := common.HexToHash("0xaa")
	for number := uint64(1); number <= 3; number++ {
		header := &types.Header{Number: new(big.Int).SetUint64(number), ParentHash: parent, GasLimit: 30_000_000}
		block := types.NewBlock(header, &types.Body{}, nil, trie.NewStackTrie(nil))
		reusable := &ReusableExecution{
			HeaderHash: block.Hash(),
			TxRoot:     block.TxHash(),
			StateDB:    stateDB.Copy(),
			Result:     &core.ProcessResult{},
		}
		if !store.publish(block, nil, stateDB, reusable, 0) {
			t.Fatalf("publish %d", number)
		}
	}
	store.mu.RLock()
	entryCount := len(store.entries)
	store.mu.RUnlock()
	if entryCount != 3 {
		t.Fatalf("entry count = %d, want 3", entryCount)
	}
	records := rawdb.ReadInvalidPreconfs(db, rawdb.InvalidPreconfQueryLimit)
	if len(records) != 0 {
		t.Fatalf("unexpected invalidation records = %+v", records)
	}

	reorgParent := common.HexToHash("0xbb")
	header := &types.Header{Number: big.NewInt(5), ParentHash: reorgParent, GasLimit: 30_000_000}
	block := types.NewBlock(header, &types.Body{}, nil, trie.NewStackTrie(nil))
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("reorg candidate publish failed")
	}
	canonicalParent := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(4), Extra: []byte{1}})
	store.reconcileThrough(4, func(number uint64) *types.Block {
		if number == 4 {
			return canonicalParent
		}
		return nil
	}, func(common.Hash) types.Receipts { return nil })
	records = rawdb.ReadInvalidPreconfs(db, 1)
	if len(records) != 1 || records[0].Number != 5 || records[0].Reason != "reorged" {
		t.Fatalf("reorg records = %+v", records)
	}
}

func TestPendingStoreReconcilesSpeculativeAncestry(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	head := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), GasLimit: 30_000_000})
	store := NewPendingStore(nil)
	blocks := make([]*types.Block, 3)
	parent := head
	for index := range blocks {
		number := head.NumberU64() + uint64(index) + 1
		header := &types.Header{Number: new(big.Int).SetUint64(number), ParentHash: parent.Hash(), GasLimit: 30_000_000, Extra: []byte{byte(number)}}
		blocks[index] = types.NewBlockWithHeader(header)
		if !store.publish(blocks[index], nil, stateDB, nil, 0) {
			t.Fatalf("publish %d", number)
		}
		parent = blocks[index]
	}
	canonical := map[uint64]*types.Block{head.NumberU64(): head}
	lookup := func(number uint64) *types.Block { return canonical[number] }
	if _, invalidations := store.reconcileThroughMemory(head.NumberU64(), lookup, func(common.Hash) types.Receipts { return nil }); len(invalidations) != 0 {
		t.Fatalf("valid lineage invalidations = %+v", invalidations)
	}

	canonical[blocks[0].NumberU64()] = blocks[0]
	if _, invalidations := store.reconcileThroughMemory(blocks[0].NumberU64(), lookup, func(common.Hash) types.Receipts { return nil }); len(invalidations) != 0 {
		t.Fatalf("matching import invalidations = %+v", invalidations)
	}
	store.mu.RLock()
	remaining := len(store.entries)
	store.mu.RUnlock()
	if remaining != 2 {
		t.Fatalf("remaining descendants = %d, want 2", remaining)
	}

	replacement := types.NewBlockWithHeader(&types.Header{Number: blocks[0].Number(), ParentHash: head.Hash(), Extra: []byte("replacement")})
	canonical[blocks[0].NumberU64()] = replacement
	_, invalidations := store.reconcileThroughMemory(blocks[0].NumberU64(), lookup, func(common.Hash) types.Receipts { return nil })
	if len(invalidations) != 2 || invalidations[0].reason != "reorged" || invalidations[1].reason != "reorged" {
		t.Fatalf("descendant invalidations = %+v", invalidations)
	}
	store.mu.RLock()
	remaining = len(store.entries)
	store.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("reorg retained %d descendants", remaining)
	}
}

func TestReconciliationDefersClaimedEntryInvalidation(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{
		Number:     big.NewInt(1),
		ParentHash: common.HexToHash("0x01"),
		Extra:      []byte("claimed"),
	})
	store := NewPendingStore(nil)
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish claimed entry")
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	store.mu.Lock()
	entry := store.entries[key]
	entry.phase = PendingImporting
	entry.claimedHash = block.Hash()
	entry.Sealed = new(ReusableExecution)
	store.mu.Unlock()

	replacement := types.NewBlockWithHeader(&types.Header{
		Number:     block.Number(),
		ParentHash: block.ParentHash(),
		Extra:      []byte("replacement"),
	})
	_, invalidations := store.reconcileThroughMemory(block.NumberU64(), func(uint64) *types.Block {
		return replacement
	}, func(common.Hash) types.Receipts { return nil })
	if len(invalidations) != 0 {
		t.Fatalf("claimed entry invalidated immediately: %+v", invalidations)
	}
	store.mu.RLock()
	entry = store.entries[key]
	if entry == nil || entry.deferredInvalidation != "canonical_mismatch" {
		store.mu.RUnlock()
		t.Fatalf("deferred invalidation = %v", entry)
	}
	store.mu.RUnlock()

	_, invalidations, removed, _ := store.completePreconf(block, nil, false)
	if !removed || len(invalidations) != 1 || invalidations[0].reason != "canonical_mismatch" {
		t.Fatalf("claim completion = removed %v, invalidations %+v", removed, invalidations)
	}
}

func TestReconciliationDefersClaimedFutureReorg(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), Extra: []byte("anchor")})
	block := types.NewBlockWithHeader(&types.Header{
		Number:     big.NewInt(3),
		ParentHash: common.HexToHash("0x03"),
	})
	store := NewPendingStore(nil)
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish future entry")
	}
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	store.mu.Lock()
	entry := store.entries[key]
	entry.phase = PendingImporting
	entry.claimedHash = block.Hash()
	store.mu.Unlock()

	_, invalidations := store.reconcileThroughMemory(anchor.NumberU64(), func(number uint64) *types.Block {
		if number == anchor.NumberU64() {
			return anchor
		}
		return nil
	}, func(common.Hash) types.Receipts { return nil })
	if len(invalidations) != 0 {
		t.Fatalf("claimed future invalidated immediately: %+v", invalidations)
	}
	store.mu.RLock()
	entry = store.entries[key]
	if entry == nil || entry.deferredInvalidation != "reorged" {
		store.mu.RUnlock()
		t.Fatalf("deferred future invalidation = %v", entry)
	}
	store.mu.RUnlock()
}

func TestCanonicalHeadClearsReorgedReceiptSuffix(t *testing.T) {
	h := partialReuseHarness(t)
	consumer, err := NewConsumer("", h.chain)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	h.chain.SetPreconfProvider(consumer)
	stateDB, err := h.chain.StateAt(h.chain.CurrentBlock().Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	parent := h.chain.CurrentBlock()
	tx1 := h.transfer(t, 0)
	block1 := types.NewBlock(
		&types.Header{Number: new(big.Int).Add(parent.Number, common.Big1), ParentHash: parent.Hash(), GasLimit: parent.GasLimit, Extra: []byte("old")},
		&types.Body{Transactions: types.Transactions{tx1}}, nil, trie.NewStackTrie(nil),
	)
	tx2 := h.transfer(t, 1)
	block2 := types.NewBlock(
		&types.Header{Number: new(big.Int).Add(block1.Number(), common.Big1), ParentHash: block1.Hash(), GasLimit: parent.GasLimit},
		&types.Body{Transactions: types.Transactions{tx2}}, nil, trie.NewStackTrie(nil),
	)
	receipt1 := &types.Receipt{TxHash: tx1.Hash(), BlockNumber: block1.Number()}
	receipt2 := &types.Receipt{TxHash: tx2.Hash(), BlockNumber: block2.Number()}
	store := consumer.pendingStore()
	if !store.publish(block1, types.Receipts{receipt1}, stateDB, nil, 0) ||
		!store.publish(block2, types.Receipts{receipt2}, stateDB, nil, 0) {
		t.Fatal("publish speculative lineage")
	}
	consumer.Index().Add(tx1, receipt1)
	consumer.Index().Add(tx2, receipt2)

	canonical, _ := buildPartialReuseBlock(t, h, nil)
	if _, err := h.chain.InsertChain(types.Blocks{canonical}, false); err != nil {
		t.Fatalf("insert canonical replacement: %v", err)
	}
	if consumer.PendingBlock() != nil {
		t.Fatal("stale pending block was visible before reconciliation")
	}
	if _, _, ok := consumer.LookupPreconf(tx2.Hash()); ok {
		t.Fatal("stale receipt was visible before reconciliation")
	}

	consumer.handleCanonicalHead()
	for _, tx := range []*types.Transaction{tx1, tx2} {
		if _, _, ok := consumer.Index().Lookup(tx.Hash()); ok {
			t.Fatalf("receipt %s survived reconciliation", tx.Hash())
		}
	}
	store.mu.RLock()
	remaining := len(store.entries)
	store.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("future store entries after reconciliation = %d", remaining)
	}
	records := rawdb.ReadInvalidPreconfs(h.chain.DB(), rawdb.InvalidPreconfQueryLimit)
	found := false
	for _, record := range records {
		if record.Number == block2.NumberU64() && record.Reason == "reorged" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing descendant invalidation: %+v", records)
	}
}

func TestPendingReadRejectsReconciledSnapshot(t *testing.T) {
	h := startExecHarness(t)
	consumer, err := NewConsumer("", h.chain)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	stateDB, err := h.chain.StateAt(h.chain.CurrentBlock().Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{Number: new(big.Int).Add(h.chain.CurrentBlock().Number, common.Big1), ParentHash: h.chain.CurrentBlock().Hash()})
	store := consumer.pendingStore()
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	store.mu.Lock()
	entry := store.entries[pendingKey{number: block.NumberU64(), parent: block.ParentHash()}]
	entry.RPCView.State = &blockingPendingStateReader{PendingStateReader: entry.RPCView.State, started: started, release: release}
	store.mu.Unlock()

	type result struct {
		block *types.Block
		err   error
	}
	done := make(chan result, 1)
	go func() {
		pending, _, err := consumer.PendingState(context.Background())
		done <- result{block: pending, err: err}
	}()
	<-started
	consumer.reconciled.Store(types.CopyHeader(h.chain.CurrentBlock()))
	close(release)
	got := <-done
	if got.err != nil || got.block != nil {
		t.Fatalf("reconciled read = %v, %v", got.block, got.err)
	}
}

func TestPendingStateCopyRespectsCancellation(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	store := NewPendingStore(nil)
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	store.mu.Lock()
	entry := store.entries[pendingKey{number: block.NumberU64(), parent: block.ParentHash()}]
	entry.RPCView.State = &blockingPendingStateReader{PendingStateReader: entry.RPCView.State, started: started, release: release}
	store.mu.Unlock()
	done := make(chan struct{})
	go func() {
		store.PendingState(context.Background())
		close(done)
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := store.PendingState(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second state copy error = %v", err)
	}
	close(release)
	<-done
}

func TestPendingStateLeaseLastsUntilRequestEnds(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	store := NewPendingStore(nil)
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish")
	}

	first, cancelFirst := context.WithCancel(context.Background())
	if _, _, err := store.PendingState(first); err != nil {
		t.Fatalf("first pending state: %v", err)
	}
	second, cancelSecond := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelSecond()
	if _, _, err := store.PendingState(second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent pending state error = %v", err)
	}
	cancelFirst()

	third, cancelThird := context.WithTimeout(context.Background(), time.Second)
	defer cancelThird()
	if _, _, err := store.PendingState(third); err != nil {
		t.Fatalf("pending state after release: %v", err)
	}
}

func TestPendingWaitsForStateCopy(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	store := NewPendingStore(nil)
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish")
	}
	if !store.acquireStateCopy(t.Context()) {
		t.Fatal("state copy slot")
	}
	done := make(chan *types.Block, 1)
	go func() {
		pending, _, _ := store.Pending()
		done <- pending
	}()
	select {
	case <-done:
		store.releaseStateCopy()
		t.Fatal("pending read did not wait")
	case <-time.After(20 * time.Millisecond):
	}
	store.releaseStateCopy()
	select {
	case pending := <-done:
		if pending != block {
			t.Fatalf("pending block = %v", pending)
		}
	case <-time.After(time.Second):
		t.Fatal("pending read did not finish")
	}
}
