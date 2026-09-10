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

package downloader

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/log"
)

func TestClassifySyncFailureReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		reason peerFailureReason
		ok     bool
	}{
		{name: "invalid chain", err: errInvalidChain, reason: peerFailureInvalidChain, ok: true},
		{name: "pruned sidechain", err: fmt.Errorf("%w: %w", errInvalidChain, errors.New(sidechainGhostStateMsg)), reason: peerFailurePrunedSidechain, ok: true},
		{name: "bad peer", err: errBadPeer, reason: peerFailureBadPeer, ok: true},
		{name: "timeout", err: errTimeout, reason: peerFailureTimeout, ok: true},
		{name: "stalling", err: errStallingPeer, reason: peerFailureStalling, ok: true},
		{name: "unsynced", err: errUnsyncedPeer, reason: peerFailureUnsynced, ok: true},
		{name: "empty header set", err: errEmptyHeaderSet, reason: peerFailureEmptyHeaderSet, ok: true},
		{name: "peers unavailable", err: ErrPeersUnavailable, reason: peerFailurePeersUnavailable, ok: true},
		{name: "too old", err: errTooOld, reason: peerFailureTooOld, ok: true},
		{name: "invalid ancestor", err: errInvalidAncestor, reason: peerFailureInvalidAncestor, ok: true},
		{name: "wrapped timeout overrides bad peer", err: fmt.Errorf("%w: header request failed: %w", errBadPeer, errTimeout), reason: peerFailureTimeout, ok: true},
		{name: "wrapped timeout overrides invalid chain", err: fmt.Errorf("%w: %w", errInvalidChain, errTimeout), reason: peerFailureTimeout, ok: true},
		{name: "wrapped context deadline overrides invalid chain", err: fmt.Errorf("%w: %w", errInvalidChain, context.DeadlineExceeded), reason: peerFailureTimeout, ok: true},
		{name: "flattened context deadline overrides invalid chain", err: fmt.Errorf("%v: %s", errInvalidChain, context.DeadlineExceeded), reason: peerFailureTimeout, ok: true},
		{name: "pruned sidechain wins over wrapped deadline", err: fmt.Errorf("%w: %s: %w", errInvalidChain, sidechainGhostStateMsg, context.DeadlineExceeded), reason: peerFailurePrunedSidechain, ok: true},
		{name: "whitelist mismatch wrapped in invalid chain", err: fmt.Errorf("%w: %w", errInvalidChain, whitelist.ErrMismatch), reason: peerFailureWhitelistMismatch, ok: true},
		{name: "bare whitelist mismatch", err: whitelist.ErrMismatch, reason: peerFailureWhitelistMismatch, ok: true},
		{name: "whitelist no remote classified", err: whitelist.ErrNoRemote, reason: peerFailureNoRemote, ok: true},
		{name: "whitelist mismatch wins over invalid chain", err: fmt.Errorf("retrieved hash chain is invalid: %w: %w", errInvalidChain, whitelist.ErrMismatch), reason: peerFailureWhitelistMismatch, ok: true},
		{name: "wrapped peers unavailable is not a chain fault", err: fmt.Errorf("%w: %w", errInvalidChain, ErrPeersUnavailable), reason: peerFailurePeersUnavailable, ok: true},
		{name: "wrapped no peers is not a chain fault", err: fmt.Errorf("%w: %w", errInvalidChain, errNoPeers), reason: peerFailurePeersUnavailable, ok: true},
		{name: "bare no peers", err: errNoPeers, reason: peerFailurePeersUnavailable, ok: true},
		{name: "wrapped disconnect backs off instead of dropping", err: fmt.Errorf("%w: header request failed: %w", errBadPeer, eth.ErrDisconnected), reason: peerFailureDisconnected, ok: true},
		{name: "bare disconnect backs off instead of dropping", err: eth.ErrDisconnected, reason: peerFailureDisconnected, ok: true},
		{name: "invalid chain wins over wrapped stalling", err: fmt.Errorf("%w: %w", errInvalidChain, errStallingPeer), reason: peerFailureInvalidChain, ok: true},
		{name: "invalid chain with deadline text is not downgraded", err: fmt.Errorf("%w: %v", errInvalidChain, errors.New("invalid merkle root: "+context.DeadlineExceeded.Error())), reason: peerFailureInvalidChain, ok: true},
		{name: "bad peer with deadline text is not downgraded", err: fmt.Errorf("%w: %v", errBadPeer, errors.New("served garbage: "+context.DeadlineExceeded.Error())), reason: peerFailureBadPeer, ok: true},
		{name: "invalid ancestor with deadline text is not downgraded", err: fmt.Errorf("%w: %v", errInvalidAncestor, errors.New(context.DeadlineExceeded.Error())), reason: peerFailureInvalidAncestor, ok: true},
		{name: "ghost-state text alone is pruned sidechain", err: errors.New(sidechainGhostStateMsg), reason: peerFailurePrunedSidechain, ok: true},
		{name: "no ancestor backs off", err: errNoAncestorFound, reason: peerFailureUnsynced, ok: true},
		{name: "invalid body handled at delivery is unclassified", err: errInvalidBody},
		{name: "invalid witness backs the source off", err: fmt.Errorf("%w: %w", errInvalidWitness, core.ErrWitnessInvalid), reason: peerFailureInvalidWitness, ok: true},
		{name: "unclassified generic", err: errors.New("some unrelated failure")},
		{name: "nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, ok := classifySyncFailure(tt.err)
			if ok != tt.ok {
				t.Fatalf("classification mismatch: have %v, want %v", ok, tt.ok)
			}
			if reason != tt.reason {
				t.Fatalf("reason mismatch: have %q, want %q", reason, tt.reason)
			}
		})
	}
}

