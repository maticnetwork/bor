package eth

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rlp"
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
	case *wit.GetWitnessPacket:
		return h.handleGetWitness(peer, packet)

	default:
		return fmt.Errorf("unknown wit packet type %T", packet)
	}
}

// handleWitnessBroadcast handles a witness broadcast from a peer.
func (h *witHandler) handleWitnessBroadcast(peer *wit.Peer, witness *stateless.Witness) error {
	var witBuf bytes.Buffer
	if err := witness.EncodeRLP(&witBuf); err != nil {
		log.Error("error in witness encoding", "caughterr", err)
	}

	rawdb.WriteWitness(h.database, witness.Header().Hash(), witBuf.Bytes())

	return nil
}

// handleGetWitness handles a GetWitnessPacket request from a peer.
func (h *witHandler) handleGetWitness(peer *wit.Peer, req *wit.GetWitnessPacket) error {
	var witnesses []rlp.RawValue

	log.Debug("handleGetWitness", "req", req)

	// Fetch witnesses from the backend
	for _, hash := range req.Hashes {
		witnesses = append(witnesses, rawdb.ReadWitness(h.database, hash))
	}

	return peer.ReplyWitnessRLP(req.RequestId, witnesses)
}
