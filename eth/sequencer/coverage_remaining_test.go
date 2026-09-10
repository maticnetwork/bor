package sequencer

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

type cancelingPendingStateReader struct {
	PendingStateReader
	cancel context.CancelFunc
}

func (r *cancelingPendingStateReader) NewStateDB() (*state.StateDB, error) {
	statedb, err := r.PendingStateReader.NewStateDB()
	r.cancel()
	return statedb, err
}

func TestConsumerPendingLogRange(t *testing.T) {
	h := startExecHarness(t)
	consumer, err := NewConsumer("", h.chain)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	parent := newPendingRPCCoverageFixture(t, h.chain.CurrentBlock().Number.Uint64()+1, h.chain.CurrentBlock().Hash())
	child := newPendingRPCCoverageFixture(t, parent.block.NumberU64()+1, parent.block.Hash())
	for _, fixture := range []*pendingRPCCoverageFixture{parent, child} {
		if !consumer.pendingStore().publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
			t.Fatalf("publish block %d", fixture.block.NumberU64())
		}
	}

	anchor, blocks, receipts := consumer.PendingLogRange()
	if anchor == nil || anchor.Hash() != h.chain.CurrentBlock().Hash() {
		t.Fatalf("pending anchor = %v", anchor)
	}
	if len(blocks) != 2 || blocks[0] != parent.block || blocks[1] != child.block {
		t.Fatalf("pending blocks = %v", blocks)
	}
	if len(receipts) != 2 || receipts[0][0].TxHash != parent.tx.Hash() || receipts[1][0].TxHash != child.tx.Hash() {
		t.Fatalf("pending receipts = %v", receipts)
	}
}

func TestPendingSnapshotCancellationAfterCopy(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
		t.Fatal("publish failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	key := pendingKey{number: fixture.block.NumberU64(), parent: fixture.block.ParentHash()}
	store.mu.Lock()
	entry := store.entries[key]
	entry.RPCView.State = &cancelingPendingStateReader{PendingStateReader: entry.RPCView.State, cancel: cancel}
	store.mu.Unlock()

	block, receipts, statedb, err := store.PendingSnapshot(ctx)
	if block != nil || receipts != nil || statedb != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot = %v, %v, %v, %v", block, receipts, statedb, err)
	}
	if block, _, _, err := store.PendingSnapshot(context.Background()); block != fixture.block || err != nil {
		t.Fatalf("state-copy lease was not released: %v, %v", block, err)
	}
}

func TestSessionUnverifiedPublicationPaths(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))

	t.Run("published", func(t *testing.T) {
		consumer := &Consumer{index: NewIndex(), store: NewPendingStore(nil)}
		t.Cleanup(consumer.Close)
		receipts := make(chan core.PreconfReceiptsEvent, 1)
		sub := consumer.SubscribePreconfReceipts(receipts)
		defer sub.Unsubscribe()
		generation := consumer.pendingStore().begin(fixture.block.NumberU64(), fixture.block.ParentHash(), true)
		payload, ok := makePendingPayload(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil)
		if !ok {
			t.Fatal("pending payload rejected")
		}
		s := &session{consumer: consumer, env: &blockEnv{
			generation: generation,
			header:     fixture.block.Header(),
			statedb:    fixture.state,
			txs:        types.Transactions{fixture.tx},
			receipts:   types.Receipts{fixture.receipt},
		}}
		s.publishUnverifiedSeal(fixture.block, payload, fixture.block.Header(), fixture.block.Hash())
		if s.env != nil || s.parked != fixture.state || s.tip != fixture.block.Hash() || s.verified != nil {
			t.Fatalf("unverified seal state = env:%v parked:%v tip:%s verified:%v", s.env, s.parked, s.tip, s.verified)
		}
		if _, _, found := consumer.index.Lookup(fixture.tx.Hash()); !found {
			t.Fatal("unverified receipt was not indexed")
		}
		select {
		case published := <-receipts:
			if len(published.Receipts) != 1 || published.Receipts[0].TxHash != fixture.tx.Hash() ||
				len(published.Transactions) != 1 || published.Transactions[0] != fixture.tx {
				t.Fatalf("receipt event = %#v", published)
			}
		case <-time.After(time.Second):
			t.Fatal("receipt publication was not announced")
		}
	})

	t.Run("stale generation", func(t *testing.T) {
		consumer := &Consumer{index: NewIndex(), store: NewPendingStore(nil)}
		generation := consumer.pendingStore().begin(fixture.block.NumberU64(), fixture.block.ParentHash(), true)
		payload, ok := makePendingPayload(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil)
		if !ok {
			t.Fatal("pending payload rejected")
		}
		s := &session{consumer: consumer, env: &blockEnv{
			generation: generation + 1,
			header:     fixture.block.Header(),
			statedb:    fixture.state,
		}}
		s.publishUnverifiedSeal(fixture.block, payload, fixture.block.Header(), fixture.block.Hash())
		if s.env != nil || s.parked != nil {
			t.Fatal("stale unverified publication retained execution state")
		}
	})
}