func TestPeerResponseDecisionActions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		reason  peerFailureReason
		action  peerResponseAction
		backoff time.Duration
	}{
		{name: "ghost-state escalation", reason: peerFailurePrunedSidechain, action: peerResponseGhostState, backoff: peerJailBackoff},
		{name: "drop invalid chain", reason: peerFailureInvalidChain, action: peerResponseDrop},
		{name: "drop bad peer", reason: peerFailureBadPeer, action: peerResponseDrop},
		{name: "drop invalid ancestor", reason: peerFailureInvalidAncestor, action: peerResponseDrop},
		{name: "mismatch escalation", reason: peerFailureWhitelistMismatch, action: peerResponseMismatch, backoff: peerSoftBackoff},
		{name: "backoff old peer", reason: peerFailureTooOld, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "backoff timeout", reason: peerFailureTimeout, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "backoff stalling", reason: peerFailureStalling, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "backoff unsynced", reason: peerFailureUnsynced, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "backoff empty header set", reason: peerFailureEmptyHeaderSet, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "backoff disconnected", reason: peerFailureDisconnected, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "backoff invalid witness", reason: peerFailureInvalidWitness, action: peerResponseBackoff, backoff: peerSoftBackoff},
		{name: "no remote is no-op", reason: peerFailureNoRemote, action: peerResponseNone},
		{name: "ignore peers unavailable", reason: peerFailurePeersUnavailable, action: peerResponseNone},
		{name: "ignore unknown reason", reason: peerFailureReason("unclassified"), action: peerResponseNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
			decision := peer.responseDecision(tt.reason)
			if decision.action != tt.action {
				t.Fatalf("action mismatch: have %v, want %v", decision.action, tt.action)
			}
			if decision.backoff != tt.backoff {
				t.Fatalf("backoff mismatch: have %v, want %v", decision.backoff, tt.backoff)
			}
			if decision.reason != tt.reason {
				t.Fatalf("reason mismatch: have %q, want %q", decision.reason, tt.reason)
			}
		})
	}
}

func TestHandleSyncFailureSkipsUnknownPeer(t *testing.T) {
	t.Parallel()

	downloader := &Downloader{peers: newPeerSet()}
	if !downloader.handleSyncFailure(nil, "missing", errTimeout) {
		t.Fatal("a classified failure for an unknown peer should still be handled")
	}
}

