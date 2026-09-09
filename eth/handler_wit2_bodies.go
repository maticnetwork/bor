package eth

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
)

// wit2 announce-cache lifecycle constants.
const (
	// wit2AnnounceTTL bounds how long we remember a signed announcement so we
	// can re-emit it on body delivery and skip duplicate relays. Must outlast
	// typical fetch+import latency so producers/relayers still have the
	// signature when stateless peers come asking for the body.
	wit2AnnounceTTL = 30 * time.Second

	// wit2RelayWindow is the per-(blockHash, peer) duplicate-suppression window.
	// Even without this, knownWitnesses dedup blocks repeats; the window adds
	// belt-and-suspenders coverage during the brief gap between receive and
	// known-cache update under concurrent gossip storms.
	wit2RelayWindow = 200 * time.Millisecond

	// witnessBodyCacheCapacity bounds the number of pre-import witness bodies
	// held in memory. Each entry is ~50MB on Polygon, so the cap keeps total
	// memory under ~500MB worst case. Older entries are evicted as new ones
	// arrive; a 10-block window comfortably covers typical block-fetch and
	// import latency.
	witnessBodyCacheCapacity = 10
)

// pendingWitnessBody holds RLP-encoded witness bytes received from the network
// before the corresponding block has been imported (and thus before the bytes
// have been written to chain storage). Lets serving peers answer GetWitness
// requests during the import gap, which is what makes early relay actually
// useful — a peer that received the body can serve it the moment its TCP
// receive completes, rather than waiting ~500ms for full block validation.
type pendingWitnessBody struct {
	bytes       []byte
	witnessHash common.Hash
	receivedAt  time.Time
}

// pendingWitnessBodyCache holds bytes by block hash with a short TTL. Entries
// are dropped after the body has been written to chain storage, or after the
// TTL expires (whichever first). The cache is a simple map; the witness body
// is large (~50MB) so the cap is set conservatively.
type pendingWitnessBodyCache struct {
	mu       sync.RWMutex
	entries  map[common.Hash]*pendingWitnessBody
	capacity int
}

func newPendingWitnessBodyCache(capacity int) *pendingWitnessBodyCache {
	return &pendingWitnessBodyCache{
		entries:  make(map[common.Hash]*pendingWitnessBody),
		capacity: capacity,
	}
}

func (c *pendingWitnessBodyCache) put(blockHash common.Hash, bytes []byte, witnessHash common.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gcLocked()
	// Only evict for a genuinely new key. An overwrite for a hash already in the
	// cache is net-zero on slot count (cacheVerifiedWitnessForServing on fetch
	// success and acceptSignedBroadcast on a near-simultaneous push can both put
	// the same block); evicting on overwrite would drop an unrelated live entry
	// and silently shrink the cache below capacity. Mirrors deferredAnnounceCache.
	if _, exists := c.entries[blockHash]; !exists && len(c.entries) >= c.capacity {
		// Evict the oldest entry. Linear scan is fine at the configured cap.
		var oldestHash common.Hash
		var oldest time.Time
		for h, e := range c.entries {
			if oldest.IsZero() || e.receivedAt.Before(oldest) {
				oldest = e.receivedAt
				oldestHash = h
			}
		}
		delete(c.entries, oldestHash)
	}
	c.entries[blockHash] = &pendingWitnessBody{
		bytes:       bytes,
		witnessHash: witnessHash,
		receivedAt:  time.Now(),
	}
}

func (c *pendingWitnessBodyCache) get(blockHash common.Hash) ([]byte, common.Hash, bool) {
	c.mu.RLock()
	e, ok := c.entries[blockHash]
	if !ok {
		c.mu.RUnlock()
		return nil, common.Hash{}, false
	}
	if time.Since(e.receivedAt) > wit2AnnounceTTL {
		// Expired: drop the large byte slice now rather than waiting for the
		// next put() to gc. Without this, a node that stops receiving witness
		// bodies retains up to capacity (10) ~50MB blobs indefinitely past the
		// TTL, since gcLocked() only fires on put().
		c.mu.RUnlock()
		c.mu.Lock()
		// Re-check under the write lock: a concurrent put() may have replaced
		// the entry with a fresh one we should not delete.
		if cur, ok2 := c.entries[blockHash]; ok2 && cur == e {
			delete(c.entries, blockHash)
		}
		c.mu.Unlock()
		return nil, common.Hash{}, false
	}
	c.mu.RUnlock()
	return e.bytes, e.witnessHash, true
}

func (c *pendingWitnessBodyCache) drop(blockHash common.Hash) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, blockHash)
}

func (c *pendingWitnessBodyCache) gcLocked() {
	cutoff := time.Now().Add(-wit2AnnounceTTL)
	for h, e := range c.entries {
		if e.receivedAt.Before(cutoff) {
			delete(c.entries, h)
		}
	}
}

