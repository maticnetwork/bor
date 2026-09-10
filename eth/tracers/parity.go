// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.

package tracers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

// defaultParityTraceTimeout is the per-tx timeout used by the Parity trace_*
// methods. Parity-style replay of heavy historical transactions (deep
// account-abstraction call trees) can take well over 5 s; the trace namespace
// is opt-in (--rpc.enabletrace) and archive-oriented, so a longer default is
// appropriate without affecting debug_trace* callers.
const defaultParityTraceTimeout = 60 * time.Second

// ReplayResult is the result wrapper returned by the Parity/OpenEthereum
// trace_call, trace_callMany, trace_replayTransaction and
// trace_replayBlockTransactions methods.
//
// The Output field is always present. The Trace, StateDiff and VMTrace fields
// are populated only when the corresponding trace type is requested via the
// traceTypes argument; otherwise they serialize to null.
type ReplayResult struct {
	Output    *hexutil.Bytes `json:"output"`
	StateDiff interface{}    `json:"stateDiff"`
	Trace     []*ParityTrace `json:"trace"`
	VMTrace   interface{}    `json:"vmTrace"`
	// TransactionHash is set by trace_replayTransaction / trace_replayBlockTransactions
	// (which replay a real, mined tx) and omitted by trace_call / trace_callMany.
	TransactionHash *common.Hash `json:"transactionHash,omitempty"`
}

// traceTypeSet captures which Parity trace outputs the caller requested.
type traceTypeSet struct {
	trace     bool
	stateDiff bool
	vmTrace   bool
}

// gasBailoutStateDB prevents trace_call's synthetic gas purchase and refund
// from changing the balance observed by the simulated call. Other balance
// changes, including value transfers, continue to use the underlying state.
type gasBailoutStateDB struct {
	vm.StateDB
}

func (s *gasBailoutStateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if reason == tracing.BalanceDecreaseGasBuy {
		return *s.GetBalance(addr)
	}
	return s.StateDB.SubBalance(addr, amount, reason)
}

func (s *gasBailoutStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	if reason == tracing.BalanceIncreaseGasReturn {
		return *s.GetBalance(addr)
	}
	return s.StateDB.AddBalance(addr, amount, reason)
}

// parseTraceTypes validates and parses the Parity traceTypes argument
// (e.g. ["trace", "stateDiff", "vmTrace"]). Unknown types are rejected.
func parseTraceTypes(types []string) (traceTypeSet, error) {
	var set traceTypeSet
	for _, t := range types {
		switch t {
		case "trace":
			set.trace = true
		case "stateDiff":
			set.stateDiff = true
		case "vmTrace":
			set.vmTrace = true
		default:
			return set, fmt.Errorf("unknown trace type: %q", t)
		}
	}
	return set, nil
}

// stripTxMeta clears the per-transaction/block identifier fields from trace
// entries. Parity's trace_call / trace_replay* responses omit these fields,
// whereas trace_block / trace_transaction include them.
func stripTxMeta(traces []*ParityTrace) {
	for _, t := range traces {
		t.BlockHash = nil
		t.BlockNumber = nil
		t.TransactionHash = nil
		t.TransactionPosition = nil
	}
}

// parityExecInput bundles the per-transaction parameters shared by
// parityTraceTx, parityStateDiffFor, and parityVMTraceFor so callers can pass
// them as a single value instead of repeating the same argument list.
type parityExecInput struct {
	tx      *types.Transaction
	msg     *core.Message
	txctx   *Context
	vmctx   vm.BlockContext
	statedb *state.StateDB
	config  *TraceConfig
}

// withStateCopy returns a copy of in with a freshly copied statedb. Used to run
// stateDiff/vmTrace on a pre-execution snapshot without consuming the live state.
func (in parityExecInput) withStateCopy() parityExecInput {
	cp := in
	cp.statedb = in.statedb.Copy()
	return cp
}

// buildReplayResult runs the requested parity tracers for one transaction and
// returns the populated ReplayResult plus the gross EVM gas used.
// statedb is consumed (advanced) by the trace run; callers that need to
// continue replaying subsequent transactions must pass a snapshot copy.
func (api *API) buildReplayResult(ctx context.Context, in parityExecInput, set traceTypeSet, includeTxMeta bool) (*ReplayResult, uint64, error) {
	result := &ReplayResult{}

	if set.stateDiff {
		sd, err := api.parityStateDiffFor(ctx, in.withStateCopy())
		if err != nil {
			return nil, 0, err
		}
		result.StateDiff = sd
	}

	if set.vmTrace {
		vt, err := api.parityVMTraceFor(ctx, in.withStateCopy())
		if err != nil {
			return nil, 0, err
		}
		result.VMTrace = vt
	}

	traces, output, gasUsed, err := api.parityTraceTx(ctx, in, includeTxMeta)
	if err != nil {
		return nil, 0, err
	}
	result.Output = output
	if set.trace {
		result.Trace = traces
	}
	return result, gasUsed, nil
}

