package sequencer

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// unobservedMismatchReason marks a height whose stored generation never became
// canonical and which this node never served a preconfirmation from — the
// audit found it after the fact. The other reasons all mean a preconfirmation
// was published to callers and then invalidated, which is a stronger claim.
const unobservedMismatchReason = "unobserved_mismatch"

// defaultAuditWindow bounds one pass. GetBlock returns a height's whole
// generation, transaction records included, so a wide walk pays for payloads
// the seal comparison never reads; roughly an hour of blocks covers a restart
// while staying cheap. Heights older than the window are recorded as
// unaudited rather than walked.
const defaultAuditWindow = 3600

// auditReadTimeout bounds one per-height store read.
const auditReadTimeout = 5 * time.Second

// auditCheckpointInterval persists progress mid-pass so a node that keeps
// restarting still converges instead of re-walking the same prefix forever.
const auditCheckpointInterval = 256

// fetchGeneration reads the latest generation stored at a height. A NotFound
// error means the store holds nothing there.
type fetchGeneration func(ctx context.Context, height uint64) ([]*pb.Entry, error)

// auditChain is the canonical-chain surface the audit needs: it compares
// stored seals against canonical hashes and never executes anything, so it
// needs no state access.
type auditChain interface {
	CurrentBlock() *types.Header
	GetCanonicalHash(number uint64) common.Hash
}

type auditor struct {
	db      ethdb.Database
	chain   auditChain
	fetch   fetchGeneration
	window  uint64
	advance func(uint64)
}

type auditVerdict int

const (
	auditMatch    auditVerdict = iota // the stored seal is the canonical block
	auditNoSeal                       // the generation was never sealed: nothing was final
	auditMismatch                     // the store sealed something else
	auditUnknown                      // not comparable (no canonical hash, undecodable seal)
)

type auditSummary struct {
	from      uint64
	through   uint64
	walked    uint64
	mismatch  uint64
	unknown   uint64
	skippedTo uint64 // highest height left unaudited by the window bound
}

// windowToAudit reports the height range this pass should walk. ok is false when
// there is nothing to do, which includes the first run on a node that has
// never audited: that seeds the watermark at the current head rather than
// walking backwards from an arbitrary point.
func (a *auditor) windowToAudit() (from, through, skippedTo uint64, ok bool) {
	head := a.chain.CurrentBlock()
	if head == nil || head.Number == nil {
		return 0, 0, 0, false
	}
	through = head.Number.Uint64()

	watermark, stored, err := rawdb.ReadPreconfAuditedThrough(a.db)
	if err != nil {
		// Absence seeds the watermark at the head; an unreadable watermark
		// must not, or a failed read would mark the whole window audited and
		// the monotonic writes would never let it back.
		log.Warn("Sequence store audit watermark unreadable", "err", err)

		return 0, 0, 0, false
	}
	if !stored {
		a.persist(through)
		log.Info("Sequence store audit watermark seeded", "height", through)

		return 0, 0, 0, false
	}
	if watermark >= through {
		return 0, 0, 0, false
	}

	from = watermark + 1
	if window := a.auditDepth(); through-from+1 > window {
		// Audit the most recent window and declare the rest unaudited:
		// callers care whether a recent preconfirmation held, and the older
		// end is the part they are least likely to ask about.
		skippedTo = through - window
		from = skippedTo + 1
	}

	return from, through, skippedTo, true
}

func (a *auditor) auditDepth() uint64 {
	if a.window == 0 {
		return defaultAuditWindow
	}
	return a.window
}

// run walks the unaudited window and records the heights where the store's
// final generation disagrees with the canonical chain.
func (a *auditor) run(ctx context.Context) (auditSummary, error) {
	from, through, skippedTo, ok := a.windowToAudit()
	if !ok {
		return auditSummary{}, nil
	}

	summary := auditSummary{from: from, through: through, skippedTo: skippedTo}
	a.recordSkippedWindow(skippedTo, from, through)

	log.Info("Auditing sequence store against canonical chain", "from", from, "through", through)

	for height := from; height <= through; height++ {
		if err := ctx.Err(); err != nil {
			a.persist(height - 1)
			return summary, err
		}

		if err := a.auditHeightInto(ctx, height, &summary); err != nil {
			a.persist(height - 1)
			return summary, err
		}

		if (height-from+1)%auditCheckpointInterval == 0 {
			a.persist(height)
		}
	}

	a.persist(through)
	log.Info("Sequence store audit complete", "from", from, "through", through,
		"walked", summary.walked, "mismatched", summary.mismatch, "uncomparable", summary.unknown)

	return summary, nil
}

// recordSkippedWindow marks the heights the depth bound left out, so an empty
// invalidation range there reads as unknown rather than clean.
func (a *auditor) recordSkippedWindow(skippedTo, from, through uint64) {
	if skippedTo == 0 {
		return
	}

	if err := rawdb.WritePreconfUnauditedThrough(a.db, skippedTo); err != nil {
		log.Warn("Failed to record unaudited sequence store window", "through", skippedTo, "err", err)
	}

	log.Warn("Sequence store audit window truncated", "unaudited", skippedTo, "from", from, "through", through)
}

// auditHeightInto compares one height and folds the verdict into summary. An
// error is a store read that failed, which ends the pass; a height the store
// does not hold is not an error.
func (a *auditor) auditHeightInto(ctx context.Context, height uint64, summary *auditSummary) error {
	entries, err := a.fetch(ctx, height)
	switch {
	case err == nil:
	case isNotFound(err):
		// The store holds nothing here: either it was down, or the producer
		// never published. Nothing was promised, so nothing to invalidate.
		summary.walked++

		return nil
	default:
		return err
	}

	summary.walked++
	a.recordVerdict(height, auditHeight(entries, height, a.chain.GetCanonicalHash(height)), summary)

	return nil
}

