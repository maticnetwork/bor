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

package stateless

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
)

// HeaderReader is an interface to pull in headers in place of block hashes for the witness.
type HeaderReader interface {
	// GetHeader retrieves a block header from the database by hash and number.
	GetHeader(hash common.Hash, number uint64) *types.Header
}

// ValidateWitnessPreState validates that the witness pre-state root matches
// the parent block's state root. The expectedBlock header is the block being
// imported — the witness context must match it (ParentHash and Number) to
// prevent a malicious peer from substituting a witness for a different block.
func ValidateWitnessPreState(witness *Witness, headerReader HeaderReader, expectedBlock *types.Header) error {
	if witness == nil {
		return fmt.Errorf("witness is nil")
	}

	// Check if witness has any headers.
	if len(witness.Headers) == 0 {
		return fmt.Errorf("witness has no headers")
	}

	// Get the witness context header (the block this witness is for).
	contextHeader := witness.Header()
	if contextHeader == nil {
		return fmt.Errorf("witness context header is nil")
	}
	// The witness header is peer-supplied: don't rely on the transport
	// decoder to have rejected a nil or genesis block number — a genesis
	// block has no parent to validate against, and Uint64()-1 on zero
	// would probe an unrelated height.
	if contextHeader.Number == nil || contextHeader.Number.Sign() <= 0 {
		return fmt.Errorf("witness context header has invalid block number: %v", contextHeader.Number)
	}

	// Verify the witness is for the expected block — a malicious peer could
	// craft a witness with a different ParentHash to bypass the pre-state check.
	if expectedBlock != nil {
		if expectedBlock.Number == nil {
			return fmt.Errorf("expected block header has nil number")
		}
		if contextHeader.ParentHash != expectedBlock.ParentHash {
			return fmt.Errorf("witness ParentHash mismatch: witness=%x, expected=%x, blockNumber=%d",
				contextHeader.ParentHash, expectedBlock.ParentHash, expectedBlock.Number.Uint64())
		}
		if contextHeader.Number.Uint64() != expectedBlock.Number.Uint64() {
			return fmt.Errorf("witness block number mismatch: witness=%d, expected=%d",
				contextHeader.Number.Uint64(), expectedBlock.Number.Uint64())
		}
	}

	// Get the parent block header from the chain.
	parentNumber := contextHeader.Number.Uint64() - 1
	parentHeader := headerReader.GetHeader(contextHeader.ParentHash, parentNumber)
	if parentHeader == nil {
		return fmt.Errorf("parent block header not found: parentHash=%x, parentNumber=%d",
			contextHeader.ParentHash, parentNumber)
	}

	// Get witness pre-state root (from first header which should be parent).
	witnessPreStateRoot := witness.Root()

	// Compare with actual parent block's state root.
	if witnessPreStateRoot != parentHeader.Root {
		return fmt.Errorf("witness pre-state root mismatch: witness=%x, parent=%x, blockNumber=%d",
			witnessPreStateRoot, parentHeader.Root, contextHeader.Number.Uint64())
	}

	return nil
}

// Witness encompasses the state required to apply a set of transactions and
// derive a post state/receipt root.
type Witness struct {
	context *types.Header // Header to which this witness belongs to, with rootHash and receiptHash zeroed out

	Headers []*types.Header     // Past headers in reverse order (0=parent, 1=parent's-parent, etc). First *must* be set.
	Codes   map[string]struct{} // Set of bytecodes ran or accessed
	State   map[string]struct{} // Set of MPT state trie nodes (account and storage together)

	chain HeaderReader // Chain reader to convert block hash ops to header proofs
	lock  sync.RWMutex // Lock to allow concurrent state insertions
}

// NewWitness creates an empty witness ready for population.
func NewWitness(context *types.Header, chain HeaderReader) (*Witness, error) {
	// When building witnesses, retrieve the parent header, which will *always*
	// be included to act as a trustless pre-root hash container
	var headers []*types.Header
	if chain != nil {
		parent := chain.GetHeader(context.ParentHash, context.Number.Uint64()-1)
		if parent == nil {
			return nil, errors.New("failed to retrieve parent header")
		}
		headers = append(headers, parent)
	}
	// Create the witness with a copy of the context header to prevent
	// callers from mutating the header after witness creation.
	// Note: Root and ReceiptHash are NOT zeroed here — they are zeroed at the
	// point of stateless execution (ProcessBlockWithWitnesses) where they are
	// recomputed. Zeroing here would break the witness manager's hash matching
	// (handleBroadcast uses witness.Header().Hash() to look up pending blocks).
	ctx := types.CopyHeader(context)

	return &Witness{
		context: ctx,
		Headers: headers,
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
		chain:   chain,
	}, nil
}

// AddBlockHash adds a "blockhash" to the witness with the designated offset from
// chain head. Under the hood, this method actually pulls in enough headers from
// the chain to cover the block being added.
//
// Safe for concurrent use — V2 BlockSTM workers call this from the EVM's
// BLOCKHASH opcode, which runs on multiple goroutines per block.
func (w *Witness) AddBlockHash(number uint64) {
	w.lock.Lock()
	defer w.lock.Unlock()
	// Keep pulling in headers until this hash is populated
	for int(w.context.Number.Uint64()-number) > len(w.Headers) {
		tail := w.Headers[len(w.Headers)-1]
		w.Headers = append(w.Headers, w.chain.GetHeader(tail.ParentHash, tail.Number.Uint64()-1))
	}
}

// AddCode adds a bytecode blob to the witness.
//
// Safe for concurrent use — V2 BlockSTM workers and the V2 settle path can
// both add code blobs simultaneously.
func (w *Witness) AddCode(code []byte) {
	if len(code) == 0 {
		return
	}
	w.lock.Lock()
	defer w.lock.Unlock()
	w.Codes[string(code)] = struct{}{}
}

// AddState inserts a batch of MPT trie nodes into the witness.
func (w *Witness) AddState(nodes map[string][]byte) {
	if len(nodes) == 0 {
		return
	}
	w.lock.Lock()
	defer w.lock.Unlock()

	for _, value := range nodes {
		w.State[string(value)] = struct{}{}
	}
}

func (w *Witness) AddKey() {
	panic("not yet implemented")
}

// Copy deep-copies the witness object.  Witness.Block isn't deep-copied as it
// is never mutated by Witness
func (w *Witness) Copy() *Witness {
	w.lock.RLock()
	defer w.lock.RUnlock()
	cpy := &Witness{
		Headers: slices.Clone(w.Headers),
		Codes:   maps.Clone(w.Codes),
		State:   maps.Clone(w.State),
		chain:   w.chain,
	}
	if w.context != nil {
		cpy.context = types.CopyHeader(w.context)
	}
	return cpy
}

// Root returns the pre-state root from the first header.
//
// Note, this method will panic in case of a bad witness (but RLP decoding will
// sanitize it and fail before that).
func (w *Witness) Root() common.Hash {
	return w.Headers[0].Root
}

func (w *Witness) Header() *types.Header {
	return w.context
}

func (w *Witness) SetHeader(header *types.Header) {
	if w != nil {
		w.context = header
	}
}

func (w *Witness) HeaderReader() HeaderReader {
	if w == nil {
		return nil
	}
	return w.chain
}

// GetWitnessFromRlp decodes a witness from its RLP encoded form.
func GetWitnessFromRlp(rlpEncodedWitness []byte) (*Witness, error) {
	var witness Witness
	stream := rlp.NewStream(bytes.NewReader(rlpEncodedWitness), 0)
	if err := witness.DecodeRLP(stream); err != nil {
		return nil, err
	}
	return &witness, nil
}