// parityBlockExec holds the resolved pre-state and EVM block context for
// replaying every transaction in a block. It is shared by the Parity block-level
// trace methods (trace_block and trace_replayBlockTransactions) so both use one
// copy of the state-setup and per-transaction message/context boilerplate.
type parityBlockExec struct {
	block    *types.Block
	statedb  *state.StateDB
	blockCtx vm.BlockContext
	signer   types.Signer
}

// setupParityBlockExec resolves the parent pre-state for block and runs the EVM
// system calls (beacon root, parent block hash) required before replaying the
// block's transactions. The caller MUST invoke the returned release function; it
// is non-nil only on success (error returns leave state unallocated).
func (api *API) setupParityBlockExec(ctx context.Context, block *types.Block) (*parityBlockExec, StateReleaseFunc, error) {
	if block.NumberU64() == 0 {
		return nil, nil, errors.New("genesis block is not traceable")
	}

	parent, err := api.blockByNumberAndHash(ctx, rpc.BlockNumber(block.NumberU64()-1), block.ParentHash())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get parent block: %w", err)
	}

	statedb, release, err := api.backend.StateAtBlock(ctx, parent, nil, true, false)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get state at block %d: %w (archive node required for historical blocks)", parent.NumberU64(), err)
	}

	// author=nil lets NewEVMBlockContext resolve the bor coinbase (CalculateCoinbase
	// post-Rio), which is the real gas-fee tip recipient.
	blockCtx := core.NewEVMBlockContext(block.Header(), api.chainContext(ctx), nil)
	evm := vm.NewEVM(blockCtx, statedb, api.backend.ChainConfig(), vm.Config{})
	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		core.ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if api.backend.ChainConfig().IsPrague(block.Number()) {
		core.ProcessParentBlockHash(block.ParentHash(), evm)
	}

	return &parityBlockExec{
		block:    block,
		statedb:  statedb,
		blockCtx: blockCtx,
		signer:   types.MakeSigner(api.backend.ChainConfig(), block.Number(), block.Time()),
	}, release, nil
}

// txInput builds the message and per-transaction Context for the txIndex-th
// transaction of the block. cumulativeGasUsed feeds the Context field consulted
// only for Bor state-sync transactions (always the block's last tx).
func (e *parityBlockExec) txInput(txIndex int, tx *types.Transaction, cumulativeGasUsed uint64) (*core.Message, *Context, error) {
	message, err := core.TransactionToMessage(tx, e.signer, e.block.BaseFee())
	if err != nil {
		return nil, nil, err
	}
	txctx := &Context{
		BlockHash:         e.block.Hash(),
		BlockNumber:       e.block.Number(),
		TxIndex:           txIndex,
		TxHash:            tx.Hash(),
		CumulativeGasUsed: cumulativeGasUsed,
		LogIndex:          len(e.statedb.Logs()),
	}
	return message, txctx, nil
}

// parityPhaseConfig builds the TraceConfig for one Parity output phase (trace,
// stateDiff or vmTrace) forcing the given tracer. Only Timeout is
// honoured from the caller's config, and an unset timeout falls back to the
// Parity-specific (longer) default — every phase re-executes the transaction,
// so each must get the full budget rather than defaultTraceTimeout (5s). This
// avoids inflating defaultTraceTimeout for debug_trace*.
func parityPhaseConfig(tracerName string, tracerCfg json.RawMessage, base *TraceConfig) *TraceConfig {
	cfg := &TraceConfig{
		Tracer:       &tracerName,
		TracerConfig: tracerCfg,
	}
	if base != nil {
		cfg.Timeout = base.Timeout
		cfg.gasBailout = base.gasBailout
	}
	if cfg.Timeout == nil {
		s := defaultParityTraceTimeout.String()
		cfg.Timeout = &s
	}
	return cfg
}

