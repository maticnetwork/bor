package wit

import (
	"fmt"

	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// PSP - TODO - add logic to all the handlers

func handleGetWitness(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the GetWitnessPacket request
	var req GetWitnessPacket
	if err := msg.Decode(&req); err != nil {
		return fmt.Errorf("failed to decode GetWitnessPacket: %w", err)
	}

	// Validate request parameters
	if req.TotalBlocks == 0 {
		return fmt.Errorf("invalid GetWitnessPacket: TotalBlocks cannot be zero")
	}

	var witnesses []rlp.RawValue

	// PSP - TODO
	/*
		// Fetch witnesses from the backend
		witnesses, err := backend.GetWitnesses(req.OriginBlock, req.TotalBlocks)
		if err != nil {
			log.Error("Failed to fetch witnesses", "err", err)
		}
	*/

	return peer.ReplyWitnessRLP(req.RequestId, witnesses)
}

// handleWitness processes an incoming witness from a peer.
func handleWitness(backend Backend, msg Decoder, peer *Peer) error {
	var witness stateless.Witness
	if err := msg.Decode(&witness); err != nil {
		log.Error("Failed to decode witness", "err", err)
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}

	// PSP - TODO
	/*
		// Store the witnes
		if err := backend.StoreWitness(&witness); err != nil {
			log.Error("Failed to store witness", "err", err)
			return err
		}
	*/

	peer.AddKnownWitness(&witness)
	log.Info("Processed witness", "peer", peer.ID())

	return nil
}
