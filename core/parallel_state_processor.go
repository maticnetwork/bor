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

package core

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"sync"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
)

// errV2TracerUnsupported is returned when V2 receives a vm.Config with a
// non-nil Tracer. Tracer hooks aren't safe across concurrent V2 workers;
// ProcessBlock's fallback runs the serial processor instead.
var errV2TracerUnsupported = errors.New("v2: tracer not supported on parallel path")

type ParallelEVMConfig struct {
	Enable               bool
	SpeculativeProcesses int
	Enforce              bool
}

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type ParallelStateProcessor struct {
	chain ChainContext // Chain context interface
	bc    *BlockChain  // Canonical block chain
}

// NewParallelStateProcessor initialises a new StateProcessor.
func NewParallelStateProcessor(chain ChainContext, bc *BlockChain) *ParallelStateProcessor {
	return &ParallelStateProcessor{
		chain: chain,
		bc:    bc,
	}
}

type ExecutionTask struct {
	msg    Message
	config *params.ChainConfig

	gasLimit                   uint64
	blockNumber                *big.Int
	blockHash                  common.Hash
	blockTime                  uint64
	tx                         *types.Transaction
	index                      int
	statedb                    *state.StateDB // State database that stores the modified values after tx execution.
	cleanStateDB               *state.StateDB // A clean copy of the initial statedb. It should not be modified.
	finalStateDB               *state.StateDB // The final statedb.
	header                     *types.Header
	blockChain                 *BlockChain
	evmConfig                  vm.Config
	result                     *ExecutionResult
	shouldDelayFeeCal          *bool
	shouldRerunWithoutFeeDelay bool
	sender                     common.Address
	totalUsedGas               *uint64
	receipts                   *types.Receipts
	allLogs                    *[]*types.Log
	coinbase                   common.Address
	blockContext               vm.BlockContext
	jumpDests                  vm.JumpDestCache
}

func (task *ExecutionTask) Execute(mvh *blockstm.MVHashMap, incarnation int) (err error) {
	evm := task.setupEVM(mvh, incarnation)

	defer func() {
		if r := recover(); r != nil {
			log.Debug("Recovered from EVM failure.", "Error:", r)
			err = blockstm.ErrExecAbortError{Dependency: task.statedb.DepTxIndex()}
		}
	}()

	if err = task.runMessage(evm); err != nil {
		return err
	}

	if task.statedb.HadInvalidRead() || err != nil {
		err = blockstm.ErrExecAbortError{Dependency: task.statedb.DepTxIndex(), OriginError: err}
		return
	}
	if mvh2 := task.statedb.GetMVHashmap(); mvh2 == nil || !mvh2.SkipFinalise {
		task.statedb.Finalise(task.config.IsEIP158(task.blockNumber))
	}
	return
}

// setupEVM prepares task.statedb and constructs an EVM bound to the
// message context.
func (task *ExecutionTask) setupEVM(mvh *blockstm.MVHashMap, incarnation int) *vm.EVM {
	task.statedb = task.cleanStateDB.Copy()
	task.statedb.SetTxContext(task.tx.Hash(), task.index)
	task.statedb.SetMVHashmap(mvh)
	task.statedb.SetIncarnation(incarnation)
	evm := vm.NewEVM(task.blockContext, task.statedb, task.config, task.evmConfig)
	if task.jumpDests != nil {
		evm.SetJumpDestCache(task.jumpDests)
	}
	evm.SetTxContext(NewEVMTxContext(&task.msg))
	return evm
}

// runMessage applies the tx via the EVM. When fee calculation is delayed,
// it also detects whether the tx read coinbase or burnt-contract balances
// and sets shouldRerunWithoutFeeDelay so the outer processor re-runs.
func (task *ExecutionTask) runMessage(evm *vm.EVM) error {
	if !*task.shouldDelayFeeCal {
		var err error
		task.result, err = ApplyMessage(evm, &task.msg, new(GasPool).AddGas(task.gasLimit))
		return err
	}
	var err error
	task.result, err = ApplyMessageNoFeeBurnOrTip(evm, task.msg, new(GasPool).AddGas(task.gasLimit))
	if task.result == nil || err != nil {
		return blockstm.ErrExecAbortError{Dependency: task.statedb.DepTxIndex(), OriginError: err}
	}
	task.detectFeeBalanceReads()
	return nil
}

// detectFeeBalanceReads flags the task for re-execution without fee delay
// if the tx observed the coinbase or burnt-contract balance, since the
// fee-delayed result is not consistent with that observation.
func (task *ExecutionTask) detectFeeBalanceReads() {
	reads := task.statedb.MVReadMap()
	if _, ok := reads[blockstm.NewSubpathKey(task.blockContext.Coinbase, state.BalancePath)]; ok {
		log.Info("Coinbase is in MVReadMap", "address", task.blockContext.Coinbase)
		task.shouldRerunWithoutFeeDelay = true
	}
	if _, ok := reads[blockstm.NewSubpathKey(task.result.BurntContractAddress, state.BalancePath)]; ok {
		log.Info("BurntContractAddress is in MVReadMap", "address", task.result.BurntContractAddress)
		task.shouldRerunWithoutFeeDelay = true
	}
}

func (task *ExecutionTask) MVReadList() []blockstm.ReadDescriptor {
	return task.statedb.MVReadList()
}

func (task *ExecutionTask) MVWriteList() []blockstm.WriteDescriptor {
	return task.statedb.MVWriteList()
}

func (task *ExecutionTask) MVFullWriteList() []blockstm.WriteDescriptor {
	return task.statedb.MVFullWriteList()
}

func (task *ExecutionTask) Sender() common.Address {
	return task.sender
}

func (task *ExecutionTask) Hash() common.Hash {
	return task.tx.Hash()
}

func (task *ExecutionTask) Dependencies() []int {
	return nil
}

