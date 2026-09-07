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

package tracers

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/ethapi/override"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	errStateNotFound = errors.New("state not found")
	errBlockNotFound = errors.New("block not found")
)

type testBackend struct {
	chainConfig *params.ChainConfig
	engine      consensus.Engine
	chaindb     ethdb.Database
	chain       *core.BlockChain

	refHook func() // Hook is invoked when the requested state is referenced
	relHook func() // Hook is invoked when the requested state is released
}

// newTestBackend creates a new test backend. OBS: After test is done, teardown must be
// invoked in order to release associated resources.
func newTestBackend(t *testing.T, n int, gspec *core.Genesis, generator func(i int, b *core.BlockGen)) *testBackend {
	backend := &testBackend{
		chainConfig: params.TestChainConfig,
		engine:      ethash.NewFaker(),
		chaindb:     rawdb.NewMemoryDatabase(),
	}
	// Generate blocks for testing
	gspec.Config = backend.chainConfig
	_, blocks, _ := core.GenerateChainWithGenesis(gspec, backend.engine, n, generator)

	// Import the canonical chain
	options := &core.BlockChainConfig{
		TrieCleanLimit: 256,
		TrieDirtyLimit: 256,
		TrieTimeLimit:  5 * time.Minute,
		SnapshotLimit:  0,
		ArchiveMode:    true, // Archive mode
	}
	chain, err := core.NewBlockChain(backend.chaindb, gspec, backend.engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	backend.chain = chain

	return backend
}

func (b *testBackend) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	return b.chain.GetHeaderByHash(hash), nil
}

func (b *testBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if number == rpc.PendingBlockNumber || number == rpc.LatestBlockNumber {
		return b.chain.CurrentHeader(), nil
	}

	return b.chain.GetHeaderByNumber(uint64(number)), nil
}

func (b *testBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	return b.chain.GetBlockByHash(hash), nil
}

func (b *testBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number == rpc.PendingBlockNumber || number == rpc.LatestBlockNumber {
		return b.chain.GetBlockByNumber(b.chain.CurrentBlock().Number.Uint64()), nil
	}

	return b.chain.GetBlockByNumber(uint64(number)), nil
}

func (b *testBackend) GetCanonicalTransaction(txHash common.Hash) (bool, *types.Transaction, common.Hash, uint64, uint64) {
	tx, hash, blockNumber, index := rawdb.ReadCanonicalTransaction(b.chaindb, txHash)
	return tx != nil, tx, hash, blockNumber, index
}

func (b *testBackend) TxIndexDone() bool {
	return true
}

func (b *testBackend) RPCGasCap() uint64 {
	return 25000000
}

func (b *testBackend) RPCRpcReturnDataLimit() uint64 {
	return 100000
}

func (b *testBackend) ChainConfig() *params.ChainConfig {
	return b.chainConfig
}

func (b *testBackend) Engine() consensus.Engine {
	return b.engine
}

func (b *testBackend) ChainDb() ethdb.Database {
	return b.chaindb
}

func (b *testBackend) CurrentHeader() *types.Header {
	return b.chain.CurrentHeader()
}

// teardown releases the associated resources.
func (b *testBackend) teardown() {
	b.chain.Stop()
}

func (b *testBackend) StateAtBlock(ctx context.Context, block *types.Block, reexec uint64, base *state.StateDB, readOnly bool, preferDisk bool) (*state.StateDB, StateReleaseFunc, error) {
	statedb, err := b.chain.StateAt(block.Root())
	if err != nil {
		return nil, nil, errStateNotFound
	}

	if b.refHook != nil {
		b.refHook()
	}

	release := func() {
		if b.relHook != nil {
			b.relHook()
		}
	}

	return statedb, release, nil
}

func (b *testBackend) StateAtTransaction(ctx context.Context, block *types.Block, txIndex int, reexec uint64) (*types.Transaction, vm.BlockContext, *state.StateDB, StateReleaseFunc, error) {
	parent := b.chain.GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return nil, vm.BlockContext{}, nil, nil, errBlockNotFound
	}

	statedb, release, err := b.StateAtBlock(ctx, parent, reexec, nil, true, false)
	if err != nil {
		return nil, vm.BlockContext{}, nil, nil, errStateNotFound
	}

	if txIndex == 0 && len(block.Transactions()) == 0 {
		return nil, vm.BlockContext{}, statedb, release, nil
	}
	// Recompute transactions up to the target index.
	signer := types.MakeSigner(b.chainConfig, block.Number(), block.Time())
	blockContext := core.NewEVMBlockContext(block.Header(), b.chain, nil)
	evm := vm.NewEVM(blockContext, statedb, b.chainConfig, vm.Config{})
	for idx, tx := range block.Transactions() {
		if idx == txIndex {
			return tx, blockContext, statedb, release, nil
		}
		msg, _ := core.TransactionToMessage(tx, signer, block.BaseFee())

		blockContext := core.NewEVMBlockContext(block.Header(), b.chain, nil)
		if idx == txIndex {
			return tx, blockContext, statedb, release, nil
		}
		if _, err := core.ApplyMessage(evm, msg, new(core.GasPool).AddGas(tx.Gas())); err != nil {
			return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("transaction %#x failed: %v", tx.Hash(), err)
		}

		statedb.Finalise(evm.ChainConfig().IsEIP158(block.Number()))
	}

	return nil, vm.BlockContext{}, nil, nil, fmt.Errorf("transaction index %d out of range for block %#x", txIndex, block.Hash())
}

func (b *testBackend) GetBorBlockTransactionWithBlockHash(ctx context.Context, txHash common.Hash, blockHash common.Hash) (*types.Transaction, common.Hash, uint64, uint64, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadBorTransactionWithBlockHash(b.ChainDb(), txHash, blockHash)
	return tx, blockHash, blockNumber, index, nil
}

// prunedTestBackend wraps testBackend and simulates a non-archive (pruned) node
// by making all historical state unavailable.
type prunedTestBackend struct {
	*testBackend
}

func (b *prunedTestBackend) StateAtBlock(_ context.Context, _ *types.Block, _ uint64, _ *state.StateDB, _ bool, _ bool) (*state.StateDB, StateReleaseFunc, error) {
	return nil, nil, errStateNotFound
}

func (b *prunedTestBackend) StateAtTransaction(_ context.Context, _ *types.Block, _ int, _ uint64) (*types.Transaction, vm.BlockContext, *state.StateDB, StateReleaseFunc, error) {
	return nil, vm.BlockContext{}, nil, nil, errStateNotFound
}

type stateTracer struct {
	Balance map[common.Address]*hexutil.Big
	Nonce   map[common.Address]hexutil.Uint64
	Storage map[common.Address]map[common.Hash]common.Hash
}

func newStateTracer(ctx *Context, cfg json.RawMessage, chainCfg *params.ChainConfig) (*Tracer, error) {
	t := &stateTracer{
		Balance: make(map[common.Address]*hexutil.Big),
		Nonce:   make(map[common.Address]hexutil.Uint64),
		Storage: make(map[common.Address]map[common.Hash]common.Hash),
	}
	return &Tracer{
		GetResult: func() (json.RawMessage, error) {
			return json.Marshal(t)
		},
		Hooks: &tracing.Hooks{
			OnBalanceChange: func(addr common.Address, prev, new *big.Int, reason tracing.BalanceChangeReason) {
				t.Balance[addr] = (*hexutil.Big)(new)
			},
			OnNonceChange: func(addr common.Address, prev, new uint64) {
				t.Nonce[addr] = hexutil.Uint64(new)
			},
			OnStorageChange: func(addr common.Address, slot common.Hash, prev, new common.Hash) {
				if t.Storage[addr] == nil {
					t.Storage[addr] = make(map[common.Hash]common.Hash)
				}
				t.Storage[addr][slot] = new
			},
		},
	}, nil
}

func TestStateHooks(t *testing.T) {
	// NOTE: intentionally NOT parallel. This test mutates the global
	// DefaultDirectory via Register("stateTracer", ...). Running it in the
	// parallel phase races with other tracing tests that read the directory
	// (directory.IsJS/New) from worker goroutines. Keeping it sequential makes
	// the global write happen-before any parallel reader resumes.

	// Initialize test accounts
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		from    = crypto.PubkeyToAddress(key.PublicKey)
		to      = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		genesis = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				from: {Balance: big.NewInt(params.Ether)},
				to: {
					Code: []byte{
						byte(vm.PUSH1), 0x2a, // stack: [42]
						byte(vm.PUSH1), 0x0, // stack: [0, 42]
						byte(vm.SSTORE), // stack: []
						byte(vm.STOP),
					},
				},
			},
		}
		genBlocks = 2
		signer    = types.HomesteadSigner{}
		nonce     = uint64(0)
		backend   = newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
			// Transfer from account[0] to account[1]
			//    value: 1000 wei
			//    fee:   0 wei
			tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    nonce,
				To:       &to,
				Value:    big.NewInt(1000),
				Gas:      params.TxGas,
				GasPrice: b.BaseFee(),
				Data:     nil}),
				signer, key)
			b.AddTx(tx)
			nonce++
		})
	)
	defer backend.teardown()
	DefaultDirectory.Register("stateTracer", newStateTracer, false)
	api := NewAPI(backend)
	tracer := "stateTracer"
	res, err := api.TraceCall(t.Context(), ethapi.TransactionArgs{From: &from, To: &to, Value: (*hexutil.Big)(big.NewInt(1000))}, rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber), &TraceCallConfig{TraceConfig: TraceConfig{Tracer: &tracer}})
	if err != nil {
		t.Fatalf("failed to trace call: %v", err)
	}
	expected := `{"Balance":{"0x00000000000000000000000000000000deadbeef":"0x3e8","0x71562b71999873db5b286df957af199ec94617f7":"0xde0975924ed6f90"},"Nonce":{"0x71562b71999873db5b286df957af199ec94617f7":"0x3"},"Storage":{"0x00000000000000000000000000000000deadbeef":{"0x0000000000000000000000000000000000000000000000000000000000000000":"0x000000000000000000000000000000000000000000000000000000000000002a"}}}`
	if expected != fmt.Sprintf("%s", res) {
		t.Fatalf("unexpected trace result: have %s want %s", res, expected)
	}
}