func TestIsSyncCancellation(t *testing.T) {
	t.Parallel()

	for _, base := range []error{errCanceled, errCancelContentProcessing, errTerminated, errCancelStateFetch, snap.ErrCancelled} {
		if !isSyncCancellation(base) {
			t.Fatalf("bare %v should be a cancellation", base)
		}
		if wrapped := fmt.Errorf("%w: %w", errInvalidChain, base); !isSyncCancellation(wrapped) {
			t.Fatalf("wrapped %v should be a cancellation", base)
		}
	}
	if isSyncCancellation(errInvalidChain) {
		t.Fatal("an invalid chain is not a cancellation")
	}
	if isSyncCancellation(errTimeout) {
		t.Fatal("a timeout is not a cancellation")
	}
	if isSyncCancellation(nil) {
		t.Fatal("a nil error is not a cancellation")
	}
}

func TestDropPeerForResponseWithoutDropper(t *testing.T) {
	t.Parallel()

	downloader := &Downloader{peers: newPeerSet()}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	downloader.dropPeerForResponse(peer, peerFailureTooOld, errTooOld)
}

func TestJailSurvivesReconnect(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()

	peer := newPeerConnection("jailed-peer", eth.ETH69, nil, log.New())
	if err := ps.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	peer.backoffFor(peerJailBackoff)
	ps.recordJail(peer, peer.backoffExpiry())

	if err := ps.Unregister(peer.id); err != nil {
		t.Fatalf("failed to unregister peer: %v", err)
	}

	reconnected := newPeerConnection("jailed-peer", eth.ETH69, nil, log.New())
	if err := ps.Register(reconnected); err != nil {
		t.Fatalf("failed to re-register peer: %v", err)
	}
	if !reconnected.backedOff() {
		t.Fatal("reconnecting peer should remain jailed for the remaining duration")
	}
}

func TestRecordJailPrunesExpiredEntries(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	expired := newPeerConnection("expired", eth.ETH69, nil, log.New())
	ps.recordJail(expired, time.Now().Add(-time.Second))
	if _, ok := ps.jailed["expired"]; ok {
		t.Fatal("already-expired jail entry should not be stored")
	}

	active := newPeerConnection("active", eth.ETH69, nil, log.New())
	stale := newPeerConnection("stale", eth.ETH69, nil, log.New())
	ps.recordJail(active, time.Now().Add(peerJailBackoff))
	ps.recordJail(stale, time.Now().Add(-time.Second))
	if _, ok := ps.jailed["stale"]; ok {
		t.Fatal("expired entry should be pruned on access")
	}
	if _, ok := ps.jailed["active"]; !ok {
		t.Fatal("active jail entry should be retained")
	}
}

func TestRecordJailPropagatesToLivePeer(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	live := newPeerConnection("dup", eth.ETH69, nil, log.New())
	if err := ps.Register(live); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	if live.backedOff() {
		t.Fatal("freshly registered peer should not be backed off")
	}

	until := time.Now().Add(peerJailBackoff)
	ps.recordJail(live, until)

	if !live.backedOff() {
		t.Fatal("recordJail must push the jail backoff onto the already-registered peer")
	}
	if got := live.backoffExpiry(); got.Before(until) {
		t.Fatalf("live peer backoff not lifted to jail expiry: have %v, want >= %v", got, until)
	}
}

func TestPersistentBackoffPrunesExpiredEntries(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	ps.jailed["expired-jail"] = now.Add(-time.Second)
	if remaining := ps.persistentBackoff("expired-jail"); remaining != 0 {
		t.Fatalf("expired jail should report no backoff: have %v, want 0", remaining)
	}
	if _, ok := ps.jailed["expired-jail"]; ok {
		t.Fatal("expired jail entry should be pruned on read")
	}

	ps.jailed["jailed"] = now.Add(peerJailBackoff)
	if remaining := ps.persistentBackoff("jailed"); remaining <= 0 {
		t.Fatalf("active jail should report remaining backoff: have %v, want > 0", remaining)
	}

	if remaining := ps.persistentBackoff("unknown"); remaining != 0 {
		t.Fatalf("unknown peer should report no backoff: have %v, want 0", remaining)
	}
}

func TestLiveOrCapturedPrefersRegisteredPeer(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet()}
	captured := newPeerConnection("peer", eth.ETH69, nil, log.New())

	if d.liveOrCaptured(captured, captured.id) != captured {
		t.Fatal("with no registered peer the captured handle must be used")
	}

	live := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(live); err != nil {
		t.Fatalf("failed to register live peer: %v", err)
	}
	if d.liveOrCaptured(captured, captured.id) != live {
		t.Fatal("a registered peer must supersede the captured handle")
	}

	if d.liveOrCaptured(nil, "absent") != nil {
		t.Fatal("an absent peer with no captured handle must be nil")
	}
}

