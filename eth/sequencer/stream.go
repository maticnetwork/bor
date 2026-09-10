package sequencer

import (
	"context"
	"sync/atomic"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/log"
)

type streamEnd int

const (
	endCtx streamEnd = iota
	endTerminal
	endTransport
	endStale
	endIdle
	endWatch
)

// idleReconcileInterval paces the idle catch-up read: a publisher with
// nothing in flight (a non-producer between spans) re-anchors near the
// store tip so a takeover build's bounded read starts close — from a
// stale anchor the read budget dies walking history and the adopt is
// missed. Var for tests.
var idleReconcileInterval = 30 * time.Second

// heldWatchInterval paces the tail re-reads taken while a sticky hold gates
// a live build. Short enough to catch a competing producer inside one block
// interval; the reads only happen while held, which is rare. Var for tests.
var heldWatchInterval = 400 * time.Millisecond

// maxInflightEntries bounds what one session keeps sent-but-unacked. Deep
// enough to saturate a group-committing ingress, shallow enough that the
// trailing entry's ack latency stays a queue drain, not a backlog: an
// unbounded sender once buried the store under 23k entries of its own
// republish churn, tripped the stall watchdog on the self-made backlog, and
// produced blind while flagged unreachable. With the cap, a stall means the
// store stopped — nobody is being promised anything — which is the only
// state where producing without a verdict is safe. Var for tests.
var maxInflightEntries = 2048

type streamResult struct {
	reason streamEnd
	// progressed reports whether any entry was confirmed this session — it
	// resets the contention backoff streak.
	progressed bool
}

type ackResult struct {
	status pb.AckStatus
	err    error
}

type sent struct {
	item  journalItem
	batch []journalItem
	at    time.Time
}

const maxTransactionsPerPublishedRecord = 64

// maxMessageBytes mirrors the store's Redpanda max.message.bytes: a larger
// record draws a permanent MESSAGE_TOO_LARGE on produce and fails the publisher.
const maxMessageBytes = 1 << 20

// recordSizeReserve leaves headroom for per-transaction protobuf framing and the
// prefix commitment, so a capped record's proto.Size stays under what the ingress
// accepts (max.message.bytes less its own recordFraming).
const recordSizeReserve = 4 * 1024

const maxRecordBytes = maxMessageBytes - recordSizeReserve

const publishedRecordBatchDelay = 5 * time.Millisecond

// stallTracker watches ack progress from outside the send loop: a hung
// store fills the stream's flow-control window and blocks Send, so no
// in-loop timer can fire — the watchdog cancels the session context
// instead, which unblocks both Send and Recv.
type stallTracker struct {
	inflight atomic.Int64
	lastAck  atomic.Int64 // unix nanos of the last ack (or session start)
}

func newStallTracker() *stallTracker {
	t := &stallTracker{}
	t.lastAck.Store(time.Now().UnixNano()) // session start counts as progress

	return t
}

// sent restarts the progress clock when it opens a fresh wait (inflight 0→1):
// the deadline must measure how long this batch of acks has been outstanding,
// not the idle gap before it. Without the reset, the first entry sent after a
// quiet stretch longer than the deadline reads as stalled the instant it goes
// out — the store never gets its window to ack — and the watchdog resets a
// healthy low-load stream. Sends that pipeline onto an existing wait leave the
// clock alone, so a genuinely hung store still trips.
func (s *stallTracker) sent() {
	if s.inflight.Add(1) == 1 {
		s.lastAck.Store(time.Now().UnixNano())
	}
}

func (s *stallTracker) acked() { s.inflight.Add(-1); s.lastAck.Store(time.Now().UnixNano()) }

func (s *stallTracker) stalled(deadline time.Duration) bool {
	return s.inflight.Load() > 0 &&
		time.Since(time.Unix(0, s.lastAck.Load())) > deadline
}

// watch cancels the session once acks stall past the deadline; it exits
// with the session. The deadline arrives as a parameter so the goroutine
// never reads publisher state it could outlive.
func (s *stallTracker) watch(ctx context.Context, cancel context.CancelFunc, deadline time.Duration) {
	tick := time.NewTicker(deadline / 4)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.stalled(deadline) {
				log.Warn("Sequencer ack stall, reconnecting",
					"inflight", s.inflight.Load(),
					"waited", time.Since(time.Unix(0, s.lastAck.Load())))
				cancel()

				return
			}
		}
	}
}

