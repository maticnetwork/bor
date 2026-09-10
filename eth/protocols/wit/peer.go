package wit

import (
	"fmt"
	"math/rand"
	"sync"

	mapset "github.com/deckarep/golang-set/v2"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
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
	// dropping broadcasts. Bumped from 10 to 64 in WIT2 to absorb transitive-relay bursts;
	// each announcement is small (33 bytes per entry, 130 bytes signed) so the memory
	// footprint stays well under 10KB per peer.
	maxQueuedWitnessAnns = 64
)

// Peer is a collection of relevant information we have about a `wit` peer.
type Peer struct {
	id string // Unique ID for the peer, cached

	*p2p.Peer                   // The embedded P2P package peer
	rw        p2p.MsgReadWriter // Input/output streams for witness protocol
	version   uint              // Protocol version negotiated

	logger log.Logger // Contextual logger with the peer id injected

	knownWitnesses    *KnownCache                        // Witness hashes this peer is known to HAVE (body served, body broadcast received). Feeds body-fetch peer selection.
	knownAnnounces    *KnownCache                        // Witness hashes this peer has SEEN an announcement for, but not necessarily the body. Used only to suppress redundant announce relay.
	queuedWitness     chan *stateless.Witness            // Queue of witness to broadcast to this peer
	queuedWitnessAnns chan *NewWitnessHashesPacket       // Queue of unsigned witness announcements to this peer (WIT1)
	queuedSignedAnns  chan *SignedNewWitnessHashesPacket // Queue of signed witness announcements to this peer (WIT2)

	reqDispatch chan *request  // Dispatch channel to send witness requests and track them until fulfillment
	reqCancel   chan *cancel   // Dispatch channel to cancel pending witness requests
	resDispatch chan *response // Dispatch channel to fulfill witness requests

	term chan struct{} // Termination channel to stop the broadcaster
}

// NewPeer creates a new WIT peer and starts its background processes.
func NewPeer(version uint, p *p2p.Peer, rw p2p.MsgReadWriter, logger log.Logger) *Peer {
	id := p.ID().String()
	peer := &Peer{
		id:                id,
		Peer:              p,
		rw:                rw,
		version:           version,
		logger:            logger.With("peer", id),
		knownWitnesses:    newKnownCache(maxKnownWitnesses),
		knownAnnounces:    newKnownCache(maxKnownWitnesses),
		queuedWitness:     make(chan *stateless.Witness, maxQueuedWitnesses),
		queuedWitnessAnns: make(chan *NewWitnessHashesPacket, maxQueuedWitnessAnns),
		queuedSignedAnns:  make(chan *SignedNewWitnessHashesPacket, maxQueuedWitnessAnns),
		reqDispatch:       make(chan *request),
		reqCancel:         make(chan *cancel),
		resDispatch:       make(chan *response),

		term: make(chan struct{}),
	}

	// Start background handlers
	go peer.broadcastWitness()
	go peer.dispatcher()

	return peer
}

// sendWitness sends witness to the peer
func (p *Peer) sendNewWitness(witness *stateless.Witness) error {
	p.knownWitnesses.Add(witness.Header().Hash())

	return p2p.Send(p.rw, NewWitnessMsg, &NewWitnessPacket{
		Witness: witness,
	})
}

// sendNewWitnessHashes sends witness hashes to the peer
func (p *Peer) sendNewWitnessHashes(packet *NewWitnessHashesPacket) error {
	return p2p.Send(p.rw, NewWitnessHashesMsg, packet)
}

// sendSignedNewWitnessHashes sends signed witness announcements to the peer.
// Only valid for WIT2+ peers; the caller must check Version() before invoking.
func (p *Peer) sendSignedNewWitnessHashes(packet *SignedNewWitnessHashesPacket) error {
	return p2p.Send(p.rw, SignedNewWitnessHashesMsg, packet)
}

// AsyncSendNewWitness queues an entire witness for broadcast to the peer. The
// witness will be sent in the background to avoid blocking the caller. If the
// queue is full, the witness will be dropped.
func (p *Peer) AsyncSendNewWitness(witness *stateless.Witness) {
	log.Debug("AsyncSendNewWitness", "hash", witness.Header().Hash(), "peer", p.id)

	// Queue the witness for broadcast
	select {
	case p.queuedWitness <- witness:
		p.knownWitnesses.Add(witness.Header().Hash())

	default:
		p.logger.Debug("Dropped witness propagation.", "hash", witness.Header().Hash(), "peer", p.id)
	}
}

// AsyncSendNewWitnessHash queues witness hash for broadcast to the peer.
func (p *Peer) AsyncSendNewWitnessHash(hash common.Hash, number uint64) {
	// Queue the witness hashes for broadcast
	select {
	case p.queuedWitnessAnns <- &NewWitnessHashesPacket{
		Hashes:  []common.Hash{hash},
		Numbers: []uint64{number},
	}:
		p.knownWitnesses.Add(hash)
	default:
		p.logger.Debug("Dropped witness hashes propagation.", "hashes", hash, "peer", p.id)
	}
}

// AsyncSendSignedWitnessAnnouncement queues a BP-signed witness announcement
// for broadcast to the peer. The peer must speak WIT2 or higher; callers are
// responsible for checking Version(). The block hash is added to the peer's
// announce-known set (NOT the body-known set) so subsequent announce gossip
// on the same hash is suppressed, while body-fetch peer selection is not
// misled into asking this peer for bytes it does not yet have.
func (p *Peer) AsyncSendSignedWitnessAnnouncement(ann SignedWitnessAnnouncement) {
	if p.version < WIT2 {
		return
	}
	select {
	case p.queuedSignedAnns <- &SignedNewWitnessHashesPacket{
		Announcements: []SignedWitnessAnnouncement{ann},
	}:
		p.knownAnnounces.Add(ann.BlockHash)
	default:
		p.logger.Debug("Dropped signed witness announcement.", "blockHash", ann.BlockHash, "peer", p.id)
	}
}

