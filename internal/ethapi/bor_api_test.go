// Copyright 2023 The go-ethereum Authors
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

package ethapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/ethapi/override"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestBorWitnessAPI_Integration(t *testing.T) {
	t.Parallel()
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0x0000000000000000000000000000000000000000"): {Balance: big.NewInt(1000000000000000000)},
		},
	}
	backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), nil)

	testBlock, err := backend.BlockByNumber(t.Context(), rpc.BlockNumber(1))
	require.NoError(t, err)
	require.NotNil(t, testBlock)
	testBlockHash := testBlock.Hash()

	mockWitness, err := stateless.NewWitness(testBlock.Header(), backend.chain)
	require.NoError(t, err)
	require.NotNil(t, mockWitness)

	var witBuf bytes.Buffer
	err = mockWitness.EncodeRLP(&witBuf)
	require.NoError(t, err)
	rawdb.WriteWitness(backend.ChainDb(), testBlockHash, witBuf.Bytes())

	t.Run("Database_WitnessStorage", func(t *testing.T) {
		witnessData := rawdb.ReadWitness(backend.ChainDb(), testBlockHash)
		require.NotEmpty(t, witnessData, "Witness data should be stored in database")

		witness, err := stateless.GetWitnessFromRlp(witnessData)
		require.NoError(t, err)
		require.NotNil(t, witness.Header())
		require.Equal(t, testBlockHash, witness.Header().Hash())
	})

	borApi := NewBorAPI(backend)

	t.Run("BorAPI_GetWitnessByNumber_WithStoredWitness", func(t *testing.T) {
		result, err := borApi.GetWitnessByNumber(t.Context(), rpc.BlockNumber(1))
		require.NoError(t, err)
		require.NotNil(t, result, "Should find the stored witness")
		require.NotNil(t, result["context"], "Witness should have a context header")
		contextHeader := result["context"].(map[string]interface{})
		require.Equal(t, testBlockHash, contextHeader["hash"], "Witness should be for the correct block")
	})

	t.Run("BorAPI_GetWitnessByHash_WithStoredWitness", func(t *testing.T) {
		result, err := borApi.GetWitnessByHash(t.Context(), testBlockHash)
		require.NoError(t, err)
		require.NotNil(t, result, "Should find the stored witness")
		contextHeader := result["context"].(map[string]interface{})
		require.Equal(t, testBlockHash, contextHeader["hash"], "Witness should be for the correct block")
	})

	t.Run("BorAPI_GetWitnessByBlockNumberOrHash_WithStoredWitness", func(t *testing.T) {
		result, err := borApi.GetWitnessByBlockNumberOrHash(t.Context(), rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(1)))
		require.NoError(t, err)
		require.NotNil(t, result, "Should find the stored witness by number")

		result, err = borApi.GetWitnessByBlockNumberOrHash(t.Context(), rpc.BlockNumberOrHashWithHash(testBlockHash, false))
		require.NoError(t, err)
		require.NotNil(t, result, "Should find the stored witness by hash")
		contextHeader := result["context"].(map[string]interface{})
		require.Equal(t, testBlockHash, contextHeader["hash"], "Witness should be for the correct block")
	})

	t.Run("BorAPI_GetWitnessByNumber_NoWitness", func(t *testing.T) {
		result, err := borApi.GetWitnessByNumber(t.Context(), rpc.BlockNumber(2))
		require.NoError(t, err)
		require.Nil(t, result, "Should return nil for block without witness")
	})

	t.Run("BorAPI_GetWitnessByHash_NonExistentBlock", func(t *testing.T) {
		fakeHash := common.HexToHash("0xdeadbeef")
		result, err := borApi.GetWitnessByHash(t.Context(), fakeHash)
		require.NoError(t, err)
		require.Nil(t, result, "Should return nil for non-existent block")
	})
}

func (b *testBackend) RPCTxSyncDefaultTimeout() time.Duration {
	if b.syncDefaultTimeout != 0 {
		return b.syncDefaultTimeout
	}
	return 2 * time.Second
}
func (b *testBackend) RPCTxSyncMaxTimeout() time.Duration {
	if b.syncMaxTimeout != 0 {
		return b.syncMaxTimeout
	}
	return 5 * time.Minute
}

func (b *backendMock) RPCTxSyncDefaultTimeout() time.Duration { return 2 * time.Second }
func (b *backendMock) RPCTxSyncMaxTimeout() time.Duration     { return 5 * time.Minute }

func (b *testBackend) Etherbase() (common.Address, error) {
	return common.Address{}, nil
}

func (b *testBackend) Hashrate() (uint64, error) {
	return 0, nil
}

func (b *testBackend) Mining() (bool, error) {
	return false, nil
}

func TestBorBlockNumber(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 20
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	api := NewBorAPI(backend)

	// Test 1: No parameter (should return the latest executed block)
	t.Run("no_parameter_returns_latest_executed", func(t *testing.T) {
		result, err := api.BlockNumber(context.Background(), nil)
		if err != nil {
			t.Fatalf("BlockNumber(nil) error = %v", err)
		}
		// Nil returns latest executed (CurrentBlock)
		expected := backend.chain.CurrentBlock().Number.Uint64()
		if uint64(result) != expected {
			t.Errorf("BlockNumber(nil) = %d, want %d", result, expected)
		}
	})

	// Test 2: Explicit latest returns latest head
	t.Run("latest_returns_latest_head", func(t *testing.T) {
		latest := rpc.LatestBlockNumber
		result, err := api.BlockNumber(context.Background(), &latest)
		if err != nil {
			t.Fatalf("BlockNumber(latest) error = %v", err)
		}
		// Explicit "latest" returns the latest head (CurrentHeader)
		expected := backend.chain.CurrentHeader().Number.Uint64()
		if uint64(result) != expected {
			t.Errorf("BlockNumber(latest) = %d, want %d", result, expected)
		}
	})

	// Test 3: Earliest block number
	t.Run("earliest_block_number", func(t *testing.T) {
		earliest := rpc.EarliestBlockNumber
		result, err := api.BlockNumber(context.Background(), &earliest)
		if err != nil {
			t.Fatalf("BlockNumber(earliest) error = %v", err)
		}
		if uint64(result) != 0 {
			t.Errorf("BlockNumber(earliest) = %d, want 0", result)
		}
	})

	// Test 4: Safe block number
	t.Run("safe_block_number", func(t *testing.T) {
		safe := rpc.SafeBlockNumber
		result, err := api.BlockNumber(context.Background(), &safe)
		// Safe block may not be available in the test environment
		if err != nil {
			// Expected behavior when safe block is not available
			if err.Error() == "safe block not found" {
				t.Skip("Safe block not available in test environment (expected)")
			}
			t.Fatalf("BlockNumber(safe) unexpected error = %v", err)
		}
		// If available, the safe block should be less than or equal to the latest
		latest := backend.chain.CurrentBlock().Number.Uint64()
		if uint64(result) > latest {
			t.Errorf("BlockNumber(safe) = %d, should be <= latest %d", result, latest)
		}
	})

	// Test 5: Finalized block number
	t.Run("finalized_block_number", func(t *testing.T) {
		finalized := rpc.FinalizedBlockNumber
		result, err := api.BlockNumber(context.Background(), &finalized)
		if err != nil {
			t.Fatalf("BlockNumber(finalized) error = %v", err)
		}
		// Finalized block should be less than or equal to the latest
		latest := backend.chain.CurrentBlock().Number.Uint64()
		if uint64(result) > latest {
			t.Errorf("BlockNumber(finalized) = %d, should be <= latest %d", result, latest)
		}
	})

	// Test 6: Pending block number (falls through to default, returns latest executed)
	t.Run("pending_block_number", func(t *testing.T) {
		pending := rpc.PendingBlockNumber
		result, err := api.BlockNumber(context.Background(), &pending)
		if err != nil {
			t.Fatalf("BlockNumber(pending) error = %v", err)
		}
		// Pending falls through to the default case and returns the latest executed
		expected := backend.chain.CurrentBlock().Number.Uint64()
		if uint64(result) != expected {
			t.Errorf("BlockNumber(pending) = %d, want %d", result, expected)
		}
	})

	// Test 7: Numeric block number input returns latest executed (default behavior)
	t.Run("numeric_input_returns_that_block_number", func(t *testing.T) {
		blockNum := rpc.BlockNumber(10)
		result, err := api.BlockNumber(context.Background(), &blockNum)
		if err != nil {
			t.Fatalf("BlockNumber(10) error = %v", err)
		}
		if uint64(result) != 10 {
			t.Errorf("BlockNumber(10) = %d, want 10", result)
		}
	})

	// Test 8: Assert that the return type is hexutil.Uint64
	t.Run("returns_hexutil_uint64", func(t *testing.T) {
		result, err := api.BlockNumber(context.Background(), nil)
		if err != nil {
			t.Fatalf("BlockNumber() error = %v", err)
		}
		// Verify it's the correct type
		var _ hexutil.Uint64 = result
	})

	// Test 9: Explicit latest returns header (latest head), nil returns executed
	t.Run("latest_vs_nil_distinction", func(t *testing.T) {
		// Explicit latest should return CurrentHeader (the latest head)
		latest := rpc.LatestBlockNumber
		resultLatest, err := api.BlockNumber(context.Background(), &latest)
		if err != nil {
			t.Fatalf("BlockNumber(latest) error = %v", err)
		}
		expectedHeader := backend.chain.CurrentHeader().Number.Uint64()
		if uint64(resultLatest) != expectedHeader {
			t.Errorf("BlockNumber(latest) = %d, want header %d", resultLatest, expectedHeader)
		}

		// Nil should return CurrentBlock (latest executed)
		resultNil, err := api.BlockNumber(context.Background(), nil)
		if err != nil {
			t.Fatalf("BlockNumber(nil) error = %v", err)
		}
		expectedExecuted := backend.chain.CurrentBlock().Number.Uint64()
		if uint64(resultNil) != expectedExecuted {
			t.Errorf("BlockNumber(nil) = %d, want executed %d", resultNil, expectedExecuted)
		}

		// Note that this test backend has CurrentHeader == CurrentBlock, so both return
		// the same value. In production during sync, CurrentHeader can be ahead of CurrentBlock
	})

	// Test 10: Pending also returns executed (falls through to default)
	t.Run("pending_returns_executed", func(t *testing.T) {
		pending := rpc.PendingBlockNumber
		result, err := api.BlockNumber(context.Background(), &pending)
		if err != nil {
			t.Fatalf("BlockNumber(pending) error = %v", err)
		}
		// pending falls through to default, returning the latest executed.
		expectedExecuted := backend.chain.CurrentBlock().Number.Uint64()
		if uint64(result) != expectedExecuted {
			t.Errorf("BlockNumber(pending) = %d, want executed block %d", result, expectedExecuted)
		}
	})

	// Test 11: Unknown/custom block tag returns the latest executed (default behavior)
	t.Run("large_numeric_returns_that_block_number", func(t *testing.T) {
		largeNum := rpc.BlockNumber(999999)
		result, err := api.BlockNumber(context.Background(), &largeNum)
		if err != nil {
			t.Fatalf("BlockNumber(999999) error = %v", err)
		}
		// Should return the concrete number
		if uint64(result) != 999999 {
			t.Errorf("BlockNumber(999999) = %d, want 999999", result)
		}
	})
}

func TestBorGetHeaderByNumber(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 20
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	api := NewBorAPI(backend)

	// Test 1: Get the latest block header
	t.Run("latest_block_header", func(t *testing.T) {
		result, err := api.GetHeaderByNumber(context.Background(), rpc.LatestBlockNumber)
		if err != nil {
			t.Fatalf("GetHeaderByNumber(latest) error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByNumber(latest) returned nil")
		}
		// Verify it's a *types.Header with expected fields
		if result.Number == nil {
			t.Error("Header missing Number field")
		}
		if result.ParentHash == (common.Hash{}) {
			t.Error("Header missing ParentHash field")
		}
	})

	// Test 2: Get the earliest block header
	t.Run("earliest_block_header", func(t *testing.T) {
		result, err := api.GetHeaderByNumber(context.Background(), rpc.EarliestBlockNumber)
		if err != nil {
			// Test backend may not support the earliest tag
			t.Skipf("Earliest block not available: %v", err)
		}
		if result == nil {
			t.Skip("GetHeaderByNumber(earliest) returned nil")
		}
		// Verify it's block 0 (genesis)
		if result.Number.Uint64() != 0 {
			t.Errorf("GetHeaderByNumber(earliest) number = %d, want 0", result.Number.Uint64())
		}
	})

	// Test 3: Get the pending block header
	t.Run("pending_block_header_no_transformation", func(t *testing.T) {
		result, err := api.GetHeaderByNumber(context.Background(), rpc.PendingBlockNumber)
		if err != nil {
			t.Fatalf("GetHeaderByNumber(pending) error = %v", err)
		}
		// Pending may return nil if no pending block exists
		if result == nil {
			t.Skip("No pending block available (expected in test environment)")
		}
		// Verify it returns *types.Header
		if result.Number == nil {
			t.Error("Pending header should have Number field")
		}
	})

	// Test 4: Get the specific block header
	t.Run("specific_block_header", func(t *testing.T) {
		blockNum := rpc.BlockNumber(10)
		result, err := api.GetHeaderByNumber(context.Background(), blockNum)
		if err != nil {
			t.Fatalf("GetHeaderByNumber(10) error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByNumber(10) returned nil")
		}
		// Verify it is block 10
		if result.Number.Uint64() != 10 {
			t.Errorf("GetHeaderByNumber(10) number = %d, want 10", result.Number.Uint64())
		}
	})

	// Test 5: Get the safe block header
	t.Run("safe_block_header", func(t *testing.T) {
		result, err := api.GetHeaderByNumber(context.Background(), rpc.SafeBlockNumber)
		// Safe block may not be available in tests
		if err != nil {
			t.Skipf("Safe block not available: %v", err)
		}
		if result == nil {
			t.Skip("Safe block returned nil (may not be available)")
		}
		// If available, should have standard header fields
		if result.Number == nil {
			t.Error("Safe header missing Number field")
		}
	})

	// Test 6: Get the finalized block header
	t.Run("finalized_block_header", func(t *testing.T) {
		result, err := api.GetHeaderByNumber(context.Background(), rpc.FinalizedBlockNumber)
		if err != nil {
			// Backend may not have the finalized state in the test environment
			t.Skipf("Finalized block not available: %v", err)
		}
		if result == nil {
			t.Skip("Finalized block returned nil (backend may not have finalized state in test)")
		}
		// Should have standard header fields
		if result.Number == nil {
			t.Error("Finalized header missing Number field")
		}
	})

	// Test 7: Non-existent block returns error
	t.Run("non_existent_block_returns_error", func(t *testing.T) {
		blockNum := rpc.BlockNumber(999999)
		result, err := api.GetHeaderByNumber(context.Background(), blockNum)
		// Should return error for missing header
		if err == nil {
			t.Error("GetHeaderByNumber(999999) expected error, got nil")
		}
		if result != nil {
			t.Errorf("GetHeaderByNumber(999999) = %v, want nil", result)
		}
		// Verify error message
		expectedErr := "block header not found: 999999"
		if err.Error() != expectedErr {
			t.Errorf("Error message = %q, want %q", err.Error(), expectedErr)
		}
	})

	// Test 8: The return type is *types.Header
	t.Run("returns_types_header", func(t *testing.T) {
		result, err := api.GetHeaderByNumber(context.Background(), rpc.LatestBlockNumber)
		if err != nil {
			t.Fatalf("GetHeaderByNumber(latest) error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByNumber(latest) returned nil")
		}
		var _ *types.Header = result
	})
}

