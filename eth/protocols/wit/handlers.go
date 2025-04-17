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
	if len(req.Hashes) == 0 {
		return fmt.Errorf("invalid GetWitnessPacket: Hashes cannot be empty")
	}

	return backend.Handle(peer, req)
}

// handleWitness processes an incoming witness response from a peer.
func handleWitness(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the WitnessPacketRLPPacket response
	packet := new(WitnessPacketRLPPacket)
	if err := msg.Decode(&packet); err != nil {
		log.Error("Failed to decode witness response packet", "err", err)
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}

	// Construct the response object, putting the entire decoded packet into Res
	res := &Response{
		id:   packet.RequestId,
		code: MsgWitness,
		Res:  packet, // Assign the *entire* packet, not just packet.WitnessPacketResponse
	}

	// Forward the response to the dispatcher
	log.Debug("Dispatching witness response packet", "peer", peer.ID(), "reqID", packet.RequestId, "count", len(packet.WitnessPacketResponse))
	return peer.dispatchResponse(res, nil)
}

func handleNewWitness(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the NewWitnessPacket request
	req := new(NewWitnessPacket)
	if err := msg.Decode(&req); err != nil {
		return fmt.Errorf("failed to decode NewWitnessPacket: %w", err)
	}

	return backend.Handle(peer, req)
}
