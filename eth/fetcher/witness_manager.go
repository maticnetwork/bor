package fetcher

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"

	ttlcache "github.com/jellydator/ttlcache/v3"
)

// Constants for witness unavailability handling
const (
	// witnessUnavailableTimeout defines how long a hash is blacklisted after no peer could provide its witness.
	witnessUnavailableTimeout = 200 * time.Millisecond // Use a much shorter timeout, closer to propagation delay

	// witnessUnavailableCleanupInterval defines how often the unavailable witness cache is cleaned.
	witnessUnavailableCleanupInterval = 1 * time.Minute

	// maxWitnessFetchRetries defines how many times we will try to fetch a
	// witness for a block hash before giving up and marking it unavailable.
	maxWitnessFetchRetries = 300 // ~30s of retries

	witnessCacheSize = 10
	witnessCacheTTL  = 2 * time.Minute

	// Witness verification constants
	witnessVerificationPeers   = 2               // Number of random peers to query for verification
	witnessVerificationTimeout = 5 * time.Second // Timeout for verification queries

	// Witness size estimation constants
	// Assuming 1M gas results in 1MB witness, and max page size is 15MB
	gasPerMB             = 1_000_000 // 1M gas per MB of witness
	maxPageSizeMB        = 15        // Maximum page size in MB
	witnessPageThreshold = 10        // Default threshold if gas ceil not available
)

// witnessRequestState tracks the state of a pending witness request.
type witnessRequestState struct {
	op           *blockOrHeaderInject // The original block/header injection operation.
	announce     *blockAnnounce       // Announcement details, non-nil if a fetch is in flight.
	retries      int                  // Number of fetch attempts already made
	emptyRetries int                  // Consecutive "body not ready yet" (empty) responses, for backoff
}

// cachedWitness represents a witness that arrived before its corresponding block
type cachedWitness struct {
	witness   *stateless.Witness
	peer      string
	timestamp time.Time
}

// signedWitnessHashFn returns the BP-signed witness content hash for a block,
// if a WIT2 signed announcement has been received and verified locally. It is
// used by the witness manager on fetch success to verify byte-correctness:
// if the encoded witness bytes don't hash to the signed witnessHash, the
// serving peer lied and is dropped. If no signed announcement is on file
// (e.g., WIT1-only fetch), the check is skipped.
type signedWitnessHashFn func(blockHash common.Hash) (witnessHash common.Hash, ok bool)

// cacheWitnessForServingFn hands successfully-fetched witness bytes to the
// network handler so peers can serve them pre-import. Called only after the
// byte-correctness check (vs. BP-signed witnessHash, when present) has passed,
// so the cached bytes are safe to serve. The witnessHash is the canonical
// keccak256 of the canonical encoding, identical to what the BP signed.
type cacheWitnessForServingFn func(blockHash common.Hash, witnessBytes []byte, witnessHash common.Hash)

// peerStrikeFn records a WIT2 misbehavior strike against a peer (by id) that
// served a non-empty witness whose bytes mismatch the on-file BP-signed hash.
// Unlike peerDropFn it does not immediately disconnect: a single mismatch is
// tolerated (a faulty/malicious BP that signed a bogus hash makes honest servers
// mismatch too), but sustained byte-serving misbehavior accrues toward the same
// disconnect threshold as bad announces. Optional; nil disables the penalty.
type peerStrikeFn func(id string)

// witnessManager handles the logic specific to fetching and managing witnesses
// for blocks, isolating it from the main BlockFetcher loop.
type witnessManager struct {
	// Parent fetcher fields/methods required
	parentQuit                   <-chan struct{}          // Parent fetcher's quit channel
	parentDropPeer               peerDropFn               // Function to drop a misbehaving peer
	parentJailPeer               peerJailFn               // Function to jail a peer to prevent reconnection (optional)
	parentEnqueueCh              chan<- *enqueueRequest   // Channel to send completed blocks+witnesses back
	parentGetBlock               blockRetrievalFn         // Function to check if block is known locally
	parentGetHeader              HeaderRetrievalFn        // Function to check if header is known locally (needed for checks)
	parentChainHeight            chainHeightFn            // Retrieve chain height for distance checks
	parentCurrentHeader          currentHeaderFn          // Retrieve current block header for gas limit
	parentSignedWitnessHash      signedWitnessHashFn      // WIT2: lookup a BP-signed witness hash for byte-correctness verification
	parentCacheWitnessForServing cacheWitnessForServingFn // WIT2: hand bytes to the handler for pre-import serving by peers
	parentStrikeWitnessServer    peerStrikeFn             // WIT2: strike a peer that served non-empty bytes mismatching the signed hash (optional)

	// Witness-specific state
	pending            map[common.Hash]*witnessRequestState         // Blocks waiting for witness or actively fetching.
	witnessUnavailable map[common.Hash]time.Time                    // Tracks hashes whose witnesses are known to be unavailable, with expiry times.
	witnessCache       *ttlcache.Cache[common.Hash, *cachedWitness] // TTL cache of witnesses that arrived before their blocks

	// WIT2 signed-hash mismatch tracking. A bad or stale BP-signed witness hash
	// makes every honest server's canonical bytes mismatch, which would stall
	// the block until the signed announcement's TTL expires. We track the
	// distinct servers that mismatch a given block's signed hash and, past
	// signedHashMismatchQuarantineThreshold, quarantine that hash so subsequent
	// fetches skip the signed-hash gate and fall back to WIT1 (import-time
	// execution arbitrates the bytes) immediately instead of waiting out the TTL.
	// Guarded by its own mutex (inner to m.mu in the rare paths that hold both).
	//
	// Unlike witnessUnavailable, entries here are cleared only by the 4
	// pending-removal exits (forget/safeEnqueue/markWitnessUnavailable/
	// handleWitnessFetchFailureExt) or a matching success — none of which fire
	// again once a hash has already left m.pending. A response that mismatches
	// AFTER that point (e.g. the block imported via broadcast, or forget()
	// already ran) still runs verifyAgainstSignedHash and creates a fresh
	// entry nothing will ever remove. wit2StateExpiry gives every entry a TTL,
	// refreshed on each touch, swept by the same cleanupTicker that expires
	// witnessUnavailable, so a late/adversarial mismatch can no longer leak
	// state for the process lifetime.
	wit2QuarantineMu  sync.Mutex
	wit2MismatchPeers map[common.Hash]map[string]struct{}
	wit2Quarantined   map[common.Hash]struct{}
	wit2StateExpiry   map[common.Hash]time.Time

	// Witness verification state
	gasCeil uint64 // Gas ceiling for calculating dynamic page threshold

	// Communication channels (owned by witnessManager)
	injectNeedWitnessCh chan *injectBlockNeedWitnessMsg // Injected blocks needing witness fetch
	injectWitnessCh     chan *injectedWitnessMsg        // Injected witnesses from broadcast

	// pokeCh is used to nudge the main loop whenever a reschedule occurs from
	// an external goroutine (e.g. BlockFetcher). Without it the loop might be
	// waiting in a select that doesn't include the timer channel, so the
	// freshly-reset timer would never be observed.
	pokeCh chan struct{}

	// Internal timer
	witnessTimer *time.Timer // Timer to trigger witness fetches for pending blocks

	// mutex protects access to mutable maps and timer manipulation so that
	// goroutines launched by the manager (e.g. fetchWitness) cannot race with
	// the main loop or each other while reading/writing these shared
	// structures.
	mu sync.Mutex
}

