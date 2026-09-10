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
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

const maxSystemCallRet = 64 << 20 // 64 MiB

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	chain ChainContext // Chain context interface
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(chain ChainContext) *StateProcessor {
	return &StateProcessor{
		chain: chain,
	}
}

// chainConfig returns the chain configuration.
func (p *StateProcessor) chainConfig() *params.ChainConfig {
	return p.chain.Config()
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, author *common.Address, interruptCtx context.Context) (*ProcessResult, error) {
	var (
		config      = p.chainConfig()
		receipts    types.Receipts
		usedGas     = new(uint64)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
		err         error

		reservedGasUsed uint64
	)

	// Set an empty context if nil
	if interruptCtx == nil {
		interruptCtx = context.Background()
	}

	// Mutate the block and state according to any hard-fork specs
	if config.DAOForkSupport && config.DAOForkBlock != nil && config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	var (
		context vm.BlockContext
		signer  = types.MakeSigner(config, header.Number, header.Time)
	)

	// Apply pre-execution system calls.
	var tracingStateDB = vm.StateDB(statedb)
	if hooks := cfg.Tracer; hooks != nil {
		tracingStateDB = state.NewHookedState(statedb, hooks)
	}
	context = NewEVMBlockContext(header, p.chain, author)
	clientUsage, err := p.applyReservedClassification(&context, statedb, header, block, signer)
	if err != nil {
		return nil, err
	}
	evm := vm.NewEVM(context, tracingStateDB, p.chainConfig(), cfg)

	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if p.chainConfig().IsPrague(block.Number()) || p.chainConfig().IsVerkle(block.Number()) {
		// EIP-2935
		ProcessParentBlockHash(block.ParentHash(), evm)
	}

	// Iterate over and process the individual transactions
	txs := block.Transactions()
	for i, tx := range txs {
		// Check if execution should be cancelled or not
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

		statedb.SetTxContext(tx.Hash(), i)

		receipt, err := ApplyTransactionWithEVM(msg, gp, statedb, blockNumber, blockHash, context.Time, tx, usedGas, evm)
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
		if _, ok := context.ReservedTxs[registryreader.ReservedKey{From: msg.From, Nonce: msg.Nonce}]; ok {
			reservedGasUsed += receipt.GasUsed
		}
	}

	// Polygon/bor: EIP-6110, EIP-7002, and EIP-7251 are not supported
	// Read requests if Prague is enabled.
	var requests [][]byte
	if p.chainConfig().IsPrague(block.Number()) && p.chainConfig().Bor == nil {
		requests = [][]byte{}
		// EIP-6110
		if err := ParseDepositLogs(&requests, allLogs, config); err != nil {
			return nil, fmt.Errorf("failed to parse deposit logs: %w", err)
		}
		// EIP-7002
		if err := ProcessWithdrawalQueue(&requests, evm); err != nil {
			return nil, fmt.Errorf("failed to process withdrawal queue: %w", err)
		}
		// EIP-7251
		if err := ProcessConsolidationQueue(&requests, evm); err != nil {
			return nil, fmt.Errorf("failed to process consolidation queue: %w", err)
		}
	}

	// State-sync transactions (post-Madhugiri) are the last tx in body. In order to produce
	// accurate traces for state-sync transactions, we fire the OnTxStart and OnTxEnd hooks
	// here before calling Finalize. The hooks are already wrapped to support state-sync tx
	// tracing. Because state-sync events are applied to state as independent EVM calls, the
	// wrapped hooks inserts a synthetic root frame to wrap all those calls.
	var (
		hasStateSyncTx   = len(txs) > 0 && txs[len(txs)-1].Type() == types.StateSyncTxType
		stateSyncReceipt *types.Receipt
		stateSyncEndErr  error
	)
	if hooks := cfg.Tracer; hooks != nil && hasStateSyncTx && hooks.OnTxStart != nil && hooks.OnTxEnd != nil {
		hooks.OnTxStart(evm.GetVMContext(), txs[len(txs)-1], params.BorSystemAddress)
		defer func() {
			hooks.OnTxEnd(stateSyncReceipt, stateSyncEndErr)
		}()
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards), apply
	// state sync event (if any), and append the receipt.
	receiptsCountBeforeFinalize := len(receipts)
	receipts, err = p.chain.Engine().Finalize(p.chain, header, tracingStateDB, block.Body(), receipts)
	if err != nil {
		stateSyncEndErr = err
		return nil, err
	}

	// apply state sync logs
	if p.chainConfig().Bor != nil && p.chainConfig().Bor.IsMadhugiri(block.Number()) {
		// Defense-in-depth: if insertStateSyncTransactionAndCalculateReceipt silently failed
		// to add the receipt, the count will be off.
		if len(block.Transactions()) != len(receipts) {
			stateSyncEndErr = fmt.Errorf("%w: receipt count mismatch, txs=%d receipts=%d", ErrStateSyncMismatch, len(block.Transactions()), len(receipts))
			return nil, stateSyncEndErr
		}
		appliedNewStateSyncReceipt := receiptsCountBeforeFinalize+1 == len(receipts)

		if appliedNewStateSyncReceipt {
			allLogs = append(allLogs, receipts[len(receipts)-1].Logs...)
			stateSyncReceipt = receipts[len(receipts)-1]
		}
	}

	return &ProcessResult{
		Receipts:            receipts,
		Requests:            requests,
		Logs:                allLogs,
		GasUsed:             *usedGas,
		ReservedGasUsed:     reservedGasUsed,
		ReservedCapacity:    context.ReservedSnapshot.EffectiveCapacity(),
		ReservedTxIndexes:   ReservedTxIndexes(txs, signer, context.ReservedTxs),
		ReservedClientUsage: clientUsage,
	}, nil
}

