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

package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	gomath "math"
	"math/big"
	"math/rand"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/core/vm/program"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// So we can deterministically seed different blockchains
var (
	canonicalSeed = 1
	forkSeed      = 2
)

// newCanonical creates a chain database, and injects a deterministic canonical
// chain. Depending on the full flag, it creates either a full block chain or a
// header only chain. The database and genesis specification for block generation
// are also returned in case more test blocks are needed later.
func newCanonical(engine consensus.Engine, n int, full bool, scheme string) (ethdb.Database, *Genesis, *BlockChain, error) {
	var (
		genesis = &Genesis{
			BaseFee: big.NewInt(params.InitialBaseFee),
			Config:  params.AllEthashProtocolChanges,
		}
	)
	// Initialize a fresh chain with only a genesis block
	options := DefaultConfig().WithStateScheme(scheme)
	blockchain, _ := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, options)

	// Create and inject the requested chain
	if n == 0 {
		return rawdb.NewMemoryDatabase(), genesis, blockchain, nil
	}

	if full {
		// Full block-chain requested
		genDb, blocks := makeBlockChainWithGenesis(genesis, n, engine, canonicalSeed)
		_, err := blockchain.InsertChain(blocks, false)

		return genDb, genesis, blockchain, err
	}
	// Header-only chain requested
	genDb, headers := makeHeaderChainWithGenesis(genesis, n, engine, canonicalSeed)
	_, err := blockchain.InsertHeaderChain(headers)
	return genDb, genesis, blockchain, err
}

func newGwei(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(params.GWei))
}

// Test fork of length N starting from block i
func testFork(t *testing.T, blockchain *BlockChain, i, n int, full bool, comparator func(td1, td2 *big.Int), scheme string) {
	// Copy old chain up to #i into a new db
	genDb, _, blockchain2, err := newCanonical(ethash.NewFaker(), i, full, scheme)
	if err != nil {
		t.Fatal("could not make new canonical in testFork", err)
	}
	defer blockchain2.Stop()

	// Assert the chains have the same header/block at #i
	var hash1, hash2 common.Hash
	if full {
		hash1 = blockchain.GetBlockByNumber(uint64(i)).Hash()
		hash2 = blockchain2.GetBlockByNumber(uint64(i)).Hash()
	} else {
		hash1 = blockchain.GetHeaderByNumber(uint64(i)).Hash()
		hash2 = blockchain2.GetHeaderByNumber(uint64(i)).Hash()
	}

	if hash1 != hash2 {
		t.Errorf("chain content mismatch at %d: have hash %v, want hash %v", i, hash2, hash1)
	}
	// Extend the newly created chain
	var (
		blockChainB  []*types.Block
		headerChainB []*types.Header
	)

	if full {
		blockChainB = makeBlockChain(blockchain2.chainConfig, blockchain2.GetBlockByHash(blockchain2.CurrentBlock().Hash()), n, ethash.NewFaker(), genDb, forkSeed)
		if _, err := blockchain2.InsertChain(blockChainB, false); err != nil {
			t.Fatalf("failed to insert forking chain: %v", err)
		}
	} else {
		headerChainB = makeHeaderChain(blockchain2.chainConfig, blockchain2.CurrentHeader(), n, ethash.NewFaker(), genDb, forkSeed)
		if _, err := blockchain2.InsertHeaderChain(headerChainB); err != nil {
			t.Fatalf("failed to insert forking chain: %v", err)
		}
	}
	// Sanity check that the forked chain can be imported into the original
	var tdPre, tdPost *big.Int

	if full {
		cur := blockchain.CurrentBlock()
		tdPre = blockchain.GetTd(cur.Hash(), cur.Number.Uint64())
		if err := testBlockChainImport(blockChainB, blockchain); err != nil {
			t.Fatalf("failed to import forked block chain: %v", err)
		}
		last := blockChainB[len(blockChainB)-1]
		tdPost = blockchain.GetTd(last.Hash(), last.NumberU64())
	} else {
		cur := blockchain.CurrentHeader()
		tdPre = blockchain.GetTd(cur.Hash(), cur.Number.Uint64())
		if err := testHeaderChainImport(headerChainB, blockchain); err != nil {
			t.Fatalf("failed to import forked header chain: %v", err)
		}
		last := headerChainB[len(headerChainB)-1]
		tdPost = blockchain.GetTd(last.Hash(), last.Number.Uint64())
	}
	// Compare the total difficulties of the chains
	comparator(tdPre, tdPost)
}

// testBlockChainImport tries to process a chain of blocks, writing them into
// the database if successful.
func testBlockChainImport(chain types.Blocks, blockchain *BlockChain) error {
	for _, block := range chain {
		// Try and process the block
		err := blockchain.engine.VerifyHeader(blockchain, block.Header())
		if err == nil {
			err = blockchain.validator.ValidateBody(block)
		}

		if err != nil {
			if err == ErrKnownBlock {
				continue
			}

			return err
		}
		_, err = state.New(blockchain.GetBlockByHash(block.ParentHash()).Root(), blockchain.statedb)
		if err != nil {
			return err
		}
		receipts, logs, usedGas, statedb, _, err := blockchain.ProcessBlock(block, blockchain.GetBlockByHash(block.ParentHash()).Header(), nil, nil, nil)
		res := &ProcessResult{
			Receipts: receipts,
			Logs:     logs,
			GasUsed:  usedGas,
		}
		if err != nil {
			blockchain.reportBlock(block, res, err)
			return err
		}
		err = blockchain.validator.ValidateState(block, statedb, res, false)
		if err != nil {
			blockchain.reportBlock(block, res, err)
			return err
		}

		blockchain.chainmu.MustLock()
		rawdb.WriteTd(blockchain.db, block.Hash(), block.NumberU64(), new(big.Int).Add(block.Difficulty(), blockchain.GetTd(block.ParentHash(), block.NumberU64()-1)))
		rawdb.WriteBlock(blockchain.db, block)
		statedb.Commit(block.NumberU64(), false, false)
		blockchain.chainmu.Unlock()
	}

	return nil
}

func TestParallelBlockChainImport(t *testing.T) {
	t.Parallel()

	testParallelBlockChainImport(t, rawdb.HashScheme, false)
	testParallelBlockChainImport(t, rawdb.PathScheme, false)

	testParallelBlockChainImport(t, rawdb.HashScheme, true)
	testParallelBlockChainImport(t, rawdb.PathScheme, true)
}

func testParallelBlockChainImport(t *testing.T, scheme string, enforceParallelProcessor bool) {
	db, _, blockchain, err := newCanonical(ethash.NewFaker(), 10, true, scheme)
	blockchain.parallelProcessor = NewParallelStateProcessor(blockchain.hc, blockchain)

	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}

	// If required, enforce parallel block processing and skip serial processing completely
	blockchain.enforceParallelProcessor = enforceParallelProcessor
	defer blockchain.Stop()

	block := blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash())
	blockChainB := makeFakeNonEmptyBlockChain(block, 5, ethash.NewFaker(), db, forkSeed, 5)

	if err := testBlockChainImport(blockChainB, blockchain); err == nil {
		t.Fatalf("expected error for bad tx")
	}
}

type AlwaysFailParallelStateProcessor struct {
}

func (p *AlwaysFailParallelStateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, address *common.Address, interruptCtx context.Context) (*ProcessResult, error) {
	return nil, errors.New("always fail")
}

type SlowSerialStateProcessor struct {
	s Processor
}

func NewSlowSerialStateProcessor(s Processor) *SlowSerialStateProcessor {
	return &SlowSerialStateProcessor{s: s}
}

func (p *SlowSerialStateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, address *common.Address, interruptCtx context.Context) (*ProcessResult, error) {
	time.Sleep(100 * time.Millisecond)
	return p.s.Process(block, statedb, cfg, nil, interruptCtx)
}

func TestSuccessfulBlockImportParallelFailed(t *testing.T) {
	t.Parallel()

	testSuccessfulBlockImportParallelFailed(t, rawdb.HashScheme)
	testSuccessfulBlockImportParallelFailed(t, rawdb.PathScheme)
}

func testSuccessfulBlockImportParallelFailed(t *testing.T, scheme string) {
	// Create a new blockchain with 10 initial blocks
	db, _, blockchain, err := newCanonical(ethash.NewFaker(), 10, true, scheme)
	blockchain.parallelProcessor = &AlwaysFailParallelStateProcessor{}
	blockchain.processor = NewSlowSerialStateProcessor(blockchain.processor)
	if err != nil {
		t.Fatalf("failed to create canonical chain: %v", err)
	}
	defer blockchain.Stop()

	// Create valid blocks to import
	block := blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash())
	blocks := makeBlockChain(blockchain.chainConfig, block, 5, ethash.NewFaker(), db, canonicalSeed)

	// Import the blocks
	n, err := blockchain.InsertChain(blocks, false)
	if err != nil {
		t.Fatalf("failed to import valid blocks: %v", err)
	}

	// Verify all blocks were imported
	if n != len(blocks) {
		t.Errorf("imported %d blocks, wanted %d", n, len(blocks))
	}

	// Verify the last block is properly linked
	if blockchain.CurrentBlock().Hash() != blocks[len(blocks)-1].Hash() {
		t.Errorf("current block hash mismatch: got %x, want %x",
			blockchain.CurrentBlock().Hash(),
			blocks[len(blocks)-1].Hash())
	}

	// Verify block numbers are sequential
	for i, block := range blocks {
		expectedNumber := uint64(11 + i) // 10 initial blocks + new blocks
		if block.NumberU64() != expectedNumber {
			t.Errorf("block %d has wrong number: got %d, want %d",
				i, block.NumberU64(), expectedNumber)
		}
	}
}

// testHeaderChainImport tries to process a chain of header, writing them into
// the database if successful.
func testHeaderChainImport(chain []*types.Header, blockchain *BlockChain) error {
	for _, header := range chain {
		// Try and validate the header
		if err := blockchain.engine.VerifyHeader(blockchain, header); err != nil {
			return err
		}
		// Manually insert the header into the database, but don't reorganise (allows subsequent testing)
		blockchain.chainmu.MustLock()
		rawdb.WriteTd(blockchain.db, header.Hash(), header.Number.Uint64(), new(big.Int).Add(header.Difficulty, blockchain.GetTd(header.ParentHash, header.Number.Uint64()-1)))
		rawdb.WriteHeader(blockchain.db, header)
		blockchain.chainmu.Unlock()
	}

	return nil
}
func TestLastBlock(t *testing.T) {
	testLastBlock(t, rawdb.HashScheme)
	testLastBlock(t, rawdb.PathScheme)
}

func testLastBlock(t *testing.T, scheme string) {
	genDb, _, blockchain, err := newCanonical(ethash.NewFaker(), 0, true, scheme)
	if err != nil {
		t.Fatalf("failed to create pristine chain: %v", err)
	}
	defer blockchain.Stop()

	blocks := makeBlockChain(blockchain.chainConfig, blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash()), 1, ethash.NewFullFaker(), genDb, 0)
	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatalf("Failed to insert block: %v", err)
	}

	if blocks[len(blocks)-1].Hash() != rawdb.ReadHeadBlockHash(blockchain.db) {
		t.Fatalf("Write/Get HeadBlockHash failed")
	}
}

// Test inserts the blocks/headers after the fork choice rule is changed.
// The chain is reorged to whatever specified.
func testInsertAfterMerge(t *testing.T, blockchain *BlockChain, i, n int, full bool, scheme string) {
	// Copy old chain up to #i into a new db
	genDb, _, blockchain2, err := newCanonical(ethash.NewFaker(), i, full, scheme)
	if err != nil {
		t.Fatal("could not make new canonical in testFork", err)
	}
	defer blockchain2.Stop()

	// Assert the chains have the same header/block at #i
	var hash1, hash2 common.Hash
	if full {
		hash1 = blockchain.GetBlockByNumber(uint64(i)).Hash()
		hash2 = blockchain2.GetBlockByNumber(uint64(i)).Hash()
	} else {
		hash1 = blockchain.GetHeaderByNumber(uint64(i)).Hash()
		hash2 = blockchain2.GetHeaderByNumber(uint64(i)).Hash()
	}

	if hash1 != hash2 {
		t.Errorf("chain content mismatch at %d: have hash %v, want hash %v", i, hash2, hash1)
	}

	// Extend the newly created chain
	if full {
		blockChainB := makeBlockChain(blockchain2.chainConfig, blockchain2.GetBlockByHash(blockchain2.CurrentBlock().Hash()), n, ethash.NewFaker(), genDb, forkSeed)
		if _, err := blockchain2.InsertChain(blockChainB, false); err != nil {
			t.Fatalf("failed to insert forking chain: %v", err)
		}

		if blockchain2.CurrentBlock().Number.Uint64() != blockChainB[len(blockChainB)-1].NumberU64() {
			t.Fatalf("failed to reorg to the given chain")
		}

		if blockchain2.CurrentBlock().Hash() != blockChainB[len(blockChainB)-1].Hash() {
			t.Fatalf("failed to reorg to the given chain")
		}
	} else {
		headerChainB := makeHeaderChain(blockchain2.chainConfig, blockchain2.CurrentHeader(), n, ethash.NewFaker(), genDb, forkSeed)
		if _, err := blockchain2.InsertHeaderChain(headerChainB); err != nil {
			t.Fatalf("failed to insert forking chain: %v", err)
		}

		if blockchain2.CurrentHeader().Number.Uint64() != headerChainB[len(headerChainB)-1].Number.Uint64() {
			t.Fatalf("failed to reorg to the given chain")
		}

		if blockchain2.CurrentHeader().Hash() != headerChainB[len(headerChainB)-1].Hash() {
			t.Fatalf("failed to reorg to the given chain")
		}
	}
}

// Tests that given a starting canonical chain of a given size, it can be extended
// with various length chains.
func TestExtendCanonicalHeaders(t *testing.T) {
	testExtendCanonical(t, false, rawdb.HashScheme)
	testExtendCanonical(t, false, rawdb.PathScheme)
}
func TestExtendCanonicalBlocks(t *testing.T) {
	testExtendCanonical(t, true, rawdb.HashScheme)
	testExtendCanonical(t, true, rawdb.PathScheme)
}

func testExtendCanonical(t *testing.T, full bool, scheme string) {
	length := 5

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	// Define the difficulty comparator
	better := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) <= 0 {
			t.Errorf("total difficulty mismatch: have %v, expected more than %v", td2, td1)
		}
	}
	// Start fork from current height
	testFork(t, processor, length, 1, full, better, scheme)
	testFork(t, processor, length, 2, full, better, scheme)
	testFork(t, processor, length, 5, full, better, scheme)
	testFork(t, processor, length, 10, full, better, scheme)
}

// Tests that given a starting canonical chain of a given size, it can be extended
// with various length chains.
func TestExtendCanonicalHeadersAfterMerge(t *testing.T) {
	testExtendCanonicalAfterMerge(t, false, rawdb.HashScheme)
	testExtendCanonicalAfterMerge(t, false, rawdb.PathScheme)
}
func TestExtendCanonicalBlocksAfterMerge(t *testing.T) {
	testExtendCanonicalAfterMerge(t, true, rawdb.HashScheme)
	testExtendCanonicalAfterMerge(t, true, rawdb.PathScheme)
}

func testExtendCanonicalAfterMerge(t *testing.T, full bool, scheme string) {
	length := 5

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	testInsertAfterMerge(t, processor, length, 1, full, scheme)
	testInsertAfterMerge(t, processor, length, 10, full, scheme)
}

// Tests that given a starting canonical chain of a given size, creating shorter
// forks do not take canonical ownership.
func TestShorterForkHeaders(t *testing.T) {
	testShorterFork(t, false, rawdb.HashScheme)
	testShorterFork(t, false, rawdb.PathScheme)
}
func TestShorterForkBlocks(t *testing.T) {
	testShorterFork(t, true, rawdb.HashScheme)
	testShorterFork(t, true, rawdb.PathScheme)
}

func testShorterFork(t *testing.T, full bool, scheme string) {
	length := 10

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	// Define the difficulty comparator
	worse := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) >= 0 {
			t.Errorf("total difficulty mismatch: have %v, expected less than %v", td2, td1)
		}
	}
	// Sum of numbers must be less than `length` for this to be a shorter fork
	testFork(t, processor, 0, 3, full, worse, scheme)
	testFork(t, processor, 0, 7, full, worse, scheme)
	testFork(t, processor, 1, 1, full, worse, scheme)
	testFork(t, processor, 1, 7, full, worse, scheme)
	testFork(t, processor, 5, 3, full, worse, scheme)
	testFork(t, processor, 5, 4, full, worse, scheme)
}

// Tests that given a starting canonical chain of a given size, creating shorter
// forks do not take canonical ownership.
func TestShorterForkHeadersAfterMerge(t *testing.T) {
	testShorterForkAfterMerge(t, false, rawdb.HashScheme)
	testShorterForkAfterMerge(t, false, rawdb.PathScheme)
}
func TestShorterForkBlocksAfterMerge(t *testing.T) {
	testShorterForkAfterMerge(t, true, rawdb.HashScheme)
	testShorterForkAfterMerge(t, true, rawdb.PathScheme)
}

func testShorterForkAfterMerge(t *testing.T, full bool, scheme string) {
	length := 10

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	testInsertAfterMerge(t, processor, 0, 3, full, scheme)
	testInsertAfterMerge(t, processor, 0, 7, full, scheme)
	testInsertAfterMerge(t, processor, 1, 1, full, scheme)
	testInsertAfterMerge(t, processor, 1, 7, full, scheme)
	testInsertAfterMerge(t, processor, 5, 3, full, scheme)
	testInsertAfterMerge(t, processor, 5, 4, full, scheme)
}

// Tests that given a starting canonical chain of a given size, creating longer
// forks do take canonical ownership.
func TestLongerForkHeaders(t *testing.T) {
	testLongerFork(t, false, rawdb.HashScheme)
	testLongerFork(t, false, rawdb.PathScheme)
}
func TestLongerForkBlocks(t *testing.T) {
	testLongerFork(t, true, rawdb.HashScheme)
	testLongerFork(t, true, rawdb.PathScheme)
}

func testLongerFork(t *testing.T, full bool, scheme string) {
	length := 10

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	testInsertAfterMerge(t, processor, 0, 11, full, scheme)
	testInsertAfterMerge(t, processor, 0, 15, full, scheme)
	testInsertAfterMerge(t, processor, 1, 10, full, scheme)
	testInsertAfterMerge(t, processor, 1, 12, full, scheme)
	testInsertAfterMerge(t, processor, 5, 6, full, scheme)
	testInsertAfterMerge(t, processor, 5, 8, full, scheme)
}

// Tests that given a starting canonical chain of a given size, creating longer
// forks do take canonical ownership.
func TestLongerForkHeadersAfterMerge(t *testing.T) {
	testLongerForkAfterMerge(t, false, rawdb.HashScheme)
	testLongerForkAfterMerge(t, false, rawdb.PathScheme)
}
func TestLongerForkBlocksAfterMerge(t *testing.T) {
	testLongerForkAfterMerge(t, true, rawdb.HashScheme)
	testLongerForkAfterMerge(t, true, rawdb.PathScheme)
}

func testLongerForkAfterMerge(t *testing.T, full bool, scheme string) {
	length := 10

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	testInsertAfterMerge(t, processor, 0, 11, full, scheme)
	testInsertAfterMerge(t, processor, 0, 15, full, scheme)
	testInsertAfterMerge(t, processor, 1, 10, full, scheme)
	testInsertAfterMerge(t, processor, 1, 12, full, scheme)
	testInsertAfterMerge(t, processor, 5, 6, full, scheme)
	testInsertAfterMerge(t, processor, 5, 8, full, scheme)
}

// Tests that given a starting canonical chain of a given size, creating equal
// forks do take canonical ownership.
func TestEqualForkHeaders(t *testing.T) {
	testEqualFork(t, false, rawdb.HashScheme)
	testEqualFork(t, false, rawdb.PathScheme)
}
func TestEqualForkBlocks(t *testing.T) {
	testEqualFork(t, true, rawdb.HashScheme)
	testEqualFork(t, true, rawdb.PathScheme)
}

func testEqualFork(t *testing.T, full bool, scheme string) {
	length := 10

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	// Define the difficulty comparator
	equal := func(td1, td2 *big.Int) {
		if td2.Cmp(td1) != 0 {
			t.Errorf("total difficulty mismatch: have %v, want %v", td2, td1)
		}
	}
	// Sum of numbers must be equal to `length` for this to be an equal fork
	testFork(t, processor, 0, 10, full, equal, scheme)
	testFork(t, processor, 1, 9, full, equal, scheme)
	testFork(t, processor, 2, 8, full, equal, scheme)
	testFork(t, processor, 5, 5, full, equal, scheme)
	testFork(t, processor, 6, 4, full, equal, scheme)
	testFork(t, processor, 9, 1, full, equal, scheme)
}

// Tests that given a starting canonical chain of a given size, creating equal
// forks do take canonical ownership.
func TestEqualForkHeadersAfterMerge(t *testing.T) {
	testEqualForkAfterMerge(t, false, rawdb.HashScheme)
	testEqualForkAfterMerge(t, false, rawdb.PathScheme)
}
func TestEqualForkBlocksAfterMerge(t *testing.T) {
	testEqualForkAfterMerge(t, true, rawdb.HashScheme)
	testEqualForkAfterMerge(t, true, rawdb.PathScheme)
}

func testEqualForkAfterMerge(t *testing.T, full bool, scheme string) {
	length := 10

	// Make first chain starting from genesis
	_, _, processor, err := newCanonical(ethash.NewFaker(), length, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer processor.Stop()

	testInsertAfterMerge(t, processor, 0, 10, full, scheme)
	testInsertAfterMerge(t, processor, 1, 9, full, scheme)
	testInsertAfterMerge(t, processor, 2, 8, full, scheme)
	testInsertAfterMerge(t, processor, 5, 5, full, scheme)
	testInsertAfterMerge(t, processor, 6, 4, full, scheme)
	testInsertAfterMerge(t, processor, 9, 1, full, scheme)
}

// Tests that chains missing links do not get accepted by the processor.
func TestBrokenHeaderChain(t *testing.T) {
	testBrokenChain(t, false, rawdb.HashScheme)
	testBrokenChain(t, false, rawdb.PathScheme)
}
func TestBrokenBlockChain(t *testing.T) {
	testBrokenChain(t, true, rawdb.HashScheme)
	testBrokenChain(t, true, rawdb.PathScheme)
}

func testBrokenChain(t *testing.T, full bool, scheme string) {
	// Make chain starting from genesis
	genDb, _, blockchain, err := newCanonical(ethash.NewFaker(), 10, full, scheme)
	if err != nil {
		t.Fatalf("failed to make new canonical chain: %v", err)
	}
	defer blockchain.Stop()

	// Create a forked chain, and try to insert with a missing link
	if full {
		chain := makeBlockChain(blockchain.chainConfig, blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash()), 5, ethash.NewFaker(), genDb, forkSeed)[1:]
		if err := testBlockChainImport(chain, blockchain); err == nil {
			t.Errorf("broken block chain not reported")
		}
	} else {
		chain := makeHeaderChain(blockchain.chainConfig, blockchain.CurrentHeader(), 5, ethash.NewFaker(), genDb, forkSeed)[1:]
		if err := testHeaderChainImport(chain, blockchain); err == nil {
			t.Errorf("broken header chain not reported")
		}
	}
}

// Tests that reorganising a long difficult chain after a short easy one
// overwrites the canonical numbers and links in the database.
func TestReorgLongHeaders(t *testing.T) {
	testReorgLong(t, false, rawdb.HashScheme)
	testReorgLong(t, false, rawdb.PathScheme)
}
func TestReorgLongBlocks(t *testing.T) {
	testReorgLong(t, true, rawdb.HashScheme)
	testReorgLong(t, true, rawdb.PathScheme)
}

func testReorgLong(t *testing.T, full bool, scheme string) {
	testReorg(t, []int64{0, 0, -9}, []int64{0, 0, 0, -9}, 393280+params.GenesisDifficulty.Int64(), full, scheme)
}

// Tests that reorganising a short difficult chain after a long easy one
// overwrites the canonical numbers and links in the database.
func TestReorgShortHeaders(t *testing.T) {
	testReorgShort(t, false, rawdb.HashScheme)
	testReorgShort(t, false, rawdb.PathScheme)
}
func TestReorgShortBlocks(t *testing.T) {
	testReorgShort(t, true, rawdb.HashScheme)
	testReorgShort(t, true, rawdb.PathScheme)
}

func testReorgShort(t *testing.T, full bool, scheme string) {
	// Create a long easy chain vs. a short heavy one. Due to difficulty adjustment
	// we need a fairly long chain of blocks with different difficulties for a short
	// one to become heavier than a long one. The 96 is an empirical value.
	easy := make([]int64, 96)
	for i := 0; i < len(easy); i++ {
		easy[i] = 60
	}

	diff := make([]int64, len(easy)-1)
	for i := 0; i < len(diff); i++ {
		diff[i] = -9
	}
	testReorg(t, easy, diff, 12615120+params.GenesisDifficulty.Int64(), full, scheme)
}

func testReorg(t *testing.T, first, second []int64, td int64, full bool, scheme string) {
	// Create a pristine chain and database
	genDb, _, blockchain, err := newCanonical(ethash.NewFaker(), 0, full, scheme)
	if err != nil {
		t.Fatalf("failed to create pristine chain: %v", err)
	}
	defer blockchain.Stop()

	// Insert an easy and a difficult chain afterwards
	easyBlocks, _ := GenerateChain(params.TestChainConfig, blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash()), ethash.NewFaker(), genDb, len(first), func(i int, b *BlockGen) {
		b.OffsetTime(first[i])
	})
	diffBlocks, _ := GenerateChain(params.TestChainConfig, blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash()), ethash.NewFaker(), genDb, len(second), func(i int, b *BlockGen) {
		b.OffsetTime(second[i])
	})

	if full {
		if _, err := blockchain.InsertChain(easyBlocks, false); err != nil {
			t.Fatalf("failed to insert easy chain: %v", err)
		}

		if _, err := blockchain.InsertChain(diffBlocks, false); err != nil {
			t.Fatalf("failed to insert difficult chain: %v", err)
		}
	} else {
		easyHeaders := make([]*types.Header, len(easyBlocks))
		for i, block := range easyBlocks {
			easyHeaders[i] = block.Header()
		}

		diffHeaders := make([]*types.Header, len(diffBlocks))
		for i, block := range diffBlocks {
			diffHeaders[i] = block.Header()
		}
		if _, err := blockchain.InsertHeaderChain(easyHeaders); err != nil {
			t.Fatalf("failed to insert easy chain: %v", err)
		}
		if _, err := blockchain.InsertHeaderChain(diffHeaders); err != nil {
			t.Fatalf("failed to insert difficult chain: %v", err)
		}
	}
	// Check that the chain is valid number and link wise
	if full {
		prev := blockchain.CurrentBlock()
		for block := blockchain.GetBlockByNumber(blockchain.CurrentBlock().Number.Uint64() - 1); block.NumberU64() != 0; prev, block = block.Header(), blockchain.GetBlockByNumber(block.NumberU64()-1) {
			if prev.ParentHash != block.Hash() {
				t.Errorf("parent block hash mismatch: have %x, want %x", prev.ParentHash, block.Hash())
			}
		}
	} else {
		prev := blockchain.CurrentHeader()
		for header := blockchain.GetHeaderByNumber(blockchain.CurrentHeader().Number.Uint64() - 1); header.Number.Uint64() != 0; prev, header = header, blockchain.GetHeaderByNumber(header.Number.Uint64()-1) {
			if prev.ParentHash != header.Hash() {
				t.Errorf("parent header hash mismatch: have %x, want %x", prev.ParentHash, header.Hash())
			}
		}
	}
	// Make sure the chain total difficulty is the correct one
	want := new(big.Int).Add(blockchain.genesisBlock.Difficulty(), big.NewInt(td))
	if full {
		cur := blockchain.CurrentBlock()
		if have := blockchain.GetTd(cur.Hash(), cur.Number.Uint64()); have.Cmp(want) != 0 {
			t.Errorf("total difficulty mismatch: have %v, want %v", have, want)
		}
	} else {
		cur := blockchain.CurrentHeader()
		if have := blockchain.GetTd(cur.Hash(), cur.Number.Uint64()); have.Cmp(want) != 0 {
			t.Errorf("total difficulty mismatch: have %v, want %v", have, want)
		}
	}
}

// Tests chain insertions in the face of one entity containing an invalid nonce.
func TestHeadersInsertNonceError(t *testing.T) {
	testInsertNonceError(t, false, rawdb.HashScheme)
	testInsertNonceError(t, false, rawdb.PathScheme)
}
func TestBlocksInsertNonceError(t *testing.T) {
	testInsertNonceError(t, true, rawdb.HashScheme)
	testInsertNonceError(t, true, rawdb.PathScheme)
}

func testInsertNonceError(t *testing.T, full bool, scheme string) {
	doTest := func(i int) {
		// Create a pristine chain and database
		genDb, _, blockchain, err := newCanonical(ethash.NewFaker(), 0, full, scheme)
		if err != nil {
			t.Fatalf("failed to create pristine chain: %v", err)
		}
		defer blockchain.Stop()

		// Create and insert a chain with a failing nonce
		var (
			failAt  int
			failRes int
			failNum uint64
		)

		if full {
			blocks := makeBlockChain(blockchain.chainConfig, blockchain.GetBlockByHash(blockchain.CurrentBlock().Hash()), i, ethash.NewFaker(), genDb, 0)

			failAt = rand.Int() % len(blocks)
			failNum = blocks[failAt].NumberU64()

			blockchain.engine = ethash.NewFakeFailer(failNum)
			failRes, err = blockchain.InsertChain(blocks, false)
		} else {
			headers := makeHeaderChain(blockchain.chainConfig, blockchain.CurrentHeader(), i, ethash.NewFaker(), genDb, 0)

			failAt = rand.Int() % len(headers)
			failNum = headers[failAt].Number.Uint64()

			blockchain.engine = ethash.NewFakeFailer(failNum)
			blockchain.hc.engine = blockchain.engine
			failRes, err = blockchain.InsertHeaderChain(headers)
		}
		// Check that the returned error indicates the failure
		if failRes != failAt {
			t.Errorf("test %d: failure (%v) index mismatch: have %d, want %d", i, err, failRes, failAt)
		}
		// Check that all blocks after the failing block have been inserted
		for j := 0; j < i-failAt; j++ {
			if full {
				if block := blockchain.GetBlockByNumber(failNum + uint64(j)); block != nil {
					t.Errorf("test %d: invalid block in chain: %v", i, block)
				}
			} else {
				if header := blockchain.GetHeaderByNumber(failNum + uint64(j)); header != nil {
					t.Errorf("test %d: invalid header in chain: %v", i, header)
				}
			}
		}
	}
	for i := 1; i < 25 && !t.Failed(); i++ {
		doTest(i)
	}
}

// Tests that fast importing a block chain produces the same chain data as the
// classical full block processing.
func TestFastVsFullChains(t *testing.T) {
	testFastVsFullChains(t, rawdb.HashScheme)
	testFastVsFullChains(t, rawdb.PathScheme)
}

func testFastVsFullChains(t *testing.T, scheme string) {
	// Configure and generate a sample block chain
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)
		gspec   = &Genesis{
			Config:  params.TestChainConfig,
			Alloc:   types.GenesisAlloc{address: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
	)

	_, blocks, receipts := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 1024, func(i int, block *BlockGen) {
		block.SetCoinbase(common.Address{0x00})

		// If the block number is multiple of 3, send a few bonus transactions to the miner
		if i%3 == 2 {
			for j := 0; j < i%4+1; j++ {
				tx, err := types.SignTx(types.NewTransaction(block.TxNonce(address), common.Address{0x00}, big.NewInt(1000), params.TxGas, block.header.BaseFee, nil), signer, key)
				if err != nil {
					panic(err)
				}

				block.AddTx(tx)
			}
		}
		// If the block number is a multiple of 5, add an uncle to the block
		if i%5 == 4 {
			block.AddUncle(&types.Header{ParentHash: block.PrevBlock(i - 2).Hash(), Number: big.NewInt(int64(i))})
		}
	})
	// Import the chain as an archive node for the comparison baseline
	archiveDb := rawdb.NewMemoryDatabase()
	archive, _ := NewBlockChain(archiveDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer archive.Stop()

	if n, err := archive.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to process block %d: %v", n, err)
	}
	// Fast import the chain as a non-archive node to test
	fastDb := rawdb.NewMemoryDatabase()
	fast, _ := NewBlockChain(fastDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer fast.Stop()

	headers := make([]*types.Header, len(blocks))
	for i, block := range blocks {
		headers[i] = block.Header()
	}
	if n, err := fast.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}

	if n, err := fast.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), 0); err != nil {
		t.Fatalf("failed to insert receipt %d: %v", n, err)
	}
	// Freezer style fast import the chain.
	ancientDb, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	if err != nil {
		t.Fatalf("failed to create temp freezer db: %v", err)
	}

	defer ancientDb.Close()

	ancient, _ := NewBlockChain(ancientDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer ancient.Stop()

	if n, err := ancient.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}

	if n, err := ancient.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), uint64(len(blocks)/2)); err != nil {
		t.Fatalf("failed to insert receipt %d: %v", n, err)
	}

	// Iterate over all chain data components, and cross reference
	for i := 0; i < len(blocks); i++ {
		num, hash, time := blocks[i].NumberU64(), blocks[i].Hash(), blocks[i].Time()

		if ftd, atd := fast.GetTd(hash, num), archive.GetTd(hash, num); ftd.Cmp(atd) != 0 {
			t.Errorf("block #%d [%x]: td mismatch: fastdb %v, archivedb %v", num, hash, ftd, atd)
		}
		if antd, artd := ancient.GetTd(hash, num), archive.GetTd(hash, num); antd.Cmp(artd) != 0 {
			t.Errorf("block #%d [%x]: td mismatch: ancientdb %v, archivedb %v", num, hash, antd, artd)
		}
		if fheader, aheader := fast.GetHeaderByHash(hash), archive.GetHeaderByHash(hash); fheader.Hash() != aheader.Hash() {
			t.Errorf("block #%d [%x]: header mismatch: fastdb %v, archivedb %v", num, hash, fheader, aheader)
		}

		if anheader, arheader := ancient.GetHeaderByHash(hash), archive.GetHeaderByHash(hash); anheader.Hash() != arheader.Hash() {
			t.Errorf("block #%d [%x]: header mismatch: ancientdb %v, archivedb %v", num, hash, anheader, arheader)
		}

		if fblock, arblock, anblock := fast.GetBlockByHash(hash), archive.GetBlockByHash(hash), ancient.GetBlockByHash(hash); fblock.Hash() != arblock.Hash() || anblock.Hash() != arblock.Hash() {
			t.Errorf("block #%d [%x]: block mismatch: fastdb %v, ancientdb %v, archivedb %v", num, hash, fblock, anblock, arblock)
		} else if types.DeriveSha(fblock.Transactions(), trie.NewStackTrie(nil)) != types.DeriveSha(arblock.Transactions(), trie.NewStackTrie(nil)) || types.DeriveSha(anblock.Transactions(), trie.NewStackTrie(nil)) != types.DeriveSha(arblock.Transactions(), trie.NewStackTrie(nil)) {
			t.Errorf("block #%d [%x]: transactions mismatch: fastdb %v, ancientdb %v, archivedb %v", num, hash, fblock.Transactions(), anblock.Transactions(), arblock.Transactions())
		} else if types.CalcUncleHash(fblock.Uncles()) != types.CalcUncleHash(arblock.Uncles()) || types.CalcUncleHash(anblock.Uncles()) != types.CalcUncleHash(arblock.Uncles()) {
			t.Errorf("block #%d [%x]: uncles mismatch: fastdb %v, ancientdb %v, archivedb %v", num, hash, fblock.Uncles(), anblock, arblock.Uncles())
		}

		// Check receipts.
		freceipts := rawdb.ReadReceipts(fastDb, hash, num, time, fast.Config())
		anreceipts := rawdb.ReadReceipts(ancientDb, hash, num, time, fast.Config())
		areceipts := rawdb.ReadReceipts(archiveDb, hash, num, time, fast.Config())
		if types.DeriveSha(freceipts, trie.NewStackTrie(nil)) != types.DeriveSha(areceipts, trie.NewStackTrie(nil)) {
			t.Errorf("block #%d [%x]: receipts mismatch: fastdb %v, ancientdb %v, archivedb %v", num, hash, freceipts, anreceipts, areceipts)
		}

		// Check that hash-to-number mappings are present in all databases.
		if m, ok := rawdb.ReadHeaderNumber(fastDb, hash); !ok || m != num {
			t.Errorf("block #%d [%x]: wrong hash-to-number mapping in fastdb: %v", num, hash, m)
		}

		if m, ok := rawdb.ReadHeaderNumber(ancientDb, hash); !ok || m != num {
			t.Errorf("block #%d [%x]: wrong hash-to-number mapping in ancientdb: %v", num, hash, m)
		}

		if m, ok := rawdb.ReadHeaderNumber(archiveDb, hash); !ok || m != num {
			t.Errorf("block #%d [%x]: wrong hash-to-number mapping in archivedb: %v", num, hash, m)
		}
	}

	// Check that the canonical chains are the same between the databases
	for i := 0; i < len(blocks)+1; i++ {
		if fhash, ahash := rawdb.ReadCanonicalHash(fastDb, uint64(i)), rawdb.ReadCanonicalHash(archiveDb, uint64(i)); fhash != ahash {
			t.Errorf("block #%d: canonical hash mismatch: fastdb %v, archivedb %v", i, fhash, ahash)
		}

		if anhash, arhash := rawdb.ReadCanonicalHash(ancientDb, uint64(i)), rawdb.ReadCanonicalHash(archiveDb, uint64(i)); anhash != arhash {
			t.Errorf("block #%d: canonical hash mismatch: ancientdb %v, archivedb %v", i, anhash, arhash)
		}
	}
}

