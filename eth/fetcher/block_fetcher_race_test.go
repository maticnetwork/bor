package fetcher

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/trie"
)

// TestBlockFetcherConcurrentMapAccess tests that BlockFetcher handles concurrent
// map access without panics, particularly testing the race conditions that
// were previously present between the main loop and goroutines.
func TestBlockFetcherConcurrentMapAccess(t *testing.T) {
	// Create a BlockFetcher with necessary dependencies
	var (
		drops     []string
		dropMutex sync.Mutex
	)

	getHeader := func(hash common.Hash) *types.Header { return nil }
	getBlock := func(hash common.Hash) *types.Block { return nil }
	verifyHeader := func(header *types.Header) error { return nil }
	broadcastBlock := func(block *types.Block, witness *stateless.Witness, propagate bool) {}
	chainHeight := func() uint64 { return 100 }
	insertHeaders := func(headers []*types.Header) (int, error) { return len(headers), nil }
	insertChain := func(blocks types.Blocks, witnesses []*stateless.Witness) (int, error) {
		return len(blocks), nil
	}
	dropPeer := func(id string) {
		dropMutex.Lock()
		drops = append(drops, id)
		dropMutex.Unlock()
	}

	fetcher := NewBlockFetcher(
		false, // not light mode
		getHeader,
		getBlock,
		verifyHeader,
		broadcastBlock,
		chainHeight,
		insertHeaders,
		insertChain,
		dropPeer,
		false, // no block tracking
		false, // no witness requirement
	)

	// Start the fetcher
	fetcher.Start()
	defer fetcher.Stop()

	// Number of concurrent goroutines to stress test
	const numGoroutines = 50
	const numOperationsPerGoroutine = 20

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test blocks
	blocks := make([]*types.Block, numGoroutines*numOperationsPerGoroutine)
	for i := range blocks {
		header := &types.Header{
			Number: big.NewInt(int64(101 + i)),
		}
		blocks[i] = types.NewBlock(header, nil, nil, trie.NewStackTrie(nil))
	}

	// Goroutine 1: Concurrent Notifications
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]
			peer := "peer-notify-" + randomString(5)

			// Mock fetchers
			headerFetcher := func(hash common.Hash, respCh chan *eth.Response) (*eth.Request, error) {
				return &eth.Request{}, nil
			}
			bodyFetcher := func(hashes []common.Hash, respCh chan *eth.Response) (*eth.Request, error) {
				return &eth.Request{}, nil
			}
			witnessFetcher := func(hash common.Hash, respCh chan *wit.Response) (*wit.Request, error) {
				return &wit.Request{}, nil
			}

			err := fetcher.Notify(
				peer,
				block.Hash(),
				block.NumberU64(),
				time.Now(),
				headerFetcher,
				bodyFetcher,
				witnessFetcher,
			)
			if err != nil && err != errTerminated {
				t.Errorf("Unexpected error in Notify: %v", err)
			}

			// Random small delay to increase chance of race conditions
			time.Sleep(time.Microsecond * time.Duration(rand.Intn(100)))
		}
	}()

	// Goroutine 2: Concurrent Enqueues
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]
			peer := "peer-enqueue-" + randomString(5)

			err := fetcher.Enqueue(peer, block)
			if err != nil && err != errTerminated {
				t.Errorf("Unexpected error in Enqueue: %v", err)
			}

			time.Sleep(time.Microsecond * time.Duration(rand.Intn(100)))
		}
	}()

	// Goroutine 3: Concurrent Header Filtering
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]
			peer := "peer-filter-" + randomString(5)

			headers := []*types.Header{block.Header()}
			filtered := fetcher.FilterHeaders(peer, headers, time.Now(), time.Now())
			_ = filtered // Ignore result

			time.Sleep(time.Microsecond * time.Duration(rand.Intn(100)))
		}
	}()

	// Goroutine 4: Concurrent Body Filtering
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			peer := "peer-body-" + randomString(5)

			transactions := [][]*types.Transaction{{}}
			uncles := [][]*types.Header{{}}
			filteredTxs, filteredUncles := fetcher.FilterBodies(peer, transactions, uncles, time.Now(), time.Now())
			_, _ = filteredTxs, filteredUncles // Ignore results

			time.Sleep(time.Microsecond * time.Duration(rand.Intn(100)))
		}
	}()

	// Goroutine 5: Random operations to trigger internal state changes
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			// Trigger done channel (simulating import completion)
			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]

			select {
			case fetcher.done <- block.Hash():
			case <-time.After(time.Millisecond):
				// Don't block if channel is full
			}

			time.Sleep(time.Microsecond * time.Duration(rand.Intn(100)))
		}
	}()

	// Wait for all goroutines to complete or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines completed successfully
	case <-ctx.Done():
		t.Fatal("Test timed out - possible deadlock or race condition")
	}

	// Give a short time for any remaining async operations to complete
	time.Sleep(100 * time.Millisecond)
}

