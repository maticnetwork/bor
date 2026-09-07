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

// The counters are the pass's report to its caller; without asserting them a
// mutation to any of the tallies goes unnoticed.
func TestAuditSummaryCounts(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	chain, sealed := auditFixture(t, 10)

	sealed[7] = testHeader(7, common.Hash{0xee}) // mismatch
	delete(sealed, 8)                            // NotFound
	chain.hashes[9] = common.Hash{}              // uncomparable: no canonical hash

	if err := rawdb.WritePreconfAuditedThrough(db, 5); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: db, chain: chain, fetch: fetchFrom(t, sealed)}
	summary, err := audit.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Heights 6..10: five walked, one mismatch at 7, one uncomparable at 9.
	if summary.walked != 5 {
		t.Fatalf("walked = %d, want 5", summary.walked)
	}
	if summary.mismatch != 1 {
		t.Fatalf("mismatch = %d, want 1", summary.mismatch)
	}
	if summary.unknown != 1 {
		t.Fatalf("unknown = %d, want 1", summary.unknown)
	}
	if summary.skippedTo != 0 {
		t.Fatalf("skippedTo = %d, want 0 for a window inside the depth", summary.skippedTo)
	}
}

func TestRecordVerdict(t *testing.T) {
	cases := []struct {
		name     string
		verdict  auditVerdict
		mismatch uint64
		unknown  uint64
		recorded bool
	}{
		{name: "mismatch", verdict: auditMismatch, mismatch: 1, recorded: true},
		{name: "unknown", verdict: auditUnknown, unknown: 1},
		{name: "match", verdict: auditMatch},
		{name: "no seal", verdict: auditNoSeal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			audit := &auditor{db: db}
			summary := auditSummary{}

			audit.recordVerdict(42, tc.verdict, &summary)

			if summary.mismatch != tc.mismatch {
				t.Fatalf("mismatch = %d, want %d", summary.mismatch, tc.mismatch)
			}
			if summary.unknown != tc.unknown {
				t.Fatalf("unknown = %d, want %d", summary.unknown, tc.unknown)
			}

			records := rawdb.ReadInvalidPreconfsInRange(db, 42, 42)
			if tc.recorded && len(records) != 1 {
				t.Fatalf("records = %+v, want one", records)
			}
			if !tc.recorded && len(records) != 0 {
				t.Fatalf("records = %+v, want none", records)
			}
		})
	}
}

// The window boundary: exactly the depth is walked whole, one more truncates.
func TestAuditWindowDepthBoundary(t *testing.T) {
	cases := []struct {
		name          string
		watermark     uint64
		wantFrom      uint64
		wantSkippedTo uint64
	}{
		{name: "exactly the depth", watermark: 90, wantFrom: 91, wantSkippedTo: 0},
		{name: "one past the depth", watermark: 89, wantFrom: 91, wantSkippedTo: 90},
		{name: "well past the depth", watermark: 1, wantFrom: 91, wantSkippedTo: 90},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := rawdb.NewMemoryDatabase()
			chain, _ := auditFixture(t, 100)

			if err := rawdb.WritePreconfAuditedThrough(db, tc.watermark); err != nil {
				t.Fatalf("seed watermark: %v", err)
			}

			audit := &auditor{db: db, chain: chain, window: 10}
			from, through, skippedTo, ok := audit.windowToAudit()
			if !ok {
				t.Fatal("no window to audit")
			}

			if from != tc.wantFrom || through != 100 || skippedTo != tc.wantSkippedTo {
				t.Fatalf("window = [%d,%d] skippedTo %d, want [%d,100] skippedTo %d",
					from, through, skippedTo, tc.wantFrom, tc.wantSkippedTo)
			}
		})
	}
}

