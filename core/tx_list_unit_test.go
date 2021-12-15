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
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

type NonceAlignment int

const (
	Ascending NonceAlignment = iota
	Descending
	Random
)

func init() {
	rand.Seed(time.Now().Unix())
}

func newNonSignedTransaction(nonce uint64, gasPrice int64) *types.Transaction {
	return types.NewTransaction(nonce, common.Address{}, big.NewInt(100), uint64(100000), big.NewInt(gasPrice), nil)
}

func newNonSignedDynamicFeeTransaction(nonce uint64, gasTipCap, gasFeeCap int64) *types.Transaction {
	return types.NewTx(&types.DynamicFeeTx{
		Nonce:     nonce,
		To:        &common.Address{},
		Gas:       100000,
		GasFeeCap: big.NewInt(gasFeeCap),
		GasTipCap: big.NewInt(gasTipCap),
		Data:      []byte{},
	})
}

func newSortedList() *txSortedMap {
	return newTxSortedMap()
}

func populateTransactions(sortedList *txSortedMap, txsCount int, nonceAlignment NonceAlignment) *txSortedMap {
	if nonceAlignment == Ascending {
		// Create transaction in an ascending nonce order
		for nonce := 0; nonce < txsCount; nonce++ {
			tx := newNonSignedTransaction(uint64(nonce), 100000)
			sortedList.Put(tx)
		}
	} else if nonceAlignment == Random {
		// Create transaction in a random nonce order
		for _, nonce := range rand.Perm(txsCount) {
			tx := newNonSignedTransaction(uint64(nonce), 100000)
			sortedList.Put(tx)
		}
	} else {
		// Create transaction in a descending nonce order
		for nonce := txsCount - 1; nonce >= 0; nonce-- {
			tx := newNonSignedTransaction(uint64(nonce), 100000)
			sortedList.Put(tx)
		}
	}
	return sortedList
}

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

func TestTxSortedListPutAscending(t *testing.T)  { testTxSortedListPut(t, Ascending) }
func TestTxSortedListPutDescending(t *testing.T) { testTxSortedListPut(t, Descending) }
func TestTxSortedListPutRandom(t *testing.T)     { testTxSortedListPut(t, Random) }

func testTxSortedListPut(t *testing.T, nonceAlignment NonceAlignment) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, nonceAlignment)

	// Check whether all of the transactions are present within the cache
	if sortedList.Len() != txsCount {
		t.Fatalf("Expected %d transactions, but got %d", txsCount, sortedList.Len())
	}

	// If transactions are ordered correctly, it is expected that all of them are retrieved back
	readies := sortedList.Ready(0)
	if len(readies) != txsCount {
		t.Fatalf("Expected %d ready transactions, but got %d", txsCount, len(readies))
	}

	// Check whether ready transactions are in nonce-increasing order
	var expectedNonce uint64 = 0
	for i, ready := range readies {
		if expectedNonce != ready.Nonce() {
			t.Fatalf("%d transaction has invalid nonce. Expected nonce %d valid transactions, but got %d", i, expectedNonce, ready.Nonce())
		}
		expectedNonce++
	}

	if length := sortedList.Len(); length != 0 {
		t.Fatalf("Expected transactions list to be empty, but it was %d length", length)
	}
}

func TestSortedListPutSameNonces(t *testing.T) {
	txsCount := 10
	sortedList := newTxSortedMap()
	for i := 0; i < txsCount; i++ {
		sortedList.Put(newNonSignedTransaction(5, int64(100000)))
	}
	if length := sortedList.Len(); length != 1 {
		t.Fatalf("Expected 1 transaction, but got %d", length)
	}
}

func TestSortedListRemove(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	remainingTxsCount := txsCount
	for i, nonce := range rand.Perm(txsCount) {
		if sortedList.Remove(uint64(nonce)) {
			remainingTxsCount--
			if length := sortedList.Len(); remainingTxsCount != length {
				t.Fatalf("%d. iteration, expected %d transactions in cache, but got %d", i+1, remainingTxsCount, length)
			}
		} else {
			t.Fatalf("%d. iteration, failed to delete transaction with nonce %d", i+1, nonce)
		}
	}

	if length := sortedList.Len(); length != 0 {
		t.Fatalf("Expected transactions list to be empty, but it size was %d", length)
	}
}

func TestSortedListGet(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	expectedNonce := uint64(rand.Intn(txsCount))
	tx := sortedList.Get(expectedNonce)
	// Check if expected nonce and gotten nonce match
	if nonce := tx.Nonce(); nonce != expectedNonce {
		t.Fatalf("Expected transaction with nonce: %d, but got transaction with nonce: %d", expectedNonce, nonce)
	}
}

