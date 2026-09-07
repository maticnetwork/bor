package miner

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// mockChainHeaderReader implements consensus.ChainHeaderReader for testing.
type mockChainHeaderReader struct {
	headers map[common.Hash]*types.Header
	byNum   map[uint64]*types.Header
}

func newMockChainHeaderReader() *mockChainHeaderReader {
	return &mockChainHeaderReader{
		headers: make(map[common.Hash]*types.Header),
		byNum:   make(map[uint64]*types.Header),
	}
}

func (m *mockChainHeaderReader) addHeader(h *types.Header) {
	m.headers[h.Hash()] = h
	m.byNum[h.Number.Uint64()] = h
}

func (m *mockChainHeaderReader) Config() *params.ChainConfig        { return params.TestChainConfig }
func (m *mockChainHeaderReader) CurrentHeader() *types.Header       { return nil }
func (m *mockChainHeaderReader) GetTd(common.Hash, uint64) *big.Int { return big.NewInt(1) }

func (m *mockChainHeaderReader) GetHeader(hash common.Hash, number uint64) *types.Header {
	h, ok := m.headers[hash]
	if ok && h.Number.Uint64() == number {
		return h
	}
	return nil
}

func (m *mockChainHeaderReader) GetHeaderByNumber(number uint64) *types.Header {
	return m.byNum[number]
}

func (m *mockChainHeaderReader) GetHeaderByHash(hash common.Hash) *types.Header {
	return m.headers[hash]
}

func TestSpeculativeChainReader_InterceptsPlaceholder(t *testing.T) {
	inner := newMockChainHeaderReader()

	// Build a simple chain: block 8 (committed), block 9 (pending)
	header8 := &types.Header{Number: big.NewInt(8), Extra: []byte("block8")}
	inner.addHeader(header8)

	// Block 9 is pending — not in the chain DB
	pendingHeader9 := &types.Header{
		Number:     big.NewInt(9),
		ParentHash: header8.Hash(),
		Extra:      []byte("block9-pending"),
	}

	placeholder := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	reader := newSpeculativeChainReader(inner, pendingHeader9, placeholder)

	// GetHeader with placeholder hash and number 9 should return pending header
	got := reader.GetHeader(placeholder, 9)
	if got == nil {
		t.Fatal("GetHeader(placeholder, 9) returned nil")
	}
	if got.Number.Uint64() != 9 {
		t.Errorf("expected block 9, got %d", got.Number.Uint64())
	}
	if string(got.Extra) != "block9-pending" {
		t.Errorf("expected pending header extra, got %s", string(got.Extra))
	}

	// GetHeaderByHash with placeholder should return pending header
	got = reader.GetHeaderByHash(placeholder)
	if got == nil {
		t.Fatal("GetHeaderByHash(placeholder) returned nil")
	}
	if got.Number.Uint64() != 9 {
		t.Errorf("expected block 9, got %d", got.Number.Uint64())
	}

	// GetHeaderByNumber(9) should return pending header
	got = reader.GetHeaderByNumber(9)
	if got == nil {
		t.Fatal("GetHeaderByNumber(9) returned nil")
	}
	if string(got.Extra) != "block9-pending" {
		t.Errorf("expected pending header, got %s", string(got.Extra))
	}
}

func TestSpeculativeChainReader_DelegatesNonPlaceholder(t *testing.T) {
	inner := newMockChainHeaderReader()

	header7 := &types.Header{Number: big.NewInt(7), Extra: []byte("block7")}
	header8 := &types.Header{Number: big.NewInt(8), Extra: []byte("block8")}
	inner.addHeader(header7)
	inner.addHeader(header8)

	pendingHeader9 := &types.Header{
		Number:     big.NewInt(9),
		ParentHash: header8.Hash(),
	}

	placeholder := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	reader := newSpeculativeChainReader(inner, pendingHeader9, placeholder)

	// Looking up block 8 by its real hash should delegate to inner
	got := reader.GetHeader(header8.Hash(), 8)
	if got == nil {
		t.Fatal("GetHeader(block8Hash, 8) returned nil")
	}
	if string(got.Extra) != "block8" {
		t.Errorf("expected block8 header, got %s", string(got.Extra))
	}

	// GetHeaderByNumber(7) should delegate
	got = reader.GetHeaderByNumber(7)
	if got == nil {
		t.Fatal("GetHeaderByNumber(7) returned nil")
	}
	if string(got.Extra) != "block7" {
		t.Errorf("expected block7 header, got %s", string(got.Extra))
	}

	// Unknown hash should return nil
	got = reader.GetHeader(common.HexToHash("0x1234"), 99)
	if got != nil {
		t.Error("expected nil for unknown hash")
	}
}

func TestSpeculativeChainReader_WalkThroughPending(t *testing.T) {
	// Simulate the snapshot walk: start at pending block 9, walk to block 8 (in chain)
	inner := newMockChainHeaderReader()

	header7 := &types.Header{Number: big.NewInt(7), Extra: []byte("block7")}
	header8 := &types.Header{Number: big.NewInt(8), ParentHash: header7.Hash(), Extra: []byte("block8")}
	inner.addHeader(header7)
	inner.addHeader(header8)

	pendingHeader9 := &types.Header{
		Number:     big.NewInt(9),
		ParentHash: header8.Hash(),
		Extra:      []byte("block9-pending"),
	}

	placeholder := common.HexToHash("0xdeadbeef00000000000000000000000000000000000000000000000000000000")
	reader := newSpeculativeChainReader(inner, pendingHeader9, placeholder)

	// Step 1: look up block 9 via placeholder → returns pending header
	h9 := reader.GetHeader(placeholder, 9)
	if h9 == nil {
		t.Fatal("step 1: pending header not found")
	}

	// Step 2: walk to block 8 using h9.ParentHash (= header8.Hash(), a real hash)
	h8 := reader.GetHeader(h9.ParentHash, 8)
	if h8 == nil {
		t.Fatal("step 2: block 8 not found via ParentHash walk")
	}
	if string(h8.Extra) != "block8" {
		t.Errorf("step 2: expected block8, got %s", string(h8.Extra))
	}

	// Step 3: walk to block 7 using h8.ParentHash
	h7 := reader.GetHeader(h8.ParentHash, 7)
	if h7 == nil {
		t.Fatal("step 3: block 7 not found via ParentHash walk")
	}
	if string(h7.Extra) != "block7" {
		t.Errorf("step 3: expected block7, got %s", string(h7.Extra))
	}
}

func TestSpeculativeChainReader_Config(t *testing.T) {
	inner := newMockChainHeaderReader()
	pendingHeader := &types.Header{Number: big.NewInt(5)}
	reader := newSpeculativeChainReader(inner, pendingHeader, common.Hash{})

	if reader.Config() != params.TestChainConfig {
		t.Error("Config() should delegate to inner")
	}
}

func TestSpeculativeChainContext_Engine(t *testing.T) {
	inner := newMockChainHeaderReader()
	pendingHeader := &types.Header{Number: big.NewInt(5)}
	reader := newSpeculativeChainReader(inner, pendingHeader, common.Hash{})

	var mockEngine consensus.Engine // nil for testing
	ctx := newSpeculativeChainContext(reader, mockEngine)

	if ctx.Engine() != mockEngine {
		t.Error("Engine() should return the provided engine")
	}
}
