package vm

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// Bytecode helpers.
func cc(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
func p1(v byte) []byte    { return []byte{byte(PUSH1), v} }
func op1(o OpCode) []byte { return []byte{byte(o)} }
func m8(offset, value byte) []byte {
	return cc(p1(value), p1(offset), op1(MSTORE8))
}
func p32(v *uint256.Int) []byte {
	b := v.Bytes32()
	out := make([]byte, 33)
	out[0] = byte(PUSH32)
	copy(out[1:], b[:])
	return out
}

// PUSH1 0, MSTORE, PUSH1 32, PUSH1 0, RETURN.
var retSeq = cc(p1(0), op1(MSTORE), p1(32), p1(0), op1(RETURN))

func withRet(code []byte) []byte { return cc(code, retSeq) }

// Binary op with small immediates.
func binSmall(a, b byte, op OpCode) []byte { return withRet(cc(p1(b), p1(a), op1(op))) }

// Binary op with 256-bit immediates.
func binBig(a, b *uint256.Int, op OpCode) []byte { return withRet(cc(p32(b), p32(a), op1(op))) }

// Ternary op with small immediates.
func ternSmall(a, b, c byte, op OpCode) []byte {
	return withRet(cc(p1(c), p1(b), p1(a), op1(op)))
}

// Unary op with a small immediate.
func unSmall(a byte, op OpCode) []byte { return withRet(cc(p1(a), op1(op))) }

// Unary op with a 256-bit immediate.
func unBig(a *uint256.Int, op OpCode) []byte { return withRet(cc(p32(a), op1(op))) }

// Compare error class instead of exact strings.
func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	var su *ErrStackUnderflow
	if errors.As(err, &su) {
		return "stack_underflow"
	}
	var so *ErrStackOverflow
	if errors.As(err, &so) {
		return "stack_overflow"
	}
	var ioc *ErrInvalidOpCode
	if errors.As(err, &ioc) {
		return "invalid_opcode"
	}
	if errors.Is(err, ErrOutOfGas) {
		return "out_of_gas"
	}
	if errors.Is(err, ErrInvalidJump) {
		return "invalid_jump"
	}
	if errors.Is(err, ErrExecutionReverted) {
		return "execution_reverted"
	}
	if errors.Is(err, ErrReturnDataOutOfBounds) {
		return "return_data_out_of_bounds"
	}
	return "other:" + err.Error()
}

type execResult struct {
	ret  []byte
	gas  uint64
	err  error
	logs []*types.Log
}

func sameLogs(a, b []*types.Log) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Address != b[i].Address || a[i].BlockNumber != b[i].BlockNumber {
			return false
		}
		if !bytes.Equal(a[i].Data, b[i].Data) {
			return false
		}
		if len(a[i].Topics) != len(b[i].Topics) {
			return false
		}
		for j := range a[i].Topics {
			if a[i].Topics[j] != b[i].Topics[j] {
				return false
			}
		}
	}
	return true
}

// Enable all forks through Osaka; keep Verkle disabled.
var diffChainConfig = &params.ChainConfig{
	ChainID:             big.NewInt(1),
	HomesteadBlock:      new(big.Int),
	DAOForkBlock:        new(big.Int),
	EIP150Block:         new(big.Int),
	EIP155Block:         new(big.Int),
	EIP158Block:         new(big.Int),
	ByzantiumBlock:      new(big.Int),
	ConstantinopleBlock: new(big.Int),
	PetersburgBlock:     new(big.Int),
	IstanbulBlock:       new(big.Int),
	MuirGlacierBlock:    new(big.Int),
	BerlinBlock:         new(big.Int),
	LondonBlock:         new(big.Int),
	ArrowGlacierBlock:   new(big.Int),
	GrayGlacierBlock:    new(big.Int),
	MergeNetsplitBlock:  new(big.Int),
	ShanghaiBlock:       new(big.Int),
	CancunBlock:         new(big.Int),
	PragueBlock:         new(big.Int),
	OsakaBlock:          new(big.Int),
	// Keep Verkle disabled so both paths remain comparable.
}

// Execute code on one interpreter path.
func execPathResultWithConfig(
	code []byte,
	input []byte,
	gas uint64,
	switchDispatch bool,
	chainCfg *params.ChainConfig,
	setup func(*state.StateDB),
) execResult {
	addr := common.BytesToAddress([]byte("contract"))
	caller := common.BytesToAddress([]byte("caller"))
	origin := common.BytesToAddress([]byte("origin"))
	coinbase := common.BytesToAddress([]byte("coinbase"))
	random := common.BigToHash(big.NewInt(99))
	blobHashes := []common.Hash{
		common.BigToHash(big.NewInt(0xB10B)),
		common.BigToHash(big.NewInt(0xB10C)),
	}

	db, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	db.CreateAccount(addr)
	db.SetCode(addr, code, tracing.CodeChangeUnspecified)
	db.CreateAccount(caller)
	db.CreateAccount(origin)
	db.SetBalance(addr, uint256.NewInt(0x1234), tracing.BalanceChangeUnspecified)
	db.SetBalance(caller, uint256.NewInt(0x5678), tracing.BalanceChangeUnspecified)
	if setup != nil {
		setup(db)
	}
	db.Finalise(true)

	bctx := BlockContext{
		CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int) {},
		GetHash:     func(n uint64) common.Hash { return common.BigToHash(new(big.Int).SetUint64(n + 0x1000)) },
		Coinbase:    coinbase,
		BlockNumber: big.NewInt(11),
		Time:        22,
		Difficulty:  big.NewInt(33),
		GasLimit:    gas,
		BaseFee:     big.NewInt(44),
		BlobBaseFee: big.NewInt(55),
		Random:      &random,
	}

	rules := chainCfg.Rules(bctx.BlockNumber, bctx.Random != nil, bctx.Time)
	db.Prepare(rules, caller, common.Address{}, &addr, ActivePrecompiles(rules), nil)

	cfg := Config{EnableEVMSwitchDispatch: switchDispatch}

	evm := NewEVM(bctx, db, chainCfg, cfg)
	evm.SetTxContext(TxContext{
		Origin:     origin,
		GasPrice:   uint256.NewInt(66),
		BlobHashes: blobHashes,
	})
	ret, gasLeft, err := evm.Call(caller, addr, input, gas, uint256.NewInt(77))
	return execResult{
		ret:  ret,
		gas:  gasLeft,
		err:  err,
		logs: db.Logs(),
	}
}

func execPathWithConfig(
	code []byte,
	input []byte,
	gas uint64,
	switchDispatch bool,
	chainCfg *params.ChainConfig,
	setup func(*state.StateDB),
) ([]byte, uint64, error) {
	res := execPathResultWithConfig(code, input, gas, switchDispatch, chainCfg, setup)
	return res.ret, res.gas, res.err
}

func setCode(db *state.StateDB, addr common.Address, code []byte) {
	db.CreateAccount(addr)
	db.SetCode(addr, code, tracing.CodeChangeUnspecified)
}

// Assert both paths agree on returndata, gas, errors, and logs.
func runDiff(t *testing.T, code []byte, gas uint64) {
	t.Helper()

	runDiffWithSetupAndInput(t, code, nil, gas, nil)
}

func runDiffWithSetup(t *testing.T, code []byte, gas uint64, setup func(*state.StateDB)) {
	t.Helper()

	runDiffWithSetupAndInput(t, code, nil, gas, setup)
}

func runDiffWithInput(t *testing.T, code []byte, input []byte, gas uint64) {
	t.Helper()

	runDiffWithSetupAndInput(t, code, input, gas, nil)
}