// runStream runs one stream session: it resends unacked journal items,
// forwards new ones as the worker appends them, and retires items as acks
// arrive (the store acks in entry order, so the oldest in-flight item owns
// each ack).
func (p *Publisher) runStream(ctx context.Context) streamResult {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := p.pub.Publish(sctx)
	if err != nil {
		return streamResult{reason: endTransport}
	}

	acks := make(chan ackResult)

	go recvAcks(sctx, stream, acks)

	// A hung store stalls acks without erroring the stream; the watchdog
	// forces a reconnect whose resend converges once the store wakes.
	tracker := newStallTracker()

	go tracker.watch(sctx, cancel, ackStallTimeout)

	// A resent entry may already have been applied (the previous stream
	// broke after the append, before the ack) — its STALE lands in
	// reconciliation, whose anchored byte-match retires exactly the
	// applied prefix. No local guessing: retiring on a STALE without
	// store confirmation can mint a fictional frontier when the earlier
	// send itself had STALEd.
	var inflight []sent

	cursor, ok := p.sendAfter(stream, p.ackedSeqSnapshot(), &inflight, tracker)
	if !ok {
		return streamResult{reason: endStale}
	}

	return p.streamLoop(ctx, sctx, stream, acks, tracker, cursor, &inflight)
}

// streamLoop is the session's steady state: wake on appends, acks, and
// ticks, send whatever became sendable, and report why the session ended.
func (p *Publisher) streamLoop(ctx, sctx context.Context, stream pb.PublisherService_PublishClient,
	acks chan ackResult, tracker *stallTracker, cursor uint64, inflight *[]sent,
) streamResult {
	progressed := false

	idle := time.NewTicker(idleReconcileInterval)
	defer idle.Stop()

	// A sticky hold stops our writes, which also stops the STALEs that
	// would otherwise bring fresh tail reads — leaving us blind to the
	// competing producer for the rest of the block. Keep looking on a
	// short cadence while held: reads are harmless, and seeing the
	// competitor is what lets the build follow their sequence.
	watch := time.NewTicker(heldWatchInterval)
	defer watch.Stop()

	ok := false

	for {
		select {
		case <-ctx.Done():
			return streamResult{reason: endCtx, progressed: progressed}
		case <-sctx.Done():
			// Watchdog cancel; recvAcks may lose the race to deliver the
			// recv error, so the session ends here.
			return streamResult{reason: endTransport, progressed: progressed}
		case <-idle.C:
			if p.quiet() {
				return streamResult{reason: endIdle, progressed: progressed}
			}
		case <-watch.C:
			if p.heldMidBuild() {
				return streamResult{reason: endWatch, progressed: progressed}
			}
		case <-p.wake:
			if p.nextSendableIsRecord(cursor) {
				timer := time.NewTimer(publishedRecordBatchDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return streamResult{reason: endCtx, progressed: progressed}
				case <-sctx.Done():
					timer.Stop()
					return streamResult{reason: endTransport, progressed: progressed}
				case <-timer.C:
				}
			}
		case ack := <-acks:
			res, done := p.handleAck(ack, inflight)
			if done {
				res.progressed = progressed

				return res
			}

			progressed = true // an OK ack retired an entry

			tracker.acked()
		}

		// One send site serves every wake source: a worker append, an ack
		// freeing a cap slot, or a tick that found nothing to end on.
		if cursor, ok = p.sendAfter(stream, cursor, inflight, tracker); !ok {
			return streamResult{reason: endStale, progressed: progressed}
		}
	}
}

func (p *Publisher) nextSendableIsRecord(cursor uint64) bool {
	items, _, ok := p.sendableAfter(cursor, 1)
	return ok && len(items) != 0 && items[0].kind == entryRecord
}

// sendableAfter snapshots, under the lock, the journal items past cursor
// that may go out now: covered by the journal, at or below the send
// ceiling (a draining seal flush below the ceiling always finishes
// delivering), and at most limit items. acked rides along so the send loop
// can skip adoption-confirmed entries. ok is false when eviction has
// created a gap past cursor — reconciliation must decide.
func (p *Publisher) sendableAfter(cursor uint64, limit int) (items []journalItem, acked uint64, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// The coverage check runs on every call, before any early-out: an
	// eviction gap is the drain cycle's reconcile trigger, and a call that
	// skips it (a full pipeline, a gated suffix) can wedge a backfill with
	// nothing else scheduled to notice — a devnet stranded a whole outage
	// window exactly that way.
	view, covered := p.journal.after(cursor)
	if !covered {
		return nil, 0, false
	}

	// A hold at or below the cursor gates everything past it — the common
	// sticky-hold shape — and seqs ascend, so answer without walking.
	if p.hold.active() && p.hold.after <= cursor {
		return nil, p.ackedSeq, true
	}

	// Truncate before trimming the held tail: gated items are always a
	// suffix, so the result is the same and the walk is bounded by the
	// cap instead of the window size.
	if len(view) > limit {
		view = view[:max(limit, 0)]
	}

	for len(view) > 0 && p.hold.gates(view[len(view)-1].seq) {
		view = view[:len(view)-1]
	}

	// Copy under the lock: view aliases the journal's backing array, which
	// worker-side evictions compact in place while Send blocks — iterating
	// the alias unlocked is a data race.
	return append([]journalItem(nil), view...), p.ackedSeq, true
}

