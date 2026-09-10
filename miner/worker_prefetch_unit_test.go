// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// Unit tests for the streaming-prefetch primitives (scanOverflow, forwardTxs,
// collectPlanBatch). These are pure functions exercised without a full worker
// setup; they cover the invariants that prior review rounds kept surfacing
// (within-iter dedup, heap preservation across budget growth, prefetched vs.
// in-flight skip semantics, accounting correctness).

package miner

import (
	"crypto/ecdsa"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

// fakeLazyResolver returns pre-registered txs by hash so LazyTransaction.Resolve()
// works without a real pool.
type fakeLazyResolver struct {
	txs map[common.Hash]*types.Transaction
}

func (r *fakeLazyResolver) Get(h common.Hash) *types.Transaction {
	return r.txs[h]
}

// scanOverflowFixture signs len(gases) txs from distinct accounts (one each so
// every tx heads its own heap bucket) with the given gas limits and gasPrices.
// Returns the signer, a heap constructor, and the raw tx slice.
func scanOverflowFixture(t *testing.T, gases []uint64, gasPrices []int64) (*fakeLazyResolver, map[common.Address][]*txpool.LazyTransaction, types.Signer, []*types.Transaction) {
	t.Helper()
	require.Equal(t, len(gases), len(gasPrices), "gases and gasPrices must be same length")

	signer := types.LatestSigner(params.TestChainConfig)
	resolver := &fakeLazyResolver{txs: make(map[common.Hash]*types.Transaction)}
	txsByAcct := make(map[common.Address][]*txpool.LazyTransaction)
	var rawTxs []*types.Transaction

	for i := range gases {
		key, err := crypto.GenerateKey()
		require.NoError(t, err)
		to := common.BigToAddress(big.NewInt(int64(i + 1)))
		tx := types.MustSignNewTx(key, signer, &types.LegacyTx{
			Nonce:    0,
			To:       &to,
			Value:    big.NewInt(0),
			Gas:      gases[i],
			GasPrice: big.NewInt(gasPrices[i]),
			Data:     nil,
		})
		rawTxs = append(rawTxs, tx)
		resolver.txs[tx.Hash()] = tx

		from := crypto.PubkeyToAddress(*(key.Public().(*ecdsa.PublicKey)))
		txsByAcct[from] = append(txsByAcct[from], &txpool.LazyTransaction{
			Pool:      resolver,
			Hash:      tx.Hash(),
			Tx:        tx,
			Time:      time.Now(),
			GasFeeCap: uint256.MustFromBig(big.NewInt(gasPrices[i])),
			GasTipCap: uint256.MustFromBig(big.NewInt(gasPrices[i])),
			Gas:       gases[i],
		})
	}
	return resolver, txsByAcct, signer, rawTxs
}

// newScanHeap constructs a fresh heap from a (cloned) txsByAcct map so repeated
// scanOverflow calls can start from the same state when needed.
func newScanHeap(signer types.Signer, txsByAcct map[common.Address][]*txpool.LazyTransaction) *transactionsByPriceAndNonce {
	cloned := make(map[common.Address][]*txpool.LazyTransaction, len(txsByAcct))
	for k, v := range txsByAcct {
		cp := make([]*txpool.LazyTransaction, len(v))
		copy(cp, v)
		cloned[k] = cp
	}
	return newTransactionsByPriceAndNonce(signer, cloned, big.NewInt(0), new(atomic.Bool))
}

// TestScanOverflow_ZeroBudget: budget=0 leaves everything untouched. The first
// Peek's ltx.Gas is > 0, and our guard should break immediately.
func TestScanOverflow_ZeroBudget(t *testing.T) {
	t.Parallel()
	_, txsByAcct, signer, _ := scanOverflowFixture(t,
		[]uint64{21000, 42000},
		[]int64{10, 20},
	)
	heap := newScanHeap(signer, txsByAcct)

	bonus, remaining := scanOverflow(heap, 0, nil, nil)
	require.Empty(t, bonus, "zero budget must yield zero bonus txs")
	require.Equal(t, uint64(0), remaining)

	// Heap must still surface the same top tx — nothing was consumed.
	top, _ := heap.Peek()
	require.NotNil(t, top, "heap must still contain accounts after a zero-budget scan")
}

// TestScanOverflow_PreservesAccountsAcrossBudgetGrowth is the regression test for
// the h.Pop() bug: a high-gas top account must remain in the heap after a
// too-small-budget scan so a later larger-budget scan can include it.
func TestScanOverflow_PreservesAccountsAcrossBudgetGrowth(t *testing.T) {
	t.Parallel()
	// Top tx gas=500k at high price; second tx gas=21k at lower price so the
	// heap orders the 500k tx first.
	_, txsByAcct, signer, rawTxs := scanOverflowFixture(t,
		[]uint64{500_000, 21_000},
		[]int64{1_000, 10},
	)
	heap := newScanHeap(signer, txsByAcct)

	// First call: budget 100k. Top tx (500k) doesn't fit; must break (not pop).
	bonus, remaining := scanOverflow(heap, 100_000, nil, nil)
	require.Empty(t, bonus, "100k budget cannot accommodate 500k top tx")
	require.Equal(t, uint64(100_000), remaining, "budget must be untouched on break")

	// Second call on the same heap: budget 600k. Top account must still be
	// present and selectable now that the budget has grown.
	bonus, remaining = scanOverflow(heap, 600_000, nil, nil)
	require.Len(t, bonus, 2, "both accounts should now be drained (500k + 21k)")
	seen := map[common.Hash]struct{}{bonus[0].Hash(): {}, bonus[1].Hash(): {}}
	require.Contains(t, seen, rawTxs[0].Hash(),
		"the previously-skipped 500k tx must re-appear once budget grows")
	require.Equal(t, uint64(600_000-500_000-21_000), remaining)
}

// TestScanOverflow_SkipsInflight: a tx already in sentThisPhase must be skipped
// without consuming budget, so other accounts get a fair scan.
func TestScanOverflow_SkipsInflight(t *testing.T) {
	t.Parallel()
	// High-priced tx first; low-priced second. Mark the high-priced as
	// in-flight; expect the low-priced to be selected with budget intact.
	_, txsByAcct, signer, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000},
		[]int64{1_000, 10},
	)
	heap := newScanHeap(signer, txsByAcct)

	sentThisPhase := map[common.Hash]struct{}{rawTxs[0].Hash(): {}}

	bonus, remaining := scanOverflow(heap, 100_000, nil, sentThisPhase)
	require.Len(t, bonus, 1, "only the non-in-flight tx should be selected")
	require.Equal(t, rawTxs[1].Hash(), bonus[0].Hash())
	require.Equal(t, uint64(100_000-21_000), remaining,
		"in-flight skip must not consume budget")
}