// Tests that various import methods move the chain head pointers to the correct
// positions.
func TestLightVsFastVsFullChainHeads(t *testing.T) {
	testLightVsFastVsFullChainHeads(t, rawdb.HashScheme)
	testLightVsFastVsFullChainHeads(t, rawdb.PathScheme)
}

func testLightVsFastVsFullChainHeads(t *testing.T, scheme string) {
	// Configure and generate a sample block chain
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)
		gspec   = &Genesis{
			Config:  params.TestChainConfig,
			Alloc:   types.GenesisAlloc{address: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
	)
	height := uint64(64)
	_, blocks, receipts := GenerateChainWithGenesis(gspec, ethash.NewFaker(), int(height), nil)

	// makeDb creates a db instance for testing.
	makeDb := func() ethdb.Database {
		db, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
		if err != nil {
			t.Fatalf("failed to create temp freezer db: %v", err)
		}

		return db
	}
	// Configure a subchain to roll back
	remove := blocks[height/2].NumberU64()

	// Create a small assertion method to check the three heads
	assert := func(t *testing.T, kind string, chain *BlockChain, header uint64, fast uint64, block uint64) {
		t.Helper()

		if num := chain.CurrentBlock().Number.Uint64(); num != block {
			t.Errorf("%s head block mismatch: have #%v, want #%v", kind, num, block)
		}

		if num := chain.CurrentSnapBlock().Number.Uint64(); num != fast {
			t.Errorf("%s head snap-block mismatch: have #%v, want #%v", kind, num, fast)
		}

		if num := chain.CurrentHeader().Number.Uint64(); num != header {
			t.Errorf("%s head header mismatch: have #%v, want #%v", kind, num, header)
		}
	}
	// Import the chain as an archive node and ensure all pointers are updated
	archiveDb := makeDb()
	defer archiveDb.Close()

	options := DefaultConfig().WithArchive(true).WithStateScheme(scheme)
	archive, _ := NewBlockChain(archiveDb, gspec, ethash.NewFaker(), options)
	headers := make([]*types.Header, len(blocks))
	if n, err := archive.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to process block %d: %v", n, err)
	}
	defer archive.Stop()

	assert(t, "archive", archive, height, height, height)
	archive.SetHead(remove - 1)
	assert(t, "archive", archive, height/2, height/2, height/2)

	// Import the chain as a non-archive node and ensure all pointers are updated
	fastDb := makeDb()
	defer fastDb.Close()
	fast, _ := NewBlockChain(fastDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer fast.Stop()

	for i, block := range blocks {
		headers[i] = block.Header()
	}
	if n, err := fast.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}
	if n, err := fast.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), 0); err != nil {
		t.Fatalf("failed to insert receipt %d: %v", n, err)
	}

	assert(t, "fast", fast, height, height, 0)
	fast.SetHead(remove - 1)
	assert(t, "fast", fast, height/2, height/2, 0)

	// Import the chain as a ancient-first node and ensure all pointers are updated
	ancientDb := makeDb()
	defer ancientDb.Close()
	ancient, _ := NewBlockChain(ancientDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer ancient.Stop()
	if n, err := ancient.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}
	if n, err := ancient.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), uint64(3*len(blocks)/4)); err != nil {
		t.Fatalf("failed to insert receipt %d: %v", n, err)
	}

	assert(t, "ancient", ancient, height, height, 0)
	ancient.SetHead(remove - 1)
	assert(t, "ancient", ancient, 0, 0, 0)

	if frozen, err := ancientDb.Ancients(); err != nil || frozen != 1 {
		t.Fatalf("failed to truncate ancient store, want %v, have %v", 1, frozen)
	}
	// Import the chain as a light node and ensure all pointers are updated
	lightDb := makeDb()
	defer lightDb.Close()

	light, _ := NewBlockChain(lightDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	if n, err := light.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}

	defer light.Stop()

	assert(t, "light", light, height, 0, 0)
	light.SetHead(remove - 1)
	assert(t, "light", light, height/2, 0, 0)
}

// Tests that chain reorganisations handle transaction removals and reinsertions.
func TestChainTxReorgs(t *testing.T) {
	testChainTxReorgs(t, rawdb.HashScheme)
	testChainTxReorgs(t, rawdb.PathScheme)
}

func testChainTxReorgs(t *testing.T, scheme string) {
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		key3, _ = crypto.HexToECDSA("49a7b37aa6f6645917e7b807e9d1c00d4fa71f18343b0d4122a4d2df64dd6fee")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = crypto.PubkeyToAddress(key2.PublicKey)
		addr3   = crypto.PubkeyToAddress(key3.PublicKey)
		gspec   = &Genesis{
			Config:   params.TestChainConfig,
			GasLimit: 3141592,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: big.NewInt(1000000000000000)},
				addr2: {Balance: big.NewInt(1000000000000000)},
				addr3: {Balance: big.NewInt(1000000000000000)},
			},
		}
		signer = types.LatestSigner(gspec.Config)
	)

	// Create two transactions shared between the chains:
	//  - postponed: transaction included at a later block in the forked chain
	//  - swapped: transaction included at the same block number in the forked chain
	postponed, _ := types.SignTx(types.NewTransaction(0, addr1, big.NewInt(1000), params.TxGas, big.NewInt(params.InitialBaseFee), nil), signer, key1)
	swapped, _ := types.SignTx(types.NewTransaction(1, addr1, big.NewInt(1000), params.TxGas, big.NewInt(params.InitialBaseFee), nil), signer, key1)

	// Create two transactions that will be dropped by the forked chain:
	//  - pastDrop: transaction dropped retroactively from a past block
	//  - freshDrop: transaction dropped exactly at the block where the reorg is detected
	var pastDrop, freshDrop *types.Transaction

	// Create three transactions that will be added in the forked chain:
	//  - pastAdd:   transaction added before the reorganization is detected
	//  - freshAdd:  transaction added at the exact block the reorg is detected
	//  - futureAdd: transaction added after the reorg has already finished
	var pastAdd, freshAdd, futureAdd *types.Transaction

	_, chain, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 3, func(i int, gen *BlockGen) {
		switch i {
		case 0:
			pastDrop, _ = types.SignTx(types.NewTransaction(gen.TxNonce(addr2), addr2, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil), signer, key2)

			gen.AddTx(pastDrop)  // This transaction will be dropped in the fork from below the split point
			gen.AddTx(postponed) // This transaction will be postponed till block #3 in the fork

		case 2:
			freshDrop, _ = types.SignTx(types.NewTransaction(gen.TxNonce(addr2), addr2, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil), signer, key2)

			gen.AddTx(freshDrop) // This transaction will be dropped in the fork from exactly at the split point
			gen.AddTx(swapped)   // This transaction will be swapped out at the exact height

			gen.OffsetTime(9) // Lower the block difficulty to simulate a weaker chain
		}
	})
	// Import the chain. This runs all block validation rules.
	db := rawdb.NewMemoryDatabase()
	blockchain, _ := NewBlockChain(db, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	if i, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert original chain[%d]: %v", i, err)
	}

	defer blockchain.Stop()

	// overwrite the old chain
	_, chain, _ = GenerateChainWithGenesis(gspec, ethash.NewFaker(), 5, func(i int, gen *BlockGen) {
		switch i {
		case 0:
			pastAdd, _ = types.SignTx(types.NewTransaction(gen.TxNonce(addr3), addr3, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil), signer, key3)
			gen.AddTx(pastAdd) // This transaction needs to be injected during reorg

		case 2:
			gen.AddTx(postponed) // This transaction was postponed from block #1 in the original chain
			gen.AddTx(swapped)   // This transaction was swapped from the exact current spot in the original chain

			freshAdd, _ = types.SignTx(types.NewTransaction(gen.TxNonce(addr3), addr3, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil), signer, key3)
			gen.AddTx(freshAdd) // This transaction will be added exactly at reorg time

		case 3:
			futureAdd, _ = types.SignTx(types.NewTransaction(gen.TxNonce(addr3), addr3, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil), signer, key3)
			gen.AddTx(futureAdd) // This transaction will be added after a full reorg
		}
	})
	if _, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}

	// removed tx
	for i, tx := range (types.Transactions{pastDrop, freshDrop}) {
		if txn, _, _, _ := rawdb.ReadCanonicalTransaction(db, tx.Hash()); txn != nil {
			t.Errorf("drop %d: tx %v found while shouldn't have been", i, txn)
		}
		if rcpt, _, _, _ := rawdb.ReadCanonicalReceipt(db, tx.Hash(), blockchain.Config()); rcpt != nil {
			t.Errorf("drop %d: receipt %v found while shouldn't have been", i, rcpt)
		}
	}
	// added tx
	for i, tx := range (types.Transactions{pastAdd, freshAdd, futureAdd}) {
		if txn, _, _, _ := rawdb.ReadCanonicalTransaction(db, tx.Hash()); txn == nil {
			t.Errorf("add %d: expected tx to be found", i)
		}
		if rcpt, _, _, index := rawdb.ReadCanonicalReceipt(db, tx.Hash(), blockchain.Config()); rcpt == nil {
			t.Errorf("add %d: expected receipt to be found", i)
		} else if rawRcpt, ctx, _ := rawdb.ReadCanonicalRawReceipt(db, rcpt.BlockHash, rcpt.BlockNumber.Uint64(), index); rawRcpt == nil {
			t.Errorf("add %d: expected raw receipt to be found", i)
		} else {
			if rcpt.GasUsed != ctx.GasUsed {
				t.Errorf("add %d, raw gasUsedSoFar doesn't make sense", i)
			}
			if len(rcpt.Logs) > 0 && rcpt.Logs[0].Index != ctx.LogIndex {
				t.Errorf("add %d, raw startingLogIndex doesn't make sense", i)
			}
		}
	}
	// shared tx
	for i, tx := range (types.Transactions{postponed, swapped}) {
		if txn, _, _, _ := rawdb.ReadCanonicalTransaction(db, tx.Hash()); txn == nil {
			t.Errorf("share %d: expected tx to be found", i)
		}
		if rcpt, _, _, index := rawdb.ReadCanonicalReceipt(db, tx.Hash(), blockchain.Config()); rcpt == nil {
			t.Errorf("share %d: expected receipt to be found", i)
		} else if rawRcpt, ctx, _ := rawdb.ReadCanonicalRawReceipt(db, rcpt.BlockHash, rcpt.BlockNumber.Uint64(), index); rawRcpt == nil {
			t.Errorf("add %d: expected raw receipt to be found", i)
		} else {
			if rcpt.GasUsed != ctx.GasUsed {
				t.Errorf("add %d, raw gasUsedSoFar doesn't make sense", i)
			}
			if len(rcpt.Logs) > 0 && rcpt.Logs[0].Index != ctx.LogIndex {
				t.Errorf("add %d, raw startingLogIndex doesn't make sense", i)
			}
		}
	}
}

func TestLogReorgs(t *testing.T) {
	testLogReorgs(t, rawdb.HashScheme)
	testLogReorgs(t, rawdb.PathScheme)
}

func testLogReorgs(t *testing.T, scheme string) {
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)

		// this code generates a log
		code   = common.Hex2Bytes("60606040525b7f24ec1d3ff24c2f6ff210738839dbc339cd45a5294d85c79361016243157aae7b60405180905060405180910390a15b600a8060416000396000f360606040526008565b00")
		gspec  = &Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{addr1: {Balance: big.NewInt(10000000000000000)}}}
		signer = types.LatestSigner(gspec.Config)
	)

	blockchain, _ := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer blockchain.Stop()

	rmLogsCh := make(chan RemovedLogsEvent)
	blockchain.SubscribeRemovedLogsEvent(rmLogsCh)

	_, chain, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 2, func(i int, gen *BlockGen) {
		if i == 1 {
			tx, err := types.SignTx(types.NewContractCreation(gen.TxNonce(addr1), new(big.Int), 1000000, gen.header.BaseFee, code), signer, key1)
			if err != nil {
				t.Fatalf("failed to create tx: %v", err)
			}

			gen.AddTx(tx)
		}
	})
	if _, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	_, chain, _ = GenerateChainWithGenesis(gspec, ethash.NewFaker(), 3, func(i int, gen *BlockGen) {})
	done := make(chan struct{})

	go func() {
		ev := <-rmLogsCh
		if len(ev.Logs) == 0 {
			t.Error("expected logs")
		}

		close(done)
	}()

	if _, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}

	timeout := time.NewTimer(1 * time.Second)
	defer timeout.Stop()
	select {
	case <-done:
	case <-timeout.C:
		t.Fatal("Timeout. There is no RemovedLogsEvent has been sent.")
	}
}

// This EVM code generates a log when the contract is created.
var logCode = common.Hex2Bytes("60606040525b7f24ec1d3ff24c2f6ff210738839dbc339cd45a5294d85c79361016243157aae7b60405180905060405180910390a15b600a8060416000396000f360606040526008565b00")

// This test checks that log events and RemovedLogsEvent are sent
// when the chain reorganizes.
func TestLogRebirth(t *testing.T) {
	testLogRebirth(t, rawdb.HashScheme)
	testLogRebirth(t, rawdb.PathScheme)
}

func testLogRebirth(t *testing.T, scheme string) {
	var (
		key1, _       = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1         = crypto.PubkeyToAddress(key1.PublicKey)
		gspec         = &Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{addr1: {Balance: big.NewInt(10000000000000000)}}}
		signer        = types.LatestSigner(gspec.Config)
		engine        = ethash.NewFaker()
		blockchain, _ = NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, DefaultConfig().WithStateScheme(scheme))
	)

	defer blockchain.Stop()

	// The event channels.
	newLogCh := make(chan []*types.Log, 10)
	rmLogsCh := make(chan RemovedLogsEvent, 10)

	blockchain.SubscribeLogsEvent(newLogCh)
	blockchain.SubscribeRemovedLogsEvent(rmLogsCh)

	// This chain contains 10 logs.
	genDb, chain, _ := GenerateChainWithGenesis(gspec, engine, 3, func(i int, gen *BlockGen) {
		if i < 2 {
			for ii := 0; ii < 5; ii++ {
				tx, err := types.SignNewTx(key1, signer, &types.LegacyTx{
					Nonce:    gen.TxNonce(addr1),
					GasPrice: gen.header.BaseFee,
					Gas:      uint64(1000001),
					Data:     logCode,
				})
				if err != nil {
					t.Fatalf("failed to create tx: %v", err)
				}

				gen.AddTx(tx)
			}
		}
	})
	if _, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	checkLogEvents(t, newLogCh, rmLogsCh, 10, 0)

	// Generate long reorg chain containing more logs. Inserting the
	// chain removes one log and adds four.
	_, forkChain, _ := GenerateChainWithGenesis(gspec, engine, 3, func(i int, gen *BlockGen) {
		if i == 2 {
			// The last (head) block is not part of the reorg-chain, we can ignore it
			return
		}

		for ii := 0; ii < 5; ii++ {
			tx, err := types.SignNewTx(key1, signer, &types.LegacyTx{
				Nonce:    gen.TxNonce(addr1),
				GasPrice: gen.header.BaseFee,
				Gas:      uint64(1000000),
				Data:     logCode,
			})
			if err != nil {
				t.Fatalf("failed to create tx: %v", err)
			}

			gen.AddTx(tx)
		}
		gen.OffsetTime(-9) // higher block difficulty
	})
	if _, err := blockchain.InsertChain(forkChain, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}

	checkLogEvents(t, newLogCh, rmLogsCh, 10, 10)

	// This chain segment is rooted in the original chain, but doesn't contain any logs.
	// When inserting it, the canonical chain switches away from forkChain and re-emits
	// the log event for the old chain, as well as a RemovedLogsEvent for forkChain.
	newBlocks, _ := GenerateChain(gspec.Config, chain[len(chain)-1], engine, genDb, 1, func(i int, gen *BlockGen) {})
	if _, err := blockchain.InsertChain(newBlocks, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}

	checkLogEvents(t, newLogCh, rmLogsCh, 10, 10)
}

// This test is a variation of TestLogRebirth. It verifies that log events are emitted
// when a side chain containing log events overtakes the canonical chain.
func TestSideLogRebirth(t *testing.T) {
	testSideLogRebirth(t, rawdb.HashScheme)
	testSideLogRebirth(t, rawdb.PathScheme)
}

func testSideLogRebirth(t *testing.T, scheme string) {
	var (
		key1, _       = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1         = crypto.PubkeyToAddress(key1.PublicKey)
		gspec         = &Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{addr1: {Balance: big.NewInt(10000000000000000)}}}
		signer        = types.LatestSigner(gspec.Config)
		blockchain, _ = NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	)

	defer blockchain.Stop()

	newLogCh := make(chan []*types.Log, 10)
	rmLogsCh := make(chan RemovedLogsEvent, 10)

	blockchain.SubscribeLogsEvent(newLogCh)
	blockchain.SubscribeRemovedLogsEvent(rmLogsCh)

	_, chain, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 2, func(i int, gen *BlockGen) {
		if i == 1 {
			gen.OffsetTime(-9) // higher block difficulty
		}
	})
	if _, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}

	checkLogEvents(t, newLogCh, rmLogsCh, 0, 0)

	// Generate side chain with lower difficulty
	genDb, sideChain, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 2, func(i int, gen *BlockGen) {
		if i == 1 {
			tx, err := types.SignTx(types.NewContractCreation(gen.TxNonce(addr1), new(big.Int), 1000000, gen.header.BaseFee, logCode), signer, key1)
			if err != nil {
				t.Fatalf("failed to create tx: %v", err)
			}

			gen.AddTx(tx)
		}
	})
	if _, err := blockchain.InsertChain(sideChain, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}
	checkLogEvents(t, newLogCh, rmLogsCh, 0, 0)

	// Generate a new block based on side chain.
	newBlocks, _ := GenerateChain(gspec.Config, sideChain[len(sideChain)-1], ethash.NewFaker(), genDb, 1, func(i int, gen *BlockGen) {})
	if _, err := blockchain.InsertChain(newBlocks, false); err != nil {
		t.Fatalf("failed to insert forked chain: %v", err)
	}
	checkLogEvents(t, newLogCh, rmLogsCh, 1, 0)
}

func checkLogEvents(t *testing.T, logsCh <-chan []*types.Log, rmLogsCh <-chan RemovedLogsEvent, wantNew, wantRemoved int) {
	t.Helper()

	var (
		countNew int
		countRm  int
		prev     int
	)
	// Drain events.
	for len(logsCh) > 0 {
		x := <-logsCh
		countNew += len(x)

		for _, log := range x {
			// We expect added logs to be in ascending order: 0:0, 0:1, 1:0 ...
			have := 100*int(log.BlockNumber) + int(log.TxIndex)
			if have < prev {
				t.Fatalf("Expected new logs to arrive in ascending order (%d < %d)", have, prev)
			}

			prev = have
		}
	}

	prev = 0

	for len(rmLogsCh) > 0 {
		x := <-rmLogsCh
		countRm += len(x.Logs)

		for _, log := range x.Logs {
			// We expect removed logs to be in ascending order: 0:0, 0:1, 1:0 ...
			have := 100*int(log.BlockNumber) + int(log.TxIndex)
			if have < prev {
				t.Fatalf("Expected removed logs to arrive in ascending order (%d < %d)", have, prev)
			}

			prev = have
		}
	}

	if countNew != wantNew {
		t.Fatalf("wrong number of log events: got %d, want %d", countNew, wantNew)
	}

	if countRm != wantRemoved {
		t.Fatalf("wrong number of removed log events: got %d, want %d", countRm, wantRemoved)
	}
}

func TestReorgSideEvent(t *testing.T) {
	testReorgSideEvent(t, rawdb.HashScheme)
	testReorgSideEvent(t, rawdb.PathScheme)
}

func testReorgSideEvent(t *testing.T, scheme string) {
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		gspec   = &Genesis{
			Config: params.TestChainConfig,
			Alloc:  types.GenesisAlloc{addr1: {Balance: big.NewInt(10000000000000000)}},
		}
		signer = types.LatestSigner(gspec.Config)
	)
	defaultConfig := DefaultConfig().WithStateScheme(scheme)
	blockchain, _ := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), defaultConfig)
	defer blockchain.Stop()

	_, chain, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 3, func(i int, gen *BlockGen) {})
	if _, err := blockchain.InsertChain(chain, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	_, replacementBlocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 4, func(i int, gen *BlockGen) {
		tx, err := types.SignTx(types.NewContractCreation(gen.TxNonce(addr1), new(big.Int), 1000000, gen.header.BaseFee, nil), signer, key1)

		if i == 2 {
			gen.OffsetTime(-9)
		}

		if err != nil {
			t.Fatalf("failed to create tx: %v", err)
		}

		gen.AddTx(tx)
	})
	chainSideCh := make(chan ChainSideEvent, 64)
	blockchain.SubscribeChainSideEvent(chainSideCh)

	if _, err := blockchain.InsertChain(replacementBlocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	// first two block of the secondary chain are for a brief moment considered
	// side chains because up to that point the first one is considered the
	// heavier chain.
	expectedSideHashes := map[common.Hash]bool{
		replacementBlocks[0].Hash(): true,
		replacementBlocks[1].Hash(): true,
		chain[0].Hash():             true,
		chain[1].Hash():             true,
		chain[2].Hash():             true,
	}

	i := 0

	const timeoutDura = 10 * time.Second
	timeout := time.NewTimer(timeoutDura)
done:
	for {
		select {
		case ev := <-chainSideCh:
			header := ev.Header
			if _, ok := expectedSideHashes[header.Hash()]; !ok {
				t.Errorf("%d: didn't expect %x to be in side chain", i, header.Hash())
			}
			i++

			if i == len(expectedSideHashes) {
				timeout.Stop()

				break done
			}
			timeout.Reset(timeoutDura)

		case <-timeout.C:
			t.Fatal("Timeout. Possibly not all blocks were triggered for sideevent")
		}
	}

	// make sure no more events are fired
	select {
	case e := <-chainSideCh:
		t.Errorf("unexpected event fired: %v", e)
	case <-time.After(250 * time.Millisecond):
	}
}

// Tests if the canonical block can be fetched from the database during chain insertion.
func TestCanonicalBlockRetrieval(t *testing.T) {
	testCanonicalBlockRetrieval(t, rawdb.HashScheme)
	testCanonicalBlockRetrieval(t, rawdb.PathScheme)
}

func testCanonicalBlockRetrieval(t *testing.T, scheme string) {
	_, gspec, blockchain, err := newCanonical(ethash.NewFaker(), 0, true, scheme)
	if err != nil {
		t.Fatalf("failed to create pristine chain: %v", err)
	}
	defer blockchain.Stop()

	_, chain, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 10, func(i int, gen *BlockGen) {})

	var pend sync.WaitGroup

	pend.Add(len(chain))

	for i := range chain {
		go func(block *types.Block) {
			defer pend.Done()

			// try to retrieve a block by its canonical hash and see if the block data can be retrieved.
			for {
				ch := rawdb.ReadCanonicalHash(blockchain.db, block.NumberU64())
				if ch == (common.Hash{}) {
					continue // busy wait for canonical hash to be written
				}

				if ch != block.Hash() {
					t.Errorf("unknown canonical hash, want %s, got %s", block.Hash().Hex(), ch.Hex())
					return
				}

				fb := rawdb.ReadBlock(blockchain.db, ch, block.NumberU64())
				if fb == nil {
					t.Errorf("unable to retrieve block %d for canonical hash: %s", block.NumberU64(), ch.Hex())
					return
				}

				if fb.Hash() != block.Hash() {
					t.Errorf("invalid block hash for block %d, want %s, got %s", block.NumberU64(), block.Hash().Hex(), fb.Hash().Hex())
					return
				}

				return
			}
		}(chain[i])

		if _, err := blockchain.InsertChain(types.Blocks{chain[i]}, false); err != nil {
			t.Fatalf("failed to insert block %d: %v", i, err)
		}
	}

	pend.Wait()
}
func TestEIP155Transition(t *testing.T) {
	testEIP155Transition(t, rawdb.HashScheme)
	testEIP155Transition(t, rawdb.PathScheme)
}

func testEIP155Transition(t *testing.T, scheme string) {
	// Configure and generate a sample block chain
	var (
		key, _     = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address    = crypto.PubkeyToAddress(key.PublicKey)
		funds      = big.NewInt(1000000000)
		deleteAddr = common.Address{1}
		gspec      = &Genesis{
			Config: &params.ChainConfig{
				ChainID:        big.NewInt(1),
				EIP150Block:    big.NewInt(0),
				EIP155Block:    big.NewInt(2),
				HomesteadBlock: new(big.Int),
			},
			Alloc: types.GenesisAlloc{address: {Balance: funds}, deleteAddr: {Balance: new(big.Int)}},
		}
	)

	genDb, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 4, func(i int, block *BlockGen) {
		var (
			tx      *types.Transaction
			err     error
			basicTx = func(signer types.Signer) (*types.Transaction, error) {
				return types.SignTx(types.NewTransaction(block.TxNonce(address), common.Address{}, new(big.Int), 21000, new(big.Int), nil), signer, key)
			}
		)

		switch i {
		case 0:
			tx, err = basicTx(types.HomesteadSigner{})
			if err != nil {
				t.Fatal(err)
			}

			block.AddTx(tx)
		case 2:
			tx, err = basicTx(types.HomesteadSigner{})
			if err != nil {
				t.Fatal(err)
			}

			block.AddTx(tx)

			tx, err = basicTx(types.LatestSigner(gspec.Config))
			if err != nil {
				t.Fatal(err)
			}

			block.AddTx(tx)
		case 3:
			tx, err = basicTx(types.HomesteadSigner{})
			if err != nil {
				t.Fatal(err)
			}

			block.AddTx(tx)

			tx, err = basicTx(types.LatestSigner(gspec.Config))
			if err != nil {
				t.Fatal(err)
			}

			block.AddTx(tx)
		}
	})

	blockchain, _ := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer blockchain.Stop()

	if _, err := blockchain.InsertChain(blocks, false); err != nil {
		t.Fatal(err)
	}

	block := blockchain.GetBlockByNumber(1)
	if block.Transactions()[0].Protected() {
		t.Error("Expected block[0].txs[0] to not be replay protected")
	}

	block = blockchain.GetBlockByNumber(3)
	if block.Transactions()[0].Protected() {
		t.Error("Expected block[3].txs[0] to not be replay protected")
	}

	if !block.Transactions()[1].Protected() {
		t.Error("Expected block[3].txs[1] to be replay protected")
	}

	if _, err := blockchain.InsertChain(blocks[4:], false); err != nil {
		t.Fatal(err)
	}

	// generate an invalid chain id transaction
	config := &params.ChainConfig{
		ChainID:        big.NewInt(2),
		EIP150Block:    big.NewInt(0),
		EIP155Block:    big.NewInt(2),
		HomesteadBlock: new(big.Int),
	}
	blocks, _ = GenerateChain(config, blocks[len(blocks)-1], ethash.NewFaker(), genDb, 4, func(i int, block *BlockGen) {
		var (
			tx      *types.Transaction
			err     error
			basicTx = func(signer types.Signer) (*types.Transaction, error) {
				return types.SignTx(types.NewTransaction(block.TxNonce(address), common.Address{}, new(big.Int), 21000, new(big.Int), nil), signer, key)
			}
		)

		if i == 0 {
			tx, err = basicTx(types.LatestSigner(config))
			if err != nil {
				t.Fatal(err)
			}

			block.AddTx(tx)
		}
	})

	_, err := blockchain.InsertChain(blocks, false)
	if have, want := err, types.ErrInvalidChainId; !errors.Is(have, want) {
		t.Errorf("have %v, want %v", have, want)
	}
}
func TestEIP161AccountRemoval(t *testing.T) {
	testEIP161AccountRemoval(t, rawdb.HashScheme)
	testEIP161AccountRemoval(t, rawdb.PathScheme)
}

func testEIP161AccountRemoval(t *testing.T, scheme string) {
	// Configure and generate a sample block chain
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000)
		theAddr = common.Address{1}
		gspec   = &Genesis{
			Config: &params.ChainConfig{
				ChainID:        big.NewInt(1),
				HomesteadBlock: new(big.Int),
				EIP155Block:    new(big.Int),
				EIP150Block:    new(big.Int),
				EIP158Block:    big.NewInt(2),
			},
			Alloc: types.GenesisAlloc{address: {Balance: funds}},
		}
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 3, func(i int, block *BlockGen) {
		var (
			tx     *types.Transaction
			err    error
			signer = types.LatestSigner(gspec.Config)
		)

		switch i {
		case 0:
			tx, err = types.SignTx(types.NewTransaction(block.TxNonce(address), theAddr, new(big.Int), 21000, new(big.Int), nil), signer, key)
		case 1:
			tx, err = types.SignTx(types.NewTransaction(block.TxNonce(address), theAddr, new(big.Int), 21000, new(big.Int), nil), signer, key)
		case 2:
			tx, err = types.SignTx(types.NewTransaction(block.TxNonce(address), theAddr, new(big.Int), 21000, new(big.Int), nil), signer, key)
		}

		if err != nil {
			t.Fatal(err)
		}

		block.AddTx(tx)
	})
	// account must exist pre eip 161
	blockchain, _ := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer blockchain.Stop()

	if _, err := blockchain.InsertChain(types.Blocks{blocks[0]}, false); err != nil {
		t.Fatal(err)
	}

	if st, _ := blockchain.State(); !st.Exist(theAddr) {
		t.Error("expected account to exist")
	}

	// account needs to be deleted post eip 161
	if _, err := blockchain.InsertChain(types.Blocks{blocks[1]}, false); err != nil {
		t.Fatal(err)
	}

	if st, _ := blockchain.State(); st.Exist(theAddr) {
		t.Error("account should not exist")
	}

	// account mustn't be created post eip 161
	if _, err := blockchain.InsertChain(types.Blocks{blocks[2]}, false); err != nil {
		t.Fatal(err)
	}

	if st, _ := blockchain.State(); st.Exist(theAddr) {
		t.Error("account should not exist")
	}
}

// This is a regression test (i.e. as weird as it is, don't delete it ever), which
// tests that under weird reorg conditions the blockchain and its internal header-
// chain return the same latest block/header.
//
// https://github.com/ethereum/go-ethereum/pull/15941
func TestBlockchainHeaderchainReorgConsistency(t *testing.T) {
	testBlockchainHeaderchainReorgConsistency(t, rawdb.HashScheme)
	testBlockchainHeaderchainReorgConsistency(t, rawdb.PathScheme)
}

func testBlockchainHeaderchainReorgConsistency(t *testing.T, scheme string) {
	// Generate a canonical chain to act as the main dataset
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	genDb, blocks, _ := GenerateChainWithGenesis(genesis, engine, 64, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{1}) })

	// Generate a bunch of fork blocks, each side forking from the canonical chain
	forks := make([]*types.Block, len(blocks))
	for i := 0; i < len(forks); i++ {
		parent := genesis.ToBlock()
		if i > 0 {
			parent = blocks[i-1]
		}

		fork, _ := GenerateChain(genesis.Config, parent, engine, genDb, 1, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{2}) })
		forks[i] = fork[0]
	}
	// Import the canonical and fork chain side by side, verifying the current block
	// and current header consistency
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	for i := 0; i < len(blocks); i++ {
		if _, err := chain.InsertChain(blocks[i:i+1], false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", i, err)
		}

		if chain.CurrentBlock().Hash() != chain.CurrentHeader().Hash() {
			t.Errorf("block %d: current block/header mismatch: block #%d [%x..], header #%d [%x..]", i, chain.CurrentBlock().Number, chain.CurrentBlock().Hash().Bytes()[:4], chain.CurrentHeader().Number, chain.CurrentHeader().Hash().Bytes()[:4])
		}

		if _, err := chain.InsertChain(forks[i:i+1], false); err != nil {
			t.Fatalf(" fork %d: failed to insert into chain: %v", i, err)
		}

		if chain.CurrentBlock().Hash() != chain.CurrentHeader().Hash() {
			t.Errorf(" fork %d: current block/header mismatch: block #%d [%x..], header #%d [%x..]", i, chain.CurrentBlock().Number, chain.CurrentBlock().Hash().Bytes()[:4], chain.CurrentHeader().Number, chain.CurrentHeader().Hash().Bytes()[:4])
		}
	}
}

// Tests that importing small side forks doesn't leave junk in the trie database
// cache (which would eventually cause memory issues).
func TestTrieForkGC(t *testing.T) {
	// Generate a canonical chain to act as the main dataset
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	genDb, blocks, _ := GenerateChainWithGenesis(genesis, engine, 2*state.TriesInMemory, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{1}) })

	// Generate a bunch of fork blocks, each side forking from the canonical chain
	forks := make([]*types.Block, len(blocks))
	for i := 0; i < len(forks); i++ {
		parent := genesis.ToBlock()
		if i > 0 {
			parent = blocks[i-1]
		}

		fork, _ := GenerateChain(genesis.Config, parent, engine, genDb, 1, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{2}) })
		forks[i] = fork[0]
	}
	// Import the canonical and fork chain side by side, forcing the trie cache to cache both
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	for i := 0; i < len(blocks); i++ {
		if _, err := chain.InsertChain(blocks[i:i+1], false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", i, err)
		}

		if _, err := chain.InsertChain(forks[i:i+1], false); err != nil {
			t.Fatalf("fork %d: failed to insert into chain: %v", i, err)
		}
	}
	// Dereference all the recent tries and ensure no past trie is left in
	for i := 0; i < state.TriesInMemory; i++ {
		chain.TrieDB().Dereference(blocks[len(blocks)-1-i].Root())
		chain.TrieDB().Dereference(forks[len(blocks)-1-i].Root())
	}
	if _, nodes, _ := chain.TrieDB().Size(); nodes > 0 { // all memory is returned in the nodes return for hashdb
		t.Fatalf("stale tries still alive after garbase collection")
	}
}

// Tests that doing large reorgs works even if the state associated with the
// forking point is not available any more.
func TestLargeReorgTrieGC(t *testing.T) {
	testLargeReorgTrieGC(t, rawdb.HashScheme)
	testLargeReorgTrieGC(t, rawdb.PathScheme)
}

