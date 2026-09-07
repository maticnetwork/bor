package sequencer

import (
	"context"
	"io"
	"math/big"
	"testing"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/params"
)

// The watermark may only advance for heights this node actually watched. A
// session that dropped leaves a window nobody compared, and it has to stay
// behind the mark so the next audit pass walks it.
func TestWatermarkAdvancesOnlyWhileWatchingTheTip(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)

	head := h.chain.CurrentBlock().Number.Uint64()
	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), head-1); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.markCanonicalHeadAudited()
	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != head-1 {
		t.Fatal("watermark advanced while the consumer was not following the store tip")
	}

	consumer.watching.Store(true)
	consumer.markCanonicalHeadAudited()

	got, ok, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB())
	if !ok || got != head {
		t.Fatalf("watermark = (%d, %v), want (%d, true)", got, ok, head)
	}
}

// A head more than one block past the mark would carry it over the catch-up
// backlog the session dropped without comparing. That asks for an audit pass
// instead of stepping.
func TestWatermarkGapAsksForAnAuditInsteadOfJumping(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	consumer.watching.Store(true)

	head := h.chain.CurrentBlock().Number.Uint64()
	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), head-2); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.markCanonicalHeadAudited()

	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != head-2 {
		t.Fatalf("watermark = %d, want it held at %d across a gap", got, head-2)
	}

	if len(consumer.auditTrigger) != 1 {
		t.Fatal("a gap did not request an audit pass")
	}
}

// With no watermark at all there is no position to step from; the audit seeds
// it.
func TestWatermarkAbsentAsksForAnAudit(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	consumer.watching.Store(true)

	consumer.markCanonicalHeadAudited()

	if _, ok, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); ok {
		t.Fatal("the live path seeded a watermark it never audited")
	}

	if len(consumer.auditTrigger) != 1 {
		t.Fatal("a missing watermark did not request an audit pass")
	}
}

// A reorg can move the head below the mark; the mark never follows it down.
func TestWatermarkHoldsWhenTheHeadMovesBack(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	consumer.watching.Store(true)

	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), 5_000); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.markCanonicalHeadAudited()

	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != 5_000 {
		t.Fatalf("watermark = %d, want 5000", got)
	}
}

func newAuditTestConsumer(h *execHarness) *Consumer {
	return &Consumer{
		chain:        h.chain,
		index:        NewIndex(),
		store:        NewPendingStore(h.chain.DB()),
		auditTrigger: make(chan struct{}, 1),
	}
}

func TestAdvanceAuditedNeverRewinds(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)

	consumer.advanceAudited(50)
	consumer.advanceAudited(20)

	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != 50 {
		t.Fatalf("watermark = %d, want 50", got)
	}
}

// The store's caught-up-to-tip frame is what makes a canonical head something
// this node observed; any other non-entry frame must not claim that.
func TestLiveFrameStartsWatching(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	sess := newSession(consumer)

	if _, err := handlePreparedStreamFrame(sess, preparedStreamFrame{}); err != nil {
		t.Fatalf("empty frame: %v", err)
	}

	if consumer.watching.Load() {
		t.Fatal("a frame that was not the live marker started watching")
	}

	if _, err := handlePreparedStreamFrame(sess, preparedStreamFrame{live: true}); err != nil {
		t.Fatalf("live frame: %v", err)
	}

	if !consumer.watching.Load() {
		t.Fatal("the live marker did not start watching")
	}

	// Catch-up is over at the live marker, so whatever it replayed past has
	// to be audited.
	if len(consumer.auditTrigger) != 1 {
		t.Fatal("reaching the tip did not request an audit pass")
	}
}

// The canonical-head handler is what the chain actually calls; the watermark
// has to advance through it, not just through the helper.
func TestHandleCanonicalHeadAdvancesTheWatermark(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	consumer.watching.Store(true)

	head := h.chain.CurrentBlock().Number.Uint64()
	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), head-1); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.handleCanonicalHead()

	if got, ok, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); !ok || got != head {
		t.Fatalf("watermark = (%d, %v), want (%d, true)", got, ok, head)
	}
}

func TestPrepareStreamFlagsTheLiveFrame(t *testing.T) {
	frames := []*pb.StreamResponse{
		{Frame: &pb.StreamResponse_Live{Live: &pb.Live{}}},
	}

	consumer := &Consumer{}
	out := make(chan preparedStreamFrame, 4)
	consumer.prepareStream(t.Context(), &sliceStream{frames: frames}, streamPreparationState{}, out)

	frame, ok := <-out
	if !ok {
		t.Fatal("no frame prepared")
	}

	if !frame.live {
		t.Fatal("the live frame was not flagged")
	}
}

type sliceStream struct {
	frames []*pb.StreamResponse
	at     int
}

func (s *sliceStream) Recv() (*pb.StreamResponse, error) {
	if s.at >= len(s.frames) {
		return nil, io.EOF
	}
	frame := s.frames[s.at]
	s.at++

	return frame, nil
}

func TestRequestAuditCoalesces(t *testing.T) {
	consumer := &Consumer{auditTrigger: make(chan struct{}, 1)}

	consumer.requestAudit()
	consumer.requestAudit()

	if len(consumer.auditTrigger) != 1 {
		t.Fatalf("queued %d passes, want 1: the window is recomputed when a pass starts", len(consumer.auditTrigger))
	}
}

// A trigger has to actually run a pass. Seeding the first watermark is the
// one pass that reaches no further than the local chain, so it exercises the
// loop without a store connection.
func TestAuditLoopRunsAPassOnTrigger(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.auditLoop(ctx)
	}()

	consumer.requestAudit()

	head := h.chain.CurrentBlock().Number.Uint64()
	deadline := time.After(2 * time.Second)
	for {
		if got, ok, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); ok && got == head {
			break
		}

		select {
		case <-deadline:
			t.Fatal("the audit loop did not run a pass for a queued trigger")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// A precondition failure must not queue an audit. The run loop retries every
// consumerRetryDelay, so a node that stays ineligible — pre-Rio, no coinbase
// map — would otherwise walk the store on a two-second loop. Observed on a
// kurtosis devnet: a pass every 2s while the chain sat below the Rio height.
func TestIneligibleSessionDoesNotQueueAnAudit(t *testing.T) {
	// Rio far in the future, so the node is not preconf-eligible.
	h := startExecHarnessBor(t, &params.BorConfig{
		Sprint:   map[string]uint64{"0": 16},
		RioBlock: big.NewInt(1_000_000),
		Coinbase: map[string]string{
			"0": "0x000000000000000000000000000000000000ba5e",
		},
		BurntContract: map[string]string{
			"0": "0x000000000000000000000000000000000000dead",
		},
	})
	consumer := newAuditTestConsumer(h)

	if err := consumer.deterministic(); err == nil {
		t.Fatal("chain is preconf-eligible; the test needs an ineligible one")
	}

	sess, err := consumer.runSession(t.Context(), nil)
	if err == nil {
		t.Fatal("an ineligible chain ran a session")
	}
	if sess != nil {
		t.Fatal("an ineligible chain produced a session")
	}

	if len(consumer.auditTrigger) != 0 {
		t.Fatal("a precondition failure queued an audit pass")
	}
}
