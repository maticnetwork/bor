// Copyright 2016 The go-ethereum Authors
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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/triedb"
)

// costValueTx builds a signed legacy transaction with an independently
// controllable value and gas price, so Cost() (value + gas*gasPrice) and
// Value() diverge by a known amount. The fixed-value transaction() helper
// can't exercise that divergence, which every test below depends on.
func costValueTx(nonce, gaslimit uint64, gasPrice, value *big.Int, key *ecdsa.PrivateKey) *types.Transaction {
	tx, _ := types.SignTx(types.NewTransaction(nonce, common.Address{0x01}, value, gaslimit, gasPrice, nil), types.HomesteadSigner{}, key)
	return tx
}

// Tests that transactions can be added to strict lists and list contents and
// nonce boundaries are correctly maintained.
func TestStrictListAdd(t *testing.T) {
	t.Parallel()

	// Generate a list of transactions to insert
	key, _ := crypto.GenerateKey()

	txs := make(types.Transactions, 1024)
	for i := 0; i < len(txs); i++ {
		txs[i] = transaction(uint64(i), 0, key)
	}
	// Insert the transactions in a random order
	list := newList(true)
	for _, v := range rand.Perm(len(txs)) {
		list.Add(txs[v], DefaultConfig.PriceBump)
	}
	// Verify internal state
	if len(list.txs.items) != len(txs) {
		t.Errorf("transaction count mismatch: have %d, want %d", len(list.txs.items), len(txs))
	}

	for i, tx := range txs {
		if list.txs.items[tx.Nonce()] != tx {
			t.Errorf("item %d: transaction mismatch: have %v, want %v", i, list.txs.items[tx.Nonce()], tx)
		}
	}
}

// TestListAddVeryExpensive tests adding txs which exceed 256 bits in cost. It is
// expected that the list does not panic.
func TestListAddVeryExpensive(t *testing.T) {
	key, _ := crypto.GenerateKey()
	list := newList(true)
	for i := 0; i < 3; i++ {
		value := big.NewInt(100)
		gasprice, _ := new(big.Int).SetString("0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", 0)
		gaslimit := uint64(i)
		tx, _ := types.SignTx(types.NewTransaction(uint64(i), common.Address{}, value, gaslimit, gasprice, nil), types.HomesteadSigner{}, key)
		t.Logf("cost: %x bitlen: %d\n", tx.Cost(), tx.Cost().BitLen())
		list.Add(tx, DefaultConfig.PriceBump)
	}
}

func BenchmarkListAdd(b *testing.B) {
	// Generate a list of transactions to insert
	key, _ := crypto.GenerateKey()

	txs := make(types.Transactions, 100000)
	for i := 0; i < len(txs); i++ {
		txs[i] = transaction(uint64(i), 0, key)
	}
	// Insert the transactions in a random order
	priceLimit := uint256.NewInt(DefaultConfig.PriceLimit)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list := newList(true)
		for _, v := range rand.Perm(len(txs)) {
			list.Add(txs[v], DefaultConfig.PriceBump)
			list.Filter(priceLimit, DefaultConfig.PriceBump, false)
		}
	}
}

