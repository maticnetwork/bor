// Package sequencer connects Bor to the sequence store: the block producer
// publishes each block's lifecycle (open, transactions, seal) as it happens.
// The wire contract and commitment chain live in
// github.com/0xPolygon/sequence-store-proto; the design and terminology
// reference is docs/sequencer-bor.md.
package sequencer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

const (
	// Probe backoff while the store is unreachable. The cap stays small so a
	// replayable gap cannot outgrow the journal while waiting.
	probeBackoffMin = time.Second
	probeBackoffMax = 5 * time.Second

	// Backoff between reconciles whose corrective publish immediately
	// re-STALEs — losing head races to another publisher.
	reconcileBackoffMin = 500 * time.Millisecond
	reconcileBackoffMax = 8 * time.Second
)

// ackStallTimeout bounds how long the send loop waits on an unacked entry before
// treating the stream as dead (a hung store stalls acks without
// erroring). Kept under the journal's coverage so the reconnect can still
// resend instead of jumping. Var for tests.
var ackStallTimeout = 5 * time.Second

// chainReader is the chain access reconciliation needs: classifying
// sealed store content against the local chain, locating the store tail
// on a cold start from the last imported block, and rebuilding collapsed
// blocks for backfill — the chain database is the archive; the journal
// holds only the work in flight.
type chainReader interface {
	GetCanonicalHash(number uint64) common.Hash
	GetHeaderByHash(hash common.Hash) *types.Header
	CurrentBlock() *types.Header
	GetBlockByNumber(number uint64) *types.Block
}

// Publisher streams block-production entries to the sequence store. Enqueue
// methods are called from the worker's goroutines and never block: folds are
// computed inline (sub-microsecond), transport happens on a background
// goroutine. When the store is unreachable the publisher keeps folding into
// its retention journal and recovers by journal replay or forward jump on
// reconnect; a STALE ack triggers tail-read reconciliation.
// Only MALFORMED acks and fold divergence are terminal.
type Publisher struct {
	// mu makes fold-and-append atomic across the worker's goroutines and
	// serializes lineage swaps against the transport goroutine.
	mu        sync.Mutex
	head      commitment.Head // local fold tip (end of the lineage)
	journal   *journal
	ackedSeq  uint64          // seq of the last store-confirmed journal item
	anchor    commitment.Head // store head confirming everything through ackedSeq
	anchored  bool            // anchor established by a completed reconcile
	confirmed bool            // anchor was ever store-confirmed
	curHeight uint64          // height of the open window being built (0 = none)
	awaitOpen bool            // drop records until the next OpenBlock (post purge)
	adopt     *adoption       // store window being adopted
	sealedTip uint64          // highest height we sealed-and-flushed
	// mode is the build treatment for the current height, decided once per
	// build-start classification; see buildMode.
	mode buildMode

	// diverged records a partial adoption for the current build; see the
	// divergence type. Reset with mode: at the build-start classification
	// and at the seal flush.
	diverged divergence

	// resync is set when the store shows another producer building the
	// height this node is building: our block must not seal beside their
	// sequence. Reading it clears it, ending the worker's current cycle —
	// the next cycle's build-start read adopts the standing window, and a
	// signal that survives to that read is cleared there as stale.
	resync bool

	// pendingFrom..pendingTo (inclusive; 0 = none) are sealed blocks
	// collapsed out of the journal while the store could not take them.
	// They live in the chain database and are rebuilt at the next
	// re-anchor (backfill). storeSealedTip is the highest height the
	// store is known to have sealed — delivered by us or read from a
	// tail — and floors the backfill: the store never needs a block at
	// or below it.
	pendingFrom    uint64
	pendingTo      uint64
	pendingEntries int // exact entry count of the collapsed range
	storeSealedTip uint64

	hold hold // the send loop's send ceiling; see the hold type

	// gate is the broadcast gate for the last sealed block; see sealGate.
	gate     sealGate
	refusals refusalStreak

	// barrierRefusals counts consecutive pre-seal barrier refusals at one
	// height — the barrier's analog of the gate's refusal cap. Every
	// refusal shape converges when the store is consistent (adopt, mute,
	// backfill, the clean-boundary proof), but a store answering
	// inconsistently across reads — Byzantine, or a load balancer over
	// replicas at different lags — can refuse every rebuild with no benign
	// state ever showing. Past the cap, liveness wins and the seal
	// proceeds; the post-seal gate and consensus remain the arbiters.
	barrierRefusals refusalStreak
	seed            commitment.Head // an empty store's head, computable without the store
	// unreachable is set while the transport is failing. Every per-block
	// wait on the store is pointless in that state, and paying them all
	// pushes blocks past their slot — which is what arms bor's span-check
	// path and turns a store outage into a chain slowdown.
	unreachable atomic.Bool

	failed atomic.Bool

	wake chan struct{}

	poll  time.Duration
	chain chainReader

	// finality reports the newest whitelisted milestone (nil when the node
	// wires none). It floors the backfill: heights at or below a canonical
	// milestone are final — permanently served by the canonical chain — so
	// their store copies serve no consumer and the drain jumps them.
	finality func() (bool, uint64, common.Hash)

	// verifySeal is the consensus engine's sealed-header verification (nil
	// when the node wires none). The gate consults it before honoring a
	// foreign store seal as ownership of a height: a seal whose signer the
	// engine rejects — a producer rotated out mid-span — can never become
	// canonical, so it must not refuse this node's block.
	verifySeal func(*types.Header) error

	pubConn  *grpc.ClientConn
	consConn *grpc.ClientConn
	pub      pb.PublisherServiceClient
	read     *reader

	// Redial bookkeeping: the endpoints to dial again, and the time of the
	// last successful store interaction of any kind. gRPC channels are
	// supposed to heal on their own; a store container restart has wedged
	// them in a permanent connect-retry loop while fresh dials worked, so
	// after redialAfter of silence the publisher starts its connections
	// over instead of trusting the channel state machine.
	pubEndpoint  string
	consEndpoint string
	lastContact  atomic.Int64 // unix nanos

	cancel context.CancelFunc
	done   chan struct{}
}

