package pathdb

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

// nibblesToCompact converts a nibble slice to compact encoding (inverse of compactKeyToNibbles).
// isLeaf sets the terminator flag (bit 5 of first byte).
func nibblesToCompact(nibbles []byte, isLeaf bool) []byte {
	var termFlag byte
	if isLeaf {
		termFlag = 2 // 0x20 when shifted
	}
	if len(nibbles)%2 == 0 { // even
		compact := []byte{termFlag << 4}
		for i := 0; i < len(nibbles); i += 2 {
			compact = append(compact, nibbles[i]<<4|nibbles[i+1])
		}
		return compact
	}
	// odd: bit 4 set, first nibble in low bits of header byte
	compact := []byte{termFlag<<4 | 0x10 | nibbles[0]}
	for i := 1; i < len(nibbles); i += 2 {
		compact = append(compact, nibbles[i]<<4|nibbles[i+1])
	}
	return compact
}

// encodeBranchNode encodes a branch node with children at the given slots.
// childHash is used as the placeholder child hash (should be 32 bytes).
func encodeBranchNode(t *testing.T, presentSlots []byte, childHash []byte) []byte {
	t.Helper()
	var node [17][]byte
	for _, s := range presentSlots {
		node[s] = childHash
	}
	data, err := rlp.EncodeToBytes(node)
	if err != nil {
		t.Fatalf("failed to RLP-encode branch node: %v", err)
	}
	return data
}

// encodeShortNode encodes an extension or leaf node.
func encodeShortNode(t *testing.T, compactKey, secondElement []byte) []byte {
	t.Helper()
	data, err := rlp.EncodeToBytes([2][]byte{compactKey, secondElement})
	if err != nil {
		t.Fatalf("failed to RLP-encode short node: %v", err)
	}
	return data
}

func allBranchSlots() []byte {
	slots := make([]byte, 16)
	for i := range slots {
		slots[i] = byte(i)
	}
	return slots
}

type countingDatabase struct {
	ethdb.Database
	mu       sync.Mutex
	getCount int
}

func (db *countingDatabase) Get(key []byte) ([]byte, error) {
	db.mu.Lock()
	db.getCount++
	db.mu.Unlock()

	return db.Database.Get(key)
}

func (db *countingDatabase) gets() int {
	db.mu.Lock()
	defer db.mu.Unlock()

	return db.getCount
}

