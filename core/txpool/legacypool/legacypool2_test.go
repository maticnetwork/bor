// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package legacypool

import (
	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
)

func pricedValuedTransaction(nonce uint64, value int64, gaslimit uint64, gasprice *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx, _ := types.SignTx(types.NewTransaction(nonce, common.Address{}, big.NewInt(value), gaslimit, gasprice, nil), types.HomesteadSigner{}, key)
	return tx
}

func count(t *testing.T, pool *LegacyPool) (pending int, queued int) {
	t.Helper()

	pending, queued = pool.stats()

	if err := validatePoolInternals(pool); err != nil {
		t.Fatalf("pool internal state corrupted: %v", err)
	}

	return pending, queued
}

func fillPool(t testing.TB, pool *LegacyPool) {
	t.Helper()
	// Create enough executable transactions to exactly fill the configured pool
	// capacity. Each account stays within AccountSlots so pending truncation
	// does not need to penalize oversized accounts during setup.
	executableTxs := types.Transactions{}
	target := int(pool.config.GlobalSlots + pool.config.GlobalQueue)
	for i := 0; len(executableTxs) < target; i++ {
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), uint256.NewInt(10000000000), tracing.BalanceChangeUnspecified)
		for j := 0; j < int(pool.config.AccountSlots) && len(executableTxs) < target; j++ {
			executableTxs = append(executableTxs, pricedTransaction(uint64(j), 100000, big.NewInt(300), key))
		}
	}
	// Import the batch and verify the pool is full and executable.
	pool.addRemotesSync(executableTxs)

	// Read pending, queued, and all.Slots() under pool.mu.RLock so the three
	// values are consistent: no concurrent mutation can interleave between the
	// two calls because all paths that modify pool.all require pool.mu.Lock.
	pool.mu.RLock()
	pending, queued := pool.stats()
	slots := pool.all.Slots()
	pool.mu.RUnlock()

	// sanity-check that the test prerequisites are ok (pending full)
	if have, want := slots, target; have != want {
		t.Fatalf("have %d, want %d", have, want)
	}
	if have, want := pending, slots; have != want {
		t.Fatalf("have %d, want %d", have, want)
	}
	if have, want := queued, 0; have != want {
		t.Fatalf("have %d, want %d", have, want)
	}

	t.Logf("pool.config: GlobalSlots=%d, GlobalQueue=%d\n", pool.config.GlobalSlots, pool.config.GlobalQueue)
	t.Logf("pending: %d queued: %d, all: %d\n", pending, queued, slots)
}

// Tests that if a batch high-priced of non-executables arrive, they do not kick out
// executable transactions
func TestTransactionFutureAttack(t *testing.T) {
	t.Parallel()

	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))

	config := testTxPoolConfig
	config.GlobalQueue = 100
	config.GlobalSlots = 100
	pool := New(config, blockchain)
	pool.Init(config.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()

	fillPool(t, pool)
	pending, _ := pool.Stats()
	// Now, future transaction attack starts, let's add a bunch of expensive non-executables, and see if the pending-count drops
	{
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), uint256.NewInt(100000000000), tracing.BalanceChangeUnspecified)
		futureTxs := types.Transactions{}
		for j := 0; j < int(pool.config.GlobalSlots+pool.config.GlobalQueue); j++ {
			futureTxs = append(futureTxs, pricedTransaction(1000+uint64(j), 100000, big.NewInt(500), key))
		}

		for i := 0; i < 5; i++ {
			pool.addRemotesSync(futureTxs)
			newPending, newQueued := count(t, pool)
			t.Logf("pending: %d queued: %d, all: %d\n", newPending, newQueued, pool.all.Slots())
		}
	}

	newPending, _ := pool.Stats()
	// Pending should not have been touched
	if have, want := newPending, pending; have < want {
		t.Errorf("wrong pending-count, have %d, want %d (GlobalSlots: %d)",
			have, want, pool.config.GlobalSlots)
	}
}

