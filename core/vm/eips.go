// Copyright 2019 The go-ethereum Authors
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
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

var activators = map[int]func(*JumpTable){
	3529: enable3529,
	3198: enable3198,
	3074: enable3074,
	2929: enable2929,
	2200: enable2200,
	1884: enable1884,
	1344: enable1344,
}

// EnableEIP enables the given EIP on the config.
// This operation writes in-place, and callers need to ensure that the globally
// defined jump tables are not polluted.
func EnableEIP(eipNum int, jt *JumpTable) error {
	enablerFn, ok := activators[eipNum]
	if !ok {
		return fmt.Errorf("undefined eip %d", eipNum)
	}
	enablerFn(jt)
	return nil
}

func ValidEip(eipNum int) bool {
	_, ok := activators[eipNum]
	return ok
}
func ActivateableEips() []string {
	var nums []string
	for k := range activators {
		nums = append(nums, fmt.Sprintf("%d", k))
	}
	sort.Strings(nums)
	return nums
}

// enable1884 applies EIP-1884 to the given jump table:
// - Increase cost of BALANCE to 700
// - Increase cost of EXTCODEHASH to 700
// - Increase cost of SLOAD to 800
// - Define SELFBALANCE, with cost GasFastStep (5)
func enable1884(jt *JumpTable) {
	// Gas cost changes
	jt[SLOAD].constantGas = params.SloadGasEIP1884
	jt[BALANCE].constantGas = params.BalanceGasEIP1884
	jt[EXTCODEHASH].constantGas = params.ExtcodeHashGasEIP1884

	// New opcode
	jt[SELFBALANCE] = &operation{
		execute:     opSelfBalance,
		constantGas: GasFastStep,
		minStack:    minStack(0, 1),
		maxStack:    maxStack(0, 1),
	}
}

func opSelfBalance(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	balance, _ := uint256.FromBig(interpreter.evm.StateDB.GetBalance(scope.Contract.Address()))
	scope.Stack.push(balance)
	return nil, nil
}

// enable1344 applies EIP-1344 (ChainID Opcode)
// - Adds an opcode that returns the current chain’s EIP-155 unique identifier
func enable1344(jt *JumpTable) {
	// New opcode
	jt[CHAINID] = &operation{
		execute:     opChainID,
		constantGas: GasQuickStep,
		minStack:    minStack(0, 1),
		maxStack:    maxStack(0, 1),
	}
}

// opChainID implements CHAINID opcode
func opChainID(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	chainId, _ := uint256.FromBig(interpreter.evm.chainConfig.ChainID)
	scope.Stack.push(chainId)
	return nil, nil
}

// enable2200 applies EIP-2200 (Rebalance net-metered SSTORE)
func enable2200(jt *JumpTable) {
	jt[SLOAD].constantGas = params.SloadGasEIP2200
	jt[SSTORE].dynamicGas = gasSStoreEIP2200
}

// enable2929 enables "EIP-2929: Gas cost increases for state access opcodes"
// https://eips.ethereum.org/EIPS/eip-2929
func enable2929(jt *JumpTable) {
	jt[SSTORE].dynamicGas = gasSStoreEIP2929

	jt[SLOAD].constantGas = 0
	jt[SLOAD].dynamicGas = gasSLoadEIP2929

	jt[EXTCODECOPY].constantGas = params.WarmStorageReadCostEIP2929
	jt[EXTCODECOPY].dynamicGas = gasExtCodeCopyEIP2929

	jt[EXTCODESIZE].constantGas = params.WarmStorageReadCostEIP2929
	jt[EXTCODESIZE].dynamicGas = gasEip2929AccountCheck

	jt[EXTCODEHASH].constantGas = params.WarmStorageReadCostEIP2929
	jt[EXTCODEHASH].dynamicGas = gasEip2929AccountCheck

	jt[BALANCE].constantGas = params.WarmStorageReadCostEIP2929
	jt[BALANCE].dynamicGas = gasEip2929AccountCheck

	jt[CALL].constantGas = params.WarmStorageReadCostEIP2929
	jt[CALL].dynamicGas = gasCallEIP2929

	jt[CALLCODE].constantGas = params.WarmStorageReadCostEIP2929
	jt[CALLCODE].dynamicGas = gasCallCodeEIP2929

	jt[STATICCALL].constantGas = params.WarmStorageReadCostEIP2929
	jt[STATICCALL].dynamicGas = gasStaticCallEIP2929

	jt[DELEGATECALL].constantGas = params.WarmStorageReadCostEIP2929
	jt[DELEGATECALL].dynamicGas = gasDelegateCallEIP2929

	// This was previously part of the dynamic cost, but we're using it as a constantGas
	// factor here
	jt[SELFDESTRUCT].constantGas = params.SelfdestructGasEIP150
	jt[SELFDESTRUCT].dynamicGas = gasSelfdestructEIP2929
}

// enable3529 enabled "EIP-3529: Reduction in refunds":
// - Removes refunds for selfdestructs
// - Reduces refunds for SSTORE
// - Reduces max refunds to 20% gas
func enable3529(jt *JumpTable) {
	jt[SSTORE].dynamicGas = gasSStoreEIP3529
	jt[SELFDESTRUCT].dynamicGas = gasSelfdestructEIP3529
}

