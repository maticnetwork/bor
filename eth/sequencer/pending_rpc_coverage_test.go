package sequencer

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

type pendingRPCCoverageFixture struct {
	address *common.Address
	block   *types.Block
	receipt *types.Receipt
	state   *state.StateDB
	tx      *types.Transaction
}

func newPendingRPCCoverageFixture(t *testing.T, number uint64, parent common.Hash) *pendingRPCCoverageFixture {
	t.Helper()
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	address := common.Address{byte(number), 0x1}
	statedb.SetNonce(address, number+4, tracing.NonceChangeUnspecified)
	tx := types.NewTransaction(number, common.Address{0xff}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	receipt := &types.Receipt{
		TxHash:            tx.Hash(),
		BlockNumber:       new(big.Int).SetUint64(number),
		TransactionIndex:  0,
		Status:            types.ReceiptStatusSuccessful,
		GasUsed:           21_000,
		CumulativeGasUsed: 21_000,
		Logs: []*types.Log{{
			Address:     address,
			BlockNumber: number,
			TxHash:      tx.Hash(),
		}},
	}
	block := types.NewBlock(
		&types.Header{
			Number:     new(big.Int).SetUint64(number),
			ParentHash: parent,
			GasLimit:   30_000_000,
			GasUsed:    receipt.CumulativeGasUsed,
			Difficulty: big.NewInt(1),
			Time:       number,
		},
		&types.Body{Transactions: types.Transactions{tx}},
		types.Receipts{receipt},
		trie.NewStackTrie(nil),
	)
	receipt.BlockHash = block.Hash()
	receipt.Logs[0].BlockHash = block.Hash()
	return &pendingRPCCoverageFixture{address: &address, block: block, receipt: receipt, state: statedb, tx: tx}
}

// publishPrefixWithTail publishes a single-transaction prefix into store and
// returns it alongside a candidate block that appends one more transaction on
// the same header, which is the shape a partial preconfirmation claim sees.
func publishPrefixWithTail(t *testing.T, store *PendingStore) (*pendingRPCCoverageFixture, *types.Block) {
	t.Helper()

	prefix := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	generation := store.begin(prefix.block.NumberU64(), prefix.block.ParentHash(), true)
	if !store.publish(prefix.block, types.Receipts{prefix.receipt}, prefix.state, nil, generation) {
		t.Fatal("prefix publish failed")
	}
	tail := types.NewTransaction(prefix.tx.Nonce()+1, common.Address{0xee}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	candidate := types.NewBlock(
		types.CopyHeader(prefix.block.Header()),
		&types.Body{Transactions: types.Transactions{prefix.tx, tail}},
		nil,
		trie.NewStackTrie(nil),
	)

	return prefix, candidate
}

func TestPendingRPCMetadataAccessors(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
		t.Fatal("publish failed")
	}

	block, receipts := store.PendingBlockAndReceipts()
	if block != fixture.block || len(receipts) != 1 || receipts[0].TxHash != fixture.tx.Hash() {
		t.Fatalf("store metadata = %v, %+v", block, receipts)
	}
	if receipts[0] == fixture.receipt {
		t.Fatal("store returned the source receipt")
	}
	blocks, receiptSets := store.PendingLogRange()
	if len(blocks) != 1 || blocks[0] != fixture.block || len(receiptSets) != 1 || len(receiptSets[0]) != 1 {
		t.Fatalf("store pending log range = %v, %v", blocks, receiptSets)
	}
	nonce, found, err := store.PendingNonce(*fixture.address)
	if err != nil || !found || nonce != 7 {
		t.Fatalf("store nonce = %d, %v, %v", nonce, err, found)
	}
	index := NewIndex()
	index.Add(fixture.tx, fixture.receipt)
	consumer := &Consumer{store: store, index: index}
	block, receipts = consumer.PendingBlockAndReceipts()
	if block != fixture.block || len(receipts) != 1 || receipts[0].TxHash != fixture.tx.Hash() {
		t.Fatalf("consumer metadata = %v, %+v", block, receipts)
	}
	nonce, found, err = consumer.PendingNonce(*fixture.address)
	if err != nil || !found || nonce != 7 {
		t.Fatalf("consumer nonce = %d, %v, %v", nonce, err, found)
	}
	tx, receipt, found := consumer.LookupPreconf(fixture.tx.Hash())
	if !found || tx != fixture.tx || receipt == nil || receipt.TxHash != fixture.tx.Hash() {
		t.Fatalf("consumer lookup = %v, %+v, %v", tx, receipt, found)
	}
}

func TestPendingLogRangeFollowsActiveChain(t *testing.T) {
	store := NewPendingStore(nil)
	parent := newPendingRPCCoverageFixture(t, 2, common.HexToHash("0x1"))
	child := newPendingRPCCoverageFixture(t, 3, parent.block.Hash())
	for _, fixture := range []*pendingRPCCoverageFixture{parent, child} {
		if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
			t.Fatalf("publish block %d", fixture.block.NumberU64())
		}
	}

	blocks, receipts := store.PendingLogRange()
	if len(blocks) != 2 || blocks[0] != parent.block || blocks[1] != child.block {
		t.Fatalf("pending blocks = %v", blocks)
	}
	if len(receipts) != 2 || receipts[0][0].TxHash != parent.tx.Hash() || receipts[1][0].TxHash != child.tx.Hash() {
		t.Fatalf("pending receipts = %v", receipts)
	}
}

