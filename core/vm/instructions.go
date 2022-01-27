// Copyright 2015 The go-ethereum Authors
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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

type dataOP struct {
	Opcode_name string `json:"opcode_name"`
	Num         int    `json:"num"`
	ExeTime     int    `json:"exeTime"`
}

func writeFile(s string, i int, bnum int, path string) {
	fmt.Println("*************", bnum)
	tempData := dataOP{Opcode_name: s, ExeTime: i, Num: bnum}
	byteArray, err := json.Marshal(tempData)
	if err != nil {
		fmt.Println(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		fmt.Println(err)
	}

	n, err := io.WriteString(f, string(byteArray))
	if err != nil {
		fmt.Println(n, err)
	}

	defer f.Close()
}

func opAdd(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Add"
	var startTime = time.Now().UnixNano()
	fmt.Println("*************", startTime)
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Add(&x, y)

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSub(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Sub"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Sub(&x, y)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMul(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Mul"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Mul(&x, y)

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opDiv(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Div"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Div(&x, y)

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSdiv(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Sdiv"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.SDiv(&x, y)

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMod(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Mod"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Mod(&x, y)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSmod(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Smod"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.SMod(&x, y)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opExp(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Exp"
	var startTime = time.Now().UnixNano()
	base, exponent := scope.Stack.pop(), scope.Stack.peek()
	exponent.Exp(&base, exponent)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSignExtend(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "SignExtend"
	var startTime = time.Now().UnixNano()
	back, num := scope.Stack.pop(), scope.Stack.peek()
	num.ExtendSign(num, &back)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opNot(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Not"
	var startTime = time.Now().UnixNano()
	x := scope.Stack.peek()
	x.Not(x)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opLt(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Lt"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	if x.Lt(y) {
		y.SetOne()
	} else {
		y.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opGt(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Gt"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	if x.Gt(y) {
		y.SetOne()
	} else {
		y.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSlt(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Slt"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	if x.Slt(y) {
		y.SetOne()
	} else {
		y.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSgt(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Sgt"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	if x.Sgt(y) {
		y.SetOne()
	} else {
		y.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opEq(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Eq"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	if x.Eq(y) {
		y.SetOne()
	} else {
		y.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opIszero(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Iszero"
	var startTime = time.Now().UnixNano()
	x := scope.Stack.peek()
	if x.IsZero() {
		x.SetOne()
	} else {
		x.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opAnd(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "And"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.And(&x, y)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opOr(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Or"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Or(&x, y)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opXor(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Xor"
	var startTime = time.Now().UnixNano()
	x, y := scope.Stack.pop(), scope.Stack.peek()
	y.Xor(&x, y)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opByte(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Byte"
	var startTime = time.Now().UnixNano()
	th, val := scope.Stack.pop(), scope.Stack.peek()
	val.Byte(&th)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opAddmod(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Addmod"
	var startTime = time.Now().UnixNano()
	x, y, z := scope.Stack.pop(), scope.Stack.pop(), scope.Stack.peek()
	if z.IsZero() {
		z.Clear()
	} else {
		z.AddMod(&x, &y, z)
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMulmod(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Mulmod"
	var startTime = time.Now().UnixNano()
	x, y, z := scope.Stack.pop(), scope.Stack.pop(), scope.Stack.peek()
	z.MulMod(&x, &y, z)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

// opSHL implements Shift Left
// The SHL instruction (shift left) pops 2 values from the stack, first arg1 and then arg2,
// and pushes on the stack arg2 shifted to the left by arg1 number of bits.
func opSHL(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	// Note, second operand is left in the stack; accumulate result into it, and no need to push it afterwards
	op := "SHL"
	var startTime = time.Now().UnixNano()
	shift, value := scope.Stack.pop(), scope.Stack.peek()
	if shift.LtUint64(256) {
		value.Lsh(value, uint(shift.Uint64()))
	} else {
		value.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

// opSHR implements Logical Shift Right
// The SHR instruction (logical shift right) pops 2 values from the stack, first arg1 and then arg2,
// and pushes on the stack arg2 shifted to the right by arg1 number of bits with zero fill.
func opSHR(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	// Note, second operand is left in the stack; accumulate result into it, and no need to push it afterwards
	op := "SHR"
	var startTime = time.Now().UnixNano()
	shift, value := scope.Stack.pop(), scope.Stack.peek()
	if shift.LtUint64(256) {
		value.Rsh(value, uint(shift.Uint64()))
	} else {
		value.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

// opSAR implements Arithmetic Shift Right
// The SAR instruction (arithmetic shift right) pops 2 values from the stack, first arg1 and then arg2,
// and pushes on the stack arg2 shifted to the right by arg1 number of bits with sign extension.
func opSAR(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "SAR"
	var startTime = time.Now().UnixNano()
	shift, value := scope.Stack.pop(), scope.Stack.peek()
	if shift.GtUint64(256) {
		if value.Sign() >= 0 {
			value.Clear()
		} else {
			// Max negative shift: all bits set
			value.SetAllOne()
		}
		return nil, nil
	}
	n := uint(shift.Uint64())
	value.SRsh(value, n)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSha3(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Sha3"
	var startTime = time.Now().UnixNano()
	offset, size := scope.Stack.pop(), scope.Stack.peek()
	data := scope.Memory.GetPtr(int64(offset.Uint64()), int64(size.Uint64()))

	if interpreter.hasher == nil {
		interpreter.hasher = sha3.NewLegacyKeccak256().(keccakState)
	} else {
		interpreter.hasher.Reset()
	}
	interpreter.hasher.Write(data)
	interpreter.hasher.Read(interpreter.hasherBuf[:])

	evm := interpreter.evm
	if evm.Config.EnablePreimageRecording {
		evm.StateDB.AddPreimage(interpreter.hasherBuf, data)
	}

	size.SetBytes(interpreter.hasherBuf[:])
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}
func opAddress(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Address"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetBytes(scope.Contract.Address().Bytes()))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opBalance(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Balance"
	var startTime = time.Now().UnixNano()
	slot := scope.Stack.peek()
	address := common.Address(slot.Bytes20())
	slot.SetFromBig(interpreter.evm.StateDB.GetBalance(address))

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opOrigin(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Origin"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetBytes(interpreter.evm.Origin.Bytes()))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}
func opCaller(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Caller"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetBytes(scope.Contract.Caller().Bytes()))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCallValue(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CallValue"
	var startTime = time.Now().UnixNano()
	v, _ := uint256.FromBig(scope.Contract.value)
	scope.Stack.push(v)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCallDataLoad(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CallDataLoad"
	var startTime = time.Now().UnixNano()
	x := scope.Stack.peek()
	if offset, overflow := x.Uint64WithOverflow(); !overflow {
		data := getData(scope.Contract.Input, offset, 32)
		x.SetBytes(data)
	} else {
		x.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCallDataSize(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CallDataSize"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetUint64(uint64(len(scope.Contract.Input))))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCallDataCopy(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CallDataCopy"
	var startTime = time.Now().UnixNano()
	var (
		memOffset  = scope.Stack.pop()
		dataOffset = scope.Stack.pop()
		length     = scope.Stack.pop()
	)
	dataOffset64, overflow := dataOffset.Uint64WithOverflow()
	if overflow {
		dataOffset64 = 0xffffffffffffffff
	}
	// These values are checked for overflow during gas cost calculation
	memOffset64 := memOffset.Uint64()
	length64 := length.Uint64()
	scope.Memory.Set(memOffset64, length64, getData(scope.Contract.Input, dataOffset64, length64))

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)

	return nil, nil
}

func opReturnDataSize(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "ReturnDataSize"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetUint64(uint64(len(interpreter.returnData))))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opReturnDataCopy(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "ReturnDataCopy"
	var startTime = time.Now().UnixNano()
	var (
		memOffset  = scope.Stack.pop()
		dataOffset = scope.Stack.pop()
		length     = scope.Stack.pop()
	)

	offset64, overflow := dataOffset.Uint64WithOverflow()
	if overflow {
		return nil, ErrReturnDataOutOfBounds
	}
	// we can reuse dataOffset now (aliasing it for clarity)
	var end = dataOffset
	end.Add(&dataOffset, &length)
	end64, overflow := end.Uint64WithOverflow()
	if overflow || uint64(len(interpreter.returnData)) < end64 {
		return nil, ErrReturnDataOutOfBounds
	}
	scope.Memory.Set(memOffset.Uint64(), length.Uint64(), interpreter.returnData[offset64:end64])

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opExtCodeSize(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "ExtCodeSize"
	var startTime = time.Now().UnixNano()
	slot := scope.Stack.peek()
	slot.SetUint64(uint64(interpreter.evm.StateDB.GetCodeSize(slot.Bytes20())))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCodeSize(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CodeSize"
	var startTime = time.Now().UnixNano()
	l := new(uint256.Int)
	l.SetUint64(uint64(len(scope.Contract.Code)))
	scope.Stack.push(l)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCodeCopy(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CodeCopy"
	var startTime = time.Now().UnixNano()
	var (
		memOffset  = scope.Stack.pop()
		codeOffset = scope.Stack.pop()
		length     = scope.Stack.pop()
	)
	uint64CodeOffset, overflow := codeOffset.Uint64WithOverflow()
	if overflow {
		uint64CodeOffset = 0xffffffffffffffff
	}
	codeCopy := getData(scope.Contract.Code, uint64CodeOffset, length.Uint64())
	scope.Memory.Set(memOffset.Uint64(), length.Uint64(), codeCopy)

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)

	return nil, nil
}

func opExtCodeCopy(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "ExtCodeCopy"
	var startTime = time.Now().UnixNano()
	var (
		stack      = scope.Stack
		a          = stack.pop()
		memOffset  = stack.pop()
		codeOffset = stack.pop()
		length     = stack.pop()
	)
	uint64CodeOffset, overflow := codeOffset.Uint64WithOverflow()
	if overflow {
		uint64CodeOffset = 0xffffffffffffffff
	}
	addr := common.Address(a.Bytes20())
	codeCopy := getData(interpreter.evm.StateDB.GetCode(addr), uint64CodeOffset, length.Uint64())
	scope.Memory.Set(memOffset.Uint64(), length.Uint64(), codeCopy)

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)

	return nil, nil
}

// opExtCodeHash returns the code hash of a specified account.
// There are several cases when the function is called, while we can relay everything
// to `state.GetCodeHash` function to ensure the correctness.
//   (1) Caller tries to get the code hash of a normal contract account, state
// should return the relative code hash and set it as the result.
//
//   (2) Caller tries to get the code hash of a non-existent account, state should
// return common.Hash{} and zero will be set as the result.
//
//   (3) Caller tries to get the code hash for an account without contract code,
// state should return emptyCodeHash(0xc5d246...) as the result.
//
//   (4) Caller tries to get the code hash of a precompiled account, the result
// should be zero or emptyCodeHash.
//
// It is worth noting that in order to avoid unnecessary create and clean,
// all precompile accounts on mainnet have been transferred 1 wei, so the return
// here should be emptyCodeHash.
// If the precompile account is not transferred any amount on a private or
// customized chain, the return value will be zero.
//
//   (5) Caller tries to get the code hash for an account which is marked as suicided
// in the current transaction, the code hash of this account should be returned.
//
//   (6) Caller tries to get the code hash for an account which is marked as deleted,
// this account should be regarded as a non-existent account and zero should be returned.
func opExtCodeHash(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "ExtCodeHash"
	var startTime = time.Now().UnixNano()
	slot := scope.Stack.peek()
	address := common.Address(slot.Bytes20())
	if interpreter.evm.StateDB.Empty(address) {
		slot.Clear()
	} else {
		slot.SetBytes(interpreter.evm.StateDB.GetCodeHash(address).Bytes())
	}

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opGasprice(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Gasprice"
	var startTime = time.Now().UnixNano()
	v, _ := uint256.FromBig(interpreter.evm.GasPrice)
	scope.Stack.push(v)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opBlockhash(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Blockhash"
	var startTime = time.Now().UnixNano()
	num := scope.Stack.peek()
	num64, overflow := num.Uint64WithOverflow()
	if overflow {
		num.Clear()
		return nil, nil
	}
	var upper, lower uint64
	upper = interpreter.evm.Context.BlockNumber.Uint64()
	if upper < 257 {
		lower = 0
	} else {
		lower = upper - 256
	}
	if num64 >= lower && num64 < upper {
		num.SetBytes(interpreter.evm.Context.GetHash(num64).Bytes())
	} else {
		num.Clear()
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCoinbase(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Coinbase"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetBytes(interpreter.evm.Context.Coinbase.Bytes()))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opTimestamp(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Timestamp"
	var startTime = time.Now().UnixNano()
	v, _ := uint256.FromBig(interpreter.evm.Context.Time)
	scope.Stack.push(v)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opNumber(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Number"
	var startTime = time.Now().UnixNano()
	v, _ := uint256.FromBig(interpreter.evm.Context.BlockNumber)
	scope.Stack.push(v)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opDifficulty(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Difficulty"
	var startTime = time.Now().UnixNano()
	v, _ := uint256.FromBig(interpreter.evm.Context.Difficulty)
	scope.Stack.push(v)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opGasLimit(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "GasLimit"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetUint64(interpreter.evm.Context.GasLimit))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opPop(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Pop"
	var startTime = time.Now().UnixNano()
	scope.Stack.pop()
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMload(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Mload"
	var startTime = time.Now().UnixNano()
	v := scope.Stack.peek()
	offset := int64(v.Uint64())
	v.SetBytes(scope.Memory.GetPtr(offset, 32))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMstore(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Mstore"
	var startTime = time.Now().UnixNano()
	// pop value of the stack
	mStart, val := scope.Stack.pop(), scope.Stack.pop()
	scope.Memory.Set32(mStart.Uint64(), &val)
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMstore8(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Mstore8"
	var startTime = time.Now().UnixNano()
	off, val := scope.Stack.pop(), scope.Stack.pop()
	scope.Memory.store[off.Uint64()] = byte(val.Uint64())
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSload(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Sload"
	var startTime = time.Now().UnixNano()
	loc := scope.Stack.peek()
	hash := common.Hash(loc.Bytes32())
	val := interpreter.evm.StateDB.GetState(scope.Contract.Address(), hash)
	loc.SetBytes(val.Bytes())
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSstore(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Sstore"
	var startTime = time.Now().UnixNano()
	loc := scope.Stack.pop()
	val := scope.Stack.pop()
	interpreter.evm.StateDB.SetState(scope.Contract.Address(),
		loc.Bytes32(), val.Bytes32())
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opJump(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Jump"
	var startTime = time.Now().UnixNano()
	pos := scope.Stack.pop()
	if !scope.Contract.validJumpdest(&pos) {
		return nil, ErrInvalidJump
	}
	*pc = pos.Uint64()
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opJumpi(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Jumpi"
	var startTime = time.Now().UnixNano()
	pos, cond := scope.Stack.pop(), scope.Stack.pop()
	if !cond.IsZero() {
		if !scope.Contract.validJumpdest(&pos) {
			return nil, ErrInvalidJump
		}
		*pc = pos.Uint64()
	} else {
		*pc++
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opJumpdest(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Jumpdest"
	var startTime = time.Now().UnixNano()
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opPc(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Pc"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetUint64(*pc))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opMsize(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Msize"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetUint64(uint64(scope.Memory.Len())))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opGas(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Gas"
	var startTime = time.Now().UnixNano()
	scope.Stack.push(new(uint256.Int).SetUint64(scope.Contract.Gas))
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCreate(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Create"
	var startTime = time.Now().UnixNano()
	var (
		value        = scope.Stack.pop()
		offset, size = scope.Stack.pop(), scope.Stack.pop()
		input        = scope.Memory.GetCopy(int64(offset.Uint64()), int64(size.Uint64()))
		gas          = scope.Contract.Gas
	)
	if interpreter.evm.chainRules.IsEIP150 {
		gas -= gas / 64
	}
	// reuse size int for stackvalue
	stackvalue := size

	scope.Contract.UseGas(gas)
	//TODO: use uint256.Int instead of converting with toBig()
	var bigVal = big0
	if !value.IsZero() {
		bigVal = value.ToBig()
	}

	res, addr, returnGas, suberr := interpreter.evm.Create(scope.Contract, input, gas, bigVal)
	// Push item on the stack based on the returned error. If the ruleset is
	// homestead we must check for CodeStoreOutOfGasError (homestead only
	// rule) and treat as an error, if the ruleset is frontier we must
	// ignore this error and pretend the operation was successful.
	if interpreter.evm.chainRules.IsHomestead && suberr == ErrCodeStoreOutOfGas {
		stackvalue.Clear()
	} else if suberr != nil && suberr != ErrCodeStoreOutOfGas {
		stackvalue.Clear()
	} else {
		stackvalue.SetBytes(addr.Bytes())
	}
	scope.Stack.push(&stackvalue)
	scope.Contract.Gas += returnGas

	if suberr == ErrExecutionReverted {
		return res, nil
	}

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCreate2(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Create2"
	var startTime = time.Now().UnixNano()
	var (
		endowment    = scope.Stack.pop()
		offset, size = scope.Stack.pop(), scope.Stack.pop()
		salt         = scope.Stack.pop()
		input        = scope.Memory.GetCopy(int64(offset.Uint64()), int64(size.Uint64()))
		gas          = scope.Contract.Gas
	)

	// Apply EIP150
	gas -= gas / 64
	scope.Contract.UseGas(gas)
	// reuse size int for stackvalue
	stackvalue := size
	//TODO: use uint256.Int instead of converting with toBig()
	bigEndowment := big0
	if !endowment.IsZero() {
		bigEndowment = endowment.ToBig()
	}
	res, addr, returnGas, suberr := interpreter.evm.Create2(scope.Contract, input, gas,
		bigEndowment, &salt)
	// Push item on the stack based on the returned error.
	if suberr != nil {
		stackvalue.Clear()
	} else {
		stackvalue.SetBytes(addr.Bytes())
	}
	scope.Stack.push(&stackvalue)
	scope.Contract.Gas += returnGas

	if suberr == ErrExecutionReverted {
		return res, nil
	}

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opCall(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Call"
	var startTime = time.Now().UnixNano()
	stack := scope.Stack
	// Pop gas. The actual gas in interpreter.evm.callGasTemp.
	// We can use this as a temporary value
	temp := stack.pop()
	gas := interpreter.evm.callGasTemp
	// Pop other call parameters.
	addr, value, inOffset, inSize, retOffset, retSize := stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop()
	toAddr := common.Address(addr.Bytes20())
	// Get the arguments from the memory.
	args := scope.Memory.GetPtr(int64(inOffset.Uint64()), int64(inSize.Uint64()))

	var bigVal = big0
	//TODO: use uint256.Int instead of converting with toBig()
	// By using big0 here, we save an alloc for the most common case (non-ether-transferring contract calls),
	// but it would make more sense to extend the usage of uint256.Int
	if !value.IsZero() {
		gas += params.CallStipend
		bigVal = value.ToBig()
	}

	ret, returnGas, err := interpreter.evm.Call(scope.Contract, toAddr, args, gas, bigVal)

	if err != nil {
		temp.Clear()
	} else {
		temp.SetOne()
	}
	stack.push(&temp)
	if err == nil || err == ErrExecutionReverted {
		ret = common.CopyBytes(ret)
		scope.Memory.Set(retOffset.Uint64(), retSize.Uint64(), ret)
	}
	scope.Contract.Gas += returnGas

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return ret, nil
}

func opCallCode(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "CallCode"
	var startTime = time.Now().UnixNano()
	// Pop gas. The actual gas is in interpreter.evm.callGasTemp.
	stack := scope.Stack
	// We use it as a temporary value
	temp := stack.pop()
	gas := interpreter.evm.callGasTemp
	// Pop other call parameters.
	addr, value, inOffset, inSize, retOffset, retSize := stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop()
	toAddr := common.Address(addr.Bytes20())
	// Get arguments from the memory.
	args := scope.Memory.GetPtr(int64(inOffset.Uint64()), int64(inSize.Uint64()))

	//TODO: use uint256.Int instead of converting with toBig()
	var bigVal = big0
	if !value.IsZero() {
		gas += params.CallStipend
		bigVal = value.ToBig()
	}

	ret, returnGas, err := interpreter.evm.CallCode(scope.Contract, toAddr, args, gas, bigVal)
	if err != nil {
		temp.Clear()
	} else {
		temp.SetOne()
	}
	stack.push(&temp)
	if err == nil || err == ErrExecutionReverted {
		ret = common.CopyBytes(ret)
		scope.Memory.Set(retOffset.Uint64(), retSize.Uint64(), ret)
	}
	scope.Contract.Gas += returnGas

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return ret, nil
}

func opDelegateCall(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "DelegateCall"
	var startTime = time.Now().UnixNano()
	stack := scope.Stack
	// Pop gas. The actual gas is in interpreter.evm.callGasTemp.
	// We use it as a temporary value
	temp := stack.pop()
	gas := interpreter.evm.callGasTemp
	// Pop other call parameters.
	addr, inOffset, inSize, retOffset, retSize := stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop()
	toAddr := common.Address(addr.Bytes20())
	// Get arguments from the memory.
	args := scope.Memory.GetPtr(int64(inOffset.Uint64()), int64(inSize.Uint64()))

	ret, returnGas, err := interpreter.evm.DelegateCall(scope.Contract, toAddr, args, gas)
	if err != nil {
		temp.Clear()
	} else {
		temp.SetOne()
	}
	stack.push(&temp)
	if err == nil || err == ErrExecutionReverted {
		ret = common.CopyBytes(ret)
		scope.Memory.Set(retOffset.Uint64(), retSize.Uint64(), ret)
	}
	scope.Contract.Gas += returnGas

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return ret, nil
}

func opStaticCall(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "StaticCall"
	var startTime = time.Now().UnixNano()
	// Pop gas. The actual gas is in interpreter.evm.callGasTemp.
	stack := scope.Stack
	// We use it as a temporary value
	temp := stack.pop()
	gas := interpreter.evm.callGasTemp
	// Pop other call parameters.
	addr, inOffset, inSize, retOffset, retSize := stack.pop(), stack.pop(), stack.pop(), stack.pop(), stack.pop()
	toAddr := common.Address(addr.Bytes20())
	// Get arguments from the memory.
	args := scope.Memory.GetPtr(int64(inOffset.Uint64()), int64(inSize.Uint64()))

	ret, returnGas, err := interpreter.evm.StaticCall(scope.Contract, toAddr, args, gas)
	if err != nil {
		temp.Clear()
	} else {
		temp.SetOne()
	}
	stack.push(&temp)
	if err == nil || err == ErrExecutionReverted {
		ret = common.CopyBytes(ret)
		scope.Memory.Set(retOffset.Uint64(), retSize.Uint64(), ret)
	}
	scope.Contract.Gas += returnGas

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return ret, nil
}

func opReturn(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Return"
	var startTime = time.Now().UnixNano()
	offset, size := scope.Stack.pop(), scope.Stack.pop()
	ret := scope.Memory.GetPtr(int64(offset.Uint64()), int64(size.Uint64()))

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return ret, nil
}

func opRevert(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Revert"
	var startTime = time.Now().UnixNano()
	offset, size := scope.Stack.pop(), scope.Stack.pop()
	ret := scope.Memory.GetPtr(int64(offset.Uint64()), int64(size.Uint64()))

	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return ret, nil
}

func opStop(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Stop"
	var startTime = time.Now().UnixNano()
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

func opSuicide(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Suicide"
	var startTime = time.Now().UnixNano()
	beneficiary := scope.Stack.pop()
	balance := interpreter.evm.StateDB.GetBalance(scope.Contract.Address())
	interpreter.evm.StateDB.AddBalance(beneficiary.Bytes20(), balance)
	interpreter.evm.StateDB.Suicide(scope.Contract.Address())
	if interpreter.cfg.Debug {
		interpreter.cfg.Tracer.CaptureEnter(SELFDESTRUCT, scope.Contract.Address(), beneficiary.Bytes20(), []byte{}, 0, balance)
		interpreter.cfg.Tracer.CaptureExit([]byte{}, 0, nil)
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

// following functions are used by the instruction jump  table

// make log instruction function
func makeLog(size int) executionFunc {
	return func(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
		op := "makeLog"
		var startTime = time.Now().UnixNano()
		topics := make([]common.Hash, size)
		stack := scope.Stack
		mStart, mSize := stack.pop(), stack.pop()
		for i := 0; i < size; i++ {
			addr := stack.pop()
			topics[i] = addr.Bytes32()
		}

		d := scope.Memory.GetCopy(int64(mStart.Uint64()), int64(mSize.Uint64()))
		interpreter.evm.StateDB.AddLog(&types.Log{
			Address: scope.Contract.Address(),
			Topics:  topics,
			Data:    d,
			// This is a non-consensus field, but assigned here because
			// core/state doesn't know the current block number.
			BlockNumber: interpreter.evm.Context.BlockNumber.Uint64(),
		})

		var endTime = time.Now().UnixNano()

		var dif = endTime - startTime
		bnum := interpreter.evm.Context.BlockNumber.Uint64()
		k := bnum
		path := fmt.Sprintf("%s%v.json", op, k)
		writeFile(op, int(dif), int(bnum), path)
		return nil, nil
	}
}

// opPush1 is a specialized version of pushN
func opPush1(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
	op := "Push1"
	var startTime = time.Now().UnixNano()
	var (
		codeLen = uint64(len(scope.Contract.Code))
		integer = new(uint256.Int)
	)
	*pc += 1
	if *pc < codeLen {
		scope.Stack.push(integer.SetUint64(uint64(scope.Contract.Code[*pc])))
	} else {
		scope.Stack.push(integer.Clear())
	}
	var endTime = time.Now().UnixNano()

	var dif = endTime - startTime
	bnum := interpreter.evm.Context.BlockNumber.Uint64()
	k := bnum
	path := fmt.Sprintf("%s%v.json", op, k)
	writeFile(op, int(dif), int(bnum), path)
	return nil, nil
}

// make push instruction function
func makePush(size uint64, pushByteSize int) executionFunc {
	return func(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
		op := "makePush"
		var startTime = time.Now().UnixNano()
		codeLen := len(scope.Contract.Code)

		startMin := codeLen
		if int(*pc+1) < startMin {
			startMin = int(*pc + 1)
		}

		endMin := codeLen
		if startMin+pushByteSize < endMin {
			endMin = startMin + pushByteSize
		}

		integer := new(uint256.Int)
		scope.Stack.push(integer.SetBytes(common.RightPadBytes(
			scope.Contract.Code[startMin:endMin], pushByteSize)))

		*pc += size
		var endTime = time.Now().UnixNano()

		var dif = endTime - startTime
		bnum := interpreter.evm.Context.BlockNumber.Uint64()
		k := bnum
		path := fmt.Sprintf("%s%v.json", op, k)
		writeFile(op, int(dif), int(bnum), path)
		return nil, nil
	}
}

// make dup instruction function
func makeDup(size int64) executionFunc {
	return func(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
		op := "makeDup"
		var startTime = time.Now().UnixNano()
		scope.Stack.dup(int(size))
		var endTime = time.Now().UnixNano()

		var dif = endTime - startTime
		bnum := interpreter.evm.Context.BlockNumber.Uint64()
		k := bnum
		path := fmt.Sprintf("%s%v.json", op, k)
		writeFile(op, int(dif), int(bnum), path)
		return nil, nil
	}
}

// make swap instruction function
func makeSwap(size int64) executionFunc {
	// switch n + 1 otherwise n would be swapped with n
	size++
	return func(pc *uint64, interpreter *EVMInterpreter, scope *ScopeContext) ([]byte, error) {
		op := "makeSwap"
		var startTime = time.Now().UnixNano()
		scope.Stack.swap(int(size))
		var endTime = time.Now().UnixNano()

		var dif = endTime - startTime
		bnum := interpreter.evm.Context.BlockNumber.Uint64()
		k := bnum
		path := fmt.Sprintf("%s%v.json", op, k)
		writeFile(op, int(dif), int(bnum), path)
		return nil, nil
	}
}
