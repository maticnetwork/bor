// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package fetcher contains the announcement based header, blocks or transaction synchronisation.
package fetcher

import (
	"errors"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/trie"
)

const (
	lightTimeout  = time.Millisecond       // Time allowance before an announced header is explicitly requested
	arriveTimeout = 500 * time.Millisecond // Time allowance before an announced block/transaction is explicitly requested
	gatherSlack   = 100 * time.Millisecond // Interval used to collate almost-expired announces with fetches
	fetchTimeout  = 5 * time.Second        // Maximum allotted time to return an explicitly requested block/transaction
)

const (
	maxUncleDist = 7 // Maximum allowed backward distance from the chain head

	// maxQueueDist is increased for bor to allow storing more block announcements
	// near chain tip
	maxQueueDist = 32 * 6 // Maximum allowed distance from the chain head to queue
	hashLimit    = 256    // Maximum number of unique blocks or headers a peer may have announced

	// blockLimit is increased for bor to allow storing more unique blocks near chain tip
	blockLimit = 64 * 3 // Maximum number of unique blocks a peer may have delivered
)

var (
	blockAnnounceInMeter   = metrics.NewRegisteredMeter("eth/fetcher/block/announces/in", nil)
	blockAnnounceOutTimer  = metrics.NewRegisteredTimer("eth/fetcher/block/announces/out", nil)
	blockAnnounceDropMeter = metrics.NewRegisteredMeter("eth/fetcher/block/announces/drop", nil)
	blockAnnounceDOSMeter  = metrics.NewRegisteredMeter("eth/fetcher/block/announces/dos", nil)

	blockBroadcastInMeter   = metrics.NewRegisteredMeter("eth/fetcher/block/broadcasts/in", nil)
	blockBroadcastOutTimer  = metrics.NewRegisteredTimer("eth/fetcher/block/broadcasts/out", nil)
	blockBroadcastDropMeter = metrics.NewRegisteredMeter("eth/fetcher/block/broadcasts/drop", nil)
	blockBroadcastDOSMeter  = metrics.NewRegisteredMeter("eth/fetcher/block/broadcasts/dos", nil)

	headerFetchMeter  = metrics.NewRegisteredMeter("eth/fetcher/block/headers", nil)
	bodyFetchMeter    = metrics.NewRegisteredMeter("eth/fetcher/block/bodies", nil)
	witnessFetchMeter = metrics.NewRegisteredMeter("eth/fetcher/block/witnesses", nil)

	headerFilterInMeter  = metrics.NewRegisteredMeter("eth/fetcher/block/filter/headers/in", nil)
	headerFilterOutMeter = metrics.NewRegisteredMeter("eth/fetcher/block/filter/headers/out", nil)
	bodyFilterInMeter    = metrics.NewRegisteredMeter("eth/fetcher/block/filter/bodies/in", nil)
	bodyFilterOutMeter   = metrics.NewRegisteredMeter("eth/fetcher/block/filter/bodies/out", nil)
)

var errTerminated = errors.New("terminated")

// HeaderRetrievalFn is a callback type for retrieving a header from the local chain.
type HeaderRetrievalFn func(common.Hash) *types.Header

// blockRetrievalFn is a callback type for retrieving a block from the local chain.
type blockRetrievalFn func(common.Hash) *types.Block

// headerRequesterFn is a callback type for sending a header retrieval request.
type headerRequesterFn func(common.Hash, chan *eth.Response) (*eth.Request, error)

// bodyRequesterFn is a callback type for sending a body retrieval request.
type bodyRequesterFn func([]common.Hash, chan *eth.Response) (*eth.Request, error)

// witnessRequesterFn is a callback type for sending a witness retrieval request.
type witnessRequesterFn func(common.Hash, chan *wit.Response) (*wit.Request, error)

// headerVerifierFn is a callback type to verify a block's header for fast propagation.
type headerVerifierFn func(header *types.Header) error

// blockBroadcasterFn is a callback type for broadcasting a block to connected peers.
type blockBroadcasterFn func(block *types.Block, witness *stateless.Witness, propagate bool)

// chainHeightFn is a callback type to retrieve the current chain height.
type chainHeightFn func() uint64

// headersInsertFn is a callback type to insert a batch of headers into the local chain.
type headersInsertFn func(headers []*types.Header) (int, error)

// chainInsertFn is a callback type to insert a batch of blocks into the local chain.
// It now includes witnesses corresponding to the blocks for stateless mode.
type chainInsertFn func(types.Blocks, []*stateless.Witness) (int, error)

// peerDropFn is a callback type for dropping a peer detected as malicious.
type peerDropFn func(id string)

// blockAnnounce is the hash notification of the availability of a new block in the
// network.
type blockAnnounce struct {
	hash   common.Hash   // Hash of the block being announced
	number uint64        // Number of the block being announced (0 = unknown | old protocol)
	header *types.Header // Header of the block partially reassembled (new protocol)
	time   time.Time     // Timestamp of the announcement

	origin string // Identifier of the peer originating the notification

	fetchHeader  headerRequesterFn  // Fetcher function to retrieve the header of an announced block
	fetchBodies  bodyRequesterFn    // Fetcher function to retrieve the body of an announced block
	fetchWitness witnessRequesterFn // Fetcher function to retrieve the witness of an announced block
}

// headerFilterTask represents a batch of headers needing fetcher filtering.
type headerFilterTask struct {
	peer          string          // The source peer of block headers
	headers       []*types.Header // Collection of headers to filter
	time          time.Time       // Arrival time of the headers
	announcedTime time.Time       // Announcement time of the availability of the block
}

// bodyFilterTask represents a batch of block bodies (transactions and uncles)
// needing fetcher filtering.
type bodyFilterTask struct {
	peer          string                 // The source peer of block bodies
	transactions  [][]*types.Transaction // Collection of transactions per block bodies
	uncles        [][]*types.Header      // Collection of uncles per block bodies
	time          time.Time              // Arrival time of the blocks' contents
	announcedTime time.Time              // Announcement time of the availability of the block
}

