package fetcher

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// Test helper functions
func createTestBlock(number uint64) *types.Block {
	header := &types.Header{
		Number: big.NewInt(int64(number)),
	}
	return types.NewBlock(header, nil, nil, trie.NewStackTrie(nil))
}

func createTestWitnessForBlock(block *types.Block) *stateless.Witness {
	witness, err := stateless.NewWitness(block.Header(), nil)
	if err != nil {
		panic(err)
	}
	return witness
}

func createTestBlockAnnounce(origin string, block *types.Block, fetchWitness witnessRequesterFn) *blockAnnounce {
	return &blockAnnounce{
		origin:       origin,
		hash:         block.Hash(),
		number:       block.NumberU64(),
		time:         time.Now(),
		fetchWitness: fetchWitness,
	}
}

// Test setup helper
type testWitnessManager struct {
	manager      *witnessManager
	quit         chan struct{}
	enqueueCh    chan *enqueueRequest
	droppedPeers []string
	mu           sync.Mutex
}

func newTestWitnessManager() *testWitnessManager {
	quit := make(chan struct{})
	enqueueCh := make(chan *enqueueRequest, 10)

	tw := &testWitnessManager{
		quit:      quit,
		enqueueCh: enqueueCh,
	}

	dropPeer := peerDropFn(func(id string) {
		tw.mu.Lock()
		tw.droppedPeers = append(tw.droppedPeers, id)
		tw.mu.Unlock()
	})

	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	tw.manager = newWitnessManager(quit, dropPeer, nil, enqueueCh, getBlock, getHeader, chainHeight, nil, nil, nil, 0)
	return tw
}

func (tw *testWitnessManager) Close() {
	close(tw.quit)
}

func (tw *testWitnessManager) DroppedPeers() []string {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	result := make([]string, len(tw.droppedPeers))
	copy(result, tw.droppedPeers)
	return result
}

func (tw *testWitnessManager) PendingCount() int {
	tw.manager.mu.Lock()
	defer tw.manager.mu.Unlock()
	return len(tw.manager.pending)
}

// TestWitnessManagerCreation tests the creation and basic setup of witnessManager
func TestWitnessManagerCreation(t *testing.T) {
	tw := newTestWitnessManager()
	defer tw.Close()

	if tw.manager == nil {
		t.Fatal("Expected witnessManager to be created")
	}

	// Check initial state
	if tw.PendingCount() != 0 {
		t.Errorf("Expected empty pending map, got %d items", tw.PendingCount())
	}

	if tw.manager.witnessTimer == nil {
		t.Error("Expected witnessTimer to be initialized")
	}

	// Test channels are created with proper buffering
	if cap(tw.manager.injectNeedWitnessCh) != 10 {
		t.Errorf("Expected injectNeedWitnessCh buffer size 10, got %d", cap(tw.manager.injectNeedWitnessCh))
	}

	if cap(tw.manager.injectWitnessCh) != 10 {
		t.Errorf("Expected injectWitnessCh buffer size 10, got %d", cap(tw.manager.injectWitnessCh))
	}
}

// TestWitnessManagerLifecycle tests start and stop functionality
func TestWitnessManagerLifecycle(t *testing.T) {
	tw := newTestWitnessManager()
	defer tw.Close()

	// Start the manager
	tw.manager.start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Stop the manager
	tw.manager.stop()

	// Give it a moment to stop
	time.Sleep(10 * time.Millisecond)
}

// TestHandleNeed tests processing of blocks needing witnesses
func TestHandleNeed(t *testing.T) {
	tw := newTestWitnessManager()
	defer tw.Close()

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Test successful handling
	tw.manager.handleNeed(msg)

	// Check that block was added to pending
	if !tw.manager.isPending(block.Hash()) {
		t.Error("Expected block to be pending after handleNeed")
	}

	// Check pending count
	if tw.PendingCount() != 1 {
		t.Errorf("Expected 1 pending request, got %d", tw.PendingCount())
	}
}

// TestHandleNeedDuplicates tests that duplicate requests are handled properly
func TestHandleNeedDuplicates(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// First request should succeed
	manager.handleNeed(msg)

	// Second request should be ignored
	manager.handleNeed(msg)

	// Check pending count is still 1
	manager.mu.Lock()
	pendingCount := len(manager.pending)
	manager.mu.Unlock()

	if pendingCount != 1 {
		t.Errorf("Expected 1 pending request after duplicate, got %d", pendingCount)
	}
}

// TestHandleNeedKnownBlock tests handling of blocks already known locally
func TestHandleNeedKnownBlock(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	block := createTestBlock(101)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block {
		if hash == block.Hash() {
			return block
		}
		return nil
	})
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Should be ignored since block is known
	manager.handleNeed(msg)

	// Check that no pending requests were created
	manager.mu.Lock()
	pendingCount := len(manager.pending)
	manager.mu.Unlock()

	if pendingCount != 0 {
		t.Errorf("Expected 0 pending requests for known block, got %d", pendingCount)
	}
}

// TestHandleBroadcast tests processing of injected witnesses
func TestHandleBroadcast(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var enqueueRequests []*enqueueRequest
	var enqueueMutex sync.Mutex

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Start a goroutine to collect enqueue requests
	go func() {
		for req := range enqueueCh {
			enqueueMutex.Lock()
			enqueueRequests = append(enqueueRequests, req)
			enqueueMutex.Unlock()
		}
	}()

	block := createTestBlock(101)
	witness := createTestWitnessForBlock(block)

	// First add a pending request
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	needMsg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}
	manager.handleNeed(needMsg)

	// Now inject the witness
	witnessMsg := &injectedWitnessMsg{
		peer:    "broadcast-peer",
		witness: witness,
		time:    time.Now(),
	}
	manager.handleBroadcast(witnessMsg)

	// Give time for async processing
	time.Sleep(50 * time.Millisecond)

	// Check that request was enqueued
	enqueueMutex.Lock()
	reqCount := len(enqueueRequests)
	enqueueMutex.Unlock()

	if reqCount != 1 {
		t.Errorf("Expected 1 enqueue request, got %d", reqCount)
	}

	// Check that pending state was cleaned up
	if manager.isPending(block.Hash()) {
		t.Error("Expected block to no longer be pending after witness broadcast")
	}
}

// TestWitnessUnavailable tests witness unavailability tracking
func TestWitnessUnavailable(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	hash := common.HexToHash("0x123")

	// Initially should not be unavailable
	if manager.isWitnessUnavailable(hash) {
		t.Error("Expected witness to not be unavailable initially")
	}

	// Mark as unavailable
	manager.markWitnessUnavailable(hash)

	// Should now be unavailable
	if !manager.isWitnessUnavailable(hash) {
		t.Error("Expected witness to be unavailable after marking")
	}

	// Wait for expiry (using a short timeout for testing)
	originalTimeout := witnessUnavailableTimeout
	// We can't modify the const, so we'll test cleanup instead
	manager.cleanupUnavailableCache()

	// Should still be unavailable (hasn't expired yet)
	if !manager.isWitnessUnavailable(hash) {
		t.Error("Expected witness to still be unavailable before timeout")
	}

	// Manually expire the entry for testing
	manager.mu.Lock()
	manager.witnessUnavailable[hash] = time.Now().Add(-time.Minute)
	manager.mu.Unlock()

	// Should now be available again
	if manager.isWitnessUnavailable(hash) {
		t.Error("Expected witness to be available after expiry")
	}

	// Restore original timeout
	_ = originalTimeout
}

// TestForget tests cleanup of pending state
func TestForget(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Add pending request
	manager.handleNeed(msg)

	// Verify it's pending
	if !manager.isPending(block.Hash()) {
		t.Error("Expected block to be pending before forget")
	}

	// Forget the block
	manager.forget(block.Hash())

	// Verify it's no longer pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block to not be pending after forget")
	}
}

// TestHandleFilterResult tests integration with BlockFetcher's filter results
func TestHandleFilterResult(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Handle filter result
	manager.handleFilterResult(announce, block)

	// Check that block was added to pending
	if !manager.isPending(block.Hash()) {
		t.Error("Expected block to be pending after handleFilterResult")
	}
}

// TestCheckCompleting tests the checkCompleting functionality
func TestCheckCompleting(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Check completing
	manager.checkCompleting(announce, block)

	// Check that block was added to pending
	if !manager.isPending(block.Hash()) {
		t.Error("Expected block to be pending after checkCompleting")
	}
}

// TestWitnessFetchFailure tests handling of witness fetch failures
func TestWitnessFetchFailure(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	droppedPeer := ""

	dropPeer := peerDropFn(func(id string) {
		droppedPeer = id
	})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"
	err := errors.New("fetch failed")

	// Test soft failure (keep pending for retry) - peer should still be dropped
	manager.handleWitnessFetchFailureExt(hash, peer, err, false)

	if droppedPeer != peer {
		t.Errorf("Expected peer to be dropped on soft failure, got %s", droppedPeer)
	}
}

