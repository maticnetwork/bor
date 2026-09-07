package sequencer

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

type stubAuditChain struct {
	head   uint64
	hashes map[uint64]common.Hash
}

func (s *stubAuditChain) CurrentBlock() *types.Header {
	return &types.Header{Number: new(big.Int).SetUint64(s.head)}
}

func (s *stubAuditChain) GetCanonicalHash(number uint64) common.Hash {
	return s.hashes[number]
}

// auditFixture builds a chain whose canonical hash at every height is the
// header the store also sealed, so the store and the chain agree everywhere
// until a test makes them disagree.
func auditFixture(t *testing.T, through uint64) (*stubAuditChain, map[uint64]*types.Header) {
	t.Helper()

	chain := &stubAuditChain{head: through, hashes: map[uint64]common.Hash{}}
	sealed := map[uint64]*types.Header{}

	for height := uint64(1); height <= through; height++ {
		header := testHeader(height, common.Hash{byte(height)})
		sealed[height] = header
		chain.hashes[height] = header.Hash()
	}

	return chain, sealed
}

func sealedGeneration(t *testing.T, header *types.Header) []*pb.Entry {
	t.Helper()

	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatalf("rlp: %v", err)
	}

	return []*pb.Entry{
		{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{BlockNumber: header.Number.Uint64()}}},
		{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{Header: raw}}},
	}
}

func fetchFrom(t *testing.T, sealed map[uint64]*types.Header) fetchGeneration {
	t.Helper()

	return func(_ context.Context, height uint64) ([]*pb.Entry, error) {
		header, ok := sealed[height]
		if !ok {
			return nil, status.Error(codes.NotFound, "unretained")
		}

		return sealedGeneration(t, header), nil
	}
}

func auditedThrough(t *testing.T, db ethdb.Database) uint64 {
	t.Helper()

	number, ok := rawdb.ReadPreconfAuditedThrough(db)
	if !ok {
		t.Fatal("no audit watermark stored")
	}

	return number
}

func TestAuditFirstRunSeedsWatermarkWithoutWalking(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 20)

	walked := 0
	audit := &auditor{db: db, chain: chain, fetch: func(ctx context.Context, height uint64) ([]*pb.Entry, error) {
		walked++
		return fetchFrom(t, sealed)(ctx, height)
	}}

	if _, err := audit.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if walked != 0 {
		t.Fatalf("first run read %d heights, want 0: a node that never audited has no window", walked)
	}

	if got := auditedThrough(t, db); got != 20 {
		t.Fatalf("watermark = %d, want the head 20", got)
	}
}

func TestAuditRecordsMismatchTheNodeNeverServed(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 12)

	// The store's final generation at height 7 sealed a different block than
	// the one that became canonical, and this node was down for it.
	sealed[7] = testHeader(7, common.Hash{0xee})

	if err := rawdb.WritePreconfAuditedThrough(db, 4); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: fetchFrom(t, sealed)}
	summary, err := audit.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if summary.from != 5 || summary.through != 12 {
		t.Fatalf("window = [%d,%d], want [5,12]", summary.from, summary.through)
	}

	if summary.mismatch != 1 {
		t.Fatalf("mismatches = %d, want 1", summary.mismatch)
	}

	records := rawdb.ReadInvalidPreconfsInRange(db, 5, 12)
	if len(records) != 1 || records[0].Number != 7 || records[0].Reason != unobservedMismatchReason {
		t.Fatalf("records = %+v, want one %s at 7", records, unobservedMismatchReason)
	}

	if got := auditedThrough(t, db); got != 12 {
		t.Fatalf("watermark = %d, want 12", got)
	}
}

func TestAuditCleanWindowRecordsNothing(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 30)

	if err := rawdb.WritePreconfAuditedThrough(db, 10); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: fetchFrom(t, sealed)}
	summary, err := audit.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if summary.mismatch != 0 || summary.unknown != 0 {
		t.Fatalf("summary = %+v, want a clean window", summary)
	}

	if records := rawdb.ReadInvalidPreconfsInRange(db, 0, 30); len(records) != 0 {
		t.Fatalf("records = %+v, want none", records)
	}

	if got := auditedThrough(t, db); got != 30 {
		t.Fatalf("watermark = %d, want 30", got)
	}
}

// Heights the store never held, and heights it left open, promised nothing —
// the watermark still has to clear them or the same window is re-walked forever.
func TestAuditAdvancesPastHeightsThatPromisedNothing(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 8)

	delete(sealed, 6) // unretained: NotFound
	unsealed := uint64(7)

	if err := rawdb.WritePreconfAuditedThrough(db, 5); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: func(ctx context.Context, height uint64) ([]*pb.Entry, error) {
		if height == unsealed {
			return []*pb.Entry{{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{BlockNumber: height}}}}, nil
		}

		return fetchFrom(t, sealed)(ctx, height)
	}}

	if _, err := audit.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if records := rawdb.ReadInvalidPreconfsInRange(db, 0, 8); len(records) != 0 {
		t.Fatalf("records = %+v, want none", records)
	}

	if got := auditedThrough(t, db); got != 8 {
		t.Fatalf("watermark = %d, want 8", got)
	}
}

func TestAuditWindowTruncatesAndMarksTheRemainderUnaudited(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 100)

	if err := rawdb.WritePreconfAuditedThrough(db, 1); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: fetchFrom(t, sealed), window: 10}
	summary, err := audit.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if summary.from != 91 || summary.through != 100 {
		t.Fatalf("window = [%d,%d], want the most recent 10: [91,100]", summary.from, summary.through)
	}

	if summary.skippedTo != 90 {
		t.Fatalf("skippedTo = %d, want 90", summary.skippedTo)
	}

	unaudited, ok := rawdb.ReadPreconfUnauditedThrough(db)
	if !ok || unaudited != 90 {
		t.Fatalf("unaudited mark = (%d, %v), want (90, true): a truncated window must not read as clean", unaudited, ok)
	}
}