// Tests that if a batch high-priced of non-executables arrive, they do not kick out
// executable transactions
func TestTransactionFuture1559(t *testing.T) {
	t.Parallel()
	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))
	pool := New(testTxPoolConfig, blockchain)
	pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()

	// Create a number of test accounts, fund them and make transactions
	fillPool(t, pool)
	pending, _ := pool.Stats()

	// Now, future transaction attack starts, let's add a bunch of expensive non-executables, and see if the pending-count drops
	{
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), uint256.NewInt(100000000000), tracing.BalanceChangeUnspecified)
		futureTxs := types.Transactions{}
		for j := 0; j < int(pool.config.GlobalSlots+pool.config.GlobalQueue); j++ {
			futureTxs = append(futureTxs, dynamicFeeTx(1000+uint64(j), 100000, big.NewInt(200), big.NewInt(101), key))
		}
		pool.addRemotesSync(futureTxs)
	}

	newPending, _ := pool.Stats()
	// Pending should not have been touched
	if have, want := newPending, pending; have != want {
		t.Errorf("Wrong pending-count, have %d, want %d (GlobalSlots: %d)",
			have, want, pool.config.GlobalSlots)
	}
}

// Tests that if a batch of balance-overdraft txs arrive, they do not kick out
// executable transactions
func TestTransactionZAttack(t *testing.T) {
	t.Parallel()
	// Create the pool to test the pricing enforcement with
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))
	pool := New(testTxPoolConfig, blockchain)
	pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()
	// Create a number of test accounts, fund them and make transactions
	fillPool(t, pool)

	countInvalidPending := func() int {
		t.Helper()

		var ivpendingNum int

		pendingtxs, _ := pool.Content()
		for account, txs := range pendingtxs {
			curBalance := new(big.Int).Set(pool.currentState.GetBalance(account).ToBig())
			for _, tx := range txs {
				if curBalance.Cmp(tx.Value()) <= 0 {
					ivpendingNum++
				} else {
					curBalance.Sub(curBalance, tx.Value())
				}
			}
		}

		if err := validatePoolInternals(pool); err != nil {
			t.Fatalf("pool internal state corrupted: %v", err)
		}

		return ivpendingNum
	}
	ivPending := countInvalidPending()
	t.Logf("invalid pending: %d\n", ivPending)

	// Now, DETER-Z attack starts, let's add a bunch of expensive non-executables
	// (from N accounts) along with balance-overdraft txs (from one account), and
	// see if the pending-count drops
	for j := 0; j < int(pool.config.GlobalQueue); j++ {
		futureTxs := types.Transactions{}
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), uint256.NewInt(100000000000), tracing.BalanceChangeUnspecified)
		futureTxs = append(futureTxs, pricedTransaction(1000+uint64(j), 21000, big.NewInt(500), key))
		pool.addRemotesSync(futureTxs)
	}

	overDraftTxs := types.Transactions{}
	{
		key, _ := crypto.GenerateKey()
		pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), uint256.NewInt(100000000000), tracing.BalanceChangeUnspecified)
		for j := 0; j < int(pool.config.GlobalSlots); j++ {
			overDraftTxs = append(overDraftTxs, pricedValuedTransaction(uint64(j), 600000000000, 21000, big.NewInt(500), key))
		}
	}
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)
	pool.addRemotesSync(overDraftTxs)

	newPending, newQueued := count(t, pool)
	newIvPending := countInvalidPending()

	t.Logf("pool.all.Slots(): %d\n", pool.all.Slots())
	t.Logf("pending: %d queued: %d, all: %d\n", newPending, newQueued, pool.all.Slots())
	t.Logf("invalid pending: %d\n", newIvPending)

	// Pending should not have been touched
	if newIvPending != ivPending {
		t.Errorf("Wrong invalid pending-count, have %d, want %d (GlobalSlots: %d, queued: %d)",
			newIvPending, ivPending, pool.config.GlobalSlots, newQueued)
	}
}