// NewPublisher dials the store and starts the transport goroutine. The
// publisher service (publish stream) and consumer service (reconcile tail
// reads) have their own endpoints. On a cold start the store
// tail is relocated from the chain's last imported block, so no
// local position is persisted.
func NewPublisher(publisherEndpoint, consumerEndpoint string, chainID uint64, poll time.Duration, chain chainReader, finality func() (bool, uint64, common.Hash)) (*Publisher, error) {
	pubConn, consConn, err := dialStore(publisherEndpoint, consumerEndpoint)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &Publisher{
		head:     commitment.Seed(chainID),
		anchor:   commitment.Seed(chainID),
		seed:     commitment.Seed(chainID),
		hold:     clearedHold(),
		journal:  newJournal(),
		wake:     make(chan struct{}, 1),
		poll:     poll,
		chain:    chain,
		finality: finality,
		pubConn:  pubConn,
		consConn: consConn,
		pub:      pb.NewPublisherServiceClient(pubConn),
		cancel:   cancel,
		done:     make(chan struct{}),
	}

	p.pubEndpoint, p.consEndpoint = publisherEndpoint, consumerEndpoint
	p.lastContact.Store(time.Now().UnixNano())

	p.read = newReader(pb.NewConsumerServiceClient(consConn), p.seed, p.markReachable)

	publishStateGauge.Update(gaugeDegraded) // until the startup reconcile anchors

	go p.run(ctx)

	return p, nil
}

// markReachable notes a successful store round trip. The reader invokes it
// on every read and retire on every ack, so a recovered store stops being
// treated as down the moment anything hears from it — and the contact
// stamp is what holds the redial back.
// SetSealVerifier wires the consensus engine's sealed-header verification
// into the broadcast gate; see the verifySeal field. Call before Start.
func (p *Publisher) SetSealVerifier(f func(*types.Header) error) {
	p.verifySeal = f
}

// gateVerifyTimeout bounds the engine's seal verification inside the gate.
// The engine resolves the signer's span, and that path waits on heimdall
// availability with no deadline of its own — on the miner's result loop,
// an unbounded wait there would freeze block import. A cached span answers
// in microseconds; a heimdall round trip in milliseconds; anything slower
// is heimdall being down, and the verdict is not worth production.
const gateVerifyTimeout = 250 * time.Millisecond

// sealConsensusInvalid reports whether a store seal decodes to a header the
// consensus engine refuses — typically a signer outside the producer set
// after a mid-span rotation. Such a seal is noise, not ownership: no chain
// will ever import its block, and honoring it wedged production on a devnet
// (the elected producer discarded its own valid block against a rotated-out
// producer's store seal while the chain sat frozen a height below). A
// verification that cannot complete in time proves nothing: the seal is
// treated as unverifiable and keeps whatever verdict stood without it. The
// abandoned call finishes on its own goroutine when heimdall recovers; the
// gate's refusal cap bounds how many can pile up.
func (p *Publisher) sealConsensusInvalid(header *types.Header) bool {
	if p.verifySeal == nil {
		return false
	}

	verdict := make(chan error, 1)

	go func() { verdict <- p.verifySeal(header) }()

	var err error
	select {
	case err = <-verdict:
	case <-time.After(gateVerifyTimeout):
		log.Warn("Sequencer seal verification timed out, treating the store seal as unverifiable",
			"number", header.Number, "store", header.Hash())

		return false
	}

	if err != nil {
		gateUnknownCount.Inc(1)
		log.Warn("Sequencer ignoring a consensus-invalid store seal",
			"number", header.Number, "store", header.Hash(), "err", err)
	}

	return err != nil
}