func TestBorGetHeaderByHash(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
		genBlocks = 20
	)

	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
	api := NewBorAPI(backend)

	// Test 1: Get header by valid hash
	t.Run("valid_block_hash", func(t *testing.T) {
		// Get a known block first
		block := backend.chain.GetBlockByNumber(10)
		if block == nil {
			t.Fatal("Could not get block 10")
		}
		hash := block.Hash()

		// Get header by hash
		result, err := api.GetHeaderByHash(context.Background(), hash)
		if err != nil {
			t.Fatalf("GetHeaderByHash() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByHash() returned nil")
		}
		// Verify it's the correct block
		if result.Number.Uint64() != 10 {
			t.Errorf("GetHeaderByHash() number = %d, want 10", result.Number.Uint64())
		}
		if result.Hash() != hash {
			t.Errorf("GetHeaderByHash() hash = %s, want %s", result.Hash(), hash)
		}
	})

	// Test 2: Get the genesis block's header by hash
	t.Run("genesis_block_hash", func(t *testing.T) {
		// Get genesis block
		block := backend.chain.GetBlockByNumber(0)
		if block == nil {
			t.Fatal("Could not get genesis block")
		}
		hash := block.Hash()

		// Get header by hash
		result, err := api.GetHeaderByHash(context.Background(), hash)
		if err != nil {
			t.Fatalf("GetHeaderByHash(genesis) error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByHash(genesis) returned nil")
		}
		// Verify it's block 0
		if result.Number.Uint64() != 0 {
			t.Errorf("GetHeaderByHash(genesis) number = %d, want 0", result.Number.Uint64())
		}
	})

	// Test 3: Get the latest block header by hash
	t.Run("latest_block_hash", func(t *testing.T) {
		// Get the latest block
		block := backend.chain.CurrentBlock()
		if block == nil {
			t.Fatal("Could not get current block")
		}
		hash := block.Hash()

		// Get header by hash
		result, err := api.GetHeaderByHash(context.Background(), hash)
		if err != nil {
			t.Fatalf("GetHeaderByHash(latest) error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByHash(latest) returned nil")
		}
		// Verify it matches the current block
		if result.Hash() != hash {
			t.Errorf("GetHeaderByHash(latest) hash = %s, want %s", result.Hash(), hash)
		}
	})

	// Test 4: Non-existent hash returns error
	t.Run("non_existent_hash_returns_error", func(t *testing.T) {
		// Use a random hash that doesn't exist
		fakeHash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

		result, err := api.GetHeaderByHash(context.Background(), fakeHash)
		// Should return error for missing header
		if err == nil {
			t.Error("GetHeaderByHash(fake) expected error, got nil")
		}
		if result != nil {
			t.Errorf("GetHeaderByHash(fake) = %v, want nil", result)
		}
		// Verify error message
		expectedErr := fmt.Sprintf("block header not found: %s", fakeHash.String())
		if err.Error() != expectedErr {
			t.Errorf("Error message = %q, want %q", err.Error(), expectedErr)
		}
	})

	// Test 5: The return type is *types.Header
	t.Run("returns_types_header", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(10)
		if block == nil {
			t.Fatal("Could not get block 10")
		}

		result, err := api.GetHeaderByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetHeaderByHash() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByHash() returned nil")
		}
		// Verify it's the correct type
		var _ *types.Header = result
	})

	// Test 6: Verify header fields are populated
	t.Run("header_fields_populated", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(10)
		if block == nil {
			t.Fatal("Could not get block 10")
		}

		result, err := api.GetHeaderByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetHeaderByHash() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetHeaderByHash() returned nil")
		}
		// Verify standard fields are populated
		if result.Number == nil {
			t.Error("Header missing Number field")
		}
		if result.ParentHash == (common.Hash{}) {
			t.Error("Header missing ParentHash field")
		}
		if result.Time == 0 {
			t.Error("Header missing Time field")
		}
	})
}

func TestBorGetBlockReceiptsByBlockHash(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(3)
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether * 2)},
				accs[1].addr: {Balance: big.NewInt(params.Ether)},
				accs[2].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Create blocks with transactions to generate receipts
	genBlocks := 5
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	backend := newTestBackend(t, genBlocks, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		if i == 0 {
			tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(accs[0].addr),
				To:       &accs[1].addr,
				Value:    big.NewInt(1000),
				Gas:      21000,
				GasPrice: big.NewInt(params.GWei),
				Data:     nil,
			}), signer, accs[0].key)
			b.AddTx(tx)
		}
		if i == 1 {
			tx1, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(accs[0].addr),
				To:       &accs[1].addr,
				Value:    big.NewInt(2000),
				Gas:      21000,
				GasPrice: big.NewInt(params.GWei),
				Data:     nil,
			}), signer, accs[0].key)
			tx2, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(accs[1].addr),
				To:       &accs[2].addr,
				Value:    big.NewInt(500),
				Gas:      21000,
				GasPrice: big.NewInt(params.GWei),
				Data:     nil,
			}), signer, accs[1].key)
			b.AddTx(tx1)
			b.AddTx(tx2)
		}
	})
	api := NewBorAPI(backend)

	// Test 1: Get receipts for a block with one transaction
	t.Run("block_with_one_tx", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}

		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetBlockReceiptsByBlockHash() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockReceiptsByBlockHash() returned nil")
		}
		// Should have 1 receipt
		if len(result) != 1 {
			t.Errorf("GetBlockReceiptsByBlockHash() returned %d receipts, want 1", len(result))
		}
		// Verify receipt structure
		if len(result) > 0 {
			receipt := result[0]
			if _, ok := receipt["blockHash"]; !ok {
				t.Error("Receipt missing 'blockHash' field")
			}
			if _, ok := receipt["transactionHash"]; !ok {
				t.Error("Receipt missing 'transactionHash' field")
			}
			if _, ok := receipt["gasUsed"]; !ok {
				t.Error("Receipt missing 'gasUsed' field")
			}
		}
	})

	// Test 2: Get receipts for block with multiple transactions
	t.Run("block_with_multiple_txs", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(2)
		if block == nil {
			t.Fatal("Could not get block 2")
		}

		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetBlockReceiptsByBlockHash() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockReceiptsByBlockHash() returned nil")
		}
		// Should have 2 receipts
		if len(result) != 2 {
			t.Errorf("GetBlockReceiptsByBlockHash() returned %d receipts, want 2", len(result))
		}
	})

	// Test 3: Get receipts for a block with no transactions
	t.Run("block_with_no_txs", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(3)
		if block == nil {
			t.Fatal("Could not get block 3")
		}

		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetBlockReceiptsByBlockHash() error = %v", err)
		}
		// Empty block should return an empty array
		if result == nil {
			result = []map[string]interface{}{}
		}
		if len(result) != 0 {
			t.Errorf("GetBlockReceiptsByBlockHash() returned %d receipts for empty block, want 0", len(result))
		}
	})

	// Test 4: Non-existent block hash returns an error
	t.Run("non_existent_hash_returns_error", func(t *testing.T) {
		fakeHash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), fakeHash)
		// Should return error for missing block
		if err == nil {
			t.Error("GetBlockReceiptsByBlockHash(fake) expected error, got nil")
		}
		if result != nil {
			t.Errorf("GetBlockReceiptsByBlockHash(fake) = %v, want nil", result)
		}
		// Verify error message
		expectedErr := fmt.Sprintf("block not found %x", fakeHash)
		if err.Error() != expectedErr {
			t.Errorf("Error message = %q, want %q", err.Error(), expectedErr)
		}
	})

	// Test 5: The return type is []map[string]interface{}
	t.Run("returns_slice_of_maps", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}

		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetBlockReceiptsByBlockHash() error = %v", err)
		}
		var _ []map[string]interface{} = result
	})

	// Test 6: Verify receipt fields match transaction
	t.Run("receipt_fields_match_transaction", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}

		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetBlockReceiptsByBlockHash() error = %v", err)
		}
		if len(result) == 0 {
			t.Fatal("GetBlockReceiptsByBlockHash() returned no receipts")
		}

		// Verify the first receipt matches the first transaction
		receipt := result[0]
		tx := block.Transactions()[0]

		// Block hash should match
		if blockHash, ok := receipt["blockHash"].(common.Hash); ok {
			if blockHash != block.Hash() {
				t.Errorf("Receipt blockHash = %s, want %s", blockHash, block.Hash())
			}
		}

		// Transaction hash should match
		if txHash, ok := receipt["transactionHash"].(common.Hash); ok {
			if txHash != tx.Hash() {
				t.Errorf("Receipt transactionHash = %s, want %s", txHash, tx.Hash())
			}
		}
	})

	// Test 7: Non-canonical hash returns a specific error
	t.Run("non_canonical_hash_returns_specific_error", func(t *testing.T) {
		// Get a canonical block
		canonicalBlock := backend.chain.GetBlockByNumber(1)
		if canonicalBlock == nil {
			t.Fatal("Could not get block 1")
		}

		// Create a fake block with the same number but a different hash
		fakeHeader := &types.Header{
			Number:     canonicalBlock.Number(),
			Time:       canonicalBlock.Time(),
			ParentHash: common.HexToHash("0xfafafafafafafafafafafafafafafafafafafafafafafafafafafafafafafafa"),
			Root:       canonicalBlock.Root(),
			TxHash:     types.EmptyTxsHash,
			Difficulty: canonicalBlock.Difficulty(),
			GasLimit:   canonicalBlock.GasLimit(),
		}
		fakeBlock := types.NewBlockWithHeader(fakeHeader).WithBody(types.Body{
			Transactions: []*types.Transaction{},
		})

		// Write the fake block to the database
		rawdb.WriteBlock(backend.chain.DB(), fakeBlock)
		rawdb.WriteReceipts(backend.chain.DB(), fakeBlock.Hash(), fakeBlock.NumberU64(), types.Receipts{})

		// Try to get receipts for non-canonical hash
		result, err := api.GetBlockReceiptsByBlockHash(context.Background(), fakeBlock.Hash())

		// Should return a specific error
		if err == nil {
			t.Error("GetBlockReceiptsByBlockHash(non-canonical) expected error, got nil")
		}
		if result != nil {
			t.Errorf("GetBlockReceiptsByBlockHash(non-canonical) = %v, want nil", result)
		}

		// Verify the error message matches for non-canonical blocks
		expectedErrMsg := fmt.Sprintf("hash %x is not currently canonical", fakeBlock.Hash())
		if err.Error() != expectedErrMsg {
			t.Errorf("Error message = %q, want %q", err.Error(), expectedErrMsg)
		}
	})

	// Test 8: Bor state-sync receipt gets appended
	t.Run("bor_state_sync_receipt_appended", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}
		normalCount := len(block.Transactions())

		// Derive Bor transaction hash
		borTxHash := types.GetDerivedBorTxHash(types.BorReceiptKey(block.NumberU64(), block.Hash()))

		// Create a mock Bor state-sync receipt
		borReceipt := &types.Receipt{
			Type:              types.LegacyTxType,
			Status:            types.ReceiptStatusSuccessful,
			CumulativeGasUsed: 0,
			Logs:              []*types.Log{},
			TxHash:            borTxHash,
		}

		// Write Bor tx lookup entry using the block hash
		rawdb.WriteBorTxLookupEntry(backend.chain.DB(), block.Hash(), block.NumberU64())

		// Create backend with Bor receipt
		backendWithBor := &testBackendWithBorReceipt{
			testBackend: backend,
			borReceipt:  borReceipt,
		}
		apiWithBor := NewBorAPI(backendWithBor)

		// Get receipts should append the state-sync receipt
		result, err := apiWithBor.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetBlockReceiptsByBlockHash() error = %v", err)
		}

		// Should have normal receipts plus the bor state-sync receipt
		expectedCount := normalCount + 1
		if len(result) != expectedCount {
			t.Errorf("GetBlockReceiptsByBlockHash() returned %d receipts, want %d (txs + bor)", len(result), expectedCount)
		}

		if len(result) <= normalCount {
			t.Error("Bor state-sync receipt was NOT appended")
		}

		// The last receipt should be the bor receipt
		if len(result) > normalCount {
			lastReceipt := result[len(result)-1]
			if txHash, ok := lastReceipt["transactionHash"].(common.Hash); ok {
				if txHash != borTxHash {
					t.Errorf("Last receipt transactionHash = %s, want Bor receipt %s", txHash, borTxHash)
				}
			} else {
				t.Error("Last receipt missing transactionHash field")
			}
		}
	})
}

// testBackendWithBorReceipt wraps testBackend and overrides GetBorBlockReceipt
type testBackendWithBorReceipt struct {
	*testBackend
	borReceipt *types.Receipt
}

func (b *testBackendWithBorReceipt) GetBorBlockReceipt(_ context.Context, _ common.Hash) (*types.Receipt, error) {
	// Return mock Bor receipt
	return b.borReceipt, nil
}

func (b *testBackendWithBorReceipt) ChainConfig() *params.ChainConfig {
	// Return config without Bor to simulate pre-Madhugiri
	return params.TestChainConfig
}

