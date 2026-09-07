package core

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// TestV2BalanceValidation verifies that speculative balance reads are caught
// by validation when a prior tx modifies the same sender's balance.
//
// Scenario:
//   - Sender has 1 ETH
//   - Tx 0 (nonce 0): sends 0.9 ETH to recipient (drains most of sender's balance)
//   - Tx 1 (nonce 1): sends 0.5 ETH to recipient (should fail — insufficient balance)
//
// In speculative parallel execution, tx 1 may read the sender's original 1 ETH
// balance (before tx 0's SubBalance delta is visible). This stale read causes
// CanTransfer to return true, and the transfer succeeds speculatively.
//
// Validation must catch this: the recorded balance delta (add=0, sub=0) no longer
// matches the real delta (add=0, sub=0.9ETH+gas) after tx 0 finishes. The
// validation failure triggers re-execution of tx 1 with the correct balance,
// where CanTransfer correctly returns false.
func TestV2BalanceValidation(t *testing.T) {
	t.Run("StaleReadCaught", testStaleBalanceReadCaught)
	t.Run("Executor", testExecutorBalanceValidation)
}

// testStaleBalanceReadCaught verifies that a speculative balance read by
// tx 1 is properly recorded, and that ValidateDetailed catches the
// staleness once tx 0 commits a delta on the same address.
//
// Production scenario this models: tx 1 reads contract X's balance for an
// EVM-level BALANCE opcode, then tx 0 commits a delta to X. Tx 1 must be
// re-executed because its balance read is no longer consistent.
//
// The previous version of this test skipped the FlushToMVStore call for
// tx 0 — without it, tx 0's writes never reached MVBalanceStore, so the
// validation re-read returned the same (0, 0) and the test concluded
// (incorrectly) that validation was broken.
func testStaleBalanceReadCaught(t *testing.T) {
	contract := common.HexToAddress("0xCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	oneEth := new(uint256.Int).Mul(uint256.NewInt(1), uint256.NewInt(1e18))

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	sdb.AddBalance(contract, oneEth, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	sb := state.NewSafeBase(base, 0)

	// Tx 1 executes speculatively before tx 0. Reads contract balance
	// without writing to it first (so the read IS recorded for validation).
	pdb1 := state.NewParallelStateDB(1, sb, store, bals)
	pdb1.EnableReadTracking()
	pdb1.Coinbase = common.HexToAddress("0xCB")
	bal := pdb1.GetBalance(contract)
	if bal.Cmp(oneEth) != 0 {
		t.Fatalf("Speculative read: expected %s, got %s", oneEth.ToBig(), bal.ToBig())
	}

	// Tx 0 executes and flushes a delta on the same address.
	pdb0 := state.NewParallelStateDB(0, sb, store, bals)
	pdb0.EnableReadTracking()
	pdb0.Coinbase = common.HexToAddress("0xCB")
	half := new(uint256.Int).Div(oneEth, uint256.NewInt(2))
	pdb0.SubBalance(contract, half, 0)
	pdb0.FlushToMVStore()

	// Tx 1's recorded read (add=0, sub=0) no longer matches the current
	// state (add=0, sub=0.5 ETH). Validation must fail with FailKey="balance".
	res := pdb1.ValidateDetailed()
	if res.Valid {
		t.Fatal("tx 1 validation should FAIL — contract balance read is now stale")
	}
	if res.FailKey != "balance" {
		t.Fatalf("expected FailKey=balance, got %q", res.FailKey)
	}
	t.Logf("validation correctly caught stale balance read: %s", res.FailKey)
}

// testExecutorBalanceValidation runs the full BlockSTM executor with two txs
// from the same sender where the second tx cannot afford the transfer after
// the first. Verifies end-to-end correctness regardless of execution order.
func testExecutorBalanceValidation(t *testing.T) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")

	// Sender has 1 ETH
	oneEth := new(uint256.Int).Mul(uint256.NewInt(1), uint256.NewInt(1e18))
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	sdb.AddBalance(sender, oneEth, 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	chainConfig := params.TestChainConfig
	baseFee := big.NewInt(875000000) // 0.875 gwei
	signer := types.NewLondonSigner(chainConfig.ChainID)

	// Tx 0: sender sends 0.9 ETH (nonce 0)
	tx0, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainConfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(9e17), // 0.9 ETH
	}), signer, key)

	// Tx 1: sender sends 0.5 ETH (nonce 1) — should fail value transfer
	tx1, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainConfig.ChainID,
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(5e17), // 0.5 ETH
	}), signer, key)

	msg0, _ := TransactionToMessage(tx0, signer, baseFee)
	msg1, _ := TransactionToMessage(tx1, signer, baseFee)

	tasks := []V2Task{
		{Index: 0, Tx: tx0, Msg: msg0},
		{Index: 1, Tx: tx1, Msg: msg1},
	}

	coinbase := common.HexToAddress("0xCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
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

	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	result := ExecuteV2BlockSTM(context.Background(), tasks, base, store, bals, blockCtx, common.Hash{}, vm.Config{}, chainConfig, 10000000, 2, nil, nil)

	t.Logf("Execution: execs=%d vfails=%d", result.ExecCount, result.VFailCount)

	// Both pdbs should exist
	for i, pdb := range result.Pdbs {
		if pdb == nil {
			t.Fatalf("tx %d: pdb is nil", i)
		}
		t.Logf("tx %d: %d balance ops", i, len(pdb.BalanceOps))
	}

	// Settle and verify final state
	finalDB := base.Copy()
	for _, pdb := range result.Pdbs {
		finalDB.SetTxContext(common.Hash{}, pdb.TxIndex)
		pdb.SettleTo(finalDB)
	}

	recipientFinal := finalDB.GetBalance(recipient)
	senderFinal := finalDB.GetBalance(sender)

	t.Logf("Final balances:")
	t.Logf("  sender:    %s", senderFinal.ToBig().String())
	t.Logf("  recipient: %s", recipientFinal.ToBig().String())

	// Recipient should have EXACTLY 0.9 ETH (only tx 0's transfer succeeds).
	// Tx 1's transfer of 0.5 ETH fails because sender's balance is too low.
	expectedRecipient := new(uint256.Int).Mul(uint256.NewInt(9), uint256.NewInt(1e17))
	if recipientFinal.Cmp(expectedRecipient) != 0 {
		t.Errorf("recipient balance = %s, expected %s (0.9 ETH)",
			recipientFinal.ToBig(), expectedRecipient.ToBig())
	}

	// Sender's final balance = 1 ETH - 0.9 ETH - gas(tx0)
	// Tx 1 fails entirely in buyGas (balance < gasLimit*gasFeeCap + value),
	// so it consumes NO gas and makes no state changes.
	gasPerTx := new(uint256.Int).Mul(uint256.NewInt(21000), uint256.NewInt(875000001))
	expectedSender := new(uint256.Int).Set(oneEth)
	expectedSender.Sub(expectedSender, new(uint256.Int).Mul(uint256.NewInt(9), uint256.NewInt(1e17))) // - 0.9 ETH
	expectedSender.Sub(expectedSender, gasPerTx)                                                      // - gas(tx0) only

	if senderFinal.Cmp(expectedSender) != 0 {
		t.Errorf("sender balance = %s, expected %s",
			senderFinal.ToBig(), expectedSender.ToBig())
	}

	// Verify no uint256 underflow (balance should be reasonable, not huge)
	if senderFinal.Cmp(oneEth) > 0 {
		t.Errorf("sender balance overflow: %s", senderFinal.ToBig())
	}
}

