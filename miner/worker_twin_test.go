package miner

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// The whole convergence argument rests on this: two producers holding one key
// and sealing the same header must emit the same signature, or "they derive
// the same block" is false and contention always produces two blocks.
// secp256k1 signing uses RFC 6979 deterministic nonces, on both the cgo and
// nocgo paths — asserted here rather than assumed.
func TestSigningIsDeterministic(t *testing.T) {
	t.Parallel()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	digest := crypto.Keccak256([]byte("the same header, signed twice"))

	first, err := crypto.Sign(digest, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	second, err := crypto.Sign(digest, key)
	if err != nil {
		t.Fatalf("sign again: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("the same key signing the same digest produced two different " +
			"signatures: two producers can never derive one block, so " +
			"contention must be resolved by refusal rather than agreement")
	}
}

// Header identity: two independent workers on the same config, given the same
// adopted window, must prepare byte-identical headers. This is the other half
// of the convergence claim — agreeing on transactions is worthless if the
// headers differ, because the block hashes then differ anyway.
//
// The seal hash is the exact thing consensus signs, so comparing it is
// stricter than comparing fields one by one.
func TestTwinWorkersPrepareIdenticalHeaders(t *testing.T) {
	t.Parallel()

	w1, _, _ := newSequencerTestWorker(t)
	w2, _, _ := newSequencerTestWorker(t)

	parent := w1.chain.CurrentBlock()
	if got := w2.chain.CurrentBlock().Hash(); got != parent.Hash() {
		t.Fatalf("the two workers start from different genesis blocks (%s vs %s)",
			parent.Hash(), got)
	}

	// The same inherited open context on both sides, as adoption supplies it.
	// The base fee is derived from the parent by rules both producers share.
	window := func() *AdoptedWindow {
		return &AdoptedWindow{
			Number:     parent.Number.Uint64() + 1,
			Timestamp:  parent.Time + 2,
			ParentHash: parent.Hash(),
			GasLimit:   parent.GasLimit,
			BaseFee:    eip1559.CalcBaseFee(w1.chain.Config(), parent),
		}
	}

	h1 := prepareAdoptedHeader(t, w1, window())
	h2 := prepareAdoptedHeader(t, w2, window())

	if clique.SealHash(h1) != clique.SealHash(h2) {
		t.Fatalf("two producers derived different headers from one open "+
			"context: seal hashes %s vs %s. Agreeing on content cannot make "+
			"them one block if the headers differ.",
			clique.SealHash(h1), clique.SealHash(h2))
	}
}

// prepareAdoptedHeader drives the real adoption path — makeHeader, then
// applyAdoption — and fails if the window is rejected, so this cannot quietly
// degrade into comparing two ordinary headers.
func prepareAdoptedHeader(t *testing.T, w *worker, window *AdoptedWindow) *types.Header {
	t.Helper()

	genParams := &generateParams{coinbase: testBankAddress, adoption: window}

	header, _, err := w.makeHeader(genParams, false)
	if err != nil {
		t.Fatalf("makeHeader: %v", err)
	}

	w.applyAdoption(genParams, header)

	if genParams.adoption == nil {
		t.Fatal("the window was rejected, so this header never inherited the " +
			"open context and proves nothing about two producers agreeing")
	}

	if header.Time != window.Timestamp {
		t.Fatalf("header did not inherit the adopted timestamp: %d vs %d",
			header.Time, window.Timestamp)
	}

	return header
}