// applyReservedClassification reads the registry at the parent state (statedb is
// the parent post-state, before block execution) and stores the resulting
// snapshot plus the per-block reserved transaction set on blockCtx. The
// quota-aware reserved set is derived once from the ordered body so every
// transaction's fee-free decision is fixed before (parallel) execution. The
// returned usage map is the same classification walk's per-client tally,
// handed back to the caller for ProcessResult rather than stored on blockCtx.
func (p *StateProcessor) applyReservedClassification(blockCtx *vm.BlockContext, statedb *state.StateDB, header *types.Header, block *types.Block, signer types.Signer) (map[uint64]registryreader.ClientUsage, error) {
	reservedSnapshot, err := ReservedSnapshotForBlock(p.chain, statedb, header)
	if err != nil {
		return nil, err
	}
	blockCtx.ReservedSnapshot = reservedSnapshot
	reservedTxs, clientUsage := registryreader.ClassifyReserved(block.Transactions(), signer, blockCtx.ReservedSnapshot)
	blockCtx.ReservedTxs = reservedTxs
	return clientUsage, nil
}

// ReservedTxIndexes returns the ascending positions within txs whose
// (sender, nonce) is in set (txs is the final block order, so a plain index
// loop already yields ascending positions). This is the single derivation
// shared by every processor path (serial, both parallel implementations) and
// the miner - see miner/worker.go's use at task-creation time - so a
// divergence between independent copies of this matching loop can't recur.
// Returns nil for an empty set (pre-fork, no registry, or nothing reserved).
func ReservedTxIndexes(txs types.Transactions, signer types.Signer, set map[registryreader.ReservedKey]struct{}) []uint64 {
	if len(set) == 0 {
		return nil
	}
	var indexes []uint64
	for i, tx := range txs {
		from, err := types.Sender(signer, tx)
		if err != nil {
			continue
		}
		if _, ok := set[registryreader.ReservedKey{From: from, Nonce: tx.Nonce()}]; ok {
			indexes = append(indexes, uint64(i))
		}
	}
	return indexes
}

// sumReservedGasUsed totals the actual gas used by transactions classified
// reserved (fee-free) in set. Gas is matched to transactions by hash, so it
// is independent of receipt/transaction ordering and ignores the trailing
// state-sync receipt (whose sender is never registered). Returns (0, nil)
// for an empty set (pre-fork, no registry, or nothing reserved).
func sumReservedGasUsed(txs types.Transactions, receipts types.Receipts, signer types.Signer, set map[registryreader.ReservedKey]struct{}) (uint64, []uint64) {
	indexes := ReservedTxIndexes(txs, signer, set)
	if len(indexes) == 0 {
		return 0, nil
	}
	gasByHash := make(map[common.Hash]uint64, len(receipts))
	for _, r := range receipts {
		gasByHash[r.TxHash] = r.GasUsed
	}
	var total uint64
	for _, idx := range indexes {
		total += gasByHash[txs[idx].Hash()]
	}
	return total, indexes
}

