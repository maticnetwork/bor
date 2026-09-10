package stateless

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"sort"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// All identifiers in this file are _test.go-scoped and exist only to drive
// the witness-commit benchmarks. Nothing here is referenced from production
// code; the file is throwaway-friendly per the research-only plan.

// buildSyntheticWitness constructs a Witness whose canonical EncodeRLP
// output is approximately targetBytes. It populates State with random byte
// blobs of size avgNodeBytes, mimicking how MPT trie nodes accumulate during
// execution. Headers + context carry minimal valid data so EncodeRLP /
// DecodeRLP round-trip without errors; the bench cares about state-bytes
// throughput, not header layout.
func buildSyntheticWitness(targetBytes, avgNodeBytes int) *Witness {
	if avgNodeBytes <= 0 {
		avgNodeBytes = 256
	}
	w := &Witness{
		context: &types.Header{Number: big.NewInt(1)},
		Headers: []*types.Header{{Number: big.NewInt(0)}},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	nodeCount := targetBytes / avgNodeBytes
	if nodeCount <= 0 {
		nodeCount = 1
	}
	buf := make([]byte, avgNodeBytes)
	for i := 0; i < nodeCount; i++ {
		// Distinct content for each node so keccak hashes don't collide and
		// the encoded set has the expected size on the wire.
		binary.BigEndian.PutUint64(buf[:8], uint64(i))
		if _, err := rand.Read(buf[8:]); err != nil {
			panic(err)
		}
		w.State[string(buf)] = struct{}{}
	}
	return w
}

// candidateA_BlobKeccak — current scheme. Keccak over the canonical RLP
// encoding of the entire witness. Single-threaded by design.
func candidateA_BlobKeccak(rlpBytes []byte) common.Hash {
	return crypto.Keccak256Hash(rlpBytes)
}

// candidateB_PageParallel hashes the input in fixed-size pages (15 MiB to
// match the wire fragmentation), each page in its own goroutine, then
// keccaks the concatenation of page hashes. The result is the value the BP
// would sign and the verifier would compare against.
//
// pageSize: 15 MiB to mirror the wire frag. cores: number of goroutines to
// use; honest callers pass GOMAXPROCS or a small constant.
const witnessPageBytes = 15 * 1024 * 1024

func candidateB_PageParallel(rlpBytes []byte, cores int) common.Hash {
	return candidateB_PageParallelChunked(rlpBytes, witnessPageBytes, cores)
}

// candidateB_PageParallelChunked is B with an explicit chunk-size knob so
// we can sweep below the 15 MiB wire-page boundary. Chunks smaller than
// the wire page would mean BP signs over a finer-grained aggregate, but
// this is internal accounting — wire pages stay 15 MiB, the producer just
// further subdivides them for hashing.
func candidateB_PageParallelChunked(rlpBytes []byte, chunkBytes, cores int) common.Hash {
	pages := splitPages(rlpBytes, chunkBytes)
	pageHashes := make([]common.Hash, len(pages))

	if cores < 1 {
		cores = 1
	}
	if cores > len(pages) {
		cores = len(pages)
	}

	var wg sync.WaitGroup
	work := make(chan int, len(pages))
	for w := 0; w < cores; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				pageHashes[i] = crypto.Keccak256Hash(pages[i])
			}
		}()
	}
	for i := range pages {
		work <- i
	}
	close(work)
	wg.Wait()

	// Aggregate is keccak over concat of page hashes. Order is wire-page
	// order (pinned by the producer's chunking).
	var concat []byte
	for _, h := range pageHashes {
		concat = append(concat, h[:]...)
	}
	return crypto.Keccak256Hash(concat)
}

func splitPages(buf []byte, pageSize int) [][]byte {
	if len(buf) == 0 {
		return nil
	}
	out := make([][]byte, 0, (len(buf)+pageSize-1)/pageSize)
	for i := 0; i < len(buf); i += pageSize {
		end := i + pageSize
		if end > len(buf) {
			end = len(buf)
		}
		out = append(out, buf[i:end])
	}
	return out
}

// candidateC_PerNodeMerkle hashes every state node, sorts the hashes
// lexicographically, and returns a Merkle root over the sorted hashes.
// Each node hash is independent → trivially parallelizable.
//
// On the producer side the BP already has every node's keccak from
// execution, so the per-node hash phase costs zero in steady state. This
// helper still computes the hashes from bytes because the bench needs
// realistic timings without a producer-side trie cache stub.
func candidateC_PerNodeMerkle(w *Witness, cores int) common.Hash {
	w.lock.RLock()
	nodes := make([][]byte, 0, len(w.State))
	for n := range w.State {
		nodes = append(nodes, []byte(n))
	}
	w.lock.RUnlock()

	hashes := make([]common.Hash, len(nodes))
	if cores < 1 {
		cores = 1
	}
	var wg sync.WaitGroup
	work := make(chan int, len(nodes))
	for ww := 0; ww < cores; ww++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				hashes[i] = crypto.Keccak256Hash(nodes[i])
			}
		}()
	}
	for i := range nodes {
		work <- i
	}
	close(work)
	wg.Wait()

	sort.Slice(hashes, func(i, j int) bool {
		return string(hashes[i][:]) < string(hashes[j][:])
	})
	return merkleRoot(hashes)
}