func TestFilterTxConditionalKnownAccounts(t *testing.T) {
	t.Parallel()

	// Create an in memory state db to test against.
	memDb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memDb, &triedb.Config{Preimages: true})
	db := state.NewDatabase(tdb, nil)
	state, _ := state.New(common.Hash{}, db)

	header := &types.Header{
		Number: big.NewInt(0),
	}

	// Create a private key to sign transactions.
	key, _ := crypto.GenerateKey()

	// Create a list.
	list := newList(true)

	// Create a transaction with no defined tx options
	// and add to the list.
	tx := transaction(0, 1000, key)
	list.Add(tx, DefaultConfig.PriceBump)

	// There should be no drops at this point.
	// No state has been modified.
	drops := list.FilterTxConditional(state, header)

	count := len(drops)
	require.Equal(t, 0, count, "got %d filtered by TxOptions when there should not be any", count)

	// Create another transaction with a known account storage root tx option
	// and add to the list.
	tx2 := transaction(1, 1000, key)

	var options types.OptionsPIP15

	options.KnownAccounts = types.KnownAccounts{
		common.Address{19: 1}: &types.Value{
			Single: common.HexToRefHash("0xe734938daf39aae1fa4ee64dc3155d7c049f28b57a8ada8ad9e86832e0253bef"),
		},
	}

	state.AddBalance(common.Address{19: 1}, uint256.NewInt(1000), tracing.BalanceChangeTransfer)

	trie, _ := state.StorageTrie(common.Address{19: 1})
	_ = trie

	state.SetState(common.Address{19: 1}, common.Hash{}, common.Hash{30: 1})

	state.Finalise(true)

	trie, _ = state.StorageTrie(common.Address{19: 1})
	_ = trie

	tx2.PutOptions(&options)
	list.Add(tx2, DefaultConfig.PriceBump)

	// There should still be no drops as no state has been modified.
	drops = list.FilterTxConditional(state, header)

	count = len(drops)
	require.Equal(t, 0, count, "got %d filtered by TxOptions when there should not be any", count)

	// Set state that conflicts with tx2's policy
	state.SetState(common.Address{19: 1}, common.Hash{}, common.Hash{31: 1})

	state.Finalise(true)

	trie, _ = state.StorageTrie(common.Address{19: 1})
	_ = trie

	// tx2 should be the single transaction filtered out
	drops = list.FilterTxConditional(state, header)

	count = len(drops)
	require.Equal(t, 1, count, "got %d filtered by TxOptions when there should be a single one", count)

	require.Equal(t, tx2, drops[0], "Got %x, expected %x", drops[0].Hash(), tx2.Hash())
}

func TestFilterTxConditionalBlockNumber(t *testing.T) {
	t.Parallel()

	// Create an in memory state db to test against.
	memDb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memDb, &triedb.Config{Preimages: true})
	db := state.NewDatabase(tdb, nil)
	state, _ := state.New(common.Hash{}, db)

	header := &types.Header{
		Number: big.NewInt(100),
	}

	// Create a private key to sign transactions.
	key, _ := crypto.GenerateKey()

	// Create a list.
	list := newList(true)

	// Create a transaction with no defined tx options
	// and add to the list.
	tx := transaction(0, 1000, key)
	list.Add(tx, DefaultConfig.PriceBump)

	// There should be no drops at this point.
	// No state has been modified.
	drops := list.FilterTxConditional(state, header)

	count := len(drops)
	require.Equal(t, 0, count, "got %d filtered by TxOptions when there should not be any", count)

	// Create another transaction with a block number option and add to the list.
	tx2 := transaction(1, 1000, key)

	var options types.OptionsPIP15

	options.BlockNumberMin = big.NewInt(90)
	options.BlockNumberMax = big.NewInt(110)

	tx2.PutOptions(&options)
	list.Add(tx2, DefaultConfig.PriceBump)

	// There should still be no drops as no state has been modified.
	drops = list.FilterTxConditional(state, header)

	count = len(drops)
	require.Equal(t, 0, count, "got %d filtered by TxOptions when there should not be any", count)

	// Set block number that conflicts with tx2's policy
	header.Number = big.NewInt(120)

	// tx2 should be the single transaction filtered out
	drops = list.FilterTxConditional(state, header)

	count = len(drops)
	require.Equal(t, 1, count, "got %d filtered by TxOptions when there should be a single one", count)

	require.Equal(t, tx2, drops[0], "Got %x, expected %x", drops[0].Hash(), tx2.Hash())
}