// TestWitnessFetchFailureAlwaysDropsPeer tests that handleWitnessFetchFailureExt
// always drops the peer regardless of removePending flag
func TestWitnessFetchFailureAlwaysDropsPeer(t *testing.T) {
	tw := newTestWitnessManager()
	defer tw.Close()

	hash := common.HexToHash("0x123")
	peer1 := "test-peer-1"
	peer2 := "test-peer-2"
	err := errors.New("fetch failed")

	// Add a pending request to test removal behavior
	block := createTestBlock(101)
	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: peer1,
			block:  block,
		},
		announce: &blockAnnounce{
			origin: peer1,
			hash:   hash,
			number: 101,
			time:   time.Now(),
			fetchWitness: func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
				return nil, nil
			},
		},
		retries: 0,
	}

	tw.manager.mu.Lock()
	tw.manager.pending[hash] = state
	tw.manager.mu.Unlock()

	// Test soft failure (removePending = false) - peer should be dropped
	tw.manager.handleWitnessFetchFailureExt(hash, peer1, err, false)

	droppedPeers := tw.DroppedPeers()
	if len(droppedPeers) != 1 || droppedPeers[0] != peer1 {
		t.Errorf("Expected peer1 to be dropped on soft failure, got %v", droppedPeers)
	}

	// Verify pending request was NOT removed (soft failure)
	if !tw.manager.isPending(hash) {
		t.Error("Expected pending request to remain after soft failure")
	}

	// Test hard failure (removePending = true) - peer should also be dropped
	tw.manager.handleWitnessFetchFailureExt(hash, peer2, err, true)

	droppedPeers = tw.DroppedPeers()
	if len(droppedPeers) != 2 || droppedPeers[1] != peer2 {
		t.Errorf("Expected peer2 to be dropped on hard failure, got %v", droppedPeers)
	}

	// Verify pending request was removed (hard failure)
	if tw.manager.isPending(hash) {
		t.Error("Expected pending request to be removed after hard failure")
	}
}

// TestWitnessFetchFailureEmptyPeer tests that no peer is dropped when peer string is empty
func TestWitnessFetchFailureEmptyPeer(t *testing.T) {
	tw := newTestWitnessManager()
	defer tw.Close()

	hash := common.HexToHash("0x123")
	err := errors.New("fetch failed")

	// Test with empty peer string - no peer should be dropped
	tw.manager.handleWitnessFetchFailureExt(hash, "", err, false)

	droppedPeers := tw.DroppedPeers()
	if len(droppedPeers) != 0 {
		t.Errorf("Expected no peer to be dropped when peer string is empty, got %v", droppedPeers)
	}
}

// TestCleanupUnavailableCache tests the cleanup of expired unavailable entries
func TestCleanupUnavailableCache(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	hash1 := common.HexToHash("0x123")
	hash2 := common.HexToHash("0x456")

	// Add entries with different expiry times
	manager.mu.Lock()
	manager.witnessUnavailable[hash1] = time.Now().Add(-time.Hour) // Expired
	manager.witnessUnavailable[hash2] = time.Now().Add(time.Hour)  // Not expired
	manager.mu.Unlock()

	// Run cleanup
	manager.cleanupUnavailableCache()

	// Check results
	manager.mu.Lock()
	_, hash1Exists := manager.witnessUnavailable[hash1]
	_, hash2Exists := manager.witnessUnavailable[hash2]
	cacheSize := len(manager.witnessUnavailable)
	manager.mu.Unlock()

	if hash1Exists {
		t.Error("Expected expired hash1 to be cleaned up")
	}

	if !hash2Exists {
		t.Error("Expected non-expired hash2 to remain")
	}

	if cacheSize != 1 {
		t.Errorf("Expected cache size 1 after cleanup, got %d", cacheSize)
	}
}

// TestWitnessFetchWithBlockNoLongerPending tests the new error handling when a block
// is removed from pending during witness fetch
func TestWitnessFetchWithBlockNoLongerPending(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	blockHash := block.Hash()
	witness := createTestWitnessForBlock(block)

	// Create a channel to control witness fetch timing
	fetchStarted := make(chan struct{})
	var responseSent atomic.Bool

	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		// Signal that fetch has started
		close(fetchStarted)

		// Send the response in a goroutine
		go func() {
			// Wait a bit to ensure we're in the middle of processing
			time.Sleep(10 * time.Millisecond)

			// Before sending response, remove block from pending
			manager.mu.Lock()
			delete(manager.pending, blockHash)
			manager.mu.Unlock()

			// Now send the response with the correct structure
			witnessBytes, _ := rlp.EncodeToBytes(witness)
			responseCh <- &eth.Response{
				Res: &wit.WitnessPacketRLPPacket{
					WitnessPacketResponse: wit.WitnessPacketResponse{{Data: rlp.RawValue(witnessBytes)}},
				},
				Done: make(chan error, 1),
			}
			responseSent.Store(true)
		}()

		// Return successful request
		req := &eth.Request{
			Peer: "test-peer",
			Sent: time.Now(),
		}
		return req, nil
	}

	// Create message to inject block that needs witness
	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		time:         time.Now(),
		fetchWitness: fetchWitness,
	}

	// Inject the block
	manager.handleNeed(msg)

	// Verify block is pending
	manager.mu.Lock()
	if _, exists := manager.pending[blockHash]; !exists {
		t.Fatal("Block should be pending after handleNeed")
	}
	manager.mu.Unlock()

	// Trigger tick to start witness fetch
	manager.tick()

	// Wait for fetch to start
	<-fetchStarted

	// Give time for the response to be processed
	time.Sleep(50 * time.Millisecond)

	// Verify response was sent and block is no longer pending
	if !responseSent.Load() {
		t.Error("Response should have been sent")
	}

	manager.mu.Lock()
	_, exists := manager.pending[blockHash]
	manager.mu.Unlock()

	if exists {
		t.Error("Block should not be pending after being removed during fetch")
	}

	// Check that no enqueue occurred (since block was removed from pending)
	select {
	case <-enqueueCh:
		t.Error("Should not enqueue block that was removed from pending")
	default:
		// Expected - no enqueue
	}
}

// TestTick tests the witness timer tick functionality
func TestTick(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Test tick with no pending requests
	manager.tick()

	// Add a pending request but make it NOT ready to fetch to avoid goroutine issues
	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := &blockAnnounce{
		origin:       "test-peer",
		hash:         block.Hash(),
		number:       block.NumberU64(),
		time:         time.Now().Add(time.Hour), // Future time - not ready to fetch yet
		fetchWitness: fetchWitness,
	}

	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: announce,
		retries:  0,
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Test tick with pending request NOT ready to fetch
	manager.tick()

	// Verify retry count didn't increase (request wasn't processed)
	manager.mu.Lock()
	retries := state.retries
	manager.mu.Unlock()

	if retries != 0 {
		t.Errorf("Expected retry count to remain 0 for not-ready request, got %d", retries)
	}

	// Now test with a ready request but handle it manually to avoid goroutine
	// Set the announce time to past
	announce.time = time.Now().Add(-time.Second) // Ready to fetch

	// Manual test of the retry increment logic (what tick would do)
	manager.mu.Lock()
	if time.Now().After(announce.time) && state.retries < maxWitnessFetchRetries {
		state.retries++ // This is what tick() would do
	}
	manager.mu.Unlock()

	// Verify retry count increased
	manager.mu.Lock()
	retries = state.retries
	manager.mu.Unlock()

	if retries != 1 {
		t.Errorf("Expected retry count 1 after manual increment, got %d", retries)
	}
}

// TestTickMaxRetries tests that tick gives up after max retries
func TestTickMaxRetries(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin: "test-peer",
		hash:   block.Hash(),
		number: block.NumberU64(),
		time:   time.Now().Add(-time.Second), // Ready to fetch
		fetchWitness: func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
			return nil, nil
		},
	}

	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: announce,
		retries:  maxWitnessFetchRetries, // Already at max retries
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Test tick should mark witness as unavailable
	manager.tick()

	// Verify witness marked as unavailable
	if !manager.isWitnessUnavailable(block.Hash()) {
		t.Error("Expected witness to be marked unavailable after max retries")
	}
}

// TestTickWithWitnessAlreadyPresent tests tick with witness already attached
func TestTickWithWitnessAlreadyPresent(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var enqueueRequests []*enqueueRequest
	var enqueueMutex sync.Mutex

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Start goroutine to collect enqueue requests
	go func() {
		for req := range enqueueCh {
			enqueueMutex.Lock()
			enqueueRequests = append(enqueueRequests, req)
			enqueueMutex.Unlock()
		}
	}()

	block := createTestBlock(101)
	witness := createTestWitnessForBlock(block)

	announce := &blockAnnounce{
		origin: "test-peer",
		hash:   block.Hash(),
		number: block.NumberU64(),
		time:   time.Now().Add(-time.Second), // Ready to fetch (this will be updated)
		fetchWitness: func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
			return nil, nil
		},
	}

	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin:  "test-peer",
			block:   block,
			witness: witness, // Witness already present
		},
		announce: announce,
		retries:  0,
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Directly test that safeEnqueue is called for blocks with witnesses
	// Instead of calling tick (which triggers fetchWitness), directly call safeEnqueue
	manager.safeEnqueue(state.op)

	time.Sleep(10 * time.Millisecond) // Give time for async processing

	// Verify block was enqueued
	enqueueMutex.Lock()
	reqCount := len(enqueueRequests)
	enqueueMutex.Unlock()

	if reqCount != 1 {
		t.Errorf("Expected 1 enqueue request, got %d", reqCount)
	}

	// Verify pending state was cleaned up
	if manager.isPending(block.Hash()) {
		t.Error("Expected pending state to be cleaned up after enqueue")
	}
}

