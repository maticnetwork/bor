package stateless

import (
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// BenchmarkWitnessEncodeRLP measures the cost of EncodeRLP, which sorts
// state nodes lexicographically before serialization. Surfaces regressions if
// the comparator changes (e.g. swapping bytes.Compare for an allocating
// alternative). Synthetic 50 MiB witness with realistic node sizes.
func BenchmarkWitnessEncodeRLP(b *testing.B) {
	for _, sizeMiB := range []int{1, 15, 50} {
		w := buildSyntheticWitness(sizeMiB<<20, 256)
		b.Run(fmt.Sprintf("%dMiB", sizeMiB), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := w.EncodeRLP(discardWriter{}); err != nil {
					b.Fatalf("encode: %v", err)
				}
			}
		})
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// benchWitnessSizes runs fn as a sub-benchmark per representative witness
// size, handing it a random buffer of that size. b.SetBytes lets
// `go test -benchmem` print throughput in MB/s alongside ns/op, which is what
// we actually want to know — the absolute size of any one witness varies, but
// per-byte cost scales linearly.
func benchWitnessSizes(b *testing.B, fn func(b *testing.B, buf []byte)) {
	for _, sizeMiB := range []int{1, 5, 15, 30, 50} {
		size := sizeMiB << 20
		buf := make([]byte, size)
		if _, err := rand.Read(buf); err != nil {
			b.Fatalf("rand: %v", err)
		}
		b.Run(fmt.Sprintf("%dMiB", sizeMiB), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ResetTimer()
			fn(b, buf)
		})
	}
}

// BenchmarkWitnessKeccakBySize measures the throughput of keccak256 over a
// pre-allocated witness-sized buffer. This is the cost the producer pays to
// compute WitnessHash on the WIT2 announce path (and the cost a relayer or
// requester pays to verify response bytes against the BP-signed WitnessHash).
//
// Run with `go test -bench=BenchmarkWitnessKeccakBySize ./core/stateless/`.
func BenchmarkWitnessKeccakBySize(b *testing.B) {
	benchWitnessSizes(b, func(b *testing.B, buf []byte) {
		for i := 0; i < b.N; i++ {
			_ = crypto.Keccak256Hash(buf)
		}
	})
}

// BenchmarkWitnessAnnounceSign measures the marginal ECDSA cost of signing the
// 32-byte announcement digest, independent of witness size. This isolates the
// secp256k1 sign cost from the keccak cost so a single number per platform is
// directly comparable to libsecp256k1 microbenchmarks.
func BenchmarkWitnessAnnounceSign(b *testing.B) {
	key, err := crypto.GenerateKey()
	if err != nil {
		b.Fatalf("key: %v", err)
	}
	digest := make([]byte, 32)
	if _, err := rand.Read(digest); err != nil {
		b.Fatalf("rand: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := crypto.Sign(digest, key); err != nil {
			b.Fatalf("sign: %v", err)
		}
	}
}

// BenchmarkWitnessHashAndSignCombined measures the realistic producer-side
// cost of the WIT2 announce path: keccak256 over witness bytes followed by
// ECDSA sign over the (small) signing digest. This is the latency the BP
// adds before emitting a signed announce. Compare against the ~500ms-per-hop
// savings: as long as this stays well under the savings, the change is a
// net win even at 50 MiB witnesses.
func BenchmarkWitnessHashAndSignCombined(b *testing.B) {
	key, err := crypto.GenerateKey()
	if err != nil {
		b.Fatalf("key: %v", err)
	}
	benchWitnessSizes(b, func(b *testing.B, buf []byte) {
		for i := 0; i < b.N; i++ {
			witnessHash := crypto.Keccak256Hash(buf)
			digest := crypto.Keccak256Hash(witnessHash[:], []byte{0x01, 0x02, 0x03, 0x04})
			if _, err := crypto.Sign(digest[:], key); err != nil {
				b.Fatalf("sign: %v", err)
			}
		}
	})
}
