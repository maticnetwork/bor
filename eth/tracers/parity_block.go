// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

// ParityTrace represents a single trace entry in the Parity/OpenEthereum format.
// Used by trace_block and other trace_* methods for compatibility with Polygon Erigon.
type ParityTrace struct {
	Action      *ParityTraceAction `json:"action,omitempty"`
	BlockHash   *common.Hash       `json:"blockHash,omitempty"`
	BlockNumber *uint64            `json:"blockNumber,omitempty"`
	Error       *string            `json:"error,omitempty"`
	// Result is always emitted (null for suicides and failed calls), matching erigon.
	Result              *ParityTraceResult `json:"result"`
	Subtraces           uint64             `json:"subtraces"`
	TraceAddress        []uint64           `json:"traceAddress"`
	TransactionHash     *common.Hash       `json:"transactionHash,omitempty"`
	TransactionPosition *uint64            `json:"transactionPosition,omitempty"`
	Type                string             `json:"type"`
}

// ParityTraceAction represents the action field in a Parity trace.
type ParityTraceAction struct {
	From           *common.Address `json:"from,omitempty"`
	To             *common.Address `json:"to,omitempty"`
	CallType       *string         `json:"callType,omitempty"`
	CreationMethod *string         `json:"creationMethod,omitempty"`
	Gas            *hexutil.Uint64 `json:"gas,omitempty"`
	Input          *hexutil.Bytes  `json:"input,omitempty"`
	Value          *hexutil.Big    `json:"value,omitempty"`
	Init           *hexutil.Bytes  `json:"init,omitempty"`
	Address        *common.Address `json:"address,omitempty"`
	RefundAddress  *common.Address `json:"refundAddress,omitempty"`
	Balance        *hexutil.Big    `json:"balance,omitempty"`
	Author         *common.Address `json:"author,omitempty"`
	RewardType     *string         `json:"rewardType,omitempty"`
}

// ParityTraceResult represents the result field in a Parity trace.
type ParityTraceResult struct {
	GasUsed *hexutil.Uint64 `json:"gasUsed,omitempty"`
	Output  *hexutil.Bytes  `json:"output,omitempty"`
	Address *common.Address `json:"address,omitempty"`
	Code    *hexutil.Bytes  `json:"code,omitempty"`
}

// TraceAPI is the collection of tracing APIs exposed over the RPC interface.
// Compatible with Parity/OpenEthereum trace_* methods.
type TraceAPI struct {
	*API
}

// Block returns Parity-format traces for all transactions in the specified block.
// This method implements the trace_block RPC method for Polygon Erigon compatibility.
//
// Defined on TraceAPI (not API) so it is only reachable through the opt-in
// trace namespace, never via the always-registered debug namespace.
func (api *TraceAPI) Block(ctx context.Context, number rpc.BlockNumber) ([]*ParityTrace, error) {
	return api.blockParity(ctx, number, nil)
}

// TraceAPIs returns the Parity/OpenEthereum-compatible "trace" namespace
// (trace_block, trace_transaction, trace_call, trace_callMany,
// trace_replayTransaction, trace_replayBlockTransactions).
//
// These methods are expensive and are NOT part of the default RPC surface; they
// must be enabled explicitly by the operator (see the rpc.enabletrace flag).
func TraceAPIs(backend Backend) []rpc.API {
	return []rpc.API{
		{
			Namespace: "trace",
			Service:   &TraceAPI{API: NewAPI(backend)},
		},
	}
}

// blockParity resolves a block by number and produces its Parity-format traces.
func (api *API) blockParity(ctx context.Context, number rpc.BlockNumber, config *TraceConfig) ([]*ParityTrace, error) {
	block, err := api.blockByNumber(ctx, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	return api.traceBlockParityByHash(ctx, block.Hash(), config)
}

// traceBlockParityByHash is the internal implementation for tracing a block in Parity format.
//
// It mirrors the sequential native-tracer path of traceBlock: each transaction
// (including the post-Madhugiri Bor state-sync transaction, which is part of the
// block body) is replayed through traceTx with the callTracer, and the resulting
// call frame is flattened into Parity/OpenEthereum trace entries. State-sync
// transactions before the Madhugiri hard fork carry no replayable event data and
// are therefore not emitted, matching upstream behaviour.
func (api *API) traceBlockParityByHash(ctx context.Context, hash common.Hash, config *TraceConfig) ([]*ParityTrace, error) {
	block, err := api.blockByHash(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("failed to get block by hash: %w", err)
	}

	exec, release, err := api.setupParityBlockExec(ctx, block)
	if err != nil {
		return nil, err
	}
	defer release()

	allTraces := make([]*ParityTrace, 0)
	var cumulativeGasUsed uint64
	for txIndex, tx := range exec.block.Transactions() {
		message, txctx, err := exec.txInput(txIndex, tx, cumulativeGasUsed)
		if err != nil {
			return nil, fmt.Errorf("failed to convert tx to message (tx %d): %w", txIndex, err)
		}

		// trace_block includes block/transaction identifiers on each entry.
		txTraces, _, gasUsed, err := api.parityTraceTx(ctx, parityExecInput{tx: tx, msg: message, txctx: txctx, vmctx: exec.blockCtx, statedb: exec.statedb, config: config}, true)
		if err != nil {
			return nil, fmt.Errorf("failed to trace tx %d: %w", txIndex, err)
		}
		cumulativeGasUsed += gasUsed

		allTraces = append(allTraces, txTraces...)
	}

	return allTraces, nil
}
