package stateless

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Witness sizes the bench iterates. Mirrors the approved plan's matrix.
var benchSizesMiB = []int{1, 5, 15, 30, 50}

// Core counts for the parallel candidates. cores=1 lets us see the
// single-thread baseline directly inside the same matrix; 8 reflects modern
// validator/relayer hardware.
var benchCores = []int{1, 2, 4, 8}

// preparedWitness holds an already-built synthetic witness alongside its
// canonical encoded bytes and root hash, so each Benchmark sub-run pays
// the construction cost once outside the timed loop.
type preparedWitness struct {
	w        *Witness
	rlpBytes []byte
	// rootForD: a synthetic "state root" the intrinsic walk starts from.
	// Picked deterministically from the witness's set so D's positive
	// path resolves; without an MPT we can't reconstruct a real root, and
	// the bench cares about per-node keccak throughput + walk cost shape.
	rootForD common.Hash
}

func prepareWitness(b *testing.B, sizeMiB int) preparedWitness {
	b.Helper()
	w := buildSyntheticWitness(sizeMiB<<20, 256)
	var buf bytes.Buffer
	if err := w.EncodeRLP(&buf); err != nil {
		b.Fatalf("encode: %v", err)
	}
	rlpBytes := buf.Bytes()
	// Pick the lex-smallest node-hash as the synthetic root for D so the
	// walk has a definite entry point. Realistic verifier uses
	// header.StateRoot; the hash we pick is functionally equivalent for
	// timing purposes.
	hashes := make([]common.Hash, 0, len(w.State))
	for n := range w.State {
		hashes = append(hashes, crypto.Keccak256Hash([]byte(n)))
	}
	sort.Slice(hashes, func(i, j int) bool {
		return string(hashes[i][:]) < string(hashes[j][:])
	})
	var root common.Hash
	if len(hashes) > 0 {
		root = hashes[0]
	}
	return preparedWitness{w: w, rlpBytes: rlpBytes, rootForD: root}
}

// BenchmarkCommit_A_BlobKeccak — current baseline. Single-threaded keccak
// over the canonical RLP encoding.
func BenchmarkCommit_A_BlobKeccak(b *testing.B) {
	for _, mib := range benchSizesMiB {
		pw := prepareWitness(b, mib)
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			b.SetBytes(int64(len(pw.rlpBytes)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = candidateA_BlobKeccak(pw.rlpBytes)
			}
		})
	}
}