// blockOrHeaderInject represents a schedules import operation.
type blockOrHeaderInject struct {
	origin string

	header  *types.Header      // Used for light mode fetcher which only cares about header.
	block   *types.Block       // Used for normal mode fetcher which imports full block.
	witness *stateless.Witness // Used for witness mode fetcher which imports witness.
}

// number returns the block number of the injected object.
func (inject *blockOrHeaderInject) number() uint64 {
	if inject.header != nil {
		return inject.header.Number.Uint64()
	}

	return inject.block.NumberU64()
}

// number returns the block hash of the injected object.
func (inject *blockOrHeaderInject) hash() common.Hash {
	if inject.header != nil {
		return inject.header.Hash()
	}

	return inject.block.Hash()
}

// injectBlockNeedWitnessMsg is used to inject a block received externally
// that requires witness fetching before final import.
type injectBlockNeedWitnessMsg struct {
	origin       string
	block        *types.Block
	time         time.Time // Arrival time
	fetchWitness witnessRequesterFn
}

// injectedWitnessMsg is used to inject a witness received externally via broadcast.
type injectedWitnessMsg struct {
	peer    string
	witness *stateless.Witness
	time    time.Time // Arrival time
}

// enqueueRequest is used to shuttle fully assembled blocks (with witness)
// from concurrent fetcher goroutines back to the main loop for safe queuing.
type enqueueRequest struct {
	op *blockOrHeaderInject
}

// BlockFetcher is responsible for accumulating block announcements from various peers
// and scheduling them for retrieval.
type BlockFetcher struct {
	light bool // The indicator whether it's a light fetcher or normal one.

	// Various event channels
	notify    chan *blockAnnounce
	inject    chan *blockOrHeaderInject
	enqueueCh chan *enqueueRequest // Channel for safe enqueue calls (used by wm)

	headerFilter chan chan *headerFilterTask
	bodyFilter   chan chan *bodyFilterTask

	done chan common.Hash
	quit chan struct{}

	// Protect concurrent map access from goroutines
	mu sync.RWMutex

	// Announce states
	announces  map[string]int                   // Per peer blockAnnounce counts to prevent memory exhaustion
	announced  map[common.Hash][]*blockAnnounce // Announced blocks, scheduled for fetching
	fetching   map[common.Hash]*blockAnnounce   // Announced blocks, currently fetching
	fetched    map[common.Hash][]*blockAnnounce // Blocks with headers fetched, scheduled for body retrieval
	completing map[common.Hash]*blockAnnounce   // Blocks with headers, currently body-completing

	// Block cache
	queue  *prque.Prque[int64, *blockOrHeaderInject] // Queue containing the import operations (block number sorted)
	queues map[string]int                            // Per peer block counts to prevent memory exhaustion
	queued map[common.Hash]*blockOrHeaderInject      // Set of already queued blocks (to dedup imports)

	// Witness Manager
	wm *witnessManager // Handles all witness-related logic

	// Callbacks
	getHeader      HeaderRetrievalFn  // Retrieves a header from the local chain
	getBlock       blockRetrievalFn   // Retrieves a block from the local chain
	verifyHeader   headerVerifierFn   // Checks if a block's headers have a valid proof of work
	broadcastBlock blockBroadcasterFn // Broadcasts a block to connected peers
	chainHeight    chainHeightFn      // Retrieves the current chain's height
	insertHeaders  headersInsertFn    // Injects a batch of headers into the chain
	insertChain    chainInsertFn      // Injects a batch of blocks into the chain
	dropPeer       peerDropFn         // Drops a peer for misbehaving

	// Testing hooks
	announceChangeHook func(common.Hash, bool)           // Method to call upon adding or deleting a hash from the blockAnnounce list
	queueChangeHook    func(common.Hash, bool)           // Method to call upon adding or deleting a block from the import queue
	fetchingHook       func([]common.Hash)               // Method to call upon starting a block (eth/61) or header (eth/62) fetch
	completingHook     func([]common.Hash)               // Method to call upon starting a block body fetch (eth/62)
	importedHook       func(*types.Header, *types.Block) // Method to call upon successful header or block import (both eth/61 and eth/62)

	// Logging
	enableBlockTracking bool // Whether to log information collected while tracking block lifecycle
	requireWitness      bool // Whether operating in a mode where witnesses are strictly required for block imports
}

// NewBlockFetcher creates a block fetcher to retrieve blocks based on hash announcements.
func NewBlockFetcher(light bool, getHeader HeaderRetrievalFn, getBlock blockRetrievalFn, verifyHeader headerVerifierFn, broadcastBlock blockBroadcasterFn, chainHeight chainHeightFn, insertHeaders headersInsertFn, insertChain chainInsertFn, dropPeer peerDropFn, enableBlockTracking bool, requireWitness bool) *BlockFetcher {
	f := &BlockFetcher{
		light:               light,
		notify:              make(chan *blockAnnounce),
		inject:              make(chan *blockOrHeaderInject),
		enqueueCh:           make(chan *enqueueRequest, 10), // Add buffering
		headerFilter:        make(chan chan *headerFilterTask),
		bodyFilter:          make(chan chan *bodyFilterTask),
		done:                make(chan common.Hash),
		quit:                make(chan struct{}),
		announces:           make(map[string]int),
		announced:           make(map[common.Hash][]*blockAnnounce),
		fetching:            make(map[common.Hash]*blockAnnounce),
		fetched:             make(map[common.Hash][]*blockAnnounce),
		completing:          make(map[common.Hash]*blockAnnounce),
		queue:               prque.New[int64, *blockOrHeaderInject](nil),
		queues:              make(map[string]int),
		queued:              make(map[common.Hash]*blockOrHeaderInject),
		getHeader:           getHeader,
		getBlock:            getBlock,
		verifyHeader:        verifyHeader,
		broadcastBlock:      broadcastBlock,
		chainHeight:         chainHeight,
		insertHeaders:       insertHeaders,
		insertChain:         insertChain,
		dropPeer:            dropPeer,
		enableBlockTracking: enableBlockTracking,
		requireWitness:      requireWitness,
	}

	// Initialize Witness Manager
	f.wm = newWitnessManager(
		f.quit,
		f.dropPeer,
		f.enqueueCh,
		f.getBlock,
		f.getHeader,
		f.chainHeight,
	)

	return f
}

