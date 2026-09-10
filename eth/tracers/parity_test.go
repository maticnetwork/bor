package tracers

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

func TestParseTraceTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   []string
		want    traceTypeSet
		wantErr bool
	}{
		{name: "empty", input: nil, want: traceTypeSet{}},
		{name: "trace only", input: []string{"trace"}, want: traceTypeSet{trace: true}},
		{name: "all", input: []string{"trace", "stateDiff", "vmTrace"}, want: traceTypeSet{trace: true, stateDiff: true, vmTrace: true}},
		{name: "duplicate", input: []string{"trace", "trace"}, want: traceTypeSet{trace: true}},
		{name: "unknown", input: []string{"bogus"}, wantErr: true},
		{name: "unknown mixed", input: []string{"trace", "bogus"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseTraceTypes(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestStripTxMeta(t *testing.T) {
	t.Parallel()

	hash := common.HexToHash("0xdead")
	num := uint64(7)
	pos := uint64(1)
	traces := []*ParityTrace{{
		BlockHash:           &hash,
		BlockNumber:         &num,
		TransactionHash:     &hash,
		TransactionPosition: &pos,
		Type:                "call",
	}}
	stripTxMeta(traces)
	tr := traces[0]
	if tr.BlockHash != nil || tr.BlockNumber != nil || tr.TransactionHash != nil || tr.TransactionPosition != nil {
		t.Fatalf("expected all tx/block meta cleared, got %+v", tr)
	}
}

// TestTraceTransactionParity exercises trace_transaction end to end against a
// synthetic chain and asserts the flat Parity output carries block/tx metadata.
func TestTraceTransactionParity(t *testing.T) {
	t.Parallel()

	traceAPI, target, from := newTransferChainAPI(t, 3)

	traces, err := traceAPI.Transaction(t.Context(), target)
	if err != nil {
		t.Fatalf("trace_transaction error: %v", err)
	}
	if len(traces) == 0 {
		t.Fatalf("expected at least one trace, got 0")
	}
	root := traces[0]
	if root.Type != "call" {
		t.Errorf("expected root type 'call', got %q", root.Type)
	}
	if len(root.TraceAddress) != 0 {
		t.Errorf("root traceAddress should be empty, got %v", root.TraceAddress)
	}
	// trace_transaction must carry block/tx identifiers.
	if root.TransactionHash == nil || *root.TransactionHash != target {
		t.Errorf("expected transactionHash %x, got %v", target, root.TransactionHash)
	}
	if root.BlockHash == nil || root.BlockNumber == nil || root.TransactionPosition == nil {
		t.Errorf("expected block/tx metadata to be populated, got %+v", root)
	}
	if root.Action == nil || root.Action.From == nil || *root.Action.From != from {
		t.Errorf("expected from %x, got %+v", from, root.Action)
	}
}

func TestTraceTransactionParity_NotFound(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(1)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {})
	defer backend.teardown()

	traceAPI := &TraceAPI{API: NewAPI(backend)}
	_, err := traceAPI.Transaction(t.Context(), crypto.Keccak256Hash([]byte("missing")))
	if err == nil {
		t.Fatalf("expected error for unknown transaction, got nil")
	}
}

func TestParityRootGasUsed(t *testing.T) {
	t.Parallel()

	msg := func(gasLimit uint64) *core.Message { return &core.Message{GasLimit: gasLimit} }

	tests := []struct {
		name      string
		in        parityExecInput
		wrapped   parityCallResult
		callFrame map[string]interface{}
		intrinsic uint64
		want      uint64
	}{
		{
			name:    "gasLeft path",
			in:      parityExecInput{msg: msg(100_000)},
			wrapped: parityCallResult{GasLeft: 40_000, GasLeftSet: true},
			want:    100_000 - 40_000 - 21_000,
		},
		{
			// Boundary: gasLimit == gasLeft+intrinsic exactly -> gross gas 0 via
			// the gasLeft path, NOT the callFrame fallback.
			name:      "gasLeft boundary equal",
			in:        parityExecInput{msg: msg(61_000)},
			wrapped:   parityCallResult{GasLeft: 40_000, GasLeftSet: true},
			callFrame: map[string]interface{}{"gasUsed": "0x7530"},
			want:      0,
		},
		{
			name:      "fallback to frame gasUsed",
			in:        parityExecInput{msg: msg(100_000)},
			wrapped:   parityCallResult{},
			callFrame: map[string]interface{}{"gasUsed": "0x7530"}, // 30000
			want:      30_000 - 21_000,
		},
		{
			name:      "fallback below intrinsic yields zero",
			in:        parityExecInput{msg: msg(100_000)},
			wrapped:   parityCallResult{},
			callFrame: map[string]interface{}{"gasUsed": "0x1000"},
			want:      0,
		},
		{
			name:      "nil msg falls back to frame",
			in:        parityExecInput{},
			wrapped:   parityCallResult{GasLeft: 40_000, GasLeftSet: true},
			callFrame: map[string]interface{}{"gasUsed": "0x7530"},
			want:      30_000 - 21_000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parityRootGasUsed(tc.in, tc.wrapped, tc.callFrame, 21_000); got != tc.want {
				t.Errorf("parityRootGasUsed = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParityTraceConfig(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		cfg := parityTraceConfig(nil)
		if cfg.Tracer == nil || *cfg.Tracer != parityCallTracerName {
			t.Errorf("tracer = %v, want %s", cfg.Tracer, parityCallTracerName)
		}
		if cfg.Timeout == nil || *cfg.Timeout != defaultParityTraceTimeout.String() {
			t.Errorf("timeout = %v, want %s", cfg.Timeout, defaultParityTraceTimeout)
		}
	})

	t.Run("caller timeout honoured, tracer forced", func(t *testing.T) {
		t.Parallel()
		timeout := "9s"
		userTracer := "callTracer"
		cfg := parityTraceConfig(&TraceConfig{Timeout: &timeout, Tracer: &userTracer})
		if cfg.Timeout == nil || *cfg.Timeout != "9s" {
			t.Errorf("timeout = %v, want 9s", cfg.Timeout)
		}
		if *cfg.Tracer != parityCallTracerName {
			t.Errorf("caller tracer must be overridden, got %s", *cfg.Tracer)
		}
	})
}

func TestParityOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		frame map[string]interface{}
		want  string
	}{
		{name: "present", frame: map[string]interface{}{"output": "0xdead"}, want: "0xdead"},
		{name: "absent defaults to 0x", frame: map[string]interface{}{}, want: "0x"},
		{name: "empty string defaults to 0x", frame: map[string]interface{}{"output": ""}, want: "0x"},
		{name: "invalid hex defaults to 0x", frame: map[string]interface{}{"output": "zz"}, want: "0x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := parityOutput(tc.frame)
			if out == nil {
				t.Fatal("parityOutput returned nil")
			}
			if got := out.String(); got != tc.want {
				t.Errorf("output = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDecodeParityCallResult(t *testing.T) {
	t.Parallel()

	t.Run("raw message", func(t *testing.T) {
		t.Parallel()
		raw := json.RawMessage(`{"frame":{"type":"CALL","gasUsed":"0x5208"},"gasLeft":100,"gasLeftSet":true}`)
		wrapped, frame, err := decodeParityCallResult(raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !wrapped.GasLeftSet || wrapped.GasLeft != 100 {
			t.Errorf("gasLeft = %d/%v, want 100/true", wrapped.GasLeft, wrapped.GasLeftSet)
		}
		if frame["type"] != "CALL" {
			t.Errorf("frame type = %v, want CALL", frame["type"])
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		t.Parallel()
		if _, _, err := decodeParityCallResult(json.RawMessage(`{bad`)); err == nil {
			t.Error("expected error for invalid json")
		}
	})

	t.Run("invalid frame", func(t *testing.T) {
		t.Parallel()
		if _, _, err := decodeParityCallResult(json.RawMessage(`{"frame":"notanobject"}`)); err == nil {
			t.Error("expected error for non-object frame")
		}
	})
}

func TestParityTxFrameCtx(t *testing.T) {
	t.Parallel()

	accounts := newAccounts(1)
	genesis := &core.Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{accounts[0].addr: {Balance: big.NewInt(params.Ether)}},
	}
	backend := newTestBackend(t, 1, genesis, func(i int, b *core.BlockGen) {})
	t.Cleanup(backend.teardown)
	api := NewAPI(backend)

	blockHash := common.HexToHash("0xbb")
	txHash := common.HexToHash("0xcafe")
	in := parityExecInput{
		txctx: &Context{
			BlockHash:   blockHash,
			BlockNumber: big.NewInt(7),
			TxIndex:     3,
			TxHash:      txHash,
		},
		vmctx: vm.BlockContext{BlockNumber: big.NewInt(7)},
	}

	fctx := api.parityTxFrameCtx(in, parityCallResult{}, map[string]interface{}{})
	if fctx.blockHash != blockHash || fctx.blockNumber != 7 {
		t.Errorf("block identity = %x/%d, want %x/7", fctx.blockHash, fctx.blockNumber, blockHash)
	}
	if fctx.txHash != txHash || fctx.txIndex != 3 {
		t.Errorf("tx identity = %x/%d, want %x/3", fctx.txHash, fctx.txIndex, txHash)
	}
	if len(fctx.precompiles) == 0 {
		t.Error("expected non-empty active precompile set")
	}

	// Without a txctx the identifiers stay zero (trace_call semantics).
	fctx = api.parityTxFrameCtx(parityExecInput{vmctx: vm.BlockContext{BlockNumber: big.NewInt(7)}}, parityCallResult{}, map[string]interface{}{})
	if fctx.blockNumber != 0 || fctx.txIndex != 0 {
		t.Errorf("nil txctx must yield zero identifiers, got %+v", fctx)
	}
}

func TestParityFrameAccessors(t *testing.T) {
	t.Parallel()

	frame := parityFrame{
		"float":  float64(42),
		"uint":   uint64(43),
		"other":  true,
		"empty":  "",
		"badBig": "not-a-number",
		"to":     "0x00000000000000000000000000000000000000aa",
	}
	if got := frame.str("float"); got != "0x2a" {
		t.Errorf("float string = %q, want 0x2a", got)
	}
	if got := frame.str("uint"); got != "0x2b" {
		t.Errorf("uint string = %q, want 0x2b", got)
	}
	if got := frame.str("other"); got != "" {
		t.Errorf("unsupported value string = %q, want empty", got)
	}
	if got := frame.bigOrZero("badBig"); got.Sign() != 0 {
		t.Errorf("invalid big integer = %v, want zero", got)
	}
	if to, ok := frame.to(); !ok || to != common.HexToAddress(frame["to"].(string)) {
		t.Errorf("to = %x/%v, want configured address", to, ok)
	}
	for _, invalid := range []parityFrame{{}, {"to": ""}, {"to": uint64(1)}} {
		if _, ok := invalid.to(); ok {
			t.Errorf("invalid to value unexpectedly accepted: %#v", invalid["to"])
		}
	}
}

func TestParityConversionEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("call type variants", func(t *testing.T) {
		t.Parallel()
		tests := map[string]string{
			"DELEGATECALL": "delegatecall",
			"STATICCALL":   "staticcall",
			"CALLCODE":     "callcode",
		}
		for opcode, want := range tests {
			traceType, callType := parityTraceType(opcode)
			if traceType != "call" || callType == nil || *callType != want {
				t.Errorf("%s mapped to %q/%v, want call/%s", opcode, traceType, callType, want)
			}
		}
		for _, opcode := range []string{"CREATE2", "SUICIDE"} {
			traceType, callType := parityTraceType(opcode)
			if callType != nil || (traceType != "create" && traceType != "suicide") {
				t.Errorf("%s mapped to %q/%v", opcode, traceType, callType)
			}
		}
	})

	t.Run("malformed child is ignored", func(t *testing.T) {
		t.Parallel()
		frame := parityFrame{"calls": []interface{}{"not-an-object", map[string]interface{}{"type": "CALL"}}}
		children := parityChildFrames(frame, nil)
		if len(children) != 1 {
			t.Fatalf("children = %d, want 1 valid frame", len(children))
		}
	})

	t.Run("create2 action", func(t *testing.T) {
		t.Parallel()
		action := parityCallAction(parityFrame{"type": "CREATE2", "input": "0x01"}, "create", nil, 0)
		if action.CreationMethod == nil || *action.CreationMethod != "create2" {
			t.Errorf("creation method = %v, want create2", action.CreationMethod)
		}
		if action.Init == nil || action.Input != nil {
			t.Errorf("create2 input fields = init:%v input:%v", action.Init, action.Input)
		}
	})

	t.Run("optional gas and input", func(t *testing.T) {
		t.Parallel()
		for _, gas := range []string{"", "invalid"} {
			action := new(ParityTraceAction)
			setParityActionGas(action, gas, 0)
			if action.Gas != nil {
				t.Errorf("gas %q produced %v, want nil", gas, action.Gas)
			}
		}
		action := new(ParityTraceAction)
		setParityActionInput(action, "", "call")
		if action.Input != nil || action.Init != nil {
			t.Errorf("empty input populated action: %+v", action)
		}
	})

	t.Run("invalid subcall gas used", func(t *testing.T) {
		t.Parallel()
		for _, gasUsed := range []string{"", "invalid"} {
			if got := parityResultGasUsed(gasUsed, false, 0); got != nil {
				t.Errorf("gasUsed %q = %v, want nil", gasUsed, got)
			}
		}
	})

	t.Run("precompile exclusions", func(t *testing.T) {
		t.Parallel()
		addr := common.HexToAddress("0x01")
		precompiles := map[common.Address]struct{}{addr: {}}
		tests := []map[string]interface{}{
			{"type": "CREATE", "to": addr.Hex()},
			{"type": "CALL"},
			{"type": "CALL", "to": common.HexToAddress("0xff").Hex()},
			{"type": "CALL", "to": addr.Hex(), "value": "0x1"},
		}
		for _, frame := range tests {
			if isPrecompileFrame(frame, true, precompiles) {
				t.Errorf("frame should be retained: %#v", frame)
			}
		}
		if !isPrecompileFrame(map[string]interface{}{"type": "CALL", "to": addr.Hex()}, true, precompiles) {
			t.Error("zero-value precompile subcall should be filtered")
		}
	})
}
