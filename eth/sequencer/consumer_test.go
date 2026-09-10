package sequencer

import (
	"math/big"
	"strings"
	"testing"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
)

func testSession() *session {
	return newSession(&Consumer{index: NewIndex()})
}

// A cold-started session adopts the first entry's prefix as its seed and
// advances past it, so a mid-stream resume verifies from that point on.
func TestSessionColdSeedAdoptsTheFirstPrefix(t *testing.T) {
	seed := commitment.Head{0xaa, 0xbb}
	raw := []byte{0x01, 0x02}
	sess := testSession()

	if err := sess.handle(recordEntry(raw, seed)); err != nil {
		t.Fatalf("cold-seed handle: %v", err)
	}

	if want := commitment.FoldTxs(seed, [][]byte{raw}); sess.head != want {
		t.Fatalf("head %x, want the seed folded past the entry %x", sess.head[:8], want[:8])
	}
}

// A seeded session accepts only entries whose prefix extends its running
// head; anything else is a gap and invalidates the stream position.
func TestSessionRejectsACommitmentGap(t *testing.T) {
	seed := commitment.Seed(testChainID)
	sess := testSession()

	if err := sess.handle(recordEntry([]byte{0x01}, seed)); err != nil {
		t.Fatalf("seeding handle: %v", err)
	}

	next := recordEntry([]byte{0x02}, sess.head)
	if err := sess.handle(next); err != nil {
		t.Fatalf("chained handle: %v", err)
	}

	gapped := recordEntry([]byte{0x03}, commitment.Head{0xde, 0xad})
	if err := sess.handle(gapped); err == nil || !strings.Contains(err.Error(), "commitment gap") {
		t.Fatalf("gapped entry must fail the position, got %v", err)
	}
}

// Wire entries with short commitment or hash fields must fail the fold, not
// panic the fixed-width conversions.
func TestSessionFoldRejectsMalformedEntries(t *testing.T) {
	sess := testSession()

	short := recordEntry([]byte{0x01}, commitment.Head{})
	short.GetRecord().PrefixCommitment = []byte{0x01}

	if _, _, err := sess.fold(short); err == nil || !strings.Contains(err.Error(), "malformed entry prefix") {
		t.Fatalf("short prefix must fail the fold, got %v", err)
	}

	open := openEntry(commitment.OpenContext{Number: 7, BaseFee: big.NewInt(1)}, commitment.Head{})
	open.GetBlockOpen().ParentHash = []byte{0x01, 0x02}

	if _, _, err := sess.fold(open); err == nil || !strings.Contains(err.Error(), "malformed open parent hash") {
		t.Fatalf("short parent hash must fail the fold, got %v", err)
	}
}

// The consumer's fold must agree with the publisher-side foldEntry on every
// entry kind — both ends of the wire chain the same bytes.
func TestSessionFoldMatchesFoldEntry(t *testing.T) {
	head := commitment.Seed(testChainID)
	oc := commitment.OpenContext{
		Number:     3,
		Timestamp:  100,
		ParentHash: [32]byte{0x0f},
		GasLimit:   30_000_000,
		BaseFee:    big.NewInt(32),
	}

	entries := []*pb.Entry{
		openEntry(oc, head),
		recordEntry([]byte{0x01, 0x02}, head),
		sealEntry([]byte{0x0a, 0x0b}, head),
	}

	for _, entry := range entries {
		sess := testSession()
		sess.head = head
		sess.seeded = true

		prefix, next, err := sess.fold(entry)
		if err != nil {
			t.Fatalf("fold: %v", err)
		}

		if prefix != head {
			t.Fatalf("claimed prefix %x, entry was built on %x", prefix[:8], head[:8])
		}

		want, err := foldEntry(head, entry)
		if err != nil {
			t.Fatalf("foldEntry: %v", err)
		}

		if next != want {
			t.Fatalf("fold %x diverges from foldEntry %x", next[:8], want[:8])
		}
	}
}
