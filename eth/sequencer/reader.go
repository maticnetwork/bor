package sequencer

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	// One Range page and the walk bound; anchored positions keep tails small.
	walkLimit    = 512
	maxWalkLoops = 128

	// Per-read deadline (the worker never waits on this — the
	// whole reconcile runs on the transport goroutine).
	tailReadTimeout = time.Second
)

var (
	errFoldDivergence = errors.New("byte-identical entry folds to a different head")
	errTailTooLong    = errors.New("tail walk exceeded bound")
)

// reader is the store-facing read layer: bounded walks that derive the head
// they land on, generation probes over the block index, and per-height
// generation fetches. It holds no publisher state — the same machinery that
// serves the publisher's reconciliation serves any consumer of the store,
// which is what a subscribing RPC node will be.
type reader struct {
	// mu guards cons, which a redial hot-swaps while the worker and
	// transport goroutines read through it.
	mu   sync.RWMutex
	cons pb.ConsumerServiceClient

	seed commitment.Head // the empty log's head, computed from the chain id

	// onRead runs after every successful store round trip. The publisher
	// hangs its reachability bookkeeping on it: the build-start read runs
	// every block, so it is what notices the store is back.
	onRead func()
}

func newReader(cons pb.ConsumerServiceClient, seed commitment.Head, onRead func()) *reader {
	return &reader{cons: cons, seed: seed, onRead: onRead}
}

func (r *reader) client() pb.ConsumerServiceClient {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.cons
}

func (r *reader) setClient(cons pb.ConsumerServiceClient) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cons = cons
}

// tailInfo summarizes one tail read: the store head S and what the tip
// looks like (classification inputs).
type tailInfo struct {
	s              commitment.Head
	tipOpen        bool
	tipOpenHeight  uint64
	tipOpenParent  common.Hash
	haveSeal       bool
	lastSealHeight uint64
	lastSealHash   common.Hash
	// lastSealHeader is the decoded header behind lastSealHash, kept so the
	// broadcast gate can consensus-verify a foreign seal before treating it
	// as ownership of the height. Nil when the seal was inferred, not read.
	lastSealHeader *types.Header
	// sealDecoded separates a seal this read actually decoded from one a
	// probe merely inferred. Only the former may drive sealed-tip
	// bookkeeping: an inferred "boundary" can be a live partial window,
	// and recording it as sealed told every backfill the height was owed
	// nothing — pinning an outage's partial delivery as a permanent hole.
	sealDecoded bool

	// window collects the trailing open window's entries for adoption,
	// capped at the journal byte bound; an over-cap window is not
	// collected and falls back to supersede.
	window      []*pb.Entry
	windowBytes int

	// explained is set when s was folded from the walked entries rather
	// than taken on the store's word. A head we cannot derive tells us
	// nothing about what produced it — in particular, whether some other
	// producer already has a live window at the height we are about to
	// open.
	explained bool

	// suffixOurs is set when the anchor rung's matcher absorbed every walked
	// entry in lockstep with the unconfirmed journal: the store's tail past
	// our anchor is a prefix of our own in-flight window, so the store's
	// window at the build height is the journal's. A mid-drain read lands
	// exactly here — the walk starts past the window's open, so tipOpen and
	// window stay unset even though the store holds nothing but our sequence.
	suffixOurs bool
}