// Start boots up the announcement based synchroniser, accepting and processing
// hash notifications and block fetches until termination requested.
func (f *BlockFetcher) Start() {
	go f.loop()
	f.wm.start() // Start the witness manager loop
}

// Stop terminates the announcement based synchroniser, canceling all pending
// operations.
func (f *BlockFetcher) Stop() {
	close(f.quit)
}

// Notify announces the fetcher of the potential availability of a new block in
// the network.
func (f *BlockFetcher) Notify(peer string, hash common.Hash, number uint64, time time.Time,
	headerFetcher headerRequesterFn, bodyFetcher bodyRequesterFn, witnessFetcher witnessRequesterFn) error {
	block := &blockAnnounce{
		hash:         hash,
		number:       number,
		time:         time,
		origin:       peer,
		fetchHeader:  headerFetcher,
		fetchBodies:  bodyFetcher,
		fetchWitness: witnessFetcher,
	}
	select {
	case f.notify <- block:
		return nil
	case <-f.quit:
		return errTerminated
	}
}

// Enqueue tries to fill gaps the fetcher's future import queue.
func (f *BlockFetcher) Enqueue(peer string, block *types.Block) error {
	op := &blockOrHeaderInject{
		origin: peer,
		block:  block,
	}
	select {
	case f.inject <- op:
		return nil
	case <-f.quit:
		return errTerminated
	}
}

// sendOrQuit is a small convenience wrapper used by public inject-APIs.
// It blocks until `msg` is delivered *or* the fetcher is shutting down.
func sendOrQuit[T any](quit <-chan struct{}, ch chan<- T, msg T) error {
	select {
	case ch <- msg:
		return nil
	case <-quit:
		return errTerminated
	}
}

// InjectBlockWithWitnessRequirement tries to inject a block that requires witness fetching.
// This is used when a block is received directly (e.g., NewBlockMsg) in stateless mode.
// It bypasses the initial header/body fetch stages but ensures witness fetching is triggered.
func (f *BlockFetcher) InjectBlockWithWitnessRequirement(origin string, block *types.Block, fetchWitness witnessRequesterFn) error {
	if fetchWitness == nil {
		// This method requires a valid fetchWitness function.
		// If no witness is needed, the standard Enqueue should be used by the caller.
		return errors.New("InjectBlockWithWitnessRequirement called with nil fetchWitness")
	}
	msg := &injectBlockNeedWitnessMsg{
		origin:       origin,
		block:        block,
		time:         time.Now(),
		fetchWitness: fetchWitness,
	}
	log.Debug("Injecting block needing witness fetch", "peer", origin, "hash", block.Hash())
	// Send to witness manager's channel
	return sendOrQuit(f.quit, f.wm.injectNeedWitnessCh, msg)
}

// InjectWitness injects a witness received via broadcast into the fetcher.
func (f *BlockFetcher) InjectWitness(peer string, witness *stateless.Witness) error {
	msg := &injectedWitnessMsg{
		peer:    peer,
		witness: witness,
		time:    time.Now(),
	}
	log.Debug("Injecting witness from broadcast", "peer", peer, "hash", witness.Header().Hash())
	// Send to witness manager's channel
	return sendOrQuit(f.quit, f.wm.injectWitnessCh, msg)
}

// FilterHeaders extracts all the headers that were explicitly requested by the fetcher,
// returning those that should be handled differently.
func (f *BlockFetcher) FilterHeaders(peer string, headers []*types.Header, time time.Time, announcedAt time.Time) []*types.Header {
	log.Debug("Filtering headers", "peer", peer, "headers", len(headers))

	// Send the filter channel to the fetcher
	filter := make(chan *headerFilterTask)

	select {
	case f.headerFilter <- filter:
	case <-f.quit:
		return nil
	}
	// Request the filtering of the header list
	select {
	case filter <- &headerFilterTask{peer: peer, headers: headers, time: time, announcedTime: announcedAt}:
	case <-f.quit:
		return nil
	}
	// Retrieve the headers remaining after filtering
	select {
	case task := <-filter:
		return task.headers
	case <-f.quit:
		return nil
	}
}

// FilterBodies extracts all the block bodies that were explicitly requested by
// the fetcher, returning those that should be handled differently.
func (f *BlockFetcher) FilterBodies(peer string, transactions [][]*types.Transaction, uncles [][]*types.Header, time time.Time, announcedAt time.Time) ([][]*types.Transaction, [][]*types.Header) {
	log.Debug("Filtering bodies", "peer", peer, "txs", len(transactions), "uncles", len(uncles))

	// Send the filter channel to the fetcher
	filter := make(chan *bodyFilterTask)

	select {
	case f.bodyFilter <- filter:
	case <-f.quit:
		return nil, nil
	}
	// Request the filtering of the body list
	select {
	case filter <- &bodyFilterTask{peer: peer, transactions: transactions, uncles: uncles, time: time, announcedTime: announcedAt}:
	case <-f.quit:
		return nil, nil
	}
	// Retrieve the bodies remaining after filtering
	select {
	case task := <-filter:
		return task.transactions, task.uncles
	case <-f.quit:
		return nil, nil
	}
}

