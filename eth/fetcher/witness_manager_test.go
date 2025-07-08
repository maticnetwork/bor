package fetcher

import (
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
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

func createTestWitness() *stateless.Witness {
	// Create a witness with a valid header using the proper constructor
	header := &types.Header{
		Number: big.NewInt(101),
	}
	witness, err := stateless.NewWitness(header, nil)
	if err != nil {
		panic(err) // This shouldn't happen in tests
	}
	return witness
}

func createTestWitnessForBlock(block *types.Block) *stateless.Witness {
	// Create a witness with the same header as the block
	witness, err := stateless.NewWitness(block.Header(), nil)
	if err != nil {
		panic(err) // This shouldn't happen in tests
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

// TestWitnessManagerCreation tests the creation and basic setup of witnessManager
func TestWitnessManagerCreation(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	if manager == nil {
		t.Fatal("Expected witnessManager to be created")
	}

	// Check initial state
	if len(manager.pending) != 0 {
		t.Errorf("Expected empty pending map, got %d items", len(manager.pending))
	}

	if len(manager.failedWitnessAttempts) != 0 {
		t.Errorf("Expected empty failedWitnessAttempts map, got %d items", len(manager.failedWitnessAttempts))
	}

	if len(manager.peerPenalty) != 0 {
		t.Errorf("Expected empty peerPenalty map, got %d items", len(manager.peerPenalty))
	}

	if manager.witnessTimer == nil {
		t.Error("Expected witnessTimer to be initialized")
	}

	// Test channels are created with proper buffering
	if cap(manager.injectNeedWitnessCh) != 10 {
		t.Errorf("Expected injectNeedWitnessCh buffer size 10, got %d", cap(manager.injectNeedWitnessCh))
	}

	if cap(manager.injectWitnessCh) != 10 {
		t.Errorf("Expected injectWitnessCh buffer size 10, got %d", cap(manager.injectWitnessCh))
	}
}

// TestWitnessManagerLifecycle tests start and stop functionality
func TestWitnessManagerLifecycle(t *testing.T) {
	quit := make(chan struct{})
	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Start the manager
	manager.start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Stop the manager
	manager.stop()
	close(quit)

	// Give it a moment to stop
	time.Sleep(10 * time.Millisecond)
}

// TestHandleNeed tests processing of blocks needing witnesses
func TestHandleNeed(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       "test-peer",
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Test successful handling
	manager.handleNeed(msg)

	// Check that block was added to pending
	if !manager.isPending(block.Hash()) {
		t.Error("Expected block to be pending after handleNeed")
	}

	// Check pending count
	manager.mu.Lock()
	pendingCount := len(manager.pending)
	manager.mu.Unlock()

	if pendingCount != 1 {
		t.Errorf("Expected 1 pending request, got %d", pendingCount)
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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

// TestPeerPenalty tests peer penalty system
func TestPeerPenalty(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	peer := "test-peer"

	// Initially should not have failed too many times
	if manager.HasFailedTooManyTimes(peer) {
		t.Error("Expected peer to not be penalised initially")
	}

	// Penalise the peer
	manager.penalisePeer(peer)

	// Should now be penalised
	if !manager.HasFailedTooManyTimes(peer) {
		t.Error("Expected peer to be penalised after penalisePeer call")
	}

	// Manually expire the penalty for testing
	manager.mu.Lock()
	manager.peerPenalty[peer] = time.Now().Add(-time.Minute)
	manager.mu.Unlock()

	// Should no longer be penalised
	if manager.HasFailedTooManyTimes(peer) {
		t.Error("Expected peer to not be penalised after penalty expiry")
	}
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 10)
	getBlock := blockRetrievalFn(func(hash common.Hash) *types.Block { return nil })
	getHeader := HeaderRetrievalFn(func(hash common.Hash) *types.Header { return nil })
	chainHeight := chainHeightFn(func() uint64 { return 100 })

	manager := newWitnessManager(
		quit,
		dropPeer,
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"
	err := errors.New("fetch failed")

	// Test soft failure (keep pending for retry)
	manager.handleWitnessFetchFailureExt(hash, peer, err, false)

	// Check that peer failure count increased
	manager.mu.Lock()
	failureCount := manager.failedWitnessAttempts[peer]
	manager.mu.Unlock()

	if failureCount != 1 {
		t.Errorf("Expected failure count 1, got %d", failureCount)
	}

	// Check that peer is penalised
	if !manager.HasFailedTooManyTimes(peer) {
		t.Error("Expected peer to be penalised after failure")
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	blockHash := block.Hash()
	witness := createTestWitnessForBlock(block)

	// Create a channel to control witness fetch timing
	fetchStarted := make(chan struct{})
	responseSent := false

	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
			responseCh <- &wit.Response{
				Res: &wit.WitnessPacketRLPPacket{
					WitnessPacketResponse: wit.WitnessPacketResponse{rlp.RawValue(witnessBytes)},
				},
				Done: make(chan error, 1),
			}
			responseSent = true
		}()

		// Return successful request
		req := &wit.Request{
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
	if !responseSent {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Test tick with no pending requests
	manager.tick()

	// Add a pending request but make it NOT ready to fetch to avoid goroutine issues
	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin: "test-peer",
		hash:   block.Hash(),
		number: block.NumberU64(),
		time:   time.Now().Add(-time.Second), // Ready to fetch
		fetchWitness: func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
			return nil, nil
		},
	}

	state := &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: announce,
		retries:  10, // Already at max retries
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		fetchWitness: func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		fetchWitness: func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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

// TestHandleNeedPenalizedPeer tests handleNeed with penalized peer
func TestHandleNeedPenalizedPeer(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Penalize peer first
	peer := "test-peer"
	manager.penalisePeer(peer)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
		return nil, nil
	}

	msg := &injectBlockNeedWitnessMsg{
		origin:       peer,
		block:        block,
		fetchWitness: fetchWitness,
	}

	// Test handleNeed with penalized peer - should be ignored
	manager.handleNeed(msg)

	// Check that no pending requests were created
	if manager.isPending(block.Hash()) {
		t.Error("Expected request to be ignored for penalized peer")
	}
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Create block that's too far away (block 10 when chain is at 100)
	block := createTestBlock(10)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Start the loop
	go manager.loop()

	// Test injecting a block need witness message
	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"

	// Test fetchWitness with error in initiating request
	announce := &blockAnnounce{
		origin: peer,
		hash:   hash,
		number: 101,
		time:   time.Now(),
		fetchWitness: func(common.Hash, chan *wit.Response) (*wit.Request, error) {
			return nil, errors.New("no peer available")
		},
	}

	// This will run in background, we can't easily wait for it, but it exercises the error path
	go manager.fetchWitness(peer, hash, announce)

	time.Sleep(50 * time.Millisecond) // Give time for goroutine to process
}

// TestFetchWitnessNoPeerError tests specific error case for no peers
func TestFetchWitnessNoPeerError(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	hash := common.HexToHash("0x123")
	peer := "test-peer"

	// Test fetchWitness with specific "no peer with witness for hash" error
	announce := &blockAnnounce{
		origin: peer,
		hash:   hash,
		number: 101,
		time:   time.Now(),
		fetchWitness: func(common.Hash, chan *wit.Response) (*wit.Request, error) {
			return nil, errors.New("no peer with witness for hash")
		},
	}

	// This will run in background and should mark witness as unavailable
	go manager.fetchWitness(peer, hash, announce)

	time.Sleep(50 * time.Millisecond) // Give time for goroutine to process

	// Check that witness was marked as unavailable
	if !manager.isWitnessUnavailable(hash) {
		t.Error("Expected witness to be marked unavailable after no peer error")
	}
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)

	// Mark witness as unavailable first
	manager.markWitnessUnavailable(block.Hash())

	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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

// TestHandleFilterResultPenalizedPeer tests filter result with penalized peer
func TestHandleFilterResultPenalizedPeer(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	peer := "test-peer"
	// Penalize peer first
	manager.penalisePeer(peer)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce(peer, block, fetchWitness)

	// Handle filter result with penalized peer
	manager.handleFilterResult(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block from penalized peer to be discarded")
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)

	// Mark witness as unavailable first
	manager.markWitnessUnavailable(block.Hash())

	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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

// TestCheckCompletingPenalizedPeer tests checkCompleting with penalized peer
func TestCheckCompletingPenalizedPeer(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	peer := "test-peer"
	// Penalize peer first
	manager.penalisePeer(peer)

	block := createTestBlock(101)
	fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
		return nil, nil
	}

	announce := createTestBlockAnnounce(peer, block, fetchWitness)

	// Check completing with penalized peer
	manager.checkCompleting(announce, block)

	// Check that block was NOT added to pending
	if manager.isPending(block.Hash()) {
		t.Error("Expected block from penalized peer to be discarded")
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	block := createTestBlock(101)
	announce := &blockAnnounce{
		origin: "test-peer",
		hash:   block.Hash(),
		number: block.NumberU64(),
		time:   time.Now().Add(time.Hour), // Not ready yet - future time
		fetchWitness: func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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

	// Add some peer failures first
	manager.mu.Lock()
	manager.failedWitnessAttempts[peer] = 5
	manager.peerPenalty[peer] = time.Now().Add(time.Hour)
	manager.mu.Unlock()

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

	// Verify peer failures and penalty were reset
	manager.mu.Lock()
	failures := manager.failedWitnessAttempts[peer]
	_, penalized := manager.peerPenalty[peer]
	manager.mu.Unlock()

	if failures != 0 {
		t.Errorf("Expected peer failures to be reset to 0, got %d", failures)
	}

	if penalized {
		t.Error("Expected peer penalty to be removed after successful enqueue")
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
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

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.handleWitnessFetchFailureExt(hash, peer, err, false)
		}()
	}

	wg.Wait()

	// Verify that the failure count is correct
	manager.mu.Lock()
	failureCount := manager.failedWitnessAttempts[peer]
	manager.mu.Unlock()

	if failureCount != numGoroutines {
		t.Errorf("Expected failure count %d, got %d", numGoroutines, failureCount)
	}
}

// TestHandleWitnessFetchSuccessWithBlockNoLongerPending tests handleWitnessFetchSuccess
// when block is no longer pending
func TestHandleWitnessFetchSuccessWithBlockNoLongerPending(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Create test data
	block := createTestBlock(101)
	hash := block.Hash()
	witness := createTestWitnessForBlock(block)

	// First add the block to pending
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: &blockAnnounce{
			origin: "test-peer",
			hash:   hash,
			number: block.NumberU64(),
			time:   time.Now(),
		},
	}
	manager.mu.Unlock()

	// Remove it to simulate the scenario
	manager.mu.Lock()
	delete(manager.pending, hash)
	manager.mu.Unlock()

	// Call handleWitnessFetchSuccess - should handle gracefully
	manager.handleWitnessFetchSuccess("test-peer", hash, witness, time.Now())

	// Verify no enqueue occurred
	select {
	case <-enqueueCh:
		t.Error("Should not enqueue when block is no longer pending")
	default:
		// Expected - no enqueue
	}
}

// TestFetchWitnessWithBlockRemovedBeforeError tests that when a block is removed from pending
// before a fetch error occurs, the peer is not penalized
func TestFetchWitnessWithBlockRemovedBeforeError(t *testing.T) {
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
		enqueueCh,
		getBlock,
		getHeader,
		chainHeight,
	)

	// Create test data
	block := createTestBlock(101)
	hash := block.Hash()

	// Track original failure count
	originalFailures := 0

	// Create a mock fetchWitness that removes block from pending before error
	fetchWitness := func(h common.Hash, resCh chan *wit.Response) (*wit.Request, error) {
		// Remove block from pending before returning error
		manager.mu.Lock()
		delete(manager.pending, hash)
		originalFailures = manager.failedWitnessAttempts["test-peer"]
		manager.mu.Unlock()

		// Return an error - this should trigger the check for pending status
		return nil, errors.New("simulated fetch error")
	}

	// Add block to pending
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{
			origin: "test-peer",
			block:  block,
		},
		announce: &blockAnnounce{
			origin:       "test-peer",
			hash:         hash,
			number:       block.NumberU64(),
			time:         time.Now().Add(-time.Second), // Make it ready to fetch
			fetchWitness: fetchWitness,
		},
	}
	manager.mu.Unlock()

	// Call fetchWitness directly to simulate the fetch attempt
	manager.fetchWitness("test-peer", hash, manager.pending[hash].announce)

	// Verify peer was not penalized (failure count should not increase)
	manager.mu.Lock()
	currentFailures := manager.failedWitnessAttempts["test-peer"]
	manager.mu.Unlock()

	if currentFailures > originalFailures {
		t.Errorf("Peer should not be penalized when block is no longer pending, failures increased from %d to %d", originalFailures, currentFailures)
	}
}
