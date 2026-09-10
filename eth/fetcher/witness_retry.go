package fetcher

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// Recovery from a witness that passed every pre-import check and still failed
// stateless execution.
//
// Everything upstream of import validates the witness against something a peer
// could forge for free: the page count is compared across peers, and the WIT2
// byte check only proves the bytes hash to what *some* block producer signed.
// Neither says the witness actually reconstructs the block's post-state. Only
// execution does, and until now a failure there was terminal — the block was
// forgotten, the peer that supplied the bytes was never identified, and a
// stateless node (which by definition has no local state to fall back on) sat
// at that height with no way forward.
//
// The recovery is deliberately small: treat the failure as evidence about the
// witness rather than about the block, ask a *different* peer for the same
// block's witness, and let import arbitrate again. Bounded, because a block
// that is genuinely invalid would otherwise walk the whole peer set.
const (
	// maxWitnessSourceRetries bounds how many alternative witness sources a
	// single block may be re-fetched from after a witness-attributable import
	// failure. Two is enough to distinguish "one peer served bad bytes" from
	// "this witness is bad everywhere" (a producer that generated it wrong, or
	// a block that is simply invalid) without letting a bad block walk the
	// peer set. Past the bound the block is dropped exactly as it is today and
	// the downloader's own peer rotation takes over.
	maxWitnessSourceRetries = 2

	// witnessSourceExclusionTTL bounds how long a per-block source exclusion
	// survives. Exclusions are normally cleared the moment the block leaves the
	// import path (success, or a failure that is not the witness's fault), but
	// a block that is never resolved at all — reorged away mid-retry, or
	// abandoned when the node falls back to the downloader — would otherwise
	// leak its entry for the process lifetime. Generous relative to the
	// seconds-scale window a block resolves in, and swept by the same ticker
	// that expires witnessUnavailable.
	witnessSourceExclusionTTL = 2 * time.Minute
)

// excludeWitnessSource records that peer supplied a witness for hash that
// failed stateless validation, so subsequent fetches for the same block skip
// it. Exclusions are per-block: a peer that serves one bad witness is not shut
// out of every other block, only re-asked for the one it already got wrong.
// An empty peer id is ignored.
func (m *witnessManager) excludeWitnessSource(hash common.Hash, peer string) {
	if peer == "" {
		return
	}
	m.witnessSourceMu.Lock()
	defer m.witnessSourceMu.Unlock()

	peers := m.witnessSourceExcluded[hash]
	if peers == nil {
		peers = make(map[string]struct{})
		m.witnessSourceExcluded[hash] = peers
	}
	peers[peer] = struct{}{}
	m.witnessSourceExpiry[hash] = time.Now().Add(witnessSourceExclusionTTL)
}

// isWitnessSourceExcluded reports whether peer has already served a witness for
// hash that failed import.
func (m *witnessManager) isWitnessSourceExcluded(hash common.Hash, peer string) bool {
	if peer == "" {
		return false
	}
	m.witnessSourceMu.Lock()
	defer m.witnessSourceMu.Unlock()

	_, excluded := m.witnessSourceExcluded[hash][peer]
	return excluded
}

// excludedWitnessSources returns the peers whose witness for hash already
// failed import. Returns nil when there are none, which is the overwhelmingly
// common case — callers on the hot fetch path can skip the lookup cost by
// checking for nil.
func (m *witnessManager) excludedWitnessSources(hash common.Hash) map[string]struct{} {
	m.witnessSourceMu.Lock()
	defer m.witnessSourceMu.Unlock()

	peers := m.witnessSourceExcluded[hash]
	if len(peers) == 0 {
		return nil
	}
	// Copy: the caller iterates it outside our lock.
	out := make(map[string]struct{}, len(peers))
	for id := range peers {
		out[id] = struct{}{}
	}
	return out
}

// clearWitnessSourceExclusions drops the exclusion set for a block. Called once
// the block leaves the import path for a reason other than a bad witness, so
// the map stays bounded by blocks actually in retry.
func (m *witnessManager) clearWitnessSourceExclusions(hash common.Hash) {
	m.witnessSourceMu.Lock()
	defer m.witnessSourceMu.Unlock()

	delete(m.witnessSourceExcluded, hash)
	delete(m.witnessSourceExpiry, hash)
}

