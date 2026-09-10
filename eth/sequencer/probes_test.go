package sequencer

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ethereum/go-ethereum/common"
)

// edgeConsumer answers Range block reads for heights 1..edge and NOT_FOUND
// above, isolating the probe arithmetic from transport.
type edgeConsumer struct {
	pb.ConsumerServiceClient
	edge uint64
}

func (c *edgeConsumer) Range(_ context.Context, req *pb.RangeRequest, _ ...grpc.CallOption) (*pb.RangeResponse, error) {
	h := req.GetBlock()
	if c.edge > 0 && h >= 1 && h <= c.edge {
		return &pb.RangeResponse{}, nil
	}

	return nil, status.Error(codes.NotFound, "unknown block")
}

func probePublisher(edge uint64) *Publisher {
	p := barePublisher()
	p.read.cons = &edgeConsumer{edge: edge}

	return p
}

func TestProbeDownTable(t *testing.T) {
	cases := []struct {
		name      string
		edge      uint64
		from      uint64
		want      uint64
		wantFound bool
	}{
		{name: "far above edge", edge: 3, from: 10, want: 3, wantFound: true},
		{name: "exact hit", edge: 3, from: 3, want: 3, wantFound: true},
		{name: "one above edge", edge: 3, from: 4, want: 3, wantFound: true},
		{name: "single block store", edge: 1, from: 1, want: 1, wantFound: true},
		{name: "descent lands inside store", edge: 2, from: 4, want: 2, wantFound: true},
		{name: "power of two descent", edge: 7, from: 8, want: 7, wantFound: true},
		// The descent gives up once its stride overshoots (step >= h): a low
		// edge far below the start is the floor-read rung's job.
		{name: "edge below descent reach", edge: 1, from: 5, wantFound: false},
		{name: "deep gap gives up", edge: 2, from: 100, wantFound: false},
		{name: "empty store", edge: 0, from: 6, wantFound: false},
		{name: "empty store from one", edge: 0, from: 1, wantFound: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := probePublisher(tc.edge)

			got, found, err := p.read.probeDown(context.Background(), tc.from)
			if err != nil {
				t.Fatalf("probeDown err: %v", err)
			}

			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}

			if found && got != tc.want {
				t.Fatalf("edge = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestProbeUpTable(t *testing.T) {
	cases := []struct {
		name string
		edge uint64
		h0   uint64
		want uint64
	}{
		{name: "from floor", edge: 3, h0: 1, want: 3},
		{name: "already at edge", edge: 3, h0: 3, want: 3},
		{name: "doubling ascent", edge: 8, h0: 1, want: 8},
		{name: "mid start", edge: 5, h0: 2, want: 5},
		{name: "long ascent", edge: 21, h0: 1, want: 21},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := probePublisher(tc.edge)

			got, err := p.read.probeUp(context.Background(), tc.h0)
			if err != nil {
				t.Fatalf("probeUp err: %v", err)
			}

			if got != tc.want {
				t.Fatalf("edge = %d, want %d", got, tc.want)
			}
		})
	}
}

type failingConsumer struct {
	pb.ConsumerServiceClient
}

func (c *failingConsumer) Range(context.Context, *pb.RangeRequest, ...grpc.CallOption) (*pb.RangeResponse, error) {
	return nil, status.Error(codes.Unavailable, "store down")
}

// Transport errors must propagate out of the probes — only NOT_FOUND means
// "unknown height"; anything else aborts the ladder rung.
func TestProbesPropagateTransportErrors(t *testing.T) {
	p := barePublisher()
	p.read.cons = &failingConsumer{}

	if _, _, err := p.read.probeDown(context.Background(), 10); err == nil {
		t.Fatal("probeDown swallowed a transport error")
	}

	if _, err := p.read.probeUp(context.Background(), 1); err == nil {
		t.Fatal("probeUp swallowed a transport error")
	}

	if _, _, err := p.read.binarySearchEdge(context.Background(), 1, 5); err == nil {
		t.Fatal("binarySearchEdge swallowed a transport error")
	}

	if _, err := p.read.blockKnown(context.Background(), 1); err == nil {
		t.Fatal("blockKnown treated a transport error as an answer")
	}
}

// Unacked accounting is by sequence arithmetic: entries evicted from the
// journal while unconfirmed still count (they are what a forward jump drops).
func TestUnackedCountsEvicted(t *testing.T) {
	p := barePublisher()

	parent := common.Hash{0xef}
	for n := uint64(1); n <= 42; n++ {
		header := testHeader(n, parent)
		p.OpenBlock(n, header.Time, header.ParentHash, header.GasLimit, header.BaseFee)
		p.SealBlock(blockFor(header, nil))
		parent = header.Hash()
	}

	if p.failed.Load() {
		t.Fatal("publisher failed during setup")
	}

	total := 2 * 42

	p.mu.Lock()
	defer p.mu.Unlock()

	if got := p.unackedLocked(); got != total {
		t.Fatalf("unacked = %d, want %d (evicted entries must count)", got, total)
	}

	if int(p.journal.nextSeq-1) != total {
		t.Fatalf("nextSeq = %d, want %d appends", p.journal.nextSeq-1, total)
	}

	if items, _ := p.journal.after(0); len(items) >= total {
		t.Fatal("eviction did not trim the journal; test premise broken")
	}

	p.ackedSeq = 3

	if got := p.unackedLocked(); got != total-3 {
		t.Fatalf("unacked after acks = %d, want %d", got, total-3)
	}
}

// endlessConsumer returns a full page on every Range call and never reports
// live — a tail longer than the walk bound from the requested position.
type endlessConsumer struct {
	pb.ConsumerServiceClient
}

func (c *endlessConsumer) Range(_ context.Context, req *pb.RangeRequest, _ ...grpc.CallOption) (*pb.RangeResponse, error) {
	n := int(req.GetLimit())
	if n == 0 || n > 64 {
		n = 64
	}

	entries := make([]*pb.Entry, n)
	for i := range entries {
		entries[i] = &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{}}}
	}

	return &pb.RangeResponse{Entries: entries, Next: make([]byte, 32)}, nil
}