// TestLockOrdering_PricedHeapNoDeadlock proves the lock hierarchy and helps
// in detecting deadlock if any.
//
//	pool.mu (W) → reheapMu → lookup.lock is the ideal ordering
//
// Runs priced.Reheap and PutMany (surrounded by pool.mu.Lock()) in parallel
// routines to check if reverse path of acquiring pool.mu.Lock() from reheapMu
// exists or not. If it does, it will lead to a deadlock.
func TestLockOrdering_PricedHeapNoDeadlock(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))
	config := testTxPoolConfig

	pool := New(config, blockchain)
	pool.Init(config.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()

	fillPool(t, pool)

	// Collect a small slice of txs from pool.all to feed into PutMany.
	var txs types.Transactions
	pool.mu.RLock()
	pool.all.Range(func(_ common.Hash, tx *types.Transaction) bool {
		txs = append(txs, tx)
		return len(txs) < 5
	})
	pool.mu.RUnlock()

	const iterations = 500
	done := make(chan struct{})

	// Thread B: Reheap acquires reheapMu then lookup.lock — never acquires pool.mu.
	// If it tried to acquire pool.mu it would deadlock with Thread A below.
	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			pool.priced.Reheap()
		}
	}()

	// Thread A: hold pool.mu (write lock) while calling PutMany (acquires reheapMu).
	// This is the exact pattern introduced by the replacesPending fix.
	for i := 0; i < iterations; i++ {
		pool.mu.Lock()
		pool.priced.PutMany(txs, pool.priced.reheaps.Load())
		pool.mu.Unlock()
	}

	select {
	case <-done:
		// success: no deadlock
	case <-time.After(60 * time.Second):
		// Timeout must tolerate -race overhead on CI — the work itself is
		// ~1000 heap operations on a 6k-entry pool, so 60s is safely past
		// legitimate completion but still catches a real hang.
		t.Fatal("deadlock detected: reheapMu→pool.mu ordering cycle exists")
	}
}

// TestLockOrderingReplacePendingNoDeadlock proves that the end-to-end
// replacesPending code path in add() — which calls pool.priced.PutMany while
// holding pool.mu — does not deadlock when Reheap runs concurrently.
func TestLockOrdering_ReplacePendingNoDeadlock(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))
	config := testTxPoolConfig
	config.GlobalSlots = 100
	config.GlobalQueue = 100
	pool := New(config, blockchain)
	pool.Init(config.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()
	fillPool(t, pool)

	// Future txs (gapped nonce, higher gas) that hit the replacesPending path:
	// the pool is full with pending txs at gasPrice=300, so each future tx at
	// gasPrice=500 triggers Discard + PutMany(drop) while holding pool.mu.
	key, _ := crypto.GenerateKey()
	pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey),
		uint256.NewInt(100000000000), tracing.BalanceChangeUnspecified)
	futureTxs := types.Transactions{}
	for j := 0; j < int(pool.config.GlobalSlots+pool.config.GlobalQueue); j++ {
		futureTxs = append(futureTxs, pricedTransaction(1000+uint64(j), 100000, big.NewInt(500), key))
	}

	reheapDone := make(chan struct{})

	// Concurrent Reheap — if it tried to acquire pool.mu it would deadlock
	// with the addRemotesSync goroutines holding pool.mu in the add() path.
	go func() {
		defer close(reheapDone)
		for i := 0; i < 500; i++ {
			pool.priced.Reheap()
		}
	}()

	for i := 0; i < 5; i++ {
		pool.addRemotesSync(futureTxs)
	}

	select {
	case <-reheapDone:
		// success: no deadlock
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock detected in replacesPending code path")
	}
}

// TestLockOrdering_RemovedNoDeadlock proves that routines using pool.mu and
// Removed (which acquires reheapMu) does not deadlock. Removed is called under
// pool.mu and should follow a one way order for acquiring lock. Removed is
// called under various path in internal tx arrangement stage.
func TestLockOrdering_RemovedNoDeadlock(t *testing.T) {
	t.Parallel()

	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))
	config := testTxPoolConfig
	config.GlobalSlots = 100
	config.GlobalQueue = 100
	pool := New(config, blockchain)
	pool.Init(config.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()
	fillPool(t, pool)

	const iterations = 2000 // Removed is a brief lock; use more iterations for coverage
	done := make(chan struct{})

	// Thread B: Reheap holds reheapMu and accesses lookup.lock — never pool.mu.
	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			pool.priced.Reheap()
		}
	}()

	// Thread A: hold pool.mu (write lock) while calling Removed (acquires reheapMu
	// briefly and may internally trigger Reheap if stales threshold is crossed).
	for i := 0; i < iterations; i++ {
		pool.mu.Lock()
		pool.priced.Removed(1)
		pool.mu.Unlock()
	}

	select {
	case <-done:
		// success: no deadlock
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock detected: reheapMu→pool.mu ordering cycle in Removed path")
	}
}