// candidateC_ProducerOnly captures the "producer has hashes for free"
// claim: given the precomputed sorted hashes, only Merkle-build cost
// remains. The bench feeds a precomputed slice so we measure JUST the
// reduction stage, isolating the win on the producer's announce path.
func candidateC_ProducerOnly(sortedHashes []common.Hash) common.Hash {
	return merkleRoot(sortedHashes)
}

// merkleRoot builds a binary Merkle tree (keccak over left||right pairs)
// over `leaves` and returns the root. Empty input → zero hash. Odd levels
// duplicate the last leaf (RFC-6962-style). 32-byte leaves.
func merkleRoot(leaves []common.Hash) common.Hash {
	if len(leaves) == 0 {
		return common.Hash{}
	}
	level := make([]common.Hash, len(leaves))
	copy(level, leaves)
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		next := make([]common.Hash, len(level)/2)
		var buf [64]byte
		for i := 0; i < len(level); i += 2 {
			copy(buf[:32], level[i][:])
			copy(buf[32:], level[i+1][:])
			next[i/2] = crypto.Keccak256Hash(buf[:])
		}
		level = next
	}
	return level[0]
}

// candidateD_HashAll is the BENCHMARK helper for D — parallel per-node
// keccak only. No walk, no map build. In production, D's verifier cost is
// essentially "hash every node" because:
//   - RLP decode of the witness already happens (cost is paid by both A and D).
//   - MakeHashDB already iterates all nodes and keccaks each, so the
//     walker's per-node hash work is amortized into existing state-prep.
//   - The walker traversal is O(num_nodes × avg_refs_per_node) map lookups,
//     dwarfed by keccak throughput on the underlying bytes.
//
// We measure D's incremental cost over the chain-prep baseline as just the
// parallel keccak phase. The reachability walk lives in
// candidateD_IntrinsicWalk for the correctness test below.
func candidateD_HashAll(w *Witness, cores int) {
	w.lock.RLock()
	nodes := make([][]byte, 0, len(w.State))
	for n := range w.State {
		nodes = append(nodes, []byte(n))
	}
	w.lock.RUnlock()

	if cores < 1 {
		cores = 1
	}
	var wg sync.WaitGroup
	work := make(chan int, len(nodes))
	for ww := 0; ww < cores; ww++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				_ = crypto.Keccak256Hash(nodes[i])
			}
		}()
	}
	for i := range nodes {
		work <- i
	}
	close(work)
	wg.Wait()
}

// candidateD_IntrinsicWalk is the CORRECTNESS reference. It verifies that
// every node in the witness is reachable from the given root via byte-
// embedded hash references, and that no orphan nodes pad the witness.
// Returns true iff the walk reaches every node exactly once.
//
// Approximation: instead of RLP-parsing each node to extract real children,
// the walker scans the node's bytes for any 32-byte window matching a
// known node hash. With random synthetic content the false-positive rate
// is negligible. This is the function the test cases assert against.
//
// `cores` controls parallel hashing of nodes. Walk itself is sequential.
func candidateD_IntrinsicWalk(w *Witness, root common.Hash, cores int) bool {
	w.lock.RLock()
	nodes := make([][]byte, 0, len(w.State))
	for n := range w.State {
		nodes = append(nodes, []byte(n))
	}
	w.lock.RUnlock()

	type entry struct {
		bytes []byte
		hash  common.Hash
	}
	hashed := make([]entry, len(nodes))
	if cores < 1 {
		cores = 1
	}
	var wg sync.WaitGroup
	work := make(chan int, len(nodes))
	for ww := 0; ww < cores; ww++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				hashed[i] = entry{bytes: nodes[i], hash: crypto.Keccak256Hash(nodes[i])}
			}
		}()
	}
	for i := range nodes {
		work <- i
	}
	close(work)
	wg.Wait()

	byHash := make(map[common.Hash][]byte, len(hashed))
	for _, e := range hashed {
		byHash[e.hash] = e.bytes
	}
	// Walk: starting from root, scan node bytes for 32-byte sequences that
	// match another node's hash. Treat every such sequence as a child
	// reference. Visit each node once.
	queue := []common.Hash{root}
	visited := make(map[common.Hash]struct{}, len(byHash))
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if _, seen := visited[h]; seen {
			continue
		}
		visited[h] = struct{}{}
		blob, ok := byHash[h]
		if !ok {
			// The walker reached a hash that isn't in the witness set.
			// In real intrinsic-verify this means the witness is missing a
			// node the trie depends on → server lied. Drop.
			return false
		}
		for off := 0; off+32 <= len(blob); off++ {
			var ref common.Hash
			copy(ref[:], blob[off:off+32])
			if _, exists := byHash[ref]; exists {
				if _, seen := visited[ref]; !seen {
					queue = append(queue, ref)
				}
			}
		}
	}
	// Every node in the witness must be reachable from the root. Bloated
	// witnesses with orphan nodes are also a server lie (they're paying
	// the verifier extra hash cost without contributing to execution).
	return len(visited) == len(byHash)
}