func (p *Publisher) markReachable() {
	p.unreachable.Store(false)
	p.lastContact.Store(time.Now().UnixNano())
}

// dialStore opens the publisher and consumer connections. gRPC's own
// reconnect backoff is capped at the probe cap: after a long outage the
// default (up to 120s) would keep the connection down well past the
// store's return, growing the gap a forward jump abandons.
func dialStore(publisherEndpoint, consumerEndpoint string) (*grpc.ClientConn, *grpc.ClientConn, error) {
	connBackoff := backoff.DefaultConfig
	connBackoff.MaxDelay = probeBackoffMax

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: connBackoff}),
	}

	pubConn, err := grpc.NewClient(publisherEndpoint, opts...)
	if err != nil {
		return nil, nil, err
	}

	consConn, err := grpc.NewClient(consumerEndpoint, opts...)
	if err != nil {
		_ = pubConn.Close()

		return nil, nil, err
	}

	return pubConn, consConn, nil
}

// redialAfter is how much store silence the publisher tolerates before
// rebuilding its connections, and the floor between rebuilds. Var for tests.
var redialAfter = time.Minute

// silentTooLong reports whether every store interaction has failed for a
// whole redial interval.
func (p *Publisher) silentTooLong() bool {
	return time.Since(time.Unix(0, p.lastContact.Load())) > redialAfter
}

// redial replaces both store connections and swaps the clients in place.
// The transport goroutine owns p.pub; the reader hands out its client
// under its own lock, so in-flight calls finish on the old connections,
// which close in the background.
func (p *Publisher) redial() {
	pubConn, consConn, err := dialStore(p.pubEndpoint, p.consEndpoint)
	if err != nil {
		log.Warn("Sequencer redial failed", "err", err)

		return
	}

	p.mu.Lock()
	oldPub, oldCons := p.pubConn, p.consConn
	p.pubConn, p.consConn = pubConn, consConn
	p.pub = pb.NewPublisherServiceClient(pubConn)
	p.mu.Unlock()

	p.read.setClient(pb.NewConsumerServiceClient(consConn))

	_ = oldPub.Close()
	_ = oldCons.Close()

	publishRedialCount.Inc(1)
	log.Warn("Sequencer redialed the store after prolonged silence")
}

// signalWake nudges the transport goroutine without blocking: the wake
// channel is a depth-1 signal, so a pending wake absorbs this one.
func (p *Publisher) signalWake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// advanceStoreSealedTipLocked records the newest seal a read decoded. Only a
// decoded seal qualifies — an inferred boundary can be a live partial window,
// and booking one as sealed is how an outage hole got pinned as permanent.
func (p *Publisher) advanceStoreSealedTipLocked(info tailInfo, origin string) {
	if info.sealDecoded && info.lastSealHeight > p.storeSealedTip {
		log.Debug("Sequencer sealed tip advance", "origin", origin,
			"from", p.storeSealedTip, "to", info.lastSealHeight)
		p.storeSealedTip = info.lastSealHeight
	}
}

// unackedLocked counts entries past the acked frontier by sequence
// arithmetic, so unacked entries already evicted from the journal still count
// (they are exactly what a forward jump abandons).
func (p *Publisher) unackedLocked() int {
	return int(p.journal.nextSeq - 1 - p.ackedSeq)
}

// heldMidBuild reports a live build whose entries are gated by a sticky
// hold: we are not writing, so only a deliberate re-read can show us what
// the competing producer is doing.
func (p *Publisher) heldMidBuild() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.curHeight != 0 && p.hold.kind == holdSticky
}

// ourOpenParentLocked is the parent hash of the window this node is
// building, read from its open entry in the journal.
func (p *Publisher) ourOpenParentLocked() (common.Hash, bool) {
	idx := p.journal.openStart()
	if idx < 0 {
		return common.Hash{}, false
	}

	open := p.journal.items[idx].entry.GetBlockOpen()
	if open == nil {
		return common.Hash{}, false
	}

	return common.BytesToHash(open.GetParentHash()), true
}

// RefreshInterval is the txpool poll cadence the worker uses while a block is
// open; zero keeps the one-shot fill.
func (p *Publisher) RefreshInterval() time.Duration {
	return p.poll
}

// Close stops the transport goroutine. No local state is persisted: a
// restart relocates the store tail from the chain's last imported block.
func (p *Publisher) Close() {
	p.cancel()
	<-p.done
}

func (p *Publisher) fail(msg string, args ...any) {
	if p.failed.CompareAndSwap(false, true) {
		publishFailedGauge.Update(1)
		publishStateGauge.Update(gaugeFailed)
		log.Error("Sequencer publishing disabled: "+msg, args...)
	}
}

func (p *Publisher) isAnchored() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.anchored
}

func (p *Publisher) setUnanchored() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.anchored = false
}