// TestPricedListDiscard_ProtectsReserved verifies that Discard never evicts a
// reserved-blockspace transaction, even though its zero fee puts it at the very
// bottom of the price heap where eviction would otherwise take it first.
func TestPricedListDiscard_ProtectsReserved(t *testing.T) {
	t.Parallel()

	makeList := func() (*pricedList, *types.Transaction) {
		all := newLookup()
		priced := newPricedList(all)
		key, _ := crypto.GenerateKey()
		reserved := pricedTransaction(0, 100000, big.NewInt(0), key) // zero tip → cheapest
		all.Add(reserved)
		for i := 1; i <= 5; i++ {
			all.Add(pricedTransaction(uint64(i), 100000, big.NewInt(int64(i)), key))
		}
		priced.Reheap()
		return priced, reserved
	}
	contains := func(txs types.Transactions, want *types.Transaction) bool {
		for _, tx := range txs {
			if tx.Hash() == want.Hash() {
				return true
			}
		}
		return false
	}

	// Baseline: without the predicate the zero-fee tx is the cheapest and evicted.
	priced, reserved := makeList()
	drop, ok, _ := priced.Discard(3)
	require.True(t, ok)
	require.True(t, contains(drop, reserved), "baseline: zero-fee tx should be discarded without protection")

	// With the reserved predicate wired, the same tx is protected.
	priced, reserved = makeList()
	priced.isReserved = func(tx *types.Transaction) bool { return tx.Hash() == reserved.Hash() }
	drop, ok, _ = priced.Discard(3)
	require.True(t, ok, "Discard should still free the requested slots from non-reserved txs")
	require.False(t, contains(drop, reserved), "reserved zero-fee tx must not be discarded")
}

// TestPricedListPut_ReheapAfterSnapshot verifies that Put is a no-op when a
// Reheap occurred after the snapshot was captured. This is the deduplication
// mechanism that prevents the same tx from appearing in the priced heap twice
// (once from Reheap reading pool.all, once from the explicit Put).
func TestPricedListPut_ReheapAfterSnapshot(t *testing.T) {
	t.Parallel()

	all := newLookup()
	priced := newPricedList(all)

	key, _ := crypto.GenerateKey()
	tx := pricedTransaction(0, 100000, big.NewInt(1), key)

	// Add tx to lookup and capture the reheap counter before any reheap runs.
	all.Add(tx)
	snapshot := priced.reheaps.Load()
	require.Equal(t, uint64(0), snapshot)

	// Reheap rebuilds the heap from pool.all (which now includes tx) and
	// bumps the reheap counter, invalidating the snapshot taken above.
	priced.Reheap()
	require.Equal(t, uint64(1), priced.reheaps.Load())

	priced.reheapMu.Lock()
	afterReheap := priced.urgent.Len() + priced.floating.Len()
	priced.reheapMu.Unlock()
	require.Equal(t, 1, afterReheap)

	// Put with the stale snapshot must be a no-op — the tx is already in the
	// heap from Reheap.
	priced.Put(tx, snapshot)

	priced.reheapMu.Lock()
	heapLen := priced.urgent.Len() + priced.floating.Len()
	priced.reheapMu.Unlock()

	require.Equal(t, 1, heapLen, "Put should not have inserted a duplicate")
	require.Equal(t, heapLen, all.Count(), "heap and lookup must stay in sync")
}