func (task *ExecutionTask) Settle() {
	if task.statedb != nil {
		if mvh := task.statedb.GetMVHashmap(); mvh != nil && mvh.SkipSettle {
			return
		}
	}
	// Disable MVHashMap during settlement so Get*/Set* calls bypass MVRead/MVWrite.
	// Safe because finalStateDB is exclusively owned by the settle goroutine —
	// workers operate on their own task.statedb (a Copy of cleanStateDB).
	mvhm := task.finalStateDB.GetMVHashmap()
	task.finalStateDB.SetMVHashmap(nil)

	task.finalStateDB.SetTxContext(task.tx.Hash(), task.index)
	coinbaseBalance := task.finalStateDB.GetBalance(task.coinbase)

	task.finalStateDB.ApplyMVWriteSet(task.statedb.MVWriteList())
	for _, l := range task.statedb.GetLogs(task.tx.Hash(), task.blockNumber.Uint64(), task.blockHash, task.blockTime) {
		task.finalStateDB.AddLog(l)
	}
	if *task.shouldDelayFeeCal {
		task.applyDelayedFee(coinbaseBalance)
	}
	for k, v := range task.statedb.Preimages() {
		task.finalStateDB.AddPreimage(k, v)
	}

	root := task.finaliseFinalState()

	// Restore MVHashMap after settlement is complete.
	task.finalStateDB.SetMVHashmap(mvhm)
	*task.totalUsedGas += task.result.UsedGas

	receipt := task.buildReceipt(root)
	*task.receipts = append(*task.receipts, receipt)
	*task.allLogs = append(*task.allLogs, receipt.Logs...)
}

// applyDelayedFee mints the fee burn (post-London) and tip on finalStateDB,
// emitting the (deprecated) fee transfer log so receipts match the serial
// path. Called only when the executing path delayed fee computation.
func (task *ExecutionTask) applyDelayedFee(coinbaseBalance *uint256.Int) {
	if task.config.IsLondon(task.blockNumber) && task.result.FeeBurnt != nil {
		// FeeBurnt is only populated for Bor-enabled chains; non-Bor configs
		// (Ethereum spec tests) implicitly burn the base fee with no credit.
		task.finalStateDB.AddBalance(task.result.BurntContractAddress,
			cmath.BigIntToUint256Int(task.result.FeeBurnt), tracing.BalanceChangeTransfer)
	}
	task.finalStateDB.AddBalance(task.coinbase,
		cmath.BigIntToUint256Int(task.result.FeeTipped), tracing.BalanceChangeTransfer)
	output1 := new(big.Int).SetBytes(task.result.SenderInitBalance.Bytes())
	output2 := new(big.Int).SetBytes(coinbaseBalance.Bytes())
	// Deprecated transfer log; do not use going forward — parameters
	// won't be updated for future EIP1559 changes. Bor-specific; skipped
	// on non-Bor chain configs.
	if task.config.Bor != nil {
		AddFeeTransferLog(
			task.finalStateDB,
			task.msg.From, task.coinbase,
			task.result.FeeTipped, task.result.SenderInitBalance,
			coinbaseBalance.ToBig(),
			output1.Sub(output1, task.result.FeeTipped),
			output2.Add(output2, task.result.FeeTipped),
		)
	}
}

// finaliseFinalState commits pending writes on finalStateDB and returns
// the post-state root for the receipt (empty post-Byzantium).
func (task *ExecutionTask) finaliseFinalState() []byte {
	if task.config.IsByzantium(task.blockNumber) {
		task.finalStateDB.Finalise(true)
		return nil
	}
	return task.finalStateDB.IntermediateRoot(task.config.IsEIP158(task.blockNumber)).Bytes()
}

// buildReceipt builds the Receipt for the settled tx with logs, bloom,
// status, contract address (if applicable), and block context.
func (task *ExecutionTask) buildReceipt(postState []byte) *types.Receipt {
	receipt := &types.Receipt{
		Type:              task.tx.Type(),
		PostState:         postState,
		CumulativeGasUsed: *task.totalUsedGas,
		TxHash:            task.tx.Hash(),
		GasUsed:           task.result.UsedGas,
		BlockHash:         task.blockHash,
		BlockNumber:       task.blockNumber,
		TransactionIndex:  uint(task.finalStateDB.TxIndex()),
	}
	if task.result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	if task.msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(task.msg.From, task.tx.Nonce())
	}
	receipt.Logs = task.finalStateDB.GetLogs(task.tx.Hash(), task.blockNumber.Uint64(), task.blockHash, task.blockTime)
	receipt.Bloom = types.CreateBloom(receipt)
	return receipt
}

var parallelizabilityTimer = metrics.NewRegisteredTimer("block/parallelizability", nil)

// UseBatchExecutor switches to the speculative batch executor.
var UseBatchExecutor = false

// chainConfig returns the chain configuration.
func (p *ParallelStateProcessor) chainConfig() *params.ChainConfig {
	return p.chain.Config()
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
// nolint:gocognit
// maybeRerunWithoutFeeDelay re-executes the block when any task observed
// the coinbase or burnt-contract balance during execution (which makes the
// fee-delayed result invalid). Returns (err, rerun) where rerun=true means
// a re-run happened (with err being its outcome).
func (p *ParallelStateProcessor) maybeRerunWithoutFeeDelay(tasks []blockstm.ExecTask,
	statedb, backupStateDB *state.StateDB, shouldDelayFeeCal *bool,
	allLogs *[]*types.Log, receipts *types.Receipts, usedGas **uint64,
	interruptCtx context.Context) (error, bool) {
	needsRerun := false
	for _, task := range tasks {
		if task.(*ExecutionTask).shouldRerunWithoutFeeDelay {
			needsRerun = true
			break
		}
	}
	if !needsRerun {
		return nil, false
	}
	*shouldDelayFeeCal = false
	*statedb = *backupStateDB //nolint:govet // single-threaded V1 rerun path; backup copy is a snapshot, not aliased
	*allLogs = []*types.Log{}
	*receipts = types.Receipts{}
	*usedGas = new(uint64)
	for _, t := range tasks {
		et := t.(*ExecutionTask)
		et.finalStateDB = statedb
		et.allLogs = allLogs
		et.receipts = receipts
		et.totalUsedGas = *usedGas
	}
	_, err := blockstm.ExecuteParallel(tasks, false, false, p.bc.parallelSpeculativeProcesses, interruptCtx)
	return err, true
}

// prewarmReaderCache pre-warms the shared reader cache with sender/recipient
// accounts so concurrent workers don't miss-and-race on the same first reads.
// Workers share the reader (statedb.Copy passes it by reference).
func prewarmReaderCache(statedb *state.StateDB, block *types.Block, signer types.Signer) {
	reader := statedb.Reader()
	seen := make(map[common.Address]struct{}, len(block.Transactions())*2)
	for _, tx := range block.Transactions() {
		if tx.Type() == types.StateSyncTxType {
			continue
		}
		warmTxAccounts(reader, signer, tx, seen)
	}
}

// warmTxAccounts pre-loads the sender and (if non-nil) recipient of tx
// into the reader cache. seen prevents redundant work across the block.
func warmTxAccounts(reader state.Reader, signer types.Signer, tx *types.Transaction,
	seen map[common.Address]struct{}) {
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return
	}
	warmOnce(reader, sender, seen)
	if to := tx.To(); to != nil {
		warmOnce(reader, *to, seen)
	}
}