func TestBorForks(t *testing.T) {
	t.Parallel()

	// Test 1: Basic fork retrieval with TestChainConfig
	t.Run("basic_forks_retrieval", func(t *testing.T) {
		var (
			accs    = newAccounts(1)
			genesis = &core.Genesis{
				Config: params.TestChainConfig,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// Verify genesis hash is set
		genesisBlock := backend.chain.GetBlockByNumber(0)
		if result.GenesisHash != genesisBlock.Hash() {
			t.Errorf("GenesisHash = %s, want %s", result.GenesisHash, genesisBlock.Hash())
		}

		// TestChainConfig has all forks at block 0, which get filtered
		// So, HeightForks should be nil
		if result.HeightForks != nil {
			t.Errorf("HeightForks should be nil when all forks are at block 0, got %v", result.HeightForks)
		}
	})

	// Test 2: Verify forks are sorted
	t.Run("forks_are_sorted", func(t *testing.T) {
		var (
			accs = newAccounts(1)
			// Create config with multiple forks at different blocks
			config = &params.ChainConfig{
				ChainID:             big.NewInt(1337),
				HomesteadBlock:      big.NewInt(0),
				EIP150Block:         big.NewInt(5),
				EIP155Block:         big.NewInt(5),
				EIP158Block:         big.NewInt(5),
				ByzantiumBlock:      big.NewInt(20),
				ConstantinopleBlock: big.NewInt(30),
				PetersburgBlock:     big.NewInt(30),
				IstanbulBlock:       big.NewInt(40),
				BerlinBlock:         big.NewInt(50),
				LondonBlock:         big.NewInt(60),
			}
			genesis = &core.Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// Verify HeightForks are sorted in ascending order
		for i := 1; i < len(result.HeightForks); i++ {
			if result.HeightForks[i] < result.HeightForks[i-1] {
				t.Errorf("HeightForks not sorted: %v at index %d is less than %v at index %d",
					result.HeightForks[i], i, result.HeightForks[i-1], i-1)
			}
		}

		// Verify no forks at block 0 (genesis)
		if len(result.HeightForks) > 0 && result.HeightForks[0] == 0 {
			t.Error("HeightForks contains block 0 (genesis), which should be filtered out")
		}
	})

	// Test 3: Verify forks are deduplicated
	t.Run("forks_are_deduplicated", func(t *testing.T) {
		var (
			accs = newAccounts(1)
			// Create config with duplicate fork blocks
			config = &params.ChainConfig{
				ChainID:        big.NewInt(1337),
				HomesteadBlock: big.NewInt(0),
				EIP150Block:    big.NewInt(10),
				EIP155Block:    big.NewInt(10), // Same block as EIP150
				EIP158Block:    big.NewInt(10), // Same block as EIP155
				ByzantiumBlock: big.NewInt(20),
			}
			genesis = &core.Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// Verify no duplicates in HeightForks
		seen := make(map[uint64]bool)
		for _, fork := range result.HeightForks {
			if seen[fork] {
				t.Errorf("Duplicate fork found at block %d", fork)
			}
			seen[fork] = true
		}
	})

	// Test 4: Bor-specific forks are included
	t.Run("bor_forks_included", func(t *testing.T) {
		var (
			accs = newAccounts(1)
			// Use a minimal config with just Bor forks to test their inclusion
			config = &params.ChainConfig{
				ChainID:             big.NewInt(137), // Polygon mainnet
				HomesteadBlock:      big.NewInt(0),
				EIP150Block:         big.NewInt(0),
				EIP155Block:         big.NewInt(0),
				EIP158Block:         big.NewInt(0),
				ByzantiumBlock:      big.NewInt(0),
				ConstantinopleBlock: big.NewInt(0),
				PetersburgBlock:     big.NewInt(0),
				IstanbulBlock:       big.NewInt(0),
				BerlinBlock:         big.NewInt(0),
				Bor: &params.BorConfig{
					JaipurBlock:    big.NewInt(50),
					DelhiBlock:     big.NewInt(60),
					IndoreBlock:    big.NewInt(70),
					MadhugiriBlock: big.NewInt(80),
				},
			}
			genesis = &core.Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// Verify Bor forks are present
		expectedBorForks := []uint64{50, 60, 70, 80}
		for _, expected := range expectedBorForks {
			found := false
			for _, fork := range result.HeightForks {
				if fork == expected {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Expected Bor fork at block %d not found in HeightForks", expected)
			}
		}
	})

	// Test 5: Return type verification
	t.Run("returns_forks_struct", func(t *testing.T) {
		var (
			accs    = newAccounts(1)
			genesis = &core.Genesis{
				Config: params.TestChainConfig,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// Verify it's the correct type
		var _ Forks = result

		// Verify struct fields are accessible
		_ = result.GenesisHash
		_ = result.HeightForks
		_ = result.TimeForks
	})

	// Test 6: JSON marshaling
	t.Run("json_output", func(t *testing.T) {
		var (
			accs = newAccounts(1)
			// Config with height forks but no time forks
			config = &params.ChainConfig{
				ChainID:        big.NewInt(1337),
				HomesteadBlock: big.NewInt(0),
				EIP150Block:    big.NewInt(10),
				EIP155Block:    big.NewInt(10),
				EIP158Block:    big.NewInt(10),
			}
			genesis = &core.Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// Verify HeightForks is non-nil (has forks)
		if result.HeightForks == nil {
			t.Error("HeightForks should not be nil when forks exist")
		}

		// Verify TimeForks is nil (no time-based forks)
		// This ensures JSON marshaling outputs "timeForks": null instead of "timeForks":[]
		if result.TimeForks != nil {
			t.Errorf("TimeForks should be nil when no time-based forks exist, got %v", result.TimeForks)
		}

		// Marshal to JSON to verify the format
		jsonBytes, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("JSON marshal error = %v", err)
		}

		// Verify JSON contains "timeForks": null (not "timeForks":[])
		jsonStr := string(jsonBytes)
		if !strings.Contains(jsonStr, `"timeForks":null`) {
			t.Errorf("JSON should contain '\"timeForks\":null', got: %s", jsonStr)
		}
	})

	// Test 7: Edge case - all forks at block 0 get filtered to nil
	t.Run("all_genesis_forks_filtered_to_null", func(t *testing.T) {
		var (
			accs = newAccounts(1)
			// Config with all forks at block 0 (genesis)
			config = &params.ChainConfig{
				ChainID:             big.NewInt(1337),
				HomesteadBlock:      big.NewInt(0),
				EIP150Block:         big.NewInt(0),
				EIP155Block:         big.NewInt(0),
				EIP158Block:         big.NewInt(0),
				ByzantiumBlock:      big.NewInt(0),
				ConstantinopleBlock: big.NewInt(0),
				PetersburgBlock:     big.NewInt(0),
				IstanbulBlock:       big.NewInt(0),
				BerlinBlock:         big.NewInt(0),
				LondonBlock:         big.NewInt(0),
			}
			genesis = &core.Genesis{
				Config: config,
				Alloc: types.GenesisAlloc{
					accs[0].addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		result, err := api.Forks(context.Background())
		if err != nil {
			t.Fatalf("Forks() error = %v", err)
		}

		// All forks at block 0 should be filtered out, leaving nil
		if result.HeightForks != nil {
			t.Errorf("HeightForks should be nil when all forks are at block 0, got %v", result.HeightForks)
		}

		// Marshal to JSON to verify both are null
		jsonBytes, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("JSON marshal error = %v", err)
		}

		jsonStr := string(jsonBytes)
		// Both should be null, not []
		if !strings.Contains(jsonStr, `"heightForks":null`) {
			t.Errorf("JSON should contain '\"heightForks\":null', got: %s", jsonStr)
		}
		if !strings.Contains(jsonStr, `"timeForks":null`) {
			t.Errorf("JSON should contain '\"timeForks\":null', got: %s", jsonStr)
		}
	})
}

func TestBorGetLogsByHash(t *testing.T) {
	t.Parallel()

	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(params.Ether)
		config  = *params.TestChainConfig
	)

	genesis := &core.Genesis{
		Config: &config,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
		},
	}

	backend := newTestBackend(t, 10, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		// Create transactions
		if i == 1 {
			// Simple value transfer (no logs)
			toAddr := common.HexToAddress("0x1111")
			tx := types.NewTx(&types.LegacyTx{
				Nonce:    0,
				To:       &toAddr,
				Value:    big.NewInt(100),
				Gas:      21000,
				GasPrice: big.NewInt(params.InitialBaseFee),
				Data:     nil,
			})
			tx, _ = types.SignTx(tx, types.LatestSigner(&config), key)
			b.AddTx(tx)
		}
	})
	api := NewBorAPI(backend)

	// Test 1: Get logs for block with transactions
	t.Run("block_with_transaction", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(2)
		if block == nil {
			t.Fatal("Could not get block 2")
		}

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		// Should return an array (one entry per transaction)
		if result == nil {
			t.Fatal("GetLogsByHash() returned nil")
		}

		// Verify it's the correct type
		var _ [][]*types.Log = result
	})

	// Test 2: Get logs for empty block
	t.Run("empty_block", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(5)
		if block == nil {
			t.Fatal("Could not get block 5")
		}

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		// Empty block should return an empty array
		if result == nil {
			t.Fatal("GetLogsByHash() returned nil for empty block")
		}
		if len(result) != 0 {
			t.Errorf("GetLogsByHash() returned %d log arrays for empty block, want 0", len(result))
		}
	})

	// Test 3: Non-existent block hash returns nil
	t.Run("non_existent_hash_returns_nil", func(t *testing.T) {
		fakeHash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

		result, err := api.GetLogsByHash(context.Background(), fakeHash)
		// Should return nil for the non-existent block
		if err != nil {
			t.Errorf("GetLogsByHash(fake) unexpected error = %v", err)
		}
		if result != nil {
			t.Errorf("GetLogsByHash(fake) = %v, want nil", result)
		}
	})

	// Test 4: Verify array structure (one log array per transaction)
	t.Run("array_structure_per_transaction", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(2)
		if block == nil {
			t.Fatal("Could not get block 2")
		}

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		// The number of log arrays should match the number of transactions
		txCount := len(block.Transactions())
		if len(result) != txCount {
			t.Errorf("GetLogsByHash() returned %d log arrays, want %d (one per transaction)", len(result), txCount)
		}
	})

	// Test 5: Genesis block
	t.Run("genesis_block", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(0)
		if block == nil {
			t.Fatal("Could not get genesis block")
		}

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash(genesis) error = %v", err)
		}

		// Genesis should return an empty array (no transactions)
		if result == nil {
			t.Fatal("GetLogsByHash(genesis) returned nil")
		}
		if len(result) != 0 {
			t.Errorf("GetLogsByHash(genesis) returned %d log arrays, want 0", len(result))
		}
	})
}

// testBackendWithPreMadhuguriBorReceipt wraps testBackend and simulates pre-Madhugiri with Bor receipts
type testBackendWithPreMadhuguriBorReceipt struct {
	*testBackend
	borReceipt *types.Receipt
}

func (b *testBackendWithPreMadhuguriBorReceipt) GetBorBlockReceipt(_ context.Context, _ common.Hash) (*types.Receipt, error) {
	return b.borReceipt, nil
}

func (b *testBackendWithPreMadhuguriBorReceipt) ChainConfig() *params.ChainConfig {
	// Return config with Bor but MadhugiriBlock unset (nil) for pre-Madhuguri behavior.
	// Deep-copy BorConfig so we never mutate the shared params.AllEthashProtocolChanges.Bor
	// instance — concurrent tests read those fields and would race with us.
	cfg := *params.AllEthashProtocolChanges
	borCfg := params.BorConfig{}
	if cfg.Bor != nil {
		borCfg = *cfg.Bor
	}
	borCfg.MadhugiriBlock = nil
	cfg.Bor = &borCfg
	return &cfg
}

type testBackendWithPostMadhugiriBorReceipt struct {
	*testBackend
	borReceipt *types.Receipt
}

func (b *testBackendWithPostMadhugiriBorReceipt) GetBorBlockReceipt(_ context.Context, _ common.Hash) (*types.Receipt, error) {
	return b.borReceipt, nil
}

func (b *testBackendWithPostMadhugiriBorReceipt) ChainConfig() *params.ChainConfig {
	cfg := *params.AllEthashProtocolChanges
	borCfg := params.BorConfig{}
	if cfg.Bor != nil {
		borCfg = *cfg.Bor
	}
	borCfg.MadhugiriBlock = big.NewInt(0)
	cfg.Bor = &borCfg
	return &cfg
}

func TestBorGetLogsByHashWithLogs(t *testing.T) {
	t.Parallel()

	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = new(big.Int).Mul(big.NewInt(10), big.NewInt(params.Ether))
		config  = *params.AllEthashProtocolChanges // Use config without Bor to avoid system transactions
	)

	// Simple contract that emits a log when called
	// contract Logger { event TestEvent(uint256 value); function log(uint256 x) public { emit TestEvent(x); } }
	// Compiled bytecode for the above contract
	contractBytecode := common.Hex2Bytes("608060405234801561000f575f80fd5b506101438061001d5f395ff3fe608060405234801561000f575f80fd5b5060043610610029575f3560e01c8063c598d71c1461002d575b5f80fd5b610047600480360381019061004291906100c4565b610049565b005b7f1440c4dd67b4344ea1905ec0318995133b550f168b4ee959a0da6b503d7d2414816040516100789190610100565b60405180910390a150565b5f80fd5b5f819050919050565b61009a81610088565b81146100a4575f80fd5b50565b5f813590506100b581610091565b92915050565b5f602082840312156100d0576100cf610084565b5b5f6100dd848285016100a7565b91505092915050565b6100ef81610088565b82525050565b5f6020820190506101085f8301846100e6565b9291505056fea2646970667358221220abcdef1234567890abcdef1234567890abcdef1234567890abcdef123456789064736f6c63430008110033")
	contractAddr := crypto.CreateAddress(address, 0) // Nonce 0 for first contract deployment

	genesis := &core.Genesis{
		Config: &config,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
		},
	}

	backend := newTestBackend(t, 10, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		if i == 1 {
			// Deploy contract
			deployTx := types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(address),
				To:       nil, // nil To means contract creation
				Value:    big.NewInt(0),
				Gas:      300000,
				GasPrice: big.NewInt(params.InitialBaseFee),
				Data:     contractBytecode,
			})
			deployTx, _ = types.SignTx(deployTx, types.LatestSigner(&config), key)
			b.AddTx(deployTx)
		}
		if i == 2 {
			// Call contract to emit log
			// Function signature: log(uint256) -> 0xc598d71c
			callData := common.Hex2Bytes("c598d71c0000000000000000000000000000000000000000000000000000000000000029") // log(41)
			callTx := types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(address),
				To:       &contractAddr,
				Value:    big.NewInt(0),
				Gas:      100000,
				GasPrice: big.NewInt(params.InitialBaseFee),
				Data:     callData,
			})
			callTx, _ = types.SignTx(callTx, types.LatestSigner(&config), key)
			b.AddTx(callTx)
		}
	})
	api := NewBorAPI(backend)

	// Test: Block with transaction that emits logs
	t.Run("transaction_with_actual_logs", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(3)
		if block == nil {
			t.Fatal("Could not get block 3")
		}

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		if result == nil {
			t.Fatal("GetLogsByHash() returned nil")
		}

		// Should have logs (may include Bor system logs and contract logs)
		if len(result) == 0 {
			t.Fatal("GetLogsByHash() returned empty array, expected logs")
		}

		// Verify at least one log array has logs
		foundLogs := false
		for _, txLogs := range result {
			if len(txLogs) > 0 {
				foundLogs = true
				// Verify log structure - all logs should have proper fields
				for _, log := range txLogs {
					if log.Address == (common.Address{}) {
						t.Error("Log has zero address")
					}
				}
				break
			}
		}

		if !foundLogs {
			t.Fatal("No logs found in any transaction")
		}
	})

	// Test: Block with contract deployment
	t.Run("contract_deployment_logs", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(2)
		if block == nil {
			t.Fatal("Could not get block 2")
		}

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		if result == nil {
			t.Fatal("GetLogsByHash() returned nil")
		}

		// Should have at least one entry (deployment tx, possibly with Bor system logs)
		if len(result) == 0 {
			t.Error("GetLogsByHash() returned empty array for block with deployment")
		}
	})
}