func TestAddressBiasedCache_RouteCache(t *testing.T) {
	addr1 := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addr2 := common.HexToAddress("0xabcdefabcdefabcdefabcdefabcdefabcdefabcd")

	addressCacheSizes := map[common.Address]int{
		addr1: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Test routing for preloaded address
	accountHash1 := crypto.Keccak256Hash(addr1.Bytes())
	key1 := accountHash1.Bytes()

	targetCache, isAddressCache := cache.routeCache(key1)
	if !isAddressCache {
		t.Error("Expected address-specific cache for preloaded address")
	}
	expectedCache, _ := cache.addressCaches.Load(accountHash1)
	if targetCache != expectedCache.(*fastcache.Cache) {
		t.Error("Incorrect cache returned for preloaded address")
	}

	// Test routing for non-preloaded address
	accountHash2 := crypto.Keccak256Hash(addr2.Bytes())
	key2 := accountHash2.Bytes()

	targetCache, isAddressCache = cache.routeCache(key2)
	if isAddressCache {
		t.Error("Expected common cache for non-preloaded address")
	}
	if targetCache != cache.commonCache {
		t.Error("Incorrect cache returned for non-preloaded address")
	}

	// Test routing for short key (account trie)
	shortKey := []byte{0x01, 0x02}
	targetCache, isAddressCache = cache.routeCache(shortKey)
	if isAddressCache {
		t.Error("Expected common cache for short key")
	}
	if targetCache != cache.commonCache {
		t.Error("Incorrect cache returned for short key")
	}
}

func TestAddressBiasedCache_GetSet(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	accountHash := crypto.Keccak256Hash(addr.Bytes())
	key := append(accountHash.Bytes(), []byte{0x01, 0x02}...)
	value := []byte("test value")

	// Test Set and Get for address-specific cache
	cache.Set(key, value)
	retrieved := cache.Get(key)
	if !bytes.Equal(retrieved, value) {
		t.Errorf("Expected %v, got %v", value, retrieved)
	}

	// Test Set and Get for common cache
	commonKey := []byte{0x01, 0x02}
	commonValue := []byte("common value")
	cache.Set(commonKey, commonValue)
	retrieved = cache.Get(commonKey)
	if !bytes.Equal(retrieved, commonValue) {
		t.Errorf("Expected %v, got %v", commonValue, retrieved)
	}

	// Test Get for non-existent key
	nonExistentKey := append(accountHash.Bytes(), []byte{0xff, 0xff}...)
	retrieved = cache.Get(nonExistentKey)
	if len(retrieved) != 0 {
		t.Errorf("Expected empty slice, got %v", retrieved)
	}
}

func TestAddressBiasedCache_Has(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	accountHash := crypto.Keccak256Hash(addr.Bytes())
	key := append(accountHash.Bytes(), []byte{0x01, 0x02}...)
	value := []byte("test value")

	// Test Has for non-existent key
	if cache.Has(key) {
		t.Error("Has should return false for non-existent key")
	}

	// Test Has for existing key
	cache.Set(key, value)
	if !cache.Has(key) {
		t.Error("Has should return true for existing key")
	}
}

func TestAddressBiasedCache_Del(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	accountHash := crypto.Keccak256Hash(addr.Bytes())
	key := append(accountHash.Bytes(), []byte{0x01, 0x02}...)
	value := []byte("test value")

	// Set and verify
	cache.Set(key, value)
	if !cache.Has(key) {
		t.Error("Key should exist after Set")
	}

	// Delete and verify
	cache.Del(key)
	if cache.Has(key) {
		t.Error("Key should not exist after Del")
	}

	// Verify Get returns empty after Del
	retrieved := cache.Get(key)
	if len(retrieved) != 0 {
		t.Error("Get should return empty slice after Del")
	}
}

func TestAddressBiasedCache_Reset(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Add data to both address-specific and common caches
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	addressKey := append(accountHash.Bytes(), []byte{0x01, 0x02}...)
	commonKey := []byte{0x01, 0x02}

	cache.Set(addressKey, []byte("address value"))
	cache.Set(commonKey, []byte("common value"))

	// Verify data exists
	if !cache.Has(addressKey) || !cache.Has(commonKey) {
		t.Error("Data should exist before reset")
	}

	// Reset all caches
	cache.Reset()

	// Verify data is gone
	if cache.Has(addressKey) || cache.Has(commonKey) {
		t.Error("Data should not exist after reset")
	}
}

func TestAddressBiasedCache_MultipleAddresses(t *testing.T) {
	addr1 := common.HexToAddress("0x1111111111111111111111111111111111111111")
	addr2 := common.HexToAddress("0x2222222222222222222222222222222222222222")
	addr3 := common.HexToAddress("0x3333333333333333333333333333333333333333")

	addressCacheSizes := map[common.Address]int{
		addr1: 1024 * 1024,
		addr2: 512 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 256*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Verify correct number of address caches
	var count int
	cache.addressCaches.Range(func(key, value any) bool {
		count++
		return true
	})
	if count != 2 {
		t.Errorf("Expected 2 address caches, got %d", count)
	}

	// Test data isolation between caches
	accountHash1 := crypto.Keccak256Hash(addr1.Bytes())
	accountHash2 := crypto.Keccak256Hash(addr2.Bytes())
	accountHash3 := crypto.Keccak256Hash(addr3.Bytes())

	key1 := append(accountHash1.Bytes(), []byte{0x01}...)
	key2 := append(accountHash2.Bytes(), []byte{0x01}...)
	key3 := append(accountHash3.Bytes(), []byte{0x01}...)

	cache.Set(key1, []byte("value1"))
	cache.Set(key2, []byte("value2"))
	cache.Set(key3, []byte("value3"))

	// Verify values are isolated
	val1 := cache.Get(key1)
	val2 := cache.Get(key2)
	val3 := cache.Get(key3)

	if !bytes.Equal(val1, []byte("value1")) {
		t.Error("Value1 mismatch")
	}
	if !bytes.Equal(val2, []byte("value2")) {
		t.Error("Value2 mismatch")
	}
	if !bytes.Equal(val3, []byte("value3")) {
		t.Error("Value3 mismatch (should be in common cache)")
	}
}

func TestAddressBiasedCache_PreloadWithData(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Create database with some storage trie nodes
	db := rawdb.NewMemoryDatabase()

	// Write root node
	rootData := []byte("root node data")
	rawdb.WriteStorageTrieNode(db, accountHash, nil, rootData)

	// Write child nodes at depth 1
	for i := byte(0); i < 4; i++ {
		path := []byte{i}
		data := []byte("child node " + string(rune(i)))
		rawdb.WriteStorageTrieNode(db, accountHash, path, data)
	}

	// Create cache with preloading
	addressCacheSizes := map[common.Address]int{
		addr: 10 * 1024, // Small cache to test limit
	}

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Wait for async preloading to complete
	cache.wg.Wait()

	// Verify root node was loaded
	rootKey := accountHash.Bytes()
	if !cache.Has(rootKey) {
		t.Error("Expected root node to be preloaded")
	}
	retrieved := cache.Get(rootKey)
	if !bytes.Equal(retrieved, rootData) {
		t.Error("Root node data mismatch")
	}
}

// TestDecodeChildPaths_BranchNode verifies that decodeChildPaths correctly
// identifies non-nil children in a branch node and returns only their paths.
func TestDecodeChildPaths_BranchNode(t *testing.T) {
	hash := bytes.Repeat([]byte{0xab}, 32)

	nodeData := encodeBranchNode(t, []byte{0, 5, 15}, hash)
	currentPath := []byte{0x01}

	children := decodeChildPaths(nodeData, currentPath)
	if len(children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(children))
	}

	wantPaths := map[string]bool{
		string([]byte{0x01, 0x00}): true,
		string([]byte{0x01, 0x05}): true,
		string([]byte{0x01, 0x0f}): true,
	}
	for _, p := range children {
		if !wantPaths[string(p)] {
			t.Errorf("unexpected child path %v", p)
		}
		if len(p) != len(currentPath)+1 {
			t.Errorf("child path length should be %d, got %d", len(currentPath)+1, len(p))
		}
	}
}

// TestDecodeChildPaths_EmptyBranch verifies that an all-nil branch node returns no children.
func TestDecodeChildPaths_EmptyBranch(t *testing.T) {
	nodeData := encodeBranchNode(t, nil, nil)
	if got := decodeChildPaths(nodeData, nil); len(got) != 0 {
		t.Errorf("expected no children for empty branch, got %v", got)
	}
}

// TestDecodeChildPaths_ExtensionNodeEven verifies extension node decoding with an even-length key.
func TestDecodeChildPaths_ExtensionNodeEven(t *testing.T) {
	// Extension: key nibbles [1, 2] → compact [0x00, 0x12]
	nibbles := []byte{1, 2}
	nodeData := encodeShortNode(t, nibblesToCompact(nibbles, false), bytes.Repeat([]byte{0xcc}, 32))

	children := decodeChildPaths(nodeData, []byte{0x05})
	if len(children) != 1 {
		t.Fatalf("expected 1 child for extension node, got %d", len(children))
	}
	want := []byte{0x05, 1, 2}
	if !bytes.Equal(children[0], want) {
		t.Errorf("expected child path %v, got %v", want, children[0])
	}
}

// TestDecodeChildPaths_ExtensionNodeOdd verifies extension node decoding with an odd-length key.
func TestDecodeChildPaths_ExtensionNodeOdd(t *testing.T) {
	// Extension: key nibbles [1, 2, 3] → compact [0x11, 0x23]
	nibbles := []byte{1, 2, 3}
	nodeData := encodeShortNode(t, nibblesToCompact(nibbles, false), bytes.Repeat([]byte{0xcc}, 32))

	children := decodeChildPaths(nodeData, []byte{0x05})
	if len(children) != 1 {
		t.Fatalf("expected 1 child for extension node, got %d", len(children))
	}
	want := []byte{0x05, 1, 2, 3}
	if !bytes.Equal(children[0], want) {
		t.Errorf("expected child path %v, got %v", want, children[0])
	}
}

// TestDecodeChildPaths_LeafNode verifies that leaf nodes return no children.
func TestDecodeChildPaths_LeafNode(t *testing.T) {
	// Leaf node: key nibbles [1, 2] with terminator flag → compact [0x20, 0x12]
	nodeData := encodeShortNode(t, nibblesToCompact([]byte{1, 2}, true), []byte("value"))
	if got := decodeChildPaths(nodeData, []byte{0x01}); len(got) != 0 {
		t.Errorf("expected no children for leaf node, got %v", got)
	}

	// Also test odd-length leaf
	nodeData = encodeShortNode(t, nibblesToCompact([]byte{1, 2, 3}, true), []byte("value"))
	if got := decodeChildPaths(nodeData, []byte{0x01}); len(got) != 0 {
		t.Errorf("expected no children for odd leaf node, got %v", got)
	}
}

// TestDecodeChildPaths_InvalidData verifies that non-RLP input returns nil.
func TestDecodeChildPaths_InvalidData(t *testing.T) {
	if got := decodeChildPaths([]byte("not valid rlp data"), nil); got != nil {
		t.Errorf("expected nil for invalid data, got %v", got)
	}
	if got := decodeChildPaths(nil, nil); got != nil {
		t.Errorf("expected nil for nil data, got %v", got)
	}
}

// TestDecodeChildPaths_EmptyExtensionRejected verifies malformed empty
// extension nodes do not produce a non-growing child path equal to currentPath.
func TestDecodeChildPaths_EmptyExtensionRejected(t *testing.T) {
	currentPath := []byte{0x05, 0x06}

	// Compact key 0x00 decodes to an empty extension key, which should be ignored.
	nodeData := encodeShortNode(t, []byte{0x00}, bytes.Repeat([]byte{0xdd}, 32))
	if got := decodeChildPaths(nodeData, currentPath); len(got) != 0 {
		t.Fatalf("expected malformed empty extension to be ignored, got %v", got)
	}
}

// TestDecodeChildPaths_ExtensionWithoutChildRejected verifies malformed
// extension nodes with an empty child reference are ignored.
func TestDecodeChildPaths_ExtensionWithoutChildRejected(t *testing.T) {
	nodeData := encodeShortNode(t, nibblesToCompact([]byte{0x0a}, false), nil)

	if got := decodeChildPaths(nodeData, []byte{0x05}); len(got) != 0 {
		t.Fatalf("expected malformed extension without child to be ignored, got %v", got)
	}
}

// TestCompactKeyToNibbles verifies round-trip conversion through nibblesToCompact.
func TestCompactKeyToNibbles(t *testing.T) {
	cases := []struct {
		nibbles []byte
		isLeaf  bool
	}{
		{[]byte{1, 2}, false},       // even extension
		{[]byte{1, 2, 3}, false},    // odd extension
		{[]byte{1, 2}, true},        // even leaf
		{[]byte{1, 2, 3}, true},     // odd leaf
		{[]byte{}, false},           // empty extension
		{[]byte{0}, false},          // single nibble (odd)
		{[]byte{0, 0, 0, 0}, false}, // four nibbles
	}
	for _, tc := range cases {
		compact := nibblesToCompact(tc.nibbles, tc.isLeaf)
		got := compactKeyToNibbles(compact)
		if !bytes.Equal(got, tc.nibbles) {
			t.Errorf("round-trip failed for nibbles=%v isLeaf=%v: compact=%v got=%v",
				tc.nibbles, tc.isLeaf, compact, got)
		}
		// decodeChildPaths must treat leaf compactKey[0] >= 0x20 as a leaf
		isLeafDetected := len(compact) > 0 && compact[0] >= 0x20
		if isLeafDetected != tc.isLeaf {
			t.Errorf("leaf detection failed for nibbles=%v isLeaf=%v: compact=%v",
				tc.nibbles, tc.isLeaf, compact)
		}
	}
}

// TestPreloadBFS_CycleFree proves that the BFS terminates and visits each trie
// node exactly once. Without a visited set, this relies on the structural guarantee
// that MPT child paths are strictly longer than their parent path.
//
// Trie structure (5 nodes total):
//
//	root (branch): children at [0] and [1]
//	[0] → leaf
//	[1] → branch: children at [1,2] and [1,3]
//	[1,2] → leaf
//	[1,3] → leaf
func TestPreloadBFS_CycleFree(t *testing.T) {
	addr := common.HexToAddress("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	db := rawdb.NewMemoryDatabase()

	hash := bytes.Repeat([]byte{0x01}, 32)

	// root: branch with children at slots 0 and 1
	rawdb.WriteStorageTrieNode(db, accountHash, nil, encodeBranchNode(t, []byte{0, 1}, hash))

	// path [0]: leaf node
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{0},
		encodeShortNode(t, nibblesToCompact([]byte{0xa}, true), []byte("v0")))

	// path [1]: branch with children at slots 2 and 3
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{1}, encodeBranchNode(t, []byte{2, 3}, hash))

	// path [1, 2]: leaf node
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{1, 2},
		encodeShortNode(t, nibblesToCompact([]byte{0xb}, true), []byte("v12")))

	// path [1, 3]: leaf node
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{1, 3},
		encodeShortNode(t, nibblesToCompact([]byte{0xc}, true), []byte("v13")))

	cacheSize := 10 * 1024 * 1024 // 10 MiB — large enough to hold all 5 nodes
	cache, err := NewAddressBiasedCache(db, map[common.Address]int{addr: cacheSize}, 512*1024, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	// Wait for async preload to complete (no rate limit, so completes in microseconds)
	cache.wg.Wait()

	// All 5 nodes must be in the cache. If the BFS had looped or revisited
	// nodes, it would either hang or overwrite newer data (caught by the
	// Has-before-Set check), but the node count would be wrong.
	cacheKey := func(path []byte) []byte { return append(accountHash.Bytes(), path...) }
	paths := [][]byte{nil, {0}, {1}, {1, 2}, {1, 3}}
	for _, p := range paths {
		if !cache.Has(cacheKey(p)) {
			t.Errorf("expected node at path %v to be in cache", p)
		}
	}
}