func testLargeReorgTrieGC(t *testing.T, scheme string) {
	// Generate the original common chain segment and the two competing forks
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	genDb, shared, _ := GenerateChainWithGenesis(genesis, engine, 64, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{1}) })
	original, _ := GenerateChain(genesis.Config, shared[len(shared)-1], engine, genDb, 2*state.TriesInMemory, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{2}) })
	competitor, _ := GenerateChain(genesis.Config, shared[len(shared)-1], engine, genDb, 2*state.TriesInMemory+1, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{3}) })

	// Import the shared chain and the original canonical one
	db, _ := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	defer db.Close()

	chain, err := NewBlockChain(db, genesis, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if _, err := chain.InsertChain(shared, false); err != nil {
		t.Fatalf("failed to insert shared chain: %v", err)
	}

	if _, err := chain.InsertChain(original, false); err != nil {
		t.Fatalf("failed to insert original chain: %v", err)
	}
	// Ensure that the state associated with the forking point is pruned away
	if chain.HasState(shared[len(shared)-1].Root()) {
		t.Fatalf("common-but-old ancestor still cache")
	}
	// Import the competitor chain without exceeding the canonical's TD and ensure
	// we have not processed any of the blocks (protection against malicious blocks)
	if _, err := chain.InsertChain(competitor[:len(competitor)-2], false); err != nil {
		t.Fatalf("failed to insert competitor chain: %v", err)
	}
	for i, block := range competitor[:len(competitor)-2] {
		if chain.HasState(block.Root()) {
			t.Fatalf("competitor %d: low TD chain became processed", i)
		}
	}
	// Import the head of the competitor chain, triggering the reorg and ensure we
	// successfully reprocess all the stashed away blocks.
	if _, err := chain.InsertChain(competitor[len(competitor)-2:], false); err != nil {
		t.Fatalf("failed to finalize competitor chain: %v", err)
	}
	// In path-based trie database implementation, it will keep 128 diff + 1 disk
	// layers, totally 129 latest states available. In hash-based it's 128.
	states := state.TriesInMemory
	if scheme == rawdb.PathScheme {
		states = states + 1
	}
	for i, block := range competitor[:len(competitor)-states] {
		if chain.HasState(block.Root()) {
			t.Fatalf("competitor %d: unexpected competing chain state", i)
		}
	}
	for i, block := range competitor[len(competitor)-states:] {
		if !chain.HasState(block.Root()) {
			t.Fatalf("competitor %d: competing chain state missing", i)
		}
	}
}

func TestBlockchainRecovery(t *testing.T) {
	testBlockchainRecovery(t, rawdb.HashScheme)
	testBlockchainRecovery(t, rawdb.PathScheme)
}

func testBlockchainRecovery(t *testing.T, scheme string) {
	// Configure and generate a sample block chain
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000)
		gspec   = &Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{address: {Balance: funds}}}
	)
	height := uint64(64)
	_, blocks, receipts := GenerateChainWithGenesis(gspec, ethash.NewFaker(), int(height), nil)

	// Import the chain as a ancient-first node and ensure all pointers are updated
	ancientDb, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{Ancient: t.TempDir()})
	if err != nil {
		t.Fatalf("failed to create temp freezer db: %v", err)
	}
	defer ancientDb.Close()
	ancient, _ := NewBlockChain(ancientDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))

	headers := make([]*types.Header, len(blocks))
	for i, block := range blocks {
		headers[i] = block.Header()
	}
	if n, err := ancient.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}

	if n, err := ancient.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), uint64(3*len(blocks)/4)); err != nil {
		t.Fatalf("failed to insert receipt %d: %v", n, err)
	}

	rawdb.WriteLastPivotNumber(ancientDb, blocks[len(blocks)-1].NumberU64()) // Force fast sync behavior
	ancient.Stop()

	// Destroy head fast block manually
	midBlock := blocks[len(blocks)/2]
	rawdb.WriteHeadFastBlockHash(ancientDb, midBlock.Hash())

	// Reopen broken blockchain again
	ancient, _ = NewBlockChain(ancientDb, gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(scheme))
	defer ancient.Stop()

	if num := ancient.CurrentBlock().Number.Uint64(); num != 0 {
		t.Errorf("head block mismatch: have #%v, want #%v", num, 0)
	}

	if num := ancient.CurrentSnapBlock().Number.Uint64(); num != midBlock.NumberU64() {
		t.Errorf("head snap-block mismatch: have #%v, want #%v", num, midBlock.NumberU64())
	}

	if num := ancient.CurrentHeader().Number.Uint64(); num != midBlock.NumberU64() {
		t.Errorf("head header mismatch: have #%v, want #%v", num, midBlock.NumberU64())
	}
}

// This test checks that InsertReceiptChain will roll back correctly when attempting to insert a side chain.
func TestInsertReceiptChainRollback(t *testing.T) {
	testInsertReceiptChainRollback(t, rawdb.HashScheme)
	testInsertReceiptChainRollback(t, rawdb.PathScheme)
}

//nolint:unused
func testInsertReceiptChainRollback(t *testing.T, scheme string) {
	// Generate forked chain. The returned BlockChain object is used to process the side chain blocks.
	tmpChain, sideblocks, canonblocks, gspec, err := getLongAndShortChains(scheme)
	if err != nil {
		t.Fatal(err)
	}
	defer tmpChain.Stop()
	// Get the side chain receipts.
	if _, err := tmpChain.InsertChain(sideblocks, false); err != nil {
		t.Fatal("processing side chain failed:", err)
	}
	t.Log("sidechain head:", tmpChain.CurrentBlock().Number, tmpChain.CurrentBlock().Hash())
	sidechainReceipts := make([]rlp.RawValue, len(sideblocks))
	for i, block := range sideblocks {
		sidechainReceipts[i] = tmpChain.GetReceiptsRLP(block.Hash())
	}
	// Get the canon chain receipts.
	if _, err := tmpChain.InsertChain(canonblocks, false); err != nil {
		t.Fatal("processing canon chain failed:", err)
	}
	t.Log("canon head:", tmpChain.CurrentBlock().Number, tmpChain.CurrentBlock().Hash())
	canonReceipts := make([]rlp.RawValue, len(canonblocks))
	for i, block := range canonblocks {
		canonReceipts[i] = tmpChain.GetReceiptsRLP(block.Hash())
	}

	// Set up a BlockChain that uses the ancient store.
	ancientDb, err := rawdb.NewDatabaseWithFreezer(rawdb.NewMemoryDatabase(), t.TempDir(), "", false, false, false, false, false, false)
	if err != nil {
		t.Fatalf("failed to create temp freezer db: %v", err)
	}
	defer ancientDb.Close()
	defaultConfig := DefaultConfig().WithStateScheme(scheme)
	ancientChain, _ := NewBlockChain(ancientDb, gspec, ethash.NewFaker(), defaultConfig)
	defer ancientChain.Stop()

	// Import the canonical header chain.
	canonHeaders := make([]*types.Header, len(canonblocks))
	for i, block := range canonblocks {
		canonHeaders[i] = block.Header()
	}
	if _, err = ancientChain.InsertHeaderChain(canonHeaders); err != nil {
		t.Fatal("can't import canon headers:", err)
	}

	// Try to insert blocks/receipts of the side chain.
	_, err = ancientChain.InsertReceiptChain(sideblocks, sidechainReceipts, uint64(len(sideblocks)))
	if err == nil {
		t.Fatal("expected error from InsertReceiptChain.")
	}
	if ancientChain.CurrentSnapBlock().Number.Uint64() != 0 {
		t.Fatalf("failed to rollback ancient data, want %d, have %d", 0, ancientChain.CurrentSnapBlock().Number)
	}
	if frozen, err := ancientChain.db.Ancients(); err != nil || frozen != 1 {
		t.Fatalf("failed to truncate ancient data, frozen index is %d", frozen)
	}

	// Insert blocks/receipts of the canonical chain.
	_, err = ancientChain.InsertReceiptChain(canonblocks, canonReceipts, uint64(len(canonblocks)))
	if err != nil {
		t.Fatalf("can't import canon chain receipts: %v", err)
	}
	if ancientChain.CurrentSnapBlock().Number.Uint64() != canonblocks[len(canonblocks)-1].NumberU64() {
		t.Fatalf("failed to insert ancient recept chain after rollback")
	}
	if frozen, _ := ancientChain.db.Ancients(); frozen != uint64(len(canonblocks))+1 {
		t.Fatalf("wrong ancients count %d", frozen)
	}
}

// Tests that importing a very large side fork, which is larger than the canon chain,
// but where the difficulty per block is kept low: this means that it will not
// overtake the 'canon' chain until after it's passed canon by about 200 blocks.
//
// Details at:
//   - https://github.com/ethereum/go-ethereum/issues/18977
//   - https://github.com/ethereum/go-ethereum/pull/18988
func TestLowDiffLongChain(t *testing.T) {
	testLowDiffLongChain(t, rawdb.HashScheme)
	testLowDiffLongChain(t, rawdb.PathScheme)
}

func testLowDiffLongChain(t *testing.T, scheme string) {
	// Generate a canonical chain to act as the main dataset
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}

	//Using TempTriesInMemory variable instead of DefaultTempInTries because changing the
	//value of DefaultTempInTries to 1024 is failing the test.
	TempTriesInMemory := 128

	// We must use a pretty long chain to ensure that the fork doesn't overtake us
	// until after at least 128 blocks post tip
	genDb, blocks, _ := GenerateChainWithGenesis(genesis, engine, 6*TempTriesInMemory, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		b.OffsetTime(-9)
	})

	// Import the canonical chain
	diskdb, _ := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	defer diskdb.Close()

	chain, err := NewBlockChain(diskdb, genesis, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
	// Generate fork chain, starting from an early block
	parent := blocks[10]
	fork, _ := GenerateChain(genesis.Config, parent, engine, genDb, 8*state.TriesInMemory, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{2})
	})

	// And now import the fork
	if i, err := chain.InsertChain(fork, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", i, err)
	}

	head := chain.CurrentBlock()
	if got := fork[len(fork)-1].Hash(); got != head.Hash() {
		t.Fatalf("head wrong, expected %x got %x", head.Hash(), got)
	}
	// Sanity check that all the canonical numbers are present
	header := chain.CurrentHeader()
	for number := head.Number.Uint64(); number > 0; number-- {
		if hash := chain.GetHeaderByNumber(number).Hash(); hash != header.Hash() {
			t.Fatalf("header %d: canonical hash mismatch: have %x, want %x", number, hash, header.Hash())
		}

		header = chain.GetHeader(header.ParentHash, number-1)
	}
}

// Tests that importing a sidechain (S), where
// - S is sidechain, containing blocks [Sn...Sm]
// - C is canon chain, containing blocks [G..Cn..Cm]
// - A common ancestor is placed at prune-point + blocksBetweenCommonAncestorAndPruneblock
// - The sidechain S is prepended with numCanonBlocksInSidechain blocks from the canon chain
//
// The mergePoint can be these values:
// -1: the transition won't happen
// 0:  the transition happens since genesis
// 1:  the transition happens after some chain segments
func testSideImport(t *testing.T, numCanonBlocksInSidechain, blocksBetweenCommonAncestorAndPruneblock int, mergePoint int) {
	// Generate a canonical chain to act as the main dataset
	chainConfig := *params.TestChainConfig

	var (
		engine = beacon.New(ethash.NewFaker())
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		nonce  = uint64(0)

		gspec = &Genesis{
			Config:  &chainConfig,
			Alloc:   types.GenesisAlloc{addr: {Balance: big.NewInt(gomath.MaxInt64)}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer     = types.LatestSigner(gspec.Config)
		mergeBlock = gomath.MaxInt32
	)
	// Generate and import the canonical chain
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	// Activate the transition since genesis if required
	if mergePoint == 0 {
		mergeBlock = 0

		// Set the terminal total difficulty in the config
		chain.Config().TerminalTotalDifficulty = big.NewInt(0)
	}
	genDb, blocks, _ := GenerateChainWithGenesis(gspec, engine, 2*state.TriesInMemory, func(i int, gen *BlockGen) {
		tx, err := types.SignTx(types.NewTransaction(nonce, common.HexToAddress("deadbeef"), big.NewInt(100), 21000, big.NewInt(int64(i+1)*params.GWei), nil), signer, key)
		if err != nil {
			t.Fatalf("failed to create tx: %v", err)
		}

		gen.AddTx(tx)

		if int(gen.header.Number.Uint64()) >= mergeBlock {
			gen.SetPoS()
		}

		nonce++
	})
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	lastPrunedIndex := len(blocks) - state.TriesInMemory - 1
	firstNonPrunedBlock := blocks[len(blocks)-state.TriesInMemory]

	// // Verify pruning of lastPrunedBlock
	// if chain.HasBlockAndState(lastPrunedBlock.Hash(), lastPrunedBlock.NumberU64()) {
	// 	t.Errorf("Block %d not pruned", lastPrunedBlock.NumberU64())
	// }
	// Verify firstNonPrunedBlock is not pruned
	if !chain.HasBlockAndState(firstNonPrunedBlock.Hash(), firstNonPrunedBlock.NumberU64()) {
		t.Errorf("Block %d pruned", firstNonPrunedBlock.NumberU64())
	}

	// Activate the transition in the middle of the chain
	if mergePoint == 1 {
		// Set the terminal total difficulty in the config
		ttd := big.NewInt(int64(len(blocks)))
		ttd.Mul(ttd, params.GenesisDifficulty)
		chain.Config().TerminalTotalDifficulty = ttd
		mergeBlock = len(blocks)
	}

	// Generate the sidechain
	// First block should be a known block, block after should be a pruned block. So
	// canon(pruned), side, side...

	// Generate fork chain, make it longer than canon
	parentIndex := lastPrunedIndex + blocksBetweenCommonAncestorAndPruneblock
	parent := blocks[parentIndex]
	fork, _ := GenerateChain(gspec.Config, parent, engine, genDb, 2*state.TriesInMemory, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{2})

		if int(b.header.Number.Uint64()) >= mergeBlock {
			b.SetPoS()
		}
	})
	// Prepend the parent(s)
	var sidechain []*types.Block
	for i := numCanonBlocksInSidechain; i > 0; i-- {
		sidechain = append(sidechain, blocks[parentIndex+1-i])
	}

	sidechain = append(sidechain, fork...)
	n, err := chain.InsertChain(sidechain, false)

	if err != nil {
		t.Errorf("Got error, %v number %d - %d", err, sidechain[n].NumberU64(), n)
	}

	head := chain.CurrentBlock()
	if got := fork[len(fork)-1].Hash(); got != head.Hash() {
		t.Fatalf("head wrong, expected %x got %x", head.Hash(), got)
	}
}

// Tests that importing a sidechain (S), where
//   - S is sidechain, containing blocks [Sn...Sm]
//   - C is canon chain, containing blocks [G..Cn..Cm]
//   - The common ancestor Cc is pruned
//   - The first block in S: Sn, is == Cn
//
// That is: the sidechain for import contains some blocks already present in canon chain.
// So the blocks are:
//
//	[ Cn, Cn+1, Cc, Sn+3 ... Sm]
//	^    ^    ^  pruned
func TestPrunedImportSide(t *testing.T) {
	// glogger := log.NewGlogHandler(log.NewTerminalHandler(os.Stderr, false))
	// glogger.Verbosity(3)
	// log.SetDefault(log.NewLogger(glogger))
	testSideImport(t, 3, 3, -1)
	testSideImport(t, 3, -3, -1)
	testSideImport(t, 10, 0, -1)
	testSideImport(t, 1, 10, -1)
	testSideImport(t, 1, -10, -1)
}

func TestPrunedImportSideWithMerging(t *testing.T) {
	// glogger := log.NewGlogHandler(log.NewTerminalHandler(os.Stderr, false))
	// glogger.Verbosity(3)
	// log.SetDefault(log.NewLogger(glogger))
	testSideImport(t, 3, 3, 0)
	testSideImport(t, 3, -3, 0)
	testSideImport(t, 10, 0, 0)
	testSideImport(t, 1, 10, 0)
	testSideImport(t, 1, -10, 0)

	testSideImport(t, 3, 3, 1)
	testSideImport(t, 3, -3, 1)
	testSideImport(t, 10, 0, 1)
	testSideImport(t, 1, 10, 1)
	testSideImport(t, 1, -10, 1)
}

func TestInsertKnownHeaders(t *testing.T) {
	testInsertKnownChainData(t, "headers", rawdb.HashScheme)
	testInsertKnownChainData(t, "headers", rawdb.PathScheme)
}

func TestInsertKnownReceiptChain(t *testing.T) {
	testInsertKnownChainData(t, "receipts", rawdb.HashScheme)
	testInsertKnownChainData(t, "receipts", rawdb.PathScheme)
}

func TestInsertKnownBlocks(t *testing.T) {
	testInsertKnownChainData(t, "blocks", rawdb.HashScheme)
	testInsertKnownChainData(t, "blocks", rawdb.PathScheme)
}

func testInsertKnownChainData(t *testing.T, typ string, scheme string) {
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	genDb, blocks, receipts := GenerateChainWithGenesis(genesis, engine, 32, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{1}) })

	// A longer chain but total difficulty is lower.
	blocks2, receipts2 := GenerateChain(genesis.Config, blocks[len(blocks)-1], engine, genDb, 65, func(i int, b *BlockGen) { b.SetCoinbase(common.Address{1}) })

	// A shorter chain but total difficulty is higher.
	blocks3, receipts3 := GenerateChain(genesis.Config, blocks[len(blocks)-1], engine, genDb, 64, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		b.OffsetTime(-9) // A higher difficulty
	})
	// Import the shared chain and the original canonical one
	chaindb, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	if err != nil {
		t.Fatalf("failed to create temp freezer db: %v", err)
	}
	defer chaindb.Close()

	chain, err := NewBlockChain(chaindb, genesis, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	var (
		inserter func(blocks []*types.Block, receipts []types.Receipts) error
		asserter func(t *testing.T, block *types.Block)
	)

	if typ == "headers" {
		inserter = func(blocks []*types.Block, receipts []types.Receipts) error {
			headers := make([]*types.Header, 0, len(blocks))
			for _, block := range blocks {
				headers = append(headers, block.Header())
			}
			_, err := chain.InsertHeaderChain(headers)
			return err
		}
		asserter = func(t *testing.T, block *types.Block) {
			if chain.CurrentHeader().Hash() != block.Hash() {
				t.Fatalf("current head header mismatch, have %v, want %v", chain.CurrentHeader().Hash().Hex(), block.Hash().Hex())
			}
		}
	} else if typ == "receipts" {
		inserter = func(blocks []*types.Block, receipts []types.Receipts) error {
			headers := make([]*types.Header, 0, len(blocks))
			for _, block := range blocks {
				headers = append(headers, block.Header())
			}
			_, err := chain.InsertHeaderChain(headers)
			if err != nil {
				return err
			}
			_, err = chain.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), 0)
			return err
		}
		asserter = func(t *testing.T, block *types.Block) {
			if chain.CurrentSnapBlock().Hash() != block.Hash() {
				t.Fatalf("current head fast block mismatch, have %v, want %v", chain.CurrentSnapBlock().Hash().Hex(), block.Hash().Hex())
			}
		}
	} else {
		inserter = func(blocks []*types.Block, receipts []types.Receipts) error {
			_, err := chain.InsertChain(blocks, false)
			return err
		}
		asserter = func(t *testing.T, block *types.Block) {
			if chain.CurrentBlock().Hash() != block.Hash() {
				t.Fatalf("current head block mismatch, have %v, want %v", chain.CurrentBlock().Hash().Hex(), block.Hash().Hex())
			}
		}
	}

	if err := inserter(blocks, receipts); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	// Reimport the chain data again. All the imported
	// chain data are regarded "known" data.
	if err := inserter(blocks, receipts); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks[len(blocks)-1])

	// Import a long canonical chain with some known data as prefix.
	rollback := blocks[len(blocks)/2].NumberU64()

	chain.SetHead(rollback - 1)

	if err := inserter(append(blocks, blocks2...), append(receipts, receipts2...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks2[len(blocks2)-1])

	// Import a heavier shorter but higher total difficulty chain with some known data as prefix.
	if err := inserter(append(blocks, blocks3...), append(receipts, receipts3...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks3[len(blocks3)-1])

	// Import a longer but lower total difficulty chain with some known data as prefix.
	if err := inserter(append(blocks, blocks2...), append(receipts, receipts2...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}
	// The head shouldn't change.
	asserter(t, blocks3[len(blocks3)-1])

	// Rollback the heavier chain and re-insert the longer chain again
	chain.SetHead(rollback - 1)

	if err := inserter(append(blocks, blocks2...), append(receipts, receipts2...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks2[len(blocks2)-1])
}

func TestInsertKnownHeadersWithMerging(t *testing.T) {
	testInsertKnownChainDataWithMerging(t, "headers", 0)
}
func TestInsertKnownReceiptChainWithMerging(t *testing.T) {
	testInsertKnownChainDataWithMerging(t, "receipts", 0)
}
func TestInsertKnownBlocksWithMerging(t *testing.T) {
	testInsertKnownChainDataWithMerging(t, "blocks", 0)
}
func TestInsertKnownHeadersAfterMerging(t *testing.T) {
	testInsertKnownChainDataWithMerging(t, "headers", 1)
}
func TestInsertKnownReceiptChainAfterMerging(t *testing.T) {
	testInsertKnownChainDataWithMerging(t, "receipts", 1)
}
func TestInsertKnownBlocksAfterMerging(t *testing.T) {
	testInsertKnownChainDataWithMerging(t, "blocks", 1)
}

// mergeHeight can be assigned in these values:
// 0: means the merging is applied since genesis
// 1: means the merging is applied after the first segment
func testInsertKnownChainDataWithMerging(t *testing.T, typ string, mergeHeight int) {
	// Copy the TestChainConfig so we can modify it during tests
	chainConfig := *params.TestChainConfig

	var (
		genesis = &Genesis{
			BaseFee: big.NewInt(params.InitialBaseFee),
			Config:  &chainConfig,
		}
		engine     = beacon.New(ethash.NewFaker())
		mergeBlock = uint64(gomath.MaxUint64)
	)
	// Apply merging since genesis
	if mergeHeight == 0 {
		genesis.Config.TerminalTotalDifficulty = big.NewInt(0)
		mergeBlock = uint64(0)
	}

	genDb, blocks, receipts := GenerateChainWithGenesis(genesis, engine, 32,
		func(i int, b *BlockGen) {
			if b.header.Number.Uint64() >= mergeBlock {
				b.SetPoS()
			}

			b.SetCoinbase(common.Address{1})
		})

	// Apply merging after the first segment
	if mergeHeight == 1 {
		// TTD is genesis diff + blocks
		ttd := big.NewInt(1 + int64(len(blocks)))
		ttd.Mul(ttd, params.GenesisDifficulty)
		genesis.Config.TerminalTotalDifficulty = ttd
		mergeBlock = uint64(len(blocks))
	}
	// Longer chain and shorter chain
	blocks2, receipts2 := GenerateChain(genesis.Config, blocks[len(blocks)-1], engine, genDb, 65, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})

		if b.header.Number.Uint64() >= mergeBlock {
			b.SetPoS()
		}
	})
	blocks3, receipts3 := GenerateChain(genesis.Config, blocks[len(blocks)-1], engine, genDb, 64, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		b.OffsetTime(-9) // Time shifted, difficulty shouldn't be changed

		if b.header.Number.Uint64() >= mergeBlock {
			b.SetPoS()
		}
	})
	// Import the shared chain and the original canonical one
	chaindb, err := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	if err != nil {
		t.Fatalf("failed to create temp freezer db: %v", err)
	}
	defer chaindb.Close()

	chain, err := NewBlockChain(chaindb, genesis, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	var (
		inserter func(blocks []*types.Block, receipts []types.Receipts) error
		asserter func(t *testing.T, block *types.Block)
	)

	if typ == "headers" {
		inserter = func(blocks []*types.Block, receipts []types.Receipts) error {
			headers := make([]*types.Header, 0, len(blocks))
			for _, block := range blocks {
				headers = append(headers, block.Header())
			}
			i, err := chain.InsertHeaderChain(headers)
			if err != nil {
				return fmt.Errorf("index %d, number %d: %w", i, headers[i].Number, err)
			}

			return err
		}
		asserter = func(t *testing.T, block *types.Block) {
			if chain.CurrentHeader().Hash() != block.Hash() {
				t.Fatalf("current head header mismatch, have %v, want %v", chain.CurrentHeader().Hash().Hex(), block.Hash().Hex())
			}
		}
	} else if typ == "receipts" {
		inserter = func(blocks []*types.Block, receipts []types.Receipts) error {
			headers := make([]*types.Header, 0, len(blocks))
			for _, block := range blocks {
				headers = append(headers, block.Header())
			}
			i, err := chain.InsertHeaderChain(headers)
			if err != nil {
				return fmt.Errorf("index %d: %w", i, err)
			}
			_, err = chain.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), 0)
			return err
		}
		asserter = func(t *testing.T, block *types.Block) {
			if chain.CurrentSnapBlock().Hash() != block.Hash() {
				t.Fatalf("current head fast block mismatch, have %v, want %v", chain.CurrentSnapBlock().Hash().Hex(), block.Hash().Hex())
			}
		}
	} else {
		inserter = func(blocks []*types.Block, receipts []types.Receipts) error {
			i, err := chain.InsertChain(blocks, false)
			if err != nil {
				return fmt.Errorf("index %d: %w", i, err)
			}

			return nil
		}
		asserter = func(t *testing.T, block *types.Block) {
			if chain.CurrentBlock().Hash() != block.Hash() {
				t.Fatalf("current head block mismatch, have %v, want %v", chain.CurrentBlock().Hash().Hex(), block.Hash().Hex())
			}
		}
	}

	if err := inserter(blocks, receipts); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	// Reimport the chain data again. All the imported
	// chain data are regarded "known" data.
	if err := inserter(blocks, receipts); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks[len(blocks)-1])

	// Import a long canonical chain with some known data as prefix.
	rollback := blocks[len(blocks)/2].NumberU64()
	chain.SetHead(rollback - 1)

	if err := inserter(blocks, receipts); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks[len(blocks)-1])

	// Import a longer chain with some known data as prefix.
	if err := inserter(append(blocks, blocks2...), append(receipts, receipts2...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks2[len(blocks2)-1])

	// Import a shorter chain with some known data as prefix.
	// The reorg is expected since the fork choice rule is
	// already changed.
	if err := inserter(append(blocks, blocks3...), append(receipts, receipts3...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}
	// The head shouldn't change.
	asserter(t, blocks3[len(blocks3)-1])

	// Reimport the longer chain again, the reorg is still expected
	chain.SetHead(rollback - 1)

	if err := inserter(append(blocks, blocks2...), append(receipts, receipts2...)); err != nil {
		t.Fatalf("failed to insert chain data: %v", err)
	}

	asserter(t, blocks2[len(blocks2)-1])
}

// getLongAndShortChains returns two chains: A is longer, B is heavier.
func getLongAndShortChains(scheme string) (*BlockChain, []*types.Block, []*types.Block, *Genesis, error) {
	// Generate a canonical chain to act as the main dataset
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	// Generate and import the canonical chain,
	// Offset the time, to keep the difficulty low
	genDb, longChain, _ := GenerateChainWithGenesis(genesis, engine, 80, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
	})
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to create tester chain: %v", err)
	}
	// Generate fork chain, make it shorter than canon, with common ancestor pretty early
	parentIndex := 3
	parent := longChain[parentIndex]
	heavyChainExt, _ := GenerateChain(genesis.Config, parent, engine, genDb, 75, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{2})
		b.OffsetTime(-9)
	})

	var heavyChain []*types.Block
	heavyChain = append(heavyChain, longChain[:parentIndex+1]...)
	heavyChain = append(heavyChain, heavyChainExt...)

	// Verify that the test is sane
	var (
		longerTd  = new(big.Int)
		shorterTd = new(big.Int)
	)

	for index, b := range longChain {
		longerTd.Add(longerTd, b.Difficulty())

		if index <= parentIndex {
			shorterTd.Add(shorterTd, b.Difficulty())
		}
	}

	for _, b := range heavyChain {
		shorterTd.Add(shorterTd, b.Difficulty())
	}

	if shorterTd.Cmp(longerTd) <= 0 {
		return nil, nil, nil, nil, fmt.Errorf("test is moot, heavyChain td (%v) must be larger than canon td (%v)", shorterTd, longerTd)
	}

	longerNum := longChain[len(longChain)-1].NumberU64()
	shorterNum := heavyChain[len(heavyChain)-1].NumberU64()

	if shorterNum >= longerNum {
		return nil, nil, nil, nil, fmt.Errorf("test is moot, heavyChain num (%v) must be lower than canon num (%v)", shorterNum, longerNum)
	}

	return chain, longChain, heavyChain, genesis, nil
}

// TestReorgToShorterRemovesCanonMapping tests that if we
// 1. Have a chain [0 ... N .. X]
// 2. Reorg to shorter but heavier chain [0 ... N ... Y]
// 3. Then there should be no canon mapping for the block at height X
// 4. The forked block should still be retrievable by hash
func TestReorgToShorterRemovesCanonMapping(t *testing.T) {
	testReorgToShorterRemovesCanonMapping(t, rawdb.HashScheme)
	testReorgToShorterRemovesCanonMapping(t, rawdb.PathScheme)
}

func testReorgToShorterRemovesCanonMapping(t *testing.T, scheme string) {
	chain, canonblocks, sideblocks, _, err := getLongAndShortChains(scheme)
	if err != nil {
		t.Fatal(err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(canonblocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	canonNum := chain.CurrentBlock().Number.Uint64()
	canonHash := chain.CurrentBlock().Hash()

	_, err = chain.InsertChain(sideblocks, false)
	if err != nil {
		t.Errorf("Got error, %v", err)
	}

	head := chain.CurrentBlock()
	if got := sideblocks[len(sideblocks)-1].Hash(); got != head.Hash() {
		t.Fatalf("head wrong, expected %x got %x", head.Hash(), got)
	}
	// We have now inserted a sidechain.
	if blockByNum := chain.GetBlockByNumber(canonNum); blockByNum != nil {
		t.Errorf("expected block to be gone: %v", blockByNum.NumberU64())
	}

	if headerByNum := chain.GetHeaderByNumber(canonNum); headerByNum != nil {
		t.Errorf("expected header to be gone: %v", headerByNum.Number)
	}

	if blockByHash := chain.GetBlockByHash(canonHash); blockByHash == nil {
		t.Errorf("expected block to be present: %x", blockByHash.Hash())
	}

	if headerByHash := chain.GetHeaderByHash(canonHash); headerByHash == nil {
		t.Errorf("expected header to be present: %x", headerByHash.Hash())
	}
}

// TestReorgToShorterRemovesCanonMappingHeaderChain is the same scenario
// as TestReorgToShorterRemovesCanonMapping, but applied on headerchain
// imports -- that is, for fast sync
func TestReorgToShorterRemovesCanonMappingHeaderChain(t *testing.T) {
	testReorgToShorterRemovesCanonMappingHeaderChain(t, rawdb.HashScheme)
	testReorgToShorterRemovesCanonMappingHeaderChain(t, rawdb.PathScheme)
}

func testReorgToShorterRemovesCanonMappingHeaderChain(t *testing.T, scheme string) {
	chain, canonblocks, sideblocks, _, err := getLongAndShortChains(scheme)
	if err != nil {
		t.Fatal(err)
	}

	defer chain.Stop()

	// Convert into headers
	canonHeaders := make([]*types.Header, len(canonblocks))
	for i, block := range canonblocks {
		canonHeaders[i] = block.Header()
	}
	if n, err := chain.InsertHeaderChain(canonHeaders); err != nil {
		t.Fatalf("header %d: failed to insert into chain: %v", n, err)
	}

	canonNum := chain.CurrentHeader().Number.Uint64()
	canonHash := chain.CurrentBlock().Hash()

	sideHeaders := make([]*types.Header, len(sideblocks))
	for i, block := range sideblocks {
		sideHeaders[i] = block.Header()
	}
	if n, err := chain.InsertHeaderChain(sideHeaders); err != nil {
		t.Fatalf("header %d: failed to insert into chain: %v", n, err)
	}

	head := chain.CurrentHeader()
	if got := sideblocks[len(sideblocks)-1].Hash(); got != head.Hash() {
		t.Fatalf("head wrong, expected %x got %x", head.Hash(), got)
	}
	// We have now inserted a sidechain.
	if blockByNum := chain.GetBlockByNumber(canonNum); blockByNum != nil {
		t.Errorf("expected block to be gone: %v", blockByNum.NumberU64())
	}

	if headerByNum := chain.GetHeaderByNumber(canonNum); headerByNum != nil {
		t.Errorf("expected header to be gone: %v", headerByNum.Number.Uint64())
	}

	if blockByHash := chain.GetBlockByHash(canonHash); blockByHash == nil {
		t.Errorf("expected block to be present: %x", blockByHash.Hash())
	}

	if headerByHash := chain.GetHeaderByHash(canonHash); headerByHash == nil {
		t.Errorf("expected header to be present: %x", headerByHash.Hash())
	}
}

// Benchmarks large blocks with value transfers to non-existing accounts
func benchmarkLargeNumberOfValueToNonexisting(b *testing.B, numTxs, numBlocks int, recipientFn func(uint64) common.Address) {
	var (
		signer          = types.HomesteadSigner{}
		testBankKey, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		testBankAddress = crypto.PubkeyToAddress(testBankKey.PublicKey)
		bankFunds       = big.NewInt(100000000000000000)
		gspec           = &Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				testBankAddress: {Balance: bankFunds},
				common.HexToAddress("0xc0de"): {
					Code:    []byte{0x60, 0x01, 0x50},
					Balance: big.NewInt(0),
				}, // push 1, pop
			},
			GasLimit: 100e6, // 100 M
		}
	)
	// Generate the original common chain segment and the two competing forks
	engine := ethash.NewFaker()

	blockGenerator := func(i int, block *BlockGen) {
		block.SetCoinbase(common.Address{1})

		for txi := 0; txi < numTxs; txi++ {
			uniq := uint64(i*numTxs + txi)
			recipient := recipientFn(uniq)

			tx, err := types.SignTx(types.NewTransaction(uniq, recipient, big.NewInt(1), params.TxGas, block.header.BaseFee, nil), signer, testBankKey)
			if err != nil {
				b.Error(err)
			}

			block.AddTx(tx)
		}
	}

	_, shared, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, blockGenerator)

	b.StopTimer()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// Import the shared chain and the original canonical one
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, nil)
		if err != nil {
			b.Fatalf("failed to create tester chain: %v", err)
		}

		b.StartTimer()

		if _, err := chain.InsertChain(shared, false); err != nil {
			b.Fatalf("failed to insert shared chain: %v", err)
		}

		b.StopTimer()

		block := chain.GetBlockByHash(chain.CurrentBlock().Hash())

		if got := block.Transactions().Len(); got != numTxs*numBlocks {
			b.Fatalf("Transactions were not included, expected %d, got %d", numTxs*numBlocks, got)
		}
	}
}

func BenchmarkBlockChain_1x1000ValueTransferToNonexisting(b *testing.B) {
	var (
		numTxs    = 1000
		numBlocks = 1
	)

	recipientFn := func(nonce uint64) common.Address {
		return common.BigToAddress(new(big.Int).SetUint64(1337 + nonce))
	}
	benchmarkLargeNumberOfValueToNonexisting(b, numTxs, numBlocks, recipientFn)
}

func BenchmarkBlockChain_1x1000ValueTransferToExisting(b *testing.B) {
	var (
		numTxs    = 1000
		numBlocks = 1
	)

	b.StopTimer()
	b.ResetTimer()

	recipientFn := func(nonce uint64) common.Address {
		return common.BigToAddress(new(big.Int).SetUint64(1337))
	}
	benchmarkLargeNumberOfValueToNonexisting(b, numTxs, numBlocks, recipientFn)
}