const (
	// witnessWaiterHashCap bounds how many block hashes we track waiters for.
	// Entries are tiny (a peer pointer + timestamp); the cap is a backstop
	// against a peer asking for many distinct not-yet-available hashes.
	witnessWaiterHashCap = 256

	// witnessWaiterPerHashCap bounds waiters recorded per hash so a burst of
	// distinct peers asking for the same not-yet-available witness can't grow a
	// single bucket without bound.
	witnessWaiterPerHashCap = 64

	// witnessWaiterTTL drops stale waiter entries (peer gave up, disconnected,
	// or obtained the body elsewhere). Aligned with the body cache TTL.
	witnessWaiterTTL = 30 * time.Second
)

// witnessWaiter records a peer that asked us for a witness body we did not yet
// have. We only record a waiter when a BP-signed announcement is on file for
// the hash, so the witness is known to exist and the registry is bounded by
// real, signed blocks rather than arbitrary peer-chosen hashes.
type witnessWaiter struct {
	peer *wit.Peer
	at   time.Time
}

// witnessWaiterRegistry tracks peers awaiting a witness body so we can push it
// to them the moment we obtain it. This restores the WIT1-style hand-off the
// WIT2 fast announce removed: WIT1 only ever announces a witness it already
// holds (and the announce marks the sender a body-holder), so a stateless
// consumer's first pull lands; WIT2 relays the signed announce ahead of the
// body, leaving the consumer to poll an announce-only relayer with repeated
// empty GetWitness until it catches up. Pushing on arrival closes that gap
// without flooding — at most one body per peer that actually asked, exactly the
// bandwidth a successful pull would have cost.
type witnessWaiterRegistry struct {
	mu      sync.Mutex
	waiters map[common.Hash]map[string]*witnessWaiter
}

func newWitnessWaiterRegistry() *witnessWaiterRegistry {
	return &witnessWaiterRegistry{waiters: make(map[common.Hash]map[string]*witnessWaiter)}
}

// record notes that peer is waiting for the body of hash. No-op for a nil peer.
func (r *witnessWaiterRegistry) record(hash common.Hash, peer *wit.Peer) {
	if peer == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gcLocked()

	per, ok := r.waiters[hash]
	if !ok {
		if len(r.waiters) >= witnessWaiterHashCap {
			// Registry full of distinct hashes; skip recording rather than
			// evict. The peer simply keeps polling (with backoff) and lands the
			// body on a later GetWitness — correctness is unaffected.
			return
		}
		per = make(map[string]*witnessWaiter)
		r.waiters[hash] = per
	}
	if _, exists := per[peer.ID()]; !exists && len(per) >= witnessWaiterPerHashCap {
		return
	}
	per[peer.ID()] = &witnessWaiter{peer: peer, at: time.Now()}
}

// has reports whether any non-expired waiter is recorded for hash. Used to skip
// the witness decode on the push path when nobody is waiting.
func (r *witnessWaiterRegistry) has(hash common.Hash) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	per, ok := r.waiters[hash]
	if !ok {
		return false
	}
	cutoff := time.Now().Add(-witnessWaiterTTL)
	for _, w := range per {
		if !w.at.Before(cutoff) {
			return true
		}
	}
	return false
}

// take returns and clears the live (non-expired) waiters for hash.
func (r *witnessWaiterRegistry) take(hash common.Hash) []*wit.Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	per, ok := r.waiters[hash]
	if !ok {
		return nil
	}
	delete(r.waiters, hash)
	cutoff := time.Now().Add(-witnessWaiterTTL)
	out := make([]*wit.Peer, 0, len(per))
	for _, w := range per {
		if w.at.Before(cutoff) {
			continue
		}
		out = append(out, w.peer)
	}
	return out
}

// forget removes peerID from every hash bucket it waits on, dropping any bucket
// left empty. Called on peer disconnect so a departed peer's *wit.Peer (and the
// per-peer caches it transitively pins) is released immediately rather than
// lingering until the TTL — symmetric with wit2PeerTracker.forget on the same
// teardown path.
func (r *witnessWaiterRegistry) forget(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for h, per := range r.waiters {
		if _, ok := per[peerID]; !ok {
			continue
		}
		delete(per, peerID)
		if len(per) == 0 {
			delete(r.waiters, h)
		}
	}
}

// gcLocked drops expired waiter entries and empty buckets. Caller holds r.mu.
func (r *witnessWaiterRegistry) gcLocked() {
	cutoff := time.Now().Add(-witnessWaiterTTL)
	for h, per := range r.waiters {
		for id, w := range per {
			if w.at.Before(cutoff) {
				delete(per, id)
			}
		}
		if len(per) == 0 {
			delete(r.waiters, h)
		}
	}
}