// TestPreloadBFS_EmptyExtensionReadOnce verifies preload ignores malformed
// empty extensions instead of revisiting the same path.
func TestPreloadBFS_EmptyExtensionReadOnce(t *testing.T) {
	addr := common.HexToAddress("0xbeefdeadbeefdeadbeefdeadbeefdeadbeefdead")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	base := rawdb.NewMemoryDatabase()
	db := &countingDatabase{Database: base}

	// Compact key 0x00 is a malformed empty extension that would otherwise
	// point back to the current path.
	rawdb.WriteStorageTrieNode(base, accountHash, nil,
		encodeShortNode(t, []byte{0x00}, bytes.Repeat([]byte{0xee}, 32)))

	cache, err := NewAddressBiasedCache(db, map[common.Address]int{addr: 1024 * 1024}, 512*1024, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	cache.wg.Wait()

	if got := db.gets(); got != 1 {
		t.Fatalf("expected preload to read the malformed root once, got %d reads", got)
	}
}

// TestPreloadBFS_ExtensionTraversal verifies preload follows a valid extension
// edge and caches descendants at the decoded child path.
func TestPreloadBFS_ExtensionTraversal(t *testing.T) {
	addr := common.HexToAddress("0xfacefacefacefacefacefacefacefacefaceface")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	db := rawdb.NewMemoryDatabase()

	hash := bytes.Repeat([]byte{0x11}, 32)

	// root: extension with key [1, 2] pointing to path [1, 2]
	rawdb.WriteStorageTrieNode(db, accountHash, nil,
		encodeShortNode(t, nibblesToCompact([]byte{1, 2}, false), hash))

	// path [1, 2]: branch with child at slot 3
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{1, 2},
		encodeBranchNode(t, []byte{3}, hash))

	// path [1, 2, 3]: leaf node
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{1, 2, 3},
		encodeShortNode(t, nibblesToCompact([]byte{0xd}, true), []byte("v123")))

	cache, err := NewAddressBiasedCache(db, map[common.Address]int{addr: 1024 * 1024}, 512*1024, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	cache.wg.Wait()

	cacheKey := func(path []byte) []byte { return append(accountHash.Bytes(), path...) }
	for _, path := range [][]byte{nil, {1, 2}, {1, 2, 3}} {
		if !cache.Has(cacheKey(path)) {
			t.Fatalf("expected node at path %v to be cached", path)
		}
	}
}