func runDiffWithSetupAndInput(t *testing.T, code []byte, input []byte, gas uint64, setup func(*state.StateDB)) {
	t.Helper()

	fast := execPathResultWithConfig(code, input, gas, true, diffChainConfig, setup)
	slow := execPathResultWithConfig(code, input, gas, false, diffChainConfig, setup)

	fastErrStr, slowErrStr := fmt.Sprint(fast.err), fmt.Sprint(slow.err)
	if fastErrStr != slowErrStr {
		t.Fatalf("error mismatch:\n  fast: %v\n  slow: %v", fast.err, slow.err)
	}
	if !bytes.Equal(fast.ret, slow.ret) {
		t.Fatalf("return data mismatch:\n  fast(%d): %x\n  slow(%d): %x",
			len(fast.ret), fast.ret, len(slow.ret), slow.ret)
	}
	if fast.gas != slow.gas {
		t.Fatalf("gas mismatch: fast=%d slow=%d", fast.gas, slow.gas)
	}
	if !sameLogs(fast.logs, slow.logs) {
		t.Fatalf("logs mismatch:\n  fast=%v\n  slow=%v", fast.logs, slow.logs)
	}
}

// Common uint256 fixtures.
var (
	maxU256  = new(uint256.Int).SetAllOne()                               // 2^256 − 1
	minI256  = new(uint256.Int).Lsh(uint256.NewInt(1), 255)               // 2^255  (most-negative signed)
	negOne   = new(uint256.Int).Sub(new(uint256.Int), uint256.NewInt(1))  // −1  in two's complement
	neg10    = new(uint256.Int).Sub(new(uint256.Int), uint256.NewInt(10)) // −10
	negThree = new(uint256.Int).Sub(new(uint256.Int), uint256.NewInt(3))  // −3
)

