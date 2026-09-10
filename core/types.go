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
	"context"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// Validator is an interface which defines the standard for block validation. It
// is only responsible for validating block contents, as the header validation is
// done by the specific consensus engines.
type Validator interface {
	// ValidateBody validates the given block's content.
	ValidateBody(block *types.Block) error

	// ValidateState validates the given statedb and optionally the process result.
	ValidateState(block *types.Block, state *state.StateDB, res *ProcessResult, stateless bool) error

	// ValidateStateCheap validates cheap post-state checks (gas, bloom, receipt root,
	// requests) without computing the expensive IntermediateRoot. Used by the
	// pipelined import path where IntermediateRoot is deferred to an SRC goroutine.
	ValidateStateCheap(block *types.Block, state *state.StateDB, res *ProcessResult) error
}

// Prefetcher is an interface for pre-caching transaction signatures and state.
type Prefetcher interface {
	// Prefetch processes the state changes according to the Ethereum rules by running
	// the transaction messages using the statedb, but any changes are discarded. The
	// only goal is to pre-cache transaction signatures and state trie nodes.
	Prefetch(block *types.Block, statedb *state.StateDB, cfg vm.Config, intermediateRootPrefetch bool, interrupt *atomic.Bool) *PrefetchResult
}

// Processor is an interface for processing blocks using a given initial state.
type Processor interface {
	// Process processes the state changes according to the Ethereum rules by running
	// the transaction messages using the statedb and applying any rewards to both
	// the processor (coinbase) and any included uncles.
	Process(block *types.Block, statedb *state.StateDB, cfg vm.Config, author *common.Address, interruptCtx context.Context) (*ProcessResult, error)
}

// ProcessResult contains the values computed by Process.
type ProcessResult struct {
	Receipts types.Receipts
	Requests [][]byte
	Logs     []*types.Log
	GasUsed  uint64
	// ReservedGasUsed is the actual gas used by transactions classified reserved
	// (fee-free) during this block's execution. ValidateState checks it against
	// the header's ReservedGasUsed post-fork, so a producer cannot stamp a value
	// that disagrees with execution (which would skew the next block's base fee).
	ReservedGasUsed uint64
	// ReservedCapacity is the reserved-blockspace registry snapshot's effective
	// capacity (Σ quotas of the client set effective for this block) used to
	// classify this block's reserved region. ValidateState checks it against
	// the header's ReservedCapacity post-fork, mirroring ReservedGasUsed.
	ReservedCapacity uint64
	// ReservedTxIndexes lists the positions within the block's transactions
	// classified reserved (fee-free), strictly ascending. Persisted alongside
	// receipts so reads can report the correct effective gas price for
	// reserved transactions without re-deriving the classification.
	ReservedTxIndexes []uint64
	// ReservedClientUsage reports, per registry client id, the declared gas
	// consumed by this block's reserved transactions against that client's
	// quota. It is derived observability data assembled from the same
	// classification walk as ReservedTxIndexes, not a consensus-checked
	// value - ValidateState never compares it against the header, which
	// carries no per-client breakdown. The Used basis is declared gas
	// (tx.Gas()) - the same basis quota admission itself is charged against -
	// not the executed gas ReservedGasUsed reports. Nil pre-fork.
	ReservedClientUsage map[uint64]registryreader.ClientUsage
}