// Loop is the main fetcher loop, checking and processing various notification
// events.
func (f *BlockFetcher) loop() {
	// Iterate the block fetching until a quit is requested
	var (
		fetchTimer    = time.NewTimer(0)
		completeTimer = time.NewTimer(0)
	)

	// Clear timers
	if !fetchTimer.Stop() {
		<-fetchTimer.C
	}
	if !completeTimer.Stop() {
		<-completeTimer.C
	}

	defer fetchTimer.Stop()
	defer completeTimer.Stop()

	for {
		// Clean up any expired block fetches
		f.mu.RLock()
		expiredHashes := make([]common.Hash, 0)
		for hash, announce := range f.fetching {
			if time.Since(announce.time) > fetchTimeout {
				expiredHashes = append(expiredHashes, hash)
			}
		}
		f.mu.RUnlock()

		for _, hash := range expiredHashes {
			f.forgetHash(hash)
		}
		// Import any queued blocks that could potentially fit
		height := f.chainHeight()

		for {
			f.mu.Lock()
			var op *blockOrHeaderInject
			if !f.queue.Empty() {
				op = f.queue.PopItem()
			}
			f.mu.Unlock()

			if op == nil {
				break
			}

			hash := op.hash()
			if f.queueChangeHook != nil {
				f.queueChangeHook(hash, false)
			}
			// If too high up the chain or phase, continue later
			number := op.number()
			if number > height+1 {
				f.mu.Lock()
				f.queue.Push(op, -int64(number))
				f.mu.Unlock()

				if f.queueChangeHook != nil {
					f.queueChangeHook(hash, true)
				}

				break
			}
			// Otherwise if fresh and still unknown, try and import
			// Also check if witness manager is handling it
			if (number+maxUncleDist < height) || (f.light && f.getHeader(hash) != nil) || (!f.light && f.getBlock(hash) != nil) {
				// If known, forget about it
				f.forgetBlock(hash)
				continue
			}
			if f.wm.isPending(hash) {
				// If witness manager is handling it, re-queue for later
				// The witness manager will enqueue it when ready
				f.queue.Push(op, -int64(number))
				if f.queueChangeHook != nil {
					f.queueChangeHook(hash, true)
				}
				break // Exit the import loop to let witness manager work
			}

			if f.light {
				f.importHeaders(op.origin, op.header)
			} else {
				// Block must have witness if required, handled by enqueue logic or WM
				f.importBlocks(op.origin, op.block, op.witness)
			}
		}

		// Wait for an outside event to occur
		select {
		case <-f.quit:
			// BlockFetcher terminating, abort all operations
			return

		case notification := <-f.notify:
			// A block was announced, make sure the peer isn't DOSing us
			blockAnnounceInMeter.Mark(1)

			count := f.announces[notification.origin] + 1
			if count > hashLimit {
				log.Debug("Peer exceeded outstanding announces", "peer", notification.origin, "limit", hashLimit)
				blockAnnounceDOSMeter.Mark(1)
				break
			}

			if notification.number == 0 {
				break // Ignore old protocol announces
			}

			// Check if peer has already failed witness attempts too many times
			if f.requireWitness && f.wm.HasFailedTooManyTimes(notification.origin) {
				log.Debug("Peer ignored announcement, too many prior witness failures", "peer", notification.origin)
				blockAnnounceDropMeter.Mark(1)
				break
			}

			// If we have a valid block number, check that it's potentially useful
			if dist := int64(notification.number) - int64(f.chainHeight()); dist < -maxUncleDist || dist > maxQueueDist {
				log.Debug("Peer discarded announcement", "peer", notification.origin, "number", notification.number, "hash", notification.hash, "distance", dist)
				blockAnnounceDropMeter.Mark(1)
				break
			}
			// All is well, schedule the announce if block's not yet downloading or completing or pending witness
			if _, ok := f.fetching[notification.hash]; ok {
				break
			}
			if _, ok := f.completing[notification.hash]; ok {
				break
			}
			if f.wm.isPending(notification.hash) {
				break // Witness manager is already handling this hash
			}

			f.announces[notification.origin] = count
			f.announced[notification.hash] = append(f.announced[notification.hash], notification)

			if f.announceChangeHook != nil && len(f.announced[notification.hash]) == 1 {
				f.announceChangeHook(notification.hash, true)
			}

			if len(f.announced) == 1 {
				f.rescheduleFetch(fetchTimer)
			}

		case op := <-f.inject:
			// A direct block insertion was requested
			blockBroadcastInMeter.Mark(1)

			if f.light || op.header != nil { // Light mode only handles headers, block injection is via headers
				log.Debug("Ignoring direct block injection in light mode or if header provided", "origin", op.origin)
				continue
			}

			// Check witness failures again specifically for witness-requiring injections
			// Note: Non-witness blocks are enqueued directly without this check here.
			if f.requireWitness && op.block != nil && f.wm.HasFailedTooManyTimes(op.origin) {
				log.Warn("Discarding injected block, peer has too many prior witness failures", "peer", op.origin, "hash", op.hash())
				continue
			}
			// Witness manager handles peer failure checks related to witness

			f.enqueue(op.origin, nil, op.block, op.witness)

		case req := <-f.enqueueCh:
			// A fetcher goroutine (or witnessManager) completed fetching block + witness
			if req.op == nil {
				log.Error("Received nil enqueue request")
				continue
			}
			// Enqueue the fully assembled block (potentially with witness)
			f.enqueue(req.op.origin, nil, req.op.block, req.op.witness)

		case hash := <-f.done:
			// A pending import finished, remove all traces of the notification
			f.forgetHash(hash)  // This calls wm.forget
			f.forgetBlock(hash) // This calls wm.forget

		case <-fetchTimer.C:
			// At least one block's timer ran out, check for needing retrieval
			request := make(map[string][]common.Hash)

			for hash, announces := range f.announced {
				// In current LES protocol(les2/les3), only header announce is
				// available, no need to wait too much time for header broadcast.
				timeout := arriveTimeout - gatherSlack
				if f.light {
					timeout = 0
				}

				if time.Since(announces[0].time) > timeout {
					// Determine if we should proceed to fetch the header for this hash.
					shouldScheduleFetch := !f.wm.isPending(hash) && // Not handled by witness manager
						f.queued[hash] == nil && // Not already in the import queue
						f.fetched[hash] == nil && // Header not already fetched
						f.completing[hash] == nil && // Body not already being completed
						((f.light && f.getHeader(hash) == nil) || (!f.light && f.getBlock(hash) == nil)) // Not in DB

					if shouldScheduleFetch {
						announceToUse := announces[rand.Intn(len(announces))]
						f.forgetHash(hash) // This also removes from f.announced

						request[announceToUse.origin] = append(request[announceToUse.origin], hash)
						f.fetching[hash] = announceToUse
					} else {
						for _, annItem := range announces { // 'announces' is f.announced[hash]
							f.announces[annItem.origin]--
							if f.announces[annItem.origin] <= 0 {
								delete(f.announces, annItem.origin)
							}
						}
						delete(f.announced, hash)
						if f.announceChangeHook != nil {
							f.announceChangeHook(hash, false)
						}
					}
				}
			}
			// Send out all block header requests
			for peer, hashes := range request {
				log.Debug("Fetching scheduled headers", "peer", peer, "list", hashes)
				if len(hashes) == 0 {
					continue
				}

				// Create a closure of the fetch and schedule in on a new thread
				announceCopy := *f.fetching[hashes[0]] // Copy announce data for goroutine
				go func(p string, hs []common.Hash, ann blockAnnounce) {
					if f.fetchingHook != nil {
						f.fetchingHook(hs)
					}
					fetchHeader := ann.fetchHeader // Use copied data
					announcedAt := ann.time

					for _, h := range hs {
						headerFetchMeter.Mark(1)
						go func(h common.Hash) {
							resCh := make(chan *eth.Response)
							if fetchHeader == nil {
								return
							} // Check before using
							req, err := fetchHeader(h, resCh)
							if err != nil {
								log.Debug("Failed to request header", "peer", p, "hash", h, "err", err)
								return
							}
							defer req.Close()

							timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
							defer timeout.Stop()

							select {
							case res := <-resCh:
								if res == nil {
									return
								} // Channel closed
								res.Done <- nil
								headersResponse, ok := res.Res.(*eth.BlockHeadersRequest)
								if !ok {
									return
								} // Invalid response type
								f.FilterHeaders(p, *headersResponse, time.Now(), announcedAt)

							case <-timeout.C:
								log.Debug("Header fetch timed out", "peer", p, "hash", h)
								f.dropPeer(p)
							case <-f.quit:
								return // Fetcher stopped
							}
						}(h)
					}
				}(peer, hashes, announceCopy)
			}
			// Schedule the next fetch if blocks are still pending
			f.rescheduleFetch(fetchTimer)

		case <-completeTimer.C:
			// At least one header's timer ran out, retrieve everything
			request := make(map[string][]common.Hash)

			for hash, announces := range f.fetched {
				// Pick a random peer to retrieve from, reset all others
				announce := announces[rand.Intn(len(announces))]

				f.forgetHash(hash) // Removes from f.fetched

				// If the block still didn't arrive, queue for completion
				// Also check if witness manager is already handling it
				if !f.wm.isPending(hash) && f.getBlock(hash) == nil {
					request[announce.origin] = append(request[announce.origin], hash)
					f.completing[hash] = announce
				}
			}
			// Send out all block body requests
			for peer, hashes := range request {
				log.Debug("Fetching scheduled bodies", "peer", peer, "list", hashes)
				if len(hashes) == 0 {
					continue
				}

				announceCopy := *f.completing[hashes[0]] // Copy announce data for goroutine
				go func(p string, hs []common.Hash, ann blockAnnounce) {
					if f.completingHook != nil {
						f.completingHook(hs)
					}
					fetchBodies := ann.fetchBodies // Use copied data
					bodyFetchMeter.Mark(int64(len(hs)))
					announcedAt := ann.time

					resCh := make(chan *eth.Response)
					if fetchBodies == nil {
						return
					} // Check before using
					req, err := fetchBodies(hs, resCh)
					if err != nil {
						log.Debug("Failed to request bodies", "peer", p, "hashes", hs, "err", err)
						return
					}
					defer req.Close()

					timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
					defer timeout.Stop()

					select {
					case res := <-resCh:
						if res == nil {
							return
						} // Channel closed
						res.Done <- nil
						bodyResponse, ok := res.Res.(*eth.BlockBodiesResponse)
						if !ok {
							return
						} // Invalid response type
						// Ignoring withdrawals here, since the block fetcher is not used post-merge.
						txs, uncles, _ := bodyResponse.Unpack()
						f.FilterBodies(p, txs, uncles, time.Now(), announcedAt)

					case <-timeout.C:
						log.Debug("Body fetch timed out", "peer", p, "hashes", hs)
						f.dropPeer(p)
					case <-f.quit:
						return // Fetcher stopped
					}
				}(peer, hashes, announceCopy)
			}
			// Schedule the next fetch if blocks are still pending
			f.rescheduleComplete(completeTimer)

		case filter := <-f.headerFilter:
			// Headers arrived from a remote peer. Extract those that were explicitly
			// requested by the fetcher, and return everything else so it's delivered
			// to other parts of the system.
			var task *headerFilterTask
			select {
			case task = <-filter:
			case <-f.quit:
				return
			}
			headerFilterInMeter.Mark(int64(len(task.headers)))

			// Split the batch of headers into unknown ones (to return to the caller),
			// known incomplete ones (requiring body retrievals) and completed blocks.
			unknown, incomplete, complete, lightHeaders := []*types.Header{}, []*blockAnnounce{}, []*types.Block{}, []*blockAnnounce{}

			for _, header := range task.headers {
				hash := header.Hash()

				// Filter fetcher-requested headers from other synchronisation algorithms
				if announce := f.fetching[hash]; announce != nil && announce.origin == task.peer && f.fetched[hash] == nil && f.completing[hash] == nil && f.queued[hash] == nil && !f.wm.isPending(hash) {
					// If the delivered header does not match the promised number, drop the announcer
					if header.Number.Uint64() != announce.number {
						log.Debug("Invalid block number fetched", "peer", announce.origin, "hash", header.Hash(), "announced", announce.number, "provided", header.Number)
						f.dropPeer(announce.origin)
						f.forgetHash(hash)
						continue
					}
					// Collect all headers only if we are running in light
					// mode and the headers are not imported by other means.
					if f.light {
						if f.getHeader(hash) == nil {
							announce.header = header
							lightHeaders = append(lightHeaders, announce)
						}
						f.forgetHash(hash)
						continue
					}
					// Only keep if not imported by other means
					if f.getBlock(hash) == nil {
						announce.header = header
						announce.time = task.time

						// If the block is empty (header only), check witness requirement
						if header.TxHash == types.EmptyTxsHash && header.UncleHash == types.EmptyUncleHash {
							log.Debug("Block empty, checking witness requirement", "peer", announce.origin, "number", header.Number, "hash", header.Hash())

							block := types.NewBlockWithHeader(header)
							block.ReceivedAt = task.time
							block.AnnouncedAt = &task.announcedTime

							// Check if a witness is required for this empty block
							if f.requireWitness && announce.fetchWitness != nil {
								// === Witness Manager Interaction ===
								// Check peer failure status BEFORE delegating
								if f.wm.HasFailedTooManyTimes(announce.origin) {
									log.Warn("[fetcher] Discarding empty block, peer has too many prior witness failures", "peer", announce.origin, "hash", header.Hash())
									f.forgetHash(hash) // Clean up announce states
									delete(f.fetching, hash)
									continue // Skip witness manager call
								}

								log.Debug("Empty block requires witness, notifying witness manager", "peer", announce.origin, "number", header.Number, "hash", header.Hash())
								f.wm.handleFilterResult(announce, block)
								delete(f.fetching, hash) // Remove from fetching state as WM is now responsible
								// === End Witness Manager Interaction ===
							} else {
								// Witness not required: Add to complete list for immediate enqueueing.
								log.Debug("Empty block doesn't require witness, queuing", "peer", announce.origin, "number", header.Number, "hash", header.Hash())
								complete = append(complete, block)
								// Use completing map temporarily to hold announce until enqueued
								f.completing[hash] = announce
								delete(f.fetching, hash) // Remove from fetching state
							}
							continue // Move to the next header in the loop
						}
						// Otherwise (block not empty) add to the list of blocks needing completion (body fetch)
						incomplete = append(incomplete, announce)
					} else {
						log.Debug("Block already imported, discarding header", "peer", announce.origin, "number", header.Number, "hash", header.Hash())
						f.forgetHash(hash)
					}
				} else {
					// BlockFetcher doesn't know about it, add to the return list
					unknown = append(unknown, header)
				}
			}

			headerFilterOutMeter.Mark(int64(len(unknown)))
			select {
			case filter <- &headerFilterTask{headers: unknown, time: task.time}:
			case <-f.quit:
				return
			}
			// Schedule the retrieved headers for body completion
			for _, announce := range incomplete {
				hash := announce.header.Hash()
				if _, ok := f.completing[hash]; ok {
					continue
				}
				// Move announce from fetching to fetched state
				f.fetched[hash] = append(f.fetched[hash], announce)
				delete(f.fetching, hash)
				if len(f.fetched) == 1 {
					f.rescheduleComplete(completeTimer)
				}
			}
			// Schedule the header for light fetcher import
			for _, announce := range lightHeaders {
				f.enqueue(announce.origin, announce.header, nil, nil)
			}
			// Schedule the header-only blocks (WITHOUT witness req) for import
			for _, block := range complete {
				hash := block.Hash()
				if announce := f.completing[hash]; announce != nil { // Check completing map now
					f.enqueue(announce.origin, nil, block, nil)
					// Enqueue call handles forgetHash -> wm.forget
				} else {
					log.Warn("Announce missing for header-only block", "hash", hash)
					f.forgetHash(hash)
				}
			}

		case filter := <-f.bodyFilter:
			// Block bodies arrived, extract any explicitly requested blocks, return the rest
			var task *bodyFilterTask
			select {
			case task = <-filter:
			case <-f.quit:
				return
			}
			bodyFilterInMeter.Mark(int64(len(task.transactions)))

			blocks := []*types.Block{}
			// abort early if there's nothing explicitly requested in f.completing
			if len(f.completing) > 0 {
				for i := 0; i < len(task.transactions) && i < len(task.uncles); i++ {
					// Match up a body to any possible completion request
					var (
						matched       = false
						blockHash     common.Hash // Calculated lazily based on header
						uncleHash     common.Hash // calculated lazily and reused
						txnHash       common.Hash // calculated lazily and reused
						matchAnnounce *blockAnnounce
					)

					// Iterate completing map to find a match based on header hashes
					for hash, announce := range f.completing {
						if announce.origin != task.peer {
							continue
						}
						// Skip if already queued or pending witness
						if f.queued[hash] != nil || f.wm.isPending(hash) {
							continue
						}

						if uncleHash == (common.Hash{}) {
							uncleHash = types.CalcUncleHash(task.uncles[i])
						}
						if uncleHash != announce.header.UncleHash {
							continue
						}

						if txnHash == (common.Hash{}) {
							txnHash = types.DeriveSha(types.Transactions(task.transactions[i]), trie.NewStackTrie(nil))
						}
						if txnHash != announce.header.TxHash {
							continue
						}

						// Found a match
						blockHash = announce.header.Hash() // Hash of the block header
						matched = true
						matchAnnounce = announce // Store the matching announce

						// If the block is already known locally, just forget the announcement.
						if f.getBlock(blockHash) != nil {
							f.forgetHash(blockHash) // forgetHash calls wm.forget
						} else {
							// Assemble the block
							block := types.NewBlockWithHeader(announce.header).WithBody(types.Body{
								Transactions: task.transactions[i],
								Uncles:       task.uncles[i],
							})
							block.ReceivedAt = task.time
							block.AnnouncedAt = &task.announcedTime

							// If witness is needed, delegate to witness manager, otherwise add to enqueue list
							if f.requireWitness && matchAnnounce.fetchWitness != nil {
								// === Witness Manager Interaction ===
								// Check peer failure status BEFORE delegating
								if f.wm.HasFailedTooManyTimes(announce.origin) {
									log.Warn("[fetcher] Discarding completed block, peer has too many prior witness failures", "peer", announce.origin, "hash", blockHash)
									f.forgetHash(blockHash) // Clean up announce states
									delete(f.completing, blockHash)
									// Skip witness manager call, but allow loop to continue to process next body
								} else {
									log.Debug("Block body received, checking witness requirement with manager", "peer", announce.origin, "number", block.NumberU64(), "hash", blockHash)
									f.wm.checkCompleting(matchAnnounce, block)
									// Remove from completing state as WM is now responsible
									delete(f.completing, blockHash)
								}
								// === End Witness Manager Interaction ===
							} else {
								log.Debug("Block body received, queuing for import (no witness needed)", "peer", announce.origin, "number", block.NumberU64(), "hash", blockHash)
								blocks = append(blocks, block)
								// Keep in completing until enqueued
							}
						}
						break // Found matching announce, move to next body
					}

					if matched {
						task.transactions = append(task.transactions[:i], task.transactions[i+1:]...)
						task.uncles = append(task.uncles[:i], task.uncles[i+1:]...)
						i--
					}
				}
			}

			bodyFilterOutMeter.Mark(int64(len(task.transactions)))
			select {
			case filter <- task: // Return unmatched bodies
			case <-f.quit:
				return
			}
			// Schedule the retrieved blocks for ordered import (only those without pending witnesses)
			for _, block := range blocks {
				// The announce should still be in f.completing map here for blocks without witness requirement
				if announce := f.completing[block.Hash()]; announce != nil {
					f.enqueue(announce.origin, nil, block, nil) // enqueue calls wm.forget
				} else {
					log.Warn("Announce missing for completed block (no witness needed)", "hash", block.Hash())
					f.forgetHash(block.Hash()) // Clean up any potential dangling state
				}
			}
		} // End select
	} // End for
}