// newWitnessManager creates and initializes a new witnessManager.
func newWitnessManager(
	parentQuit <-chan struct{},
	parentDropPeer peerDropFn,
	parentJailPeer peerJailFn,
	parentEnqueueCh chan<- *enqueueRequest,
	parentGetBlock blockRetrievalFn,
	parentGetHeader HeaderRetrievalFn,
	parentChainHeight chainHeightFn,
	parentCurrentHeader currentHeaderFn,
	parentSignedWitnessHash signedWitnessHashFn,
	parentCacheWitnessForServing cacheWitnessForServingFn,
	gasCeil uint64,
) *witnessManager {
	// Create TTL cache with 1 minute expiration for witnesses
	witnessCache := ttlcache.New[common.Hash, *cachedWitness](
		ttlcache.WithTTL[common.Hash, *cachedWitness](witnessCacheTTL),
		ttlcache.WithCapacity[common.Hash, *cachedWitness](witnessCacheSize),
	)

	m := &witnessManager{
		parentQuit:                   parentQuit,
		parentDropPeer:               parentDropPeer,
		parentJailPeer:               parentJailPeer,
		parentEnqueueCh:              parentEnqueueCh,
		parentGetBlock:               parentGetBlock,
		parentGetHeader:              parentGetHeader,
		parentChainHeight:            parentChainHeight,
		parentCurrentHeader:          parentCurrentHeader,
		parentSignedWitnessHash:      parentSignedWitnessHash,
		parentCacheWitnessForServing: parentCacheWitnessForServing,
		pending:                      make(map[common.Hash]*witnessRequestState),
		witnessUnavailable:           make(map[common.Hash]time.Time),
		witnessCache:                 witnessCache,
		wit2MismatchPeers:            make(map[common.Hash]map[string]struct{}),
		wit2Quarantined:              make(map[common.Hash]struct{}),
		wit2StateExpiry:              make(map[common.Hash]time.Time),
		gasCeil:                      gasCeil,
		injectNeedWitnessCh:          make(chan *injectBlockNeedWitnessMsg, 10),
		injectWitnessCh:              make(chan *injectedWitnessMsg, 10),
		witnessTimer:                 time.NewTimer(0),
		pokeCh:                       make(chan struct{}, 1),
	}
	m.stopAndDrainTimer()
	return m
}

// stopAndDrainTimer stops the witness timer and drains its channel if it
// already fired. Safe to call whether or not the timer is active.
func (m *witnessManager) stopAndDrainTimer() {
	if !m.witnessTimer.Stop() {
		select {
		case <-m.witnessTimer.C:
		default:
		}
	}
}

// start begins the witness manager's internal loop in a new goroutine.
func (m *witnessManager) start() {
	// Start the TTL cache's automatic expiration
	go m.witnessCache.Start()
	go m.loop()
}

// stop cleanly shuts down the witness manager's timer and loop.
func (m *witnessManager) stop() {
	m.witnessTimer.Stop()
	m.witnessCache.Stop()
}

// loop is the main event loop for the witness manager.
func (m *witnessManager) loop() {
	defer m.witnessTimer.Stop()
	cleanupTicker := time.NewTicker(witnessUnavailableCleanupInterval)
	defer cleanupTicker.Stop()

	lastTick := time.Now()

	for {
		timerChan, updatedLastTick := m.armTimerChan(lastTick)
		lastTick = updatedLastTick

		select {
		case <-m.parentQuit:
			log.Info("Witness manager stopping")
			return

		case msg, ok := <-m.injectNeedWitnessCh:
			if !ok {
				log.Debug("Witness manager injectNeedWitnessCh closed unexpectedly")
				m.injectNeedWitnessCh = nil // Avoid busy-looping
				continue
			}
			log.Debug("[wm] Received injectNeedWitnessCh message", "hash", msg.block.Hash())
			m.handleNeed(msg)

		case msg, ok := <-m.injectWitnessCh:
			if !ok {
				log.Debug("Witness manager injectWitnessCh closed unexpectedly")
				m.injectWitnessCh = nil // Avoid busy-looping
				continue
			}
			log.Debug("[wm] Received injectWitnessCh message", "hash", msg.witness.Header().Hash())
			m.handleBroadcast(msg)

		case <-timerChan:
			lastTick = time.Now()
			m.logTimerTick(lastTick)
			m.tick()

		case <-cleanupTicker.C:
			log.Debug("[wm] Cleanup ticker triggered")
			m.cleanupUnavailableCache()
			m.cleanupWit2QuarantineState()

		// A poke indicates the timer was rescheduled by another goroutine. We
		// simply loop around so that the timer channel is re-evaluated with the
		// new configuration.
		case <-m.pokeCh:
			lastTick = time.Now()
			continue
		}
	}
}

