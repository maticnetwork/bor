// Copyright 2026 The go-ethereum Authors
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

package eth

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// encodeBlockFixture renders a state-sync test case into the storage-encoded receipt
// list and block body a serving node would read out of the database, along with the
// predicate identifying the state-sync receipt's position.
func encodeBlockFixture(t *testing.T, index int) (canonDB, canonBody rlp.RawValue, isStateSyncReceipt func(int) bool) {
	t.Helper()

	test := stateSyncReceiptsTests[index]

	var isBorReceiptPresent bool
	blockReceipts := make([]types.ReceiptForStorage, 0)
	if test.normalReceipts != nil {
		blockReceipts = append(blockReceipts, test.normalReceipts...)
	}
	if test.stateSyncReceipt != nil {
		blockReceipts = append(blockReceipts, *test.stateSyncReceipt)
		isBorReceiptPresent = true
	}
	isStateSyncReceipt = func(index int) bool {
		return isBorReceiptPresent && index == len(blockReceipts)-1
	}

	canonDB, err := rlp.EncodeToBytes(blockReceipts)
	if err != nil {
		t.Fatalf("can't encode receipts: %v", err)
	}
	canonBody, err = rlp.EncodeToBytes(types.Body{Transactions: test.txs})
	if err != nil {
		t.Fatalf("can't encode body: %v", err)
	}
	return canonDB, canonBody, isStateSyncReceipt
}

// serveInChunks repeatedly serves one block's receipts under the given size limit,
// following the truncation flag exactly as a peer resuming a partial response would,
// and returns the decoded chunks. It reports false when the limit is too small for
// even the first receipt, which is a legal response rather than a failure.
func serveInChunks(t *testing.T, canonDB, canonBody rlp.RawValue, isStateSyncReceipt func(int) bool, sizeLimit uint64) ([]*ReceiptList69, bool) {
	t.Helper()

	var (
		chunks []*ReceiptList69
		first  uint64
	)
	for round := 0; ; round++ {
		if round > len(canonDB) {
			t.Fatal("chunked serving failed to terminate")
		}
		out, incomplete, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{
			firstIndex: first,
			sizeLimit:  sizeLimit,
		})
		if err != nil {
			t.Fatalf("blockReceiptsToNetwork69 error: %v", err)
		}
		if out == nil {
			if round == 0 {
				return nil, false
			}
			t.Fatal("serving stalled after making progress")
		}
		var rl ReceiptList69
		if err := rlp.DecodeBytes(out, &rl); err != nil {
			t.Fatalf("can't decode chunk: %v", err)
		}
		chunks = append(chunks, &rl)
		if !incomplete {
			return chunks, true
		}
		first += uint64(rl.Len())
	}
}

// reassemble merges chunks the way Peer.bufferReceipts does for a block spanning
// several responses.
func reassemble(chunks []*ReceiptList69) *ReceiptList69 {
	head := chunks[0]
	for _, chunk := range chunks[1:] {
		head.Append(chunk)
	}
	return head
}

// TestPartialReceipts_ChunkedReassembly drives every size limit that splits a
// block, and requires the reassembled list to be indistinguishable from the whole-block
// response. This is the property the eth/70 buffering relies on: partial chunks are only
// bounds-checked, and correctness is established once the block is whole again.
func TestPartialReceipts_ChunkedReassembly(t *testing.T) {
	for i := range stateSyncReceiptsTests {
		canonDB, canonBody, isStateSyncReceipt := encodeBlockFixture(t, i)

		whole, incomplete, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{})
		if err != nil {
			t.Fatalf("test[%d]: unbounded conversion failed: %v", i, err)
		}
		if incomplete {
			t.Fatalf("test[%d]: unbounded conversion reported truncation", i)
		}

		// A single-receipt block has no boundary to split at, so only multi-receipt
		// fixtures are required to exercise the chunking path.
		receiptCount := len(stateSyncReceiptsTests[i].normalReceipts)
		if stateSyncReceiptsTests[i].stateSyncReceipt != nil {
			receiptCount++
		}

		var split int
		for limit := uint64(1); limit <= uint64(len(whole))+8; limit++ {
			chunks, ok := serveInChunks(t, canonDB, canonBody, isStateSyncReceipt, limit)
			if !ok {
				continue
			}
			if len(chunks) > 1 {
				split++
			}
			merged := reassemble(chunks)

			encoded, err := rlp.EncodeToBytes(merged)
			if err != nil {
				t.Fatalf("test[%d] limit %d: can't re-encode: %v", i, limit, err)
			}
			if !bytes.Equal(encoded, whole) {
				t.Fatalf("test[%d] limit %d: reassembled list differs\nhave: %x\nwant: %x", i, limit, encoded, whole)
			}
			if !bytes.Equal(merged.EncodeForStorage(), canonDB) {
				t.Fatalf("test[%d] limit %d: reassembled storage encoding differs", i, limit)
			}
		}
		if receiptCount > 1 && split == 0 {
			t.Fatalf("test[%d]: no size limit ever split the block, test is vacuous", i)
		}
	}
}