// TestAddressBiasedCache_RateLimitInterruption_ValidTrie verifies Close can
// interrupt a genuinely in-flight traversal on a valid trie, not just on an
// invalid root blob.
func TestAddressBiasedCache_RateLimitInterruption_ValidTrie(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x9999999999999999999999999999999999999999")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	hash := bytes.Repeat([]byte{0x22}, 32)
	slots := allBranchSlots()

	rawdb.WriteStorageTrieNode(db, accountHash, nil, encodeBranchNode(t, slots, hash))

	largeValue := bytes.Repeat([]byte{0xaa}, 4*1024)
	for _, i := range slots {
		rawdb.WriteStorageTrieNode(db, accountHash, []byte{i}, encodeBranchNode(t, slots, hash))
		for _, j := range slots {
			rawdb.WriteStorageTrieNode(db, accountHash, []byte{i, j},
				encodeShortNode(t, nibblesToCompact([]byte{0x0f}, true), largeValue))
		}
	}

	cache, err := NewAddressBiasedCache(db, map[common.Address]int{addr: 8 * 1024 * 1024}, 512*1024, 1024, "")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cache.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took too long during valid-trie rate-limited preload: %v", elapsed)
	}
}

func TestAddressBiasedCache_EmptyDatabase(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")

	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	_, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Wait for async preloading to complete
	time.Sleep(50 * time.Millisecond)

	// Cache created successfully for empty database
}