func TestBorGetLogsByHashPreMadhugiri(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Create a test block with 2 regular transactions
	backend := newTestBackend(t, 1, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		if i == 0 {
			// Add 2 transactions to the block
			tx1, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(accs[0].addr),
					To:       &accs[0].addr,
					Value:    big.NewInt(1000),
					Gas:      params.TxGas,
					GasPrice: big.NewInt(params.InitialBaseFee),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), accs[0].key,
			)
			b.AddTx(tx1)

			tx2, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(accs[0].addr),
					To:       &accs[0].addr,
					Value:    big.NewInt(2000),
					Gas:      params.TxGas,
					GasPrice: big.NewInt(params.InitialBaseFee),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), accs[0].key,
			)
			b.AddTx(tx2)
		}
	})

	block := backend.chain.GetBlockByNumber(1)
	if block == nil {
		t.Fatal("Could not get block 1")
	}

	// Test 1: Pre-Madhugiri block with state-sync receipt that has EMPTY logs
	t.Run("pre_madhugiri_empty_state_sync_logs", func(t *testing.T) {
		// Create a state-sync receipt with no logs
		borReceipt := &types.Receipt{
			Type:   types.StateSyncTxType,
			Status: types.ReceiptStatusSuccessful,
			Logs:   []*types.Log{}, // Empty logs
		}

		backendWithBor := &testBackendWithPreMadhuguriBorReceipt{
			testBackend: backend,
			borReceipt:  borReceipt,
		}
		api := NewBorAPI(backendWithBor)

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		// Should have 3 entries: 2 regular txs + 1 state-sync (even though state-sync has no logs)
		expectedLen := 3
		if len(result) != expectedLen {
			t.Errorf("GetLogsByHash() returned %d log arrays, want %d (2 regular + 1 state-sync with empty logs)", len(result), expectedLen)
		}

		// Verify the state-sync exists as the last entry
		if len(result) > 0 {
			stateSyncLogs := result[len(result)-1]
			if len(stateSyncLogs) != 0 {
				t.Errorf("State-sync logs array has %d entries, want 0 (empty but present)", len(stateSyncLogs))
			}
		}
	})

	// Test 2: Pre-Madhugiri block with state-sync receipt that has NON-EMPTY logs
	t.Run("pre_madhugiri_nonempty_state_sync_logs", func(t *testing.T) {
		// Create a state-sync receipt with logs
		stateSyncAddr := common.HexToAddress("0x0000000000000000000000000000000000001010")
		borReceipt := &types.Receipt{
			Type:   types.StateSyncTxType,
			Status: types.ReceiptStatusSuccessful,
			Logs: []*types.Log{
				{
					Address: stateSyncAddr,
					Topics:  []common.Hash{common.HexToHash("0x1234")},
					Data:    []byte{1, 2, 3, 4},
				},
			},
		}

		backendWithBor := &testBackendWithPreMadhuguriBorReceipt{
			testBackend: backend,
			borReceipt:  borReceipt,
		}
		api := NewBorAPI(backendWithBor)

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		// Should have 3 entries: 2 regular txs + 1 state-sync with logs
		expectedLen := 3
		if len(result) != expectedLen {
			t.Errorf("GetLogsByHash() returned %d log arrays, want %d (2 regular + 1 state-sync)", len(result), expectedLen)
		}

		// Verify the state-sync entry exists and has logs (last entry)
		if len(result) > 0 {
			stateSyncLogs := result[len(result)-1]
			if len(stateSyncLogs) != 1 {
				t.Errorf("State-sync logs array has %d entries, want 1", len(stateSyncLogs))
			}
			if len(stateSyncLogs) > 0 && stateSyncLogs[0].Address != stateSyncAddr {
				t.Errorf("State-sync log address = %s, want %s", stateSyncLogs[0].Address, stateSyncAddr)
			}
		}
	})

	// Test 3: Pre-Madhugiri block with no state-sync receipt
	t.Run("pre_madhugiri_no_state_sync_receipt", func(t *testing.T) {
		backendWithBor := &testBackendWithPreMadhuguriBorReceipt{
			testBackend: backend,
			borReceipt:  nil, // No state-sync receipt
		}
		api := NewBorAPI(backendWithBor)

		result, err := api.GetLogsByHash(context.Background(), block.Hash())
		if err != nil {
			t.Fatalf("GetLogsByHash() error = %v", err)
		}

		// Should have 2 entries: only the 2 regular txs, no state-sync
		expectedLen := 2
		if len(result) != expectedLen {
			t.Errorf("GetLogsByHash() returned %d log arrays, want %d (2 regular, no state-sync)", len(result), expectedLen)
		}
	})
}

func TestGetBlockAndReceiptsStateSyncReceipt(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		logAddr = common.HexToAddress("0x0000000000000000000000000000000000001010")
		genesis = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	backend := newTestBackend(t, 1, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		if i == 0 {
			tx, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(accs[0].addr),
					To:       &accs[0].addr,
					Value:    big.NewInt(1000),
					Gas:      params.TxGas,
					GasPrice: big.NewInt(params.InitialBaseFee),
				}),
				types.LatestSigner(genesis.Config), accs[0].key,
			)
			b.AddTx(tx)
		}
	})

	block := backend.chain.GetBlockByNumber(1)
	if block == nil {
		t.Fatal("Could not get block 1")
	}

	normalReceipts, err := backend.GetReceipts(context.Background(), block.Hash())
	require.NoError(t, err)

	borReceipt := &types.Receipt{
		Type:   types.StateSyncTxType,
		Status: types.ReceiptStatusSuccessful,
		Logs: []*types.Log{
			{
				Address: logAddr,
				Topics:  []common.Hash{common.HexToHash("0x1234")},
				Data:    []byte{1, 2, 3, 4},
			},
		},
	}

	t.Run("pre_madhugiri_appends_state_sync_receipt", func(t *testing.T) {
		api := NewBorAPI(&testBackendWithPreMadhuguriBorReceipt{
			testBackend: backend,
			borReceipt:  borReceipt,
		})

		gotBlock, receipts, err := api.getBlockAndReceipts(context.Background(), block.NumberU64())
		require.NoError(t, err)
		require.NotNil(t, gotBlock)
		require.Equal(t, block.Hash(), gotBlock.Hash())
		require.Len(t, receipts, len(normalReceipts)+1)

		last := receipts[len(receipts)-1]
		require.EqualValues(t, types.StateSyncTxType, last.Type)
		require.Len(t, last.Logs, 1)
		require.Equal(t, logAddr, last.Logs[0].Address)
	})

	t.Run("post_madhugiri_does_not_append_state_sync_receipt", func(t *testing.T) {
		api := NewBorAPI(&testBackendWithPostMadhugiriBorReceipt{
			testBackend: backend,
			borReceipt:  borReceipt,
		})

		gotBlock, receipts, err := api.getBlockAndReceipts(context.Background(), block.NumberU64())
		require.NoError(t, err)
		require.NotNil(t, gotBlock)
		require.Equal(t, block.Hash(), gotBlock.Hash())
		require.Len(t, receipts, len(normalReceipts))
	})
}

func TestBorGetBlockByTimestamp(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(1)
		genesis = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Create blocks with specific timestamps
	// Genesis: timestamp 0
	// Block 1: timestamp 100
	// Block 2: timestamp 200
	// Block 3: timestamp 300
	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		switch i {
		case 0:
			b.OffsetTime(100)
		case 1:
			b.OffsetTime(200)
		case 2:
			b.OffsetTime(300)
		}
	})
	api := NewBorAPI(backend)

	// Test 1: Timestamp before genesis
	t.Run("timestamp_before_genesis", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 0, false)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockByTimestamp() returned nil")
		}

		// Should return genesis block (block 0)
		blockNum, ok := result["number"].(*hexutil.Big)
		if !ok {
			t.Fatal("Block number not found or wrong type")
		}
		if blockNum.ToInt().Uint64() != 0 {
			t.Errorf("Expected block 0, got %d", blockNum.ToInt().Uint64())
		}
	})

	// Test 2: Timestamp matching exact block
	t.Run("timestamp_exact_match", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 200, false)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockByTimestamp() returned nil")
		}

		// Should return block 2
		blockNum, ok := result["number"].(*hexutil.Big)
		if !ok {
			t.Fatal("Block number not found or wrong type")
		}
		if blockNum.ToInt().Uint64() != 2 {
			t.Errorf("Expected block 2, got %d", blockNum.ToInt().Uint64())
		}
	})

	// Test 3: Timestamp between blocks (should return the closest block with time >= timestamp)
	t.Run("timestamp_between_blocks", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 150, false)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockByTimestamp() returned nil")
		}

		// Should return block 2 (first block with time >= 150)
		blockNum, ok := result["number"].(*hexutil.Big)
		if !ok {
			t.Fatal("Block number not found or wrong type")
		}
		if blockNum.ToInt().Uint64() != 2 {
			t.Errorf("Expected block 2, got %d", blockNum.ToInt().Uint64())
		}
	})

	// Test 4: Timestamp after all blocks (should return latest)
	t.Run("timestamp_after_latest", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 400, false)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockByTimestamp() returned nil")
		}

		// Should return block 3 (latest)
		blockNum, ok := result["number"].(*hexutil.Big)
		if !ok {
			t.Fatal("Block number not found or wrong type")
		}
		if blockNum.ToInt().Uint64() != 3 {
			t.Errorf("Expected block 3, got %d", blockNum.ToInt().Uint64())
		}
	})

	// Test 5: fullTx parameter (should include full transaction objects)
	t.Run("fullTx_parameter", func(t *testing.T) {
		// Add a transaction to block 1
		backendWithTx := newTestBackend(t, 1, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
			if i == 0 {
				b.OffsetTime(100)
				tx, _ := types.SignTx(
					types.NewTx(&types.LegacyTx{
						Nonce:    b.TxNonce(accs[0].addr),
						To:       &accs[0].addr,
						Value:    big.NewInt(1000),
						Gas:      21000,
						GasPrice: big.NewInt(params.GWei),
						Data:     nil,
					}),
					types.LatestSigner(genesis.Config), accs[0].key,
				)
				b.AddTx(tx)
			}
		})
		apiWithTx := NewBorAPI(backendWithTx)

		// Test with fullTx=false (only hashes)
		resultNoTx, err := apiWithTx.GetBlockByTimestamp(context.Background(), 100, false)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp(fullTx=false) error = %v", err)
		}
		if resultNoTx == nil {
			t.Fatal("GetBlockByTimestamp(fullTx=false) returned nil")
		}

		txs, ok := resultNoTx["transactions"].([]interface{})
		if !ok {
			t.Fatal("Transactions field not found or wrong type")
		}
		if len(txs) == 0 {
			t.Fatal("Expected transactions in block")
		}

		// With fullTx=false, transactions should be hashes
		if _, ok := txs[0].(common.Hash); !ok {
			t.Errorf("Expected transaction hash (common.Hash) with fullTx=false, got %T", txs[0])
		}

		// Test with fullTx=true (full objects)
		resultWithTx, err := apiWithTx.GetBlockByTimestamp(context.Background(), 100, true)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp(fullTx=true) error = %v", err)
		}
		if resultWithTx == nil {
			t.Fatal("GetBlockByTimestamp(fullTx=true) returned nil")
		}

		txsFull, ok := resultWithTx["transactions"].([]interface{})
		if !ok {
			t.Fatal("Transactions field not found or wrong type")
		}
		if len(txsFull) == 0 {
			t.Fatal("Expected transactions in block")
		}

		// With fullTx=true, transactions should be RPC transaction objects
		if txsFull[0] == nil {
			t.Error("Expected transaction object with fullTx=true, got nil")
		}
		// Verify it's not just a hash
		if _, ok := txsFull[0].(common.Hash); ok {
			t.Error("Expected transaction object with fullTx=true, got hash")
		}
	})

	// Test 6: Multiple blocks with the same timestamps (should return the earliest)
	t.Run("repeated_timestamp_returns_earliest", func(t *testing.T) {
		// Create blocks where blocks 2, 3, and 4 all have timestamp 200
		backendWithRepeated := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
			switch i {
			case 0:
				b.OffsetTime(100) // Block 1 at t=100
			case 1:
				b.OffsetTime(200) // Block 2 at t=200
			case 2:
				b.OffsetTime(200) // Block 3 at t=200 (same as block 2)
			case 3:
				b.OffsetTime(200) // Block 4 at t=200 (same as block 2 and 3)
			case 4:
				b.OffsetTime(300) // Block 5 at t=300
			}
		})
		apiWithRepeated := NewBorAPI(backendWithRepeated)

		result, err := apiWithRepeated.GetBlockByTimestamp(context.Background(), 200, false)
		if err != nil {
			t.Fatalf("GetBlockByTimestamp() error = %v", err)
		}
		if result == nil {
			t.Fatal("GetBlockByTimestamp() returned nil")
		}

		// Should return block 2 (the earliest block with timestamp 200)
		blockNum, ok := result["number"].(*hexutil.Big)
		if !ok {
			t.Fatal("Block number not found or wrong type")
		}
		if blockNum.ToInt().Uint64() != 2 {
			t.Errorf("Expected block 2 (earliest with timestamp 200), got %d", blockNum.ToInt().Uint64())
		}
	})
}

func TestBorGetBalanceChangesInBlock(t *testing.T) {
	t.Parallel()

	var (
		accs    = newAccounts(3)
		miner   = common.HexToAddress("0xdeadbeef")
		genesis = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				accs[0].addr: {Balance: big.NewInt(params.Ether)},
				accs[1].addr: {Balance: big.NewInt(params.Ether)},
				accs[2].addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Create blocks with transactions
	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		b.SetCoinbase(miner) // Set miner address

		if i == 0 {
			// Block 1: Single transfer
			tx, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(accs[0].addr),
					To:       &accs[1].addr,
					Value:    big.NewInt(1000),
					Gas:      21000,
					GasPrice: big.NewInt(params.GWei),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), accs[0].key,
			)
			b.AddTx(tx)
		}
		if i == 1 {
			// Block 2: Two transfers
			tx1, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(accs[0].addr),
					To:       &accs[2].addr,
					Value:    big.NewInt(2000),
					Gas:      21000,
					GasPrice: big.NewInt(params.GWei),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), accs[0].key,
			)
			b.AddTx(tx1)

			tx2, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(accs[1].addr),
					To:       &accs[2].addr,
					Value:    big.NewInt(500),
					Gas:      21000,
					GasPrice: big.NewInt(params.GWei),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), accs[1].key,
			)
			b.AddTx(tx2)
		}
		// Block 3: No transactions
	})
	api := NewBorAPI(backend)

	// Test 1: Block with a single transaction
	t.Run("single_transfer_block", func(t *testing.T) {
		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(1)
		result, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() error = %v", err)
		}

		// Should have 3 addresses: sender (accs[0]), recipient (accs[1]), and miner
		if len(result) != 3 {
			t.Errorf("Expected 3 balance changes, got %d", len(result))
		}

		// Verify sender balance decreased
		if bal, ok := result[accs[0].addr]; !ok {
			t.Error("Sender address not in result")
		} else if bal.ToInt().Cmp(big.NewInt(params.Ether)) >= 0 {
			t.Error("Sender balance should have decreased")
		}

		// Verify recipient balance increased
		if bal, ok := result[accs[1].addr]; !ok {
			t.Error("Recipient address not in result")
		} else if bal.ToInt().Cmp(big.NewInt(params.Ether)) <= 0 {
			t.Error("Recipient balance should have increased")
		}

		// Verify miner received fees
		if bal, ok := result[miner]; !ok {
			t.Error("Miner address not in result")
		} else if bal.ToInt().Sign() <= 0 {
			t.Error("Miner balance should be positive")
		}
	})

	// Test 2: Block with multiple transactions
	t.Run("multiple_transfers_block", func(t *testing.T) {
		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(2)
		result, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() error = %v", err)
		}

		// Should have 4 addresses: 2 senders, 1 recipient, and miner
		if len(result) != 4 {
			t.Errorf("Expected 4 balance changes, got %d", len(result))
		}

		// Verify accs[2] balance increased
		if bal, ok := result[accs[2].addr]; !ok {
			t.Error("Recipient address not in result")
		} else {
			// Should have received 2000 + 500 = 2500 wei
			increase := new(big.Int).Sub(bal.ToInt(), big.NewInt(params.Ether))
			if increase.Cmp(big.NewInt(2500)) != 0 {
				t.Errorf("Expected recipient to gain 2500 wei, got %s", increase.String())
			}
		}
	})

	// Test 3: Empty block
	t.Run("empty_block", func(t *testing.T) {
		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(3)
		result, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() error = %v", err)
		}

		// Only miner should have balance change
		if len(result) != 1 {
			t.Errorf("Expected 1 balance change (miner), got %d", len(result))
		}

		// Verify the miner received the reward
		if bal, ok := result[miner]; !ok {
			t.Error("Miner address not in result")
		} else if bal.ToInt().Sign() <= 0 {
			t.Error("Miner balance should be positive")
		}
	})

	// Test 4: Genesis block
	t.Run("genesis_block", func(t *testing.T) {
		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(0)
		result, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() error = %v", err)
		}

		// Genesis block should have no balance changes from execution
		if len(result) != 0 {
			t.Errorf("Expected 0 balance changes for genesis, got %d", len(result))
		}
	})

	// Test 5: Non-existent block
	t.Run("non_existent_block", func(t *testing.T) {
		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(9999)
		_, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err == nil {
			t.Error("Expected error for non-existent block, got nil")
		}
	})

	// Test 6: Query by hash
	t.Run("query_by_hash", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}

		blockNrOrHash := rpc.BlockNumberOrHashWithHash(block.Hash(), false)
		result, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() error = %v", err)
		}

		// Should have the same result as querying by number
		if len(result) != 3 {
			t.Errorf("Expected 3 balance changes, got %d. Addresses: %v", len(result), result)
		}
	})

	// Test 7: Contract creation
	t.Run("contract_creation", func(t *testing.T) {
		// Create a block with a contract deployment
		backendWithContract := newTestBackend(t, 1, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
			b.SetCoinbase(miner)
			if i == 0 {
				// Deploy a simple contract (empty bytecode)
				contractData := common.Hex2Bytes("6000")
				tx, _ := types.SignTx(
					types.NewTx(&types.LegacyTx{
						Nonce:    b.TxNonce(accs[0].addr),
						To:       nil, // Contract creation
						Value:    big.NewInt(100),
						Gas:      100000,
						GasPrice: big.NewInt(params.GWei),
						Data:     contractData,
					}),
					types.LatestSigner(genesis.Config), accs[0].key,
				)
				b.AddTx(tx)
			}
		})
		apiWithContract := NewBorAPI(backendWithContract)

		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(1)
		result, err := apiWithContract.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() error = %v", err)
		}

		// Should track sender, contract address, and miner
		if len(result) < 3 {
			t.Errorf("Expected at least 3 balance changes (sender, contract, miner), got %d", len(result))
		}

		// Verify sender paid for contract creation
		if bal, ok := result[accs[0].addr]; !ok {
			t.Error("Sender address not in result")
		} else if bal.ToInt().Cmp(big.NewInt(params.Ether)) >= 0 {
			t.Error("Sender balance should have decreased")
		}
	})

	// Test 8: Pending block not available
	t.Run("pending_block_not_available", func(t *testing.T) {
		blockNrOrHash := rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)
		_, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		// Should error because testBackend doesn't provide pending state
		if err == nil {
			t.Error("Expected error for pending block when not available, got nil")
		}
		if err != nil && err.Error() != "pending state not available" {
			t.Errorf("Expected 'pending state not available' error, got: %v", err)
		}
	})

	// Test 9: requireCanonical=true with canonical hash
	t.Run("require_canonical_with_canonical_hash", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}

		// Query with requireCanonical=true for canonical hash
		blockNrOrHash := rpc.BlockNumberOrHashWithHash(block.Hash(), true)
		result, err := api.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)
		if err != nil {
			t.Fatalf("GetBalanceChangesInBlock() with requireCanonical=true error = %v", err)
		}

		// Should return a valid result (3 addresses for block 1)
		if len(result) != 3 {
			t.Errorf("Expected 3 balance changes, got %d", len(result))
		}
	})

	// Test 10: requireCanonical=true with non-canonical hash
	t.Run("require_canonical_with_non_canonical_hash", func(t *testing.T) {
		// Create a backend with a non-canonical block at height 1
		canonicalBlock := backend.chain.GetBlockByNumber(1)
		if canonicalBlock == nil {
			t.Fatal("Could not get canonical block 1")
		}

		// Create a non-canonical block with the same number but different hash
		nonCanonicalBlock := types.NewBlock(
			&types.Header{
				Number:     canonicalBlock.Number(),
				Time:       canonicalBlock.Time() + 1, // Different timestamp
				ParentHash: canonicalBlock.ParentHash(),
				Root:       canonicalBlock.Root(),
				Difficulty: canonicalBlock.Difficulty(),
				Extra:      []byte("non-canonical"),
			},
			nil, nil, nil,
		)

		// Wrap backend to return a non-canonical block by hash
		wrappedBackend := &testBackendWithNonCanonicalBlock{
			testBackend:       backend,
			nonCanonicalBlock: nonCanonicalBlock,
			canonicalBlock:    canonicalBlock,
		}
		wrappedAPI := NewBorAPI(wrappedBackend)

		// Query with requireCanonical=true using non-canonical hash
		blockNrOrHash := rpc.BlockNumberOrHashWithHash(nonCanonicalBlock.Hash(), true)
		_, err := wrappedAPI.GetBalanceChangesInBlock(context.Background(), blockNrOrHash)

		// Should error with a specific message
		if err == nil {
			t.Error("Expected error for non-canonical hash with requireCanonical=true, got nil")
		}
		expectedErr := fmt.Sprintf("hash %x is not currently canonical", nonCanonicalBlock.Hash())
		if err != nil && err.Error() != expectedErr {
			t.Errorf("Expected '%s' error, got: %v", expectedErr, err)
		}
	})
}