// ApplyTransactionWithEVM attempts to apply a transaction to the given state database
// and uses the input parameters for its environment similar to ApplyTransaction. However,
// this method takes an already created EVM instance as input.
func ApplyTransactionWithEVM(msg *Message, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, blockTime uint64, tx *types.Transaction, usedGas *uint64, evm *vm.EVM) (receipt *types.Receipt, err error) {
	if hooks := evm.Config.Tracer; hooks != nil {
		if hooks.OnTxStart != nil {
			hooks.OnTxStart(evm.GetVMContext(), tx, msg.From)
		}
		if hooks.OnTxEnd != nil {
			defer func() { hooks.OnTxEnd(receipt, err) }()
		}
	}

	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(msg)
	evm.SetTxContext(txContext)

	var result *ExecutionResult

	backupMVHashMap := statedb.GetMVHashmap()

	// pause recording read and write
	statedb.SetMVHashmap(nil)

	coinbaseBalance := statedb.GetBalance(evm.Context.Coinbase)

	// resume recording read and write
	statedb.SetMVHashmap(backupMVHashMap)

	result, err = ApplyMessageNoFeeBurnOrTip(evm, *msg, gp)
	if err != nil {
		return nil, err
	}

	// stop recording read and write
	statedb.SetMVHashmap(nil)

	if evm.ChainConfig().IsLondon(blockNumber) && result.FeeBurnt != nil {
		// FeeBurnt is only populated for Bor-enabled chains in stateTransition.execute
		// (see core/state_transition.go); for non-Bor configs the base fee is
		// implicitly burned and there's no contract to credit.
		// Use `evm.StateDB` for using hooked state db if tracing is enabled
		evm.StateDB.AddBalance(result.BurntContractAddress, cmath.BigIntToUint256Int(result.FeeBurnt), tracing.BalanceChangeTransfer)
	}

	evm.StateDB.AddBalance(evm.Context.Coinbase, cmath.BigIntToUint256Int(result.FeeTipped), tracing.BalanceChangeTransfer)
	output1 := new(big.Int).SetBytes(result.SenderInitBalance.Bytes())
	output2 := new(big.Int).SetBytes(coinbaseBalance.Bytes())

	// Deprecating transfer log and will be removed in future fork. PLEASE DO NOT USE this transfer log going forward. Parameters won't get updated as expected going forward with EIP1559
	// add transfer log
	// We use `evm.StateDB` instance instead of normal `statedb` as if tracing is enabled, we may have a hooked state db instance
	// for the fee transfer log.
	if evm.ChainConfig().Bor != nil {
		// Bor-specific fee transfer log; skipped on non-Bor chain configs.
		AddFeeTransferLog(
			evm.StateDB,

			msg.From,
			evm.Context.Coinbase,

			result.FeeTipped,
			result.SenderInitBalance,
			coinbaseBalance.ToBig(),
			output1.Sub(output1, result.FeeTipped),
			output2.Add(output2, result.FeeTipped),
		)
	}

	if result.Err == vm.ErrInterrupt {
		return nil, result.Err
	}

	// Update the state with pending changes.
	var root []byte

	if evm.ChainConfig().IsByzantium(blockNumber) {
		evm.StateDB.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(evm.ChainConfig().IsEIP158(blockNumber)).Bytes()
	}

	*usedGas += result.UsedGas

	// Merge the tx-local access event into the "block-local" one, in order to collect
	// all values, so that the witness can be built.
	if statedb.Database().TrieDB().IsVerkle() {
		statedb.AccessEvents().Merge(evm.AccessEvents)
	}
	return MakeReceipt(evm, result, statedb, blockNumber, blockHash, blockTime, tx, *usedGas, root), nil
}