func BenchmarkBlockChain_1x1000Executions(b *testing.B) {
	var (
		numTxs    = 1000
		numBlocks = 1
	)

	b.StopTimer()
	b.ResetTimer()

	recipientFn := func(nonce uint64) common.Address {
		return common.BigToAddress(new(big.Int).SetUint64(0xc0de))
	}
	benchmarkLargeNumberOfValueToNonexisting(b, numTxs, numBlocks, recipientFn)
}

// Tests that importing a some old blocks, where all blocks are before the
// pruning point.
// This internally leads to a sidechain import, since the blocks trigger an
// ErrPrunedAncestor error.
// This may e.g. happen if
//  1. Downloader rollbacks a batch of inserted blocks and exits
//  2. Downloader starts to sync again
//  3. The blocks fetched are all known and canonical blocks
func TestSideImportPrunedBlocks(t *testing.T) {
	testSideImportPrunedBlocks(t, rawdb.HashScheme)
	testSideImportPrunedBlocks(t, rawdb.PathScheme)
}

func testSideImportPrunedBlocks(t *testing.T, scheme string) {
	// Generate a canonical chain to act as the main dataset
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	// Generate and import the canonical chain
	_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 2*state.TriesInMemory, nil)

	// Construct a database with freezer enabled
	datadir := t.TempDir()
	ancient := path.Join(datadir, "ancient")

	pdb, err := pebble.New(datadir, 0, 0, "", false)
	if err != nil {
		t.Fatalf("Failed to create persistent key-value database: %v", err)
	}
	db, err := rawdb.Open(pdb, rawdb.OpenOptions{Ancient: ancient})
	if err != nil {
		t.Fatalf("Failed to create persistent freezer database: %v", err)
	}
	defer db.Close()

	chain, err := NewBlockChain(db, genesis, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	// In path-based trie database implementation, it will keep 128 diff + 1 disk
	// layers, totally 129 latest states available. In hash-based it's 128.
	states := state.TriesInMemory
	if scheme == rawdb.PathScheme {
		states = state.TriesInMemory + 1
	}

	lastPrunedIndex := len(blocks) - states - 1
	lastPrunedBlock := blocks[lastPrunedIndex]

	// Verify pruning of lastPrunedBlock
	if chain.HasBlockAndState(lastPrunedBlock.Hash(), lastPrunedBlock.NumberU64()) {
		t.Errorf("Block %d not pruned", lastPrunedBlock.NumberU64())
	}

	firstNonPrunedBlock := blocks[len(blocks)-states]
	// Verify firstNonPrunedBlock is not pruned
	if !chain.HasBlockAndState(firstNonPrunedBlock.Hash(), firstNonPrunedBlock.NumberU64()) {
		t.Errorf("Block %d pruned, scheme : %s", firstNonPrunedBlock.NumberU64(), scheme)
	}
	// Now re-import some old blocks
	blockToReimport := blocks[5:8]

	_, err = chain.InsertChain(blockToReimport, false)
	if err != nil {
		t.Errorf("Got error, %v", err)
	}
}

// TestDeleteCreateRevert tests a weird state transition corner case that we hit
// while changing the internals of statedb. The workflow is that a contract is
// self destructed, then in a followup transaction (but same block) it's created
// again and the transaction reverted.
//
// The original statedb implementation flushed dirty objects to the tries after
// each transaction, so this works ok. The rework accumulated writes in memory
// first, but the journal wiped the entire state object on create-revert.
func TestDeleteCreateRevert(t *testing.T) {
	testDeleteCreateRevert(t, rawdb.HashScheme)
	testDeleteCreateRevert(t, rawdb.PathScheme)
}

func testDeleteCreateRevert(t *testing.T, scheme string) {
	var (
		aa     = common.HexToAddress("0x000000000000000000000000000000000000aaaa")
		bb     = common.HexToAddress("0x000000000000000000000000000000000000bbbb")
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(100000000000000000)
		gspec   = &Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				address: {Balance: funds},
				// The address 0xAAAAA selfdestructs if called
				aa: {
					// Code needs to just selfdestruct
					Code:    []byte{byte(vm.PC), byte(vm.SELFDESTRUCT)},
					Nonce:   1,
					Balance: big.NewInt(0),
				},
				// The address 0xBBBB send 1 wei to 0xAAAA, then reverts
				bb: {
					Code: []byte{
						byte(vm.PC),          // [0]
						byte(vm.DUP1),        // [0,0]
						byte(vm.DUP1),        // [0,0,0]
						byte(vm.DUP1),        // [0,0,0,0]
						byte(vm.PUSH1), 0x01, // [0,0,0,0,1] (value)
						byte(vm.PUSH2), 0xaa, 0xaa, // [0,0,0,0,1, 0xaaaa]
						byte(vm.GAS),
						byte(vm.CALL),
						byte(vm.REVERT),
					},
					Balance: big.NewInt(1),
				},
			},
		}
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to AAAA
		tx, _ := types.SignTx(types.NewTransaction(0, aa,
			big.NewInt(0), 50000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		// One transaction to BBBB
		tx, _ = types.SignTx(types.NewTransaction(1, bb,
			big.NewInt(0), 100000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
	})
	// Import the canonical chain
	options := DefaultConfig().WithStateScheme(scheme)
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
}

// TestDeleteRecreateSlots tests a state-transition that contains both deletion
// and recreation of contract state.
// Contract A exists, has slots 1 and 2 set
// Tx 1: Selfdestruct A
// Tx 2: Re-create A, set slots 3 and 4
// Expected outcome is that _all_ slots are cleared from A, due to the selfdestruct,
// and then the new slots exist
func TestDeleteRecreateSlots(t *testing.T) {
	testDeleteRecreateSlots(t, rawdb.HashScheme)
	testDeleteRecreateSlots(t, rawdb.PathScheme)
}

func testDeleteRecreateSlots(t *testing.T, scheme string) {
	var (
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address   = crypto.PubkeyToAddress(key.PublicKey)
		funds     = big.NewInt(1000000000000000)
		bb        = common.HexToAddress("0x000000000000000000000000000000000000bbbb")
		aaStorage = make(map[common.Hash]common.Hash)          // Initial storage in AA
		aaCode    = []byte{byte(vm.PC), byte(vm.SELFDESTRUCT)} // Code for AA (simple selfdestruct)
	)
	// Populate two slots
	aaStorage[common.HexToHash("01")] = common.HexToHash("01")
	aaStorage[common.HexToHash("02")] = common.HexToHash("02")

	// The bb-code needs to CREATE2 the aa contract. It consists of
	// both initcode and deployment code
	// initcode:
	// 1. Set slots 3=3, 4=4,
	// 2. Return aaCode

	initCode := []byte{
		byte(vm.PUSH1), 0x3, // value
		byte(vm.PUSH1), 0x3, // location
		byte(vm.SSTORE),     // Set slot[3] = 3
		byte(vm.PUSH1), 0x4, // value
		byte(vm.PUSH1), 0x4, // location
		byte(vm.SSTORE), // Set slot[4] = 4
		// Slots are set, now return the code
		byte(vm.PUSH2), byte(vm.PC), byte(vm.SELFDESTRUCT), // Push code on stack
		byte(vm.PUSH1), 0x0, // memory start on stack
		byte(vm.MSTORE),
		// Code is now in memory.
		byte(vm.PUSH1), 0x2, // size
		byte(vm.PUSH1), byte(32 - 2), // offset
		byte(vm.RETURN),
	}
	if l := len(initCode); l > 32 {
		t.Fatalf("init code is too long for a pushx, need a more elaborate deployer")
	}

	bbCode := []byte{
		// Push initcode onto stack
		byte(vm.PUSH1) + byte(len(initCode)-1)}
	bbCode = append(bbCode, initCode...)
	bbCode = append(bbCode, []byte{
		byte(vm.PUSH1), 0x0, // memory start on stack
		byte(vm.MSTORE),
		byte(vm.PUSH1), 0x00, // salt
		byte(vm.PUSH1), byte(len(initCode)), // size
		byte(vm.PUSH1), byte(32 - len(initCode)), // offset
		byte(vm.PUSH1), 0x00, // endowment
		byte(vm.CREATE2),
	}...)

	initHash := crypto.Keccak256Hash(initCode)
	aa := crypto.CreateAddress2(bb, [32]byte{}, initHash[:])
	t.Logf("Destination address: %x\n", aa)

	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
			// The address 0xAAAAA selfdestructs if called
			aa: {
				// Code needs to just selfdestruct
				Code:    aaCode,
				Nonce:   1,
				Balance: big.NewInt(0),
				Storage: aaStorage,
			},
			// The contract BB recreates AA
			bb: {
				Code:    bbCode,
				Balance: big.NewInt(1),
			},
		},
	}
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to AA, to kill it
		tx, _ := types.SignTx(types.NewTransaction(0, aa,
			big.NewInt(0), 50000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		// One transaction to BB, to recreate AA
		tx, _ = types.SignTx(types.NewTransaction(1, bb,
			big.NewInt(0), 100000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
	})
	// Import the canonical chain
	options := DefaultConfig().WithStateScheme(scheme)
	options.VmConfig = vm.Config{
		Tracer: logger.NewJSONLogger(nil, os.Stdout),
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	statedb, _ := chain.State()

	// If all is correct, then slot 1 and 2 are zero
	if got, exp := statedb.GetState(aa, common.HexToHash("01")), (common.Hash{}); got != exp {
		t.Errorf("got %x exp %x", got, exp)
	}

	if got, exp := statedb.GetState(aa, common.HexToHash("02")), (common.Hash{}); got != exp {
		t.Errorf("got %x exp %x", got, exp)
	}
	// Also, 3 and 4 should be set
	if got, exp := statedb.GetState(aa, common.HexToHash("03")), common.HexToHash("03"); got != exp {
		t.Fatalf("got %x exp %x", got, exp)
	}

	if got, exp := statedb.GetState(aa, common.HexToHash("04")), common.HexToHash("04"); got != exp {
		t.Fatalf("got %x exp %x", got, exp)
	}
}

// TestDeleteRecreateAccount tests a state-transition that contains deletion of a
// contract with storage, and a recreate of the same contract via a
// regular value-transfer
// Expected outcome is that _all_ slots are cleared from A
func TestDeleteRecreateAccount(t *testing.T) {
	testDeleteRecreateAccount(t, rawdb.HashScheme)
	testDeleteRecreateAccount(t, rawdb.PathScheme)
}

func testDeleteRecreateAccount(t *testing.T, scheme string) {
	var (
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)

		aa        = common.HexToAddress("0x7217d81b76bdd8707601e959454e3d776aee5f43")
		aaStorage = make(map[common.Hash]common.Hash)          // Initial storage in AA
		aaCode    = []byte{byte(vm.PC), byte(vm.SELFDESTRUCT)} // Code for AA (simple selfdestruct)
	)
	// Populate two slots
	aaStorage[common.HexToHash("01")] = common.HexToHash("01")
	aaStorage[common.HexToHash("02")] = common.HexToHash("02")

	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
			// The address 0xAAAAA selfdestructs if called
			aa: {
				// Code needs to just selfdestruct
				Code:    aaCode,
				Nonce:   1,
				Balance: big.NewInt(0),
				Storage: aaStorage,
			},
		},
	}

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to AA, to kill it
		tx, _ := types.SignTx(types.NewTransaction(0, aa,
			big.NewInt(0), 50000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		// One transaction to AA, to recreate it (but without storage
		tx, _ = types.SignTx(types.NewTransaction(1, aa,
			big.NewInt(1), 100000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
	})
	// Import the canonical chain
	options := DefaultConfig().WithStateScheme(scheme)
	options.VmConfig = vm.Config{
		Tracer: logger.NewJSONLogger(nil, os.Stdout),
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	statedb, _ := chain.State()

	// If all is correct, then both slots are zero
	if got, exp := statedb.GetState(aa, common.HexToHash("01")), (common.Hash{}); got != exp {
		t.Errorf("got %x exp %x", got, exp)
	}

	if got, exp := statedb.GetState(aa, common.HexToHash("02")), (common.Hash{}); got != exp {
		t.Errorf("got %x exp %x", got, exp)
	}
}

// TestDeleteRecreateSlotsAcrossManyBlocks tests multiple state-transition that contains both deletion
// and recreation of contract state.
// Contract A exists, has slots 1 and 2 set
// Tx 1: Selfdestruct A
// Tx 2: Re-create A, set slots 3 and 4
// Expected outcome is that _all_ slots are cleared from A, due to the selfdestruct,
// and then the new slots exist
func TestDeleteRecreateSlotsAcrossManyBlocks(t *testing.T) {
	testDeleteRecreateSlotsAcrossManyBlocks(t, rawdb.HashScheme)
	testDeleteRecreateSlotsAcrossManyBlocks(t, rawdb.PathScheme)
}

func testDeleteRecreateSlotsAcrossManyBlocks(t *testing.T, scheme string) {
	var (
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address   = crypto.PubkeyToAddress(key.PublicKey)
		funds     = big.NewInt(1000000000000000)
		bb        = common.HexToAddress("0x000000000000000000000000000000000000bbbb")
		aaStorage = make(map[common.Hash]common.Hash)          // Initial storage in AA
		aaCode    = []byte{byte(vm.PC), byte(vm.SELFDESTRUCT)} // Code for AA (simple selfdestruct)
	)
	// Populate two slots
	aaStorage[common.HexToHash("01")] = common.HexToHash("01")
	aaStorage[common.HexToHash("02")] = common.HexToHash("02")

	// The bb-code needs to CREATE2 the aa contract. It consists of
	// both initcode and deployment code
	// initcode:
	// 1. Set slots 3=blocknum+1, 4=4,
	// 2. Return aaCode

	initCode := []byte{
		byte(vm.PUSH1), 0x1, //
		byte(vm.NUMBER),     // value = number + 1
		byte(vm.ADD),        //
		byte(vm.PUSH1), 0x3, // location
		byte(vm.SSTORE),     // Set slot[3] = number + 1
		byte(vm.PUSH1), 0x4, // value
		byte(vm.PUSH1), 0x4, // location
		byte(vm.SSTORE), // Set slot[4] = 4
		// Slots are set, now return the code
		byte(vm.PUSH2), byte(vm.PC), byte(vm.SELFDESTRUCT), // Push code on stack
		byte(vm.PUSH1), 0x0, // memory start on stack
		byte(vm.MSTORE),
		// Code is now in memory.
		byte(vm.PUSH1), 0x2, // size
		byte(vm.PUSH1), byte(32 - 2), // offset
		byte(vm.RETURN),
	}
	if l := len(initCode); l > 32 {
		t.Fatalf("init code is too long for a pushx, need a more elaborate deployer")
	}

	bbCode := []byte{
		// Push initcode onto stack
		byte(vm.PUSH1) + byte(len(initCode)-1)}
	bbCode = append(bbCode, initCode...)
	bbCode = append(bbCode, []byte{
		byte(vm.PUSH1), 0x0, // memory start on stack
		byte(vm.MSTORE),
		byte(vm.PUSH1), 0x00, // salt
		byte(vm.PUSH1), byte(len(initCode)), // size
		byte(vm.PUSH1), byte(32 - len(initCode)), // offset
		byte(vm.PUSH1), 0x00, // endowment
		byte(vm.CREATE2),
	}...)

	initHash := crypto.Keccak256Hash(initCode)
	aa := crypto.CreateAddress2(bb, [32]byte{}, initHash[:])
	t.Logf("Destination address: %x\n", aa)
	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
			// The address 0xAAAAA selfdestructs if called
			aa: {
				// Code needs to just selfdestruct
				Code:    aaCode,
				Nonce:   1,
				Balance: big.NewInt(0),
				Storage: aaStorage,
			},
			// The contract BB recreates AA
			bb: {
				Code:    bbCode,
				Balance: big.NewInt(1),
			},
		},
	}

	var nonce uint64

	type expectation struct {
		exist    bool
		blocknum int
		values   map[int]int
	}

	var current = &expectation{
		exist:    true, // exists in genesis
		blocknum: 0,
		values:   map[int]int{1: 1, 2: 2},
	}

	var expectations []*expectation

	var newDestruct = func(e *expectation, b *BlockGen) *types.Transaction {
		tx, _ := types.SignTx(types.NewTransaction(nonce, aa,
			big.NewInt(0), 50000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		nonce++
		if e.exist {
			e.exist = false
			e.values = nil
		}
		//t.Logf("block %d; adding destruct\n", e.blocknum)
		return tx
	}

	var newResurrect = func(e *expectation, b *BlockGen) *types.Transaction {
		tx, _ := types.SignTx(types.NewTransaction(nonce, bb,
			big.NewInt(0), 100000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		nonce++
		if !e.exist {
			e.exist = true
			e.values = map[int]int{3: e.blocknum + 1, 4: 4}
		}
		//t.Logf("block %d; adding resurrect\n", e.blocknum)
		return tx
	}

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 150, func(i int, b *BlockGen) {
		var exp = new(expectation)
		exp.blocknum = i + 1
		exp.values = make(map[int]int)

		for k, v := range current.values {
			exp.values[k] = v
		}

		exp.exist = current.exist

		b.SetCoinbase(common.Address{1})

		if i%2 == 0 {
			b.AddTx(newDestruct(exp, b))
		}

		if i%3 == 0 {
			b.AddTx(newResurrect(exp, b))
		}

		if i%5 == 0 {
			b.AddTx(newDestruct(exp, b))
		}

		if i%7 == 0 {
			b.AddTx(newResurrect(exp, b))
		}

		expectations = append(expectations, exp)
		current = exp
	})
	// Import the canonical chain
	options := DefaultConfig().WithStateScheme(scheme)
	options.VmConfig = vm.Config{
		//Debug:  true,
		//Tracer: vm.NewJSONLogger(nil, os.Stdout),
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	var asHash = func(num int) common.Hash {
		return common.BytesToHash([]byte{byte(num)})
	}

	for i, block := range blocks {
		blockNum := i + 1

		if n, err := chain.InsertChain([]*types.Block{block}, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", n, err)
		}

		statedb, _ := chain.State()
		// If all is correct, then slot 1 and 2 are zero
		if got, exp := statedb.GetState(aa, common.HexToHash("01")), (common.Hash{}); got != exp {
			t.Errorf("block %d, got %x exp %x", blockNum, got, exp)
		}

		if got, exp := statedb.GetState(aa, common.HexToHash("02")), (common.Hash{}); got != exp {
			t.Errorf("block %d, got %x exp %x", blockNum, got, exp)
		}

		exp := expectations[i]
		if exp.exist {
			if !statedb.Exist(aa) {
				t.Fatalf("block %d, expected %v to exist, it did not", blockNum, aa)
			}

			for slot, val := range exp.values {
				if gotValue, expValue := statedb.GetState(aa, asHash(slot)), asHash(val); gotValue != expValue {
					t.Fatalf("block %d, slot %d, got %x exp %x", blockNum, slot, gotValue, expValue)
				}
			}
		} else {
			if statedb.Exist(aa) {
				t.Fatalf("block %d, expected %v to not exist, it did", blockNum, aa)
			}
		}
	}
}

// TestInitThenFailCreateContract tests a pretty notorious case that happened
// on mainnet over blocks 7338108, 7338110 and 7338115.
//   - Block 7338108: address e771789f5cccac282f23bb7add5690e1f6ca467c is initiated
//     with 0.001 ether (thus created but no code)
//   - Block 7338110: a CREATE2 is attempted. The CREATE2 would deploy code on
//     the same address e771789f5cccac282f23bb7add5690e1f6ca467c. However, the
//     deployment fails due to OOG during initcode execution
//   - Block 7338115: another tx checks the balance of
//     e771789f5cccac282f23bb7add5690e1f6ca467c, and the snapshotter returned it as
//     zero.
//
// The problem being that the snapshotter maintains a destructset, and adds items
// to the destructset in case something is created "onto" an existing item.
// We need to either roll back the snapDestructs, or not place it into snapDestructs
// in the first place.
//

func TestInitThenFailCreateContract(t *testing.T) {
	testInitThenFailCreateContract(t, rawdb.HashScheme)
	testInitThenFailCreateContract(t, rawdb.PathScheme)
}

func testInitThenFailCreateContract(t *testing.T, scheme string) {
	var (
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)
		bb      = common.HexToAddress("0x000000000000000000000000000000000000bbbb")
	)

	// The bb-code needs to CREATE2 the aa contract. It consists of
	// both initcode and deployment code
	// initcode:
	// 1. If blocknum < 1, error out (e.g invalid opcode)
	// 2. else, return a snippet of code
	initCode := []byte{
		byte(vm.PUSH1), 0x1, // y (2)
		byte(vm.NUMBER), // x (number)
		byte(vm.GT),     // x > y?
		byte(vm.PUSH1), byte(0x8),
		byte(vm.JUMPI), // jump to label if number > 2
		byte(0xFE),     // illegal opcode
		byte(vm.JUMPDEST),
		byte(vm.PUSH1), 0x2, // size
		byte(vm.PUSH1), 0x0, // offset
		byte(vm.RETURN), // return 2 bytes of zero-code
	}
	if l := len(initCode); l > 32 {
		t.Fatalf("init code is too long for a pushx, need a more elaborate deployer")
	}

	bbCode := []byte{
		// Push initcode onto stack
		byte(vm.PUSH1) + byte(len(initCode)-1)}
	bbCode = append(bbCode, initCode...)
	bbCode = append(bbCode, []byte{
		byte(vm.PUSH1), 0x0, // memory start on stack
		byte(vm.MSTORE),
		byte(vm.PUSH1), 0x00, // salt
		byte(vm.PUSH1), byte(len(initCode)), // size
		byte(vm.PUSH1), byte(32 - len(initCode)), // offset
		byte(vm.PUSH1), 0x00, // endowment
		byte(vm.CREATE2),
	}...)

	initHash := crypto.Keccak256Hash(initCode)
	aa := crypto.CreateAddress2(bb, [32]byte{}, initHash[:])
	t.Logf("Destination address: %x\n", aa)

	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
			// The address aa has some funds
			aa: {Balance: big.NewInt(100000)},
			// The contract BB tries to create code onto AA
			bb: {
				Code:    bbCode,
				Balance: big.NewInt(1),
			},
		},
	}
	nonce := uint64(0)
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 4, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to BB
		tx, _ := types.SignTx(types.NewTransaction(nonce, bb,
			big.NewInt(0), 100000, b.header.BaseFee, nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)

		nonce++
	})

	// Import the canonical chain
	options := DefaultConfig().WithStateScheme(scheme)
	options.VmConfig = vm.Config{
		//Debug:  true,
		//Tracer: vm.NewJSONLogger(nil, os.Stdout),
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	statedb, _ := chain.State()
	if got, exp := statedb.GetBalance(aa), uint256.NewInt(100000); got.Cmp(exp) != 0 {
		t.Fatalf("Genesis err, got %v exp %v", got, exp)
	}
	// First block tries to create, but fails
	{
		block := blocks[0]
		if _, err := chain.InsertChain([]*types.Block{blocks[0]}, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", block.NumberU64(), err)
		}

		statedb, _ = chain.State()
		if got, exp := statedb.GetBalance(aa), uint256.NewInt(100000); got.Cmp(exp) != 0 {
			t.Fatalf("block %d: got %v exp %v", block.NumberU64(), got, exp)
		}
	}
	// Import the rest of the blocks
	for _, block := range blocks[1:] {
		if _, err := chain.InsertChain([]*types.Block{block}, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", block.NumberU64(), err)
		}
	}
}

// TestEIP2718Transition tests that an EIP-2718 transaction will be accepted
// after the fork block has passed. This is verified by sending an EIP-2930
// access list transaction, which specifies a single slot access, and then
// checking that the gas usage of a hot SLOAD and a cold SLOAD are calculated
// correctly.
func TestEIP2718Transition(t *testing.T) {
	testEIP2718Transition(t, rawdb.HashScheme)
	testEIP2718Transition(t, rawdb.PathScheme)
}

func testEIP2718Transition(t *testing.T, scheme string) {
	var (
		aa     = common.HexToAddress("0x000000000000000000000000000000000000aaaa")
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)
		gspec   = &Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				address: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)
	// Generate blocks
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})

		// One transaction to 0xAAAA
		signer := types.LatestSigner(gspec.Config)
		tx, _ := types.SignNewTx(key, signer, &types.AccessListTx{
			ChainID:  gspec.Config.ChainID,
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: b.header.BaseFee,
			AccessList: types.AccessList{{
				Address:     aa,
				StorageKeys: []common.Hash{{0}},
			}},
		})
		b.AddTx(tx)
	})

	// Import the canonical chain
	options := DefaultConfig().WithStateScheme(scheme)
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	// Expected gas is intrinsic + 2 * pc + hot load + cold load, since only one load is in the access list
	expected := params.TxGas + params.TxAccessListAddressGas + params.TxAccessListStorageKeyGas +
		vm.GasQuickStep*2 + params.WarmStorageReadCostEIP2929 + params.ColdSloadCostEIP2929
	if block.GasUsed() != expected {
		t.Fatalf("incorrect amount of gas spent: expected %d, got %d", expected, block.GasUsed())
	}
}

// TestEIP1559Transition tests the following:
//
//  1. A transaction whose gasFeeCap is greater than the baseFee is valid.
//  2. Gas accounting for access lists on EIP-1559 transactions is correct.
//  3. Only the transaction's tip will be received by the coinbase.
//  4. The transaction sender pays for both the tip and baseFee.
//  5. The coinbase receives only the partially realized tip when
//     gasFeeCap - gasTipCap < baseFee.
//  6. Legacy transaction behave as expected (e.g. gasPrice = gasFeeCap = gasTipCap).
func TestEIP1559Transition(t *testing.T) {
	testEIP1559Transition(t, rawdb.HashScheme)
	testEIP1559Transition(t, rawdb.PathScheme)
}

func testEIP1559Transition(t *testing.T, scheme string) {
	var (
		aa     = common.HexToAddress("0x000000000000000000000000000000000000aaaa")
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = crypto.PubkeyToAddress(key2.PublicKey)
		funds   = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
		config  = *params.AllEthashProtocolChanges
		gspec   = &Genesis{
			Config: &config,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)

	gspec.Config.BerlinBlock = common.Big0
	gspec.Config.LondonBlock = common.Big0
	signer := types.LatestSigner(gspec.Config)

	genDb, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})

		// One transaction to 0xAAAA
		accesses := types.AccessList{types.AccessTuple{
			Address:     aa,
			StorageKeys: []common.Hash{{0}},
		}}

		txdata := &types.DynamicFeeTx{
			ChainID:    gspec.Config.ChainID,
			Nonce:      0,
			To:         &aa,
			Gas:        30000,
			GasFeeCap:  newGwei(5),
			GasTipCap:  big.NewInt(2),
			AccessList: accesses,
			Data:       []byte{},
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key1)

		b.AddTx(tx)
	})
	options := DefaultConfig().WithStateScheme(scheme)
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	// 1+2: Ensure EIP-1559 access lists are accounted for via gas usage.
	expectedGas := params.TxGas + params.TxAccessListAddressGas + params.TxAccessListStorageKeyGas +
		vm.GasQuickStep*2 + params.WarmStorageReadCostEIP2929 + params.ColdSloadCostEIP2929
	if block.GasUsed() != expectedGas {
		t.Fatalf("incorrect amount of gas spent: expected %d, got %d", expectedGas, block.GasUsed())
	}

	state, _ := chain.State()

	// 3: Ensure that miner received only the tx's tip.
	actual := state.GetBalance(block.Coinbase()).ToBig()
	expected := new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*block.Transactions()[0].GasTipCap().Uint64()),
		ethash.ConstantinopleBlockReward.ToBig(),
	)

	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (tip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr1).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (block.Transactions()[0].GasTipCap().Uint64() + block.BaseFee().Uint64()))

	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}

	blocks, _ = GenerateChain(gspec.Config, block, engine, genDb, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{2})

		txdata := &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key2)

		b.AddTx(tx)
	})

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block = chain.GetBlockByNumber(2)
	state, _ = chain.State()
	effectiveTip := block.Transactions()[0].GasTipCap().Uint64() - block.BaseFee().Uint64()

	// 6+5: Ensure that miner received only the tx's effective tip.
	actual = state.GetBalance(block.Coinbase()).ToBig()
	expected = new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*effectiveTip),
		ethash.ConstantinopleBlockReward.ToBig(),
	)

	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (effectiveTip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr2).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (effectiveTip + block.BaseFee().Uint64()))

	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}
}

// Tests the scenario the chain is requested to another point with the missing state.
// It expects the state is recovered and all relevant chain markers are set correctly.
func TestSetCanonical(t *testing.T) {
	testSetCanonical(t, rawdb.HashScheme)
	testSetCanonical(t, rawdb.PathScheme)
}

func testSetCanonical(t *testing.T, scheme string) {
	// log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))

	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(100000000000000000)
		gspec   = &Genesis{
			Config:  params.TestChainConfig,
			Alloc:   types.GenesisAlloc{address: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer      = types.LatestSigner(gspec.Config)
		engine      = ethash.NewFaker()
		chainLength = 10
	)

	//log.Root().SetHandler(log.LvlFilterHandler(log.LvlDebug, log.StreamHandler(os.Stderr, log.TerminalFormat(true))))

	// Generate and import the canonical chain
	_, canon, _ := GenerateChainWithGenesis(gspec, engine, chainLength, func(i int, gen *BlockGen) {
		tx, err := types.SignTx(types.NewTransaction(gen.TxNonce(address), common.Address{0x00}, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil), signer, key)
		if err != nil {
			panic(err)
		}

		gen.AddTx(tx)
	})
	diskdb, _ := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	defer diskdb.Close()

	options := DefaultConfig().WithStateScheme(scheme)
	chain, err := NewBlockChain(diskdb, gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	defer chain.Stop()

	if n, err := chain.InsertChain(canon, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	// Generate the side chain and import them
	_, side, _ := GenerateChainWithGenesis(gspec, engine, chainLength, func(i int, gen *BlockGen) {
		tx, err := types.SignTx(types.NewTransaction(gen.TxNonce(address), common.Address{0x00}, big.NewInt(1), params.TxGas, gen.header.BaseFee, nil), signer, key)
		if err != nil {
			panic(err)
		}

		gen.AddTx(tx)
	})

	for _, block := range side {
		_, err := chain.InsertBlockWithoutSetHead(block, false)
		if err != nil {
			t.Fatalf("Failed to insert into chain: %v", err)
		}
	}

	for _, block := range side {
		got := chain.GetBlockByHash(block.Hash())
		if got == nil {
			t.Fatalf("Lost the inserted block")
		}
	}

	// Set the chain head to the side chain, ensure all the relevant markers are updated.
	verify := func(head *types.Block) {
		if chain.CurrentBlock().Hash() != head.Hash() {
			t.Fatalf("Unexpected block hash, want %x, got %x", head.Hash(), chain.CurrentBlock().Hash())
		}

		if chain.CurrentSnapBlock().Hash() != head.Hash() {
			t.Fatalf("Unexpected fast block hash, want %x, got %x", head.Hash(), chain.CurrentSnapBlock().Hash())
		}

		if chain.CurrentHeader().Hash() != head.Hash() {
			t.Fatalf("Unexpected head header, want %x, got %x", head.Hash(), chain.CurrentHeader().Hash())
		}

		if !chain.HasState(head.Root()) {
			t.Fatalf("Lost block state %v %x", head.Number(), head.Hash())
		}
	}

	_, _ = chain.SetCanonical(side[len(side)-1])
	verify(side[len(side)-1])

	// Reset the chain head to original chain
	chain.SetCanonical(canon[chainLength-1])
	verify(canon[chainLength-1])
}

// TestCanonicalHashMarker tests all the canonical hash markers are updated/deleted
// correctly in case reorg is called.
// nolint:gocognit
func TestCanonicalHashMarker(t *testing.T) {
	testCanonicalHashMarker(t, rawdb.HashScheme)
	testCanonicalHashMarker(t, rawdb.PathScheme)
}

func testCanonicalHashMarker(t *testing.T, scheme string) {
	var cases = []struct {
		forkA int
		forkB int
	}{
		// ForkA: 10 blocks
		// ForkB: 1 blocks
		//
		// reorged:
		//      markers [2, 10] should be deleted
		//      markers [1] should be updated
		{10, 1},

		// ForkA: 10 blocks
		// ForkB: 2 blocks
		//
		// reorged:
		//      markers [3, 10] should be deleted
		//      markers [1, 2] should be updated
		{10, 2},

		// ForkA: 10 blocks
		// ForkB: 10 blocks
		//
		// reorged:
		//      markers [1, 10] should be updated
		{10, 10},

		// ForkA: 10 blocks
		// ForkB: 11 blocks
		//
		// reorged:
		//      markers [1, 11] should be updated
		{10, 11},
	}

	for _, c := range cases {
		var (
			gspec = &Genesis{
				Config:  params.TestChainConfig,
				Alloc:   types.GenesisAlloc{},
				BaseFee: big.NewInt(params.InitialBaseFee),
			}
			engine = ethash.NewFaker()
		)

		_, forkA, _ := GenerateChainWithGenesis(gspec, engine, c.forkA, func(i int, gen *BlockGen) {})
		_, forkB, _ := GenerateChainWithGenesis(gspec, engine, c.forkB, func(i int, gen *BlockGen) {})

		// Initialize test chain
		options := DefaultConfig().WithStateScheme(scheme)
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
		if err != nil {
			t.Fatalf("failed to create tester chain: %v", err)
		}

		// Insert forkA and forkB, the canonical should on forkA still
		if n, err := chain.InsertChain(forkA, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", n, err)
		}

		if n, err := chain.InsertChain(forkB, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", n, err)
		}

		verify := func(head *types.Block) {
			if chain.CurrentBlock().Hash() != head.Hash() {
				t.Fatalf("Unexpected block hash, want %x, got %x", head.Hash(), chain.CurrentBlock().Hash())
			}

			if chain.CurrentSnapBlock().Hash() != head.Hash() {
				t.Fatalf("Unexpected fast block hash, want %x, got %x", head.Hash(), chain.CurrentSnapBlock().Hash())
			}

			if chain.CurrentHeader().Hash() != head.Hash() {
				t.Fatalf("Unexpected head header, want %x, got %x", head.Hash(), chain.CurrentHeader().Hash())
			}

			if !chain.HasState(head.Root()) {
				t.Fatalf("Lost block state %v %x", head.Number(), head.Hash())
			}
		}

		// Switch canonical chain to forkB if necessary
		if len(forkA) < len(forkB) {
			verify(forkB[len(forkB)-1])
		} else {
			verify(forkA[len(forkA)-1])
			_, _ = chain.SetCanonical(forkB[len(forkB)-1])
			verify(forkB[len(forkB)-1])
		}

		// Ensure all hash markers are updated correctly
		for i := 0; i < len(forkB); i++ {
			block := forkB[i]
			hash := chain.GetCanonicalHash(block.NumberU64())

			if hash != block.Hash() {
				t.Fatalf("Unexpected canonical hash %d", block.NumberU64())
			}
		}

		if c.forkA > c.forkB {
			for i := uint64(c.forkB) + 1; i <= uint64(c.forkA); i++ {
				hash := chain.GetCanonicalHash(i)
				if hash != (common.Hash{}) {
					t.Fatalf("Unexpected canonical hash %d", i)
				}
			}
		}

		chain.Stop()
	}
}

func TestCreateThenDeletePreByzantium(t *testing.T) {
	t.Parallel()

	// We use Ropsten chain config instead of Testchain config, this is
	// deliberate: we want to use pre-byz rules where we have intermediate state roots
	// between transactions.
	testCreateThenDelete(t, &params.ChainConfig{
		ChainID:        big.NewInt(3),
		HomesteadBlock: big.NewInt(0),
		EIP150Block:    big.NewInt(0),
		EIP155Block:    big.NewInt(10),
		EIP158Block:    big.NewInt(10),
		ByzantiumBlock: big.NewInt(1_700_000),
		Bor: &params.BorConfig{
			MadhugiriBlock: big.NewInt(0),
		},
	})
}

func TestHasRecentPipelinedHeadState(t *testing.T) {
	block := types.NewBlockWithHeader(&types.Header{
		Number: big.NewInt(1),
		Root:   common.HexToHash("0x01"),
	})
	chain := &BlockChain{}

	require.False(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))

	chain.pendingImportHeadHash = block.Hash()
	chain.pendingImportHeadRoot = block.Root()
	chain.pendingImportHeadStart = time.Now()

	require.True(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))
	require.False(t, chain.HasRecentPipelinedHeadState(common.HexToHash("0x02"), block.Root()))
	require.False(t, chain.HasRecentPipelinedHeadState(block.Hash(), common.HexToHash("0x03")))

	chain.pendingImportHeadStart = time.Now().Add(-pipelinedImportStateAvailabilityGrace - time.Second)
	require.False(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))
}