// tryWalk runs one ladder rung; done=false falls through to the next rung:
// the position is unknown to the store (NOT_FOUND) or so far behind the tip
// that walking from it exceeds the bound — the probe and floor rungs anchor
// near the tip with a short walk instead. A recTerminal outcome is a fold
// divergence, which only a matched walk (an absorb hook that compares
// prefixes) can produce — the caller owns what terminal means.
func (r *reader) tryWalk(ctx context.Context, first *pb.RangeRequest, absorb func(*pb.Entry) error) (tailInfo, reconcileOutcome, bool) {
	info, err := r.walk(ctx, first, absorb)

	switch {
	case err == nil:
		return info, recOK, true
	case isNotFound(err):
		return tailInfo{}, recRetry, false
	case errors.Is(err, errTailTooLong):
		log.Warn("Sequencer tail read position too far behind, probing near tip")

		return tailInfo{}, recRetry, false
	case errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded:
		// This rung's budget slice expired mid-walk — the position is too
		// far behind to walk in time (gRPC surfaces the expiry as a status
		// error that does not wrap the context sentinel). Fall through:
		// the probe and floor rungs anchor near the tip with a short walk.
		log.Warn("Sequencer tail read rung out of budget, probing near tip")

		return tailInfo{}, recRetry, false
	case errors.Is(err, errFoldDivergence):
		return tailInfo{}, recTerminal, true
	default:
		log.Warn("Sequencer tail read", "err", err)

		return tailInfo{}, recRetry, true
	}
}

func (r *reader) probedWalk(ctx context.Context, from uint64) (tailInfo, reconcileOutcome, bool) {
	h, found, err := r.probeDown(ctx, from)
	if err != nil {
		return tailInfo{}, recRetry, true
	}

	if !found {
		return tailInfo{}, recRetry, false
	}

	return r.tryWalk(ctx, blockReq(h), nil)
}

// floorRead anchors on the earliest retained entry: an empty store yields
// the seed; a known floor height anchors an upward probe to the tip edge.
func (r *reader) floorRead(ctx context.Context) (tailInfo, reconcileOutcome) {
	resp, err := r.rangeOnce(ctx, &pb.RangeRequest{Limit: 1})
	if err != nil {
		return tailInfo{}, recRetry
	}

	if len(resp.GetEntries()) == 0 && resp.GetLive() {
		s, ok := headFrom(resp.GetNext())
		if !ok {
			return tailInfo{}, recRetry
		}

		// An empty store is the one head we can derive with no entries at
		// all: it must be the seed. Anything else is the store telling us
		// about history it did not show us.
		return tailInfo{s: s, explained: s == r.seed}, recOK
	}

	// An empty page that is not live is coherent (a trailing gateway not
	// yet at its tip): nothing to anchor a probe on — fall through to the
	// full floor walk, which loops pages to live.
	if info, out, done := r.probeAnchoredWalk(ctx, resp); done {
		return info, out
	}

	// Records carry no height (or the probe raced retention): full floor walk.
	info, err := r.walk(ctx, &pb.RangeRequest{}, nil)
	if err != nil {
		return tailInfo{}, recRetry
	}

	return info, recOK
}

// probeAnchoredWalk anchors a walk at the tip edge probed up from the floor
// entry's height; done reports whether that resolved the tail.
func (r *reader) probeAnchoredWalk(ctx context.Context, resp *pb.RangeResponse) (tailInfo, reconcileOutcome, bool) {
	if len(resp.GetEntries()) == 0 {
		return tailInfo{}, recRetry, false
	}

	h, ok := entryHeight(resp.GetEntries()[0])
	if !ok {
		return tailInfo{}, recRetry, false
	}

	edge, err := r.probeUp(ctx, h)
	if err != nil {
		return tailInfo{}, recRetry, false
	}

	return r.tryWalk(ctx, blockReq(edge), nil)
}

// walk reads Range pages from first until live, tracking the tip. An absorb
// hook (the anchor rung's journal matcher) inspects every entry and may end
// the walk with its error — fold-integrity checks live behind it.
func (r *reader) walk(ctx context.Context, first *pb.RangeRequest, absorb func(*pb.Entry) error) (tailInfo, error) {
	var info tailInfo

	f := r.newFolder(first)

	req := first
	req.Limit = walkLimit

	for loops := 0; ; loops++ {
		if loops >= maxWalkLoops {
			return info, errTailTooLong
		}

		resp, err := r.rangeOnce(ctx, req)
		if err != nil {
			return info, err
		}

		if err := foldPage(resp, f, &info, absorb); err != nil {
			return info, err
		}

		if s, ok := headFrom(resp.GetNext()); ok {
			info.s = s
			f.reached(s)
		}

		if resp.GetLive() {
			info.explained = f.ok

			return info, nil
		}

		req = &pb.RangeRequest{
			After: &pb.RangeRequest_Head{Head: resp.GetNext()},
			Limit: walkLimit,
		}
	}
}

