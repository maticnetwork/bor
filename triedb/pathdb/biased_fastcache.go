package pathdb

import (
	stdcontext "context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"golang.org/x/time/rate"
)

var (
	// Biased cache metrics for address-specific cache effectiveness
	biasedAddressCacheHitMeter   = metrics.NewRegisteredMeter("pathdb/biased/address/hit", nil)
	biasedAddressCacheMissMeter  = metrics.NewRegisteredMeter("pathdb/biased/address/miss", nil)
	biasedAddressCacheReadMeter  = metrics.NewRegisteredMeter("pathdb/biased/address/read", nil)
	biasedAddressCacheWriteMeter = metrics.NewRegisteredMeter("pathdb/biased/address/write", nil)
)

// AddressBiasedCache is a wrapper around fastcache that maintains separate
// caches for specific addresses and a common cache for everything else.
// It preloads storage trie nodes for specified addresses into dedicated caches.
type AddressBiasedCache struct {
	// Address-specific caches, one per preloaded address
	addressCaches sync.Map // map[common.Hash]*fastcache.Cache

	// Common cache for all other data
	commonCache *fastcache.Cache

	// Set of preloaded addresses for fast lookup
	preloadedAddrs sync.Map // map[common.Hash]struct{}

	// Context for canceling preload operations
	ctx    stdcontext.Context
	cancel stdcontext.CancelFunc
	wg     sync.WaitGroup // Wait for all preloads to finish

	// Rate limiting for preload operations (bytes per second, 0 = unlimited)
	rateLimitBPS int64

	// Directory used to persist/reload per-address caches across restarts.
	// Empty string disables persistence (in-memory-only, matches legacy behavior).
	journalDir string
}

// snapshotPath returns the on-disk path used to persist/reload the given
// address's cache. journalDir is expected to already be an absolute,
// resolved directory (see triedb/pathdb.Config.JournalDirectory).
func snapshotPath(journalDir string, accountHash common.Hash) string {
	return filepath.Join(journalDir, "addresscache", accountHash.Hex()+".cache")
}

// NewAddressBiasedCache creates a new address-biased cache with preloading.
// It scans the database for storage trie nodes of the specified addresses and
// loads them into dedicated caches. The addressCacheSizes maps each address to
// its desired cache size in bytes. The commonCacheSize specifies the size
// of the cache for non-preloaded data. The rateLimitBPS limits preload I/O
// in bytes per second (0 = unlimited).
// Preloading happens asynchronously in the background.
func NewAddressBiasedCache(db ethdb.Database, addressCacheSizes map[common.Address]int, commonCacheSize int, rateLimitBPS int64, journalDir string) (*AddressBiasedCache, error) {
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cache := &AddressBiasedCache{
		commonCache:  fastcache.New(commonCacheSize),
		ctx:          ctx,
		cancel:       cancel,
		rateLimitBPS: rateLimitBPS,
		journalDir:   journalDir,
	}

	// Initialize caches synchronously, but preload asynchronously
	for addr, cacheSize := range addressCacheSizes {
		warm := cache.initAddressCache(addr, cacheSize)
		if warm {
			continue
		}

		// Start async preloading
		cache.wg.Add(1)
		go cache.preloadAddressAsync(db, addr, cacheSize)
	}

	return cache, nil
}

// initAddressCache initializes the cache structure for an address synchronously.
// If a persisted snapshot exists at the address's snapshot path and matches
// the configured cache size, it is reloaded and the cache is considered warm
// (the caller should skip preloadAddressAsync for this address). Otherwise a
// fresh empty cache is created and the cache is considered cold. Staleness of
// a reloaded cache is not a correctness concern: reader.Node already hash-
// verifies every cache hit and evicts+refetches on mismatch, regardless of
// why the cached blob is stale.
func (c *AddressBiasedCache) initAddressCache(addr common.Address, cacheSize int) (warm bool) {
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	var addrCache *fastcache.Cache
	if c.journalDir != "" {
		addrCache = fastcache.LoadFromFileOrNew(snapshotPath(c.journalDir, accountHash), cacheSize)
	} else {
		addrCache = fastcache.New(cacheSize)
	}

	var stats fastcache.Stats
	addrCache.UpdateStats(&stats)
	warm = stats.EntriesCount > 0

	// Mark this address as preloaded
	c.preloadedAddrs.Store(accountHash, struct{}{})
	c.addressCaches.Store(accountHash, addrCache)

	return warm
}