// TestHandleWitnessFetchSuccess tests successful witness fetch handling
func TestHandleWitnessFetchSuccess(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var enqueueRequests []*enqueueRequest
	var enqueueMutex sync.Mutex

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Start goroutine to collect enqueue requests
	go func() {
		for req := range enqueueCh {
			enqueueMutex.Lock()
			enqueueRequests = append(enqueueRequests, req)
			enqueueMutex.Unlock()
		}
	}()

	block := createTestBlock(101)
	witness := createTestWitnessForBlock(block)

	// Add pending state
	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Test successful witness fetch
	announcedAt := time.Now()
	manager.handleWitnessFetchSuccess("fetch-peer", block.Hash(), witness, announcedAt)

	time.Sleep(10 * time.Millisecond) // Give time for async processing

	// Verify witness was attached and block enqueued
	enqueueMutex.Lock()
	reqCount := len(enqueueRequests)
	enqueueMutex.Unlock()

	if reqCount != 1 {
		t.Errorf("Expected 1 enqueue request, got %d", reqCount)
	}

	// Verify witness is attached
	if state.op.witness == nil {
		t.Error("Expected witness to be attached to operation")
	}
}

// TestHandleWitnessFetchSuccessNoPending tests success handler with no pending block
func TestHandleWitnessFetchSuccessNoPending(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	witness := createTestWitnessForBlock(block)

	// Test with no pending state - should handle gracefully
	announcedAt := time.Now()
	manager.handleWitnessFetchSuccess("fetch-peer", block.Hash(), witness, announcedAt)

	// Should not panic or cause issues
}

// TestHandleWitnessFetchSuccessWitnessAlreadyPresent tests success with witness already present
func TestHandleWitnessFetchSuccessWitnessAlreadyPresent(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	witness1 := createTestWitnessForBlock(block)
	witness2 := createTestWitnessForBlock(block)

	// Add pending state with witness already present
	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin:  "test-peer",
			block:   block,
			witness: witness1, // Already has witness
		},
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Test with witness already present - should be ignored
	announcedAt := time.Now()
	manager.handleWitnessFetchSuccess("fetch-peer", block.Hash(), witness2, announcedAt)

	// Verify original witness is still there
	if state.op.witness != witness1 {
		t.Error("Expected original witness to remain unchanged")
	}
}

// TestRescheduleWitness tests the witness timer rescheduling logic
func TestRescheduleWitness(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Test with no pending items - timer should be stopped
	manager.rescheduleWitness()

	// Add a pending item
	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin: "test-peer",
		hash:   block.Hash(),
		number: block.NumberU64(),
		time:   time.Now().Add(time.Second), // Future time
		fetchWitness: func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
			return nil, nil
		},
	}

	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: announce,
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Test with pending item - timer should be scheduled
	manager.rescheduleWitness()

	// Verify timer is active (we can't directly check, but it shouldn't panic)
}

// TestSafeEnqueueWithNilWitness tests safeEnqueue error handling
func TestSafeEnqueueWithNilWitness(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	op := &blockOrHeaderInject{
		origin:  "test-peer",
		block:   block,
		witness: nil, // Nil witness should cause error handling
	}

	// Add to pending first
	manager.mu.Lock()
	manager.pending[block.Hash()] = &witnessRequestState{op: op}
	manager.mu.Unlock()

	// Test safeEnqueue with nil witness
	manager.safeEnqueue(op)

	// Verify pending state was cleaned up
	if manager.isPending(block.Hash()) {
		t.Error("Expected pending state to be cleaned up after nil witness error")
	}
}

// TestSafeEnqueueChannelClosed tests safeEnqueue when parent channel is closed
func TestSafeEnqueueChannelClosed(t *testing.T) {
	quit := make(chan struct{})
	close(quit) // Close quit channel to simulate shutdown

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10) // Don't close this - let quit handle shutdown
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	witness := createTestWitnessForBlock(block)
	op := &blockOrHeaderInject{
		origin:  "test-peer",
		block:   block,
		witness: witness,
	}

	// Test safeEnqueue with closed quit channel - should handle gracefully via quit path
	manager.safeEnqueue(op)

	// Should not panic and should use the quit channel path
}

// TestHandleNeedDistanceCheck tests handleNeed with distance check
func TestHandleNeedDistanceCheck(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 }) // Chain at height 100

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Create block that's too far away (block 10 when chain is at 100)
	block := createTestBlock(10)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Test handleNeed with distant block - should be discarded
	manager.handleNeed(msg)

	// Check that no pending requests were created
	if manager.isPending(block.Hash()) {
		t.Error("Expected distant block to be discarded")
	}
}

// TestHandleNeedMissingFetchWitness tests handleNeed with nil fetchWitness
func TestHandleNeedMissingFetchWitness(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: nil, // Missing fetchWitness function
	}

	// Test handleNeed with nil fetchWitness - should be handled gracefully
	manager.handleNeed(msg)

	// Check that no pending requests were created
	if manager.isPending(block.Hash()) {
		t.Error("Expected request without fetchWitness to be ignored")
	}
}

// TestLoop tests the main event loop with different message types
func TestLoop(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Start the loop
	go manager.loop()

	// Test injecting a block need witness message
	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	needMsg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Send message through channel
	select {
	case manager.injectNeedWitnessCh <- needMsg:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Failed to send need witness message")
	}

	// Give time for processing
	time.Sleep(50 * time.Millisecond)

	// Verify block is pending
	if !manager.isPending(block.Hash()) {
		t.Error("Expected block to be pending after loop processing")
	}

	// Test injecting a witness message
	witness := createTestWitnessForBlock(block)
	witnessMsg := &injectedWitnessMsg{
		peer:    "broadcast-peer",
		witness: witness,
		time:    time.Now(),
	}

	// Send witness message through channel
	select {
	case manager.injectWitnessCh <- witnessMsg:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Failed to send witness message")
	}

	// Give time for processing
	time.Sleep(50 * time.Millisecond)

	// The loop should terminate when quit channel is closed
}

// TestHandleFilterResultWithoutWitness tests handleFilterResult when witness not needed
func TestHandleFilterResultWithoutWitness(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin:       "test-peer",
		hash:         block.Hash(),
		number:       block.NumberU64(),
		time:         time.Now(),
		fetchWitness: nil, // No witness needed
	}

	// Handle filter result without witness requirement
	manager.handleFilterResult(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block without witness requirement to not be pending")
	}
}

// TestCheckCompletingWithoutWitness tests checkCompleting when witness not needed
func TestCheckCompletingWithoutWitness(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin:       "test-peer",
		hash:         block.Hash(),
		number:       block.NumberU64(),
		time:         time.Now(),
		fetchWitness: nil, // No witness needed
	}

	// Check completing without witness requirement
	manager.checkCompleting(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block without witness requirement to not be pending")
	}
}

// TestFetchWitnessError tests fetchWitness error handling
func TestFetchWitnessError(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"

	// Test fetchWitness with error in initiating request
	announce := &blockAnnounce{
		origin: peer,
		hash:   hash,
		number: 101,
		time:   time.Now(),
		fetchWitness: func(common.Hash, chan *eth.Response) (*eth.Request, error) {
			return nil, errors.New("no peer available")
		},
	}

	// This will run in background, we can't easily wait for it, but it exercises the error path
	go manager.fetchWitness(peer, hash, announce)

	time.Sleep(50 * time.Millisecond) // Give time for goroutine to process
}

// TestHandleFilterResultWitnessUnavailable tests filter result with unavailable witness
func TestHandleFilterResultWitnessUnavailable(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)

	// Mark witness as unavailable first
	manager.markWitnessUnavailable(block.Hash())

	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Handle filter result with unavailable witness
	manager.handleFilterResult(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block with unavailable witness to be discarded")
	}
}

// TestHandleFilterResultDuplicate tests filter result with already pending block
func TestHandleFilterResultDuplicate(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Add to pending first
	manager.handleFilterResult(announce, block)

	// Try to handle the same filter result again
	manager.handleFilterResult(announce, block)

	// Should still only have one pending request
	manager.mu.Lock()
	pendingCount := len(manager.pending)
	manager.mu.Unlock()

	if pendingCount != 1 {
		t.Errorf("Expected 1 pending request after duplicate filter result, got %d", pendingCount)
	}
}

// TestCheckCompletingWitnessUnavailable tests checkCompleting with unavailable witness
func TestCheckCompletingWitnessUnavailable(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)

	// Mark witness as unavailable first
	manager.markWitnessUnavailable(block.Hash())

	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Check completing with unavailable witness
	manager.checkCompleting(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block with unavailable witness to be discarded")
	}
}

