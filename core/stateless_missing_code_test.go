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
	"errors"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
)

// codeReadRecorder wraps the code-backing disk database and records which
// contract-code blobs stateless execution actually reads. A code read is a Get
// whose value hashes to the 32-byte tail of its key (rawdb stores code under
// codePrefix||codeHash), so this identifies code reads without depending on the
// unexported prefix. It lets the test target only the handful of contracts this
// block depends on rather than sweeping every blob in the shared fixture.
type codeReadRecorder struct {
	ethdb.Database
	reads map[common.Hash]struct{}
}

func (r *codeReadRecorder) Get(key []byte) ([]byte, error) {
	v, err := r.Database.Get(key)
	if err == nil && len(key) >= common.HashLength && len(v) > 0 {
		if tail := common.BytesToHash(key[len(key)-common.HashLength:]); crypto.Keccak256Hash(v) == tail {
			r.reads[tail] = struct{}{}
		}
	}
	return v, err
}

// TestExecuteStatelessRejectsMissingCode proves that when a called contract's
// bytecode is absent from local disk, the production stateless path rejects the
// block with ErrStatelessIncompleteState naming the missing hash — instead of
// silently running the contract as code-less and surfacing the divergence only
// as a misleading ErrGasUsedMismatch.
//
// This is the exact condition behind the bor mainnet-soak gas-mismatch
// incident: WIT2 witnesses do not carry code, so a stateless node reads code
// from its own disk; when the bytecode-heal/fast-forward accounting left a
// called contract's code absent, StateDB recorded a sticky error and nil-served
// the read, and nothing on the stateless path consulted db.Error(). The
// test-only serial replay (executeStatelessSerial) already gates on db.Error();
// this asserts the production ExecuteStateless now does the same.
func TestExecuteStatelessRejectsMissingCode(t *testing.T) {
	bd, diskdb := loadSingleWitnessRegenBlock(t, singleRegenBlockHex)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	pb := prepareBlocks([]testBlockData{bd}, diskdb, config)[0]

	// Reproduce ProcessBlockWithWitnesses' prelude: hand ExecuteStateless a
	// block with the roots it is expected to recompute zeroed out.
	newTask := func() *types.Block {
		ctx := pb.block.Header()
		ctx.Root = common.Hash{}
		ctx.ReceiptHash = common.Hash{}
		return types.NewBlockWithHeader(ctx).WithBody(*pb.block.Body())
	}

	// Baseline through the recorder: with all code present the block verifies
	// cleanly, and we capture exactly which code blobs it reads.
	rec := &codeReadRecorder{Database: diskdb, reads: make(map[common.Hash]struct{})}
	if _, _, _, _, err := ExecuteStateless(config, vm.Config{}, newTask(), pb.witness, &pb.author, engine, rec); err != nil {
		t.Fatalf("baseline ExecuteStateless with complete code failed: %v", err)
	}
	if len(rec.reads) == 0 {
		t.Fatal("fixture block reads no contract code; cannot exercise the guard")
	}

	// Removing any code the block reads must surface as
	// ErrStatelessIncompleteState naming the hash — never a bare gas/root
	// mismatch, a Process-level symptom like "gas limit reached", or a silent
	// pass. Each removal is a full serial replay, so cap the sweep in -short.
	tested := 0
	for h := range rec.reads {
		code := rawdb.ReadCode(diskdb, h)
		if len(code) == 0 {
			t.Fatalf("recorded code %x is not present to remove", h)
		}
		rawdb.DeleteCode(diskdb, h)

		_, _, _, _, err := ExecuteStateless(config, vm.Config{}, newTask(), pb.witness, &pb.author, engine, diskdb)

		rawdb.WriteCode(diskdb, h, code) // restore for the next iteration

		switch {
		case err == nil:
			t.Fatalf("removing read code %x was silently tolerated (no error)", h)
		case !errors.Is(err, ErrStatelessIncompleteState):
			t.Fatalf("removing read code %x produced %v, want ErrStatelessIncompleteState", h, err)
		case !strings.Contains(err.Error(), strings.TrimPrefix(h.Hex(), "0x")):
			t.Fatalf("error for missing code %x does not name the hash: %v", h, err)
		}

		if tested++; testing.Short() && tested >= 1 {
			break
		}
	}
}