// The watermark is written at the checkpoint interval, so a pass that is
// interrupted repeatedly still converges instead of re-walking its prefix.
func TestAuditCheckpointsMidPass(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	through := uint64(auditCheckpointInterval) + 10
	chain, sealed := auditFixture(t, through)

	if err := rawdb.WritePreconfAuditedThrough(db, 0); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	checkpoint := uint64(auditCheckpointInterval)
	var atCheckpoint uint64
	seen := false

	audit := &auditor{db: db, chain: chain, window: through, fetch: func(ctx context.Context, height uint64) ([]*pb.Entry, error) {
		if height == checkpoint+1 && !seen {
			seen = true
			atCheckpoint, _ = rawdb.ReadPreconfAuditedThrough(db)
		}

		return fetchFrom(t, sealed)(ctx, height)
	}}

	if _, err := audit.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !seen {
		t.Fatalf("the pass never reached height %d", checkpoint+1)
	}
	if atCheckpoint != checkpoint {
		t.Fatalf("watermark at the checkpoint = %d, want %d", atCheckpoint, checkpoint)
	}
}

// persist routes through the consumer's guarded writer when one is set, and
// writes directly otherwise.
func TestAuditPersistUsesTheInjectedWriter(t *testing.T) {
	db := rawdb.NewMemoryDatabase()

	var advanced []uint64
	audit := &auditor{db: db, advance: func(number uint64) { advanced = append(advanced, number) }}

	audit.persist(11)

	if len(advanced) != 1 || advanced[0] != 11 {
		t.Fatalf("advance calls = %v, want [11]", advanced)
	}
	if _, ok := rawdb.ReadPreconfAuditedThrough(db); ok {
		t.Fatal("persist wrote directly while a writer was injected")
	}
}

// persist holds at the stored height rather than lowering it.
func TestAuditPersistNeverLowersTheWatermark(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	audit := &auditor{db: db}

	audit.persist(20)
	audit.persist(20)
	audit.persist(19)

	if got, _ := rawdb.ReadPreconfAuditedThrough(db); got != 20 {
		t.Fatalf("watermark = %d, want 20", got)
	}
}

func TestRecordSkippedWindowOnlyWritesAGap(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	audit := &auditor{db: db}

	audit.recordSkippedWindow(0, 1, 10)
	if _, ok := rawdb.ReadPreconfUnauditedThrough(db); ok {
		t.Fatal("an untruncated window recorded a gap")
	}

	audit.recordSkippedWindow(7, 8, 10)
	if got, ok := rawdb.ReadPreconfUnauditedThrough(db); !ok || got != 7 {
		t.Fatalf("unaudited mark = (%d, %v), want (7, true)", got, ok)
	}
}

// failingWriteDB fails every write, direct or batched, so the pass's
// write-error paths run.
type failingWriteDB struct {
	ethdb.Database
}

func (failingWriteDB) Put([]byte, []byte) error { return errWriteRefused }

func (d failingWriteDB) NewBatch() ethdb.Batch {
	return failingBatch{Batch: d.Database.NewBatch()}
}

type failingBatch struct {
	ethdb.Batch
}

func (failingBatch) Write() error { return errWriteRefused }

var errWriteRefused = errors.New("write refused")

// A database that refuses writes must not stop the walk: the pass logs and
// keeps auditing, because the alternative is losing the whole window.
func TestAuditSurvivesWriteFailures(t *testing.T) {
	chain, sealed := auditFixture(t, 100)
	sealed[95] = testHeader(95, common.Hash{0xee})

	backing := rawdb.NewMemoryDatabase()
	if err := rawdb.WritePreconfAuditedThrough(backing, 1); err != nil {
		t.Fatalf("seed watermark: %v", err)
	}

	audit := &auditor{db: failingWriteDB{backing}, chain: chain, fetch: fetchFrom(t, sealed), window: 10}
	summary, err := audit.run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The walk still covered the window and still counted the mismatch, even
	// though none of the three writes it attempted could land.
	if summary.walked != 10 {
		t.Fatalf("walked = %d, want 10", summary.walked)
	}
	if summary.mismatch != 1 {
		t.Fatalf("mismatch = %d, want 1", summary.mismatch)
	}
	if got, _ := rawdb.ReadPreconfAuditedThrough(backing); got != 1 {
		t.Fatalf("watermark = %d, want it unchanged at 1 when writes fail", got)
	}
}