// TestScanOverflow_SkipsPrefetched: a tx already in prefetchedHashes must be
// skipped without consuming budget.
func TestScanOverflow_SkipsPrefetched(t *testing.T) {
	t.Parallel()
	_, txsByAcct, signer, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000},
		[]int64{1_000, 10},
	)
	heap := newScanHeap(signer, txsByAcct)

	prefetched := &sync.Map{}
	prefetched.Store(rawTxs[0].Hash(), struct{}{})

	bonus, remaining := scanOverflow(heap, 100_000, prefetched, nil)
	require.Len(t, bonus, 1, "only the non-prefetched tx should be selected")
	require.Equal(t, rawTxs[1].Hash(), bonus[0].Hash())
	require.Equal(t, uint64(100_000-21_000), remaining,
		"prefetched skip must not consume budget")
}

// TestForwardTxs_RecordsSentHashes: every tx delivered to a roomy channel must
// end up in sentThisPhase.
func TestForwardTxs_RecordsSentHashes(t *testing.T) {
	t.Parallel()
	_, _, _, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000, 21_000},
		[]int64{1, 2, 3},
	)
	ch := make(chan *types.Transaction, len(rawTxs))
	sent := map[common.Hash]struct{}{}

	forwardTxs(ch, rawTxs, sent)

	require.Len(t, sent, len(rawTxs), "all forwarded txs must be recorded")
	for _, tx := range rawTxs {
		_, ok := sent[tx.Hash()]
		require.True(t, ok, "tx %s must be in sentThisPhase", tx.Hash())
	}
	require.Len(t, ch, len(rawTxs), "channel should have received every tx")
}