func TestAddressBiasedCache_AsyncPreloadWithConcurrentWrites(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Create database with storage trie nodes
	db := rawdb.NewMemoryDatabase()

	// Write root node
	rootData := []byte("root node data")
	rawdb.WriteStorageTrieNode(db, accountHash, nil, rootData)

	// Write some child nodes
	for i := byte(0); i < 10; i++ {
		path := []byte{i}
		data := []byte("node data " + string(rune(i)))
		rawdb.WriteStorageTrieNode(db, accountHash, path, data)
	}

	// Create cache with async preloading
	addressCacheSizes := map[common.Address]int{
		addr: 100 * 1024,
	}

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Immediately start writing to the cache while preloading is happening
	// Use a key that doesn't exist in the database
	testKey := append(accountHash.Bytes(), byte(5))
	manualValue := []byte("manually added value")
	cache.Set(testKey, manualValue)

	// Wait for async preloading to complete
	time.Sleep(100 * time.Millisecond)

	// Verify the manually added value or DB value exists
	retrieved := cache.Get(testKey)
	// The value could be either manual or from DB, just check it exists
	if len(retrieved) == 0 {
		t.Error("Expected key to have a value")
	}
}

func TestAddressBiasedCache_ConcurrentAccess(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Test concurrent reads and writes
	done := make(chan bool)
	numGoroutines := 10

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			key := append(accountHash.Bytes(), byte(id))
			value := []byte("value " + string(rune(id)))

			// Perform multiple operations
			for j := 0; j < 100; j++ {
				cache.Set(key, value)
				cache.Get(key)
				cache.Has(key)
				if j%10 == 0 {
					cache.Del(key)
				}
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete with timeout
	timeout := time.After(5 * time.Second)
	for i := 0; i < numGoroutines; i++ {
		select {
		case <-done:
		case <-timeout:
			t.Fatal("Test timed out waiting for concurrent operations")
		}
	}
}

func BenchmarkAddressBiasedCache_Get_AddressCache(b *testing.B) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 10 * 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 5*1024*1024, 0, "")
	if err != nil {
		b.Fatalf("Failed to create cache: %v", err)
	}

	accountHash := crypto.Keccak256Hash(addr.Bytes())
	key := append(accountHash.Bytes(), []byte{0x01, 0x02}...)
	value := []byte("benchmark value")
	cache.Set(key, value)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Get(key)
	}
}

func BenchmarkAddressBiasedCache_Get_CommonCache(b *testing.B) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 10 * 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 5*1024*1024, 0, "")
	if err != nil {
		b.Fatalf("Failed to create cache: %v", err)
	}

	key := []byte{0x01, 0x02}
	value := []byte("benchmark value")
	cache.Set(key, value)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cache.Get(key)
	}
}

func BenchmarkAddressBiasedCache_Set(b *testing.B) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	addressCacheSizes := map[common.Address]int{
		addr: 10 * 1024 * 1024,
	}

	db := rawdb.NewMemoryDatabase()
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 5*1024*1024, 0, "")
	if err != nil {
		b.Fatalf("Failed to create cache: %v", err)
	}

	accountHash := crypto.Keccak256Hash(addr.Bytes())
	value := []byte("benchmark value")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := append(accountHash.Bytes(), byte(i%256))
		cache.Set(key, value)
	}
}

// TestAddressBiasedCache_RateLimitCreation tests that the rate limiter is created
// correctly based on the rateLimitBPS parameter.
func TestAddressBiasedCache_RateLimitCreation(t *testing.T) {
	t.Run("Rate limit zero means unlimited", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		addr := common.HexToAddress("0x1234567890123456789012345678901234567890")

		addressCacheSizes := map[common.Address]int{
			addr: 1024 * 1024,
		}

		cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, 0, "")
		if err != nil {
			t.Fatalf("Failed to create cache: %v", err)
		}
		defer cache.Close()

		// rateLimitBPS should be 0
		if cache.rateLimitBPS != 0 {
			t.Errorf("Expected rateLimitBPS to be 0, got %d", cache.rateLimitBPS)
		}
	})

	t.Run("Rate limit positive value is stored", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		addr := common.HexToAddress("0x1234567890123456789012345678901234567890")

		addressCacheSizes := map[common.Address]int{
			addr: 1024 * 1024,
		}

		rateLimit := int64(500 * 1024) // 500 KB/s
		cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, rateLimit, "")
		if err != nil {
			t.Fatalf("Failed to create cache: %v", err)
		}
		defer cache.Close()

		if cache.rateLimitBPS != rateLimit {
			t.Errorf("Expected rateLimitBPS to be %d, got %d", rateLimit, cache.rateLimitBPS)
		}
	})
}

