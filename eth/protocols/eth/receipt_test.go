// Copyright 2024 The go-ethereum Authors
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

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// miniDeriveFields derives the necessary receipt fields to make types.DeriveSha work.
func miniDeriveFields(r *types.Receipt, txType byte) {
	r.Type = txType
	r.Bloom = types.CreateBloom(r)
}

var receiptsTestLogs1 = []*types.Log{{Address: common.Address{1}, Topics: []common.Hash{{1}}}}
var receiptsTestLogs2 = []*types.Log{
	{Address: common.Address{2}, Topics: []common.Hash{{21}, {22}}, Data: []byte{2, 2, 32, 32}},
	{Address: common.Address{3}, Topics: []common.Hash{{31}, {32}}, Data: []byte{3, 3, 32, 32}},
}

var receiptsTests = []struct {
	input []types.ReceiptForStorage
	txs   []*types.Transaction
	root  common.Hash
}{
	{
		input: []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
		txs:   []*types.Transaction{types.NewTx(&types.LegacyTx{})},
	},
	{
		input: []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
		txs:   []*types.Transaction{types.NewTx(&types.DynamicFeeTx{})},
	},
	{
		input: []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
		txs:   []*types.Transaction{types.NewTx(&types.AccessListTx{})},
	},
	{
		input: []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: receiptsTestLogs1}},
		txs:   []*types.Transaction{types.NewTx(&types.LegacyTx{})},
	},
	{
		input: []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: receiptsTestLogs2}},
		txs:   []*types.Transaction{types.NewTx(&types.AccessListTx{})},
	},
}

var stateSyncReceiptsTests = []struct {
	normalReceipts       []types.ReceiptForStorage
	stateSyncReceipt     *types.ReceiptForStorage
	txs                  []*types.Transaction
	rootWithoutStateSync common.Hash
	rootWithStateSync    common.Hash
}{
	// Only state-sync receipt
	{
		normalReceipts:   nil,
		stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 0, Status: 1, Logs: nil, Type: 0},
		txs:              []*types.Transaction{types.NewBorTransaction()},
	},
	// Normal + state-sync receipts with 0 cumulative gas used for state-sync receipt
	{
		normalReceipts:   []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
		stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 0, Status: 1, Logs: nil, Type: 0},
		txs:              []*types.Transaction{types.NewTx(&types.LegacyTx{}), types.NewBorTransaction()},
	},
	// Normal + state-sync receipts with non-zero cumulative gas used for state-sync receipt
	{
		normalReceipts:   []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
		stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 555, Status: 1, Logs: nil, Type: 0},
		txs:              []*types.Transaction{types.NewTx(&types.LegacyTx{}), types.NewBorTransaction()},
	},
	// Multiple normal + state-sync receipts with non-zero cumulative gas used for state-sync receipt
	{
		normalReceipts:   []types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}, {CumulativeGasUsed: 666, Status: 1, Logs: nil}},
		stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 666, Status: 1, Logs: nil, Type: 0},
		txs:              []*types.Transaction{types.NewTx(&types.LegacyTx{}), types.NewBorTransaction()},
	},
}