// Main differential suite.
func TestDispatchDifferential(t *testing.T) {
	t.Parallel()
	const G = uint64(100_000)
	revertCalleeAddr := common.BytesToAddress([]byte{0x42})
	stopCalleeAddr := common.BytesToAddress([]byte{0x43})
	transientRevertCalleeAddr := common.BytesToAddress([]byte{0x44})
	transientSuccessCalleeAddr := common.BytesToAddress([]byte{0x45})
	revertCalleeCode := cc(
		p1(0xDE), p1(0), op1(MSTORE8),
		p1(0xAD), p1(1), op1(MSTORE8),
		p1(2), p1(0), op1(REVERT),
	)
	transientRevertCalleeCode := cc(
		p1(0xAA), p1(0), op1(TSTORE),
		p1(0), p1(0), op1(REVERT),
	)
	transientSuccessCalleeCode := cc(
		p1(0xAA), p1(0), op1(TSTORE),
		op1(STOP),
	)
	setupRevertCallee := func(db *state.StateDB) {
		setCode(db, revertCalleeAddr, revertCalleeCode)
	}
	setupRevertAndStopCallees := func(db *state.StateDB) {
		setupRevertCallee(db)
		setCode(db, stopCalleeAddr, op1(STOP))
	}
	setupTransientRevertCallee := func(db *state.StateDB) {
		setCode(db, transientRevertCalleeAddr, transientRevertCalleeCode)
	}
	setupTransientSuccessCallee := func(db *state.StateDB) {
		setCode(db, transientSuccessCalleeAddr, transientSuccessCalleeCode)
	}

	type dc struct {
		name string
		code []byte
		gas  uint64
	}

	cases := []dc{
		// ============================================================
		//  STOP
		// ============================================================
		{"STOP/happy", op1(STOP), G},
		{"STOP/implicit_end", []byte{}, G},

		// ============================================================
		//  ADD  (a + b)
		// ============================================================
		{"ADD/happy/3+5", binSmall(3, 5, ADD), G},
		{"ADD/happy/0+7", binSmall(0, 7, ADD), G},
		{"ADD/happy/255+1", binSmall(255, 1, ADD), G},
		{"ADD/happy/overflow", binBig(uint256.NewInt(1), maxU256, ADD), G},
		{"ADD/err/underflow_empty", cc(op1(ADD), op1(STOP)), G},
		{"ADD/err/underflow_one", cc(p1(5), op1(ADD), op1(STOP)), G},
		{"ADD/err/out_of_gas", binSmall(3, 5, ADD), 2},

		// ============================================================
		//  MUL  (a * b)
		// ============================================================
		{"MUL/happy/3*5", binSmall(3, 5, MUL), G},
		{"MUL/happy/0*7", binSmall(0, 7, MUL), G},
		{"MUL/happy/1*255", binSmall(1, 255, MUL), G},
		{"MUL/happy/overflow", binBig(uint256.NewInt(2), maxU256, MUL), G},
		{"MUL/err/underflow", cc(op1(MUL), op1(STOP)), G},

		// ============================================================
		//  SUB  (a − b  where a = top)
		// ============================================================
		{"SUB/happy/10-3", binSmall(10, 3, SUB), G},
		{"SUB/happy/5-0", binSmall(5, 0, SUB), G},
		{"SUB/happy/wrap", binSmall(3, 10, SUB), G},
		{"SUB/err/underflow", cc(op1(SUB), op1(STOP)), G},

		// ============================================================
		//  DIV  (a / b)
		// ============================================================
		{"DIV/happy/10/3", binSmall(10, 3, DIV), G},
		{"DIV/happy/255/1", binSmall(255, 1, DIV), G},
		{"DIV/happy/zero_divisor", binSmall(10, 0, DIV), G},
		{"DIV/err/underflow", cc(op1(DIV), op1(STOP)), G},

		// ============================================================
		//  SDIV  (signed a / b)
		// ============================================================
		{"SDIV/happy/10/3", binSmall(10, 3, SDIV), G},
		{"SDIV/happy/neg10/3", binBig(neg10, uint256.NewInt(3), SDIV), G},
		{"SDIV/happy/10/neg3", binBig(uint256.NewInt(10), negThree, SDIV), G},
		{"SDIV/happy/zero_divisor", binSmall(10, 0, SDIV), G},
		{"SDIV/edge/min_div_neg1", binBig(minI256, negOne, SDIV), G},
		{"SDIV/err/underflow", cc(op1(SDIV), op1(STOP)), G},

		// ============================================================
		//  MOD  (a % b)
		// ============================================================
		{"MOD/happy/10%3", binSmall(10, 3, MOD), G},
		{"MOD/happy/zero_mod", binSmall(10, 0, MOD), G},
		{"MOD/happy/7%7", binSmall(7, 7, MOD), G},
		{"MOD/err/underflow", cc(op1(MOD), op1(STOP)), G},

		// ============================================================
		//  SMOD  (signed a % b)
		// ============================================================
		{"SMOD/happy/10%3", binSmall(10, 3, SMOD), G},
		{"SMOD/happy/neg10%3", binBig(neg10, uint256.NewInt(3), SMOD), G},
		{"SMOD/happy/zero_mod", binSmall(10, 0, SMOD), G},
		{"SMOD/err/underflow", cc(op1(SMOD), op1(STOP)), G},

		// ============================================================
		//  ADDMOD  ((a + b) % N)
		// ============================================================
		{"ADDMOD/happy/(10+10)%8", ternSmall(10, 10, 8, ADDMOD), G},
		{"ADDMOD/happy/zero_mod", ternSmall(10, 10, 0, ADDMOD), G},
		{"ADDMOD/happy/overflow",
			withRet(cc(p1(8), p32(maxU256), p1(1), op1(ADDMOD))), G},
		{"ADDMOD/err/underflow", cc(p1(1), p1(2), op1(ADDMOD), op1(STOP)), G},

		// ============================================================
		//  MULMOD  ((a * b) % N)
		// ============================================================
		{"MULMOD/happy/(10*10)%8", ternSmall(10, 10, 8, MULMOD), G},
		{"MULMOD/happy/zero_mod", ternSmall(10, 10, 0, MULMOD), G},
		{"MULMOD/err/underflow", cc(p1(1), p1(2), op1(MULMOD), op1(STOP)), G},

		// ============================================================
		//  EXP  (a ** b)  — non-inlined, goes through default path
		// ============================================================
		{"EXP/happy/2**10", binSmall(2, 10, EXP), G},
		{"EXP/happy/2**0", binSmall(2, 0, EXP), G},
		{"EXP/happy/0**0", binSmall(0, 0, EXP), G},
		{"EXP/happy/0**5", binSmall(0, 5, EXP), G},
		{"EXP/happy/3**3", binSmall(3, 3, EXP), G},
		{"EXP/happy/big_exp", binBig(uint256.NewInt(2), uint256.NewInt(255), EXP), G},
		{"EXP/err/underflow_empty", cc(op1(EXP), op1(STOP)), G},
		{"EXP/err/underflow_one", cc(p1(5), op1(EXP), op1(STOP)), G},
		{"EXP/err/out_of_gas", binSmall(2, 10, EXP), 5},

		// ============================================================
		//  SIGNEXTEND
		// ============================================================
		{"SIGNEXTEND/happy/extend_byte0_0xff", binSmall(0, 0xff, SIGNEXTEND), G},
		{"SIGNEXTEND/happy/extend_byte0_0x7f", binSmall(0, 0x7f, SIGNEXTEND), G},
		{"SIGNEXTEND/happy/noop_byte31", binSmall(31, 42, SIGNEXTEND), G},
		{"SIGNEXTEND/err/underflow", cc(op1(SIGNEXTEND), op1(STOP)), G},

		// ============================================================
		//  LT  (a < b → 1, else 0)
		// ============================================================
		{"LT/happy/true_3<5", binSmall(3, 5, LT), G},
		{"LT/happy/false_5<3", binSmall(5, 3, LT), G},
		{"LT/happy/equal", binSmall(5, 5, LT), G},
		{"LT/err/underflow", cc(op1(LT), op1(STOP)), G},

		// ============================================================
		//  GT  (a > b → 1, else 0)
		// ============================================================
		{"GT/happy/true_5>3", binSmall(5, 3, GT), G},
		{"GT/happy/false_3>5", binSmall(3, 5, GT), G},
		{"GT/happy/equal", binSmall(5, 5, GT), G},
		{"GT/err/underflow", cc(op1(GT), op1(STOP)), G},

		// ============================================================
		//  SLT  (signed a < b)
		// ============================================================
		{"SLT/happy/true_neg<pos", binBig(neg10, uint256.NewInt(5), SLT), G},
		{"SLT/happy/false_pos<neg", binBig(uint256.NewInt(5), neg10, SLT), G},
		{"SLT/happy/equal", binSmall(5, 5, SLT), G},
		{"SLT/err/underflow", cc(op1(SLT), op1(STOP)), G},

		// ============================================================
		//  SGT  (signed a > b)
		// ============================================================
		{"SGT/happy/true_pos>neg", binBig(uint256.NewInt(5), neg10, SGT), G},
		{"SGT/happy/false_neg>pos", binBig(neg10, uint256.NewInt(5), SGT), G},
		{"SGT/happy/equal", binSmall(5, 5, SGT), G},
		{"SGT/err/underflow", cc(op1(SGT), op1(STOP)), G},

		// ============================================================
		//  EQ
		// ============================================================
		{"EQ/happy/equal", binSmall(42, 42, EQ), G},
		{"EQ/happy/not_equal", binSmall(42, 99, EQ), G},
		{"EQ/happy/big_equal", binBig(maxU256, maxU256, EQ), G},
		{"EQ/err/underflow", cc(op1(EQ), op1(STOP)), G},

		// ============================================================
		//  ISZERO
		// ============================================================
		{"ISZERO/happy/zero", unSmall(0, ISZERO), G},
		{"ISZERO/happy/nonzero", unSmall(42, ISZERO), G},
		{"ISZERO/happy/max", unBig(maxU256, ISZERO), G},
		{"ISZERO/err/underflow", cc(op1(ISZERO), op1(STOP)), G},

		// ============================================================
		//  AND / OR / XOR
		// ============================================================
		{"AND/happy/0xff&0x0f", binSmall(0xff, 0x0f, AND), G},
		{"AND/happy/zero", binSmall(0xff, 0, AND), G},
		{"AND/err/underflow", cc(op1(AND), op1(STOP)), G},

		{"OR/happy/0xf0|0x0f", binSmall(0xf0, 0x0f, OR), G},
		{"OR/happy/same", binSmall(0xAB, 0xAB, OR), G},
		{"OR/err/underflow", cc(op1(OR), op1(STOP)), G},

		{"XOR/happy/0xff^0x0f", binSmall(0xff, 0x0f, XOR), G},
		{"XOR/happy/cancel", binSmall(42, 42, XOR), G},
		{"XOR/err/underflow", cc(op1(XOR), op1(STOP)), G},

		// ============================================================
		//  NOT
		// ============================================================
		{"NOT/happy/zero", unSmall(0, NOT), G},
		{"NOT/happy/max", unBig(maxU256, NOT), G},
		{"NOT/happy/one", unSmall(1, NOT), G},
		{"NOT/err/underflow", cc(op1(NOT), op1(STOP)), G},

		// ============================================================
		//  BYTE  (byte(n, x) — nth byte from MSB)
		// ============================================================
		{"BYTE/happy/byte31", binSmall(31, 0xAB, BYTE), G},
		{"BYTE/happy/byte30", binSmall(30, 0xAB, BYTE), G},
		{"BYTE/happy/out_of_range", binSmall(32, 0xAB, BYTE), G},
		{"BYTE/err/underflow", cc(op1(BYTE), op1(STOP)), G},

		// ============================================================
		//  SHL / SHR / SAR
		// ============================================================
		{"SHL/happy/shift1", binSmall(1, 1, SHL), G},
		{"SHL/happy/shift0", binSmall(0xff, 0, SHL), G},
		{"SHL/happy/shift256", binBig(uint256.NewInt(0xff), uint256.NewInt(256), SHL), G},
		{"SHL/err/underflow", cc(op1(SHL), op1(STOP)), G},

		{"SHR/happy/shift1", binSmall(0x80, 1, SHR), G},
		{"SHR/happy/shift0", binSmall(0xff, 0, SHR), G},
		{"SHR/happy/shift256", binBig(uint256.NewInt(0xff), uint256.NewInt(256), SHR), G},
		{"SHR/err/underflow", cc(op1(SHR), op1(STOP)), G},

		{"SAR/happy/positive", binSmall(0x80, 1, SAR), G},
		{"SAR/happy/negative", binBig(negOne, uint256.NewInt(1), SAR), G},
		{"SAR/happy/large_shift_pos", binBig(uint256.NewInt(42), uint256.NewInt(257), SAR), G},
		{"SAR/happy/large_shift_neg", binBig(negOne, uint256.NewInt(257), SAR), G},
		{"SAR/err/underflow", cc(op1(SAR), op1(STOP)), G},

		// ============================================================
		//  CLZ  (count leading zeros) — EIP-7939, non-inlined
		// ============================================================
		{"CLZ/happy/zero", unSmall(0, CLZ), G},
		{"CLZ/happy/one", unSmall(1, CLZ), G},
		{"CLZ/happy/0x80", unSmall(0x80, CLZ), G},
		{"CLZ/happy/0xff", unSmall(0xff, CLZ), G},
		{"CLZ/happy/max", unBig(maxU256, CLZ), G},
		{"CLZ/err/underflow", cc(op1(CLZ), op1(STOP)), G},

		// ============================================================
		//  POP
		// ============================================================
		{"POP/happy", cc(p1(42), p1(99), op1(POP), retSeq), G},
		{"POP/err/underflow", cc(op1(POP), op1(STOP)), G},

		// ============================================================
		//  JUMP
		// ============================================================
		{"JUMP/happy", []byte{
			byte(PUSH1), 4,
			byte(JUMP),
			byte(INVALID),
			byte(JUMPDEST),
			byte(PUSH1), 42,
			byte(PUSH1), 0, byte(MSTORE),
			byte(PUSH1), 32, byte(PUSH1), 0, byte(RETURN),
		}, G},
		{"JUMP/err/invalid_dest", []byte{
			byte(PUSH1), 3,
			byte(JUMP),
			byte(PUSH1), 0,
		}, G},
		{"JUMP/err/underflow", cc(op1(JUMP), op1(STOP)), G},

		// ============================================================
		//  JUMPI
		// ============================================================
		{"JUMPI/happy/taken", []byte{
			byte(PUSH1), 1,
			byte(PUSH1), 6,
			byte(JUMPI),
			byte(INVALID),
			byte(JUMPDEST),
			byte(PUSH1), 42,
			byte(PUSH1), 0, byte(MSTORE),
			byte(PUSH1), 32, byte(PUSH1), 0, byte(RETURN),
		}, G},
		{"JUMPI/happy/not_taken", []byte{
			byte(PUSH1), 0,
			byte(PUSH1), 99,
			byte(JUMPI),
			byte(PUSH1), 42,
			byte(PUSH1), 0, byte(MSTORE),
			byte(PUSH1), 32, byte(PUSH1), 0, byte(RETURN),
		}, G},
		{"JUMPI/err/invalid_dest", []byte{
			byte(PUSH1), 1,
			byte(PUSH1), 3,
			byte(JUMPI),
			byte(PUSH1), 0,
		}, G},
		{"JUMPI/err/underflow_empty", cc(op1(JUMPI), op1(STOP)), G},
		{"JUMPI/err/underflow_one", cc(p1(1), op1(JUMPI), op1(STOP)), G},

		// ============================================================
		//  PC
		// ============================================================
		{"PC/happy/at_offset_0", withRet(op1(PC)), G},
		{"PC/happy/at_offset_3", withRet(cc(p1(99), op1(POP), op1(PC))), G},

		// ============================================================
		//  MSIZE
		// ============================================================
		{"MSIZE/happy/no_memory", withRet(op1(MSIZE)), G},

		// ============================================================
		//  JUMPDEST
		// ============================================================
		{"JUMPDEST/happy", cc(op1(JUMPDEST), p1(42), retSeq), G},

		// ============================================================
		//  INVALID
		// ============================================================
		{"INVALID/error", []byte{byte(INVALID)}, G},

		// ============================================================
		//  PUSH0
		// ============================================================
		{"PUSH0/happy", withRet(op1(PUSH0)), G},

		// ============================================================
		//  PUSH1 special cases
		// ============================================================
		{"PUSH1/happy/42", withRet(p1(42)), G},
		{"PUSH1/happy/0", withRet(p1(0)), G},
		{"PUSH1/happy/255", withRet(p1(255)), G},
		{"PUSH1/edge/truncated", []byte{byte(PUSH1)}, G},

		// ============================================================
		//  PUSH2
		// ============================================================
		{"PUSH2/happy", withRet([]byte{byte(PUSH2), 0xAB, 0xCD}), G},
		{"PUSH2/edge/partial", []byte{byte(PUSH2), 0xAB}, G},
		{"PUSH2/edge/empty", []byte{byte(PUSH2)}, G},

		// ============================================================
		//  PUSH3
		// ============================================================
		{"PUSH3/happy", withRet([]byte{byte(PUSH3), 0x01, 0x02, 0x03}), G},

		// ============================================================
		//  PUSH4
		// ============================================================
		{"PUSH4/happy", withRet([]byte{byte(PUSH4), 0x01, 0x02, 0x03, 0x04}), G},

		// ============================================================
		//  Undefined opcode
		// ============================================================
		{"undefined/0x0C", []byte{0x0C}, G},
		{"undefined/0x0D", []byte{0x0D}, G},

		// ============================================================
		//  SHL/SHR/SAR with shift >= 256 (must clear or sign-extend)
		// ============================================================
		{"SHL/clear/shift_256", binBig(uint256.NewInt(256), uint256.NewInt(0xff), SHL), G},
		{"SHL/clear/shift_1000", binBig(uint256.NewInt(1000), uint256.NewInt(0xff), SHL), G},
		{"SHR/clear/shift_256", binBig(uint256.NewInt(256), uint256.NewInt(0xff), SHR), G},
		{"SHR/clear/shift_1000", binBig(uint256.NewInt(1000), uint256.NewInt(0xff), SHR), G},
		{"SAR/clear/positive_shift_257", binBig(uint256.NewInt(257), uint256.NewInt(42), SAR), G},
		{"SAR/allone/negative_shift_257", binBig(uint256.NewInt(257), negOne, SAR), G},

		// ============================================================
		//  Out-of-gas at gas-flush points (STOP, JUMP, JUMPI, JUMPDEST, INVALID)
		//
		//  The switch dispatch accumulates gas across cheap ops and only
		//  flushes at control-flow boundaries. These test OOG during flush.
		// ============================================================
		{"out_of_gas/mul_tight", binSmall(3, 5, MUL), 5},
		{"out_of_gas/jump", []byte{
			byte(PUSH1), 4, byte(JUMP), byte(INVALID), byte(JUMPDEST), byte(STOP),
		}, 5},

		// OOG when STOP flushes accumulated gas from prior ADDs.
		{"out_of_gas/stop_flush", cc(p1(1), p1(2), op1(ADD), p1(3), op1(ADD), op1(POP), op1(STOP)), 10},
		// OOG when JUMPDEST flushes (PUSH1 + JUMP + JUMPDEST = 8+8+1 gas).
		{"out_of_gas/jumpdest_flush", []byte{
			byte(PUSH1), 4, byte(JUMP), byte(INVALID), byte(JUMPDEST), byte(STOP),
		}, 10},
		// OOG when JUMPI flushes.
		{"out_of_gas/jumpi_flush", []byte{
			byte(PUSH1), 1, byte(PUSH1), 8, byte(JUMPI), byte(INVALID), byte(INVALID), byte(INVALID),
			byte(JUMPDEST), byte(STOP),
		}, 12},
		// OOG when INVALID flushes accumulated gas.
		{"out_of_gas/invalid_flush", cc(p1(1), p1(2), op1(ADD), op1(POP), op1(INVALID)), 10},

		// ============================================================
		//  PUSH3/PUSH4 with truncated code (padding branch)
		// ============================================================
		{"PUSH3/edge/partial_1byte", []byte{byte(PUSH3), 0xAB}, G},
		{"PUSH3/edge/partial_2byte", []byte{byte(PUSH3), 0xAB, 0xCD}, G},
		{"PUSH4/edge/partial_1byte", []byte{byte(PUSH4), 0xAB}, G},
		{"PUSH4/edge/partial_2byte", []byte{byte(PUSH4), 0xAB, 0xCD}, G},
		{"PUSH4/edge/partial_3byte", []byte{byte(PUSH4), 0xAB, 0xCD, 0xEF}, G},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runDiff(t, tc.code, tc.gas)
		})
	}

	// =================================================================
	//  PUSH5 – PUSH32: programmatic happy-path + truncated
	// =================================================================
	for n := 5; n <= 32; n++ {
		opByte := byte(PUSH1) + byte(n-1) // PUSH1=0x60 .. PUSH32=0x7F

		t.Run(fmt.Sprintf("PUSH%d/happy", n), func(t *testing.T) {
			t.Parallel()
			code := make([]byte, 1+n)
			code[0] = opByte
			for i := 0; i < n; i++ {
				code[1+i] = byte(0xA0 + i%16)
			}
			runDiff(t, withRet(code), G)
		})

		t.Run(fmt.Sprintf("PUSH%d/edge/truncated", n), func(t *testing.T) {
			t.Parallel()
			code := make([]byte, 1+n/2) // only half the data bytes
			code[0] = opByte
			for i := 1; i < len(code); i++ {
				code[i] = 0xBB
			}
			runDiff(t, code, G)
		})
	}

	// =================================================================
	//  DUP1 – DUP16: happy path + underflow
	// =================================================================
	for n := 1; n <= 16; n++ {
		dupByte := byte(DUP1) + byte(n-1)

		t.Run(fmt.Sprintf("DUP%d/happy", n), func(t *testing.T) {
			t.Parallel()
			var code []byte
			for i := 0; i < n; i++ {
				code = append(code, byte(PUSH1), byte(i+1))
			}
			code = append(code, dupByte)
			runDiff(t, withRet(code), G)
		})

		t.Run(fmt.Sprintf("DUP%d/err/underflow", n), func(t *testing.T) {
			t.Parallel()
			var code []byte
			for i := 0; i < n-1; i++ {
				code = append(code, byte(PUSH1), byte(i+1))
			}
			code = append(code, dupByte, byte(STOP))
			runDiff(t, code, G)
		})
	}

	// =================================================================
	//  SWAP1 – SWAP16: happy path + underflow
	// =================================================================
	for n := 1; n <= 16; n++ {
		swapByte := byte(SWAP1) + byte(n-1)

		t.Run(fmt.Sprintf("SWAP%d/happy", n), func(t *testing.T) {
			t.Parallel()
			var code []byte
			for i := 0; i <= n; i++ {
				code = append(code, byte(PUSH1), byte(i+1))
			}
			code = append(code, swapByte)
			runDiff(t, withRet(code), G)
		})

		t.Run(fmt.Sprintf("SWAP%d/err/underflow", n), func(t *testing.T) {
			t.Parallel()
			var code []byte
			for i := 0; i < n; i++ {
				code = append(code, byte(PUSH1), byte(i+1))
			}
			code = append(code, swapByte, byte(STOP))
			runDiff(t, code, G)
		})
	}

	// =================================================================
	//  Stack overflow: push 1024 items then try one more
	// =================================================================
	t.Run("stack_overflow/push", func(t *testing.T) {
		t.Parallel()
		code := make([]byte, 0, 1024*2+3)
		for i := 0; i < 1024; i++ {
			code = append(code, byte(PUSH1), 0)
		}
		code = append(code, byte(PUSH1), 0, byte(STOP))
		runDiff(t, code, 1_000_000)
	})

	t.Run("stack_overflow/push0", func(t *testing.T) {
		t.Parallel()
		code := make([]byte, 0, 1025)
		for i := 0; i < 1024; i++ {
			code = append(code, byte(PUSH0))
		}
		code = append(code, byte(PUSH0))
		runDiff(t, code, 1_000_000)
	})

	t.Run("stack_overflow/dup", func(t *testing.T) {
		t.Parallel()
		code := make([]byte, 0, 1024*2+2)
		for i := 0; i < 1024; i++ {
			code = append(code, byte(PUSH1), 0)
		}
		code = append(code, byte(DUP1), byte(STOP))
		runDiff(t, code, 1_000_000)
	})

	t.Run("stack_overflow/pc", func(t *testing.T) {
		t.Parallel()
		code := make([]byte, 0, 1024*2+2)
		for i := 0; i < 1024; i++ {
			code = append(code, byte(PUSH1), 0)
		}
		code = append(code, byte(PC), byte(STOP))
		runDiff(t, code, 1_000_000)
	})

	t.Run("stack_overflow/msize", func(t *testing.T) {
		t.Parallel()
		code := make([]byte, 0, 1024*2+2)
		for i := 0; i < 1024; i++ {
			code = append(code, byte(PUSH1), 0)
		}
		code = append(code, byte(MSIZE), byte(STOP))
		runDiff(t, code, 1_000_000)
	})

	// =================================================================
	//  Compound programs: exercise multiple inlined opcodes together
	// =================================================================
	t.Run("compound/arithmetic_chain", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(
			p1(3), p1(2), p1(1), p1(3), p1(5),
			op1(ADD),
			op1(MUL),
			op1(SUB),
			op1(DIV),
		))
		runDiff(t, code, G)
	})

	t.Run("compound/logic_chain", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(
			p1(0xff), p1(0xf0), p1(0x0f), p1(0xff),
			op1(AND),
			op1(OR),
			op1(XOR),
			op1(ISZERO),
		))
		runDiff(t, code, G)
	})

	t.Run("compound/dup_swap_pop", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(
			p1(20), p1(10),
			op1(DUP2),
			op1(SWAP1),
			op1(POP),
			op1(ADD),
		))
		runDiff(t, code, G)
	})

	t.Run("compound/jump_loop_3_iters", func(t *testing.T) {
		t.Parallel()
		code := []byte{
			byte(PUSH1), 3,
			byte(JUMPDEST),
			byte(DUP1),
			byte(ISZERO),
			byte(PUSH1), 15,
			byte(JUMPI),
			byte(PUSH1), 1,
			byte(SWAP1),
			byte(SUB),
			byte(PUSH1), 2,
			byte(JUMP),
			byte(JUMPDEST),
			byte(PUSH1), 0, byte(MSTORE),
			byte(PUSH1), 32, byte(PUSH1), 0, byte(RETURN),
		}
		runDiff(t, code, G)
	})

	t.Run("compound/shifts_combined", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(
			p1(2), p1(4), p1(0x0F),
			op1(SHL),
			op1(SHR),
		))
		runDiff(t, code, G)
	})

	t.Run("compound/signextend_then_sar", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(
			p1(4), p1(0), p1(0x80),
			op1(SIGNEXTEND),
			op1(SAR),
		))
		runDiff(t, code, G)
	})

	// =================================================================
	//  Non-inlined opcodes through default path (verifies handoff)
	// =================================================================

	// --- Memory ---
	t.Run("default_path/MLOAD", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(42), p1(0), op1(MSTORE), p1(0), op1(MLOAD), retSeq)
		runDiff(t, code, G)
	})

	t.Run("default_path/MSTORE8", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(0xAB), p1(31), op1(MSTORE8), p1(32), p1(0), op1(RETURN))
		runDiff(t, code, G)
	})

	t.Run("default_path/MCOPY", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(42), p1(0), op1(MSTORE),
			p1(32), p1(0), p1(32), op1(MCOPY),
			p1(32), p1(32), op1(RETURN),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/MCOPY/overlap_forward_after_accumulated_fast_gas", func(t *testing.T) {
		t.Parallel()
		code := cc(
			m8(0, 0x00), m8(1, 0x01), m8(2, 0x02), m8(3, 0x03), m8(4, 0x04),
			m8(5, 0x05), m8(6, 0x06), m8(7, 0x07), m8(8, 0x08),
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(8), p1(0), p1(1), op1(MCOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/MCOPY/overlap_backward_after_accumulated_fast_gas", func(t *testing.T) {
		t.Parallel()
		code := cc(
			m8(0, 0x00), m8(1, 0x01), m8(2, 0x02), m8(3, 0x03), m8(4, 0x04),
			m8(5, 0x05), m8(6, 0x06), m8(7, 0x07), m8(8, 0x08),
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(8), p1(1), p1(0), op1(MCOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/MCOPY/zero_length_huge_offsets", func(t *testing.T) {
		t.Parallel()
		code := cc(
			m8(0, 0x11),
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(0), p32(maxU256), p32(maxU256), op1(MCOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiff(t, code, G)
	})

	// --- Closure state (0x30 range) ---
	t.Run("default_path/ADDRESS", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(ADDRESS)), G)
	})

	t.Run("default_path/BALANCE", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(op1(ADDRESS), op1(BALANCE)))
		runDiff(t, code, G)
	})

	t.Run("default_path/ORIGIN", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(ORIGIN)), G)
	})

	t.Run("default_path/CALLER", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(CALLER)), G)
	})

	t.Run("default_path/CALLVALUE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(CALLVALUE)), G)
	})

	t.Run("default_path/CALLDATALOAD", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(p1(0), op1(CALLDATALOAD)))
		runDiffWithInput(t, code, []byte{0xDE, 0xAD}, G)
	})

	t.Run("default_path/CALLDATASIZE", func(t *testing.T) {
		t.Parallel()
		runDiffWithInput(t, withRet(op1(CALLDATASIZE)), []byte{0xDE, 0xAD}, G)
	})

	t.Run("default_path/CALLDATACOPY", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(32), p1(0), p1(0), op1(CALLDATACOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiffWithInput(t, code, []byte{0xDE, 0xAD}, G)
	})

	t.Run("default_path/CODESIZE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(CODESIZE)), G)
	})

	t.Run("default_path/CODECOPY", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(4), p1(0), p1(0), op1(CODECOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/GASPRICE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(GASPRICE)), G)
	})

	t.Run("default_path/EXTCODESIZE", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(op1(ADDRESS), op1(EXTCODESIZE)))
		runDiff(t, code, G)
	})

	t.Run("default_path/EXTCODECOPY", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(4), p1(0), p1(0), op1(ADDRESS),
			op1(EXTCODECOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/RETURNDATASIZE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(RETURNDATASIZE)), G)
	})

	t.Run("default_path/EXTCODEHASH", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(op1(ADDRESS), op1(EXTCODEHASH)))
		runDiff(t, code, G)
	})

	// --- Block operations (0x40 range) ---
	t.Run("default_path/BLOCKHASH", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(p1(0), op1(BLOCKHASH)))
		runDiff(t, code, G)
	})

	t.Run("default_path/COINBASE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(COINBASE)), G)
	})

	t.Run("default_path/TIMESTAMP", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(TIMESTAMP)), G)
	})

	t.Run("default_path/NUMBER", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(NUMBER)), G)
	})

	t.Run("default_path/PREVRANDAO", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(PREVRANDAO)), G)
	})

	t.Run("default_path/GASLIMIT", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(GASLIMIT)), G)
	})

	t.Run("default_path/CHAINID", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(CHAINID)), G)
	})

	t.Run("default_path/SELFBALANCE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(SELFBALANCE)), G)
	})

	t.Run("default_path/BASEFEE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(BASEFEE)), G)
	})

	t.Run("default_path/BLOBHASH", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(p1(0), op1(BLOBHASH)))
		runDiff(t, code, G)
	})

	t.Run("default_path/BLOBBASEFEE", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(BLOBBASEFEE)), G)
	})

	// --- Storage (0x54–0x55) ---
	t.Run("default_path/SSTORE_SLOAD", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(42), p1(0), op1(SSTORE),
			p1(0), op1(SLOAD),
			retSeq,
		)
		runDiff(t, code, G)
	})

	// --- Transient storage (0x5c–0x5d, EIP-1153) ---
	t.Run("default_path/TSTORE_TLOAD", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(42), p1(0), op1(TSTORE),
			p1(0), op1(TLOAD),
			retSeq,
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/DELEGATECALL/TSTORE_persists_on_success", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(0), p1(0), p1(0), p1(0), p1(0x45), p1(0xFF), op1(DELEGATECALL),
			op1(POP),
			p1(0), op1(TLOAD),
			retSeq,
		)
		runDiffWithSetup(t, code, G, setupTransientSuccessCallee)
	})

	t.Run("default_path/DELEGATECALL/TSTORE_revert_rolls_back", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(0), p1(0), p1(0), p1(0), p1(0x44), p1(0xFF), op1(DELEGATECALL),
			op1(POP),
			p1(0), op1(TLOAD),
			retSeq,
		)
		runDiffWithSetup(t, code, G, setupTransientRevertCallee)
	})

	// --- GAS ---
	t.Run("default_path/GAS/simple", func(t *testing.T) {
		t.Parallel()
		runDiff(t, withRet(op1(GAS)), G)
	})

	t.Run("default_path/GAS/after_accumulated_gas", func(t *testing.T) {
		t.Parallel()
		code := withRet(cc(
			p1(1), p1(2), p1(3), p1(4),
			op1(ADD),
			op1(ADD),
			op1(POP),
			op1(GAS),
		))
		runDiff(t, code, G)
	})

	// --- Crypto ---
	t.Run("default_path/KECCAK256", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(32), p1(0), op1(KECCAK256), retSeq)
		runDiff(t, code, G)
	})

	// --- Logging (0xa0–0xa4) ---
	t.Run("default_path/LOG0", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(42), p1(0), op1(MSTORE), p1(32), p1(0), op1(LOG0), op1(STOP))
		runDiff(t, code, G)
	})

	t.Run("default_path/LOG1", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(42), p1(0), op1(MSTORE), p1(0xAA), p1(32), p1(0), op1(LOG1), op1(STOP))
		runDiff(t, code, G)
	})

	t.Run("default_path/LOG2", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(42), p1(0), op1(MSTORE), p1(0xBB), p1(0xAA), p1(32), p1(0), op1(LOG2), op1(STOP))
		runDiff(t, code, G)
	})

	t.Run("default_path/LOG3", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(42), p1(0), op1(MSTORE), p1(0xCC), p1(0xBB), p1(0xAA), p1(32), p1(0), op1(LOG3), op1(STOP))
		runDiff(t, code, G)
	})

	t.Run("default_path/LOG4", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(42), p1(0), op1(MSTORE), p1(0xDD), p1(0xCC), p1(0xBB), p1(0xAA), p1(32), p1(0), op1(LOG4), op1(STOP))
		runDiff(t, code, G)
	})

	// --- REVERT ---
	t.Run("default_path/REVERT/empty", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(0), p1(0), op1(REVERT))
		runDiff(t, code, G)
	})

	t.Run("default_path/REVERT/with_data", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(0xDE), p1(0), op1(MSTORE8), p1(0xAD), p1(1), op1(MSTORE8),
			p1(32), p1(0), op1(REVERT))
		runDiff(t, code, G)
	})

	t.Run("default_path/REVERT/after_accumulated_fast_gas", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(0), p1(0), op1(REVERT),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/REVERT/with_data_after_accumulated_fast_gas", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(0xDE), p1(0), op1(MSTORE8),
			p1(0xAD), p1(1), op1(MSTORE8),
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(2), p1(0), op1(REVERT),
		)
		runDiff(t, code, G)
	})

	t.Run("default_path/CALL/revert_then_returndatacopy", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(1), p1(2), op1(ADD), op1(POP),
			p1(0), p1(0), p1(0), p1(0), p1(0), p1(0x42), p1(0xFF), op1(CALL),
			op1(POP),
			p1(2), p1(0), p1(0), op1(RETURNDATACOPY),
			p1(32), p1(0), op1(RETURN),
		)
		runDiffWithSetup(t, code, G, setupRevertCallee)
	})

	t.Run("default_path/CALL/revert_then_returndatacopy_out_of_bounds", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(0), p1(0), p1(0), p1(0), p1(0), p1(0x42), p1(0xFF), op1(CALL),
			op1(POP),
			p1(3), p1(0), p1(0), op1(RETURNDATACOPY),
			op1(STOP),
		)
		runDiffWithSetup(t, code, G, setupRevertCallee)
	})

	t.Run("default_path/CALL/revert_then_success_clears_returndata", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(0), p1(0), p1(0), p1(0), p1(0), p1(0x42), p1(0xFF), op1(CALL),
			op1(POP),
			p1(0), p1(0), p1(0), p1(0), p1(0), p1(0x43), p1(0xFF), op1(CALL),
			op1(POP),
			withRet(op1(RETURNDATASIZE)),
		)
		runDiffWithSetup(t, code, G, setupRevertAndStopCallees)
	})
}

