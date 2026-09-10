package sequencer

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
)

// gateVerdict is the broadcast gate's resolution state. gateLost outranks
// gateConfirmed by construction: a lost verdict is terminal, and the seal
// ack that would confirm can no longer arrive for a rejected seal.
type gateVerdict int

const (
	gatePending   gateVerdict = iota // awaiting the seal ack, the chain, or the deadline
	gateConfirmed                    // the store acked the gated seal
	gateLost                         // the store rejected it: another window owns the height
)

// sealGate identifies the sealed block whose broadcast awaits the store's
// verdict. A zero height means no seal is gated. refuseOnTimeout inverts
// the liveness default: normally an undecided height broadcasts, but a
// block built over a seal the store already holds must not.
type sealGate struct {
	height          uint64
	hash            common.Hash
	verdict         gateVerdict
	refuseOnTimeout bool

	// contested records a STALE landing while the verdict was pending: the
	// head moved under the flush, so our own reconcile loop is producing
	// the verdict and the wait earns the longer budget.
	contested bool

	// tolerateSealed carries a modeOverSealed decision to the gate: this
	// block knowingly sealed a height the store closed because recovery
	// declined, so a foreign store seal must not refuse it — the
	// chain-canonical check still can.
	tolerateSealed bool

	// published means a flush in the journal carries this block; a refusal
	// must drop it (a refused block is never canonical, so its flush could
	// never resolve) and roll the sealed tip back to prevTip so the rebuild
	// at this height can adopt instead of muting itself.
	published bool
	prevTip   uint64

	// txs is the gated block's transaction sequence, kept for the last
	// look a verdictless timeout takes before broadcasting.
	txs []common.Hash

	// lostHeader is the decoded foreign seal that lost the gate, when a
	// tail read carried it. It lets the verdict consumer consensus-verify
	// the claim: a seal the engine rejects owns nothing. Nil when the loss
	// came from a STALE ack alone (no header to inspect).
	lostHeader *types.Header
}

// refusalStreak counts consecutive gate refusals at one height. The escape
// valve: refuse → rebuild is convergent when the rebuild can adopt what
// stands in the store, but a window it cannot adopt (dead producer, foreign
// parent) would refuse forever — past the cap, liveness wins and the block
// broadcasts without a verdict.
type refusalStreak struct {
	height uint64
	count  int
}

// maxGateRefusals bounds the refuse → rebuild cycle per height.
const maxGateRefusals = 3

// ConfirmSeal waits for the store's verdict on the block just sealed. The
// store's head CAS admits exactly one seal at a lineage position, so the
// seal race has a single winner — this is where the loser finds out.
//
// Confirmation comes from our seal entry's ack. Refusal comes from our own
// chain: the winner's block importing at our height IS the rejection
// notice, needing no store read at all. No verdict inside the budget means
// the store is slow, unreachable, or the height genuinely unresolved — the
// caller broadcasts (production never waits on the store), which also
// covers the phantom case of a winner that sealed in the store and then
// died without broadcasting.
func (p *Publisher) ConfirmSeal(timeout time.Duration) miner.SealVerdict {
	if p.unreachable.Load() {
		return p.settle(miner.SealUnknown) // no verdict is coming; do not wait for one
	}

	start := time.Now()

	var chainCheck time.Time

	for {
		p.mu.Lock()
		g := p.gate
		p.mu.Unlock()

		if v, done := p.storedVerdict(g); done {
			return v
		}

		// The canonical lookup is an uncached database read, and this wait
		// can now last a whole block period: take it on a coarse tick
		// rather than every poll.
		if now := time.Now(); p.chain != nil && now.After(chainCheck) {
			chainCheck = now.Add(50 * time.Millisecond)

			if v, done := p.chainVerdict(g); done {
				return v
			}
		}

		// A contested gate earns the longer budget: a STALE while pending
		// means our own reconcile loop is producing the verdict, and cutting
		// the wait short is what broadcast an empty block three seconds
		// before its refusal would have arrived.
		budget := timeout
		if g.contested {
			budget = max(budget, contestedGateTimeout)
		}

		failed := p.failed.Load()
		if !failed && time.Since(start) <= budget {
			time.Sleep(2 * time.Millisecond)

			continue
		}

		return p.expiredVerdict(g, failed)
	}
}

// storedVerdict resolves the gate from what the store already decided: an
// ack confirmed it, a foreign seal lost it, or nothing is gated at all.
func (p *Publisher) storedVerdict(g sealGate) (miner.SealVerdict, bool) {
	switch {
	case g.height == 0:
		return miner.SealUnknown, true // nothing gated (muted or failed build)
	case g.verdict == gateLost:
		// A loss is only as good as the seal claiming the height: one the
		// consensus engine rejects (a rotated-out producer's) can never own
		// it, and refusing this node's block against such a seal froze a
		// devnet chain. A tail read hands the header over directly; a STALE
		// ack carries none, so the standing seal is fetched once. When the
		// seal cannot be inspected the refusal stays — the STALE is real
		// evidence, and the cap bounds it.
		if g.lostHeader != nil && p.sealConsensusInvalid(g.lostHeader) {
			return p.settle(miner.SealUnknown), true
		}

		if g.lostHeader == nil && p.lostSealInvalid(g) {
			return p.settle(miner.SealUnknown), true
		}

		return p.refuseGated(), true
	case g.verdict == gateConfirmed:
		return p.settle(miner.SealConfirmed), true
	}

	return miner.SealUnknown, false
}