// armTimerChan prepares the timer channel for the next select iteration.
// Returns nil channel (never fires) when nothing is pending. Forces a timer
// reset if too long has passed since the last tick to recover from stuck
// timers, updating lastTick when it does so.
func (m *witnessManager) armTimerChan(lastTick time.Time) (<-chan time.Time, time.Time) {
	m.mu.Lock()
	pendingCount := len(m.pending)
	m.mu.Unlock()

	if pendingCount == 0 {
		// Nothing to fetch — drain the timer so a stale fire doesn't wake us up.
		m.stopAndDrainTimer()
		return nil, lastTick
	}

	// If too long since last tick, reset the timer to ensure we don't get stuck.
	if time.Since(lastTick) > 10*time.Second {
		log.Debug("[wm] Long time since last tick, forcing timer reset", "sinceLastTick", time.Since(lastTick))
		m.rescheduleWitness()
		lastTick = time.Now()
	}
	return m.witnessTimer.C, lastTick
}

// logTimerTick emits a debug log with the current pending count.
func (m *witnessManager) logTimerTick(t time.Time) {
	m.mu.Lock()
	pendingCount := len(m.pending)
	m.mu.Unlock()
	log.Debug("[wm] Witness timer triggered", "time", t, "pendingCount", pendingCount)
}

// handleNeed processes a block injected via InjectBlockWithWitnessRequirement.
func (m *witnessManager) handleNeed(msg *injectBlockNeedWitnessMsg) {
	hash := msg.block.Hash()
	number := msg.block.NumberU64()
	log.Debug("[wm] Processing injected block needing witness", "peer", msg.origin, "number", number, "hash", hash)

	// --- Perform necessary checks (similar to BlockFetcher enqueue) ---

	// Check if witness is known to be unavailable
	if m.isWitnessUnavailable(hash) {
		log.Debug("[wm] Witness for injected block known to be unavailable, discarding", "hash", hash)
		return
	}

	// Check if already processed/pending
	m.mu.Lock()
	pendingCount := len(m.pending)
	if _, ok := m.pending[hash]; ok {
		m.mu.Unlock()
		log.Debug("[wm] Injected block already pending witness", "hash", hash)
		return
	}
	// Check if block is actually known locally (using parent's function)
	if m.parentGetBlock(hash) != nil {
		m.mu.Unlock()
		log.Debug("[wm] Injected block already known locally", "hash", hash)
		return
	}

	// Check distance (using parent's function). Match block_fetcher.go's
	// `<` comparison so a block at exactly dist == -maxUncleDist is treated
	// the same by both: accepted. An inconsistent boundary would let
	// block_fetcher import such a block while witness_manager drops it.
	if dist := int64(number) - int64(m.parentChainHeight()); dist < -maxUncleDist {
		m.mu.Unlock()
		log.Debug("[wm] Discarded injected block, too far away", "peer", msg.origin, "number", number, "hash", hash, "distance", dist)
		return // Doesn't count towards DOS limits as it's injected, just drop.
	}

	// Check if witness fetcher was provided (should be guaranteed by public func)
	if msg.fetchWitness == nil {
		m.mu.Unlock()
		log.Error("[wm] Injected block message missing fetchWitness function", "hash", hash, "origin", msg.origin)
		return // Cannot proceed without fetcher
	}

	// Check if we have a cached witness for this block
	if item := m.witnessCache.Get(hash); item != nil {
		cached := item.Value()
		// Use the cached witness
		op := &blockOrHeaderInject{
			origin:  msg.origin,
			block:   msg.block,
			witness: cached.witness,
		}
		m.witnessCache.Delete(hash)
		m.mu.Unlock()

		log.Debug("[wm] Found cached witness for block, using it", "hash", hash, "cachedPeer", cached.peer)
		m.safeEnqueue(op)
		return
	}

	// --- Add to pending state ---
	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: msg.origin,
			block:  msg.block,
		},
		// Create minimal announce struct needed for fetching
		announce: &blockAnnounce{
			origin:       msg.origin,
			hash:         hash,
			number:       number,
			time:         time.Now(), // Use current time as 'ready to fetch' time
			fetchWitness: msg.fetchWitness,
		},
	}
	m.pending[hash] = state

	m.mu.Unlock()

	log.Debug("[wm] Added injected block to witness pending queue", "peer", msg.origin, "number", number, "hash", hash, "prevPending", pendingCount, "newPending", pendingCount+1)

	// Ensure the timer is armed for the newly-added request.
	m.rescheduleWitness()
}

// handleBroadcast processes a witness injected via InjectWitness.
func (m *witnessManager) handleBroadcast(msg *injectedWitnessMsg) {
	hash := msg.witness.Header().Hash()
	log.Debug("[wm] Processing injected witness", "peer", msg.peer, "hash", hash, "number", msg.witness.Header().Number.Uint64())

	// We'll access maps under lock; then perform enqueue outside.
	m.mu.Lock()
	state, pending := m.pending[hash]

	log.Debug("[wm] Checking for pending block for witness", "hash", hash, "isPending", pending, "pendingCount", len(m.pending))

	if pending {
		// Ensure witness isn't already set
		if state.op.witness == nil {
			state.op.witness = msg.witness
			// Update block timestamps if needed
			if state.op.block != nil && msg.time.After(state.op.block.ReceivedAt) {
				state.op.block.ReceivedAt = msg.time
			}
			log.Debug("[wm] Successfully attached witness to pending block", "hash", hash, "number", msg.witness.Header().Number.Uint64(), "origin", state.op.origin)
		} else {
			log.Debug("[wm] Pending state already has witness, ignoring", "hash", hash)
		}
	}
	m.mu.Unlock()

	if pending {
		log.Debug("[wm] Enqueueing block with newly attached witness", "hash", hash)
		m.safeEnqueue(state.op)
	} else {
		// Cache the witness for later use when the block arrives
		m.witnessCache.Set(hash, &cachedWitness{
			witness:   msg.witness,
			peer:      msg.peer,
			timestamp: msg.time,
		}, ttlcache.DefaultTTL)
		log.Debug("[wm] No matching pending block for injected witness, caching for later", "hash", hash, "peer", msg.peer)
	}
}

