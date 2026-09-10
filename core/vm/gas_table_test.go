// Copyright 2017 The go-ethereum Authors
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

package vm

import (
	"bytes"
	"errors"
	"math"
	"math/big"
	"sort"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func TestMemoryGasCost(t *testing.T) {
	tests := []struct {
		size     uint64
		cost     uint64
		overflow bool
	}{
		{0x1fffffffe0, 36028809887088637, false},
		{0x1fffffffe1, 0, true},
	}
	for i, tt := range tests {
		v, err := memoryGasCost(&Memory{}, tt.size)
		if (err == ErrGasUintOverflow) != tt.overflow {
			t.Errorf("test %d: overflow mismatch: have %v, want %v", i, err == ErrGasUintOverflow, tt.overflow)
		}

		if v != tt.cost {
			t.Errorf("test %d: gas cost mismatch: have %v, want %v", i, v, tt.cost)
		}
	}
}

var eip2200Tests = []struct {
	original byte
	gaspool  uint64
	input    string
	used     uint64
	refund   uint64
	failure  error
}{
	{0, math.MaxUint64, "0x60006000556000600055", 1612, 0, nil},                // 0 -> 0 -> 0
	{0, math.MaxUint64, "0x60006000556001600055", 20812, 0, nil},               // 0 -> 0 -> 1
	{0, math.MaxUint64, "0x60016000556000600055", 20812, 19200, nil},           // 0 -> 1 -> 0
	{0, math.MaxUint64, "0x60016000556002600055", 20812, 0, nil},               // 0 -> 1 -> 2
	{0, math.MaxUint64, "0x60016000556001600055", 20812, 0, nil},               // 0 -> 1 -> 1
	{1, math.MaxUint64, "0x60006000556000600055", 5812, 15000, nil},            // 1 -> 0 -> 0
	{1, math.MaxUint64, "0x60006000556001600055", 5812, 4200, nil},             // 1 -> 0 -> 1
	{1, math.MaxUint64, "0x60006000556002600055", 5812, 0, nil},                // 1 -> 0 -> 2
	{1, math.MaxUint64, "0x60026000556000600055", 5812, 15000, nil},            // 1 -> 2 -> 0
	{1, math.MaxUint64, "0x60026000556003600055", 5812, 0, nil},                // 1 -> 2 -> 3
	{1, math.MaxUint64, "0x60026000556001600055", 5812, 4200, nil},             // 1 -> 2 -> 1
	{1, math.MaxUint64, "0x60026000556002600055", 5812, 0, nil},                // 1 -> 2 -> 2
	{1, math.MaxUint64, "0x60016000556000600055", 5812, 15000, nil},            // 1 -> 1 -> 0
	{1, math.MaxUint64, "0x60016000556002600055", 5812, 0, nil},                // 1 -> 1 -> 2
	{1, math.MaxUint64, "0x60016000556001600055", 1612, 0, nil},                // 1 -> 1 -> 1
	{0, math.MaxUint64, "0x600160005560006000556001600055", 40818, 19200, nil}, // 0 -> 1 -> 0 -> 1
	{1, math.MaxUint64, "0x600060005560016000556000600055", 10818, 19200, nil}, // 1 -> 0 -> 1 -> 0
	{1, 2306, "0x6001600055", 2306, 0, ErrOutOfGas},                            // 1 -> 1 (2300 sentry + 2xPUSH)
	{1, 2307, "0x6001600055", 806, 0, nil},                                     // 1 -> 1 (2301 sentry + 2xPUSH)
}

func TestEIP2200(t *testing.T) {
	for i, tt := range eip2200Tests {
		address := common.BytesToAddress([]byte("contract"))

		statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		statedb.CreateAccount(address)
		statedb.SetCode(address, hexutil.MustDecode(tt.input), tracing.CodeChangeUnspecified)
		statedb.SetState(address, common.Hash{}, common.BytesToHash([]byte{tt.original}))
		statedb.Finalise(true) // Push the state into the "original" slot

		vmctx := BlockContext{
			CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
			Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int, *params.Rules) {},
		}
		evm := NewEVM(vmctx, statedb, params.AllEthashProtocolChanges, Config{ExtraEips: []int{2200}})

		_, gas, err := evm.Call(common.Address{}, address, nil, tt.gaspool, new(uint256.Int))
		if !errors.Is(err, tt.failure) {
			t.Errorf("test %d: failure mismatch: have %v, want %v", i, err, tt.failure)
		}

		if used := tt.gaspool - gas; used != tt.used {
			t.Errorf("test %d: gas used mismatch: have %v, want %v", i, used, tt.used)
		}
		if refund := evm.StateDB.GetRefund(); refund != tt.refund {
			t.Errorf("test %d: gas refund mismatch: have %v, want %v", i, refund, tt.refund)
		}
	}
}