func TestTraceCall(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 10
	signer := types.HomesteadSigner{}
	nonce := uint64(0)
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
		nonce++

		if i == genBlocks-2 {
			// Transfer from account[0] to account[2]
			tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    nonce,
				To:       &accounts[2].addr,
				Value:    big.NewInt(1000),
				Gas:      params.TxGas,
				GasPrice: b.BaseFee(),
				Data:     nil}),
				signer, accounts[0].key)
			b.AddTx(tx)
			nonce++

			// Transfer from account[0] to account[1] again
			tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce:    nonce,
				To:       &accounts[1].addr,
				Value:    big.NewInt(1000),
				Gas:      params.TxGas,
				GasPrice: b.BaseFee(),
				Data:     nil}),
				signer, accounts[0].key)
			b.AddTx(tx)
			nonce++
		}
	})

	uintPtr := func(i int) *hexutil.Uint { x := hexutil.Uint(i); return &x }

	defer backend.teardown()
	api := NewAPI(backend)

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		call        ethapi.TransactionArgs
		config      *TraceCallConfig
		expectErr   error
		expect      string
	}{
		// Standard JSON trace upon the genesis, plain transfer.
		{
			blockNumber: rpc.BlockNumber(0),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}`,
		},
		// Standard JSON trace upon the head, plain transfer.
		{
			blockNumber: rpc.BlockNumber(genBlocks),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}`,
		},
		// Upon the last state, default to the post block's state
		{
			blockNumber: rpc.BlockNumber(genBlocks - 1),
			call: ethapi.TransactionArgs{
				From:  &accounts[2].addr,
				To:    &accounts[0].addr,
				Value: (*hexutil.Big)(new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(100))),
			},
			config: nil,
			expect: `{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}`,
		},
		// Before the first transaction, should be failed
		{
			blockNumber: rpc.BlockNumber(genBlocks - 1),
			call: ethapi.TransactionArgs{
				From:  &accounts[2].addr,
				To:    &accounts[0].addr,
				Value: (*hexutil.Big)(new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(100))),
			},
			config:    &TraceCallConfig{TxIndex: uintPtr(0)},
			expectErr: fmt.Errorf("tracing failed: insufficient funds for gas * price + value: address %s have 1000000000000000000 want 1000000000000000100", accounts[2].addr),
		},
		// Before the target transaction, should be failed
		{
			blockNumber: rpc.BlockNumber(genBlocks - 1),
			call: ethapi.TransactionArgs{
				From:  &accounts[2].addr,
				To:    &accounts[0].addr,
				Value: (*hexutil.Big)(new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(100))),
			},
			config:    &TraceCallConfig{TxIndex: uintPtr(1)},
			expectErr: fmt.Errorf("tracing failed: insufficient funds for gas * price + value: address %s have 1000000000000000000 want 1000000000000000100", accounts[2].addr),
		},
		// After the target transaction, should be succeeded
		{
			blockNumber: rpc.BlockNumber(genBlocks - 1),
			call: ethapi.TransactionArgs{
				From:  &accounts[2].addr,
				To:    &accounts[0].addr,
				Value: (*hexutil.Big)(new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(100))),
			},
			config:    &TraceCallConfig{TxIndex: uintPtr(2)},
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}`,
		},
		// Standard JSON trace upon the non-existent block, error expects
		{
			blockNumber: rpc.BlockNumber(genBlocks + 1),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: fmt.Errorf("block #%d not found", genBlocks+1),
			//expect:    nil,
		},
		// Standard JSON trace upon the latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}`,
		},
		// Tracing on 'pending' should fail:
		{
			blockNumber: rpc.PendingBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    nil,
			expectErr: errors.New("tracing on top of pending is not supported"),
		},
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				Input: &hexutil.Bytes{0x43}, // blocknumber
			},
			config: &TraceCallConfig{
				BlockOverrides: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			expectErr: nil,
			expect: ` {"gas":53018,"failed":false,"returnValue":"0x","structLogs":[
		{"pc":0,"op":"NUMBER","gas":24946984,"gasCost":2,"depth":1,"stack":[]},
		{"pc":1,"op":"STOP","gas":24946982,"gasCost":0,"depth":1,"stack":["0x1337"]}]}`,
		},
		// Tests issue #33014 where accessing nil block number override panics.
		{
			blockNumber: rpc.BlockNumber(0),
			call: ethapi.TransactionArgs{
				From:  &accounts[0].addr,
				To:    &accounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config: &TraceCallConfig{
				BlockOverrides: &override.BlockOverrides{},
			},
			expectErr: nil,
			expect:    `{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}`,
		},
	}

	for i, testspec := range testSuite {
		result, err := api.TraceCall(t.Context(), testspec.call, rpc.BlockNumberOrHash{BlockNumber: &testspec.blockNumber}, testspec.config)
		if testspec.expectErr != nil {
			if err == nil {
				t.Errorf("test %d: expect error %v, got nothing", i, testspec.expectErr)
				continue
			}
			if !reflect.DeepEqual(err.Error(), testspec.expectErr.Error()) {
				t.Errorf("test %d: error mismatch, want '%v', got '%v'", i, testspec.expectErr, err)
			}
		} else {
			if err != nil {
				t.Errorf("test %d: expect no error, got %v", i, err)
				continue
			}

			var have *logger.ExecutionResult
			if err := json.Unmarshal(result.(json.RawMessage), &have); err != nil {
				t.Errorf("test %d: failed to unmarshal result %v", i, err)
			}

			var want *logger.ExecutionResult
			if err := json.Unmarshal([]byte(testspec.expect), &want); err != nil {
				t.Errorf("test %d: failed to unmarshal result %v", i, err)
			}

			if !reflect.DeepEqual(have, want) {
				t.Errorf("test %d: result mismatch, want %v, got %v", i, testspec.expect, string(result.(json.RawMessage)))
			}
		}
	}
}