// preloadAddressAsync loads storage trie nodes for the given account hash using
// BFS traversal, prioritizing shallow nodes (most frequently accessed) until
// the cache is full. This naturally loads nodes by depth, filling the cache
// with as many upper-level nodes as possible. This function runs asynchronously.
// Rate limiting is applied to prevent overwhelming the disk during sync.
//
// No visited set is needed: decodeChildPaths decodes the actual trie structure
// and ignores malformed non-growing short nodes. For valid Merkle Patricia Trie
// nodes, child paths are strictly longer than the parent path, making revisits
// structurally impossible.
func (c *AddressBiasedCache) preloadAddressAsync(db ethdb.Database, addr common.Address, cacheSize int) {
	defer c.wg.Done()
	startTime := time.Now()

	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Get the address cache
	cacheValue, ok := c.addressCaches.Load(accountHash)
	if !ok {
		log.Error("Address cache not found during preload", "address", addr.Hex())
		return
	}
	addrCache := cacheValue.(*fastcache.Cache)

	// Create rate limiter if configured (burst of 64KB for smoother throttling)
	var limiter *rate.Limiter
	if c.rateLimitBPS > 0 {
		limiter = rate.NewLimiter(rate.Limit(c.rateLimitBPS), 64*1024)
	}

	// Local stats for logging progress
	var entriesLoaded int
	var totalBytesLoaded uint64

	rateLimitStr := "unlimited"
	if c.rateLimitBPS > 0 {
		rateLimitStr = fmt.Sprintf("%s/s", common.StorageSize(c.rateLimitBPS))
	}
	log.Info("Starting storage trie preload",
		"address", addr.Hex(),
		"account hash", accountHash.Hex(),
		"cache size", common.StorageSize(cacheSize).String(),
		"rate limit", rateLimitStr)

	var maxDepthReached int
	const logInterval = 100000

	// BFS traversal to load nodes by depth until cache is full.
	// We do not maintain a visited set: decodeChildPaths only enqueues paths
	// that correspond to actual trie nodes and ignores malformed non-growing
	// short nodes. Valid MPT paths strictly increase in length with each level,
	// so the traversal is cycle-free by construction.
	type queueItem struct {
		path  []byte
		depth int
	}
	queue := []queueItem{{path: nil, depth: 0}} // Start from root

	for len(queue) > 0 {
		// Check for shutdown signal periodically
		select {
		case <-c.ctx.Done():
			log.Info("Preload interrupted by shutdown",
				"account hash", accountHash.Hex(),
				"entries", entriesLoaded,
				"max depth", maxDepthReached,
				"size", common.StorageSize(totalBytesLoaded).String(),
				"elapsed", time.Since(startTime))
			return
		default:
		}

		item := queue[0]
		queue = queue[1:]

		// Track maximum depth reached
		if item.depth > maxDepthReached {
			maxDepthReached = item.depth
		}

		// Read the node from database
		nodeData := rawdb.ReadStorageTrieNode(db, accountHash, item.path)
		if len(nodeData) == 0 {
			// Node doesn't exist, skip
			continue
		}

		// Apply rate limiting after reading, based on actual bytes read
		if limiter != nil {
			if err := limiter.WaitN(c.ctx, len(nodeData)); err != nil {
				if c.ctx.Err() != nil {
					log.Info("Preload interrupted during shutdown",
						"account hash", accountHash.Hex(),
						"entries", entriesLoaded,
						"max depth", maxDepthReached,
						"size", common.StorageSize(totalBytesLoaded).String(),
						"elapsed", time.Since(startTime))
					return
				}
				// Node exceeds burst size — skip it and continue preloading
				log.Warn("Preload skipping oversized node",
					"account hash", accountHash.Hex(),
					"node size", len(nodeData),
					"burst", limiter.Burst())
				continue
			}
		}

		// Check if adding this node would exceed cache size
		// Key format: owner (32 bytes) + path
		nodeSize := uint64(common.HashLength + len(item.path) + len(nodeData))

		// Preload 66.6% of the cache size to allow hot paths to be added later
		if totalBytesLoaded+nodeSize > uint64(cacheSize*2/3) {
			log.Info("Cache size limit reached, stopping preload",
				"account hash", accountHash.Hex(),
				"entries", entriesLoaded,
				"current depth", item.depth,
				"max depth reached", maxDepthReached,
				"size", common.StorageSize(totalBytesLoaded).String())
			break
		}

		// Construct the cache key using the same format as nodeCacheKey
		// Format: owner (32 bytes) + path
		key := append(accountHash.Bytes(), item.path...)

		// Skip if key already exists to avoid overwriting potentially newer data.
		// Both Has and Set are thread-safe on fastcache (internal sharding), but
		// the Has → Set sequence is not atomic: a flusher's Set(newer) can land
		// between our Has(false) and our Set(older), leaving the cache holding
		// the stale blob.
		//
		// Stale-blob safety has two layers:
		//   1. reader.Node hash-checks every cache hit. A stale blob produces
		//      a hash mismatch (Verkle's noHashCheck path is not used by Bor).
		//   2. On hash mismatch from locCleanCache, reader.Node evicts the
		//      offending entry and retries from disk (see reader.go's
		//      evictCachedNode). The cache self-heals on the next read of
		//      that key — it does not stay poisoned until natural eviction.
		// Worst case is one extra disk fetch per stale-blob occurrence.
		if addrCache.Has(key) {
			continue
		}

		addrCache.Set(key, nodeData)

		entriesLoaded++
		totalBytesLoaded += nodeSize

		// Log progress periodically
		if entriesLoaded%logInterval == 0 {
			log.Info("Preloading storage trie progress",
				"account hash", accountHash.Hex(),
				"entries", entriesLoaded,
				"current depth", item.depth,
				"max depth", maxDepthReached,
				"size", common.StorageSize(totalBytesLoaded).String(),
				"cache usage", fmt.Sprintf("%.1f%%", float64(totalBytesLoaded)*100/float64(cacheSize)),
				"elapsed", time.Since(startTime))
		}

		// Decode actual children from the node and enqueue them.
		// Only real trie children are returned, keeping queue size proportional
		// to trie width rather than growing exponentially with depth.
		childPaths := decodeChildPaths(nodeData, item.path)
		for _, childPath := range childPaths {
			queue = append(queue, queueItem{
				path:  childPath,
				depth: item.depth + 1,
			})
		}
	}

	// Log the completion
	loadTime := time.Since(startTime)
	log.Info("Completed storage trie preload",
		"account hash", accountHash.Hex(),
		"entries", entriesLoaded,
		"max depth", maxDepthReached,
		"size", common.StorageSize(totalBytesLoaded).String(),
		"cache usage", fmt.Sprintf("%.1f%%", float64(totalBytesLoaded)*100/float64(cacheSize)),
		"time", loadTime)
}