var createGasTests = []struct {
	code       string
	eip3860    bool
	gasUsed    uint64
	minimumGas uint64
}{
	// legacy create(0, 0, 0xc000) without 3860 used
	{"0x61C00060006000f0" + "600052" + "60206000F3", false, 41237, 41237},
	// legacy create(0, 0, 0xc000) _with_ 3860
	{"0x61C00060006000f0" + "600052" + "60206000F3", true, 44309, 44309},
	// create2(0, 0, 0xc001, 0) without 3860
	{"0x600061C00160006000f5" + "600052" + "60206000F3", false, 50471, 50471},
	// create2(0, 0, 0xc001, 0) (too large), with 3860
	{"0x600061C00160006000f5" + "600052" + "60206000F3", true, 32012, 100_000},
	// create2(0, 0, 0xc000, 0)
	// This case is trying to deploy code at (within) the limit
	{"0x600061C00060006000f5" + "600052" + "60206000F3", true, 53528, 53528},
	// create2(0, 0, 0xc001, 0)
	// This case is trying to deploy code exceeding the limit
	{"0x600061C00160006000f5" + "600052" + "60206000F3", true, 32024, 100000},
}

func TestCreateGas(t *testing.T) {
	t.Parallel()

	for i, tt := range createGasTests {
		var gasUsed = uint64(0)

		doCheck := func(testGas int) bool {
			address := common.BytesToAddress([]byte("contract"))
			statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
			statedb.CreateAccount(address)
			statedb.SetCode(address, hexutil.MustDecode(tt.code), tracing.CodeChangeUnspecified)
			statedb.Finalise(true)

			vmctx := BlockContext{
				CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
				Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int, *params.Rules) {},
				BlockNumber: big.NewInt(0),
			}
			config := Config{}
			chainConfig := params.AllEthashProtocolChanges

			if tt.eip3860 {
				config.ExtraEips = []int{3860}
				vmctx.Random = new(common.Hash)

				chainConfig = params.MergedTestChainConfig
			}

			evm := NewEVM(vmctx, statedb, chainConfig, config)
			var startGas = uint64(testGas)
			ret, gas, err := evm.Call(common.Address{}, address, nil, startGas, new(uint256.Int))
			if err != nil {
				return false
			}

			gasUsed = startGas - gas

			if len(ret) != 32 {
				t.Fatalf("test %d: expected 32 bytes returned, have %d", i, len(ret))
			}

			if bytes.Equal(ret, make([]byte, 32)) {
				// Failure
				return false
			}

			return true
		}
		minGas := sort.Search(100_000, doCheck)

		if uint64(minGas) != tt.minimumGas {
			t.Fatalf("test %d: min gas error, want %d, have %d", i, tt.minimumGas, minGas)
		}
		// If the deployment succeeded, we also check the gas used
		if minGas < 100_000 {
			if gasUsed != tt.gasUsed {
				t.Errorf("test %d: gas used mismatch: have %v, want %v", i, gasUsed, tt.gasUsed)
			}
		}
	}
}