// Verify PUSH0 stays fork-gated pre-Shanghai.
func TestPreShanghaiForkGate(t *testing.T) {
	preShanghaiConfig := &params.ChainConfig{
		ChainID:             big.NewInt(1),
		HomesteadBlock:      new(big.Int),
		DAOForkBlock:        new(big.Int),
		EIP150Block:         new(big.Int),
		EIP155Block:         new(big.Int),
		EIP158Block:         new(big.Int),
		ByzantiumBlock:      new(big.Int),
		ConstantinopleBlock: new(big.Int),
		PetersburgBlock:     new(big.Int),
		IstanbulBlock:       new(big.Int),
		MuirGlacierBlock:    new(big.Int),
		BerlinBlock:         new(big.Int),
		LondonBlock:         new(big.Int),
		ArrowGlacierBlock:   new(big.Int),
		GrayGlacierBlock:    new(big.Int),
		MergeNetsplitBlock:  new(big.Int),
		// Shanghai disabled.
	}

	code := cc(op1(PUSH0), retSeq)
	gas := uint64(100_000)

	_, _, errSlow := execPathWithConfig(code, nil, gas, false, preShanghaiConfig, nil)
	slowClass := classifyErr(errSlow)
	if slowClass != "invalid_opcode" {
		t.Fatalf("slow path: expected invalid_opcode, got %q (%v)", slowClass, errSlow)
	}

	_, _, errFast := execPathWithConfig(code, nil, gas, true, preShanghaiConfig, nil)
	fastClass := classifyErr(errFast)

	if fastClass != slowClass {
		t.Fatalf("fork gate bug: fast path returned %q but slow path returned %q\n"+
			"  fast err: %v\n  slow err: %v\n"+
			"  runSwitch inlines PUSH0 unconditionally — needs IsShanghai gate",
			fastClass, slowClass, errFast, errSlow)
	}
	t.Logf("both paths correctly reject PUSH0 pre-Shanghai: %q", fastClass)
}