// chainVerdict resolves the gate from our own chain: the winner's block
// importing at our height is the rejection notice, our own hash there is
// a twin's harmless duplicate broadcast.
func (p *Publisher) chainVerdict(g sealGate) (miner.SealVerdict, bool) {
	switch canonical := p.chain.GetCanonicalHash(g.height); {
	case canonical == g.hash:
		return p.settle(miner.SealConfirmed), true
	case canonical != (common.Hash{}):
		return p.refuseGated(), true
	}

	return miner.SealUnknown, false
}

// expiredVerdict resolves a gate whose budget ran out: refuse when the
// seal was withheld pending a decision, otherwise recheck the store once
// and let liveness broadcast on anything short of an affirmative refusal.
func (p *Publisher) expiredVerdict(g sealGate, failed bool) miner.SealVerdict {
	if g.refuseOnTimeout {
		return p.refuseGated()
	}

	if !failed {
		switch p.gateRecheck(g) {
		case miner.SealRefused:
			return p.refuseGated()
		case miner.SealConfirmed:
			return p.settle(miner.SealConfirmed)
		}
	}

	return p.settle(miner.SealUnknown)
}

// settle resolves the gate with a non-refusal verdict: clear, count, return.
// Refusals go through refuseGated, which also unwinds the refused flush.
func (p *Publisher) settle(v miner.SealVerdict) miner.SealVerdict {
	p.clearGate()

	if v == miner.SealConfirmed {
		gateConfirmedCount.Inc(1)
	} else {
		gateUnknownCount.Inc(1)
	}

	return v
}

// contestedGateTimeout is the verdict budget once the gate is contested:
// one block period, tied to recoverGrace so the two waits scale together —
// losing a slot is acceptable, losing acked records to a premature
// broadcast is not.
const contestedGateTimeout = recoverGrace

// refuseGated resolves the gate as refused. The block will never broadcast,
// so the flush describing it is dropped — a refused block is never
// canonical, and a flush that can never resolve wedges every later build
// behind it — and the sealed tip it bumped rolls back so the rebuild at
// this height can adopt what stands in the store instead of muting itself.
// Past the per-height refusal cap, liveness wins: the rebuild evidently
// cannot adopt its way to convergence, and the block broadcasts without a
// verdict.
func (p *Publisher) refuseGated() miner.SealVerdict {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.gate.height != p.refusals.height {
		p.refusals = refusalStreak{height: p.gate.height}
	}

	p.refusals.count++

	if p.refusals.count > maxGateRefusals {
		log.Warn("Sequencer gate refused this height repeatedly, broadcasting for liveness",
			"number", p.gate.height, "refusals", p.refusals.count)
		p.gate = sealGate{}
		gateUnknownCount.Inc(1)

		return miner.SealUnknown
	}

	if p.gate.published {
		p.dropRefusedFlushLocked(p.gate.height, p.gate.hash)

		if p.sealedTip == p.gate.height {
			p.sealedTip = p.gate.prevTip
		}
	}

	p.gate = sealGate{}
	gateRefusedCount.Inc(1)

	return miner.SealRefused
}

// dropRefusedFlushLocked removes the refused block's undelivered trailing
// entries, and anything a concurrent work cycle chained above them — those
// describe blocks that re-flush from their own bodies at seal time. An
// acked prefix is store content and stays; older stacked flushes below the
// refused height are still owed and stay. The hold clears with the flush it
// gated, or the next barrier would resync against a lineage that no longer
// exists.
func (p *Publisher) dropRefusedFlushLocked(height uint64, hash common.Hash) {
	if ours, ok := p.sealedHashAtLocked(height); !ok || ours != hash {
		return // the trailing seal is not the refused block's
	}

	cut := p.journal.cutFromHeight(p.ackedSeq, height)
	if cut == len(p.journal.items) {
		return
	}

	publishDropMeter.Mark(int64(len(p.journal.items) - cut))
	p.rewindJournalLocked(cut)
	p.syncWindowLocked()
	p.hold = clearedHold()
	publishQueueGauge.Update(int64(p.unackedLocked()))
}