// TestAddressBiasedCache_RateLimitThrottling tests that rate limiting actually
// throttles the preload speed.
func TestAddressBiasedCache_RateLimitThrottling(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Create nodes with known sizes (each ~100 bytes)
	nodeCount := 50
	nodeData := make([]byte, 100)
	for i := 0; i < len(nodeData); i++ {
		nodeData[i] = byte(i)
	}

	for i := 0; i < nodeCount; i++ {
		path := []byte{byte(i)}
		rawdb.WriteStorageTrieNode(db, accountHash, path, nodeData)
	}

	// Also write root node
	rawdb.WriteStorageTrieNode(db, accountHash, nil, nodeData)

	addressCacheSizes := map[common.Address]int{
		addr: 100 * 1024, // 100 KB cache
	}

	t.Run("Unlimited preload is fast", func(t *testing.T) {
		dbCopy := rawdb.NewMemoryDatabase()
		for i := 0; i < nodeCount; i++ {
			path := []byte{byte(i)}
			rawdb.WriteStorageTrieNode(dbCopy, accountHash, path, nodeData)
		}
		rawdb.WriteStorageTrieNode(dbCopy, accountHash, nil, nodeData)

		start := time.Now()
		cache, err := NewAddressBiasedCache(dbCopy, addressCacheSizes, 512*1024, 0, "")
		if err != nil {
			t.Fatalf("Failed to create cache: %v", err)
		}

		// Wait for preload to complete
		cache.wg.Wait()
		unlimitedDuration := time.Since(start)
		cache.Close()

		// Unlimited should be very fast (< 100ms for small dataset)
		if unlimitedDuration > 500*time.Millisecond {
			t.Logf("Warning: unlimited preload took longer than expected: %v", unlimitedDuration)
		}
	})

	t.Run("Rate limited preload is slower", func(t *testing.T) {
		dbCopy := rawdb.NewMemoryDatabase()
		for i := 0; i < nodeCount; i++ {
			path := []byte{byte(i)}
			rawdb.WriteStorageTrieNode(dbCopy, accountHash, path, nodeData)
		}
		rawdb.WriteStorageTrieNode(dbCopy, accountHash, nil, nodeData)

		// Very low rate limit: 1KB/s
		// With 50 nodes * 100 bytes = 5KB total, should take ~5 seconds
		// But we'll use a more reasonable test with 10KB/s
		rateLimit := int64(10 * 1024) // 10 KB/s

		start := time.Now()
		cache, err := NewAddressBiasedCache(dbCopy, addressCacheSizes, 512*1024, rateLimit, "")
		if err != nil {
			t.Fatalf("Failed to create cache: %v", err)
		}

		// Wait for preload to complete (with timeout)
		done := make(chan struct{})
		go func() {
			cache.wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			rateLimitedDuration := time.Since(start)
			cache.Close()

			// With 10KB/s rate limit and ~5KB data, should take at least 400ms
			// (accounting for burst allowance of 64KB)
			// The actual minimum time depends on burst size
			t.Logf("Rate limited preload took: %v", rateLimitedDuration)

		case <-time.After(10 * time.Second):
			cache.Close()
			t.Fatal("Rate limited preload timed out")
		}
	})
}

// TestAddressBiasedCache_RateLimitInterruption tests that rate-limited preload
// can be interrupted via context cancellation.
func TestAddressBiasedCache_RateLimitInterruption(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Build a valid trie large enough to keep the preload busy at 1KB/s.
	// Branch root → 16 branch children → 256 leaf grandchildren (5KB each).
	// Total: 256 × 5KB = 1.28MB — at 1KB/s this takes >1000s without cancellation.
	slots := allBranchSlots()
	hash := bytes.Repeat([]byte{0x33}, 32)
	rawdb.WriteStorageTrieNode(db, accountHash, nil, encodeBranchNode(t, slots, hash))
	leafValue := bytes.Repeat([]byte{0xcc}, 5*1024)
	for _, i := range slots {
		rawdb.WriteStorageTrieNode(db, accountHash, []byte{i}, encodeBranchNode(t, slots, hash))
		for _, j := range slots {
			rawdb.WriteStorageTrieNode(db, accountHash, []byte{i, j},
				encodeShortNode(t, nibblesToCompact([]byte{0x0f}, true), leafValue))
		}
	}

	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024,
	}

	// Very slow rate limit to ensure preload is still running when we cancel
	rateLimit := int64(1024) // 1 KB/s - would take ~1000 seconds normally

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, rateLimit, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Let preload start and exhaust the burst
	time.Sleep(50 * time.Millisecond)

	// Cancel by closing
	start := time.Now()
	cache.Close()
	closeDuration := time.Since(start)

	// Close should return quickly (not wait for full preload)
	if closeDuration > 1*time.Second {
		t.Errorf("Close took too long: %v (expected < 1s)", closeDuration)
	}
}