func TestFilterTxConditionalTimestamp(t *testing.T) {
	t.Parallel()

	// Create an in memory state db to test against.
	memDb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memDb, &triedb.Config{Preimages: true})
	db := state.NewDatabase(tdb, nil)
	state, _ := state.New(common.Hash{}, db)

	header := &types.Header{
		Number: big.NewInt(0),
		Time:   100,
	}

	// Create a private key to sign transactions.
	key, _ := crypto.GenerateKey()

	// Create a list.
	list := newList(true)

	// Create a transaction with no defined tx options
	// and add to the list.
	tx := transaction(0, 1000, key)
	list.Add(tx, DefaultConfig.PriceBump)

	// There should be no drops at this point.
	// No state has been modified.
	drops := list.FilterTxConditional(state, header)

	count := len(drops)
	require.Equal(t, 0, count, "got %d filtered by TxOptions when there should not be any", count)

	// Create another transaction with a timestamp option and add to the list.
	tx2 := transaction(1, 1000, key)

	var options types.OptionsPIP15

	minTimestamp := uint64(90)
	maxTimestamp := uint64(110)

	options.TimestampMin = &minTimestamp
	options.TimestampMax = &maxTimestamp

	tx2.PutOptions(&options)
	list.Add(tx2, DefaultConfig.PriceBump)

	// There should still be no drops as no state has been modified.
	drops = list.FilterTxConditional(state, header)

	count = len(drops)
	require.Equal(t, 0, count, "got %d filtered by TxOptions when there should not be any", count)

	// Set timestamp that conflicts with tx2's policy
	header.Time = 120

	// tx2 should be the single transaction filtered out
	drops = list.FilterTxConditional(state, header)

	count = len(drops)
	require.Equal(t, 1, count, "got %d filtered by TxOptions when there should be a single one", count)

	require.Equal(t, tx2, drops[0], "Got %x, expected %x", drops[0].Hash(), tx2.Hash())
}

// TestPricedListReheapSnapshot validates the reheap snapshot mechanism:
//   - Reheap increments the reheaps counter
//   - Put/PutMany insert when the snapshot matches the current counter
//   - Put/PutMany skip when the snapshot is stale
func TestPricedListReheapSnapshot(t *testing.T) {
	t.Parallel()

	all := newLookup()
	priced := newPricedList(all)

	key, _ := crypto.GenerateKey()
	tx1 := pricedTransaction(0, 100000, big.NewInt(1), key)
	tx2 := pricedTransaction(1, 100000, big.NewInt(2), key)
	tx3 := pricedTransaction(2, 100000, big.NewInt(3), key)

	heapLen := func() int {
		priced.reheapMu.Lock()
		defer priced.reheapMu.Unlock()
		return priced.urgent.Len() + priced.floating.Len()
	}

	// Initial counter is 0.
	require.Equal(t, uint64(0), priced.reheaps.Load())

	// Put with matching snapshot inserts.
	priced.Put(tx1, 0)
	require.Equal(t, 1, heapLen())

	// PutMany with matching snapshot inserts.
	priced.PutMany(types.Transactions{tx2, tx3}, 0)
	require.Equal(t, 3, heapLen())

	// Reheap increments the counter.
	all.Add(tx1)
	priced.Reheap()
	require.Equal(t, uint64(1), priced.reheaps.Load())

	// After Reheap, only tx1 is in lookup so heap has 1 entry.
	require.Equal(t, 1, heapLen())

	// Put with stale snapshot (0) is a no-op.
	priced.Put(tx1, 0)
	require.Equal(t, 1, heapLen())

	// PutMany with stale snapshot (0) is a no-op.
	priced.PutMany(types.Transactions{tx2, tx3}, 0)
	require.Equal(t, 1, heapLen())

	// Put with current snapshot (1) inserts.
	priced.Put(tx2, 1)
	require.Equal(t, 2, heapLen())

	// PutMany with current snapshot (1) inserts.
	priced.PutMany(types.Transactions{tx3}, 1)
	require.Equal(t, 3, heapLen())
}

func BenchmarkListCapOneTx(b *testing.B) {
	// Generate a list of transactions to insert
	key, _ := crypto.GenerateKey()

	txs := make(types.Transactions, 32)
	for i := 0; i < len(txs); i++ {
		txs[i] = transaction(uint64(i), 0, key)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list := newList(true)
		// Insert the transactions in a random order
		for _, v := range rand.Perm(len(txs)) {
			list.Add(txs[v], DefaultConfig.PriceBump)
		}
		b.StartTimer()
		list.Cap(list.Len() - 1)
		b.StopTimer()
	}
}