func TestPeerBackoffStateTransitions(t *testing.T) {
	t.Parallel()

	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	peer.backoffFor(0)
	if !peer.backoffExpiry().IsZero() {
		t.Fatalf("zero-duration backoff should leave expiry unset: have %v", peer.backoffExpiry())
	}
	if peer.backedOff() {
		t.Fatal("peer should not be backed off for zero duration")
	}

	peer.backoffFor(30 * time.Second)
	if !peer.backedOff() {
		t.Fatal("peer should be backed off for positive duration")
	}

	peer.lock.Lock()
	peer.backoff = time.Now().Add(-time.Second)
	peer.lock.Unlock()

	if remaining := peer.backoffRemaining(); remaining != 0 {
		t.Fatalf("expired backoff mismatch: have %v, want 0", remaining)
	}

	peer.lock.RLock()
	expired := peer.backoff.IsZero()
	peer.lock.RUnlock()
	if !expired {
		t.Fatal("expired backoff was not cleared")
	}
}

func TestPrunedSidechainJailsPeer(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet()}
	peer := newPeerConnection("ghost", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	ghostErr := fmt.Errorf("%w: %s", errInvalidChain, sidechainGhostStateMsg)
	d.respondToPeer(peer, peerFailurePrunedSidechain, ghostErr)
	if !peer.backedOff() {
		t.Fatal("a pruned-sidechain attack must bench the peer")
	}
	if _, ok := d.peers.jailed[peer.id]; !ok {
		t.Fatal("a pruned-sidechain attack must jail the peer")
	}
}

func TestPrunedSidechainEscalatesToDrop(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("ghost", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	ghostErr := fmt.Errorf("%w: %s", errInvalidChain, sidechainGhostStateMsg)

	for i := 1; i < prunedSidechainDropThreshold; i++ {
		d.respondToPeer(peer, peerFailurePrunedSidechain, ghostErr)
		if remaining := peer.backoffRemaining(); remaining <= peerSoftBackoff {
			t.Fatalf("ghost-state strike %d should jail the peer: have %v, want > %v", i, remaining, peerSoftBackoff)
		}
		if until, ok := d.peers.jailed[peer.id]; !ok || time.Until(until) <= peerSoftBackoff {
			t.Fatalf("ghost-state strike %d should record a long jail", i)
		}
		select {
		case id := <-dropped:
			t.Fatalf("ghost-state strike %d must jail, not drop: %q", i, id)
		default:
		}
	}

	d.respondToPeer(peer, peerFailurePrunedSidechain, ghostErr)
	select {
	case id := <-dropped:
		if id != peer.id {
			t.Fatalf("dropped wrong peer: have %q, want %q", id, peer.id)
		}
	default:
		t.Fatal("a peer that keeps launching ghost-state attacks should eventually be dropped")
	}
	if _, ok := d.peers.ghostStrikes[peer.id]; ok {
		t.Fatal("dropping a peer for repeated ghost-state should clear its tally")
	}
}

func TestRecordSoftFailureDecays(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	if got := ps.recordSoftFailure("peer", now); got != 1 {
		t.Fatalf("first strike count mismatch: have %d, want 1", got)
	}
	if got := ps.recordSoftFailure("peer", now.Add(time.Minute)); got != 2 {
		t.Fatalf("second strike count mismatch: have %d, want 2", got)
	}
	if got := ps.recordSoftFailure("peer", now.Add(softFailureWindow+time.Second)); got != 1 {
		t.Fatalf("a strike past the window should reset the tally: have %d, want 1", got)
	}
}

func TestRecordSoftFailureIsolatesPeers(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()
	ps.recordSoftFailure("a", now)
	ps.recordSoftFailure("a", now)

	if got := ps.recordSoftFailure("b", now); got != 1 {
		t.Fatalf("distinct peers must not share a tally: have %d, want 1", got)
	}

	ps.clearSoftFailures("a")
	if got := ps.recordSoftFailure("a", now); got != 1 {
		t.Fatalf("clearing must reset the tally: have %d, want 1", got)
	}
}

func TestReportBadBlockWithoutReporter(t *testing.T) {
	t.Parallel()

	d := &Downloader{}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})

	d.reportBadBlock(block)
}