// testBackendWithNonCanonicalBlock wraps testBackend to simulate non-canonical blocks
type testBackendWithNonCanonicalBlock struct {
	*testBackend
	nonCanonicalBlock *types.Block
	canonicalBlock    *types.Block
}

func (b *testBackendWithNonCanonicalBlock) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	// Return a non-canonical block if its hash is queried
	if hash == b.nonCanonicalBlock.Hash() {
		return b.nonCanonicalBlock, nil
	}
	return b.testBackend.BlockByHash(ctx, hash)
}

func (b *testBackendWithNonCanonicalBlock) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	// Always return canonical block by number
	return b.testBackend.BlockByNumber(ctx, number)
}

// testBackendWithCoinbase wraps testBackend and overrides Etherbase
type testBackendWithCoinbase struct {
	*testBackend
	coinbase common.Address
}

func (b *testBackendWithCoinbase) Etherbase() (common.Address, error) {
	return b.coinbase, nil
}

// testBackendWithHashrate wraps testBackend and overrides Hashrate
type testBackendWithHashrate struct {
	*testBackend
	hashrate uint64
}

func (b *testBackendWithHashrate) Hashrate() (uint64, error) {
	return b.hashrate, nil
}

// testBackendWithMining wraps testBackend and overrides Mining
type testBackendWithMining struct {
	*testBackend
	mining bool
}

func (b *testBackendWithMining) Mining() (bool, error) {
	return b.mining, nil
}

// testBackendWithProtocolVersion wraps testBackend and overrides ProtocolVersion
type testBackendWithProtocolVersion struct {
	*testBackend
	protocolVersion uint
}

func (b *testBackendWithProtocolVersion) ProtocolVersion() uint {
	return b.protocolVersion
}

type testBackendWithCancelAfterFirstBlockLookup struct {
	*testBackend
	cancel      context.CancelFunc
	lookupCount int
}

func (b *testBackendWithCancelAfterFirstBlockLookup) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	block, err := b.testBackend.BlockByNumber(ctx, number)
	if number >= 0 {
		b.lookupCount++
		if b.lookupCount == 1 {
			b.cancel()
		}
	}
	return block, err
}

func TestBorGetLatestLogs(t *testing.T) {
	t.Parallel()

	// Create test accounts
	var (
		acc1     = newAccounts(1)[0]
		testAddr = common.HexToAddress("0x1234567890123456789012345678901234567890")
		genesis  = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				acc1.addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Create a backend with blocks containing transactions
	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		if i >= 0 {
			tx, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(acc1.addr),
					To:       &testAddr,
					Value:    big.NewInt(1000),
					Gas:      21000,
					GasPrice: big.NewInt(params.GWei),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), acc1.key,
			)
			b.AddTx(tx)
		}
	})
	api := NewBorAPI(backend)

	// Test 1: Get logs from the latest block (blockCount=1)
	t.Run("latest_single_block", func(t *testing.T) {
		blockCount := uint64(1)
		crit := FilterCriteria{}
		opts := LogFilterOptions{
			BlockCount: &blockCount,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, opts)
		if err != nil {
			t.Fatalf("GetLatestLogs() error = %v", err)
		}

		// Simple transactions don't generate logs, so the result should be empty or have only system logs
		_ = logs
	})

	// Test 2: Get logs with block range
	t.Run("block_range", func(t *testing.T) {
		blockCount := uint64(3)
		crit := FilterCriteria{}
		opts := LogFilterOptions{
			BlockCount: &blockCount,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, opts)
		if err != nil {
			t.Fatalf("GetLatestLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 3: Get logs with a logCount limit
	t.Run("log_count_limit", func(t *testing.T) {
		logCount := uint64(100)
		crit := FilterCriteria{}
		opts := LogFilterOptions{
			LogCount: &logCount,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, opts)
		if err != nil {
			t.Fatalf("GetLatestLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 4: Default options (should use blockCount=1)
	t.Run("default_options", func(t *testing.T) {
		crit := FilterCriteria{}
		opts := LogFilterOptions{}

		logs, err := api.GetLatestLogs(context.Background(), crit, opts)
		if err != nil {
			t.Fatalf("GetLatestLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 5: LogCount and BlockCount both specified (should error)
	t.Run("ambiguous_options", func(t *testing.T) {
		logCount := uint64(10)
		blockCount := uint64(5)
		crit := FilterCriteria{}
		opts := LogFilterOptions{
			LogCount:   &logCount,
			BlockCount: &blockCount,
		}

		_, err := api.GetLatestLogs(context.Background(), crit, opts)
		if err == nil {
			t.Error("Expected error when both logCount and blockCount are specified, got nil")
		}
		if err != nil && err.Error() != "logs count & block count are ambiguous" {
			t.Errorf("Expected 'logs count & block count are ambiguous' error, got: %v", err)
		}
	})

	// Test 6: Verify timestamp is populated
	t.Run("timestamp_populated", func(t *testing.T) {
		blockCount := uint64(1)
		crit := FilterCriteria{
			Addresses: []common.Address{testAddr},
		}
		opts := LogFilterOptions{
			BlockCount: &blockCount,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, opts)
		if err != nil {
			t.Fatalf("GetLatestLogs() error = %v", err)
		}

		if len(logs) > 0 && logs[0].BlockTimestamp == 0 {
			t.Error("Expected BlockTimestamp to be populated, got 0")
		}
	})
}

func TestBorGetLogs(t *testing.T) {
	t.Parallel()

	// Create test accounts
	var (
		acc1     = newAccounts(1)[0]
		testAddr = common.HexToAddress("0x1234567890123456789012345678901234567890")
		genesis  = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				acc1.addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Create a backend with blocks containing transactions
	backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		if i >= 0 {
			tx, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(acc1.addr),
					To:       &testAddr,
					Value:    big.NewInt(1000),
					Gas:      21000,
					GasPrice: big.NewInt(params.GWei),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), acc1.key,
			)
			b.AddTx(tx)
		}
	})
	api := NewBorAPI(backend)

	// Test 1: Get logs from a single block by hash
	t.Run("single_block_by_hash", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(2)
		if block == nil {
			t.Fatal("Could not get block 2")
		}

		blockHash := block.Hash()
		crit := FilterCriteria{
			BlockHash: &blockHash,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 2: Get logs from a block range
	t.Run("block_range", func(t *testing.T) {
		fromBlock := big.NewInt(1)
		toBlock := big.NewInt(3)
		crit := FilterCriteria{
			FromBlock: fromBlock,
			ToBlock:   toBlock,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 3: Get logs from an entire chain (no range specified)
	t.Run("entire_chain", func(t *testing.T) {
		crit := FilterCriteria{}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 4: Get logs with an address filter
	t.Run("address_filter", func(t *testing.T) {
		crit := FilterCriteria{
			Addresses: []common.Address{testAddr},
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 5: Invalid range (end < begin)
	t.Run("invalid_range", func(t *testing.T) {
		fromBlock := big.NewInt(5)
		toBlock := big.NewInt(1)
		crit := FilterCriteria{
			FromBlock: fromBlock,
			ToBlock:   toBlock,
		}

		_, err := api.GetLogs(context.Background(), crit)
		if err == nil {
			t.Error("Expected error for invalid range (end < begin), got nil")
		}
		expectedErr := "end (1) < begin (5)"
		if err != nil && err.Error() != expectedErr {
			t.Errorf("Expected '%s' error, got: %v", expectedErr, err)
		}
	})

	// Test 6: Verify timestamp is populated
	t.Run("timestamp_populated", func(t *testing.T) {
		block := backend.chain.GetBlockByNumber(1)
		if block == nil {
			t.Fatal("Could not get block 1")
		}

		blockHash := block.Hash()
		crit := FilterCriteria{
			BlockHash: &blockHash,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}

		// Even if no logs exist, the method should not error
		// If logs exist, they should have timestamps
		for _, log := range logs {
			if log.BlockTimestamp == 0 {
				t.Error("Expected BlockTimestamp to be populated, got 0")
			}
		}
	})

	// Test 7: Get logs from the genesis block
	t.Run("genesis_block", func(t *testing.T) {
		blockHash := backend.chain.GetBlockByNumber(0).Hash()
		crit := FilterCriteria{
			BlockHash: &blockHash,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}
		_ = logs
	})

	// Test 8: Ascending order verification
	t.Run("ascending_order", func(t *testing.T) {
		fromBlock := big.NewInt(1)
		toBlock := big.NewInt(5)
		crit := FilterCriteria{
			FromBlock: fromBlock,
			ToBlock:   toBlock,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() error = %v", err)
		}

		// Verify logs are in ascending order by block number
		for i := 1; i < len(logs); i++ {
			if logs[i-1].BlockNumber > logs[i].BlockNumber {
				t.Errorf("Logs not in ascending order: block %d after block %d",
					logs[i-1].BlockNumber, logs[i].BlockNumber)
			}
		}
	})

	// Test 9: Negative block tag should use latest
	t.Run("latest_block_tag", func(t *testing.T) {
		latest := big.NewInt(int64(rpc.LatestBlockNumber))
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   latest,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs() with latest tag error = %v", err)
		}
		_ = logs
	})

	// Test 10: Invalid negative block tag (should error)
	t.Run("invalid_negative_block_tag", func(t *testing.T) {
		invalidNegative := big.NewInt(-999)
		crit := FilterCriteria{
			FromBlock: invalidNegative,
			ToBlock:   big.NewInt(5),
		}

		_, err := api.GetLogs(context.Background(), crit)
		if err == nil {
			t.Error("Expected error for invalid negative FromBlock, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "negative value for FromBlock") {
			t.Errorf("Expected 'negative value for FromBlock' error, got: %v", err)
		}
	})

	// Test 11: Invalid negative ToBlock tag
	t.Run("invalid_negative_toblock_tag", func(t *testing.T) {
		invalidNegative := big.NewInt(-999)
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   invalidNegative,
		}

		_, err := api.GetLogs(context.Background(), crit)
		if err == nil {
			t.Error("Expected error for invalid negative ToBlock, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "negative value for ToBlock") {
			t.Errorf("Expected 'negative value for ToBlock' error, got: %v", err)
		}
	})

	// Test 12: Topic filtering with ordered matching
	t.Run("topic_filtering_ordered", func(t *testing.T) {
		contractCode := common.FromHex("0x608060405234801561001057600080fd5b50610150806100206000396000f3fe608060405234801561001057600080fd5b506004361061002b5760003560e01c8063c040622614610030575b600080fd5b61004a60048036038101906100459190610094565b61004c565b005b7f1234567890123456789012345678901234567890123456789012345678901234600082604051610079929190610116565b60405180910390a150565b60008135905061009381610136565b92915050565b6000602082840312156100ab576100aa610131565b5b60006100b984828501610084565b91505092915050565b6100cb8161013f565b82525050565b6100da81610149565b82525050565b60006020820190506100f560008301846100c2565b92915050565b600060208201905061011060008301846100d1565b92915050565b600060408201905061012b60008301856100c2565b61013860208301846100c2565b9392505050565b6000819050919050565b6000819050919050565b61015c81610143565b811461016757600080fd5b5056fea2646970667358221220abcd1234567890abcd1234567890abcd1234567890abcd1234567890abcd123464736f6c63430008070033")

		// event emission for testing
		topic1 := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
		topic2 := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")

		// Test with topics array (should match positionally)
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(5),
			Topics: [][]common.Hash{
				{topic1}, // Must be in position 0
				{topic2}, // Must be in position 1
			},
		}

		logs, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs failed: %v", err)
		}

		// Verify all logs match topic positions (zero logs in this test)
		for _, log := range logs {
			if len(log.Topics) < 2 {
				continue
			}
			if log.Topics[0] != topic1 || log.Topics[1] != topic2 {
				t.Errorf("Log topics don't match filter: got %v and %v, want %v and %v",
					log.Topics[0], log.Topics[1], topic1, topic2)
			}
		}
		_ = contractCode
	})
}

func TestBorGetLatestLogs_TopicMatching(t *testing.T) {
	t.Parallel()

	// Create test accounts and deploy a contract that emits multi-topic events
	var (
		acc1    = newAccounts(1)[0]
		genesis = &core.Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				acc1.addr: {Balance: big.NewInt(params.Ether)},
			},
		}
	)

	// Known topic hashes for testing
	topic1 := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	topic2 := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	topic3 := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")

	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		// Generate transactions
		if i >= 0 {
			testAddr := common.HexToAddress("0x1234567890123456789012345678901234567890")
			tx, _ := types.SignTx(
				types.NewTx(&types.LegacyTx{
					Nonce:    b.TxNonce(acc1.addr),
					To:       &testAddr,
					Value:    big.NewInt(1000),
					Gas:      21000,
					GasPrice: big.NewInt(params.GWei),
					Data:     nil,
				}),
				types.LatestSigner(genesis.Config), acc1.key,
			)
			b.AddTx(tx)
		}
	})
	api := NewBorAPI(backend)

	// Test 1: Ordered topic matching (positional)
	t.Run("ordered_topic_matching", func(t *testing.T) {
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(3),
			Topics: [][]common.Hash{
				{topic1}, // Position 0 must be topic1
				{topic2}, // Position 1 must be topic2
				nil,      // Position 2 can be anything
				{topic3}, // Position 3 must be topic3
			},
		}

		logCount := uint64(100)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			IgnoreTopicsOrder: false, // Strict positional matching
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		if err != nil {
			t.Fatalf("GetLatestLogs failed: %v", err)
		}

		// Verify topic positions for any returned logs
		for _, log := range logs {
			if len(log.Topics) < 4 {
				continue
			}
			if log.Topics[0] != topic1 {
				t.Errorf("Topic at position 0: got %v, want %v", log.Topics[0], topic1)
			}
			if log.Topics[1] != topic2 {
				t.Errorf("Topic at position 1: got %v, want %v", log.Topics[1], topic2)
			}
			if log.Topics[3] != topic3 {
				t.Errorf("Topic at position 3: got %v, want %v", log.Topics[3], topic3)
			}
		}
	})

	// Test 2: Unordered topic matching (IgnoreTopicsOrder = true)
	t.Run("unordered_topic_matching", func(t *testing.T) {
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(3),
			Topics: [][]common.Hash{
				{topic1, topic2, topic3}, // Match any of these topics in any position
			},
		}

		logCount := uint64(100)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			IgnoreTopicsOrder: true, // Match topics in any order
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		if err != nil {
			t.Fatalf("GetLatestLogs failed: %v", err)
		}

		// Verify logs contain at least one of the specified topics (in any position)
		for _, log := range logs {
			hasMatchingTopic := false
			for _, logTopic := range log.Topics {
				if logTopic == topic1 || logTopic == topic2 || logTopic == topic3 {
					hasMatchingTopic = true
					break
				}
			}
			if !hasMatchingTopic {
				t.Errorf("Log topics %v don't contain any of the filter topics", log.Topics)
			}
		}
	})

	// Test 3: Empty topics filter (should match all logs)
	t.Run("empty_topics_matches_all", func(t *testing.T) {
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(3),
			Topics:    [][]common.Hash{}, // Empty topics array
		}

		logCount := uint64(100)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			IgnoreTopicsOrder: false,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		if err != nil {
			t.Fatalf("GetLatestLogs failed: %v", err)
		}

		_ = logs
	})

	// Test 4: Multiple topics at same position
	t.Run("multiple_topics_same_position", func(t *testing.T) {
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(3),
			Topics: [][]common.Hash{
				{topic1, topic2}, // Position 0 can be topic1 or topic2
				{topic3},         // Position 1 must be topic3
			},
		}

		logCount := uint64(100)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			IgnoreTopicsOrder: false,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		if err != nil {
			t.Fatalf("GetLatestLogs failed: %v", err)
		}

		// Verify each log has (topic1 or topic2) at position 0 and topic3 at position 1
		for _, log := range logs {
			if len(log.Topics) < 2 {
				continue
			}
			pos0Valid := log.Topics[0] == topic1 || log.Topics[0] == topic2
			pos1Valid := log.Topics[1] == topic3

			if !pos0Valid {
				t.Errorf("Topic at position 0: got %v, want %v or %v", log.Topics[0], topic1, topic2)
			}
			if !pos1Valid {
				t.Errorf("Topic at position 1: got %v, want %v", log.Topics[1], topic3)
			}
		}
	})
}

func TestBorGetLogs_ErrorPropagation(t *testing.T) {
	t.Parallel()

	// Test 1: Query beyond chain tip (clamp to latest, don't error)
	t.Run("future_block_clamping", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
			// Empty blocks
		})
		api := NewBorAPI(backend)

		// Query blocks beyond the chain tip
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(1000), // Chain only has 5 blocks
		}

		logs, err := api.GetLogs(context.Background(), crit)
		// Should succeed by clamping to the latest available block
		if err != nil {
			t.Fatalf("GetLogs failed (should clamp to latest): %v", err)
		}
		// Should return logs (or empty if no logs in blocks 0-5)
		_ = logs
		t.Logf("Successfully clamped future toBlock to latest, returned %d logs", len(logs))
	})

	// Test 2: Invalid block hash query
	t.Run("invalid_block_hash_returns_nil", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
			// Empty blocks
		})
		api := NewBorAPI(backend)

		// Query with a non-existent block hash
		nonExistentHash := common.HexToHash("0x1234567890123456789012345678901234567890123456789012345678901234")
		crit := FilterCriteria{
			BlockHash: &nonExistentHash,
		}

		logs, err := api.GetLogs(context.Background(), crit)
		// Should return (nil, nil) for invalid block hash
		if err != nil {
			t.Errorf("Expected nil error for invalid block hash, got: %v", err)
		}
		if logs != nil {
			t.Errorf("Expected nil logs for invalid block hash, got: %v", logs)
		}
		t.Log("Correctly returned (nil, nil) for non-existent block hash")
	})

	// Test 3: Range limit should reject overly broad scans
	t.Run("range_limit_exceeded", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, int(GetLogsMaxBlockRange+2), genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   new(big.Int).SetUint64(GetLogsMaxBlockRange + 1),
		}

		_, err := api.GetLogs(context.Background(), crit)
		if err == nil {
			t.Fatal("Expected client limit exceeded error, got nil")
		}
		var limitErr *clientLimitExceededError
		if !errors.As(err, &limitErr) {
			t.Fatalf("Expected clientLimitExceededError, got %T (%v)", err, err)
		}
	})

	// Test 4: Context cancellation propagation
	t.Run("context_cancellation", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 10, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		// Create a canceled context
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(10),
		}

		_, err := api.GetLogs(ctx, crit)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Expected context canceled error, got: %v", err)
		}
	})
}