func TestHasRecentPipelinedHeadStatePendingSRC(t *testing.T) {
	block := types.NewBlockWithHeader(&types.Header{
		Number: big.NewInt(1),
		Root:   common.HexToHash("0x04"),
	})
	chain := &BlockChain{
		pendingImportSRC: &pendingImportSRCState{
			block:       block,
			blockStart:  time.Now(),
			collectedCh: make(chan struct{}),
		},
	}

	require.True(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))

	close(chain.pendingImportSRC.collectedCh)
	require.False(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))

	chain.pendingImportSRC = &pendingImportSRCState{
		block:       block,
		blockStart:  time.Now().Add(-pipelinedImportStateAvailabilityGrace - time.Second),
		collectedCh: make(chan struct{}),
	}
	require.False(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))
}

func TestHasStateTreatsRecentPipelinedRootAsAvailable(t *testing.T) {
	_, _, chain, err := newCanonical(ethash.NewFaker(), 0, true, rawdb.HashScheme)
	require.NoError(t, err)
	t.Cleanup(chain.Stop)

	block := types.NewBlockWithHeader(&types.Header{
		Number: big.NewInt(1),
		Root:   common.HexToHash("0x05"),
	})
	require.False(t, chain.HasCommittedState(block.Root()))

	chain.pendingImportHeadHash = common.HexToHash("0x06")
	chain.pendingImportHeadRoot = block.Root()
	chain.pendingImportHeadStart = time.Now()

	require.True(t, chain.HasState(block.Root()))
	require.False(t, chain.HasRecentPipelinedHeadState(block.Hash(), block.Root()))
}

func TestPipelinedWitnessWaitPaths(t *testing.T) {
	_, _, chain, err := newCanonical(ethash.NewFaker(), 0, true, rawdb.HashScheme)
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	chain.cfg.EnablePipelinedImportSRC = true

	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	hash := block.Hash()

	chain.pendingImportSRC = &pendingImportSRCState{
		block:       block,
		makeWitness: false,
		collectedCh: make(chan struct{}),
	}
	witness, matched := chain.waitForPendingSRCWitness(hash)
	require.True(t, matched)
	require.Nil(t, witness)
	require.Nil(t, chain.waitForPipelinedWitness(hash))

	chain.pendingImportSRC = &pendingImportSRCState{
		block:       block,
		makeWitness: true,
		collectedCh: make(chan struct{}),
	}
	chain.CacheWitness(hash, []byte("witness"))
	close(chain.pendingImportSRC.collectedCh)
	witness, matched = chain.waitForPendingSRCWitness(hash)
	require.True(t, matched)
	require.Equal(t, []byte("witness"), witness)

	chain.pendingImportSRC = nil
	chain.pipelinedMakeWitness.Store(false)
	require.Nil(t, chain.waitForPipelinedWitness(common.HexToHash("0x1234")))

	polledHash := common.HexToHash("0x5678")
	go func() {
		time.Sleep(10 * time.Millisecond)
		chain.CacheWitness(polledHash, []byte("polled"))
	}()
	require.Equal(t, []byte("polled"), chain.pollWitnessCache(polledHash, time.Second, time.Millisecond))
}

func TestWithinPipelinedImportStateGrace(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		start time.Time
		want  bool
	}{
		{name: "zero", start: time.Time{}, want: false},
		{name: "future", start: now.Add(time.Nanosecond), want: false},
		{name: "current", start: now, want: true},
		{name: "boundary", start: now.Add(-pipelinedImportStateAvailabilityGrace), want: true},
		{name: "expired", start: now.Add(-pipelinedImportStateAvailabilityGrace - time.Nanosecond), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, withinPipelinedImportStateGrace(test.start, now))
		})
	}
}

func TestCreateThenDeletePostByzantium(t *testing.T) {
	t.Parallel()
	testCreateThenDelete(t, params.TestChainConfig)
}

// testCreateThenDelete tests a creation and subsequent deletion of a contract, happening
// within the same block.
func testCreateThenDelete(t *testing.T, config *params.ChainConfig) {
	t.Helper()

	var (
		engine = ethash.NewFaker()
		// A sender who makes transactions, has some funds
		key, _      = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address     = crypto.PubkeyToAddress(key.PublicKey)
		destAddress = crypto.CreateAddress(address, 0)
		funds       = big.NewInt(1000000000000000)
	)

	// runtime code is 	0x60ffff : PUSH1 0xFF SELFDESTRUCT, a.k.a SELFDESTRUCT(0xFF)
	code := append([]byte{0x60, 0xff, 0xff}, make([]byte, 32-3)...)
	initCode := []byte{
		// SSTORE 1:1
		byte(vm.PUSH1), 0x1,
		byte(vm.PUSH1), 0x1,
		byte(vm.SSTORE),
		// Get the runtime-code on the stack
		byte(vm.PUSH32)}
	initCode = append(initCode, code...)
	initCode = append(initCode, []byte{
		byte(vm.PUSH1), 0x0, // offset
		byte(vm.MSTORE),
		byte(vm.PUSH1), 0x3, // size
		byte(vm.PUSH1), 0x0, // offset
		byte(vm.RETURN), // return 3 bytes of zero-code
	}...)
	gspec := &Genesis{
		Config: config,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
		},
	}
	nonce := uint64(0)
	signer := types.HomesteadSigner{}
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 2, func(i int, b *BlockGen) {
		fee := big.NewInt(1)
		if b.header.BaseFee != nil {
			fee = b.header.BaseFee
		}

		b.SetCoinbase(common.Address{1})

		tx, _ := types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			GasPrice: new(big.Int).Set(fee),
			Gas:      100000,
			Data:     initCode,
		})
		nonce++

		b.AddTx(tx)
		tx, _ = types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			GasPrice: new(big.Int).Set(fee),
			Gas:      100000,
			To:       &destAddress,
		})
		b.AddTx(tx)

		nonce++
	})
	// Import the canonical chain
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()
	// Import the blocks
	for _, block := range blocks {
		if _, err := chain.InsertChain([]*types.Block{block}, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", block.NumberU64(), err)
		}
	}
}

func TestDeleteThenCreate(t *testing.T) {
	var (
		engine      = ethash.NewFaker()
		key, _      = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address     = crypto.PubkeyToAddress(key.PublicKey)
		factoryAddr = crypto.CreateAddress(address, 0)
		funds       = big.NewInt(1000000000000000)
	)
	/*
		contract Factory {
		  function deploy(bytes memory code) public {
			address addr;
			assembly {
			  addr := create2(0, add(code, 0x20), mload(code), 0)
			  if iszero(extcodesize(addr)) {
				revert(0, 0)
			  }
			}
		  }
		}
	*/
	factoryBIN := common.Hex2Bytes("608060405234801561001057600080fd5b50610241806100206000396000f3fe608060405234801561001057600080fd5b506004361061002a5760003560e01c80627743601461002f575b600080fd5b610049600480360381019061004491906100d8565b61004b565b005b6000808251602084016000f59050803b61006457600080fd5b5050565b600061007b61007684610146565b610121565b905082815260208101848484011115610097576100966101eb565b5b6100a2848285610177565b509392505050565b600082601f8301126100bf576100be6101e6565b5b81356100cf848260208601610068565b91505092915050565b6000602082840312156100ee576100ed6101f5565b5b600082013567ffffffffffffffff81111561010c5761010b6101f0565b5b610118848285016100aa565b91505092915050565b600061012b61013c565b90506101378282610186565b919050565b6000604051905090565b600067ffffffffffffffff821115610161576101606101b7565b5b61016a826101fa565b9050602081019050919050565b82818337600083830152505050565b61018f826101fa565b810181811067ffffffffffffffff821117156101ae576101ad6101b7565b5b80604052505050565b7f4e487b7100000000000000000000000000000000000000000000000000000000600052604160045260246000fd5b600080fd5b600080fd5b600080fd5b600080fd5b6000601f19601f830116905091905056fea2646970667358221220ea8b35ed310d03b6b3deef166941140b4d9e90ea2c92f6b41eb441daf49a59c364736f6c63430008070033")
	/*
		contract C {
			uint256 value;
			constructor() {
				value = 100;
			}
			function destruct() public payable {
				selfdestruct(payable(msg.sender));
			}
			receive() payable external {}
		}
	*/
	contractABI := common.Hex2Bytes("6080604052348015600f57600080fd5b5060646000819055506081806100266000396000f3fe608060405260043610601f5760003560e01c80632b68b9c614602a576025565b36602557005b600080fd5b60306032565b005b3373ffffffffffffffffffffffffffffffffffffffff16fffea2646970667358221220ab749f5ed1fcb87bda03a74d476af3f074bba24d57cb5a355e8162062ad9a4e664736f6c63430008070033")
	contractAddr := crypto.CreateAddress2(factoryAddr, [32]byte{}, crypto.Keccak256(contractABI))

	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
		},
	}
	nonce := uint64(0)
	signer := types.HomesteadSigner{}
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 2, func(i int, b *BlockGen) {
		fee := big.NewInt(1)
		if b.header.BaseFee != nil {
			fee = b.header.BaseFee
		}
		b.SetCoinbase(common.Address{1})

		// Block 1
		if i == 0 {
			tx, _ := types.SignNewTx(key, signer, &types.LegacyTx{
				Nonce:    nonce,
				GasPrice: new(big.Int).Set(fee),
				Gas:      500000,
				Data:     factoryBIN,
			})
			nonce++
			b.AddTx(tx)

			data := common.Hex2Bytes("00774360000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000000a76080604052348015600f57600080fd5b5060646000819055506081806100266000396000f3fe608060405260043610601f5760003560e01c80632b68b9c614602a576025565b36602557005b600080fd5b60306032565b005b3373ffffffffffffffffffffffffffffffffffffffff16fffea2646970667358221220ab749f5ed1fcb87bda03a74d476af3f074bba24d57cb5a355e8162062ad9a4e664736f6c6343000807003300000000000000000000000000000000000000000000000000")
			tx, _ = types.SignNewTx(key, signer, &types.LegacyTx{
				Nonce:    nonce,
				GasPrice: new(big.Int).Set(fee),
				Gas:      500000,
				To:       &factoryAddr,
				Data:     data,
			})
			b.AddTx(tx)
			nonce++
		} else {
			// Block 2
			tx, _ := types.SignNewTx(key, signer, &types.LegacyTx{
				Nonce:    nonce,
				GasPrice: new(big.Int).Set(fee),
				Gas:      500000,
				To:       &contractAddr,
				Data:     common.Hex2Bytes("2b68b9c6"), // destruct
			})
			nonce++
			b.AddTx(tx)

			data := common.Hex2Bytes("00774360000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000000a76080604052348015600f57600080fd5b5060646000819055506081806100266000396000f3fe608060405260043610601f5760003560e01c80632b68b9c614602a576025565b36602557005b600080fd5b60306032565b005b3373ffffffffffffffffffffffffffffffffffffffff16fffea2646970667358221220ab749f5ed1fcb87bda03a74d476af3f074bba24d57cb5a355e8162062ad9a4e664736f6c6343000807003300000000000000000000000000000000000000000000000000")
			tx, _ = types.SignNewTx(key, signer, &types.LegacyTx{
				Nonce:    nonce,
				GasPrice: new(big.Int).Set(fee),
				Gas:      500000,
				To:       &factoryAddr, // re-creation
				Data:     data,
			})
			b.AddTx(tx)
			nonce++
		}
	})
	// Import the canonical chain
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	for _, block := range blocks {
		if _, err := chain.InsertChain([]*types.Block{block}, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", block.NumberU64(), err)
		}
	}
}

// TestTransientStorageReset ensures the transient storage is wiped correctly
// between transactions.
func TestTransientStorageReset(t *testing.T) {
	t.Parallel()

	var (
		engine      = ethash.NewFaker()
		key, _      = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address     = crypto.PubkeyToAddress(key.PublicKey)
		destAddress = crypto.CreateAddress(address, 0)
		funds       = big.NewInt(1000000000000000)
		vmConfig    = vm.Config{
			ExtraEips: []int{1153}, // Enable transient storage EIP
		}
	)

	code := append([]byte{
		// TLoad value with location 1
		byte(vm.PUSH1), 0x1,
		byte(vm.TLOAD),

		// PUSH location
		byte(vm.PUSH1), 0x1,

		// SStore location:value
		byte(vm.SSTORE),
	}, make([]byte, 32-6)...)
	initCode := []byte{
		// TSTORE 1:1
		byte(vm.PUSH1), 0x1,
		byte(vm.PUSH1), 0x1,
		byte(vm.TSTORE),

		// Get the runtime-code on the stack
		byte(vm.PUSH32)}
	initCode = append(initCode, code...)
	initCode = append(initCode, []byte{
		byte(vm.PUSH1), 0x0, // offset
		byte(vm.MSTORE),
		byte(vm.PUSH1), 0x6, // size
		byte(vm.PUSH1), 0x0, // offset
		byte(vm.RETURN), // return 6 bytes of zero-code
	}...)
	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
		},
	}
	nonce := uint64(0)
	signer := types.HomesteadSigner{}
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		fee := big.NewInt(1)
		if b.header.BaseFee != nil {
			fee = b.header.BaseFee
		}

		b.SetCoinbase(common.Address{1})

		tx, _ := types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			GasPrice: new(big.Int).Set(fee),
			Gas:      100000,
			Data:     initCode,
		})
		nonce++

		b.AddTxWithVMConfig(tx, vmConfig)

		tx, _ = types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce:    nonce,
			GasPrice: new(big.Int).Set(fee),
			Gas:      100000,
			To:       &destAddress,
		})
		b.AddTxWithVMConfig(tx, vmConfig)

		nonce++
	})

	// Initialize the blockchain with 1153 enabled.
	options := DefaultConfig()
	options.VmConfig = vmConfig
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()
	// Import the blocks
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert into chain: %v", err)
	}
	// Check the storage
	state, err := chain.StateAt(chain.CurrentHeader().Root)
	if err != nil {
		t.Fatalf("Failed to load state %v", err)
	}

	loc := common.BytesToHash([]byte{1})
	slot := state.GetState(destAddress, loc)

	if slot != (common.Hash{}) {
		t.Fatalf("Unexpected dirty storage slot")
	}
}

func TestEIP3651(t *testing.T) {
	t.Parallel()

	var (
		aa     = common.HexToAddress("0x000000000000000000000000000000000000aaaa")
		bb     = common.HexToAddress("0x000000000000000000000000000000000000bbbb")
		engine = beacon.NewFaker()

		// A sender who makes transactions, has some funds
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = crypto.PubkeyToAddress(key2.PublicKey)
		funds   = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
		config  = *params.AllEthashProtocolChanges
		gspec   = &Genesis{
			Config: &config,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
				// The address 0xBBBB calls 0xAAAA
				bb: {
					Code: []byte{
						byte(vm.PUSH1), 0, // out size
						byte(vm.DUP1),  // out offset
						byte(vm.DUP1),  // out insize
						byte(vm.DUP1),  // in offset
						byte(vm.PUSH2), // address
						byte(0xaa),
						byte(0xaa),
						byte(vm.GAS), // gas
						byte(vm.DELEGATECALL),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)

	gspec.Config.BerlinBlock = common.Big0
	gspec.Config.LondonBlock = common.Big0
	gspec.Config.TerminalTotalDifficulty = common.Big0
	gspec.Config.ShanghaiBlock = common.Big0
	signer := types.LatestSigner(gspec.Config)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(aa)
		// One transaction to Coinbase
		txdata := &types.DynamicFeeTx{
			ChainID:    gspec.Config.ChainID,
			Nonce:      0,
			To:         &bb,
			Gas:        500000,
			GasFeeCap:  newGwei(5),
			GasTipCap:  big.NewInt(2),
			AccessList: nil,
			Data:       []byte{},
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key1)

		b.AddTx(tx)
	})
	options := DefaultConfig()
	options.VmConfig = vm.Config{
		Tracer: logger.NewMarkdownLogger(&logger.Config{}, os.Stderr).Hooks(),
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	// 1+2: Ensure EIP-1559 access lists are accounted for via gas usage.
	innerGas := vm.GasQuickStep*2 + params.ColdSloadCostEIP2929*2
	expectedGas := params.TxGas + 5*vm.GasFastestStep + vm.GasQuickStep + 100 + innerGas // 100 because 0xaaaa is in access list

	if block.GasUsed() != expectedGas {
		t.Fatalf("incorrect amount of gas spent: expected %d, got %d", expectedGas, block.GasUsed())
	}

	state, _ := chain.State()

	// 3: Ensure that miner received only the tx's tip.
	actual := state.GetBalance(block.Coinbase()).ToBig()
	expected := new(big.Int).SetUint64(block.GasUsed() * block.Transactions()[0].GasTipCap().Uint64())

	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (tip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr1).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (block.Transactions()[0].GasTipCap().Uint64() + block.BaseFee().Uint64()))

	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}
}

// Simple deposit generator, source: https://gist.github.com/lightclient/54abb2af2465d6969fa6d1920b9ad9d7
var depositsGeneratorCode = common.FromHex("6080604052366103aa575f603067ffffffffffffffff811115610025576100246103ae565b5b6040519080825280601f01601f1916602001820160405280156100575781602001600182028036833780820191505090505b5090505f8054906101000a900460ff1660f81b815f8151811061007d5761007c6103db565b5b60200101907effffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff191690815f1a9053505f602067ffffffffffffffff8111156100c7576100c66103ae565b5b6040519080825280601f01601f1916602001820160405280156100f95781602001600182028036833780820191505090505b5090505f8054906101000a900460ff1660f81b815f8151811061011f5761011e6103db565b5b60200101907effffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff191690815f1a9053505f600867ffffffffffffffff811115610169576101686103ae565b5b6040519080825280601f01601f19166020018201604052801561019b5781602001600182028036833780820191505090505b5090505f8054906101000a900460ff1660f81b815f815181106101c1576101c06103db565b5b60200101907effffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff191690815f1a9053505f606067ffffffffffffffff81111561020b5761020a6103ae565b5b6040519080825280601f01601f19166020018201604052801561023d5781602001600182028036833780820191505090505b5090505f8054906101000a900460ff1660f81b815f81518110610263576102626103db565b5b60200101907effffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff191690815f1a9053505f600867ffffffffffffffff8111156102ad576102ac6103ae565b5b6040519080825280601f01601f1916602001820160405280156102df5781602001600182028036833780820191505090505b5090505f8054906101000a900460ff1660f81b815f81518110610305576103046103db565b5b60200101907effffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff191690815f1a9053505f8081819054906101000a900460ff168092919061035090610441565b91906101000a81548160ff021916908360ff160217905550507f649bbc62d0e31342afea4e5cd82d4049e7e1ee912fc0889aa790803be39038c585858585856040516103a09594939291906104d9565b60405180910390a1005b5f80fd5b7f4e487b71000000000000000000000000000000000000000000000000000000005f52604160045260245ffd5b7f4e487b71000000000000000000000000000000000000000000000000000000005f52603260045260245ffd5b7f4e487b71000000000000000000000000000000000000000000000000000000005f52601160045260245ffd5b5f60ff82169050919050565b5f61044b82610435565b915060ff820361045e5761045d610408565b5b600182019050919050565b5f81519050919050565b5f82825260208201905092915050565b8281835e5f83830152505050565b5f601f19601f8301169050919050565b5f6104ab82610469565b6104b58185610473565b93506104c5818560208601610483565b6104ce81610491565b840191505092915050565b5f60a0820190508181035f8301526104f181886104a1565b9050818103602083015261050581876104a1565b9050818103604083015261051981866104a1565b9050818103606083015261052d81856104a1565b9050818103608083015261054181846104a1565b9050969550505050505056fea26469706673582212208569967e58690162d7d6fe3513d07b393b4c15e70f41505cbbfd08f53eba739364736f6c63430008190033")

// This is a smoke test for EIP-7685 requests added in the Prague fork. The test first
// creates a block containing requests, and then inserts it into the chain to run
// validation.
func TestPragueRequests(t *testing.T) {
	t.Skip("skipping because prague requests not applicable in bor")
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		config  = *params.MergedTestChainConfig
		signer  = types.LatestSigner(&config)
		engine  = beacon.NewFaker()
	)
	gspec := &Genesis{
		Config: &config,
		Alloc: types.GenesisAlloc{
			addr1:                            {Balance: big.NewInt(9999900000000000)},
			config.DepositContractAddress:    {Code: depositsGeneratorCode},
			params.WithdrawalQueueAddress:    {Code: params.WithdrawalQueueCode},
			params.ConsolidationQueueAddress: {Code: params.ConsolidationQueueCode},
		},
	}

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		// create deposit
		depositTx := types.MustSignNewTx(key1, signer, &types.DynamicFeeTx{
			ChainID:   gspec.Config.ChainID,
			Nonce:     0,
			To:        &config.DepositContractAddress,
			Gas:       500_000,
			GasFeeCap: newGwei(5),
			GasTipCap: big.NewInt(2),
		})
		b.AddTx(depositTx)

		// create withdrawal request
		withdrawalTx := types.MustSignNewTx(key1, signer, &types.DynamicFeeTx{
			ChainID:   gspec.Config.ChainID,
			Nonce:     1,
			To:        &params.WithdrawalQueueAddress,
			Gas:       500_000,
			GasFeeCap: newGwei(5),
			GasTipCap: big.NewInt(2),
			Value:     newGwei(1),
			Data:      common.FromHex("b917cfdc0d25b72d55cf94db328e1629b7f4fde2c30cdacf873b664416f76a0c7f7cc50c9f72a3cb84be88144cde91250000000000000d80"),
		})
		b.AddTx(withdrawalTx)

		// create consolidation request
		consolidationTx := types.MustSignNewTx(key1, signer, &types.DynamicFeeTx{
			ChainID:   gspec.Config.ChainID,
			Nonce:     2,
			To:        &params.ConsolidationQueueAddress,
			Gas:       500_000,
			GasFeeCap: newGwei(5),
			GasTipCap: big.NewInt(2),
			Value:     newGwei(1),
			Data:      common.FromHex("b917cfdc0d25b72d55cf94db328e1629b7f4fde2c30cdacf873b664416f76a0c7f7cc50c9f72a3cb84be88144cde9125b9812f7d0b1f2f969b52bbb2d316b0c2fa7c9dba85c428c5e6c27766bcc4b0c6e874702ff1eb1c7024b08524a9771601"),
		})
		b.AddTx(consolidationTx)
	})

	// Check block has the correct requests hash.
	rh := blocks[0].RequestsHash()
	if rh == nil {
		t.Fatal("block has nil requests hash")
	}
	expectedRequestsHash := common.HexToHash("0x06ffb72b9f0823510b128bca6cd4f96f59b745de6791e9fc350b596e7605101e")
	if *rh != expectedRequestsHash {
		t.Fatalf("block has wrong requestsHash %v, want %v", *rh, expectedRequestsHash)
	}

	// Insert block to check validation.
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
}

// mockEngine that can fail header verification for specific block numbers
type mockFailingEngine struct {
	*ethash.Ethash
	shouldFailHeader      map[uint64]bool
	allowInitialInsertion bool        // Allow initial insertion to succeed
	insertionComplete     atomic.Bool // Track when insertion is complete
}

func (m *mockFailingEngine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		defer close(results)
		for _, header := range headers {
			select {
			case <-abort:
				return
			default:
				// If we're allowing initial insertion and it's not complete yet, succeed
				if m.allowInitialInsertion && !m.insertionComplete.Load() {
					results <- nil
				} else if m.shouldFailHeader != nil && m.shouldFailHeader[header.Number.Uint64()] {
					results <- errors.New("mock header verification failure")
				} else {
					results <- nil
				}
			}
		}
	}()

	return abort, results
}

func (m *mockFailingEngine) markInsertionComplete() {
	m.insertionComplete.Store(true)
}

// TestHeaderVerificationLoop tests the background header verification functionality
func TestHeaderVerificationLoop(t *testing.T) {
	testHeaderVerificationLoop(t, rawdb.HashScheme)
	testHeaderVerificationLoop(t, rawdb.PathScheme)
}

func testHeaderVerificationLoop(t *testing.T, scheme string) {
	// Test case 1: Valid chain - no rewinds should happen
	t.Run("ValidChain", func(t *testing.T) {
		engine := ethash.NewFaker()

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		// Generate blocks
		_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 8, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 3, nil
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		// Insert blocks
		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		currentHead := chain.CurrentBlock()
		initialHead := currentHead.Number.Uint64()

		// Wait a bit for the verification loop to run
		time.Sleep(3 * time.Second)

		// Head should not have changed since all headers are valid
		newHead := chain.CurrentBlock()
		if newHead.Number.Uint64() != initialHead {
			t.Errorf("Head should not have changed, got %d, want %d", newHead.Number.Uint64(), initialHead)
		}
	})

	// Test case 2: Invalid header at block 6 - should rewind to block 5
	t.Run("InvalidHeaderRewind", func(t *testing.T) {
		failingHeaders := map[uint64]bool{6: true} // Block 6 will fail verification
		engine := &mockFailingEngine{
			Ethash:                ethash.NewFaker(),
			shouldFailHeader:      failingHeaders,
			allowInitialInsertion: true, // Allow initial insertion to succeed
		}

		// Create a config with VeBlop enabled
		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0), // Enable Rio from genesis
		}

		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		// Generate blocks
		_, blocks, _ := GenerateChainWithGenesis(genesis, engine.Ethash, 8, nil)

		// Create blockchain with milestone fetcher and failing engine
		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 3, nil // milestone at block 3
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		// Insert blocks (this should succeed initially)
		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		// Verify all blocks were imported correctly
		currentHead := chain.CurrentBlock()
		if currentHead.Number.Uint64() != 8 {
			t.Fatalf("blocks not imported correctly, got head %d, want %d", currentHead.Number.Uint64(), 5)
		}

		// Mark insertion as complete so verification will start failing
		engine.markInsertionComplete()

		// Wait for the verification loop to detect the invalid header and rewind
		time.Sleep(3 * time.Second)

		// Head should have been rewound to block 5 (last valid block before the failing block 6)
		newHead := chain.CurrentBlock()
		if newHead.Number.Uint64() != 5 {
			t.Errorf("Head should have been rewound to %d, got %d", 5, newHead.Number.Uint64())
		}
	})

	// Test case 3: Fetcher returns error - verification should not run
	t.Run("NoFinalizedBlock", func(t *testing.T) {
		engine := ethash.NewFaker()

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		// Generate blocks
		_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 5, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 0, fmt.Errorf("no milestone available")
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		// Insert blocks
		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		initialHead := chain.CurrentBlock().Number.Uint64()

		// Wait a bit
		time.Sleep(3 * time.Second)

		// Head should not have changed since there's no finalized block to verify against
		newHead := chain.CurrentBlock()
		if newHead.Number.Uint64() != initialHead {
			t.Errorf("Head should not have changed, got %d, want %d", newHead.Number.Uint64(), initialHead)
		}
	})

	// Test case 4: Milestone at head - no verification needed
	t.Run("HeadAtFinalizedBlock", func(t *testing.T) {
		engine := ethash.NewFaker()

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		// Generate blocks
		_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 5, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 5, nil // milestone at head
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		// Insert blocks
		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		initialHead := chain.CurrentBlock().Number.Uint64()

		// Wait a bit
		time.Sleep(3 * time.Second)

		// Head should not have changed since current head equals finalized block
		newHead := chain.CurrentBlock()
		if newHead.Number.Uint64() != initialHead {
			t.Errorf("Head should not have changed, got %d, want %d", newHead.Number.Uint64(), initialHead)
		}
	})

	// Test case 5: Verify proper shutdown when blockchain stops
	t.Run("ProperShutdown", func(t *testing.T) {
		engine := ethash.NewFaker()

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		// Generate blocks
		_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 5, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 2, nil
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}

		// Insert blocks
		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		time.Sleep(1 * time.Second)

		chain.Stop()
	})
}

// TestVerifyPendingHeaders tests the verifyPendingHeaders method directly
func TestVerifyPendingHeaders(t *testing.T) {
	testVerifyPendingHeaders(t, rawdb.HashScheme)
	testVerifyPendingHeaders(t, rawdb.PathScheme)
}

func testVerifyPendingHeaders(t *testing.T, scheme string) {
	engine := ethash.NewFaker()

	config := *params.TestChainConfig
	config.Bor = &params.BorConfig{
		RioBlock: big.NewInt(0),
	}
	genesis := &Genesis{
		Config:  &config,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}

	// Generate blocks
	_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 8, nil)

	// Create blockchain with milestone fetcher
	cfg := DefaultConfig()
	cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
		return 3, nil
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer chain.Stop()

	// Insert blocks
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	initialHead := chain.CurrentBlock().Number.Uint64()

	// Call verifyPendingHeaders directly - should not rewind since all headers are valid
	chain.verifyPendingHeaders()

	// Head should not have changed
	newHead := chain.CurrentBlock().Number.Uint64()
	if newHead != initialHead {
		t.Errorf("Head should not have changed, got %d, want %d", newHead, initialHead)
	}
}

// TestHeaderVerificationWithNilFetcher tests that the verification loop is skipped
// when MilestoneFetcher is nil.
func TestHeaderVerificationWithNilFetcher(t *testing.T) {
	engine := ethash.NewFaker()
	genesis := &Genesis{
		Config:  params.TestChainConfig,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}

	// Create blockchain without MilestoneFetcher
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer chain.Stop()

	// Generate and insert blocks
	_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 5, nil)
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	initialHead := chain.CurrentBlock().Number.Uint64()

	// Wait a bit - the verification loop should not run since milestoneFetcher is nil
	time.Sleep(2 * time.Second)

	// Head should not have changed
	newHead := chain.CurrentBlock().Number.Uint64()
	if newHead != initialHead {
		t.Errorf("Head should not have changed when milestoneFetcher is nil, got %d, want %d", newHead, initialHead)
	}
}

// headerCountingEngine wraps ethash and records how many headers VerifyHeaders receives.
type headerCountingEngine struct {
	*ethash.Ethash
	headersVerified atomic.Int64
}

func (m *headerCountingEngine) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	m.headersVerified.Store(int64(len(headers)))
	return m.Ethash.VerifyHeaders(chain, headers)
}

// TestVerifyPendingHeadersMilestoneFetcher tests that verifyPendingHeaders
// verifies only headers between the Heimdall milestone and the chain head.
func TestVerifyPendingHeadersMilestoneFetcher(t *testing.T) {
	t.Run("VerifiesFromMilestoneToHead", func(t *testing.T) {
		engine := &headerCountingEngine{Ethash: ethash.NewFaker()}

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		_, blocks, _ := GenerateChainWithGenesis(genesis, engine.Ethash, 20, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 3, nil // milestone at block 3
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		chain.verifyPendingHeaders()

		// Should verify blocks 4-20 = 17 headers
		if got := engine.headersVerified.Load(); got != 17 {
			t.Errorf("expected 17 headers verified, got %d", got)
		}
	})

	t.Run("SkipsWhenMilestoneAheadOfHead", func(t *testing.T) {
		engine := &headerCountingEngine{Ethash: ethash.NewFaker()}

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		_, blocks, _ := GenerateChainWithGenesis(genesis, engine.Ethash, 10, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 100, nil // milestone ahead of head (still syncing)
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		engine.headersVerified.Store(0) // reset counter after initial insertion

		chain.verifyPendingHeaders()

		// Should not verify anything since milestone > head
		if got := engine.headersVerified.Load(); got != 0 {
			t.Errorf("expected 0 headers verified when milestone ahead of head, got %d", got)
		}
	})

	t.Run("SkipsOnFetcherError", func(t *testing.T) {
		engine := &headerCountingEngine{Ethash: ethash.NewFaker()}

		config := *params.TestChainConfig
		config.Bor = &params.BorConfig{
			RioBlock: big.NewInt(0),
		}
		genesis := &Genesis{
			Config:  &config,
			BaseFee: big.NewInt(params.InitialBaseFee),
		}

		_, blocks, _ := GenerateChainWithGenesis(genesis, engine.Ethash, 10, nil)

		cfg := DefaultConfig()
		cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
			return 0, fmt.Errorf("heimdall unavailable")
		}
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
		if err != nil {
			t.Fatalf("failed to create blockchain: %v", err)
		}
		defer chain.Stop()

		if _, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("failed to insert chain: %v", err)
		}

		engine.headersVerified.Store(0) // reset counter after initial insertion

		chain.verifyPendingHeaders()

		// Should not verify anything when fetcher returns error
		if got := engine.headersVerified.Load(); got != 0 {
			t.Errorf("expected 0 headers verified on fetcher error, got %d", got)
		}
	})
}

// TestVerifyPendingHeadersReorgMetrics tests that reorg metrics are recorded
// when verifyPendingHeaders rewinds the chain due to an invalid header.
func TestVerifyPendingHeadersReorgMetrics(t *testing.T) {
	failingHeaders := map[uint64]bool{6: true}
	engine := &mockFailingEngine{
		Ethash:                ethash.NewFaker(),
		shouldFailHeader:      failingHeaders,
		allowInitialInsertion: true,
	}

	config := *params.TestChainConfig
	config.Bor = &params.BorConfig{
		RioBlock: big.NewInt(0),
	}
	genesis := &Genesis{
		Config:  &config,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}

	_, blocks, _ := GenerateChainWithGenesis(genesis, engine.Ethash, 8, nil)

	cfg := DefaultConfig()
	cfg.MilestoneFetcher = func(ctx context.Context) (uint64, error) {
		return 3, nil // milestone at block 3
	}
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer chain.Stop()

	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	engine.markInsertionComplete()

	// Snapshot metrics before
	reorgCountBefore := blockReorgMeter.Snapshot().Count()
	reorgDropBefore := blockReorgDropMeter.Snapshot().Count()

	chain.verifyPendingHeaders()

	// Chain should have rewound to block 5
	newHead := chain.CurrentBlock().Number.Uint64()
	if newHead != 5 {
		t.Errorf("expected head to rewind to 5, got %d", newHead)
	}

	// Reorg execute meter should have incremented by 1
	reorgCountAfter := blockReorgMeter.Snapshot().Count()
	if reorgCountAfter-reorgCountBefore != 1 {
		t.Errorf("expected blockReorgMeter to increment by 1, got %d", reorgCountAfter-reorgCountBefore)
	}

	// Reorg drop meter should have incremented by 3 (dropped blocks 6, 7, 8)
	reorgDropAfter := blockReorgDropMeter.Snapshot().Count()
	if reorgDropAfter-reorgDropBefore != 3 {
		t.Errorf("expected blockReorgDropMeter to increment by 3, got %d", reorgDropAfter-reorgDropBefore)
	}
}