func warmOnce(reader state.Reader, addr common.Address, seen map[common.Address]struct{}) {
	if _, ok := seen[addr]; ok {
		return
	}
	seen[addr] = struct{}{}
	reader.Account(addr) //nolint:errcheck
}

func (p *ParallelStateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, author *common.Address, interruptCtx context.Context) (processResult *ProcessResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("recovered from panic during parallel execution", "err", r)
			processResult = nil
			err = fmt.Errorf("panic during parallel execution: %v", r)
		}
	}()

	var (
		config      = p.chainConfig()
		receipts    types.Receipts
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		blockTime   = block.Time()
		allLogs     []*types.Log
		usedGas     = new(uint64)
	)

	// Set an empty context if nil
	if interruptCtx == nil {
		interruptCtx = context.Background()
	}

	// Mutate the block and state according to any hard-fork specs
	if config.DAOForkSupport && config.DAOForkBlock != nil && config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}

	tasks := make([]blockstm.ExecTask, 0, len(block.Transactions()))
	sharedJumpDests := vm.NewSyncJumpDestCache()

	shouldDelayFeeCal := true

	blockContext := NewEVMBlockContext(header, p.bc, author)
	coinbase := blockContext.Coinbase

	context := NewEVMBlockContext(header, p.bc.hc, author)

	vmenv := vm.NewEVM(context, statedb, config, cfg)

	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, vmenv)
	}
	if config.IsPrague(block.Number()) {
		// EIP-2935
		ProcessParentBlockHash(block.ParentHash(), vmenv)
	}
	signer := types.MakeSigner(config, header.Number, header.Time)
	prewarmReaderCache(statedb, block, signer)

	// Iterate over and process the individual transactions
	txs := block.Transactions()
	for i, tx := range txs {
		if tx.Type() == types.StateSyncTxType {
			continue
		}
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			log.Error("error creating message", "err", err)
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		cleansdb := statedb.Copy()

		if msg.From == coinbase {
			shouldDelayFeeCal = false
		}

		task := &ExecutionTask{
			msg:               *msg,
			config:            config,
			gasLimit:          block.GasLimit(),
			blockNumber:       blockNumber,
			blockHash:         blockHash,
			blockTime:         blockTime,
			tx:                tx,
			index:             i,
			cleanStateDB:      cleansdb,
			finalStateDB:      statedb,
			blockChain:        p.bc,
			header:            header,
			evmConfig:         cfg,
			shouldDelayFeeCal: &shouldDelayFeeCal,
			sender:            msg.From,
			totalUsedGas:      usedGas,
			receipts:          &receipts,
			allLogs:           &allLogs,
			coinbase:          coinbase,
			blockContext:      blockContext,
			jumpDests:         sharedJumpDests,
		}

		tasks = append(tasks, task)
	}

	backupStateDB := statedb.Copy()

	profile := false
	result, err := blockstm.ExecuteParallel(tasks, profile, false, p.bc.parallelSpeculativeProcesses, interruptCtx)

	if err == nil && profile && result.Deps != nil {
		_, weight := result.Deps.LongestPath(*result.Stats)

		serialWeight := uint64(0)

		for i := 0; i < len(result.Deps.GetVertices()); i++ {
			serialWeight += (*result.Stats)[i].End - (*result.Stats)[i].Start
		}

		parallelizabilityTimer.Update(time.Duration(serialWeight * 100 / weight))
	}

	if rerunErr, rerun := p.maybeRerunWithoutFeeDelay(tasks, statedb, backupStateDB,
		&shouldDelayFeeCal, &allLogs, &receipts, &usedGas, interruptCtx); rerun {
		err = rerunErr
	}

	if err != nil {
		return nil, err
	}

	// Polygon/bor: EIP-6110, EIP-7002, and EIP-7251 are not supported
	var requests [][]byte

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards), apply
	// state sync event (if any), and append the receipt.
	receiptsCountBeforeFinalize := len(receipts)
	receipts, err = p.chain.Engine().Finalize(p.bc.hc, header, statedb, block.Body(), receipts)
	if err != nil {
		return nil, err
	}

	// apply state sync logs
	if config.Bor != nil && config.Bor.IsMadhugiri(block.Number()) {
		// Defense-in-depth: if insertStateSyncTransactionAndCalculateReceipt silently failed
		// to add the receipt, the count will be off.
		if len(block.Transactions()) != len(receipts) {
			return nil, fmt.Errorf("%w: receipt count mismatch, txs=%d receipts=%d", ErrStateSyncMismatch, len(block.Transactions()), len(receipts))
		}
		appliedNewStateSyncReceipt := receiptsCountBeforeFinalize+1 == len(receipts)

		if appliedNewStateSyncReceipt {
			allLogs = append(allLogs, receipts[len(receipts)-1].Logs...)
		}
	}

	return &ProcessResult{
		Receipts: receipts,
		Requests: requests,
		Logs:     allLogs,
		GasUsed:  *usedGas,
	}, nil
}

// ---------------------------------------------------------------------------
// V2 BlockSTM integration: V2Task + V2Env implementations
// ---------------------------------------------------------------------------

// V2Task holds a transaction for V2 BlockSTM execution.
type V2Task struct {
	Index int
	Tx    *types.Transaction
	Msg   *Message
}

// v2Task implements blockstm.V2Task.
type v2Task struct {
	index int
	tx    *types.Transaction
	msg   *Message
}

func (t *v2Task) Index() int             { return t.index }
func (t *v2Task) Sender() common.Address { return t.msg.From }
func (t *v2Task) To() *common.Address    { return t.tx.To() }
func (t *v2Task) Authorities() []common.Address {
	if t.tx == nil {
		return nil
	}
	return t.tx.SetCodeAuthorities()
}

