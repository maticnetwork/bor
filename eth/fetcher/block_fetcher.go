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
	"github.com/ethereum/go-ethereum/rlp"
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

	headerFetchMeter = metrics.NewRegisteredMeter("eth/fetcher/block/headers", nil)
	bodyFetchMeter   = metrics.NewRegisteredMeter("eth/fetcher/block/bodies", nil)

	headerFilterInMeter  = metrics.NewRegisteredMeter("eth/fetcher/block/filter/headers/in", nil)
	headerFilterOutMeter = metrics.NewRegisteredMeter("eth/fetcher/block/filter/headers/out", nil)
	bodyFilterInMeter    = metrics.NewRegisteredMeter("eth/fetcher/block/filter/bodies/in", nil)
	bodyFilterOutMeter   = metrics.NewRegisteredMeter("eth/fetcher/block/filter/bodies/out", nil)

	witnessFetchMeter = metrics.NewRegisteredMeter("eth/fetcher/block/witnesses", nil)
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
type witnessRequesterFn func([]common.Hash, chan *wit.Response) (*wit.Request, error)

// headerVerifierFn is a callback type to verify a block's header for fast propagation.
type headerVerifierFn func(header *types.Header) error

// blockBroadcasterFn is a callback type for broadcasting a block to connected peers.
type blockBroadcasterFn func(block *types.Block, propagate bool)

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

