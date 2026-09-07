package eth

import (
	"errors"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
)

var errInvalidSignatureLength = errors.New("invalid wit2 announce signature length")

// Metrics for WIT2 signed-announce path. Emitted only when metrics are enabled.
var (
	wit2RelayInMeter                    = metrics.NewRegisteredMeter("eth/wit2/announce/relay_in", nil)
	wit2RelayOutMeter                   = metrics.NewRegisteredMeter("eth/wit2/announce/relay_out", nil)
	wit2InvalidSigMeter                 = metrics.NewRegisteredMeter("eth/wit2/announce/invalid_sig", nil)
	wit2NotValidatorMeter               = metrics.NewRegisteredMeter("eth/wit2/announce/not_validator", nil)
	wit2DuplicateMeter                  = metrics.NewRegisteredMeter("eth/wit2/announce/duplicate", nil)
	wit2BroadcastByteMismatchMeter      = metrics.NewRegisteredMeter("eth/wit2/serve/broadcast_byte_mismatch", nil)
	wit2BroadcastUnverifiedSkippedMeter = metrics.NewRegisteredMeter("eth/wit2/serve/broadcast_unverified_skipped", nil)
	wit2DeferredPerPeerDropMeter        = metrics.NewRegisteredMeter("eth/wit2/announce/deferred_per_peer_drop", nil)
	wit2DeferredPerBlockDropMeter       = metrics.NewRegisteredMeter("eth/wit2/announce/deferred_per_block_drop", nil)
	wit2HeaderUnknownMeter              = metrics.NewRegisteredMeter("eth/wit2/announce/header_unknown", nil)
	wit2ConflictingWitnessHashMeter     = metrics.NewRegisteredMeter("eth/wit2/announce/conflicting_witness_hash", nil)
	wit2RateLimitDropMeter              = metrics.NewRegisteredMeter("eth/wit2/announce/rate_limit_drop", nil)
	wit2StrikeDisconnectMeter           = metrics.NewRegisteredMeter("eth/wit2/announce/strike_disconnect", nil)
	wit2WaiterPushMeter                 = metrics.NewRegisteredMeter("eth/wit2/serve/waiter_push", nil)
	wit2WaiterPushOversizeMeter         = metrics.NewRegisteredMeter("eth/wit2/serve/waiter_push_oversize", nil)
	wit2BroadcastUnknownHeaderDropMeter = metrics.NewRegisteredMeter("eth/wit2/serve/broadcast_unknown_header_drop", nil)
	wit2BroadcastDeferredImportMeter    = metrics.NewRegisteredMeter("eth/wit2/serve/broadcast_deferred_import_only", nil)
	wit2FetchTriggerRateLimitDropMeter  = metrics.NewRegisteredMeter("eth/wit2/serve/fetch_trigger_rate_limit_drop", nil)
	// wit2RelayFetchTriggeredMeter is marked synchronously, once per hash,
	// the moment triggerRelayFetch passes its per-hash dedup gate and
	// commits to spawning a fetch goroutine — before the goroutine itself
	// runs, so (unlike relayFetchInFlight, which a fast no-candidate failure
	// can clear before an observer gets to check it) this count is race-free
	// to assert on immediately after the call returns.
	wit2RelayFetchTriggeredMeter = metrics.NewRegisteredMeter("eth/wit2/serve/relay_fetch_triggered", nil)
	// wit2RelayFetchConcurrencyDropMeter counts triggers dropped by the
	// global concurrency cap (relayFetchSem) — distinct from
	// wit2FetchTriggerRateLimitDropMeter, which counts drops from the
	// per-peer rate limiter instead.
	wit2RelayFetchConcurrencyDropMeter = metrics.NewRegisteredMeter("eth/wit2/serve/relay_fetch_concurrency_drop", nil)
)

// witnessPushMaxSize caps the encoded size of a witness we full-push to
// waiting peers via NewWitness. The wit protocol rejects inbound messages
// larger than 16MB (wit.maxMessageSize), so pushing a bigger body would make
// every waiter drop us as a protocol violator — the paged GetWitness path
// exists precisely for those witnesses. The margin covers the NewWitnessPacket
// RLP envelope around the witness bytes. Oversized witnesses simply stay on
// the pull path: by the time any push could fire we hold servable bytes, so
// the waiter's next (backed-off) poll gets real pages instead of empty.
const witnessPushMaxSize = MaximumResponseSize - 64*1024