func (a *auditor) recordVerdict(height uint64, verdict auditVerdict, summary *auditSummary) {
	switch verdict {
	case auditMismatch:
		summary.mismatch++

		if err := rawdb.WriteInvalidPreconf(a.db, height, unobservedMismatchReason); err != nil {
			log.Warn("Failed to record unobserved preconfirmation mismatch", "number", height, "err", err)
		}
	case auditUnknown:
		summary.unknown++
	case auditMatch, auditNoSeal:
	}
}

// persist raises the watermark. advance is injected so the consumer can route
// every write through one mutex-guarded path; a bare auditor (tests) writes
// directly.
func (a *auditor) persist(number uint64) {
	if a.advance != nil {
		a.advance(number)
		return
	}
	current, stored, err := rawdb.ReadPreconfAuditedThrough(a.db)
	if err != nil {
		log.Warn("Sequence store audit watermark unreadable; holding", "height", number, "err", err)

		return
	}
	if stored && current >= number {
		return
	}

	if err := rawdb.WritePreconfAuditedThrough(a.db, number); err != nil {
		log.Warn("Failed to persist sequence store audit watermark", "height", number, "err", err)
	}
}

// auditHeight compares the seal a generation ends with against the canonical
// hash at that height.
func auditHeight(entries []*pb.Entry, height uint64, canonical common.Hash) auditVerdict {
	seal := lastSeal(entries)
	if seal == nil {
		return auditNoSeal
	}
	if canonical == (common.Hash{}) {
		return auditUnknown
	}

	header, err := decodeSealHeader(seal.GetHeader())
	if err != nil {
		log.Warn("Sequence store audit found an undecodable seal", "number", height, "err", err)
		return auditUnknown
	}
	if header.Number == nil || header.Number.Uint64() != height {
		log.Warn("Sequence store audit found a seal at the wrong height", "number", height, "sealed", header.Number)
		return auditUnknown
	}
	if header.Hash() != canonical {
		return auditMismatch
	}

	return auditMatch
}

func lastSeal(entries []*pb.Entry) *pb.BlockSeal {
	for i := len(entries) - 1; i >= 0; i-- {
		if seal := entries[i].GetBlockSeal(); seal != nil {
			return seal
		}
	}

	return nil
}

// SetAuditWindow bounds how many blocks one audit pass walks. Zero keeps the
// package default. Call it before Start; the value is read by the audit loop.
func (c *Consumer) SetAuditWindow(window uint64) {
	c.auditWindow = window
}

// requestAudit asks for an audit pass without waiting for one. The trigger
// holds a single slot: a pass already queued covers everything a second
// request would, since the window is recomputed when the pass starts.
func (c *Consumer) requestAudit() {
	select {
	case c.auditTrigger <- struct{}{}:
	default:
	}
}

// auditLoop runs audit passes off the session loop, so closing a gap never
// delays a reconnect and the node keeps serving preconfirmations at the tip
// while the walk runs.
func (c *Consumer) auditLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.auditTrigger:
			c.runAuditPass(ctx)
		}
	}
}

func (c *Consumer) runAuditPass(ctx context.Context) {
	audit := &auditor{db: c.chain.DB(), chain: c.chain, window: c.auditWindow, advance: c.advanceAudited}

	// Resolve the window before building a client: the common case is nothing
	// to audit and a trigger fires on every session retry. grpc.NewClient is
	// lazy, so this saves a client and its teardown rather than a connection,
	// and it keeps a bad endpoint from logging once per retry while there is
	// no work to do. run recomputes the window, so removing this changes
	// nothing observable in-process — there is deliberately no test for it.
	from, through, _, ok := audit.windowToAudit()
	if !ok {
		return
	}

	conn, err := grpc.NewClient(c.endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(pendingInputLimit+1024*1024)))
	if err != nil {
		log.Warn("Sequence store audit could not dial", "from", from, "through", through, "err", err)
		return
	}

	defer func() {
		if cerr := conn.Close(); cerr != nil {
			log.Warn("Sequence store audit connection close", "err", cerr)
		}
	}()

	client := pb.NewConsumerServiceClient(conn)
	audit.fetch = func(ctx context.Context, height uint64) ([]*pb.Entry, error) {
		readCtx, cancel := context.WithTimeout(ctx, auditReadTimeout)
		defer cancel()

		resp, err := client.GetBlock(readCtx, &pb.GetBlockRequest{BlockNumber: height})
		if err != nil {
			return nil, err
		}

		return resp.GetEntries(), nil
	}

	if _, err := audit.run(ctx); err != nil && ctx.Err() == nil {
		log.Warn("Sequence store audit stopped early", "err", err)
	}
}

// advanceAudited raises the persisted audit watermark. It never lowers it: the
// audit pass and the canonical-head path both advance it, and a pass that
// finishes after the live path has moved on must not rewind the mark.
func (c *Consumer) advanceAudited(number uint64) {
	c.auditMu.Lock()
	defer c.auditMu.Unlock()

	db := c.chain.DB()
	current, stored, err := rawdb.ReadPreconfAuditedThrough(db)
	if err != nil {
		log.Warn("Sequence store audit watermark unreadable; holding", "height", number, "err", err)

		return
	}
	if stored && current >= number {
		return
	}

	if err := rawdb.WritePreconfAuditedThrough(db, number); err != nil {
		log.Warn("Failed to persist sequence store audit watermark", "height", number, "err", err)
	}
}
