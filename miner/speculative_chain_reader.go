package miner

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// speculativeChainReader wraps a real ChainHeaderReader and intercepts
// hash-based lookups for a pending block whose hash is not yet known
// (because its state root is still being computed by the SRC goroutine).
//
// During pipelined SRC, block N+1's Prepare() needs to look up block N's
// header — but block N hasn't been written to the chain DB yet. The wrapper
// maps a deterministic placeholder hash to block N's provisional header
// (complete except for Root), allowing Prepare() and snapshot walks to proceed.
//
// The snapshot walk (bor.go:686) starts from header.ParentHash. For the
// speculative header, that's the placeholder hash. The wrapper returns
// pendingParentHeader for that lookup. Subsequent walk steps use
// pendingParentHeader.ParentHash (= hash(block_{N-1})), which is in the
// real chain DB, so the walk continues normally.
type speculativeChainReader struct {
	inner               consensus.ChainHeaderReader
	pendingParentHeader *types.Header // block N's header (complete except Root)
	placeholderHash     common.Hash   // the placeholder used as block N+1's ParentHash
}

// newSpeculativeChainReader creates a wrapper that intercepts lookups for
// the pending parent block.
//
// pendingParentHeader must have all fields set except Root. The caller must
// ensure that pendingParentHeader.ParentHash points to a block that IS in
// the chain DB (block N-1).
//
// placeholderHash is a deterministic sentinel used as ParentHash in the
// speculative block N+1 header. It must NOT collide with any real block hash.
func newSpeculativeChainReader(
	inner consensus.ChainHeaderReader,
	pendingParentHeader *types.Header,
	placeholderHash common.Hash,
) *speculativeChainReader {
	return &speculativeChainReader{
		inner:               inner,
		pendingParentHeader: pendingParentHeader,
		placeholderHash:     placeholderHash,
	}
}

func (s *speculativeChainReader) Config() *params.ChainConfig {
	return s.inner.Config()
}

func (s *speculativeChainReader) CurrentHeader() *types.Header {
	return s.inner.CurrentHeader()
}

func (s *speculativeChainReader) GetHeader(hash common.Hash, number uint64) *types.Header {
	if hash == s.placeholderHash && number == s.pendingParentHeader.Number.Uint64() {
		return s.pendingParentHeader
	}
	return s.inner.GetHeader(hash, number)
}

func (s *speculativeChainReader) GetHeaderByNumber(number uint64) *types.Header {
	if number == s.pendingParentHeader.Number.Uint64() {
		return s.pendingParentHeader
	}
	return s.inner.GetHeaderByNumber(number)
}

func (s *speculativeChainReader) GetHeaderByHash(hash common.Hash) *types.Header {
	if hash == s.placeholderHash {
		return s.pendingParentHeader
	}
	return s.inner.GetHeaderByHash(hash)
}

func (s *speculativeChainReader) GetTd(hash common.Hash, number uint64) *big.Int {
	if hash == s.placeholderHash && number == s.pendingParentHeader.Number.Uint64() {
		// The pending parent being genesis can't happen in practice (the
		// speculative path only builds on a produced block), but guard the
		// subtraction against underflow regardless.
		parentNumber := s.pendingParentHeader.Number.Uint64()
		if parentNumber == 0 {
			return s.inner.GetTd(s.pendingParentHeader.ParentHash, 0)
		}
		// Return the parent's TD. This is an approximation — the real TD
		// would include block N's difficulty, but Bor's Prepare() does not
		// use TD from GetTd. Seal() uses it for broadcast, but that happens
		// after the real header is assembled.
		return s.inner.GetTd(s.pendingParentHeader.ParentHash, parentNumber-1)
	}
	return s.inner.GetTd(hash, number)
}

// speculativeChainContext wraps speculativeChainReader and adds the Engine()
// method, satisfying core.ChainContext. This is needed because
// NewEVMBlockContext takes a ChainContext.
type speculativeChainContext struct {
	*speculativeChainReader
	engine consensus.Engine
}

// newSpeculativeChainContext creates a ChainContext backed by the speculative
// reader and the given consensus engine.
func newSpeculativeChainContext(
	reader *speculativeChainReader,
	engine consensus.Engine,
) *speculativeChainContext {
	return &speculativeChainContext{
		speculativeChainReader: reader,
		engine:                 engine,
	}
}

func (s *speculativeChainContext) Engine() consensus.Engine {
	return s.engine
}