func TestReportBadBlockBoundsError(t *testing.T) {
	t.Parallel()

	called := false
	d := &Downloader{
		badBlock: func(*types.Header, *types.Header) { called = true },
		skeleton: &skeleton{db: rawdb.NewMemoryDatabase()},
	}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})

	d.reportBadBlock(block)
	if called {
		t.Fatal("badBlock should not be invoked when beacon bounds are unavailable")
	}
}

func TestHandleSyncFailureUnclassified(t *testing.T) {
	t.Parallel()

	downloader := &Downloader{peers: newPeerSet()}
	if downloader.handleSyncFailure(nil, "peer", errors.New("unclassified")) {
		t.Fatal("unclassified error should not trigger a peer response")
	}
}

func TestHandleSyncFailureClassified(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	if !d.handleSyncFailure(peer, peer.id, errInvalidChain) {
		t.Fatal("a classified sync failure should be handled")
	}
	select {
	case id := <-dropped:
		if id != peer.id {
			t.Fatalf("dropped wrong peer: have %q, want %q", id, peer.id)
		}
	default:
		t.Fatal("a handled invalid chain should drop the peer")
	}
}

func TestHandleSyncFailureTimeoutBacksOff(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	if !d.handleSyncFailure(peer, peer.id, errTimeout) {
		t.Fatal("a classified timeout should be handled")
	}
	select {
	case id := <-dropped:
		t.Fatalf("a transient timeout must not drop the peer: %q", id)
	default:
	}
	if !peer.backedOff() {
		t.Fatal("a transient timeout should back the peer off")
	}
	if _, ok := d.peers.jailed[peer.id]; !ok {
		t.Fatal("a soft backoff should persist across reconnect")
	}
}

// TestHandleSyncFailureInvalidWitnessBacksOff covers the downloader's half of
// the bad-witness gap. A stateless import that rejects a witness used to be
// reported as a bare errInvalidBody, which classifySyncFailure deliberately
// ignores because body-hash faults are answered at delivery time — so the
// failure produced no backoff and no rotation, and the sync loop re-picked the
// same best-TD peer and re-downloaded the same unusable witness. It must back
// the peer off — which moves the next attempt to a different source — without
// dropping it, since a witness the producer generated wrong makes every peer
// look equally guilty.
func TestHandleSyncFailureInvalidWitnessBacksOff(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	err := fmt.Errorf("%w: %w", errInvalidWitness, core.ErrWitnessInvalid)
	if !d.handleSyncFailure(peer, peer.id, err) {
		t.Fatal("a stateless import failure should be handled")
	}
	select {
	case id := <-dropped:
		t.Fatalf("a bad witness must not drop the peer: %q", id)
	default:
	}
	if !peer.backedOff() {
		t.Fatal("a bad witness should back the peer off so the next sync picks another source")
	}

	if reason, ok := classifySyncFailure(err); !ok || reason != peerFailureInvalidWitness {
		t.Fatalf("classifySyncFailure(errInvalidWitness) = (%q, %v), want (%q, true)", reason, ok, peerFailureInvalidWitness)
	}
}

func TestBackoffEscalatesToJailAfterRepeatStrikes(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	for i := 0; i < softFailureJailThreshold-1; i++ {
		d.respondToPeer(peer, peerFailureTimeout, errTimeout)
		if remaining := peer.backoffRemaining(); remaining > peerSoftBackoff {
			t.Fatalf("soft strike %d should stay within soft backoff: have %v, want <= %v", i+1, remaining, peerSoftBackoff)
		}
		clearPeerBackoff(peer)
	}

	d.respondToPeer(peer, peerFailureTimeout, errTimeout)
	if remaining := peer.backoffRemaining(); remaining <= peerSoftBackoff {
		t.Fatalf("the escalating strike should jail the peer: have %v, want > %v", remaining, peerSoftBackoff)
	}
	if until, ok := d.peers.jailed[peer.id]; !ok || time.Until(until) <= peerSoftBackoff {
		t.Fatal("the escalating strike should record a long jail")
	}
	if _, ok := d.peers.softStrikes[peer.id]; ok {
		t.Fatal("escalating soft failures to a jail should clear the soft tally")
	}
	select {
	case id := <-dropped:
		t.Fatalf("escalation must jail, not drop: %q", id)
	default:
	}
}

