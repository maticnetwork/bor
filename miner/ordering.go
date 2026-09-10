// Copyright 2014 The go-ethereum Authors
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

package miner

import (
	"container/heap"
	"math/big"
	"sync/atomic"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
)

// txWithMinerFee wraps a transaction with its gas price or effective miner gasTipCap
type txWithMinerFee struct {
	tx   *txpool.LazyTransaction
	from common.Address
	fees *uint256.Int
}

// newTxWithMinerFee creates a wrapped transaction, calculating the effective
// miner gasTipCap if a base fee is provided.
// Returns error in case of a negative effective miner gasTipCap.
func newTxWithMinerFee(tx *txpool.LazyTransaction, from common.Address, baseFee *uint256.Int) (*txWithMinerFee, error) {
	tip := new(uint256.Int).Set(tx.GasTipCap)
	if baseFee != nil {
		if tx.GasFeeCap.Cmp(baseFee) < 0 {
			return nil, types.ErrGasFeeCapTooLow
		}
		tip = new(uint256.Int).Sub(tx.GasFeeCap, baseFee)
		if tip.Gt(tx.GasTipCap) {
			tip = tx.GasTipCap
		}
	}
	return &txWithMinerFee{
		tx:   tx,
		from: from,
		fees: tip,
	}, nil
}

// newReservedTxWithMinerFee wraps a reserved-region transaction. Unlike
// newTxWithMinerFee it never rejects a below-base-fee (including zero-fee)
// transaction — those are exactly the transactions the reserved region exists
// to serve. A below-base-fee transaction's effective tip is clamped to zero so
// the ascending reserved heap pops it before transactions carrying a fallback
// tip that could instead compete in the normal region.
func newReservedTxWithMinerFee(tx *txpool.LazyTransaction, from common.Address, baseFee *uint256.Int) (*txWithMinerFee, error) {
	tip := new(uint256.Int).Set(tx.GasTipCap)
	if baseFee != nil {
		if tx.GasFeeCap.Cmp(baseFee) < 0 {
			tip = uint256.NewInt(0)
		} else {
			tip = new(uint256.Int).Sub(tx.GasFeeCap, baseFee)
			if tip.Gt(tx.GasTipCap) {
				tip = new(uint256.Int).Set(tx.GasTipCap)
			}
		}
	}
	return &txWithMinerFee{tx: tx, from: from, fees: tip}, nil
}

// wrapTxWithMinerFee dispatches to the reserved or normal fee wrapper.
func wrapTxWithMinerFee(tx *txpool.LazyTransaction, from common.Address, baseFee *uint256.Int, reserved bool) (*txWithMinerFee, error) {
	if reserved {
		return newReservedTxWithMinerFee(tx, from, baseFee)
	}
	return newTxWithMinerFee(tx, from, baseFee)
}

// txByPriceAndTime implements both the sort and the heap interface, making it useful
// for all at once sorting as well as individually adding and removing elements.
//
// ascending flips the fee comparison: the normal market pops highest effective
// tip first (ascending=false); the reserved region pops lowest first so
// zero-/below-base-fee transactions are served before fallback-fee ones.
type txByPriceAndTime struct {
	items     []*txWithMinerFee
	ascending bool
}

func (s txByPriceAndTime) Len() int { return len(s.items) }
func (s txByPriceAndTime) Less(i, j int) bool {
	// If the prices are equal, use the time the transaction was first seen for
	// deterministic sorting
	cmp := s.items[i].fees.Cmp(s.items[j].fees)
	if cmp == 0 {
		return s.items[i].tx.Time.Before(s.items[j].tx.Time)
	}
	if s.ascending {
		return cmp < 0
	}
	return cmp > 0
}
func (s txByPriceAndTime) Swap(i, j int) { s.items[i], s.items[j] = s.items[j], s.items[i] }

func (s *txByPriceAndTime) Push(x interface{}) {
	s.items = append(s.items, x.(*txWithMinerFee))
}

func (s *txByPriceAndTime) Pop() interface{} {
	old := s.items
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	s.items = old[0 : n-1]
	return x
}

// transactionsByPriceAndNonce represents a set of transactions that can return
// transactions in a profit-maximizing sorted order, while supporting removing
// entire batches of transactions for non-executable accounts.
type transactionsByPriceAndNonce struct {
	txs      map[common.Address][]*txpool.LazyTransaction // Per account nonce-sorted list of transactions
	heads    txByPriceAndTime                             // Next transaction for each unique account (price heap)
	signer   types.Signer                                 // Signer for the set of transactions
	baseFee  *uint256.Int                                 // Current base fee
	reserved bool                                         // Reserved-region ordering: ascending tip, never drop below-base-fee txs
}

// newTransactionsByPriceAndNonce creates a transaction set that can retrieve
// price sorted transactions in a nonce-honouring way.
//
// Note, the input map is reowned so the caller should not interact any more with
// if after providing it to the constructor.
//
// The construction is halted if interrupt is set (during block building timeout).
func newTransactionsByPriceAndNonce(signer types.Signer, txs map[common.Address][]*txpool.LazyTransaction, baseFee *big.Int, interrupt *atomic.Bool) *transactionsByPriceAndNonce {
	return newTxByPriceAndNonce(signer, txs, baseFee, interrupt, false)
}