// v2Env implements blockstm.V2Env, providing the EVM execution environment.
type v2Env struct {
	base        *state.StateDB
	store       *blockstm.MVStore
	bals        *blockstm.MVBalanceStore
	blockCtx    vm.BlockContext
	vmConfig    vm.Config
	chainConfig *params.ChainConfig
	gasLimit    uint64
	// jumpDests is a fallback per-v2Env JUMPDEST cache used only when the
	// caller did NOT supply vmConfig.SharedJumpDestCache. In production,
	// blockchain.go wires sharedCaches.jumpDests (warmed by the prefetcher)
	// onto vmConfig and vm.NewEVM picks it up — overriding it here would
	// throw away the prefetcher's analysis. The fallback path matters for
	// callers that bypass ProcessBlock (benchmarks, single-block witness
	// processing) where no shared cache is provided.
	jumpDests vm.JumpDestCache
	safeBase  *state.SafeBase             // shared across all workers (with read cache)
	recycleCh chan *state.ParallelStateDB // pool of reusable PDBs
}

func (e *v2Env) BaseNonce(addr common.Address) uint64 {
	return e.base.GetNonce(addr)
}

func (e *v2Env) BaseCodeSize(addr common.Address) int {
	return e.base.GetCodeSize(addr)
}

// Shared closures — allocated once, reused across all workers.
var sharedTransferLogFn = state.TransferLogFn(func(db *state.StateDB, sender, recipient common.Address, amount, input1, input2, output1, output2 *big.Int) {
	AddTransferLog(db, sender, recipient, amount, input1, input2, output1, output2)
})
var sharedFeeLogFn = state.TransferLogFn(func(db *state.StateDB, sender, recipient common.Address, amount, input1, input2, output1, output2 *big.Int) {
	AddFeeTransferLog(db, sender, recipient, amount, input1, input2, output1, output2)
})

func (e *v2Env) Recycle(st blockstm.V2TxState) {
	if pdb, ok := st.(*state.ParallelStateDB); ok {
		select {
		case e.recycleCh <- pdb:
		default: // channel full, let GC handle it
		}
	}
}

func (e *v2Env) Execute(task blockstm.V2Task, workerID int, incarnation int,
	senderNonces map[common.Address]uint64,
	coinbase common.Address, waitForTx func(int), waitForFinal func(int), deferWrites bool) blockstm.V2TxState {
	t := task.(*v2Task)
	pdb := e.preparePDB(t, incarnation, senderNonces, coinbase, waitForTx, waitForFinal, deferWrites)

	evm := vm.NewEVM(e.blockCtx, pdb, e.chainConfig, e.vmConfig)
	// Only override with the per-v2Env fallback cache when no shared cache
	// is configured. vm.NewEVM has already wired vmConfig.SharedJumpDestCache
	// onto evm.jumpDests — overriding it would discard the prefetcher's work.
	if e.vmConfig.SharedJumpDestCache == nil && e.jumpDests != nil {
		evm.SetJumpDestCache(e.jumpDests)
	}
	evm.SetTxContext(NewEVMTxContext(t.msg))

	e.applyMessage(t, evm, pdb)
	return pdb
}

// preparePDB returns a configured ParallelStateDB for tx t — recycled
// from the pool when available, otherwise freshly allocated.
func (e *v2Env) preparePDB(t *v2Task, incarnation int, senderNonces map[common.Address]uint64,
	coinbase common.Address, waitForTx, waitForFinal func(int), deferWrites bool) *state.ParallelStateDB {
	var pdb *state.ParallelStateDB
	select {
	case pdb = <-e.recycleCh:
		pdb.Reset(t.index, e.safeBase, e.store, e.bals)
	default:
		pdb = state.NewParallelStateDB(t.index, e.safeBase, e.store, e.bals)
	}
	pdb.Incarnation = incarnation
	pdb.SenderNonces = senderNonces
	pdb.Coinbase = coinbase
	pdb.Sender = t.msg.From
	pdb.WaitForTx = waitForTx
	pdb.WaitForFinal = waitForFinal
	// Bor-specific transfer/fee logs only on Bor chain configs. Leaving
	// these nil on Ethereum-spec runs makes the V2 settle path skip the
	// log emission (see emitTransferLog/emitFeeLog nil-guards) and matches
	// the serial path's behaviour after the matching gates in
	// state_processor.go and state_transition.go.
	if e.chainConfig != nil && e.chainConfig.Bor != nil {
		pdb.TransferLogFn = sharedTransferLogFn
		pdb.FeeLogFn = sharedFeeLogFn
	} else {
		pdb.TransferLogFn = nil
		pdb.FeeLogFn = nil
	}
	pdb.DeferMVWrites = deferWrites
	pdb.EnableReadTracking()
	return pdb
}

// applyMessage runs the EVM for tx t against pdb, recovering panics and
// recording UsedGas / ExecFailed / FeeData on the PDB.
func (e *v2Env) applyMessage(t *v2Task, evm *vm.EVM, pdb *state.ParallelStateDB) {
	defer func() {
		if r := recover(); r != nil {
			// Incarnation 0 is speculative; SSTORE-refund / similar gas
			// accounting can underflow when a prior tx's writes aren't
			// settled yet. V2 re-executes and the re-exec sees the real
			// committed state.
			if pdb.Incarnation == 0 {
				log.Debug("V2 tx execution panic (speculative, will re-exec)", "tx", t.index, "err", r)
			} else {
				log.Error("V2 tx execution panic", "tx", t.index, "incarnation", pdb.Incarnation, "err", r)
			}
			pdb.Panicked = true
		}
	}()
	result, execErr := ApplyMessageNoFeeLog(evm, t.msg, new(GasPool).AddGas(e.gasLimit))
	if result == nil {
		// Consensus-level error (bad nonce, insufficient upfront gas, intrinsic
		// gas underflow, blob fork-gating, etc.). Serial returns this as a
		// block-fatal error; V2 must do the same. Record the error on the PDB
		// so settle skips it and the processor aborts the block.
		pdb.ExecErr = execErr
		return
	}
	pdb.UsedGas = result.UsedGas
	pdb.ExecFailed = result.Failed()
	if result.Failed() && len(result.ReturnData) > 0 {
		log.Debug("V2 tx reverted", "tx", t.index, "gas", result.UsedGas,
			"revert", fmt.Sprintf("%x", result.ReturnData), "err", execErr)
	}
	// FeeData is for log generation only — BalancesApplied=true skips balance changes.
	pdb.FeeData = &state.FeeData{
		FeeBurnt:             result.FeeBurnt,
		FeeTipped:            result.FeeTipped,
		BurntContractAddress: result.BurntContractAddress,
		SenderInitBalance:    result.SenderInitBalance,
		BalancesApplied:      true,
	}
}