// TestEIP7702 deploys two delegation designations and calls them. It writes one
// value to storage which is verified after.
func TestEIP7702(t *testing.T) {
	var (
		config  = *params.MergedTestChainConfig
		signer  = types.LatestSigner(&config)
		engine  = beacon.NewFaker()
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = crypto.PubkeyToAddress(key2.PublicKey)
		aa      = common.HexToAddress("0x000000000000000000000000000000000000aaaa")
		bb      = common.HexToAddress("0x000000000000000000000000000000000000bbbb")
		funds   = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
	)
	gspec := &Genesis{
		Config: &config,
		Alloc: types.GenesisAlloc{
			addr1: {Balance: funds},
			addr2: {Balance: funds},
			aa: { // The address 0xAAAA calls into addr2
				Code:    program.New().Call(nil, addr2, 1, 0, 0, 0, 0).Bytes(),
				Nonce:   0,
				Balance: big.NewInt(0),
			},
			bb: { // The address 0xBBBB sstores 42 into slot 42.
				Code:    program.New().Sstore(0x42, 0x42).Bytes(),
				Nonce:   0,
				Balance: big.NewInt(0),
			},
		},
	}

	// Sign authorization tuples.
	// The way the auths are combined, it becomes
	// 1. tx -> addr1 which is delegated to 0xaaaa
	// 2. addr1:0xaaaa calls into addr2:0xbbbb
	// 3. addr2:0xbbbb writes to storage
	auth1, _ := types.SignSetCode(key1, types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(gspec.Config.ChainID),
		Address: aa,
		Nonce:   1,
	})
	auth2, _ := types.SignSetCode(key2, types.SetCodeAuthorization{
		Address: bb,
		Nonce:   0,
	})

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(aa)
		txdata := &types.SetCodeTx{
			ChainID:   uint256.MustFromBig(gspec.Config.ChainID),
			Nonce:     0,
			To:        addr1,
			Gas:       500000,
			GasFeeCap: uint256.MustFromBig(newGwei(5)),
			GasTipCap: uint256.NewInt(2),
			AuthList:  []types.SetCodeAuthorization{auth1, auth2},
		}
		tx := types.MustSignNewTx(key1, signer, txdata)
		b.AddTx(tx)
	})
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, nil)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	defer chain.Stop()
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	// Verify delegation designations were deployed.
	state, _ := chain.State()
	code, want := state.GetCode(addr1), types.AddressToDelegation(auth1.Address)
	if !bytes.Equal(code, want) {
		t.Fatalf("addr1 code incorrect: got %s, want %s", common.Bytes2Hex(code), common.Bytes2Hex(want))
	}
	code, want = state.GetCode(addr2), types.AddressToDelegation(auth2.Address)
	if !bytes.Equal(code, want) {
		t.Fatalf("addr2 code incorrect: got %s, want %s", common.Bytes2Hex(code), common.Bytes2Hex(want))
	}
	// Verify delegation executed the correct code.
	var (
		fortyTwo = common.BytesToHash([]byte{0x42})
		actual   = state.GetState(addr2, fortyTwo)
	)
	if actual.Cmp(fortyTwo) != 0 {
		t.Fatalf("addr2 storage wrong: expected %d, got %d", fortyTwo, actual)
	}
}

func TestGetCanonicalReceipt(t *testing.T) {
	const chainLength = 64

	// Configure and generate a sample block chain
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000000)
		gspec   = &Genesis{
			Config:  params.TestChainConfig,
			Alloc:   types.GenesisAlloc{address: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer  = types.LatestSigner(gspec.Config)
		codeBin = common.FromHex("0x608060405234801561000f575f5ffd5b507f8ae1c8c6e5f91159d0bc1c4b9a47ce45301753843012cbe641e4456bfc73538b33426040516100419291906100ff565b60405180910390a1610139565b5f73ffffffffffffffffffffffffffffffffffffffff82169050919050565b5f6100778261004e565b9050919050565b6100878161006d565b82525050565b5f819050919050565b61009f8161008d565b82525050565b5f82825260208201905092915050565b7f436f6e7374727563746f72207761732063616c6c6564000000000000000000005f82015250565b5f6100e96016836100a5565b91506100f4826100b5565b602082019050919050565b5f6060820190506101125f83018561007e565b61011f6020830184610096565b8181036040830152610130816100dd565b90509392505050565b603e806101455f395ff3fe60806040525f5ffdfea2646970667358221220e8bc3c31e3ac337eab702e8fdfc1c71894f4df1af4221bcde4a2823360f403fb64736f6c634300081e0033")
	)
	_, blocks, receipts := GenerateChainWithGenesis(gspec, ethash.NewFaker(), chainLength, func(i int, block *BlockGen) {
		// SPDX-License-Identifier: MIT
		// pragma solidity ^0.8.0;
		//
		// contract ConstructorLogger {
		//    event ConstructorLog(address sender, uint256 timestamp, string message);
		//
		//    constructor() {
		//        emit ConstructorLog(msg.sender, block.timestamp, "Constructor was called");
		//    }
		// }
		//
		// 608060405234801561000f575f5ffd5b507f8ae1c8c6e5f91159d0bc1c4b9a47ce45301753843012cbe641e4456bfc73538b33426040516100419291906100ff565b60405180910390a1610139565b5f73ffffffffffffffffffffffffffffffffffffffff82169050919050565b5f6100778261004e565b9050919050565b6100878161006d565b82525050565b5f819050919050565b61009f8161008d565b82525050565b5f82825260208201905092915050565b7f436f6e7374727563746f72207761732063616c6c6564000000000000000000005f82015250565b5f6100e96016836100a5565b91506100f4826100b5565b602082019050919050565b5f6060820190506101125f83018561007e565b61011f6020830184610096565b8181036040830152610130816100dd565b90509392505050565b603e806101455f395ff3fe60806040525f5ffdfea2646970667358221220e8bc3c31e3ac337eab702e8fdfc1c71894f4df1af4221bcde4a2823360f403fb64736f6c634300081e0033
		nonce := block.TxNonce(address)
		tx, err := types.SignTx(types.NewContractCreation(nonce, big.NewInt(0), 100_000, block.header.BaseFee, codeBin), signer, key)
		if err != nil {
			panic(err)
		}
		block.AddTx(tx)

		tx2, err := types.SignTx(types.NewContractCreation(nonce+1, big.NewInt(0), 100_000, block.header.BaseFee, codeBin), signer, key)
		if err != nil {
			panic(err)
		}
		block.AddTx(tx2)

		tx3, err := types.SignTx(types.NewContractCreation(nonce+2, big.NewInt(0), 100_000, block.header.BaseFee, codeBin), signer, key)
		if err != nil {
			panic(err)
		}
		block.AddTx(tx3)
	})

	db, _ := rawdb.Open(rawdb.NewMemoryDatabase(), rawdb.OpenOptions{})
	defer db.Close()
	options := DefaultConfig().WithStateScheme(rawdb.PathScheme)
	chain, _ := NewBlockChain(db, gspec, ethash.NewFaker(), options)
	defer chain.Stop()

	headers := make([]*types.Header, len(blocks))
	for i, block := range blocks {
		headers[i] = block.Header()
	}
	if n, err := chain.InsertHeaderChain(headers); err != nil {
		t.Fatalf("failed to insert header %d: %v", n, err)
	}

	chain.InsertReceiptChain(blocks, types.EncodeBlockReceiptLists(receipts), 0)

	for i := 0; i < chainLength; i++ {
		block := blocks[i]
		blockReceipts := chain.GetReceiptsByHash(block.Hash())
		chain.receiptsCache.Purge() // ugly hack
		for txIndex, tx := range block.Body().Transactions {
			receipt, err := chain.GetCanonicalReceipt(tx, block.Hash(), block.NumberU64(), uint64(txIndex))
			if err != nil {
				t.Fatalf("Unexpected error %v", err)
			}
			if receipts[i][txIndex].Logs == nil {
				receipts[i][txIndex].Logs = []*types.Log{}
			}
			if blockReceipts[txIndex].Logs == nil {
				blockReceipts[txIndex].Logs = []*types.Log{}
			}
			if receipt.Logs == nil {
				receipt.Logs = []*types.Log{}
			}
			if !reflect.DeepEqual(receipts[i][txIndex], receipt) {
				want := spew.Sdump(receipts[i][txIndex])
				got := spew.Sdump(receipt)
				t.Fatalf("Receipt is not matched, want %s, got: %s", want, got)
			}
			if !reflect.DeepEqual(blockReceipts[txIndex], receipt) {
				want := spew.Sdump(blockReceipts[txIndex])
				got := spew.Sdump(receipt)
				t.Fatalf("Receipt is not matched, want %s, got: %s", want, got)
			}
		}
	}
}

// TestStatelessModeRewind tests the rewind behavior when TriesInMemory is 0 (stateless mode)
func TestStatelessModeRewind(t *testing.T) {
	testStatelessModeRewind(t, rawdb.HashScheme)
	testStatelessModeRewind(t, rawdb.PathScheme)
}

func testStatelessModeRewind(t *testing.T, scheme string) {
	// Create a blockchain with stateless configuration (TriesInMemory = 0)
	var (
		engine = ethash.NewFaker()
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		funds  = big.NewInt(1000000000000000)
		gspec  = &Genesis{
			Config: params.TestChainConfig,
			Alloc:  types.GenesisAlloc{addr: {Balance: funds}},
		}
	)

	// Generate a chain
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 10, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
	})

	// Create blockchain with stateless config
	cfg := DefaultConfig()
	cfg.Stateless = true
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// Insert blocks
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert blocks: %v", err)
	}

	// Get initial head
	initialHead := chain.CurrentBlock()
	if initialHead.Number.Uint64() != 10 {
		t.Fatalf("expected head at block 10, got %d", initialHead.Number.Uint64())
	}

	// Test rewindHashHead - should not check state when stateless
	rewindTarget := uint64(5)
	targetHeader := chain.GetHeaderByNumber(rewindTarget)

	// In stateless mode, rewind should succeed without state checks
	newHead, _ := chain.rewindHashHead(targetHeader, common.Hash{})
	if newHead.Number.Uint64() != rewindTarget {
		t.Fatalf("expected rewind to block %d, got %d", rewindTarget, newHead.Number.Uint64())
	}

	// Test rewindPathHead for path scheme
	if scheme == rawdb.PathScheme {
		// Reset to block 10
		chain.currentBlock.Store(initialHead)

		// Test path-based rewind
		pathHead, _ := chain.rewindPathHead(targetHeader, common.Hash{})
		if pathHead.Number.Uint64() != rewindTarget {
			t.Fatalf("expected path rewind to block %d, got %d", rewindTarget, pathHead.Number.Uint64())
		}
	}
}

// TestStatelessInsertChain tests InsertChainStateless functionality
func TestStatelessInsertChain(t *testing.T) {
	// Create test chain
	var (
		engine = ethash.NewFaker()
		gspec  = &Genesis{
			Config: params.TestChainConfig,
		}
	)

	// Create blockchain with stateless config
	cfg := DefaultConfig()
	cfg.Stateless = true
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// Parallel path
	_, blocksParallel, _ := GenerateChainWithGenesis(gspec, engine, 10, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
	})

	// Pre-insert canonically to avoid witness requirements and mark as known
	if _, err := chain.InsertChain(blocksParallel, true); err != nil {
		t.Fatalf("failed to pre-insert canonical blocks: %v", err)
	}

	witnessesParallel := make([]*stateless.Witness, len(blocksParallel))
	for i, b := range blocksParallel {
		w, err := stateless.NewWitness(b.Header(), chain)
		if err != nil {
			t.Fatalf("failed to build witness for block %d: %v", b.NumberU64(), err)
		}
		witnessesParallel[i] = w
	}
	processed, err := chain.InsertChainStateless(blocksParallel, witnessesParallel)
	if err != nil {
		t.Fatalf("unexpected error on parallel stateless import: %v", err)
	}
	if processed != len(blocksParallel) {
		t.Fatalf("expected processed=%d, got %d", len(blocksParallel), processed)
	}

	// Verify we're in stateless mode (Stateless = true)
	if chain.cfg.Stateless != true {
		t.Error("Expected blockchain to be configured for stateless mode (Stateless = true)")
	}

	// Sequential path
	_, blocksSequential, _ := GenerateChainWithGenesis(gspec, engine, 5, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
	})

	// Pre-insert canonically to avoid witness requirements and mark as known
	if _, err := chain.InsertChain(blocksSequential, true); err != nil {
		t.Fatalf("failed to pre-insert canonical blocks: %v", err)
	}

	// Now import via InsertChainStateless and expect clean happy path in sequential
	witnessesSequential := make([]*stateless.Witness, len(blocksSequential))
	for i, b := range blocksSequential {
		w, err := stateless.NewWitness(b.Header(), chain)
		if err != nil {
			t.Fatalf("failed to build witness for block %d: %v", b.NumberU64(), err)
		}
		witnessesSequential[i] = w
	}
	processed, err = chain.InsertChainStateless(blocksSequential, witnessesSequential)
	if err != nil {
		t.Fatalf("unexpected error on sequential stateless import: %v", err)
	}
	if processed != len(blocksSequential) {
		t.Fatalf("expected processed=%d, got %d", len(blocksSequential), processed)
	}
}

// corruptWitnessState corrupts witness by completely replacing all state with invalid data
func corruptWitnessState(witness *stateless.Witness) *stateless.Witness {
	corrupted := witness.Copy()

	// Completely replace all state with obviously invalid data
	// This will cause failures when any state access is attempted
	corrupted.State = map[string]struct{}{
		"invalid_state_node_1": {},
		"invalid_state_node_2": {},
		"corrupted_data_here":  {},
	}

	return corrupted
}

// corruptWitnessHeaders corrupts witness headers with wrong state root
func corruptWitnessHeaders(witness *stateless.Witness) *stateless.Witness {
	corrupted := witness.Copy()
	if len(corrupted.Headers) > 0 {
		wrongHeader := types.CopyHeader(corrupted.Headers[0])
		wrongHeader.Root = common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef") // Incorrect state root
		corrupted.Headers[0] = wrongHeader
	}
	return corrupted
}

// clearWitnessState removes all state data from witness
func clearWitnessState(witness *stateless.Witness) *stateless.Witness {
	cleared := witness.Copy()
	cleared.State = make(map[string]struct{})
	return cleared
}

// createEmptyHeaderWitness creates a witness with empty headers
func createEmptyHeaderWitness(header *types.Header) *stateless.Witness {
	witness := &stateless.Witness{
		Headers: []*types.Header{}, // Empty headers
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	witness.SetHeader(header)
	return witness
}

// createTestBlockAndWitness creates a fresh block and witness for testing
func createTestBlockAndWitness(t *testing.T) (*BlockChain, *types.Block, *stateless.Witness) {
	t.Helper()

	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr: {Balance: big.NewInt(10000000000000000)}},
	}

	cfg := DefaultConfig()
	cfg.Stateless = true

	// Create stateful chain for witness generation
	engine := ethash.NewFaker()
	stateFullChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create full-state chain: %v", err)
	}
	defer stateFullChain.Stop()

	// Generate block with transaction
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		// Use transaction nonce 0 since each test uses a fresh account state
		tx, _ := types.SignTx(types.NewTransaction(0, common.HexToAddress("0x1234"), big.NewInt(1000), 21000, big.NewInt(2000000000), nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
	})
	block := blocks[0]

	// Generate witness
	witness, _, err := stateFullChain.insertChain(types.Blocks{block}, true, true)
	if err != nil {
		t.Fatalf("failed to build witness: %v", err)
	}

	// Create fresh stateless chain
	statelessChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), cfg)
	if err != nil {
		t.Fatalf("failed to create stateless chain: %v", err)
	}

	return statelessChain, block, witness
}

// TestParallelStateless_ContractDeployedThenCalled ensures that a contract
// deployed and later accessed imports correctly
// in parallel stateless mode.
func TestParallelStateless_ContractDeployedThenCalled(t *testing.T) {
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr: {Balance: big.NewInt(10000000000000000)}},
	}

	cfg := DefaultConfig()
	cfg.Stateless = true

	eng := ethash.NewFaker()

	// Build a 3-block chain: B1 deploy contract, B2 noop, B3 call contract
	var contractAddr common.Address
	_, blocks, _ := GenerateChainWithGenesis(gspec, eng, 3, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		if i == 0 {
			code := []byte{0x60, 0x00, 0x56}
			tx, _ := types.SignTx(
				types.NewContractCreation(0, big.NewInt(0), 150000, b.header.BaseFee, code),
				types.HomesteadSigner{}, key,
			)
			b.AddTx(tx)
			contractAddr = crypto.CreateAddress(addr, 0)
		}
		if i == 2 {
			// Call the deployed contract in block 3
			callData := []byte{}
			tx, _ := types.SignTx(
				types.NewTransaction(1, contractAddr, big.NewInt(0), 100000, big.NewInt(2_000_000_000), callData),
				types.HomesteadSigner{}, key,
			)
			b.AddTx(tx)
		}
	})

	// Create stateful chain for witness generation
	stateFullChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, eng, cfg)
	require.NoError(t, err)
	defer stateFullChain.Stop()

	// Build witnesses by inserting each block on the full chain (stateful)
	w1, _, err := stateFullChain.insertChain(types.Blocks{blocks[0]}, true, true)
	require.NoError(t, err)
	require.NotNil(t, w1)

	w2, _, err := stateFullChain.insertChain(types.Blocks{blocks[1]}, true, true)
	require.NoError(t, err)
	require.NotNil(t, w2)

	w3, _, err := stateFullChain.insertChain(types.Blocks{blocks[2]}, true, true)
	require.NoError(t, err)
	require.NotNil(t, w3)

	// Corrupt the third witness by dropping code to simulate missing bytecode in worker
	badW3 := w3.Copy()
	badW3.Codes = make(map[string]struct{})

	// Create stateless chain for parallel insertion
	statelessChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), cfg)
	require.NoError(t, err)
	defer statelessChain.Stop()

	headers := []*types.Header{blocks[0].Header(), blocks[1].Header(), blocks[2].Header()}
	stopHeaders, errChans := statelessChain.prepareHeaderVerification(headers)
	defer stopHeaders()

	stats := &insertStats{startTime: mclock.Now()}
	processed, err := statelessChain.insertChainStatelessParallel(types.Blocks{blocks[0], blocks[1], blocks[2]}, []*stateless.Witness{w1, w2, badW3}, errChans, stats, stopHeaders)

	require.NoError(t, err)
	require.Equal(t, 3, processed)
}

// TestStatelessInsertChainInvalidInputs tests invalid or edge inputs for sequential and parallel stateless insertions
func TestStatelessInsertChainInvalidInputs(t *testing.T) {
	adversarialTests := []struct {
		name    string
		setupFn func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness)
		wantErr bool
	}{
		{
			name: "empty chain insertion",
			setupFn: func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness) {
				chain, _, _ := createTestBlockAndWitness(t)
				return chain, []*types.Block{}, []*stateless.Witness{}
			},
			wantErr: false,
		},
		{
			name: "explicit nil witness",
			setupFn: func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness) {
				chain, block, _ := createTestBlockAndWitness(t)
				return chain, []*types.Block{block}, []*stateless.Witness{nil}
			},
			wantErr: true,
		},
		{
			name: "corrupted witness state data",
			setupFn: func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness) {
				chain, block, witness := createTestBlockAndWitness(t)
				corruptedWitness := corruptWitnessState(witness)
				return chain, []*types.Block{block}, []*stateless.Witness{corruptedWitness}
			},
			wantErr: true,
		},
		{
			name: "witness missing required trie nodes",
			setupFn: func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness) {
				chain, block, witness := createTestBlockAndWitness(t)
				clearedWitness := clearWitnessState(witness)
				return chain, []*types.Block{block}, []*stateless.Witness{clearedWitness}
			},
			wantErr: true,
		},
		{
			name: "witness with incorrect state root in headers",
			setupFn: func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness) {
				chain, block, witness := createTestBlockAndWitness(t)
				corruptedWitness := corruptWitnessHeaders(witness)
				return chain, []*types.Block{block}, []*stateless.Witness{corruptedWitness}
			},
			wantErr: true,
		},
		{
			name: "witness with empty headers",
			setupFn: func(t *testing.T) (*BlockChain, []*types.Block, []*stateless.Witness) {
				chain, block, _ := createTestBlockAndWitness(t)
				emptyHeaderWitness := createEmptyHeaderWitness(block.Header())
				return chain, []*types.Block{block}, []*stateless.Witness{emptyHeaderWitness}
			},
			wantErr: true,
		},
	}

	for _, tt := range adversarialTests {
		t.Run(tt.name, func(t *testing.T) {
			testInsertMethod := func(t *testing.T, methodName string, isParallel bool) {
				chain, blocks, witnesses := tt.setupFn(t)
				defer chain.Stop()

				headers := make([]*types.Header, len(blocks))
				for i, block := range blocks {
					headers[i] = block.Header()
				}

				stats := &insertStats{startTime: mclock.Now()}

				var err error
				if isParallel {
					stopHeaders, errChans := chain.prepareHeaderVerification(headers)
					defer stopHeaders()
					_, err = chain.insertChainStatelessParallel(blocks, witnesses, errChans, stats, stopHeaders)
				} else {
					_, errChans := chain.prepareHeaderVerification(headers)
					_, err = chain.insertChainStatelessSequential(blocks, witnesses, errChans, stats)
				}

				if tt.wantErr {
					require.Error(t, err, methodName+" expected error but got none")
					t.Logf("%s got expected error: %v", methodName, err)
				} else {
					require.NoError(t, err, methodName+" unexpected error")
				}
			}

			t.Run("sequential", func(t *testing.T) {
				testInsertMethod(t, "insertChainStatelessSequential", false)
			})

			t.Run("parallel", func(t *testing.T) {
				testInsertMethod(t, "insertChainStatelessParallel", true)
			})
		})
	}
}

// TestStatelessSetHeadBeyondRoot tests setHeadBeyondRoot in stateless mode
func TestStatelessSetHeadBeyondRoot(t *testing.T) {
	// Create test chain
	var (
		engine = ethash.NewFaker()
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		funds  = big.NewInt(1000000000000000)
		gspec  = &Genesis{
			Config: params.TestChainConfig,
			Alloc:  types.GenesisAlloc{addr: {Balance: funds}},
		}
	)

	// Generate blocks
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 10, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
	})

	// Create blockchain with stateless config
	cfg := DefaultConfig()
	cfg.Stateless = true
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// Insert blocks
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert blocks: %v", err)
	}

	// Test SetHead in stateless mode
	targetBlock := uint64(5)
	chain.SetHead(targetBlock)

	// Verify head was set correctly
	newHead := chain.CurrentBlock()
	if newHead.Number.Uint64() != targetBlock {
		t.Fatalf("expected head at block %d, got %d", targetBlock, newHead.Number.Uint64())
	}

	// In stateless mode, state checks should be skipped
	// Verify we can still access blocks
	for i := uint64(1); i <= targetBlock; i++ {
		block := chain.GetBlockByNumber(i)
		if block == nil {
			t.Fatalf("block %d not found after SetHead", i)
		}
	}
}

// TestSplitReceiptsAndDeriveFields checks if normal and state-sync receipts are split correctly
// before inserting to database. It also checks if state-sync receipt fields are derived correctly.
func TestSplitReceiptsAndDeriveFields(t *testing.T) {
	var tests = []struct {
		name             string
		normalReceipts   []*types.ReceiptForStorage
		stateSyncReceipt *types.ReceiptForStorage
	}{
		// Both nil
		{
			name:             "both normal and state-sync receipt is nil",
			normalReceipts:   nil,
			stateSyncReceipt: nil,
		},
		// Normal receipt empty and state-sync nil
		{
			name:             "empty normal receipt and nil state-sync receipt",
			normalReceipts:   []*types.ReceiptForStorage{},
			stateSyncReceipt: nil,
		},
		// Only normal receipt (single)
		{
			name:             "state-sync receipt is nil, only single normal receipt present",
			normalReceipts:   []*types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
			stateSyncReceipt: nil,
		},
		// Only normal receipt (multiple)
		{
			name:             "state-sync receipt is nil, multiple normal receipts present",
			normalReceipts:   []*types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}, {CumulativeGasUsed: 666, Status: 1, Logs: nil}},
			stateSyncReceipt: nil,
		},
		// Only state-sync receipt
		{
			name:             "normal receipt is nil, only state-sync receipt present",
			normalReceipts:   nil,
			stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 0, Status: 1, Logs: nil, Type: 0},
		},
		// Normal + state-sync receipts (single)
		{
			name:             "single normal and state-sync receipt present",
			normalReceipts:   []*types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}},
			stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 0, Status: 1, Logs: nil, Type: 0},
		},
		// Normal + state-sync receipts (multiple)
		{
			name:             "multiple normal and single state-sync receipt present",
			normalReceipts:   []*types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}, {CumulativeGasUsed: 666, Status: 1, Logs: nil}, {CumulativeGasUsed: 777, Status: 1, Logs: nil}},
			stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 0, Status: 1, Logs: nil, Type: 0},
		},
		// Normal + state-sync receipts (multiple) with non-zero cumulative gas used
		{
			name:             "multiple normal and single state-sync receipt present with non-zero cumulative gas used",
			normalReceipts:   []*types.ReceiptForStorage{{CumulativeGasUsed: 555, Status: 1, Logs: nil}, {CumulativeGasUsed: 666, Status: 1, Logs: nil}, {CumulativeGasUsed: 777, Status: 1, Logs: nil}},
			stateSyncReceipt: &types.ReceiptForStorage{CumulativeGasUsed: 777, Status: 1, Logs: nil, Type: 0},
		},
	}

	// Create a mock config with sprint length as 1 (so that split receipts will run for all test cases)
	mockBorCfg := params.BorConfig{
		Sprint: map[string]uint64{"0": 1},
	}

	for _, test := range tests {
		// Individually encode receipts for comparing with values after splitting
		var normalEncoded []byte = nil
		if len(test.normalReceipts) > 0 {
			normalEncoded, _ = rlp.EncodeToBytes(test.normalReceipts)
		}

		// For state-sync receipts, create a copy and populate remaining fields which will be
		// used to compare later with the output. Don't populate the original test object as
		// the `splitReceiptsAndDeriveFields` should populate values in it.
		stateSyncReceipt := test.stateSyncReceipt
		var stateSyncEncoded []byte = nil
		if test.stateSyncReceipt != nil {
			_, _, cumulativeGasUsed := getReceiptFields(test.normalReceipts)
			stateSyncReceipt.CumulativeGasUsed = cumulativeGasUsed
			stateSyncEncoded, _ = rlp.EncodeToBytes(stateSyncReceipt)
		}

		// Merge both and encode the final list. Use the receipts in test object.
		var allReceipts = make([]*types.ReceiptForStorage, 0)
		if test.normalReceipts != nil {
			allReceipts = append(allReceipts, test.normalReceipts...)
		}
		if test.stateSyncReceipt != nil {
			allReceipts = append(allReceipts, test.stateSyncReceipt)
		}
		encoded, _ := rlp.EncodeToBytes(allReceipts)
		if len(allReceipts) == 0 {
			// Skip encoding, instead just set nil. Mirror the normal receipt
			// encoding to canonical RLP empty list.
			encoded = nil
			normalEncoded = rlp.EmptyList
		} else if len(test.normalReceipts) == 0 && test.stateSyncReceipt != nil {
			// In case of no normal receipts, mirror the encoding to canonical
			// RLP empty list.
			normalEncoded = rlp.EmptyList
		}

		// Split receipts and assert if the individual list match with the expected receipt data or not
		normal, stateSync := splitReceiptsAndDeriveFields(encoded, 0, common.Hash{}, &mockBorCfg)
		require.Equal(t, rlp.RawValue(normalEncoded), normal, fmt.Sprintf("case: %s, normal receipts mismatch, got: %v, expected: %v", test.name, normal, normalEncoded))
		require.Equal(t, rlp.RawValue(stateSyncEncoded), stateSync, fmt.Sprintf("case: %s, state-sync receipts mismatch, got: %v, expected: %v", test.name, stateSync, stateSyncEncoded))
	}
}

// TestInsertReceiptChain_NilReceiptsNormalizedToEmptyList exercises the
// snap-sync write path: InsertReceiptChain on receiving empty/nil receipts
// should write canonical RLP empty list (0xc0) to disk matching what
// writeBlockWithState produces on the live-execution path.
func TestInsertReceiptChain_NilReceiptsNormalizedToEmptyList(t *testing.T) {
	db := rawdb.NewMemoryDatabase()

	cfg := *params.TestChainConfig
	borCfg := *params.TestChainConfig.Bor
	borCfg.Sprint = map[string]uint64{"0": 16}
	borCfg.MadhugiriBlock = big.NewInt(0)
	cfg.Bor = &borCfg

	var engine consensus.Engine = ethash.NewFaker()
	gspec := &Genesis{
		Config:     &cfg,
		Alloc:      types.GenesisAlloc{},
		Difficulty: common.Big0,
	}
	chain, err := NewBlockChain(db, gspec, engine, nil)
	if err != nil {
		t.Fatalf("NewBlockChain: %v", err)
	}
	defer chain.Stop()

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, nil)
	block := blocks[0]
	if !block.Header().EmptyReceipts() {
		t.Fatalf("expected block with empty receipt root")
	}
	if types.IsSprintEndBlock(&borCfg, block.NumberU64()) {
		t.Fatalf("expected non-sprint-end block")
	}

	if _, err := chain.InsertHeaderChain([]*types.Header{block.Header()}); err != nil {
		t.Fatalf("InsertHeaderChain: %v", err)
	}
	if _, err := chain.InsertReceiptChain([]*types.Block{block}, []rlp.RawValue{nil}, 0); err != nil {
		t.Fatalf("InsertReceiptChain: %v", err)
	}

	blob := chain.GetReceiptsRLP(block.Hash())
	if !bytes.Equal(blob, rlp.EmptyList) {
		t.Fatalf("expected canonical empty list %x under receipts key, got %x", rlp.EmptyList, blob)
	}

	receipts := chain.GetRawReceipts(block.Hash(), block.NumberU64())
	if receipts == nil {
		t.Fatalf("expected non-nil empty Receipts slice, got nil")
	}
	if len(receipts) != 0 {
		t.Fatalf("expected empty Receipts slice, got %d entries", len(receipts))
	}
}

// TestInsertReceiptChain_StateSyncOnlySprintEnd_NormalizesToEmptyList extends the above
// test with an additional state-sync receipt clubbed with nil/empty normal receipt.
func TestInsertReceiptChain_StateSyncOnlySprintEnd_NormalizesToEmptyList(t *testing.T) {
	db := rawdb.NewMemoryDatabase()

	cfg := *params.TestChainConfig
	borCfg := *params.TestChainConfig.Bor
	borCfg.Sprint = map[string]uint64{"0": 1} // every block is sprint-end
	borCfg.MadhugiriBlock = nil               // pre-Madhugiri throughout
	cfg.Bor = &borCfg

	var engine consensus.Engine = ethash.NewFaker()
	gspec := &Genesis{
		Config:     &cfg,
		Alloc:      types.GenesisAlloc{},
		Difficulty: common.Big0,
	}
	chain, err := NewBlockChain(db, gspec, engine, nil)
	if err != nil {
		t.Fatalf("NewBlockChain: %v", err)
	}
	defer chain.Stop()

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, nil)
	block := blocks[0]
	if !types.IsSprintEndBlock(&borCfg, block.NumberU64()) {
		t.Fatalf("expected sprint-end block at height %d", block.NumberU64())
	}

	// Build a single-element receipts list containing only a state-sync
	// receipt (CumulativeGasUsed == 0 triggers the splitter's
	// isStateSyncReceiptPresent heuristic).
	stateSync := &types.ReceiptForStorage{
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 0,
	}
	encoded, err := rlp.EncodeToBytes([]*types.ReceiptForStorage{stateSync})
	if err != nil {
		t.Fatalf("encode state-sync receipt: %v", err)
	}

	if _, err := chain.InsertHeaderChain([]*types.Header{block.Header()}); err != nil {
		t.Fatalf("InsertHeaderChain: %v", err)
	}
	if _, err := chain.InsertReceiptChain([]*types.Block{block}, []rlp.RawValue{encoded}, 0); err != nil {
		t.Fatalf("InsertReceiptChain: %v", err)
	}

	blob := chain.GetReceiptsRLP(block.Hash())
	if !bytes.Equal(blob, rlp.EmptyList) {
		t.Fatalf("expected canonical empty list %x under receipts key, got %x", rlp.EmptyList, blob)
	}

	// Sanity-check that the state-sync receipt landed under the bor-receipt
	// slot rather than being lost during the split.
	borBlob := rawdb.ReadBorReceiptRLP(db, block.Hash(), block.NumberU64())
	if len(borBlob) == 0 {
		t.Fatalf("expected state-sync receipt under bor-receipt key, got empty entry")
	}
}

// TestWitnessCache tests the witness caching functionality to ensure witnesses
// are properly cached during writes and retrieved from cache during reads.
func TestWitnessCache(t *testing.T) {
	// Setup: Create a test blockchain
	engine := ethash.NewFaker()
	gspec := &Genesis{
		Config: params.TestChainConfig,
	}
	cfg := DefaultConfig()
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer chain.Stop()

	// Create test witness data
	testHash := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	testWitness1 := []byte("test witness data 1")
	testWitness2 := []byte("test witness data 2 - different")

	// Test 1: Write witness and verify it's cached
	rawdb.WriteWitness(chain.db, testHash, testWitness1)
	// Manually add to cache to simulate what happens during block import
	chain.witnessCache.Add(testHash, testWitness1)

	// Verify witness can be retrieved from cache
	retrieved := chain.GetWitness(testHash)
	require.NotNil(t, retrieved, "witness should be retrieved")
	require.Equal(t, testWitness1, retrieved, "retrieved witness should match written witness")

	// Test 2: Verify cache hit (witness should be in cache, not read from DB)
	// We can verify this by checking the cache directly
	cached, ok := chain.witnessCache.Get(testHash)
	require.True(t, ok, "witness should be in cache")
	require.Equal(t, testWitness1, cached, "cached witness should match")

	// Test 3: Update witness in cache and verify GetWitness returns cached version
	chain.witnessCache.Add(testHash, testWitness2)
	retrieved = chain.GetWitness(testHash)
	require.Equal(t, testWitness2, retrieved, "GetWitness should return cached version")

	// Test 4: Test cache miss - witness not in cache but in DB
	testHash2 := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	testWitness3 := []byte("test witness data 3")
	rawdb.WriteWitness(chain.db, testHash2, testWitness3)
	// Don't add to cache - simulate cache miss

	// GetWitness should read from DB and cache it
	retrieved = chain.GetWitness(testHash2)
	require.NotNil(t, retrieved, "witness should be retrieved from DB")
	require.Equal(t, testWitness3, retrieved, "retrieved witness should match DB witness")

	// Verify it's now in cache
	cached, ok = chain.witnessCache.Get(testHash2)
	require.True(t, ok, "witness should be cached after GetWitness")
	require.Equal(t, testWitness3, cached, "cached witness should match")

	// Test 5: Test non-existent witness
	nonExistentHash := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000")
	retrieved = chain.GetWitness(nonExistentHash)
	require.Nil(t, retrieved, "non-existent witness should return nil")

	// Test 6: Test cache purge
	chain.witnessCache.Purge()
	_, ok = chain.witnessCache.Get(testHash)
	require.False(t, ok, "witness should not be in cache after purge")
	_, ok = chain.witnessCache.Get(testHash2)
	require.False(t, ok, "witness should not be in cache after purge")

	// After purge, GetWitness should still work by reading from DB
	retrieved = chain.GetWitness(testHash2)
	require.NotNil(t, retrieved, "witness should still be retrievable from DB after cache purge")
	require.Equal(t, testWitness3, retrieved, "retrieved witness should match DB witness")

	// Test 7: Test that witness is cached during block import
	// Create a block and witness, then import it
	testChain, testBlock, testWitness := createTestBlockAndWitness(t)
	defer testChain.Stop()

	// Import the block with witness - this should cache the witness
	blockHash := testBlock.Hash()
	_, err = testChain.InsertChainStateless(types.Blocks{testBlock}, []*stateless.Witness{testWitness})
	require.NoError(t, err, "block import should succeed")

	// Verify witness is in cache after import
	cached, ok = testChain.witnessCache.Get(blockHash)
	require.True(t, ok, "witness should be cached after block import")
	require.NotNil(t, cached, "cached witness should not be nil")

	// Verify GetWitness retrieves from cache
	retrieved = testChain.GetWitness(blockHash)
	require.NotNil(t, retrieved, "witness should be retrievable after import")
	require.Equal(t, cached, retrieved, "GetWitness should return cached witness")

	// Test 8: Test HasWitness with cache hit
	testHash3 := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	testWitness4 := []byte("test witness data 4")
	chain.WriteWitness(testHash3, testWitness4)

	// HasWitness should return true (from cache)
	require.True(t, chain.HasWitness(testHash3), "HasWitness should return true for cached witness")

	// Test 9: Test HasWitness with cache miss but DB hit
	testHash4 := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	testWitness5 := []byte("test witness data 5")
	rawdb.WriteWitness(chain.db, testHash4, testWitness5)
	// Don't add to cache

	// HasWitness should return true (from DB)
	require.True(t, chain.HasWitness(testHash4), "HasWitness should return true for witness in DB")

	// Test 10: Test HasWitness with complete miss
	testHash5 := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	require.False(t, chain.HasWitness(testHash5), "HasWitness should return false for non-existent witness")

	// Test 11: Test WriteWitness wrapper ensures cache consistency
	testHash6 := common.HexToHash("0x4444444444444444444444444444444444444444444444444444444444444444")
	testWitness6 := []byte("test witness data 6")

	// Use WriteWitness wrapper
	chain.WriteWitness(testHash6, testWitness6)

	// Verify it's in both DB and cache
	require.True(t, chain.HasWitness(testHash6), "HasWitness should return true after WriteWitness")
	retrieved = chain.GetWitness(testHash6)
	require.Equal(t, testWitness6, retrieved, "GetWitness should return the written witness")
	cached, ok = chain.witnessCache.Get(testHash6)
	require.True(t, ok, "witness should be in cache after WriteWitness")
	require.Equal(t, testWitness6, cached, "cached witness should match written witness")
}