// decodeChildPaths decodes an RLP-encoded trie node and returns the nibble paths
// of its actual children relative to currentPath.
//
// Branch nodes (17-element RLP list) yield paths for each non-nil child slot.
// Extension nodes (2-element list, no terminator) yield the single child path.
// Leaf nodes (2-element list, terminator present) yield no children.
// Any decode error yields no children.
//
// Because only real trie children are returned, the caller's BFS queue stays
// proportional to trie width rather than growing as 16^depth. Malformed
// non-growing short nodes are ignored, and valid MPT child paths are strictly
// longer than the parent path, so a visited set is not required.
func decodeChildPaths(nodeData []byte, currentPath []byte) [][]byte {
	var rawNode []rlp.RawValue
	if err := rlp.DecodeBytes(nodeData, &rawNode); err != nil {
		return nil
	}

	switch len(rawNode) {
	case 17: // Branch node — up to 16 children at slots 0–15
		var children [][]byte
		for i := byte(0); i < 16; i++ {
			// A nil child is encoded as RLP empty string: 0x80 (1 byte) or
			// absent entirely (0 bytes). Any other encoding means a child exists.
			if len(rawNode[i]) <= 1 {
				continue
			}
			childPath := make([]byte, len(currentPath)+1)
			copy(childPath, currentPath)
			childPath[len(currentPath)] = i
			children = append(children, childPath)
		}
		return children

	case 2: // Short node — extension or leaf
		var compactKey []byte
		if err := rlp.DecodeBytes(rawNode[0], &compactKey); err != nil || len(compactKey) == 0 {
			return nil
		}
		// High nibble of first byte encodes node type:
		//   0x0, 0x1 → extension (no terminator)
		//   0x2, 0x3 → leaf (terminator present, no children)
		if compactKey[0] >= 0x20 {
			return nil // leaf node
		}
		// Extension: derive child path by appending the decoded nibbles
		nibbles := compactKeyToNibbles(compactKey)
		if len(nibbles) == 0 {
			return nil
		}
		// A valid extension must reference a real child. Ignore malformed
		// encodings with an empty child reference.
		if len(rawNode[1]) <= 1 {
			return nil
		}
		childPath := make([]byte, len(currentPath)+len(nibbles))
		copy(childPath, currentPath)
		copy(childPath[len(currentPath):], nibbles)
		return [][]byte{childPath}
	}

	return nil
}