// Verify stack overflow on every inlined PUSH/DUP variant.
// The bug this catches: PUSH/DUP overflow checks must use consistent
// limit values between the switch dispatch and the standard interpreter.
func TestStackOverflowAllPushDupVariants(t *testing.T) {
	t.Parallel()
	const gas = uint64(1_000_000)

	// Fill stack to 1024 with PUSH0, then try one more push/dup.
	fullStack := make([]byte, 1024)
	for i := range fullStack {
		fullStack[i] = byte(PUSH0)
	}

	// Every PUSH variant should overflow on a full stack.
	pushOps := []struct {
		name string
		tail []byte
	}{
		{"PUSH0", []byte{byte(PUSH0)}},
		{"PUSH1", []byte{byte(PUSH1), 0x42}},
		{"PUSH2", []byte{byte(PUSH2), 0x00, 0x42}},
		{"PUSH3", []byte{byte(PUSH3), 0x00, 0x00, 0x42}},
		{"PUSH4", []byte{byte(PUSH4), 0x00, 0x00, 0x00, 0x42}},
		{"PUSH5", []byte{byte(PUSH5), 0, 0, 0, 0, 0x42}},
		{"PUSH32", append([]byte{byte(PUSH32)}, make([]byte, 32)...)},
	}
	for _, tc := range pushOps {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			code := cc(fullStack, tc.tail, op1(STOP))
			runDiff(t, code, gas)
		})
	}

	// DUP1-DUP16: fill stack to 1024, then DUP should overflow.
	for n := 1; n <= 16; n++ {
		t.Run(fmt.Sprintf("DUP%d", n), func(t *testing.T) {
			t.Parallel()
			code := cc(fullStack, op1(OpCode(byte(DUP1)+byte(n-1))), op1(STOP))
			runDiff(t, code, gas)
		})
	}

	// PC and MSIZE on a full stack should also overflow.
	for _, op := range []OpCode{PC, MSIZE} {
		t.Run(op.String(), func(t *testing.T) {
			t.Parallel()
			code := cc(fullStack, op1(op), op1(STOP))
			runDiff(t, code, gas)
		})
	}
}