// sendAfter sends the sendable journal items past cursor, up to the
// in-flight cap, returning the new cursor. ok is false on an eviction gap.
// Acks free capacity, and the send after each ack refills — without that
// pairing a capped drain with an idle worker would send one window and
// stop, since nothing else wakes the loop. A full pipeline still calls
// down: the coverage check must run even when nothing can be sent.
func (p *Publisher) sendAfter(stream pb.PublisherService_PublishClient, cursor uint64, inflight *[]sent, tracker *stallTracker) (uint64, bool) {
	items, acked, ok := p.sendableAfter(cursor, maxInflightEntries-len(*inflight))
	if !ok {
		return cursor, false
	}

	for index := 0; index < len(items); {
		item := items[index]
		// An adoption can mark entries acked between
		// this session's sends: the store already holds them, and sending
		// them again guarantees a STALE (the adopter is silent).
		if item.seq <= acked {
			cursor = item.seq
			index++
			continue
		}

		entry, batch := coalescePublishedRecord(items[index:], acked)
		if err := stream.Send(&pb.PublishRequest{Entry: entry}); err != nil {
			// The recv side reports the definitive error; keep the cursor so
			// nothing is skipped.
			break
		}

		last := batch[len(batch)-1]
		*inflight = append(*inflight, sent{item: item, batch: batch, at: time.Now()})
		tracker.sent()
		cursor = last.seq
		index += len(batch)

		publishedCounter.Inc(1)
	}

	return cursor, true
}

func coalescePublishedRecord(items []journalItem, acked uint64) (*pb.Entry, []journalItem) {
	first := items[0]
	record := first.entry.GetRecord()
	if record == nil {
		return first.entry, []journalItem{first}
	}

	batch := make([]journalItem, 0, maxTransactionsPerPublishedRecord)
	rawTransactions := make([][]byte, 0, maxTransactionsPerPublishedRecord)
	var inputBytes uint64
	for _, item := range items {
		candidate := item.entry.GetRecord()
		if item.seq <= acked || candidate == nil || item.height != first.height ||
			len(rawTransactions)+len(candidate.GetTransactions()) > maxTransactionsPerPublishedRecord {
			break
		}
		candidateBytes := uint64(0)
		for _, raw := range candidate.GetTransactions() {
			candidateBytes += uint64(len(raw))
		}
		// A lone transaction over the cap still ships via the single-record
		// return below; the store judges it rather than losing it here.
		if inputBytes+candidateBytes > uint64(maxRecordBytes) {
			break
		}
		batch = append(batch, item)
		rawTransactions = append(rawTransactions, candidate.GetTransactions()...)
		inputBytes += candidateBytes
	}
	if len(batch) == 0 {
		return first.entry, []journalItem{first}
	}
	if len(batch) == 1 {
		return first.entry, batch
	}
	return &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		Transactions:     rawTransactions,
		PrefixCommitment: record.GetPrefixCommitment(),
	}}}, batch
}

// handleAck retires or rejects the oldest in-flight entry. done=true ends
// the session with the returned result; done=false means the ack was OK
// and an entry was retired — the session progressed.
func (p *Publisher) handleAck(ack ackResult, inflight *[]sent) (streamResult, bool) {
	if ack.err != nil {
		return streamResult{reason: endTransport}, true
	}

	if len(*inflight) == 0 {
		p.fail("ack without a pending entry")

		return streamResult{reason: endTerminal}, true
	}

	first := (*inflight)[0]
	*inflight = (*inflight)[1:]

	switch ack.status {
	case pb.AckStatus_ACK_STATUS_OK:
		items := first.batch
		if len(items) == 0 {
			items = []journalItem{first.item}
		}
		for _, item := range items {
			if p.retire(item, first.at) {
				return streamResult{reason: endWatch}, true
			}
		}

		return streamResult{}, false
	case pb.AckStatus_ACK_STATUS_STALE_COMMITMENT:
		p.markGateLost(first.item)

		// Even for a resend that may have been applied before its stream
		// broke, reconciliation decides: the anchored byte-match retires
		// exactly the applied prefix, without inventing a frontier.
		return streamResult{reason: endStale}, true
	case pb.AckStatus_ACK_STATUS_RATE_LIMITED:
		// The rejected entry never advanced the store head and everything
		// pipelined behind it failed the head check too — a fresh stream
		// resending in order is exact.
		return streamResult{reason: endTransport}, true
	default:
		// Terminal but safe: no fence, no resend. A MALFORMED here means the
		// store's max.message.bytes is below our record cap.
		p.fail("store rejected entry", "status", ack.status, "seq", first.item.seq)

		return streamResult{reason: endTerminal}, true
	}
}

