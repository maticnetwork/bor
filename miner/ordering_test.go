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
	"crypto/ecdsa"
	"math/big"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestTransactionPriceNonceSortLegacy(t *testing.T) {
	t.Parallel()
	testTransactionPriceNonceSort(t, nil)
}

func TestTransactionPriceNonceSort1559(t *testing.T) {
	t.Parallel()
	testTransactionPriceNonceSort(t, big.NewInt(0))
	testTransactionPriceNonceSort(t, big.NewInt(5))
	testTransactionPriceNonceSort(t, big.NewInt(50))
}

// Tests that transactions can be correctly sorted according to their price in
// decreasing order, but at the same time with increasing nonces when issued by
// the same account.
func testTransactionPriceNonceSort(t *testing.T, baseFee *big.Int) {
	// Generate a batch of accounts to start with
	keys := make([]*ecdsa.PrivateKey, 25)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
	}
	signer := types.LatestSignerForChainID(common.Big1)

	// Generate a batch of transactions with overlapping values, but shifted nonces
	groups := map[common.Address][]*txpool.LazyTransaction{}
	expectedCount := 0
	for start, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		count := 25
		for i := 0; i < 25; i++ {
			var tx *types.Transaction
			gasFeeCap := rand.Intn(50)
			if baseFee == nil {
				tx = types.NewTx(&types.LegacyTx{
					Nonce:    uint64(start + i),
					To:       &common.Address{},
					Value:    big.NewInt(100),
					Gas:      100,
					GasPrice: big.NewInt(int64(gasFeeCap)),
					Data:     nil,
				})
			} else {
				tx = types.NewTx(&types.DynamicFeeTx{
					Nonce:     uint64(start + i),
					To:        &common.Address{},
					Value:     big.NewInt(100),
					Gas:       100,
					GasFeeCap: big.NewInt(int64(gasFeeCap)),
					GasTipCap: big.NewInt(int64(rand.Intn(gasFeeCap + 1))),
					Data:      nil,
				})
				if count == 25 && int64(gasFeeCap) < baseFee.Int64() {
					count = i
				}
			}
			tx, err := types.SignTx(tx, signer, key)
			if err != nil {
				t.Fatalf("failed to sign tx: %s", err)
			}
			groups[addr] = append(groups[addr], &txpool.LazyTransaction{
				Hash:      tx.Hash(),
				Tx:        tx,
				Time:      tx.Time(),
				GasFeeCap: uint256.MustFromBig(tx.GasFeeCap()),
				GasTipCap: uint256.MustFromBig(tx.GasTipCap()),
				Gas:       tx.Gas(),
				BlobGas:   tx.BlobGas(),
			})
		}
		expectedCount += count
	}
	// Sort the transactions and cross check the nonce ordering
	txset := newTransactionsByPriceAndNonce(signer, groups, baseFee, nil)

	txs := types.Transactions{}
	for tx, _ := txset.Peek(); tx != nil; tx, _ = txset.Peek() {
		txs = append(txs, tx.Tx)
		txset.Shift()
	}
	if len(txs) != expectedCount {
		t.Errorf("expected %d transactions, found %d", expectedCount, len(txs))
	}
	for i, txi := range txs {
		fromi, _ := types.Sender(signer, txi)

		// Make sure the nonce order is valid
		for j, txj := range txs[i+1:] {
			fromj, _ := types.Sender(signer, txj)
			if fromi == fromj && txi.Nonce() > txj.Nonce() {
				t.Errorf("invalid nonce ordering: tx #%d (A=%x N=%v) < tx #%d (A=%x N=%v)", i, fromi[:4], txi.Nonce(), i+j, fromj[:4], txj.Nonce())
			}
		}
		// If the next tx has different from account, the price must be lower than the current one
		if i+1 < len(txs) {
			next := txs[i+1]
			fromNext, _ := types.Sender(signer, next)
			tip, err := txi.EffectiveGasTip(baseFee)
			nextTip, nextErr := next.EffectiveGasTip(baseFee)
			if err != nil || nextErr != nil {
				t.Errorf("error calculating effective tip: %v, %v", err, nextErr)
			}
			if fromi != fromNext && tip.Cmp(nextTip) < 0 {
				t.Errorf("invalid gasprice ordering: tx #%d (A=%x P=%v) < tx #%d (A=%x P=%v)", i, fromi[:4], txi.GasPrice(), i+1, fromNext[:4], next.GasPrice())
			}
		}
	}
}

