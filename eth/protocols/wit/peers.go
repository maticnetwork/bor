package wit

import (
	"sync"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
)

const (
	// maxKnownWitnesses is the maximum number of witness hashes to keep in the known list
	maxKnownWitnesses = 1000

	// maxQueuedWitnesses is the maximum number of witness propagations to queue up before
	// dropping broadcasts
	maxQueuedWitnesses = 10

	// maxQueuedWitnessAnns is the maximum number of witness announcements to queue up before
	// dropping broadcasts
	maxQueuedWitnessAnns = 10
)

// Peer is a collection of relevant information we have about a `wit` peer.
type Peer struct {
	id string // Unique ID for the peer, cached

	*p2p.Peer                   // The embedded P2P package peer
	rw        p2p.MsgReadWriter // Input/output streams for witness protocol
	version   uint              // Protocol version negotiated

	logger log.Logger // Contextual logger with the peer id injected

	// PSP - update witBroadcast and witAnnounce structs to use the actuall witness structs
	// as we do in the eth package
	knownWitnesses *knownCache        // Set of witness hashes known to be known by this peer
	witBroadcast   chan []common.Hash // Queue of witness hashes to broadcast to this peer
	witAnnounce    chan []common.Hash // Queue of witness announcements to this peer

	reqDispatch chan *request  // Dispatch channel to send witness requests and track them until fulfillment
	reqCancel   chan *cancel   // Dispatch channel to cancel pending witness requests
	resDispatch chan *response // Dispatch channel to fulfill witness requests

	term chan struct{} // Termination channel to stop the broadcaster
	// PSP - review all the instances of the lock and unlock
	// and see if we can use a more efficient locking strategy
	lock sync.RWMutex // Mutex protecting the internal fields
}

// NewPeer creates a new WIT peer and starts its background processes.
func NewPeer(id string, p2pPeer *p2p.Peer, rw p2p.MsgReadWriter, version uint, logger log.Logger) *Peer {
	peer := &Peer{
		id:             id,
		Peer:           p2pPeer,
		rw:             rw,
		version:        version,
		logger:         logger.With("peer", id),
		knownWitnesses: newKnownCache(maxKnownWitnesses),
		witBroadcast:   make(chan []common.Hash, maxQueuedWitnesses),
		witAnnounce:    make(chan []common.Hash, maxQueuedWitnessAnns),
		reqDispatch:    make(chan *request),
		reqCancel:      make(chan *cancel),
		resDispatch:    make(chan *response),

		term: make(chan struct{}),
	}

	// Start background handlers
	go peer.witnessPropagator()
	go peer.requestHandler()

	return peer
}

// Close signals the broadcast goroutine to terminate. Only ever call this if
// you created the peer yourself via NewPeer. Otherwise let whoever created it
// clean it up!
func (p *Peer) Close() {
	close(p.term)
}

// ID retrieves the peer's unique identifier.
func (p *Peer) ID() string {
	return p.id
}

// Version retrieves the peer's negotiated `wit` protocol version.
func (p *Peer) Version() uint {
	return p.version
}

// Log overrides the P2P logger with the higher level one containing only the id.
func (p *Peer) Log() log.Logger {
	return p.logger
}

// KnownWitnesses retrieves the set of witness hashes known to be known by this peer.
func (p *Peer) KnownWitnesses() *knownCache {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.knownWitnesses
}

// AddKnownWitnesses adds a list of witness hashes to the set of known witnesses.
func (p *Peer) AddKnownWitnesses(hashes ...common.Hash) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.knownWitnesses.Add(hashes...)
}

// KnownWitnessesCount returns the number of known witness hashes.
func (p *Peer) KnownWitnessesCount() int {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.knownWitnesses.Cardinality()
}

// KnownWitnessesContains checks if a witness hash is known to be known by this peer.
func (p *Peer) KnownWitnessesContains(hash common.Hash) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.knownWitnesses.Contains(hash)
}

// knownCache is a cache for known hashes.
type knownCache struct {
	hashes mapset.Set[common.Hash]
	max    int
}

// newKnownCache creates a new knownCache with a max capacity.
func newKnownCache(max int) *knownCache {
	return &knownCache{
		max:    max,
		hashes: mapset.NewSet[common.Hash](),
	}
}

// Add adds a list of elements to the set.
func (k *knownCache) Add(hashes ...common.Hash) {
	for k.hashes.Cardinality() > max(0, k.max-len(hashes)) {
		k.hashes.Pop()
	}
	for _, hash := range hashes {
		k.hashes.Add(hash)
	}
}

// Contains returns whether the given item is in the set.
func (k *knownCache) Contains(hash common.Hash) bool {
	return k.hashes.Contains(hash)
}

// Cardinality returns the number of elements in the set.
func (k *knownCache) Cardinality() int {
	return k.hashes.Cardinality()
}