func init() {
	for i := range receiptsTests {
		// derive basic fields
		for j := range receiptsTests[i].input {
			r := (*types.Receipt)(&receiptsTests[i].input[j])
			txType := receiptsTests[i].txs[j].Type()
			miniDeriveFields(r, txType)
		}
		// compute expected root
		receipts := make(types.Receipts, len(receiptsTests[i].input))
		for j, sr := range receiptsTests[i].input {
			r := types.Receipt(sr)
			receipts[j] = &r
		}
		receiptsTests[i].root = types.DeriveSha(receipts, trie.NewStackTrie(nil))
	}

	// Duplicate tests for pre and post hardfork scenarios.
	stateSyncReceiptsTests = append(stateSyncReceiptsTests, stateSyncReceiptsTests...)
	for i := range stateSyncReceiptsTests {
		// derive basic fields for normal receipts skipping the state-sync receipts
		for j := range stateSyncReceiptsTests[i].normalReceipts {
			r := (*types.Receipt)(&stateSyncReceiptsTests[i].normalReceipts[j])
			txType := stateSyncReceiptsTests[i].txs[j].Type()
			miniDeriveFields(r, txType)
		}
		// compute expected root excluding the state-sync transaction
		receipts := make(types.Receipts, len(stateSyncReceiptsTests[i].normalReceipts))
		for j, sr := range stateSyncReceiptsTests[i].normalReceipts {
			r := types.Receipt(sr)
			receipts[j] = &r
		}
		// Compute the root excluding state-sync receipt (pre hardfork case)
		stateSyncReceiptsTests[i].rootWithoutStateSync = types.DeriveSha(receipts, trie.NewStackTrie(nil))

		// Append the state-sync receipt and compute the root (post hardfork case)
		r := types.Receipt(*stateSyncReceiptsTests[i].stateSyncReceipt)
		receipts = append(receipts, &r)
		stateSyncReceiptsTests[i].rootWithStateSync = types.DeriveSha(receipts, trie.NewStackTrie(nil))
	}
}

func TestReceiptList69(t *testing.T) {
	// Create a mock isStateSyncReceipt function
	isStateSyncReceipt := func(index int) bool {
		return false
	}

	for i, test := range receiptsTests {
		// encode receipts from types.ReceiptForStorage object.
		canonDB, _ := rlp.EncodeToBytes(test.input)

		// encode block body from types object.
		blockBody := types.Body{Transactions: test.txs}
		canonBody, _ := rlp.EncodeToBytes(blockBody)

		// convert from storage encoding to network encoding
		network, _, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{})
		if err != nil {
			t.Fatalf("test[%d]: blockReceiptsToNetwork69 error: %v", i, err)
		}

		// parse as Receipts response list from network encoding
		var rl ReceiptList69
		if err := rlp.DecodeBytes(network, &rl); err != nil {
			t.Fatalf("test[%d]: can't decode network receipts: %v", i, err)
		}
		rlStorageEnc := rl.EncodeForStorage()
		if !bytes.Equal(rlStorageEnc, canonDB) {
			t.Fatalf("test[%d]: re-encoded receipts not equal\nhave: %x\nwant: %x", i, rlStorageEnc, canonDB)
		}
		rlNetworkEnc, _ := rlp.EncodeToBytes(&rl)
		if !bytes.Equal(rlNetworkEnc, network) {
			t.Fatalf("test[%d]: re-encoded network receipt list not equal\nhave: %x\nwant: %x", i, rlNetworkEnc, network)
		}

		// compute root hash from ReceiptList69 and compare.
		responseHash := types.DeriveSha(&rl, trie.NewStackTrie(nil))
		if responseHash != test.root {
			t.Fatalf("test[%d]: wrong root hash from ReceiptList69\nhave: %v\nwant: %v", i, responseHash, test.root)
		}
	}
}

