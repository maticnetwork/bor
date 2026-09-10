// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// The worker's sequence-store integration: publish the block lifecycle as
// it happens, follow the store's owner for a height instead of competing
// with it, and gate the broadcast on the store's verdict
// (design docs/sequencer-bor.md).

// sequencerOpen publishes the block-open record when a production build
// starts. A rebuild of the same height or a build on a new parent
// republishes — downstream re-anchoring handles both. Gated on IsRunning:
// pending-block maintenance also builds work cycles, but those never seal.
func (w *worker) sequencerOpen(work *environment) {
	if work.sequencerMuted || !w.sequencingActive(work.header.Number) {
		return
	}

	w.sequencer.OpenBlock(work.header.Number.Uint64(), work.header.Time,
		work.header.ParentHash, work.header.GasLimit, work.header.BaseFee)
}

// fillBlock fills the pending block from the txpool: one snapshot on the
// stock path, or — with a sequencer attached and producing — repeated
// snapshots until announce time, so transactions are executed and streamed
// to the sequence store as they arrive instead of waiting for the next
// slot (continuous building). An adopted window seeds the block first.
func (w *worker) fillBlock(interrupt *atomic.Int32, work *environment, genParams *generateParams) error {
	if genParams.adoption != nil {
		w.seedAdopted(work, genParams.adoption)

		// Past the announce time already — a rebuild that inherited a
		// window late. The sequenced transactions are committed, which is
		// everything consumers were promised, so close here. Filling from
		// the pool would push the seal further past its deadline to add
		// content nobody is waiting on, and each added transaction is one
		// more preconfirmation riding a block that is already late.
		if time.Until(work.header.GetActualTime()) <= sealMargin {
			return nil
		}
	}

	poll := w.sequencerPoll(work.header.Number)
	if poll <= 0 || work.sequencerMuted {
		return w.fillTransactions(interrupt, work, genParams)
	}

	return w.fillUntilAnnounce(interrupt, work, genParams, poll)
}

// fetchAdoption asks the sequencer for a dangling store window matching the
// prepared header. It runs after engine.Prepare, so only a build the
// engine authorized ever reads or adopts from the store — a doomed backup
// build taking a snapshot of a still-streaming window would leave a stale
// armed offer behind for the rotation taker to seal short.
func (w *worker) fetchAdoption(genParams *generateParams, header *types.Header) {
	genParams.adoption = nil

	if genParams.sequencerMuted || !genParams.production || !w.sequencingActive(header.Number) {
		return
	}

	genParams.adoption = w.sequencer.AdoptWindow(header.Number.Uint64(), header.ParentHash)
}

// sequencerActive gates the whole sequence-store integration on Rio for
// bor chains (post-Rio everywhere). Pre-Rio, every validator
// builds every height with per-succession timing rules; publishing,
// continuous filling, and adoption all interact with that regime — adopted
// timestamps are invalid for other signers, and the extra build churn
// starves out-of-turn seal delays. Below Rio the worker behaves stock.
func sequencerActive(bor *params.BorConfig, number *big.Int) bool {
	return bor == nil || bor.IsRio(number)
}

// sealBarrier reports whether the built block may be sealed, and when it may
// not, whether the worker should rebuild rather than drop the slot.
//
// The store is the arbiter of who owns a height: seal only once our window is
// confirmed there. Two blocks at one height is precisely what leaves
// consumers holding revoked preconfirmations.
func (w *worker) sealBarrier(work *environment) bool {
	// A muted build published nothing, so the store holds no window of ours
	// to confirm — and Seal refuses its block regardless.
	if work.sequencerMuted || !w.sequencingActive(work.header.Number) ||
		w.sequencer.AwaitSequenced(sequenceBarrierTimeout,
			work.header.Number.Uint64(), work.txs) {
		return true
	}

	// Consuming the resync signal here keeps it from leaking into the next
	// cycle and tells the two refusal shapes apart in the log. Either way
	// the cycle ends; the next one's build-start read follows the store.
	if w.sequencer.ResyncNeeded() {
		log.Warn("Not sealing: the store holds records this block does not cover",
			"number", work.header.Number)
	} else {
		log.Warn("Not sealing: another producer holds this height in the store",
			"number", work.header.Number)
	}

	return false
}

// sequencingActive reports whether a mining build at this height feeds the
// sequence store: a sequencer is attached, this node is actually producing
// (IsRunning gates out the pending-block/payload snapshot builds, which
// never seal), and the height is post-Rio. The result-loop SealBlock hook
// gates without IsRunning — it only ever sees blocks this node sealed.
func (w *worker) sequencingActive(number *big.Int) bool {
	return w.sequencer != nil && w.IsRunning() && sequencerActive(w.chainConfig.Bor, number)
}