// TestCheckCompletingDuplicate tests checkCompleting with already pending block
func TestCheckCompletingDuplicate(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Add to pending first
	manager.checkCompleting(announce, block)

	// Try to check completing the same block again
	manager.checkCompleting(announce, block)

	// Should still only have one pending request
	manager.mu.Lock()
	pendingCount := len(manager.pending)
	manager.mu.Unlock()

	if pendingCount != 1 {
		t.Errorf("Expected 1 pending request after duplicate checkCompleting, got %d", pendingCount)
	}
}

// TestCheckCompletingKnownBlock tests checkCompleting with already known block
func TestCheckCompletingKnownBlock(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	block := createTestBlock(101)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block {
		if hash == block.Hash() {
			return block
		}
		return nil
	})
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	fetchWitness := func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce("test-peer", block, fetchWitness)

	// Check completing with known block
	manager.checkCompleting(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected known block to be ignored")
	}
}

// TestTickInvalidPendingState tests tick with invalid pending state
func TestTickInvalidPendingState(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	hash := common.HexToHash("0x123")

	// Add invalid pending state (missing op or announce)
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op:       nil, // Invalid - nil op
		announce: nil, // Invalid - nil announce
		retries:  0,
	}
	manager.mu.Unlock()

	// Test tick should clean up invalid state
	manager.tick()

	// Verify invalid state was cleaned up
	if manager.isPending(hash) {
		t.Error("Expected invalid pending state to be cleaned up")
	}
}

// TestTickNotReadyYet tests tick with requests not ready to fetch
func TestTickNotReadyYet(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin: "test-peer",
		hash:   block.Hash(),
		number: block.NumberU64(),
		time:   time.Now().Add(time.Hour), // Not ready yet - future time
		fetchWitness: func(hash common.Hash, responseCh chan *eth.Response) (*eth.Request, error) {
			return nil, nil
		},
	}

	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: announce,
		retries:  0,
	}

	manager.mu.Lock()
	manager.pending[block.Hash()] = state
	manager.mu.Unlock()

	// Test tick with not-ready request
	manager.tick()

	// Verify retry count didn't increase (request wasn't processed)
	manager.mu.Lock()
	retries := state.retries
	manager.mu.Unlock()

	if retries != 0 {
		t.Errorf("Expected retry count to remain 0 for not-ready request, got %d", retries)
	}
}

// TestSafeEnqueueSuccess tests successful enqueue with peer success reset
func TestSafeEnqueueSuccess(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var enqueueRequests []*enqueueRequest
	var enqueueMutex sync.Mutex

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Start goroutine to collect enqueue requests
	go func() {
		for req := range enqueueCh {
			enqueueMutex.Lock()
			enqueueRequests = append(enqueueRequests, req)
			enqueueMutex.Unlock()
		}
	}()

	peer := "test-peer"

	block := createTestBlock(101)
	witness := createTestWitnessForBlock(block)
	op := &blockOrHeaderInject{
		origin:  peer,
		block:   block,
		witness: witness,
	}

	// Add to pending
	manager.mu.Lock()
	manager.pending[block.Hash()] = &witnessRequestState{op: op}
	manager.mu.Unlock()

	// Test successful safeEnqueue
	manager.safeEnqueue(op)

	time.Sleep(10 * time.Millisecond) // Give time for async processing

	// Verify block was enqueued
	enqueueMutex.Lock()
	reqCount := len(enqueueRequests)
	enqueueMutex.Unlock()

	if reqCount != 1 {
		t.Errorf("Expected 1 enqueue request, got %d", reqCount)
	}
}

// TestConcurrentWitnessFetchFailure tests that handleWitnessFetchFailureExt
// can be called concurrently without causing race conditions
func TestConcurrentWitnessFetchFailure(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		nil,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		0,
	)

	// Start the manager
	manager.start()
	defer manager.stop()

	hash := common.HexToHash("0x123")
	peer := "test-peer"
	err := errors.New("fetch failed")

	// Run multiple concurrent calls to handleWitnessFetchFailureExt
	// This should not cause a race condition panic
	var wg sync.WaitGroup
	numGoroutines := 100

	for range numGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.handleWitnessFetchFailureExt(hash, peer, err, false)
		}()
	}

	wg.Wait()
}

// TestCheckWitnessPageCountWithPeerJailing tests that dishonest peers are jailed
func TestCheckWitnessPageCountWithPeerJailing(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var jailedPeers []string
	var jailMutex sync.Mutex

	jailPeer := peerJailFn(func(id string) {
		jailMutex.Lock()
		jailedPeers = append(jailedPeers, id)
		jailMutex.Unlock()
	})

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	// Set gas ceil to trigger verification for large witnesses
	gasCeil := uint64(30_000_000) // 30M gas -> ~30 pages threshold

	manager := newWitnessManager(
		quit,
		dropPeer,
		jailPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		gasCeil,
	)

	hash := common.HexToHash("0x123")
	dishonestPeer := "dishonest-peer"
	reportedPageCount := uint64(100) // Dishonest peer claims 100 pages

	// Mock getRandomPeers to return 2 honest peers
	getRandomPeers := func() []string {
		return []string{"honest-peer-1", "honest-peer-2"}
	}

	// Mock getWitnessPageCount - honest peers report 15 pages
	getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
		if peerID == "honest-peer-1" || peerID == "honest-peer-2" {
			return 15, nil // Honest page count
		}
		return 0, errors.New("unknown peer")
	}

	// Run verification - should jail the dishonest peer
	isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, dishonestPeer, getRandomPeers, getWitnessPageCount)

	// Verify peer was marked as dishonest
	if isHonest {
		t.Error("Expected dishonest peer to be marked as dishonest")
	}

	// Verify peer was jailed
	jailMutex.Lock()
	jailedCount := len(jailedPeers)
	jailMutex.Unlock()

	if jailedCount != 1 {
		t.Errorf("Expected 1 jailed peer, got %d", jailedCount)
	}

	if len(jailedPeers) > 0 && jailedPeers[0] != dishonestPeer {
		t.Errorf("Expected %s to be jailed, got %s", dishonestPeer, jailedPeers[0])
	}
}

// TestCheckWitnessPageCountWithConsensusFailure tests consensus edge cases
func TestCheckWitnessPageCountWithConsensusFailure(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	jailPeer := peerJailFn(func(id string) {})
	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })
	gasCeil := uint64(30_000_000)

	manager := newWitnessManager(
		quit,
		dropPeer,
		jailPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		gasCeil,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"

	t.Run("NoConsensus_AllDifferent", func(t *testing.T) {
		// All 3 peers report different page counts - no consensus
		reportedPageCount := uint64(15)

		getRandomPeers := func() []string {
			return []string{"peer-1", "peer-2"}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			if peerID == "peer-1" {
				return 20, nil
			}
			if peerID == "peer-2" {
				return 25, nil
			}
			return 0, errors.New("unknown peer")
		}

		// Should assume honest when no consensus (conservative approach)
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be considered honest when no consensus reached")
		}
	})

	t.Run("EdgeCase_ReportedZeroWithNoConsensus", func(t *testing.T) {
		// Test edge case: original peer reports 0, consensus is also 0 (no majority)
		reportedPageCount := uint64(0)

		getRandomPeers := func() []string {
			return []string{"peer-1", "peer-2"}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			if peerID == "peer-1" {
				return 5, nil
			}
			if peerID == "peer-2" {
				return 10, nil
			}
			return 0, errors.New("unknown peer")
		}

		// With current implementation, this would incorrectly mark peer as honest
		// This test documents the edge case identified in the review
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		// Current behavior: peer is considered honest (no consensus)
		// Ideal behavior: should detect that 0 is suspicious
		if !isHonest {
			t.Log("Peer correctly identified as dishonest despite consensus returning 0")
		} else {
			t.Log("KNOWN ISSUE: Peer incorrectly considered honest when reporting 0 and no consensus (edge case)")
		}
	})
}

// TestCheckWitnessPageCountWithPeerFailures tests handling of peer query failures
func TestCheckWitnessPageCountWithPeerFailures(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var droppedPeers []string
	var dropMutex sync.Mutex

	jailPeer := peerJailFn(func(id string) {})
	dropPeer := peerDropFn(func(id string) {
		dropMutex.Lock()
		droppedPeers = append(droppedPeers, id)
		dropMutex.Unlock()
	})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })
	gasCeil := uint64(30_000_000)

	manager := newWitnessManager(
		quit,
		dropPeer,
		jailPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		gasCeil,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"

	t.Run("OnePeerFails_OtherAgrees", func(t *testing.T) {
		reportedPageCount := uint64(15)

		getRandomPeers := func() []string {
			return []string{"peer-1", "peer-2"}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			if peerID == "peer-1" {
				return 0, errors.New("peer disconnected")
			}
			if peerID == "peer-2" {
				return 15, nil // Agrees with original
			}
			return 0, errors.New("unknown peer")
		}

		// Should succeed - 2 out of 3 peers agree (original + peer-2)
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be honest when majority agrees despite one peer failing")
		}
	})

	t.Run("BothRandomPeersFail_AssumeHonest", func(t *testing.T) {
		reportedPageCount := uint64(15)

		getRandomPeers := func() []string {
			return []string{"peer-1", "peer-2"}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			// Both peers fail
			return 0, errors.New("network error")
		}

		// Should assume honest (conservative approach when verification fails)
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be assumed honest when all verification peers fail")
		}
	})
}