func TestPendingParentState(t *testing.T) {
	store := NewPendingStore(nil)
	parent := newPendingRPCCoverageFixture(t, 2, common.HexToHash("0x1"))
	if !store.publish(parent.block, types.Receipts{parent.receipt}, parent.state, nil, 0) {
		t.Fatal("parent publish failed")
	}
	child := newPendingRPCCoverageFixture(t, 3, parent.block.Hash())
	if !store.publish(child.block, types.Receipts{child.receipt}, child.state, nil, 0) {
		t.Fatal("child publish failed")
	}
	parentState, err := store.PendingParentState(child.block)
	if err != nil {
		t.Fatalf("pending parent state: %v", err)
	}
	if parentState == nil || parentState.GetNonce(*parent.address) != 6 {
		t.Fatalf("parent state = %v", parentState)
	}
	consumer := &Consumer{store: store}
	parentState, err = consumer.PendingParentState(t.Context(), child.block)
	if err != nil {
		t.Fatalf("consumer pending parent state: %v", err)
	}
	if parentState == nil || parentState.GetNonce(*parent.address) != 6 {
		t.Fatalf("consumer parent state = %v", parentState)
	}
}

func TestPendingRPCEmptyStore(t *testing.T) {
	store := NewPendingStore(nil)
	consumer := &Consumer{store: store, index: NewIndex()}

	if block, receipts, statedb := store.Pending(); block != nil || receipts != nil || statedb != nil {
		t.Fatalf("store pending = %v, %v, %v", block, receipts, statedb)
	}
	if block, statedb, err := store.PendingState(t.Context()); block != nil || statedb != nil || err != nil {
		t.Fatalf("store pending state = %v, %v, %v", block, statedb, err)
	}
	if block := store.PendingBlock(); block != nil {
		t.Fatalf("store pending block = %v", block)
	}
	if block, receipts := store.PendingBlockAndReceipts(); block != nil || receipts != nil {
		t.Fatalf("store pending metadata = %v, %v", block, receipts)
	}
	if blocks, receipts := store.PendingLogRange(); blocks != nil || receipts != nil {
		t.Fatalf("store pending log range = %v, %v", blocks, receipts)
	}
	if nonce, found, err := store.PendingNonce(common.Address{}); nonce != 0 || err != nil || found {
		t.Fatalf("store nonce = %d, %v, %v", nonce, err, found)
	}
	if block, receipts, statedb := consumer.Pending(); block != nil || receipts != nil || statedb != nil {
		t.Fatalf("consumer pending = %v, %v, %v", block, receipts, statedb)
	}
	if block, statedb, err := consumer.PendingState(t.Context()); block != nil || statedb != nil || err != nil {
		t.Fatalf("consumer pending state = %v, %v, %v", block, statedb, err)
	}
	if block := consumer.PendingBlock(); block != nil {
		t.Fatalf("consumer pending block = %v", block)
	}
	if block, receipts := consumer.PendingBlockAndReceipts(); block != nil || receipts != nil {
		t.Fatalf("consumer pending metadata = %v, %v", block, receipts)
	}
	if nonce, found, err := consumer.PendingNonce(common.Address{}); nonce != 0 || err != nil || found {
		t.Fatalf("consumer nonce = %d, %v, %v", nonce, err, found)
	}
	if tx, receipt, found := consumer.LookupPreconf(common.Hash{}); tx != nil || receipt != nil || found {
		t.Fatalf("consumer lookup = %v, %v, %v", tx, receipt, found)
	}
}