// newReservedTransactionsByNonce builds an ordering for the reserved region. It
// never drops below-base-fee (including zero-fee) transactions and pops them
// before transactions carrying a fallback tip (ascending effective tip), while
// honouring per-account nonce order. Same interface as the normal ordering, so
// commitTransactions consumes it unchanged.
func newReservedTransactionsByNonce(signer types.Signer, txs map[common.Address][]*txpool.LazyTransaction, baseFee *big.Int, interrupt *atomic.Bool) *transactionsByPriceAndNonce {
	return newTxByPriceAndNonce(signer, txs, baseFee, interrupt, true)
}

func newTxByPriceAndNonce(signer types.Signer, txs map[common.Address][]*txpool.LazyTransaction, baseFee *big.Int, interrupt *atomic.Bool, reserved bool) *transactionsByPriceAndNonce {
	// Convert the basefee from header format to uint256 format
	var baseFeeUint *uint256.Int
	if baseFee != nil {
		baseFeeUint = uint256.MustFromBig(baseFee)
	}
	if interrupt == nil {
		interrupt = new(atomic.Bool)
	}
	// Initialize a price and received time based heap with the head transactions
	heads := txByPriceAndTime{items: make([]*txWithMinerFee, 0, len(txs)), ascending: reserved}
	for from, accTxs := range txs {
		// Check for the flag to interrupt block building on timeout.
		if interrupt.Load() {
			// We could send partial set of sorted transactions but they'll anyways
			// be rejected during commit transactions. Instead send an empty list.
			return emptyTransactionsByPriceAndNonce(signer, baseFeeUint, reserved)
		}
		wrapped, err := wrapTxWithMinerFee(accTxs[0], from, baseFeeUint, reserved)
		if err != nil {
			delete(txs, from)
			continue
		}
		heads.items = append(heads.items, wrapped)
		txs[from] = accTxs[1:]
	}
	heap.Init(&heads)

	// Assemble and return the transaction set
	return &transactionsByPriceAndNonce{
		txs:      txs,
		heads:    heads,
		signer:   signer,
		baseFee:  baseFeeUint,
		reserved: reserved,
	}
}

func emptyTransactionsByPriceAndNonce(signer types.Signer, baseFee *uint256.Int, reserved bool) *transactionsByPriceAndNonce {
	return &transactionsByPriceAndNonce{
		signer:   signer,
		txs:      map[common.Address][]*txpool.LazyTransaction{},
		heads:    txByPriceAndTime{items: make([]*txWithMinerFee, 0), ascending: reserved},
		baseFee:  baseFee,
		reserved: reserved,
	}
}

// Peek returns the next transaction by price.
func (t *transactionsByPriceAndNonce) Peek() (*txpool.LazyTransaction, *uint256.Int) {
	if len(t.heads.items) == 0 {
		return nil, nil
	}
	return t.heads.items[0].tx, t.heads.items[0].fees
}

// PeekFrom returns the sender of the next transaction without resolving the
// full transaction. The reserved selection scan uses it to group transactions
// by sender as it pops them in ascending-fee order.
func (t *transactionsByPriceAndNonce) PeekFrom() (common.Address, bool) {
	if len(t.heads.items) == 0 {
		return common.Address{}, false
	}
	return t.heads.items[0].from, true
}

// Shift replaces the current best head with the next one from the same account.
func (t *transactionsByPriceAndNonce) Shift() {
	acc := t.heads.items[0].from
	if txs, ok := t.txs[acc]; ok && len(txs) > 0 {
		if wrapped, err := wrapTxWithMinerFee(txs[0], acc, t.baseFee, t.reserved); err == nil {
			t.heads.items[0], t.txs[acc] = wrapped, txs[1:]
			heap.Fix(&t.heads, 0)
			return
		}
	}
	heap.Pop(&t.heads)
}

func (t *transactionsByPriceAndNonce) GetTxs() int {
	return len(t.txs)
}

// Pop removes the best transaction, *not* replacing it with the next one from
// the same account. This should be used when a transaction cannot be executed
// and hence all subsequent ones should be discarded from the same account.
func (t *transactionsByPriceAndNonce) Pop() {
	heap.Pop(&t.heads)
}

// Empty returns if the price heap is empty. It can be used to check it simpler
// than calling peek and checking for nil return.
func (t *transactionsByPriceAndNonce) Empty() bool {
	return len(t.heads.items) == 0
}

// Clear removes the entire content of the heap.
func (t *transactionsByPriceAndNonce) Clear() {
	t.heads.items, t.txs = nil, nil
}

// clone returns a shallow copy of the heap suitable for non-destructive scanning.
// LazyTransaction pointers are shared with the original; only the per-account queue
// slices and the heads slice are newly allocated. The heap invariant is preserved in
// the copy because we duplicate the slice in its current heap-ordered state.
func (t *transactionsByPriceAndNonce) clone() *transactionsByPriceAndNonce {
	clonedTxs := make(map[common.Address][]*txpool.LazyTransaction, len(t.txs))
	for addr, queue := range t.txs {
		c := make([]*txpool.LazyTransaction, len(queue))
		copy(c, queue)
		clonedTxs[addr] = c
	}
	clonedItems := make([]*txWithMinerFee, len(t.heads.items))
	copy(clonedItems, t.heads.items)
	return &transactionsByPriceAndNonce{
		txs:      clonedTxs,
		heads:    txByPriceAndTime{items: clonedItems, ascending: t.heads.ascending},
		signer:   t.signer,
		baseFee:  t.baseFee,
		reserved: t.reserved,
	}
}