// compactKeyToNibbles converts a compact-encoded trie key to its nibble representation.
// It does not include the terminator byte. See Ethereum Yellow Paper appendix C.
func compactKeyToNibbles(compact []byte) []byte {
	if len(compact) == 0 {
		return nil
	}
	firstByte := compact[0]
	// Pre-allocate: each remaining byte contributes 2 nibbles; odd-length flag adds 1.
	n := len(compact[1:]) * 2
	if firstByte&0x10 != 0 {
		n++
	}
	nibbles := make([]byte, 0, n)
	// Bit 4 of the first byte is the odd-length flag: if set, the low nibble of
	// the first byte is the first nibble of the key.
	if firstByte&0x10 != 0 {
		nibbles = append(nibbles, firstByte&0x0f)
	}
	for _, b := range compact[1:] {
		nibbles = append(nibbles, b>>4, b&0x0f)
	}
	return nibbles
}

// routeCache determines which cache should be used for the given key.
// Returns the appropriate cache and true if it's an address-specific cache,
// or the common cache and false otherwise.
//
// Note: The key format used by nodeCacheKey is:
//   - For account trie: path only
//   - For storage trie: owner (32 bytes) + path
func (c *AddressBiasedCache) routeCache(key []byte) (*fastcache.Cache, bool) {
	if len(key) >= common.HashLength {
		accountHash := common.BytesToHash(key[:common.HashLength])
		if cache, ok := c.addressCaches.Load(accountHash); ok {
			return cache.(*fastcache.Cache), true
		}
	}

	return c.commonCache, false
}

// Get retrieves the value for the given key from the appropriate cache
func (c *AddressBiasedCache) Get(key []byte) []byte {
	cache, isAddressCache := c.routeCache(key)
	value := cache.Get(nil, key)

	if isAddressCache {
		if len(value) > 0 {
			biasedAddressCacheHitMeter.Mark(1)
			biasedAddressCacheReadMeter.Mark(int64(len(value)))
		} else {
			biasedAddressCacheMissMeter.Mark(1)
		}
	}

	return value
}

// Set stores the key-value pair in the appropriate cache
func (c *AddressBiasedCache) Set(key, value []byte) {
	cache, isAddressCache := c.routeCache(key)
	cache.Set(key, value)

	if isAddressCache {
		biasedAddressCacheWriteMeter.Mark(int64(len(value)))
	}
}

// Has checks if the key exists in the appropriate cache
func (c *AddressBiasedCache) Has(key []byte) bool {
	cache, _ := c.routeCache(key)
	return cache.Has(key)
}

// Del removes the key from the appropriate cache
func (c *AddressBiasedCache) Del(key []byte) {
	cache, _ := c.routeCache(key)
	cache.Del(key)
}

// Reset resets all caches
func (c *AddressBiasedCache) Reset() {
	c.commonCache.Reset()
	c.addressCaches.Range(func(key, value any) bool {
		cache := value.(*fastcache.Cache)
		cache.Reset()
		return true
	})
}

// Close cancels all background preload operations and waits for them to finish.
// This ensures graceful shutdown and prevents goroutines from blocking application termination.
func (c *AddressBiasedCache) Close() {
	if c.cancel != nil {
		c.cancel()  // Signal all goroutines to stop
		c.wg.Wait() // Wait for them to finish
	}
}