func TestTraceCallMany(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(3)
	genBlocks := 10 // chain head ends up at block number genBlocks
	// Tiny contracts whose top-of-stack reflects a block-context field, so the
	// active value shows up on the struct logger's stack.
	numberAddr := common.HexToAddress("0x000000000000000000000000000000000000aaaa")    // NUMBER; STOP
	timeAddr := common.HexToAddress("0x000000000000000000000000000000000000bbbb")      // TIMESTAMP; STOP
	blockhashAddr := common.HexToAddress("0x000000000000000000000000000000000000cccc") // blockhash(number-1); STOP
	basefeeAddr := common.HexToAddress("0x000000000000000000000000000000000000eeee")   // BASEFEE; STOP
	headHashAddr := common.HexToAddress("0x000000000000000000000000000000000000ffff")  // blockhash(genBlocks); STOP
	// An account with no genesis balance; funded only via a state override.
	overrideAddr := common.HexToAddress("0x000000000000000000000000000000000000dddd")
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
			numberAddr:       {Code: []byte{0x43, 0x00}, Balance: big.NewInt(0)},
			timeAddr:         {Code: []byte{0x42, 0x00}, Balance: big.NewInt(0)},
			// NUMBER, PUSH1 1, SWAP1, SUB, BLOCKHASH, STOP -> pushes blockhash(number-1).
			blockhashAddr: {Code: []byte{0x43, 0x60, 0x01, 0x90, 0x03, 0x40, 0x00}, Balance: big.NewInt(0)},
			basefeeAddr:   {Code: []byte{0x48, 0x00}, Balance: big.NewInt(0)}, // BASEFEE, STOP
			// PUSH1 genBlocks, BLOCKHASH, STOP -> pushes blockhash(genBlocks), the real head.
			headHashAddr: {Code: []byte{0x60, byte(genBlocks), 0x40, 0x00}, Balance: big.NewInt(0)},
		},
	}
	signer := types.HomesteadSigner{}
	nonce := uint64(0)
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		send := func(to common.Address) {
			tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
				Nonce: nonce, To: &to, Value: big.NewInt(1000), Gas: params.TxGas, GasPrice: b.BaseFee(),
			}), signer, accounts[0].key)
			b.AddTx(tx)
			nonce++
		}
		send(accounts[1].addr)
		if i == genBlocks-2 {
			// A 3-tx block: accounts[2] is funded by the tx at index 1, so the
			// state before/after that index is observably different.
			send(accounts[2].addr)
			send(accounts[1].addr)
		}
	})
	defer backend.teardown()
	api := NewAPI(backend)

	latest := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	transfer := func(from, to common.Address, value *big.Int) ethapi.TransactionArgs {
		return ethapi.TransactionArgs{From: &from, To: &to, Value: (*hexutil.Big)(value)}
	}
	mustResult := func(t *testing.T, v interface{}) *logger.ExecutionResult {
		t.Helper()
		var r *logger.ExecutionResult
		if err := json.Unmarshal(v.(json.RawMessage), &r); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		return r
	}
	// topOfStack returns the top stack word recorded by the struct logger right
	// before the final STOP — i.e. the value the tiny contract pushed.
	topOfStack := func(t *testing.T, v interface{}) string {
		t.Helper()
		res := mustResult(t, v)
		for i := len(res.StructLogs) - 1; i >= 0; i-- {
			var entry struct {
				Stack []string `json:"stack"`
			}
			if err := json.Unmarshal(res.StructLogs[i], &entry); err != nil {
				t.Fatalf("failed to unmarshal struct log: %v", err)
			}
			if len(entry.Stack) > 0 {
				return entry.Stack[len(entry.Stack)-1]
			}
		}
		t.Fatalf("no stack entries in trace")
		return ""
	}

	t.Run("single call", func(t *testing.T) {
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{
			transfer(accounts[0].addr, accounts[1].addr, big.NewInt(1000)),
		}}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(res) != 1 || len(res[0]) != 1 {
			t.Fatalf("unexpected result shape: %v", res)
		}
		if got := mustResult(t, res[0][0]); got.Failed || got.Gas != params.TxGas {
			t.Fatalf("unexpected trace: failed=%v gas=%d", got.Failed, got.Gas)
		}
	})

	t.Run("state persists across calls", func(t *testing.T) {
		// Call A funds accounts[0]->accounts[2]; call B then spends more than its
		// running balance would allow without A, so B only succeeds if A persisted.
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{
			transfer(accounts[0].addr, accounts[2].addr, big.NewInt(2000)),
			transfer(accounts[2].addr, accounts[1].addr, new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(2500))),
		}}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i, r := range res[0] {
			if got := mustResult(t, r); got.Failed {
				t.Fatalf("call %d failed; state should persist across calls in a bundle", i)
			}
		}
	})

	t.Run("block number override and per-bundle advance", func(t *testing.T) {
		numberCall := ethapi.TransactionArgs{From: &accounts[0].addr, To: &numberAddr}
		bundles := []Bundle{
			{
				Transactions:  []ethapi.TransactionArgs{numberCall},
				BlockOverride: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			{Transactions: []ethapi.TransactionArgs{numberCall}},
		}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := topOfStack(t, res[0][0]); got != "0x1337" {
			t.Fatalf("bundle 0 block number: want 0x1337, got %s", got)
		}
		// The second bundle inherits the prior bundle's block number, advanced by one.
		if got := topOfStack(t, res[1][0]); got != "0x1338" {
			t.Fatalf("bundle 1 block number: want 0x1338 (advanced), got %s", got)
		}
	})

	t.Run("block time override and per-bundle advance", func(t *testing.T) {
		timeCall := ethapi.TransactionArgs{From: &accounts[0].addr, To: &timeAddr}
		ts := hexutil.Uint64(0x9999)
		bundles := []Bundle{
			{
				Transactions:  []ethapi.TransactionArgs{timeCall},
				BlockOverride: &override.BlockOverrides{Time: &ts},
			},
			{Transactions: []ethapi.TransactionArgs{timeCall}},
		}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := topOfStack(t, res[0][0]); got != "0x9999" {
			t.Fatalf("bundle 0 timestamp: want 0x9999, got %s", got)
		}
		if got := topOfStack(t, res[1][0]); got != "0x999a" {
			t.Fatalf("bundle 1 timestamp: want 0x999a (advanced), got %s", got)
		}
	})

	t.Run("config block override applies as the base for all bundles", func(t *testing.T) {
		// config.BlockOverrides is the request-level base override (applied before any
		// bundle). Every bundle builds on it, then advances.
		numberCall := ethapi.TransactionArgs{From: &accounts[0].addr, To: &numberAddr}
		cfg := &TraceCallConfig{BlockOverrides: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x4242))}}
		bundles := []Bundle{
			{Transactions: []ethapi.TransactionArgs{numberCall}},
			{Transactions: []ethapi.TransactionArgs{numberCall}},
		}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := topOfStack(t, res[0][0]); got != "0x4242" {
			t.Fatalf("bundle 0 did not observe config block number: want 0x4242, got %s", got)
		}
		if got := topOfStack(t, res[1][0]); got != "0x4243" {
			t.Fatalf("bundle 1 block number: want 0x4243 (advanced), got %s", got)
		}
	})

	t.Run("blockhash resolves when overriding to head+1", func(t *testing.T) {
		// Overriding the block number to head+1 must rewire GetHash so that
		// blockhash(head) returns the real head hash instead of zero.
		head := backend.chain.CurrentBlock().Number.Uint64()
		bundles := []Bundle{{
			Transactions:  []ethapi.TransactionArgs{{From: &accounts[0].addr, To: &blockhashAddr}},
			BlockOverride: &override.BlockOverrides{Number: (*hexutil.Big)(new(big.Int).SetUint64(head + 1))},
		}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := topOfStack(t, res[0][0]); got == "0x0" {
			t.Fatalf("blockhash(head) resolved to zero; the head+1 parent-hash fixup did not apply")
		}
	})

	t.Run("blockhash of the real head survives sequential number overrides", func(t *testing.T) {
		// Regression: the n+1 fixup must not mutate shared state across bundles.
		// Bundle 0 overrides to head+1 (fires the fixup); bundle 1 overrides to head+2.
		// blockhash(head) — a real historical block — must stay resolvable in both,
		// not get clobbered to zero by a synthetic parent hash carried over from bundle 0.
		head := backend.chain.CurrentBlock().Number.Uint64()
		if head != uint64(genBlocks) {
			t.Fatalf("test assumes head == genBlocks (%d), got %d", genBlocks, head)
		}
		headCall := []ethapi.TransactionArgs{{From: &accounts[0].addr, To: &headHashAddr}}
		bundles := []Bundle{
			{Transactions: headCall, BlockOverride: &override.BlockOverrides{Number: (*hexutil.Big)(new(big.Int).SetUint64(head + 1))}},
			{Transactions: headCall, BlockOverride: &override.BlockOverrides{Number: (*hexutil.Big)(new(big.Int).SetUint64(head + 2))}},
		}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i := range bundles {
			if got := topOfStack(t, res[i][0]); got == "0x0" {
				t.Fatalf("bundle %d: blockhash(head) resolved to zero; shared-header mutation corrupted GetHash", i)
			}
		}
	})

	t.Run("blockhash of the real head resolves after the natural per-bundle advance", func(t *testing.T) {
		// No explicit block override: bundle 0 runs at the head block, so blockhash(head)
		// is the current block -> 0; bundles 1 and 2 advance to head+1 and head+2 via the
		// per-bundle advance, where blockhash(head) must resolve to the real head hash in
		// every advanced bundle, not just the first (#32175).
		head := backend.chain.CurrentBlock().Number.Uint64()
		if head != uint64(genBlocks) {
			t.Fatalf("test assumes head == genBlocks (%d), got %d", genBlocks, head)
		}
		headCall := []ethapi.TransactionArgs{{From: &accounts[0].addr, To: &headHashAddr}}
		bundles := []Bundle{{Transactions: headCall}, {Transactions: headCall}, {Transactions: headCall}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Bundle 0 is at the head block, so blockhash(head) is the current block -> 0.
		if got := topOfStack(t, res[0][0]); got != "0x0" {
			t.Fatalf("bundle 0 runs at head; blockhash(head) should be 0 (current block), got %s", got)
		}
		// Bundle 1 (head+1) must resolve blockhash(head) to the real head hash...
		headHash := topOfStack(t, res[1][0])
		if headHash == "0x0" {
			t.Fatalf("bundle 1 runs at head+1 via advance; blockhash(head) must resolve to the real head hash, got 0")
		}
		// ...and bundle 2 (head+2) must return the same real head hash, not drift to 0.
		if got := topOfStack(t, res[2][0]); got != headHash {
			t.Fatalf("bundle 2 runs at head+2 via advance; blockhash(head) should still be %s, got %s", headHash, got)
		}
	})

	t.Run("basefee is zeroed for zero-gas-price calls", func(t *testing.T) {
		// A call with no fee fields ends up with gasPrice 0, which lowers the
		// block context basefee to 0 so the BASEFEE opcode reads 0.
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{{From: &accounts[0].addr, To: &basefeeAddr}}}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := topOfStack(t, res[0][0]); got != "0x0" {
			t.Fatalf("BASEFEE should read 0 for a zero-gas-price call, got %s", got)
		}
	})

	t.Run("transaction index selects mid-block state", func(t *testing.T) {
		// In the 3-tx block, accounts[2] is funded by the tx at index 1. A spend
		// from accounts[2] above its genesis balance fails before that tx and
		// succeeds after it.
		blockNr := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(genBlocks - 1))
		spend := transfer(accounts[2].addr, accounts[0].addr, new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(100)))
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{spend}}}

		beforeIdx, afterIdx := 0, 2
		if _, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: blockNr, TransactionIndex: &beforeIdx}, nil); err == nil {
			t.Fatalf("tracing before the funding tx should fail with insufficient funds")
		}
		// Index 0 is a valid (non-negative) selector: a funded sender succeeds there.
		okAtZero := []Bundle{{Transactions: []ethapi.TransactionArgs{transfer(accounts[0].addr, accounts[1].addr, big.NewInt(1))}}}
		if _, err := api.TraceCallMany(t.Context(), okAtZero, StateContext{BlockNumber: blockNr, TransactionIndex: &beforeIdx}, nil); err != nil {
			t.Fatalf("tracing at index 0 with a funded sender should succeed, got: %v", err)
		}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: blockNr, TransactionIndex: &afterIdx}, nil)
		if err != nil {
			t.Fatalf("tracing after the funding tx should succeed, got: %v", err)
		}
		if got := mustResult(t, res[0][0]); got.Failed {
			t.Fatalf("call after funding tx unexpectedly failed")
		}
		// transactionIndex -1 is the "full block" sentinel: same as omitting it.
		fullBlock := -1
		if _, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: blockNr, TransactionIndex: &fullBlock}, nil); err != nil {
			t.Fatalf("tracing with index -1 (full block) should succeed, got: %v", err)
		}
	})

	t.Run("state override is applied to base state", func(t *testing.T) {
		// overrideAddr has no genesis balance; the override funds it so its spend succeeds.
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{
			transfer(overrideAddr, accounts[0].addr, new(big.Int).Add(big.NewInt(params.Ether), big.NewInt(7))),
		}}}
		cfg := &TraceCallConfig{StateOverrides: &override.StateOverride{
			overrideAddr: {Balance: (*hexutil.Big)(new(big.Int).Mul(big.NewInt(5), big.NewInt(params.Ether)))},
		}}
		if _, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, nil); err == nil {
			t.Fatalf("without the override the unfunded sender should fail")
		}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, cfg)
		if err != nil {
			t.Fatalf("with the balance override the call should succeed, got: %v", err)
		}
		if got := mustResult(t, res[0][0]); got.Failed {
			t.Fatalf("call unexpectedly failed despite balance override")
		}
	})

	t.Run("state override relocating a precompile is honored in execution", func(t *testing.T) {
		// Move the identity precompile (0x04) to a fresh address; the destination
		// must behave as the precompile (echo its input). This only works if the
		// override-mutated precompile set reaches traceTx, not a freshly recomputed one.
		identity := common.BytesToAddress([]byte{0x04})
		dest := common.HexToAddress("0x0000000000000000000000000000000000009999")
		input := hexutil.Bytes{0xde, 0xad, 0xbe, 0xef}
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{{
			From: &accounts[0].addr, To: &dest, Input: &input,
		}}}}
		cfg := &TraceCallConfig{StateOverrides: &override.StateOverride{
			identity: {MovePrecompileTo: &dest},
		}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := mustResult(t, res[0][0])
		if got.Failed {
			t.Fatalf("call to relocated precompile failed")
		}
		if got.ReturnValue.String() != input.String() {
			t.Fatalf("relocated identity precompile: want return %s, got %s", input.String(), got.ReturnValue.String())
		}
	})

	t.Run("state override on a precompile slot is honored in execution", func(t *testing.T) {
		// Overriding a precompile address with code frees the precompile slot, so the
		// code runs instead of the precompile. This exercises the precompile-set detection
		// for the slot-override case (not just MovePrecompileTo): the mutated set, where
		// 0x04 is no longer a precompile, must reach traceTx — otherwise 0x04 keeps
		// behaving as the identity precompile and echoes the input.
		identity := common.BytesToAddress([]byte{0x04})
		input := hexutil.Bytes{0xde, 0xad, 0xbe, 0xef}
		code := hexutil.Bytes{0x00} // STOP -> returns no data (identity would echo the input)
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{{
			From: &accounts[0].addr, To: &identity, Input: &input,
		}}}}
		cfg := &TraceCallConfig{StateOverrides: &override.StateOverride{
			identity: {Code: &code},
		}}
		res, err := api.TraceCallMany(t.Context(), bundles, StateContext{BlockNumber: latest}, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := mustResult(t, res[0][0])
		if got.Failed {
			t.Fatalf("call to overridden precompile slot failed")
		}
		if got.ReturnValue.String() == input.String() {
			t.Fatalf("0x04 still ran as the identity precompile; the slot override was not honored")
		}
	})

	t.Run("honors context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{
			transfer(accounts[0].addr, accounts[1].addr, big.NewInt(1)),
		}}}
		if _, err := api.TraceCallMany(ctx, bundles, StateContext{BlockNumber: latest}, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	})

	t.Run("errors", func(t *testing.T) {
		send := transfer(accounts[0].addr, accounts[1].addr, big.NewInt(1))
		withTx := []Bundle{{Transactions: []ethapi.TransactionArgs{send}}}
		slot := common.Hash{0x1}

		// Over-limit requests: empty TransactionArgs are fine since validateBundles
		// runs before any tracing, so nothing is executed.
		tooManyBundles := make([]Bundle, maxTraceCallManyBundles+1)
		for i := range tooManyBundles {
			tooManyBundles[i] = Bundle{Transactions: []ethapi.TransactionArgs{send}}
		}
		tooManyPerBundle := []Bundle{{Transactions: make([]ethapi.TransactionArgs, maxTraceCallManyCallsPerBundle+1)}}
		// Enough bundles, each at the per-bundle cap, to exceed the total cap while
		// staying under the bundle-count cap — so the total check is what trips.
		tooManyTotal := make([]Bundle, maxTraceCallManyTotalCalls/maxTraceCallManyCallsPerBundle+1)
		for i := range tooManyTotal {
			tooManyTotal[i] = Bundle{Transactions: make([]ethapi.TransactionArgs, maxTraceCallManyCallsPerBundle)}
		}

		tests := []struct {
			name    string
			bundles []Bundle
			sc      StateContext
			config  *TraceCallConfig
			wantErr string
		}{
			{"empty bundles", nil, StateContext{BlockNumber: latest}, nil, "empty bundles"},
			{"bundles without transactions", []Bundle{{}}, StateContext{BlockNumber: latest}, nil, "empty bundles"},
			{"too many bundles", tooManyBundles, StateContext{BlockNumber: latest}, nil, "too many bundles"},
			{"too many calls in a bundle", tooManyPerBundle, StateContext{BlockNumber: latest}, nil, "too many calls in a single bundle"},
			{"too many calls in total", tooManyTotal, StateContext{BlockNumber: latest}, nil, "too many calls across all bundles"},
			{"pending block", withTx, StateContext{BlockNumber: rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)}, nil, "tracing on top of pending is not supported"},
			{"unknown block", withTx, StateContext{BlockNumber: rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(genBlocks + 1))}, nil, fmt.Sprintf("block #%d not found", genBlocks+1)},
			{"no block or hash", withTx, StateContext{}, nil, "invalid arguments; neither block nor hash specified"},
			{"transaction index below -1", withTx, StateContext{BlockNumber: latest, TransactionIndex: func() *int { i := -2; return &i }()}, nil, "transaction index -2 out of range"},
			{
				"conflicting fee fields",
				[]Bundle{{Transactions: []ethapi.TransactionArgs{{
					From: &accounts[0].addr, To: &accounts[1].addr,
					GasPrice:     (*hexutil.Big)(big.NewInt(1)),
					MaxFeePerGas: (*hexutil.Big)(big.NewInt(1)),
				}}}},
				StateContext{BlockNumber: latest}, nil,
				"both gasPrice and (maxFeePerGas or maxPriorityFeePerGas) specified",
			},
			{
				"unsupported block override",
				[]Bundle{{Transactions: []ethapi.TransactionArgs{send}, BlockOverride: &override.BlockOverrides{BeaconRoot: &slot}}},
				StateContext{BlockNumber: latest}, nil, `block override "beaconRoot" is not supported`,
			},
			{
				"conflicting state override",
				withTx, StateContext{BlockNumber: latest},
				&TraceCallConfig{StateOverrides: &override.StateOverride{
					accounts[0].addr: {
						State:     map[common.Hash]common.Hash{{}: {}},
						StateDiff: map[common.Hash]common.Hash{{}: {}},
					},
				}},
				"has both 'state' and 'stateDiff'",
			},
			{
				"non-increasing block number across bundles",
				[]Bundle{
					{Transactions: []ethapi.TransactionArgs{send}, BlockOverride: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x500))}},
					{Transactions: []ethapi.TransactionArgs{send}, BlockOverride: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x100))}},
				},
				StateContext{BlockNumber: latest}, nil, "block numbers must be in order",
			},
			{
				"non-increasing block time across bundles",
				[]Bundle{
					{Transactions: []ethapi.TransactionArgs{send}, BlockOverride: &override.BlockOverrides{Time: func() *hexutil.Uint64 { t := hexutil.Uint64(0x9999); return &t }()}},
					{Transactions: []ethapi.TransactionArgs{send}, BlockOverride: &override.BlockOverrides{Time: func() *hexutil.Uint64 { t := hexutil.Uint64(0x10); return &t }()}},
				},
				StateContext{BlockNumber: latest}, nil, "block timestamps must be in order",
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := api.TraceCallMany(t.Context(), tc.bundles, tc.sc, tc.config)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
			})
		}
	})
}