// TestListAggregatesConsistency drives Add (including a fee-bumped
// replacement), Forward, Cap, and Remove in sequence on a single list and
// checks totalcost/totalvalue against the sum of the surviving transactions'
// Cost()/Value() after every step. The two aggregates are meant to be
// maintained in lockstep regardless of which operation touched the list.
func TestListAggregatesConsistency(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	l := newList(true)

	sum := func(txs ...*types.Transaction) (cost, value *big.Int) {
		cost, value = new(big.Int), new(big.Int)
		for _, tx := range txs {
			cost.Add(cost, tx.Cost())
			value.Add(value, tx.Value())
		}
		return cost, value
	}
	assertTotals := func(t *testing.T, txs ...*types.Transaction) {
		t.Helper()
		wantCost, wantValue := sum(txs...)
		// big.Int's zero value distinguishes a nil internal slice from an
		// empty one under reflect.DeepEqual, so compare via Cmp rather than
		// require.Equal on the *big.Int values themselves.
		require.Zero(t, wantCost.Cmp(l.totalcost.ToBig()), "totalcost: want %s, have %s", wantCost, l.totalcost)
		require.Zero(t, wantValue.Cmp(l.totalvalue.ToBig()), "totalvalue: want %s, have %s", wantValue, l.totalvalue)
	}

	tx0 := costValueTx(0, 100, big.NewInt(1000), big.NewInt(500), key) // cost 100500
	tx1 := costValueTx(1, 200, big.NewInt(1000), big.NewInt(700), key) // cost 200700
	tx2 := costValueTx(2, 50, big.NewInt(1000), big.NewInt(300), key)  // cost 50300
	inserted, _ := l.Add(tx0, DefaultConfig.PriceBump)
	require.True(t, inserted)
	inserted, _ = l.Add(tx1, DefaultConfig.PriceBump)
	require.True(t, inserted)
	inserted, _ = l.Add(tx2, DefaultConfig.PriceBump)
	require.True(t, inserted)
	assertTotals(t, tx0, tx1, tx2)
	require.Equal(t, tx1.Cost(), l.costcap.ToBig(), "costcap after Add")
	require.Equal(t, tx1.Value(), l.valuecap.ToBig(), "valuecap after Add")

	// Replace tx1 with a strictly higher fee (required for Add to accept the
	// replacement) and a different value/gas, so both aggregates must reflect
	// the swap, not an addition.
	tx1b := costValueTx(1, 200, big.NewInt(1200), big.NewInt(900), key) // cost 240900
	inserted, old := l.Add(tx1b, DefaultConfig.PriceBump)
	require.True(t, inserted)
	require.Equal(t, tx1.Hash(), old.Hash())
	assertTotals(t, tx0, tx1b, tx2)
	require.Equal(t, tx1b.Cost(), l.costcap.ToBig(), "costcap grows to the replacement's higher cost")
	require.Equal(t, tx1b.Value(), l.valuecap.ToBig(), "valuecap grows to the replacement's higher value")

	// Forward past tx0's nonce.
	forwarded := l.Forward(1)
	require.Len(t, forwarded, 1)
	assertTotals(t, tx1b, tx2)

	// Cap down to the single lowest remaining nonce (tx1b), dropping tx2.
	capped := l.Cap(1)
	require.Len(t, capped, 1)
	require.Equal(t, tx2.Hash(), capped[0].Hash())
	assertTotals(t, tx1b)

	// Remove the last transaction; both aggregates return to zero.
	removed, _ := l.Remove(tx1b)
	require.True(t, removed)
	assertTotals(t)
}