// TestWitnessManagerConcurrentAccess tests concurrent access to WitnessManager maps
func TestWitnessManagerConcurrentAccess(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 100)
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

	// Start the witness manager
	manager.start()

	const numGoroutines = 30
	const numOperationsPerGoroutine = 15

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create test blocks
	blocks := make([]*types.Block, numGoroutines*numOperationsPerGoroutine)
	for i := range blocks {
		header := &types.Header{
			Number: big.NewInt(int64(101 + i)),
		}
		blocks[i] = types.NewBlock(header, nil, nil, trie.NewStackTrie(nil))
	}

	// Goroutine 1: Concurrent handleNeed calls via channel
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]
			peer := "peer-need-" + randomString(5)

			fetchWitness := func(hash common.Hash, responseCh chan *wit.Response) (*wit.Request, error) {
				return &wit.Request{}, nil
			}

			msg := &injectBlockNeedWitnessMsg{
				origin:       peer,
				block:        block,
				fetchWitness: fetchWitness,
			}

			select {
			case manager.injectNeedWitnessCh <- msg:
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
				// Don't block if channel is full
			}
			time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))
		}
	}()

	// Goroutine 2: Concurrent handleBroadcast calls via channel
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]
			peer := "peer-broadcast-" + randomString(5)

			witness, err := stateless.NewWitness(block.Header(), nil)
			if err != nil {
				continue
			}

			msg := &injectedWitnessMsg{
				peer:    peer,
				witness: witness,
				time:    time.Now(),
			}

			select {
			case manager.injectWitnessCh <- msg:
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
				// Don't block if channel is full
			}
			time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))
		}
	}()

	// Goroutine 3: Concurrent forget calls
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]

			manager.forget(block.Hash())
			time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))
		}
	}()

	// Goroutine 4: Concurrent isPending calls
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]

			_ = manager.isPending(block.Hash())
			time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))
		}
	}()

	// Goroutine 5: Concurrent peer penalty operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			peer := "peer-penalty-" + randomString(5)

			// Randomly penalize or check penalty status
			if rand.Intn(2) == 0 {
				manager.penalisePeer(peer)
			} else {
				_ = manager.HasFailedTooManyTimes(peer)
			}

			time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))
		}
	}()

	// Goroutine 6: Concurrent witness unavailable operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOperationsPerGoroutine; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			blockIdx := rand.Intn(len(blocks))
			block := blocks[blockIdx]

			// Randomly mark unavailable or check availability
			if rand.Intn(2) == 0 {
				manager.markWitnessUnavailable(block.Hash())
			} else {
				_ = manager.isWitnessUnavailable(block.Hash())
			}

			time.Sleep(time.Microsecond * time.Duration(rand.Intn(50)))
		}
	}()

	// Goroutine 7: Concurrent cleanup operations
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ { // Fewer cleanup operations
			select {
			case <-ctx.Done():
				return
			default:
			}

			manager.cleanupUnavailableCache()
			time.Sleep(time.Millisecond * time.Duration(rand.Intn(100)))
		}
	}()

	// Wait for all goroutines to complete or timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines completed successfully
	case <-ctx.Done():
		t.Fatal("Test timed out - possible deadlock or race condition")
	}

	// Give a short time for any remaining async operations to complete
	time.Sleep(100 * time.Millisecond)
}