func TestTraceTransaction(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(2)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	target := common.Hash{}
	signer := types.HomesteadSigner{}
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
		target = tx.Hash()
	})

	defer backend.teardown()
	api := NewAPI(backend)

	result, err := api.TraceTransaction(t.Context(), target, nil)
	if err != nil {
		t.Errorf("Failed to trace transaction %v", err)
	}

	var have *logger.ExecutionResult

	if err := json.Unmarshal(result.(json.RawMessage), &have); err != nil {
		t.Errorf("failed to unmarshal result %v", err)
	}

	if !reflect.DeepEqual(have, &logger.ExecutionResult{
		Gas:         params.TxGas,
		Failed:      false,
		ReturnValue: []byte{},
		StructLogs:  []json.RawMessage{},
	}) {
		t.Error("Transaction tracing result is different")
	}

	// Test non-existent transaction
	_, err = api.TraceTransaction(t.Context(), common.Hash{42}, nil)
	if !errors.Is(err, errTxNotFound) {
		t.Fatalf("want %v, have %v", errTxNotFound, err)
	}
}

// nolint:typecheck
func TestTraceBlock(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 10
	signer := types.HomesteadSigner{}
	var txHash common.Hash
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
		txHash = tx.Hash()
	})

	defer backend.teardown()
	api := NewAPI(backend)

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		config      *TraceConfig
		want        string
		expectErr   error
	}{
		// Trace genesis block, expect error
		{
			blockNumber: rpc.BlockNumber(0),
			expectErr:   errors.New("genesis is not traceable"),
		},
		// Trace head block
		{
			blockNumber: rpc.BlockNumber(genBlocks),
			want:        fmt.Sprintf(`[{"txHash":"%v","result":{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}}]`, txHash),
		},
		// Trace non-existent block
		{
			blockNumber: rpc.BlockNumber(genBlocks + 1),
			expectErr:   fmt.Errorf("block #%d not found", genBlocks+1),
		},
		// Trace latest block
		{
			blockNumber: rpc.LatestBlockNumber,
			want:        fmt.Sprintf(`[{"txHash":"%v","result":{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}}]`, txHash),
		},
		// Trace pending block
		{
			blockNumber: rpc.PendingBlockNumber,
			want:        fmt.Sprintf(`[{"txHash":"%v","result":{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}}]`, txHash),
		},
	}

	for i, tc := range testSuite {
		result, err := api.TraceBlockByNumber(t.Context(), tc.blockNumber, tc.config)
		if tc.expectErr != nil {
			if err == nil {
				t.Errorf("test %d, want error %v", i, tc.expectErr)
				continue
			}

			if !reflect.DeepEqual(err, tc.expectErr) {
				t.Errorf("test %d: error mismatch, want %v, get %v", i, tc.expectErr, err)
			}

			continue
		}

		if err != nil {
			t.Errorf("test %d, want no error, have %v", i, err)
			continue
		}

		have, _ := json.Marshal(result)
		want := tc.want

		if string(have) != want {
			t.Errorf("test %d, result mismatch, have\n%v\n, want\n%v\n", i, string(have), want)
		}
	}
}

