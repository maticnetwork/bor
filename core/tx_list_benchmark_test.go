package core

import (
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func init() {
	rand.Seed(time.Now().Unix())
}

func BenchmarkTxListAdd(b *testing.B) {
	// Generate a list of transactions to insert
	key, _ := crypto.GenerateKey()

	txs := make(types.Transactions, 100000)
	for i := 0; i < len(txs); i++ {
		txs[i] = transaction(uint64(i), 0, key)
	}
	// Insert the transactions in a random order
	list := newTxList(true)
	priceLimit := big.NewInt(int64(DefaultTxPoolConfig.PriceLimit))
	b.ResetTimer()
	for _, v := range rand.Perm(len(txs)) {
		list.Add(txs[v], DefaultTxPoolConfig.PriceBump)
		list.Filter(priceLimit, DefaultTxPoolConfig.PriceBump)
	}
}

// Benchmark txPricedList.Reheap
func BenchmarkTxPricedListReheap1KLegacyTxs(b *testing.B) {
	benchmarkTxPricedListLegacyTxsReheap(b, 1000)
}
func BenchmarkTxPricedListReheap10KLegacyTxs(b *testing.B) {
	benchmarkTxPricedListLegacyTxsReheap(b, 10000)
}
func BenchmarkTxPricedListReheap100KLegacyTxs(b *testing.B) {
	benchmarkTxPricedListLegacyTxsReheap(b, 100000)
}
func BenchmarkTxPricedListReheap1MLegacyTxs(b *testing.B) {
	benchmarkTxPricedListLegacyTxsReheap(b, 1000000)
}

func benchmarkTxPricedListLegacyTxsReheap(b *testing.B, txsCount int) {
	// Generate a slice of transactions to insert
	txs := make(types.Transactions, txsCount)

	for i := 0; i < len(txs); i++ {
		// Create priced transaction with random gas price
		txs[i] = newNonSignedTransaction(uint64(i), rand.Int63n(10000))
	}

	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		list := newTxPricedList(newTxLookup())
		// Insert the transactions in a random order.
		// Add only remote transactions, since they are subject to moving from urgent to floating price heap.
		for _, tx := range txs {
			list.all.Add(tx, false)
		}

		// Benchmark txPricedList.Reheap()
		b.StartTimer()
		list.Reheap()
		b.StopTimer()
	}
}

func BenchmarkTxPricedListReheap1KDynamicFeeTxs(b *testing.B) {
	benchmarkTxPricedListDynamicFeeTxsReheap(b, 1000)
}
func BenchmarkTxPricedListReheap10KDynamicFeeTxs(b *testing.B) {
	benchmarkTxPricedListDynamicFeeTxsReheap(b, 10000)
}
func BenchmarkTxPricedListReheap100KDynamicFeeTxs(b *testing.B) {
	benchmarkTxPricedListDynamicFeeTxsReheap(b, 100000)
}
func BenchmarkTxPricedListReheap1MDynamicFeeTxs(b *testing.B) {
	benchmarkTxPricedListLegacyTxsReheap(b, 1000000)
}

func benchmarkTxPricedListDynamicFeeTxsReheap(b *testing.B, txsCount int) {
	// Generate a slice of transactions to insert
	txs := make(types.Transactions, txsCount)

	for i := 0; i < len(txs); i++ {
		// Create priced transaction with random gas price
		txs[i] = newNonSignedDynamicFeeTransaction(uint64(i), rand.Int63n(100), rand.Int63n(10000))
	}

	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		list := newTxPricedList(newTxLookup())
		// Insert the transactions in a random order.
		// Add only remote transactions, since they are subject to moving from urgent to floating price heap.
		for _, tx := range txs {
			list.all.Add(tx, false)
		}

		// Benchmark txPricedList.Reheap()
		b.StartTimer()
		list.Reheap()
		b.StopTimer()
	}
}

// Benchmark Put
func BenchmarkSortedListPut1KAscendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 1000, Ascending)
}
func BenchmarkSortedListPut10KAscendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 10000, Ascending)
}
func BenchmarkSortedListPut100KAscendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 100000, Ascending)
}
func BenchmarkSortedListPut1MAscendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 1000000, Ascending)
}