// TestForwardTxs_DropsOnFullChannelDoesNotRecord: if the channel is full and a
// send is dropped, sentThisPhase must NOT record that hash (otherwise the
// overflow scan would skip a tx that never made it to a worker).
func TestForwardTxs_DropsOnFullChannelDoesNotRecord(t *testing.T) {
	t.Parallel()
	_, _, _, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000, 21_000},
		[]int64{1, 2, 3},
	)
	// Channel capacity 1 → first tx lands, the rest are dropped by the
	// non-blocking select.
	ch := make(chan *types.Transaction, 1)
	sent := map[common.Hash]struct{}{}

	forwardTxs(ch, rawTxs, sent)

	require.Len(t, sent, 1, "only the single tx that actually landed should be recorded")
	require.Len(t, ch, 1)

	// The recorded hash must match the tx that's sitting in the channel.
	delivered := <-ch
	_, recorded := sent[delivered.Hash()]
	require.True(t, recorded, "the delivered tx's hash must be the one recorded")
}

// TestForwardTxs_NilSentMapIsSafe: backward-compat path (block-equivalence
// wrappers pass nil) must not panic.
func TestForwardTxs_NilSentMapIsSafe(t *testing.T) {
	t.Parallel()
	_, _, _, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000},
		[]int64{1},
	)
	ch := make(chan *types.Transaction, 1)
	require.NotPanics(t, func() {
		forwardTxs(ch, rawTxs, nil)
	})
	require.Len(t, ch, 1)
}

// TestCollectPlanBatch_ClosedPlanCh: closing planCh returns builderDone=true
// with any already-buffered txs.
func TestCollectPlanBatch_ClosedPlanCh(t *testing.T) {
	t.Parallel()
	_, _, _, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000},
		[]int64{1, 2},
	)
	planCh := make(chan *types.Transaction, len(rawTxs))
	for _, tx := range rawTxs {
		planCh <- tx
	}
	close(planCh)

	batch, newGasCh, delta, done := collectPlanBatch(planCh, nil, 50*time.Millisecond, nil, nil)
	require.True(t, done, "closed planCh must surface builderDone=true")
	require.Len(t, batch, len(rawTxs), "buffered txs must be drained into batch")
	require.Equal(t, uint64(0), delta)
	require.Nil(t, newGasCh)
}

// TestCollectPlanBatch_TimerFiresOnEmptyInput: if no signal arrives within the
// window, the batch must return cleanly with empty state and builderDone=false.
func TestCollectPlanBatch_TimerFiresOnEmptyInput(t *testing.T) {
	t.Parallel()
	planCh := make(chan *types.Transaction)
	gasCh := make(chan uint64)

	start := time.Now()
	batch, newGasCh, delta, done := collectPlanBatch(planCh, gasCh, 25*time.Millisecond, nil, nil)
	elapsed := time.Since(start)

	require.False(t, done, "timer expiry must not mark the builder done")
	require.Empty(t, batch)
	require.Equal(t, uint64(0), delta)
	require.Equal(t, (<-chan uint64)(gasCh), newGasCh, "idle gasCh must pass through unchanged")
	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond,
		"timer must block for ~window before returning")
	require.Less(t, elapsed, 200*time.Millisecond,
		"timer must not drag far past the window")
}

// TestCollectPlanBatch_FreedGasAccumulates: multiple freed-gas values must sum
// into budgetDelta within a single window.
func TestCollectPlanBatch_FreedGasAccumulates(t *testing.T) {
	t.Parallel()
	planCh := make(chan *types.Transaction)
	gasCh := make(chan uint64, 3)
	gasCh <- 1_000
	gasCh <- 2_500
	gasCh <- 500

	_, _, delta, done := collectPlanBatch(planCh, gasCh, 25*time.Millisecond, nil, nil)
	require.False(t, done)
	require.Equal(t, uint64(4_000), delta,
		"budgetDelta must sum all freed-gas values received within the window")
}

// TestCollectPlanBatch_SkipsPrefetched: a tx whose hash is in prefetchedHashes
// must be dropped on the way in (not forwarded in batch).
func TestCollectPlanBatch_SkipsPrefetched(t *testing.T) {
	t.Parallel()
	_, _, _, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000},
		[]int64{1, 2},
	)
	planCh := make(chan *types.Transaction, len(rawTxs))
	for _, tx := range rawTxs {
		planCh <- tx
	}
	close(planCh)

	prefetched := &sync.Map{}
	prefetched.Store(rawTxs[0].Hash(), struct{}{})

	batch, _, _, done := collectPlanBatch(planCh, nil, 25*time.Millisecond, prefetched, nil)
	require.True(t, done)
	require.Len(t, batch, 1, "prefetched tx must be filtered out")
	require.Equal(t, rawTxs[1].Hash(), batch[0].Hash())
}

