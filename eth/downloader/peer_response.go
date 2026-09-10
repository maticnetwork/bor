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
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/log"
)

const (
	peerJailBackoff = 5 * time.Minute
	peerSoftBackoff = 30 * time.Second
	peerDropBackoff = 30 * time.Minute

	softFailureWindow        = 10 * time.Minute
	softFailureJailThreshold = 4

	whitelistMismatchWindow        = 30 * time.Minute
	whitelistMismatchJailThreshold = 2
	whitelistMismatchDropThreshold = 4

	prunedSidechainWindow        = 30 * time.Minute
	prunedSidechainDropThreshold = 2

	backoffPruneInterval = time.Minute

	maxStrikeEntries = 4096
)

type peerFailureReason string

const (
	peerFailureInvalidChain      peerFailureReason = "invalid-chain"
	peerFailureInvalidWitness    peerFailureReason = "invalid-witness"
	peerFailurePrunedSidechain   peerFailureReason = "pruned-sidechain"
	peerFailureBadPeer           peerFailureReason = "bad-peer"
	peerFailureTimeout           peerFailureReason = "timeout"
	peerFailureStalling          peerFailureReason = "stalling"
	peerFailureUnsynced          peerFailureReason = "unsynced"
	peerFailureEmptyHeaderSet    peerFailureReason = "empty-header-set"
	peerFailurePeersUnavailable  peerFailureReason = "peers-unavailable"
	peerFailureTooOld            peerFailureReason = "too-old"
	peerFailureInvalidAncestor   peerFailureReason = "invalid-ancestor"
	peerFailureWhitelistMismatch peerFailureReason = "whitelist-mismatch"
	peerFailureDisconnected      peerFailureReason = "disconnected"
	peerFailureNoRemote          peerFailureReason = "whitelist-no-remote"
)

type peerResponseAction uint8

const (
	peerResponseNone peerResponseAction = iota
	peerResponseDrop
	peerResponseBackoff
	peerResponseMismatch
	peerResponseGhostState
)

type peerResponseDecision struct {
	action  peerResponseAction
	backoff time.Duration
	reason  peerFailureReason
}

func (d *Downloader) liveOrCaptured(captured *peerConnection, id string) *peerConnection {
	if live := d.peers.Peer(id); live != nil {
		return live
	}
	return captured
}

func (d *Downloader) handleSyncFailure(peer *peerConnection, id string, err error) bool {
	reason, ok := classifySyncFailure(err)
	if !ok {
		return false
	}
	peer = d.liveOrCaptured(peer, id)
	if peer == nil {
		log.Debug("Downloader peer response skipped for unknown peer", "peer", id, "reason", reason, "err", err)
		return true
	}
	d.respondToPeer(peer, reason, err)
	return true
}

func classifySyncFailure(err error) (peerFailureReason, bool) {
	switch {
	case isPrunedSidechainMismatch(err):
		return peerFailurePrunedSidechain, true
	case isTransientFailure(err):
		return peerFailureTimeout, true
	case errors.Is(err, eth.ErrDisconnected):
		return peerFailureDisconnected, true
	case isWhitelistMismatch(err):
		return peerFailureWhitelistMismatch, true
	case errors.Is(err, whitelist.ErrNoRemote):
		return peerFailureNoRemote, true
	case errors.Is(err, ErrPeersUnavailable), errors.Is(err, errNoPeers):
		return peerFailurePeersUnavailable, true
	case errors.Is(err, errInvalidWitness):
		// A witness that fails stateless validation says the bytes are
		// unusable, not that this peer forged them: the witness may have come
		// from another peer in the fetch queue, and a producer that generated
		// it wrong makes every peer look equally guilty. Back the peer off
		// rather than dropping it — that alone moves the next sync attempt to a
		// different source, which is the recovery we want, without walking the
		// whole peer set on a bad block.
		return peerFailureInvalidWitness, true
	case errors.Is(err, errInvalidChain):
		return peerFailureInvalidChain, true
	case errors.Is(err, errBadPeer):
		return peerFailureBadPeer, true
	case errors.Is(err, errStallingPeer):
		return peerFailureStalling, true
	case errors.Is(err, errUnsyncedPeer), errors.Is(err, errNoAncestorFound):
		return peerFailureUnsynced, true
	case errors.Is(err, errEmptyHeaderSet):
		return peerFailureEmptyHeaderSet, true
	case errors.Is(err, errTooOld):
		return peerFailureTooOld, true
	case errors.Is(err, errInvalidAncestor):
		return peerFailureInvalidAncestor, true
	default:
		return "", false
	}
}

func isWhitelistMismatch(err error) bool {
	return errors.Is(err, whitelist.ErrMismatch)
}

func isSyncCancellation(err error) bool {
	return errors.Is(err, errCanceled) || errors.Is(err, errCancelContentProcessing) || errors.Is(err, errTerminated) || errors.Is(err, errCancelStateFetch) || errors.Is(err, snap.ErrCancelled)
}

func isTransientFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, errInvalidChain) || errors.Is(err, errBadPeer) || errors.Is(err, errInvalidAncestor) {
		return false
	}
	return strings.Contains(err.Error(), context.DeadlineExceeded.Error())
}