// blameExcludedWitnessSources strikes every peer whose witness for hash failed
// import, and then clears the block's exclusion set. Called only once the block
// has actually imported from another peer's witness.
//
// That ordering is the whole point. A witness failing to execute proves the
// bytes are unusable, never who made them unusable: the producer builds the
// witness and honest peers relay it verbatim, so a producer that emits a bad
// one makes every relay look equally guilty. Striking on failure alone would
// therefore punish honest peers for a producer's fault across every block of
// that producer's sprint — and with the ladder at wit2MisbehaviorStrikeLimit
// strikes per wit2MisbehaviorWindow, a node with few witness-capable peers
// would disconnect them exactly when witnesses are hardest to come by.
//
// A successful import from a different source is the one observation that
// separates the cases: the block was importable all along, so the sources that
// failed it really did serve bad bytes. Only then is blame provable, and the
// per-block exclusion set makes it at most one strike per (peer, block) — the
// same dedup the WIT2 byte-mismatch path settled on.
func (m *witnessManager) blameExcludedWitnessSources(hash common.Hash) {
	if m.parentStrikeWitnessServer == nil {
		m.clearWitnessSourceExclusions(hash)
		return
	}

	m.witnessSourceMu.Lock()
	peers := m.witnessSourceExcluded[hash]
	delete(m.witnessSourceExcluded, hash)
	delete(m.witnessSourceExpiry, hash)
	m.witnessSourceMu.Unlock()

	// Struck outside the lock: the striker reaches into the peer set and may
	// disconnect, which must not happen with our mutex held.
	for peer := range peers {
		witnessSourceBlamedMeter.Mark(1)
		log.Warn("Blaming witness source: its witness failed where another peer's succeeded",
			"peer", peer, "block", hash)
		m.parentStrikeWitnessServer(peer)
	}
}

// cleanupWitnessSourceExclusions removes exclusion sets whose TTL has lapsed.
// Backstops clearWitnessSourceExclusions for blocks that never reach a terminal
// import outcome. Called from the same ticker that expires witnessUnavailable.
func (m *witnessManager) cleanupWitnessSourceExclusions() {
	now := time.Now()
	cleaned := 0

	m.witnessSourceMu.Lock()
	for hash, expiry := range m.witnessSourceExpiry {
		if now.After(expiry) {
			delete(m.witnessSourceExpiry, hash)
			delete(m.witnessSourceExcluded, hash)
			cleaned++
		}
	}
	m.witnessSourceMu.Unlock()

	if cleaned > 0 {
		log.Debug("[wm] Cleaned up expired witness source exclusions", "removed", cleaned)
	}
}

// retryWithNewSource re-arms a witness fetch for a block whose witness failed
// stateless validation, after excluding the source that supplied the failing
// bytes. The op is reused rather than rebuilt so the retry budget carried on it
// survives across attempts.
//
// Must be called from the parent fetcher's main loop, after that loop has run
// forgetHash/forgetBlock for the block. Re-arming from the import goroutine
// instead would race the forget and have the fresh pending state deleted out
// from under it.
func (m *witnessManager) retryWithNewSource(op *blockOrHeaderInject) {
	if op == nil || op.block == nil || op.fetchWitness == nil {
		return
	}
	hash := op.hash()

	m.excludeWitnessSource(hash, op.witnessOrigin)

	// A cached witness for this block came from the same broadcast wave as the
	// one that just failed; drop it so the retry cannot pick it straight back
	// up if the block is re-announced mid-retry.
	m.witnessCache.Delete(hash)

	op.witness = nil
	op.witnessOrigin = ""
	op.witnessRetries++

	m.mu.Lock()
	if _, exists := m.pending[hash]; exists {
		// Something already re-armed this hash (a fresh announce racing the
		// failed import). Leave it alone rather than resetting its retry state.
		m.mu.Unlock()
		return
	}
	m.pending[hash] = &witnessRequestState{
		op: op,
		announce: &blockAnnounce{
			origin:       op.origin,
			hash:         hash,
			number:       op.number(),
			time:         time.Now(),
			fetchWitness: op.fetchWitness,
		},
	}
	m.mu.Unlock()

	witnessSourceRetryMeter.Mark(1)
	log.Warn("Witness failed stateless validation, retrying from a different peer",
		"number", op.number(), "hash", hash, "attempt", op.witnessRetries, "max", maxWitnessSourceRetries)

	m.rescheduleWitness()
}