func BenchmarkSortedListPut1KDescendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 1000, Descending)
}
func BenchmarkSortedListPut10KDescendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 10000, Descending)
}
func BenchmarkSortedListPut100KDescendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 100000, Descending)
}
func BenchmarkSortedListPut1MDescendingNonces(b *testing.B) {
	benchmarkSortedListPut(b, 1000000, Descending)
}

func BenchmarkSortedListPut1KRandomNonces(b *testing.B) {
	benchmarkSortedListPut(b, 1000, Random)
}
func BenchmarkSortedListPut10KRandomNonces(b *testing.B) {
	benchmarkSortedListPut(b, 10000, Random)
}
func BenchmarkSortedListPut100KRandomNonces(b *testing.B) {
	benchmarkSortedListPut(b, 100000, Random)
}
func BenchmarkSortedListPut1MRandomNonces(b *testing.B) {
	benchmarkSortedListPut(b, 1000000, Random)
}

func benchmarkSortedListPut(b *testing.B, txsCount int, nonceAlignment NonceAlignment) {
	sortedList := newSortedList()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		populateTransactions(sortedList, txsCount, nonceAlignment)
		b.StopTimer()

		sortedList.Forward(uint64(txsCount))
	}
}

// Benchmark Remove
func BenchmarkSortedListRemove1K(b *testing.B)   { benchmarkSortedListRemove(b, 1000) }
func BenchmarkSortedListRemove10K(b *testing.B)  { benchmarkSortedListRemove(b, 10000) }
func BenchmarkSortedListRemove100K(b *testing.B) { benchmarkSortedListRemove(b, 100000) }
func BenchmarkSortedListRemove1M(b *testing.B)   { benchmarkSortedListRemove(b, 1000000) }

func benchmarkSortedListRemove(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	txs := make([]*types.Transaction, 0, txsCount)
	for i := 0; i < txsCount; i++ {
		txs = append(txs, newNonSignedTransaction(uint64(i), int64(100000)))
	}

	removedNonces := make([]uint64, 0, txsCount)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		// Remove transactions in random nonces order
		for _, nonce := range rand.Perm(txsCount) {
			removedNonces = append(removedNonces, uint64(nonce))
			sortedList.Remove(uint64(nonce))
		}
		b.StopTimer()

		// Restore previously removed nonces
		for _, nonce := range removedNonces {
			sortedList.Put(txs[nonce])
		}
		removedNonces = nil
	}
}

// Benchmark Get
func BenchmarkSortedListGet1K(b *testing.B)   { benchmarkSortedListGet(b, 1000) }
func BenchmarkSortedListGet10K(b *testing.B)  { benchmarkSortedListGet(b, 10000) }
func BenchmarkSortedListGet100K(b *testing.B) { benchmarkSortedListGet(b, 100000) }
func BenchmarkSortedListGet1M(b *testing.B)   { benchmarkSortedListGet(b, 1000000) }

func benchmarkSortedListGet(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		for _, nonce := range rand.Perm(txsCount) {
			sortedList.Get(uint64(nonce))
		}
		b.StopTimer()

		populateTransactions(sortedList, txsCount, Ascending)
	}
}

// Benchmark Filter
func BenchmarkSortedListFilter1K(b *testing.B)   { benchmarkSortedListFilter(b, 1000) }
func BenchmarkSortedListFilter10K(b *testing.B)  { benchmarkSortedListFilter(b, 10000) }
func BenchmarkSortedListFilter100K(b *testing.B) { benchmarkSortedListFilter(b, 100000) }
func BenchmarkSortedListFilter1M(b *testing.B)   { benchmarkSortedListFilter(b, 1000000) }

func benchmarkSortedListFilter(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		// Filter out all the added transactions (which will remove transactions from the list too)
		sortedList.Filter(func(tx *types.Transaction) bool { return true })
		b.StopTimer()

		populateTransactions(sortedList, txsCount, Ascending)
	}
}

