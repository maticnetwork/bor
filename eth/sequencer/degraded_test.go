package sequencer

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ethereum/go-ethereum/common"
)

// flakyConsumer drops a fixed share of tail reads, reproducing the devnet
// state that a clean in-process store cannot: "rung out of budget" fired 68
// and 77 times in one five-minute window, and every decision that depends on
// a read was making it blind that often.
//
// Failures are counted rather than random so a failure here reproduces
// exactly. slow adds latency instead of an error, for the case where the read
// returns but too late to be useful.
type flakyConsumer struct {
	pb.ConsumerServiceClient

	mu        sync.Mutex
	calls     int
	failEvery int
	slow      time.Duration
	failures  int
}

func (c *flakyConsumer) Range(ctx context.Context, req *pb.RangeRequest, opts ...grpc.CallOption) (*pb.RangeResponse, error) {
	c.mu.Lock()
	c.calls++
	drop := c.failEvery > 0 && c.calls%c.failEvery == 0

	if drop {
		c.failures++
	}

	slow := c.slow
	c.mu.Unlock()

	if slow > 0 {
		time.Sleep(slow)
	}

	if drop {
		return nil, status.Error(codes.DeadlineExceeded, "tail read out of budget")
	}

	return c.ConsumerServiceClient.Range(ctx, req, opts...)
}

// degrade wraps a publisher's consumer client so its reads start failing.
func degrade(p *Publisher, failEvery int, slow time.Duration) *flakyConsumer {
	f := &flakyConsumer{failEvery: failEvery, slow: slow}

	p.mu.Lock()
	defer p.mu.Unlock()

	f.ConsumerServiceClient = p.read.cons
	p.read.cons = f

	return f
}

// blockableConsumer can be switched off and on, so a test can correlate a
// decision with whether the read behind it worked. A raw failure ratio does
// not discriminate: readTail issues several Range calls per check, so
// "seals <= successful reads" holds even when every decision is blind.
type blockableConsumer struct {
	pb.ConsumerServiceClient

	mu      sync.Mutex
	blocked bool
}

func (c *blockableConsumer) Range(ctx context.Context, req *pb.RangeRequest, opts ...grpc.CallOption) (*pb.RangeResponse, error) {
	c.mu.Lock()
	blocked := c.blocked
	c.mu.Unlock()

	if blocked {
		return nil, status.Error(codes.DeadlineExceeded, "tail read out of budget")
	}

	return c.ConsumerServiceClient.Range(ctx, req, opts...)
}

func (c *blockableConsumer) block(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.blocked = v
}

// A contested height refuses without needing a read at all: the STALE that
// armed the hold is the store's own verdict, so degraded reads cannot turn
// contention into a blind seal — the failure that once sealed 10 of 10
// contested heights while the store was unreadable. An uncontested drained
// producer with an unreadable store still seals: production never waits.
func TestContestedRefusalSurvivesUnreadableStore(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	p.OpenBlock(2, 1700000002, parent, 30_000_000, fee25())
	p.PublishTx(testTx(t, 1))
	waitDrained(t, p, 5*time.Second)

	gate := &blockableConsumer{}

	p.mu.Lock()
	gate.ConsumerServiceClient = p.read.cons
	p.read.cons = gate
	p.mu.Unlock()

	gate.block(true)

	// Uncontested, drained, store unreadable: liveness seals.
	if !awaitOurWindow(p, 300*time.Millisecond) {
		t.Fatal("an unreadable store blocked an uncontested seal")
	}

	// Contested: the hold refuses with no read required.
	p.mu.Lock()
	p.hold = hold{after: p.ackedSeq, kind: holdSticky}
	p.mu.Unlock()

	for height := uint64(2); height <= 11; height++ {
		p.mu.Lock()
		p.curHeight = height
		p.mu.Unlock()

		if awaitOurWindow(p, 200*time.Millisecond) {
			t.Fatalf("sealed contested height %d with the store unreadable", height)
		}
	}
}

// Sustained contention across many heights, not one. The devnet had 36
// contested heights in a single run; a per-height test cannot show damage
// that only accumulates.
func TestSustainedContentionAuditsBounded(t *testing.T) {
	h := startHarness(t)
	p := newTestPublisher(t, h, &fakeChain{})

	sealed := publishBlock(t, p, 1, common.Hash{0xef}, 1)
	waitHead(t, h, p, 5*time.Second)
	parent := sealHash(t, sealed)

	degrade(p, 4, 0)

	const heights = 8

	rivals := 0

	for height := uint64(2); height <= heights; height++ {
		// A rival opens its own generation and gets acked, then we contend.
		foreignWindow(t, h, height, parent, testTx(t, 9))
		rivals++

		p.mu.Lock()
		p.curHeight = height
		p.hold = hold{after: p.ackedSeq, kind: holdSticky}
		p.mu.Unlock()

		awaitOurWindow(p, 200*time.Millisecond)

		parent = common.Hash{byte(height)} // each rival window on its own tip
	}

	// Nothing this node published can be stranded: it never sealed, so the
	// only records at risk are the rivals' own.
	audit := auditStore(t, h, map[uint64][]common.Hash{1: sealed1Txs(t, h)})

	if len(audit.Displaced) > 0 {
		t.Fatalf("sustained contention displaced %d records that were "+
			"promised at a specific height", len(audit.Displaced))
	}

	if len(audit.Revoked) > rivals {
		t.Fatalf("revoked %d records but only %d rivals published: contention "+
			"is costing more than the losers' own promises",
			len(audit.Revoked), rivals)
	}
}
