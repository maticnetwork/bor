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

// TestV2_SelfDestructSelfBeneficiaryPreservesBalance pins a regression where
// V2's SelfDestruct6780 double-drained the contract balance whenever
// SELFDESTRUCT's beneficiary was the contract itself (e.g. CALLCODE → callee
// SELFDESTRUCT(caller), which the EVM resolves to selfdestruct(caller) with
// caller as both subject and beneficiary).
//
// The EVM opcode (opSelfdestruct6780) does SubBalance(addr) + AddBalance(beneficiary)
// itself, then calls SelfDestruct6780. Serial StateDB.SelfDestruct6780 is a
// pure read on the non-newContract branch. V2's ParallelStateDB.SelfDestruct6780
// used to do an extra SubBalance, which (a) double-drained the contract on the
// self-beneficiary case (balance ended at zero instead of being preserved) and
// (b) would have produced a negative-delta in the cross-beneficiary case.
//
// Reproduces the spec-test stCallCodes/callcallcodecall_010_SuicideEnd shape:
// outerA → CALL → outerB → CALLCODE → outerC; outerC ends with
// SELFDESTRUCT(outerB). EIP-6780 (Cancun+) means outerB (existing pre-tx) is
// not deleted, only its balance is touched. Self-beneficiary → balance
// preserved.
func TestV2_SelfDestructSelfBeneficiaryPreservesBalance(t *testing.T) {
	t.Run("Serial", func(t *testing.T) { runSelfDestructSelfBeneficiary(t, false) })
	t.Run("V2", func(t *testing.T) { runSelfDestructSelfBeneficiary(t, true) })
}

func runSelfDestructSelfBeneficiary(t *testing.T, useV2 bool) {
	// Bytecodes lifted from spec-test stCallCodes fixture.
	outerA := common.HexToAddress("0xc31c10fb8301304a36534755c9e38dd553acca54")
	outerB := common.HexToAddress("0xc31c10fb8301304a36534755c9e38dd553accb54")
	outerC := common.HexToAddress("0xc31c10fb8301304a36534755c9e38dd553accc54")
	leaf := common.HexToAddress("0xc31c10fb8301304a36534755c9e38dd553acc954")

	codeA := common.FromHex("6040600060406000600073c31c10fb8301304a36534755c9e38dd553accb54620249f0f160005500")                                           // CALL outerB
	codeB := common.FromHex("6040600060406000600073c31c10fb8301304a36534755c9e38dd553accc54620186a0f260015500")                                           // CALLCODE outerC
	codeC := common.FromHex("6040600060406000600073c31c10fb8301304a36534755c9e38dd553acc95461c350f160025573c31c10fb8301304a36534755c9e38dd553accb54ff00") // CALL leaf, then SELFDESTRUCT(outerB)
	codeLeaf := common.FromHex("600160035500")                                                                                                            // SSTORE(3,1)

	cfg := *params.MergedTestChainConfig
	cfg.Bor = nil
	cfg.ChainID = big.NewInt(1)

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))

	const outerBStartingBalance uint64 = 10_000_000_000 // 10G wei
	sdb.SetCode(outerA, codeA, tracing.CodeChangeUnspecified)
	sdb.AddBalance(outerA, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)
	sdb.SetCode(outerB, codeB, tracing.CodeChangeUnspecified)
	sdb.AddBalance(outerB, uint256.NewInt(outerBStartingBalance), tracing.BalanceChangeUnspecified)
	sdb.SetCode(outerC, codeC, tracing.CodeChangeUnspecified)
	sdb.AddBalance(outerC, uint256.NewInt(outerBStartingBalance), tracing.BalanceChangeUnspecified)
	sdb.SetCode(leaf, codeLeaf, tracing.CodeChangeUnspecified)
	sdb.AddBalance(leaf, uint256.NewInt(outerBStartingBalance), tracing.BalanceChangeUnspecified)

	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)

	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)

	statedb, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(cfg.ChainID)
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(7),
		Gas:       1_000_000,
		To:        &outerA,
		Value:     big.NewInt(0),
	}), signer, key)

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30_000_000,
		BlockNumber: big.NewInt(1),
		Time:        12,
		BaseFee:     big.NewInt(7),
		Random:      &common.Hash{},
	}

	if !useV2 {
		evm := vm.NewEVM(blockCtx, statedb, &cfg, vm.Config{})
		msg, _ := TransactionToMessage(tx, signer, blockCtx.BaseFee)
		evm.SetTxContext(NewEVMTxContext(msg))
		gp := NewGasPool(blockCtx.GasLimit)
		if _, err := ApplyMessage(evm, msg, gp); err != nil {
			t.Fatalf("ApplyMessage: %v", err)
		}
		statedb.Finalise(true)
	} else {
		msg, _ := TransactionToMessage(tx, signer, blockCtx.BaseFee)
		tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}
		readBase := statedb.Copy()
		readBase.EnableConcurrentReads()
		store := blockstm.NewMVStore()
		bals := blockstm.NewMVBalanceStore()
		_ = ExecuteV2BlockSTM(context.Background(), tasks, readBase, store, bals, blockCtx, common.Hash{}, vm.Config{}, &cfg,
			blockCtx.GasLimit, 1, statedb, nil)
		statedb.Finalise(true)
	}

	// Critical assertion: outerB (the SELFDESTRUCT subject WITH self-as-
	// beneficiary) must keep its 10G balance — it pre-existed the tx
	// (EIP-6780 → no delete) and self-transfer is a no-op.
	if got := statedb.GetBalance(outerB).Uint64(); got != outerBStartingBalance {
		t.Errorf("outerB balance: got %d, want %d (preserved by self-beneficiary SELFDESTRUCT)", got, outerBStartingBalance)
	}
}
