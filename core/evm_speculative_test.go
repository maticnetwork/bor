package core

import (
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// mockChainContext implements ChainContext for testing SpeculativeGetHashFn.
type mockChainContext struct {
	headers map[uint64]*types.Header
}

func (m *mockChainContext) Config() *params.ChainConfig {
	return params.TestChainConfig
}

func (m *mockChainContext) CurrentHeader() *types.Header {
	return nil
}

func (m *mockChainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	return m.headers[number]
}

func (m *mockChainContext) GetHeaderByNumber(number uint64) *types.Header {
	return m.headers[number]
}

func (m *mockChainContext) GetHeaderByHash(hash common.Hash) *types.Header {
	for _, h := range m.headers {
		if h.Hash() == hash {
			return h
		}
	}
	return nil
}

func (m *mockChainContext) GetTd(hash common.Hash, number uint64) *big.Int {
	return big.NewInt(1)
}

func (m *mockChainContext) Engine() consensus.Engine {
	return nil
}

// buildChain builds a simple chain of headers from 0 to count-1.
func buildChain(count int) (*mockChainContext, []*types.Header) {
	headers := make([]*types.Header, count)
	chain := &mockChainContext{headers: make(map[uint64]*types.Header)}

	for i := 0; i < count; i++ {
		h := &types.Header{
			Number:     big.NewInt(int64(i)),
			ParentHash: common.Hash{},
			Extra:      []byte("test"),
		}
		if i > 0 {
			h.ParentHash = headers[i-1].Hash()
		}
		headers[i] = h
		chain.headers[uint64(i)] = h
	}

	return chain, headers
}

func TestSpeculativeGetHashFn_Tier1_LazyResolve(t *testing.T) {
	chain, headers := buildChain(10)

	// Block N=9 is pending (SRC running), block N-1=8 is committed.
	blockN1Header := headers[8] // block 8
	pendingBlockN := uint64(9)
	expectedBlockNHash := common.HexToHash("0xdeadbeef")

	var srcCalled bool
	srcDone := func() common.Hash {
		srcCalled = true
		return expectedBlockNHash
	}

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, srcDone, nil)

	// Tier 1: BLOCKHASH(9) should lazy-resolve
	result := fn(9)
	if result != expectedBlockNHash {
		t.Errorf("Tier 1: expected %x, got %x", expectedBlockNHash, result)
	}
	if !srcCalled {
		t.Error("Tier 1: srcDone was not called")
	}

	// Second call should return cached value without calling srcDone again
	srcCalled = false
	result = fn(9)
	if result != expectedBlockNHash {
		t.Errorf("Tier 1 (cached): expected %x, got %x", expectedBlockNHash, result)
	}
}

func TestSpeculativeGetHashFn_Tier1_SetsAbortFlag(t *testing.T) {
	chain, headers := buildChain(10)

	blockN1Header := headers[8]
	pendingBlockN := uint64(9)
	expectedBlockNHash := common.HexToHash("0xdeadbeef")
	var accessed atomic.Bool

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, func() common.Hash {
		return expectedBlockNHash
	}, &accessed)

	result := fn(9)
	if result != expectedBlockNHash {
		t.Errorf("Tier 1: expected %x, got %x", expectedBlockNHash, result)
	}
	if !accessed.Load() {
		t.Fatal("Tier 1: BLOCKHASH(N) access did not set abort flag")
	}
}

func TestSpeculativeGetHashFn_Tier2_ImmediateParent(t *testing.T) {
	chain, headers := buildChain(10)

	blockN1Header := headers[8] // block 8
	pendingBlockN := uint64(9)
	expectedN1Hash := blockN1Header.Hash()

	srcDone := func() common.Hash {
		t.Error("srcDone should not be called for Tier 2")
		return common.Hash{}
	}

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, srcDone, nil)

	// Tier 2: BLOCKHASH(8) should return block 8's hash immediately
	result := fn(8)
	if result != expectedN1Hash {
		t.Errorf("Tier 2: expected %x, got %x", expectedN1Hash, result)
	}
}