// rescheduleFetch resets the specified fetch timer to the next blockAnnounce timeout.
func (f *BlockFetcher) rescheduleFetch(fetch *time.Timer) {
	// Short circuit if no blocks are announced
	if len(f.announced) == 0 {
		return
	}
	// Schedule announcement retrieval quickly for light mode
	// since server won't send any headers to client.
	if f.light {
		fetch.Reset(lightTimeout)
		return
	}
	// Otherwise find the earliest expiring announcement
	earliest := time.Now()
	for _, announces := range f.announced {
		if earliest.After(announces[0].time) {
			earliest = announces[0].time
		}
	}

	fetch.Reset(arriveTimeout - time.Since(earliest))
}

// rescheduleComplete resets the specified completion timer to the next fetch timeout.
func (f *BlockFetcher) rescheduleComplete(complete *time.Timer) {
	// Short circuit if no headers are fetched
	if len(f.fetched) == 0 {
		return
	}
	// Otherwise find the earliest expiring announcement
	earliest := time.Now()
	for _, announces := range f.fetched {
		if earliest.After(announces[0].time) {
			earliest = announces[0].time
		}
	}

	complete.Reset(gatherSlack - time.Since(earliest))
}

// enqueue schedules a new header or block import operation, if the component
// to be imported has not yet been seen.
func (f *BlockFetcher) enqueue(peer string, header *types.Header, block *types.Block, witness *stateless.Witness) {
	var (
		hash   common.Hash
		number uint64
	)

	// Determine hash and number from block first, then header
	if block != nil {
		hash, number = block.Hash(), block.NumberU64()
	} else if header != nil {
		hash, number = header.Hash(), header.Number.Uint64()
	} else {
		log.Error("Enqueue called with nil header and block", "peer", peer)
		return
	}

	f.mu.Lock()

	// Ensure the peer isn't DOSing us
	count := f.queues[peer] + 1
	if count > blockLimit {
		log.Debug("Discarded delivered header or block, exceeded allowance", "peer", peer, "number", number, "hash", hash, "limit", blockLimit)
		blockBroadcastDOSMeter.Mark(1)
		f.mu.Unlock()
		f.forgetHash(hash) // Calls wm.forget
		return
	}
	// Discard any past or too distant blocks
	if dist := int64(number) - int64(f.chainHeight()); dist < -maxUncleDist || dist > maxQueueDist {
		log.Debug("Discarded delivered header or block, too far away", "peer", peer, "number", number, "hash", hash, "distance", dist)
		blockBroadcastDropMeter.Mark(1)
		f.mu.Unlock()
		f.forgetHash(hash) // Calls wm.forget
		return
	}
	// Discard if already queued or pending witness
	if _, ok := f.queued[hash]; ok {
		f.mu.Unlock()
		return
	}
	if f.wm.isPending(hash) {
		log.Debug("Block already pending witness, not queuing", "hash", hash)
		f.mu.Unlock()
		return
	}

	// Schedule the block for future importing
	op := &blockOrHeaderInject{origin: peer}
	if header != nil {
		op.header = header
	} else if block != nil { // Prioritize block over header if both somehow provided
		op.block = block
		// Attach witness only if block is present
		op.witness = witness
	} else {
		log.Error("Invalid state in enqueue: header and block are nil", "peer", peer, "hash", hash)
		f.mu.Unlock()
		return // Should not happen due to check above
	}

	f.queues[peer] = count
	f.queued[hash] = op
	f.queue.Push(op, -int64(number))

	if f.queueChangeHook != nil {
		f.queueChangeHook(hash, true)
	}

	log.Debug("Queued delivered header or block", "peer", peer, "number", number, "hash", hash, "queued", f.queue.Size())
	f.mu.Unlock()

	// Notify witness manager to forget any pending state for this hash,
	// as the block has now been successfully queued for import.
	f.wm.forget(hash)
}

