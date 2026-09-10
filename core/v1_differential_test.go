package core

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// ---------------------------------------------------------------------------
// V1 ParallelStateProcessor differential harness.
//
// Drives the V1 BlockSTM path via blockstm.ExecuteParallel + ExecutionTasks,
// compares the resulting state root to serial execution. First unit-level
// exercise of V1 Execute/Settle/setupEVM/runMessage/applyDelayedFee/
// finaliseFinalState/buildReceipt — previously 0% covered outside the
// mainnet integration test.
// ---------------------------------------------------------------------------

type v1Scenario struct {
	name    string
	funding map[common.Address]*uint256.Int // pre-block balances
	txBuild func(chainID *big.Int, keys []*ecdsa.PrivateKey, recipients []common.Address) []*types.Transaction
}

func v1Scenarios() []v1Scenario {
	return []v1Scenario{
		{
			name:    "independent_transfers",
			funding: nil, // filled by harness
			txBuild: func(chainID *big.Int, keys []*ecdsa.PrivateKey, recipients []common.Address) []*types.Transaction {
				return []*types.Transaction{
					mustSignTx(chainID, keys[0], 0, recipients[0], big.NewInt(1e17)),
					mustSignTx(chainID, keys[1], 0, recipients[1], big.NewInt(2e17)),
					mustSignTx(chainID, keys[2], 0, recipients[2], big.NewInt(3e17)),
				}
			},
		},
		{
			name: "same_sender_nonce_chain",
			txBuild: func(chainID *big.Int, keys []*ecdsa.PrivateKey, recipients []common.Address) []*types.Transaction {
				// Three txs from keys[0] with increasing nonces.
				return []*types.Transaction{
					mustSignTx(chainID, keys[0], 0, recipients[0], big.NewInt(1e17)),
					mustSignTx(chainID, keys[0], 1, recipients[1], big.NewInt(1e17)),
					mustSignTx(chainID, keys[0], 2, recipients[2], big.NewInt(1e17)),
				}
			},
		},
		{
			name: "multi_sender_to_same_recipient",
			txBuild: func(chainID *big.Int, keys []*ecdsa.PrivateKey, recipients []common.Address) []*types.Transaction {
				// Three senders all transfer to recipients[0] — commutative
				// balance accumulation.
				return []*types.Transaction{
					mustSignTx(chainID, keys[0], 0, recipients[0], big.NewInt(1e17)),
					mustSignTx(chainID, keys[1], 0, recipients[0], big.NewInt(2e17)),
					mustSignTx(chainID, keys[2], 0, recipients[0], big.NewInt(3e17)),
				}
			},
		},
	}
}

// mustSignTx constructs and signs a DynamicFee transfer tx.
func mustSignTx(chainID *big.Int, key *ecdsa.PrivateKey, nonce uint64, to common.Address, value *big.Int) *types.Transaction {
	signer := types.NewLondonSigner(chainID)
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &to,
		Value:     value,
	}), signer, key)
	if err != nil {
		panic(err)
	}
	return tx
}

// Deterministic test keys — serial and V1 paths must use the same
// senders/recipients so state roots are comparable.
var v1TestKeyHex = []string{
	"a38dbe10a1b51b9e7a6c6d52f87ec2e2e8b0d7a93c1bab7ab4e57b1a47cba8c0",
	"52d98e4f7c68e80a6ec8f5be8b3d0b4a3b8c63d0d5a3fbf1cbac3df0de1dc3b1",
	"b7e1d25ae97a9d5f6e9fdcf8a9e40a3eb7e4f5a81d29f7e19a04a4b6d5f3e28c",
}

func v1TestKeys(t *testing.T) []*ecdsa.PrivateKey {
	t.Helper()
	keys := make([]*ecdsa.PrivateKey, len(v1TestKeyHex))
	for i, hex := range v1TestKeyHex {
		k, err := crypto.HexToECDSA(hex)
		if err != nil {
			t.Fatalf("decode key %d: %v", i, err)
		}
		keys[i] = k
	}
	return keys
}

// newV1TestStateDB creates a fresh StateDB with the deterministic senders
// pre-funded at 1 ETH each. Recipients are fixed addresses too so both
// serial and V1 paths produce comparable state roots.
func newV1TestStateDB(t *testing.T, keys []*ecdsa.PrivateKey) (*state.StateDB, []common.Address, state.Database) {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	db := state.NewDatabase(tdb, nil)
	sdb, err := state.New(common.Hash{}, db)
	if err != nil {
		t.Fatal(err)
	}
	oneEth := new(uint256.Int).Mul(uint256.NewInt(1), uint256.NewInt(1e18))

	recipients := make([]common.Address, len(keys))
	for i, key := range keys {
		addr := crypto.PubkeyToAddress(key.PublicKey)
		sdb.AddBalance(addr, oneEth, 0)
		sdb.SetNonce(addr, 0, 0)
		recipients[i] = common.BigToAddress(big.NewInt(int64(0x1000 + i)))
	}
	root, err := sdb.Commit(0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatal(err)
	}
	sdb, err = state.New(root, db)
	if err != nil {
		t.Fatal(err)
	}
	return sdb, recipients, db
}