// Exercise the default fallback path in the switch dispatch for ops
// that aren't inlined (CREATE, SELFDESTRUCT, etc). These go through
// the jumpTable lookup instead of a dedicated case.
func TestDefaultFallbackPath(t *testing.T) {
	t.Parallel()
	const G = uint64(500_000)

	// CREATE: deploy minimal contract (STOP), check both paths agree.
	t.Run("CREATE", func(t *testing.T) {
		t.Parallel()
		// Store STOP opcode at memory[0], then CREATE with 1-byte initcode.
		code := cc(
			p1(byte(STOP)), p1(0), op1(MSTORE8),
			p1(1), p1(0), p1(0), op1(CREATE),
			retSeq,
		)
		runDiff(t, code, G)
	})

	// SELFDESTRUCT: exercises fallback dynamic gas + state mutation.
	t.Run("SELFDESTRUCT", func(t *testing.T) {
		t.Parallel()
		code := cc(op1(ADDRESS), op1(SELFDESTRUCT))
		runDiff(t, code, G)
	})

	// SHA3/KECCAK256 with dynamic memory expansion.
	t.Run("KECCAK256/large_offset", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(64), p1(0), op1(KECCAK256), retSeq)
		runDiff(t, code, G)
	})

	// RETURNDATACOPY out of bounds (requires a prior CALL to set returndata).
	t.Run("RETURNDATACOPY/oob_no_call", func(t *testing.T) {
		t.Parallel()
		// No prior call → returndata is empty → copying 1 byte is out of bounds.
		code := cc(p1(1), p1(0), p1(0), op1(RETURNDATACOPY), op1(STOP))
		runDiff(t, code, G)
	})

	// STATICCALL to an empty address (exercises fallback with dynamic gas).
	t.Run("STATICCALL/empty_target", func(t *testing.T) {
		t.Parallel()
		code := cc(
			p1(0), p1(0), p1(0), p1(0),
			p1(0xEE), // target address (empty account)
			p1(0xFF), // gas
			op1(STATICCALL),
			retSeq,
		)
		runDiff(t, code, G)
	})
}