// TestV2GasDeterminism verifies that V2 parallel execution produces
// identical gas across multiple runs. With DeferMVWrites=true, intermediate
// values are never visible, so gas is deterministic regardless of scheduling.
func TestV2GasDeterminism(t *testing.T) {
	blocks, diskdb := loadEmbeddedBlocks(t)
	if len(blocks) == 0 {
		t.Skip("no embedded blocks available")
	}

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	runBlock := func(bd testBlockData) (uint64, error) {
		author := getAuthor(config, bd.witness.Header())
		memdb := bd.witness.MakeHashDB(diskdb)
		sdb, err := state.New(bd.witness.Root(), state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
		if err != nil {
			return 0, err
		}
		hc := &benchHeaderChain{config: config, chainDb: memdb,
			headerCache: lru.NewCache[common.Hash, *types.Header](256), engine: engine}
		bc := &BlockChain{hc: &HeaderChain{config: config, chainDb: memdb,
			headerCache: lru.NewCache[common.Hash, *types.Header](256), engine: engine}}

		res, err := NewV2StateProcessor(hc, bc, 16).Process(bd.block, sdb, vm.Config{}, &author, context.Background())
		if err != nil {
			return 0, err
		}
		return res.GasUsed, nil
	}

	// Pick a block with enough txs and a complete embedded witness fixture.
	// V2 now surfaces base-read errors instead of continuing with zero-ish
	// values, so incomplete fixtures are skipped rather than used for gas
	// determinism assertions.
	var bd testBlockData
	var expectedGas uint64
	candidates, incomplete := 0, 0
	for _, b := range blocks {
		if len(b.block.Transactions()) <= 50 {
			continue
		}
		candidates++
		gas, err := runBlock(b)
		if err != nil {
			if strings.Contains(err.Error(), "v2: base read: missing trie node") {
				incomplete++
				continue
			}
			t.Fatal(err)
		}
		bd = b
		expectedGas = gas
		break
	}
	if candidates == 0 {
		t.Skip("no block with >50 txs")
	}
	if bd.block == nil {
		// Every candidate hit a base read miss. Skipping here would let a
		// regression that makes V2 report missing nodes on complete fixtures
		// pass as a green CI run, so fail instead.
		t.Fatalf("all %d candidate fixtures failed V2 base reads; expected at least one complete embedded witness", incomplete)
	}

	for run := 1; run < 5; run++ {
		gas, err := runBlock(bd)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if gas != expectedGas {
			t.Errorf("run %d: gas %d != expected %d (non-deterministic!)", run, gas, expectedGas)
		}
	}
}

// A transaction involving a selfdestruct and a transfer makes BlockSTM V2 execution differ from
// serial execution. Both state roots are identical, but the receipt hash of the BlockSTM V2 differs.
// The receipt hash differs because a wrong log is emitted in the BlockSTM V2 case. This is Bor specific
// transfer (LogTransfer event) emitted for native value transfers.

// Note that the bloom matches, because
// serial      -> LogTransfer: token=0x0000000000000000000000000000000000001010 from=0x000000000000000000000000000000000000aaaa to=0x000000000000000000000000000000000000BbBB amount=10 input1=10 input2=10 output1=0 output2=20
// BlockSTM V2 -> LogTransfer: token=0x0000000000000000000000000000000000001010 from=0x000000000000000000000000000000000000aaaa to=0x000000000000000000000000000000000000BbBB amount=10 input1=10 input2=0 output1=0 output2=10

// Run:
// go test ./core/ -run TestV2_SelfDestructTransferLog_MispairsWithSerial -v
func TestV2_SelfDestructTransferLog_MispairsWithSerial(t *testing.T) {
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	gen, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	cfg := *params.MergedTestChainConfig

	// x Smart Contract. Simple code with two paths. Selft destruct to Y or call z contract. This contract is the entry point.
	//   if (msg.sender == Z) { selfdestruct(payable(Y)); }
	//   else { Z.call{value: 0}(""); payable(Y).transfer(A); }
	x := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	xCode := common.FromHex("3373000000000000000000000000000000000000cccc146056575f5f5f5f5f73000000000000000000000000000000000000cccc5af1505f5f5f5f600a73000000000000000000000000000000000000bbbb5af150005b73000000000000000000000000000000000000bbbbff")

	// y is just an EOA. Hardcoded above.
	// 0x000000000000000000000000000000000000bbbb

	// z Smart Contract. Calls X and selfdestructs itself.
	//   X.call{value: 0}(""); selfdestruct(payable(X));
	z := common.HexToAddress("0x000000000000000000000000000000000000cccc")
	zCode := common.FromHex("5f5f5f5f5f73000000000000000000000000000000000000aaaa5af15073000000000000000000000000000000000000aaaaff")

	weiToTransfer := uint64(10)
	gen.SetCode(x, xCode, tracing.CodeChangeUnspecified)
	gen.AddBalance(x, uint256.NewInt(weiToTransfer), tracing.BalanceChangeUnspecified)
	gen.SetCode(z, zCode, tracing.CodeChangeUnspecified)
	gen.AddBalance(z, uint256.NewInt(weiToTransfer), tracing.BalanceChangeUnspecified)

	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	gen.AddBalance(sender, uint256.NewInt(1e18), tracing.BalanceChangeUnspecified)

	root, _ := gen.Commit(0, false, false)
	tdb.Commit(root, false)

	signer := types.NewLondonSigner(cfg.ChainID)

	// Note x contract is the entry point
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(7),
		Gas:       1_000_000,
		To:        &x,
		Value:     big.NewInt(0),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, big.NewInt(7))

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0x00000000000000000000000000000000c0ffee00"),
		GasLimit:    30_000_000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(7),
		Random:      &common.Hash{},
	}

	// Serial Execution
	serialDB, _ := state.New(root, state.NewDatabase(tdb, nil))
	serialDB.SetTxContext(tx.Hash(), 0)
	serialEVM := vm.NewEVM(blockCtx, serialDB, &cfg, vm.Config{})
	usedGas := uint64(0)
	serialReceipt, err := ApplyTransactionWithEVM(msg, new(GasPool).AddGas(blockCtx.GasLimit),
		serialDB, blockCtx.BlockNumber, common.Hash{}, blockCtx.Time, tx, &usedGas, serialEVM)
	if err != nil {
		t.Fatalf("serial ApplyTransactionWithEVM: %v", err)
	}

	// BlockSTM V2 Execution
	v2DB, _ := state.New(root, state.NewDatabase(tdb, nil))
	v2DB.SetTxContext(tx.Hash(), 0)
	readBase := v2DB.Copy()
	readBase.EnableConcurrentReads()
	res := ExecuteV2BlockSTM(context.Background(), []V2Task{{Index: 0, Tx: tx, Msg: msg}},
		readBase, blockstm.NewMVStore(), blockstm.NewMVBalanceStore(),
		blockCtx, common.Hash{}, vm.Config{}, &cfg, blockCtx.GasLimit, 1, v2DB, nil)
	v2DB.Finalise(true)

	serialLog := decodeTransferLog(t, serialReceipt)
	v2Log := decodeTransferLog(t, res.Receipts[0])

	t.Logf("serial LogTransfer: %s", serialLog)
	t.Logf("v2     LogTransfer: %s", v2Log)

	if serialLog.data() != v2Log.data() {
		t.Errorf("V2 BlockSTM receipt diverges from serial for the same transaction")
	}
}