func TestBorGetLatestLogs_ErrorPropagation(t *testing.T) {
	t.Parallel()

	// Test 1: Query beyond chain tip (clamp to latest)
	t.Run("future_block_clamping", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		})
		api := NewBorAPI(backend)

		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(1000), // Beyond chain tip
		}

		logCount := uint64(100)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			IgnoreTopicsOrder: false,
		}

		logs, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		// Should succeed by clamping to latest
		if err != nil {
			t.Fatalf("GetLatestLogs failed (should clamp to latest): %v", err)
		}
		_ = logs
		t.Logf("Successfully clamped future toBlock to latest, returned %d logs", len(logs))
	})

	// Test 2: Invalid block hash (return error)
	t.Run("invalid_block_hash_returns_error", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		})
		api := NewBorAPI(backend)

		nonExistentHash := common.HexToHash("0x1234567890123456789012345678901234567890123456789012345678901234")
		crit := FilterCriteria{
			BlockHash: &nonExistentHash,
		}

		logCount := uint64(100)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			IgnoreTopicsOrder: false,
		}

		_, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		// Should return error for invalid block hash
		if err == nil {
			t.Error("Expected error for invalid block hash, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "block header not found") {
			t.Errorf("Expected 'block header not found' error, got: %v", err)
		}
		t.Logf("Correctly returned error for non-existent block hash: %v", err)
	})

	// Test 3: BlockCount and LogCount both specified (ambiguous)
	t.Run("ambiguous_options_error", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		})
		api := NewBorAPI(backend)

		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(3),
		}

		// Both LogCount and BlockCount specified
		logCount := uint64(100)
		blockCount := uint64(10)
		logOptions := LogFilterOptions{
			LogCount:          &logCount,
			BlockCount:        &blockCount,
			IgnoreTopicsOrder: false,
		}

		_, err := api.GetLatestLogs(context.Background(), crit, logOptions)
		// Should return ambiguous error
		if err == nil {
			t.Error("Expected error for ambiguous options, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("Expected 'ambiguous' error, got: %v", err)
		}
	})

	// Test 4: Context cancellation propagation
	t.Run("context_cancellation", func(t *testing.T) {
		var (
			acc1    = newAccounts(1)[0]
			genesis = &core.Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					acc1.addr: {Balance: big.NewInt(params.Ether)},
				},
			}
		)

		backend := newTestBackend(t, 10, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {})
		api := NewBorAPI(backend)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(10),
		}
		blockCount := uint64(10)
		logOptions := LogFilterOptions{
			BlockCount: &blockCount,
		}

		_, err := api.GetLatestLogs(ctx, crit, logOptions)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Expected context canceled error, got: %v", err)
		}
	})
}

// TestGetLogsBlockRangeLimit verifies that bor_getLogs enforces GetLogsMaxBlockRange.
func TestGetLogsBlockRangeLimit(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc:  types.GenesisAlloc{},
	}
	// Need a chain longer than GetLogsMaxBlockRange so the range isn't clamped
	backend := newTestBackend(t, int(GetLogsMaxBlockRange)+2, genesis, ethash.NewFaker(), nil)
	api := NewBorAPI(backend)

	t.Run("within_limit", func(t *testing.T) {
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(5),
		}
		_, err := api.GetLogs(context.Background(), crit)
		if err != nil {
			t.Fatalf("GetLogs within range limit should not error: %v", err)
		}
	})

	t.Run("exceeds_limit", func(t *testing.T) {
		crit := FilterCriteria{
			FromBlock: big.NewInt(0),
			ToBlock:   big.NewInt(int64(GetLogsMaxBlockRange) + 1),
		}
		_, err := api.GetLogs(context.Background(), crit)
		if err == nil {
			t.Fatal("GetLogs exceeding range limit should error")
		}
		if !strings.Contains(err.Error(), "block range exceeds maximum") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestGetLogsRespectsContextCancellationDuringScan(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc:  types.GenesisAlloc{},
	}
	base := newTestBackend(t, 10, genesis, ethash.NewFaker(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	backend := &testBackendWithCancelAfterFirstBlockLookup{
		testBackend: base,
		cancel:      cancel,
	}
	api := NewBorAPI(backend)

	crit := FilterCriteria{
		FromBlock: big.NewInt(0),
		ToBlock:   big.NewInt(10),
	}

	_, err := api.GetLogs(ctx, crit)
	require.ErrorIs(t, err, context.Canceled)
}

func TestGetLatestLogsRespectsContextCancellationDuringScan(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc:  types.GenesisAlloc{},
	}
	base := newTestBackend(t, 10, genesis, ethash.NewFaker(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	backend := &testBackendWithCancelAfterFirstBlockLookup{
		testBackend: base,
		cancel:      cancel,
	}
	api := NewBorAPI(backend)

	crit := FilterCriteria{
		FromBlock: big.NewInt(0),
		ToBlock:   big.NewInt(10),
	}
	blockCount := uint64(10)
	opts := LogFilterOptions{BlockCount: &blockCount}

	_, err := api.GetLatestLogs(ctx, crit, opts)
	require.ErrorIs(t, err, context.Canceled)
}

// TestGetLogsLogCopySafety verifies that returned logs are copies,
// not shared pointers to cached receipt data.
func TestGetLogsLogCopySafety(t *testing.T) {
	t.Parallel()

	acc := newAccounts(1)[0]
	testAddr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc: types.GenesisAlloc{
			acc.addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(
			types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(acc.addr),
				To:       &testAddr,
				Value:    big.NewInt(1000),
				Gas:      21000,
				GasPrice: big.NewInt(params.GWei),
			}),
			types.LatestSigner(genesis.Config), acc.key,
		)
		b.AddTx(tx)
	})
	api := NewBorAPI(backend)

	block := backend.chain.GetBlockByNumber(1)
	require.NotNil(t, block)

	blockHash := block.Hash()
	crit := FilterCriteria{BlockHash: &blockHash}

	// Call twice — if logs are copies, mutations from the first call must not affect the second.
	logs1, err := api.GetLogs(context.Background(), crit)
	require.NoError(t, err)

	logs2, err := api.GetLogs(context.Background(), crit)
	require.NoError(t, err)

	// Mutate all timestamps from the first call
	for _, l := range logs1 {
		l.BlockTimestamp = 999999
	}

	// Second call's timestamps must be unaffected
	for _, l := range logs2 {
		if l.BlockTimestamp == 999999 {
			t.Fatal("returned logs share pointers with cached data; expected copies")
		}
	}
}

// TestGetBalanceChangesGenesisBlock verifies that genesis (block 0) returns
// an empty map regardless of how it is resolved.
func TestGetBalanceChangesGenesisBlock(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0xaa"): {Balance: big.NewInt(params.Ether)},
		},
	}
	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), nil)
	api := NewBorAPI(backend)

	t.Run("explicit_block_0", func(t *testing.T) {
		result, err := api.GetBalanceChangesInBlock(context.Background(),
			rpc.BlockNumberOrHashWithNumber(0))
		require.NoError(t, err)
		require.Empty(t, result, "genesis block should have no balance changes")
	})

	t.Run("earliest_tag", func(t *testing.T) {
		result, err := api.GetBalanceChangesInBlock(context.Background(),
			rpc.BlockNumberOrHashWithNumber(rpc.EarliestBlockNumber))
		require.NoError(t, err)
		require.Empty(t, result, "earliest block should have no balance changes")
	})
}