// TestReceiptList69_WithStateSync tests encoding/decoding of ReceiptList69 with state-sync
// receipts included. It tests each case independently and always excludes state-sync receipt
// from root hash calculations.
func TestReceiptList69_WithStateSync(t *testing.T) {
	// The tests tries to replicate behaviour of how the getReceipts query would
	// handle normal and state-sync receipts.
	for i, test := range stateSyncReceiptsTests {
		// Cases are duplicated (for a different test). Skip re-running again.
		if i == len(stateSyncReceiptsTests)/2 {
			break
		}
		// Track existence of bor receipts for encoding
		var isBorReceiptPresent bool

		// Merge both receipts
		var blockReceipts = make([]types.ReceiptForStorage, 0)
		if test.normalReceipts != nil {
			blockReceipts = append(blockReceipts, test.normalReceipts...)
		}
		if test.stateSyncReceipt != nil {
			blockReceipts = append(blockReceipts, *test.stateSyncReceipt)
			isBorReceiptPresent = true
		}

		// isStateSyncReceipt denotes whether a receipt belongs to state-sync transaction or not
		isStateSyncReceipt := func(index int) bool {
			// If bor receipt is present, it will always be at the end of list
			if isBorReceiptPresent && index == len(blockReceipts)-1 {
				return true
			}
			return false
		}

		// encode receipts from types.ReceiptForStorage object.
		canonDB, _ := rlp.EncodeToBytes(blockReceipts)

		// encode block body from types object.
		blockBody := types.Body{Transactions: test.txs}
		canonBody, _ := rlp.EncodeToBytes(blockBody)

		// convert from storage encoding to network encoding
		network, _, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{})
		if err != nil {
			t.Fatalf("test[%d]: blockReceiptsToNetwork69 error: %v", i, err)
		}

		// parse as Receipts response list from network encoding
		var rl ReceiptList69
		if err := rlp.DecodeBytes(network, &rl); err != nil {
			t.Fatalf("test[%d]: can't decode network receipts: %v", i, err)
		}
		rlStorageEnc := rl.EncodeForStorage()
		if !bytes.Equal(rlStorageEnc, canonDB) {
			t.Fatalf("test[%d]: re-encoded receipts not equal\nhave: %x\nwant: %x", i, rlStorageEnc, canonDB)
		}
		rlNetworkEnc, _ := rlp.EncodeToBytes(&rl)
		if !bytes.Equal(rlNetworkEnc, network) {
			t.Fatalf("test[%d]: re-encoded network receipt list not equal\nhave: %x\nwant: %x", i, rlNetworkEnc, network)
		}

		// Exclude the state-sync receipt from root hash calculations
		rl.ExcludeStateSyncReceipt()

		// compute root hash from ReceiptList69 and compare.
		responseHash := types.DeriveSha(&rl, trie.NewStackTrie(nil))
		if responseHash != test.rootWithoutStateSync {
			t.Fatalf("test[%d]: wrong root hash from ReceiptList69\nhave: %v\nwant: %v", i, responseHash, test.rootWithoutStateSync)
		}
	}
}