func TestPendingRPCStateErrors(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
		t.Fatal("publish failed")
	}
	consumer := &Consumer{store: store}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if block, statedb, err := consumer.PendingState(cancelled); block != nil || statedb != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pending state = %v, %v, %v", block, statedb, err)
	}

	failure := errors.New("state read failed")
	broken, err := state.NewWithReader(types.EmptyRootHash, state.NewDatabaseForTesting(), failingStateReader{err: failure})
	if err != nil {
		t.Fatalf("errored state: %v", err)
	}
	broken.GetBalance(*fixture.address)
	key := pendingKey{number: fixture.block.NumberU64(), parent: fixture.block.ParentHash()}
	store.mu.Lock()
	store.entries[key].RPCView.State = &pendingStateReader{state: broken}
	store.mu.Unlock()

	if block, statedb, err := consumer.PendingState(t.Context()); block != nil || statedb != nil || !errors.Is(err, failure) {
		t.Fatalf("errored pending state = %v, %v, %v", block, statedb, err)
	}
	if nonce, found, err := consumer.PendingNonce(*fixture.address); nonce != 0 || !found || !errors.Is(err, failure) {
		t.Fatalf("errored pending nonce = %d, %v, %v", nonce, err, found)
	}
	child := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).Add(fixture.block.Number(), common.Big1),
		ParentHash: fixture.block.Hash(),
	})
	if statedb, err := consumer.PendingParentState(t.Context(), child); statedb != nil || !errors.Is(err, failure) {
		t.Fatalf("errored pending parent state = %v, %v", statedb, err)
	}
}

