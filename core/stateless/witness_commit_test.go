// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package stateless

import (
	"bytes"
	"math/big"
	"runtime"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestWitnessCommitHashDeterministic(t *testing.T) {
	in := bytes.Repeat([]byte{0xab}, 5*WitnessCommitChunkBytes+1234)
	a := WitnessCommitHash(in)
	b := WitnessCommitHash(in)
	if a != b {
		t.Fatalf("non-deterministic: %s vs %s", a.Hex(), b.Hex())
	}
}

// TestWitnessCommitHashWorkerInvariant pins the load-bearing property: the
// committed hash MUST NOT depend on GOMAXPROCS. If it does, two honest peers
// running with different parallelism would diverge on the same witness.
func TestWitnessCommitHashWorkerInvariant(t *testing.T) {
	in := bytes.Repeat([]byte{0xcd}, 6*WitnessCommitChunkBytes+777)
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	one := WitnessCommitHash(in)

	runtime.GOMAXPROCS(8)
	eight := WitnessCommitHash(in)

	if one != eight {
		t.Fatalf("hash depends on GOMAXPROCS: 1=%s 8=%s", one.Hex(), eight.Hex())
	}
}

// TestWitnessCommitHashEmptyInput pins the empty-witness behavior so producer
// and verifier agree on the degenerate case.
func TestWitnessCommitHashEmptyInput(t *testing.T) {
	if got := WitnessCommitHash(nil); got != (common.Hash{}) {
		t.Fatalf("expected zero hash for nil, got %s", got.Hex())
	}
	if got := WitnessCommitHash([]byte{}); got != (common.Hash{}) {
		t.Fatalf("expected zero hash for empty slice, got %s", got.Hex())
	}
}

// TestWitnessCommitHashSingleSubChunk pins the small-input shape: an input
// shorter than one chunk hashes to keccak256(keccak256(input)), since the
// scheme always wraps a final aggregate-keccak around the chunk-hash list.
func TestWitnessCommitHashSingleSubChunk(t *testing.T) {
	in := bytes.Repeat([]byte{0x42}, 4096)
	got := WitnessCommitHash(in)

	inner := crypto.Keccak256Hash(in)
	want := crypto.Keccak256Hash(inner[:])
	if got != want {
		t.Fatalf("single-subchunk shape mismatch: got %s want %s", got.Hex(), want.Hex())
	}
}

// TestWitnessCommitHashFromWitness pins the convenience wrapper to the
// primitive: encoding a witness with the canonical EncodeRLP and hashing those
// bytes must equal WitnessCommitHashFromWitness on the same witness, so the
// producer (wrapper) and verifier (raw-bytes) paths can never diverge.
func TestWitnessCommitHashFromWitness(t *testing.T) {
	w := &Witness{
		context: &types.Header{Number: big.NewInt(100)},
		Headers: []*types.Header{{Number: big.NewInt(99)}},
		State:   map[string]struct{}{"statenode": {}},
	}

	got, err := WitnessCommitHashFromWitness(w)
	if err != nil {
		t.Fatalf("WitnessCommitHashFromWitness: %v", err)
	}

	var buf bytes.Buffer
	if err := w.EncodeRLP(&buf); err != nil {
		t.Fatalf("EncodeRLP: %v", err)
	}
	if want := WitnessCommitHash(buf.Bytes()); got != want {
		t.Fatalf("wrapper mismatch: got %s want %s", got.Hex(), want.Hex())
	}
}

// TestWitnessCommitHashMultiChunkShape spot-checks the multi-chunk recipe so a
// silent change in concat order or chunking would be caught immediately.
func TestWitnessCommitHashMultiChunkShape(t *testing.T) {
	a := bytes.Repeat([]byte{0x01}, WitnessCommitChunkBytes)
	b := bytes.Repeat([]byte{0x02}, WitnessCommitChunkBytes)
	c := bytes.Repeat([]byte{0x03}, 1234)
	in := append(append(append([]byte{}, a...), b...), c...)

	ha := crypto.Keccak256Hash(a)
	hb := crypto.Keccak256Hash(b)
	hc := crypto.Keccak256Hash(c)
	concat := append(append(append([]byte{}, ha[:]...), hb[:]...), hc[:]...)
	want := crypto.Keccak256Hash(concat)

	if got := WitnessCommitHash(in); got != want {
		t.Fatalf("multi-chunk shape mismatch: got %s want %s", got.Hex(), want.Hex())
	}
}

// TestSplitWitnessChunks covers the chunk boundary math: exact multiples, a
// remainder tail (the off-by-one-prone case), an input smaller than one chunk,
// and the empty input. Chunk bytes must always sum back to the input length.
func TestSplitWitnessChunks(t *testing.T) {
	cases := []struct {
		n, size, wantChunks, wantLast int
	}{
		{0, 4, 0, 0}, // empty input -> no chunks
		{3, 4, 1, 3}, // smaller than one chunk -> single short chunk
		{4, 4, 1, 4}, // exact single chunk
		{5, 4, 2, 1}, // remainder tail: full chunk + 1-byte chunk
		{8, 4, 2, 4}, // exact multiple
	}
	for _, c := range cases {
		out := splitWitnessChunks(make([]byte, c.n), c.size)
		if len(out) != c.wantChunks {
			t.Fatalf("n=%d size=%d: want %d chunks, got %d", c.n, c.size, c.wantChunks, len(out))
		}
		total := 0
		for _, ch := range out {
			total += len(ch)
		}
		if total != c.n {
			t.Fatalf("n=%d size=%d: chunk bytes must sum to input %d, got %d", c.n, c.size, c.n, total)
		}
		if c.wantChunks > 0 && len(out[len(out)-1]) != c.wantLast {
			t.Fatalf("n=%d size=%d: last chunk want %d bytes, got %d", c.n, c.size, c.wantLast, len(out[len(out)-1]))
		}
	}
}

// TestWitnessCommitWorkerCount covers the fan-out clamps: at least one worker even
// with no work, never more workers than chunks, and never above the cap.
func TestWitnessCommitWorkerCount(t *testing.T) {
	if got := witnessCommitWorkerCount(0); got != 1 {
		t.Fatalf("zero chunks must clamp to 1 worker, got %d", got)
	}
	if got := witnessCommitWorkerCount(1); got != 1 {
		t.Fatalf("one chunk must use exactly 1 worker, got %d", got)
	}
	if got := witnessCommitWorkerCount(1 << 20); got > witnessCommitMaxWorkers || got < 1 {
		t.Fatalf("large fan-out must clamp to [1,%d], got %d", witnessCommitMaxWorkers, got)
	}
}