func clearPeerBackoff(peer *peerConnection) {
	peer.lock.Lock()
	peer.backoff = time.Time{}
	peer.lock.Unlock()
}

func TestBackoffForClaim(t *testing.T) {
	t.Parallel()

	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if !peer.backoffForClaim(peerSoftBackoff) {
		t.Fatal("the first claim on an un-benched peer should succeed")
	}
	if !peer.backedOff() {
		t.Fatal("a claimed peer should be benched")
	}
	if peer.backoffForClaim(peerSoftBackoff) {
		t.Fatal("a claim on an already-benched peer should report it was already benched")
	}
}

func TestBackoffSoftFailureDedupsWhileBenched(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := ps.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	if penalized, _ := ps.backoffSoftFailure(peer); !penalized {
		t.Fatal("the first soft failure should penalize the peer")
	}
	if penalized, _ := ps.backoffSoftFailure(peer); penalized {
		t.Fatal("a peer already benched must not be penalized again for a concurrent failure")
	}
	if got := ps.softStrikes[peer.id].count; got != 1 {
		t.Fatalf("a benched peer must not accrue a second strike: have %d, want 1", got)
	}
}

func TestRecordMismatchDecays(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	if got := ps.recordMismatch("peer", now); got != 1 {
		t.Fatalf("first mismatch count mismatch: have %d, want 1", got)
	}
	if got := ps.recordMismatch("peer", now.Add(time.Minute)); got != 2 {
		t.Fatalf("second mismatch count mismatch: have %d, want 2", got)
	}
	if got := ps.recordMismatch("peer", now.Add(whitelistMismatchWindow+time.Second)); got != 1 {
		t.Fatalf("a mismatch past the window should reset the tally: have %d, want 1", got)
	}
}

func TestRecordMismatchIsolatesPeers(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()
	ps.recordMismatch("a", now)
	ps.recordMismatch("a", now)

	if got := ps.recordMismatch("b", now); got != 1 {
		t.Fatalf("distinct peers must not share a mismatch tally: have %d, want 1", got)
	}

	ps.clearMismatches("a")
	if got := ps.recordMismatch("a", now); got != 1 {
		t.Fatalf("clearing must reset the mismatch tally: have %d, want 1", got)
	}
}

func TestRecordGhostStateDecays(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	if got := ps.recordGhostState("peer", now); got != 1 {
		t.Fatalf("first ghost-state count mismatch: have %d, want 1", got)
	}
	if got := ps.recordGhostState("peer", now.Add(time.Minute)); got != 2 {
		t.Fatalf("second ghost-state count mismatch: have %d, want 2", got)
	}
	if got := ps.recordGhostState("peer", now.Add(prunedSidechainWindow+time.Second)); got != 1 {
		t.Fatalf("a ghost-state past the window should reset the tally: have %d, want 1", got)
	}
}

func TestRecordGhostStateIsolatesPeers(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()
	ps.recordGhostState("a", now)
	ps.recordGhostState("a", now)

	if got := ps.recordGhostState("b", now); got != 1 {
		t.Fatalf("distinct peers must not share a ghost-state tally: have %d, want 1", got)
	}

	ps.clearGhostStates("a")
	if got := ps.recordGhostState("a", now); got != 1 {
		t.Fatalf("clearing must reset the ghost-state tally: have %d, want 1", got)
	}
}

func TestRecordSoftFailureWindowBoundary(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	if got := ps.recordSoftFailure("p", now); got != 1 {
		t.Fatalf("first strike count mismatch: have %d, want 1", got)
	}
	if got := ps.recordSoftFailure("p", now.Add(softFailureWindow)); got != 2 {
		t.Fatalf("a strike exactly at the window edge must not reset the tally: have %d, want 2", got)
	}
}