// nolint:typecheck
func TestTracingWithOverrides(t *testing.T) {
	t.Parallel()
	// Initialize test accounts
	accounts := newAccounts(3)
	ecRecoverAddress := common.HexToAddress("0x0000000000000000000000000000000000000001")
	storageAccount := common.Address{0x13, 37}
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
			// An account with existing storage
			storageAccount: {
				Balance: new(big.Int),
				Storage: map[common.Hash]common.Hash{
					common.HexToHash("0x03"): common.HexToHash("0x33"),
					common.HexToHash("0x04"): common.HexToHash("0x44"),
				},
			},
		},
	}
	genBlocks := 10
	signer := types.HomesteadSigner{}
	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
	})
	defer backend.teardown()
	api := NewAPI(backend)
	randomAccounts := newAccounts(3)

	type res struct {
		Gas         int
		Failed      bool
		ReturnValue string
	}

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		call        ethapi.TransactionArgs
		config      *TraceCallConfig
		expectErr   error
		want        string
	}{
		// Call which can only succeed if state is state overridden
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{Balance: newRPCBalance(new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether)))},
				},
			},
			want: `{"gas":21000,"failed":false,"returnValue":"0x"}`,
		},
		// Invalid call without state overriding
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From:  &randomAccounts[0].addr,
				To:    &randomAccounts[1].addr,
				Value: (*hexutil.Big)(big.NewInt(1000)),
			},
			config:    &TraceCallConfig{},
			expectErr: core.ErrInsufficientFunds,
		},
		// Successful simple contract call
		//
		// // SPDX-License-Identifier: GPL-3.0
		//
		//  pragma solidity >=0.7.0 <0.8.0;
		//
		//  /**
		//   * @title Storage
		//   * @dev Store & retrieve value in a variable
		//   */
		//  contract Storage {
		//      uint256 public number;
		//      constructor() {
		//          number = block.number;
		//      }
		//  }
		{
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &randomAccounts[2].addr,
				Data: newRPCBytes(common.Hex2Bytes("8381f58a")), // call number()
			},
			config: &TraceCallConfig{
				//Tracer: &tracer,
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						Code:      newRPCBytes(common.Hex2Bytes("6080604052348015600f57600080fd5b506004361060285760003560e01c80638381f58a14602d575b600080fd5b60336049565b6040518082815260200191505060405180910390f35b6000548156fea2646970667358221220eab35ffa6ab2adfe380772a48b8ba78e82a1b820a18fcb6f59aa4efb20a5f60064736f6c63430007040033")),
						StateDiff: newStates([]common.Hash{{}}, []common.Hash{common.BigToHash(big.NewInt(123))}),
					},
				},
			},
			want: `{"gas":23347,"failed":false,"returnValue":"0x000000000000000000000000000000000000000000000000000000000000007b"}`,
		},
		{ // Override blocknumber
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &accounts[0].addr,
				// BLOCKNUMBER PUSH1 MSTORE
				Input: newRPCBytes(common.Hex2Bytes("4360005260206000f3")),
			},
			config: &TraceCallConfig{
				BlockOverrides: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			want: `{"gas":59537,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000001337"}`,
		},
		{ // Override blocknumber, and query a blockhash
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &accounts[0].addr,
				Input: &hexutil.Bytes{
					0x60, 0x00, 0x40, // BLOCKHASH(0)
					0x60, 0x00, 0x52, // STORE memory offset 0
					0x61, 0x13, 0x36, 0x40, // BLOCKHASH(0x1336)
					0x60, 0x20, 0x52, // STORE memory offset 32
					0x61, 0x13, 0x37, 0x40, // BLOCKHASH(0x1337)
					0x60, 0x40, 0x52, // STORE memory offset 64
					0x60, 0x60, 0x60, 0x00, 0xf3, // RETURN (0-96)

				}, // blocknumber
			},
			config: &TraceCallConfig{
				BlockOverrides: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(0x1337))},
			},
			want: `{"gas":72666,"failed":false,"returnValue":"0x000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"}`,
		},
		{ // Override blocknumber with block n+1 and query a blockhash (resolves issue #32175)
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &accounts[0].addr,
				Input: newRPCBytes([]byte{
					byte(vm.PUSH1), byte(genBlocks),
					byte(vm.BLOCKHASH),
					byte(vm.PUSH1), 0x00,
					byte(vm.MSTORE),
					byte(vm.PUSH1), 0x20,
					byte(vm.PUSH1), 0x00,
					byte(vm.RETURN),
				}),
			},
			config: &TraceCallConfig{
				BlockOverrides: &override.BlockOverrides{Number: (*hexutil.Big)(big.NewInt(int64(genBlocks + 1)))},
			},
			want: fmt.Sprintf(`{"gas":59590,"failed":false,"returnValue":"%s"}`, backend.chain.GetHeaderByNumber(uint64(genBlocks)).Hash().Hex()),
		},
		/*
			pragma solidity =0.8.12;

			contract Test {
			    uint private x;

			    function test2() external {
			        x = 1337;
			        revert();
			    }

			    function test() external returns (uint) {
			        x = 1;
			        try this.test2() {} catch (bytes memory) {}
			        return x;
			    }
			}
		*/
		{ // First with only code override, not storage override
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &randomAccounts[2].addr,
				Data: newRPCBytes(common.Hex2Bytes("f8a8fd6d")), //
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						Code: newRPCBytes(common.Hex2Bytes("6080604052348015600f57600080fd5b506004361060325760003560e01c806366e41cb7146037578063f8a8fd6d14603f575b600080fd5b603d6057565b005b60456062565b60405190815260200160405180910390f35b610539600090815580fd5b60006001600081905550306001600160a01b03166366e41cb76040518163ffffffff1660e01b8152600401600060405180830381600087803b15801560a657600080fd5b505af192505050801560b6575060015b60e9573d80801560e1576040519150601f19603f3d011682016040523d82523d6000602084013e60e6565b606091505b50505b506000549056fea26469706673582212205ce45de745a5308f713cb2f448589177ba5a442d1a2eff945afaa8915961b4d064736f6c634300080c0033")),
					},
				},
			},
			want: `{"gas":44100,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000000001"}`,
		},
		{ // Same again, this time with storage override
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &randomAccounts[2].addr,
				Data: newRPCBytes(common.Hex2Bytes("f8a8fd6d")), //
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					randomAccounts[2].addr: override.OverrideAccount{
						Code:  newRPCBytes(common.Hex2Bytes("6080604052348015600f57600080fd5b506004361060325760003560e01c806366e41cb7146037578063f8a8fd6d14603f575b600080fd5b603d6057565b005b60456062565b60405190815260200160405180910390f35b610539600090815580fd5b60006001600081905550306001600160a01b03166366e41cb76040518163ffffffff1660e01b8152600401600060405180830381600087803b15801560a657600080fd5b505af192505050801560b6575060015b60e9573d80801560e1576040519150601f19603f3d011682016040523d82523d6000602084013e60e6565b606091505b50505b506000549056fea26469706673582212205ce45de745a5308f713cb2f448589177ba5a442d1a2eff945afaa8915961b4d064736f6c634300080c0033")),
						State: newStates([]common.Hash{{}}, []common.Hash{{}}),
					},
				},
			},
			//want: `{"gas":46900,"failed":false,"returnValue":"0000000000000000000000000000000000000000000000000000000000000539"}`,
			want: `{"gas":44100,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000000001"}`,
		},
		{ // No state override
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &storageAccount,
				Data: newRPCBytes(common.Hex2Bytes("f8a8fd6d")), //
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					storageAccount: override.OverrideAccount{
						Code: newRPCBytes([]byte{
							// SLOAD(3) + SLOAD(4) (which is 0x77)
							byte(vm.PUSH1), 0x04,
							byte(vm.SLOAD),
							byte(vm.PUSH1), 0x03,
							byte(vm.SLOAD),
							byte(vm.ADD),
							// 0x77 -> MSTORE(0)
							byte(vm.PUSH1), 0x00,
							byte(vm.MSTORE),
							// RETURN (0, 32)
							byte(vm.PUSH1), 32,
							byte(vm.PUSH1), 00,
							byte(vm.RETURN),
						}),
					},
				},
			},
			want: `{"gas":25288,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000000077"}`,
		},
		{ // Full state override
			// The original storage is
			// 3: 0x33
			// 4: 0x44
			// With a full override, where we set 3:0x11, the slot 4 should be
			// removed. So SLOT(3)+SLOT(4) should be 0x11.
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &storageAccount,
				Data: newRPCBytes(common.Hex2Bytes("f8a8fd6d")), //
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					storageAccount: override.OverrideAccount{
						Code: newRPCBytes([]byte{
							// SLOAD(3) + SLOAD(4) (which is now 0x11 + 0x00)
							byte(vm.PUSH1), 0x04,
							byte(vm.SLOAD),
							byte(vm.PUSH1), 0x03,
							byte(vm.SLOAD),
							byte(vm.ADD),
							// 0x11 -> MSTORE(0)
							byte(vm.PUSH1), 0x00,
							byte(vm.MSTORE),
							// RETURN (0, 32)
							byte(vm.PUSH1), 32,
							byte(vm.PUSH1), 00,
							byte(vm.RETURN),
						}),
						State: newStates(
							[]common.Hash{common.HexToHash("0x03")},
							[]common.Hash{common.HexToHash("0x11")}),
					},
				},
			},
			want: `{"gas":25288,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000000011"}`,
		},
		{ // Partial state override
			// The original storage is
			// 3: 0x33
			// 4: 0x44
			// With a partial override, where we set 3:0x11, the slot 4 as before.
			// So SLOT(3)+SLOT(4) should be 0x55.
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &storageAccount,
				Data: newRPCBytes(common.Hex2Bytes("f8a8fd6d")), //
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					storageAccount: override.OverrideAccount{
						Code: newRPCBytes([]byte{
							// SLOAD(3) + SLOAD(4) (which is now 0x11 + 0x44)
							byte(vm.PUSH1), 0x04,
							byte(vm.SLOAD),
							byte(vm.PUSH1), 0x03,
							byte(vm.SLOAD),
							byte(vm.ADD),
							// 0x55 -> MSTORE(0)
							byte(vm.PUSH1), 0x00,
							byte(vm.MSTORE),
							// RETURN (0, 32)
							byte(vm.PUSH1), 32,
							byte(vm.PUSH1), 00,
							byte(vm.RETURN),
						}),
						StateDiff: map[common.Hash]common.Hash{
							common.HexToHash("0x03"): common.HexToHash("0x11"),
						},
					},
				},
			},
			want: `{"gas":25288,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000000055"}`,
		},
		{ // Call to precompile ECREC (0x01), but code was modified to add 1 to input
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &ecRecoverAddress,
				Data: newRPCBytes(common.Hex2Bytes("0000000000000000000000000000000000000000000000000000000000000001")),
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{
						Balance: newRPCBalance(new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether))),
					},
					ecRecoverAddress: override.OverrideAccount{
						// The code below adds one to input
						Code:             newRPCBytes(common.Hex2Bytes("60003560010160005260206000f3")),
						MovePrecompileTo: &randomAccounts[2].addr,
					},
				},
			},
			want: `{"gas":21167,"failed":false,"returnValue":"0x0000000000000000000000000000000000000000000000000000000000000002"}`,
		},
		{ // Call to ECREC Precompiled on a different address, expect the original behaviour of ECREC precompile
			blockNumber: rpc.LatestBlockNumber,
			call: ethapi.TransactionArgs{
				From: &randomAccounts[0].addr,
				To:   &randomAccounts[2].addr, // Moved EcRecover
				Data: newRPCBytes(common.Hex2Bytes("82f3df49d3645876de6313df2bbe9fbce593f21341a7b03acdb9423bc171fcc9000000000000000000000000000000000000000000000000000000000000001cba13918f50da910f2d55a7ea64cf716ba31dad91856f45908dde900530377d8a112d60f36900d18eb8f9d3b4f85a697b545085614509e3520e4b762e35d0d6bd")),
			},
			config: &TraceCallConfig{
				StateOverrides: &override.StateOverride{
					randomAccounts[0].addr: override.OverrideAccount{
						Balance: newRPCBalance(new(big.Int).Mul(big.NewInt(1), big.NewInt(params.Ether))),
					},
					ecRecoverAddress: override.OverrideAccount{
						// The code below adds one to input
						Code:             newRPCBytes(common.Hex2Bytes("60003560010160005260206000f3")),
						MovePrecompileTo: &randomAccounts[2].addr, // Move EcRecover to this address
					},
				},
			},
			want: `{"gas":25664,"failed":false,"returnValue":"0x000000000000000000000000c6e93f4c1920eaeaa1e699f76a7a8c18e3056074"}`,
		},
	}

	for i, tc := range testSuite {
		result, err := api.TraceCall(t.Context(), tc.call, rpc.BlockNumberOrHash{BlockNumber: &tc.blockNumber}, tc.config)
		if tc.expectErr != nil {
			if err == nil {
				t.Errorf("test %d: want error %v, have nothing", i, tc.expectErr)
				continue
			}

			if !errors.Is(err, tc.expectErr) {
				t.Errorf("test %d: error mismatch, want %v, have %v", i, tc.expectErr, err)
			}

			continue
		}

		if err != nil {
			t.Errorf("test %d: want no error, have %v", i, err)
			continue
		}
		// Turn result into res-struct
		var (
			have res
			want res
		)

		resBytes, _ := json.Marshal(result)
		json.Unmarshal(resBytes, &have)
		json.Unmarshal([]byte(tc.want), &want)

		if !reflect.DeepEqual(have, want) {
			t.Logf("result: %v\n", string(resBytes))
			t.Errorf("test %d, result mismatch, have\n%v\n, want\n%v\n", i, have, want)
		}
	}
}

type Account struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

func newAccounts(n int) (accounts []Account) {
	for i := 0; i < n; i++ {
		key, _ := crypto.GenerateKey()
		addr := crypto.PubkeyToAddress(key.PublicKey)
		accounts = append(accounts, Account{key: key, addr: addr})
	}
	slices.SortFunc(accounts, func(a, b Account) int { return a.addr.Cmp(b.addr) })
	return accounts
}

func newRPCBalance(balance *big.Int) *hexutil.Big {
	rpcBalance := (*hexutil.Big)(balance)
	return rpcBalance
}

func newRPCBytes(bytes []byte) *hexutil.Bytes {
	rpcBytes := hexutil.Bytes(bytes)
	return &rpcBytes
}

func newStates(keys []common.Hash, vals []common.Hash) map[common.Hash]common.Hash {
	if len(keys) != len(vals) {
		panic("invalid input")
	}

	m := make(map[common.Hash]common.Hash)
	for i := 0; i < len(keys); i++ {
		m[keys[i]] = vals[i]
	}
	return m
}

