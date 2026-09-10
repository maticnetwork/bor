package sequencer

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	"github.com/0xPolygon/sequence-store-proto/devstore"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
)

// setForTest swaps a package variable for the test's duration.
func setForTest[T any](t *testing.T, p *T, v T) {
	t.Helper()

	old := *p
	*p = v

	t.Cleanup(func() { *p = old })
}

// stallPublisher accepts the stream and reads entries but never acks — a
// hung store from the send loop's point of view.
type stallPublisher struct {
	pb.UnimplementedPublisherServiceServer
	sessions atomic.Int64
	received atomic.Int64
}

func (s *stallPublisher) Publish(stream pb.PublisherService_PublishServer) error {
	s.sessions.Add(1)

	for {
		if _, err := stream.Recv(); err != nil {
			return err
		}

		s.received.Add(1)
	}
}

// startStallStore serves a store whose publish stream never acks, with reads
// answered from an empty devstore so the startup reconcile anchors.
func startStallStore(t *testing.T) (*stallPublisher, *Publisher) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	stall := &stallPublisher{}
	srv := grpc.NewServer()
	pb.RegisterPublisherServiceServer(srv, stall)
	pb.RegisterConsumerServiceServer(srv, devstore.New(testChainID))

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	p, err := NewPublisher(lis.Addr().String(), lis.Addr().String(), testChainID, 0, nil, nil)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	t.Cleanup(p.Close)

	return stall, p
}

// A store that stops acking must trip the stall deadline and reconnect
// (acks stall → degraded) rather than sitting live forever.
func TestStreamAckStallReconnects(t *testing.T) {
	setForTest(t, &ackStallTimeout, 100*time.Millisecond)

	stall, p := startStallStore(t)

	publishBlock(t, p, 1, common.Hash{0xef}, 1)

	waitFor(t, 5*time.Second, func() bool { return stall.sessions.Load() >= 3 })

	if p.failed.Load() {
		t.Fatal("ack stall must degrade and retry, not fail terminally")
	}
}

// A store that is not acking gets at most the in-flight cap: an unbounded
// sender once buried the store under its own backlog and turned the stall
// watchdog into a false outage signal.
func TestSendPausesAtTheInflightCap(t *testing.T) {
	setForTest(t, &maxInflightEntries, 4)
	// Keep the watchdog out of the way: a reconnect resends and would count
	// the same entries twice.
	setForTest(t, &ackStallTimeout, time.Minute)

	stall, p := startStallStore(t)

	publishBlock(t, p, 1, common.Hash{0xef}, 10) // open + 10 records + seal

	waitFor(t, 5*time.Second, func() bool { return stall.received.Load() == 4 })
	time.Sleep(200 * time.Millisecond)

	if got := stall.received.Load(); got != 4 {
		t.Fatalf("sender pushed %d entries into a non-acking store, cap is 4", got)
	}
}

// A cap smaller than the window must not wedge the drain: acks free slots
// and the post-ack send refills, so the whole journal still delivers even
// when the worker appends nothing further.
func TestCappedSenderStillDrainsCompletely(t *testing.T) {
	setForTest(t, &maxInflightEntries, 2)

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	publishBlock(t, p, 1, common.Hash{0xef}, 6) // open + 6 records + seal
	waitHead(t, h, p, 5*time.Second)
}

// An eviction gap must be reported even when a hold gates everything past
// the cursor: the gap is the drain cycle's reconcile trigger, and a devnet
// stranded a whole outage window when an early-out answered before the
// coverage check ran.
func TestGapDetectionOutranksTheHoldEarlyOut(t *testing.T) {
	p := barePublisher()
	p.journal.nextSeq = 5
	p.journal.append(nil, commitment.Head{}, commitment.Head{0x01}, entryRecord, 3, 0, nil)
	p.hold = hold{after: 0, kind: holdBuild}

	if _, _, ok := p.sendableAfter(1, 100); ok {
		t.Fatal("a gated suffix hid the eviction gap; the drain cycle never reconciles")
	}
}

// A store restart once wedged live gRPC channels in a permanent
// connect-retry loop while fresh dials worked. After a whole interval of
// silence the publisher rebuilds its connections rather than trusting the
// channel state machine — and keeps working through the swap.
func TestRedialAfterProlongedSilence(t *testing.T) {
	setForTest(t, &redialAfter, 400*time.Millisecond)

	h := startHarness(t)
	p := newTestPublisher(t, h, nil)

	first := publishBlock(t, p, 1, common.Hash{0xef}, 2)
	waitHead(t, h, p, 5*time.Second)

	before := publishRedialCount.Snapshot().Count()

	h.stop()
	waitFor(t, 10*time.Second, func() bool {
		return publishRedialCount.Snapshot().Count() > before
	})

	h.resume()
	publishBlock(t, p, 2, first.Hash(), 2)
	waitHead(t, h, p, 10*time.Second)
}

// The ack-stall watchdog measures how long the current batch of acks has been
// outstanding, not the idle gap before it. sent() opening a fresh wait
// (inflight 0→1) must restart the clock, or the first entry sent after a quiet
// stretch longer than the deadline reads as stalled the instant it goes out —
// the store never gets its window to ack — and the watchdog resets a healthy
// low-load stream, the break behind the auditor's phantom reorg at 51933.
func TestStallTrackerIgnoresIdleGapBeforeAFreshSend(t *testing.T) {
	deadline := 5 * time.Second
	tr := newStallTracker()

	// A quiet stretch longer than the deadline, nothing in flight.
	idle := time.Now().Add(-10 * time.Second)
	tr.lastAck.Store(idle.UnixNano())

	if tr.stalled(deadline) {
		t.Fatal("an idle stream with nothing in flight must not read as stalled")
	}

	// The first entry after the lull opens a fresh wait: the clock restarts,
	// so it is not instantly stalled before the store can ack.
	tr.sent()

	if tr.stalled(deadline) {
		t.Fatal("the first send after an idle gap tripped the watchdog before " +
			"the store had any chance to ack")
	}

	// A store that then genuinely withholds the ack past the deadline still
	// trips: the clock measures this outstanding entry.
	tr.lastAck.Store(time.Now().Add(-10 * time.Second).UnixNano())

	if !tr.stalled(deadline) {
		t.Fatal("a real ack drought on an in-flight entry must still stall")
	}
}

// Sends that pipeline onto an existing wait must not restart the clock — only
// a fresh wait does — so a hung store we keep feeding still trips the watchdog.
func TestStallTrackerPipelinedSendKeepsTheClock(t *testing.T) {
	deadline := 5 * time.Second
	tr := newStallTracker()

	tr.sent() // inflight 0→1, clock fresh

	// The entry sits unacked past the deadline while a second entry pipelines
	// behind it; the second send must not reset the wait.
	tr.lastAck.Store(time.Now().Add(-10 * time.Second).UnixNano())
	tr.sent() // inflight 1→2

	if !tr.stalled(deadline) {
		t.Fatal("a pipelined send reset the stall clock and masked a hung store")
	}
}