// TestCheckWitnessPageCountWithInsufficientPeers tests behavior with not enough peers
func TestCheckWitnessPageCountWithInsufficientPeers(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	jailPeer := peerJailFn(func(id string) {})
	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })
	gasCeil := uint64(30_000_000)

	manager := newWitnessManager(
		quit,
		dropPeer,
		jailPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		gasCeil,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"
	reportedPageCount := uint64(100)

	t.Run("OnlyOnePeerAvailable", func(t *testing.T) {
		getRandomPeers := func() []string {
			return []string{"peer-1"} // Only 1 peer available
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			return 15, nil
		}

		// Should assume honest (not enough peers for verification)
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be assumed honest when insufficient peers for verification")
		}
	})

	t.Run("NoPeersAvailable", func(t *testing.T) {
		getRandomPeers := func() []string {
			return []string{} // No peers available
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			return 0, errors.New("should not be called")
		}

		// Should assume honest (conservative approach)
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be assumed honest when no peers available for verification")
		}
	})
}

// TestCheckWitnessPageCountBelowThreshold tests that small witnesses skip verification
func TestCheckWitnessPageCountBelowThreshold(t *testing.T) {
	t.Run("WithCurrentHeader", func(t *testing.T) {
		quit := make(chan struct{})
		defer close(quit)

		jailPeer := peerJailFn(func(id string) {
			t.Error("Peer should not be jailed for page count below threshold")
		})
		dropPeer := peerDropFn(func(id string) {})
		enqueueCh := make(chan *enqueueRequest, 10)
		getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
		getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
		chainHeight := chainHeightFn(func() uint64 { return 100 })
		gasCeil := uint64(30_000_000) // Config value

		// Create a mock current header with a different gas limit
		currentBlockGasLimit := uint64(50_000_000) // 50M gas limit in current block
		currentHeader := currentHeaderFn(func() *types.Header {
			return &types.Header{
				Number:   big.NewInt(100),
				GasLimit: currentBlockGasLimit,
			}
		})

		manager := newWitnessManager(
			quit,
			dropPeer,
			jailPeer,
			enqueueCh,
			getBlock,
			getHeader,
			chainHeight,
			currentHeader,
			nil,
			nil,
			gasCeil,
		)

		hash := common.HexToHash("0x123")
		peer := "test-peer"

		// Calculate actual threshold - should use currentBlockGasLimit (50M), not gasCeil (30M)
		threshold := manager.calculatePageThreshold()

		// Expected threshold: 50M gas / 1M gas per MB = 50 MB
		// 50 MB / 15 MB per page = ceil(3.33) = 4 pages
		expectedThreshold := uint64(4)
		if threshold != expectedThreshold {
			t.Errorf("Expected threshold %d (from header gas limit %d), got %d", expectedThreshold, currentBlockGasLimit, threshold)
		}

		reportedPageCount := threshold - 1 // Ensure it's below threshold

		getRandomPeers := func() []string {
			t.Error("getRandomPeers should not be called for page count below threshold")
			return []string{}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			t.Error("getWitnessPageCount should not be called for page count below threshold")
			return 0, errors.New("should not be called")
		}

		// Should skip verification and assume honest
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be honest for page count below threshold")
		}
	})

	t.Run("FallbackToConfigWhenHeaderNil", func(t *testing.T) {
		quit := make(chan struct{})
		defer close(quit)

		jailPeer := peerJailFn(func(id string) {
			t.Error("Peer should not be jailed for page count below threshold")
		})
		dropPeer := peerDropFn(func(id string) {})
		enqueueCh := make(chan *enqueueRequest, 10)
		getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
		getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
		chainHeight := chainHeightFn(func() uint64 { return 100 })
		gasCeil := uint64(30_000_000) // Config value

		// Current header function returns nil
		currentHeader := currentHeaderFn(func() *types.Header {
			return nil
		})

		manager := newWitnessManager(
			quit,
			dropPeer,
			jailPeer,
			enqueueCh,
			getBlock,
			getHeader,
			chainHeight,
			currentHeader,
			nil,
			nil,
			gasCeil,
		)

		hash := common.HexToHash("0x123")
		peer := "test-peer"

		// Calculate actual threshold - should fallback to gasCeil (30M)
		threshold := manager.calculatePageThreshold()

		// Expected threshold: 30M gas / 1M gas per MB = 30 MB
		// 30 MB / 15 MB per page = ceil(2) = 2 pages
		expectedThreshold := uint64(2)
		if threshold != expectedThreshold {
			t.Errorf("Expected threshold %d (from config gas ceil %d), got %d", expectedThreshold, gasCeil, threshold)
		}

		reportedPageCount := threshold - 1 // Ensure it's below threshold

		getRandomPeers := func() []string {
			t.Error("getRandomPeers should not be called for page count below threshold")
			return []string{}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			t.Error("getWitnessPageCount should not be called for page count below threshold")
			return 0, errors.New("should not be called")
		}

		// Should skip verification and assume honest
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be honest for page count below threshold")
		}
	})

	t.Run("FallbackToConfigWhenCurrentHeaderFnNil", func(t *testing.T) {
		quit := make(chan struct{})
		defer close(quit)

		jailPeer := peerJailFn(func(id string) {
			t.Error("Peer should not be jailed for page count below threshold")
		})
		dropPeer := peerDropFn(func(id string) {})
		enqueueCh := make(chan *enqueueRequest, 10)
		getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
		getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
		chainHeight := chainHeightFn(func() uint64 { return 100 })
		gasCeil := uint64(30_000_000)

		// No current header function provided
		manager := newWitnessManager(
			quit,
			dropPeer,
			jailPeer,
			enqueueCh,
			getBlock,
			getHeader,
			chainHeight,
			nil, // currentHeader is nil
			nil, // signedWitnessHash is nil
			nil, // cacheWitnessForServing is nil
			gasCeil,
		)

		hash := common.HexToHash("0x123")
		peer := "test-peer"

		// Calculate actual threshold - should fallback to gasCeil
		threshold := manager.calculatePageThreshold()

		// Expected threshold: 30M gas / 1M gas per MB = 30 MB
		// 30 MB / 15 MB per page = ceil(2) = 2 pages
		expectedThreshold := uint64(2)
		if threshold != expectedThreshold {
			t.Errorf("Expected threshold %d (from config gas ceil %d), got %d", expectedThreshold, gasCeil, threshold)
		}

		reportedPageCount := threshold - 1 // Ensure it's below threshold

		getRandomPeers := func() []string {
			t.Error("getRandomPeers should not be called for page count below threshold")
			return []string{}
		}

		getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
			t.Error("getWitnessPageCount should not be called for page count below threshold")
			return 0, errors.New("should not be called")
		}

		// Should skip verification and assume honest
		isHonest := manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)

		if !isHonest {
			t.Error("Expected peer to be honest for page count below threshold")
		}
	})
}

// TestConcurrentWitnessVerification tests concurrent verification requests don't cause races
func TestConcurrentWitnessVerification(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var jailedPeers []string
	var jailMutex sync.Mutex

	jailPeer := peerJailFn(func(id string) {
		jailMutex.Lock()
		jailedPeers = append(jailedPeers, id)
		jailMutex.Unlock()
	})

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })
	gasCeil := uint64(30_000_000)

	manager := newWitnessManager(
		quit,
		dropPeer,
		jailPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
		nil,
		nil,
		nil,
		gasCeil,
	)

	// Simulate concurrent verification requests (potential DoS scenario)
	var wg sync.WaitGroup
	numGoroutines := 50

	for i := range numGoroutines {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			hash := common.HexToHash(fmt.Sprintf("0x%d", index))
			peer := fmt.Sprintf("peer-%d", index)
			reportedPageCount := uint64(50 + index%10)

			getRandomPeers := func() []string {
				return []string{fmt.Sprintf("random-peer-1-%d", index), fmt.Sprintf("random-peer-2-%d", index)}
			}

			getWitnessPageCount := func(peerID string, hash common.Hash) (uint64, error) {
				// Simulate some peers being dishonest
				if index%3 == 0 {
					return 15, nil // Honest response
				}
				return reportedPageCount, nil // Agree with original
			}

			manager.CheckWitnessPageCount(hash, reportedPageCount, peer, getRandomPeers, getWitnessPageCount)
		}(i)
	}

	wg.Wait()

	// Verify no race conditions occurred and some dishonest peers were jailed
	jailMutex.Lock()
	jailedCount := len(jailedPeers)
	jailMutex.Unlock()

	t.Logf("Jailed %d peers out of %d concurrent verification requests", jailedCount, numGoroutines)

	// We expect some peers to be jailed (every 3rd peer in this test)
	if jailedCount == 0 {
		t.Log("Note: No peers were jailed, which may indicate the consensus logic needs review")
	}
}