// MakeReceipt generates the receipt object for a transaction given its execution result.
func MakeReceipt(evm *vm.EVM, result *ExecutionResult, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, blockTime uint64, tx *types.Transaction, usedGas uint64, root []byte) *types.Receipt {
	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}

	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * params.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = evm.Context.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if tx.To() == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash, blockTime)
	receipt.Bloom = types.CreateBloom(receipt)
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(evm *vm.EVM, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(evm.ChainConfig(), header.Number, header.Time), header.BaseFee)
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the EVM environment
	return ApplyTransactionWithEVM(msg, gp, statedb, header.Number, header.Hash(), header.Time, tx, usedGas, evm)
}

// ProcessBeaconBlockRoot applies the EIP-4788 system call to the beacon block root
// contract. This method is exported to be used in tests.
func ProcessBeaconBlockRoot(beaconRoot common.Hash, evm *vm.EVM) {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.BeaconRootsAddress,
		Data:      beaconRoot[:],
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(params.BeaconRootsAddress)
	_, _, _ = evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	evm.StateDB.Finalise(true)
}

// ProcessParentBlockHash stores the parent block hash in the history storage contract
// as per EIP-2935/7709.
func ProcessParentBlockHash(prevHash common.Hash, evm *vm.EVM) {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.HistoryStorageAddress,
		Data:      prevHash.Bytes(),
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(params.HistoryStorageAddress)
	_, _, err := evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	if err != nil {
		panic(err)
	}
	if evm.StateDB.AccessEvents() != nil {
		evm.StateDB.AccessEvents().Merge(evm.AccessEvents)
	}
	evm.StateDB.Finalise(true)
}

// ProcessWithdrawalQueue calls the EIP-7002 withdrawal queue contract.
// It returns the opaque request data returned by the contract.
func ProcessWithdrawalQueue(requests *[][]byte, evm *vm.EVM) error {
	return processRequestsSystemCall(requests, evm, 0x01, params.WithdrawalQueueAddress)
}

// ProcessConsolidationQueue calls the EIP-7251 consolidation queue contract.
// It returns the opaque request data returned by the contract.
func ProcessConsolidationQueue(requests *[][]byte, evm *vm.EVM) error {
	return processRequestsSystemCall(requests, evm, 0x02, params.ConsolidationQueueAddress)
}

func processRequestsSystemCall(requests *[][]byte, evm *vm.EVM, requestType byte, addr common.Address) error {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &addr,
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(addr)
	ret, _, err := evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	evm.StateDB.Finalise(true)
	if err != nil {
		return fmt.Errorf("system call failed to execute: %v", err)
	}
	if len(ret) == 0 {
		return nil // skip empty output
	}
	if len(ret) > maxSystemCallRet || len(ret) > math.MaxInt-1 {
		return fmt.Errorf("system call output too large")
	}
	// Append prefixed requestsData to the requests list.
	requestsData := []byte{requestType}
	requestsData = append(requestsData, ret...)
	requestsData[0] = requestType
	copy(requestsData[1:], ret)
	*requests = append(*requests, requestsData)
	return nil
}

var depositTopic = common.HexToHash("0x649bbc62d0e31342afea4e5cd82d4049e7e1ee912fc0889aa790803be39038c5")

// ParseDepositLogs extracts the EIP-6110 deposit values from logs emitted by
// BeaconDepositContract.
func ParseDepositLogs(requests *[][]byte, logs []*types.Log, config *params.ChainConfig) error {
	deposits := make([]byte, 1) // note: first byte is 0x00 (== deposit request type)
	for _, log := range logs {
		if log.Address == config.DepositContractAddress && len(log.Topics) > 0 && log.Topics[0] == depositTopic {
			request, err := types.DepositLogToRequest(log.Data)
			if err != nil {
				return fmt.Errorf("unable to parse deposit data: %v", err)
			}
			deposits = append(deposits, request...)
		}
	}
	if len(deposits) > 1 {
		*requests = append(*requests, deposits)
	}
	return nil
}

func onSystemCallStart(tracer *tracing.Hooks, ctx *tracing.VMContext) {
	if tracer.OnSystemCallStartV2 != nil {
		tracer.OnSystemCallStartV2(ctx)
	} else if tracer.OnSystemCallStart != nil {
		tracer.OnSystemCallStart()
	}
}