// Benchmark Forward
func BenchmarkSortedListForward1KTxs(b *testing.B)   { benchmarkSortedListForward(b, 1000) }
func BenchmarkSortedListForward10KTxs(b *testing.B)  { benchmarkSortedListForward(b, 10000) }
func BenchmarkSortedListForward100KTxs(b *testing.B) { benchmarkSortedListForward(b, 100000) }
func BenchmarkSortedListForward1MTxs(b *testing.B)   { benchmarkSortedListForward(b, 1000000) }

func benchmarkSortedListForward(b *testing.B, txsCount int) {
	// Generate a list of transactions to insert
	txsMap := populateTransactions(newSortedList(), txsCount, Ascending)
	nonceThreshold := uint64(txsCount / 4)

	txs := make([]*types.Transaction, 0, txsCount)
	for i := 0; i < txsCount; i++ {
		// Create non-priced transaction
		tx := newNonSignedTransaction(uint64(i), int64(100000))
		txs = append(txs, tx)
		txsMap.Put(tx)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		txsMap.Forward(nonceThreshold)
		b.StopTimer()

		// Fill txs which were dropped out of the tx map and nonce heap in previous increment
		for j := 0; j < int(nonceThreshold); j++ {
			txsMap.Put(txs[j])
		}
	}
}

// Benchmark Ready
func BenchmarkSortedListReady1KTxs(b *testing.B)   { benchmarkSortedListReady(b, 1000) }
func BenchmarkSortedListReady10KTxs(b *testing.B)  { benchmarkSortedListReady(b, 10000) }
func BenchmarkSortedListReady100KTxs(b *testing.B) { benchmarkSortedListReady(b, 100000) }
func BenchmarkSortedListReady1MTxs(b *testing.B)   { benchmarkSortedListReady(b, 1000000) }

func benchmarkSortedListReady(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		sortedList.Ready(0)
		b.StopTimer()

		populateTransactions(sortedList, txsCount, Ascending)
	}
}

// Benchmark Cap
func BenchmarkSortedListCap1KTxs(b *testing.B)   { benchmarkSortedListCap(b, 1000) }
func BenchmarkSortedListCap10KTxs(b *testing.B)  { benchmarkSortedListCap(b, 10000) }
func BenchmarkSortedListCap100KTxs(b *testing.B) { benchmarkSortedListCap(b, 100000) }
func BenchmarkSortedListCap1MTxs(b *testing.B)   { benchmarkSortedListCap(b, 1000000) }

func benchmarkSortedListCap(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)
	txs := make([]*types.Transaction, 0, txsCount)
	txs = append(txs, sortedList.Flatten()...)

	hardLimit := txsCount >> 1
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		sortedList.Cap(hardLimit)
		b.StopTimer()

		for j := hardLimit; j < txsCount; j++ {
			sortedList.Put(txs[j])
		}
	}
}

// Benchmark LastElement
func BenchmarkSortedListLastElement1KTxs(b *testing.B)   { benchmarkSortedListLastElement(b, 1000) }
func BenchmarkSortedListLastElement10KTxs(b *testing.B)  { benchmarkSortedListLastElement(b, 10000) }
func BenchmarkSortedListLastElement100KTxs(b *testing.B) { benchmarkSortedListLastElement(b, 100000) }
func BenchmarkSortedListLastElement1MTxs(b *testing.B)   { benchmarkSortedListLastElement(b, 1000000) }

func benchmarkSortedListLastElement(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sortedList.LastElement()
	}
}

// Benchmark Flatten
func BenchmarkSortedListFlatten1KTxs(b *testing.B)   { benchmarkSortedListFlatten(b, 1000) }
func BenchmarkSortedListFlatten10KTxs(b *testing.B)  { benchmarkSortedListFlatten(b, 10000) }
func BenchmarkSortedListFlatten100KTxs(b *testing.B) { benchmarkSortedListFlatten(b, 100000) }
func BenchmarkSortedListFlatten1MTxs(b *testing.B)   { benchmarkSortedListFlatten(b, 1000000) }

func benchmarkSortedListFlatten(b *testing.B, txsCount int) {
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sortedList.Flatten()
	}
}