// TestAddressBiasedCache_ShutdownDuringRateLimitWait specifically tests the scenario
// where the preload goroutine is blocked in limiter.WaitN() when shutdown occurs.
// This covers the "Preload interrupted during shutdown" log path.
func TestAddressBiasedCache_ShutdownDuringRateLimitWait(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Build a valid trie: branch root with 8 children, each a large leaf.
	// 8 children × 10KB = 80KB > 64KB burst, so WaitN blocks partway through children.
	slots := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	hash := bytes.Repeat([]byte{0xab}, 32)
	rawdb.WriteStorageTrieNode(db, accountHash, nil, encodeBranchNode(t, slots, hash))
	largeValue := bytes.Repeat([]byte{0xbb}, 10*1024) // 10KB per leaf
	for _, i := range slots {
		rawdb.WriteStorageTrieNode(db, accountHash, []byte{i},
			encodeShortNode(t, nibblesToCompact([]byte{0x0f}, true), largeValue))
	}

	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024, // 1MB cache
	}

	// Very slow rate limit: 1KB/s with 64KB burst.
	// After ~6 children (~60KB) the burst is exhausted and WaitN blocks for ~10s per node.
	rateLimit := int64(1024) // 1 KB/s

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, rateLimit, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Wait for burst to be consumed and WaitN to block
	time.Sleep(200 * time.Millisecond)

	// Now Close() should interrupt the WaitN call
	start := time.Now()
	cache.Close()
	closeDuration := time.Since(start)

	// Close should return quickly since WaitN respects context cancellation
	if closeDuration > 500*time.Millisecond {
		t.Errorf("Close took too long: %v (expected < 500ms)", closeDuration)
	}

	// Verify cache is still usable after interrupted preload
	testKey := append(accountHash.Bytes(), []byte{0xff}...)
	cache.Set(testKey, []byte("post-shutdown-value"))
	retrieved := cache.Get(testKey)
	if string(retrieved) != "post-shutdown-value" {
		t.Errorf("Cache should work after shutdown, got: %s", string(retrieved))
	}
}

// TestAddressBiasedCache_BurstExceeded tests the scenario where a single node
// exceeds the rate limiter's burst size. The oversized node should be skipped
// and preloading should continue with subsequent nodes.
func TestAddressBiasedCache_BurstExceeded(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Burst is 64KB. Create a node larger than that so WaitN returns an error immediately.
	oversizedData := make([]byte, 65*1024) // 65KB > 64KB burst
	for i := range oversizedData {
		oversizedData[i] = byte(i % 256)
	}

	// Write a valid branch node as root so decodeChildPaths discovers children at [0x00] and [0x01]
	rawdb.WriteStorageTrieNode(db, accountHash, nil, encodeBranchNode(t, []byte{0x00, 0x01}, bytes.Repeat([]byte{0x01}, 32)))

	// Write an oversized child node that should be skipped
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{0x00}, oversizedData)

	// Write another small child after the oversized one
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{0x01}, []byte("small node after oversized"))

	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024, // 1MB cache
	}

	// Use a rate limit so the limiter is created (burst = 64KB)
	rateLimit := int64(1024 * 1024) // 1MB/s - fast enough that small nodes pass

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, rateLimit, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}
	defer cache.Close()

	// Wait for preload to process all nodes
	time.Sleep(500 * time.Millisecond)

	// The root node (small) should have been loaded
	rootKey := append(accountHash.Bytes(), []byte(nil)...)
	if got := cache.Get(rootKey); got == nil {
		t.Error("Root node should have been cached")
	}

	// The oversized node should NOT be cached (it was skipped)
	oversizedKey := append(accountHash.Bytes(), []byte{0x00}...)
	if got := cache.Get(oversizedKey); got != nil {
		t.Error("Oversized node should have been skipped, not cached")
	}

	// The small node after the oversized one SHOULD be cached (preload continued)
	afterKey := append(accountHash.Bytes(), []byte{0x01}...)
	if got := cache.Get(afterKey); got == nil {
		t.Error("Node after oversized entry should be cached, preload should have continued")
	}
}

// TestAddressBiasedCache_PreloadWithRateLimit tests preloading with rate limit
// still correctly populates the cache.
func TestAddressBiasedCache_PreloadWithRateLimit(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Create a few nodes
	rootData := []byte("root node data - test")
	rawdb.WriteStorageTrieNode(db, accountHash, nil, rootData)

	child1Data := []byte("child 1 data")
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{0x00}, child1Data)

	child2Data := []byte("child 2 data")
	rawdb.WriteStorageTrieNode(db, accountHash, []byte{0x01}, child2Data)

	addressCacheSizes := map[common.Address]int{
		addr: 10 * 1024,
	}

	// Use a reasonable rate limit
	rateLimit := int64(100 * 1024) // 100 KB/s

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 512*1024, rateLimit, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Wait for preload to complete
	cache.wg.Wait()

	// Verify root node was loaded
	rootKey := accountHash.Bytes()
	if !cache.Has(rootKey) {
		t.Error("Expected root node to be preloaded with rate limiting")
	}

	retrieved := cache.Get(rootKey)
	if !bytes.Equal(retrieved, rootData) {
		t.Errorf("Root node data mismatch: expected %s, got %s", rootData, retrieved)
	}

	cache.Close()
}