// quiet reports a lineage with nothing in flight: either fully
// drained, or every unacked entry gated behind the send ceiling (a parked
// adopter's dead buffer). Both states leave the idle catch-up read free —
// requiring a full drain would let a held buffer starve the catch-up all
// span, leaving a stale anchor exactly when a takeover needs a fresh one.
func (p *Publisher) quiet() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.unackedLocked() == 0 {
		return true
	}

	if !p.hold.active() {
		return false
	}

	for _, it := range p.journal.items {
		if it.seq > p.ackedSeq && !p.hold.gates(it.seq) {
			return false // sendable in-flight work: not quiet
		}
	}

	return true
}

func (p *Publisher) ackedSeqSnapshot() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.ackedSeq
}

func recvAcks(ctx context.Context, stream pb.PublisherService_PublishClient, acks chan<- ackResult) {
	for {
		resp, err := stream.Recv()

		result := ackResult{err: err}
		if err == nil {
			result.status = resp.GetStatus()
		}

		select {
		case acks <- result:
		case <-ctx.Done():
			return
		}

		if err != nil {
			return
		}
	}
}

// retire marks one item store-confirmed. It reports whether the retired
// item was the backfill batch's last entry with blocks still pending — the
// cue for the caller to reconcile the next batch in.
func (p *Publisher) retire(item journalItem, at time.Time) (drain bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// A lagging ack may describe an entry from a lineage that a swap
	// (adoption, refold, rewind) has since replaced: advancing ackedSeq or
	// the anchor from it would regress the frontier and fake an eviction
	// gap, forcing a spurious forward-jump over a live window. The ack is
	// only meaningful while the exact entry still stands in the journal.
	if cur, ok := p.journal.itemAt(item.seq); !ok || cur.post != item.post || item.seq <= p.ackedSeq {
		return false
	}

	p.ackedSeq = item.seq
	p.anchor = item.post
	p.confirmed = true
	p.markReachable()

	if item.kind == entrySeal && item.height > p.storeSealedTip {
		log.Debug("Sequencer sealed tip advance", "origin", "seal-ack",
			"from", p.storeSealedTip, "to", item.height)
		p.storeSealedTip = item.height
	}

	// Confirmation is keyed to the gated block's hash, not just its height:
	// a refused block and its rebuild share a height, and a late ack for
	// the first attempt's seal must not confirm the second's gate — that
	// would broadcast content the store's seal does not describe.
	if item.kind == entrySeal && p.gate.height == item.height &&
		p.gate.verdict == gatePending {
		if header, err := decodeSealHeader(item.entry.GetBlockSeal().GetHeader()); err == nil &&
			header.Hash() == p.gate.hash {
			p.gate.verdict = gateConfirmed
		}
	}

	// A build-start hold only orders the new window behind the draining
	// flush: once the flush's seal is home, lift it so the window streams
	// mid-block instead of batching until its own seal. During an outage
	// drain the ceiling stays: the store is owed older blocks first, and
	// the retired batch is the cue to reconcile the next one in.
	if item.kind == entrySeal && p.hold.kind == holdBuild && !p.hold.gates(item.seq) {
		if p.pendingFrom != 0 {
			drain = true
		} else {
			p.hold = clearedHold()
			p.signalWake()
		}
	}

	publishQueueGauge.Update(int64(p.unackedLocked()))

	publishAckTimer.UpdateSince(at)

	return drain
}

// runState carries the run loop's backoff bookkeeping.
type runState struct {
	probe      time.Duration
	contention int
	lastRedial time.Time
}

// run owns transport: anchor via reconciliation, then send loop the journal into the
// store, reconciling on STALE and probing with backoff while unreachable.
func (p *Publisher) run(ctx context.Context) {
	defer close(p.done)
	defer func() {
		_ = p.pubConn.Close()
		_ = p.consConn.Close()
	}()

	state := runState{probe: probeBackoffMin}

	for ctx.Err() == nil && !p.failed.Load() {
		if !p.step(ctx, &state) {
			return
		}
	}
}