// pushWitnessToWaiters delivers the full witness body to peers that previously
// asked us for it and got an empty answer (we did not hold the body yet). The
// moment we obtain the bytes the waiting consumer receives them and imports,
// instead of continuing to poll us with empty GetWitness. encodedSize is the
// canonical RLP size of the witness, used to keep the push under the wit
// protocol message cap.
func (h *handler) pushWitnessToWaiters(hash common.Hash, witness *stateless.Witness, encodedSize int) {
	if h.witnessWaiters == nil || witness == nil {
		return
	}
	if encodedSize > witnessPushMaxSize {
		// Too large for a single NewWitness message — leave the waiters on
		// the paged pull path (entries expire by TTL; the bytes are already
		// servable, so their next poll succeeds).
		wit2WaiterPushOversizeMeter.Mark(1)
		log.Debug("wit2: witness too large for full push; serving via paged pull only",
			"hash", hash, "size", encodedSize, "cap", witnessPushMaxSize)
		return
	}
	for _, p := range h.witnessWaiters.take(hash) {
		if p.KnownWitnessContainsHash(hash) {
			continue // already delivered / known to hold it
		}
		p.AsyncSendNewWitness(witness)
		wit2WaiterPushMeter.Mark(1)
	}
}

// flushWitnessWaitersForImported pushes a just-imported block's witness to any
// peer that asked us for it before we held it. This covers the dominant case
// the fetch/broadcast push hooks miss: a node (especially a full / producing
// node) that obtains the witness by generating it during native block import,
// rather than by pulling it or receiving a gossip broadcast. Called from the
// chain-head loop on every new head; cheap no-op when no peer is waiting.
func (h *handler) flushWitnessWaitersForImported(blockHash common.Hash) {
	if h.witnessWaiters == nil || !h.witnessWaiters.has(blockHash) {
		return
	}
	body := h.chain.GetWitness(blockHash)
	if len(body) == 0 {
		return
	}
	h.pushWitnessBytesToWaiters(blockHash, body)
}

// pushWitnessBytesToWaiters decodes verified witness bytes (already checked
// against the BP-signed hash by the caller) and pushes them to waiting peers.
// The decode — re-encoded canonically on send — round-trips to the same bytes,
// so downstream byte-correctness checks still pass. Skipped entirely when no
// peer is waiting, so the common (no-waiter) case pays nothing.
func (h *handler) pushWitnessBytesToWaiters(hash common.Hash, witnessBytes []byte) {
	if h.witnessWaiters == nil || len(witnessBytes) == 0 || !h.witnessWaiters.has(hash) {
		return
	}
	if len(witnessBytes) > witnessPushMaxSize {
		// Skip the decode entirely — the push would be over the wit message
		// cap anyway; waiters fall back to the paged pull path.
		wit2WaiterPushOversizeMeter.Mark(1)
		log.Debug("wit2: witness too large for full push; serving via paged pull only",
			"hash", hash, "size", len(witnessBytes), "cap", witnessPushMaxSize)
		return
	}
	var witness stateless.Witness
	if err := rlp.DecodeBytes(witnessBytes, &witness); err != nil {
		log.Warn("wit2: failed to decode witness bytes for waiter push", "hash", hash, "err", err)
		return
	}
	h.pushWitnessToWaiters(hash, &witness, len(witnessBytes))
}

