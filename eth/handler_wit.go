package eth

import (
	"fmt"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/p2p/enode"
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
func (h *witHandler) handleWitnessBroadcast(_ *wit.Peer, _ *stateless.Witness) error {
	//  TODO(@pratikspatil024) - implement this

	/*
		// Store the witnes
		if err := backend.StoreWitness(&witness); err != nil {
			log.Error("Failed to store witness", "err", err)
			return err
		}
	*/

	return nil
}

// handleGetWitness handles a GetWitnessPacket request from a peer.
func (h *witHandler) handleGetWitness(_ *wit.Peer, _ *wit.GetWitnessPacket) error {
	//  TODO(@pratikspatil024) - implement this

	/*
		var witnesses []rlp.RawValue

		// Fetch witnesses from the backend
		witnesses, err := backend.GetWitnesses(req.OriginBlock, req.TotalBlocks)
		if err != nil {
			log.Error("Failed to fetch witnesses", "err", err)
		}

		return peer.ReplyWitnessRLP(req.RequestId, witnesses)
	*/

	return nil
}