// tick is called when the witnessTimer fires, triggering witness fetches.
func (m *witnessManager) tick() {
	log.Debug("[wm] Witness timer tick", "pending", len(m.pending))
	now := time.Now()

	m.mu.Lock()
	m.logPendingStatesAtTickLocked(now)
	readyToFetch, toMarkUnavailable := m.collectReadyHashesLocked(now)
	prematureOps := m.extractPrematureOpsLocked(readyToFetch)
	m.mu.Unlock()

	// Mark exhausted retries as unavailable (acquires lock internally, reschedules timer).
	for _, hash := range toMarkUnavailable {
		m.markWitnessUnavailable(hash)
	}

	// Enqueue any pending entries that already have a witness attached (e.g. arrived via broadcast).
	for _, op := range prematureOps {
		log.Debug("[wm] Enqueueing pending block with already attached witness", "hash", op.hash())
		m.safeEnqueue(op)
	}

	m.mu.Lock()
	requests := m.buildPeerRequestsLocked(readyToFetch, now)
	m.mu.Unlock()

	m.dispatchPeerRequests(requests)

	// Schedule the next fetch if blocks are still pending
	m.rescheduleWitness()
}

// logPendingStatesAtTickLocked emits a debug trace of the current pending
// witness requests. Caller must hold m.mu.
func (m *witnessManager) logPendingStatesAtTickLocked(now time.Time) {
	if len(m.pending) == 0 {
		return
	}
	pendingStates := make([]string, 0, len(m.pending))
	for h, state := range m.pending {
		readyStr := "not-ready"
		if state.announce != nil && now.After(state.announce.time) {
			readyStr = "ready"
		}
		statusStr := "no-witness"
		if state.op != nil && state.op.witness != nil {
			statusStr = "has-witness"
		}
		pendingStates = append(pendingStates,
			fmt.Sprintf("%s:%s:%s:%d", h.Hex()[:8], readyStr, statusStr, state.retries))
	}
	log.Debug("[wm] Pending states at tick", "states", pendingStates)
}

// collectReadyHashesLocked walks the pending map and partitions entries into
// hashes that are ready to fetch and hashes that have exhausted their retry
// budget and should be marked unavailable. Caller must hold m.mu.
//
// Invalid entries (missing op or announce) are cleaned up in place.
func (m *witnessManager) collectReadyHashesLocked(now time.Time) (readyToFetch, toMarkUnavailable []common.Hash) {
	for hash, state := range m.pending {
		// Must have an op and announce to be fetchable
		if state.op == nil || state.announce == nil {
			log.Debug("[wm] Invalid pending state found", "hash", hash)
			delete(m.pending, hash)
			continue
		}

		// Witness already present? Should have been enqueued.
		if state.op.witness != nil {
			log.Debug("[wm] Pending state found with witness already present", "hash", hash)
			readyToFetch = append(readyToFetch, hash)
			continue
		}

		// Not ready yet (announce time still in the future).
		if !now.After(state.announce.time) {
			continue
		}

		// Give up if we've retried too many times.
		if state.retries >= maxWitnessFetchRetries {
			log.Debug("[wm] Max witness retries reached, marking unavailable", "hash", hash, "retries", state.retries)
			toMarkUnavailable = append(toMarkUnavailable, hash)
			continue
		}

		// Increment retry counter and schedule fetch.
		state.retries++
		log.Debug("[wm] Scheduling witness fetch", "hash", hash, "retry", state.retries)
		readyToFetch = append(readyToFetch, hash)
	}
	return readyToFetch, toMarkUnavailable
}

// extractPrematureOpsLocked returns the ops for entries that already have
// their witness attached, so they can be enqueued immediately. Caller must
// hold m.mu.
func (m *witnessManager) extractPrematureOpsLocked(readyToFetch []common.Hash) []*blockOrHeaderInject {
	var ops []*blockOrHeaderInject
	for _, h := range readyToFetch {
		if st := m.pending[h]; st != nil && st.op != nil && st.op.witness != nil {
			ops = append(ops, st.op)
		}
	}
	return ops
}

// buildPeerRequestsLocked groups ready hashes by peer and updates announce
// timestamps to enforce per-request backoff. Caller must hold m.mu.
func (m *witnessManager) buildPeerRequestsLocked(readyToFetch []common.Hash, now time.Time) map[string]map[common.Hash]*blockAnnounce {
	requests := make(map[string]map[common.Hash]*blockAnnounce)
	for _, hash := range readyToFetch {
		state := m.pending[hash]
		if state == nil || state.announce == nil {
			continue
		}
		announce := state.announce

		if _, ok := requests[announce.origin]; !ok {
			requests[announce.origin] = make(map[common.Hash]*blockAnnounce)
		}
		requests[announce.origin][hash] = announce

		// Update announce time for backoff — prevents immediate retry and
		// effectively marks the request as "in-flight".
		announce.time = now.Add(fetchTimeout)
	}
	return requests
}

// dispatchPeerRequests spawns a fetch goroutine per (peer, hash) pair.
// Must be called without holding m.mu.
func (m *witnessManager) dispatchPeerRequests(requests map[string]map[common.Hash]*blockAnnounce) {
	for peer, hashAnnounceMap := range requests {
		m.dispatchPeerFetches(peer, hashAnnounceMap)
	}
}

// dispatchPeerFetches launches fetch goroutines for a single peer's hashes,
// handling invalid announcements as hard failures.
func (m *witnessManager) dispatchPeerFetches(peer string, hashAnnounceMap map[common.Hash]*blockAnnounce) {
	if len(hashAnnounceMap) == 0 {
		return
	}

	// Collect hashes for logging only.
	hashesToFetch := make([]common.Hash, 0, len(hashAnnounceMap))
	for hash := range hashAnnounceMap {
		hashesToFetch = append(hashesToFetch, hash)
	}
	log.Debug("[wm] Fetching scheduled witnesses", "peer", peer, "list", hashesToFetch)

	for hash, announce := range hashAnnounceMap {
		if announce == nil || announce.fetchWitness == nil {
			m.handleWitnessFetchFailureExt(hash, "", errors.New("missing fetch configuration"), true)
			continue
		}
		go m.fetchWitness(peer, hash, announce)
	}
}