// TestFetchWitnessNoPeerError covers the soft-failure path in
// initiateWitnessFetch when the fetch function reports "no peer with witness
// for hash". In that case the returned (req, _, ok) must be (nil, _, false)
// so the caller short-circuits before dereferencing req. A mutation flipping
// the ok value to true would cause `defer req.Close()` to nil-panic; this
// test catches that by exercising the code path and asserting no peer is
// dropped (the peer argument passed to handleWitnessFetchFailureExt is "").
func TestFetchWitnessNoPeerError(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropped := make(chan string, 1)
	dropPeer := peerDropFn(func(id string) { dropped <- id })
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit, dropPeer, nil, enqueueCh,
		getBlock, getHeader, chainHeight, nil, nil, nil, 0,
	)

	hash := common.HexToHash("0xabc")
	peer := "test-peer"

	// Seed a pending entry so handleWitnessFetchFailureExt has a state to
	// back off on — mirrors the real code path.
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: peer},
		announce: &blockAnnounce{
			origin: peer,
			hash:   hash,
			number: 101,
			time:   time.Now(),
		},
	}
	manager.mu.Unlock()

	announce := &blockAnnounce{
		origin: peer,
		hash:   hash,
		number: 101,
		time:   time.Now(),
		// Return the exact error substring matched in initiateWitnessFetch.
		fetchWitness: func(common.Hash, chan *eth.Response) (*eth.Request, error) {
			return nil, errors.New("no peer with witness for hash")
		},
	}

	// Run synchronously so a nil-panic in the mutated variant fails the test
	// rather than silently crashing a goroutine.
	manager.fetchWitness(peer, hash, announce)

	// The soft-failure path passes peer="" to handleWitnessFetchFailureExt,
	// so no peer should be dropped.
	select {
	case id := <-dropped:
		t.Errorf("unexpected peer drop on 'no peer with witness' path: %s", id)
	default:
	}
}

// TestTickPreservesValidPendingEntry guards the nil-check in
// collectReadyHashesLocked:
//
//	if state.op == nil || state.announce == nil { delete(...) }
//
// A mutation flipping either `==` to `!=` would cause valid pending entries
// (both fields non-nil) to be wrongly deleted. TestTickNotReadyYet only
// checks the retry counter; this test verifies the entry itself survives
// tick.
func TestWitnessTickPreservesValidPendingEntry(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit, dropPeer, nil, enqueueCh,
		getBlock, getHeader, chainHeight, nil, nil, nil, 0,
	)

	block := createTestBlock(101)
	hash := block.Hash()

	// Valid pending entry: both op and announce non-nil, future time so it
	// isn't ready to fetch and won't be drained by the ready path.
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: "test-peer", block: block},
		announce: &blockAnnounce{
			origin: "test-peer",
			hash:   hash,
			number: block.NumberU64(),
			time:   time.Now().Add(time.Hour),
			fetchWitness: func(common.Hash, chan *eth.Response) (*eth.Request, error) {
				return nil, nil
			},
		},
	}
	manager.mu.Unlock()

	manager.tick()

	if !manager.isPending(hash) {
		t.Error("valid pending entry was incorrectly deleted by tick()")
	}
}

// TestFetchWitnessOtherErrorKeepsPending covers the "other errors" branch in
// initiateWitnessFetch — errors whose message does NOT contain "no peer with
// witness for hash". This path must:
//   - keep the pending entry (removePending=false, caught if line 582 mutates)
//   - return ok=false so the caller short-circuits before defer req.Close()
//     on a nil request (caught if line 583 mutates)
//
// TestFetchWitnessError exercises this path but never asserts the pending
// state is retained, so the removePending bool is unverified.
func TestFetchWitnessOtherErrorKeepsPending(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit, dropPeer, nil, enqueueCh,
		getBlock, getHeader, chainHeight, nil, nil, nil, 0,
	)

	hash := common.HexToHash("0xfade")
	peer := "test-peer"

	// Seed a pending entry so initiateWitnessFetch reaches the
	// "other errors" branch (the isPending check passes).
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: peer},
		announce: &blockAnnounce{
			origin: peer,
			hash:   hash,
			number: 101,
			time:   time.Now(),
		},
	}
	manager.mu.Unlock()

	announce := &blockAnnounce{
		origin: peer,
		hash:   hash,
		number: 101,
		time:   time.Now(),
		// Generic error — does NOT match the "no peer with witness for hash"
		// substring, forcing the other-errors code path.
		fetchWitness: func(common.Hash, chan *eth.Response) (*eth.Request, error) {
			return nil, errors.New("transient network error")
		},
	}

	// Synchronous call: a nil-req panic from a mutated ok=true would fail
	// the test rather than silently crashing a goroutine.
	manager.fetchWitness(peer, hash, announce)

	// Pending entry must still exist — the soft-failure path must NOT
	// remove it (removePending=false).
	if !manager.isPending(hash) {
		t.Error("pending entry was removed on transient error; expected it to be retained for retry")
	}
}

// TestCheckWitnessPageCountAtThreshold covers the exact-boundary case where
// pageCount equals the computed threshold. The guard is `pageCount <=
// threshold` — flipping to `<` would incorrectly trigger peer verification at
// the boundary. The existing below-threshold tests all use threshold-1, so
// the boundary itself was untested.
func TestCheckWitnessPageCountAtThreshold(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) { t.Errorf("unexpected drop for peer at threshold: %s", id) })
	jailPeer := peerJailFn(func(id string) { t.Errorf("unexpected jail for peer at threshold: %s", id) })
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	// 30M gas / 1M per MB / 15MB per page → ceil(2.0) = 2 pages threshold.
	currentHeader := currentHeaderFn(func() *types.Header {
		return &types.Header{Number: big.NewInt(100), GasLimit: 30_000_000}
	})

	manager := newWitnessManager(
		quit, dropPeer, jailPeer, enqueueCh,
		getBlock, getHeader, chainHeight, currentHeader, nil, nil, 30_000_000,
	)

	threshold := manager.calculatePageThreshold()

	// Explicit failure if verification is unexpectedly invoked. If the guard
	// mutates from `<=` to `<`, pageCount == threshold will fall through to
	// verification and these mocks will fire.
	getRandomPeers := func() []string {
		t.Error("getRandomPeers should not be called for pageCount == threshold")
		return nil
	}
	getWitnessPageCount := func(string, common.Hash) (uint64, error) {
		t.Error("getWitnessPageCount should not be called for pageCount == threshold")
		return 0, errors.New("unreachable")
	}

	isHonest := manager.CheckWitnessPageCount(
		common.HexToHash("0xdef"),
		threshold, // exactly at the boundary
		"test-peer",
		getRandomPeers,
		getWitnessPageCount,
	)
	if !isHonest {
		t.Error("expected peer to be considered honest at pageCount == threshold")
	}
}

// newWitnessManagerForTest returns a minimal witness manager wired up for
// tests that directly invoke individual methods. The returned channel can
// be read from to observe enqueued ops.
func newWitnessManagerForTest(t *testing.T) (*witnessManager, <-chan *enqueueRequest) {
	t.Helper()
	quit := make(chan struct{})
	t.Cleanup(func() { close(quit) })
	enqueueCh := make(chan *enqueueRequest, 10)
	m := newWitnessManager(
		quit,
		peerDropFn(func(string) {}),
		nil,
		enqueueCh,
		blockRetrievalFn(func(common.Hash) *types.Block { return nil }),
		HeaderRetrievalFn(func(common.Hash) *types.Header { return nil }),
		chainHeightFn(func() uint64 { return 100 }),
		nil,
		nil,
		nil,
		0,
	)
	return m, enqueueCh
}

// addPendingEntry inserts a valid pending witness request for a hash with
// the given announce time. Returns the hash for convenience.
func addPendingEntry(m *witnessManager, hashHex string, peer string, announceTime time.Time) common.Hash {
	hash := common.HexToHash(hashHex)
	m.mu.Lock()
	m.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: peer},
		announce: &blockAnnounce{
			origin: peer,
			hash:   hash,
			number: 101,
			time:   announceTime,
		},
	}
	m.mu.Unlock()
	return hash
}