// Tests that if multiple transactions have the same price, the ones seen earlier
// are prioritized to avoid network spam attacks aiming for a specific ordering.
func TestTransactionTimeSort(t *testing.T) {
	t.Parallel()
	// Generate a batch of accounts to start with
	keys := make([]*ecdsa.PrivateKey, 5)
	for i := 0; i < len(keys); i++ {
		keys[i], _ = crypto.GenerateKey()
	}
	signer := types.HomesteadSigner{}

	// Generate a batch of transactions with overlapping prices, but different creation times
	groups := map[common.Address][]*txpool.LazyTransaction{}
	for start, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)

		tx, _ := types.SignTx(types.NewTransaction(0, common.Address{}, big.NewInt(100), 100, big.NewInt(1), nil), signer, key)
		tx.SetTime(time.Unix(0, int64(len(keys)-start)))

		groups[addr] = append(groups[addr], &txpool.LazyTransaction{
			Hash:      tx.Hash(),
			Tx:        tx,
			Time:      tx.Time(),
			GasFeeCap: uint256.MustFromBig(tx.GasFeeCap()),
			GasTipCap: uint256.MustFromBig(tx.GasTipCap()),
			Gas:       tx.Gas(),
			BlobGas:   tx.BlobGas(),
		})
	}
	// Sort the transactions and cross check the nonce ordering
	txset := newTransactionsByPriceAndNonce(signer, groups, nil, nil)

	txs := types.Transactions{}
	for tx, _ := txset.Peek(); tx != nil; tx, _ = txset.Peek() {
		txs = append(txs, tx.Tx)
		txset.Shift()
	}
	if len(txs) != len(keys) {
		t.Errorf("expected %d transactions, found %d", len(keys), len(txs))
	}
	for i, txi := range txs {
		fromi, _ := types.Sender(signer, txi)
		if i+1 < len(txs) {
			next := txs[i+1]
			fromNext, _ := types.Sender(signer, next)

			if txi.GasPrice().Cmp(next.GasPrice()) < 0 {
				t.Errorf("invalid gasprice ordering: tx #%d (A=%x P=%v) < tx #%d (A=%x P=%v)", i, fromi[:4], txi.GasPrice(), i+1, fromNext[:4], next.GasPrice())
			}
			// Make sure time order is ascending if the txs have the same gas price
			if txi.GasPrice().Cmp(next.GasPrice()) == 0 && txi.Time().After(next.Time()) {
				t.Errorf("invalid received time ordering: tx #%d (A=%x T=%v) > tx #%d (A=%x T=%v)", i, fromi[:4], txi.Time(), i+1, fromNext[:4], next.Time())
			}
		}
	}
}

// ----------------------------------------------------------------------------
// Reserved-region ordering
//
// newReservedTransactionsByNonce flips the normal market's comparator: it pops
// the LOWEST effective tip first (so zero-fee reserved transactions win quota
// over fallback-fee ones) and it never drops a below-base-fee transaction —
// those are exactly the transactions the reserved region exists to serve.
// Per-sender nonce order and the arrival-time tie-break are unchanged.
// Transaction stubs (feeTx et al.) live in reserved_test.go.

// drainOrder pops the heap dry and returns the sender of each popped
// transaction, in pop order.
func drainOrder(h *transactionsByPriceAndNonce) []common.Address {
	var order []common.Address
	for {
		ltx, _ := h.Peek()
		if ltx == nil {
			return order
		}
		from, _ := h.PeekFrom()
		order = append(order, from)
		h.Shift()
	}
}

