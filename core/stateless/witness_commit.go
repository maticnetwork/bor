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
	"runtime"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// WitnessCommitChunkBytes is the protocol-fixed chunk size for the WIT2
// witness commitment. Producer and verifier MUST agree on this constant.
// Changing it changes the meaning of every WitnessHash on the wire.
const WitnessCommitChunkBytes = 1 << 20 // 1 MiB

// witnessCommitMaxWorkers caps the keccak fan-out. The chosen value reflects
// the bench finding on Apple M4 Pro that 8 P-cores saturate the keccak
// primitive; over-subscribing onto E-cores doesn't add throughput.
const witnessCommitMaxWorkers = 8

// WitnessCommitHash returns the WIT2 witness commitment over the canonical
// RLP encoding of a witness: keccak256 of the concatenation of chunk hashes,
// where each chunk is keccak256 over a WitnessCommitChunkBytes-sized window
// of rlpBytes. The output is invariant in worker count — only the input
// bytes and the chunk-size constant determine the result, so producer and
// verifier always agree byte-for-byte regardless of GOMAXPROCS.
//
// Empty input returns the zero hash, distinct from keccak256("") so empty
// witnesses are unambiguously identified across the protocol.
func WitnessCommitHash(rlpBytes []byte) common.Hash {
	if len(rlpBytes) == 0 {
		return common.Hash{}
	}
	chunkHashes := hashWitnessChunks(splitWitnessChunks(rlpBytes, WitnessCommitChunkBytes))

	concat := make([]byte, 0, len(chunkHashes)*common.HashLength)
	for _, h := range chunkHashes {
		concat = append(concat, h[:]...)
	}
	return crypto.Keccak256Hash(concat)
}

// hashWitnessChunks keccaks each chunk, fanning out across a bounded worker
// pool. Single-chunk inputs (≤1 MiB) skip the goroutine pool — the fan-out
// cost would dominate the keccak.
func hashWitnessChunks(chunks [][]byte) []common.Hash {
	chunkHashes := make([]common.Hash, len(chunks))
	if len(chunks) == 1 {
		chunkHashes[0] = crypto.Keccak256Hash(chunks[0])
		return chunkHashes
	}

	var wg sync.WaitGroup
	work := make(chan int, len(chunks))
	for w := 0; w < witnessCommitWorkerCount(len(chunks)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				chunkHashes[i] = crypto.Keccak256Hash(chunks[i])
			}
		}()
	}
	for i := range chunks {
		work <- i
	}
	close(work)
	wg.Wait()
	return chunkHashes
}

// witnessCommitWorkerCount clamps the keccak fan-out to the available
// parallelism, the configured cap, and the amount of work on hand.
func witnessCommitWorkerCount(chunks int) int {
	workers := runtime.GOMAXPROCS(0)
	if workers > witnessCommitMaxWorkers {
		workers = witnessCommitMaxWorkers
	}
	if workers > chunks {
		workers = chunks
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

// WitnessCommitHashFromWitness encodes a witness with the canonical sorted
// EncodeRLP and returns its WitnessCommitHash. Callers that already have
// canonical RLP bytes should use WitnessCommitHash directly to skip the
// re-encoding cost.
func WitnessCommitHashFromWitness(w *Witness) (common.Hash, error) {
	var buf bytes.Buffer
	if err := w.EncodeRLP(&buf); err != nil {
		return common.Hash{}, err
	}
	return WitnessCommitHash(buf.Bytes()), nil
}

func splitWitnessChunks(buf []byte, chunkSize int) [][]byte {
	out := make([][]byte, 0, (len(buf)+chunkSize-1)/chunkSize)
	for i := 0; i < len(buf); i += chunkSize {
		end := i + chunkSize
		if end > len(buf) {
			end = len(buf)
		}
		out = append(out, buf[i:end])
	}
	return out
}
