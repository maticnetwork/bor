package fetcher

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/log"
)

// WIT2 fast-path tuning: how the manager re-polls announce-only relayers that
// answer "body not ready yet" while still pulling the witness themselves.
const (
	// emptyResponseFastRetries is how many consecutive "body not ready yet"
	// (empty) responses we re-poll immediately before backing off. WIT2's fast
	// signed announce reaches us ahead of the body, so the only candidate body
	// source is often an announce-only relayer that has not finished pulling +
	// importing the block. The first couple of re-polls stay immediate so we
	// pick the body up the instant the relayer obtains it (the common case);
	// after that, a relayer answering empty is genuinely waiting on its own
	// upstream and re-polling it every ~gatherSlack only hammers it.
	emptyResponseFastRetries = 2

	// emptyResponseBaseBackoff / emptyResponseMaxBackoff bound the exponential
	// backoff applied to repeated empty responses past the fast-retry window.
	// The witness provably exists (a BP signed its hash) so we never give the
	// request up here; we only slow the poll cadence to avoid the empty-poll
	// storm observed on devnet (~15x the WIT1 empty-response count).
	emptyResponseBaseBackoff = 100 * time.Millisecond
	emptyResponseMaxBackoff  = 1 * time.Second
)

// cacheVerifiedWitnessForServing forwards canonical-encoded witness bytes
// (already verified against a BP-signed witness hash by the caller) to the
// handler so other peers can fetch them pre-import. No-op when no cache
// callback is configured (legacy WIT1-only paths) or when body is empty —
// the latter signals the WIT1 path with no signed hash on file, where
// caching unverified bytes would expose us to byte-blame from downstream
// peers.
func (m *witnessManager) cacheVerifiedWitnessForServing(blockHash common.Hash, body []byte, witnessHash common.Hash) {
	if m.parentCacheWitnessForServing == nil || len(body) == 0 {
		return
	}
	m.parentCacheWitnessForServing(blockHash, body, witnessHash)
}

