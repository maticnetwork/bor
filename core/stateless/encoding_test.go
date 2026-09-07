package stateless

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// TestDecodeRLP_BorWitnessFormat verifies that a witness RLP-encoded in the
// canonical 3-field BorWitness format is correctly decoded.
func TestDecodeRLP_BorWitnessFormat(t *testing.T) {
	contextHeader := &types.Header{Number: big.NewInt(100)}
	parentHeader := &types.Header{Number: big.NewInt(99)}
	node1 := []byte("statenode1")
	node2 := []byte("statenode2")

	bw := &BorWitness{
		Context: contextHeader,
		Headers: []*types.Header{parentHeader},
		State:   [][]byte{node1, node2},
	}

	encoded, err := rlp.EncodeToBytes(bw)
	if err != nil {
		t.Fatalf("failed to encode BorWitness: %v", err)
	}

	var w Witness
	if err := rlp.DecodeBytes(encoded, &w); err != nil {
		t.Fatalf("DecodeRLP failed for BorWitness input: %v", err)
	}

	if w.context.Number.Cmp(contextHeader.Number) != 0 {
		t.Errorf("context number: got %v, want %v", w.context.Number, contextHeader.Number)
	}
	if len(w.Headers) != 1 || w.Headers[0].Number.Cmp(parentHeader.Number) != 0 {
		t.Errorf("headers mismatch after decode")
	}

	// All State items from BorWitness should land in w.State.
	if len(w.State) != 2 {
		t.Errorf("state len: got %d, want 2", len(w.State))
	}
	for _, node := range [][]byte{node1, node2} {
		if _, ok := w.State[string(node)]; !ok {
			t.Errorf("state node %q missing after decode", node)
		}
	}

	// Codes are not part of the BorWitness wire format.
	if len(w.Codes) != 0 {
		t.Errorf("Codes should be empty after BorWitness decode, got %d entries", len(w.Codes))
	}
}

// TestDecodeRLP_ExtWitnessFormat verifies backward compatibility: a witness
// encoded in the legacy 5-field ExtWitness format is correctly decoded via the
// fallback path, with Codes and State placed in their respective internal maps.
func TestDecodeRLP_ExtWitnessFormat(t *testing.T) {
	contextHeader := &types.Header{Number: big.NewInt(100)}
	parentHeader := &types.Header{Number: big.NewInt(99)}
	code1 := hexutil.Bytes("contractbytecode")
	node1 := hexutil.Bytes("statetrienode")

	ext := &ExtWitness{
		Context: contextHeader,
		Headers: []*types.Header{parentHeader},
		Codes:   []hexutil.Bytes{code1},
		State:   []hexutil.Bytes{node1},
		Keys:    nil,
	}

	encoded, err := rlp.EncodeToBytes(ext)
	if err != nil {
		t.Fatalf("failed to encode ExtWitness: %v", err)
	}

	var w Witness
	if err := rlp.DecodeBytes(encoded, &w); err != nil {
		t.Fatalf("DecodeRLP failed for ExtWitness input: %v", err)
	}

	if w.context.Number.Cmp(contextHeader.Number) != 0 {
		t.Errorf("context number: got %v, want %v", w.context.Number, contextHeader.Number)
	}
	if len(w.Headers) != 1 || w.Headers[0].Number.Cmp(parentHeader.Number) != 0 {
		t.Errorf("headers mismatch after decode")
	}

	if len(w.Codes) != 1 {
		t.Errorf("Codes len: got %d, want 1", len(w.Codes))
	}
	if _, ok := w.Codes[string(code1)]; !ok {
		t.Errorf("code %q missing from Codes after ExtWitness decode", code1)
	}

	if len(w.State) != 1 {
		t.Errorf("State len: got %d, want 1", len(w.State))
	}
	if _, ok := w.State[string(node1)]; !ok {
		t.Errorf("node %q missing from State after ExtWitness decode", node1)
	}
}