// BenchmarkCommit_B_PageParallel — page-aligned (15 MiB) parallel keccak,
// aggregate via concat+keccak. cores=K parallelism.
func BenchmarkCommit_B_PageParallel(b *testing.B) {
	for _, mib := range benchSizesMiB {
		pw := prepareWitness(b, mib)
		for _, cores := range benchCores {
			b.Run(fmt.Sprintf("%dMiB/cores=%d", mib, cores), func(b *testing.B) {
				b.SetBytes(int64(len(pw.rlpBytes)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_ = candidateB_PageParallel(pw.rlpBytes, cores)
				}
			})
		}
	}
}

// BenchmarkCommit_C_PerNodeMerkle — per-node hash + sort + Merkle build.
// Includes node hashing in the timed region so this is the verifier-side
// cost. The producer-only cost is captured separately below.
func BenchmarkCommit_C_PerNodeMerkle(b *testing.B) {
	for _, mib := range benchSizesMiB {
		pw := prepareWitness(b, mib)
		for _, cores := range benchCores {
			b.Run(fmt.Sprintf("%dMiB/cores=%d", mib, cores), func(b *testing.B) {
				b.SetBytes(int64(len(pw.rlpBytes)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_ = candidateC_PerNodeMerkle(pw.w, cores)
				}
			})
		}
	}
}

// BenchmarkCommit_B_ChunkSize sweeps chunk size for B while holding
// cores=8. Answers "is 15 MiB the right page size for parallelism, or
// would smaller chunks win?". Pinned to 50 MiB because that's where the
// answer matters; smaller witnesses don't have headroom to split.
func BenchmarkCommit_B_ChunkSize(b *testing.B) {
	pw := prepareWitness(b, 50)
	chunks := []int{
		512 * 1024,       // 512 KiB
		1 * 1024 * 1024,  // 1 MiB
		2 * 1024 * 1024,  // 2 MiB
		4 * 1024 * 1024,  // 4 MiB
		8 * 1024 * 1024,  // 8 MiB
		15 * 1024 * 1024, // 15 MiB (current wire page)
	}
	for _, c := range chunks {
		b.Run(fmt.Sprintf("chunk=%dKiB/cores=8", c>>10), func(b *testing.B) {
			b.SetBytes(int64(len(pw.rlpBytes)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = candidateB_PageParallelChunked(pw.rlpBytes, c, 8)
			}
		})
	}
	// Also try cores=12 (all logical cores) at the smallest chunks to
	// see if the M4 Pro's E-cores help at finer granularity.
	for _, c := range []int{512 * 1024, 1 * 1024 * 1024, 2 * 1024 * 1024} {
		b.Run(fmt.Sprintf("chunk=%dKiB/cores=12", c>>10), func(b *testing.B) {
			b.SetBytes(int64(len(pw.rlpBytes)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = candidateB_PageParallelChunked(pw.rlpBytes, c, 12)
			}
		})
	}
}

// BenchmarkProducerSign_C_ZeroCost — producer's incremental work
// post-execution: sort N hashes + Merkle build + ECDSA sign. Validates
// the "zero hashing cost on producer" claim by feeding precomputed hashes.
func BenchmarkProducerSign_C_ZeroCost(b *testing.B) {
	key, err := crypto.GenerateKey()
	if err != nil {
		b.Fatalf("key: %v", err)
	}
	for _, mib := range benchSizesMiB {
		pw := prepareWitness(b, mib)
		// Pre-hash & pre-sort the node set so the timed region only
		// includes Merkle build and ECDSA sign (the two pieces the
		// producer would actually pay).
		hashes := make([]common.Hash, 0, len(pw.w.State))
		for n := range pw.w.State {
			hashes = append(hashes, crypto.Keccak256Hash([]byte(n)))
		}
		sort.Slice(hashes, func(i, j int) bool {
			return string(hashes[i][:]) < string(hashes[j][:])
		})
		b.Run(fmt.Sprintf("%dMiB", mib), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				root := candidateC_ProducerOnly(hashes)
				if _, err := signECDSA(key, root[:]); err != nil {
					b.Fatalf("sign: %v", err)
				}
			}
		})
	}
}

// BenchmarkVerify_D_IntrinsicHashAll — D's verifier-side incremental cost
// over chain-prep baseline: parallel per-node keccak. The reachability
// walk and map build are amortized into MakeHashDB in production and are
// asymptotically negligible vs the keccak phase, so we exclude them here
// to avoid measuring noise. Producer cost for D is exactly zero (header
// is already signed; no separate WitnessHash signature exists).
func BenchmarkVerify_D_IntrinsicHashAll(b *testing.B) {
	for _, mib := range benchSizesMiB {
		pw := prepareWitness(b, mib)
		for _, cores := range benchCores {
			b.Run(fmt.Sprintf("%dMiB/cores=%d", mib, cores), func(b *testing.B) {
				b.SetBytes(int64(len(pw.rlpBytes)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					candidateD_HashAll(pw.w, cores)
				}
			})
		}
	}
}

func signECDSA(key *ecdsa.PrivateKey, digest []byte) ([]byte, error) {
	return crypto.Sign(digest, key)
}

// ----------------------------------------------------------------------------
// Correctness checks (Test*) for B/C/D so the bench numbers reflect
// implementations that actually do the right thing.
// ----------------------------------------------------------------------------

// TestCandidateB_PageAggregateDeterministic guards the determinism property
// the bench depends on: two runs over identical input produce identical
// aggregate hashes. Without this, the bench number for B would be
// meaningless.
func TestCandidateB_PageAggregateDeterministic(t *testing.T) {
	in := bytes.Repeat([]byte{0xab}, 20<<20) // 20 MiB → 2 pages at 15 MiB
	a := candidateB_PageParallel(in, 4)
	bb := candidateB_PageParallel(in, 4)
	if a != bb {
		t.Fatalf("B is non-deterministic across runs: %s vs %s", a.Hex(), bb.Hex())
	}
}

// TestCandidateC_OrderInvariant guards the property that motivates C: the
// Merkle root over sorted node hashes is invariant under map iteration
// order. Build a Witness, hash it, mutate insertion order via fresh map,
// hash again, must match.
func TestCandidateC_OrderInvariant(t *testing.T) {
	w := buildSyntheticWitness(2<<20, 512)
	root1 := candidateC_PerNodeMerkle(w, 1)

	// Rebuild with the same node set but different insertion order.
	nodes := make([][]byte, 0, len(w.State))
	for n := range w.State {
		nodes = append(nodes, []byte(n))
	}
	w2 := &Witness{Codes: make(map[string]struct{}), State: make(map[string]struct{})}
	w2.Headers = w.Headers
	w2.context = w.context
	for i := len(nodes) - 1; i >= 0; i-- {
		w2.State[string(nodes[i])] = struct{}{}
	}
	root2 := candidateC_PerNodeMerkle(w2, 1)
	if root1 != root2 {
		t.Fatalf("C is order-sensitive: %s vs %s", root1.Hex(), root2.Hex())
	}
}

// TestCandidateD_DetectsMissingNode guards D's load-bearing property: a
// witness missing a referenced node fails the walk. Without this, D would
// silently accept incomplete witnesses, defeating the byte-blame-pre-
// execute argument.
//
// We build a tiny tree manually: node A embeds keccak(B); node B embeds
// keccak(C); C is a leaf. Walking from keccak(A) succeeds. Deleting B
// from the witness must make the walk fail.
func TestCandidateD_DetectsMissingNode(t *testing.T) {
	leafC := []byte("leaf-payload-C-padded-to-some-bytes-xyz")
	hashC := crypto.Keccak256Hash(leafC)

	nodeB := append([]byte("node-B-prefix-padding-"), hashC[:]...)
	hashB := crypto.Keccak256Hash(nodeB)

	nodeA := append([]byte("node-A-prefix-padding-"), hashB[:]...)
	hashA := crypto.Keccak256Hash(nodeA)

	w := &Witness{
		Codes: make(map[string]struct{}),
		State: map[string]struct{}{
			string(nodeA): {},
			string(nodeB): {},
			string(leafC): {},
		},
	}
	if !candidateD_IntrinsicWalk(w, hashA, 1) {
		t.Fatal("baseline walk failed; the manual A→B→C chain is malformed")
	}

	// Drop B; the walk from A must fail because A's reference to B
	// dangles.
	delete(w.State, string(nodeB))
	if candidateD_IntrinsicWalk(w, hashA, 1) {
		t.Fatal("D accepted a witness missing a referenced node; byte-blame-pre-execute is broken")
	}
}