// TestBuilderTxProvider_NoDuplicateForwards is the regression test for the
// within-iteration dedup bug (PR #2192 review): a tx that arrives via planCh
// must not also be forwarded via scanOverflow within the same 2ms batch window
// when freed-gas signals arrive in that same window. Covers both the within-
// iteration and cross-iteration dedup invariants.
//
// Setup: a worker with a real txpool (so buildOverflowHeap can draw a
// price-ordered snapshot), no pre-prefetched hashes (so every pool tx is a
// valid overflow candidate), and a planCh that the test drives with each pool
// tx individually, interleaved with freed-gas signals. The fix pre-populates
// sentThisPhase with the plan batch before scanOverflow; without it, the top
// plan tx would re-surface via the overflow heap and get forwarded twice.
func TestBuilderTxProvider_NoDuplicateForwards(t *testing.T) {
	t.Parallel()

	w, b, engine, ctrl := setupBorWorkerWithPrefetch(t, 100, 2*time.Second)
	defer engine.Close()
	defer ctrl.Finish()
	defer w.close()

	const totalTxs = 40
	addTransactionBatch(b, totalTxs, false)
	time.Sleep(200 * time.Millisecond)

	pending := b.txPool.Pending(txpool.PendingFilter{}, nil)
	require.NotEmpty(t, pending)

	var poolTxs []*types.Transaction
	for _, lazyTxs := range pending {
		for _, ltx := range lazyTxs {
			if tx := ltx.Resolve(); tx != nil {
				poolTxs = append(poolTxs, tx)
			}
		}
	}
	require.GreaterOrEqual(t, len(poolTxs), 10)

	parent := w.chain.CurrentBlock()
	_, stateDB, prefetchReader, processReader, err := w.chain.StateAtWithReaders(parent.Root)
	require.NoError(t, err)

	w.mu.RLock()
	header, _, err := w.makeHeader(&generateParams{
		timestamp:      uint64(time.Now().Unix()),
		coinbase:       testBankAddress,
		parentHash:     parent.Hash(),
		statedb:        stateDB,
		prefetchReader: prefetchReader,
		processReader:  processReader,
	}, false)
	w.mu.RUnlock()
	require.NoError(t, err)

	planCh := make(chan *types.Transaction, len(poolTxs))
	gasFreedCh := make(chan uint64, len(poolTxs))
	streamCh := make(chan *types.Transaction, len(poolTxs)*4)

	genParams := &generateParams{
		prefetchedTxHashes: &sync.Map{}, // empty: every pool tx is an overflow candidate
		builderStarted:     new(atomic.Bool),
		builderPlanCh:      planCh,
		builderGasFreedCh:  gasFreedCh,
	}
	genParams.builderStarted.Store(true)

	var interrupt atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.runBuilderTxProvider(streamCh, header, genParams, &interrupt)
	}()

	// Drive the provider: for each pool tx, push it onto planCh and simultaneously
	// deliver a generous freed-gas signal. Both arrive within the same 2ms window,
	// so collectPlanBatch returns with the tx in `batch` AND a non-zero budgetDelta.
	// Without the within-iteration dedup guard, scanOverflow would re-emit the same
	// tx — it still heads the overflow heap because it's not yet in sentThisPhase
	// (forwardTxs hasn't run) and not yet in prefetchedHashes (onSuccess hasn't
	// fired).
	for _, tx := range poolTxs {
		planCh <- tx
		gasFreedCh <- 500_000 // plenty of budget for any bonus candidate
	}
	close(gasFreedCh)
	time.Sleep(50 * time.Millisecond) // let the provider drain both channels
	close(planCh)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runBuilderTxProvider did not exit")
	}
	close(streamCh)

	counts := make(map[common.Hash]int)
	for tx := range streamCh {
		counts[tx.Hash()]++
	}

	for h, n := range counts {
		require.LessOrEqual(t, n, 1, "tx %s forwarded %d times — dedup failed", h, n)
	}
	t.Logf("runBuilderTxProvider forwarded %d unique hashes across %d pool txs with interleaved freed-gas; no duplicates.",
		len(counts), len(poolTxs))
}