// TestBlockFetcherMapStateConsistency tests that map operations maintain consistency
func TestBlockFetcherMapStateConsistency(t *testing.T) {
	getHeader := func(hash common.Hash) *types.Header { return nil }
	getBlock := func(hash common.Hash) *types.Block { return nil }
	verifyHeader := func(header *types.Header) error { return nil }
	broadcastBlock := func(block *types.Block, witness *stateless.Witness, propagate bool) {}
	chainHeight := func() uint64 { return 100 }
	insertHeaders := func(headers []*types.Header) (int, error) { return len(headers), nil }
	insertChain := func(blocks types.Blocks, witnesses []*stateless.Witness) (int, error) {
		return len(blocks), nil
	}
	dropPeer := func(id string) {}

	fetcher := NewBlockFetcher(
		false,
		getHeader,
		getBlock,
		verifyHeader,
		broadcastBlock,
		chainHeight,
		insertHeaders,
		insertChain,
		dropPeer,
		false,
		false,
	)

	fetcher.Start()
	defer fetcher.Stop()

	// Test that forgetHash properly cleans up all related maps
	header := &types.Header{Number: big.NewInt(101)}
	block := types.NewBlock(header, nil, nil, trie.NewStackTrie(nil))
	hash := block.Hash()
	peer := "test-peer"

	// Add to various maps manually (simulating internal state)
	fetcher.mu.Lock()
	fetcher.announces[peer] = 1
	fetcher.announced[hash] = []*blockAnnounce{{origin: peer, hash: hash}}
	fetcher.fetching[hash] = &blockAnnounce{origin: peer, hash: hash}
	fetcher.fetched[hash] = []*blockAnnounce{{origin: peer, hash: hash}}
	fetcher.completing[hash] = &blockAnnounce{origin: peer, hash: hash}
	fetcher.mu.Unlock()

	// Call forgetHash
	fetcher.forgetHash(hash)

	// Verify all maps are cleaned up
	fetcher.mu.RLock()
	if _, ok := fetcher.announced[hash]; ok {
		t.Error("announced map not cleaned up by forgetHash")
	}
	if _, ok := fetcher.fetching[hash]; ok {
		t.Error("fetching map not cleaned up by forgetHash")
	}
	if _, ok := fetcher.fetched[hash]; ok {
		t.Error("fetched map not cleaned up by forgetHash")
	}
	if _, ok := fetcher.completing[hash]; ok {
		t.Error("completing map not cleaned up by forgetHash")
	}
	fetcher.mu.RUnlock()
}

// TestWitnessManagerStateConsistency tests WitnessManager state consistency
func TestWitnessManagerStateConsistency(t *testing.T) {
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
	hash := block.Hash()

	// Test that forget properly cleans up pending state
	manager.mu.Lock()
	manager.pending[hash] = &witnessRequestState{
		op: &blockOrHeaderInject{origin: "test-peer", block: block},
	}
	manager.mu.Unlock()

	if !manager.isPending(hash) {
		t.Error("Expected block to be pending before forget")
	}

	manager.forget(hash)

	if manager.isPending(hash) {
		t.Error("Expected block to not be pending after forget")
	}

	// Test peer penalty consistency
	peer := "test-peer"
	manager.penalisePeer(peer)

	if !manager.HasFailedTooManyTimes(peer) {
		t.Error("Expected peer to be penalized")
	}

	// Manually expire penalty
	manager.mu.Lock()
	manager.peerPenalty[peer] = time.Now().Add(-time.Hour)
	manager.mu.Unlock()

	if manager.HasFailedTooManyTimes(peer) {
		t.Error("Expected peer penalty to be expired")
	}
}

// Helper function to generate random strings for peer IDs
func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// TestBlockFetcherMemoryLeaks tests that maps don't grow indefinitely
func TestBlockFetcherMemoryLeaks(t *testing.T) {
	getHeader := func(hash common.Hash) *types.Header { return nil }
	getBlock := func(hash common.Hash) *types.Block { return nil }
	verifyHeader := func(header *types.Header) error { return nil }
	broadcastBlock := func(block *types.Block, witness *stateless.Witness, propagate bool) {}
	chainHeight := func() uint64 { return 100 }
	insertHeaders := func(headers []*types.Header) (int, error) { return len(headers), nil }
	insertChain := func(blocks types.Blocks, witnesses []*stateless.Witness) (int, error) {
		return len(blocks), nil
	}
	dropPeer := func(id string) {}

	fetcher := NewBlockFetcher(
		false,
		getHeader,
		getBlock,
		verifyHeader,
		broadcastBlock,
		chainHeight,
		insertHeaders,
		insertChain,
		dropPeer,
		false,
		false,
	)

	fetcher.Start()
	defer fetcher.Stop()

	// Add and remove many blocks to test memory cleanup
	for i := 0; i < 1000; i++ {
		header := &types.Header{Number: big.NewInt(int64(101 + i))}
		block := types.NewBlock(header, nil, nil, trie.NewStackTrie(nil))
		hash := block.Hash()

		// Enqueue and immediately mark as done to trigger cleanup
		err := fetcher.Enqueue("test-peer", block)
		if err != nil && err != errTerminated {
			t.Errorf("Unexpected error in Enqueue: %v", err)
		}

		// Trigger cleanup via done channel
		select {
		case fetcher.done <- hash:
		case <-time.After(time.Millisecond):
		}
	}

	// Give time for cleanup
	time.Sleep(100 * time.Millisecond)

	// Check that maps aren't growing unbounded
	fetcher.mu.RLock()
	totalMapSize := len(fetcher.announced) + len(fetcher.fetching) +
		len(fetcher.fetched) + len(fetcher.completing) + len(fetcher.queued)
	fetcher.mu.RUnlock()

	if totalMapSize > 100 {
		t.Errorf("Maps may be leaking memory, total size: %d", totalMapSize)
	}
}