// fetchWitness performs a single witness fetch in a goroutine.
func (m *witnessManager) fetchWitness(peer string, hash common.Hash, announce *blockAnnounce) {
	resCh := make(chan *eth.Response)

	m.mu.Lock()
	announcedAt := announce.time // Capture the original 'ready-to-fetch' time for logging/timestamping
	m.mu.Unlock()

	witnessFetchMeter.Mark(1)

	req, effectivePeer, ok := m.initiateWitnessFetch(hash, announce, resCh)
	if !ok {
		return
	}
	defer req.Close()

	m.awaitWitnessResponse(effectivePeer, hash, resCh, announcedAt)
}

// initiateWitnessFetch requests a witness from a peer and checks that the
// block is still pending afterwards. Returns the created request on success,
// or ok=false after handling the failure.
func (m *witnessManager) initiateWitnessFetch(hash common.Hash, announce *blockAnnounce, resCh chan *eth.Response) (*eth.Request, string, bool) {
	req, err := announce.fetchWitness(hash, resCh)
	peer := ""
	if req != nil {
		peer = req.Peer
	}

	if err != nil {
		log.Debug("[wm] Failed to initiate witness fetch request", "peer", peer, "hash", hash, "err", err)
		wrappedErr := fmt.Errorf("request initiation failed: %w", err)

		// "No peer with witness" is a soft failure with no peer to penalize.
		if strings.Contains(err.Error(), "no peer with witness for hash") {
			m.handleWitnessFetchFailureExt(hash, "", wrappedErr, false)
			return nil, "", false
		}

		// For other errors, check if still pending before handling failure.
		if !m.isPending(hash) {
			log.Debug("[wm] Skipping witness fetch failure handling, block no longer pending", "peer", peer, "hash", hash)
			return nil, "", false
		}
		m.handleWitnessFetchFailureExt(hash, peer, wrappedErr, false)
		return nil, "", false
	}

	// Successful request creation — verify the block is still pending.
	if !m.isPending(hash) {
		log.Debug("[wm] Skipping witness fetch, block no longer pending", "peer", peer, "hash", hash)
		m.handleWitnessFetchFailureExt(hash, "", fmt.Errorf("request initiation failed: %w", err), false)
		req.Close()
		return nil, "", false
	}
	return req, peer, true
}

// awaitWitnessResponse waits for the fetch response, delegating to success
// or failure handlers depending on the outcome.
func (m *witnessManager) awaitWitnessResponse(peer string, hash common.Hash, resCh chan *eth.Response, announcedAt time.Time) {
	timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
	defer timeout.Stop()

	select {
	case res := <-resCh:
		m.processWitnessResponse(peer, hash, res, announcedAt)
	case <-timeout.C:
		log.Info("[wm] Witness fetch timed out for peer", "peer", peer, "hash", hash)
		m.handleWitnessFetchFailureExt(hash, peer, errors.New("fetch timeout"), false)
	case <-m.parentQuit:
		log.Debug("[wm] Witness fetch cancelled due to shutdown", "peer", peer, "hash", hash)
	}
}

// processWitnessResponse validates the witness payload and dispatches to the
// success handler if it passes checks.
func (m *witnessManager) processWitnessResponse(peer string, hash common.Hash, res *eth.Response, announcedAt time.Time) {
	if res == nil {
		log.Debug("[wm] Witness response channel closed unexpectedly", "peer", peer, "hash", hash)
		m.handleWitnessFetchFailureExt(hash, peer, errors.New("response channel closed"), false)
		return
	}
	res.Done <- nil // Signal consumption

	// Assuming NewWitnessPacket contains only one witness.
	witness, ok := res.Res.([]*stateless.Witness)
	if !ok {
		log.Debug("[wm] Invalid witness response type received", "peer", peer, "hash", hash, "type", fmt.Sprintf("%T", res.Res))
		m.handleWitnessFetchFailureExt(hash, peer, errors.New("invalid response type"), false)
		return
	}
	if len(witness) == 0 {
		// Empty/unavailable response: the peer doesn't have the body yet
		// (e.g. WIT2 announce-only relayer that has not finished importing).
		// This is the expected steady state on the WIT2 fast path, not a
		// failure — back off the request (keeping the responder; dropping on
		// "no body" is what makes announce-only fallback peers unsafe to ask,
		// which would erase the WIT2 multi-hop latency win at hop>=2).
		log.Debug("[wm] Received empty witness response from peer", "peer", peer, "hash", hash)
		m.handleWitnessBodyNotReady(hash)
		return
	}

	// WIT2: byte-correctness check. If we have a BP-signed announcement on
	// file for this block, the encoded witness bytes must hash to the
	// signed witnessHash. State-root failures (content-correctness) are
	// handled later in the import path and do NOT drop the server.
	body, witnessHash, ok := m.verifyAgainstSignedHash(peer, hash, witness[0])
	if !ok {
		return
	}

	// WIT2: hand the verified bytes to the handler for pre-import serving.
	// Done before import-side enqueue so a peer asking us for the body
	// during the chain-write window gets bytes from the in-flight cache
	// rather than empty results. body is nil on the WIT1 path (no signed
	// hash on file) — cacheVerifiedWitnessForServing no-ops in that case.
	m.cacheVerifiedWitnessForServing(hash, body, witnessHash)

	metrics.RecordPerItemDuration(blockWitnessItemDownloadTimer, res.Time, 1)
	m.handleWitnessFetchSuccess(peer, hash, witness[0], announcedAt)
}

