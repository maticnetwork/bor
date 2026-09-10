// Regression test for parallel-EVM balance resolution across a same-transaction
// contract creation + SELFDESTRUCT + late funding, followed by a CREATE2
// redeploy in a later transaction. Under EIP-6780 the account is deleted at
// the destroying tx's finalisation and value received after the opcode is
// burned, so a later tx must observe a zero balance. BlockSTM V2 must match
// serial execution: the balance delta store is destruction aware, so the
// burned value is not exposed to the redeploy.
package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/core/vm/program"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

const (
	pocSDAmount        = uint64(7)
	pocSDMainnetBlock  = uint64(91_949_701)
	pocSDBlockGasLimit = uint64(30_000_000)
)

var pocSDCoinbase = common.HexToAddress("0xcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbcb")

type pocSDContracts struct {
	factory     common.Address
	controller  common.Address
	beneficiary common.Address
	drain       common.Address
	receiver    common.Address
	target      common.Address
	factoryCode []byte
	controlCode []byte
	attackKey   *ecdsa.PrivateKey
	drainKey    *ecdsa.PrivateKey
}

func TestV2ParallelPostSelfDestructBalanceMatchesSerial(t *testing.T) {
	c := pocSDBuildContracts(t)
	chainConfig := params.BorMainnetChainConfig
	baseFee := big.NewInt(1)
	tdb, root := pocSDBaseState(t, c)

	signer := types.MakeSigner(chainConfig, new(big.Int).SetUint64(pocSDMainnetBlock), 1)
	txs, msgs := pocSDExploitTxs(t, c, signer, baseFee)
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    pocSDCoinbase,
		GasLimit:    pocSDBlockGasLimit,
		BlockNumber: new(big.Int).SetUint64(pocSDMainnetBlock),
		Time:        1,
		BaseFee:     baseFee,
		Random:      &common.Hash{},
	}

	serialDB := pocSDRunSerial(t, tdb, root, txs, msgs, blockCtx, chainConfig)
	serialRoot := serialDB.IntermediateRoot(true)
	if got := serialDB.GetBalance(c.target); !got.IsZero() {
		t.Fatalf("serial target balance=%s, want 0", got)
	}
	if got := serialDB.GetBalance(c.beneficiary); !got.IsZero() {
		t.Fatalf("serial beneficiary balance=%s, want 0", got)
	}

	for _, workers := range []int{1, 2, 8} {
		t.Run(pocSDWorkerName(workers), func(t *testing.T) {
			v2DB := pocSDRunV2(t, tdb, root, txs, msgs, blockCtx, chainConfig, workers)
			if got := v2DB.GetBalance(c.target); !got.IsZero() {
				t.Fatalf("V2 target balance=%s, want 0", got)
			}
			if got := v2DB.GetBalance(c.beneficiary); !got.IsZero() {
				t.Fatalf("V2 beneficiary balance=%s, want 0", got)
			}
			if got := v2DB.IntermediateRoot(true); got != serialRoot {
				t.Fatalf("V2 root %s != serial root %s", got, serialRoot)
			}
		})
	}
}

func pocSDBuildContracts(t testing.TB) pocSDContracts {
	t.Helper()
	attackKey := pocSDKeys(1, 0xa1)[0]
	drainKey, err := crypto.HexToECDSA(
		"000000000000000000000000000000000000000000000000000000000000d00d",
	)
	if err != nil {
		t.Fatal(err)
	}
	c := pocSDContracts{
		factory:     common.HexToAddress("0x00000000000000000000000000000000000000c0"),
		controller:  common.HexToAddress("0x00000000000000000000000000000000000000c1"),
		beneficiary: common.HexToAddress("0x00000000000000000000000000000000000000c2"),
		receiver:    common.HexToAddress("0x00000000000000000000000000000000000000c4"),
		attackKey:   attackKey,
		drainKey:    drainKey,
		drain:       crypto.PubkeyToAddress(drainKey.PublicKey),
	}

	targetRuntime := pocSDTargetRuntime(c.drain)
	targetInit := program.New().
		Push0().Push0().Push0().Push0().
		Op(vm.SELFBALANCE).Push(c.beneficiary).Op(vm.GAS, vm.CALL, vm.POP).
		ReturnViaCodeCopy(targetRuntime).
		Bytes()
	var salt [32]byte
	c.target = crypto.CreateAddress2(c.factory, salt, crypto.Keccak256(targetInit))
	c.factoryCode = program.New().
		Mstore(targetInit, 0).
		Push(0).Push(len(targetInit)).Push(0).Push(0).
		Op(vm.CREATE2, vm.POP, vm.STOP).
		Bytes()
	c.controlCode = program.New().
		Call(nil, c.factory, 0, 0, 0, 0, 0).Op(vm.POP).
		Call(nil, c.target, 0, 0, 0, 0, 0).Op(vm.POP).
		Push(1).Push(0).Op(vm.MSTORE8).
		Call(nil, c.target, pocSDAmount, 0, 1, 0, 0).Op(vm.POP, vm.STOP).
		Bytes()
	return c
}