// Test fallback-path edge cases: stack overflow on non-inlined ops,
// constant gas OOG, dynamic gas OOG, and memory size overflow.
func TestDefaultFallbackEdgeCases(t *testing.T) {
	t.Parallel()
	const G = uint64(500_000)

	// GAS opcode (not inlined) on a full stack → fallback stack overflow.
	t.Run("stack_overflow/GAS", func(t *testing.T) {
		t.Parallel()
		fullStack := make([]byte, 1024)
		for i := range fullStack {
			fullStack[i] = byte(PUSH0)
		}
		code := cc(fullStack, op1(GAS), op1(STOP))
		runDiff(t, code, 1_000_000)
	})

	// MLOAD with zero gas → fallback constant gas OOG.
	t.Run("oog/constant_gas", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(0), op1(MLOAD), op1(STOP))
		runDiff(t, code, 2) // MLOAD costs 3 constant gas
	})

	// MSTORE to a huge offset → fallback dynamic gas OOG from memory expansion.
	t.Run("oog/dynamic_gas_memory", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(0), p32(uint256.NewInt(0xFFFFFF)), op1(MSTORE), op1(STOP))
		runDiff(t, code, 100)
	})

	// MSTORE to max uint256 offset → memory size overflow (GasUintOverflow).
	t.Run("oog/memory_size_overflow", func(t *testing.T) {
		t.Parallel()
		code := cc(p1(0), p32(maxU256), op1(MSTORE), op1(STOP))
		runDiff(t, code, G)
	})

	// Bare JUMPDEST with only 0 gas → OOG on JUMPDEST gas flush.
	t.Run("oog/jumpdest_zero_gas", func(t *testing.T) {
		t.Parallel()
		code := []byte{byte(JUMPDEST), byte(STOP)}
		runDiff(t, code, 0)
	})
}

