package sequencer

import (
	"context"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
	"github.com/ethereum/go-ethereum/common"
	"google.golang.org/grpc"
)

// The head a walk lands on has to be derived from the entries the walk read.
// Taking the store's word for it makes a passing CAS mean only that we echoed
// back the value we were handed, which is not a check on shared history.
func TestFoldedHeadMustMatchTheReportedHead(t *testing.T) {
	seed := commitment.Seed(1)
	open := &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
		BlockNumber:      2,
		BlockTimestamp:   1700000002,
		ParentHash:       common.Hash{0xaa}.Bytes(),
		GasLimit:         30_000_000,
		BaseFee:          big25gwei(),
		PrefixCommitment: seed.Bytes(),
	}}}

	real, err := foldEntry(seed, open)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}

	tests := []struct {
		name     string
		reported commitment.Head
		want     bool
	}{
		{"head the entries produce", real, true},
		{"head the entries do not produce", commitment.Head{0x9e}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &folder{cur: seed, ok: true}
			f.fold(open)
			f.reached(tc.reported)

			if f.ok != tc.want {
				t.Fatalf("explained = %v, want %v", f.ok, tc.want)
			}
		})
	}
}

// A block-anchored walk that comes back empty derives no head — but the empty
// page is itself the answer a boundary read needs: nothing follows the block
// we started at, so opening there cannot land on another producer's window.
func TestEmptyPageStillExplainsABoundary(t *testing.T) {
	f := &folder{ok: true, awaiting: true}
	f.reached(commitment.Head{0x11})

	if !f.ok {
		t.Fatal("an empty page was treated as an unexplained head: every " +
			"clean boundary would hold and the chain would stop opening blocks")
	}
}

// Entries that arrive without a base we can establish summarize into a head
// we cannot account for — the case that must not be trusted.
func TestUnbasedEntriesLeaveTheHeadUnexplained(t *testing.T) {
	f := &folder{ok: true, awaiting: true}
	f.fold(&pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{}}})
	f.reached(commitment.Head{0x11})

	if f.ok {
		t.Fatal("a mid-window start explained a head it never derived")
	}
}

// tamperingConsumer reports a head that its own entries do not produce.
type tamperingConsumer struct {
	pb.ConsumerServiceClient
}

func (c *tamperingConsumer) Range(ctx context.Context, req *pb.RangeRequest,
	opts ...grpc.CallOption,
) (*pb.RangeResponse, error) {
	resp, err := c.ConsumerServiceClient.Range(ctx, req, opts...)
	if err != nil || len(resp.GetEntries()) == 0 {
		return resp, err
	}

	resp.Next = commitment.Head{0x9e, 0x9e}.Bytes()

	return resp, nil
}

// End to end: a store head that the entries behind it do not produce must not
// become the position this publisher builds on.
func TestTamperedHeadIsNotAdopted(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{canonical: map[uint64]common.Hash{}})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	appendForeignOpen(t, h, 2, parent)
	appendForeignRecord(t, h, testTx(t, 0))

	p.mu.Lock()
	p.read.cons = &tamperingConsumer{ConsumerServiceClient: p.read.cons}
	p.mu.Unlock()

	if w := p.AdoptWindow(2, parent); w != nil {
		t.Fatal("adopted a window whose head the store misreported")
	}

	p.mu.Lock()
	anchor := p.anchor
	p.mu.Unlock()

	if anchor == (commitment.Head{0x9e, 0x9e}) {
		t.Fatal("anchored on a head no entry chain produces")
	}
}