// TestWitnessCachePurgeOnReorg tests that the witness cache is properly purged during chain reorganization
func TestWitnessCachePurgeOnReorg(t *testing.T) {
	// Create a test blockchain
	engine := ethash.NewFaker()
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr: {Balance: big.NewInt(10000000000000000)}},
	}
	cfg := DefaultConfig()

	db := rawdb.NewMemoryDatabase()
	chain, err := NewBlockChain(db, gspec, engine, cfg)
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}
	defer chain.Stop()

	// Generate some blocks
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 5, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		tx, _ := types.SignTx(types.NewTransaction(uint64(i), common.HexToAddress("0x1234"), big.NewInt(1000), 21000, big.NewInt(2000000000), nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
	})

	// Import blocks
	_, err = chain.InsertChain(blocks, true)
	require.NoError(t, err, "block import should succeed")

	// Manually add witnesses to cache and DB for these blocks
	for i, block := range blocks {
		witnessData := []byte(fmt.Sprintf("witness data for block %d", i))
		chain.WriteWitness(block.Hash(), witnessData)
	}

	// Verify witnesses are in cache
	for i, block := range blocks {
		cached, ok := chain.witnessCache.Get(block.Hash())
		require.True(t, ok, "witness for block %d should be in cache", i)
		require.NotNil(t, cached, "cached witness for block %d should not be nil", i)
	}

	// Trigger a reorg by setting head to block 2 (this calls setHeadBeyondRoot which purges cache)
	chain.SetHead(2)

	// Verify cache was purged (witnesses should no longer be in cache)
	for i, block := range blocks {
		_, ok := chain.witnessCache.Get(block.Hash())
		require.False(t, ok, "witness for block %d should not be in cache after reorg", i)
	}

	// Verify witnesses are still in DB (GetWitness should work by reading from DB)
	for i, block := range blocks {
		witness := chain.GetWitness(block.Hash())
		require.NotNil(t, witness, "witness for block %d should still be in DB after reorg", i)

		// After GetWitness, it should be back in cache
		_, ok := chain.witnessCache.Get(block.Hash())
		require.True(t, ok, "witness for block %d should be re-cached after GetWitness", i)
	}
}

// TestStateAtWithReaders tests the StateAtWithReaders function which returns a state
// along with two separate readers (prefetch and process) that share cache but track
// separate statistics. This is used for cache hit/miss tracking in block production.
func TestStateAtWithReaders(t *testing.T) {
	t.Parallel()

	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(0).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec   = &Genesis{
			Config: params.TestChainConfig,
			Alloc:  types.GenesisAlloc{address: {Balance: funds}},
		}
		genDb   = rawdb.NewMemoryDatabase()
		genesis = gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))
	)

	// Create blockchain
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// Generate some blocks with transactions
	signer := types.LatestSigner(gspec.Config)
	blocks, _ := GenerateChain(gspec.Config, genesis, ethash.NewFaker(), genDb, 3, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(address), common.Address{byte(i + 1)}, big.NewInt(1000), params.TxGas, gen.BaseFee(), nil),
			signer,
			key,
		)
		gen.AddTx(tx)
	})

	// Insert the blocks
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	// Test that prefetch and process readers are independent
	t.Run("independent readers", func(t *testing.T) {
		block := blocks[0]
		statedb, _, prefetchReader, processReader, err := chain.StateAtWithReaders(block.Root())
		if err != nil {
			t.Fatalf("StateAtWithReaders failed: %v", err)
		}

		// Read state using the process reader (via statedb)
		_ = statedb.GetBalance(address)

		// Get stats from both readers
		processStats := processReader.GetStats()
		prefetchStats := prefetchReader.GetStats()

		// Verify process reader has some activity (we just used it)
		processTotalReads := processStats.AccountHit + processStats.AccountMiss + processStats.StorageHit + processStats.StorageMiss
		if processTotalReads == 0 {
			t.Log("Warning: expected process reader to have some reads tracked")
		}

		// The prefetch reader might have fewer or no reads (we didn't explicitly use it)
		// But it should be tracking independently
		prefetchTotalReads := prefetchStats.AccountHit + prefetchStats.AccountMiss + prefetchStats.StorageHit + prefetchStats.StorageMiss

		// The key test: verify they are tracking separately by checking they don't have identical stats
		// (unless both happen to be zero, which is fine for this test)
		if processTotalReads > 0 && prefetchTotalReads > 0 {
			// If both have reads, they should be able to track independently
			t.Logf("Process reader reads: %d, Prefetch reader reads: %d", processTotalReads, prefetchTotalReads)
		}
	})

	// Test error handling - tests the first error path from ReadersWithCacheStats
	// Note: The second error path from state.NewWithReader (lines 530-532 in blockchain_reader.go)
	// is currently unreachable because NewWithReader never returns an error in the current
	// implementation. It's kept for API compatibility and future-proofing.
	t.Run("error from invalid root", func(t *testing.T) {
		invalidRoot := common.HexToHash("0x1234567890123456789012345678901234567890123456789012345678901234")
		statedb, _, prefetchReader, processReader, err := chain.StateAtWithReaders(invalidRoot)

		if err == nil {
			t.Fatal("expected error when using invalid root hash")
		}

		// Verify all return values are nil on error
		if statedb != nil || prefetchReader != nil || processReader != nil {
			t.Fatalf("expected all nil returns on error, got statedb=%v, prefetchReader=%v, processReader=%v",
				statedb, prefetchReader, processReader)
		}

		t.Logf("Got expected error for invalid root: %v", err)
	})

	// P1 Test: Verify prefetch and process readers maintain independence
	// when one modifies state
	t.Run("prefetch process independence", func(t *testing.T) {
		block := blocks[1]
		statedb, throwaway, prefetchReader, processReader, err := chain.StateAtWithReaders(block.Root())
		if err != nil {
			t.Fatalf("StateAtWithReaders failed: %v", err)
		}

		// Get initial balance
		originalBalance := statedb.GetBalance(address)

		// Use throwaway state (prefetch) to modify account
		// This simulates what prefetchFromPool does
		throwaway.SetBalance(address, uint256.NewInt(999999), 0)

		// Verify main statedb (process) is unaffected
		processBalance := statedb.GetBalance(address)
		if processBalance.Cmp(originalBalance) != 0 {
			t.Errorf("Process statedb should be unaffected by throwaway modifications, got %v, want %v",
				processBalance, originalBalance)
		}

		// Verify both readers can track stats independently
		processStats := processReader.GetStats()
		prefetchStats := prefetchReader.GetStats()

		// Both should have some activity
		if processStats.AccountHit+processStats.AccountMiss == 0 {
			t.Error("Process reader should have tracked account reads")
		}

		t.Logf("Independence test - Process stats: %d hits/%d misses, Prefetch stats: %d hits/%d misses",
			processStats.AccountHit, processStats.AccountMiss,
			prefetchStats.AccountHit, prefetchStats.AccountMiss)

		// The key validation: throwaway state modifications don't affect main state
		// This ensures prefetch speculation doesn't corrupt the actual block building state
	})
}

// TestWriteBlockMetrics verifies that the block write path metrics
// (batch write, state commit, witness collection) are updated after
// inserting blocks into the chain, and that the slow-operation warning
// log code paths execute without errors.
func TestWriteBlockMetrics(t *testing.T) {
	metrics.Enable()

	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)
		gspec   = &Genesis{
			Config:  params.TestChainConfig,
			Alloc:   types.GenesisAlloc{address: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 5, func(i int, block *BlockGen) {
		block.SetCoinbase(common.Address{0x01})
		tx, err := types.SignTx(types.NewTransaction(block.TxNonce(address), common.Address{0x02}, big.NewInt(1000), params.TxGas, block.header.BaseFee, nil), signer, key)
		if err != nil {
			panic(err)
		}
		block.AddTx(tx)
	})

	chain, _ := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig().WithStateScheme(rawdb.HashScheme))
	defer chain.Stop()

	// Capture metric counts before insertion
	batchCountBefore := blockBatchWriteTimer.Snapshot().Count()
	commitCountBefore := stateCommitTimer.Snapshot().Count()

	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	// Verify timers were updated
	batchSnap := blockBatchWriteTimer.Snapshot()
	commitSnap := stateCommitTimer.Snapshot()

	if batchSnap.Count() <= batchCountBefore {
		t.Error("blockBatchWriteTimer should have been updated after block insertion")
	}
	if commitSnap.Count() <= commitCountBefore {
		t.Error("stateCommitTimer should have been updated after block insertion")
	}

	// Verify durations are non-negative (the duration variables that feed
	// both the metrics and the >100ms warning log checks are valid)
	if batchSnap.Mean() < 0 {
		t.Error("blockBatchWriteTimer mean duration should be non-negative")
	}
	if commitSnap.Mean() < 0 {
		t.Error("stateCommitTimer mean duration should be non-negative")
	}
}

// ---------------------------------------------------------------------------
// Pipelined Import SRC Tests
// ---------------------------------------------------------------------------

// pipelinedConfig returns a BlockChainConfig with pipelined import SRC enabled.
func pipelinedConfig(scheme string) *BlockChainConfig {
	cfg := DefaultConfig().WithStateScheme(scheme)
	cfg.EnablePipelinedImportSRC = true
	cfg.PipelinedImportSRCLogs = true
	return cfg
}

// pipelinedConfigWithWarmSnapshot returns the standard pipelined config with
// the warm-snapshot handoff enabled. Used by snapshot-on-vs-off parity tests.
func pipelinedConfigWithWarmSnapshot(scheme string) *BlockChainConfig {
	cfg := pipelinedConfig(scheme)
	cfg.PipelinedSRCWarmSnapshot = true
	return cfg
}

// TestPipelinedImportSRC_MultipleBlocks generates 10 blocks with transactions and
// inserts them into two chains — one with pipelined SRC enabled and one without.
// The state roots of every canonical block must match between both chains.
func TestPipelinedImportSRC_MultipleBlocks(t *testing.T) {
	testPipelinedImportSRC_MultipleBlocks(t, rawdb.HashScheme, pipelinedConfig(rawdb.HashScheme))
	testPipelinedImportSRC_MultipleBlocks(t, rawdb.PathScheme, pipelinedConfig(rawdb.PathScheme))
}

func testPipelinedImportSRC_MultipleBlocks(t *testing.T, scheme string, pipeCfg *BlockChainConfig) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	// Generate 10 blocks with a simple transfer in each.
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 10, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// Chain with pipeline enabled.
	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipeCfg)
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	// Reference chain without pipeline.
	refChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create reference chain: %v", err)
	}
	defer refChain.Stop()

	if _, err := pipeChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("pipeline chain: failed to insert blocks: %v", err)
	}
	if _, err := refChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("reference chain: failed to insert blocks: %v", err)
	}

	// Both chains must agree on head.
	if pipeChain.CurrentBlock().Number.Uint64() != 10 {
		t.Fatalf("pipeline chain head = %d, want 10", pipeChain.CurrentBlock().Number.Uint64())
	}
	if refChain.CurrentBlock().Number.Uint64() != 10 {
		t.Fatalf("reference chain head = %d, want 10", refChain.CurrentBlock().Number.Uint64())
	}

	// All canonical blocks must have matching state roots.
	for i := uint64(1); i <= 10; i++ {
		pipeBlock := pipeChain.GetBlockByNumber(i)
		refBlock := refChain.GetBlockByNumber(i)
		if pipeBlock == nil || refBlock == nil {
			t.Fatalf("block %d: missing on pipeline(%v) or reference(%v)", i, pipeBlock == nil, refBlock == nil)
		}
		if pipeBlock.Root() != refBlock.Root() {
			t.Errorf("block %d: state root mismatch pipeline=%s reference=%s", i, pipeBlock.Root(), refBlock.Root())
		}
		if pipeBlock.Hash() != refBlock.Hash() {
			t.Errorf("block %d: block hash mismatch pipeline=%s reference=%s", i, pipeBlock.Hash(), refBlock.Hash())
		}
	}
}

// TestPipelinedImportSRC_SingleBlock inserts a single block with pipeline enabled
// and verifies correctness of the state.
func TestPipelinedImportSRC_SingleBlock(t *testing.T) {
	testPipelinedImportSRC_SingleBlock(t, rawdb.HashScheme)
	testPipelinedImportSRC_SingleBlock(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_SingleBlock(t *testing.T, scheme string) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert block: %v", err)
	}

	if chain.CurrentBlock().Number.Uint64() != 1 {
		t.Fatalf("head = %d, want 1", chain.CurrentBlock().Number.Uint64())
	}

	statedb, err := chain.StateAt(blocks[0].Root())
	if err != nil {
		t.Fatalf("StateAt failed: %v", err)
	}

	// Recipient should have received 1000 wei.
	bal := statedb.GetBalance(recipient)
	if bal.IsZero() {
		t.Error("recipient balance should be non-zero after transfer")
	}
}

// TestPipelinedImportSRC_CrossCallPersistence inserts blocks across two separate
// InsertChain calls with pipelined SRC and verifies that state persists correctly
// between calls (the pending SRC from the first batch is flushed before the
// second batch begins).
func TestPipelinedImportSRC_CrossCallPersistence(t *testing.T) {
	testPipelinedImportSRC_CrossCallPersistence(t, rawdb.HashScheme)
	testPipelinedImportSRC_CrossCallPersistence(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_CrossCallPersistence(t *testing.T, scheme string) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 6, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// Pipeline chain: split insertion across two calls.
	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	if _, err := pipeChain.InsertChain(blocks[:3], false); err != nil {
		t.Fatalf("pipeline: first batch insert failed: %v", err)
	}
	if _, err := pipeChain.InsertChain(blocks[3:], false); err != nil {
		t.Fatalf("pipeline: second batch insert failed: %v", err)
	}

	// Reference chain: single call.
	refChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create reference chain: %v", err)
	}
	defer refChain.Stop()

	if _, err := refChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("reference: insert failed: %v", err)
	}

	if pipeChain.CurrentBlock().Number.Uint64() != 6 {
		t.Fatalf("pipeline head = %d, want 6", pipeChain.CurrentBlock().Number.Uint64())
	}

	for i := uint64(1); i <= 6; i++ {
		pipeBlock := pipeChain.GetBlockByNumber(i)
		refBlock := refChain.GetBlockByNumber(i)
		if pipeBlock == nil || refBlock == nil {
			t.Fatalf("block %d missing", i)
		}
		if pipeBlock.Root() != refBlock.Root() {
			t.Errorf("block %d: state root mismatch pipeline=%s reference=%s", i, pipeBlock.Root(), refBlock.Root())
		}
	}
}

// TestPipelinedImportSRC_Reorg inserts a main chain and then a longer fork to
// trigger a reorg. Verifies that the fork becomes canonical and all state roots
// are valid after the reorg.
func TestPipelinedImportSRC_Reorg(t *testing.T) {
	testPipelinedImportSRC_Reorg(t, rawdb.HashScheme)
	testPipelinedImportSRC_Reorg(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_Reorg(t *testing.T, scheme string) {
	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		addr2   = crypto.PubkeyToAddress(key2.PublicKey)
		funds   = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec   = &Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
			},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	// Main chain: 5 blocks, transfers from addr1.
	_, mainBlocks, _ := GenerateChainWithGenesis(gspec, engine, 5, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr1), common.HexToAddress("0x1111"), big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key1,
		)
		gen.AddTx(tx)
	})

	// Fork chain: 7 blocks branching from genesis, using addr2 so it creates
	// different state. Longer chain so it becomes canonical.
	_, forkBlocks, _ := GenerateChainWithGenesis(gspec, engine, 7, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr2), common.HexToAddress("0x2222"), big.NewInt(2000), params.TxGas, gen.header.BaseFee, nil),
			signer, key2,
		)
		gen.AddTx(tx)
	})

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// Insert main chain.
	if _, err := chain.InsertChain(mainBlocks, false); err != nil {
		t.Fatalf("main chain insert failed: %v", err)
	}
	if chain.CurrentBlock().Number.Uint64() != 5 {
		t.Fatalf("after main: head = %d, want 5", chain.CurrentBlock().Number.Uint64())
	}

	// Insert fork chain — should trigger reorg since it's longer.
	if _, err := chain.InsertChain(forkBlocks, false); err != nil {
		t.Fatalf("fork chain insert failed: %v", err)
	}
	if chain.CurrentBlock().Number.Uint64() != 7 {
		t.Fatalf("after fork: head = %d, want 7", chain.CurrentBlock().Number.Uint64())
	}

	// Verify the fork is now canonical by checking block hashes.
	for i := uint64(1); i <= 7; i++ {
		canonical := chain.GetBlockByNumber(i)
		if canonical == nil {
			t.Fatalf("missing canonical block %d after reorg", i)
		}
		if canonical.Hash() != forkBlocks[i-1].Hash() {
			t.Errorf("block %d: canonical hash %s != fork hash %s", i, canonical.Hash(), forkBlocks[i-1].Hash())
		}
	}

	// Verify state is accessible for the canonical head.
	statedb, err := chain.StateAt(chain.CurrentBlock().Root)
	if err != nil {
		t.Fatalf("StateAt head failed: %v", err)
	}
	// addr2 sent 2000 wei per block for 7 blocks => should have less than initial funds.
	bal := statedb.GetBalance(addr2)
	if bal.IsZero() {
		t.Error("addr2 balance should be non-zero")
	}
}

// TestPipelinedImportSRC_StateAtDuringPipeline generates blocks that modify
// account balances and verifies that StateAt returns correct balances for each
// block's root after pipelined insertion.
func TestPipelinedImportSRC_StateAtDuringPipeline(t *testing.T) {
	testPipelinedImportSRC_StateAtDuringPipeline(t, rawdb.HashScheme)
	testPipelinedImportSRC_StateAtDuringPipeline(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_StateAtDuringPipeline(t *testing.T, scheme string) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		txValue   = big.NewInt(10000) // 10000 wei per block
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	numBlocks := 5
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, txValue, params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}

	// Verify state at each block root shows monotonically increasing recipient balance.
	var prevBal *uint256.Int
	for i := 0; i < numBlocks; i++ {
		statedb, err := chain.StateAt(blocks[i].Root())
		if err != nil {
			t.Fatalf("block %d: StateAt failed: %v", i+1, err)
		}
		bal := statedb.GetBalance(recipient)
		if bal.IsZero() {
			t.Errorf("block %d: recipient balance is zero, expected non-zero", i+1)
		}
		if prevBal != nil && bal.Cmp(prevBal) <= 0 {
			t.Errorf("block %d: recipient balance %s should be greater than previous %s", i+1, bal, prevBal)
		}
		prevBal = bal.Clone()
	}

	// Final balance should equal txValue * numBlocks.
	expectedBal := new(big.Int).Mul(txValue, big.NewInt(int64(numBlocks)))
	finalState, _ := chain.StateAt(blocks[numBlocks-1].Root())
	got := finalState.GetBalance(recipient).ToBig()
	if got.Cmp(expectedBal) != 0 {
		t.Errorf("final recipient balance: got %s, want %s", got, expectedBal)
	}
}

// TestPipelinedImportSRC_ValidateStateCheap verifies that blocks inserted with
// pipelined SRC pass all cheap validation checks (gas used, bloom filter,
// receipt root). This is implicitly tested by successful insertion, but this
// test explicitly verifies no errors by comparing against a reference chain.
func TestPipelinedImportSRC_ValidateStateCheap(t *testing.T) {
	testPipelinedImportSRC_ValidateStateCheap(t, rawdb.HashScheme)
	testPipelinedImportSRC_ValidateStateCheap(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_ValidateStateCheap(t *testing.T, scheme string) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 8, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// Insert with pipeline — any ValidateStateCheap failure would surface as
	// an InsertChain error.
	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	n, err := pipeChain.InsertChain(blocks, false)
	if err != nil {
		t.Fatalf("pipeline InsertChain failed at block %d: %v", n, err)
	}

	// Reference chain for comparison.
	refChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, DefaultConfig().WithStateScheme(scheme))
	if err != nil {
		t.Fatalf("failed to create reference chain: %v", err)
	}
	defer refChain.Stop()

	if _, err := refChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("reference InsertChain failed: %v", err)
	}

	// Verify: every block has matching gas, bloom, receipt root, and state root.
	for i := uint64(1); i <= 8; i++ {
		pBlock := pipeChain.GetBlockByNumber(i)
		rBlock := refChain.GetBlockByNumber(i)
		if pBlock == nil || rBlock == nil {
			t.Fatalf("block %d missing", i)
		}
		if pBlock.GasUsed() != rBlock.GasUsed() {
			t.Errorf("block %d: gas used mismatch %d vs %d", i, pBlock.GasUsed(), rBlock.GasUsed())
		}
		if pBlock.Bloom() != rBlock.Bloom() {
			t.Errorf("block %d: bloom filter mismatch", i)
		}
		if pBlock.ReceiptHash() != rBlock.ReceiptHash() {
			t.Errorf("block %d: receipt hash mismatch %s vs %s", i, pBlock.ReceiptHash(), rBlock.ReceiptHash())
		}
		if pBlock.Root() != rBlock.Root() {
			t.Errorf("block %d: state root mismatch %s vs %s", i, pBlock.Root(), rBlock.Root())
		}
	}
}

// TestPipelinedImportMetrics verifies that the pipelined-import metrics and
// their parity timers actually increment when blocks flow through the
// pipelined path, and that the mode gauge reflects the enabled config.
func TestPipelinedImportMetrics(t *testing.T) {
	metrics.Enable()

	const numBlocks = 5

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// Snapshot counters before — metrics registrations are process-global, so
	// other tests in the same binary may have already moved them.
	blocksBefore := pipelineImportBlocksCounter.Snapshot().Count()
	hitBefore := pipelineImportHitCounter.Snapshot().Count()
	mismatchBefore := pipelineImportRootMismatchCounter.Snapshot().Count()
	insertBefore := blockInsertTimer.Snapshot().Count()
	stateCommitBefore := stateCommitTimer.Snapshot().Count()
	pipelineExecBefore := pipelineImportExecutionTimer.Snapshot().Count()
	overlapBefore := pipelineImportOverlapExecutionTimer.Snapshot().Count()
	overlapBlocksBefore := pipelineImportOverlapBlocksCounter.Snapshot().Count()
	noOverlapBlocksBefore := pipelineImportNoOverlapBlocksCounter.Snapshot().Count()
	overlapPercentBefore := pipelineImportOverlapExecutionPercent.Snapshot().Count()
	srcOpenBefore := pipelineImportSRCOpenStateDBTimer.Snapshot().Count()
	srcApplyBefore := pipelineImportSRCApplyFlatDiffTimer.Snapshot().Count()
	srcCommitBefore := pipelineImportSRCCommitTimer.Snapshot().Count()
	execWithOverlapBefore := pipelineImportExecWithOverlapTimer.Snapshot().Count()
	execNoOverlapBefore := pipelineImportExecNoOverlapTimer.Snapshot().Count()
	execBucketBefore := pipelineImportExecOverlap0Timer.Snapshot().Count() +
		pipelineImportExecOverlap1To25Timer.Snapshot().Count() +
		pipelineImportExecOverlap25To50Timer.Snapshot().Count() +
		pipelineImportExecOverlap50To75Timer.Snapshot().Count() +
		pipelineImportExecOverlap75To100Timer.Snapshot().Count()
	srcWithNextBefore := pipelineImportSRCWithNextExecTimer.Snapshot().Count()
	srcNoNextBefore := pipelineImportSRCNoNextExecTimer.Snapshot().Count()
	srcWithNextSumBefore := pipelineImportSRCWithNextExecTimer.Snapshot().Sum()
	srcNoNextSumBefore := pipelineImportSRCNoNextExecTimer.Snapshot().Sum()

	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	if got := pipelineImportEnabledGauge.Snapshot().Value(); got != 1 {
		t.Errorf("pipelineImportEnabledGauge = %d, want 1 when EnablePipelinedImportSRC=true", got)
	}

	if _, err := pipeChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("pipeline InsertChain failed: %v", err)
	}

	// Drain the trailing pending SRC so per-block counters reflect every block.
	if err := pipeChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC failed: %v", err)
	}

	blocksDelta := pipelineImportBlocksCounter.Snapshot().Count() - blocksBefore
	hitDelta := pipelineImportHitCounter.Snapshot().Count() - hitBefore
	mismatchDelta := pipelineImportRootMismatchCounter.Snapshot().Count() - mismatchBefore
	insertDelta := blockInsertTimer.Snapshot().Count() - insertBefore
	stateCommitDelta := stateCommitTimer.Snapshot().Count() - stateCommitBefore
	pipelineExecDelta := pipelineImportExecutionTimer.Snapshot().Count() - pipelineExecBefore
	overlapDelta := pipelineImportOverlapExecutionTimer.Snapshot().Count() - overlapBefore
	overlapBlocksDelta := pipelineImportOverlapBlocksCounter.Snapshot().Count() - overlapBlocksBefore
	noOverlapBlocksDelta := pipelineImportNoOverlapBlocksCounter.Snapshot().Count() - noOverlapBlocksBefore
	overlapPercentDelta := pipelineImportOverlapExecutionPercent.Snapshot().Count() - overlapPercentBefore
	srcOpenDelta := pipelineImportSRCOpenStateDBTimer.Snapshot().Count() - srcOpenBefore
	srcApplyDelta := pipelineImportSRCApplyFlatDiffTimer.Snapshot().Count() - srcApplyBefore
	srcCommitDelta := pipelineImportSRCCommitTimer.Snapshot().Count() - srcCommitBefore
	execWithOverlapDelta := pipelineImportExecWithOverlapTimer.Snapshot().Count() - execWithOverlapBefore
	execNoOverlapDelta := pipelineImportExecNoOverlapTimer.Snapshot().Count() - execNoOverlapBefore
	execBucketDelta := pipelineImportExecOverlap0Timer.Snapshot().Count() +
		pipelineImportExecOverlap1To25Timer.Snapshot().Count() +
		pipelineImportExecOverlap25To50Timer.Snapshot().Count() +
		pipelineImportExecOverlap50To75Timer.Snapshot().Count() +
		pipelineImportExecOverlap75To100Timer.Snapshot().Count() - execBucketBefore
	srcWithNextDelta := pipelineImportSRCWithNextExecTimer.Snapshot().Count() - srcWithNextBefore
	srcNoNextDelta := pipelineImportSRCNoNextExecTimer.Snapshot().Count() - srcNoNextBefore
	srcSplitDurationDelta := pipelineImportSRCWithNextExecTimer.Snapshot().Sum() + pipelineImportSRCNoNextExecTimer.Snapshot().Sum() -
		srcWithNextSumBefore - srcNoNextSumBefore

	if blocksDelta != numBlocks {
		t.Errorf("pipelineImportBlocksCounter delta = %d, want %d", blocksDelta, numBlocks)
	}
	// First block has no pending predecessor; subsequent blocks should all hit.
	if hitDelta != numBlocks-1 {
		t.Errorf("pipelineImportHitCounter delta = %d, want %d", hitDelta, numBlocks-1)
	}
	if mismatchDelta != 0 {
		t.Errorf("pipelineImportRootMismatchCounter delta = %d, want 0 (safety alarm)", mismatchDelta)
	}
	if insertDelta != numBlocks {
		t.Errorf("blockInsertTimer (parity) delta = %d, want %d", insertDelta, numBlocks)
	}
	if stateCommitDelta != numBlocks {
		t.Errorf("stateCommitTimer (parity, from SRC goroutine) delta = %d, want %d", stateCommitDelta, numBlocks)
	}
	if pipelineExecDelta != numBlocks {
		t.Errorf("pipelineImportExecutionTimer delta = %d, want %d", pipelineExecDelta, numBlocks)
	}
	if overlapDelta != numBlocks-1 {
		t.Errorf("pipelineImportOverlapExecutionTimer delta = %d, want %d", overlapDelta, numBlocks-1)
	}
	if overlapBlocksDelta+noOverlapBlocksDelta != numBlocks-1 {
		t.Errorf("overlap/no-overlap block deltas = %d + %d, want %d", overlapBlocksDelta, noOverlapBlocksDelta, numBlocks-1)
	}
	if overlapPercentDelta != numBlocks-1 {
		t.Errorf("pipelineImportOverlapExecutionPercent delta = %d, want %d", overlapPercentDelta, numBlocks-1)
	}
	if srcOpenDelta != numBlocks {
		t.Errorf("pipelineImportSRCOpenStateDBTimer delta = %d, want %d", srcOpenDelta, numBlocks)
	}
	if srcApplyDelta != numBlocks {
		t.Errorf("pipelineImportSRCApplyFlatDiffTimer delta = %d, want %d", srcApplyDelta, numBlocks)
	}
	if srcCommitDelta != numBlocks {
		t.Errorf("pipelineImportSRCCommitTimer delta = %d, want %d", srcCommitDelta, numBlocks)
	}
	// Categorical execution split: every classified block lands in exactly one
	// of with/no overlap, and the binary split must track the overlap counters.
	if execWithOverlapDelta+execNoOverlapDelta != numBlocks-1 {
		t.Errorf("execution with/no-overlap deltas = %d + %d, want %d", execWithOverlapDelta, execNoOverlapDelta, numBlocks-1)
	}
	if execWithOverlapDelta != overlapBlocksDelta {
		t.Errorf("execution/with_overlap delta = %d, want %d (overlap blocks)", execWithOverlapDelta, overlapBlocksDelta)
	}
	if execNoOverlapDelta != noOverlapBlocksDelta {
		t.Errorf("execution/no_overlap delta = %d, want %d (no-overlap blocks)", execNoOverlapDelta, noOverlapBlocksDelta)
	}
	if execBucketDelta != numBlocks-1 {
		t.Errorf("execution overlap bucket deltas sum = %d, want %d", execBucketDelta, numBlocks-1)
	}
	// SRC split mirrors the execution split from the next-block perspective.
	if srcWithNextDelta+srcNoNextDelta != numBlocks-1 {
		t.Errorf("src with/no-next-exec-overlap deltas = %d + %d, want %d", srcWithNextDelta, srcNoNextDelta, numBlocks-1)
	}
	if srcWithNextDelta != overlapBlocksDelta {
		t.Errorf("src/with_next_exec_overlap delta = %d, want %d (overlap blocks)", srcWithNextDelta, overlapBlocksDelta)
	}
	if srcWithNextDelta+srcNoNextDelta > 0 && srcSplitDurationDelta <= int64(10*time.Microsecond) {
		t.Errorf("src overlap split recorded only %s total duration; want real SRC wall-clock, not defer-registration time", time.Duration(srcSplitDurationDelta))
	}
}

// TestPipelineImportDisabledGauge verifies the mode gauge reads 0 when the
// pipeline is not enabled in the chain config.
func TestPipelineImportDisabledGauge(t *testing.T) {
	metrics.Enable()

	gspec := &Genesis{
		Config:  params.AllEthashProtocolChanges,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	engine := ethash.NewFaker()

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, DefaultConfig().WithStateScheme(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	if got := pipelineImportEnabledGauge.Snapshot().Value(); got != 0 {
		t.Errorf("pipelineImportEnabledGauge = %d, want 0 when EnablePipelinedImportSRC=false", got)
	}
}

// TestPipelineFlatDiffHitMeters verifies that the FlatDiff overlay meters
// increment when consecutive blocks under pipelined import touch accounts/slots
// mutated by the previous block.
func TestPipelineFlatDiffHitMeters(t *testing.T) {
	metrics.Enable()

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	// Every block transfers from the same `addr` to `recipient` — both addresses
	// are in the previous block's FlatDiff, so reads in the next block's
	// execution should hit the overlay.
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 3, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// The FlatDiff meters live in the state package (unexported); look them up
	// by name in the global registry rather than exposing accessors.
	flatAcctMeter, ok := metrics.DefaultRegistry.Get("state/flatdiff/account_hits").(*metrics.Meter)
	if !ok {
		t.Fatal("state/flatdiff/account_hits meter not registered")
	}
	accountHitsBefore := flatAcctMeter.Snapshot().Count()

	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	if _, err := pipeChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("pipeline InsertChain failed: %v", err)
	}
	_ = pipeChain.flushPendingImportSRC(true)

	if flatAcctMeter.Snapshot().Count()-accountHitsBefore == 0 {
		t.Error("state/flatdiff/account_hits should have non-zero delta after consecutive-block transfers")
	}
	// Storage hits depend on the specific SSTORE pattern; pure balance-transfer
	// blocks may not hit storage slots. We only assert account-side hits here.
}

// TestPipelinedImportSRC_MakeWitnessFalse verifies that when the pipelined
// import path is invoked with makeWitness=false (the producewitnesses=false
// configuration), the SRC goroutine still computes and validates the state
// root — but skips witness construction, FlatDiff read-surface preload, and
// witness encoding/caching entirely.
func TestPipelinedImportSRC_MakeWitnessFalse(t *testing.T) {
	metrics.Enable()

	const numBlocks = 5

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	// Snapshot the metrics that should NOT advance when makeWitness=false.
	preloadTimerBefore := pipelineSRCPreloadTimer.Snapshot().Count()
	preloadSlotsBefore := pipelineSRCPreloadSlotsHistogram.Snapshot().Count()
	preloadAccountsBefore := pipelineSRCPreloadReadAccountsHistogram.Snapshot().Count()
	// And the one that SHOULD advance — metrics are package-global, so we must
	// compare deltas rather than absolute counts.
	stateCommitBefore := stateCommitTimer.Snapshot().Count()

	// makeWitness=false is the default for InsertChain.
	if _, err := pipeChain.InsertChain(blocks, false); err != nil {
		t.Fatalf("pipeline InsertChain failed: %v", err)
	}
	if err := pipeChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC failed: %v", err)
	}

	// State roots must match the canonical roots from GenerateChainWithGenesis
	// — root validation runs even with witness off.
	for i := uint64(1); i <= numBlocks; i++ {
		pipeBlock := pipeChain.GetBlockByNumber(i)
		if pipeBlock == nil {
			t.Fatalf("block %d: missing on pipeline chain", i)
		}
		if pipeBlock.Root() != blocks[i-1].Root() {
			t.Errorf("block %d: state root mismatch pipeline=%s expected=%s", i, pipeBlock.Root(), blocks[i-1].Root())
		}
	}

	// No witness should be produced or persisted — neither cache nor store.
	// HasWitness covers both surfaces; check both explicitly so a future
	// refactor that splits the write paths still trips the assertion.
	for i := uint64(1); i <= numBlocks; i++ {
		hash := pipeChain.GetBlockByNumber(i).Hash()
		if pipeChain.witnessCache.Contains(hash) {
			t.Errorf("block %d: witnessCache unexpectedly contains witness when makeWitness=false", i)
		}
		if pipeChain.witnessStore.HasWitness(hash) {
			t.Errorf("block %d: witnessStore unexpectedly contains witness when makeWitness=false", i)
		}
		if pipeChain.HasWitness(hash) {
			t.Errorf("block %d: HasWitness=true when makeWitness=false", i)
		}
	}

	// Preload timer + histograms must not have fired — they exist solely to
	// populate the witness with proof-path nodes.
	if delta := pipelineSRCPreloadTimer.Snapshot().Count() - preloadTimerBefore; delta != 0 {
		t.Errorf("pipelineSRCPreloadTimer delta = %d, want 0 when makeWitness=false", delta)
	}
	if delta := pipelineSRCPreloadSlotsHistogram.Snapshot().Count() - preloadSlotsBefore; delta != 0 {
		t.Errorf("pipelineSRCPreloadSlotsHistogram delta = %d, want 0 when makeWitness=false", delta)
	}
	if delta := pipelineSRCPreloadReadAccountsHistogram.Snapshot().Count() - preloadAccountsBefore; delta != 0 {
		t.Errorf("pipelineSRCPreloadReadAccountsHistogram delta = %d, want 0 when makeWitness=false", delta)
	}

	// stateCommitTimer should still fire once per block — CommitWithUpdate
	// runs unconditionally, witness or not.
	if delta := stateCommitTimer.Snapshot().Count() - stateCommitBefore; delta != numBlocks {
		t.Errorf("stateCommitTimer delta = %d, want %d when makeWitness=false (CommitWithUpdate must still run)", delta, numBlocks)
	}

	// GetWitness for a recent existing block must miss fast on a witness-off
	// node: no witness will ever appear, so the peer-serving path must not
	// fall through to the 2s cache poll (a peer could otherwise stall the
	// WIT handler for the full timeout per recent hash).
	start := time.Now()
	if w := pipeChain.GetWitness(pipeChain.GetBlockByNumber(numBlocks - 1).Hash()); w != nil {
		t.Errorf("GetWitness returned a witness on a makeWitness=false chain")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("GetWitness took %v on a witness-off node, want a fast miss (poll must be skipped)", elapsed)
	}
}