func TestRecordMismatchWindowBoundary(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	if got := ps.recordMismatch("p", now); got != 1 {
		t.Fatalf("first mismatch count mismatch: have %d, want 1", got)
	}
	if got := ps.recordMismatch("p", now.Add(whitelistMismatchWindow)); got != 2 {
		t.Fatalf("a mismatch exactly at the window edge must not reset the tally: have %d, want 2", got)
	}
}

func TestRecordGhostStateWindowBoundary(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()

	if got := ps.recordGhostState("p", now); got != 1 {
		t.Fatalf("first ghost-state count mismatch: have %d, want 1", got)
	}
	if got := ps.recordGhostState("p", now.Add(prunedSidechainWindow)); got != 2 {
		t.Fatalf("a ghost-state exactly at the window edge must not reset the tally: have %d, want 2", got)
	}
}

func TestRecordJailPrunesExpiredOnInterval(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	ps.jailed["expired"] = time.Now().Add(-time.Hour)
	ps.lastJailPrune = time.Now().Add(-2 * backoffPruneInterval)

	peer := newPeerConnection("fresh", eth.ETH69, nil, log.New())
	ps.recordJail(peer, time.Now().Add(peerJailBackoff))

	if _, ok := ps.jailed["expired"]; ok {
		t.Fatal("an expired jail entry should be pruned once the prune interval elapses")
	}
	if _, ok := ps.jailed["fresh"]; !ok {
		t.Fatal("the active jail entry should be recorded")
	}
}

func TestRecordJailKeepsLongerExpiry(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := ps.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	long := time.Now().Add(peerJailBackoff)
	ps.recordJail(peer, long)
	ps.recordJail(peer, time.Now().Add(peerSoftBackoff))

	if got := ps.jailed[peer.id]; got.Before(long) {
		t.Fatalf("a shorter jail must not overwrite a longer one: have %v, want >= %v", got, long)
	}
}

func TestStrikeMapsAreBounded(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	now := time.Now()
	for i := 0; i < maxStrikeEntries+10; i++ {
		id := fmt.Sprintf("peer-%d", i)
		ps.recordSoftFailure(id, now)
		ps.recordMismatch(id, now)
		ps.recordGhostState(id, now)
		ps.recordJail(newPeerConnection(fmt.Sprintf("jail-%d", i), eth.ETH69, nil, log.New()), now.Add(time.Hour))
	}

	if got := len(ps.softStrikes); got > maxStrikeEntries {
		t.Fatalf("softStrikes exceeded cap: have %d, want <= %d", got, maxStrikeEntries)
	}
	if got := len(ps.mismatchStrikes); got > maxStrikeEntries {
		t.Fatalf("mismatchStrikes exceeded cap: have %d, want <= %d", got, maxStrikeEntries)
	}
	if got := len(ps.ghostStrikes); got > maxStrikeEntries {
		t.Fatalf("ghostStrikes exceeded cap: have %d, want <= %d", got, maxStrikeEntries)
	}
	if got := len(ps.jailed); got > maxStrikeEntries {
		t.Fatalf("jailed exceeded cap: have %d, want <= %d", got, maxStrikeEntries)
	}
}

func TestStrikeEvictionRemovesOldest(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	base := time.Now()
	for i := 0; i < maxStrikeEntries; i++ {
		ps.recordSoftFailure(fmt.Sprintf("peer-%d", i), base.Add(time.Duration(i)*time.Millisecond))
	}
	ps.recordSoftFailure("newcomer", base.Add(time.Duration(maxStrikeEntries)*time.Millisecond))

	if _, ok := ps.softStrikes["peer-0"]; ok {
		t.Fatal("eviction should remove the oldest strike record")
	}
	if _, ok := ps.softStrikes["newcomer"]; !ok {
		t.Fatal("the newest strike record should be retained")
	}
	if _, ok := ps.softStrikes["peer-1"]; !ok {
		t.Fatal("a newer strike record should not be evicted")
	}
}