// TestPricedListPut_ConcurrentReheapAfterSnapshot is the concurrent variant
// of TestPricedListPut_ReheapAfterSnapshot. A goroutine runs Reheap after
// `pool.all` is updated and the test ensures that duplicate entries into
// the heap are prevented.
func TestPricedListPut_ConcurrentReheapAfterSnapshot(t *testing.T) {
	t.Parallel()

	all := newLookup()
	priced := newPricedList(all)

	key, _ := crypto.GenerateKey()
	tx := pricedTransaction(0, 100000, big.NewInt(1), key)

	addDone := make(chan struct{})
	reheapDone := make(chan struct{})

	// Goroutine starts first, waiting for the Add signal.
	go func() {
		<-addDone
		priced.Reheap()
		close(reheapDone)
	}()

	all.Add(tx)
	// Signal the goroutine to run Reheap now that Add is done and later take a snapshot.
	close(addDone)
	snapshot := priced.reheaps.Load()
	require.Equal(t, uint64(0), snapshot)

	<-reheapDone

	priced.reheapMu.Lock()
	afterReheap := priced.urgent.Len() + priced.floating.Len()
	priced.reheapMu.Unlock()
	require.Equal(t, 1, afterReheap)

	// Put with the stale snapshot must be a no-op.
	priced.Put(tx, snapshot)

	priced.reheapMu.Lock()
	heapLen := priced.urgent.Len() + priced.floating.Len()
	priced.reheapMu.Unlock()

	require.Equal(t, 1, heapLen, "Put should not have inserted a duplicate")
	require.Equal(t, heapLen, all.Count(), "heap and lookup must stay in sync")
}

// TestPricedListPut_ReheapBeforeAdd verifies the happy path: Reheap runs
// before the tx is added to the lookup, so Put with a matching snapshot
// correctly inserts the tx exactly once — no duplicates.
func TestPricedListPut_ReheapBeforeAdd(t *testing.T) {
	t.Parallel()

	all := newLookup()
	priced := newPricedList(all)

	key, _ := crypto.GenerateKey()
	tx := pricedTransaction(0, 100000, big.NewInt(1), key)

	// Reheap on an empty lookup — the tx doesn't exist yet, so it won't
	// appear in the heap. This bumps the reheap counter to 1.
	priced.Reheap()
	require.Equal(t, uint64(1), priced.reheaps.Load())

	priced.reheapMu.Lock()
	require.Equal(t, 0, priced.urgent.Len()+priced.floating.Len())
	priced.reheapMu.Unlock()

	// Add tx to lookup and capture snapshot — it matches the current reheap
	// counter since no new reheap has happened.
	all.Add(tx)
	snapshot := priced.reheaps.Load()
	require.Equal(t, uint64(1), snapshot)

	// Put with a valid (non-stale) snapshot must insert the tx.
	priced.Put(tx, snapshot)

	priced.reheapMu.Lock()
	heapLen := priced.urgent.Len() + priced.floating.Len()
	priced.reheapMu.Unlock()

	require.Equal(t, 1, heapLen, "Put should have inserted the tx")
	require.Equal(t, heapLen, all.Count(), "heap and lookup must stay in sync")
}

func BenchmarkFutureAttack(b *testing.B) {
	// Create the pool to test the limit enforcement with
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	blockchain := newTestBlockChain(eip1559Config, 1000000, statedb, new(event.Feed))
	config := testTxPoolConfig
	config.GlobalQueue = 100
	config.GlobalSlots = 100
	pool := New(config, blockchain)
	pool.Init(testTxPoolConfig.PriceLimit, blockchain.CurrentBlock(), newReserver())
	defer pool.Close()
	fillPool(b, pool)

	key, _ := crypto.GenerateKey()
	pool.currentState.AddBalance(crypto.PubkeyToAddress(key.PublicKey), uint256.NewInt(100000000000), tracing.BalanceChangeUnspecified)
	futureTxs := types.Transactions{}

	for n := 0; n < b.N; n++ {
		futureTxs = append(futureTxs, pricedTransaction(1000+uint64(n), 100000, big.NewInt(500), key))
	}
	b.ResetTimer()

	for i := 0; i < 5; i++ {
		pool.addRemotesSync(futureTxs)
	}
}