// handleWitnessFetchSuccess processes a successfully fetched witness.
// It needs the original origin from the op state for consistency checks.
func (m *witnessManager) handleWitnessFetchSuccess(fetchPeer string, hash common.Hash, witness *stateless.Witness, announcedAt time.Time) {
	m.mu.Lock()
	state, exists := m.pending[hash]
	if !exists {
		m.mu.Unlock()
		// Block is no longer pending (e.g., already imported, timed out elsewhere, forgotten)
		log.Debug("[wm] Witness received, but block no longer pending", "peer", fetchPeer, "hash", hash)
		return
	}
	// Check if witness already arrived via broadcast
	if state.op.witness != nil {
		m.mu.Unlock()
		log.Debug("[wm] Witness received via fetch, but already present (likely from broadcast)", "peer", fetchPeer, "hash", hash)
		return // Already handled
	}

	log.Debug("[wm] Witness received via fetch, queuing block for import", "peer", fetchPeer, "origin", state.op.origin, "number", state.op.number(), "hash", hash)

	// Attach witness (under lock)
	state.op.witness = witness
	m.mu.Unlock()

	// Update timestamps on the block
	if state.op.block != nil {
		state.op.block.ReceivedAt = time.Now() // Use witness arrival time
		// Use the announce time from when the fetch was scheduled as AnnouncedAt
		// Note: This might not be the *absolute* first announcement time.
		state.op.block.AnnouncedAt = &announcedAt
	}

	// Enqueue and clean up pending state
	m.safeEnqueue(state.op)
}

// rescheduleWitness resets the internal timer to the next required wake-up time.
func (m *witnessManager) rescheduleWitness() {
	m.mu.Lock()
	m.stopAndDrainTimer()
	earliest := m.earliestPendingAnnounceLocked()
	m.mu.Unlock()

	if earliest.IsZero() {
		log.Debug("[wm] No pending witness fetches, timer stopped")
		return
	}

	// Clamp to a small positive value so we don't spin on already-due fetches.
	delay := time.Until(earliest.Add(gatherSlack))
	if delay <= 0 {
		delay = 10 * time.Millisecond
	}
	m.witnessTimer.Reset(delay)

	// Nudge the main loop to re-evaluate the select with the new timer.
	select {
	case m.pokeCh <- struct{}{}:
	default:
	}
}

// earliestPendingAnnounceLocked returns the earliest announce time among
// pending entries that still need a witness fetched. Returns zero time if
// no such entries exist. Caller must hold m.mu.
func (m *witnessManager) earliestPendingAnnounceLocked() time.Time {
	var earliest time.Time
	for _, state := range m.pending {
		if state.announce == nil || state.op == nil || state.op.witness != nil {
			continue
		}
		if earliest.IsZero() || state.announce.time.Before(earliest) {
			earliest = state.announce.time
		}
	}
	return earliest
}

// handleWitnessFetchFailureExt handles a witness fetch failure with an option
// to remove the pending request entirely (hard failure) or to keep it for
// retries (soft failure).
func (m *witnessManager) handleWitnessFetchFailureExt(hash common.Hash, peer string, fetchErr error, removePending bool) {
	log.Debug("[wm] Witness fetch failed", "hash", hash, "peer", peer, "err", fetchErr, "removePending", removePending)

	m.mu.Lock()
	if removePending {
		delete(m.pending, hash)
		m.clearSignedHashMismatch(hash)
	} else {
		if state := m.pending[hash]; state != nil {
			// back-off before next retry
			state.announce.time = time.Now()
		}
	}
	m.mu.Unlock()

	if peer != "" {
		m.parentDropPeer(peer)
	}

	m.rescheduleWitness()
}

// safeEnqueue attempts to enqueue a completed operation (block+witness) via the parent's channel.
func (m *witnessManager) safeEnqueue(op *blockOrHeaderInject) {
	hash := op.hash()

	m.mu.Lock()
	// Safety check: make sure we have a valid operation with witness
	if op.witness == nil {
		// This should ideally not happen if called correctly
		log.Error("[wm] safeEnqueue called with nil witness", "hash", hash, "origin", op.origin)
		delete(m.pending, hash) // Clean up broken state
		m.mu.Unlock()
		m.rescheduleWitness()
		return
	}

	// Remove the pending state while holding the lock, ensuring any concurrent
	// isPending checks will see the updated state.
	delete(m.pending, hash)
	m.clearSignedHashMismatch(hash)
	m.mu.Unlock()

	// Now with lock released, attempt to send the request to parent fetcher
	req := &enqueueRequest{op: op}
	select {
	case m.parentEnqueueCh <- req:
		log.Debug("[wm] Successfully enqueued completed block+witness", "hash", hash, "origin", op.origin, "number", op.number())
	case <-m.parentQuit:
		log.Debug("[wm] Failed to enqueue block+witness, fetcher shutting down", "hash", hash)
		// Nothing more to do; the parent is quitting.
	}

	// Ensure timer reflects potential state change
	m.rescheduleWitness()
}

// forget cleans up any pending state for a given hash. Called when a block is
// imported or discarded by the main fetcher *before* witness handling completed.
func (m *witnessManager) forget(hash common.Hash) {
	m.mu.Lock()
	if _, exists := m.pending[hash]; exists {
		log.Debug("[wm] Forgetting pending witness state", "hash", hash)
		delete(m.pending, hash)
	}
	m.clearSignedHashMismatch(hash)
	m.mu.Unlock()
	// Ensure timer reflects potential state change
	m.rescheduleWitness()
}

// isPending checks if a witness fetch is currently active or queued for a given hash.
func (m *witnessManager) isPending(hash common.Hash) bool {
	m.mu.Lock()
	_, exists := m.pending[hash]
	m.mu.Unlock()
	return exists
}

// isWitnessUnavailable checks if a witness is currently blacklisted as unavailable.
func (m *witnessManager) isWitnessUnavailable(hash common.Hash) bool {
	m.mu.Lock()
	expiry, exists := m.witnessUnavailable[hash]
	if exists {
		if time.Now().Before(expiry) {
			m.mu.Unlock()
			return true
		}
		// Entry expired, clean it up
		delete(m.witnessUnavailable, hash)
	}
	m.mu.Unlock()
	return false
}

