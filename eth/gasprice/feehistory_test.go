// Copyright 2021 The go-ethereum Authors
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

package gasprice

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

type futurePendingBackend struct {
	*testBackend
	block *types.Block
}

func (b *futurePendingBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return b.block, nil
}

func TestResolveBlockRangeTruncatesSpeculativeGap(t *testing.T) {
	backend := newTestBackend(t, big.NewInt(16), big.NewInt(28), false)
	defer backend.teardown()
	future := &futurePendingBackend{
		testBackend: backend,
		block:       types.NewBlockWithHeader(&types.Header{Number: big.NewInt(testHead + 2)}),
	}
	oracle := NewOracle(future, Config{MaxHeaderHistory: 1000}, nil)

	pending, _, last, count, err := oracle.resolveBlockRange(t.Context(), rpc.PendingBlockNumber, 2)
	if err != nil {
		t.Fatal(err)
	}
	if last != testHead+2 || count != 1 {
		t.Fatalf("resolved range = (%d, %d), want (%d, 1)", last, count, testHead+2)
	}
	if pending == nil {
		t.Fatal("speculative range lost the pending snapshot")
	}

	pending, receipts, last, count, err := oracle.resolveBlockRange(t.Context(), rpc.PendingBlockNumber, 3)
	if err != nil {
		t.Fatal(err)
	}
	if last != testHead || count != 1 {
		t.Fatalf("resolved historical range = (%d, %d), want (%d, 1)", last, count, testHead)
	}
	if pending != nil || receipts != nil {
		t.Fatal("historical range retained the pending snapshot")
	}

	pending, receipts, last, count, err = oracle.resolveBlockRange(t.Context(), rpc.PendingBlockNumber, 5)
	if err != nil {
		t.Fatal(err)
	}
	if last != testHead || count != 3 {
		t.Fatalf("resolved historical range = (%d, %d), want (%d, 3)", last, count, testHead)
	}
	if pending != nil || receipts != nil {
		t.Fatal("canonical history retained the pending snapshot")
	}

	_, _, last, count, err = oracle.resolveBlockRange(t.Context(), rpc.PendingBlockNumber, 1)
	if err != nil {
		t.Fatal(err)
	}
	if last != testHead+2 || count != 1 {
		t.Fatalf("resolved speculative range = (%d, %d), want (%d, 1)", last, count, testHead+2)
	}

	first, _, baseFee, ratio, _, _, err := oracle.FeeHistory(t.Context(), 5, rpc.PendingBlockNumber, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Uint64() != testHead-2 || len(baseFee) != 4 || len(ratio) != 3 {
		t.Fatalf("fee history = first %d, base fees %d, ratios %d", first.Uint64(), len(baseFee), len(ratio))
	}
}

func TestFeeHistory(t *testing.T) {
	var cases = []struct {
		pending             bool
		maxHeader, maxBlock uint64
		count               uint64
		last                rpc.BlockNumber
		percent             []float64
		expFirst            uint64
		expCount            int
		expErr              error
	}{
		{false, 1000, 1000, 10, 30, nil, 21, 10, nil},
		{false, 1000, 1000, 10, 30, []float64{0, 10}, 21, 10, nil},
		{false, 1000, 1000, 10, 30, []float64{20, 10}, 0, 0, errInvalidPercentile},
		{false, 1000, 1000, 1000000000, 30, nil, 0, 31, nil},
		{false, 1000, 1000, 1000000000, rpc.LatestBlockNumber, nil, 0, 33, nil},
		{false, 1000, 1000, 10, 40, nil, 0, 0, errRequestBeyondHead},
		{true, 1000, 1000, 10, 40, nil, 0, 0, errRequestBeyondHead},
		{false, 20, 2, 100, rpc.LatestBlockNumber, nil, 13, 20, nil},
		{false, 20, 2, 100, rpc.LatestBlockNumber, []float64{0, 10}, 31, 2, nil},
		{false, 20, 2, 100, 32, []float64{0, 10}, 31, 2, nil},
		{false, 1000, 1000, 1, rpc.PendingBlockNumber, nil, 0, 0, nil},
		{false, 1000, 1000, 2, rpc.PendingBlockNumber, nil, 32, 1, nil},
		{true, 1000, 1000, 2, rpc.PendingBlockNumber, nil, 32, 2, nil},
		{true, 1000, 1000, 2, rpc.PendingBlockNumber, []float64{0, 10}, 32, 2, nil},
		{false, 1000, 1000, 2, rpc.FinalizedBlockNumber, []float64{0, 10}, 24, 2, nil},
		{false, 1000, 1000, 2, rpc.SafeBlockNumber, []float64{0, 10}, 24, 2, nil},
	}

	for i, c := range cases {
		config := Config{
			MaxHeaderHistory: c.maxHeader,
			MaxBlockHistory:  c.maxBlock,
		}
		backend := newTestBackend(t, big.NewInt(16), big.NewInt(28), c.pending)
		oracle := NewOracle(backend, config, nil)

		first, reward, baseFee, ratio, blobBaseFee, blobRatio, err := oracle.FeeHistory(t.Context(), c.count, c.last, c.percent)
		backend.teardown()

		expReward := c.expCount
		if len(c.percent) == 0 {
			expReward = 0
		}

		expBaseFee := c.expCount
		if expBaseFee != 0 {
			expBaseFee++
		}

		if first.Uint64() != c.expFirst {
			t.Fatalf("Test case %d: first block mismatch, want %d, got %d", i, c.expFirst, first)
		}

		if len(reward) != expReward {
			t.Fatalf("Test case %d: reward array length mismatch, want %d, got %d", i, expReward, len(reward))
		}

		if len(baseFee) != expBaseFee {
			t.Fatalf("Test case %d: baseFee array length mismatch, want %d, got %d", i, expBaseFee, len(baseFee))
		}

		if len(ratio) != c.expCount {
			t.Fatalf("Test case %d: gasUsedRatio array length mismatch, want %d, got %d", i, c.expCount, len(ratio))
		}
		for _, ratio := range ratio {
			if ratio > 1 {
				t.Fatalf("Test case %d: gasUsedRatio greater than 1, got %f", i, ratio)
			}
		}
		if len(blobRatio) != c.expCount {
			t.Fatalf("Test case %d: blobGasUsedRatio array length mismatch, want %d, got %d", i, c.expCount, len(blobRatio))
		}
		for _, ratio := range blobRatio {
			if ratio > 1 {
				t.Fatalf("Test case %d: blobGasUsedRatio greater than 1, got %f", i, ratio)
			}
		}
		if len(blobBaseFee) != len(baseFee) {
			t.Fatalf("Test case %d: blobBaseFee array length mismatch, want %d, got %d", i, len(baseFee), len(blobBaseFee))
		}
		if err != c.expErr && !errors.Is(err, c.expErr) {
			t.Fatalf("Test case %d: error mismatch, want %v, got %v", i, c.expErr, err)
		}
	}
}