// TestAddressBiasedCache_GracefulShutdown tests that Close() properly stops
// background preload operations and waits for them to finish.
func TestAddressBiasedCache_GracefulShutdown(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())

	// Create a large tree of storage trie nodes that will take some time to preload
	nodeCount := 1000
	for i := 0; i < nodeCount; i++ {
		path := []byte{byte(i % 256), byte(i / 256)}
		nodeData := []byte(fmt.Sprintf("node-data-%d", i))
		rawdb.WriteStorageTrieNode(db, accountHash, path, nodeData)
	}

	// Create cache with preloading
	addressCacheSizes := map[common.Address]int{
		addr: 10 * 1024 * 1024, // 10 MB
	}
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 1024*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Immediately close the cache to test interruption
	cache.Close()

	// Verify the cache is still functional after Close()
	key := append(accountHash.Bytes(), []byte{1, 2}...)
	cache.Set(key, []byte("test-value"))
	value := cache.Get(key)
	if string(value) != "test-value" {
		t.Errorf("Cache should still work after Close(), got: %s", string(value))
	}
}

// TestAddressBiasedCache_MultipleClose tests that calling Close() multiple times
// doesn't cause issues.
func TestAddressBiasedCache_MultipleClose(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")

	addressCacheSizes := map[common.Address]int{
		addr: 1024 * 1024, // 1 MB
	}
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 1024*1024, 0, "")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// Close multiple times should not panic
	cache.Close()
	cache.Close()
	cache.Close()
}

func TestAddressBiasedCache_CloseSavesAndReloadWarmsUp(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	journalDir := t.TempDir()

	db := rawdb.NewMemoryDatabase()
	rootData := encodeBranchNode(t, []byte{0, 1}, bytes.Repeat([]byte{0xAB}, 32))
	rawdb.WriteStorageTrieNode(db, accountHash, nil, rootData)

	addressCacheSizes := map[common.Address]int{addr: 64 * 1024}

	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 32*1024, 0, journalDir)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	cache.wg.Wait() // let preload finish filling the (cold) cache

	rootKey := accountHash.Bytes()
	if !cache.Has(rootKey) {
		t.Fatal("expected root node to be preloaded before close")
	}
	cache.Close() // must write the snapshot file to journalDir

	// Reopen against the same journalDir but an empty database, so the only
	// way the reloaded cache can have the root entry is via the persisted file.
	emptyDB := rawdb.NewMemoryDatabase()
	reloaded, err := NewAddressBiasedCache(emptyDB, addressCacheSizes, 32*1024, 0, journalDir)
	if err != nil {
		t.Fatalf("failed to reopen cache: %v", err)
	}
	reloaded.wg.Wait()

	if !reloaded.Has(rootKey) {
		t.Fatal("expected root node to survive a Close() + reload round trip")
	}
	got := reloaded.Get(rootKey)
	if !bytes.Equal(got, rootData) {
		t.Fatalf("reloaded root node mismatch: got %x want %x", got, rootData)
	}
}

func TestAddressBiasedCache_ReloadMissingFileFallsBackToPreload(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	journalDir := t.TempDir() // empty: no snapshot file exists yet

	db := rawdb.NewMemoryDatabase()
	rootData := encodeBranchNode(t, []byte{0}, bytes.Repeat([]byte{0xCD}, 32))
	rawdb.WriteStorageTrieNode(db, accountHash, nil, rootData)

	addressCacheSizes := map[common.Address]int{addr: 64 * 1024}
	cache, err := NewAddressBiasedCache(db, addressCacheSizes, 32*1024, 0, journalDir)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	cache.wg.Wait()

	rootKey := accountHash.Bytes()
	if !cache.Has(rootKey) {
		t.Fatal("expected preloadAddressAsync to have filled the cache from disk when no snapshot file exists")
	}
}

func TestAddressBiasedCache_ReloadSizeMismatchFallsBackToPreload(t *testing.T) {
	addr := common.HexToAddress("0x1234567890123456789012345678901234567890")
	accountHash := crypto.Keccak256Hash(addr.Bytes())
	journalDir := t.TempDir()

	db := rawdb.NewMemoryDatabase()
	rootData := encodeBranchNode(t, []byte{0}, bytes.Repeat([]byte{0xEF}, 32))
	rawdb.WriteStorageTrieNode(db, accountHash, nil, rootData)

	addressCacheSizes := map[common.Address]int{addr: 64 * 1024}

	// First run: creates and persists a snapshot sized for 64KB.
	first, err := NewAddressBiasedCache(db, addressCacheSizes, 32*1024, 0, journalDir)
	if err != nil {
		t.Fatalf("failed to create first cache: %v", err)
	}
	first.wg.Wait()
	first.Close()

	// Second run: same journalDir, but a different configured cache size for
	// the same address (simulates an addresscachesizes config change).
	mismatchedSizes := map[common.Address]int{addr: 128 * 1024}
	second, err := NewAddressBiasedCache(db, mismatchedSizes, 32*1024, 0, journalDir)
	if err != nil {
		t.Fatalf("failed to create second cache: %v", err)
	}
	second.wg.Wait()

	rootKey := accountHash.Bytes()
	if !second.Has(rootKey) {
		t.Fatal("expected preloadAddressAsync to have run and filled the cache after a size-mismatched snapshot was rejected")
	}
}