// TestPartialReceipts_StateSyncAcrossChunkBoundary is the Bor-specific case.
// The state-sync receipt is identified positionally and always sits last, so a response
// truncated before it must not disturb where it lands after reassembly, nor the derived
// receipt root on either side of the Madhugiri gate.
func TestPartialReceipts_StateSyncAcrossChunkBoundary(t *testing.T) {
	for i, test := range stateSyncReceiptsTests {
		if test.stateSyncReceipt == nil || len(test.normalReceipts) == 0 {
			continue // no boundary to place the split at
		}
		canonDB, canonBody, isStateSyncReceipt := encodeBlockFixture(t, i)

		var split bool
		for limit := uint64(1); limit <= uint64(len(canonDB))+8; limit++ {
			chunks, ok := serveInChunks(t, canonDB, canonBody, isStateSyncReceipt, limit)
			if !ok || len(chunks) < 2 {
				continue
			}
			split = true
			merged := reassemble(chunks)

			// Post-Madhugiri: the state-sync receipt takes part in the receipt root.
			if root := types.DeriveSha(merged, trie.NewStackTrie(nil)); root != test.rootWithStateSync {
				t.Fatalf("test[%d] limit %d: post-Madhugiri root mismatch\nhave: %v\nwant: %v", i, limit, root, test.rootWithStateSync)
			}
			// Pre-Madhugiri: it is excluded, which is a positional decision over the
			// reassembled list and would break if a chunk boundary displaced it.
			merged.ExcludeStateSyncReceipt()
			if root := types.DeriveSha(merged, trie.NewStackTrie(nil)); root != test.rootWithoutStateSync {
				t.Fatalf("test[%d] limit %d: pre-Madhugiri root mismatch\nhave: %v\nwant: %v", i, limit, root, test.rootWithoutStateSync)
			}
		}
		if !split {
			t.Fatalf("test[%d]: block never split across chunks, test is vacuous", i)
		}
	}
}

// TestPartialReceipts_FirstReceiptTooLarge covers the response that cannot make
// any progress: the serving side reports no output rather than an empty truncated list,
// which the caller turns into a finished response.
func TestPartialReceipts_FirstReceiptTooLarge(t *testing.T) {
	canonDB, canonBody, isStateSyncReceipt := encodeBlockFixture(t, 1)

	out, incomplete, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{sizeLimit: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Fatalf("expected no output, got %x", out)
	}
	if incomplete {
		t.Fatal("a response with no progress must not be marked incomplete")
	}
}

// TestPartialReceipts_FirstIndexSkips checks that firstIndex omits exactly the
// receipts already delivered, leaving the tail byte-identical to the whole response.
func TestPartialReceipts_FirstIndexSkips(t *testing.T) {
	canonDB, canonBody, isStateSyncReceipt := encodeBlockFixture(t, 3)

	whole, _, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{})
	if err != nil {
		t.Fatalf("unbounded conversion failed: %v", err)
	}
	var full ReceiptList69
	if err := rlp.DecodeBytes(whole, &full); err != nil {
		t.Fatalf("can't decode full list: %v", err)
	}

	for skip := 0; skip <= full.Len(); skip++ {
		out, incomplete, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{firstIndex: uint64(skip)})
		if err != nil {
			t.Fatalf("skip %d: conversion failed: %v", skip, err)
		}
		if incomplete {
			t.Fatalf("skip %d: unbounded conversion reported truncation", skip)
		}
		var tail ReceiptList69
		if err := rlp.DecodeBytes(out, &tail); err != nil {
			t.Fatalf("skip %d: can't decode tail: %v", skip, err)
		}
		if want := full.Len() - skip; tail.Len() != want {
			t.Fatalf("skip %d: got %d receipts, want %d", skip, tail.Len(), want)
		}
	}
}

func TestReceiptList69_Append(t *testing.T) {
	receipts := []*types.Receipt{
		{CumulativeGasUsed: 100, Status: 1},
		{CumulativeGasUsed: 200, Status: 1},
		{CumulativeGasUsed: 300, Status: 1},
	}
	whole := NewReceiptList69(receipts)

	head := NewReceiptList69(receipts[:1])
	head.Append(NewReceiptList69(receipts[1:]))

	if head.Len() != whole.Len() {
		t.Fatalf("appended list has %d receipts, want %d", head.Len(), whole.Len())
	}
	if !bytes.Equal(head.EncodeForStorage(), whole.EncodeForStorage()) {
		t.Fatal("appended list does not match the whole list")
	}
}