// TestCollectPlanBatch_SkipsInflight is the regression test for the
// scanOverflow→plan cross-iteration dedup edge: a tx already in sentThisPhase
// (from a prior scanOverflow emission whose worker is still mid-EVM and hasn't
// populated prefetchedHashes yet) arriving via planCh must be dropped, not
// forwarded a second time.
func TestCollectPlanBatch_SkipsInflight(t *testing.T) {
	t.Parallel()
	_, _, _, rawTxs := scanOverflowFixture(t,
		[]uint64{21_000, 21_000, 21_000},
		[]int64{1, 2, 3},
	)
	planCh := make(chan *types.Transaction, len(rawTxs))
	for _, tx := range rawTxs {
		planCh <- tx
	}
	close(planCh)

	// Simulate: rawTxs[0] was already forwarded by a prior scanOverflow
	// iteration; its worker is still executing so prefetchedHashes is empty
	// but sentThisPhase has the hash.
	sentThisPhase := map[common.Hash]struct{}{rawTxs[0].Hash(): {}}

	batch, _, _, done := collectPlanBatch(planCh, nil, 25*time.Millisecond, nil, sentThisPhase)
	require.True(t, done)
	require.Len(t, batch, 2, "in-flight tx must be filtered out")
	for _, tx := range batch {
		require.NotEqual(t, rawTxs[0].Hash(), tx.Hash(),
			"tx already in sentThisPhase must not appear in batch")
	}
}

// TestCollectPlanBatch_ClosedGasChPassesThrough: once gasCh is closed, the
// returned channel must be nil so subsequent iterations stop selecting on it.
func TestCollectPlanBatch_ClosedGasChPassesThrough(t *testing.T) {
	t.Parallel()
	planCh := make(chan *types.Transaction)
	gasCh := make(chan uint64)
	close(gasCh)

	_, newGasCh, _, done := collectPlanBatch(planCh, gasCh, 25*time.Millisecond, nil, nil)
	require.False(t, done)
	require.Nil(t, newGasCh, "closed gasCh must be nilled out so the next iteration ignores it")
}

// TestStreamIdleBatch_LocalBudgetEnforced verifies that streamIdleBatch's
// per-loop gas accounting (gaspool.SubGas) actually caps forwarded txs at the
// per-loop budget, independent of the global totalGasPool. Each tx individually
// fits in the budget; only the cumulative subtraction stops the loop. Without
// the SubGas call the heap drains entirely, so this test distinguishes the
// correctly-accounted path from a mutation that drops the subtraction.
func TestStreamIdleBatch_LocalBudgetEnforced(t *testing.T) {
	t.Parallel()

	// 3 txs, each 60k gas; per-loop budget will be 100k. Only the first
	// should fit once the local gaspool tracks subtractions.
	const perTxGas uint64 = 60_000
	_, txsByAcct, signer, rawTxs := scanOverflowFixture(t,
		[]uint64{perTxGas, perTxGas, perTxGas},
		[]int64{30, 20, 10},
	)
	heap := newScanHeap(signer, txsByAcct)

	w := &worker{}
	_ = w // streamIdleBatch is a method but only uses parameters; cast through receiver.

	// Cap headerGasLimit at 100k so loopGasLimit = min(totalGasPool, header) = 100k.
	const headerGasLimit uint64 = 100_000
	totalGasPool := core.NewGasPool(10_000_000)
	localPrefetched := map[common.Hash]struct{}{}

	// Buffer wide enough that the "channel full" early-return never triggers.
	txsCh := make(chan *types.Transaction, 16)

	w.streamIdleBatch(txsCh, heap, totalGasPool, localPrefetched, headerGasLimit)
	close(txsCh)

	var forwarded []*types.Transaction
	for tx := range txsCh {
		forwarded = append(forwarded, tx)
	}

	require.Len(t, forwarded, 1,
		"local gas budget (100k) must cap forwards at 1 tx of 60k — "+
			"forwarding more proves gaspool.SubGas was skipped")
	require.Equal(t, rawTxs[0].Hash(), forwarded[0].Hash(),
		"highest-gas-price tx must be forwarded first")
	require.Contains(t, localPrefetched, rawTxs[0].Hash(),
		"forwarded tx must be recorded in localPrefetched")
	require.Equal(t, 10_000_000-perTxGas, totalGasPool.Gas(),
		"totalGasPool must decrement by exactly the forwarded tx's gas")
}