// sequencerMutedBuild reports whether this build must stay out of the
// sequence store: the signer is configured but Seal would refuse its block,
// so a published sequence would preconfirm content that can never land, and
// its open would contend the store's height election against the real
// producer. The build itself still runs so the pending snapshot stays fresh
// for RPC reads. Only consulted for builds that would otherwise sequence.
func (w *worker) sequencerMutedBuild(genParams *generateParams, header *types.Header) bool {
	if !genParams.production || !w.sequencingActive(header.Number) {
		return false
	}

	engine, ok := w.engine.(*bor.Bor)
	if !ok {
		return false
	}

	return !engine.IsAuthorizedSigner(w.chain, header)
}

func (w *worker) applyAdoption(genParams *generateParams, header *types.Header) {
	a := genParams.adoption
	if a == nil {
		return
	}

	genParams.adoption = nil // reinstated only when every bound holds

	if reason := w.adoptionReject(a, header); reason != "" {
		// Declining is not a no-op: the build goes on to publish its own
		// open at a height the store already holds a window for, which
		// starts a second generation there. Both producers then preconfirm
		// and only one block can seal, so every line here is a mismatch or
		// a displacement waiting to happen.
		log.Warn("Declined the store's window, opening our own",
			"number", a.Number, "txs", len(a.Txs), "reason", reason)

		return
	}

	header.Time = a.Timestamp
	header.GasLimit = a.GasLimit

	deadline := time.Now().Add(adoptionSeedBudget)
	if adopted := time.Unix(int64(a.Timestamp), 0); adopted.After(deadline) {
		deadline = adopted
	}

	header.ActualTime = deadline
	genParams.adoption = a

	log.Info("Adopting sequenced window", "number", a.Number,
		"txs", len(a.Txs), "deadline", deadline.Format(time.RFC3339Nano))
}

// adoptionMinTime is the lowest timestamp an adopted window may carry:
// the parent period under bor, parent.Time+1 without a bor config.
func adoptionMinTime(bor *params.BorConfig, parentTime, number uint64) uint64 {
	if bor != nil {
		return parentTime + bor.CalculatePeriod(number)
	}

	return parentTime + 1
}

// maxAdoptedFutureSeconds mirrors consensus/bor's
// maxAllowedFutureBlockTimeSeconds: the verifier refuses any block further
// in the future, so a window beyond it cannot seal a valid block — and
// adopting one stalls the slot, since fillUntilAnnounce sleeps toward the
// window's announce time.
const maxAdoptedFutureSeconds = 30

// sequencerPoll returns the sequencing poll cadence, or zero when the block
// being built is not sequenced (no sequencer, not producing, or one-shot
// fill configured).
func (w *worker) sequencerPoll(number *big.Int) time.Duration {
	if !w.sequencingActive(number) {
		return 0
	}

	return w.sequencer.RefreshInterval()
}

// fillUntilAnnounce fills the block immediately, then keeps re-snapshotting
// the txpool until sealMargin before the block's announce time.
// Already-committed transactions are skipped by the nonce checks inside
// commitTransactions; an interrupt aborts exactly as it does on the stock
// path.
func (w *worker) fillUntilAnnounce(interrupt *atomic.Int32, work *environment, genParams *generateParams, poll time.Duration) error {
	if err := w.fillTransactions(interrupt, work, genParams); err != nil {
		return err
	}

	for {
		remaining := time.Until(work.header.GetActualTime()) - sealMargin
		if remaining <= 0 {
			return nil
		}

		time.Sleep(min(remaining, poll))

		if err := w.haltFill(interrupt, work.header.Number); err != nil {
			return err
		}

		if err := w.fillTransactions(interrupt, work, genParams); err != nil {
			return err
		}
	}
}

// haltFill reports why the fill loop should stop short of the announce
// deadline, or nil to keep filling.
func (w *worker) haltFill(interrupt *atomic.Int32, number *big.Int) error {
	if interrupt != nil {
		if signal := interrupt.Load(); signal != commitInterruptNone {
			return signalToErr(signal)
		}
	}

	// Another producer reached the store first for this height: abandon this
	// build so the next one adopts their window and follows their ordering,
	// instead of sealing a block that diverges from it.
	if w.sequencingActive(number) && w.sequencer.ResyncNeeded() {
		return errRebuildForSequence
	}

	return nil
}

// sealAndGate publishes the seal record the moment the sealed block exists —
// ahead of the chain write and the announcement — so stream consumers can
// close the block, and reports whether the block may be broadcast.
//
// The store's head CAS elects one seal per height; the gate is where the
// loser learns it lost and withholds its block, so a height gets one
// broadcast instead of a fork. No verdict inside the budget broadcasts
// anyway — production never waits on the store.
func (w *worker) sealAndGate(block *types.Block) bool {
	if w.sequencer == nil || !sequencerActive(w.chainConfig.Bor, block.Number()) {
		return true
	}

	w.sequencer.SealBlock(block)

	if w.sequencer.ConfirmSeal(sealGateTimeout) == SealRefused {
		log.Warn("Discarding sealed block: another producer's block owns this height",
			"number", block.Number(), "hash", block.Hash())

		return false
	}

	return true
}