// V2ExecutionResult wraps blockstm.V2ExecutionResult with typed PDB access.
type V2ExecutionResult struct {
	Pdbs     []*state.ParallelStateDB
	Receipts types.Receipts
	Logs     []*types.Log
	GasUsed  uint64
	// PanickedIdx is the index of the first tx whose execution panicked,
	// or -1 if none. Settlement skips panicked txs (their state is partial
	// and would corrupt finalDB), and the caller must propagate an error
	// rather than commit a half-applied block.
	PanickedIdx int
	// ExecErrIdx is the index of the first tx whose ApplyMessage returned a
	// consensus-level error (bad nonce, intrinsic gas, etc.), or -1 if none.
	// ExecErr holds that error. Settlement skips such txs and the processor
	// surfaces the error to abort the block — matching the serial path's
	// behaviour at core/state_processor.go:222.
	ExecErrIdx int
	ExecErr    error
	// ReadErr is the first database read failure observed either by the
	// settle-path statedb or by a SETTLED incarnation of some tx (speculative
	// incarnations that read outside the canonical state and were invalidated
	// don't count — see ParallelStateDB.BaseReadErr). The caller must discard
	// the result because StateDB getters return zero-ish values after
	// recording read errors.
	ReadErr error
	*blockstm.V2ExecutionResult
}

// (each validated tx is settled immediately while later txs execute).
// If finalDB is nil, settlement must be done by the caller after return.
//
// If tasks[i].Msg is nil, TransactionToMessage is called in parallel
// (signature recovery across all txs concurrently).
func ExecuteV2BlockSTM(
	ctx context.Context,
	tasks []V2Task,
	base *state.StateDB,
	store *blockstm.MVStore,
	bals *blockstm.MVBalanceStore,
	blockCtx vm.BlockContext,
	blockHash common.Hash,
	vmConfig vm.Config,
	chainConfig *params.ChainConfig,
	gasLimit uint64,
	numWorkers int,
	finalDB *state.StateDB,
	conflictAddrs map[common.Address]bool,
) *V2ExecutionResult {
	if idx, err := recoverTaskMessages(tasks, chainConfig, blockCtx); err != nil {
		return &V2ExecutionResult{
			Pdbs:              make([]*state.ParallelStateDB, len(tasks)),
			PanickedIdx:       -1,
			ExecErrIdx:        idx,
			ExecErr:           err,
			V2ExecutionResult: &blockstm.V2ExecutionResult{},
		}
	}

	// Without this, the SafeBase pool copies share base.reader, and
	// concurrent worker goroutines race on the trie's internal resolve
	// cache (caught by `go test -race`). Production reaches this path
	// via BlockChain.ProcessBlock which calls EnableConcurrentReads on
	// parallelStatedb; tests calling ExecuteV2BlockSTM directly were
	// missing this setup, so make the wrapper defensive and idempotent.
	base.EnableConcurrentReads()

	// Start the witness read-set prewalker only after concurrent reads are
	// enabled: it shares the trie reader with the workers, and starting it
	// before the enable write above would be an unsynchronized read/write
	// on the same reader. It walks cached keys into the witness while the
	// workers execute, keeping the settle drain in CollectStateWitness off
	// the block's critical path. No-op when no witness is being recorded.
	if finalDB != nil {
		stopPrewalk := finalDB.StartWitnessReadSetPrewalk()
		defer stopPrewalk()
	}

	itasks := make([]blockstm.V2Task, len(tasks))
	for i := range tasks {
		itasks[i] = &v2Task{index: tasks[i].Index, tx: tasks[i].Tx, msg: tasks[i].Msg}
	}

	env := newV2Env(base, store, bals, blockCtx, vmConfig, chainConfig, gasLimit, numWorkers)

	var receipts types.Receipts
	var allLogs []*types.Log
	var totalUsedGas uint64
	panickedIdx := -1
	execErrIdx := -1
	var execErr error
	var settleReadErr error
	var settleFn blockstm.V2SettleFn
	if finalDB != nil {
		settleFn = newV2SettleFn(tasks, env, finalDB, blockCtx, blockHash, chainConfig, &receipts, &allLogs, &totalUsedGas, &panickedIdx, &execErrIdx, &execErr, &settleReadErr)
	}

	raw := blockstm.ExecuteV2BlockSTM(ctx, itasks, env, blockCtx.Coinbase, numWorkers, conflictAddrs, settleFn)
	// Reads by the settle/finalize path go directly through base; its error
	// is unconditionally fatal. Per-worker base read failures are judged per
	// settled incarnation below — a speculative incarnation chasing stale
	// values may legitimately read outside the canonical state (on
	// witness-backed replay such nodes simply don't exist) and is then
	// invalidated, so env.safeBase.Error() would over-trigger here.
	readErr := base.Error()

	// V2 worker code reads land in env.safeBase.codeCache (each blob loaded
	// once, deduplicated by sync.Map). When witness collection is on, dump
	// every cached blob into the witness — finalDB.IntermediateRoot's
	// per-stateObject AddCode loop only catches code attached to objects
	// that actually settled.
	if finalDB != nil {
		if w := finalDB.Witness(); w != nil {
			env.safeBase.CollectCodeWitness(w.AddCode)
		}
	}

	pdbs := make([]*state.ParallelStateDB, len(raw.States))
	for i, s := range raw.States {
		if s != nil {
			pdbs[i] = s.(*state.ParallelStateDB)
		}
	}
	// Worker base-read failures are checked inside the settle callback, the
	// only point where a pdb is provably the settled incarnation. Scanning
	// raw.States here instead would read recycled pool objects: settlement
	// returns each pdb to the pool, a later tx's execution Resets and reuses
	// it, and the old raw.States slot keeps pointing at the mutated object —
	// so a speculative error from a LATER tx shows up under an earlier index.
	if readErr == nil {
		readErr = settleReadErr
	}
	// If settle never ran (finalDB nil), surface panics / exec errors / base
	// read failures from the PDBs so the caller can fail the block rather
	// than commit partial state. Safe in this mode only: without settlement
	// nothing recycles a pdb that raw.States still references.
	if finalDB == nil {
		for i, p := range pdbs {
			if p == nil {
				continue
			}
			if p.Panicked && panickedIdx < 0 {
				panickedIdx = i
			}
			if p.ExecErr != nil && execErrIdx < 0 {
				execErrIdx = i
				execErr = p.ExecErr
			}
			if p.BaseReadErr != nil && readErr == nil {
				readErr = p.BaseReadErr
			}
		}
	}

	return &V2ExecutionResult{
		Pdbs:              pdbs,
		Receipts:          receipts,
		Logs:              allLogs,
		GasUsed:           totalUsedGas,
		PanickedIdx:       panickedIdx,
		ExecErrIdx:        execErrIdx,
		ExecErr:           execErr,
		ReadErr:           readErr,
		V2ExecutionResult: raw,
	}
}

