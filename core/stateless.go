// Copyright 2024 The go-ethereum Authors
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
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// statelessIncompleteStateMeter counts blocks rejected because stateless
// execution could not load required state or contract code (see
// ErrStatelessIncompleteState). A non-zero rate on a stateless node signals a
// witness-completeness or bytecode-heal gap rather than genuine consensus
// divergence.
var statelessIncompleteStateMeter = metrics.NewRegisteredMeter("chain/stateless/incomplete_state", nil)

// ExecuteStateless runs a stateless execution based on a witness, verifies
// everything it can locally and returns the state root and receipt root, that
// need the other side to explicitly check.
//
// This method is a bit of a sore thumb here, but:
//   - It cannot be placed in core/stateless, because state.New prodces a circular dep
//   - It cannot be placed outside of core, because it needs to construct a dud headerchain
//
// TODO(karalabe): Would be nice to resolve both issues above somehow and move it.
func ExecuteStateless(config *params.ChainConfig, vmconfig vm.Config, block *types.Block, witness *stateless.Witness, author *common.Address, consensus consensus.Engine, diskdb ethdb.Database) (common.Hash, common.Hash, *state.StateDB, *ProcessResult, error) {
	// Sanity check if the supplied block accidentally contains a set root or
	// receipt hash. If so, be very loud, but still continue.
	if block.Root() != (common.Hash{}) {
		log.Error("stateless runner received state root it's expected to calculate (faulty consensus client)", "block", block.Number())
	}
	if block.ReceiptHash() != (common.Hash{}) {
		log.Error("stateless runner received receipt root it's expected to calculate (faulty consensus client)", "block", block.Number())
	}
	// Create and populate the state database to serve as the stateless backend
	memdb := witness.MakeHashDB(diskdb)
	db, err := state.New(witness.Root(), state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
	if err != nil {
		return common.Hash{}, common.Hash{}, nil, nil, err
	}
	// Create a blockchain that is idle, but can be used to access headers through
	headerChain := &HeaderChain{
		config:      config,
		chainDb:     memdb,
		headerCache: lru.NewCache[common.Hash, *types.Header](256),
		engine:      consensus,
	}
	processor := NewStateProcessor(headerChain)
	validator := NewBlockValidator(config, nil) // No chain, we only validate the state, not the block

	res, err := processor.Process(block, db, vmconfig, author, context.Background())

	// A missing witness trie node, or a called contract's bytecode absent from
	// local disk (WIT2 witnesses do not carry code), is caught by StateDB
	// (state/statedb.go's setError/dbErr) but only recorded as a sticky flag —
	// it never halts execution, so the read silently nil-serves and execution
	// continues against phantom-zero state. That can either flow into a wrong
	// gas result (surfacing only as a misleading ErrGasUsedMismatch, or a state
	// root mismatch on the full path) or push the divergent execution into a
	// secondary failure — e.g. "gas limit reached" — that Process returns as its
	// own error, masking the true cause. In both cases db.Error() names the real
	// problem, so check it FIRST and prefer it over any Process error: the
	// computed result is untrustworthy the moment a required read could not be
	// served. The test-only serial replay (executeStatelessSerial) already gates
	// on db.Error() this way; this makes the production path do the same. db/res
	// are preserved so callers doing forensic capture still see the computed
	// (wrong) result, not just the error string.
	if dbErr := db.Error(); dbErr != nil {
		statelessIncompleteStateMeter.Mark(1)
		log.Error("stateless execution hit incomplete state or code; rejecting block",
			"block", block.Number(), "hash", block.Hash(), "err", dbErr, "procErr", err)
		return common.Hash{}, common.Hash{}, db, res, fmt.Errorf("%w: %v", ErrStatelessIncompleteState, dbErr)
	}
	if err != nil {
		return common.Hash{}, common.Hash{}, nil, nil, err
	}

	if err = validator.ValidateState(block, db, res, true); err != nil {
		return common.Hash{}, common.Hash{}, nil, nil, err
	}
	// Almost everything validated, but receipt and state root needs to be returned
	receiptRoot := types.DeriveSha(res.Receipts, trie.NewStackTrie(nil))
	stateRoot := db.IntermediateRoot(config.IsEIP158(block.Number()))
	return stateRoot, receiptRoot, db, res, nil
}