// parityTraceConfig builds the TraceConfig for the trace phase: the
// parityCallTracer is always forced (the conversion requires a structured call
// frame plus the tx gas refund for gross root gasUsed).
func parityTraceConfig(base *TraceConfig) *TraceConfig {
	return parityPhaseConfig(parityCallTracerName, json.RawMessage(`{}`), base)
}

// decodeParityCallResult unpacks a traceTx result into the parityCallTracer
// wrapper and its embedded callTracer frame.
func decodeParityCallResult(res interface{}) (parityCallResult, map[string]interface{}, error) {
	var wrapped parityCallResult

	// The tracer returns json.RawMessage; marshal defensively otherwise.
	raw, ok := res.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(res); err != nil {
			return wrapped, nil, fmt.Errorf("marshal trace result: %w", err)
		}
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return wrapped, nil, fmt.Errorf("unmarshal trace result: %w", err)
	}

	var callFrame map[string]interface{}
	if err := json.Unmarshal(wrapped.Frame, &callFrame); err != nil {
		return wrapped, nil, fmt.Errorf("unmarshal call frame: %w", err)
	}
	return wrapped, callFrame, nil
}

// parityIntrinsicGas computes the exact transaction intrinsic gas so the root
// trace's gas/gasUsed exclude it (Parity/erigon semantics). Using
// core.IntrinsicGas handles access lists, auth lists and the relevant EIPs
// precisely. Returns 0 for a nil message.
func (api *API) parityIntrinsicGas(in parityExecInput) uint64 {
	if in.msg == nil {
		return 0
	}
	rules := api.backend.ChainConfig().Rules(in.vmctx.BlockNumber, in.vmctx.Random != nil, in.vmctx.Time)
	ig, err := core.IntrinsicGas(in.msg.Data, in.msg.AccessList, in.msg.SetCodeAuthorizations, in.msg.To == nil, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai)
	if err != nil {
		return 0
	}
	return ig
}

// parityRootGasUsed computes the root trace's gross EVM execution gas =
// gasLimit - postExecGasRemaining - intrinsic. This excludes the intrinsic
// cost, the EIP-7623 data floor and gas refunds, matching erigon. Falls back to
// the callTracer's (net) gasUsed if the post-execution gas wasn't observed.
func parityRootGasUsed(in parityExecInput, wrapped parityCallResult, callFrame map[string]interface{}, intrinsicGas uint64) uint64 {
	if in.msg != nil && wrapped.GasLeftSet && in.msg.GasLimit >= wrapped.GasLeft+intrinsicGas {
		return in.msg.GasLimit - wrapped.GasLeft - intrinsicGas
	}
	if s, ok := callFrame["gasUsed"].(string); ok {
		if gu, err := hexutil.DecodeUint64(s); err == nil && gu >= intrinsicGas {
			return gu - intrinsicGas
		}
	}
	return 0
}

// parityBlockPrecompiles returns the active precompile set for the block so
// isPrecompileFrame can filter all fork-appropriate precompile addresses
// (standard 0x01-0x0a, BLS 0x0b-0x11, p256 0x0100) rather than a hardcoded range.
func (api *API) parityBlockPrecompiles(in parityExecInput) map[common.Address]struct{} {
	rules := api.backend.ChainConfig().Rules(in.vmctx.BlockNumber, in.vmctx.Random != nil, in.vmctx.Time)
	activeAddrs := vm.ActivePrecompiles(rules)
	precompileSet := make(map[common.Address]struct{}, len(activeAddrs))
	for _, a := range activeAddrs {
		precompileSet[a] = struct{}{}
	}
	return precompileSet
}

// parityTxFrameCtx assembles the per-transaction conversion context from the
// trace inputs and the decoded tracer result.
func (api *API) parityTxFrameCtx(in parityExecInput, wrapped parityCallResult, callFrame map[string]interface{}) parityFrameCtx {
	fctx := parityFrameCtx{precompiles: api.parityBlockPrecompiles(in)}
	if in.txctx != nil {
		fctx.blockHash = in.txctx.BlockHash
		if in.txctx.BlockNumber != nil {
			fctx.blockNumber = in.txctx.BlockNumber.Uint64()
		}
		fctx.txHash = in.txctx.TxHash
		fctx.txIndex = uint64(in.txctx.TxIndex)
	}
	fctx.intrinsicGas = api.parityIntrinsicGas(in)
	fctx.rootGasUsed = parityRootGasUsed(in, wrapped, callFrame, fctx.intrinsicGas)
	return fctx
}