// recoverTaskMessages signature-recovers any task with nil Msg. Returns
// the lowest failing task index and error; (-1, nil) on success.
func recoverTaskMessages(tasks []V2Task, chainConfig *params.ChainConfig, blockCtx vm.BlockContext) (int, error) {
	needRecovery := false
	for i := range tasks {
		if tasks[i].Msg == nil {
			needRecovery = true
			break
		}
	}
	if !needRecovery {
		return -1, nil
	}
	signer := types.MakeSigner(chainConfig, blockCtx.BlockNumber, blockCtx.Time)
	baseFee := blockCtx.BaseFee
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstIdx = -1
		firstErr error
	)
	for i := range tasks {
		if tasks[i].Msg != nil {
			continue
		}
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			msg, err := TransactionToMessage(tasks[idx].Tx, signer, baseFee)
			if err != nil {
				errMu.Lock()
				if firstErr == nil || idx < firstIdx {
					firstIdx = idx
					firstErr = err
				}
				errMu.Unlock()
				return
			}
			tasks[idx].Msg = msg
		}()
	}
	wg.Wait()
	return firstIdx, firstErr
}

// newV2Env builds a v2Env wired up with the shared SafeBase, jumpDest cache,
// and PDB recycle pool.
func newV2Env(base *state.StateDB, store *blockstm.MVStore, bals *blockstm.MVBalanceStore,
	blockCtx vm.BlockContext, vmConfig vm.Config, chainConfig *params.ChainConfig,
	gasLimit uint64, numWorkers int) *v2Env {
	poolSize := numWorkers
	if poolSize < 2 {
		poolSize = 2
	}
	sharedSafeBase := state.NewSafeBase(base, poolSize)
	// Allocate the per-v2Env fallback only when the caller didn't supply a
	// shared cache. Production (blockchain.go) sets vmConfig.SharedJumpDestCache
	// on the prefetcher-warmed cache, so allocating here would just be dead
	// memory. Benchmarks and single-block witness paths bypass that wiring.
	var fallbackJumpDests vm.JumpDestCache
	if vmConfig.SharedJumpDestCache == nil {
		fallbackJumpDests = vm.NewSyncJumpDestCache()
	}
	return &v2Env{
		base:        base,
		store:       store,
		bals:        bals,
		blockCtx:    blockCtx,
		vmConfig:    vmConfig,
		chainConfig: chainConfig,
		gasLimit:    gasLimit,
		jumpDests:   fallbackJumpDests,
		safeBase:    sharedSafeBase,
		recycleCh:   make(chan *state.ParallelStateDB, numWorkers*blockstm.InFlightTaskMultiplier),
	}
}

// newV2SettleFn returns a settle callback that applies a tx's PDB writes to
// finalDB and produces a receipt. The closure is sequential — invoked in
// tx-index order — so accumulator vars (receipts, allLogs, totalUsedGas,
// panickedIdx, execErrIdx) are accessed without synchronization.
//
// If a panicked PDB reaches settlement, its state is partial and unsafe to
// commit — record the index for the caller to fail the block, recycle the
// PDB, and skip both SettleTo and receipt generation. The same applies to
// PDBs whose ApplyMessage returned a consensus-level error.
func newV2SettleFn(tasks []V2Task, env *v2Env, finalDB *state.StateDB,
	blockCtx vm.BlockContext, blockHash common.Hash, chainConfig *params.ChainConfig,
	receipts *types.Receipts, allLogs *[]*types.Log, totalUsedGas *uint64,
	panickedIdx *int, execErrIdx *int, execErr *error, readErr *error) blockstm.V2SettleFn {
	isByzantium := chainConfig.IsByzantium(blockCtx.BlockNumber)
	isEIP158 := chainConfig.IsEIP158(blockCtx.BlockNumber)
	return func(txIdx int, st blockstm.V2TxState) {
		if st == nil {
			return
		}
		pdb := st.(*state.ParallelStateDB)
		if pdb.Panicked {
			if *panickedIdx < 0 {
				*panickedIdx = txIdx
			}
			env.Recycle(st)
			return
		}
		if pdb.ExecErr != nil {
			if *execErrIdx < 0 {
				*execErrIdx = txIdx
				*execErr = pdb.ExecErr
			}
			env.Recycle(st)
			return
		}
		// This is the settled incarnation — a base read failure here means a
		// zero-ish read reached consensus state, which must abort the block.
		// Checked at settle time (not post-hoc over raw.States) because
		// Recycle below hands the pdb to later txs for reuse; see the readErr
		// note in ExecuteV2BlockSTM's caller. Speculative incarnations that
		// failed a base read and were invalidated never reach this callback.
		if pdb.BaseReadErr != nil {
			if *readErr == nil {
				*readErr = pdb.BaseReadErr
			}
			env.Recycle(st)
			return
		}
		tx := tasks[txIdx].Tx
		finalDB.SetTxContext(tx.Hash(), tasks[txIdx].Index)
		pdb.SettleTo(finalDB)

		*totalUsedGas += pdb.UsedGas
		var root []byte
		if !isByzantium {
			root = finalDB.IntermediateRoot(isEIP158).Bytes()
		}
		receipt := buildV2Receipt(tx, pdb, tasks[txIdx].Msg, root, *totalUsedGas, finalDB, blockCtx, blockHash)
		*receipts = append(*receipts, receipt)
		*allLogs = append(*allLogs, receipt.Logs...)

		// Return PDB to pool for reuse by subsequent txs.
		env.Recycle(st)
	}
}