// foldPage runs one page's entries through the absorb hook and the folder,
// tracking the tip; an absorb error ends the walk with it.
func foldPage(resp *pb.RangeResponse, f *folder, info *tailInfo, absorb func(*pb.Entry) error) error {
	for _, entry := range resp.GetEntries() {
		if absorb != nil {
			if err := absorb(entry); err != nil {
				return err
			}
		}

		f.fold(entry)
		trackTip(info, entry)
	}

	return nil
}

// folder derives the head the walk lands on instead of accepting the one the
// store reports. Without it a CAS proves only that we echoed back the value
// we were handed — the commitment stops being a check on shared history and
// becomes a sequence token.
//
// A walk from our own anchor has a verified base. A walk that starts at a
// block takes the base its open declares, which is weaker but still chains
// every entry after it — and, crucially, means we have read the window.
// A walk that starts anywhere else has no base and cannot explain anything.
type folder struct {
	cur      commitment.Head
	ok       bool
	awaiting bool // no base yet; the first entry must be an open that names one
	sawAny   bool
}

func (r *reader) newFolder(first *pb.RangeRequest) *folder {
	switch a := first.GetAfter().(type) {
	case *pb.RangeRequest_Head:
		if base, ok := headFrom(a.Head); ok {
			return &folder{cur: base, ok: true}
		}
	case nil:
		// A walk from the log's start: the base is the seed, which we
		// compute from the chain id without asking the store.
		return &folder{cur: r.seed, ok: true}
	}

	return &folder{ok: true, awaiting: true}
}

func (f *folder) fold(e *pb.Entry) {
	f.sawAny = true

	if !f.ok {
		return
	}

	if f.awaiting {
		open := e.GetBlockOpen()
		if open == nil {
			f.ok = false // mid-window start: nothing to fold from

			return
		}

		f.cur = commitment.Head(open.GetPrefixCommitment())
		f.awaiting = false
	}

	next, err := foldEntry(f.cur, e)
	if err != nil {
		f.ok = false

		return
	}

	f.cur = next
}

// reached compares the store's reported head for this page against the one
// the page's own entries produce.
func (f *folder) reached(s commitment.Head) {
	switch {
	case !f.ok:
		return
	case f.awaiting && !f.sawAny:
		// A block-anchored walk that returned nothing: the head value stays
		// underived, but the fact a boundary read needs — that no
		// generation follows the block we started at — is what the empty
		// page attests. Opening here cannot land on anyone's live window.
	case f.awaiting:
		// Entries came back, but not from a base we could establish, so
		// they summarize into a head we cannot account for.
		f.ok = false
	case f.cur != s:
		f.ok = false

		readHeadMismatch.Inc(1)
		log.Warn("Store head does not match the entries it returned",
			"reported", s, "folded", f.cur)
	}
}

func (r *reader) rangeOnce(ctx context.Context, req *pb.RangeRequest) (*pb.RangeResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, tailReadTimeout)
	defer cancel()

	resp, err := r.client().Range(cctx, req)
	if err == nil && r.onRead != nil {
		r.onRead()
	}

	return resp, err
}

// generation fetches the entries of the newest generation standing at a
// height — open, records, and seal when one closed it.
func (r *reader) generation(ctx context.Context, height uint64) ([]*pb.Entry, error) {
	resp, err := r.client().GetBlock(ctx, &pb.GetBlockRequest{BlockNumber: height})
	if err != nil {
		return nil, err
	}

	return resp.GetEntries(), nil
}

