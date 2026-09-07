package core

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// TestV2_PreExecSystemCallVisibleToTx pins a regression where V2 BlockSTM
// failed every Cancun spec test that exercised EIP-4788's user-facing read
// path of the BeaconRoots system contract.
//
// Bug: V2 used to let SafeBase serve storage directly from the trie reader's
// cache. If that cache was warmed before applyV2PreExecSystemCalls wrote the
// EIP-4788 timestamp/root pair, workers could read the pre-write zero instead
// of StateDB's pending storage. BeaconRoots' user-call path then reverted and
// the block's gas/state diverged from the spec.
//
// Fix: SafeBase storage misses go through StateDB.GetState, which owns pending
// writes, FlatDiff overlays, and trie/cache fallback semantics.
func TestV2_PreExecSystemCallVisibleToTx(t *testing.T) {
	t.Run("Serial", func(t *testing.T) { runEIP4788Roundtrip(t, false) })
	t.Run("V2", func(t *testing.T) { runEIP4788Roundtrip(t, true) })
}

func runEIP4788Roundtrip(t *testing.T, useV2 bool) {
	// Caller bytecode lifted from the failing spec fixture
	// cancun/eip4788_beacon_root/test_beacon_root_contract_calls.json:
	// CALLDATACOPY input → CALL BeaconRoots(input, 32) → store the call's
	// success flag in slot 0, returndata word in slot 1, returndatasize in
	// slot 2, returndata copy in slot 3.
	callerCode := common.FromHex("366000602037602060003660206000720f3df6d732807ef1319fb7b8bb8522d0beac02620186a0f16000556000516001553d6002553d600060003e600051600355")
	callerAddr := common.HexToAddress("0x0b8ca5086109677d9f1c2381f33ec418f0d933c3")

	cfg := *params.MergedTestChainConfig
	cfg.Bor = nil // exercise the Ethereum-spec path; nothing here uses Bor.

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))

	sdb.SetCode(params.BeaconRootsAddress, params.BeaconRootsCode, tracing.CodeChangeUnspecified)
	sdb.SetNonce(params.BeaconRootsAddress, 1, tracing.NonceChangeUnspecified)
	sdb.SetCode(callerAddr, callerCode, tracing.CodeChangeUnspecified)
	sdb.SetNonce(callerAddr, 1, tracing.NonceChangeUnspecified)
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)

	statedb, _ := state.New(root, state.NewDatabase(tdb, nil))

	beaconRoot := common.HexToHash("0x6c31fc15422ebad28aaf9089c306702f67540b53c7eea8b7d2941044b027100f")
	timestamp := uint64(12)

	signer := types.NewLondonSigner(cfg.ChainID)
	calldata := common.LeftPadBytes(big.NewInt(int64(timestamp)).Bytes(), 32)
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(7),
		Gas:       1_000_000,
		To:        &callerAddr,
		Value:     big.NewInt(0),
		Data:      calldata,
	}), signer, key)

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30_000_000,
		BlockNumber: big.NewInt(1),
		Time:        timestamp,
		BaseFee:     big.NewInt(7),
		Random:      &common.Hash{},
	}

	if !useV2 {
		evm := vm.NewEVM(blockCtx, statedb, &cfg, vm.Config{})
		ProcessBeaconBlockRoot(beaconRoot, evm)
		msg, _ := TransactionToMessage(tx, signer, blockCtx.BaseFee)
		evm.SetTxContext(NewEVMTxContext(msg))
		gp := new(GasPool).AddGas(blockCtx.GasLimit)
		if _, err := ApplyMessage(evm, msg, gp); err != nil {
			t.Fatalf("ApplyMessage: %v", err)
		}
	} else {
		// Re-create the production V2 setup: read BeaconRoots[12] before the
		// system call writes to it, then run applyV2PreExecSystemCalls, then
		// ExecuteV2BlockSTM. Workers must observe StateDB's post-system-call
		// pending storage, not the earlier committed-state read.
		_ = statedb.GetState(params.BeaconRootsAddress, common.BigToHash(new(big.Int).SetUint64(timestamp%8191)))
		body := &types.Body{Transactions: types.Transactions{tx}}
		header := &types.Header{
			Number:           big.NewInt(1),
			Time:             timestamp,
			GasLimit:         blockCtx.GasLimit,
			ParentBeaconRoot: &beaconRoot,
			BaseFee:          blockCtx.BaseFee,
		}
		block := types.NewBlockWithHeader(header).WithBody(*body)
		applyV2PreExecSystemCalls(block, statedb, &cfg, vm.Config{}, blockCtx)

		msg, _ := TransactionToMessage(tx, signer, blockCtx.BaseFee)
		tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

		readBase := statedb.Copy()
		readBase.EnableConcurrentReads()
		store := blockstm.NewMVStore()
		bals := blockstm.NewMVBalanceStore()
		_ = ExecuteV2BlockSTM(context.Background(), tasks, readBase, store, bals, blockCtx, block.Hash(), vm.Config{}, &cfg,
			blockCtx.GasLimit, 1, statedb, nil)
	}

	want := map[uint64]common.Hash{
		0: common.BigToHash(big.NewInt(1)),
		1: beaconRoot,
		2: common.BigToHash(big.NewInt(0x20)),
		3: beaconRoot,
	}
	for slot, expected := range want {
		got := statedb.GetState(callerAddr, common.BigToHash(new(big.Int).SetUint64(slot)))
		if got != expected {
			t.Errorf("caller storage[%d]: got %s, want %s", slot, got.Hex(), expected.Hex())
		}
	}
}