// nolint:typecheck
func TestTraceChain(t *testing.T) {
	t.Parallel()

	// Initialize test accounts
	accounts := newAccounts(3)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(params.Ether)},
			accounts[1].addr: {Balance: big.NewInt(params.Ether)},
			accounts[2].addr: {Balance: big.NewInt(params.Ether)},
		},
	}
	genBlocks := 50
	signer := types.HomesteadSigner{}

	var (
		ref   atomic.Uint32 // total refs has made
		rel   atomic.Uint32 // total rels has made
		nonce uint64
	)

	backend := newTestBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		// Transfer from account[0] to account[1]
		//    value: 1000 wei
		//    fee:   0 wei
		for j := 0; j < i+1; j++ {
			tx, _ := types.SignTx(types.NewTransaction(nonce, accounts[1].addr, big.NewInt(1000), params.TxGas, b.BaseFee(), nil), signer, accounts[0].key)
			b.AddTx(tx)

			nonce += 1
		}
	})
	defer backend.teardown()
	backend.refHook = func() { ref.Add(1) }
	backend.relHook = func() { rel.Add(1) }
	api := NewAPI(backend)

	single := `{"txHash":"0x0000000000000000000000000000000000000000000000000000000000000000","result":{"gas":21000,"failed":false,"returnValue":"0x","structLogs":[]}}`
	var cases = []struct {
		start  uint64
		end    uint64
		config *TraceConfig
	}{
		{0, 50, nil},  // the entire chain range, blocks [1, 50]
		{10, 20, nil}, // the middle chain range, blocks [11, 20]
	}

	for _, c := range cases {
		ref.Store(0)
		rel.Store(0)

		from, _ := api.blockByNumber(t.Context(), rpc.BlockNumber(c.start))
		to, _ := api.blockByNumber(t.Context(), rpc.BlockNumber(c.end))
		resCh := api.traceChain(from, to, c.config, nil)

		next := c.start + 1
		for result := range resCh {
			if have, want := uint64(result.Block), next; have != want {
				t.Fatalf("unexpected tracing block, have %d want %d", have, want)
			}
			if have, want := len(result.Traces), int(next); have != want {
				t.Fatalf("unexpected result length, have %d want %d", have, want)
			}

			for _, trace := range result.Traces {
				trace.TxHash = common.Hash{}
				blob, _ := json.Marshal(trace)
				if have, want := string(blob), single; have != want {
					t.Fatalf("unexpected tracing result, have\n%v\nwant:\n%v", have, want)
				}
			}

			next += 1
		}

		if next != c.end+1 {
			t.Error("Missing tracing block")
		}

		if nref, nrel := ref.Load(), rel.Load(); nref != nrel {
			t.Errorf("Ref and deref actions are not equal, ref %d rel %d", nref, nrel)
		}
	}
}

// newTestMergedBackend creates a post-merge chain
func newTestMergedBackend(t *testing.T, n int, gspec *core.Genesis, generator func(i int, b *core.BlockGen)) *testBackend {
	backend := &testBackend{
		chainConfig: gspec.Config,
		engine:      beacon.NewFaker(),
		chaindb:     rawdb.NewMemoryDatabase(),
	}
	// Generate blocks for testing
	_, blocks, _ := core.GenerateChainWithGenesis(gspec, backend.engine, n, generator)

	// Import the canonical chain
	options := &core.BlockChainConfig{
		TrieCleanLimit: 256,
		TrieDirtyLimit: 256,
		TrieTimeLimit:  5 * time.Minute,
		SnapshotLimit:  0,
		ArchiveMode:    true, // Archive mode
	}
	chain, err := core.NewBlockChain(backend.chaindb, gspec, backend.engine, options)
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}
	backend.chain = chain
	return backend
}

func TestTraceBlockWithBasefee(t *testing.T) {
	t.Parallel()
	accounts := newAccounts(1)
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	genesis := &core.Genesis{
		Config: params.AllDevChainProtocolChanges,
		Alloc: types.GenesisAlloc{
			accounts[0].addr: {Balance: big.NewInt(1 * params.Ether)},
			target: {Nonce: 1, Code: []byte{
				byte(vm.BASEFEE), byte(vm.STOP),
			}},
		},
	}
	genBlocks := 1
	signer := types.HomesteadSigner{}
	var txHash common.Hash
	var baseFee = new(big.Int)
	backend := newTestMergedBackend(t, genBlocks, genesis, func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &target,
			Value:    big.NewInt(0),
			Gas:      5 * params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
		txHash = tx.Hash()
		baseFee.Set(b.BaseFee())
	})
	defer backend.teardown()
	api := NewAPI(backend)

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		config      *TraceConfig
		want        string
	}{
		// Trace head block
		{
			blockNumber: rpc.BlockNumber(genBlocks),
			want:        fmt.Sprintf(`[{"txHash":"%#x","result":{"gas":21002,"failed":false,"returnValue":"0x","structLogs":[{"pc":0,"op":"BASEFEE","gas":84000,"gasCost":2,"depth":1,"stack":[]},{"pc":1,"op":"STOP","gas":83998,"gasCost":0,"depth":1,"stack":["%#x"]}]}}]`, txHash, baseFee),
		},
	}
	for i, tc := range testSuite {
		result, err := api.TraceBlockByNumber(t.Context(), tc.blockNumber, tc.config)
		if err != nil {
			t.Errorf("test %d, want no error, have %v", i, err)
			continue
		}
		have, _ := json.Marshal(result)
		want := tc.want
		if string(have) != want {
			t.Errorf("test %d, result mismatch\nhave: %v\nwant: %v\n", i, string(have), want)
		}
	}
}

func TestStandardTraceBlockToFile(t *testing.T) {
	var (
		// A sender who makes transactions, has some funds
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)

		// first contract the sender transacts with
		aa     = common.HexToAddress("0x7217d81b76bdd8707601e959454e3d776aee5f43")
		aaCode = []byte{byte(vm.PUSH1), 0x00, byte(vm.POP)}

		// second contract the sender transacts with
		bb     = common.HexToAddress("0x7217d81b76bdd8707601e959454e3d776aee5f44")
		bbCode = []byte{byte(vm.PUSH2), 0x00, 0x01, byte(vm.POP)}
	)

	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
			aa: {
				Code:    aaCode,
				Nonce:   1,
				Balance: big.NewInt(0),
			},
			bb: {
				Code:    bbCode,
				Nonce:   1,
				Balance: big.NewInt(0),
			},
		},
	}
	txHashs := make([]common.Hash, 0, 2)
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})
		// first tx to aa
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Value:    big.NewInt(0),
			Gas:      50000,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		txHashs = append(txHashs, tx.Hash())
		// second tx to bb
		tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    1,
			To:       &bb,
			Value:    big.NewInt(1),
			Gas:      100000,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		txHashs = append(txHashs, tx.Hash())
	})
	defer backend.teardown()

	var testSuite = []struct {
		blockNumber rpc.BlockNumber
		config      *StdTraceConfig
		want        []string
	}{
		{
			// test that all traces in the block were outputted if no trace config is specified
			blockNumber: rpc.LatestBlockNumber,
			config:      nil,
			want: []string{
				`{"pc":0,"op":96,"gas":"0x7148","gasCost":"0x3","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"PUSH1"}
{"pc":2,"op":80,"gas":"0x7145","gasCost":"0x2","memSize":0,"stack":["0x0"],"depth":1,"refund":0,"opName":"POP"}
{"pc":3,"op":0,"gas":"0x7143","gasCost":"0x0","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"STOP"}
{"output":"","gasUsed":"0x5"}
`,
				`{"pc":0,"op":97,"gas":"0x13498","gasCost":"0x3","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"PUSH2"}
{"pc":3,"op":80,"gas":"0x13495","gasCost":"0x2","memSize":0,"stack":["0x1"],"depth":1,"refund":0,"opName":"POP"}
{"pc":4,"op":0,"gas":"0x13493","gasCost":"0x0","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"STOP"}
{"output":"","gasUsed":"0x5"}
`,
			},
		},
		{
			// test that only a specific tx is traced if specified
			blockNumber: rpc.LatestBlockNumber,
			config:      &StdTraceConfig{TxHash: txHashs[1]},
			want: []string{
				`{"pc":0,"op":97,"gas":"0x13498","gasCost":"0x3","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"PUSH2"}
{"pc":3,"op":80,"gas":"0x13495","gasCost":"0x2","memSize":0,"stack":["0x1"],"depth":1,"refund":0,"opName":"POP"}
{"pc":4,"op":0,"gas":"0x13493","gasCost":"0x0","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"STOP"}
{"output":"","gasUsed":"0x5"}
`,
			},
		},
	}

	api := NewAPI(backend)
	for i, tc := range testSuite {
		block, _ := api.blockByNumber(t.Context(), tc.blockNumber)
		txTraces, err := api.StandardTraceBlockToFile(t.Context(), block.Hash(), tc.config)
		if err != nil {
			t.Fatalf("test index %d received error %v", i, err)
		}
		for j, traceFileName := range txTraces {
			traceReceived, err := os.ReadFile(traceFileName)
			if err != nil {
				t.Fatalf("could not read trace file: %v", err)
			}
			if tc.want[j] != string(traceReceived) {
				t.Fatalf("unexpected trace result.  expected\n'%s'\n\nreceived\n'%s'\n", tc.want[j], string(traceReceived))
			}
		}
	}
}

func TestTraceBadBlock(t *testing.T) {
	t.Parallel()

	var (
		accounts        = newAccounts(2)
		storageContract = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		signer          = types.HomesteadSigner{}
		txHashs         = make([]common.Hash, 0, 2)
		genesis         = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accounts[0].addr: {Balance: big.NewInt(params.Ether)},
				accounts[1].addr: {Balance: big.NewInt(params.Ether)},
				storageContract: {
					Nonce:   1,
					Balance: big.NewInt(0),
					Code: []byte{
						byte(vm.PUSH1), 0x2a, // push 42
						byte(vm.PUSH1), 0x00, // push slot 0
						byte(vm.SSTORE), // sstore(0, 42)
						byte(vm.STOP),
					},
				},
			},
		}
	)
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {
		// tx 0: plain transfer
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    0,
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
		txHashs = append(txHashs, tx.Hash())

		// tx 1: call storage contract (executes PUSH1, PUSH1, SSTORE, STOP)
		tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    1,
			To:       &storageContract,
			Value:    big.NewInt(0),
			Gas:      50000,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
		txHashs = append(txHashs, tx.Hash())
	})
	defer backend.teardown()

	// Write the block as a bad block so parent state is available
	block := backend.chain.GetBlockByNumber(1)
	rawdb.WriteBadBlock(backend.chaindb, block)

	api := NewAPI(backend)
	result, err := api.TraceBadBlock(context.Background(), block.Hash(), nil)
	if err != nil {
		t.Fatalf("want no error, have %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 tx traces, got %d", len(result))
	}

	// First tx: plain transfer
	have, _ := json.Marshal(result)
	var traces []struct {
		TxHash common.Hash `json:"txHash"`
		Result struct {
			Gas        uint64            `json:"gas"`
			Failed     bool              `json:"failed"`
			StructLogs []json.RawMessage `json:"structLogs"`
		} `json:"result"`
	}
	if err := json.Unmarshal(have, &traces); err != nil {
		t.Fatalf("failed to unmarshal traces: %v", err)
	}
	if traces[0].TxHash != txHashs[0] {
		t.Errorf("tx 0: hash mismatch, have %v, want %v", traces[0].TxHash, txHashs[0])
	}
	if traces[0].Result.Gas != params.TxGas {
		t.Errorf("tx 0: gas mismatch, have %d, want %d", traces[0].Result.Gas, params.TxGas)
	}
	if len(traces[0].Result.StructLogs) != 0 {
		t.Errorf("tx 0: expected empty structLogs for plain transfer, got %d entries", len(traces[0].Result.StructLogs))
	}

	// Second tx: contract call
	if traces[1].TxHash != txHashs[1] {
		t.Errorf("tx 1: hash mismatch, have %v, want %v", traces[1].TxHash, txHashs[1])
	}
	if traces[1].Result.Failed {
		t.Error("tx 1: expected success, got failed")
	}
	// Contract has 4 opcodes: PUSH1, PUSH1, SSTORE, STOP
	if len(traces[1].Result.StructLogs) != 4 {
		t.Errorf("tx 1: expected 4 structLog entries for contract call, got %d", len(traces[1].Result.StructLogs))
	}

	// Non-existent bad block
	_, err = api.TraceBadBlock(context.Background(), common.Hash{42}, nil)
	if err == nil {
		t.Fatal("want error for non-existent bad block, have none")
	}
	wantErr := fmt.Sprintf("bad block %#x not found", common.Hash{42})
	if err.Error() != wantErr {
		t.Errorf("error mismatch, want '%s', have '%v'", wantErr, err)
	}
}