// verifySignedAnnouncement returns the recovered signer address if the
// signature is structurally valid; otherwise an error. Validator-set
// membership is checked separately against the consensus engine.
func verifySignedAnnouncement(ann wit.SignedWitnessAnnouncement) (common.Address, error) {
	if len(ann.Signature) != wit.SignatureLength {
		return common.Address{}, errInvalidSignatureLength
	}
	digest := wit.WitnessAnnouncementSigningHash(ann.BlockHash, ann.BlockNumber, ann.WitnessHash)
	// Normalize the recovery id to 0/1 before recovery. External signers (Clef)
	// return V in 27/28 form for any mimetype other than Clique — see
	// accounts/external.SignData, which only de-offsets MimetypeClique — and
	// crypto.Ecrecover rejects any V >= 4. Work on a copy so the cached and
	// relayed signature bytes stay byte-for-byte as the producer emitted them;
	// every receiver normalizes independently.
	sig := ann.Signature
	if v := sig[crypto.RecoveryIDOffset]; v == 27 || v == 28 {
		sig = append([]byte(nil), ann.Signature...)
		sig[crypto.RecoveryIDOffset] -= 27
	}
	pubkey, err := crypto.Ecrecover(digest.Bytes(), sig)
	if err != nil {
		return common.Address{}, err
	}
	var addr common.Address
	copy(addr[:], crypto.Keccak256(pubkey[1:])[12:])
	return addr, nil
}

// cosendWitnessAnnouncement co-sends a witness announcement to every peer
// that just received the full block via the propagate=true fanout, provided
// the peer doesn't already have the witness. WIT2 peers receive the signed
// variant when one is available — our own (we produced the block) or the
// producer's (relayed to us and cached). Otherwise, and for older peers,
// the unsigned WIT1 hash announce is sent: truthful, since this path is
// gated on HasWitness. Skipped entirely when the local node hasn't yet
// stored the witness.
func (h *handler) cosendWitnessAnnouncement(blockHash common.Hash, blockNumber uint64, transfer []*ethPeer, staticAndTrustedPeers []*ethPeer) {
	if !h.chain.HasWitness(blockHash) {
		return
	}
	ann, hasSigned := h.signLocalWitnessAnnouncement(blockHash, blockNumber)
	witnessRecipientsByID := make(map[string]*witPeer)
	for _, wp := range h.peers.peersWithoutWitness(blockHash) {
		witnessRecipientsByID[wp.Peer.ID()] = wp
	}
	cosend := func(id string) {
		wp, ok := witnessRecipientsByID[id]
		if !ok {
			return
		}
		if hasSigned && wp.Peer.Version() >= wit.WIT2 {
			wp.Peer.AsyncSendSignedWitnessAnnouncement(ann)
		} else {
			wp.Peer.AsyncSendNewWitnessHash(blockHash, blockNumber)
		}
	}
	for _, peer := range transfer {
		cosend(peer.Peer.ID())
	}
	for _, peer := range staticAndTrustedPeers {
		cosend(peer.ID())
	}
}

// lookupSignedWitnessHash returns the BP-signed witness hash for a block, if
// the local cache has a verified announcement. Used by the witness manager
// on fetch success to verify byte-correctness against the signed commitment.
func (h *handler) lookupSignedWitnessHash(blockHash common.Hash) (common.Hash, bool) {
	ann, ok := h.signedWitnesses.get(blockHash)
	if !ok {
		return common.Hash{}, false
	}
	return ann.WitnessHash, true
}

// cacheVerifiedWitnessForServing receives canonical-encoded witness bytes from
// the fetcher after a successful, byte-verified paged download and stores them
// in the in-flight cache so peers can fetch the body before this node finishes
// chain-write. Bytes here have already passed verifyAgainstSignedHash (when a
// signed announcement was on file), or arrived via WIT1 unsigned path; in both
// cases they're the same bytes the upstream peer agreed upon, so serving them
// to downstream peers cannot expose this node to byte-mismatch drops beyond
// the upstream's already-incurred risk.
func (h *handler) cacheVerifiedWitnessForServing(blockHash common.Hash, witnessBytes []byte, witnessHash common.Hash) {
	if h.pendingWitnessBodies == nil {
		return
	}
	h.pendingWitnessBodies.put(blockHash, witnessBytes, witnessHash)
	// We now hold servable bytes: hand them straight to any peer that asked for
	// this body before we had it, so a stateless consumer stops polling us with
	// empty GetWitness and imports immediately.
	h.pushWitnessBytesToWaiters(blockHash, witnessBytes)
}