// buildV2Receipt constructs the Receipt for a settled tx.
func buildV2Receipt(tx *types.Transaction, pdb *state.ParallelStateDB, msg *Message,
	postState []byte, cumulativeGasUsed uint64, finalDB *state.StateDB, blockCtx vm.BlockContext, blockHash common.Hash) *types.Receipt {
	receipt := &types.Receipt{
		Type:              tx.Type(),
		PostState:         postState,
		CumulativeGasUsed: cumulativeGasUsed,
		TxHash:            tx.Hash(),
		GasUsed:           pdb.UsedGas,
		BlockHash:         blockHash,
		BlockNumber:       blockCtx.BlockNumber,
		TransactionIndex:  uint(finalDB.TxIndex()),
	}
	if pdb.ExecFailed {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(msg.From, tx.Nonce())
	}
	receipt.Logs = finalDB.GetLogs(tx.Hash(), blockCtx.BlockNumber.Uint64(), blockHash, blockCtx.Time)
	receipt.Bloom = types.CreateBloom(receipt)
	return receipt
}

// ---------------------------------------------------------------------------
// V2StateProcessor implements the Processor interface for production use.
// ---------------------------------------------------------------------------

// V2StateProcessor processes blocks using V2 BlockSTM parallel execution.
type V2StateProcessor struct {
	chain      ChainContext
	bc         *BlockChain
	numWorkers int
	// conflictAddrs tracks To addresses that caused validation failures in
	// recent blocks. Used to chain cross-contract txs that share indirect state.
	conflictAddrs map[common.Address]bool
}

// NewV2StateProcessor creates a new V2 parallel state processor.
//
// numWorkers must be >= 1. Values <= 0 are clamped to runtime.NumCPU() to
// match the practical default and to prevent deadlocks: with zero workers,
// the executor's dispatcher window evaluates to zero and the very first
// task waits forever on an execDone channel that no worker will close
// (see core/blockstm/v2_executor.go:355).
func NewV2StateProcessor(chain ChainContext, bc *BlockChain, numWorkers int) *V2StateProcessor {
	if numWorkers <= 0 {
		numWorkers = runtime.NumCPU()
	}
	return &V2StateProcessor{
		chain:      chain,
		bc:         bc,
		numWorkers: numWorkers,
	}
}

func (p *V2StateProcessor) chainConfig() *params.ChainConfig {
	return p.chain.Config()
}

// Process processes the state changes according to the Polygon rules by running
// the transaction messages using V2 BlockSTM parallel execution.
// The caller should provide a statedb that is NOT shared with any read-only base.
// In production, ProcessBlock creates an independent parallelStatedb for this.
func (p *V2StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, author *common.Address, interruptCtx context.Context) (*ProcessResult, error) {
	// Tracer hooks are not goroutine-safe; concurrent V2 workers sharing
	// one Tracer would race. Refuse so ProcessBlock's fallback runs V1.
	if cfg.Tracer != nil {
		return nil, errV2TracerUnsupported
	}
	tProcess := time.Now()
	config := p.chainConfig()
	header := block.Header()

	if interruptCtx == nil {
		interruptCtx = context.Background()
	}

	// Hard-fork mutations + pre-execution system calls.
	if config.DAOForkSupport && config.DAOForkBlock != nil && config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	blockCtx := NewEVMBlockContext(header, p.chain, author)
	applyV2PreExecSystemCalls(block, statedb, config, cfg, blockCtx)

	tasks, err := buildV2Tasks(block, config, header, interruptCtx)
	if err != nil {
		return nil, err
	}
	tSetup := time.Now()

	finalDB := statedb
	// Preserve the witness pointer wired by ProcessBlock.StartPrefetcher
	// across the prefetcher swap. StateDB.StartPrefetcher unconditionally
	// overwrites s.witness, so passing nil here would silently turn off
	// every s.witness != nil-gated collection point (CollectStateWitness,
	// CollectCodeWitness, settle-phase trie walks) for the rest of V2's
	// execution — the produced witness would land empty.
	prevWitness := finalDB.Witness()
	finalDB.StopPrefetcher()
	finalDB.StartPrefetcher("v2_settle", prevWitness, nil)
	finalDB.SkipTimers()
	// Copy() deep-copies the witness; re-share so BLOCKHASH writes reach finalDB.
	readBase := statedb.Copy()
	readBase.SetWitness(prevWitness)
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	tCopy := time.Now()

	result := ExecuteV2BlockSTM(interruptCtx, tasks, readBase, store, bals, blockCtx, block.Hash(), cfg, config,
		block.GasLimit(), p.numWorkers, finalDB, p.conflictAddrs)

	tExec := time.Now()
	p.conflictAddrs = collectVFailToAddrs(tasks, result.VFailIdxs)

	// Refuse to commit a partially-applied block: a panicked PDB had its
	// settle skipped, so finalDB is missing that tx's effects. Returning an
	// error lets BlockChain.ProcessBlock fall back to the serial processor
	// (which will surface the same panic, or succeed if it was a V2-only bug).
	if result.PanickedIdx >= 0 {
		return nil, fmt.Errorf("v2: tx %d panicked during execution", result.PanickedIdx)
	}
	if result.ValidationPanic != nil {
		return nil, fmt.Errorf("v2: validation panic: %v", result.ValidationPanic)
	}
	if result.ReadErr != nil {
		return nil, fmt.Errorf("v2: base read: %w", result.ReadErr)
	}
	// Same logic for ApplyMessage consensus-level errors (bad nonce,
	// insufficient upfront gas, intrinsic gas underflow, etc.). Serial returns
	// the underlying error from state_processor.go:222 and aborts the block;
	// V2 must do the same so a malformed tx never settles as a zero-gas
	// no-op success.
	if result.ExecErrIdx >= 0 {
		return nil, fmt.Errorf("v2: tx %d apply message: %w", result.ExecErrIdx, result.ExecErr)
	}

	return p.finalizeV2Block(block, statedb, header, config, tasks, result,
		tProcess, tSetup, tCopy, tExec)
}

