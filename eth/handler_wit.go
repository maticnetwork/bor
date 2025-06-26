package eth

import (
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
)

const (
	// witnessRequestTimeout defines how long to wait for an in-flight witness computation.
	witnessRequestTimeout = 5 * time.Second
)

// witHandler implements the eth.Backend interface to handle the various network
// packets that are sent as replies or broadcasts.
type witHandler handler

func (h *witHandler) Chain() *core.BlockChain { return h.chain }

// RunPeer is invoked when a peer joins on the `wit` protocol.
func (h *witHandler) RunPeer(peer *wit.Peer, hand wit.Handler) error {
	return (*handler)(h).runWitExtension(peer, hand)
}

// PeerInfo retrieves all known `wit` information about a peer.
func (h *witHandler) PeerInfo(id enode.ID) interface{} {
	if p := h.peers.peer(id.String()); p != nil {
		if p.witPeer != nil {
			return p.witPeer.info()
		}
	}

	return nil
}

// Handle is invoked from a peer's message handler when it receives a new remote
// message that the handler couldn't consume and serve itself.
func (h *witHandler) Handle(peer *wit.Peer, packet wit.Packet) error {
	log.Debug("witHandler Handle", "packet", packet)
	// Consume any broadcasts and announces, forwarding the rest to the downloader
	switch packet := packet.(type) {
	case *wit.NewWitnessPacket:
		return h.handleWitnessBroadcast(peer, packet.Witness)
	case *wit.NewWitnessHashesPacket:
		return h.handleWitnessHashesAnnounce(peer, packet.Hashes, packet.Numbers)
	case *wit.GetWitnessPacket:
		// Call handleGetWitness which returns the raw RLP data
		witnessesRLPBytes, err := h.handleGetWitness(peer, packet)
		if err != nil {
			return fmt.Errorf("failed to handle GetWitnessPacket: %w", err)
		}
		// Convert [][]byte to []rlp.RawValue before replying
		witnessesRLP := make([]rlp.RawValue, len(witnessesRLPBytes))
		for i, b := range witnessesRLPBytes {
			witnessesRLP[i] = rlp.RawValue(b)
		}
		// Reply using the retrieved RLP data
		return peer.ReplyWitnessRLP(packet.RequestId, witnessesRLP)

	default:
		return fmt.Errorf("unknown wit packet type %T", packet)
	}
}

// handleWitnessBroadcast handles a witness broadcast from a peer.
func (h *witHandler) handleWitnessBroadcast(peer *wit.Peer, witness *stateless.Witness) error {
	peer.AddKnownWitness(witness.Header().Hash())
	hash := witness.Header().Hash()

	// Inject the witness into the block fetcher's cache
	if h.blockFetcher != nil {
		log.Debug("Injecting witness into block fetcher", "hash", hash, "peer", peer.ID())
		// Verify witness header matches a known block hash
		blockHash := witness.Header().Hash()
		log.Debug("Witness details", "blockHash", blockHash, "header", witness.Header().Number)

		if err := h.blockFetcher.InjectWitness(peer.ID(), witness); err != nil {
			peer.Log().Warn("Failed to inject broadcast witness into fetcher", "hash", hash, "err", err)
			// Don't return error, just log, as block might still be importable via other means
		}
	} else {
		// This shouldn't happen in normal operation, but log if it does
		peer.Log().Warn("Block fetcher nil in witHandler, cannot inject witness")
	}

	return nil
}

// handleWitnessHashesAnnounce handles a witness hashes broadcast from a peer.
func (h *witHandler) handleWitnessHashesAnnounce(peer *wit.Peer, hashes []common.Hash, numbers []uint64) error {
	for _, hash := range hashes {
		peer.AddKnownWitness(hash)
	}
	return nil
}

// handleGetWitness retrieves witnesses for the requested block hashes and returns them as raw RLP data.
// It now returns the data and error, rather than sending the reply directly.
// The returned data is [][]byte, as rlp.RawValue is essentially []byte.
func (h *witHandler) handleGetWitness(peer *wit.Peer, req *wit.GetWitnessPacket) ([][]byte, error) {
	witnessesRLPBytes := make([][]byte, 0, len(req.Hashes))

	log.Debug("handleGetWitness processing request", "peer", peer.ID(), "reqID", req.RequestId, "hashes", len(req.Hashes))

	// Fetch witnesses from the backend
	for _, hash := range req.Hashes {
		// Call the blockchain method which now returns raw RLP bytes
		witnessBytes := rawdb.ReadWitness(h.Chain().DB(), hash)
		if len(witnessBytes) > 0 {
			log.Trace("Witness found in DB immediately", "hash", hash)
			witnessesRLPBytes = append(witnessesRLPBytes, witnessBytes)
		}
	}
	// Return the collected RLP data
	log.Debug("handleGetWitness returning witnesses", "peer", peer.ID(), "reqID", req.RequestId, "count", len(witnessesRLPBytes))
	return witnessesRLPBytes, nil
}
