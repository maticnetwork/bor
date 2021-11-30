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

package core

import (
	"math/big"
	"math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Tests that transactions can be added to strict lists and list contents and
// nonce boundaries are correctly maintained.
func TestStrictTxListAdd(t *testing.T) {
	// Generate a list of transactions to insert
	key, _ := crypto.GenerateKey()

	txs := make(types.Transactions, 1024)
	for i := 0; i < len(txs); i++ {
		txs[i] = transaction(uint64(i), 0, key)
	}
	// Insert the transactions in a random order
	list := newTxList(true)
	for _, v := range rand.Perm(len(txs)) {
		list.Add(txs[v], DefaultTxPoolConfig.PriceBump)
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
func BenchmarkTxListReheap1KTxs(b *testing.B)   { benchmarkTxListReheap(b, 1000) }
func BenchmarkTxListReheap10KTxs(b *testing.B)  { benchmarkTxListReheap(b, 10000) }
func BenchmarkTxListReheap100KTxs(b *testing.B) { benchmarkTxListReheap(b, 100000) }

func benchmarkTxListReheap(b *testing.B, txCount int) {
	// Generate a slice of transactions to insert
	txs := make(types.Transactions, txCount)
	key, _ := crypto.GenerateKey()

	for i := 0; i < len(txs); i++ {
		// Create priced transaction with random gas price
		txs[i] = pricedTransaction(uint64(i), 100000, big.NewInt(rand.Int63n(10000)), key)
	}

	// Benchmark txPricedList.Reheap()
	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		// Insert the transactions in a random order
		list := newTxPricedList(newTxLookup())
		for _, ind := range rand.Perm(len(txs)) {
			list.all.Add(txs[ind], false)
		}

		b.StartTimer()
		list.Reheap()
		b.StopTimer()
	}
}

// Benchmark txSortedMap.Forward()
func BenchmarkTxSortedMapForward1KTxs(b *testing.B)   { benchmarkTxSortedMapForward(b, 1000) }
func BenchmarkTxSortedMapForward10KTxs(b *testing.B)  { benchmarkTxSortedMapForward(b, 10000) }
func BenchmarkTxSortedMapForward100KTxs(b *testing.B) { benchmarkTxSortedMapForward(b, 100000) }

func benchmarkTxSortedMapForward(b *testing.B, txCount int) {
	// Generate a list of transactions to insert
	key, _ := crypto.GenerateKey()
	txsMap := newTxSortedMap()
	nonceThreshold := uint64(txCount / 4)

	txs := make([]*types.Transaction, 0, txCount)
	for i := 0; i < txCount; i++ {
		// Create non-priced transaction
		tx := transaction(uint64(i), uint64(100000), key)
		txs = append(txs, tx)
		txsMap.Put(tx)
	}

	// Benchmark txSortedMap.Forward()
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