func pocSDTargetRuntime(drain common.Address) []byte {
	// Selector 0 SELFDESTRUCTs to the attacker drain; selector 1 stops.
	// Program.Push emits the shortest PUSH width, so calculate JUMPDEST.
	width := len(new(big.Int).SetBytes(drain.Bytes()).Bytes())
	if width == 0 {
		width = 1
	}
	jumpDest := 13 + width
	return program.New().
		Push0().Op(vm.CALLDATALOAD).Push(0xf8).Op(vm.SHR).
		Push(1).Op(vm.EQ).Push(jumpDest).Op(vm.JUMPI).
		Push(drain).Op(vm.SELFDESTRUCT, vm.JUMPDEST, vm.STOP).
		Bytes()
}

func pocSDBaseState(t testing.TB, c pocSDContracts) (*triedb.Database, common.Hash) {
	t.Helper()
	tdb := triedb.NewDatabase(rawdb.NewMemoryDatabase(), triedb.HashDefaults)
	sdb, err := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	sdb.AddBalance(
		crypto.PubkeyToAddress(c.attackKey.PublicKey),
		uint256.NewInt(1_000_000_000_000_000_000),
		tracing.BalanceChangeUnspecified,
	)
	sdb.SetCode(c.factory, c.factoryCode, tracing.CodeChangeUnspecified)
	sdb.SetCode(c.controller, c.controlCode, tracing.CodeChangeUnspecified)
	root, err := sdb.Commit(0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatal(err)
	}
	return tdb, root
}

func pocSDExploitTxs(t testing.TB, c pocSDContracts, signer types.Signer, baseFee *big.Int) (types.Transactions, []*Message) {
	t.Helper()
	specs := []struct {
		to    common.Address
		value uint64
	}{
		{c.controller, pocSDAmount},
		{c.factory, 0},
	}
	txs := make(types.Transactions, 0, len(specs))
	msgs := make([]*Message, 0, len(specs))
	for i, spec := range specs {
		to := spec.to
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   signer.ChainID(),
			Nonce:     uint64(i),
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(1_000_000_000),
			Gas:       750_000,
			To:        &to,
			Value:     new(big.Int).SetUint64(spec.value),
		}), signer, c.attackKey)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			t.Fatal(err)
		}
		txs = append(txs, tx)
		msgs = append(msgs, msg)
	}
	return txs, msgs
}

func pocSDRunSerial(t testing.TB, tdb *triedb.Database, root common.Hash, txs types.Transactions, msgs []*Message, blockCtx vm.BlockContext, chainConfig *params.ChainConfig) *state.StateDB {
	t.Helper()
	sdb, err := state.New(root, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	pool := new(GasPool).AddGas(blockCtx.GasLimit)
	var usedGas uint64
	for i, tx := range txs {
		sdb.SetTxContext(tx.Hash(), i)
		evm := vm.NewEVM(blockCtx, sdb, chainConfig, vm.Config{})
		if _, err := ApplyTransactionWithEVM(
			msgs[i], pool, sdb, blockCtx.BlockNumber, common.Hash{},
			blockCtx.Time, tx, &usedGas, evm,
		); err != nil {
			t.Fatalf("serial tx %d: %v", i, err)
		}
	}
	return sdb
}

func pocSDRunV2(t testing.TB, tdb *triedb.Database, root common.Hash, txs types.Transactions, msgs []*Message, blockCtx vm.BlockContext, chainConfig *params.ChainConfig, workers int) *state.StateDB {
	t.Helper()
	base, err := state.New(root, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	finalDB, err := state.New(root, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	tasks := make([]V2Task, len(txs))
	for i := range txs {
		tasks[i] = V2Task{Index: i, Tx: txs[i], Msg: msgs[i]}
	}
	result := ExecuteV2BlockSTM(
		context.Background(), tasks, base, blockstm.NewMVStore(),
		blockstm.NewMVBalanceStore(), blockCtx, common.Hash{}, vm.Config{},
		chainConfig, blockCtx.GasLimit, workers, finalDB, nil,
	)
	if result.PanickedIdx >= 0 {
		t.Fatalf("V2 tx %d panicked", result.PanickedIdx)
	}
	if result.ExecErrIdx >= 0 {
		t.Fatalf("V2 tx %d: %v", result.ExecErrIdx, result.ExecErr)
	}
	return finalDB
}

func pocSDKeys(n int, tag byte) []*ecdsa.PrivateKey {
	keys := make([]*ecdsa.PrivateKey, n)
	for i := range n {
		seed := sha256.Sum256([]byte{tag, byte(i)})
		key, err := crypto.ToECDSA(seed[:])
		if err != nil {
			panic(err)
		}
		keys[i] = key
	}
	return keys
}

func pocSDWorkerName(workers int) string {
	return "workers_" + new(big.Int).SetInt64(int64(workers)).String()
}