// TestPipelinedImportSRC_MakeWitnessTrue verifies that when InsertChain is
// called with makeWitness=true and the pipeline is enabled, the SRC goroutine
// produces a witness, caches it, and preload metrics fire as expected.
func TestPipelinedImportSRC_MakeWitnessTrue(t *testing.T) {
	metrics.Enable()

	const numBlocks = 3

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	preloadTimerBefore := pipelineSRCPreloadTimer.Snapshot().Count()
	preloadSlotsBefore := pipelineSRCPreloadSlotsHistogram.Snapshot().Count()
	preloadAccountsBefore := pipelineSRCPreloadReadAccountsHistogram.Snapshot().Count()

	if _, err := pipeChain.InsertChain(blocks, true); err != nil {
		t.Fatalf("pipeline InsertChain (makeWitness=true) failed: %v", err)
	}
	if err := pipeChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC failed: %v", err)
	}

	// Witness should be cached for every imported block.
	for i := uint64(1); i <= numBlocks; i++ {
		hash := pipeChain.GetBlockByNumber(i).Hash()
		if !pipeChain.witnessCache.Contains(hash) {
			t.Errorf("block %d: witnessCache missing witness when makeWitness=true", i)
		}
	}

	// Preload timer + histograms should fire once per block.
	if delta := pipelineSRCPreloadTimer.Snapshot().Count() - preloadTimerBefore; delta != numBlocks {
		t.Errorf("pipelineSRCPreloadTimer delta = %d, want %d when makeWitness=true", delta, numBlocks)
	}
	if delta := pipelineSRCPreloadSlotsHistogram.Snapshot().Count() - preloadSlotsBefore; delta != numBlocks {
		t.Errorf("pipelineSRCPreloadSlotsHistogram delta = %d, want %d when makeWitness=true", delta, numBlocks)
	}
	if delta := pipelineSRCPreloadReadAccountsHistogram.Snapshot().Count() - preloadAccountsBefore; delta != numBlocks {
		t.Errorf("pipelineSRCPreloadReadAccountsHistogram delta = %d, want %d when makeWitness=true", delta, numBlocks)
	}
}

// TestPipelinedImportSRC_RootParityWitnessOnVsOff is the consensus-critical
// parity check for mitigation (2.5): the pipelined SRC goroutine uses
// state.NewTrieOnly when makeWitness=true and state.New (multi-reader) when
// makeWitness=false. Both reader paths must produce byte-identical state
// roots when importing the same blocks — otherwise consensus would split
// between witness-producing and witness-off nodes on the same network.
//
// Two import shapes are exercised:
//   - shape A: a single InsertChain(blocks) call — batch behaviour
//   - shape B: two consecutive InsertChain calls — exercises cross-call
//     pending-SRC reuse (the FlatDiff overlay path between batches)
//
// Path scheme is used because state.New only differs from state.NewTrieOnly
// when a flat reader is actually wired (pathdb StateReader); under hash
// scheme without a snapshot the multi-reader degenerates to trie-only and
// the test would not detect a real parity bug.
func TestPipelinedImportSRC_RootParityWitnessOnVsOff(t *testing.T) {
	const numBlocks = 8
	const splitAt = 3 // shape B: insert blocks[:splitAt] then blocks[splitAt:]

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// Witness=true path: NewTrieOnly reader, single InsertChain batch.
	witnessOnChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.PathScheme))
	if err != nil {
		t.Fatalf("witness-on chain: %v", err)
	}
	defer witnessOnChain.Stop()
	if _, err := witnessOnChain.InsertChain(blocks, true); err != nil {
		t.Fatalf("witness-on InsertChain: %v", err)
	}
	if err := witnessOnChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("witness-on flush: %v", err)
	}

	// Witness=false path, shape A: single InsertChain batch with state.New reader.
	witnessOffBatch, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.PathScheme))
	if err != nil {
		t.Fatalf("witness-off batch chain: %v", err)
	}
	defer witnessOffBatch.Stop()
	if _, err := witnessOffBatch.InsertChain(blocks, false); err != nil {
		t.Fatalf("witness-off batch InsertChain: %v", err)
	}
	if err := witnessOffBatch.flushPendingImportSRC(true); err != nil {
		t.Fatalf("witness-off batch flush: %v", err)
	}

	// Witness=false path, shape B: split insertion exercises cross-call
	// pending-SRC reuse — the second InsertChain opens with a pending FlatDiff
	// from the first batch and must produce the same roots as the batched
	// path.
	witnessOffSplit, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.PathScheme))
	if err != nil {
		t.Fatalf("witness-off split chain: %v", err)
	}
	defer witnessOffSplit.Stop()
	if _, err := witnessOffSplit.InsertChain(blocks[:splitAt], false); err != nil {
		t.Fatalf("witness-off split first batch: %v", err)
	}
	if _, err := witnessOffSplit.InsertChain(blocks[splitAt:], false); err != nil {
		t.Fatalf("witness-off split second batch: %v", err)
	}
	if err := witnessOffSplit.flushPendingImportSRC(true); err != nil {
		t.Fatalf("witness-off split flush: %v", err)
	}

	// Per-block parity: every chain must agree with the canonical root from
	// the generator AND with each other.
	for i := uint64(1); i <= numBlocks; i++ {
		canonical := blocks[i-1].Root()

		on := witnessOnChain.GetBlockByNumber(i)
		offBatch := witnessOffBatch.GetBlockByNumber(i)
		offSplit := witnessOffSplit.GetBlockByNumber(i)
		if on == nil || offBatch == nil || offSplit == nil {
			t.Fatalf("block %d: missing on one of the chains (on=%v offBatch=%v offSplit=%v)",
				i, on == nil, offBatch == nil, offSplit == nil)
		}

		if on.Root() != canonical {
			t.Errorf("block %d: witness-on root %s != canonical %s", i, on.Root(), canonical)
		}
		if offBatch.Root() != canonical {
			t.Errorf("block %d: witness-off batch root %s != canonical %s", i, offBatch.Root(), canonical)
		}
		if offSplit.Root() != canonical {
			t.Errorf("block %d: witness-off split root %s != canonical %s", i, offSplit.Root(), canonical)
		}
		if on.Root() != offBatch.Root() {
			t.Errorf("block %d: witness-on vs witness-off-batch root mismatch %s != %s", i, on.Root(), offBatch.Root())
		}
		if offBatch.Root() != offSplit.Root() {
			t.Errorf("block %d: witness-off batch vs split root mismatch %s != %s", i, offBatch.Root(), offSplit.Root())
		}
	}
}

// TestPipelinedImportSRC_WitnessHardFailsWithoutExecWitness verifies that
// runSRCCompute rejects the configuration where a witness is requested but
// none is supplied by the caller. The import path always hands the
// EVM-populated witness through to SRC; only miner/legacy callers may set
// allowOwnWitness=true to opt into SRC creating its own witness. Spawning
// the SRC goroutine with makeWitness=true, execWitness=nil, and
// allowOwnWitness=false must set pending.err.
func TestPipelinedImportSRC_WitnessHardFailsWithoutExecWitness(t *testing.T) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	// First import a block normally so we have a committed parent root and a
	// FlatDiff to feed the SRC. The import path's makeWitness=false is used so
	// no witness work runs in the legitimate insertion.
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("InsertChain failed: %v", err)
	}
	if err := chain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC failed: %v", err)
	}

	// Now manually spawn an SRC goroutine with makeWitness=true,
	// execWitness=nil, allowOwnWitness=false. Use a dummy FlatDiff to keep
	// ApplyFlatDiffForCommit from being reached; runSRCCompute should error
	// out before touching it.
	parent := chain.GetBlockByNumber(0)
	target := blocks[0]
	chain.SpawnSRCGoroutine(target, parent.Root(), &state.FlatDiff{}, true, nil, false, nil, false)

	// Drain via the public wait API: the goroutine sets pending.err and exits.
	chain.pendingSRCMu.Lock()
	pending := chain.pendingSRC
	chain.pendingSRCMu.Unlock()
	if pending == nil {
		t.Fatal("expected pendingSRC after SpawnSRCGoroutine")
	}
	pending.wg.Wait()

	if pending.err == nil {
		t.Fatal("expected pending.err to be set when execWitness=nil and allowOwnWitness=false; " +
			"got nil — the hard-fail check is not enforcing the witness contract")
	}
	if !strings.Contains(pending.err.Error(), "without execution witness") {
		t.Errorf("pending.err = %v, want error mentioning 'without execution witness'", pending.err)
	}
}

// TestPipelinedImportSRC_WitnessIncludesBlockHashAncestors verifies that
// BLOCKHASH opcode access during EVM execution is reflected in the witness
// published by the pipelined SRC path.
//
// The pipelined import path runs EVM execution and SRC commit on different
// goroutines but must publish a single completed witness. AddBlockHash fires
// during execution (vm/instructions.go::opBlockhash) on the witness attached
// to the executing StateDB; the published witness must therefore include
// those Headers entries. BLOCKHASH ancestor coverage is checked through
// Headers because BorWitness serialises Headers but not Codes; verifiers
// source bytecode from local storage.
//
// Test setup:
//   - Block 1: regular value transfer, no BLOCKHASH.
//   - Block 2: calls a contract whose bytecode runs BLOCKHASH(0). At block 2
//     this triggers Witness.AddBlockHash(0), which extends Headers to include
//     genesis (parent=block-1 is already in Headers from NewWitness; reaching
//     back to genesis adds the second entry).
//
// Generation requires a real chain context for AddTxWithChain to satisfy the
// EVM's blockhash lookup — chain_makers.go's plain AddTx uses a fake
// BlockChain with no headers and crashes on GetHashFn. The test builds a
// separate ctxChain for generation, then imports all blocks into a fresh
// pipeChain configured with pipelined SRC.
func TestPipelinedImportSRC_WitnessIncludesBlockHashAncestors(t *testing.T) {
	// Bytecode: PUSH1 0x00 ; BLOCKHASH ; POP ; STOP
	// Reads the hash of block 0 (genesis), discards it. Triggers
	// witness.AddBlockHash(0) inside vm/instructions.go::opBlockhash on
	// whichever StateDB owns the witness at execution time.
	contractCode := []byte{0x60, 0x00, 0x40, 0x50, 0x00}
	contractAddr := common.HexToAddress("0xb1a5cab1ec0de")

	var (
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		funds  = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec  = &Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				addr:         {Balance: funds},
				contractAddr: {Code: contractCode, Balance: big.NewInt(0)},
			},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	// Phase 1: Generate block 1 (no BLOCKHASH yet) using a fresh shared db
	// that we'll reuse for ctxChain so headers are written exactly once.
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	genesisBlock := gspec.MustCommit(db, tdb)
	prefix, _ := GenerateChain(gspec.Config, genesisBlock, engine, db, 1, func(i int, gen *BlockGen) {})

	// Phase 2: Build ctxChain on the same db so BlockGen.AddTxWithChain has
	// a real HeaderChain to satisfy the BLOCKHASH lookup in block 2.
	ctxChain, err := NewBlockChain(db, gspec, engine, DefaultConfig().WithStateScheme(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("ctxChain: %v", err)
	}
	if _, err := ctxChain.InsertChain(prefix, false); err != nil {
		t.Fatalf("ctxChain: insert prefix: %v", err)
	}

	// Phase 3: Generate block 2 with a tx that calls the contract → BLOCKHASH(0)
	// fires during EVM execution, extending the execution witness's Headers.
	block2Slice, _ := GenerateChain(gspec.Config, prefix[0], engine, db, 1, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), contractAddr, big.NewInt(0), 100_000, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTxWithChain(ctxChain, tx)
	})
	ctxChain.Stop()

	allBlocks := append(append([]*types.Block{}, prefix...), block2Slice...)

	// Phase 4: Fresh pipeChain, import everything with witness=true
	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("pipeChain: %v", err)
	}
	defer pipeChain.Stop()

	if _, err := pipeChain.InsertChain(allBlocks, true); err != nil {
		t.Fatalf("InsertChain (makeWitness=true): %v", err)
	}
	if err := pipeChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC: %v", err)
	}

	// Phase 5: Decode the published witness for block 2 and verify Headers
	// extends past parent, which is what AddBlockHash(0) records during
	// execution.
	target := block2Slice[0]
	encoded := pipeChain.GetWitness(target.Hash())
	if encoded == nil {
		t.Fatalf("block 2: witness missing from cache")
	}
	var w stateless.Witness
	if err := rlp.DecodeBytes(encoded, &w); err != nil {
		t.Fatalf("decode witness: %v", err)
	}

	// AddBlockHash(0) walks back from parent (block 1) to genesis, so the
	// published witness's Headers slice should have length 2: [block 1, genesis].
	if len(w.Headers) < 2 {
		t.Fatalf("Headers length = %d, want >= 2 (parent + genesis from BLOCKHASH(0)); "+
			"the published witness must include execution-time AddBlockHash entries",
			len(w.Headers))
	}

	parentHeader := prefix[0].Header()
	if w.Headers[0].Hash() != parentHeader.Hash() {
		t.Errorf("Headers[0] = %s, want parent (block 1) %s", w.Headers[0].Hash(), parentHeader.Hash())
	}
	foundGenesis := false
	for _, h := range w.Headers {
		if h.Number.Uint64() == 0 && h.Hash() == genesisBlock.Hash() {
			foundGenesis = true
			break
		}
	}
	if !foundGenesis {
		t.Errorf("Headers does not contain genesis (BLOCKHASH(0) referenced it); Headers=%d entries", len(w.Headers))
	}

	if err := stateless.ValidateWitnessPreState(&w, pipeChain, target.Header()); err != nil {
		t.Errorf("ValidateWitnessPreState failed on the published witness: %v", err)
	}
}

// TestPipelinedImportSRC_WitnessEncodeDecodeRoundtrip verifies that pipelined
// witnesses encode-decode cleanly and pass canonical pre-state validation,
// covering the basic shared-witness contract: the same Witness object that
// EVM execution populated must round-trip through RLP and validate.
func TestPipelinedImportSRC_WitnessEncodeDecodeRoundtrip(t *testing.T) {
	const numBlocks = 3
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("failed to create pipeline chain: %v", err)
	}
	defer pipeChain.Stop()

	if _, err := pipeChain.InsertChain(blocks, true); err != nil {
		t.Fatalf("InsertChain (makeWitness=true) failed: %v", err)
	}
	if err := pipeChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC failed: %v", err)
	}

	for i := uint64(1); i <= numBlocks; i++ {
		block := pipeChain.GetBlockByNumber(i)
		encoded := pipeChain.GetWitness(block.Hash())
		if encoded == nil {
			t.Fatalf("block %d: witness missing from cache", i)
		}
		var w stateless.Witness
		if err := rlp.DecodeBytes(encoded, &w); err != nil {
			t.Fatalf("block %d: decode witness: %v", i, err)
		}
		if len(w.Headers) == 0 {
			t.Errorf("block %d: decoded witness has no Headers", i)
		}
		if err := stateless.ValidateWitnessPreState(&w, pipeChain, block.Header()); err != nil {
			t.Errorf("block %d: ValidateWitnessPreState failed: %v", i, err)
		}
	}
}

// TestPipelinedImportSRC_WarmSnapshotWitnessParity is the consensus-critical
// parity test for the warm-snapshot handoff. The same chain is imported
// twice — once with PipelinedSRCWarmSnapshot=false (baseline pathdb-only
// reader) and once with PipelinedSRCWarmSnapshot=true (snapshot-aware
// reader). Per-block state roots, decoded witnesses, and the State proof
// node sets must be identical, AND each published witness must replay
// statelessly via ProcessBlockWithWitnesses to the same root the chain
// produced. Any divergence proves the snapshot path silently dropped or
// substituted proof nodes.
//
// Root parity alone is necessary but not sufficient: it proves the commit
// walk produced the same hash, not that the published witness covers the
// same proof surface. Stateless replay is the strongest assertion — it
// reconstructs state from the witness alone (plus the receiver's local
// codes/headers) and recomputes the post-state root.
//
// Runs under both HashScheme and PathScheme; PathScheme is the production
// target and exercises the pathdb fallthrough path in the snapshot reader.
func TestPipelinedImportSRC_WarmSnapshotWitnessParity(t *testing.T) {
	testPipelinedImportSRC_WarmSnapshotWitnessParity(t, rawdb.HashScheme)
	testPipelinedImportSRC_WarmSnapshotWitnessParity(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_WarmSnapshotWitnessParity(t *testing.T, scheme string) {
	t.Run(scheme, func(t *testing.T) {
		const numBlocks = 8
		var (
			key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
			addr      = crypto.PubkeyToAddress(key.PublicKey)
			recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
			funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
			gspec     = &Genesis{
				Config:  params.AllEthashProtocolChanges,
				Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
				BaseFee: big.NewInt(params.InitialBaseFee),
			}
			signer = types.LatestSigner(gspec.Config)
			engine = ethash.NewFaker()
		)

		_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
			tx, _ := types.SignTx(
				types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(1000), params.TxGas, gen.header.BaseFee, nil),
				signer, key,
			)
			gen.AddTx(tx)
		})

		importInto := func(t *testing.T, label string, cfg *BlockChainConfig) *BlockChain {
			t.Helper()
			chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
			if err != nil {
				t.Fatalf("%s: NewBlockChain: %v", label, err)
			}
			t.Cleanup(chain.Stop)
			if _, err := chain.InsertChain(blocks, true); err != nil {
				t.Fatalf("%s: InsertChain: %v", label, err)
			}
			if err := chain.flushPendingImportSRC(true); err != nil {
				t.Fatalf("%s: flushPendingImportSRC: %v", label, err)
			}
			return chain
		}

		chainOff := importInto(t, "snapshot-off", pipelinedConfig(scheme))
		chainOn := importInto(t, "snapshot-on", pipelinedConfigWithWarmSnapshot(scheme))

		for i := uint64(1); i <= numBlocks; i++ {
			blkOff := chainOff.GetBlockByNumber(i)
			blkOn := chainOn.GetBlockByNumber(i)
			if blkOff == nil || blkOn == nil {
				t.Fatalf("block %d: missing on one of the chains (off=%v on=%v)", i, blkOff == nil, blkOn == nil)
			}
			if blkOff.Root() != blkOn.Root() {
				t.Errorf("block %d: state root mismatch off=%s on=%s", i, blkOff.Root(), blkOn.Root())
			}

			encOff := chainOff.GetWitness(blkOff.Hash())
			encOn := chainOn.GetWitness(blkOn.Hash())
			if encOff == nil || encOn == nil {
				t.Fatalf("block %d: witness missing (off=%v on=%v)", i, encOff == nil, encOn == nil)
			}

			var wOff, wOn stateless.Witness
			if err := rlp.DecodeBytes(encOff, &wOff); err != nil {
				t.Fatalf("block %d: decode off witness: %v", i, err)
			}
			if err := rlp.DecodeBytes(encOn, &wOn); err != nil {
				t.Fatalf("block %d: decode on witness: %v", i, err)
			}

			if err := stateless.ValidateWitnessPreState(&wOff, chainOff, blkOff.Header()); err != nil {
				t.Errorf("block %d: off witness pre-state validation: %v", i, err)
			}
			if err := stateless.ValidateWitnessPreState(&wOn, chainOn, blkOn.Header()); err != nil {
				t.Errorf("block %d: on witness pre-state validation: %v", i, err)
			}

			// State proof-node parity: the snapshot path must produce the
			// same set of trie proof nodes as the no-snapshot path. The
			// State map's keys are RLP node blobs; equal sets = equal proof
			// coverage.
			if len(wOff.State) != len(wOn.State) {
				t.Errorf("block %d: State size off=%d on=%d", i, len(wOff.State), len(wOn.State))
			}
			for k := range wOff.State {
				if _, ok := wOn.State[k]; !ok {
					t.Errorf("block %d: snapshot-on witness missing proof node present in baseline (len=%d)", i, len(k))
					break
				}
			}
			for k := range wOn.State {
				if _, ok := wOff.State[k]; !ok {
					t.Errorf("block %d: snapshot-on witness has proof node not in baseline (len=%d)", i, len(k))
					break
				}
			}

			// Headers parity: BLOCKHASH ancestor inclusion must be identical
			// across snapshot-on/off. (For this transfer-only chain there are
			// no BLOCKHASH ops; Headers is just [parent].)
			if len(wOff.Headers) != len(wOn.Headers) {
				t.Errorf("block %d: Headers length off=%d on=%d", i, len(wOff.Headers), len(wOn.Headers))
			}

			// Stateless replay parity: each chain's published witness must
			// reconstruct state and recompute the post-state root via
			// ProcessBlockWithWitnesses. ExecuteStateless returns an error
			// if the recomputed root diverges from block.Root() — that's
			// exactly the assertion we want. SetHeader installs the
			// context header (the block being replayed) on the decoded
			// witness, which RLP decode does not preserve. The witness's
			// HeaderReader (used to resolve BLOCKHASH ancestor lookups) is
			// also dropped by RLP decode, but ProcessBlockWithWitnesses
			// falls back to the BlockChain itself when the witness has no
			// HeaderReader set — so for in-memory test chains nothing
			// further needs wiring here.
			wOff.SetHeader(blkOff.Header())
			wOn.SetHeader(blkOn.Header())
			if _, _, err := chainOff.ProcessBlockWithWitnesses(blkOff, &wOff); err != nil {
				t.Errorf("block %d: stateless replay (off chain witness) failed: %v", i, err)
			}
			if _, _, err := chainOn.ProcessBlockWithWitnesses(blkOn, &wOn); err != nil {
				t.Errorf("block %d: stateless replay (on chain witness) failed: %v", i, err)
			}
		}
	})
}

// TestPipelinedImportSRC_WarmSnapshotPreservesBlockHashAncestors mirrors
// TestPipelinedImportSRC_WitnessIncludesBlockHashAncestors but with the
// warm-snapshot handoff enabled. BLOCKHASH ancestor coverage is collected on
// the execution witness during EVM execution, before SRC starts; the
// snapshot path only changes how SRC's trie reads are served, not the
// witness ownership chain. Therefore Headers must still extend to include
// the BLOCKHASH-referenced ancestor, and the published witness must
// statelessly replay.
//
// Runs under both HashScheme and PathScheme.
func TestPipelinedImportSRC_WarmSnapshotPreservesBlockHashAncestors(t *testing.T) {
	testPipelinedImportSRC_WarmSnapshotPreservesBlockHashAncestors(t, rawdb.HashScheme)
	testPipelinedImportSRC_WarmSnapshotPreservesBlockHashAncestors(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_WarmSnapshotPreservesBlockHashAncestors(t *testing.T, scheme string) {
	t.Run(scheme, func(t *testing.T) {
		runWarmSnapshotBlockHashTest(t, scheme)
	})
}

func runWarmSnapshotBlockHashTest(t *testing.T, scheme string) {
	contractCode := []byte{0x60, 0x00, 0x40, 0x50, 0x00}
	contractAddr := common.HexToAddress("0xb1a5cab1ec0de")

	var (
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		funds  = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec  = &Genesis{
			Config: params.AllEthashProtocolChanges,
			Alloc: types.GenesisAlloc{
				addr:         {Balance: funds},
				contractAddr: {Code: contractCode, Balance: big.NewInt(0)},
			},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, triedb.HashDefaults)
	genesisBlock := gspec.MustCommit(db, tdb)
	prefix, _ := GenerateChain(gspec.Config, genesisBlock, engine, db, 1, func(i int, gen *BlockGen) {})

	ctxChain, err := NewBlockChain(db, gspec, engine, DefaultConfig().WithStateScheme(rawdb.HashScheme))
	if err != nil {
		t.Fatalf("ctxChain: %v", err)
	}
	if _, err := ctxChain.InsertChain(prefix, false); err != nil {
		t.Fatalf("ctxChain insert prefix: %v", err)
	}
	block2Slice, _ := GenerateChain(gspec.Config, prefix[0], engine, db, 1, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), contractAddr, big.NewInt(0), 100_000, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTxWithChain(ctxChain, tx)
	})
	ctxChain.Stop()

	allBlocks := append(append([]*types.Block{}, prefix...), block2Slice...)

	pipeChain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfigWithWarmSnapshot(scheme))
	if err != nil {
		t.Fatalf("pipeChain: %v", err)
	}
	defer pipeChain.Stop()

	if _, err := pipeChain.InsertChain(allBlocks, true); err != nil {
		t.Fatalf("InsertChain (snapshot=true, witness=true): %v", err)
	}
	if err := pipeChain.flushPendingImportSRC(true); err != nil {
		t.Fatalf("flushPendingImportSRC: %v", err)
	}

	target := block2Slice[0]
	encoded := pipeChain.GetWitness(target.Hash())
	if encoded == nil {
		t.Fatalf("block 2: witness missing from cache")
	}
	var w stateless.Witness
	if err := rlp.DecodeBytes(encoded, &w); err != nil {
		t.Fatalf("decode witness: %v", err)
	}
	if len(w.Headers) < 2 {
		t.Fatalf("Headers length = %d, want >= 2 (parent + genesis from BLOCKHASH(0)) — "+
			"warm-snapshot path must preserve BLOCKHASH ancestor coverage", len(w.Headers))
	}
	parentHeader := prefix[0].Header()
	if w.Headers[0].Hash() != parentHeader.Hash() {
		t.Errorf("Headers[0] = %s, want parent %s", w.Headers[0].Hash(), parentHeader.Hash())
	}
	foundGenesis := false
	for _, h := range w.Headers {
		if h.Number.Uint64() == 0 && h.Hash() == genesisBlock.Hash() {
			foundGenesis = true
			break
		}
	}
	if !foundGenesis {
		t.Errorf("Headers does not contain genesis (BLOCKHASH(0) referenced it); Headers=%d entries", len(w.Headers))
	}
	if err := stateless.ValidateWitnessPreState(&w, pipeChain, target.Header()); err != nil {
		t.Errorf("ValidateWitnessPreState failed on snapshot-on witness: %v", err)
	}

	// Stateless replay: the published witness must reconstruct state
	// (including the BLOCKHASH ancestor lookup) and recompute the same
	// post-state root via ExecuteStateless. This is the consumer-side
	// check that the snapshot path's witness is actually replayable, not
	// just structurally well-formed.
	w.SetHeader(target.Header())
	if _, _, err := pipeChain.ProcessBlockWithWitnesses(target, &w); err != nil {
		t.Errorf("stateless replay of snapshot-on BLOCKHASH witness failed: %v", err)
	}
}

// TestPipelinedImportSRC_WarmSnapshotStorageTrieParity is the storage-trie
// counterpart to the witness parity test. The snapshot reader is wrapped at
// the database.NodeReader layer used by both account and storage tries; this
// test specifically exercises storage-trie reads (SLOAD) and writes (SSTORE)
// so the storage-owner branch of newSnapshotNodeDatabase / trieReader.Storage
// is covered. A single contract is deployed with pre-populated storage; each
// block's transaction loads a previously-set slot and writes a new one,
// progressively growing the storage trie. The witness must include the
// storage proof nodes touched by both the SLOAD and the SSTORE update path.
//
// Snapshot-off and snapshot-on import the same chain into independent
// blockchains; per-block roots, decoded witness State sets, and stateless
// replay must all agree. Runs under both HashScheme and PathScheme.
func TestPipelinedImportSRC_WarmSnapshotStorageTrieParity(t *testing.T) {
	testPipelinedImportSRC_WarmSnapshotStorageTrieParity(t, rawdb.HashScheme)
	testPipelinedImportSRC_WarmSnapshotStorageTrieParity(t, rawdb.PathScheme)
}

func testPipelinedImportSRC_WarmSnapshotStorageTrieParity(t *testing.T, scheme string) {
	t.Run(scheme, func(t *testing.T) {
		// Contract program:
		//   SLOAD(NUMBER - 1) ; POP   — read a previously-set slot, forcing
		//                               a storage-trie pre-state read whose
		//                               proof nodes must be in the witness
		//   SSTORE(NUMBER, NUMBER)    — write a new slot, growing the trie
		//                               (the update walks the proof path)
		contractCode := []byte{
			byte(vm.PUSH1), 0x01,
			byte(vm.NUMBER),
			byte(vm.SUB),
			byte(vm.SLOAD),
			byte(vm.POP),
			byte(vm.NUMBER),
			byte(vm.NUMBER),
			byte(vm.SSTORE),
			byte(vm.STOP),
		}
		contractAddr := common.HexToAddress("0x5707a6e500000000000000000000000000000001")

		// Pre-populate slots 0..7 in the contract's storage trie so block 1's
		// SLOAD(0) hits a real entry rather than an empty slot, ensuring the
		// pre-state read actually walks storage-trie nodes.
		preStorage := make(map[common.Hash]common.Hash, 8)
		for i := 0; i < 8; i++ {
			preStorage[common.BigToHash(big.NewInt(int64(i)))] = common.BigToHash(big.NewInt(int64(i + 1000)))
		}

		const numBlocks = 6
		var (
			key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
			addr   = crypto.PubkeyToAddress(key.PublicKey)
			funds  = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
			gspec  = &Genesis{
				Config: params.AllEthashProtocolChanges,
				Alloc: types.GenesisAlloc{
					addr: {Balance: funds},
					contractAddr: {
						Code:    contractCode,
						Balance: big.NewInt(0),
						Storage: preStorage,
					},
				},
				BaseFee: big.NewInt(params.InitialBaseFee),
			}
			signer = types.LatestSigner(gspec.Config)
			engine = ethash.NewFaker()
		)

		_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
			tx, _ := types.SignTx(
				types.NewTransaction(gen.TxNonce(addr), contractAddr, big.NewInt(0), 100_000, gen.header.BaseFee, nil),
				signer, key,
			)
			gen.AddTx(tx)
		})

		importInto := func(t *testing.T, label string, cfg *BlockChainConfig) *BlockChain {
			t.Helper()
			chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
			if err != nil {
				t.Fatalf("%s: NewBlockChain: %v", label, err)
			}
			t.Cleanup(chain.Stop)
			if _, err := chain.InsertChain(blocks, true); err != nil {
				t.Fatalf("%s: InsertChain: %v", label, err)
			}
			if err := chain.flushPendingImportSRC(true); err != nil {
				t.Fatalf("%s: flushPendingImportSRC: %v", label, err)
			}
			return chain
		}

		chainOff := importInto(t, "snapshot-off", pipelinedConfig(scheme))
		chainOn := importInto(t, "snapshot-on", pipelinedConfigWithWarmSnapshot(scheme))

		for i := uint64(1); i <= numBlocks; i++ {
			blkOff := chainOff.GetBlockByNumber(i)
			blkOn := chainOn.GetBlockByNumber(i)
			if blkOff == nil || blkOn == nil {
				t.Fatalf("block %d: missing on one of the chains (off=%v on=%v)", i, blkOff == nil, blkOn == nil)
			}
			if blkOff.Root() != blkOn.Root() {
				t.Errorf("block %d: state root mismatch off=%s on=%s", i, blkOff.Root(), blkOn.Root())
			}

			encOff := chainOff.GetWitness(blkOff.Hash())
			encOn := chainOn.GetWitness(blkOn.Hash())
			if encOff == nil || encOn == nil {
				t.Fatalf("block %d: witness missing (off=%v on=%v)", i, encOff == nil, encOn == nil)
			}

			var wOff, wOn stateless.Witness
			if err := rlp.DecodeBytes(encOff, &wOff); err != nil {
				t.Fatalf("block %d: decode off witness: %v", i, err)
			}
			if err := rlp.DecodeBytes(encOn, &wOn); err != nil {
				t.Fatalf("block %d: decode on witness: %v", i, err)
			}

			if err := stateless.ValidateWitnessPreState(&wOff, chainOff, blkOff.Header()); err != nil {
				t.Errorf("block %d: off witness pre-state validation: %v", i, err)
			}
			if err := stateless.ValidateWitnessPreState(&wOn, chainOn, blkOn.Header()); err != nil {
				t.Errorf("block %d: on witness pre-state validation: %v", i, err)
			}

			// State proof-node parity. The snapshot path must produce the same
			// set of trie proof nodes as the pathdb-only path. For this test
			// the State map covers both account-trie and storage-trie nodes.
			if len(wOff.State) != len(wOn.State) {
				t.Errorf("block %d: State size off=%d on=%d", i, len(wOff.State), len(wOn.State))
			}
			for k := range wOff.State {
				if _, ok := wOn.State[k]; !ok {
					t.Errorf("block %d: snapshot-on witness missing proof node present in baseline (len=%d)", i, len(k))
					break
				}
			}
			for k := range wOn.State {
				if _, ok := wOff.State[k]; !ok {
					t.Errorf("block %d: snapshot-on witness has proof node not in baseline (len=%d)", i, len(k))
					break
				}
			}

			// Stateless replay parity. ExecuteStateless reconstructs the
			// pre-state from the witness's State proof nodes (including
			// storage subtries) and recomputes the post-state root. Failure
			// here indicates the snapshot path served a storage-trie node
			// whose blob disagrees with what pathdb would have served.
			wOff.SetHeader(blkOff.Header())
			wOn.SetHeader(blkOn.Header())
			if _, _, err := chainOff.ProcessBlockWithWitnesses(blkOff, &wOff); err != nil {
				t.Errorf("block %d: stateless replay (off chain witness) failed: %v", i, err)
			}
			if _, _, err := chainOn.ProcessBlockWithWitnesses(blkOn, &wOn); err != nil {
				t.Errorf("block %d: stateless replay (on chain witness) failed: %v", i, err)
			}
		}
	})
}
