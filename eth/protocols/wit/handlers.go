package wit

import (
	"fmt"

	"github.com/ethereum/go-ethereum/log"
)

// handleGetWitness processes a GetWitnessPacket request from a peer.
func handleGetWitness(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the GetWitnessPacket request
	req := new(GetWitnessPacket)
	if err := msg.Decode(&req); err != nil {
		return fmt.Errorf("failed to decode GetWitnessPacket: %w", err)
	}

	// Validate request parameters
	if req.TotalBlocks == 0 {
		return fmt.Errorf("invalid GetWitnessPacket: TotalBlocks cannot be zero")
	}

	return backend.Handle(peer, req)
}

// handleWitness processes an incoming witness from a peer.
func handleWitness(backend Backend, msg Decoder, peer *Peer) error {
	packet := new(NewWitnessPacket)
	if err := msg.Decode(&packet); err != nil {
		log.Error("Failed to decode witness", "err", err)
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}

	peer.AddKnownWitness(packet.Witness)
	log.Info("Processed witness", "peer", peer.ID())

	return backend.Handle(peer, packet)
}