// importHeaders spawns a new goroutine to run a header insertion into the chain.
// If the header's number is at the same height as the current import phase, it
// updates the phase states accordingly.
func (f *BlockFetcher) importHeaders(peer string, header *types.Header) {
	hash := header.Hash()
	log.Debug("Importing propagated header", "peer", peer, "number", header.Number, "hash", hash)

	go func() {
		defer func() { f.done <- hash }()
		// If the parent's unknown, abort insertion
		parent := f.getHeader(header.ParentHash)
		if parent == nil {
			log.Debug("Unknown parent of propagated header", "peer", peer, "number", header.Number, "hash", hash, "parent", header.ParentHash)
			return
		}
		// Validate the header and if something went wrong, drop the peer
		if err := f.verifyHeader(header); err != nil && err != consensus.ErrFutureBlock {
			log.Debug("Propagated header verification failed", "peer", peer, "number", header.Number, "hash", hash, "err", err)
			f.dropPeer(peer)

			return
		}
		// Run the actual import and log any issues
		if _, err := f.insertHeaders([]*types.Header{header}); err != nil {
			log.Debug("Propagated header import failed", "peer", peer, "number", header.Number, "hash", hash, "err", err)
			return
		}
		// Invoke the testing hook if needed
		if f.importedHook != nil {
			f.importedHook(header, nil)
		}
	}()
}