func (d *Downloader) respondToPeer(peer *peerConnection, reason peerFailureReason, err error) {
	decision := peer.responseDecision(reason)

	switch decision.action {
	case peerResponseBackoff:
		d.backoffPeer(peer, decision, err)
	case peerResponseMismatch:
		d.escalateMismatch(peer, decision, err)
	case peerResponseGhostState:
		d.escalateGhostState(peer, decision, err)
	case peerResponseDrop:
		d.dropPeerForResponse(peer, decision.reason, err)
	case peerResponseNone:
		peer.log.Warn("Synchronisation stalled, no peer action taken", "reason", reason, "err", err)
	}
}

func (d *Downloader) benchPeer(peer *peerConnection, backoff time.Duration) {
	peer.backoffFor(backoff)
	d.peers.recordJail(peer, peer.backoffExpiry())
}

func (d *Downloader) jailPeer(peer *peerConnection, backoff time.Duration, reason peerFailureReason, err error) {
	d.benchPeer(peer, backoff)
	peerJailMeter.Mark(1)
	peer.log.Warn("Downloader: locally jailing peer", "reason", reason, "err", err, "requested", common.PrettyDuration(backoff), "effective", common.PrettyDuration(peer.backoffRemaining()))
}

func (d *Downloader) backoffPeer(peer *peerConnection, decision peerResponseDecision, err error) {
	penalized, jailed := d.peers.backoffSoftFailure(peer)
	if !penalized {
		return
	}
	if jailed {
		peerJailMeter.Mark(1)
		peer.log.Warn("Downloader: escalating repeated soft failures to local jail", "reason", decision.reason, "err", err, "effective", common.PrettyDuration(peer.backoffRemaining()))
		return
	}
	peerSoftBackoffMeter.Mark(1)
	peer.log.Warn("Downloader: backing off peer", "reason", decision.reason, "err", err, "requested", common.PrettyDuration(decision.backoff), "effective", common.PrettyDuration(peer.backoffRemaining()))
}

func (d *Downloader) escalateMismatch(peer *peerConnection, decision peerResponseDecision, err error) {
	peerMismatchMeter.Mark(1)
	strikes := d.peers.recordMismatch(peer.id, time.Now())
	switch {
	case strikes >= whitelistMismatchDropThreshold:
		d.peers.clearMismatches(peer.id)
		peer.log.Warn("Downloader: dropping peer after persistent whitelist mismatch", "reason", decision.reason, "strikes", strikes, "err", err)
		d.dropPeerForResponse(peer, decision.reason, err)
	case strikes >= whitelistMismatchJailThreshold:
		peer.log.Warn("Downloader: escalating repeated whitelist mismatch to local jail", "reason", decision.reason, "strikes", strikes, "err", err)
		d.jailPeer(peer, peerJailBackoff, decision.reason, err)
	default:
		d.benchPeer(peer, decision.backoff)
		peer.log.Warn("Downloader: backing off peer after whitelist mismatch", "reason", decision.reason, "err", err, "strikes", strikes, "requested", common.PrettyDuration(decision.backoff), "effective", common.PrettyDuration(peer.backoffRemaining()))
	}
}

func (d *Downloader) escalateGhostState(peer *peerConnection, decision peerResponseDecision, err error) {
	peerGhostStateMeter.Mark(1)
	strikes := d.peers.recordGhostState(peer.id, time.Now())
	if strikes >= prunedSidechainDropThreshold {
		d.peers.clearGhostStates(peer.id)
		peer.log.Warn("Downloader: dropping peer after repeated sidechain ghost-state attacks", "reason", decision.reason, "strikes", strikes, "err", err)
		d.dropPeerForResponse(peer, decision.reason, err)
		return
	}
	peer.log.Warn("Downloader: jailing peer for sidechain ghost-state attack", "reason", decision.reason, "strikes", strikes, "err", err)
	d.jailPeer(peer, decision.backoff, decision.reason, err)
}

func (p *peerConnection) responseDecision(reason peerFailureReason) peerResponseDecision {
	decision := peerResponseDecision{
		reason: reason,
	}

	switch reason {
	case peerFailurePrunedSidechain:
		decision.action = peerResponseGhostState
		decision.backoff = peerJailBackoff
	case peerFailureInvalidChain, peerFailureBadPeer, peerFailureInvalidAncestor:
		decision.action = peerResponseDrop
	case peerFailureWhitelistMismatch:
		decision.action = peerResponseMismatch
		decision.backoff = peerSoftBackoff
	case peerFailureTimeout, peerFailureStalling, peerFailureUnsynced, peerFailureEmptyHeaderSet, peerFailureTooOld, peerFailureDisconnected, peerFailureInvalidWitness:
		decision.action = peerResponseBackoff
		decision.backoff = peerSoftBackoff
	default:
		decision.action = peerResponseNone
	}
	return decision
}

func (d *Downloader) dropPeerForResponse(peer *peerConnection, reason peerFailureReason, err error) {
	d.benchPeer(peer, peerDropBackoff)
	peer.log.Warn("Synchronisation failed, dropping peer", "reason", reason, "err", err, "mode", d.getMode())

	if d.dropPeer == nil {
		log.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", peer.id)
		return
	}
	peerDropResponseMeter.Mark(1)
	d.dropPeer(peer.id)
}

const sidechainGhostStateMsg = "sidechain ghost-state attack"

func isPrunedSidechainMismatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), sidechainGhostStateMsg)
}