// Test that fast and slow dispatch return the same stack overflow error text.
func TestStackOverflowErrorMessageParity(t *testing.T) {
	const gas = uint64(1_000_000)

	code := make([]byte, 0, 1025)
	for i := 0; i < 1024; i++ {
		code = append(code, byte(PUSH0))
	}
	code = append(code, byte(PUSH0))

	_, _, errFast := execPathWithConfig(code, nil, gas, true, diffChainConfig, nil)
	_, _, errSlow := execPathWithConfig(code, nil, gas, false, diffChainConfig, nil)

	if fastClass, slowClass := classifyErr(errFast), classifyErr(errSlow); fastClass != "stack_overflow" || slowClass != "stack_overflow" {
		t.Fatalf("expected stack_overflow from both paths, got fast=%q (%v) slow=%q (%v)", fastClass, errFast, slowClass, errSlow)
	}
	if errFast == nil || errSlow == nil {
		t.Fatalf("expected concrete overflow errors, got fast=%v slow=%v", errFast, errSlow)
	}
	if errFast.Error() != errSlow.Error() {
		t.Fatalf("stack overflow message mismatch:\n  fast: %q\n  slow: %q", errFast.Error(), errSlow.Error())
	}
}

// makeEVM creates an EVM with the given code deployed, returning the EVM and
// the contract address. The caller can set interrupt/abort before or during Call.
func makeEVM(code []byte, gas uint64, switchDispatch bool) (*EVM, common.Address) {
	addr := common.BytesToAddress([]byte("contract"))
	caller := common.BytesToAddress([]byte("caller"))
	origin := common.BytesToAddress([]byte("origin"))
	coinbase := common.BytesToAddress([]byte("coinbase"))
	random := common.BigToHash(big.NewInt(99))

	db, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	db.CreateAccount(addr)
	db.SetCode(addr, code, tracing.CodeChangeUnspecified)
	db.CreateAccount(caller)
	db.SetBalance(caller, uint256.NewInt(0x5678), tracing.BalanceChangeUnspecified)
	db.Finalise(true)

	bctx := BlockContext{
		CanTransfer: func(StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(StateDB, common.Address, common.Address, *uint256.Int) {},
		GetHash:     func(n uint64) common.Hash { return common.BigToHash(new(big.Int).SetUint64(n + 0x1000)) },
		Coinbase:    coinbase,
		BlockNumber: big.NewInt(11),
		Time:        22,
		Difficulty:  big.NewInt(33),
		GasLimit:    gas,
		BaseFee:     big.NewInt(44),
		BlobBaseFee: big.NewInt(55),
		Random:      &random,
	}

	rules := diffChainConfig.Rules(bctx.BlockNumber, bctx.Random != nil, bctx.Time)
	db.Prepare(rules, caller, common.Address{}, &addr, ActivePrecompiles(rules), nil)

	evm := NewEVM(bctx, db, diffChainConfig, Config{EnableEVMSwitchDispatch: switchDispatch})
	evm.SetTxContext(TxContext{Origin: origin, GasPrice: uint256.NewInt(66)})
	return evm, addr
}

// runWithInterrupt runs code on a single path and fires the interrupt flag
// after a short delay. Returns the error from EVM.Call.
func runWithInterrupt(code []byte, gas uint64, switchDispatch bool) error {
	evm, addr := makeEVM(code, gas, switchDispatch)
	interrupt := new(atomic.Bool)
	evm.SetInterrupt(interrupt)

	go func() {
		time.Sleep(5 * time.Millisecond)
		interrupt.Store(true)
	}()

	caller := common.BytesToAddress([]byte("caller"))
	_, _, err := evm.Call(caller, addr, nil, gas, uint256.NewInt(0))
	return err
}

// runWithAbort runs code on a single path and fires the abort flag
// after a short delay. Returns the error from EVM.Call.
func runWithAbort(code []byte, gas uint64, switchDispatch bool) error {
	evm, addr := makeEVM(code, gas, switchDispatch)

	go func() {
		time.Sleep(5 * time.Millisecond)
		evm.abort.Store(true)
	}()

	caller := common.BytesToAddress([]byte("caller"))
	_, _, err := evm.Call(caller, addr, nil, gas, uint256.NewInt(0))
	return err
}

// TestInterruptDuringExecution verifies that setting the interrupt flag
// mid-execution stops both paths and both return the same error.
func TestInterruptDuringExecution(t *testing.T) {
	t.Parallel()
	// High enough that the loop can't exhaust gas before the 5ms interrupt
	// fires, even under CPU contention.
	const gas = uint64(10_000_000_000)

	// Infinite loop — only the interrupt can stop it.
	loop := []byte{
		byte(JUMPDEST), // pc=0
		byte(PUSH1), 0, // pc=1,2
		byte(JUMP), // pc=3 → back to 0
	}

	errFast := runWithInterrupt(loop, gas, true)
	errSlow := runWithInterrupt(loop, gas, false)

	if fmt.Sprint(errFast) != fmt.Sprint(errSlow) {
		t.Fatalf("interrupt error mismatch:\n  fast: %v\n  slow: %v", errFast, errSlow)
	}
	if !errors.Is(errFast, ErrInterrupt) {
		t.Fatalf("expected ErrInterrupt, got %v", errFast)
	}
}

// TestAbortDuringJump verifies that setting evm.abort mid-execution causes
// JUMP/JUMPI to stop, and both paths produce the same result.
func TestAbortDuringJump(t *testing.T) {
	t.Parallel()
	// See TestInterruptDuringExecution.
	const gas = uint64(10_000_000_000)

	jumpLoop := []byte{
		byte(JUMPDEST), // pc=0
		byte(PUSH1), 0, // pc=1,2
		byte(JUMP), // pc=3 → back to 0
	}
	jumpiLoop := []byte{
		byte(JUMPDEST), // pc=0
		byte(PUSH1), 1, // pc=1,2 → condition (true)
		byte(PUSH1), 0, // pc=3,4 → dest
		byte(JUMPI), // pc=5 → back to 0
	}

	for _, tc := range []struct {
		name string
		code []byte
	}{
		{"JUMP", jumpLoop},
		{"JUMPI", jumpiLoop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			errFast := runWithAbort(tc.code, gas, true)
			errSlow := runWithAbort(tc.code, gas, false)

			if fmt.Sprint(errFast) != fmt.Sprint(errSlow) {
				t.Fatalf("abort error mismatch:\n  fast: %v\n  slow: %v", errFast, errSlow)
			}
			// abort → errStopToken → Run() converts to nil.
			if errFast != nil {
				t.Fatalf("expected nil error after abort, got %v", errFast)
			}
		})
	}
}