// markWitnessUnavailable adds a hash to the temporary blacklist.
func (m *witnessManager) markWitnessUnavailable(hash common.Hash) {
	expiry := time.Now().Add(witnessUnavailableTimeout)
	log.Debug("[wm] Marking witness as unavailable", "hash", hash, "until", expiry)
	m.mu.Lock()
	m.witnessUnavailable[hash] = expiry
	// Remove from pending state if it exists, as we won't fetch it now
	delete(m.pending, hash)
	// Clear any signed-hash mismatch/quarantine state for this block too. This is
	// the fourth pending-removal exit (alongside the soft-fail removePending,
	// safeEnqueue, and forget paths); without it a quarantined hash that then
	// exhausts its fetch retries leaks its quarantine entry for the process
	// lifetime (the quarantine maps have no TTL or periodic GC).
	m.clearSignedHashMismatch(hash)
	m.mu.Unlock()

	m.rescheduleWitness() // Recalculate timer based on remaining pending items
}

// cleanupUnavailableCache removes expired entries from the witnessUnavailable map.
func (m *witnessManager) cleanupUnavailableCache() {
	now := time.Now()
	cleaned := 0
	m.mu.Lock()
	for hash, expiry := range m.witnessUnavailable {
		if now.After(expiry) {
			delete(m.witnessUnavailable, hash)
			cleaned++
		}
	}
	m.mu.Unlock()
	if cleaned > 0 {
		log.Debug("[wm] Cleaned up unavailable witness cache", "removed", cleaned, "remaining", len(m.witnessUnavailable))
	}
}

// handleFilterResult processes headers or bodies received from the network,
// identifying blocks that now require witness fetching.
// This is called from BlockFetcher's FilterHeaders case for empty blocks.
func (m *witnessManager) handleFilterResult(announce *blockAnnounce, block *types.Block) {
	m.mu.Lock()

	hash := block.Hash()
	log.Debug("[wm] Handling filter result (empty block check)", "hash", hash, "peer", announce.origin)

	// Check if witness is needed and fetch function is available
	if announce.fetchWitness == nil {
		log.Debug("[wm] Filter result block does not require witness", "hash", hash)
		m.mu.Unlock()
		return // BlockFetcher will enqueue directly
	}

	m.mu.Unlock()

	// Check if witness is known to be unavailable
	if m.isWitnessUnavailable(hash) {
		log.Debug("[wm] Witness for filter result block known to be unavailable, discarding", "hash", hash)
		return
	}

	m.mu.Lock()
	if _, exists := m.pending[hash]; exists {
		m.mu.Unlock()
		log.Debug("[wm] Block from filter result already pending witness", "hash", hash)
		return
	}
	m.mu.Unlock()

	// Check if we have a cached witness for this block
	if item := m.witnessCache.Get(hash); item != nil {
		cached := item.Value()
		// Use the cached witness
		op := &blockOrHeaderInject{
			origin:  announce.origin,
			block:   block,
			witness: cached.witness,
		}
		m.witnessCache.Delete(hash)
		log.Debug("[wm] Found cached witness for filter result block, using it", "hash", hash, "cachedPeer", cached.peer)
		m.safeEnqueue(op)
		return
	}

	log.Debug("[wm] Block from filter result requires witness, adding to pending", "hash", hash, "peer", announce.origin)
	state := &witnessRequestState{
		op: &blockOrHeaderInject{ // Create the op here
			origin: announce.origin,
			block:  block, // The header-only block
		},
		announce: &blockAnnounce{ // Copy relevant details from original announce
			origin:       announce.origin,
			hash:         hash,
			number:       block.NumberU64(),
			time:         time.Now(), // Ready to fetch now
			fetchWitness: announce.fetchWitness,
		},
	}

	m.mu.Lock()
	m.pending[hash] = state
	m.mu.Unlock()

	m.rescheduleWitness()
}

// checkCompleting is called from blockFetcher's bodyFilter case when a block body arrives for a
// previously header-only request that might need a witness.
func (m *witnessManager) checkCompleting(announce *blockAnnounce, block *types.Block) {
	// We'll use locking similar.
	hash := block.Hash()
	log.Debug("[wm] Checking completed block from bodyFilter", "hash", hash, "peer", announce.origin)

	if announce.fetchWitness != nil {
		if m.isWitnessUnavailable(hash) {
			log.Debug("[wm] Witness for completed block known to be unavailable, discarding", "hash", hash)
			return
		}

		m.mu.Lock()
		if _, exists := m.pending[hash]; exists {
			m.mu.Unlock()
			log.Debug("[wm] Block already pending witness (from checkCompleting)", "hash", hash)
			return // Already being handled
		}
		m.mu.Unlock()

		// Check if block known locally (might have been imported between header and body arrival)
		if m.parentGetBlock(hash) != nil {
			log.Debug("[wm] Completed block already known locally", "hash", hash)
			return
		}

		log.Debug("[wm] Completed block requires witness, adding to pending", "hash", hash, "peer", announce.origin)
		state := &witnessRequestState{
			op: &blockOrHeaderInject{ // Create the op here
				origin: announce.origin,
				block:  block, // The now complete block
			},
			announce: &blockAnnounce{ // Copy relevant details from original announce
				origin:       announce.origin,
				hash:         hash,
				number:       block.NumberU64(),
				time:         time.Now(), // Ready to fetch now
				fetchWitness: announce.fetchWitness,
			},
		}
		m.mu.Lock()
		m.pending[hash] = state
		m.mu.Unlock()
		m.rescheduleWitness()
	} else {
		// No witness needed, BlockFetcher should enqueue directly
		log.Debug("[wm] Completed block does not require witness", "hash", hash)
	}
}

var ErrNoWitnessPeerAvailable = errors.New("no peer with witness available") // Define a potential specific error