// gateRecheck is the last look before a verdictless broadcast: one
// height-anchored probe at the gated height, taken after the whole wait.
// The reads that precede it can each be individually fresh and still miss
// a rival's burst landing between them; what this read shows is exactly
// what the broadcast would bury. Anything unreadable keeps the timeout
// verdict — production never waits on a store it cannot see.
func (p *Publisher) gateRecheck(g sealGate) miner.SealVerdict {
	if p.unreachable.Load() || p.read == nil || p.read.cons == nil {
		return miner.SealUnknown
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTailTimeout)
	defer cancel()

	// One walk anchored at the gated height answers everything: an unknown
	// height reads NOT_FOUND (nothing stands here), a live generation is
	// served from its open, a sealed one from just past its seal.
	info, out, done := p.tryWalk(ctx, blockReq(g.height), false)
	if !done || out != recOK {
		return miner.SealUnknown
	}

	if info.tipOpen && info.tipOpenHeight == g.height {
		if storeTxs, ok := windowTxHashes(info.window); ok && !windowLeadsHashes(storeTxs, g.txs) {
			gateRecheckRefused.Inc(1)
			log.Warn("Sequencer refusing broadcast: acked records stand at this height the block does not carry",
				"number", g.height, "store", len(storeTxs), "block", len(g.txs))

			return miner.SealRefused
		}

		return miner.SealUnknown
	}

	// A generation at the gated height with no live window is a sealed one.
	// The walk is served from just past the seal, so the generation itself —
	// seal included — comes from the block fetch.
	return p.recheckSealedGeneration(ctx, g)
}

// recheckSealedGeneration fetches the generation standing at the gated
// height and applies the seal policy (verdictForSeal) to its seal.
func (p *Publisher) recheckSealedGeneration(ctx context.Context, g sealGate) miner.SealVerdict {
	entries, err := p.read.generation(ctx, g.height)
	if err != nil {
		return miner.SealUnknown
	}

	for _, e := range entries {
		seal := e.GetBlockSeal()
		if seal == nil {
			continue
		}

		header, err := decodeSealHeader(seal.GetHeader())
		if err != nil {
			return miner.SealUnknown
		}

		v := g.verdictForSeal(header.Hash())
		if v == miner.SealRefused && p.sealConsensusInvalid(header) {
			return miner.SealUnknown
		}

		if v == miner.SealRefused {
			gateRecheckRefused.Inc(1)
			log.Warn("Sequencer refusing broadcast: the store sealed this height with other content",
				"number", g.height, "store", header.Hash())
		}

		return v
	}

	return miner.SealUnknown // a live generation after all: nothing decisive
}

// lostSealInvalid fetches the seal standing at the lost height and reports
// whether consensus rejects it — the check a header-less loss (a STALE ack)
// needs before its refusal is honored. Unreadable, undecodable, or unsealed
// keeps the refusal.
func (p *Publisher) lostSealInvalid(g sealGate) bool {
	if p.verifySeal == nil || p.unreachable.Load() || p.read == nil || p.read.cons == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTailTimeout)
	defer cancel()

	entries, err := p.read.generation(ctx, g.height)
	if err != nil {
		return false
	}

	for _, e := range entries {
		seal := e.GetBlockSeal()
		if seal == nil {
			continue
		}

		header, err := decodeSealHeader(seal.GetHeader())
		if err != nil || header.Hash() == g.hash {
			return false // unreadable, or our own seal delivered after all
		}

		return p.sealConsensusInvalid(header)
	}

	return false
}

func (p *Publisher) clearGate() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.gate = sealGate{}
}

// gatePendingLocked reports whether an unbroadcast seal at this height is
// still awaiting its verdict.
func (p *Publisher) gatePendingLocked(height uint64) bool {
	return p.gate.height != 0 && p.gate.height == height && p.gate.verdict != gateConfirmed
}

// verdictForSeal is the gate's one seal policy: our own seal standing at
// the gated height confirms the broadcast (a twin delivered our copy), a
// foreign one refuses it — unless the build knowingly sealed over it (the
// liveness fallback), which leaves the verdict to the budget.
func (g sealGate) verdictForSeal(hash common.Hash) miner.SealVerdict {
	switch {
	case hash == g.hash:
		return miner.SealConfirmed
	case g.tolerateSealed:
		return miner.SealUnknown
	default:
		return miner.SealRefused
	}
}

// resolveGateFromSealLocked applies the seal policy to a decoded store seal
// at the gated height. An undecoded seal proves nothing and leaves the
// budget to decide.
func (p *Publisher) resolveGateFromSealLocked(info tailInfo) {
	if !info.sealDecoded || p.gate.verdict != gatePending {
		return
	}

	switch p.gate.verdictForSeal(info.lastSealHash) {
	case miner.SealConfirmed:
		p.gate.verdict = gateConfirmed
	case miner.SealRefused:
		p.gate.verdict = gateLost
		// Keep the decoded header so the verdict consumer can consensus-
		// verify the claim outside this lock (verifySeal can reach heimdall;
		// p.mu must never wait on that).
		p.gate.lostHeader = info.lastSealHeader
	}
}

// markGateLost records what a STALE means for the gated block. Any STALE
// while the verdict is pending marks the gate contested — the head moved
// under our flush, reconciliation is now producing the verdict, and the
// wait earns the longer budget. A STALE on the gated seal itself is the
// verdict: another producer's window owns the height, and waiting out the
// liveness budget after that would broadcast a second block into a height
// the store has already given to someone else.
func (p *Publisher) markGateLost(item journalItem) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.gate.height == 0 || p.gate.verdict != gatePending {
		return
	}

	p.gate.contested = true

	if item.kind == entrySeal && p.gate.height == item.height {
		p.gate.verdict = gateLost
	}
}