func trackTip(info *tailInfo, entry *pb.Entry) {
	switch k := entry.GetKind().(type) {
	case *pb.Entry_BlockOpen:
		info.tipOpen = true
		info.tipOpenHeight = k.BlockOpen.GetBlockNumber()
		info.tipOpenParent = common.BytesToHash(k.BlockOpen.GetParentHash())
		info.window = info.window[:0]
		info.windowBytes = 0
		collectWindow(info, entry)
	case *pb.Entry_Record:
		collectWindow(info, entry)
	case *pb.Entry_BlockSeal:
		header, err := decodeSealHeader(k.BlockSeal.GetHeader())
		if err != nil {
			log.Warn("Sequencer tail seal undecodable", "err", err)

			return
		}

		info.tipOpen = false
		info.haveSeal = true
		info.sealDecoded = true
		info.lastSealHeight = header.Number.Uint64()
		info.lastSealHash = header.Hash()
		info.lastSealHeader = header
		info.window = nil
		info.windowBytes = 0
	}
}

// collectWindow accumulates the trailing window's entries up to the journal
// byte bound; past it the window is dropped for good (unadoptable).
func collectWindow(info *tailInfo, entry *pb.Entry) {
	if info.windowBytes > journalMaxBytes {
		return
	}

	if info.windowBytes += proto.Size(entry); info.windowBytes > journalMaxBytes {
		info.window = nil

		return
	}

	if info.window != nil || entry.GetBlockOpen() != nil {
		info.window = append(info.window, entry)
	}
}

// probeDown finds the highest store-known height at or below from:
// exponential descent, then binary search.
func (r *reader) probeDown(ctx context.Context, from uint64) (uint64, bool, error) {
	h, step := from, uint64(1)

	// The lowest height the descent has proven unknown: the edge search
	// need not re-probe it. In the common build-start case (from unknown,
	// from-1 known) this leaves the binary search nothing to do at all —
	// re-probing cost a wasted store round trip on every block.
	unknown := from + 1

	for {
		known, err := r.blockKnown(ctx, h)
		if err != nil {
			return 0, false, err
		}

		if known {
			break
		}

		unknown = h

		// step starts at 1 and doubles, so this also covers h <= 1.
		if step >= h {
			return 0, false, nil
		}

		h -= step
		step *= 2
	}

	return r.binarySearchEdge(ctx, h, unknown)
}

// probeUp finds the highest store-known height starting from known floor
// height h0.
func (r *reader) probeUp(ctx context.Context, h0 uint64) (uint64, error) {
	lo, step := h0, uint64(1)

	for {
		known, err := r.blockKnown(ctx, lo+step)
		if err != nil {
			return 0, err
		}

		if !known {
			break
		}

		lo += step
		step *= 2
	}

	edge, _, err := r.binarySearchEdge(ctx, lo, lo+step)

	return edge, err
}

// binarySearchEdge returns the highest known height in [lo, hi) given lo is
// known and hi is unknown (or the exclusive bound).
func (r *reader) binarySearchEdge(ctx context.Context, lo, hi uint64) (uint64, bool, error) {
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2

		known, err := r.blockKnown(ctx, mid)
		if err != nil {
			return 0, false, err
		}

		if known {
			lo = mid
		} else {
			hi = mid
		}
	}

	return lo, true, nil
}

func (r *reader) blockKnown(ctx context.Context, h uint64) (bool, error) {
	_, err := r.rangeOnce(ctx, &pb.RangeRequest{
		After: &pb.RangeRequest_Block{Block: h},
		Limit: 1,
	})

	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

func isNotFound(err error) bool {
	return status.Code(err) == codes.NotFound
}

// headFrom converts wire bytes to a Head when the length matches; short
// or oversized bytes are not a position.
func headFrom(b []byte) (commitment.Head, bool) {
	if len(b) != len(commitment.Head{}) {
		return commitment.Head{}, false
	}

	return commitment.Head(b), true
}

func headReq(h commitment.Head) *pb.RangeRequest {
	return &pb.RangeRequest{After: &pb.RangeRequest_Head{Head: h.Bytes()}}
}

func blockReq(h uint64) *pb.RangeRequest {
	return &pb.RangeRequest{After: &pb.RangeRequest_Block{Block: h}}
}
