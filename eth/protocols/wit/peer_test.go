package wit

import (
	"crypto/rand"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

func setupPeer() *Peer {
	logger := log.New()
	var id enode.ID
	rand.Read(id[:])
	p2pPeer := p2p.NewPeer(id, "test-peer", nil)
	// Create a pipe and start a background reader to prevent blocking
	app, net := p2p.MsgPipe()
	// Start background reader to prevent WriteMsg from blocking
	go func() {
		for {
			msg, err := app.ReadMsg()
			if err != nil {
				return
			}
			msg.Discard()
		}
	}()
	return NewPeer(1, p2pPeer, net, logger)
}

var testHeader1 = &types.Header{
	Number: big.NewInt(10),
}

var testHeader2 = &types.Header{
	Number: big.NewInt(15),
}

var testHeader3 = &types.Header{
	Number: big.NewInt(16),
}

// Create context headers for each witness (these will be used for witness.Header().Hash())
var testContextHeader1 = &types.Header{
	Number: big.NewInt(11), // Context should be the block the witness is for
}

var testContextHeader2 = &types.Header{
	Number: big.NewInt(17), // Different from testContextHeader1
}

var testContextHeader3 = &types.Header{
	Number: big.NewInt(18), // Different from both above
}

func createWitness(context *types.Header, headers []*types.Header) *stateless.Witness {
	// Create a new witness with the context and set the headers
	w, _ := stateless.NewWitness(context, nil, false)
	w.Headers = headers
	return w
}

var testWitness1 = createWitness(testContextHeader1, []*types.Header{testHeader1})
var testWitness2 = createWitness(testContextHeader2, []*types.Header{testHeader2, testHeader3})
var testWitness3 = createWitness(testContextHeader3, []*types.Header{testHeader1, testHeader2, testHeader3})

func TestAddKnownWitness(t *testing.T) {
	peer := setupPeer()

	peer.AddKnownWitness(testWitness1.Header().Hash())
	assert.True(t, peer.KnownWitnessesContains(testWitness1), "Witness should be known by the peer")
	assert.Equal(t, 1, peer.knownWitnesses.Cardinality(), "Known witnesses count should be 1")

	peer.AddKnownWitness(testWitness2.Header().Hash())
	assert.True(t, peer.KnownWitnessesContains(testWitness2), "Witness should be known by the peer")
	assert.Equal(t, 2, peer.knownWitnesses.Cardinality(), "Known witnesses count should be 2")

	peer.AddKnownWitness(testWitness3.Header().Hash())
	assert.True(t, peer.KnownWitnessesContains(testWitness3), "Witness should be known by the peer")

	// The witnesses now have different context headers, so they have different hashes
	// even though testWitness1 and testWitness3 share some of the same headers in their Headers slice
	assert.Equal(t, 3, peer.knownWitnesses.Cardinality(), "Known witnesses count should be 3")
}

// setupPeerWithPipe creates a peer with a message pipe for testing message sending
func setupPeerWithPipe() (*Peer, *p2p.MsgPipeRW, *p2p.MsgPipeRW) {
	logger := log.New()
	var id enode.ID
	rand.Read(id[:])
	p2pPeer := p2p.NewPeer(id, "test-peer", nil)
	app, net := p2p.MsgPipe()
	peer := NewPeer(1, p2pPeer, net, logger)
	return peer, app, net
}

// TestRequestWitnessMetadata tests the RequestWitnessMetadata function
func TestRequestWitnessMetadata(t *testing.T) {
	t.Run("SingleHash", func(t *testing.T) {
		peer := setupPeer()
		defer peer.Close()

		hash := common.Hash{0x01, 0x02, 0x03}
		sink := make(chan *Response, 1)

		// Request witness metadata
		req, err := peer.RequestWitnessMetadata([]common.Hash{hash}, sink)
		assert.NoError(t, err, "RequestWitnessMetadata should not return error")
		assert.NotNil(t, req, "Request should not be nil")
		assert.Equal(t, uint64(GetWitnessMetadataMsg), req.code, "Request code should be GetWitnessMetadataMsg")
		assert.Equal(t, uint64(WitnessMetadataMsg), req.want, "Request want should be WitnessMetadataMsg")

		// Verify request data structure
		packetData, ok := req.data.(*GetWitnessMetadataPacket)
		assert.True(t, ok, "Request data should be *GetWitnessMetadataPacket")
		assert.Equal(t, req.id, packetData.RequestId, "Request ID should match in data")
		assert.Equal(t, 1, len(packetData.Hashes), "Should have one hash in request data")
		assert.Equal(t, hash, packetData.Hashes[0], "Hash should match in request data")

		// Clean up
		req.Close()
	})

	t.Run("MultipleHashes", func(t *testing.T) {
		peer := setupPeer()
		defer peer.Close()

		hashes := []common.Hash{
			{0x01, 0x02, 0x03},
			{0x04, 0x05, 0x06},
			{0x07, 0x08, 0x09},
		}
		sink := make(chan *Response, 1)

		// Request witness metadata
		req, err := peer.RequestWitnessMetadata(hashes, sink)
		assert.NoError(t, err, "RequestWitnessMetadata should not return error")
		assert.NotNil(t, req, "Request should not be nil")

		// Verify request data structure
		packetData, ok := req.data.(*GetWitnessMetadataPacket)
		assert.True(t, ok, "Request data should be *GetWitnessMetadataPacket")
		assert.Equal(t, len(hashes), len(packetData.Hashes), "Should have correct number of hashes")
		for i, hash := range hashes {
			assert.Equal(t, hash, packetData.Hashes[i], "Hash at index %d should match", i)
		}

		// Clean up
		req.Close()
	})

	t.Run("EmptyHashes", func(t *testing.T) {
		peer := setupPeer()
		defer peer.Close()

		sink := make(chan *Response, 1)

		// Request witness metadata with empty hashes
		req, err := peer.RequestWitnessMetadata([]common.Hash{}, sink)
		assert.NoError(t, err, "RequestWitnessMetadata should not return error for empty hashes")
		assert.NotNil(t, req, "Request should not be nil")

		// Verify request data structure
		packetData, ok := req.data.(*GetWitnessMetadataPacket)
		assert.True(t, ok, "Request data should be *GetWitnessMetadataPacket")
		assert.Equal(t, 0, len(packetData.Hashes), "Should have zero hashes")

		// Clean up
		req.Close()
	})

	t.Run("TooManyHashes", func(t *testing.T) {
		peer := setupPeer()
		defer peer.Close()

		hashes := make([]common.Hash, MaxWitnessMetadataServe+1)
		sink := make(chan *Response, 1)

		req, err := peer.RequestWitnessMetadata(hashes, sink)
		assert.Error(t, err, "RequestWitnessMetadata should reject over-limit hashes")
		assert.Nil(t, req, "Request should be nil on over-limit hashes")
	})

	t.Run("RequestIDUniqueness", func(t *testing.T) {
		peer := setupPeer()
		defer peer.Close()

		hash := common.Hash{0x01}
		sink1 := make(chan *Response, 1)
		sink2 := make(chan *Response, 1)

		// Make two requests
		req1, err1 := peer.RequestWitnessMetadata([]common.Hash{hash}, sink1)
		assert.NoError(t, err1)

		req2, err2 := peer.RequestWitnessMetadata([]common.Hash{hash}, sink2)
		assert.NoError(t, err2)

		// Verify request IDs are different
		assert.NotEqual(t, req1.id, req2.id, "Request IDs should be unique")

		// Clean up
		req1.Close()
		req2.Close()
	})
}

func TestRequestWitness(t *testing.T) {
	t.Run("TooManyPages", func(t *testing.T) {
		peer := setupPeer()
		defer peer.Close()

		pages := make([]WitnessPageRequest, MaxWitnessServe+1)
		sink := make(chan *Response, 1)

		req, err := peer.RequestWitness(pages, sink)
		assert.Error(t, err, "RequestWitness should reject over-limit pages")
		assert.Nil(t, req, "Request should be nil on over-limit pages")
	})
}

// TestReplyWitnessMetadata tests the ReplyWitnessMetadata function
func TestReplyWitnessMetadata(t *testing.T) {
	t.Run("SingleMetadata", func(t *testing.T) {
		peer, app, _ := setupPeerWithPipe()
		defer peer.Close()

		requestID := uint64(12345)
		metadata := []WitnessMetadataResponse{
			{
				Hash:        common.Hash{0x01, 0x02, 0x03},
				TotalPages:  10,
				WitnessSize: 1024 * 1024,
				BlockNumber: 100,
				Available:   true,
			},
		}

		// Start reading messages - filter for the one we want
		var packet WitnessMetadataPacket
		readDone := make(chan error, 1)
		readerReady := make(chan struct{})
		go func() {
			close(readerReady)
			// Read messages until we get the WitnessMetadataMsg
			for {
				msg, err := app.ReadMsg()
				if err != nil {
					readDone <- err
					return
				}
				if msg.Code == WitnessMetadataMsg {
					// Decode the packet
					if err := msg.Decode(&packet); err != nil {
						readDone <- err
						return
					}
					readDone <- nil
					return
				}
				// Discard other messages
				msg.Discard()
			}
		}()

		// Wait for reader to be ready
		<-readerReady
		time.Sleep(50 * time.Millisecond)

		// Send reply in goroutine to avoid blocking test
		errCh := make(chan error, 1)
		go func() {
			errCh <- peer.ReplyWitnessMetadata(requestID, metadata)
		}()

		// Verify the message was received
		select {
		case err := <-readDone:
			assert.NoError(t, err, "Should receive WitnessMetadataMsg")
			assert.Equal(t, requestID, packet.RequestId, "Request ID should match")
			assert.Equal(t, 1, len(packet.Metadata), "Should have one metadata entry")
			assert.Equal(t, metadata[0].Hash, packet.Metadata[0].Hash, "Hash should match")
			assert.Equal(t, metadata[0].TotalPages, packet.Metadata[0].TotalPages, "TotalPages should match")
			assert.Equal(t, metadata[0].WitnessSize, packet.Metadata[0].WitnessSize, "WitnessSize should match")
			assert.Equal(t, metadata[0].BlockNumber, packet.Metadata[0].BlockNumber, "BlockNumber should match")
			assert.Equal(t, metadata[0].Available, packet.Metadata[0].Available, "Available should match")
		case <-time.After(2 * time.Second):
			t.Fatal("Timeout waiting for WitnessMetadataMsg")
		}

		// Check for send error
		select {
		case err := <-errCh:
			assert.NoError(t, err, "ReplyWitnessMetadata should not return error")
		case <-time.After(100 * time.Millisecond):
			// Send might still be in progress, that's okay
		}

		app.Close()
	})

	t.Run("MultipleMetadata", func(t *testing.T) {
		peer, app, _ := setupPeerWithPipe()
		defer peer.Close()

		requestID := uint64(67890)
		metadata := []WitnessMetadataResponse{
			{
				Hash:        common.Hash{0x01, 0x02, 0x03},
				TotalPages:  10,
				WitnessSize: 1024 * 1024,
				BlockNumber: 100,
				Available:   true,
			},
			{
				Hash:        common.Hash{0x04, 0x05, 0x06},
				TotalPages:  20,
				WitnessSize: 2048 * 1024,
				BlockNumber: 200,
				Available:   true,
			},
			{
				Hash:        common.Hash{0x07, 0x08, 0x09},
				TotalPages:  5,
				WitnessSize: 512 * 1024,
				BlockNumber: 300,
				Available:   false,
			},
		}

		// Start reading messages - filter for the one we want
		var packet WitnessMetadataPacket
		readDone := make(chan error, 1)
		readerReady := make(chan struct{})
		go func() {
			close(readerReady)
			// Read messages until we get the WitnessMetadataMsg
			for {
				msg, err := app.ReadMsg()
				if err != nil {
					readDone <- err
					return
				}
				if msg.Code == WitnessMetadataMsg {
					// Decode the packet
					if err := msg.Decode(&packet); err != nil {
						readDone <- err
						return
					}
					readDone <- nil
					return
				}
				// Discard other messages
				msg.Discard()
			}
		}()

		// Wait for reader to be ready
		<-readerReady
		time.Sleep(50 * time.Millisecond)

		// Send reply in goroutine
		errCh := make(chan error, 1)
		go func() {
			errCh <- peer.ReplyWitnessMetadata(requestID, metadata)
		}()

		// Verify the message was received
		select {
		case err := <-readDone:
			assert.NoError(t, err, "Should receive WitnessMetadataMsg")
			assert.Equal(t, requestID, packet.RequestId, "Request ID should match")
			assert.Equal(t, len(metadata), len(packet.Metadata), "Should have correct number of metadata entries")

			for i, expected := range metadata {
				assert.Equal(t, expected.Hash, packet.Metadata[i].Hash, "Hash at index %d should match", i)
				assert.Equal(t, expected.TotalPages, packet.Metadata[i].TotalPages, "TotalPages at index %d should match", i)
				assert.Equal(t, expected.WitnessSize, packet.Metadata[i].WitnessSize, "WitnessSize at index %d should match", i)
				assert.Equal(t, expected.BlockNumber, packet.Metadata[i].BlockNumber, "BlockNumber at index %d should match", i)
				assert.Equal(t, expected.Available, packet.Metadata[i].Available, "Available at index %d should match", i)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Timeout waiting for WitnessMetadataMsg")
		}

		// Check for send error
		select {
		case err := <-errCh:
			assert.NoError(t, err, "ReplyWitnessMetadata should not return error")
		case <-time.After(100 * time.Millisecond):
		}

		app.Close()
	})

	t.Run("EmptyMetadata", func(t *testing.T) {
		peer, app, _ := setupPeerWithPipe()
		defer peer.Close()

		requestID := uint64(11111)
		metadata := []WitnessMetadataResponse{}

		// Start reading messages - filter for the one we want
		var packet WitnessMetadataPacket
		readDone := make(chan error, 1)
		readerReady := make(chan struct{})
		go func() {
			close(readerReady)
			// Read messages until we get the WitnessMetadataMsg
			for {
				msg, err := app.ReadMsg()
				if err != nil {
					readDone <- err
					return
				}
				if msg.Code == WitnessMetadataMsg {
					// Decode the packet
					if err := msg.Decode(&packet); err != nil {
						readDone <- err
						return
					}
					readDone <- nil
					return
				}
				// Discard other messages
				msg.Discard()
			}
		}()

		// Wait for reader to be ready
		<-readerReady
		time.Sleep(50 * time.Millisecond)

		// Send reply in goroutine
		errCh := make(chan error, 1)
		go func() {
			errCh <- peer.ReplyWitnessMetadata(requestID, metadata)
		}()

		// Verify the message was received
		select {
		case err := <-readDone:
			assert.NoError(t, err, "Should receive WitnessMetadataMsg")
			assert.Equal(t, requestID, packet.RequestId, "Request ID should match")
			assert.Equal(t, 0, len(packet.Metadata), "Should have zero metadata entries")
		case <-time.After(2 * time.Second):
			t.Fatal("Timeout waiting for WitnessMetadataMsg")
		}

		// Check for send error
		select {
		case err := <-errCh:
			assert.NoError(t, err, "ReplyWitnessMetadata should not return error for empty metadata")
		case <-time.After(100 * time.Millisecond):
		}

		app.Close()
	})

	t.Run("UnavailableWitness", func(t *testing.T) {
		peer, app, _ := setupPeerWithPipe()
		defer peer.Close()

		requestID := uint64(22222)
		metadata := []WitnessMetadataResponse{
			{
				Hash:      common.Hash{0xAA, 0xBB, 0xCC},
				Available: false,
			},
		}

		// Start reading messages - filter for the one we want
		var packet WitnessMetadataPacket
		readDone := make(chan error, 1)
		readerReady := make(chan struct{})
		go func() {
			close(readerReady)
			// Read messages until we get the WitnessMetadataMsg
			for {
				msg, err := app.ReadMsg()
				if err != nil {
					readDone <- err
					return
				}
				if msg.Code == WitnessMetadataMsg {
					// Decode the packet
					if err := msg.Decode(&packet); err != nil {
						readDone <- err
						return
					}
					readDone <- nil
					return
				}
				// Discard other messages
				msg.Discard()
			}
		}()

		// Wait for reader to be ready
		<-readerReady
		time.Sleep(50 * time.Millisecond)

		// Send reply in goroutine
		errCh := make(chan error, 1)
		go func() {
			errCh <- peer.ReplyWitnessMetadata(requestID, metadata)
		}()

		// Verify the message was received
		select {
		case err := <-readDone:
			assert.NoError(t, err, "Should receive WitnessMetadataMsg")
			assert.Equal(t, requestID, packet.RequestId, "Request ID should match")
			assert.Equal(t, 1, len(packet.Metadata), "Should have one metadata entry")
			assert.False(t, packet.Metadata[0].Available, "Available should be false")
		case <-time.After(2 * time.Second):
			t.Fatal("Timeout waiting for WitnessMetadataMsg")
		}

		// Check for send error
		select {
		case err := <-errCh:
			assert.NoError(t, err, "ReplyWitnessMetadata should not return error")
		case <-time.After(100 * time.Millisecond):
		}

		app.Close()
	})
}

// TestKnownCacheConcurrency verifies that KnownCache is safe for concurrent access
// without external locking. Run with -race to detect data races.
func TestKnownCacheConcurrency(t *testing.T) {
	cache := newKnownCache(100)

	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			h := common.Hash{byte(n)}
			cache.Add(h)
			cache.Contains(h)
			cache.Cardinality()
		}(i)
	}

	wg.Wait()
	assert.LessOrEqual(t, cache.Cardinality(), 100)
}

// TestKnownCacheEviction verifies that the cache evicts entries when at capacity.
func TestKnownCacheEviction(t *testing.T) {
	cache := newKnownCache(5)

	for i := 0; i < 10; i++ {
		cache.Add(common.Hash{byte(i)})
	}

	assert.LessOrEqual(t, cache.Cardinality(), 5, "Cache should not exceed max capacity")
}