// enable3198 applies EIP-3198 (BASEFEE Opcode)
// - Adds an opcode that returns the current block's base fee.
func enable3198(jt *JumpTable) {
	// New opcode
	jt[BASEFEE] = &operation{
		execute:     opBaseFee,
		constantGas: GasQuickStep,
		minStack:    minStack(0, 1),
		maxStack:    maxStack(0, 1),
	}
}

// opBaseFee implements BASEFEE opcode
func opBaseFee(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	baseFee, _ := uint256.FromBig(interpreter.evm.Context.BaseFee)
	scope.Stack.push(baseFee)
	return nil, nil
}

func enable3074(jt *JumpTable) {
	jt[AUTH] = &operation{
		execute:     opAuth,
		constantGas: params.AuthGasEIP3074,
		dynamicGas:  gasReturn, // memory expansion gas cost (auth_memory_expansion_fee) shall be calculated in the same way as RETURN...?
		minStack:    minStack(3, 1),
		maxStack:    maxStack(3, 1),
	}

	jt[AUTHCALL] = &operation{
		execute:     opAuthCall,
		constantGas: params.WarmStorageReadCostEIP2929,
		dynamicGas:  gasAuthCallEIP2929,
		minStack:    minStack(8, 1),
		maxStack:    maxStack(8, 1),
		memorySize:  memoryAuthCall,
		// returns:     true, // TODO !?!?!
	}
}

// memory[offset : offset+32 ] - yParity
// memory[offset+32 : offset+64 ] - r
// memory[offset+64 : offset+96 ] - s
// memory[offset+96 : offset+128] - commit
func crackAuthMemory(mem []byte) (v, r, s *uint256.Int, commit []byte) {
	if len(mem) != 128 {
		return
	}
	v = new(uint256.Int).SetBytes(mem[:32])
	r = new(uint256.Int).SetBytes(mem[32:64])
	s = new(uint256.Int).SetBytes(mem[64:96])
	commit = mem[96:]
	return
}

// EIP-3074 messages are of the form
// keccak256(magic ++ chainId ++ invoker ++ commit)
func make3074Hash(chainId *big.Int, invokerAddr common.Address, commit []byte) []byte {
	var msg [97]byte
	msg[0] = 0x03
	copy(msg[1:], math.PaddedBigBytes(chainId, 32)[:32])
	copy(msg[45:65], invokerAddr.Bytes())
	copy(msg[65:], commit)

	return crypto.Keccak256(msg[:])
}

func opAuth(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	stack := scope.Stack
	memory := scope.Memory

	authority, offset, length := stack.pop(), stack.pop(), stack.pop()

	authAddr := common.Address(authority.Bytes20())
	v, r, s, commit := crackAuthMemory(memory.GetPtr(int64(offset.Uint64()), int64(length.Uint64())))

	// Zero out the current authorized account. Only update it if an address is successfully recovered from the signature.
	scope.Authorized = nil
	result := new(uint256.Int) // init to zero / false

	if v != nil && v.BitLen() < 8 &&
		crypto.ValidateSignatureValues(byte(v.Uint64()), r.ToBig(), s.ToBig(), true) {

		hash := make3074Hash(interpreter.evm.ChainConfig().ChainID, scope.Contract.Address(), commit)

		sig := make([]byte, 65)
		r.WriteToSlice(sig[0:32])
		s.WriteToSlice(sig[32:64])
		sig[64] = byte(v.Uint64())

		if pub, err := crypto.Ecrecover(hash[:], sig); err == nil {
			var addr common.Address
			copy(addr[:], crypto.Keccak256(pub[1:])[12:])
			if addr == authAddr {
				scope.Authorized = &addr
				result.SetOne()
			}
		}
	}

	stack.push(result)

	return nil, nil
}

func opAuthCall(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	// If no authorized account is set, revert.
	if scope.Authorized == nil {
		return nil, ErrNoAuthorizedAccount
	}

	stack := scope.Stack
	// Pop gas. The actual gas in interpreter.evm.callGasTemp.
	// We can use this as a temporary value
	temp := stack.pop()
	gas := interpreter.evm.callGasTemp
	// Pop other call parameters.
	addr, value, extValue, inOffset, inSize, retOffset, retSize := stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop()
	toAddr := common.Address(addr.Bytes20())
	// Get the arguments from the memory.
	args := scope.Memory.GetPtr(int64(inOffset.Uint64()), int64(inSize.Uint64()))

	ret, returnGas, err := interpreter.evm.AuthCall(scope.Contract, *scope.Authorized, toAddr, args, gas, value.ToBig(), extValue.ToBig())

	if err == ErrInsufficientBalance {
		return nil, ErrInsufficientBalance
	} else if err != nil {
		temp.Clear()
	} else {
		temp.SetOne()
	}
	stack.push(&temp)
	if err == nil || err == ErrExecutionReverted {
		scope.Memory.Set(retOffset.Uint64(), retSize.Uint64(), ret)
	}
	scope.Contract.Gas += returnGas

	return ret, nil
}