// TestListFilterBothBases pins Filter's basis independence: a full-cost pass
// (valueBasis=false) only ever mutates costcap and drops on tx.Cost(); a
// value pass (valueBasis=true) only ever mutates valuecap and drops on
// tx.Value(). Neither pass touches the other basis's cap.
func TestListFilterBothBases(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	const bigGasLimit = 1_000_000_000

	tests := []struct {
		name       string
		valueBasis bool
		survivor   *types.Transaction // stays: below the limit on the basis under test
		dropped    *types.Transaction // removed: above the limit on the basis under test
	}{
		{
			name:       "full-cost basis mutates only costcap",
			valueBasis: false,
			survivor:   costValueTx(0, 100, big.NewInt(1), big.NewInt(50), key),  // cost 150
			dropped:    costValueTx(1, 100, big.NewInt(20), big.NewInt(50), key), // cost 2050
		},
		{
			name:       "value basis mutates only valuecap",
			valueBasis: true,
			survivor:   costValueTx(0, 100, big.NewInt(1000), big.NewInt(100), key),  // value 100
			dropped:    costValueTx(1, 100, big.NewInt(1000), big.NewInt(2000), key), // value 2000
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			l := newList(true)
			l.Add(tt.survivor, DefaultConfig.PriceBump)
			l.Add(tt.dropped, DefaultConfig.PriceBump)

			// capNow/otherCapNow re-read the fields (rather than aliasing the
			// *uint256.Int pointers up front) since Filter rebinds whichever
			// one it lowers to a new object.
			capNow := func() *big.Int {
				if tt.valueBasis {
					return l.valuecap.ToBig()
				}
				return l.costcap.ToBig()
			}
			otherCapNow := func() *big.Int {
				if tt.valueBasis {
					return l.costcap.ToBig()
				}
				return l.valuecap.ToBig()
			}
			otherWant := otherCapNow()

			removed, _ := l.Filter(uint256.NewInt(500), bigGasLimit, tt.valueBasis)
			require.Len(t, removed, 1)
			require.Equal(t, tt.dropped.Hash(), removed[0].Hash())
			require.Equal(t, big.NewInt(500), capNow(), "active cap lowered to the limit")
			require.Equal(t, otherWant, otherCapNow(), "other basis's cap untouched")
		})
	}
}

// TestListFilterCapCorruptionRegression is the regression pinned in
// core/txpool/legacypool/list.go's Filter doc comment: a value-basis pass
// over a reserved sender must never leave the list unable to shed the same
// transactions once priced at full cost again (e.g. after deregistration).
//
// Before the dual-cap fix, both bases shared a single costcap. A value-basis
// pass that didn't short-circuit would unconditionally lower that shared cap
// to the balance (list.go:413 in the pre-fix code) without removing anything
// (the transaction's value fits); the next full-cost pass at the same limit
// would then short-circuit on the already-lowered cap (list.go:410) and
// never re-examine the transaction's real (unpayable) cost, stranding it in
// the pool forever.
func TestListFilterCapCorruptionRegression(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	l := newList(true)

	// A fallback-fee-shaped transaction: affordable value, unaffordable full
	// cost (gas*gasPrice alone vastly exceeds the balance below).
	tx := costValueTx(0, 100_000, big.NewInt(1_000_000), big.NewInt(500), key)
	l.Add(tx, DefaultConfig.PriceBump)

	balance := uint256.NewInt(1000)
	const gasLimit = 30_000_000

	// The reserved-sender pass: prices on value alone. The transaction's
	// value (500) fits, so nothing is removed.
	removed, invalid := l.Filter(balance, gasLimit, true)
	require.Empty(t, removed)
	require.Empty(t, invalid)
	require.True(t, l.Contains(0), "tx must survive the value-basis pass")

	// Simulated deregistration: no list-level action, just a later full-cost
	// pass at the identical limit.
	removed, invalid = l.Filter(balance, gasLimit, false)
	require.Empty(t, invalid)
	require.Len(t, removed, 1, "the full-cost pass must still see and drop the unpayable tx")
	require.Equal(t, tx.Hash(), removed[0].Hash())
	require.True(t, l.Empty())
	require.True(t, l.totalcost.IsZero())
	require.True(t, l.totalvalue.IsZero())
}