// A rung whose position is too far behind the tip to walk within bound must
// fall through the ladder (like NOT_FOUND), not spin on the same rung.
func TestTryWalkFallsThroughOnTooLongTail(t *testing.T) {
	p := barePublisher()
	p.read.cons = &endlessConsumer{}

	_, out, done := p.tryWalk(context.Background(), &pb.RangeRequest{}, false)
	if done {
		t.Fatal("too-long tail must fall through to the next ladder rung")
	}

	if out != recRetry {
		t.Fatalf("outcome = %v, want recRetry", out)
	}
}

// slowEndlessConsumer serves endless slow pages for tail walks from one
// specific position (a stale anchor over a huge history) and delegates
// everything else to the real store client.
type slowEndlessConsumer struct {
	pb.ConsumerServiceClient
	stale []byte
}

func (c *slowEndlessConsumer) Range(ctx context.Context, req *pb.RangeRequest, opts ...grpc.CallOption) (*pb.RangeResponse, error) {
	if h := req.GetHead(); h != nil && bytes.Equal(h, c.stale) {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			// Like the real gRPC client: a status error, not the raw
			// context sentinel.
			return nil, status.Error(codes.DeadlineExceeded, ctx.Err().Error())
		}

		n := int(req.GetLimit())
		if n == 0 || n > 64 {
			n = 64
		}

		entries := make([]*pb.Entry, n)
		for i := range entries {
			entries[i] = &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{}}}
		}

		return &pb.RangeResponse{Entries: entries, Next: c.stale}, nil
	}

	return c.ConsumerServiceClient.Range(ctx, req, opts...)
}

// A takeover build whose publisher anchored far behind (an idle adopter)
// must still find and adopt the dangling window: the stale-anchor rung
// runs out of its budget slice and the ladder falls through to the
// tip-edge probe instead of consuming the whole read budget.
func TestStaleAnchorTakeoverStillAdopts(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	parent := sealHash(t, sealed)
	tx := testTx(t, 0)
	foreignWindow(t, h, 2, parent, tx)

	// Pin the anchor (and the persisted head) to a position that walks
	// forever — the stale-anchor shape after a long idle stretch.
	stale := commitment.Head{0xaa, 0xbb}
	p.mu.Lock()
	p.read.cons = &slowEndlessConsumer{ConsumerServiceClient: p.read.cons, stale: stale.Bytes()}
	p.anchor = stale
	p.confirmed = true
	p.mu.Unlock()

	w := p.AdoptWindow(2, parent)
	if w == nil || len(w.Txs) != 1 || w.Txs[0].Hash() != tx.Hash() {
		t.Fatalf("stale-anchor read missed the window: %+v", w)
	}
}

// An idle publisher (a non-producer between spans) periodically re-anchors
// near the store tip, so its eventual takeover read starts close.
func TestIdleReconcileKeepsAnchorFresh(t *testing.T) {
	restore := idleReconcileInterval
	idleReconcileInterval = 80 * time.Millisecond

	t.Cleanup(func() { idleReconcileInterval = restore })

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)

	// A foreign producer seals 2 and opens 3: the idle publisher should
	// re-anchor at window 3's base without any build of its own.
	parent := sealHash(t, sealed)
	ts, gasLimit := foreignWindow(t, h, 2, parent, testTx(t, 0))
	appendForeignSeal(t, h, windowHeader(2, parent, ts, gasLimit))

	base := h.store.Head()
	foreignWindow(t, h, 3, common.Hash{0x33}, testTx(t, 1))

	waitFor(t, 5*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()

		return p.anchor == base
	})
}

// emptyNotLiveConsumer models a trailing gateway: an empty page that is
// not yet at the tip. floorRead must fall through to the full floor walk
// instead of indexing the empty page (a crash found by adversarial review).
type emptyNotLiveConsumer struct{ pb.ConsumerServiceClient }

func (c *emptyNotLiveConsumer) Range(_ context.Context, req *pb.RangeRequest, _ ...grpc.CallOption) (*pb.RangeResponse, error) {
	if req.GetLimit() == 1 {
		return &pb.RangeResponse{Next: make([]byte, 32), Live: false}, nil
	}

	return &pb.RangeResponse{Next: make([]byte, 32), Live: true}, nil
}

func TestFloorReadEmptyNotLivePage(t *testing.T) {
	p := barePublisher()
	p.read.cons = &emptyNotLiveConsumer{}

	info, out := p.read.floorRead(t.Context())
	if out == recTerminal {
		t.Fatalf("empty not-live floor page must not be terminal: %v", out)
	}

	_ = info
}
