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

package core

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// StatePrefetcher is a basic Prefetcher that executes transactions from a block
// on top of the parent state, aiming to prefetch potentially useful state data
// from disk. Transactions are executed in parallel to fully leverage the
// SSD's read performance.
type StatePrefetcher struct {
	config *params.ChainConfig // Chain configuration options
	chain  *HeaderChain        // Canonical block chain
}

// NewStatePrefetcher initialises a new statePrefetcher.
func NewStatePrefetcher(config *params.ChainConfig, chain *HeaderChain) *StatePrefetcher {
	return &StatePrefetcher{
		config: config,
		chain:  chain,
	}
}

// PrefetchResult contains the results of prefetching transactions
type PrefetchResult struct {
	TotalGasUsed  uint64
	SuccessfulTxs []common.Hash
}

// Prefetch processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb, but any changes are discarded. The
// only goal is to warm the state caches.
//
// This is a thin wrapper over PrefetchStream: it feeds the block's transactions
// into a channel, closes it, and runs the stream to completion. Behavior is
// identical to the pre-stream implementation.
func (p *StatePrefetcher) Prefetch(block *types.Block, statedb *state.StateDB, cfg vm.Config, intermediateRootPrefetch bool, interrupt *atomic.Bool) *PrefetchResult {
	txs := block.Transactions()
	ch := make(chan *types.Transaction, len(txs))
	for _, tx := range txs {
		ch <- tx
	}
	close(ch)
	return p.PrefetchStream(block.Header(), statedb, cfg, intermediateRootPrefetch, interrupt, nil, ch, nil)
}

// PrefetchStream warms state caches by executing transactions read from txsCh
// in parallel. It spins up a fixed worker pool once and keeps it alive for the
// whole call; workers exit when txsCh is closed or when hardKill is set.
//
//	header                   — block header used for EVM context.
//	statedb                  — parent state snapshot; each worker makes a per-tx Copy.
//	cfg                      — VM config (no tracer recommended).
//	intermediateRootPrefetch — if true, compute IntermediateRoot after each tx.
//	hardKill                 — set by the caller to exit the stream permanently;
//	                           workers return at loop entry.
//	evmAbort                 — soft, repeatable interrupt. When set, aborts in-flight
//	                           EVM work and causes workers to skip (not consume)
//	                           subsequent txs until the caller resets it. Lets the
//	                           caller implement phase transitions without tearing
//	                           down the worker pool. May be nil.
//	txsCh                    — transaction source; stream exits when this closes.
//	onSuccess                — called from worker goroutines on each successful tx.
//	                           Must be safe for concurrent invocation. May be nil.
func (p *StatePrefetcher) PrefetchStream(
	header *types.Header,
	statedb *state.StateDB,
	cfg vm.Config,
	intermediateRootPrefetch bool,
	hardKill *atomic.Bool,
	evmAbort *atomic.Bool,
	txsCh <-chan *types.Transaction,
	onSuccess func(hash common.Hash, gasUsed uint64),
) *PrefetchResult {
	evmInterrupt := resolveEvmInterrupt(evmAbort, hardKill)

	ctx := &streamCtx{
		p:                        p,
		header:                   header,
		statedb:                  statedb,
		reader:                   statedb.Reader(),
		signer:                   types.MakeSigner(p.config, header.Number, header.Time),
		cfg:                      cfg,
		intermediateRootPrefetch: intermediateRootPrefetch,
		hardKill:                 hardKill,
		evmAbort:                 evmAbort,
		evmInterrupt:             evmInterrupt,
		txsCh:                    txsCh,
		onSuccess:                onSuccess,
	}

	workers := max(1, 4*runtime.NumCPU()/5)
	var pool sync.WaitGroup
	pool.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer pool.Done()
			// Isolate worker panics: prefetching is best-effort and operates on a
			// throwaway state copy, so a panic here must never take down the node.
			// The parent runPrefetcher goroutine has its own recover but Go's
			// recover only catches panics in its own goroutine, not children.
			defer func() {
				if r := recover(); r != nil {
					// processTx already incremented txIndex before dispatching
					// to prefetchOneTx, so a panicked tx must count as a
					// failure to keep valid+invalid meters consistent.
					ctx.fails.Add(1)
					blockPrefetchWorkerPanicMeter.Mark(1)
					log.Error("prefetch worker panicked", "err", r)
				}
			}()
			ctx.runWorker()
		}()
	}
	pool.Wait()

	processed := ctx.txIndex.Load()
	blockPrefetchTxsValidMeter.Mark(processed - ctx.fails.Load())
	blockPrefetchTxsInvalidMeter.Mark(ctx.fails.Load())

	return &PrefetchResult{
		TotalGasUsed:  ctx.totalGasUsed.Load(),
		SuccessfulTxs: ctx.successfulTxs,
	}
}

