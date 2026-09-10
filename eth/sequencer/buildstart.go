package sequencer

import (
	"context"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// The build-start read: what stands in the store at the height about to be
// built, and how a height sealed there but missing from the chain recovers.

type buildStartState int

const (
	buildAtBoundary buildStartState = iota // info holds this height's live state
	buildSealedPast                        // the store sealed at or past this height
	buildBehind                            // the store lacks the parent: owed a backfill
	buildUnknown                           // the store could not be read in budget
	buildRecover                           // sealed in the store, absent from the chain
	buildSealedWait                        // sealed in the store, its block still owed to us
)

// recoverState is how a height sealed in the store but missing from the chain
// should be handled.
type recoverState int

const (
	recoverNone  recoverState = iota // not recoverable: treat as an ordinary sealed height
	recoverWait                      // the block may still arrive; do not rebuild yet
	recoverReady                     // the sealer is presumed dead; rebuild its prefix
)

// buildStartRead resolves what stands at the height about to be built. It
// anchors at the height itself — probing down for the nearest generation —
// rather than at our own anchor: the anchor-based walk only sees entries
// written after our last confirmed write, and goes blind to a live window
// after a mid-window rebase. This read cannot.
//
// The probe finds generations, sealed or live (the store's block index
// serves both), so the walk disambiguates: a live generation is served from
// its open, and a sealed one from just past its seal.
func (p *Publisher) buildStartRead(ctx context.Context, number uint64) (tailInfo, buildStartState) {
	h, found, err := p.read.probeDown(ctx, number)
	if err != nil {
		return tailInfo{}, buildUnknown
	}

	if !found {
		// No generation at or below this height. An empty store is a clean
		// boundary — a fresh chain, or an operator wipe, where the open
		// forward-jumps from the seed by design. A non-empty store without
		// our parent is behind.
		info, out := p.read.floorRead(ctx)
		if out != recOK {
			return tailInfo{}, buildUnknown
		}

		if number <= 1 || (!info.haveSeal && !info.tipOpen && len(info.window) == 0) {
			return info, boundaryOrHold(info)
		}

		return info, buildBehind
	}

	// probeDown's contract is h <= number whenever found; a height above
	// the query means the probe regressed. Fail the read loudly rather
	// than misclassify — buildUnknown builds from the pool, adopting
	// nothing.
	if h > number {
		log.Error("Sequence store probe above its query", "height", h, "number", number)
		return tailInfo{}, buildUnknown
	}

	if h < number-1 {
		return tailInfo{haveSeal: true, lastSealHeight: h}, buildBehind
	}

	// h is our height or our parent's. Walk from its boundary: a live
	// generation at h is served from its open, so the window is in view.
	info, out, done := p.tryWalk(ctx, blockReq(h), false)
	if !done || out != recOK {
		return tailInfo{}, buildUnknown
	}

	switch {
	case info.tipOpen && info.tipOpenHeight == number:
		// A live window at our height: the adopt case.
		return info, buildAtBoundary

	case info.tipOpen && info.tipOpenHeight < number:
		// A dangling unsealed window at our parent's height: its producer
		// died before the seal flushed. The store is owed the canonical
		// parent — the backfill supersedes the dangling window with it.
		return info, buildBehind

	case h == number || info.tipOpen ||
		(info.haveSeal && info.lastSealHeight >= number):
		// Our height is sealed, or the store's newest window is past us:
		// the height is closed. Unless the chain never received that block —
		// then its producer sealed and died before broadcasting, and this
		// build recovers the height instead of muting it away forever.
		switch rec, st := p.recoverSealed(ctx, number); st {
		case recoverReady:
			return rec, buildRecover
		case recoverWait:
			return info, buildSealedWait
		}

		return info, buildSealedPast
	}

	// h == number-1, sealed, nothing after it: a clean boundary. Carry the
	// probe's knowledge — the parent is sealed — for the dead-build discard
	// and the sealed-tip bookkeeping.
	if !info.haveSeal {
		info.haveSeal, info.lastSealHeight = true, number-1
	}

	return info, boundaryOrHold(info)
}

// boundaryOrHold refuses to mint a generation on a head we cannot derive.
//
// A boundary read says "nothing stands at this height" — but a head taken on
// the store's word says nothing of the sort, because we never saw what
// produced it. Opening there is how a second generation lands on top of
// another producer's live window: the CAS passes, since passing only means
// we echoed the value we were handed. Holding costs this block's records;
// opening blind costs the other producer's.
func boundaryOrHold(info tailInfo) buildStartState {
	if !info.explained {
		readUnexplained.Inc(1)
		log.Warn("Sequencer holding build: store head could not be derived from entries read")

		return buildUnknown
	}

	return buildAtBoundary
}

// recoverGrace is how long a height sealed in the store may stay missing from
// the chain before another build rebuilds it. One block period: long enough
// that an in-flight broadcast wins the race, short enough that a dead sealer
// costs one slot rather than the chain.
const recoverGrace = 4 * time.Second

// recoverSealed reconstructs a sealed generation the chain never received.
//
// A producer that got its seal acked and then died leaves the height closed
// in the store and empty on the chain: muting there strands it forever, and
// building fresh content there would orphan every record the dead producer
// already had acked — those are preconfirmations, and the store is the proof
// they were issued. So the recovery build must carry exactly that prefix,
// which means reading the generation back out of the store.
//
// Only the content is recovered. The store already holds the complete
// generation, seal included, so the rebuild publishes nothing — the caller
// mutes it.
func (p *Publisher) recoverSealed(ctx context.Context, number uint64) (tailInfo, recoverState) {
	if p.chain == nil || p.chain.GetCanonicalHash(number) != (common.Hash{}) {
		return tailInfo{}, recoverNone // the chain has the block: an ordinary loss
	}

	entries, err := p.read.generation(ctx, number)
	if err != nil {
		return tailInfo{}, recoverNone
	}

	if len(entries) == 0 || entries[0].GetBlockOpen() == nil {
		return tailInfo{}, recoverNone
	}

	// Give the sealer its block period to broadcast before rebuilding on its
	// behalf: the block may simply still be in flight. Measuring from the
	// block's own timestamp rather than from now means a build that starts
	// late does not add another period of waiting.
	if ts := entries[0].GetBlockOpen().GetBlockTimestamp(); ts != 0 &&
		time.Now().Before(time.Unix(int64(ts), 0).Add(recoverGrace)) {
		return tailInfo{}, recoverWait
	}

	// Everything up to (not including) the seal is the window; the seal
	// itself stays the store's, not ours to reissue. A generation with no
	// seal is a live window, not a phantom — the probe resolves generations
	// rather than sealed blocks, so this is the disambiguation.
	window := make([]*pb.Entry, 0, len(entries))
	sealed := false

	for _, e := range entries {
		if e.GetBlockSeal() != nil {
			sealed = true

			break
		}

		window = append(window, e)
	}

	if !sealed {
		return tailInfo{}, recoverNone
	}

	cur := commitment.Head(window[0].GetBlockOpen().GetPrefixCommitment())

	for _, e := range window {
		next, err := foldEntry(cur, e)
		if err != nil {
			return tailInfo{}, recoverNone
		}

		cur = next
	}

	open := window[0].GetBlockOpen()

	publishRecoverCount.Inc(1)
	log.Warn("Recovering a height sealed in the store but absent from the chain",
		"number", number, "records", len(window)-1)

	return tailInfo{
		s:             cur,
		tipOpen:       true,
		tipOpenHeight: number,
		tipOpenParent: common.BytesToHash(open.GetParentHash()),
		window:        window,
	}, recoverReady
}