func TestPendingHelperRemainingEdges(t *testing.T) {
	view := &PendingRPCView{Logs: []*types.Log{nil}, receiptBlockHash: common.HexToHash("0x1")}
	if logs := removedLogs(view); len(logs) != 1 || logs[0] != nil {
		t.Fatalf("removed nil logs = %v", logs)
	}
	entry := &pendingEntry{executedReceipts: types.Receipts{{Logs: []*types.Log{nil, {}}}}}
	if logs := removedEntryLogs(entry); len(logs) != 2 || logs[0] != nil || logs[1] == nil || !logs[1].Removed {
		t.Fatalf("removed entry logs = %v", logs)
	}

	result := cloneProcessResult(&core.ProcessResult{Logs: []*types.Log{nil}})
	if len(result.Logs) != 1 || result.Logs[0] != nil {
		t.Fatalf("cloned nil process log = %v", result.Logs)
	}
	receipt := cloneReceipt(&types.Receipt{Logs: []*types.Log{nil}})
	if len(receipt.Logs) != 1 || receipt.Logs[0] != nil {
		t.Fatalf("cloned nil receipt log = %v", receipt.Logs)
	}
	hash := common.HexToHash("0x2")
	receipt = cloneReceiptWithBlockHash(&types.Receipt{Logs: []*types.Log{nil}}, hash)
	if receipt.BlockHash != hash || receipt.Logs[0] != nil {
		t.Fatalf("block-hash clone = %+v", receipt)
	}

	tx := types.NewTransaction(0, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	if sameTransactionSlicePrefix(types.Transactions{tx, tx}, types.Transactions{tx}) {
		t.Fatal("long transaction slice matched a shorter block")
	}
	if sameTransactionSlicePrefix(types.Transactions{nil}, types.Transactions{tx}) {
		t.Fatal("nil transaction matched a block transaction")
	}
	if sameConsensusReceipts(types.Receipts{{}}, nil) {
		t.Fatal("receipt slices of different lengths matched")
	}
	if !sameConsensusReceipt(nil, nil) || sameConsensusReceipt(nil, new(types.Receipt)) {
		t.Fatal("nil receipt equality is not exact")
	}
	if sameConsensusReceipt(&types.Receipt{Logs: []*types.Log{nil}}, &types.Receipt{Logs: []*types.Log{{}}}) {
		t.Fatal("nil log matched a non-nil log")
	}
	if !sameConsensusReceipt(&types.Receipt{Logs: []*types.Log{nil}}, &types.Receipt{Logs: []*types.Log{nil}}) {
		t.Fatal("matching nil logs differed")
	}
}

func TestExecutionRemainingBranches(t *testing.T) {
	new(speculativeFinalizationChain).SetStateSync(nil)
	want := errors.New("prepared transaction failed")
	if _, _, err := new(blockEnv).applyPrepared(nil, preparedTransaction{err: want}); !errors.Is(err, want) {
		t.Fatalf("prepared error = %v", err)
	}

	h := startExecHarness(t)
	parent := h.chain.CurrentBlock()
	statedb, err := h.chain.StateAt(parent.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	env := newBlockEnv(h.chain, statedb, openOn(parent, h.config, commitment.Head{}).GetBlockOpen(), nil)
	tx := h.transfer(t, 0)
	if got, receipt, err := env.applyPrepared(nil, preparedTransaction{tx: tx}); err != nil || got != tx || receipt == nil {
		t.Fatalf("unverified prepared transaction = %v, %v, %v", got, receipt, err)
	}

	stateSync := types.NewTx(new(types.StateSyncTx))
	header := pendingHeader(openOn(parent, h.config, commitment.Head{}).GetBlockOpen())
	block := types.NewBlock(header, &types.Body{Transactions: types.Transactions{tx, stateSync}}, nil, trie.NewStackTrie(nil))
	prefix := &pendingPrefix{
		Transactions: types.Transactions{tx},
		StateDB:      statedb.Copy(),
		Result: &core.ProcessResult{Receipts: types.Receipts{{
			TxHash: tx.Hash(), BlockNumber: block.Number(),
		}}},
	}
	consumer := &Consumer{chain: h.chain}
	if _, err := consumer.completePreconfPrefix(block, prefix); err == nil {
		t.Fatal("state-sync suffix used a preconfirmation prefix")
	}

	consumer = &Consumer{index: NewIndex(), store: NewPendingStore(nil)}
	s := &session{consumer: consumer, env: &blockEnv{
		txs:        make(types.Transactions, pendingEagerPublicationTxs),
		indexedTxs: pendingEagerPublicationTxs,
	}}
	s.executePreparedRecord(new(pb.Record), nil)
	if s.env != nil {
		t.Fatal("invalid pending snapshot retained its environment")
	}
	consumer.sealVerify.Store(true)
	if err := consumer.verifyPreconfSeal(nil, nil); !errors.Is(err, errSealVerificationDeferred) {
		t.Fatalf("concurrent seal verification = %v", err)
	}
}

func TestSessionPredicateEdges(t *testing.T) {
	consumer := new(Consumer)
	consumer.clearCanonicalHandoff(nil)
	consumer.clearCanonicalHandoffThrough(nil)
	consumer.clearCanonicalHandoffThrough(new(types.Header))
	if detachedCanonicalHash(nil) != (common.Hash{}) || detachedCanonicalHash(new(blockEnv)) != (common.Hash{}) {
		t.Fatal("empty detached canonical state returned a hash")
	}
	if consumer.canonicalTargetActive(nil) || consumer.canonicalTargetActive(new(types.Header)) {
		t.Fatal("incomplete canonical target was active")
	}
	if consumer.canonicalTransitionMatches(nil) || consumer.canonicalTransitionMatches(new(types.Header)) {
		t.Fatal("incomplete canonical transition matched")
	}
	if state := (*session)(nil).preparationSnapshot(); state != (streamPreparationState{}) {
		t.Fatalf("nil preparation snapshot = %+v", state)
	}
}

func TestRemainingStoreAndIndexEdges(t *testing.T) {
	store := NewPendingStore(nil)
	if statedb, err := store.PendingParentState(nil); statedb != nil || err != nil {
		t.Fatalf("nil pending parent state = %v, %v", statedb, err)
	}
	if statedb, err := store.PendingParentState(types.NewBlockWithHeader(&types.Header{Number: new(big.Int)})); statedb != nil || err != nil {
		t.Fatalf("genesis pending parent state = %v, %v", statedb, err)
	}
	key := pendingKey{number: 2, parent: common.HexToHash("0x1")}
	store.entries[key] = nil
	if logs, invalidations := store.reconcileFutureLocked(1, nil); len(logs) != 0 || len(invalidations) != 0 {
		t.Fatalf("nil future entry = %v, %v", logs, invalidations)
	}
	if hash, broken := futureAnchor(nil); hash != (common.Hash{}) || !broken {
		t.Fatalf("nil future anchor = %s, %v", hash, broken)
	}
	if pendingViewBlock(new(pendingEntry)) != nil {
		t.Fatal("empty pending entry returned a block")
	}

	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	badReceipt := cloneReceipt(fixture.receipt)
	badReceipt.BlockNumber = big.NewInt(4)
	if store.acceptExecutedRecord(3, fixture.block.ParentHash(), 1, 0,
		types.Transactions{fixture.tx, fixture.tx}, types.Receipts{fixture.receipt, badReceipt}) {
		t.Fatal("mixed-height receipt batch was accepted")
	}
	if _, ok := preparePendingPayload(&blockEnv{}, fixture.block, common.Hash{}, nil); ok {
		t.Fatal("payload without state was accepted")
	}

	index := NewIndex()
	index.dropLowestLocked()
	if index.CountCanonical(nil) != 0 {
		t.Fatal("nil canonical block was counted")
	}
	index.byBlock[3] = []common.Hash{common.HexToHash("0xdead")}
	index.Seal(3, common.HexToHash("0xbeef"))
	other := cloneReceipt(fixture.receipt)
	other.BlockNumber = big.NewInt(4)
	if index.AddBatch(types.Transactions{fixture.tx, fixture.tx}, types.Receipts{fixture.receipt, other}) {
		t.Fatal("mixed-height index batch was accepted")
	}
}

func TestEntryRejectsOversizedBaseFee(t *testing.T) {
	entry := openEntry(commitment.OpenContext{
		ParentHash: [32]byte{1},
		BaseFee:    new(big.Int).Lsh(big.NewInt(1), 8*pendingOpenBaseFeeLimit),
	}, commitment.Head{})
	if err := validateEntryShape(entry); err == nil {
		t.Fatal("oversized base fee was accepted")
	}
}