// parityTraceTx executes a single message with the callTracer and flattens the
// resulting call frame into Parity traces. It also returns the top-level call
// output and the gas used by the execution.
//
// The block/transaction identifier fields on the returned traces are populated
// from txctx; when includeTxMeta is false they are cleared (trace_call /
// trace_replay* semantics). The callTracer is always forced regardless of any
// tracer set on the input config, since the Parity conversion requires a
// structured call frame; only Timeout is honoured from it.
func (api *API) parityTraceTx(ctx context.Context, in parityExecInput, includeTxMeta bool) ([]*ParityTrace, *hexutil.Bytes, uint64, error) {
	res, gasUsed, err := api.traceTx(ctx, in.tx, in.msg, in.txctx, in.vmctx, in.statedb, parityTraceConfig(in.config), nil)
	if err != nil {
		return nil, nil, 0, err
	}

	wrapped, callFrame, err := decodeParityCallResult(res)
	if err != nil {
		return nil, nil, 0, err
	}

	traces, err := convertCallFrameToParityTraces(callFrame, []uint64{}, api.parityTxFrameCtx(in, wrapped, callFrame))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("convert trace: %w", err)
	}
	if !includeTxMeta {
		stripTxMeta(traces)
	}

	return traces, parityOutput(callFrame), gasUsed, nil
}

// parityOutput extracts the top-level call output as hex bytes, defaulting to
// an empty byte slice (which serializes to "0x").
func parityOutput(callFrame map[string]interface{}) *hexutil.Bytes {
	out := hexutil.Bytes{}
	if o, ok := callFrame["output"].(string); ok && o != "" {
		if b, err := hexutil.Decode(o); err == nil {
			out = b
		}
	}
	return &out
}

// Transaction implements the trace_transaction RPC method: it returns the
// Parity-format traces for a single mined transaction identified by its hash.
func (api *TraceAPI) Transaction(ctx context.Context, txHash common.Hash) ([]*ParityTrace, error) {
	return api.transactionParity(ctx, txHash, nil)
}

// canonicalTxTraceEnv resolves a mined transaction by hash and prepares
// everything needed to re-execute it at its position in its block: the tx, its
// message, per-transaction Context, EVM block context and the historical state.
// Shared by debug_traceTransaction and trace_transaction. The caller must
// invoke the returned release function on success.
func (api *API) canonicalTxTraceEnv(ctx context.Context, hash common.Hash, config *TraceConfig) (parityExecInput, StateReleaseFunc, error) {
	found, _, blockHash, blockNumber, index := api.backend.GetCanonicalTransaction(hash)
	if !found {
		// Warn in case tx indexer is not done.
		if !api.backend.TxIndexDone() {
			return parityExecInput{}, nil, ethapi.NewTxIndexingError()
		}
		// Only mined txes are supported.
		return parityExecInput{}, nil, errTxNotFound
	}
	if blockNumber == 0 {
		return parityExecInput{}, nil, errors.New("genesis is not traceable")
	}

	block, err := api.blockByNumberAndHash(ctx, rpc.BlockNumber(blockNumber), blockHash)
	if err != nil {
		return parityExecInput{}, nil, err
	}

	tx, vmctx, statedb, release, err := api.backend.StateAtTransaction(ctx, block, int(index))
	if err != nil {
		return parityExecInput{}, nil, err
	}

	txctx := &Context{
		BlockHash:   blockHash,
		BlockNumber: block.Number(),
		TxIndex:     int(index),
		TxHash:      hash,
		// CumulativeGasUsed is only consulted for Bor state-sync transactions,
		// which are always the last tx in a block; use the block's total gas.
		CumulativeGasUsed: block.GasUsed(),
		LogIndex:          len(statedb.Logs()),
	}

	msg, err := core.TransactionToMessage(tx, types.MakeSigner(api.backend.ChainConfig(), block.Number(), block.Time()), block.BaseFee())
	if err != nil {
		release()
		return parityExecInput{}, nil, err
	}

	return parityExecInput{tx: tx, msg: msg, txctx: txctx, vmctx: vmctx, statedb: statedb, config: config}, release, nil
}

// transactionParity is the internal implementation backing trace_transaction.
func (api *API) transactionParity(ctx context.Context, hash common.Hash, config *TraceConfig) ([]*ParityTrace, error) {
	in, release, err := api.canonicalTxTraceEnv(ctx, hash, config)
	if err != nil {
		return nil, err
	}
	defer release()

	traces, _, _, err := api.parityTraceTx(ctx, in, true)
	return traces, err
}