// streamCtx bundles the per-call state shared by all workers in one PrefetchStream
// invocation. Workers call runWorker to pull from txsCh and warm state caches.
type streamCtx struct {
	p                        *StatePrefetcher
	header                   *types.Header
	statedb                  *state.StateDB
	reader                   state.Reader
	signer                   types.Signer
	cfg                      vm.Config
	intermediateRootPrefetch bool
	hardKill                 *atomic.Bool
	evmAbort                 *atomic.Bool
	evmInterrupt             *atomic.Bool
	txsCh                    <-chan *types.Transaction
	onSuccess                func(common.Hash, uint64)

	fails         atomic.Int64
	totalGasUsed  atomic.Uint64
	txIndex       atomic.Int64
	txsMutex      sync.Mutex
	successfulTxs []common.Hash
}

// runWorker pulls transactions from the stream and processes each one until the
// channel closes or hardKill fires. While evmAbort is set, buffered transactions
// are consumed but skipped (the pool stays alive for subsequent phases).
func (s *streamCtx) runWorker() {
	for tx := range s.txsCh {
		if s.hardKill != nil && s.hardKill.Load() {
			return
		}
		if s.evmAbort != nil && s.evmAbort.Load() {
			continue
		}
		s.processTx(tx)
	}
}

func (s *streamCtx) processTx(tx *types.Transaction) {
	idx := int(s.txIndex.Add(1) - 1)
	gasUsed, ok := s.p.prefetchOneTx(
		tx, idx, s.header, s.statedb, s.reader, s.signer, s.cfg,
		s.intermediateRootPrefetch, s.evmInterrupt, &s.fails,
	)
	if !ok {
		return
	}
	s.totalGasUsed.Add(gasUsed)
	s.txsMutex.Lock()
	s.successfulTxs = append(s.successfulTxs, tx.Hash())
	s.txsMutex.Unlock()
	if s.onSuccess != nil {
		s.onSuccess(tx.Hash(), gasUsed)
	}
}

// prefetchOneTx executes a single transaction on a copy of statedb to warm caches.
// Shared worker body used by both Prefetch (block-oriented) and PrefetchStream
// (streaming). Returns the execution gas used and a success flag. On any EVM
// interrupt or unrecoverable error it returns (0, false) and increments fails.
func (p *StatePrefetcher) prefetchOneTx(
	tx *types.Transaction,
	txIdx int,
	header *types.Header,
	statedb *state.StateDB,
	reader state.Reader,
	signer types.Signer,
	cfg vm.Config,
	intermediateRootPrefetch bool,
	interrupt *atomic.Bool,
	fails *atomic.Int64,
) (uint64, bool) {
	if interrupt != nil && interrupt.Load() {
		// Match every other failure path in this function — the docstring
		// promises fails is incremented on every (0,false) return so the
		// {valid,invalid} meters add up to the txIndex without double-counting.
		fails.Add(1)
		return 0, false
	}

	sender, err := preloadReaderForTx(reader, tx, signer)
	if err != nil {
		fails.Add(1)
		return 0, false
	}
	_ = sender // sender is pre-cached via reader.Account

	stateCpy := statedb.Copy()
	msg, err := TransactionToMessage(tx, signer, header.BaseFee)
	if err != nil {
		fails.Add(1)
		return 0, false
	}
	msg.SkipNonceChecks = true // stream order may diverge from nonce order
	stateCpy.SetTxContext(tx.Hash(), txIdx)

	evm := vm.NewEVM(NewEVMBlockContext(header, p.chain, nil), stateCpy, p.config, cfg)
	evm.SetInterrupt(interrupt)

	result, err := ApplyMessage(evm, msg, NewGasPool(header.GasLimit))
	if err != nil {
		fails.Add(1)
		return 0, false
	}
	if intermediateRootPrefetch {
		stateCpy.IntermediateRoot(true)
	}
	return result.UsedGas, true
}

// resolveEvmInterrupt picks the atomic flag wired into the EVM interrupt for the
// duration of a prefetch stream. evmAbort (soft, per-phase) takes precedence;
// hardKill is the fallback for callers that don't supply one.
func resolveEvmInterrupt(evmAbort, hardKill *atomic.Bool) *atomic.Bool {
	if evmAbort != nil {
		return evmAbort
	}
	return hardKill
}

// preloadReaderForTx issues non-blocking reads against the state reader for the
// accounts, code, and access-list slots the tx is likely to touch. This warms
// the caches before EVM execution. Returns the recovered sender so callers can
// avoid a second signature-recovery pass.
func preloadReaderForTx(reader state.Reader, tx *types.Transaction, signer types.Signer) (common.Address, error) {
	sender, err := types.Sender(signer, tx)
	if err != nil {
		return common.Address{}, err
	}
	reader.Account(sender)

	if to := tx.To(); to != nil {
		if account, _ := reader.Account(*to); account != nil &&
			!bytes.Equal(account.CodeHash, types.EmptyCodeHash.Bytes()) {
			reader.Code(*to, common.BytesToHash(account.CodeHash))
		}
	}
	for _, list := range tx.AccessList() {
		reader.Account(list.Address)
		for _, slot := range list.StorageKeys {
			reader.Storage(list.Address, slot)
		}
	}
	return sender, nil
}