// verifyAgainstSignedHash returns the canonically-encoded witness bytes and
// the BP-signed witness hash they match, when a signed hash is on file and
// verification succeeds. body is nil on the WIT1 path (no signed hash to
// verify against) so callers can skip the pre-import serving cache. ok is
// false when verification fails; the offending peer has already been
// reported. Local EncodeRLP failure on a successfully-decoded witness is
// the local node's bug, not peer misbehavior, so it does not drop the peer.
func (m *witnessManager) verifyAgainstSignedHash(peer string, hash common.Hash, witness *stateless.Witness) (body []byte, witnessHash common.Hash, ok bool) {
	if m.parentSignedWitnessHash == nil {
		return nil, common.Hash{}, true
	}
	expected, has := m.parentSignedWitnessHash(hash)
	if !has || m.isSignedHashQuarantined(hash) {
		// No signed hash on file, or it has been quarantined after distinct
		// servers repeatedly mismatched it (bad/stale producer hash): fall back
		// to the WIT1 path so import-time execution arbitrates the bytes.
		return nil, common.Hash{}, true
	}
	var buf bytes.Buffer
	if err := witness.EncodeRLP(&buf); err != nil {
		log.Warn("[wm] Failed to encode received witness for hash check", "peer", peer, "hash", hash, "err", err)
		m.handleWitnessFetchFailureExt(hash, "", fmt.Errorf("witness encode failed: %w", err), false)
		return nil, common.Hash{}, false
	}
	encoded := buf.Bytes()
	actual := stateless.WitnessCommitHash(encoded)
	if actual != expected {
		witnessByteMismatchMeter.Mark(1)
		// We cannot blame the byte-server on signed-hash disagreement alone:
		// the announcement only proves *some* BP signed *some* hash. A faulty
		// or malicious scheduled producer that signed a bogus hash would
		// otherwise weaponise this path to disconnect every honest peer
		// serving the canonical witness. Reject the bytes (don't cache for
		// serving), back off the pending request so another peer/announcement
		// gets tried, and let import-time execution validation pin blame.
		//
		// A single bad server is not enough to distrust the signed hash. But if
		// distinct servers all mismatch the same signed hash, the hash itself is
		// the likely culprit (bad/stale producer signature): quarantine it so
		// the next fetch falls back to WIT1 immediately instead of stalling the
		// block until the signed announcement's TTL expires.
		quarantined, firstMismatchForPeer := m.recordSignedHashMismatch(hash, peer)
		if quarantined {
			log.Warn("[wm] BP-signed witness hash repeatedly unmatched by distinct servers; quarantining to WIT1 fallback so the block can import",
				"block", hash, "expected", expected)
		} else {
			log.Warn("[wm] Witness bytes do not match BP-signed hash; not caching, retrying with another peer",
				"peer", peer, "block", hash, "expected", expected, "actual", actual)
		}
		// Penalize the server. It returned a NON-EMPTY witness whose bytes
		// contradict the on-file signed commitment — provably misbehaving relative
		// to an honest empty "not ready" response. This is a STRIKE, not a drop:
		// a faulty/malicious BP that signed a bogus hash makes honest servers
		// mismatch too, so a single mismatch stays tolerated, but a sybil that
		// repeatedly serves garbage (to weaponise the distinct-server quarantine as
		// a targeted WIT1 downgrade, or to feed bytes that fail import) accrues
		// toward disconnect instead of mismatching for free. Import-time execution
		// remains the final arbiter of byte content.
		//
		// Strike only a peer's FIRST mismatch per block. The mismatch path keeps
		// the pending request alive and reschedules ~gatherSlack later; when a
		// block has a single announce-known peer, resolveWitnessFetchPeer returns
		// that same peer on every retry, so striking each time would jail an honest
		// sole witness source in ~1s — inverting the "single mismatch tolerated"
		// guarantee. Per-(peer, block) dedup keeps the cross-block sybil penalty
		// (distinct blocks each strike once) and the distinct-server quarantine
		// intact while removing the self-DoS on sparse topologies.
		if firstMismatchForPeer && peer != "" && m.parentStrikeWitnessServer != nil {
			m.parentStrikeWitnessServer(peer)
		}
		m.handleWitnessFetchFailureExt(hash, "", errors.New("witness hash mismatch"), false)
		return nil, common.Hash{}, false
	}
	// Bytes matched: forget any earlier mismatch noise for this block.
	m.clearSignedHashMismatch(hash)
	return encoded, expected, true
}

// signedHashMismatchQuarantineThreshold is how many DISTINCT servers must serve
// bytes that fail the BP-signed-hash check for a block before we stop trusting
// that signed hash and fall back to WIT1. One bad server cannot trigger it; a
// hash that distinct honest servers all disagree with is itself the suspect.
const signedHashMismatchQuarantineThreshold = 2

// wit2QuarantineStateTTL bounds how long a wit2MismatchPeers/wit2Quarantined
// entry can survive without being touched again, so a mismatch that arrives
// after the block's own pending-removal exits already ran (and therefore will
// never call clearSignedHashMismatch again) still gets cleaned up eventually
// instead of leaking for the process lifetime. Generous relative to the
// seconds-scale window a block normally resolves in, so it never interferes
// with legitimate in-flight quarantine tracking.
const wit2QuarantineStateTTL = 2 * time.Minute

// isSignedHashQuarantined reports whether the signed hash for a block has been
// quarantined (distinct servers repeatedly mismatched it).
func (m *witnessManager) isSignedHashQuarantined(hash common.Hash) bool {
	m.wit2QuarantineMu.Lock()
	defer m.wit2QuarantineMu.Unlock()
	_, ok := m.wit2Quarantined[hash]
	return ok
}

