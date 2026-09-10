// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// ReplayBlockTransactions implements the Parity/OpenEthereum
// trace_replayBlockTransactions RPC method. It replays every transaction in the
// requested block and returns one ReplayResult per transaction, in block order.
//
// The traceTypes argument selects which outputs to populate: "trace",
// "stateDiff" and "vmTrace" (unrequested ones serialize to null).
//
// Unlike trace_block, the per-transaction trace entries returned here do NOT
// carry block/transaction identifier metadata, matching Parity's trace_replay*
// semantics.
func (api *TraceAPI) ReplayBlockTransactions(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash, traceTypes []string) ([]*ReplayResult, error) {
	set, err := parseTraceTypes(traceTypes)
	if err != nil {
		return nil, err
	}

	// Resolve the requested block. Hash takes precedence over number, mirroring
	// the convention used by other block-or-hash trace methods.
	var block *types.Block
	if hash, ok := blockNrOrHash.Hash(); ok {
		block, err = api.blockByHash(ctx, hash)
	} else if number, ok := blockNrOrHash.Number(); ok {
		if number == rpc.PendingBlockNumber {
			return nil, errors.New("tracing on top of pending is not supported")
		}
		block, err = api.blockByNumber(ctx, number)
	} else {
		return nil, errors.New("invalid arguments; neither block nor hash specified")
	}
	if err != nil {
		return nil, err
	}

	return api.replayBlockTransactions(ctx, block, set)
}

// replayBlockTransactions performs the per-transaction replay for a resolved
// block. It mirrors the state setup and sequential per-transaction loop of
// traceBlockParityByHash, but emits one ReplayResult per transaction (with
// trace metadata stripped) instead of a flat list of block traces.
func (api *API) replayBlockTransactions(ctx context.Context, block *types.Block, set traceTypeSet) ([]*ReplayResult, error) {
	exec, release, err := api.setupParityBlockExec(ctx, block)
	if err != nil {
		return nil, err
	}
	defer release()

	txs := exec.block.Transactions()
	results := make([]*ReplayResult, 0, len(txs))
	var cumulativeGasUsed uint64
	for txIndex, tx := range txs {
		message, txctx, err := exec.txInput(txIndex, tx, cumulativeGasUsed)
		if err != nil {
			return nil, fmt.Errorf("failed to convert tx to message (tx %d): %w", txIndex, err)
		}

		// includeTxMeta=false: trace_replay* entries omit block/tx identifiers.
		in := parityExecInput{tx: tx, msg: message, txctx: txctx, vmctx: exec.blockCtx, statedb: exec.statedb}
		result, gasUsed, err := api.buildReplayResult(ctx, in, set, false)
		if err != nil {
			return nil, fmt.Errorf("failed to trace tx %d: %w", txIndex, err)
		}
		cumulativeGasUsed += gasUsed

		txHash := tx.Hash()
		result.TransactionHash = &txHash
		results = append(results, result)
	}

	return results, nil
}