// finalizeV2Block runs consensus-engine finalization, merges state-sync logs,
// prefetches dirty storage for the root computation, and builds the final
// ProcessResult for V2StateProcessor.
func (p *V2StateProcessor) finalizeV2Block(block *types.Block, statedb *state.StateDB,
	header *types.Header, config *params.ChainConfig,
	tasks []V2Task, result *V2ExecutionResult,
	tProcess, tSetup, tCopy, tExec time.Time,
) (*ProcessResult, error) {
	receiptsCountBeforeFinalize := len(result.Receipts)
	receipts, err := p.chain.Engine().Finalize(p.chain, header, statedb, block.Body(), result.Receipts)
	if err != nil {
		return nil, err
	}

	// Prefetch storage tries for accounts dirtied by engine.Finalize
	// (state sync contract, validator rewards) so IntermediateRoot
	// doesn't need to load them from pebble synchronously.
	statedb.FinaliseFastWithPrefetch(true)

	// V2 worker reads went through pool copies that share `statedb`'s reader
	// by reference, so the trie tracers on that reader hold every node V2
	// touched. finalDB.IntermediateRoot only iterates finalDB.stateObjects
	// for witness collection, missing addresses that were ONLY read (never
	// settled). Pull the read-side witness directly from the shared reader.
	// This must run after engine.Finalize: its state reads (state sync at
	// sprint boundaries) are part of the witness contract too, and on nodes
	// with a flat reader they are served without ever reaching a trie tracer.
	statedb.CollectStateWitness()
	tFinalize := time.Now()

	logV2BlockStats(block, tasks, result, tProcess, tSetup, tCopy, tExec, tFinalize)

	allLogs := result.Logs
	if config.Bor != nil && config.Bor.IsMadhugiri(block.Number()) {
		if len(block.Transactions()) != len(receipts) {
			return nil, fmt.Errorf("err in bor.Finalize: %w", ErrStateSyncProcessing)
		}
		if receiptsCountBeforeFinalize+1 == len(receipts) {
			allLogs = append(allLogs, receipts[len(receipts)-1].Logs...)
		}
	}

	// EIP-7685 execution-layer requests (EIP-6110 deposits / EIP-7002
	// withdrawals / EIP-7251 consolidations). Bor doesn't ship these, but
	// when running with config.Bor == nil (Ethereum spec tests) the V2
	// path must mirror state_processor.Process — otherwise CalcRequestsHash
	// derives an empty-list hash and ValidateState fails the block on
	// header.RequestsHash mismatch.
	var requests [][]byte
	if config.IsPrague(block.Number()) && config.Bor == nil {
		requests = [][]byte{}
		if err := ParseDepositLogs(&requests, allLogs, config); err != nil {
			return nil, fmt.Errorf("failed to parse deposit logs: %w", err)
		}
		blockCtx := NewEVMBlockContext(header, p.chain, nil)
		evm := vm.NewEVM(blockCtx, statedb, config, vm.Config{})
		if err := ProcessWithdrawalQueue(&requests, evm); err != nil {
			return nil, fmt.Errorf("failed to process withdrawal queue: %w", err)
		}
		if err := ProcessConsolidationQueue(&requests, evm); err != nil {
			return nil, fmt.Errorf("failed to process consolidation queue: %w", err)
		}
	}

	return &ProcessResult{
		Receipts: receipts,
		Requests: requests,
		Logs:     allLogs,
		GasUsed:  result.GasUsed,
	}, nil
}

// applyV2PreExecSystemCalls runs the EIP-4788 beacon root and EIP-2935
// parent-hash system contracts when active for this block.
func applyV2PreExecSystemCalls(block *types.Block, statedb *state.StateDB,
	config *params.ChainConfig, cfg vm.Config, blockCtx vm.BlockContext) {
	evm := vm.NewEVM(blockCtx, statedb, config, cfg)
	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if config.IsPrague(block.Number()) || config.IsVerkle(block.Number()) {
		ProcessParentBlockHash(block.ParentHash(), evm)
	}
}

// buildV2Tasks converts non-StateSync transactions in the block into V2Tasks,
// short-circuiting if interruptCtx is canceled.
func buildV2Tasks(block *types.Block, config *params.ChainConfig, header *types.Header,
	interruptCtx context.Context) ([]V2Task, error) {
	signer := types.MakeSigner(config, header.Number, header.Time)
	var tasks []V2Task
	for i, tx := range block.Transactions() {
		select {
		case <-interruptCtx.Done():
			return nil, interruptCtx.Err()
		default:
		}
		if tx.Type() == types.StateSyncTxType {
			continue
		}
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		tasks = append(tasks, V2Task{Index: i, Tx: tx, Msg: msg})
	}
	return tasks, nil
}

// collectVFailToAddrs returns the To-address set of the failed txs in
// vfailIdxs — used to seed cross-contract conflict prediction for the
// next block.
func collectVFailToAddrs(tasks []V2Task, vfailIdxs []int) map[common.Address]bool {
	out := make(map[common.Address]bool)
	for _, idx := range vfailIdxs {
		if idx >= len(tasks) {
			continue
		}
		if to := tasks[idx].Tx.To(); to != nil {
			out[*to] = true
		}
	}
	return out
}

// logV2BlockStats emits the V2 block-level diagnostics line.
func logV2BlockStats(block *types.Block, tasks []V2Task, result *V2ExecutionResult,
	tProcess, tSetup, tCopy, tExec, tFinalize time.Time) {
	log.Debug("V2 block stats", "num", block.NumberU64(), "txs", len(tasks),
		"execs", result.ExecCount, "vfails", result.VFailCount,
		"cats", result.VFailCats,
		"setup", common.PrettyDuration(tSetup.Sub(tProcess)),
		"copy", common.PrettyDuration(tCopy.Sub(tSetup)),
		"exec", common.PrettyDuration(tExec.Sub(tCopy)),
		"finalize", common.PrettyDuration(tFinalize.Sub(tExec)),
		"total", common.PrettyDuration(tFinalize.Sub(tProcess)),
		"phase1", common.PrettyDuration(result.Phase1),
		"val_wait", common.PrettyDuration(result.ValWaitDur),
		"val_check", common.PrettyDuration(result.ValCheckDur),
		"val_reexec", common.PrettyDuration(result.ValReexDur),
		"settle", common.PrettyDuration(result.SettleDur))
}