// TestReceiptList69_WithStateSync_e2e is an end-to-end test which simulates how receipts
// are encoded and decoded in p2p handlers. Instead of testing case by case, it assembles
// all receipts (assuming they belong to different consecutive blocks) into a single p2p
// packet and decodes the whole packet back. It also tests the behaviour of state-sync
// hardfork activation after which state-sync receipts are included in the `ReceiptHash`
// calculation.
func TestReceiptList69_WithStateSync_e2e(t *testing.T) {
	var receipts []rlp.RawValue
	var encodedReceipts [][]byte

	// Assemble all receipts from all cases into a single p2p packet
	for i, test := range stateSyncReceiptsTests {
		// Track existence of bor receipts for encoding
		var isBorReceiptPresent bool

		// Merge both receipts
		var blockReceipts = make([]types.ReceiptForStorage, 0)
		if test.normalReceipts != nil {
			blockReceipts = append(blockReceipts, test.normalReceipts...)
		}
		if test.stateSyncReceipt != nil {
			blockReceipts = append(blockReceipts, *test.stateSyncReceipt)
			isBorReceiptPresent = true
		}

		// isStateSyncReceipt denotes whether a receipt belongs to state-sync transaction or not
		isStateSyncReceipt := func(index int) bool {
			// If bor receipt is present, it will always be at the end of list
			if isBorReceiptPresent && index == len(blockReceipts)-1 {
				return true
			}
			return false
		}

		// encode receipts from types.ReceiptForStorage object.
		canonDB, _ := rlp.EncodeToBytes(blockReceipts)
		encodedReceipts = append(encodedReceipts, canonDB)

		// encode block body from types object.
		blockBody := types.Body{Transactions: test.txs}
		canonBody, _ := rlp.EncodeToBytes(blockBody)

		// convert from storage encoding to network encoding
		network, _, err := blockReceiptsToNetwork69(canonDB, canonBody, isStateSyncReceipt, receiptQueryParams{})
		if err != nil {
			t.Fatalf("test[%d]: blockReceiptsToNetwork69 error: %v", i, err)
		}

		receipts = append(receipts, network)
	}

	// Create a p2p packet containing all receipts (in network69 format).
	// The p2p packet is written to a buffer stream and is read back again
	// in same way via an interface. We can directly use the rlp encode
	// and decode bytes function to replicate same behaviour.
	packet := &ReceiptsRLPPacket{
		RequestId:           0,
		ReceiptsRLPResponse: receipts,
	}
	encodedPacket, _ := rlp.EncodeToBytes(packet)

	// Receiver side which decodes the packet
	res := new(ReceiptsPacket[*ReceiptList69])
	rlp.DecodeBytes(encodedPacket, res)

	// Borrowed from `handleReceipts`
	// Assign temporary hashing buffer to each list item, the same buffer is shared
	// between all receipt list instances.
	buffers := new(receiptListBuffers)
	for i := range res.List {
		res.List[i].setBuffers(buffers)
	}

	// For 8 cases, test 4 pre hardfork and 4 post hardfork scenarios
	borCfg := &params.BorConfig{
		MadhugiriBlock: big.NewInt(4),
	}
	// Simulating the way packet is delivered to the receipt queue
	deliver := func(packet interface{}) (ReceiptsRLPResponse, func(int, *big.Int) common.Hash) {
		return EncodeReceiptsAndPrepareHasher(packet, borCfg)
	}

	receipts, getReceiptListHashes := deliver(res.List)
	if receipts == nil || getReceiptListHashes == nil {
		t.Fatalf("EncodeReceiptsAndPrepareHasher failed, invalid packet")
	}

	// Check if the receipts on the receiver side match with what was sent
	for i, r := range receipts {
		if !bytes.Equal(r, encodedReceipts[i]) {
			t.Fatalf("re-encoded receipts not equal\nhave: %x\nwant: %x", r, encodedReceipts[i])
		}
	}

	// Check if the `ReceiptHash` calculations matches
	for i := 0; i < len(receipts); i++ {
		got := getReceiptListHashes(i, big.NewInt(int64(i)))
		expected := stateSyncReceiptsTests[i].rootWithoutStateSync
		if borCfg.IsMadhugiri(big.NewInt(int64(i))) {
			expected = stateSyncReceiptsTests[i].rootWithStateSync
		}
		if got != expected {
			t.Fatalf("wrong root hash from ReceiptList69\nhave: %v\nwant: %v", got, expected)
		}
	}
}

func TestReceiptList68(t *testing.T) {
	for i, test := range receiptsTests {
		// encode receipts from types.ReceiptForStorage object.
		canonDB, _ := rlp.EncodeToBytes(test.input)

		// encode block body from types object.
		blockBody := types.Body{Transactions: test.txs}
		canonBody, _ := rlp.EncodeToBytes(blockBody)

		// convert from storage encoding to network encoding
		network, err := blockReceiptsToNetwork68(canonDB, canonBody)
		if err != nil {
			t.Fatalf("test[%d]: blockReceiptsToNetwork68 error: %v", i, err)
		}

		// parse as Receipts response list from network encoding
		var rl ReceiptList68
		if err := rlp.DecodeBytes(network, &rl); err != nil {
			t.Fatalf("test[%d]: can't decode network receipts: %v", i, err)
		}
		rlStorageEnc := rl.EncodeForStorage()
		if !bytes.Equal(rlStorageEnc, canonDB) {
			t.Fatalf("test[%d]: re-encoded receipts not equal\nhave: %x\nwant: %x", i, rlStorageEnc, canonDB)
		}
		rlNetworkEnc, _ := rlp.EncodeToBytes(&rl)
		if !bytes.Equal(rlNetworkEnc, network) {
			t.Fatalf("test[%d]: re-encoded network receipt list not equal\nhave: %x\nwant: %x", i, rlNetworkEnc, network)
		}

		// compute root hash from ReceiptList68 and compare.
		responseHash := types.DeriveSha(&rl, trie.NewStackTrie(nil))
		if responseHash != test.root {
			t.Fatalf("test[%d]: wrong root hash from ReceiptList68\nhave: %v\nwant: %v", i, responseHash, test.root)
		}
	}
}