func TestSortedListReady(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	nonceGappedTxsCount := 3
	// Put nonce gapped transactions
	for i := 1; i <= nonceGappedTxsCount; i++ {
		sortedList.Put(newNonSignedTransaction(uint64(txsCount+i*10), 100000))
	}

	// Get all non nonce-gapped transactions, from the beggining
	readies := sortedList.Ready(0)
	if len(readies) != txsCount {
		t.Fatalf("Expected %d readies, but got %d", txsCount, len(readies))
	}

	// After Ready is invoked, only nonce-gapped transactions remain in the cache
	if sortedList.Len() != nonceGappedTxsCount {
		t.Fatalf("Expected %d transactions remain in the list after invoking Ready function, but got %d", nonceGappedTxsCount, sortedList.Len())
	}

	// Sending less starting nonce than present in cache
	readies = sortedList.Ready(0)
	if readies != nil {
		t.Fatalf("Expected nil readies, but got %d of them", len(readies))
	}

	// Set start nonce to first nonce gapped transaction and make sure it is returned from Ready function
	readies = sortedList.Ready(uint64(txsCount + 10))
	if len(readies) != 1 {
		t.Fatalf("Expected one ready transaction, but got %d of them.", len(readies))
	}
	if tx := readies[0]; tx.Nonce() != uint64(txsCount+10) {
		t.Fatalf("Expected transaction nonce %d, but got %d", uint64(txsCount+10), tx.Nonce())
	}
	nonceGappedTxsCount--
	// Only one nonce gapped transaction was poped from the transaction list, so there are rest of those remaining in list
	if length := sortedList.Len(); length != nonceGappedTxsCount {
		t.Fatalf("Expected that transaction cache has %d, but it has %d transactions", nonceGappedTxsCount, length)
	}
}

func TestSortedListForward(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	// All transactions should be removed from cache and returned back
	removedTxs := sortedList.Forward(uint64(txsCount))
	if removedCount := len(removedTxs); removedCount != txsCount {
		t.Fatalf("Expected %d removed transactions, but got %d of them.", txsCount, len(removedTxs))
	}
	if length := sortedList.Len(); length != 0 {
		t.Fatalf("Expected that transaction cache is empty, but it has %d items", length)
	}

	// Create nonce-gapped transactions
	for i := 0; i < txsCount; i++ {
		tx := newNonSignedTransaction(uint64(5*i), 100000)
		sortedList.Put(tx)
	}
	// All transactions should be removed from cache and returned back
	removedTxs = sortedList.Forward(uint64(txsCount * 5))
	if removedCount := len(removedTxs); removedCount != txsCount {
		t.Fatalf("Expected %d removed transactions, but got %d of them.", txsCount, len(removedTxs))
	}
	if length := sortedList.Len(); length != 0 {
		t.Fatalf("Expected that transaction cache is empty, but it has %d items", length)
	}
}

func TestSortedListCap(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	drops := sortedList.Cap(txsCount * 2)
	if drops != nil {
		t.Fatalf("Not expected drops to be returned, but received %v", drops)
	}

	drops = sortedList.Cap(txsCount / 2)
	if len(drops) != txsCount/2 {
		t.Fatalf("Expected %d drops to be returned, but received %d", txsCount/2, len(drops))
	}
	if sortedList.Len() != txsCount/2 {
		t.Fatalf("Expected %d transactions remaining in cache, but remained %d", txsCount/2, sortedList.Len())
	}
}

func TestSortedListFilter(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	filteredTxs := sortedList.Filter(func(t *types.Transaction) bool {
		return t.Nonce() > uint64(txsCount/2-1)
	})
	if filteredTxsCount := filteredTxs.Len(); filteredTxsCount != txsCount/2 {
		t.Fatalf("Expected %d returned filtered transactions, but got %d", txsCount/2, filteredTxsCount)
	}
	if remainingTxsCount := sortedList.Len(); remainingTxsCount != txsCount/2 {
		t.Fatalf("Expected %d transactions remaining in cache, but got %d", txsCount/2, remainingTxsCount)
	}
}

func TestSortedListFlatten(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	flattenList := sortedList.Flatten()

	// Check whether all of the transactions are present within the cache
	if sortedList.Len() != txsCount {
		t.Fatalf("Expected %d transactions, but got %d", txsCount, sortedList.Len())
	}

	// Check whether ready transactions are in nonce-increasing order
	var expectedNonce uint64 = 0
	for i, tx := range flattenList {
		if expectedNonce != tx.Nonce() {
			t.Fatalf("%d transaction has invalid nonce. Expected nonce %d valid transactions, but got %d", i, expectedNonce, tx.Nonce())
		}
		expectedNonce++
	}
}

func TestSortedListLastElement(t *testing.T) {
	txsCount := 10
	sortedList := populateTransactions(newSortedList(), txsCount, Ascending)

	tx := sortedList.LastElement()
	if tx.Nonce() != uint64(txsCount-1) {
		t.Fatalf("Expected transaction nonce %d, but got %d", uint64(txsCount-1), tx.Nonce())
	}
}