// calculatePageThreshold calculates the dynamic page threshold based on gas ceiling
// Formula: ceil(gasCeil (in millions) / maxPageSizeMB)
// Example: 50M gas / 15MB per page = ceil(3.33) = 4 pages
func (m *witnessManager) calculatePageThreshold() uint64 {
	// Try to get the actual gas limit from the current block header
	if m.parentCurrentHeader != nil {
		if header := m.parentCurrentHeader(); header != nil {
			actualGasLimit := header.GasLimit
			gasCeilMB := actualGasLimit / gasPerMB

			// Ceiling division: (a + b - 1) / b
			threshold := (gasCeilMB + maxPageSizeMB - 1) / maxPageSizeMB

			// Ensure minimum threshold of 1 page
			if threshold < 1 {
				threshold = 1
			}

			log.Debug("[wm] Calculated dynamic page threshold from block header",
				"blockNumber", header.Number.Uint64(), "blockGasLimit", actualGasLimit,
				"gasCeilMB", gasCeilMB, "threshold", threshold)
			witnessThresholdGauge.Update(int64(threshold))
			return threshold
		}
	}

	// Fallback to config value if header not available
	if m.gasCeil == 0 {
		witnessThresholdGauge.Update(int64(witnessPageThreshold))
		return witnessPageThreshold // Return default if gas ceil not set
	}

	// Convert gas ceil to millions and divide by max page size in MB using ceiling division
	gasCeilMB := m.gasCeil / gasPerMB

	// Ceiling division: (a + b - 1) / b
	threshold := (gasCeilMB + maxPageSizeMB - 1) / maxPageSizeMB

	// Ensure minimum threshold of 1 page
	if threshold < 1 {
		threshold = 1
	}

	log.Debug("[wm] Calculated dynamic page threshold from config", "gasCeil", m.gasCeil, "gasCeilMB", gasCeilMB, "threshold", threshold)
	witnessThresholdGauge.Update(int64(threshold))
	return threshold
}

// getConsensusPageCountWithOriginal gets consensus page count including the original peer
func (m *witnessManager) getConsensusPageCountWithOriginal(peers []string, hash common.Hash, originalPageCount uint64, getWitnessPageCount func(peer string, hash common.Hash) (uint64, error)) uint64 {
	// Start with the original peer's count
	countMap := make(map[uint64]int)
	countMap[originalPageCount] = 1

	// Query random peers and add their counts
	for _, peer := range peers {
		pageCount, err := getWitnessPageCount(peer, hash)
		if err == nil {
			countMap[pageCount]++
		}
	}

	// Find the most common page count (majority vote)
	var maxCount int
	var consensusCount uint64
	for count, freq := range countMap {
		if freq > maxCount {
			maxCount = freq
			consensusCount = count
		}
	}

	// Log the consensus result
	log.Debug("[wm] Consensus calculation", "counts", countMap, "consensus", consensusCount, "maxVotes", maxCount)

	// Only return consensus if we have majority (at least 2 out of 3)
	if maxCount >= 2 {
		return consensusCount
	}

	// No clear majority, return 0 (will be treated as no consensus)
	return 0
}

// CheckWitnessPageCount checks if a witness page count should trigger verification
// Returns true if peer is honest (or under threshold), false if peer should be dropped
func (m *witnessManager) CheckWitnessPageCount(hash common.Hash, pageCount uint64, peer string, getRandomPeers func() []string, getWitnessPageCount func(peer string, hash common.Hash) (uint64, error)) bool {
	// Track that a verification check is being performed
	witnessVerifyCheckMeter.Mark(1)

	// Calculate dynamic threshold based on gas ceiling
	threshold := m.calculatePageThreshold()

	// If page count is within threshold, no verification needed
	// No peer queries are made in this case
	if pageCount <= threshold {
		log.Debug("[wm] Witness page count within threshold, no verification needed", "peer", peer, "pageCount", pageCount, "threshold", threshold)
		witnessPageCountBelowThresholdMeter.Mark(1)
		return true
	}

	// Page count exceeds threshold - verify synchronously
	log.Debug("[wm] Witness page count exceeds threshold, running synchronous verification", "peer", peer, "hash", hash, "pageCount", pageCount, "threshold", threshold)
	witnessPageCountAboveThresholdMeter.Mark(1)
	return m.verifyWitnessPageCountSync(hash, pageCount, peer, getRandomPeers, getWitnessPageCount)
}

// verifyWitnessPageCountSync verifies a witness page count synchronously and returns result
func (m *witnessManager) verifyWitnessPageCountSync(hash common.Hash, reportedPageCount uint64, reportingPeer string, getRandomPeers func() []string, getWitnessPageCount func(peer string, hash common.Hash) (uint64, error)) bool {
	// Get random peers for verification
	randomPeers := getRandomPeers()
	if len(randomPeers) < witnessVerificationPeers {
		// Not enough peers for verification, assume honest (conservative approach)
		log.Debug("[wm] Not enough peers for verification, assuming honest", "peer", reportingPeer, "availablePeers", len(randomPeers))
		witnessVerifyPeersInsuffMeter.Mark(1)
		return true
	}

	// Select random peers for verification
	selectedPeers := randomPeers[:witnessVerificationPeers]

	// Query selected peers for page count and include original peer's count in consensus
	consensusPageCount := m.getConsensusPageCountWithOriginal(selectedPeers, hash, reportedPageCount, getWitnessPageCount)

	// Determine if original peer is honest based on majority consensus
	if consensusPageCount != reportedPageCount && consensusPageCount != 0 {
		// Peer is dishonest - drop and jail immediately
		log.Warn("Dropping dishonest peer - consensus verification failed", "peer", reportingPeer, "reported", reportedPageCount, "consensus", consensusPageCount)
		witnessVerifyFailureMeter.Mark(1)

		m.parentDropPeer(reportingPeer)
		witnessVerifyDropMeter.Mark(1)

		// Also jail the peer to prevent reconnection
		if m.parentJailPeer != nil {
			log.Warn("Jailing dishonest peer", "peer", reportingPeer)
			m.parentJailPeer(reportingPeer)
			witnessVerifyJailMeter.Mark(1)
		}
		return false
	}

	// Track no consensus case
	if consensusPageCount == 0 {
		witnessVerifyNoConsensusMeter.Mark(1)
	}

	// Peer is honest or no consensus (assume honest to avoid false positives)
	log.Debug("[wm] Peer verification successful", "peer", reportingPeer, "pageCount", reportedPageCount, "hash", hash)
	witnessVerifySuccessMeter.Mark(1)
	return true
}