// emptyBodyRLP returns the RLP encoding of an empty block body shaped as
// txTypesInBody expects: an outer list whose first field is the (empty)
// transactions list. Mirrors what GetBodyRLP returns for an empty block.
func emptyBodyRLP() rlp.RawValue {
	// Outer list (0xc2) containing two empty lists (0xc0, 0xc0): [txs, uncles].
	return rlp.RawValue{0xc2, 0xc0, 0xc0}
}

// TestBlockReceiptsToNetwork68_EmptyReceipts validates that in the final
// network encoding in eth/68, empty receipts are handled.
func TestBlockReceiptsToNetwork68_EmptyReceipts(t *testing.T) {
	body := emptyBodyRLP()
	tests := []struct {
		name  string
		input rlp.RawValue
	}{
		{"nil RawValue", nil},
		{"zero-length RawValue", rlp.RawValue{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := blockReceiptsToNetwork68(tc.input, body)
			if err != nil {
				t.Fatalf("expected fallback to canonical empty list, got err: %v", err)
			}
			if !bytes.Equal(out, rlp.EmptyList) {
				t.Fatalf("expected encoded empty list %x, got %x", rlp.EmptyList, out)
			}
		})
	}
}

// TestBlockReceiptsToNetwork68_MalformedInput_ReturnsError ensures that
// error due to invalid input (unparseable blob) is surfaced to the caller.
func TestBlockReceiptsToNetwork68_MalformedInput_ReturnsError(t *testing.T) {
	body := emptyBodyRLP()
	// 0x81 0x02 is an RLP single-byte string (not a list).
	_, err := blockReceiptsToNetwork68(rlp.RawValue{0x81, 0x02}, body)
	if err == nil {
		t.Fatalf("expected error for malformed (non-list) receipts blob, got nil")
	}
}

// TestBlockReceiptsToNetwork69_EmptyReceipts validates that in the final
// network encoding in eth/69, empty receipts are handled.
func TestBlockReceiptsToNetwork69_EmptyReceipts(t *testing.T) {
	body := emptyBodyRLP()
	noStateSync := func(int) bool { return false }
	tests := []struct {
		name  string
		input rlp.RawValue
	}{
		{"nil RawValue", nil},
		{"zero-length RawValue", rlp.RawValue{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := blockReceiptsToNetwork69(tc.input, body, noStateSync, receiptQueryParams{})
			if err != nil {
				t.Fatalf("expected fallback to canonical empty list, got err: %v", err)
			}
			if !bytes.Equal(out, rlp.EmptyList) {
				t.Fatalf("expected encoded empty list %x, got %x", rlp.EmptyList, out)
			}
		})
	}
}

// TestBlockReceiptsToNetwork69_MalformedInput_ReturnsError ensures that
// error due to invalid input (unparseable blob) is surfaced to the caller.
func TestBlockReceiptsToNetwork69_MalformedInput_ReturnsError(t *testing.T) {
	body := emptyBodyRLP()
	noStateSync := func(int) bool { return false }
	_, _, err := blockReceiptsToNetwork69(rlp.RawValue{0x81, 0x02}, body, noStateSync, receiptQueryParams{})
	if err == nil {
		t.Fatalf("expected error for malformed (non-list) receipts blob, got nil")
	}
}