func TestPendingRPCSerializesStateCopies(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0) {
		t.Fatal("publish failed")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	// Release on cleanup as well as on the happy path: an early t.Fatal would
	// otherwise leave the blocked reader parked on release for the rest of the
	// test binary, still holding this store's state-copy slot.
	var releaseOnce sync.Once
	releaseCopy := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseCopy)

	key := pendingKey{number: fixture.block.NumberU64(), parent: fixture.block.ParentHash()}
	store.mu.Lock()
	entry := store.entries[key]
	originalReader := entry.RPCView.State
	entry.RPCView.State = &blockingPendingStateReader{
		PendingStateReader: originalReader,
		started:            started,
		release:            release,
	}
	store.mu.Unlock()

	type stateResult struct {
		block *types.Block
		state *state.StateDB
		err   error
	}
	stateDone := make(chan stateResult, 1)
	go func() {
		block, statedb, err := store.PendingState(context.Background())
		stateDone <- stateResult{block: block, state: statedb, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first state copy did not start")
	}

	type pendingResult struct {
		block    *types.Block
		receipts types.Receipts
		state    *state.StateDB
	}
	// The first copy holds the slot, so a non-blocking acquire must fail. This
	// is the deterministic half of the assertion; the timed wait below covers
	// the observable behaviour of Pending() itself.
	select {
	case store.stateCopy <- struct{}{}:
		<-store.stateCopy
		t.Fatal("state copy slot was free while a copy was in flight")
	default:
	}

	pendingDone := make(chan pendingResult, 1)
	go func() {
		block, receipts, statedb := store.Pending()
		pendingDone <- pendingResult{block: block, receipts: receipts, state: statedb}
	}()
	select {
	case result := <-pendingDone:
		t.Fatalf("second state copy returned before the first completed: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}

	store.mu.Lock()
	entry.RPCView.State = originalReader
	store.mu.Unlock()
	releaseCopy()
	var first stateResult
	select {
	case first = <-stateDone:
	case <-time.After(time.Second):
		t.Fatal("first state copy did not finish")
	}
	if first.err != nil || first.block != fixture.block || first.state == nil || first.state.GetNonce(*fixture.address) != 7 {
		t.Fatalf("first state copy = %v, %v, %v", first.block, first.state, first.err)
	}
	var second pendingResult
	select {
	case second = <-pendingDone:
	case <-time.After(time.Second):
		t.Fatal("second state copy did not finish")
	}
	if second.block != fixture.block || len(second.receipts) != 1 || second.state == nil || second.state.GetNonce(*fixture.address) != 7 {
		t.Fatalf("second state copy = %v, %v, %v", second.block, second.receipts, second.state)
	}
}

// mutatingPendingStateReader replaces the consumer's reconciliation marker the
// first time the pending state is touched, standing in for a head change that
// lands while a read is already in flight. This is the only way to reach the
// closing anchor check: setting the marker up front instead trips the opening
// check, and every accessor returns before the store is ever consulted.
type mutatingPendingStateReader struct {
	PendingStateReader
	once sync.Once
	on   func()
}

func (r *mutatingPendingStateReader) trip() { r.once.Do(r.on) }

func (r *mutatingPendingStateReader) NewStateDB() (*state.StateDB, error) {
	r.trip()
	return r.PendingStateReader.NewStateDB()
}

func (r *mutatingPendingStateReader) GetNonceWithError(address common.Address) (uint64, error) {
	r.trip()
	return r.PendingStateReader.GetNonceWithError(address)
}

// TestPendingAccessorsRecheckAnchorAfterRead covers the closing
// pendingReadAnchorValid call, which the opening guard alone cannot reach.
// Verified by mutation: deleting the trailing check from any accessor below
// makes its subtest fail.
//
// Only the four accessors that consult the pending state reader are covered,
// because that is the only injection point the store exposes. PendingBlock,
// PendingBlockAndReceipts and LookupPreconf read straight out of the view under
// a lock, so their closing check has no deterministic hook; they are covered
// for the opening guard by TestPendingAccessorsRefuseUnreconciledHead.
func TestPendingAccessorsRecheckAnchorAfterRead(t *testing.T) {
	for _, test := range []struct {
		name   string
		assert func(t *testing.T, c *Consumer)
	}{
		{"Pending", func(t *testing.T, c *Consumer) {
			if b, r, s := c.Pending(); b != nil || r != nil || s != nil {
				t.Fatalf("Pending = %v, %v, %v", b, r, s)
			}
		}},
		{"PendingState", func(t *testing.T, c *Consumer) {
			if b, s, err := c.PendingState(t.Context()); b != nil || s != nil || err != nil {
				t.Fatalf("PendingState = %v, %v, %v", b, s, err)
			}
		}},
		{"PendingParentState", func(t *testing.T, c *Consumer) {
			parent := c.pendingStore().PendingBlock()
			child := types.NewBlockWithHeader(&types.Header{
				Number:     new(big.Int).Add(parent.Number(), common.Big1),
				ParentHash: parent.Hash(),
			})
			if s, err := c.PendingParentState(t.Context(), child); s != nil || err != nil {
				t.Fatalf("PendingParentState = %v, %v", s, err)
			}
		}},
		{"PendingNonce", func(t *testing.T, c *Consumer) {
			if n, found, err := c.PendingNonce(common.Address{}); n != 0 || err != nil || found {
				t.Fatalf("PendingNonce = %d, %v, %v", n, err, found)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startExecHarness(t)
			consumer, err := NewConsumer("", h.chain)
			if err != nil {
				t.Fatalf("consumer: %v", err)
			}
			stateDB, err := h.chain.StateAt(h.chain.CurrentBlock().Root)
			if err != nil {
				t.Fatalf("state: %v", err)
			}
			head := h.chain.CurrentBlock()
			block := types.NewBlockWithHeader(&types.Header{
				Number:     new(big.Int).Add(head.Number, common.Big1),
				ParentHash: head.Hash(),
			})
			if !consumer.pendingStore().publish(block, nil, stateDB, nil, 0) {
				t.Fatal("publish failed")
			}

			store := consumer.pendingStore()
			key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
			store.mu.Lock()
			entry := store.entries[key]
			entry.RPCView.State = &mutatingPendingStateReader{
				PendingStateReader: entry.RPCView.State,
				// A fresh header with the same hash: the head is unchanged, but
				// the marker is a different pointer, which is exactly what a
				// reconciliation update looks like to the closing check.
				on: func() { consumer.reconciled.Store(types.CopyHeader(head)) },
			}
			store.mu.Unlock()

			test.assert(t, consumer)
		})
	}
}

// TestPendingAccessorsRefuseUnreconciledHead pins the invariant that every
// Consumer pending accessor refuses to serve while the reconciliation marker
// does not match the chain head. This covers the OPENING guard only; the
// closing re-check is covered by TestPendingAccessorsRecheckAnchorAfterRead.
func TestPendingAccessorsRefuseUnreconciledHead(t *testing.T) {
	h := startExecHarness(t)
	consumer, err := NewConsumer("", h.chain)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	stateDB, err := h.chain.StateAt(h.chain.CurrentBlock().Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	head := h.chain.CurrentBlock()
	block := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).Add(head.Number, common.Big1),
		ParentHash: head.Hash(),
	})
	if !consumer.pendingStore().publish(block, nil, stateDB, nil, 0) {
		t.Fatal("publish failed")
	}

	// Sanity check: while the marker matches the head, reads are served. Without
	// this the table below would pass even if the store were simply empty.
	if got := consumer.PendingBlock(); got == nil {
		t.Fatal("reconciled head served no pending block")
	}

	// Point the marker at a header that is not the chain head.
	consumer.reconciled.Store(&types.Header{Number: big.NewInt(9_999_999)})

	for _, test := range []struct {
		name   string
		assert func(t *testing.T)
	}{
		{"Pending", func(t *testing.T) {
			if b, r, s := consumer.Pending(); b != nil || r != nil || s != nil {
				t.Fatalf("Pending = %v, %v, %v", b, r, s)
			}
		}},
		{"PendingState", func(t *testing.T) {
			if b, s, err := consumer.PendingState(t.Context()); b != nil || s != nil || err != nil {
				t.Fatalf("PendingState = %v, %v, %v", b, s, err)
			}
		}},
		{"PendingBlock", func(t *testing.T) {
			if b := consumer.PendingBlock(); b != nil {
				t.Fatalf("PendingBlock = %v", b)
			}
		}},
		{"PendingBlockAndReceipts", func(t *testing.T) {
			if b, r := consumer.PendingBlockAndReceipts(); b != nil || r != nil {
				t.Fatalf("PendingBlockAndReceipts = %v, %v", b, r)
			}
		}},
		{"PendingParentState", func(t *testing.T) {
			if s, err := consumer.PendingParentState(t.Context(), block); s != nil || err != nil {
				t.Fatalf("PendingParentState = %v, %v", s, err)
			}
		}},
		{"PendingNonce", func(t *testing.T) {
			if n, found, err := consumer.PendingNonce(common.Address{}); n != 0 || err != nil || found {
				t.Fatalf("PendingNonce = %d, %v, %v", n, err, found)
			}
		}},
		{"LookupPreconf", func(t *testing.T) {
			if tx, receipt, found := consumer.LookupPreconf(common.Hash{}); tx != nil || receipt != nil || found {
				t.Fatalf("LookupPreconf = %v, %v, %v", tx, receipt, found)
			}
		}},
	} {
		t.Run(test.name, test.assert)
	}
}

func TestPendingStoreSupersessionLifecycle(t *testing.T) {
	for _, test := range []struct {
		name    string
		publish func(*PendingStore, *pendingRPCCoverageFixture) bool
	}{
		{
			name: "begin",
			publish: func(store *PendingStore, fixture *pendingRPCCoverageFixture) bool {
				generation := store.begin(fixture.block.NumberU64(), fixture.block.ParentHash(), true)
				return store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, generation)
			},
		},
		{
			name: "direct publish",
			publish: func(store *PendingStore, fixture *pendingRPCCoverageFixture) bool {
				return store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, nil, 0)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			store := NewPendingStore(db)
			first := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
			second := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x2"))
			if !store.publish(first.block, types.Receipts{first.receipt}, first.state, nil, 0) {
				t.Fatal("first publish failed")
			}
			if !test.publish(store, second) {
				t.Fatal("replacement publish failed")
			}
			if pending := store.PendingBlock(); pending != second.block {
				t.Fatalf("pending replacement = %v, want %v", pending, second.block)
			}
			store.mu.RLock()
			firstEntry := store.entries[pendingKey{number: first.block.NumberU64(), parent: first.block.ParentHash()}]
			secondEntry := store.entries[pendingKey{number: second.block.NumberU64(), parent: second.block.ParentHash()}]
			entryCount := len(store.entries)
			store.mu.RUnlock()
			if firstEntry != nil || secondEntry == nil || entryCount != 1 {
				t.Fatalf("replacement entries = first=%+v second=%+v count=%d", firstEntry, secondEntry, entryCount)
			}
			records := rawdb.ReadInvalidPreconfs(db, rawdb.InvalidPreconfQueryLimit)
			if len(records) != 1 || records[0].Number != first.block.NumberU64() || records[0].Reason != "superseded" {
				t.Fatalf("supersession records = %+v", records)
			}
		})
	}
}