// readCountingStateDB counts committed-state reads so a test can assert whether
// the SSTORE gas path performed the slot read.
type readCountingStateDB struct {
	StateDB
	reads int
}

func (c *readCountingStateDB) GetStateAndCommittedState(addr common.Address, hash common.Hash) (common.Hash, common.Hash) {
	c.reads++
	return c.StateDB.GetStateAndCommittedState(addr, hash)
}

// hampiTestChainConfig copies the shared test chain config with Hampi scheduled at
// forkBlock, so a test can exercise both sides of the gate without mutating globals.
func hampiTestChainConfig(forkBlock *big.Int) *params.ChainConfig {
	cfg := *params.AllEthashProtocolChanges
	bor := *params.AllEthashProtocolChanges.Bor
	bor.HampiBlock = forkBlock
	cfg.Bor = &bor

	return &cfg
}

// TestPIP88SStoreAccessCheck pins the ordering invariant of the PIP-88 SSTORE gas
// function: the cold access cost exceeds the reentrancy sentry, so clearing the
// sentry does not imply the caller can afford a cold slot access. From Hampi the
// committed state read must not happen until affordability is confirmed, and
// before Hampi the pre-existing ordering must be preserved exactly. The wantReads
// column is the load-bearing assertion — an error-string check alone would not
// catch a read performed before the guard.
func TestPIP88SStoreAccessCheck(t *testing.T) {
	addr := common.BytesToAddress([]byte("victim"))
	sentry := params.SstoreSentryGasEIP2200 // 2300
	cold := params.ColdSstoreCostPIP88      // 2940
	const forkBlock = 100

	tests := []struct {
		name      string
		warm      bool
		gas       uint64
		block     int64
		wantErr   string
		wantReads int
	}{
		// At and after activation the read is deferred behind the affordability check.
		{"at fork, cold, at sentry, rejected before read", false, sentry, forkBlock, "not enough gas for reentrancy sentry", 0},
		{"at fork, cold, above sentry below access, rejected before read", false, sentry + 1, forkBlock, "not enough gas for slot access", 0},
		{"at fork, cold, one below access, rejected before read", false, cold - 1, forkBlock, "not enough gas for slot access", 0},
		{"at fork, cold, at access, reads", false, cold, forkBlock, "", 1},
		{"at fork, warm, above sentry, reads", true, sentry + 1, forkBlock, "", 1},
		{"after fork, cold, in window, rejected before read", false, cold - 1, forkBlock + 1, "not enough gas for slot access", 0},

		// Before activation the gate is inert: the read still happens inside the
		// window and the new error is never surfaced.
		{"before fork, cold, in window, still reads", false, cold - 1, forkBlock - 1, "", 1},
		{"before fork, cold, just above sentry, still reads", false, sentry + 1, forkBlock - 1, "", 1},
		{"before fork, cold, at sentry, rejected by sentry", false, sentry, forkBlock - 1, "not enough gas for reentrancy sentry", 0},
		{"before fork, cold, at access, reads", false, cold, forkBlock - 1, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
			inner.CreateAccount(addr)
			if tt.warm {
				inner.AddSlotToAccessList(addr, common.Hash{})
			}
			sdb := &readCountingStateDB{StateDB: inner}
			blockCtx := BlockContext{BlockNumber: big.NewInt(tt.block)}
			evm := NewEVM(blockCtx, sdb, hampiTestChainConfig(big.NewInt(forkBlock)), Config{})

			contract := NewContract(common.Address{}, addr, uint256.NewInt(0), tt.gas, nil)
			stack := newstack()
			stack.push(uint256.NewInt(1)) // value  (Back(1))
			stack.push(new(uint256.Int))  // key    (peek) -> slot 0
			_, err := gasSStorePIP88(evm, contract, stack, nil, 0)

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if sdb.reads != tt.wantReads {
				t.Fatalf("committed-state reads = %d, want %d", sdb.reads, tt.wantReads)
			}
		})
	}
}