// Bor's native LogTransfer event
const logTransferABI = `[{"anonymous":false,"name":"LogTransfer","type":"event","inputs":[
  {"indexed":true, "name":"token",  "type":"address"},
  {"indexed":true, "name":"from",   "type":"address"},
  {"indexed":true, "name":"to",     "type":"address"},
  {"indexed":false,"name":"amount", "type":"uint256"},
  {"indexed":false,"name":"input1", "type":"uint256"},
  {"indexed":false,"name":"input2", "type":"uint256"},
  {"indexed":false,"name":"output1","type":"uint256"},
  {"indexed":false,"name":"output2","type":"uint256"}]}]`

// transferLog is a decoded LogTransfer. Official Polygon Bor native transfers.
type transferLog struct {
	Token, From, To                          common.Address
	Amount, Input1, Input2, Output1, Output2 *big.Int
}

func (l transferLog) String() string {
	return fmt.Sprintf("token=%s from=%s to=%s amount=%d input1=%d input2=%d output1=%d output2=%d",
		l.Token.Hex(), l.From.Hex(), l.To.Hex(), l.Amount, l.Input1, l.Input2, l.Output1, l.Output2)
}

// data returns the 5 non-indexed words for a simple (==) comparison.
func (l transferLog) data() [5]uint64 {
	return [5]uint64{l.Amount.Uint64(), l.Input1.Uint64(), l.Input2.Uint64(), l.Output1.Uint64(), l.Output2.Uint64()}
}

// decodeTransferLog finds the Bor LogTransfer in a receipt and ABI-decodes it
// (indexed topics + data) into named fields via bind.UnpackLog -- no manual
// byte slicing.
func decodeTransferLog(t *testing.T, r *types.Receipt) transferLog {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(logTransferABI))
	if err != nil {
		t.Fatalf("parse LogTransfer ABI: %v", err)
	}
	bc := bind.NewBoundContract(feeAddress, parsed, nil, nil, nil)
	for _, l := range r.Logs {
		if l.Address == feeAddress && len(l.Topics) > 0 && l.Topics[0] == transferLogSig {
			var e transferLog
			if err := bc.UnpackLog(&e, "LogTransfer", *l); err != nil {
				t.Fatalf("UnpackLog: %v", err)
			}
			return e
		}
	}
	t.Fatalf("no Bor LogTransfer (0x%x at %s) found in receipt %s", transferLogSig, feeAddress, r.TxHash)
	return transferLog{}
}
