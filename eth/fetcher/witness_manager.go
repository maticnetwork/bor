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

	//	"github.com/ethereum/go-ethereum/eth/protocols/eth" // Not directly needed here
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
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
	maxWitnessFetchRetries = 10

	// witnessFailurePenalty defines how long a peer is considered "penalised"
	// after a witness-related failure. While a peer is penalised, the
	// witnessManager will not initiate new witness requests for that peer and
	// the BlockFetcher will also ignore its announcements that require a
	// witness. Once the penalty window elapses the peer is automatically
	// eligible again.
	witnessFailurePenalty = 5 * time.Second

	witnessCacheSize = 10
	witnessCacheTTL  = 2 * time.Minute
)

// witnessRequestState tracks the state of a pending witness request.
type witnessRequestState struct {
	op       *blockOrHeaderInject // The original block/header injection operation.
	announce *blockAnnounce       // Announcement details, non-nil if a fetch is in flight.
	retries  int                  // Number of fetch attempts already made
}

// cachedWitness represents a witness that arrived before its corresponding block
type cachedWitness struct {
	witness   *stateless.Witness
	peer      string
	timestamp time.Time
}

// witnessManager handles the logic specific to fetching and managing witnesses
// for blocks, isolating it from the main BlockFetcher loop.
type witnessManager struct {
	// Parent fetcher fields/methods required
	parentQuit        <-chan struct{}        // Parent fetcher's quit channel
	parentDropPeer    peerDropFn             // Function to drop a misbehaving peer
	parentEnqueueCh   chan<- *enqueueRequest // Channel to send completed blocks+witnesses back
	parentGetBlock    blockRetrievalFn       // Function to check if block is known locally
	parentGetHeader   HeaderRetrievalFn      // Function to check if header is known locally (needed for checks)
	parentChainHeight chainHeightFn          // Retrieve chain height for distance checks

	// Witness-specific state
	pending               map[common.Hash]*witnessRequestState         // Blocks waiting for witness or actively fetching.
	failedWitnessAttempts map[string]int                               // Tracks witness fetch failures per peer.
	peerPenalty           map[string]time.Time                         // Tracks peers currently penalised until a given time.
	witnessUnavailable    map[common.Hash]time.Time                    // Tracks hashes whose witnesses are known to be unavailable, with expiry times.
	witnessCache          *ttlcache.Cache[common.Hash, *cachedWitness] // TTL cache of witnesses that arrived before their blocks

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
	parentEnqueueCh chan<- *enqueueRequest,
	parentGetBlock blockRetrievalFn,
	parentGetHeader HeaderRetrievalFn,
	parentChainHeight chainHeightFn,
) *witnessManager {
	// Create TTL cache with 1 minute expiration for witnesses
	witnessCache := ttlcache.New[common.Hash, *cachedWitness](
		ttlcache.WithTTL[common.Hash, *cachedWitness](witnessCacheTTL),
		ttlcache.WithCapacity[common.Hash, *cachedWitness](witnessCacheSize),
	)

	m := &witnessManager{
		parentQuit:            parentQuit,
		parentDropPeer:        parentDropPeer,
		parentEnqueueCh:       parentEnqueueCh,
		parentGetBlock:        parentGetBlock,
		parentGetHeader:       parentGetHeader,
		parentChainHeight:     parentChainHeight,
		pending:               make(map[common.Hash]*witnessRequestState),
		failedWitnessAttempts: make(map[string]int),
		peerPenalty:           make(map[string]time.Time),
		witnessUnavailable:    make(map[common.Hash]time.Time),
		witnessCache:          witnessCache,
		injectNeedWitnessCh:   make(chan *injectBlockNeedWitnessMsg, 10),
		injectWitnessCh:       make(chan *injectedWitnessMsg, 10),
		witnessTimer:          time.NewTimer(0),
		pokeCh:                make(chan struct{}, 1),
	}
	// Clear the timer channel initially
	if !m.witnessTimer.Stop() {
		// Non-blocking read in case the timer fired
		select {
		case <-m.witnessTimer.C:
		default:
		}
	}
	return m
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
		var timerChan <-chan time.Time

		// Check pending count under mutex protection
		m.mu.Lock()
		pendingCount := len(m.pending)
		m.mu.Unlock()

		if pendingCount > 0 {
			// Only listen to timer if there are pending items
			timerChan = m.witnessTimer.C
			// If too long since last tick, reset the timer to ensure we don't get stuck
			if time.Since(lastTick) > 10*time.Second {
				log.Debug("[wm] Long time since last tick, forcing timer reset", "sinceLastTick", time.Since(lastTick))
				m.rescheduleWitness()
				lastTick = time.Now()
			}
		} else {
			// Ensure timer is stopped if nothing is pending
			if !m.witnessTimer.Stop() {
				select {
				case <-m.witnessTimer.C:
				default:
				}
			}
		}

		select {
		// Handle signals from parent BlockFetcher
		case <-m.parentQuit:
			log.Info("Witness manager stopping")
			return

		// Handle injected blocks needing witness
		case msg, ok := <-m.injectNeedWitnessCh:
			if !ok {
				log.Debug("Witness manager injectNeedWitnessCh closed unexpectedly")
				m.injectNeedWitnessCh = nil // Avoid busy-looping
				continue
			}
			log.Debug("[wm] Received injectNeedWitnessCh message", "hash", msg.block.Hash())
			m.handleNeed(msg)

		// Handle injected witnesses from broadcast
		case msg, ok := <-m.injectWitnessCh:
			if !ok {
				log.Debug("Witness manager injectWitnessCh closed unexpectedly")
				m.injectWitnessCh = nil // Avoid busy-looping
				continue
			}
			log.Debug("[wm] Received injectWitnessCh message", "hash", msg.witness.Header().Hash())
			m.handleBroadcast(msg)

		// Handle witness timer triggers
		case <-timerChan: // Listen on the conditional channel
			lastTick = time.Now()
			log.Debug("[wm] Witness timer triggered", "time", lastTick, "pendingCount", len(m.pending))
			m.tick()

		// Handle periodic cleanup of the unavailable witness cache
		case <-cleanupTicker.C:
			log.Debug("[wm] Cleanup ticker triggered")
			m.cleanupUnavailableCache()

		// A poke indicates the timer was rescheduled by another goroutine. We
		// simply loop around so that the timer channel is re-evaluated with the
		// new configuration.
		case <-m.pokeCh:
			lastTick = time.Now()
			continue
		}
	}
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

	// Check if peer is currently penalised (do this outside of main lock to avoid deadlock)
	if m.HasFailedTooManyTimes(msg.origin) {
		log.Debug("[wm] Discarding injected block, peer is currently penalised for witness failures", "peer", msg.origin, "hash", hash)
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

	// Check distance (using parent's function)
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
	// Map from peer ID -> map of block hash -> announce struct
	requests := make(map[string]map[common.Hash]*blockAnnounce)

	now := time.Now()
	readyToFetch := []common.Hash{}

	m.mu.Lock()

	// Debug: Log current pending items at tick time
	if len(m.pending) > 0 {
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

	// Identify pending requests that are ready to be fetched
	for hash, state := range m.pending {
		// Must have an op and announce to be fetchable
		if state.op == nil || state.announce == nil {
			log.Debug("[wm] Invalid pending state found", "hash", hash)
			delete(m.pending, hash) // Clean up invalid state
			continue
		}

		// Witness already present? Should have been enqueued.
		if state.op.witness != nil {
			log.Debug("[wm] Pending state found with witness already present", "hash", hash)
			// we will enqueue outside of lock to avoid deadlock
			readyToFetch = append(readyToFetch, hash) // mark for enqueue path
			continue
		}

		// Check if ready (announce time is in the past)
		if now.After(state.announce.time) {
			// Give up if we've retried too many times
			if state.retries >= maxWitnessFetchRetries {
				log.Debug("[wm] Max witness retries reached, marking unavailable", "hash", hash, "retries", state.retries)
				toMark := hash // avoid referencing loop var later
				m.mu.Unlock()
				m.markWitnessUnavailable(toMark)
				m.mu.Lock()
				continue
			}
			// Increment retry counter and schedule fetch
			state.retries++
			log.Debug("[wm] Scheduling witness fetch", "hash", hash, "retry", state.retries)
			readyToFetch = append(readyToFetch, hash)
		}
	}

	// We may need to enqueue those that already had witness present
	prematureStates := []*blockOrHeaderInject{}
	for _, h := range readyToFetch {
		if st := m.pending[h]; st != nil && st.op != nil && st.op.witness != nil {
			prematureStates = append(prematureStates, st.op)
		}
	}
	m.mu.Unlock()

	// Enqueue outside lock using captured states
	for _, op := range prematureStates {
		log.Debug("[wm] Enqueueing pending block with already attached witness", "hash", op.hash())
		m.safeEnqueue(op)
	}

	// Prepare requests per peer
	m.mu.Lock()
	for _, hash := range readyToFetch {
		state := m.pending[hash]
		if state == nil || state.announce == nil { // Check again, might have been removed
			continue
		}
		announce := state.announce

		// Add to request map
		if _, ok := requests[announce.origin]; !ok {
			requests[announce.origin] = make(map[common.Hash]*blockAnnounce)
		}
		requests[announce.origin][hash] = announce

		// Update announce time for backoff - prevent immediate retry
		// This effectively marks the request as "in-flight"
		announce.time = now.Add(fetchTimeout)
		// state.announce = announce // announce is a pointer, modification is reflected
	}
	m.mu.Unlock()

	// Send out all block witness requests
	for peer, hashAnnounceMap := range requests {
		// Check if peer is currently penalised
		if m.HasFailedTooManyTimes(peer) {
			log.Debug("[wm] Skipping witness fetch batch, peer currently penalised", "peer", peer, "hashes", len(hashAnnounceMap))
			m.mu.Lock()
			if expiry, ok := m.peerPenalty[peer]; ok {
				for h := range hashAnnounceMap {
					if st := m.pending[h]; st != nil {
						st.announce.time = expiry.Add(gatherSlack)
					}
				}
			}
			m.mu.Unlock()
			// Reschedule timer based on the new future times.
			m.rescheduleWitness()
			continue // Skip this peer for now, hashes remain pending
		}

		// Collect hashes for logging
		hashesToFetch := make([]common.Hash, 0, len(hashAnnounceMap))
		for hash := range hashAnnounceMap {
			hashesToFetch = append(hashesToFetch, hash)
		}
		if len(hashesToFetch) == 0 {
			continue
		}
		log.Debug("[wm] Fetching scheduled witnesses", "peer", peer, "list", hashesToFetch)

		// Process each hash for the peer individually
		for hash, announce := range hashAnnounceMap {
			// Ensure we have a valid announce and fetchWitness function
			if announce == nil || announce.fetchWitness == nil {
				log.Debug("[wm] Missing announce or fetchWitness for witness request", "peer", peer, "hash", hash)
				// Hard failure: remove from pending so we don't spin forever
				m.handleWitnessFetchFailureExt(hash, peer, errors.New("missing fetch configuration"), true)
				continue
			}

			// Launch goroutine for fetch
			go m.fetchWitness(peer, hash, announce)
		}
	}

	// Schedule the next fetch if blocks are still pending
	m.rescheduleWitness()
}

// fetchWitness performs a single witness fetch in a goroutine.
func (m *witnessManager) fetchWitness(peer string, hash common.Hash, announce *blockAnnounce) {
	resCh := make(chan *wit.Response)

	announcedAt := announce.time // Capture the original 'ready-to-fetch' time for logging/timestamping
	witnessFetchMeter.Mark(1)

	req, err := announce.fetchWitness(hash, resCh)
	if err != nil {
		log.Debug("[wm] Failed to initiate witness fetch request", "peer", peer, "hash", hash, "err", err)
		// Check if the error specifically indicates no peers were available
		if strings.Contains(err.Error(), "no peer with witness for hash") {
			log.Debug("[wm] Marking witness as unavailable based on fetch initiation error", "hash", hash)
			m.markWitnessUnavailable(hash)
			// Don't penalize the announcing peer in this case
			return
		}

		// For other errors, check if still pending before handling failure
		m.mu.Lock()
		if _, exists := m.pending[hash]; !exists {
			m.mu.Unlock()
			log.Debug("[wm] Skipping witness fetch failure handling, block no longer pending", "peer", peer, "hash", hash)
			return
		}
		m.mu.Unlock()

		// Penalize the peer for other initiation failures
		m.handleWitnessFetchFailureExt(hash, peer, fmt.Errorf("request initiation failed: %w", err), false)
		return
	}

	// Check if still pending after successful request creation
	m.mu.Lock()
	if _, exists := m.pending[hash]; !exists {
		m.mu.Unlock()
		log.Debug("[wm] Skipping witness fetch, block no longer pending", "peer", peer, "hash", hash)
		req.Close()
		return
	}
	m.mu.Unlock()
	defer req.Close()

	timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
	defer timeout.Stop()

	select {
	case res := <-resCh:
		if res == nil {
			log.Debug("[wm] Witness response channel closed unexpectedly", "peer", peer, "hash", hash)
			m.handleWitnessFetchFailureExt(hash, peer, errors.New("response channel closed"), false)
			return
		}
		res.Done <- nil // Signal consumption

		// Assuming NewWitnessPacket contains only one witness.
		packet, ok := res.Res.(*wit.WitnessPacketRLPPacket)
		if !ok {
			log.Debug("[wm] Invalid witness response type received", "peer", peer, "hash", hash, "type", fmt.Sprintf("%T", res.Res))
			m.handleWitnessFetchFailureExt(hash, peer, errors.New("invalid response type"), false)
			return
		}

		if len(packet.WitnessPacketResponse) == 0 {
			log.Debug("[wm] Received empty witness response from peer", "peer", peer, "hash", hash)
			m.handleWitnessFetchFailureExt(hash, peer, errors.New("empty witness response"), false)
			return
		}

		witness := &stateless.Witness{}
		err = rlp.DecodeBytes(packet.WitnessPacketResponse[0], witness)
		if err != nil {
			log.Debug("[wm] Failed to decode witness RLP", "peer", peer, "hash", hash, "err", err)
			m.handleWitnessFetchFailureExt(hash, peer, fmt.Errorf("RLP decoding failed: %w", err), false)
			return
		}

		// Process successful fetch
		m.handleWitnessFetchSuccess(peer, hash, witness, announcedAt)

	case <-timeout.C:
		log.Info("[wm] Witness fetch timed out for peer", "peer", peer, "hash", hash)
		m.handleWitnessFetchFailureExt(hash, peer, errors.New("fetch timeout"), false)
	case <-m.parentQuit:
		log.Debug("[wm] Witness fetch cancelled due to shutdown", "peer", peer, "hash", hash)
		// Don't penalize peer for shutdown
	}
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
	// Stop any existing timer safely under lock
	if !m.witnessTimer.Stop() {
		select {
		case <-m.witnessTimer.C:
		default:
		}
	}
	// Find the earliest time we need to wake up
	var earliest time.Time
	pendingFetches := 0
	for _, state := range m.pending {
		if state.announce != nil && state.op != nil && state.op.witness == nil {
			pendingFetches++
			if earliest.IsZero() || state.announce.time.Before(earliest) {
				earliest = state.announce.time
			}
		}
	}
	m.mu.Unlock()

	if earliest.IsZero() {
		log.Debug("[wm] No pending witness fetches, timer stopped")
		return
	}

	delay := time.Until(earliest.Add(gatherSlack))
	// If delay is negative or zero, set it to a small positive value to trigger soon
	if delay <= 0 {
		delay = 10 * time.Millisecond // Small delay to avoid CPU spinning
	}
	m.witnessTimer.Reset(delay)

	// Nudge the main loop to wake up
	select {
	case m.pokeCh <- struct{}{}:
	default:
	}
}

// handleWitnessFetchFailureExt handles a witness fetch failure with an option
// to remove the pending request entirely (hard failure) or to keep it for
// retries (soft failure).
func (m *witnessManager) handleWitnessFetchFailureExt(hash common.Hash, peer string, fetchErr error, removePending bool) {
	log.Debug("[wm] Witness fetch failed", "hash", hash, "peer", peer, "err", fetchErr, "removePending", removePending)

	m.mu.Lock()
	if removePending {
		delete(m.pending, hash)
	} else {
		if state := m.pending[hash]; state != nil {
			// back-off before next retry
			state.announce.time = time.Now().Add(fetchTimeout)
		}
	}
	// Track peer failures (for metrics)
	m.failedWitnessAttempts[peer]++
	failures := m.failedWitnessAttempts[peer]

	// Penalise peer with time-based window - do this under the same lock
	until := time.Now().Add(witnessFailurePenalty)
	m.peerPenalty[peer] = until
	m.mu.Unlock()

	log.Debug("[wm] Penalising peer for witness fetch failure", "peer", peer, "failures", failures, "penalty", witnessFailurePenalty, "until", until)

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
	m.mu.Unlock()

	// Now with lock released, attempt to send the request to parent fetcher
	req := &enqueueRequest{op: op}
	select {
	case m.parentEnqueueCh <- req:
		log.Debug("[wm] Successfully enqueued completed block+witness", "hash", hash, "origin", op.origin, "number", op.number())
		// Reset failure count and penalty for the originating peer upon success.
		m.mu.Lock()
		delete(m.failedWitnessAttempts, op.origin)
		delete(m.peerPenalty, op.origin)
		m.mu.Unlock()
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

	// Check if peer currently penalised
	if m.HasFailedTooManyTimes(announce.origin) {
		log.Debug("[wm] Discarding block from filter result, peer currently penalised", "peer", announce.origin, "hash", hash)
		return
	}

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

		// Check if peer currently penalised
		if m.HasFailedTooManyTimes(announce.origin) {
			log.Debug("[wm] Discarding completed block, peer currently penalised", "peer", announce.origin, "hash", hash)
			return
		}

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

func (m *witnessManager) HasFailedTooManyTimes(peer string) bool {
	m.mu.Lock()
	ts, ok := m.peerPenalty[peer]
	if !ok {
		m.mu.Unlock()
		return false
	}
	if time.Now().After(ts) {
		delete(m.peerPenalty, peer)
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	return true
}

// penalisePeer marks a peer as penalised for witnessFailurePenalty duration.
func (m *witnessManager) penalisePeer(peer string) {
	until := time.Now().Add(witnessFailurePenalty)
	m.mu.Lock()
	m.peerPenalty[peer] = until
	m.mu.Unlock()
}

var ErrNoWitnessPeerAvailable = errors.New("no peer with witness available") // Define a potential specific error