func TestIntermediateRoots(t *testing.T) {
	t.Parallel()

	// Initialize test accounts and a contract that writes to storage.
	var (
		accounts        = newAccounts(2)
		storageContract = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		signer          = types.HomesteadSigner{}
		genesis         = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc: types.GenesisAlloc{
				accounts[0].addr: {Balance: big.NewInt(params.Ether)},
				accounts[1].addr: {Balance: big.NewInt(params.Ether)},
				// Contract: SSTORE(CALLVALUE, CALLVALUE)
				storageContract: {
					Nonce:   1,
					Balance: big.NewInt(0),
					Code: []byte{
						byte(vm.CALLVALUE),
						byte(vm.CALLVALUE),
						byte(vm.SSTORE),
						byte(vm.STOP),
					},
				},
			},
		}
	)
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {
		// tx 0: plain transfer
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    0,
			To:       &accounts[1].addr,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)

		// tx 1: sstore(1, 1)
		tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    1,
			To:       &storageContract,
			Value:    big.NewInt(1),
			Gas:      50000,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)

		// tx 2: sstore(2, 2)
		tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    2,
			To:       &storageContract,
			Value:    big.NewInt(2),
			Gas:      50000,
			GasPrice: b.BaseFee(),
			Data:     nil}),
			signer, accounts[0].key)
		b.AddTx(tx)
	})
	defer backend.teardown()

	api := NewAPI(backend)
	block := backend.chain.GetBlockByNumber(1)

	// Should return one root per tx
	roots, err := api.IntermediateRoots(context.Background(), block.Hash(), nil)
	if err != nil {
		t.Fatalf("want no error, have %v", err)
	}
	if len(roots) != 3 {
		t.Fatalf("root count mismatch, have %d, want 3", len(roots))
	}
	for i, root := range roots {
		if root == (common.Hash{}) {
			t.Errorf("root[%d] should not be zero", i)
		}
	}
	if roots[0] == roots[1] {
		t.Error("root[0] and root[1] should differ (transfer vs sstore)")
	}
	if roots[1] == roots[2] {
		t.Error("root[1] and root[2] should differ (sstore to different slots)")
	}

	// Intermediate roots of a bad block
	rawdb.WriteBadBlock(backend.chaindb, block)
	badRoots, err := api.IntermediateRoots(context.Background(), block.Hash(), nil)
	if err != nil {
		t.Fatalf("want no error for bad block fallback, have %v", err)
	}
	if !reflect.DeepEqual(roots, badRoots) {
		t.Errorf("bad block roots mismatch, have %v, want %v", badRoots, roots)
	}

	// Genesis block: should return error
	genesisBlock := backend.chain.GetBlockByNumber(0)
	_, err = api.IntermediateRoots(context.Background(), genesisBlock.Hash(), nil)
	if err == nil || err.Error() != "genesis is not traceable" {
		t.Fatalf("want 'genesis is not traceable' error, have %v", err)
	}

	// Non-existent block: should return error
	_, err = api.IntermediateRoots(context.Background(), common.Hash{42}, nil)
	if err == nil {
		t.Fatal("want error for non-existent block, have none")
	}
	wantErr := fmt.Sprintf("block %#x not found", common.Hash{42})
	if err.Error() != wantErr {
		t.Errorf("error mismatch, want '%s', have '%v'", wantErr, err)
	}
}

func TestStandardTraceBadBlockToFile(t *testing.T) {
	var (
		key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		address = crypto.PubkeyToAddress(key.PublicKey)
		funds   = big.NewInt(1000000000000000)

		aa     = common.HexToAddress("0x7217d81b76bdd8707601e959454e3d776aee5f43")
		aaCode = []byte{byte(vm.PUSH1), 0x00, byte(vm.POP)}

		bb     = common.HexToAddress("0x7217d81b76bdd8707601e959454e3d776aee5f44")
		bbCode = []byte{byte(vm.PUSH2), 0x00, 0x01, byte(vm.POP)}
	)

	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc: types.GenesisAlloc{
			address: {Balance: funds},
			aa: {
				Code:    aaCode,
				Nonce:   1,
				Balance: big.NewInt(0),
			},
			bb: {
				Code:    bbCode,
				Nonce:   1,
				Balance: big.NewInt(0),
			},
		},
	}
	txHashs := make([]common.Hash, 0, 2)
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Value:    big.NewInt(0),
			Gas:      50000,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		txHashs = append(txHashs, tx.Hash())

		tx, _ = types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    1,
			To:       &bb,
			Value:    big.NewInt(1),
			Gas:      100000,
			GasPrice: b.BaseFee(),
			Data:     nil,
		}), types.HomesteadSigner{}, key)
		b.AddTx(tx)
		txHashs = append(txHashs, tx.Hash())
	})
	defer backend.teardown()

	// Write the block as a bad block
	block := backend.chain.GetBlockByNumber(1)
	rawdb.WriteBadBlock(backend.chaindb, block)

	var testSuite = []struct {
		config *StdTraceConfig
		want   []string
	}{
		{
			// All txs traced
			config: nil,
			want: []string{
				`{"pc":0,"op":96,"gas":"0x7148","gasCost":"0x3","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"PUSH1"}
{"pc":2,"op":80,"gas":"0x7145","gasCost":"0x2","memSize":0,"stack":["0x0"],"depth":1,"refund":0,"opName":"POP"}
{"pc":3,"op":0,"gas":"0x7143","gasCost":"0x0","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"STOP"}
{"output":"","gasUsed":"0x5"}
`,
				`{"pc":0,"op":97,"gas":"0x13498","gasCost":"0x3","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"PUSH2"}
{"pc":3,"op":80,"gas":"0x13495","gasCost":"0x2","memSize":0,"stack":["0x1"],"depth":1,"refund":0,"opName":"POP"}
{"pc":4,"op":0,"gas":"0x13493","gasCost":"0x0","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"STOP"}
{"output":"","gasUsed":"0x5"}
`,
			},
		},
		{
			// Specific tx traced
			config: &StdTraceConfig{TxHash: txHashs[1]},
			want: []string{
				`{"pc":0,"op":97,"gas":"0x13498","gasCost":"0x3","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"PUSH2"}
{"pc":3,"op":80,"gas":"0x13495","gasCost":"0x2","memSize":0,"stack":["0x1"],"depth":1,"refund":0,"opName":"POP"}
{"pc":4,"op":0,"gas":"0x13493","gasCost":"0x0","memSize":0,"stack":[],"depth":1,"refund":0,"opName":"STOP"}
{"output":"","gasUsed":"0x5"}
`,
			},
		},
	}

	api := NewAPI(backend)
	for i, tc := range testSuite {
		txTraces, err := api.StandardTraceBadBlockToFile(context.Background(), block.Hash(), tc.config)
		if err != nil {
			t.Fatalf("test %d: unexpected error %v", i, err)
		}
		if len(txTraces) != len(tc.want) {
			t.Fatalf("test %d: file count mismatch, have %d, want %d", i, len(txTraces), len(tc.want))
		}
		for j, traceFileName := range txTraces {
			defer os.Remove(traceFileName)
			traceReceived, err := os.ReadFile(traceFileName)
			if err != nil {
				t.Fatalf("test %d: could not read trace file: %v", i, err)
			}
			if tc.want[j] != string(traceReceived) {
				t.Fatalf("test %d, trace %d: result mismatch\nhave:\n%s\nwant:\n%s", i, j, string(traceReceived), tc.want[j])
			}
		}
	}

	// Non-existent bad block
	_, err := api.StandardTraceBadBlockToFile(context.Background(), common.Hash{42}, nil)
	if err == nil {
		t.Fatal("want error for non-existent bad block, have none")
	}
}

func TestTraceBlockParity(t *testing.T) {
	t.Parallel()

	const genBlocks = 5
	traceAPI, _, _ := newTransferChainAPI(t, genBlocks)

	var testSuite = []struct {
		name        string
		blockNumber rpc.BlockNumber
		expectErr   bool
	}{
		{
			name:        "genesis block should error",
			blockNumber: rpc.BlockNumber(0),
			expectErr:   true,
		},
		{
			name:        "non-existent block should error",
			blockNumber: rpc.BlockNumber(genBlocks + 1),
			expectErr:   true,
		},
		{
			name:        "latest block returns transaction traces",
			blockNumber: rpc.LatestBlockNumber,
		},
	}

	for _, tc := range testSuite {
		t.Run(tc.name, func(t *testing.T) {
			traces, err := traceAPI.Block(context.Background(), tc.blockNumber)
			if tc.expectErr {
				if err == nil {
					t.Errorf("expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if len(traces) == 0 {
					t.Error("expected at least one transaction trace")
				}
			}
		})
	}
}

// TestConvertCallFrameToParityTraces tests the conversion logic.
var (
	parityTestTxHash    = common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	parityTestBlockHash = common.HexToHash("0xabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd")
)

func parityTestCtx(intrinsicGas, rootGasUsed uint64, precompiles map[common.Address]struct{}) parityFrameCtx {
	return parityFrameCtx{
		txHash:       parityTestTxHash,
		blockHash:    parityTestBlockHash,
		blockNumber:  100,
		intrinsicGas: intrinsicGas,
		rootGasUsed:  rootGasUsed,
		precompiles:  precompiles,
	}
}

func mustConvertFrame(t *testing.T, frame map[string]interface{}, ctx parityFrameCtx) []*ParityTrace {
	t.Helper()
	traces, err := convertCallFrameToParityTraces(frame, []uint64{}, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return traces
}

func TestConvertParityTrace_SimpleCall(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "CALL", "from": "0x0000000000000000000000000000000000000001",
		"to":  "0x0000000000000000000000000000000000000002",
		"gas": "0x5208", "gasUsed": "0x5208", "input": "0x", "output": "0x", "value": "0x3e8",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, nil))
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(traces))
	}
	tr := traces[0]
	if tr.Type != "call" {
		t.Errorf("expected type 'call', got %s", tr.Type)
	}
	if tr.Action == nil || tr.Action.CallType == nil || *tr.Action.CallType != "call" {
		t.Error("callType mismatch")
	}
	if tr.Subtraces != 0 {
		t.Errorf("expected 0 subtraces, got %d", tr.Subtraces)
	}
}