// recordSignedHashMismatch records that peer served bytes mismatching the
// signed hash for block hash. quarantined is true the moment the distinct-server
// threshold is reached and the hash is newly quarantined (so the caller logs the
// downgrade exactly once). firstForPeer is true only when this is peer's first
// recorded mismatch for this block, so the caller can strike a peer at most once
// per (peer, block) rather than once per retry — the retry loop re-hits the same
// sole announce-known peer every ~gatherSlack, and an honest server serving
// canonical bytes against a bad/stale BP hash would otherwise be jailed in ~1s.
// An empty peer string is ignored.
func (m *witnessManager) recordSignedHashMismatch(hash common.Hash, peer string) (quarantined bool, firstForPeer bool) {
	if peer == "" {
		return false, false
	}
	m.wit2QuarantineMu.Lock()
	defer m.wit2QuarantineMu.Unlock()
	m.wit2StateExpiry[hash] = time.Now().Add(wit2QuarantineStateTTL)
	if _, done := m.wit2Quarantined[hash]; done {
		return false, false
	}
	peers := m.wit2MismatchPeers[hash]
	if peers == nil {
		peers = make(map[string]struct{})
		m.wit2MismatchPeers[hash] = peers
	}
	_, seen := peers[peer]
	firstForPeer = !seen
	peers[peer] = struct{}{}
	if len(peers) >= signedHashMismatchQuarantineThreshold {
		m.wit2Quarantined[hash] = struct{}{}
		delete(m.wit2MismatchPeers, hash)
		return true, firstForPeer
	}
	return false, firstForPeer
}

// clearSignedHashMismatch drops all mismatch/quarantine state for a block. Call
// it once the block's witness is resolved or the request is abandoned so the
// maps stay bounded by in-flight fetches.
func (m *witnessManager) clearSignedHashMismatch(hash common.Hash) {
	m.wit2QuarantineMu.Lock()
	defer m.wit2QuarantineMu.Unlock()
	delete(m.wit2MismatchPeers, hash)
	delete(m.wit2Quarantined, hash)
	delete(m.wit2StateExpiry, hash)
}

// cleanupWit2QuarantineState removes wit2MismatchPeers/wit2Quarantined entries
// whose TTL has lapsed. Backstops the 4 pending-removal exits for the case a
// mismatch response arrives after they already ran for a hash (so none of
// them will run again for it): without this sweep such an entry would leak
// for the process lifetime. Called from the same ticker that expires
// witnessUnavailable.
func (m *witnessManager) cleanupWit2QuarantineState() {
	now := time.Now()
	cleaned := 0
	m.wit2QuarantineMu.Lock()
	for hash, expiry := range m.wit2StateExpiry {
		if now.After(expiry) {
			delete(m.wit2StateExpiry, hash)
			delete(m.wit2MismatchPeers, hash)
			delete(m.wit2Quarantined, hash)
			cleaned++
		}
	}
	m.wit2QuarantineMu.Unlock()
	if cleaned > 0 {
		log.Debug("[wm] Cleaned up expired wit2 quarantine state", "removed", cleaned)
	}
}

// handleWitnessBodyNotReady backs off a pending witness request after an empty
// ("body not ready yet") response, without dropping the responder and without
// giving the request up. On the WIT2 fast path the signed announce reaches us
// ahead of the body, so the only candidate source is frequently an
// announce-only relayer still pulling+importing the block; it answers empty
// until it has the bytes. The first emptyResponseFastRetries re-polls stay
// immediate to catch the body the instant the relayer obtains it; beyond that
// we back off exponentially (capped) so a relayer that is itself waiting
// upstream is not hammered every ~gatherSlack. The witness provably exists — a
// BP signed its hash — so we never discard the request here.
func (m *witnessManager) handleWitnessBodyNotReady(hash common.Hash) {
	m.mu.Lock()
	if state := m.pending[hash]; state != nil && state.announce != nil {
		state.emptyRetries++
		state.announce.time = time.Now().Add(emptyResponseBackoff(state.emptyRetries))
	}
	m.mu.Unlock()

	m.rescheduleWitness()
}

// emptyResponseBackoff returns how far into the future the next re-poll should
// be deferred after n consecutive empty responses. The first
// emptyResponseFastRetries attempts return 0 (re-poll on the next tick); past
// that the delay doubles from emptyResponseBaseBackoff up to
// emptyResponseMaxBackoff.
func emptyResponseBackoff(n int) time.Duration {
	if n <= emptyResponseFastRetries {
		return 0
	}
	shift := uint(n - emptyResponseFastRetries - 1)
	// Cap the shift so the left-shift can't overflow before the clamp below.
	if shift > 16 {
		shift = 16
	}
	d := emptyResponseBaseBackoff << shift
	if d > emptyResponseMaxBackoff {
		d = emptyResponseMaxBackoff
	}
	return d
}