// TestEncodeRLP_UsesBorWitnessFormat verifies that EncodeRLP produces the
// canonical 3-field BorWitness format and that codes are excluded from it.
func TestEncodeRLP_UsesBorWitnessFormat(t *testing.T) {
	w := &Witness{
		context: &types.Header{Number: big.NewInt(100)},
		Headers: []*types.Header{{Number: big.NewInt(99)}},
		Codes:   map[string]struct{}{"contractcode": {}},
		State:   map[string]struct{}{"statenode": {}},
	}

	encoded, err := rlp.EncodeToBytes(w)
	if err != nil {
		t.Fatalf("EncodeRLP failed: %v", err)
	}

	// The output must be decodable as BorWitness (3 fields).
	var bw BorWitness
	if err := rlp.DecodeBytes(encoded, &bw); err != nil {
		t.Fatalf("encoded output is not a valid BorWitness: %v", err)
	}

	// Only State nodes should be present — codes are not encoded.
	if len(bw.State) != 1 || string(bw.State[0]) != "statenode" {
		t.Errorf("BorWitness.State = %v, want [statenode]", bw.State)
	}

	// The output must NOT be decodable as ExtWitness (5 fields).
	var ext ExtWitness
	if err := rlp.DecodeBytes(encoded, &ext); err == nil {
		t.Error("expected ExtWitness decode to fail for BorWitness output, but it succeeded")
	}
}

// TestRoundtrip_BorWitnessFormat verifies the full encode→decode cycle using
// EncodeRLP and DecodeRLP (BorWitness path).
func TestRoundtrip_BorWitnessFormat(t *testing.T) {
	original := &Witness{
		context: &types.Header{Number: big.NewInt(200)},
		Headers: []*types.Header{{Number: big.NewInt(199)}, {Number: big.NewInt(198)}},
		Codes:   map[string]struct{}{"code": {}},
		State:   map[string]struct{}{"node1": {}, "node2": {}},
	}

	encoded, err := rlp.EncodeToBytes(original)
	if err != nil {
		t.Fatalf("EncodeRLP failed: %v", err)
	}

	var decoded Witness
	if err := rlp.DecodeBytes(encoded, &decoded); err != nil {
		t.Fatalf("DecodeRLP failed: %v", err)
	}

	if decoded.context.Number.Cmp(original.context.Number) != 0 {
		t.Errorf("context number: got %v, want %v", decoded.context.Number, original.context.Number)
	}
	if len(decoded.Headers) != len(original.Headers) {
		t.Errorf("headers count: got %d, want %d", len(decoded.Headers), len(original.Headers))
	}

	// State nodes are preserved across the roundtrip.
	for node := range original.State {
		if _, ok := decoded.State[node]; !ok {
			t.Errorf("state node %q missing after roundtrip", node)
		}
	}
	if len(decoded.State) != len(original.State) {
		t.Errorf("state len: got %d, want %d", len(decoded.State), len(original.State))
	}

	// Codes are not in the BorWitness wire format and will be empty after decode.
	if len(decoded.Codes) != 0 {
		t.Errorf("Codes should be empty after BorWitness roundtrip, got %d", len(decoded.Codes))
	}
}

// TestEncodeRLP_DeterministicAcrossInsertionOrder is the regression test for
// the WIT2 byte-blame model. State entries arrive via a Go map, whose
// iteration order is randomised, so without sorting in EncodeRLP two
// witnesses with identical logical content would encode to different bytes
// and hash differently. Receivers verifying response bytes against the BP-
// signed witness hash would falsely drop honest peers.
func TestEncodeRLP_DeterministicAcrossInsertionOrder(t *testing.T) {
	const N = 64
	nodes := make([][]byte, N)
	for i := 0; i < N; i++ {
		nodes[i] = []byte{byte(i), byte(i ^ 0x5a), byte(i ^ 0xa5)}
	}

	makeWitness := func(insertionOrder []int) *Witness {
		w := &Witness{
			Headers: []*types.Header{{Number: big.NewInt(1)}},
			Codes:   make(map[string]struct{}),
			State:   make(map[string]struct{}, len(insertionOrder)),
		}
		w.context = &types.Header{Number: big.NewInt(2)}
		for _, i := range insertionOrder {
			w.State[string(nodes[i])] = struct{}{}
		}
		return w
	}

	encode := func(w *Witness) []byte {
		raw, err := rlp.EncodeToBytes(w)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return raw
	}

	forward := make([]int, N)
	for i := range forward {
		forward[i] = i
	}
	reverse := make([]int, N)
	for i := range reverse {
		reverse[i] = N - 1 - i
	}

	wForward := makeWitness(forward)
	wReverse := makeWitness(reverse)
	if got, want := encode(wForward), encode(wReverse); string(got) != string(want) {
		t.Fatalf("EncodeRLP must be deterministic across map insertion orders; got divergent bytes (%d vs %d)", len(got), len(want))
	}

	// Re-encoding the same witness multiple times must also yield identical
	// bytes, even though Go map iteration is fresh each call.
	first := encode(wForward)
	for i := 0; i < 5; i++ {
		if string(encode(wForward)) != string(first) {
			t.Fatalf("repeat encode call %d differs from first", i)
		}
	}
}