// signLocalWitnessAnnouncement looks up the witness body for blockHash, hashes
// it, and signs the announcement digest using the engine's authorized signer.
// The result is cached so subsequent broadcasts of the same block reuse the
// signature without recomputing the keccak.
//
// Returns (announcement, true) on success. Returns (_, false) if any of:
// - no signer configured (full node not producing blocks)
// - the local signer is not the sealer of blockHash (foreign block)
// - witness bytes not yet stored in chain
// - signing failed
//
// Cost: one chunked-parallel WitnessCommitHash over the stored witness
// (~14ms for a 50MB witness on 8 cores; see core/stateless.WitnessCommitHash)
// plus ~100μs ECDSA. Off the block-production critical path; runs once per
// produced block on the announce path, and the result is cached.
func (h *handler) signLocalWitnessAnnouncement(blockHash common.Hash, blockNumber uint64) (wit.SignedWitnessAnnouncement, bool) {
	if cached, ok := h.signedWitnesses.get(blockHash); ok {
		return cached, true
	}

	borEngine, ok := h.chain.Engine().(*bor.Bor)
	if !ok {
		return wit.SignedWitnessAnnouncement{}, false
	}
	signer := borEngine.CurrentSigner()
	if (signer == common.Address{}) {
		return wit.SignedWitnessAnnouncement{}, false
	}
	// Only the producer of the block may sign its announcement. Receivers
	// enforce announce-signer == header-sealer and strike-disconnect on a
	// mismatch, so signing a foreign block guarantees rejection plus peer
	// discipline against us — and caching the self-signed announce here
	// would shadow the producer's real one (signedWitnesses dedups by
	// blockHash), suppressing its transitive relay. For blocks we did not
	// seal, the caller falls back to the unsigned WIT1 hash announce, which
	// is truthful: every announce path is gated on HasWitness.
	if !maySignAnnouncementForBlock(borEngine, h.chain.GetHeaderByHash(blockHash), signer, blockNumber, blockHash) {
		return wit.SignedWitnessAnnouncement{}, false
	}

	witnessHash, ok := h.canonicalWitnessHash(blockHash)
	if !ok {
		return wit.SignedWitnessAnnouncement{}, false
	}
	preimage := wit.WitnessAnnouncementSigningPreImage(blockHash, blockNumber, witnessHash)
	_, sig, err := borEngine.SignBytes(accounts.MimetypeBorWitnessAnnounce, preimage)
	if err != nil {
		log.Warn("wit2: failed to sign witness announcement", "blockHash", blockHash, "err", err)
		return wit.SignedWitnessAnnouncement{}, false
	}
	// Canonicalize the recovery id to 0/1 at the source: external signers (Clef)
	// return V in 27/28 form for this mimetype, which crypto.Ecrecover rejects.
	// Emitting a canonical signature keeps the wire clean; receivers also
	// normalize defensively in verifySignedAnnouncement.
	if len(sig) == wit.SignatureLength && (sig[crypto.RecoveryIDOffset] == 27 || sig[crypto.RecoveryIDOffset] == 28) {
		sig[crypto.RecoveryIDOffset] -= 27
	}

	ann := wit.SignedWitnessAnnouncement{
		BlockHash:   blockHash,
		BlockNumber: blockNumber,
		WitnessHash: witnessHash,
		Signature:   sig,
	}
	// Honor the cache's conflict decision. We reach here only when the early
	// get() above missed, so under honest operation putIfNewer inserts into an
	// empty slot and returns true. A false return means a different WitnessHash
	// for this block raced into the cache between the get() and here — for our
	// own sealed block the witness bytes are deterministic, so this should be
	// unreachable, but if it happens the cached entry is the one already being
	// relayed/served: return it rather than announcing a hash we won't serve.
	if !h.signedWitnesses.putIfNewer(ann) {
		if cached, ok := h.signedWitnesses.get(blockHash); ok {
			return cached, true
		}
		return wit.SignedWitnessAnnouncement{}, false
	}
	return ann, true
}

// maySignAnnouncementForBlock reports whether the locally authorized signer
// sealed blockHash and is therefore the one party entitled to sign a WIT2
// witness announcement for it. Same producer binding the receive side
// enforces (verifyScheduledProducer), applied at the origination side. A nil
// or number-mismatched header refuses: an announce we cannot bind locally
// must not be signed either.
func maySignAnnouncementForBlock(borEngine *bor.Bor, header *types.Header, localSigner common.Address, blockNumber uint64, blockHash common.Hash) bool {
	ok, _ := verifyScheduledProducer(borEngine, header, localSigner, blockNumber, blockHash)
	return ok
}