func TestConvertParityTrace_CreateOperation(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "CREATE", "from": "0x0000000000000000000000000000000000000001",
		"to":  "0x0000000000000000000000000000000000000003",
		"gas": "0x30000", "gasUsed": "0x20000",
		"input": "0x6060604052", "output": "0x6080604052", "value": "0x0",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, nil))
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(traces))
	}
	tr := traces[0]
	if tr.Type != "create" {
		t.Errorf("expected type 'create', got %s", tr.Type)
	}
	if tr.Action == nil || tr.Action.Init == nil {
		t.Error("init field should be set for create")
	}
	if tr.Result == nil || tr.Result.Code == nil {
		t.Error("code field should be set for create result")
	}
	if tr.Action.To != nil {
		t.Errorf("create action must not have 'to', got %s", tr.Action.To)
	}
	if tr.Action.CreationMethod == nil || *tr.Action.CreationMethod != "create" {
		t.Errorf("create action creationMethod should be 'create', got %v", tr.Action.CreationMethod)
	}
}

func TestConvertParityTrace_PlainTransferZeroGrossGas(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "CALL", "from": "0xaa00000000000000000000000000000000000001",
		"to":  "0xbb00000000000000000000000000000000000002",
		"gas": "0x5208", "gasUsed": "0x5208", "input": "0x", "output": "0x", "value": "0x1",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0x5208, 0, nil))
	if traces[0].Result == nil || traces[0].Result.GasUsed == nil || uint64(*traces[0].Result.GasUsed) != 0 {
		t.Errorf("plain transfer gross gasUsed should be 0, got %v", traces[0].Result.GasUsed)
	}
}

func TestConvertParityTrace_FailedCallNonRevert(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "CALL", "from": "0x0000000000000000000000000000000000000001",
		"to":  "0x0000000000000000000000000000000000000002",
		"gas": "0x5208", "gasUsed": "0x5208", "input": "0x", "output": "0x", "value": "0x3e8",
		"error": "out of gas",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, nil))
	if len(traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(traces))
	}
	tr := traces[0]
	if tr.Error == nil || *tr.Error != "out of gas" {
		t.Errorf("error should be raw 'out of gas', got %v", tr.Error)
	}
	if tr.Result != nil {
		t.Errorf("result must be nil for non-revert errors, got %+v", tr.Result)
	}
}

func TestConvertParityTrace_NestedCalls(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "CALL", "from": "0x0000000000000000000000000000000000000001",
		"to":  "0x0000000000000000000000000000000000000002",
		"gas": "0x10000", "gasUsed": "0x8000", "input": "0x", "output": "0x", "value": "0x0",
		"calls": []interface{}{
			map[string]interface{}{
				"type": "CALL", "from": "0x0000000000000000000000000000000000000002",
				"to":  "0x00000000000000000000000000000000000000ff",
				"gas": "0x5000", "gasUsed": "0x3000", "input": "0x", "output": "0x", "value": "0x0",
			},
		},
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, nil))
	if len(traces) != 2 {
		t.Fatalf("expected 2 traces (parent + child), got %d", len(traces))
	}
	if traces[0].Subtraces != 1 {
		t.Errorf("expected 1 subtrace for parent, got %d", traces[0].Subtraces)
	}
	if len(traces[0].TraceAddress) != 0 {
		t.Error("parent should have empty traceAddress")
	}
	if len(traces[1].TraceAddress) != 1 || traces[1].TraceAddress[0] != 0 {
		t.Errorf("child should have traceAddress [0], got %v", traces[1].TraceAddress)
	}
}

func TestConvertParityTrace_PrecompileChildOmitted(t *testing.T) {
	t.Parallel()
	precompiles := map[common.Address]struct{}{
		common.HexToAddress("0x0000000000000000000000000000000000000001"): {},
	}
	frame := map[string]interface{}{
		"type": "CALL", "from": "0xaa00000000000000000000000000000000000001",
		"to":  "0xbb00000000000000000000000000000000000002",
		"gas": "0x10000", "gasUsed": "0x8000", "input": "0x", "output": "0x", "value": "0x0",
		"calls": []interface{}{
			map[string]interface{}{
				"type": "STATICCALL", "from": "0xbb00000000000000000000000000000000000002",
				"to":  "0x0000000000000000000000000000000000000001",
				"gas": "0x1000", "gasUsed": "0xbb8", "input": "0x", "output": "0x",
			},
			map[string]interface{}{
				"type": "CALL", "from": "0xbb00000000000000000000000000000000000002",
				"to":  "0xcc00000000000000000000000000000000000003",
				"gas": "0x2000", "gasUsed": "0x1000", "input": "0x", "output": "0x", "value": "0x0",
			},
		},
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, precompiles))
	if len(traces) != 2 {
		t.Fatalf("expected 2 traces (precompile filtered), got %d", len(traces))
	}
	if traces[0].Subtraces != 1 {
		t.Errorf("expected 1 subtrace after filtering precompile, got %d", traces[0].Subtraces)
	}
	if traces[1].Action == nil || traces[1].Action.To == nil ||
		*traces[1].Action.To != common.HexToAddress("0xcc00000000000000000000000000000000000003") {
		t.Errorf("kept child should be the non-precompile call, got %+v", traces[1].Action)
	}
	if len(traces[1].TraceAddress) != 1 || traces[1].TraceAddress[0] != 0 {
		t.Errorf("kept child should reindex to [0], got %v", traces[1].TraceAddress)
	}
}

func TestConvertParityTrace_RevertedCall(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "CALL", "from": "0xaa00000000000000000000000000000000000001",
		"to":  "0xbb00000000000000000000000000000000000002",
		"gas": "0x5208", "gasUsed": "0x2e", "input": "0x", "output": "0x", "value": "0x0",
		"error": "execution reverted",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0x2e, nil))
	tr := traces[0]
	if tr.Error == nil || *tr.Error != "Reverted" {
		t.Errorf("expected error 'Reverted', got %v", tr.Error)
	}
	if tr.Result == nil || tr.Result.GasUsed == nil || uint64(*tr.Result.GasUsed) != 0x2e {
		t.Errorf("expected result.gasUsed 0x2e on revert, got %+v", tr.Result)
	}
	if tr.Result.Output == nil || len(*tr.Result.Output) != 0 {
		t.Errorf("expected empty output (0x) on revert, got %v", tr.Result.Output)
	}
}

func TestConvertParityTrace_StaticcallValue(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "STATICCALL", "from": "0xaa00000000000000000000000000000000000001",
		"to":  "0xbb00000000000000000000000000000000000002",
		"gas": "0x5208", "gasUsed": "0x100", "input": "0x", "output": "0x",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, nil))
	tr := traces[0]
	if tr.Action == nil || tr.Action.Value == nil || tr.Action.Value.ToInt().Sign() != 0 {
		t.Errorf("staticcall action.value should be 0x0, got %v", tr.Action.Value)
	}
	if tr.Result == nil || tr.Result.Output == nil {
		t.Errorf("empty output should serialize as 0x (non-nil), got %+v", tr.Result)
	}
}

func TestConvertParityTrace_SuicideActionShape(t *testing.T) {
	t.Parallel()
	frame := map[string]interface{}{
		"type": "SELFDESTRUCT", "from": "0xaa00000000000000000000000000000000000001",
		"to":    "0xbb00000000000000000000000000000000000002",
		"value": "0x7e9", "gas": "0x0", "gasUsed": "0x0",
	}
	traces := mustConvertFrame(t, frame, parityTestCtx(0, 0, nil))
	tr := traces[0]
	if tr.Type != "suicide" {
		t.Errorf("expected type 'suicide', got %q", tr.Type)
	}
	a := tr.Action
	if a.Address == nil || *a.Address != common.HexToAddress("0xaa00000000000000000000000000000000000001") {
		t.Errorf("suicide address mismatch: %v", a.Address)
	}
	if a.RefundAddress == nil || *a.RefundAddress != common.HexToAddress("0xbb00000000000000000000000000000000000002") {
		t.Errorf("suicide refundAddress mismatch: %v", a.RefundAddress)
	}
	if a.Balance == nil || a.Balance.ToInt().Int64() != 0x7e9 {
		t.Errorf("suicide balance mismatch: %v", a.Balance)
	}
	if a.From != nil || a.To != nil || a.Gas != nil || a.CallType != nil || a.Input != nil {
		t.Errorf("suicide action must not carry call fields: %+v", a)
	}
	if tr.Result != nil {
		t.Errorf("suicide must have no result, got %+v", tr.Result)
	}
}

// assertRPCMethodRegistered calls method on a client backed by the given API
// set and fails if the method is not registered. Execution errors (e.g.
// "genesis is not traceable") are acceptable — they prove the method exists.
func assertRPCMethodRegistered(t *testing.T, apisFor func(Backend) []rpc.API, method string) {
	t.Helper()

	server := newRegisteredRPCServer(t, apisFor)
	client := rpc.DialInProc(server)
	defer client.Close()

	var result interface{}
	if err := client.Call(&result, method, "0x1"); err != nil {
		if strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "not available") {
			t.Fatalf("%s method not registered: %v", method, err)
		}
		t.Logf("%s returned expected error: %v", method, err)
	}
}

// withTraceAPIs returns the default debug APIs plus the opt-in trace namespace.
func withTraceAPIs(b Backend) []rpc.API {
	return append(APIs(b), TraceAPIs(b)...)
}

// TestTraceBlockRPCRegistration tests that the trace_block RPC method is properly
// registered. The trace namespace lives behind TraceAPIs (opt-in via
// rpc.enabletrace); register it explicitly here.
func TestTraceBlockRPCRegistration(t *testing.T) {
	t.Parallel()
	assertRPCMethodRegistered(t, withTraceAPIs, "trace_block")
}

// TestDebugTraceBlockParityNotRegistered pins the gating contract: Parity block
// tracing is NOT reachable through the always-on debug namespace; it lives only
// behind the opt-in trace namespace (trace_block).
func TestDebugTraceBlockParityNotRegistered(t *testing.T) {
	t.Parallel()

	server := newRegisteredRPCServer(t, APIs)
	client := rpc.DialInProc(server)
	defer client.Close()

	var result interface{}
	err := client.Call(&result, "debug_traceBlockParity", "0x1")
	if err == nil || !(strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not available")) {
		t.Fatalf("debug_traceBlockParity must not be registered, got err=%v result=%v", err, result)
	}
}

// TestTraceNamespaceOptIn asserts the opt-in contract: without TraceAPIs the
// trace namespace is absent (method-not-found); with TraceAPIs it is present.
func TestTraceNamespaceOptIn(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		withTrace bool
	}{
		{"disabled: trace_block not found", false},
		{"enabled: trace_block present", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			apisFor := APIs
			if tc.withTrace {
				apisFor = withTraceAPIs
			}
			server := newRegisteredRPCServer(t, apisFor)
			client := rpc.DialInProc(server)
			defer client.Close()

			var result interface{}
			err := client.Call(&result, "trace_block", "0x1")
			notFound := err != nil && (strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not available"))

			if !tc.withTrace && !notFound {
				t.Errorf("trace disabled: expected method-not-found error, got err=%v result=%v", err, result)
			}
			if tc.withTrace && notFound {
				t.Errorf("trace enabled: expected method to be registered, got: %v", err)
			}
		})
	}
}