// importBlocks spawns a new goroutine to run a block insertion into the chain. If the
// block's number is at the same height as the current import phase, it updates
// the phase states accordingly.
func (f *BlockFetcher) importBlocks(peer string, block *types.Block, witness *stateless.Witness) {
	hash := block.Hash()

	// Run the import on a new thread
	log.Debug("Importing propagated block", "peer", peer, "number", block.Number(), "hash", hash)

	go func() {
		defer func() { f.done <- hash }()

		// If the parent's unknown, abort insertion
		parent := f.getBlock(block.ParentHash())
		if parent == nil {
			log.Debug("Unknown parent of propagated block", "peer", peer, "number", block.Number(), "hash", hash, "parent", block.ParentHash())
			return
		}
		// Quickly validate the header and propagate the block if it passes
		switch err := f.verifyHeader(block.Header()); err {
		case nil:
			// All ok, quickly propagate to our peers
			blockBroadcastOutTimer.UpdateSince(block.ReceivedAt)

			go f.broadcastBlock(block, witness, true)

		case consensus.ErrFutureBlock:
			// Weird future block, don't fail, but neither propagate

		default:
			// Something went very wrong, drop the peer
			log.Debug("Propagated block verification failed", "peer", peer, "number", block.Number(), "hash", hash, "err", err)
			f.dropPeer(peer)

			return
		}
		// Run the actual import and log any issues
		// Pass the witness along with the block to the insertion function.
		// Create slices even for a single block/witness to match the expected signature.
		if _, err := f.insertChain(types.Blocks{block}, []*stateless.Witness{witness}); err != nil {
			log.Debug("Propagated block import failed", "peer", peer, "number", block.Number(), "hash", hash, "err", err)
			return
		}

		if f.enableBlockTracking {
			// Log the insertion event
			var (
				msg         string
				delayInMs   uint64
				prettyDelay common.PrettyDuration
			)

			if block.AnnouncedAt != nil {
				msg = "[block tracker] Inserted new block with announcement"
				delayInMs = uint64(time.Since(*block.AnnouncedAt).Milliseconds())
				prettyDelay = common.PrettyDuration(time.Since(*block.AnnouncedAt))
			} else {
				msg = "[block tracker] Inserted new block without announcement"
				delayInMs = uint64(time.Since(block.ReceivedAt).Milliseconds())
				prettyDelay = common.PrettyDuration(time.Since(block.ReceivedAt))
			}

			totalDelayInMs := uint64(time.Now().UnixMilli()) - block.Time()*1000
			totalDelay := common.PrettyDuration(time.Millisecond * time.Duration(totalDelayInMs))

			log.Info(msg, "number", block.Number().Uint64(), "hash", hash, "delay", prettyDelay, "delayInMs", delayInMs, "totalDelay", totalDelay, "totalDelayInMs", totalDelayInMs)
		}

		// If import succeeded, broadcast the block
		blockAnnounceOutTimer.UpdateSince(block.ReceivedAt)
		go f.broadcastBlock(block, witness, false)

		// Invoke the testing hook if needed
		if f.importedHook != nil {
			f.importedHook(nil, block)
		}
	}()
}