// canonicalWitnessHash reads the witness bytes for blockHash from chain
// storage and returns the WIT2 chunked-aggregate commitment over those bytes.
// Witness.EncodeRLP is now deterministic (state nodes sorted), so every newly
// written witness blob is canonical at write time and can be hashed directly
// without a decode/re-encode round-trip — saving roughly the cost of one RLP
// pass on the announce path. Returns (_, false) when no witness is on file.
func (h *handler) canonicalWitnessHash(blockHash common.Hash) (common.Hash, bool) {
	stored := h.chain.GetWitness(blockHash)
	if len(stored) == 0 {
		return common.Hash{}, false
	}
	return stateless.WitnessCommitHash(stored), true
}

// isScheduledProducer binds the recovered signer of a wit2 announcement to the
// actual block producer of the announced block. When the block header is
// locally available — the common case — we recover the seal-signer of the
// header and require an exact address match. Validator-set membership is no
// longer sufficient: any current validator could otherwise sign an
// announcement for another producer's block hash with a forged WitnessHash,
// poisoning this node's cache and dropping honest serving peers.
//
// Returns (ok, headerAvailable):
//   - ok=true, headerAvailable=true: signer matches the block producer; safe
//     to cache and relay.
//   - ok=false, headerAvailable=true: confirmed bad signer (block number
//     mismatch, given hash cryptographically commits to number, or a
//     recovered sealer that disagrees with the announced signer). The caller
//     MUST strike the relayer.
//   - ok=false, headerAvailable=false: the announce cannot be bound to a
//     producer right now, through no fault of the announcer. The caller MUST
//     NOT strike. This covers two cases: the header is not yet local (the
//     cosend window where a signed announce races the block to the
//     receiver), and the header IS local but its seal is unrecoverable
//     (Author() errors) — a defect in the shared header, not evidence about
//     any individual signer, since every relayer for that hash would hit the
//     identical failure. The handler stashes the announce in the deferred
//     queue; the chain-head loop re-evaluates it once the block arrives (and
//     an unrecoverable-seal header simply ages out of that queue, since no
//     future re-check will change the outcome).
//
// Header presence is checked first regardless of engine: an announce we
// cannot match to a local block is by definition unverifiable here. Only
// after the header is on file do we route into the bor-specific producer
// recovery (or short-circuit to ok=true on non-bor test chains).
func (h *handler) isScheduledProducer(signer common.Address, blockNumber uint64, blockHash common.Hash) (bool, bool) {
	header := h.chain.GetHeaderByHash(blockHash)
	if header == nil {
		wit2HeaderUnknownMeter.Mark(1)
		return false, false
	}
	borEngine, isBor := h.chain.Engine().(*bor.Bor)
	if !isBor {
		// Non-bor chain (tests): header presence already validated above; the
		// producer check is bor-specific and intentionally skipped here.
		if header.Number.Uint64() != blockNumber {
			return false, true
		}
		return true, true
	}
	return verifyScheduledProducer(borEngine, header, signer, blockNumber, blockHash)
}

// drainDeferredAnnouncesFor re-evaluates every deferred candidate announcement
// for blockHash once its header is local. Multiple candidates may be on file
// (an honest producer's announce plus any forged header-racing ones); the one
// whose signer is the actual block producer is promoted into signedWitnesses,
// its sender credited as announce-known, and the announce relayed. Candidates
// that re-check as a confirmed mis-binding (signer ≠ producer) are dropped —
// relayers cannot be re-struck post-hoc since we lost the peer reference between
// deferral and drain. If the header is still not local for all candidates they
// are re-stashed for the next chain-head event.
//
// Called from the chain-head subscription on each new block. Also exposed for
// direct invocation in tests.
func (h *handler) drainDeferredAnnouncesFor(blockHash common.Hash) {
	if h.deferredAnnounces == nil {
		return
	}
	candidates, ok := h.deferredAnnounces.take(blockHash)
	if !ok {
		return
	}
	promoted := false
	for _, entry := range candidates {
		if h.drainDeferredCandidate(blockHash, entry, promoted) {
			promoted = true
		}
	}
}