// TestWitnessManagerMemoryLeaks tests WitnessManager memory cleanup
func TestWitnessManagerMemoryLeaks(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 100)
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

	// Add and remove many entries to test cleanup
	for i := 0; i < 1000; i++ {
		header := &types.Header{Number: big.NewInt(int64(101 + i))}
		block := types.NewBlock(header, nil, nil, trie.NewStackTrie(nil))
		hash := block.Hash()

		// Mark as unavailable and then forget
		manager.markWitnessUnavailable(hash)
		manager.forget(hash)

		// Penalize peer and then reset
		peer := "test-peer-" + randomString(5)
		manager.penalisePeer(peer)
		manager.mu.Lock()
		delete(manager.peerPenalty, peer)
		delete(manager.failedWitnessAttempts, peer)
		manager.mu.Unlock()
	}

	// Run cleanup
	manager.cleanupUnavailableCache()

	// Check map sizes
	manager.mu.Lock()
	totalMapSize := len(manager.pending) + len(manager.failedWitnessAttempts) +
		len(manager.peerPenalty) + len(manager.witnessUnavailable)
	manager.mu.Unlock()

	if totalMapSize > 1000 {
		t.Errorf("WitnessManager maps may be leaking memory, total size: %d", totalMapSize)
	}
}

// TestWitnessManagerMapAccessRace specifically tests the race condition
// that was fixed in handleWitnessFetchFailureExt where map access occurred
// after the mutex was unlocked
func TestWitnessManagerMapAccessRace(t *testing.T) {
	quit := make(chan struct{})
	defer close(quit)

	dropPeer := peerDropFn(func(id string) {})
	enqueueCh := make(chan *enqueueRequest, 100)
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
	go manager.loop()
	defer manager.stop()

	// Create test data
	hash := common.HexToHash("0x123")
	peer1 := "test-peer-1"
	peer2 := "test-peer-2"
	err := fmt.Errorf("fetch failed")

	// Run intensive concurrent operations that previously caused
	// the race condition panic
	var wg sync.WaitGroup
	numGoroutines := 50
	operationsPerGoroutine := 20

	// Goroutine 1: Concurrent failure handling (the problematic function)
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			peer := peer1
			if idx%2 == 0 {
				peer = peer2
			}

			for j := 0; j < operationsPerGoroutine; j++ {
				// This call previously had a race condition
				manager.handleWitnessFetchFailureExt(hash, peer, err, false)

				// Add some variability to increase chance of race
				if j%3 == 0 {
					time.Sleep(time.Microsecond)
				}
			}
		}(i)
	}

	// Goroutine 2: Concurrent penalty checks (accessing the same maps)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			peer := peer1
			if idx%2 == 0 {
				peer = peer2
			}

			for j := 0; j < operationsPerGoroutine*2; j++ {
				// This accesses the same maps that the failure handler modifies
				_ = manager.HasFailedTooManyTimes(peer)

				if j%5 == 0 {
					time.Sleep(time.Microsecond)
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify that the maps are in a consistent state
	manager.mu.Lock()
	failures1 := manager.failedWitnessAttempts[peer1]
	failures2 := manager.failedWitnessAttempts[peer2]
	manager.mu.Unlock()

	expectedTotal := numGoroutines/2*operationsPerGoroutine + (numGoroutines-numGoroutines/2)*operationsPerGoroutine
	actualTotal := failures1 + failures2

	if actualTotal != expectedTotal {
		t.Errorf("Expected total failures %d, got %d (peer1: %d, peer2: %d)",
			expectedTotal, actualTotal, failures1, failures2)
	}
}