// witnessFilterTask represents a batch of block witnesses needing fetcher filtering.
type witnessFilterTask struct {
	peer          string               // The source peer of block witnesses
	witnesses     []*stateless.Witness // Collection of witnesses per block
	time          time.Time            // Arrival time of the witnesses
	announcedTime time.Time            // Announcement time of the availability of the block
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
	notify            chan *blockAnnounce
	inject            chan *blockOrHeaderInject
	injectNeedWitness chan *injectBlockNeedWitnessMsg // New channel
	injectWitnessCh   chan *injectedWitnessMsg        // Channel for injected witnesses
	enqueueCh         chan *enqueueRequest            // Channel for safe enqueue calls

	headerFilter  chan chan *headerFilterTask
	bodyFilter    chan chan *bodyFilterTask
	witnessFilter chan chan *witnessFilterTask

	done chan common.Hash
	quit chan struct{}

	// Announce states
	announces  map[string]int                   // Per peer blockAnnounce counts to prevent memory exhaustion
	announced  map[common.Hash][]*blockAnnounce // Announced blocks, scheduled for fetching
	fetching   map[common.Hash]*blockAnnounce   // Announced blocks, currently fetching
	fetched    map[common.Hash][]*blockAnnounce // Blocks with headers fetched, scheduled for body retrieval
	completing map[common.Hash]*blockAnnounce   // Blocks with headers, currently body-completing
	witnessing map[common.Hash]*blockAnnounce   // Blocks with bodies, currently witness-completing

	// Block cache
	queue             *prque.Prque[int64, *blockOrHeaderInject] // Queue containing the import operations (block number sorted)
	queues            map[string]int                            // Per peer block counts to prevent memory exhaustion
	queued            map[common.Hash]*blockOrHeaderInject      // Set of already queued blocks (to dedup imports)
	pendingWitness    map[common.Hash]*blockOrHeaderInject      // Blocks fully assembled but waiting for witness
	receivedWitnesses map[common.Hash]*injectedWitnessMsg       // Witnesses received via broadcast, waiting for block body

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
	return &BlockFetcher{
		light:               light,
		notify:              make(chan *blockAnnounce),
		inject:              make(chan *blockOrHeaderInject),
		injectNeedWitness:   make(chan *injectBlockNeedWitnessMsg, 100),
		injectWitnessCh:     make(chan *injectedWitnessMsg, 100),
		enqueueCh:           make(chan *enqueueRequest, 100),
		headerFilter:        make(chan chan *headerFilterTask),
		bodyFilter:          make(chan chan *bodyFilterTask),
		witnessFilter:       make(chan chan *witnessFilterTask),
		done:                make(chan common.Hash),
		quit:                make(chan struct{}),
		announces:           make(map[string]int),
		announced:           make(map[common.Hash][]*blockAnnounce),
		fetching:            make(map[common.Hash]*blockAnnounce),
		fetched:             make(map[common.Hash][]*blockAnnounce),
		completing:          make(map[common.Hash]*blockAnnounce),
		witnessing:          make(map[common.Hash]*blockAnnounce),
		queue:               prque.New[int64, *blockOrHeaderInject](nil),
		queues:              make(map[string]int),
		queued:              make(map[common.Hash]*blockOrHeaderInject),
		pendingWitness:      make(map[common.Hash]*blockOrHeaderInject),
		receivedWitnesses:   make(map[common.Hash]*injectedWitnessMsg),
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
}

// Start boots up the announcement based synchroniser, accepting and processing
// hash notifications and block fetches until termination requested.
func (f *BlockFetcher) Start() {
	go f.loop()
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

	select {
	case f.injectNeedWitness <- msg:
		log.Trace("Injecting block needing witness fetch", "peer", origin, "hash", block.Hash())
		return nil
	case <-f.quit:
		return errTerminated
	default:
		// This case handles if the channel buffer is full.
		log.Warn("Block fetcher injectNeedWitness channel full, dropping block injection", "hash", block.Hash())
		return errors.New("fetcher injectNeedWitness channel full")
	}
}

// InjectWitness injects a witness received via broadcast into the fetcher.
func (f *BlockFetcher) InjectWitness(peer string, witness *stateless.Witness) error {
	msg := &injectedWitnessMsg{
		peer:    peer,
		witness: witness,
		time:    time.Now(),
	}
	select {
	case f.injectWitnessCh <- msg:
		log.Trace("Injecting witness from broadcast", "peer", peer, "hash", witness.Header().Hash())
		return nil
	case <-f.quit:
		return errTerminated
	default:
		// This case handles if the channel buffer is full.
		log.Warn("Block fetcher injectWitnessCh channel full, dropping witness injection", "hash", witness.Header().Hash())
		return errors.New("fetcher injectWitnessCh channel full")
	}
}

// FilterHeaders extracts all the headers that were explicitly requested by the fetcher,
// returning those that should be handled differently.
func (f *BlockFetcher) FilterHeaders(peer string, headers []*types.Header, time time.Time, announcedAt time.Time) []*types.Header {
	log.Trace("Filtering headers", "peer", peer, "headers", len(headers))

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
	log.Trace("Filtering bodies", "peer", peer, "txs", len(transactions), "uncles", len(uncles))

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

// FilterWitnesses extracts all the block witnesses that were explicitly requested by the fetcher,
// returning those that should be handled differently.
func (f *BlockFetcher) FilterWitnesses(peer string, witnesses []*stateless.Witness, time time.Time, announcedAt time.Time) []*stateless.Witness {
	log.Trace("Filtering witnesses", "peer", peer, "witnesses", len(witnesses))

	// Send the filter channel to the fetcher
	filter := make(chan *witnessFilterTask)

	select {
	case f.witnessFilter <- filter:
	case <-f.quit:
		return nil
	}
	// Request the filtering of the witness list
	select {
	case filter <- &witnessFilterTask{peer: peer, witnesses: witnesses, time: time, announcedTime: announcedAt}:
	case <-f.quit:
		return nil
	}
	// Retrieve the witnesses remaining after filtering
	select {
	case task := <-filter:
		return task.witnesses
	case <-f.quit:
		return nil
	}
}

// Loop is the main fetcher loop, checking and processing various notification
// events.
func (f *BlockFetcher) loop() {
	// Iterate the block fetching until a quit is requested
	var (
		fetchTimer    = time.NewTimer(0)
		completeTimer = time.NewTimer(0)
		witnessTimer  = time.NewTimer(0)
	)

	<-fetchTimer.C // clear out the channel
	<-completeTimer.C
	<-witnessTimer.C

	defer fetchTimer.Stop()
	defer completeTimer.Stop()
	defer witnessTimer.Stop()

	for {
		// Clean up any expired block fetches
		for hash, announce := range f.fetching {
			if time.Since(announce.time) > fetchTimeout {
				f.forgetHash(hash)
			}
		}
		// Import any queued blocks that could potentially fit
		height := f.chainHeight()

		for !f.queue.Empty() {
			op := f.queue.PopItem()

			hash := op.hash()
			if f.queueChangeHook != nil {
				f.queueChangeHook(hash, false)
			}
			// If too high up the chain or phase, continue later
			number := op.number()
			if number > height+1 {
				f.queue.Push(op, -int64(number))

				if f.queueChangeHook != nil {
					f.queueChangeHook(hash, true)
				}

				break
			}
			// Otherwise if fresh and still unknown, try and import
			if (number+maxUncleDist < height) || (f.light && f.getHeader(hash) != nil) || (!f.light && f.getBlock(hash) != nil) {
				f.forgetBlock(hash)
				continue
			}

			if f.light {
				f.importHeaders(op.origin, op.header)
			} else {
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
				break
			}
			// If we have a valid block number, check that it's potentially useful
			if dist := int64(notification.number) - int64(f.chainHeight()); dist < -maxUncleDist || dist > maxQueueDist {
				log.Debug("Peer discarded announcement", "peer", notification.origin, "number", notification.number, "hash", notification.hash, "distance", dist)
				blockAnnounceDropMeter.Mark(1)

				break
			}
			// All is well, schedule the announce if block's not yet downloading
			if _, ok := f.fetching[notification.hash]; ok {
				break
			}

			if _, ok := f.completing[notification.hash]; ok {
				break
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
			// A direct block insertion was requested, try and fill any pending gaps
			blockBroadcastInMeter.Mark(1)

			// Now only direct block injection is allowed, drop the header injection
			// here silently if we receive.
			// Also, witness cannot be injected directly this way.
			if f.light || op.witness != nil {
				continue
			}

			// If witnesses are strictly required, discard directly injected blocks without one.
			if f.requireWitness && op.block != nil && op.witness == nil {
				log.Warn("Discarding injected block, witness required but not provided", "hash", op.hash(), "number", op.number(), "origin", op.origin)
				continue
			}

			f.enqueue(op.origin, nil, op.block, nil)

		case msg := <-f.injectNeedWitness:
			// Handle injected block that requires witness fetch
			hash := msg.block.Hash()
			number := msg.block.NumberU64()
			log.Trace("Processing injected block needing witness", "peer", msg.origin, "number", number, "hash", hash)

			// --- Perform necessary checks (similar to enqueue) ---

			// Check if already processed/pending
			if _, ok := f.queued[hash]; ok {
				log.Trace("Injected block already queued", "hash", hash)
				continue
			}
			if _, ok := f.pendingWitness[hash]; ok {
				log.Trace("Injected block already pending witness", "hash", hash)
				continue
			}
			// Check if block is actually known locally
			if f.getBlock(hash) != nil {
				log.Trace("Injected block already known", "hash", hash)
				continue
			}

			// Check distance
			if dist := int64(number) - int64(f.chainHeight()); dist < -maxUncleDist || dist > maxQueueDist {
				log.Debug("Discarded injected block, too far away", "peer", msg.origin, "number", number, "hash", hash, "distance", dist)
				// Doesn't count towards DOS limits as it's injected, just drop.
				continue
			}

			// Check if witness fetcher was provided
			if msg.fetchWitness == nil {
				log.Error("Injected block message missing fetchWitness function", "hash", hash, "origin", msg.origin)
				continue // Cannot proceed without fetcher
			}

			// --- Check cache before potentially fetching ---
			op := &blockOrHeaderInject{
				origin: msg.origin,
				block:  msg.block,
			}
			if cachedWitnessMsg, exists := f.receivedWitnesses[hash]; exists {
				log.Trace("Found cached witness for injected block", "hash", hash, "number", number)
				op.witness = cachedWitnessMsg.witness
				// Update timestamps using cached witness time
				if op.block != nil {
					if cachedWitnessMsg.time.After(op.block.ReceivedAt) {
						op.block.ReceivedAt = cachedWitnessMsg.time
					}
					// Keep original AnnouncedAt if available
				}
				// Enqueue immediately
				req := &enqueueRequest{op: op}
				select {
				case f.enqueueCh <- req:
				case <-f.quit:
					log.Trace("Fetcher quit while sending enqueue request for cached witness")
				}
				// Clean up cache
				delete(f.receivedWitnesses, hash)
				continue // Skip adding to pending/witnessing
			}

			// --- Add state to trigger witness fetch (if not cached) ---
			announce := &blockAnnounce{ // Create minimal announce struct
				origin:       msg.origin,
				hash:         hash,
				number:       number,
				time:         msg.time, // Use time from message
				fetchWitness: msg.fetchWitness,
				// fetchHeader/fetchBodies are nil, not needed for this path
			}

			f.pendingWitness[hash] = op
			f.witnessing[hash] = announce

			// Ensure the witness timer is scheduled
			f.rescheduleWitness(witnessTimer)

		case req := <-f.enqueueCh:
			// A fetcher goroutine completed fetching block + witness, enqueue safely.
			if req.op == nil {
				log.Error("Received nil enqueue request")
				continue
			}
			// Call the internal enqueue function which handles checks and queue push.
			// Pass nil for header as we only handle full blocks here.
			f.enqueue(req.op.origin, nil, req.op.block, req.op.witness)

		case injectedWitness := <-f.injectWitnessCh:
			// A witness was injected directly (likely from broadcast)
			hash := injectedWitness.witness.Header().Hash()
			log.Trace("Processing injected witness", "peer", injectedWitness.peer, "hash", hash)

			// Check if we are already tracking this witness (e.g., duplicate broadcast)
			if _, exists := f.receivedWitnesses[hash]; exists {
				log.Trace("Duplicate injected witness", "hash", hash)
				continue // Ignore duplicate
			}

			// Check if the block corresponding to this witness is currently pending
			if op, pending := f.pendingWitness[hash]; pending {
				log.Trace("Found matching pending block for injected witness", "hash", hash, "number", op.number())
				// Attach the witness
				op.witness = injectedWitness.witness

				// Update timestamps if block exists
				if op.block != nil {
					// Use the witness arrival time if it's later than the block's ReceivedAt
					if injectedWitness.time.After(op.block.ReceivedAt) {
						op.block.ReceivedAt = injectedWitness.time
					}
					// We don't have AnnouncedAt for the witness broadcast easily, keep block's AnnouncedAt
				}

				// Send for enqueuing
				req := &enqueueRequest{op: op}
				select {
				case f.enqueueCh <- req:
					// Request sent successfully
				case <-f.quit:
					log.Trace("Fetcher quit while sending enqueue request for injected witness")
				}

				// Clean up states
				delete(f.pendingWitness, hash)
				// Also remove from witnessing map if it was there
				if announce := f.witnessing[hash]; announce != nil {
					// Decrement announce counts if needed (optional, as this witness didn't come from announce)
					// f.announces[announce.origin]-- ... logic ...
					delete(f.witnessing, hash)
				}
			} else {
				// Block is not yet pending, cache the witness
				log.Trace("No matching pending block, caching injected witness", "hash", hash)
				f.receivedWitnesses[hash] = injectedWitness
				// TODO: Add logic for TTL or cache eviction? For now, keep indefinitely.
			}

		case hash := <-f.done:
			// A pending import finished, remove all traces of the notification
			f.forgetHash(hash)
			f.forgetBlock(hash)

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
					// Pick a random peer to retrieve from, reset all others
					announce := announces[rand.Intn(len(announces))]

					f.forgetHash(hash)

					// If the block still didn't arrive, queue for fetching
					if (f.light && f.getHeader(hash) == nil) || (!f.light && f.getBlock(hash) == nil) {
						request[announce.origin] = append(request[announce.origin], hash)
						f.fetching[hash] = announce
					}
				}
			}
			// Send out all block header requests
			for peer, hashes := range request {
				log.Trace("Fetching scheduled headers", "peer", peer, "list", hashes)

				// Create a closure of the fetch and schedule in on a new thread
				fetchHeader, hashes, announcedAt := f.fetching[hashes[0]].fetchHeader, hashes, f.fetching[hashes[0]].time
				go func(peer string) {
					if f.fetchingHook != nil {
						f.fetchingHook(hashes)
					}

					for _, hash := range hashes {
						headerFetchMeter.Mark(1)

						go func(hash common.Hash) {
							resCh := make(chan *eth.Response)

							req, err := fetchHeader(hash, resCh)
							if err != nil {
								return // Legacy code, yolo
							}
							defer req.Close()

							timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
							defer timeout.Stop()

							select {
							case res := <-resCh:
								res.Done <- nil
								f.FilterHeaders(peer, *res.Res.(*eth.BlockHeadersRequest), time.Now(), announcedAt)

							case <-timeout.C:
								// The peer didn't respond in time. The request
								// was already rescheduled at this point, we were
								// waiting for a catchup. With an unresponsive
								// peer however, it's a protocol violation.
								f.dropPeer(peer)
							}
						}(hash)
					}
				}(peer)
			}
			// Schedule the next fetch if blocks are still pending
			f.rescheduleFetch(fetchTimer)

		case <-completeTimer.C:
			// At least one header's timer ran out, retrieve everything
			request := make(map[string][]common.Hash)

			for hash, announces := range f.fetched {
				// Pick a random peer to retrieve from, reset all others
				announce := announces[rand.Intn(len(announces))]

				f.forgetHash(hash)

				// If the block still didn't arrive, queue for completion
				if f.getBlock(hash) == nil {
					request[announce.origin] = append(request[announce.origin], hash)
					f.completing[hash] = announce
				}
			}
			// Send out all block body requests
			for peer, hashes := range request {
				log.Trace("Fetching scheduled bodies", "peer", peer, "list", hashes)

				// Create a closure of the fetch and schedule in on a new thread
				if f.completingHook != nil {
					f.completingHook(hashes)
				}

				fetchBodies := f.completing[hashes[0]].fetchBodies
				bodyFetchMeter.Mark(int64(len(hashes)))
				announcedAt := f.completing[hashes[0]].time

				go func(peer string, hashes []common.Hash) {
					resCh := make(chan *eth.Response)

					req, err := fetchBodies(hashes, resCh)
					if err != nil {
						return // Legacy code, yolo
					}
					defer req.Close()

					timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
					defer timeout.Stop()

					select {
					case res := <-resCh:
						res.Done <- nil
						// Ignoring withdrawals here, since the block fetcher is not used post-merge.
						txs, uncles, _, _ := res.Res.(*eth.BlockBodiesResponse).Unpack()
						f.FilterBodies(peer, txs, uncles, time.Now(), announcedAt)

					case <-timeout.C:
						// The peer didn't respond in time. The request
						// was already rescheduled at this point, we were
						// waiting for a catchup. With an unresponsive
						// peer however, it's a protocol violation.
						f.dropPeer(peer)
					}
				}(peer, hashes)
			}
			// Schedule the next fetch if blocks are still pending
			f.rescheduleComplete(completeTimer)

		case <-witnessTimer.C:
			// At least one body's timer ran out, retrieve everything
			// Map from peer ID -> map of block hash -> announce struct
			request := make(map[string]map[common.Hash]*blockAnnounce)

			// Iterate over blocks waiting for witness fetch to be initiated.
			// This includes non-empty blocks whose bodies arrived, and empty blocks
			// identified in FilterHeaders that require witnesses.
			for hash, announce := range f.witnessing {
				// Check if witness is already cached
				if cachedWitnessMsg, exists := f.receivedWitnesses[hash]; exists {
					if op, pending := f.pendingWitness[hash]; pending {
						log.Trace("Found cached witness for block entering witness timer", "hash", hash, "number", op.number())
						op.witness = cachedWitnessMsg.witness
						// Update timestamps
						if op.block != nil {
							if cachedWitnessMsg.time.After(op.block.ReceivedAt) {
								op.block.ReceivedAt = cachedWitnessMsg.time
							}
							// Use announce time if available for AnnouncedAt
							if announce != nil {
								op.block.AnnouncedAt = &announce.time
							}
						}
						// Enqueue immediately
						req := &enqueueRequest{op: op}
						select {
						case f.enqueueCh <- req:
						case <-f.quit:
						}
						// Clean up states
						delete(f.pendingWitness, hash)
						delete(f.witnessing, hash)
						delete(f.receivedWitnesses, hash)
						continue // Skip adding to fetch request
					} else {
						// Witness cached, but block not pending? Should not happen.
						log.Warn("Cached witness found but block not pending in witness timer", "hash", hash)
						delete(f.receivedWitnesses, hash) // Clean cache anyway
						delete(f.witnessing, hash)        // Clean witnessing state
						continue
					}
				}

				// Add to request map, capturing the announce struct
				if _, ok := request[announce.origin]; !ok {
					request[announce.origin] = make(map[common.Hash]*blockAnnounce)
				}
				request[announce.origin][hash] = announce

				// Remove from witnessing immediately to prevent duplicate requests
				// while this fetch is in flight.
				delete(f.witnessing, hash)
			}
			// Send out all block witness requests
			for peer, hashAnnounceMap := range request {
				// Collect hashes for logging and potential batching if protocol supported it
				hashesToFetch := make([]common.Hash, 0, len(hashAnnounceMap))
				for hash := range hashAnnounceMap {
					hashesToFetch = append(hashesToFetch, hash)
				}
				if len(hashesToFetch) == 0 {
					continue
				}
				log.Trace("Fetching scheduled witnesses", "peer", peer, "list", hashesToFetch)

				// Create a closure of the fetch and schedule in on a new thread
				if f.completingHook != nil {
					f.completingHook(hashesToFetch)
				}

				// Process each hash for the peer individually
				go func(peer string, currentHashAnnounceMap map[common.Hash]*blockAnnounce) {
					for hash, announce := range currentHashAnnounceMap {
						// Ensure we have a valid announce and fetchWitness function
						if announce == nil || announce.fetchWitness == nil {
							log.Warn("Missing announce or fetchWitness for witness request", "peer", peer, "hash", hash)
							continue
						}

						announcedAt := announce.time // Get the original announce time HERE
						witnessFetchMeter.Mark(1)
						go func(hash common.Hash, announce *blockAnnounce, announcedAt time.Time) {
							resCh := make(chan *wit.Response)
							req, err := announce.fetchWitness([]common.Hash{hash}, resCh) // Use captured announce
							if err != nil {
								return // Legacy code, yolo
							}
							defer req.Close()

							timeout := time.NewTimer(2 * fetchTimeout) // 2x leeway before dropping the peer
							defer timeout.Stop()

							select {
							case res := <-resCh:
								res.Done <- nil
								// Assuming NewWitnessPacket contains only one witness.
								// If it can contain multiple, this logic needs adjustment.
								packet := res.Res.(*wit.WitnessPacketRLPPacket)

								if len(packet.WitnessPacketResponse) != 1 {
									log.Warn("Received multiple witnesses for a single block", "peer", peer, "hash", hash, "count", len(packet.WitnessPacketResponse))
									return
								}

								witness := &stateless.Witness{}
								err := rlp.DecodeBytes(packet.WitnessPacketResponse[0], witness)
								if err != nil {
									log.Debug("Failed to decode witness", "error", err)
									return
								}

								if op := f.pendingWitness[hash]; op != nil {
									// Check if the announce struct (passed into the goroutine)
									// is still valid and matches the peer who responded.
									// We don't check f.witnessing map again, as we deleted the entry
									// when initiating the request.
									if announce != nil && announce.origin == peer {
										log.Trace("Witness received, queuing block for import", "peer", peer, "number", op.number(), "hash", hash)

										// Attach witness and enqueue
										op.witness = witness

										// Update timestamps on the block
										if op.block != nil {
											op.block.ReceivedAt = time.Now() // Use witness arrival time
											op.block.AnnouncedAt = &announcedAt
										}

										// Create the enqueue request and send it back to the main loop
										req := &enqueueRequest{op: op}
										select {
										case f.enqueueCh <- req:
											// Request sent successfully
										case <-f.quit:
											log.Trace("Fetcher quit while sending enqueue request")
										}

										// Clean up states
										delete(f.pendingWitness, hash)
									} else {
										// Witness received, but origin doesn't match or announce missing.
										log.Trace("Witness received, but announce origin mismatch or missing", "peer", peer, "hash", hash)
										// Do not enqueue, let forgetHash eventually clean up pendingWitness entry.
									}
								} else {
									// Witness received, but block is not pending witness (e.g., already imported or timed out)
									log.Trace("Witness received, but block not pending", "peer", peer, "hash", hash)
								}
								// Note: We are not calling f.FilterWitnesses here anymore. If unsolicited witnesses need filtering,
								// a separate mechanism/channel might be needed.

							case <-timeout.C:
								// The peer didn't respond in time. The request
								// was already rescheduled at this point, we were
								// waiting for a catchup. With an unresponsive
								// peer however, it's a protocol violation.
								f.dropPeer(peer)
							}
						}(hash, announce, announcedAt)
					}
				}(peer, hashAnnounceMap)
			}
			// Schedule the next fetch if blocks are still pending
			f.rescheduleWitness(witnessTimer)

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
				if announce := f.fetching[hash]; announce != nil && announce.origin == task.peer && f.fetched[hash] == nil && f.completing[hash] == nil && f.queued[hash] == nil {
					// If the delivered header does not match the promised number, drop the announcer
					if header.Number.Uint64() != announce.number {
						log.Trace("Invalid block number fetched", "peer", announce.origin, "hash", header.Hash(), "announced", announce.number, "provided", header.Number)
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
							log.Trace("Block empty, checking witness requirement", "peer", announce.origin, "number", header.Number, "hash", header.Hash())

							block := types.NewBlockWithHeader(header)
							block.ReceivedAt = task.time
							block.AnnouncedAt = &task.announcedTime

							// Check if a witness is required for this empty block
							if announce.fetchWitness != nil {
								// Witness required: Check cache first.
								op := &blockOrHeaderInject{
									origin: announce.origin,
									block:  block,
								}
								if cachedWitnessMsg, exists := f.receivedWitnesses[hash]; exists {
									log.Trace("Found cached witness for empty block", "hash", hash, "number", header.Number)
									op.witness = cachedWitnessMsg.witness
									// Update timestamps
									if op.block != nil {
										if cachedWitnessMsg.time.After(op.block.ReceivedAt) {
											op.block.ReceivedAt = cachedWitnessMsg.time
										}
										if announce != nil {
											op.block.AnnouncedAt = &announce.time
										}
									}
									// Enqueue immediately
									req := &enqueueRequest{op: op}
									select {
									case f.enqueueCh <- req:
									case <-f.quit:
									}
									// Clean up cache and fetching state
									delete(f.receivedWitnesses, hash)
									delete(f.fetching, hash) // Remove from fetching
									continue                 // Skip adding to witnessing state
								}

								// Witness not cached, proceed to pending/witnessing
								log.Trace("Empty block requires witness, pending fetch", "peer", announce.origin, "number", header.Number, "hash", header.Hash())
								f.pendingWitness[hash] = op
								f.witnessing[hash] = announce     // Move state
								delete(f.fetching, hash)          // Remove from fetching
								f.rescheduleWitness(witnessTimer) // Ensure witness fetch timer is updated
							} else {
								// Witness not required: Add to complete list for immediate enqueueing.
								log.Trace("Empty block doesn't require witness, queuing", "peer", announce.origin, "number", header.Number, "hash", header.Hash())
								complete = append(complete, block)
								// Move state out of fetching so forgetHash works correctly when block is done.
								// Use completing map temporarily.
								f.completing[hash] = announce
								delete(f.fetching, hash) // Remove from fetching state
							}
							continue // Move to the next header in the loop
						}
						// Otherwise (block not empty) add to the list of blocks needing completion (body fetch)
						incomplete = append(incomplete, announce)
					} else {
						log.Trace("Block already imported, discarding header", "peer", announce.origin, "number", header.Number, "hash", header.Hash())
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

				f.fetched[hash] = append(f.fetched[hash], announce)
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
					// Enqueue call should eventually lead to forgetHash which cleans up completing state.
				} else {
					// This case should not happen if logic above is correct.
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
			// abort early if there's nothing explicitly requested
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
						if f.queued[hash] != nil || f.pendingWitness[hash] != nil {
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
						// This check might be redundant if FilterHeaders already handled it, but good for safety.
						if f.getBlock(blockHash) != nil {
							f.forgetHash(blockHash)
						} else {
							// Assemble the block
							block := types.NewBlockWithHeader(announce.header).WithBody(types.Body{
								Transactions: task.transactions[i],
								Uncles:       task.uncles[i],
							})
							block.ReceivedAt = task.time
							block.AnnouncedAt = &task.announcedTime

							// If witness is needed, store in pendingWitness map, otherwise add to enqueue list
							if matchAnnounce.fetchWitness != nil {
								op := &blockOrHeaderInject{
									origin: announce.origin,
									block:  block,
								}
								// Check cache first
								if cachedWitnessMsg, exists := f.receivedWitnesses[blockHash]; exists {
									log.Trace("Found cached witness for block body", "hash", blockHash, "number", block.NumberU64())
									op.witness = cachedWitnessMsg.witness
									// Update timestamps
									if op.block != nil {
										if cachedWitnessMsg.time.After(op.block.ReceivedAt) {
											op.block.ReceivedAt = cachedWitnessMsg.time
										}
										if matchAnnounce != nil {
											op.block.AnnouncedAt = &matchAnnounce.time
										}
									}
									// Enqueue immediately
									req := &enqueueRequest{op: op}
									select {
									case f.enqueueCh <- req:
									case <-f.quit:
									}
									// Clean up cache and completing state
									delete(f.receivedWitnesses, blockHash)
									delete(f.completing, blockHash)
									// Matched block body handled, need to adjust loop variables
									task.transactions = append(task.transactions[:i], task.transactions[i+1:]...)
									task.uncles = append(task.uncles[:i], task.uncles[i+1:]...)
									i--
									continue // Move to next body
								}

								// Witness not cached, proceed to pending/witnessing
								log.Trace("Block body received, pending witness fetch", "peer", announce.origin, "number", block.NumberU64(), "hash", blockHash)
								f.pendingWitness[blockHash] = op
								// Move announce from completing to witnessing state
								f.witnessing[blockHash] = announce
								delete(f.completing, blockHash)
								// Don't add to 'blocks' list yet
							} else {
								log.Trace("Block body received, queuing for import", "peer", announce.origin, "number", block.NumberU64(), "hash", blockHash)
								blocks = append(blocks, block)
								// Keep in completing until enqueued, enqueue call below will handle moving state by calling forgetHash eventually.
							}
						}
						break // Found matching announce, move to next body
					}

					if matched {
						task.transactions = append(task.transactions[:i], task.transactions[i+1:]...)
						task.uncles = append(task.uncles[:i], task.uncles[i+1:]...)
						i--

						continue
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
					f.enqueue(announce.origin, nil, block, nil)
				} else {
					// This case should ideally not happen if logic above is correct
					log.Warn("Announce missing for completed block", "hash", block.Hash())
					f.forgetHash(block.Hash()) // Clean up any potential dangling state
				}
			}
		}
	}
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

// rescheduleWitness resets the specified witness timer to the next fetch timeout.
func (f *BlockFetcher) rescheduleWitness(witness *time.Timer) {
	// Short circuit if no bodies are fetched OR waiting for witness
	if len(f.witnessing) == 0 { // Check witnessing map instead of completing
		return
	}
	// Otherwise find the earliest expiring announcement
	earliest := time.Now()
	for _, announce := range f.witnessing { // Use witnessing map
		if earliest.After(announce.time) {
			earliest = announce.time
		}
	}

	witness.Reset(gatherSlack - time.Since(earliest)) // Use gatherSlack like complete timer? Or fetchTimeout? Let's keep gatherSlack.
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
		// This case should not happen with current logic, but defensively log if it does.
		log.Error("Enqueue called with nil header, block, and witness", "peer", peer)
		return
	}

	// Ensure the peer isn't DOSing us
	count := f.queues[peer] + 1
	if count > blockLimit {
		log.Debug("Discarded delivered header or block, exceeded allowance", "peer", peer, "number", number, "hash", hash, "limit", blockLimit)
		blockBroadcastDOSMeter.Mark(1)
		f.forgetHash(hash)

		return
	}
	// Discard any past or too distant blocks
	if dist := int64(number) - int64(f.chainHeight()); dist < -maxUncleDist || dist > maxQueueDist {
		log.Debug("Discarded delivered header or block, too far away", "peer", peer, "number", number, "hash", hash, "distance", dist)
		blockBroadcastDropMeter.Mark(1)
		f.forgetHash(hash)

		return
	}
	// Schedule the block for future importing
	if _, ok := f.queued[hash]; !ok {
		op := &blockOrHeaderInject{origin: peer}
		if header != nil {
			op.header = header
		} else if block != nil { // Prioritize block over header if both somehow provided
			op.block = block
			// Attach witness only if block is present
			op.witness = witness
		} else {
			// Already returned above if all are nil, this path shouldn't be hit.
			log.Error("Invalid state in enqueue: header and block are nil", "peer", peer, "hash", hash)
			return
		}

		f.queues[peer] = count
		f.queued[hash] = op
		f.queue.Push(op, -int64(number))

		if f.queueChangeHook != nil {
			f.queueChangeHook(hash, true)
		}

		log.Debug("Queued delivered header or block", "peer", peer, "number", number, "hash", hash, "queued", f.queue.Size())
	}

	// Remove any pending witness requests and decrement the DOS counters
	if announce := f.witnessing[hash]; announce != nil {
		f.announces[announce.origin]--
		if f.announces[announce.origin] <= 0 {
			delete(f.announces, announce.origin)
		}

		delete(f.witnessing, hash)
	}

	// Remove from pending witness map
	delete(f.pendingWitness, hash)
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

			go f.broadcastBlock(block, true)

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

		go f.broadcastBlock(block, false)

		// Invoke the testing hook if needed
		if f.importedHook != nil {
			f.importedHook(nil, block)
		}
	}()
}

// forgetHash removes all traces of a block announcement from the fetcher's
// internal state.
func (f *BlockFetcher) forgetHash(hash common.Hash) {
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

	// Remove any pending witness requests and decrement the DOS counters
	if announce := f.witnessing[hash]; announce != nil {
		f.announces[announce.origin]--
		if f.announces[announce.origin] <= 0 {
			delete(f.announces, announce.origin)
		}

		delete(f.witnessing, hash)
	}

	// Remove from received witness cache as it's no longer needed
	delete(f.receivedWitnesses, hash)
}

// forgetBlock removes all traces of a queued block from the fetcher's internal
// state.
func (f *BlockFetcher) forgetBlock(hash common.Hash) {
	if insert := f.queued[hash]; insert != nil {
		f.queues[insert.origin]--
		if f.queues[insert.origin] == 0 {
			delete(f.queues, insert.origin)
		}

		delete(f.queued, hash)
	}
}