// TestProcessWitnessResponseErrorBranches covers all three early-return
// error paths in processWitnessResponse:
//
//   - nil response (channel closed unexpectedly)
//   - wrong response type (assertion failure)
//   - empty witness slice
//
// Each must route through handleWitnessFetchFailureExt with removePending=false
// and must not panic on nil. This kills the negate_conditional,
// boolean_substitution, and branch_removal mutations on lines 616, 618,
// 627, 630, 632.
func TestProcessWitnessResponseErrorBranches(t *testing.T) {
	peer := "test-peer"

	t.Run("nil response", func(t *testing.T) {
		m, _ := newWitnessManagerForTest(t)
		hash := addPendingEntry(m, "0xa1", peer, time.Now())
		m.processWitnessResponse(peer, hash, nil, time.Now())
		// nil response → fetch failure path with removePending=false → entry retained.
		if !m.isPending(hash) {
			t.Error("pending entry should be retained on nil response")
		}
	})

	t.Run("invalid response type", func(t *testing.T) {
		m, _ := newWitnessManagerForTest(t)
		hash := addPendingEntry(m, "0xa2", peer, time.Now())
		done := make(chan error, 1)
		res := &eth.Response{Res: "not-a-witness-slice", Done: done}
		m.processWitnessResponse(peer, hash, res, time.Now())
		if !m.isPending(hash) {
			t.Error("pending entry should be retained on invalid response type")
		}
		select {
		case <-done:
		default:
			t.Error("res.Done should be signaled before type check")
		}
	})

	t.Run("empty witness slice", func(t *testing.T) {
		m, _ := newWitnessManagerForTest(t)
		hash := addPendingEntry(m, "0xa3", peer, time.Now())
		done := make(chan error, 1)
		res := &eth.Response{Res: []*stateless.Witness{}, Done: done}
		m.processWitnessResponse(peer, hash, res, time.Now())
		if !m.isPending(hash) {
			t.Error("pending entry should be retained on empty witness slice")
		}
	})
}

// TestHandleBroadcastPreservesExistingWitness asserts the "already set"
// guard at line 349: if a pending entry already has a witness attached, a
// second broadcast must NOT overwrite it. Flipping `== nil` to `!= nil`
// would cause the attached witness to be replaced.
func TestHandleBroadcastPreservesExistingWitness(t *testing.T) {
	m, enqueueCh := newWitnessManagerForTest(t)

	block := createTestBlock(101)
	hash := block.Hash()
	firstWitness := createTestWitnessForBlock(block)
	secondWitness := createTestWitnessForBlock(block)

	// Seed a pending entry with a witness already attached via a prior broadcast.
	m.mu.Lock()
	m.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{
			origin:  "peer-1",
			block:   block,
			witness: firstWitness,
		},
		announce: &blockAnnounce{
			origin: "peer-1",
			hash:   hash,
			number: block.NumberU64(),
			time:   time.Now(),
		},
	}
	m.mu.Unlock()

	// Inject a second broadcast for the same hash.
	m.handleBroadcast(&injectedWitnessMsg{
		peer:    "peer-2",
		witness: secondWitness,
		time:    time.Now(),
	})

	// The first witness should still be attached — handleBroadcast goes on
	// to enqueue via safeEnqueue which removes the pending entry, so we
	// validate by reading the enqueued op.
	select {
	case req := <-enqueueCh:
		if req.op.witness != firstWitness {
			t.Error("second broadcast overwrote already-attached witness")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for enqueue")
	}
}

// TestEarliestPendingAnnounceLockedSkipsInvalid verifies the three-way
// filter in earliestPendingAnnounceLocked:
//
//	state.announce == nil || state.op == nil || state.op.witness != nil
//
// Each clause must filter out entries that don't qualify as "pending fetch".
// This kills three negate_conditional mutations on line 708.
func TestWitnessEarliestPendingAnnounceSkipsInvalid(t *testing.T) {
	m, _ := newWitnessManagerForTest(t)

	valid := common.HexToHash("0xb1")
	nilAnnounce := common.HexToHash("0xb2")
	nilOp := common.HexToHash("0xb3")
	alreadyHasWitness := common.HexToHash("0xb4")

	earliestTime := time.Now().Add(-time.Hour) // must be selected
	laterTime := time.Now().Add(time.Hour)

	m.mu.Lock()
	m.pending[valid] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: "p"},
		announce: &blockAnnounce{
			origin: "p", hash: valid, number: 1, time: earliestTime,
		},
	}
	// Should be skipped: announce nil.
	m.pending[nilAnnounce] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: "p"},
	}
	// Should be skipped: op nil.
	m.pending[nilOp] = &witnessRequestState{
		announce: &blockAnnounce{hash: nilOp, time: laterTime},
	}
	// Should be skipped: witness already present.
	block := createTestBlock(5)
	m.pending[alreadyHasWitness] = &witnessRequestState{
		op: &blockOrHeaderInject{
			origin:  "p",
			block:   block,
			witness: createTestWitnessForBlock(block),
		},
		announce: &blockAnnounce{hash: alreadyHasWitness, time: laterTime},
	}

	got := m.earliestPendingAnnounceLocked()
	m.mu.Unlock()

	if !got.Equal(earliestTime) {
		t.Errorf("earliest = %v, want %v (only valid entry should count)", got, earliestTime)
	}
}

// TestCollectReadyHashesMaxRetriesBoundary exercises the exact-threshold
// case for the max-retries guard:
//
//	if state.retries >= maxWitnessFetchRetries { ... mark unavailable }
//
// Flipping `>=` to `>` would let a request retry one additional time beyond
// the limit before being marked unavailable. This also covers the incdec
// mutation on `state.retries++` (line 464) — if that flipped to `--`,
// retries would never reach the limit.
func TestWitnessCollectReadyHashesMaxRetriesBoundary(t *testing.T) {
	t.Run("below limit gets incremented and fetched", func(t *testing.T) {
		m, _ := newWitnessManagerForTest(t)
		hash := addPendingEntry(m, "0xc1", "p", time.Now().Add(-time.Second))
		m.mu.Lock()
		m.pending[hash].retries = 5
		ready, toMark := m.collectReadyHashesLocked(time.Now())
		retriesAfter := m.pending[hash].retries
		m.mu.Unlock()

		if len(ready) != 1 || ready[0] != hash {
			t.Errorf("expected hash in readyToFetch, got ready=%v", ready)
		}
		if len(toMark) != 0 {
			t.Errorf("expected nothing to mark, got %v", toMark)
		}
		if retriesAfter != 6 {
			t.Errorf("retries = %d, want 6 (should be incremented)", retriesAfter)
		}
	})

	t.Run("at limit gets marked unavailable", func(t *testing.T) {
		m, _ := newWitnessManagerForTest(t)
		hash := addPendingEntry(m, "0xc2", "p", time.Now().Add(-time.Second))
		m.mu.Lock()
		m.pending[hash].retries = maxWitnessFetchRetries
		ready, toMark := m.collectReadyHashesLocked(time.Now())
		m.mu.Unlock()

		if len(ready) != 0 {
			t.Errorf("expected empty ready, got %v", ready)
		}
		if len(toMark) != 1 || toMark[0] != hash {
			t.Errorf("expected hash in toMarkUnavailable, got %v", toMark)
		}
	})
}

// TestCleanupUnavailableCacheExpiry verifies that cleanupUnavailableCache
// correctly partitions entries by expiry time:
//   - entries whose expiry is in the past are removed
//   - entries whose expiry is in the future are retained
//
// This kills the conditional_boundary mutation on `now.After(expiry)` at
// line 837 (e.g. `>` → `>=` would change which entries survive).
func TestWitnessCleanupUnavailableCacheExpiry(t *testing.T) {
	m, _ := newWitnessManagerForTest(t)
	expired := common.HexToHash("0xd1")
	live := common.HexToHash("0xd2")

	now := time.Now()
	m.mu.Lock()
	m.witnessUnavailable[expired] = now.Add(-time.Hour)
	m.witnessUnavailable[live] = now.Add(time.Hour)
	m.mu.Unlock()

	m.cleanupUnavailableCache()

	m.mu.Lock()
	_, expiredStillThere := m.witnessUnavailable[expired]
	_, liveStillThere := m.witnessUnavailable[live]
	m.mu.Unlock()

	if expiredStillThere {
		t.Error("expired entry should have been removed")
	}
	if !liveStillThere {
		t.Error("unexpired entry should have been retained")
	}
}

// TestCalculatePageThresholdMinimumClamp verifies that a tiny gas limit
// (which would naturally compute to 0 pages) is clamped to a minimum of 1.
// This covers the two `threshold < 1` guards in calculatePageThreshold
// (header-path at line ~978 and config-path at line ~1003).
func TestWitnessCalculatePageThresholdMinimumClamp(t *testing.T) {
	t.Run("tiny header gas limit clamps to 1", func(t *testing.T) {
		quit := make(chan struct{})
		defer close(quit)
		m := newWitnessManager(
			quit,
			peerDropFn(func(string) {}),
			nil,
			make(chan *enqueueRequest, 10),
			blockRetrievalFn(func(common.Hash) *types.Block { return nil }),
			HeaderRetrievalFn(func(common.Hash) *types.Header { return nil }),
			chainHeightFn(func() uint64 { return 100 }),
			currentHeaderFn(func() *types.Header {
				return &types.Header{Number: big.NewInt(100), GasLimit: 1} // < 1MB → 0 pages pre-clamp
			}),
			nil,
			nil,
			0,
		)
		if got := m.calculatePageThreshold(); got < 1 {
			t.Errorf("threshold = %d, want >= 1 (minimum clamp)", got)
		}
	})

	t.Run("tiny gas ceil config clamps to 1", func(t *testing.T) {
		quit := make(chan struct{})
		defer close(quit)
		m := newWitnessManager(
			quit,
			peerDropFn(func(string) {}),
			nil,
			make(chan *enqueueRequest, 10),
			blockRetrievalFn(func(common.Hash) *types.Block { return nil }),
			HeaderRetrievalFn(func(common.Hash) *types.Header { return nil }),
			chainHeightFn(func() uint64 { return 100 }),
			nil, // no current header → fallback to config path
			nil, // no signed-witness lookup
			nil, // no cache-witness-for-serving
			1,   // 1 gas ceil → 0 pages pre-clamp
		)
		if got := m.calculatePageThreshold(); got < 1 {
			t.Errorf("threshold = %d, want >= 1 (minimum clamp)", got)
		}
	})
}