// step runs one iteration: anchor when needed, otherwise one send loop session.
func (p *Publisher) step(ctx context.Context, state *runState) bool {
	// A whole interval of silence means the connections themselves are
	// suspect — a store restart has wedged the gRPC channels while fresh
	// dials worked. Rate-limited to one rebuild per interval.
	if p.silentTooLong() && time.Since(state.lastRedial) > redialAfter {
		p.redial()
		state.lastRedial = time.Now()
	}

	if !p.isAnchored() {
		return p.anchorStep(ctx, state)
	}

	publishStateGauge.Update(gaugeLive)

	res := p.runStream(ctx)

	if res.reason != endTransport {
		p.unreachable.Store(false) // the store answered, whatever it said
	}

	// Price the next reconcile and carry the streak forward, cleared whenever
	// this session made progress — whatever ended it.
	delay := state.priceContention(res)

	switch res.reason {
	case endCtx, endTerminal:
		return false
	case endIdle:
		return p.idleReanchor(ctx)
	case endWatch:
		// A held build re-reading the store: reconcile only, keeping the
		// build's height so the tail is classified against it.
		return p.reconcile(ctx) != recTerminal
	case endTransport:
		p.unreachable.Store(true)

		return state.degradedSleep(ctx)
	default: // endStale
		publishStaleCount.Inc(1)

		if !contentionPause(ctx, delay) {
			return false
		}

		state.probe = probeBackoffMin
		p.setUnanchored()

		return true
	}
}

func (p *Publisher) anchorStep(ctx context.Context, state *runState) bool {
	if p.reconcile(ctx) == recOK {
		p.unreachable.Store(false) // the read went through
		state.probe = probeBackoffMin

		return true
	}

	if p.failed.Load() {
		return false
	}

	return state.degradedSleep(ctx)
}

func (s *runState) degradedSleep(ctx context.Context) bool {
	publishStateGauge.Update(gaugeDegraded)

	if !sleepCtx(ctx, s.probe) {
		return false
	}

	s.probe = min(s.probe*2, probeBackoffMax)

	return true
}

// idleReanchor re-anchors near the store tip after a quiet interval, so a
// later takeover build's bounded read starts close. A window still held
// after the quiet period is parked — its build died and its seal will
// never come — so drop its height, letting the reconcile refold the dead
// buffer instead of holding the anchor stale forever. A freely streaming
// window (no hold) is a healthy incumbent momentarily drained between
// txs, not a dead build: leaving its height intact keeps a coincident
// foreign write on the hold-and-let-our-seal-win path rather than
// superseding a window we may still seal.
func (p *Publisher) idleReanchor(ctx context.Context) bool {
	p.mu.Lock()
	if p.hold.active() {
		p.curHeight = 0
	}
	p.mu.Unlock()

	return p.reconcile(ctx) != recTerminal
}

// priceContention updates the streak for the session that just ended and
// returns the delay owed before the next reconcile (nonzero only on the
// stale path).
func (s *runState) priceContention(res streamResult) time.Duration {
	delay, streak := advanceContention(res.progressed, res.reason == endStale, s.contention)
	s.contention = streak

	return delay
}

// contentionPause waits out the reconcile backoff before a stale retry,
// reporting false if the context ended during the wait.
func contentionPause(ctx context.Context, delay time.Duration) bool {
	if delay == 0 {
		return true
	}

	publishStateGauge.Update(gaugeContending)

	return sleepCtx(ctx, delay)
}

// advanceContention prices the gap before the next reconcile and returns the
// streak to carry forward. Repeated stale-without-progress reconciles — head
// races another writer never wins — lengthen the wait; the first is immediate.
// Progress clears the streak whatever ended the session: a lost-ack resend
// STALEs before any OK, so its own session never counts as progress, and until
// a later healthy session cleared the streak it survived every quiet hour and
// each blip paid the whole accumulated ladder. At the 8s cap on a 1s chain
// that outlasts finality, stranding a completable flush behind a backfill jump
// and leaving a store hole the auditor reads as a reorg.
func advanceContention(progressed, stale bool, streak int) (time.Duration, int) {
	if progressed {
		return 0, 0
	}

	if !stale {
		return 0, streak
	}

	var delay time.Duration
	if streak > 0 {
		delay = min(reconcileBackoffMin<<(streak-1), reconcileBackoffMax)
	}

	return delay, streak + 1
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
