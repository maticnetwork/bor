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
	if len(req.WitnessPages) == 0 {
		return fmt.Errorf("invalid GetWitnessPacket: Hashes cannot be empty")
	}
	if len(req.WitnessPages) > MaxWitnessServe {
		return fmt.Errorf("witness request exceeds %d page limit: got %d", MaxWitnessServe, len(req.WitnessPages))
	}

	return backend.Handle(peer, req)
}

// handleWitness processes an incoming witness response from a peer.
func handleWitness(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the WitnessPacketRLPPacket response
	packet := new(WitnessPacketRLPPacket)
	if err := msg.Decode(packet); err != nil {
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

func handleNewWitnessHashes(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the NewWitnessHashesPacket request
	req := new(NewWitnessHashesPacket)
	if err := msg.Decode(&req); err != nil {
		return fmt.Errorf("failed to decode NewWitnessHashesPacket: %w", err)
	}

	return backend.Handle(peer, req)
}

// MaxSignedAnnouncesPerPacket caps how many signed witness announcements a
// single SignedNewWitnessHashesPacket may carry. Each announcement triggers
// ecrecover and a header lookup downstream, so an unbounded packet is a cheap
// DoS vector. 64 matches maxQueuedWitnessAnns: the relay queue and the wire
// limit move together so a packet that fits the queue also fits the wire.
const MaxSignedAnnouncesPerPacket = 64

// handleSignedNewWitnessHashes processes a SignedNewWitnessHashesPacket from a
// peer (WIT2+). The packet is forwarded to the backend, which is responsible
// for signature verification, validator-set check, relay, and triggering the
// body fetch. We cap the announcement count at decode time so the backend
// never sees an unbounded packet.
func handleSignedNewWitnessHashes(backend Backend, msg Decoder, peer *Peer) error {
	req := new(SignedNewWitnessHashesPacket)
	if err := msg.Decode(&req); err != nil {
		return fmt.Errorf("failed to decode SignedNewWitnessHashesPacket: %w", err)
	}
	if len(req.Announcements) == 0 {
		return fmt.Errorf("invalid SignedNewWitnessHashesPacket: Announcements cannot be empty")
	}
	if len(req.Announcements) > MaxSignedAnnouncesPerPacket {
		return fmt.Errorf("SignedNewWitnessHashesPacket exceeds cap: %d > %d", len(req.Announcements), MaxSignedAnnouncesPerPacket)
	}
	return backend.Handle(peer, req)
}

// handleGetWitnessMetadata processes a GetWitnessMetadataPacket request from a peer.
func handleGetWitnessMetadata(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the GetWitnessMetadataPacket request
	req := new(GetWitnessMetadataPacket)
	if err := msg.Decode(&req); err != nil {
		return fmt.Errorf("failed to decode GetWitnessMetadataPacket: %w", err)
	}

	// Validate request parameters
	if len(req.Hashes) == 0 {
		return fmt.Errorf("invalid GetWitnessMetadataPacket: Hashes cannot be empty")
	}
	if len(req.Hashes) > MaxWitnessMetadataServe {
		return fmt.Errorf("witness metadata request exceeds %d hash limit: got %d", MaxWitnessMetadataServe, len(req.Hashes))
	}

	return backend.Handle(peer, req)
}

// handleWitnessMetadata processes an incoming witness metadata response from a peer.
func handleWitnessMetadata(backend Backend, msg Decoder, peer *Peer) error {
	// Decode the WitnessMetadataPacket response
	packet := new(WitnessMetadataPacket)
	if err := msg.Decode(packet); err != nil {
		log.Error("Failed to decode witness metadata response packet", "err", err)
		return fmt.Errorf("%w: message %v: %v", errDecode, msg, err)
	}

	// Construct the response object
	res := &Response{
		id:   packet.RequestId,
		code: WitnessMetadataMsg,
		Res:  packet,
	}

	// Forward the response to the dispatcher
	log.Debug("Dispatching witness metadata response packet", "peer", peer.ID(), "reqID", packet.RequestId, "count", len(packet.Metadata))
	return peer.dispatchResponse(res, nil)
}
