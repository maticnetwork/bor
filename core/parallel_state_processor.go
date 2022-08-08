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
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type ParallelStateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewParallelStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *ParallelStateProcessor {
	return &ParallelStateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

type ExecutionTask struct {
	msg    types.Message
	config *params.ChainConfig

	gasLimit                   uint64
	blockNumber                *big.Int
	blockHash                  common.Hash
	tx                         *types.Transaction
	index                      int
	statedb                    *state.StateDB // State database that stores the modified values after tx execution.
	cleanStateDB               *state.StateDB // A clean copy of the initial statedb. It should not be modified.
	finalStateDB               *state.StateDB // A copy of the final statedb.
	finalDBMutex               *sync.RWMutex
	header                     *types.Header
	blockChain                 *BlockChain
	evmConfig                  vm.Config
	result                     *ExecutionResult
	shouldDelayFeeCal          *bool
	shouldRerunWithoutFeeDelay bool
}

func (task *ExecutionTask) Execute(mvh *blockstm.MVHashMap, incarnation int) (err error) {
	task.statedb = task.cleanStateDB.Copy()
	task.statedb.Prepare(task.tx.Hash(), task.index)
	task.statedb.SetMVHashmap(mvh)
	task.statedb.SetIncarnation(incarnation)

	blockContext := NewEVMBlockContext(task.header, task.blockChain, nil)

	evm := vm.NewEVM(blockContext, vm.TxContext{}, task.statedb, task.config, task.evmConfig)

	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(task.msg)
	evm.Reset(txContext, task.statedb)

	defer func() {
		if r := recover(); r != nil {
			// In some pre-matured executions, EVM will panic. Recover from panic and retry the execution.
			log.Debug("Recovered from EVM failure. Error:\n", r)

			err = blockstm.ErrExecAbortError{DependencyIndex: task.statedb.DepTxIndex()}

			return
		}
	}()

	// Apply the transaction to the current state (included in the env).
	if *task.shouldDelayFeeCal {
		task.result, err = ApplyMessageNoFeeBurnOrTip(evm, task.msg, new(GasPool).AddGas(task.gasLimit))
		if _, ok := task.statedb.MVReadMap()[blockstm.NewSubpathKey(blockContext.Coinbase, state.BalancePath)]; ok {
			log.Info("Coinbase is in MVReadMap", "address", blockContext.Coinbase)

			task.shouldRerunWithoutFeeDelay = true
		}

		if _, ok := task.statedb.MVReadMap()[blockstm.NewSubpathKey(task.result.BurntContractAddress, state.BalancePath)]; ok {
			log.Info("BurntContractAddress is in MVReadMap", "address", task.result.BurntContractAddress)
			task.shouldRerunWithoutFeeDelay = true
		}
	} else {
		task.result, err = ApplyMessage(evm, task.msg, new(GasPool).AddGas(task.gasLimit))
	}

	if task.statedb.HadInvalidRead() || err != nil {
		err = blockstm.ErrExecAbortError{DependencyIndex: task.statedb.DepTxIndex()}
		return
	}

	task.statedb.Finalise(false)

	task.finalDBMutex.Lock()
	for _, v := range task.statedb.MVFullWriteList() {
		path := v.Path

		if path.IsAddress() {
			task.finalStateDB.GetOrNewStateObject(path.GetAddress())
		} else if path.IsState() {
			addr := path.GetAddress()
			stateKey := path.GetStateKey()
			task.finalStateDB.GetState(addr, stateKey)
		}
	}
	task.finalDBMutex.Unlock()

	return
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

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
// nolint:gocognit
func (p *ParallelStateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		usedGas     = new(uint64)
	)

	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}

	tasks := make([]blockstm.ExecTask, 0, len(block.Transactions()))

	shouldDelayFeeCal := true

	coinbase, _ := p.bc.Engine().Author(header)

	finalDBMutex := sync.RWMutex{}

	// Iterate over and process the individual transactions
	for i, tx := range block.Transactions() {
		msg, err := tx.AsMessage(types.MakeSigner(p.config, header.Number), header.BaseFee)
		if err != nil {
			log.Error("error creating message", "err", err)
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}

		cleansdb := statedb.Copy()

		if msg.From() == coinbase {
			shouldDelayFeeCal = false
		}

		task := &ExecutionTask{
			msg:               msg,
			config:            p.config,
			gasLimit:          block.GasLimit(),
			blockNumber:       blockNumber,
			blockHash:         blockHash,
			tx:                tx,
			index:             i,
			cleanStateDB:      cleansdb,
			finalStateDB:      statedb,
			finalDBMutex:      &finalDBMutex,
			blockChain:        p.bc,
			header:            header,
			evmConfig:         cfg,
			shouldDelayFeeCal: &shouldDelayFeeCal,
		}

		tasks = append(tasks, task)
	}

	_, err := blockstm.ExecuteParallel(tasks)

	for _, task := range tasks {
		task := task.(*ExecutionTask)
		if task.shouldRerunWithoutFeeDelay {
			shouldDelayFeeCal = false
			_, err = blockstm.ExecuteParallel(tasks)

			break
		}
	}

	if err != nil {
		log.Error("blockstm error executing block", "err", err)
		return nil, nil, 0, err
	}

	london := p.config.IsLondon(blockNumber)

	for _, task := range tasks {
		task := task.(*ExecutionTask)
		statedb.Prepare(task.tx.Hash(), task.index)

		coinbaseBalance := statedb.GetBalance(coinbase)

		statedb.ApplyMVWriteSet(task.MVWriteList())

		for _, l := range task.statedb.GetLogs(task.tx.Hash(), blockHash) {
			statedb.AddLog(l)
		}

		if shouldDelayFeeCal {
			if london {
				statedb.AddBalance(task.result.BurntContractAddress, task.result.FeeBurnt)
			}

			statedb.AddBalance(coinbase, task.result.FeeTipped)
			output1 := new(big.Int).SetBytes(task.result.senderInitBalance.Bytes())
			output2 := new(big.Int).SetBytes(coinbaseBalance.Bytes())

			// Deprecating transfer log and will be removed in future fork. PLEASE DO NOT USE this transfer log going forward. Parameters won't get updated as expected going forward with EIP1559
			// add transfer log
			AddFeeTransferLog(
				statedb,

				task.msg.From(),
				coinbase,

				task.result.FeeTipped,
				task.result.senderInitBalance,
				coinbaseBalance,
				output1.Sub(output1, task.result.FeeTipped),
				output2.Add(output2, task.result.FeeTipped),
			)
		}

		for k, v := range task.statedb.Preimages() {
			statedb.AddPreimage(k, v)
		}

		// Update the state with pending changes.
		var root []byte

		if !p.config.IsByzantium(blockNumber) {
			root = statedb.IntermediateRoot(p.config.IsEIP158(blockNumber)).Bytes()
		}

		*usedGas += task.result.UsedGas

		// Create a new receipt for the transaction, storing the intermediate root and gas used
		// by the tx.
		receipt := &types.Receipt{Type: task.tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
		if task.result.Failed() {
			receipt.Status = types.ReceiptStatusFailed
		} else {
			receipt.Status = types.ReceiptStatusSuccessful
		}

		receipt.TxHash = task.tx.Hash()
		receipt.GasUsed = task.result.UsedGas

		// If the transaction created a contract, store the creation address in the receipt.
		if task.msg.To() == nil {
			receipt.ContractAddress = crypto.CreateAddress(task.msg.From(), task.tx.Nonce())
		}

		// Set the receipt logs and create the bloom filter.
		receipt.Logs = statedb.GetLogs(task.tx.Hash(), blockHash)
		receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
		receipt.BlockHash = blockHash
		receipt.BlockNumber = blockNumber
		receipt.TransactionIndex = uint(statedb.TxIndex())

		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
	}

	statedb.Finalise(p.config.IsEIP158(blockNumber))

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.engine.Finalize(p.bc, header, statedb, block.Transactions(), block.Uncles())

	return receipts, allLogs, *usedGas, nil
}