// drainDeferredCandidate re-evaluates a single deferred candidate for blockHash
// once its header is local, returning true only when it promoted the candidate
// into signedWitnesses (so the caller can suppress promotion of any further
// producer-signed duplicate). alreadyPromoted reports whether an earlier
// candidate in the same drain already won promotion. Extracted from
// drainDeferredAnnouncesFor purely to keep each function's control flow shallow;
// the branch order and side effects are identical to the inline loop body.
func (h *handler) drainDeferredCandidate(blockHash common.Hash, entry *deferredAnnounceEntry, alreadyPromoted bool) bool {
	signer, err := verifySignedAnnouncement(entry.announcement)
	if err != nil {
		// Should be unreachable: we re-verified the same bytes that already
		// passed the signature check at acceptSignedAnnouncement time.
		// Surfaced via metric in case a future refactor reorders this.
		wit2InvalidSigMeter.Mark(1)
		log.Debug("wit2: deferred announce failed signature re-check", "blockHash", blockHash, "err", err)
		return false
	}
	prodOk, headerAvailable := h.isScheduledProducer(signer, entry.announcement.BlockNumber, blockHash)
	if !prodOk {
		if !headerAvailable {
			// Header still not local — re-stash with fresh receivedAt so the
			// next chain-head event can try again before the TTL expires.
			h.deferredAnnounces.put(entry.announcement, entry.peerID)
			return false
		}
		wit2NotValidatorMeter.Mark(1)
		log.Debug("wit2: deferred announce signer is not the scheduled producer",
			"blockHash", blockHash, "signer", signer)
		// Same confirmed misbehavior acceptSignedAnnouncement strikes
		// synchronously (signer != scheduled producer, header known) — without
		// this, timing a forged announce to always arrive before its header is
		// local lets an attacker repeat it indefinitely without ever accruing
		// a strike.
		h.strikeWit2PeerByID(entry.peerID)
		return false
	}
	// Producer match. Promote the first one; any further producer-signed
	// candidate is a duplicate of the same authorized signer.
	if alreadyPromoted {
		return false
	}
	if !h.signedWitnesses.putIfNewer(entry.announcement) {
		wit2DuplicateMeter.Mark(1)
		return false
	}
	// Credit the original sender as announce-known so we don't re-relay back.
	if peer := h.peers.peer(entry.peerID); peer != nil && peer.witPeer != nil {
		peer.witPeer.Peer.AddKnownAnnounce(blockHash)
	}
	h.relaySignedAnnouncement(entry.peerID, entry.announcement)
	return true
}

// verifyScheduledProducer is the pure decision logic for binding a wit2
// announcement signer to the block producer of `blockHash`. Split from
// isScheduledProducer so it can be unit-tested without standing up a full
// handler. Returns the same (ok, headerAvailable) shape — see
// isScheduledProducer for the contract.
func verifyScheduledProducer(borEngine *bor.Bor, header *types.Header, signer common.Address, blockNumber uint64, blockHash common.Hash) (bool, bool) {
	if header == nil {
		wit2HeaderUnknownMeter.Mark(1)
		log.Debug("wit2: header for announced block not yet local; deferring until block arrives",
			"blockHash", blockHash, "blockNumber", blockNumber)
		return false, false
	}
	if header.Number.Uint64() != blockNumber {
		log.Debug("wit2: announce blockNumber does not match local header",
			"blockHash", blockHash, "announced", blockNumber, "local", header.Number.Uint64())
		return false, true
	}
	producer, err := borEngine.Author(header)
	if err != nil {
		// Author() failing is a property of the header itself (malformed/
		// missing seal), not of who announced it: every relayer for this exact
		// block hash would hit the identical error, so it proves nothing about
		// this signer's honesty. Treat it like header-not-yet-local — defer,
		// do not strike — rather than folding it into the confirmed-bad-signer
		// branch below.
		log.Debug("wit2: failed to recover header sealer; treating as unverifiable, not misbehavior",
			"blockHash", blockHash, "err", err)
		return false, false
	}
	if producer != signer {
		log.Debug("wit2: announce signer is not the block producer",
			"blockHash", blockHash, "producer", producer, "signer", signer)
		return false, true
	}
	return true, true
}