func TestJailEvictionRemovesSoonestExpiry(t *testing.T) {
	t.Parallel()

	ps := newPeerSet()
	base := time.Now().Add(time.Hour)
	for i := 0; i < maxStrikeEntries; i++ {
		peer := newPeerConnection(fmt.Sprintf("jail-%d", i), eth.ETH69, nil, log.New())
		ps.recordJail(peer, base.Add(time.Duration(i)*time.Millisecond))
	}
	newcomer := newPeerConnection("jail-new", eth.ETH69, nil, log.New())
	ps.recordJail(newcomer, base.Add(time.Duration(maxStrikeEntries)*time.Millisecond))

	if _, ok := ps.jailed["jail-0"]; ok {
		t.Fatal("jail eviction should remove the soonest-expiring entry")
	}
	if _, ok := ps.jailed["jail-new"]; !ok {
		t.Fatal("the new jail entry should be retained")
	}
}

func TestDropForResponseBenchesPeer(t *testing.T) {
	t.Parallel()

	d := &Downloader{peers: newPeerSet(), dropPeer: func(string) {}}
	peer := newPeerConnection("bad", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	d.dropPeerForResponse(peer, peerFailureInvalidChain, errInvalidChain)

	until, ok := d.peers.jailed[peer.id]
	if !ok {
		t.Fatal("a dropped peer should be benched so a same-id reconnect stays jailed")
	}
	if remaining := time.Until(until); remaining <= peerJailBackoff {
		t.Fatalf("a drop should bench longer than a plain jail: have %v, want > %v", remaining, peerJailBackoff)
	}
}

func TestWhitelistMismatchDoesNotDropOnFirstStrike(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}

	mismatchErr := fmt.Errorf("%w: %w", errInvalidChain, whitelist.ErrMismatch)
	if !d.handleSyncFailure(peer, peer.id, mismatchErr) {
		t.Fatal("a whitelist mismatch should be handled")
	}
	select {
	case id := <-dropped:
		t.Fatalf("a first whitelist mismatch must not drop the peer: %q", id)
	default:
	}
	if remaining := peer.backoffRemaining(); remaining <= 0 || remaining > peerSoftBackoff {
		t.Fatalf("a first whitelist mismatch should soft-backoff the peer: have %v, want (0, %v]", remaining, peerSoftBackoff)
	}
}

func TestWhitelistMismatchEscalatesBackoffJailDrop(t *testing.T) {
	t.Parallel()

	dropped := make(chan string, 1)
	d := &Downloader{peers: newPeerSet(), dropPeer: func(id string) { dropped <- id }}
	peer := newPeerConnection("peer", eth.ETH69, nil, log.New())
	if err := d.peers.Register(peer); err != nil {
		t.Fatalf("failed to register peer: %v", err)
	}
	mismatchErr := fmt.Errorf("%w: %w", errInvalidChain, whitelist.ErrMismatch)

	for i := 1; i < whitelistMismatchJailThreshold; i++ {
		d.respondToPeer(peer, peerFailureWhitelistMismatch, mismatchErr)
		if remaining := peer.backoffRemaining(); remaining > peerSoftBackoff {
			t.Fatalf("mismatch strike %d should stay within soft backoff: have %v, want <= %v", i, remaining, peerSoftBackoff)
		}
		select {
		case id := <-dropped:
			t.Fatalf("mismatch strike %d must not drop the peer: %q", i, id)
		default:
		}
	}

	for i := whitelistMismatchJailThreshold; i < whitelistMismatchDropThreshold; i++ {
		d.respondToPeer(peer, peerFailureWhitelistMismatch, mismatchErr)
		if remaining := peer.backoffRemaining(); remaining <= peerSoftBackoff {
			t.Fatalf("mismatch strike %d should jail the peer: have %v, want > %v", i, remaining, peerSoftBackoff)
		}
		if until, ok := d.peers.jailed[peer.id]; !ok || time.Until(until) <= peerSoftBackoff {
			t.Fatalf("mismatch strike %d should record a long jail", i)
		}
		select {
		case id := <-dropped:
			t.Fatalf("mismatch strike %d must jail, not drop: %q", i, id)
		default:
		}
	}

	d.respondToPeer(peer, peerFailureWhitelistMismatch, mismatchErr)
	select {
	case id := <-dropped:
		if id != peer.id {
			t.Fatalf("dropped wrong peer: have %q, want %q", id, peer.id)
		}
	default:
		t.Fatal("a persistent whitelist mismatch should eventually drop the peer")
	}
	if _, ok := d.peers.mismatchStrikes[peer.id]; ok {
		t.Fatal("dropping a peer for persistent mismatch should clear its tally")
	}
}