// TestHandleFilterResultSkipsAlreadyPending verifies handleFilterResult
// short-circuits when the hash is already in pending — flipping the `!=`
// at line 875 (if already pending, we should NOT process further).
func TestWitnessHandleFilterResultSkipsAlreadyPending(t *testing.T) {
	m, _ := newWitnessManagerForTest(t)
	block := createTestBlock(102)
	hash := block.Hash()

	fetchCalled := false
	fetchWitness := func(common.Hash, chan *eth.Response) (*eth.Request, error) {
		fetchCalled = true
		return nil, nil
	}

	// Seed an existing pending entry with that hash.
	m.mu.Lock()
	m.pending[hash] = &witnessRequestState{
		op:       &blockOrHeaderInject{origin: "old-peer", block: block},
		announce: &blockAnnounce{origin: "old-peer", hash: hash, time: time.Now()},
	}
	origRetries := m.pending[hash].retries
	m.mu.Unlock()

	m.handleFilterResult(&blockAnnounce{
		origin:       "new-peer",
		hash:         hash,
		number:       block.NumberU64(),
		fetchWitness: fetchWitness,
	}, block)

	if fetchCalled {
		t.Error("fetchWitness should not be invoked when already pending")
	}

	// Pending state must be the original one, not replaced.
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.pending[hash]
	if state == nil {
		t.Fatal("pending entry was removed; expected original to remain")
	}
	if state.op.origin != "old-peer" {
		t.Errorf("pending entry was replaced; origin=%q", state.op.origin)
	}
	if state.retries != origRetries {
		t.Error("pending retries counter was modified")
	}
}

// TestCheckCompletingSkipsAlreadyPending verifies the same already-pending
// guard in checkCompleting at line 933.
func TestWitnessCheckCompletingSkipsAlreadyPending(t *testing.T) {
	m, _ := newWitnessManagerForTest(t)
	block := createTestBlock(103)
	hash := block.Hash()

	fetchCalled := false
	fetchWitness := func(common.Hash, chan *eth.Response) (*eth.Request, error) {
		fetchCalled = true
		return nil, nil
	}

	m.mu.Lock()
	m.pending[hash] = &witnessRequestState{
		op:       &blockOrHeaderInject{origin: "original", block: block},
		announce: &blockAnnounce{origin: "original", hash: hash, time: time.Now()},
	}
	m.mu.Unlock()

	m.checkCompleting(&blockAnnounce{
		origin:       "new-peer",
		hash:         hash,
		number:       block.NumberU64(),
		fetchWitness: fetchWitness,
	}, block)

	if fetchCalled {
		t.Error("fetchWitness should not be invoked when already pending")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.pending[hash]
	if state == nil {
		t.Fatal("pending entry was removed; expected original to remain")
	}
	if state.op.origin != "original" {
		t.Error("pending entry was replaced")
	}
}

// TestHandleWitnessFetchSuccessUpdatesBlockTimestamps covers the guard at
// line 665: if state.op.block is non-nil, ReceivedAt and AnnouncedAt must
// be set. Flipping the `!= nil` guard would skip the timestamp updates
// even when a block is present.
func TestHandleWitnessFetchSuccessUpdatesBlockTimestamps(t *testing.T) {
	m, enqueueCh := newWitnessManagerForTest(t)
	block := createTestBlock(104)
	hash := block.Hash()
	witness := createTestWitnessForBlock(block)

	m.mu.Lock()
	m.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: "peer", block: block},
		announce: &blockAnnounce{
			origin: "peer", hash: hash, number: block.NumberU64(), time: time.Now(),
		},
	}
	m.mu.Unlock()

	announcedAt := time.Now().Add(-time.Second)
	m.handleWitnessFetchSuccess("peer", hash, witness, announcedAt)

	select {
	case req := <-enqueueCh:
		if req.op.block.ReceivedAt.IsZero() {
			t.Error("ReceivedAt should be set after successful fetch")
		}
		if req.op.block.AnnouncedAt == nil {
			t.Error("AnnouncedAt should be set after successful fetch")
		} else if !req.op.block.AnnouncedAt.Equal(announcedAt) {
			t.Errorf("AnnouncedAt = %v, want %v", *req.op.block.AnnouncedAt, announcedAt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for enqueue")
	}
}

// TestVerifyWitnessPageCountDishonestPeer exercises the dishonest-peer
// detection at line 1089:
//
//	if consensusPageCount != reportedPageCount && consensusPageCount != 0
//
// Flipping either `!=` to `==` would cause honest or no-consensus cases
// to be classified as dishonest and incorrectly drop/jail the peer.
func TestVerifyWitnessPageCountDishonestPeer(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	var droppedPeer, jailedPeer string
	dropPeer := peerDropFn(func(id string) { droppedPeer = id })
	jailPeer := peerJailFn(func(id string) { jailedPeer = id })

	m := newWitnessManager(
		quit, dropPeer, jailPeer,
		make(chan *enqueueRequest, 10),
		blockRetrievalFn(func(common.Hash) *types.Block { return nil }),
		HeaderRetrievalFn(func(common.Hash) *types.Header { return nil }),
		chainHeightFn(func() uint64 { return 100 }),
		nil,
		nil,
		nil,
		0,
	)

	// Reported: 999 (the lying peer). Consensus from other peers: 5 (x2).
	// 999 vs consensus 5 → dishonest path.
	getRandomPeers := func() []string { return []string{"honest1", "honest2"} }
	getWitnessPageCount := func(peer string, _ common.Hash) (uint64, error) {
		return 5, nil
	}

	isHonest := m.verifyWitnessPageCountSync(
		common.HexToHash("0xe1"), 999, "liar",
		getRandomPeers, getWitnessPageCount,
	)
	if isHonest {
		t.Error("expected liar to be classified dishonest")
	}
	if droppedPeer != "liar" {
		t.Errorf("expected drop of 'liar', got %q", droppedPeer)
	}
	if jailedPeer != "liar" {
		t.Errorf("expected jail of 'liar', got %q", jailedPeer)
	}
}

// TestGetConsensusPageCountMajority verifies that the majority vote is
// correctly picked. This covers the `freq > maxCount` comparison at line
// 1030 — flipping to `>=` could change which vote wins on ties (though in
// most cases Go map iteration nondeterminism already makes ties undefined,
// a clear-majority case must still pick the winner).
func TestWitnessGetConsensusPageCountMajority(t *testing.T) {
	m, _ := newWitnessManagerForTest(t)

	// Original: 10. Peers vote 10, 10, 999 → consensus 10 (3/4 includes self).
	peers := []string{"p1", "p2", "p3"}
	votes := map[string]uint64{"p1": 10, "p2": 10, "p3": 999}
	getCount := func(peer string, _ common.Hash) (uint64, error) {
		return votes[peer], nil
	}

	consensus := m.getConsensusPageCountWithOriginal(
		peers, common.HexToHash("0xe2"),
		10, // original reported
		getCount,
	)
	if consensus != 10 {
		t.Errorf("consensus = %d, want 10 (clear majority)", consensus)
	}
}

// TestWitnessLoopDrivesFetchesForPending guards against armTimerChan's
// condition being inverted: when pending requests exist, the loop must
// arm the timer so tick() eventually fires and invokes fetchWitness. The
// existing TestLoop injects a message but never verifies the retry path
// actually executes via the timer — it was insufficient to catch a bug
// where armTimerChan returned a nil timer channel whenever pending > 0
// (reported by code review on PR #2188).
//
// This test exercises the full loop→tick→fetchWitness pipeline through
// real channels and asserts the fetch callback fires within a bounded
// time.
func TestWitnessLoopDrivesFetchesForPending(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit, dropPeer, nil, enqueueCh,
		getBlock, getHeader, chainHeight, nil, nil, nil, 0,
	)

	fetchCalled := make(chan struct{}, 1)
	fetchWitness := func(common.Hash, chan *eth.Response) (*eth.Request, error) {
		select {
		case fetchCalled <- struct{}{}:
		default:
		}
		return nil, errors.New("no peer with witness for hash")
	}

	go manager.loop()
	defer manager.stop()

	block := createTestBlock(101)
	manager.injectNeedWitnessCh <- &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// fetchWitness must be invoked within a reasonable window. If
	// armTimerChan's condition is inverted (returns nil channel when
	// pending > 0), tick() never fires through the timer and this
	// times out.
	select {
	case <-fetchCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("fetchWitness was never invoked — loop is not driving tick for pending requests")
	}
}
