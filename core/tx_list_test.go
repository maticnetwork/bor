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
	"math/rand"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
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
	priceLimit := uint256.NewInt(DefaultTxPoolConfig.PriceLimit)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		list := newTxList(true)

		for _, v := range rand.Perm(len(txs)) {
			list.Add(txs[v], DefaultTxPoolConfig.PriceBump)
			list.Filter(priceLimit, DefaultTxPoolConfig.PriceBump)
		}
	}
}

func TestFilterTxConditional(t *testing.T) {
	// Create an in memory state db to test against.
	memDb := rawdb.NewMemoryDatabase()
	db := state.NewDatabase(memDb)
	state, _ := state.New(common.Hash{}, db, nil)

	// Create a private key to sign transactions.
	key, _ := crypto.GenerateKey()

	// Create a list.
	list := newTxList(true)

	// Create a transaction with no defined tx options
	// and add to the list.
	tx := transaction(0, 1000, key)
	list.Add(tx, DefaultTxPoolConfig.PriceBump)

	// There should be no drops at this point.
	// No state has been modified.
	drops := list.FilterTxConditional(state)

	if count := len(drops); count != 0 {
		t.Fatalf("got %d filtered by TxOptions when there should not be any", count)
	}

	// Create another transaction with a known account storage root tx option
	// and add to the list.
	tx2 := transaction(1, 1000, key)

	var options types.OptionsAA4337

	options.KnownAccounts = types.KnownAccounts{
		common.Address{19: 1}: &types.Value{
			Single: common.HexToRefHash("0xe734938daf39aae1fa4ee64dc3155d7c049f28b57a8ada8ad9e86832e0253bef"),
		},
	}

	state.SetState(common.Address{19: 1}, common.Hash{}, common.Hash{30: 1})
	tx2.PutOptions(&options)
	list.Add(tx2, DefaultTxPoolConfig.PriceBump)

	// There should still be no drops as no state has been modified.
	drops = list.FilterTxConditional(state)

	if count := len(drops); count != 0 {
		t.Fatalf("got %d filtered by TxOptions when there should not be any", count)
	}

	// Set state that conflicts with tx2's policy
	state.SetState(common.Address{19: 1}, common.Hash{}, common.Hash{31: 1})

	// tx2 should be the single transaction filtered out
	drops = list.FilterTxConditional(state)

	if count := len(drops); count != 1 {
		t.Fatalf("got %d filtered by TxOptions when there should be a single one", count)
	}

	if drops[0] != tx2 {
		t.Fatalf("Got %x, expected %x", drops[0].Hash(), tx2.Hash())
	}
}