func TestReceiptList69_LogsSize(t *testing.T) {
	for _, test := range []struct {
		name     string
		receipts []*types.Receipt
	}{
		{"empty list", nil},
		{"no logs", []*types.Receipt{{CumulativeGasUsed: 100, Status: 1}}},
		{"single log", []*types.Receipt{{CumulativeGasUsed: 100, Status: 1, Logs: receiptsTestLogs1}}},
		{"many logs", []*types.Receipt{
			{CumulativeGasUsed: 100, Status: 1, Logs: receiptsTestLogs1},
			{CumulativeGasUsed: 200, Status: 1, Logs: receiptsTestLogs2},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rl := NewReceiptList69(test.receipts)
			size, err := rl.LogsSize()
			if err != nil {
				t.Fatalf("LogsSize failed: %v", err)
			}

			var want uint64
			for _, r := range test.receipts {
				encoded, err := rlp.EncodeToBytes(r.Logs)
				if err != nil {
					t.Fatalf("can't encode logs: %v", err)
				}
				content, _, err := rlp.SplitList(encoded)
				if err != nil {
					t.Fatalf("can't split logs: %v", err)
				}
				want += uint64(len(content))
			}
			if size != want {
				t.Fatalf("log size %d, want %d", size, want)
			}
		})
	}
}

// TestValidateLastBlockReceipt covers the bounds applied to a truncated response, which
// are the only checks a partial chunk gets before it is buffered.
func TestValidateLastBlockReceipt(t *testing.T) {
	newList := func(n int, logs []*types.Log) *ReceiptList69 {
		receipts := make([]*types.Receipt, n)
		for i := range receipts {
			receipts[i] = &types.Receipt{CumulativeGasUsed: uint64(i + 1), Status: 1, Logs: logs}
		}
		return NewReceiptList69(receipts)
	}

	for _, test := range []struct {
		name     string
		receipts *ReceiptList69
		gasUsed  uint64
		wantErr  bool
	}{
		{
			// 2 receipts need 42000 gas, and the block reports having spent it.
			name:     "within the gas derived bound",
			receipts: newList(2, nil),
			gasUsed:  2 * params.TxGas,
		},
		{
			// A Bor block carries one receipt more than its gas can account for,
			// because the state-sync transaction is free.
			name:     "state-sync receipt beyond the gas derived bound",
			receipts: newList(3, nil),
			gasUsed:  2 * params.TxGas,
		},
		{
			name:     "more receipts than the block could pay for",
			receipts: newList(4, nil),
			gasUsed:  2 * params.TxGas,
			wantErr:  true,
		},
		{
			// The block's gas could not have paid for this much log data, which costs
			// params.LogDataGas per byte.
			name:     "log data beyond the block gas limit",
			receipts: newList(1, []*types.Log{{Data: make([]byte, params.TxGas/params.LogDataGas+1)}}),
			gasUsed:  params.TxGas,
			wantErr:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &Peer{receiptBuffer: make(map[uint64]*receiptRequest)}

			_, err := p.validateLastBlockReceipt([]*ReceiptList69{test.receipts}, 1, test.gasUsed, 0)
			if test.wantErr && err == nil {
				t.Fatal("expected an error, got none")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateLastBlockReceiptAmsterdamGate pins the fork gate to Bor's block-based
// schedule. Upstream keys the reduced minimum transaction gas off a timestamp; Bor has
// no Amsterdam timestamp, and the fork is dormant, so the pre-fork bound is what a peer
// is held to today.
func TestValidateLastBlockReceiptAmsterdamGate(t *testing.T) {
	// Receipts that only fit under the reduced Amsterdam minimum: 5 receipts against
	// 21000 gas is over 21000/21000+1, but well under 21000/4500+1.
	receipts := make([]*types.Receipt, 5)
	for i := range receipts {
		receipts[i] = &types.Receipt{CumulativeGasUsed: uint64(i + 1), Status: 1}
	}
	list := []*ReceiptList69{NewReceiptList69(receipts)}

	dormant := &params.ChainConfig{LondonBlock: big.NewInt(0)}
	p := &Peer{chainConfig: dormant, receiptBuffer: make(map[uint64]*receiptRequest)}
	if _, err := p.validateLastBlockReceipt(list, 1, params.TxGas, 0); err == nil {
		t.Fatal("expected the pre-Amsterdam bound to reject the response")
	}

	active := &params.ChainConfig{LondonBlock: big.NewInt(0), AmsterdamBlock: big.NewInt(0)}
	p = &Peer{chainConfig: active, receiptBuffer: make(map[uint64]*receiptRequest)}
	if _, err := p.validateLastBlockReceipt(list, 1, params.TxGas, 0); err != nil {
		t.Fatalf("expected the Amsterdam bound to accept the response, got %v", err)
	}
}