func TestAuditPersistsProgressWhenTheStoreFails(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 40)

	if err := rawdb.WritePreconfAuditedThrough(db, 9); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	transport := errors.New("connection reset")
	audit := &auditor{db: db, chain: chain, fetch: func(ctx context.Context, height uint64) ([]*pb.Entry, error) {
		if height == 15 {
			return nil, transport
		}

		return fetchFrom(t, sealed)(ctx, height)
	}}

	if _, err := audit.run(context.Background()); !errors.Is(err, transport) {
		t.Fatalf("err = %v, want the transport error", err)
	}

	if got := auditedThrough(t, db); got != 14 {
		t.Fatalf("watermark = %d, want 14: progress up to the failing height is kept", got)
	}
}

func TestAuditPersistsProgressOnCancel(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 40)

	if err := rawdb.WritePreconfAuditedThrough(db, 9); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	audit := &auditor{db: db, chain: chain, fetch: func(fctx context.Context, height uint64) ([]*pb.Entry, error) {
		if height == 12 {
			cancel()
		}

		return fetchFrom(t, sealed)(fctx, height)
	}}

	defer cancel()

	if _, err := audit.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	if got := auditedThrough(t, db); got != 12 {
		t.Fatalf("watermark = %d, want 12", got)
	}
}

func TestAuditDoesNotRewindTheWatermark(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 20)

	if err := rawdb.WritePreconfAuditedThrough(db, 5); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: fetchFrom(t, sealed)}

	// The live path races ahead while the pass is walking.
	if err := rawdb.WritePreconfAuditedThrough(db, 500); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if _, err := audit.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := auditedThrough(t, db); got != 500 {
		t.Fatalf("watermark = %d, want 500: a finishing pass must not rewind it", got)
	}
}

func TestAuditWindowIsEmptyAtTheHead(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, _ := auditFixture(t, 20)

	if err := rawdb.WritePreconfAuditedThrough(db, 20); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: func(context.Context, uint64) ([]*pb.Entry, error) {
		t.Fatal("audited with nothing to audit")
		return nil, nil
	}}

	if _, _, _, ok := audit.windowToAudit(); ok {
		t.Fatal("window is non-empty at the head")
	}

	if _, err := audit.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestAuditHeight(t *testing.T) {
	header := testHeader(9, common.Hash{0x09})
	other := testHeader(9, common.Hash{0xaa})

	cases := []struct {
		name      string
		entries   []*pb.Entry
		canonical common.Hash
		want      auditVerdict
	}{
		{
			name:      "seal is canonical",
			entries:   sealedGeneration(t, header),
			canonical: header.Hash(),
			want:      auditMatch,
		},
		{
			name:      "seal is not canonical",
			entries:   sealedGeneration(t, other),
			canonical: header.Hash(),
			want:      auditMismatch,
		},
		{
			name:      "never sealed",
			entries:   []*pb.Entry{{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{BlockNumber: 9}}}},
			canonical: header.Hash(),
			want:      auditNoSeal,
		},
		{
			name:      "no canonical block to compare",
			entries:   sealedGeneration(t, header),
			canonical: common.Hash{},
			want:      auditUnknown,
		},
		{
			name:      "undecodable seal",
			entries:   []*pb.Entry{{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{Header: []byte{0xde, 0xad}}}}},
			canonical: header.Hash(),
			want:      auditUnknown,
		},
		{
			name:      "seal carries the wrong height",
			entries:   sealedGeneration(t, testHeader(11, common.Hash{0x0b})),
			canonical: header.Hash(),
			want:      auditUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auditHeight(tc.entries, 9, tc.canonical); got != tc.want {
				t.Fatalf("verdict = %d, want %d", got, tc.want)
			}
		})
	}
}

// The last seal in a generation is the one that counts: a republished window
// can carry an earlier seal ahead of the one that stands.
func TestLastSealTakesTheFinalSeal(t *testing.T) {
	first := testHeader(4, common.Hash{0x01})
	last := testHeader(4, common.Hash{0x02})

	entries := append(sealedGeneration(t, first), sealedGeneration(t, last)...)

	seal := lastSeal(entries)
	if seal == nil {
		t.Fatal("no seal found")
	}

	header, err := decodeSealHeader(seal.GetHeader())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if header.Hash() != last.Hash() {
		t.Fatal("lastSeal returned an earlier seal")
	}

	if lastSeal(nil) != nil {
		t.Fatal("empty generation yielded a seal")
	}
}

func TestAuditDepthFallsBackToTheDefault(t *testing.T) {
	if got := (&auditor{}).auditDepth(); got != defaultAuditWindow {
		t.Fatalf("depth = %d, want %d", got, defaultAuditWindow)
	}

	if got := (&auditor{window: 7}).auditDepth(); got != 7 {
		t.Fatalf("depth = %d, want 7", got)
	}
}

func TestAuditWindowWithoutAHead(t *testing.T) {
	audit := &auditor{db: rawdb.NewMemoryDatabase(), chain: &headlessAuditChain{}}
	if _, _, _, ok := audit.windowToAudit(); ok {
		t.Fatal("window resolved without a canonical head")
	}
}

type headlessAuditChain struct{}

func (headlessAuditChain) CurrentBlock() *types.Header         { return nil }
func (headlessAuditChain) GetCanonicalHash(uint64) common.Hash { return common.Hash{} }