func TestSpeculativeGetHashFn_OlderTiersDoNotSetAbortFlag(t *testing.T) {
	chain, headers := buildChain(10)

	blockN1Header := headers[8]
	pendingBlockN := uint64(9)
	var accessed atomic.Bool

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, func() common.Hash {
		t.Fatal("srcDone should not be called for Tier 2/3")
		return common.Hash{}
	}, &accessed)

	_ = fn(8)
	if accessed.Load() {
		t.Fatal("Tier 2: BLOCKHASH(N-1) incorrectly set abort flag")
	}

	_ = fn(7)
	if accessed.Load() {
		t.Fatal("Tier 3: BLOCKHASH(N-2) incorrectly set abort flag")
	}
}

func TestSpeculativeGetHashFn_Tier3_OlderBlocks(t *testing.T) {
	chain, headers := buildChain(10)

	blockN1Header := headers[8] // block 8
	pendingBlockN := uint64(9)

	srcDone := func() common.Hash {
		t.Error("srcDone should not be called for Tier 3")
		return common.Hash{}
	}

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, srcDone, nil)

	// Tier 3: BLOCKHASH(7) should resolve via chain walk from block 8
	expectedHash7 := headers[7].Hash()
	result := fn(7)
	if result != expectedHash7 {
		t.Errorf("Tier 3 (block 7): expected %x, got %x", expectedHash7, result)
	}

	// BLOCKHASH(5) — deeper walk
	expectedHash5 := headers[5].Hash()
	result = fn(5)
	if result != expectedHash5 {
		t.Errorf("Tier 3 (block 5): expected %x, got %x", expectedHash5, result)
	}

	// BLOCKHASH(0) — genesis
	expectedHash0 := headers[0].Hash()
	result = fn(0)
	if result != expectedHash0 {
		t.Errorf("Tier 3 (block 0): expected %x, got %x", expectedHash0, result)
	}
}

func TestSpeculativeGetHashFn_FutureBlock(t *testing.T) {
	chain, headers := buildChain(10)

	blockN1Header := headers[8]
	pendingBlockN := uint64(9)

	srcDone := func() common.Hash {
		t.Error("srcDone should not be called for future blocks")
		return common.Hash{}
	}

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, srcDone, nil)

	// BLOCKHASH(10) — future block, should return zero
	result := fn(10)
	if result != (common.Hash{}) {
		t.Errorf("Future block: expected zero hash, got %x", result)
	}

	// BLOCKHASH(11) — also future
	result = fn(11)
	if result != (common.Hash{}) {
		t.Errorf("Future block 11: expected zero hash, got %x", result)
	}
}

func TestSpeculativeGetHashFn_Tier1_Blocking(t *testing.T) {
	chain, headers := buildChain(10)

	blockN1Header := headers[8]
	pendingBlockN := uint64(9)
	expectedHash := common.HexToHash("0xabcdef")

	var wg sync.WaitGroup
	wg.Add(1)

	srcDone := func() common.Hash {
		wg.Wait() // block until released
		return expectedHash
	}

	fn := SpeculativeGetHashFn(blockN1Header, chain, pendingBlockN, srcDone, nil)

	// Start BLOCKHASH(9) in a goroutine — it should block
	resultCh := make(chan common.Hash, 1)
	go func() {
		resultCh <- fn(9)
	}()

	// Verify it hasn't resolved yet
	select {
	case <-resultCh:
		t.Error("BLOCKHASH(9) resolved before srcDone was released")
	case <-time.After(100 * time.Millisecond):
		// expected — still blocking
	}

	// Release srcDone
	wg.Done()

	// Now it should resolve
	select {
	case result := <-resultCh:
		if result != expectedHash {
			t.Errorf("Tier 1 blocking: expected %x, got %x", expectedHash, result)
		}
	case <-time.After(2 * time.Second):
		t.Error("BLOCKHASH(9) did not resolve after srcDone was released")
	}
}
