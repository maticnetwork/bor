package sequencer

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

// auditStoreStub serves GetBlock for the audit and nothing else: the pass
// reads one generation per height and never streams.
type auditStoreStub struct {
	pb.UnimplementedConsumerServiceServer

	t      *testing.T
	sealed map[uint64]*types.Header
	asked  chan uint64
}

func (s *auditStoreStub) GetBlock(_ context.Context, req *pb.GetBlockRequest) (*pb.GetBlockResponse, error) {
	height := req.GetBlockNumber()
	select {
	case s.asked <- height:
	default:
	}

	header, ok := s.sealed[height]
	if !ok {
		return nil, status.Error(codes.NotFound, "unretained")
	}

	return &pb.GetBlockResponse{Entries: sealedGeneration(s.t, header)}, nil
}

// countingListener reports connections, so a test can assert on the dial
// itself rather than on a side effect that happens either way.
type countingListener struct {
	net.Listener

	accepted atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}

	return conn, err
}

func startAuditStore(t *testing.T, sealed map[uint64]*types.Header) (string, *auditStoreStub, *countingListener) {
	t.Helper()

	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	lis := &countingListener{Listener: base}
	stub := &auditStoreStub{t: t, sealed: sealed, asked: make(chan uint64, 64)}
	srv := grpc.NewServer()
	pb.RegisterConsumerServiceServer(srv, stub)

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return base.Addr().String(), stub, lis
}

// The pass as the consumer actually runs it: dial the store, read each height
// over gRPC, and route the watermark through the consumer's guarded writer.
func TestAuditPassOverGRPCRecordsMismatches(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock().Number.Uint64()

	sealed := map[uint64]*types.Header{}
	for height := uint64(1); height <= head; height++ {
		block := h.chain.GetBlockByNumber(height)
		if block == nil {
			t.Fatalf("no canonical block at %d", height)
		}
		sealed[height] = block.Header()
	}

	// The store's final generation at the head sealed a block the chain never
	// adopted.
	sealed[head] = testHeader(head, common.Hash{0xee})

	endpoint, stub, lis := startAuditStore(t, sealed)

	consumer := newAuditTestConsumer(h)
	consumer.endpoint = endpoint

	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), 0); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.runAuditPass(t.Context())

	if got, ok, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); !ok || got != head {
		t.Fatalf("watermark = (%d, %v), want (%d, true)", got, ok, head)
	}

	records := rawdb.ReadInvalidPreconfsInRange(h.chain.DB(), 1, head)
	if len(records) != 1 || records[0].Number != head || records[0].Reason != unobservedMismatchReason {
		t.Fatalf("records = %+v, want one %s at %d", records, unobservedMismatchReason, head)
	}

	if len(stub.asked) == 0 {
		t.Fatal("the pass never read the store")
	}

	// The counter has to be able to see a dial, or its use below proves nothing.
	if lis.accepted.Load() == 0 {
		t.Fatal("the listener counted no connection for a pass that read the store")
	}
}

// With nothing to audit the pass reads nothing and leaves the watermark
// alone. It deliberately does not assert that no client was built: the
// pre-dial short-circuit in runAuditPass has no observable effect, because
// grpc.NewClient is lazy and run recomputes the window anyway.
func TestAuditPassWithNothingToAuditReadsNothing(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock().Number.Uint64()

	endpoint, stub, lis := startAuditStore(t, map[uint64]*types.Header{})

	consumer := newAuditTestConsumer(h)
	consumer.endpoint = endpoint

	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), head); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.runAuditPass(t.Context())

	if len(stub.asked) != 0 {
		t.Fatalf("the pass read %d heights with nothing to audit", len(stub.asked))
	}
	if got := lis.accepted.Load(); got != 0 {
		t.Fatalf("the pass connected %d times with nothing to audit", got)
	}

	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != head {
		t.Fatalf("watermark = %d, want it untouched at %d", got, head)
	}
}

// A store that cannot be reached leaves the watermark where it was, so the
// window is retried rather than silently marked audited.
func TestAuditPassHoldsTheWatermarkWhenTheStoreIsUnreachable(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	consumer.endpoint = "127.0.0.1:1"

	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), 1); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.runAuditPass(t.Context())

	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != 1 {
		t.Fatalf("watermark = %d, want it held at 1", got)
	}
}

func TestSetAuditWindow(t *testing.T) {
	consumer := &Consumer{}
	if consumer.auditWindow != 0 {
		t.Fatal("a fresh consumer carries an audit window")
	}

	consumer.SetAuditWindow(512)
	if consumer.auditWindow != 512 {
		t.Fatalf("auditWindow = %d, want 512", consumer.auditWindow)
	}
}

// A malformed endpoint fails at client construction; the pass logs and leaves
// the watermark alone rather than treating the window as audited.
func TestAuditPassHandlesAnUndialableEndpoint(t *testing.T) {
	h := startExecHarness(t)
	consumer := newAuditTestConsumer(h)
	consumer.endpoint = "unknown-scheme://%%"

	if err := rawdb.WritePreconfAuditedThrough(h.chain.DB(), 1); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	consumer.runAuditPass(t.Context())

	if got, _, _ := rawdb.ReadPreconfAuditedThrough(h.chain.DB()); got != 1 {
		t.Fatalf("watermark = %d, want it held at 1", got)
	}
}