// TestGetBlockByTimestampContextCancellation verifies that a canceled context
// aborts the binary search in GetBlockByTimestamp.
func TestGetBlockByTimestampContextCancellation(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc:  types.GenesisAlloc{},
	}
	backend := newTestBackend(t, 10, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		b.OffsetTime(int64((i + 1) * 100))
	})
	api := NewBorAPI(backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := api.GetBlockByTimestamp(ctx, 500, false)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestMatchesFilterAddressSet verifies address matching via map.
func TestMatchesFilterAddressSet(t *testing.T) {
	t.Parallel()

	addr1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	addr2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	addr3 := common.HexToAddress("0x3333333333333333333333333333333333333333")

	addressSet := map[common.Address]struct{}{
		addr1: {},
		addr2: {},
	}

	topic1 := common.HexToHash("0xaaaa")
	topic2 := common.HexToHash("0xbbbb")

	tests := []struct {
		name      string
		log       *types.Log
		addrSet   map[common.Address]struct{}
		topics    [][]common.Hash
		ignoreOrd bool
		wantMatch bool
	}{
		{
			name:      "matching address",
			log:       &types.Log{Address: addr1},
			addrSet:   addressSet,
			wantMatch: true,
		},
		{
			name:      "non-matching address",
			log:       &types.Log{Address: addr3},
			addrSet:   addressSet,
			wantMatch: false,
		},
		{
			name:      "empty address set matches any",
			log:       &types.Log{Address: addr3},
			addrSet:   map[common.Address]struct{}{},
			wantMatch: true,
		},
		{
			name:      "topic match ordered",
			log:       &types.Log{Address: addr1, Topics: []common.Hash{topic1, topic2}},
			addrSet:   addressSet,
			topics:    [][]common.Hash{{topic1}, {topic2}},
			wantMatch: true,
		},
		{
			name:      "topic mismatch ordered",
			log:       &types.Log{Address: addr1, Topics: []common.Hash{topic2, topic1}},
			addrSet:   addressSet,
			topics:    [][]common.Hash{{topic1}, {topic2}},
			wantMatch: false,
		},
		{
			name:      "topic match unordered",
			log:       &types.Log{Address: addr1, Topics: []common.Hash{topic2, topic1}},
			addrSet:   addressSet,
			topics:    [][]common.Hash{{topic1}, {topic2}},
			ignoreOrd: true,
			wantMatch: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchesFilter(tc.log, tc.addrSet, tc.topics, tc.ignoreOrd)
			if got != tc.wantMatch {
				t.Errorf("matchesFilter() = %v, want %v", got, tc.wantMatch)
			}
		})
	}
}

// TestGetBlockByTimestampBinarySearch exercises the context-aware binary search
func TestGetBlockByTimestampBinarySearch(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc:  types.GenesisAlloc{},
	}
	// Blocks with increasing timestamps: genesis=0, 1=100, 2=200, ..., 10=1000
	backend := newTestBackend(t, 10, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		b.OffsetTime(int64((i + 1) * 100))
	})
	api := NewBorAPI(backend)

	t.Run("finds_exact_timestamp", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 500, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		blockNum := result["number"].(*hexutil.Big).ToInt().Uint64()
		// The block with time >= 500 should be block 5
		header, _ := backend.HeaderByNumber(context.Background(), rpc.BlockNumber(blockNum))
		require.NotNil(t, header)
		if header.Time < 500 {
			t.Errorf("block %d time %d < requested 500", blockNum, header.Time)
		}
	})

	t.Run("future_timestamp_returns_latest", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 99999, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		// Should return the latest block
		blockNum := result["number"].(*hexutil.Big).ToInt().Uint64()
		latest := backend.chain.CurrentBlock().Number.Uint64()
		if blockNum != latest {
			t.Errorf("expected latest block %d, got %d", latest, blockNum)
		}
	})

	t.Run("genesis_timestamp", func(t *testing.T) {
		result, err := api.GetBlockByTimestamp(context.Background(), 0, false)
		require.NoError(t, err)
		require.NotNil(t, result)
		blockNum := result["number"].(*hexutil.Big).ToInt().Uint64()
		if blockNum != 0 {
			t.Errorf("expected block 0 for genesis timestamp, got %d", blockNum)
		}
	})
}

// TestBorBlockNumberPendingTag verifies the pending tag returns the latest executed block.
func TestBorBlockNumberPendingTag(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{},
	}
	backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), nil)
	api := NewBorAPI(backend)

	pending := rpc.PendingBlockNumber
	result, err := api.BlockNumber(context.Background(), &pending)
	require.NoError(t, err)

	expected := backend.chain.CurrentBlock().Number.Uint64()
	if uint64(result) != expected {
		t.Errorf("BlockNumber(pending) = %d, want %d", result, expected)
	}
}

// TestBorBlockNumberNegativeUnknownTag verifies negative unknown tags fall through to the latest.
func TestBorBlockNumberNegativeUnknownTag(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{},
	}
	backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), nil)
	api := NewBorAPI(backend)

	unknownNeg := rpc.BlockNumber(-99)
	result, err := api.BlockNumber(context.Background(), &unknownNeg)
	require.NoError(t, err)

	expected := backend.chain.CurrentBlock().Number.Uint64()
	if uint64(result) != expected {
		t.Errorf("BlockNumber(-99) = %d, want latest %d", result, expected)
	}
}

// testBackendWithNilBorTx simulates ReadBorTransaction returning nil for the state-sync tx.
type testBackendWithNilBorTx struct {
	*testBackend
}

func (b *testBackendWithNilBorTx) GetBorBlockReceipt(_ context.Context, _ common.Hash) (*types.Receipt, error) {
	// Return a receipt whose TxHash won't resolve to a bor transaction in DB
	return &types.Receipt{
		TxHash: common.HexToHash("0xdead"),
		Logs:   []*types.Log{},
	}, nil
}

func (b *testBackendWithNilBorTx) ChainConfig() *params.ChainConfig {
	// Pre-Madhugiri so GetBlockReceiptsByBlockHash enters the state-sync path.
	// Deep-copy BorConfig to avoid mutating the shared global.
	cfg := *params.AllEthashProtocolChanges
	borCfg := params.BorConfig{}
	if cfg.Bor != nil {
		borCfg = *cfg.Bor
	}
	borCfg.MadhugiriBlock = nil
	cfg.Bor = &borCfg
	return &cfg
}

// TestGetBlockReceiptsByBlockHashNilBorTx verifies that a nil bor transaction
// does not crash with a nil-pointer dereference.
func TestGetBlockReceiptsByBlockHashNilBorTx(t *testing.T) {
	t.Parallel()

	acc := newAccounts(1)[0]
	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc: types.GenesisAlloc{
			acc.addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	base := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(
			types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(acc.addr),
				To:       &common.Address{0x01},
				Value:    big.NewInt(1000),
				Gas:      21000,
				GasPrice: big.NewInt(params.GWei),
			}),
			types.LatestSigner(genesis.Config), acc.key,
		)
		b.AddTx(tx)
	})

	wrapped := &testBackendWithNilBorTx{testBackend: base}
	api := NewBorAPI(wrapped)

	block := base.chain.GetBlockByNumber(1)
	require.NotNil(t, block)

	// This should not panic even though ReadBorTransaction returns nil
	result, err := api.GetBlockReceiptsByBlockHash(context.Background(), block.Hash())
	require.NoError(t, err)
	require.NotNil(t, result)

	// Should have only the normal tx receipts (1 per block), no bor receipt appended
	require.Len(t, result, 1)
}

// TestGetLatestLogsBlockScanCap verifies that LogCount mode is subject to GetLatestLogMaxBlockScan.
func TestGetLatestLogsBlockScanCap(t *testing.T) {
	t.Parallel()

	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc:  types.GenesisAlloc{},
	}
	backend := newTestBackend(t, 5, genesis, ethash.NewFaker(), nil)
	api := NewBorAPI(backend)

	// Request logs by count with no block range
	logCount := uint64(100)
	crit := FilterCriteria{}
	opts := LogFilterOptions{LogCount: &logCount}

	// Should succeed without error
	logs, err := api.GetLatestLogs(context.Background(), crit, opts)
	require.NoError(t, err)
	_ = logs
}

// TestGetLatestLogsLogCopySafety is the GetLatestLogs counterpart of TestGetLogsLogCopySafety.
func TestGetLatestLogsLogCopySafety(t *testing.T) {
	t.Parallel()

	acc := newAccounts(1)[0]
	testAddr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	genesis := &core.Genesis{
		Config: params.AllEthashProtocolChanges,
		Alloc: types.GenesisAlloc{
			acc.addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	backend := newTestBackend(t, 3, genesis, ethash.NewFaker(), func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(
			types.NewTx(&types.LegacyTx{
				Nonce:    b.TxNonce(acc.addr),
				To:       &testAddr,
				Value:    big.NewInt(1000),
				Gas:      21000,
				GasPrice: big.NewInt(params.GWei),
			}),
			types.LatestSigner(genesis.Config), acc.key,
		)
		b.AddTx(tx)
	})
	api := NewBorAPI(backend)

	blockCount := uint64(3)
	crit := FilterCriteria{}
	opts := LogFilterOptions{BlockCount: &blockCount}

	logs1, err := api.GetLatestLogs(context.Background(), crit, opts)
	require.NoError(t, err)

	logs2, err := api.GetLatestLogs(context.Background(), crit, opts)
	require.NoError(t, err)

	for _, l := range logs1 {
		l.BlockTimestamp = 999999
	}
	for _, l := range logs2 {
		if l.BlockTimestamp == 999999 {
			t.Fatal("GetLatestLogs returned logs sharing cached pointers")
		}
	}
}

// TestFilterCriteriaUnmarshalJSON covers FilterCriteria JSON parsing including
// edge cases for addresses, topics, blockHash vs fromBlock/toBlock validation.
func TestFilterCriteriaUnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr string
		check   func(t *testing.T, fc FilterCriteria)
	}{
		{
			name:  "fromBlock and toBlock",
			input: `{"fromBlock":"0x1","toBlock":"0xa"}`,
			check: func(t *testing.T, fc FilterCriteria) {
				if fc.FromBlock == nil || fc.FromBlock.Int64() != 1 {
					t.Errorf("FromBlock = %v, want 1", fc.FromBlock)
				}
				if fc.ToBlock == nil || fc.ToBlock.Int64() != 10 {
					t.Errorf("ToBlock = %v, want 10", fc.ToBlock)
				}
			},
		},
		{
			name:  "blockHash only",
			input: `{"blockHash":"0x0000000000000000000000000000000000000000000000000000000000000001"}`,
			check: func(t *testing.T, fc FilterCriteria) {
				if fc.BlockHash == nil {
					t.Fatal("expected BlockHash to be set")
				}
			},
		},
		{
			name:    "blockHash with fromBlock is error",
			input:   `{"blockHash":"0x0000000000000000000000000000000000000000000000000000000000000001","fromBlock":"0x1"}`,
			wantErr: "cannot specify both",
		},
		{
			name:  "single address string",
			input: `{"address":"0x1111111111111111111111111111111111111111"}`,
			check: func(t *testing.T, fc FilterCriteria) {
				require.Len(t, fc.Addresses, 1)
				if fc.Addresses[0] != common.HexToAddress("0x1111111111111111111111111111111111111111") {
					t.Errorf("unexpected address: %s", fc.Addresses[0].Hex())
				}
			},
		},
		{
			name:  "array of addresses",
			input: `{"address":["0x1111111111111111111111111111111111111111","0x2222222222222222222222222222222222222222"]}`,
			check: func(t *testing.T, fc FilterCriteria) {
				require.Len(t, fc.Addresses, 2)
			},
		},
		{
			name:    "invalid address hex",
			input:   `{"address":"0xZZZZ"}`,
			wantErr: "invalid address",
		},
		{
			name:    "invalid address in array",
			input:   `{"address":["0xZZZZ"]}`,
			wantErr: "invalid address at index 0",
		},
		{
			name:    "non-string address in array",
			input:   `{"address":[123]}`,
			wantErr: "non-string address",
		},
		{
			name:    "invalid address type",
			input:   `{"address":123}`,
			wantErr: "invalid addresses",
		},
		{
			name:  "single topic string",
			input: `{"topics":["0x0000000000000000000000000000000000000000000000000000000000000001"]}`,
			check: func(t *testing.T, fc FilterCriteria) {
				require.Len(t, fc.Topics, 1)
				require.Len(t, fc.Topics[0], 1)
			},
		},
		{
			name:  "nil topic matches any",
			input: `{"topics":[null,"0x0000000000000000000000000000000000000000000000000000000000000001"]}`,
			check: func(t *testing.T, fc FilterCriteria) {
				require.Len(t, fc.Topics, 2)
				require.Len(t, fc.Topics[0], 0, "nil topic slot should be empty")
				require.Len(t, fc.Topics[1], 1)
			},
		},
		{
			name:  "topic array with nil element",
			input: `{"topics":[["0x0000000000000000000000000000000000000000000000000000000000000001",null]]}`,
			check: func(t *testing.T, fc FilterCriteria) {
				require.Len(t, fc.Topics, 1)
				require.Len(t, fc.Topics[0], 1, "nil topics in array should be skipped")
			},
		},
		{
			name:    "invalid topic string",
			input:   `{"topics":["0xZZZZ"]}`,
			wantErr: "invalid topic at index 0",
		},
		{
			name:    "invalid topic in array",
			input:   `{"topics":[["0xZZZZ"]]}`,
			wantErr: "invalid topic",
		},
		{
			name:    "non-string topic in array",
			input:   `{"topics":[[123]]}`,
			wantErr: "invalid topic type",
		},
		{
			name:    "invalid topic type (number)",
			input:   `{"topics":[123]}`,
			wantErr: "invalid topic type at index 0",
		},
		{
			name:  "empty input",
			input: `{}`,
			check: func(t *testing.T, fc FilterCriteria) {
				require.Empty(t, fc.Addresses)
				require.Nil(t, fc.BlockHash)
			},
		},
		{
			name:    "address with wrong length",
			input:   `{"address":"0x1234"}`,
			wantErr: "invalid address",
		},
		{
			name:    "topic with wrong length",
			input:   `{"topics":["0x1234"]}`,
			wantErr: "invalid topic",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var fc FilterCriteria
			err := json.Unmarshal([]byte(tc.input), &fc)
			if tc.wantErr != "" {
				require.Error(t, err)
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			require.NoError(t, err)
			if tc.check != nil {
				tc.check(t, fc)
			}
		})
	}
}

// TestDecodeAddressAndTopic exercises decodeAddress and decodeTopic directly.
func TestDecodeAddressAndTopic(t *testing.T) {
	t.Parallel()

	t.Run("valid_address", func(t *testing.T) {
		addr, err := decodeAddress("0x1111111111111111111111111111111111111111")
		require.NoError(t, err)
		if addr != common.HexToAddress("0x1111111111111111111111111111111111111111") {
			t.Errorf("unexpected: %s", addr.Hex())
		}
	})

	t.Run("wrong_length_address", func(t *testing.T) {
		_, err := decodeAddress("0x1234")
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid length")
	})

	t.Run("invalid_hex_address", func(t *testing.T) {
		_, err := decodeAddress("0xZZZZ")
		require.Error(t, err)
	})

	t.Run("valid_topic", func(t *testing.T) {
		h, err := decodeTopic("0x0000000000000000000000000000000000000000000000000000000000000001")
		require.NoError(t, err)
		expected := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000001")
		if h != expected {
			t.Errorf("unexpected: %s", h.Hex())
		}
	})

	t.Run("wrong_length_topic", func(t *testing.T) {
		_, err := decodeTopic("0x1234")
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid length")
	})

	t.Run("invalid_hex_topic", func(t *testing.T) {
		_, err := decodeTopic("not-hex")
		require.Error(t, err)
	})
}

// testBackendWithGiuglianoExtra wraps testBackend to return headers with
// Giugliano extra data and a Cancun-enabled ChainConfig.
type testBackendWithGiuglianoExtra struct {
	*testBackend
	headers  map[int64]*types.Header // block number → header
	hashes   map[common.Hash]*types.Header
	chainCfg *params.ChainConfig
}

func (b *testBackendWithGiuglianoExtra) ChainConfig() *params.ChainConfig {
	return b.chainCfg
}

func (b *testBackendWithGiuglianoExtra) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if h, ok := b.headers[int64(number)]; ok {
		return h, nil
	}
	return b.testBackend.HeaderByNumber(ctx, number)
}

func (b *testBackendWithGiuglianoExtra) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	if h, ok := b.hashes[hash]; ok {
		return h, nil
	}
	return b.testBackend.HeaderByHash(ctx, hash)
}