// TestNewReservedTxWithMinerFee pins the reserved wrapper's effective-tip
// computation, including the below-base-fee clamp to zero that the normal
// wrapper instead turns into a drop.
func TestNewReservedTxWithMinerFee(t *testing.T) {
	t.Parallel()

	baseFee := uint256.NewInt(100)
	cases := []struct {
		name    string
		feeCap  uint64
		tipCap  uint64
		baseFee *uint256.Int
		want    uint64
	}{
		{name: "nil base fee uses tip cap", feeCap: 150, tipCap: 7, baseFee: nil, want: 7},
		{name: "zero-fee clamps to zero", feeCap: 0, tipCap: 0, baseFee: baseFee, want: 0},
		{name: "below base fee clamps to zero", feeCap: 50, tipCap: 10, baseFee: baseFee, want: 0},
		{name: "at base fee has no headroom", feeCap: 100, tipCap: 30, baseFee: baseFee, want: 0},
		{name: "tip cap below headroom wins", feeCap: 150, tipCap: 10, baseFee: baseFee, want: 10},
		{name: "headroom caps a large tip", feeCap: 150, tipCap: 80, baseFee: baseFee, want: 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ltx := &txpool.LazyTransaction{GasFeeCap: uint256.NewInt(tc.feeCap), GasTipCap: uint256.NewInt(tc.tipCap), Gas: 21000}

			wrapped, err := newReservedTxWithMinerFee(ltx, common.Address{}, tc.baseFee)
			require.NoError(t, err, "reserved wrapper never rejects")
			require.Equal(t, tc.want, wrapped.fees.Uint64())
		})
	}

	// The normal wrapper rejects the below-base-fee transaction the reserved
	// wrapper just clamped — the divergence this whole mode exists for.
	_, err := newTxWithMinerFee(&txpool.LazyTransaction{GasFeeCap: uint256.NewInt(50), GasTipCap: uint256.NewInt(10)}, common.Address{}, baseFee)
	require.ErrorIs(t, err, types.ErrGasFeeCapTooLow)
}

// TestReservedOrdering pins the reserved heap's two departures from the normal
// market ordering: it pops ascending effective tip (zero-/below-base-fee first)
// and it never drops a below-base-fee transaction. Per-sender nonce order is
// honoured.
func TestReservedOrdering(t *testing.T) {
	t.Parallel()

	baseFee := big.NewInt(100)
	a := common.HexToAddress("0x0a")
	b := common.HexToAddress("0x0b")

	// a: nonce0 fallback-fee (tip 50), nonce1 fallback-fee (tip 60).
	// b: nonce0 zero-fee (below base fee), nonce1 fallback-fee (tip 10).
	txs := map[common.Address][]*txpool.LazyTransaction{
		a: {feeTx(0, 150, 50), feeTx(1, 160, 60)},
		b: {feeTx(0, 0, 0), feeTx(1, 110, 10)},
	}

	h := newReservedTransactionsByNonce(nil, txs, baseFee, nil)

	// Nothing dropped, and b's zero-fee tx leads: b nonce0 (tip 0), b nonce1
	// (tip 10), then a nonce0 (tip 50), a nonce1 (tip 60).
	require.Equal(t, []common.Address{b, b, a, a}, drainOrder(h), "ascending pop, no below-base-fee drop")
}

// TestReservedVsNormalOrderingBelowBaseFee runs the same input through both
// heap modes and pins their divergence, guarding each against regressions in
// the shared code: the normal market drops below-base-fee transactions (at
// construction for an account's head, at Shift for its successors) while the
// reserved mode keeps every transaction, clamped to tip zero — but still
// behind the sender's earlier nonces.
func TestReservedVsNormalOrderingBelowBaseFee(t *testing.T) {
	t.Parallel()

	baseFee := big.NewInt(100)
	x := common.HexToAddress("0x0a") // head below base fee
	y := common.HexToAddress("0x0b") // fully above base fee
	z := common.HexToAddress("0x0c") // successor below base fee

	newInput := func() map[common.Address][]*txpool.LazyTransaction {
		return map[common.Address][]*txpool.LazyTransaction{
			x: {feeTx(0, 0, 0)},
			y: {feeTx(1, 150, 20)},
			z: {feeTx(2, 150, 30), feeTx(3, 50, 10)},
		}
	}

	// Normal mode: x dies at construction, z's second tx dies at Shift.
	// Descending tips leave z (30) before y (20).
	normal := newTransactionsByPriceAndNonce(nil, newInput(), baseFee, nil)
	require.Equal(t, []common.Address{z, y}, drainOrder(normal))

	// Reserved mode: everything survives, ascending. x (0) first; z's clamped
	// zero-tip tx pops last because nonce order gates it behind z's tip-30 tx.
	reserved := newReservedTransactionsByNonce(nil, newInput(), baseFee, nil)
	require.Equal(t, []common.Address{x, y, z, z}, drainOrder(reserved))
}

