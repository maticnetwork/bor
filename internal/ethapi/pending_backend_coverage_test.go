package ethapi

import (
	"bytes"
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

type pendingMetadataCoverageBackend struct {
	*testBackend
	block        *types.Block
	receipts     types.Receipts
	blockCalls   int
	receiptCalls int
	parentState  *state.StateDB
}

func (b *pendingMetadataCoverageBackend) PendingBlock() *types.Block {
	b.blockCalls++
	return b.block
}

func (b *pendingMetadataCoverageBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	b.receiptCalls++
	return b.block, b.receipts
}

func (b *pendingMetadataCoverageBackend) PendingParentState(_ context.Context, _ *types.Block) (*state.StateDB, error) {
	return b.parentState, nil
}

func TestPendingBackendCoverage(t *testing.T) {
	transactionAPI, _, _ := setupTransactionsToApiTest(t)
	backend := transactionAPI.b.(*testBackend)
	block := backend.pending
	receipts := backend.pendingReceipts
	metadata := &pendingMetadataCoverageBackend{
		testBackend: backend,
		block:       block,
		receipts:    receipts,
	}
	parentState, _, err := backend.StateAndHeaderByNumber(t.Context(), rpc.BlockNumber(block.NumberU64()-1))
	if err != nil {
		t.Fatalf("parent state: %v", err)
	}
	metadata.parentState = parentState

	if got := pendingBlock(metadata); got != block || metadata.blockCalls != 1 {
		t.Fatalf("metadata pending block = %v, calls = %d", got, metadata.blockCalls)
	}
	gotBlock, gotReceipts := pendingBlockAndReceipts(metadata)
	if gotBlock != block || len(gotReceipts) != len(receipts) || metadata.receiptCalls != 1 {
		t.Fatalf("metadata pending receipts = %v, %v, calls = %d", gotBlock, gotReceipts, metadata.receiptCalls)
	}
	if got := pendingBlock(backend); got != block {
		t.Fatalf("legacy pending block = %v, want %v", got, block)
	}
	gotBlock, gotReceipts = pendingBlockAndReceipts(backend)
	if gotBlock != block || len(gotReceipts) != len(receipts) {
		t.Fatalf("legacy pending receipts = %v, %v", gotBlock, gotReceipts)
	}

	pending := rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)
	blockAPI := NewBlockChainAPI(metadata)
	result, err := blockAPI.GetBlockReceipts(t.Context(), pending)
	if err != nil || len(result) != len(receipts) {
		t.Fatalf("metadata pending block receipts = %v, %v", result, err)
	}
	legacyResult, err := NewBlockChainAPI(backend).GetBlockReceipts(t.Context(), pending)
	if err != nil || len(legacyResult) != len(receipts) {
		t.Fatalf("legacy pending block receipts = %v, %v", legacyResult, err)
	}

	multiBlock := types.NewBlockWithHeader(&types.Header{Number: new(big.Int).Add(block.Number(), big.NewInt(1)), ParentHash: common.HexToHash("0x1")})
	gotParentState, err := pendingParentState(t.Context(), metadata, multiBlock)
	if err != nil || gotParentState != parentState {
		t.Fatalf("multi-block pending parent state = %v, %v", gotParentState, err)
	}
	if _, err := pendingParentState(t.Context(), backend, multiBlock); err == nil {
		t.Fatal("missing multi-block pending parent-state provider did not fail")
	}
	gotParentState, err = pendingParentState(t.Context(), metadata, block)
	if err != nil || gotParentState == nil {
		t.Fatalf("immediate pending parent state = %v, %v", gotParentState, err)
	}

	metadata.block = nil
	metadata.receipts = nil
	if _, err := blockAPI.GetBlockReceipts(t.Context(), pending); err == nil || !strings.Contains(err.Error(), "pending receipts is not available") {
		t.Fatalf("empty pending receipts error = %v", err)
	}
	metadata.block = block
	if _, err := blockAPI.GetBlockReceipts(t.Context(), pending); err == nil || !strings.Contains(err.Error(), "receipts length mismatch") {
		t.Fatalf("mismatched pending receipts error = %v", err)
	}

	tx := block.Transactions()[0]
	backend.preconf.mu.Lock()
	backend.preconf.tx = tx
	backend.preconf.mu.Unlock()
	raw, err := transactionAPI.GetRawTransactionByHash(t.Context(), tx.Hash())
	if err != nil {
		t.Fatalf("raw preconfirmation transaction: %v", err)
	}
	want, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("raw preconfirmation transaction = %x, want %x", raw, want)
	}
}
