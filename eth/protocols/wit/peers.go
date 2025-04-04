package wit

import (
	"sync"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rlp"
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

	knownWitnesses *knownCache             // Set of witness hashes (`witness.Headers[0].Hash()`) known to be known by this peer
	queuedWitness  chan *stateless.Witness // Queue of witness to broadcast to this peer
	// queuedWitnessAnns    chan *stateless.Witness // Queue of witness announcements to this peer

	reqDispatch chan *request  // Dispatch channel to send witness requests and track them until fulfillment
	reqCancel   chan *cancel   // Dispatch channel to cancel pending witness requests
	resDispatch chan *response // Dispatch channel to fulfill witness requests

	term chan struct{} // Termination channel to stop the broadcaster
	// PSP - review all the instances of the lock and unlock
	// and see if we can use a more efficient locking strategy
	lock sync.RWMutex // Mutex protecting the internal fields
}

// NewPeer creates a new WIT peer and starts its background processes.
func NewPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter, logger log.Logger) *Peer {
	id := p.ID().String()
	peer := &Peer{
		id:             id,
		Peer:           p,
		rw:             rw,
		version:        version,
		logger:         logger.With("peer", id),
		knownWitnesses: newKnownCache(maxKnownWitnesses),
		queuedWitness:  make(chan *stateless.Witness, maxQueuedWitnesses),
		// queuedWitnessAnns:    make(chan *stateless.Witness, maxQueuedWitnessAnns),
		reqDispatch: make(chan *request),
		reqCancel:   make(chan *cancel),
		resDispatch: make(chan *response),

		term: make(chan struct{}),
	}

	// Start background handlers
	go peer.broadcastWitness()
	go peer.dispatcher()

	return peer
}

// sendWitness sends witness to the peer
func (p *Peer) sendNewWitness(witness *stateless.Witness) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.knownWitnesses.Add(witness)

	return p2p.Send(p.rw, MsgWitness, &NewWitnessPacket{
		Witness: witness,
	})
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

// AddKnownWitnesses adds a witness hash to the set of known witness hashes.
func (p *Peer) AddKnownWitness(witness *stateless.Witness) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.knownWitnesses.Add(witness)
}

// KnownWitnessesCount returns the number of known witness.
func (p *Peer) KnownWitnessesCount() int {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.knownWitnesses.Cardinality()
}

// KnownWitnessesContains checks if a witness is known to be known by this peer.
func (p *Peer) KnownWitnessesContains(witness *stateless.Witness) bool {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.knownWitnesses.Contains(witness)
}

// ReplyWitnessRLP is the response to GetWitness
func (p *Peer) ReplyWitnessRLP(requestID uint64, witnesses []rlp.RawValue) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Send the response
	return p2p.Send(p.rw, MsgWitness, &WitnessPacketRLPPacket{
		RequestId:             requestID,
		WitnessPacketResponse: witnesses,
	})
}

// knownCache is a cache for known witness, identified by the hash of the parent witness block.
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

// Add adds a witness to the set.
func (k *knownCache) Add(witness *stateless.Witness) {
	for k.hashes.Cardinality() > max(0, k.max-1) {
		k.hashes.Pop()
	}
	k.hashes.Add(witness.Headers[0].Hash())
}

// Contains returns whether the given item is in the set.
func (k *knownCache) Contains(withess *stateless.Witness) bool {
	return k.hashes.Contains(withess.Headers[0].Hash())
}

// Cardinality returns the number of elements in the set.
func (k *knownCache) Cardinality() int {
	return k.hashes.Cardinality()
}