// RequestWitness sends a request to the peer for witnesses by witness pages.
func (p *Peer) RequestWitness(witnessPages []WitnessPageRequest, sink chan *Response) (*Request, error) {
	log.Debug("Requesting witnesses", "peer", p.id, "count", len(witnessPages))
	if len(witnessPages) > MaxWitnessServe {
		return nil, fmt.Errorf("witness request exceeds %d page limit: got %d", MaxWitnessServe, len(witnessPages))
	}
	id := rand.Uint64()

	req := &Request{
		id:   id,
		sink: sink,
		code: GetMsgWitness,
		want: MsgWitness,
		data: &GetWitnessPacket{
			RequestId: id,
			GetWitnessRequest: &GetWitnessRequest{
				WitnessPages: witnessPages,
			},
		},
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// RequestWitnessMetadata sends a request to the peer for witness metadata (page count only).
func (p *Peer) RequestWitnessMetadata(hashes []common.Hash, sink chan *Response) (*Request, error) {
	log.Debug("Requesting witness metadata", "peer", p.id, "count", len(hashes))
	if len(hashes) > MaxWitnessMetadataServe {
		return nil, fmt.Errorf("witness metadata request exceeds %d hash limit: got %d", MaxWitnessMetadataServe, len(hashes))
	}
	id := rand.Uint64()

	req := &Request{
		id:   id,
		sink: sink,
		code: GetWitnessMetadataMsg,
		want: WitnessMetadataMsg,
		data: &GetWitnessMetadataPacket{
			RequestId: id,
			GetWitnessMetadataRequest: &GetWitnessMetadataRequest{
				Hashes: hashes,
			},
		},
	}
	if err := p.dispatchRequest(req); err != nil {
		return nil, err
	}
	return req, nil
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
func (p *Peer) KnownWitnesses() *KnownCache {
	return p.knownWitnesses
}

// AddKnownWitnesses adds a witness hash to the set of known witness hashes.
func (p *Peer) AddKnownWitness(hash common.Hash) {
	p.knownWitnesses.Add(hash)
}

// AddKnownAnnounce records that this peer has seen the signed announcement for
// `hash`, without claiming the peer holds the body. Used to suppress redundant
// announce-relay only; body-fetch peer selection ignores this set.
func (p *Peer) AddKnownAnnounce(hash common.Hash) {
	p.knownAnnounces.Add(hash)
}

// KnownAnnounceContainsHash reports whether this peer is known to have seen an
// announcement for `hash` (either inbound or outbound). False does not imply
// the peer is unaware — only that this side has no record.
func (p *Peer) KnownAnnounceContainsHash(hash common.Hash) bool {
	return p.knownAnnounces.hashes.Contains(hash)
}

// KnownWitnessesCount returns the number of known witness.
func (p *Peer) KnownWitnessesCount() int {
	return p.knownWitnesses.Cardinality()
}

// KnownWitnessesContains checks if a witness is known to be known by this peer.
func (p *Peer) KnownWitnessesContains(witness *stateless.Witness) bool {
	return p.knownWitnesses.Contains(witness.Header().Hash())
}

func (p *Peer) KnownWitnessContainsHash(hash common.Hash) bool {
	return p.knownWitnesses.hashes.Contains(hash)
}

// ReplyWitness is the response to GetWitness
func (p *Peer) ReplyWitness(requestID uint64, response *WitnessPacketResponse) error {
	return p2p.Send(p.rw, MsgWitness, &WitnessPacketRLPPacket{
		RequestId:             requestID,
		WitnessPacketResponse: *response,
	})
}

// ReplyWitnessMetadata is the response to GetWitnessMetadata
func (p *Peer) ReplyWitnessMetadata(requestID uint64, metadata []WitnessMetadataResponse) error {
	return p2p.Send(p.rw, WitnessMetadataMsg, &WitnessMetadataPacket{
		RequestId: requestID,
		Metadata:  metadata,
	})
}

// KnownCache is a thread-safe cache for known witness hashes, identified by the
// hash of the parent witness block. The internal mutex guards the Pop+Add
// eviction sequence in Add(); individual reads use the underlying thread-safe
// mapset and do not need external synchronization.
type KnownCache struct {
	mu     sync.Mutex
	hashes mapset.Set[common.Hash]
	max    int
}

// newKnownCache creates a new knownCache with a max capacity.
func newKnownCache(max int) *KnownCache {
	return &KnownCache{
		max:    max,
		hashes: mapset.NewSet[common.Hash](),
	}
}

// Add adds a witness to the set, evicting old entries if at capacity.
func (k *KnownCache) Add(hash common.Hash) {
	k.mu.Lock()
	defer k.mu.Unlock()

	for k.hashes.Cardinality() > max(0, k.max-1) {
		k.hashes.Pop()
	}
	k.hashes.Add(hash)
}

// Contains returns whether the given item is in the set.
func (k *KnownCache) Contains(hash common.Hash) bool {
	return k.hashes.Contains(hash)
}

// Cardinality returns the number of elements in the set.
func (k *KnownCache) Cardinality() int {
	return k.hashes.Cardinality()
}