// forgetHash removes all traces of a block announcement from the fetcher's
// internal state.
func (f *BlockFetcher) forgetHash(hash common.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Remove all pending announces and decrement DOS counters
	if announceMap, ok := f.announced[hash]; ok {
		for _, announce := range announceMap {
			f.announces[announce.origin]--
			if f.announces[announce.origin] <= 0 {
				delete(f.announces, announce.origin)
			}
		}
		delete(f.announced, hash)
		if f.announceChangeHook != nil {
			f.announceChangeHook(hash, false)
		}
	}
	// Remove any pending fetches and decrement the DOS counters
	if announce := f.fetching[hash]; announce != nil {
		f.announces[announce.origin]--
		if f.announces[announce.origin] <= 0 {
			delete(f.announces, announce.origin)
		}
		delete(f.fetching, hash)
	}

	// Remove any pending completion requests and decrement the DOS counters
	for _, announce := range f.fetched[hash] {
		f.announces[announce.origin]--
		if f.announces[announce.origin] <= 0 {
			delete(f.announces, announce.origin)
		}
	}
	delete(f.fetched, hash)

	// Remove any pending completions and decrement the DOS counters
	if announce := f.completing[hash]; announce != nil {
		f.announces[announce.origin]--
		if f.announces[announce.origin] <= 0 {
			delete(f.announces, announce.origin)
		}
		delete(f.completing, hash)
	}

	// Notify witness manager to forget any pending state
	f.wm.forget(hash)
}

// forgetBlock removes all traces of a queued block from the fetcher's internal
// state.
func (f *BlockFetcher) forgetBlock(hash common.Hash) {
	f.mu.Lock()
	insert := f.queued[hash]
	if insert != nil {
		f.queues[insert.origin]--
		if f.queues[insert.origin] == 0 {
			delete(f.queues, insert.origin)
		}
		delete(f.queued, hash)
	}
	f.mu.Unlock()

	if insert != nil {
		// Notify witness manager only if we actually removed it from the queue
		// Note: enqueue and done cases already call wm.forget, so this might be redundant
		// Let's keep it for cases where forgetBlock is called directly after checking getBlock/getHeader
		f.wm.forget(hash)
	}
}