// TestReservedOrderingTimeTieBreak confirms the arrival-time tie-break carries
// over to the reserved mode: equal effective tips (here two zero-fee txs) pop
// earliest-seen first, the same spam-resistance rule as the normal market.
func TestReservedOrderingTimeTieBreak(t *testing.T) {
	t.Parallel()

	baseFee := big.NewInt(100)
	a := common.HexToAddress("0x0a")
	b := common.HexToAddress("0x0b")

	// feeTx's first argument seeds arrival time: b's tx is seen before a's.
	txs := map[common.Address][]*txpool.LazyTransaction{
		a: {feeTx(5, 0, 0)},
		b: {feeTx(3, 0, 0)},
	}

	h := newReservedTransactionsByNonce(nil, txs, baseFee, nil)
	require.Equal(t, []common.Address{b, a}, drainOrder(h), "equal tips pop in arrival order")
}

// TestReservedOrderingShiftKeepsAscending guards the Shift path: replacing a
// popped head with the sender's next nonce must re-wrap it in reserved mode.
// If Shift fell back to the normal wrapper, a's below-base-fee successor would
// be rejected and the whole account silently dropped mid-drain.
func TestReservedOrderingShiftKeepsAscending(t *testing.T) {
	t.Parallel()

	baseFee := big.NewInt(100)
	a := common.HexToAddress("0x0a")
	b := common.HexToAddress("0x0b")

	txs := map[common.Address][]*txpool.LazyTransaction{
		a: {feeTx(0, 0, 0), feeTx(1, 0, 0)},
		b: {feeTx(2, 110, 10)},
	}

	h := newReservedTransactionsByNonce(nil, txs, baseFee, nil)

	var fees []uint64
	for {
		ltx, fee := h.Peek()
		if ltx == nil {
			break
		}
		fees = append(fees, fee.Uint64())
		h.Shift()
	}
	require.Equal(t, []uint64{0, 0, 10}, fees, "a's successor stays clamped at zero and ahead of b")
}

// TestOrderingInterruptYieldsEmpty pins that both heap modes honour the block
// building interrupt at construction by yielding an empty set.
func TestOrderingInterruptYieldsEmpty(t *testing.T) {
	t.Parallel()

	baseFee := big.NewInt(100)
	txs := func() map[common.Address][]*txpool.LazyTransaction {
		return map[common.Address][]*txpool.LazyTransaction{common.HexToAddress("0x0a"): {feeTx(0, 150, 10)}}
	}

	interrupted := new(atomic.Bool)
	interrupted.Store(true)

	require.True(t, newTransactionsByPriceAndNonce(nil, txs(), baseFee, interrupted).Empty(), "normal mode")
	require.True(t, newReservedTransactionsByNonce(nil, txs(), baseFee, interrupted).Empty(), "reserved mode")

	// Sanity: without the interrupt the same input is not empty.
	require.False(t, newReservedTransactionsByNonce(nil, txs(), baseFee, new(atomic.Bool)).Empty())
}

// TestClonePreservesReservedOrdering guards the sendPlan/prefetch path: cloning
// a reserved heap must keep the ascending, never-drop ordering so the plan the
// prefetcher sees matches what commitTransactions will execute.
func TestClonePreservesReservedOrdering(t *testing.T) {
	t.Parallel()

	baseFee := big.NewInt(100)
	a := common.HexToAddress("0x0a")
	b := common.HexToAddress("0x0b")
	txs := map[common.Address][]*txpool.LazyTransaction{
		a: {feeTx(0, 150, 50)}, // fallback-fee
		b: {feeTx(1, 0, 0)},    // zero-fee
	}

	clone := newReservedTransactionsByNonce(nil, txs, baseFee, nil).clone()
	require.True(t, clone.reserved, "clone keeps the reserved flag")
	require.True(t, clone.heads.ascending, "clone keeps ascending ordering")

	// Zero-fee still pops before fallback-fee on the clone.
	from, ok := clone.PeekFrom()
	require.True(t, ok)
	require.Equal(t, b, from)
}
