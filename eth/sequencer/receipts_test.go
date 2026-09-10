package sequencer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func indexedTx(nonce uint64) *types.Transaction {
	return types.NewTx(&types.LegacyTx{Nonce: nonce})
}

func indexedReceipt(height uint64) *types.Receipt {
	return &types.Receipt{
		BlockNumber: new(big.Int).SetUint64(height),
		Logs:        []*types.Log{{}},
	}
}

// Sealing must hand new receipt copies to future readers while leaving the
// pointers earlier RPC readers hold untouched.
func TestIndexSealSwapsCopiesForReaders(t *testing.T) {
	ix := NewIndex()
	tx := indexedTx(1)

	ix.Add(tx, indexedReceipt(5))

	before, _, ok := ix.Lookup(tx.Hash())
	if !ok {
		t.Fatal("added receipt not found")
	}

	sealedHash := common.Hash{0x5e}
	ix.Seal(5, sealedHash)

	if before.BlockHash != (common.Hash{}) || before.Logs[0].BlockHash != (common.Hash{}) {
		t.Fatal("sealing mutated a receipt an RPC reader already holds")
	}

	after, _, _ := ix.Lookup(tx.Hash())
	if after.BlockHash != sealedHash || after.Logs[0].BlockHash != sealedHash {
		t.Fatalf("sealed lookup carries %s, want %s on receipt and logs", after.BlockHash, sealedHash)
	}
}

// ClearFrom voids re-anchored heights upward; EvictThrough drops imported
// heights downward. Between them a height survives only while speculative.
func TestIndexClearAndEvictBounds(t *testing.T) {
	ix := NewIndex()
	txs := map[uint64]*types.Transaction{}

	for h := uint64(1); h <= 3; h++ {
		txs[h] = indexedTx(h)
		ix.Add(txs[h], indexedReceipt(h))
	}

	ix.ClearFrom(3)
	ix.EvictThrough(1)

	for h, want := range map[uint64]bool{1: false, 2: true, 3: false} {
		if _, _, ok := ix.Lookup(txs[h].Hash()); ok != want {
			t.Fatalf("height %d held=%v, want %v", h, ok, want)
		}
	}
}

// The index stays bounded when canonical imports stall: admitting a new
// height at the cap drops the lowest one.
func TestIndexCapDropsTheLowestHeight(t *testing.T) {
	ix := NewIndex()
	first := indexedTx(0)
	ix.Add(first, indexedReceipt(1))

	for h := uint64(2); h <= maxIndexedHeights; h++ {
		ix.Add(indexedTx(h), indexedReceipt(h))
	}

	over := indexedTx(maxIndexedHeights + 1)
	ix.Add(over, indexedReceipt(maxIndexedHeights+1))

	if _, _, ok := ix.Lookup(first.Hash()); ok {
		t.Fatal("lowest height survived past the cap")
	}

	if _, _, ok := ix.Lookup(over.Hash()); !ok {
		t.Fatal("newly admitted height missing")
	}
}

func TestIndexResetDropsEverything(t *testing.T) {
	ix := NewIndex()
	tx := indexedTx(9)

	ix.Add(tx, indexedReceipt(9))
	ix.Reset()

	if _, _, ok := ix.Lookup(tx.Hash()); ok {
		t.Fatal("reset left a receipt behind")
	}
}

func TestIndexAddBatchIsAllOrNothing(t *testing.T) {
	ix := NewIndex()
	first := indexedTx(10)
	second := indexedTx(11)

	if ix.AddBatch(types.Transactions{first, second}, types.Receipts{indexedReceipt(7)}) {
		t.Fatal("mismatched batch was accepted")
	}
	if _, _, ok := ix.Lookup(first.Hash()); ok {
		t.Fatal("mismatched batch changed the index")
	}

	if ix.AddBatch(types.Transactions{first, second}, types.Receipts{indexedReceipt(7), indexedReceipt(8)}) {
		t.Fatal("cross-height batch was accepted")
	}
	if _, _, ok := ix.Lookup(first.Hash()); ok {
		t.Fatal("cross-height batch changed the index")
	}

	if !ix.AddBatch(types.Transactions{first, second}, types.Receipts{indexedReceipt(7), indexedReceipt(7)}) {
		t.Fatal("valid batch was rejected")
	}
	for _, tx := range (types.Transactions{first, second}) {
		if _, _, ok := ix.Lookup(tx.Hash()); !ok {
			t.Fatalf("batch transaction %s missing", tx.Hash())
		}
	}
}

func TestIndexCountsOnlyMatchingCanonicalTransactions(t *testing.T) {
	ix := NewIndex()
	first := indexedTx(20)
	second := indexedTx(21)
	other := indexedTx(22)
	ix.Add(first, indexedReceipt(9))
	ix.Add(other, indexedReceipt(9))

	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(9)}).WithBody(types.Body{
		Transactions: types.Transactions{first, second},
	})
	if count := ix.CountCanonical(block); count != 1 {
		t.Fatalf("canonical receipt count = %d, want 1", count)
	}
}