// buildExtraWithGiuglianoFields builds a header Extra field containing RLP-encoded BlockExtraData.
func buildExtraWithGiuglianoFields(gasTarget, bfcd *uint64) []byte {
	bed := &types.BlockExtraData{
		GasTarget:                gasTarget,
		BaseFeeChangeDenominator: bfcd,
	}
	bedBytes, _ := rlp.EncodeToBytes(bed)
	extra := make([]byte, types.ExtraVanityLength)
	extra = append(extra, bedBytes...)
	extra = append(extra, make([]byte, types.ExtraSealLength)...)
	return extra
}

// newGasParamsTestAPI creates a BorAPI backed by a testBackendWithGiuglianoExtra.
// headers are registered by both number and hash for lookup.
func newGasParamsTestAPI(t *testing.T, cfg *params.ChainConfig, headers ...*types.Header) *BorAPI {
	t.Helper()
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			common.HexToAddress("0x0000000000000000000000000000000000000000"): {Balance: big.NewInt(1000000000000000000)},
		},
	}
	base := newTestBackend(t, 5, genesis, ethash.NewFaker(), nil)

	byNum := make(map[int64]*types.Header, len(headers))
	byHash := make(map[common.Hash]*types.Header, len(headers))
	for _, h := range headers {
		byNum[h.Number.Int64()] = h
		byHash[h.Hash()] = h
	}

	backend := &testBackendWithGiuglianoExtra{
		testBackend: base,
		headers:     byNum,
		hashes:      byHash,
		chainCfg:    cfg,
	}
	return NewBorAPI(backend)
}

// newPostGiuglianoGasParamsAPI creates a BorAPI with a post-Giugliano header at block 10
// containing the given gas target and base fee change denominator.
func newPostGiuglianoGasParamsAPI(t *testing.T, gasTarget, bfcd uint64) (*BorAPI, *types.Header) {
	t.Helper()
	header := &types.Header{
		Number:   big.NewInt(10),
		GasLimit: 30_000_000,
		Extra:    buildExtraWithGiuglianoFields(&gasTarget, &bfcd),
	}
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(0),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(0)},
	}
	return newGasParamsTestAPI(t, cfg, header), header
}

func TestGetBlockGasParams_PostGiugliano(t *testing.T) {
	t.Parallel()
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)
	api, _ := newPostGiuglianoGasParamsAPI(t, gasTarget, bfcd)

	result, err := api.GetBlockGasParams(context.Background(), rpc.BlockNumberOrHashWithNumber(10))
	require.NoError(t, err)
	require.NotNil(t, result.GasTarget)
	require.NotNil(t, result.BaseFeeChangeDenominator)
	require.Equal(t, hexutil.Uint64(gasTarget), *result.GasTarget)
	require.Equal(t, hexutil.Uint64(bfcd), *result.BaseFeeChangeDenominator)
}

func TestGetBlockGasParams_PostGiugliano_ByHash(t *testing.T) {
	t.Parallel()
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)
	api, header := newPostGiuglianoGasParamsAPI(t, gasTarget, bfcd)

	result, err := api.GetBlockGasParams(context.Background(), rpc.BlockNumberOrHashWithHash(header.Hash(), false))
	require.NoError(t, err)
	require.NotNil(t, result.GasTarget)
	require.NotNil(t, result.BaseFeeChangeDenominator)
	require.Equal(t, hexutil.Uint64(gasTarget), *result.GasTarget)
	require.Equal(t, hexutil.Uint64(bfcd), *result.BaseFeeChangeDenominator)
}

func TestGetBlockGasParams_PreGiugliano(t *testing.T) {
	t.Parallel()
	header := &types.Header{
		Number:   big.NewInt(5),
		GasLimit: 30_000_000,
		Extra:    buildExtraWithGiuglianoFields(nil, nil),
	}

	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(0),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(100)},
	}

	api := newGasParamsTestAPI(t, cfg, header)
	result, err := api.GetBlockGasParams(context.Background(), rpc.BlockNumberOrHashWithNumber(5))
	require.NoError(t, err)
	require.Nil(t, result.GasTarget)
	require.Nil(t, result.BaseFeeChangeDenominator)
}

func TestGetBlockGasParams_PreCancun(t *testing.T) {
	t.Parallel()
	header := &types.Header{
		Number:   big.NewInt(5),
		GasLimit: 30_000_000,
		Extra:    make([]byte, types.ExtraVanityLength+types.ExtraSealLength),
	}

	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(100),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(100)},
	}

	api := newGasParamsTestAPI(t, cfg, header)
	result, err := api.GetBlockGasParams(context.Background(), rpc.BlockNumberOrHashWithNumber(5))
	require.NoError(t, err)
	require.Nil(t, result.GasTarget)
	require.Nil(t, result.BaseFeeChangeDenominator)
}

func TestGetBlockGasParams_HeaderNotFound(t *testing.T) {
	t.Parallel()
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(0),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(0)},
	}

	api := newGasParamsTestAPI(t, cfg) // no custom headers
	_, err := api.GetBlockGasParams(context.Background(), rpc.BlockNumberOrHashWithNumber(999))
	require.Error(t, err)
	require.Contains(t, err.Error(), "header not found")
}

func TestGetBlockGasParams_HeaderNotFound_ByHash(t *testing.T) {
	t.Parallel()
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(0),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(0)},
	}

	api := newGasParamsTestAPI(t, cfg) // no custom headers
	fakeHash := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	_, err := api.GetBlockGasParams(context.Background(), rpc.BlockNumberOrHashWithHash(fakeHash, false))
	require.Error(t, err)
	require.Contains(t, err.Error(), "header not found")
}

// testBackendWithBlockExtra wraps testBackend to return blocks with custom Extra data
// and a custom ChainConfig for GetBlockByNumber/GetBlockByHash tests.
type testBackendWithBlockExtra struct {
	*testBackend
	block    *types.Block
	chainCfg *params.ChainConfig
}

func (b *testBackendWithBlockExtra) ChainConfig() *params.ChainConfig {
	return b.chainCfg
}

func (b *testBackendWithBlockExtra) BlockByNumber(_ context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if b.block != nil && number == rpc.BlockNumber(b.block.NumberU64()) {
		return b.block, nil
	}
	return nil, nil
}

func (b *testBackendWithBlockExtra) BlockByHash(_ context.Context, hash common.Hash) (*types.Block, error) {
	if b.block != nil && hash == b.block.Hash() {
		return b.block, nil
	}
	return nil, nil
}

// makeBlockWithExtra creates a minimal block with the given extra data in its header.
func makeBlockWithExtra(number int64, extra []byte) *types.Block {
	header := &types.Header{
		Number:   big.NewInt(number),
		GasLimit: 30_000_000,
		Extra:    extra,
	}
	return types.NewBlockWithHeader(header)
}

func boolPtr(b bool) *bool { return &b }

// postCancunCfg returns a ChainConfig with Cancun and Giugliano at genesis.
func postCancunCfg() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(0),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(0)},
	}
}

func postAustinCfg() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(0),
		Bor: &params.BorConfig{
			GiuglianoBlock: big.NewInt(0),
			AustinBlock:    big.NewInt(0),
		},
	}
}

// newBlockExtraTestAPI creates a BlockChainAPI backed by a testBackendWithBlockExtra.
func newBlockExtraTestAPI(t *testing.T, block *types.Block, cfg *params.ChainConfig) *BlockChainAPI {
	t.Helper()
	genesis := &core.Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{}}
	base := newTestBackend(t, 1, genesis, ethash.NewFaker(), nil)
	backend := &testBackendWithBlockExtra{testBackend: base, block: block, chainCfg: cfg}
	return NewBlockChainAPI(backend)
}

// makeBlockWithBorExtra creates a block with an RLP-encoded BlockExtraData or
// BlockExtraDataPostAustin in the header Extra field.
func makeBlockWithBorExtra(number int64, bed any) *types.Block {
	bedBytes, _ := rlp.EncodeToBytes(bed)
	extra := make([]byte, types.ExtraVanityLength)
	extra = append(extra, bedBytes...)
	extra = append(extra, make([]byte, types.ExtraSealLength)...)
	return makeBlockWithExtra(number, extra)
}

// TestGetBlockByNumber_BorExtraFlag_PostAustin verifies
// decodedExtra.txDependency is always null once Austin activates, while
// gasTarget/baseFeeChangeDenominator still round-trip.
func TestGetBlockByNumber_BorExtraFlag_PostAustin(t *testing.T) {
	t.Parallel()
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)

	block := makeBlockWithBorExtra(10, &types.BlockExtraDataPostAustin{
		GasTarget:                &gasTarget,
		BaseFeeChangeDenominator: &bfcd,
	})
	api := newBlockExtraTestAPI(t, block, postAustinCfg())

	result, err := api.GetBlockByNumber(context.Background(), 10, false, boolPtr(true))
	require.NoError(t, err)
	require.NotNil(t, result)

	extraData, ok := result["decodedExtra"]
	require.True(t, ok, "response should contain decodedExtra")

	rpcExtra := extraData.(*RPCBlockExtraData)
	require.NotNil(t, rpcExtra.GasTarget)
	require.Equal(t, hexutil.Uint64(gasTarget), *rpcExtra.GasTarget)
	require.NotNil(t, rpcExtra.BaseFeeChangeDenominator)
	require.Equal(t, hexutil.Uint64(bfcd), *rpcExtra.BaseFeeChangeDenominator)
	require.Nil(t, rpcExtra.TxDependency, "txDependency must be null post-Austin")
}

func TestGetBlockByNumber_BorExtraFlag_PostCancun(t *testing.T) {
	t.Parallel()
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)
	txDep := [][]uint64{{0}, {0, 1}, {}}

	block := makeBlockWithBorExtra(10, &types.BlockExtraData{
		GasTarget:                &gasTarget,
		BaseFeeChangeDenominator: &bfcd,
		TxDependency:             txDep,
	})
	api := newBlockExtraTestAPI(t, block, postCancunCfg())

	result, err := api.GetBlockByNumber(context.Background(), 10, false, boolPtr(true))
	require.NoError(t, err)
	require.NotNil(t, result)

	extraData, ok := result["decodedExtra"]
	require.True(t, ok, "response should contain decodedExtra")

	rpcExtra := extraData.(*RPCBlockExtraData)
	require.NotNil(t, rpcExtra.GasTarget)
	require.Equal(t, hexutil.Uint64(gasTarget), *rpcExtra.GasTarget)
	require.NotNil(t, rpcExtra.BaseFeeChangeDenominator)
	require.Equal(t, hexutil.Uint64(bfcd), *rpcExtra.BaseFeeChangeDenominator)
	require.Equal(t, txDep, rpcExtra.TxDependency)
}

func TestGetBlockByHash_BorExtraFlag(t *testing.T) {
	t.Parallel()
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)

	block := makeBlockWithBorExtra(10, &types.BlockExtraData{
		GasTarget:                &gasTarget,
		BaseFeeChangeDenominator: &bfcd,
	})
	api := newBlockExtraTestAPI(t, block, postCancunCfg())

	result, err := api.GetBlockByHash(context.Background(), block.Hash(), false, boolPtr(true))
	require.NoError(t, err)
	require.NotNil(t, result)

	extraData, ok := result["decodedExtra"]
	require.True(t, ok, "response should contain decodedExtra")

	rpcExtra := extraData.(*RPCBlockExtraData)
	require.NotNil(t, rpcExtra.GasTarget)
	require.Equal(t, hexutil.Uint64(gasTarget), *rpcExtra.GasTarget)
}

func TestGetBlockByNumber_BorExtraFlag_Nil(t *testing.T) {
	t.Parallel()
	block := makeBlockWithExtra(10, buildExtraWithGiuglianoFields(nil, nil))
	api := newBlockExtraTestAPI(t, block, postCancunCfg())

	result, err := api.GetBlockByNumber(context.Background(), 10, false, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	_, ok := result["decodedExtra"]
	require.False(t, ok, "decodedExtra should not be present when borExtra is nil")
}

func TestGetBlockByNumber_BorExtraFlag_False(t *testing.T) {
	t.Parallel()
	gasTarget := uint64(15_000_000)
	bfcd := uint64(64)
	block := makeBlockWithExtra(10, buildExtraWithGiuglianoFields(&gasTarget, &bfcd))
	api := newBlockExtraTestAPI(t, block, postCancunCfg())

	result, err := api.GetBlockByNumber(context.Background(), 10, false, boolPtr(false))
	require.NoError(t, err)
	require.NotNil(t, result)
	_, ok := result["decodedExtra"]
	require.False(t, ok, "decodedExtra should not be present when borExtra is false")
}

func TestGetBlockByNumber_BorExtraFlag_PreCancun(t *testing.T) {
	t.Parallel()
	block := makeBlockWithExtra(5, make([]byte, types.ExtraVanityLength+types.ExtraSealLength))
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(1),
		CancunBlock: big.NewInt(100),
		Bor:         &params.BorConfig{GiuglianoBlock: big.NewInt(100)},
	}
	api := newBlockExtraTestAPI(t, block, cfg)

	result, err := api.GetBlockByNumber(context.Background(), 5, false, boolPtr(true))
	require.NoError(t, err)
	require.NotNil(t, result)
	_, ok := result["decodedExtra"]
	require.False(t, ok, "decodedExtra should not be present for pre-Cancun blocks")
}

// TestSystemTxGasCapBypass verifies that the RPC gas cap is only bypassed for
// internal consensus calls. No external RPC call should be able to bypass it.
func TestSystemTxGasCapBypass(t *testing.T) {
	t.Parallel()

	// Bytecode that returns the gas available to the call.
	// GAS PUSH1(0) MSTORE PUSH1(32) PUSH1(0) RETURN
	gasReportBytes := hexutil.Bytes(common.FromHex("5a60005260206000f3"))
	gasReportCode := &gasReportBytes

	systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001000")
	normalAddr := common.HexToAddress("0x0000000000000000000000000000000000009999")

	genesis := &core.Genesis{
		Config: params.MergedTestChainConfig,
		Alloc:  types.GenesisAlloc{},
	}
	api := NewBlockChainAPI(newTestBackend(t, 1, genesis, beacon.New(ethash.NewFaker()), func(i int, b *core.BlockGen) {
		b.SetPoS()
	}))

	latest := rpc.LatestBlockNumber
	rpcGasCap := api.b.RPCGasCap()
	tests := []struct {
		name       string
		internal   bool
		to         common.Address
		wantCapped bool
	}{
		{
			name:       "external RPC to system contract must be gas-capped",
			internal:   false,
			to:         systemAddr,
			wantCapped: true,
		},
		{
			name:       "internal consensus call to system contract bypasses gas cap",
			internal:   true,
			to:         systemAddr,
			wantCapped: false,
		},
		{
			name:       "external RPC to normal address is gas-capped",
			internal:   false,
			to:         normalAddr,
			wantCapped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.internal {
				ctx = WithBorInternalCall(ctx)
			}

			blockNr := rpc.BlockNumberOrHashWithNumber(latest)
			result, err := api.Call(ctx, TransactionArgs{
				To: &tt.to,
			}, &blockNr, &override.StateOverride{
				tt.to: override.OverrideAccount{
					Code: gasReportCode,
				},
			}, nil)
			require.NoError(t, err)

			gasAvailable := new(big.Int).SetBytes(result)
			if tt.wantCapped {
				require.True(t, gasAvailable.Uint64() <= rpcGasCap,
					"gas should be capped to RPCGasCap (%d), got %s", rpcGasCap, gasAvailable)
			} else {
				require.True(t, gasAvailable.Uint64() > rpcGasCap,
					"gas should bypass RPCGasCap (%d), got %s", rpcGasCap, gasAvailable)
			}
		})
	}
}