// runV1Serial applies each tx sequentially through a single StateDB.
func runV1Serial(t *testing.T, sc v1Scenario, chainConfig *params.ChainConfig) common.Hash {
	t.Helper()
	keys := v1TestKeys(t)
	sdb, recipients, _ := newV1TestStateDB(t, keys)
	baseFee := big.NewInt(875000000)
	coinbase := common.HexToAddress("0xC0")
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		GasLimit:    10000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     baseFee,
	}

	txs := sc.txBuild(chainConfig.ChainID, keys, recipients)
	signer := types.NewLondonSigner(chainConfig.ChainID)

	var usedGas uint64
	for i, tx := range txs {
		sdb.SetTxContext(tx.Hash(), i)
		msg, err := TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			t.Fatalf("tx %d msg: %v", i, err)
		}
		evm := vm.NewEVM(blockCtx, sdb, chainConfig, vm.Config{})
		evm.SetTxContext(NewEVMTxContext(msg))
		result, err := ApplyMessage(evm, msg, NewGasPool(blockCtx.GasLimit))
		if err != nil {
			t.Fatalf("tx %d apply: %v", i, err)
		}
		usedGas += result.UsedGas
		sdb.Finalise(true)
	}
	_ = usedGas
	return sdb.IntermediateRoot(true)
}

// runV1Parallel drives the V1 BlockSTM path via blockstm.ExecuteParallel.
func runV1Parallel(t *testing.T, sc v1Scenario, chainConfig *params.ChainConfig) common.Hash {
	t.Helper()
	keys := v1TestKeys(t)
	sdb, recipients, _ := newV1TestStateDB(t, keys)
	baseFee := big.NewInt(875000000)
	coinbase := common.HexToAddress("0xC0")
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		GasLimit:    10000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     baseFee,
	}

	txs := sc.txBuild(chainConfig.ChainID, keys, recipients)
	signer := types.NewLondonSigner(chainConfig.ChainID)

	header := &types.Header{
		Number:   blockCtx.BlockNumber,
		Time:     blockCtx.Time,
		BaseFee:  baseFee,
		GasLimit: blockCtx.GasLimit,
	}
	_ = header

	// shouldDelayFeeCal=false: V1 uses ApplyMessage (same as serial), so
	// both paths perform EIP-1559 burn + tip inline. The fee-delay path is
	// separately exercised via the V2 flow and the maybeRerunWithoutFeeDelay
	// re-run logic.
	shouldDelayFeeCal := false
	var receipts types.Receipts
	var allLogs []*types.Log
	usedGas := new(uint64)
	jumpDests := vm.NewSyncJumpDestCache()

	tasks := make([]blockstm.ExecTask, 0, len(txs))
	for i, tx := range txs {
		msg, err := TransactionToMessage(tx, signer, baseFee)
		if err != nil {
			t.Fatalf("tx %d msg: %v", i, err)
		}
		task := &ExecutionTask{
			msg:               *msg,
			config:            chainConfig,
			gasLimit:          blockCtx.GasLimit,
			blockNumber:       blockCtx.BlockNumber,
			blockHash:         common.Hash{},
			blockTime:         blockCtx.Time,
			tx:                tx,
			index:             i,
			cleanStateDB:      sdb.Copy(),
			finalStateDB:      sdb,
			evmConfig:         vm.Config{},
			shouldDelayFeeCal: &shouldDelayFeeCal,
			sender:            msg.From,
			totalUsedGas:      usedGas,
			receipts:          &receipts,
			allLogs:           &allLogs,
			coinbase:          coinbase,
			blockContext:      blockCtx,
			jumpDests:         jumpDests,
		}
		tasks = append(tasks, task)
	}

	_, err := blockstm.ExecuteParallel(tasks, false, false, 2, context.Background())
	if err != nil {
		t.Fatalf("ExecuteParallel: %v", err)
	}

	return sdb.IntermediateRoot(true)
}

// TestV1ParallelStateProcessor_Differential runs each scenario through the
// V1 parallel path and the serial path, asserting byte-identical state roots.
func TestV1ParallelStateProcessor_Differential(t *testing.T) {
	chainConfig := params.TestChainConfig
	for _, sc := range v1Scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			serialRoot := runV1Serial(t, sc, chainConfig)
			v1Root := runV1Parallel(t, sc, chainConfig)
			if serialRoot != v1Root {
				t.Fatalf("%s: root mismatch\n  serial = %s\n  v1     = %s",
					sc.name, serialRoot.Hex(), v1Root.Hex())
			}
		})
	}
}