func TestPendingStoreClaimRequiresCanonicalBase(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	reusable := &ReusableExecution{
		HeaderHash: fixture.block.Hash(),
		TxRoot:     fixture.block.TxHash(),
		StateDB:    fixture.state.Copy(),
		Result: &core.ProcessResult{
			Receipts: types.Receipts{fixture.receipt},
			GasUsed:  fixture.block.GasUsed(),
		},
	}
	generation := store.begin(fixture.block.NumberU64(), fixture.block.ParentHash(), false)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, reusable, generation) {
		t.Fatal("publish failed")
	}
	store.mu.RLock()
	sealed := store.entries[pendingKey{number: fixture.block.NumberU64(), parent: fixture.block.ParentHash()}].Sealed
	store.mu.RUnlock()
	if sealed != nil {
		t.Fatal("speculative-base entry retained reusable execution")
	}
	if execution, ok := store.ClaimPreconf(fixture.block); ok || execution != nil {
		t.Fatalf("speculative-base claim = %+v, %v", execution, ok)
	}
}

func TestPendingStoreRejectsDeferredPartialClaim(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	store := NewPendingStore(db)
	prefix, candidate := publishPrefixWithTail(t, store)
	if claimed, ok := store.claimPreconfPrefix(candidate); !ok || claimed == nil {
		t.Fatalf("prefix claim = %+v, %v", claimed, ok)
	}
	if logs, invalidations := store.invalidateFromMemory(candidate.NumberU64(), "session_lost"); len(logs) != 0 || len(invalidations) != 0 {
		t.Fatalf("deferred invalidation = %v, %+v", logs, invalidations)
	}
	logs := store.RejectClaimedPreconf(candidate)
	if len(logs) != 1 || !logs[0].Removed || prefix.receipt.Logs[0].Removed {
		t.Fatalf("removed logs = %+v; source removed = %v", logs, prefix.receipt.Logs[0].Removed)
	}
	if pending := store.PendingBlock(); pending != nil {
		t.Fatalf("rejected prefix remained pending: %v", pending)
	}
	records := rawdb.ReadInvalidPreconfs(db, rawdb.InvalidPreconfQueryLimit)
	if len(records) != 1 || records[0].Number != candidate.NumberU64() || records[0].Reason != "session_lost" {
		t.Fatalf("deferred invalidation records = %+v", records)
	}
}
