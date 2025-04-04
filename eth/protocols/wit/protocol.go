package wit

import (
	"errors"

	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/rlp"
)

// Constants to match up protocol versions and messages
const (
	WIT1 = 1
)

// ProtocolName is the official short name of the `wit` protocol used during
// devp2p capability negotiation.
const ProtocolName = "wit"

// ProtocolVersions are the supported versions of the `wit` protocol (first
// is primary).
var ProtocolVersions = []uint{WIT1}

// protocolLengths are the number of implemented message corresponding to
// different protocol versions.
// PSP - TODO: Add protocol lengths when implemented
var protocolLengths = map[uint]uint64{WIT1: 0}

// maxMessageSize is the maximum cap on the size of a protocol message.
const maxMessageSize = 10 * 1024 * 1024

const (
	GetMsgWitness = 0x03
	MsgWitness    = 0x04
)

var (
	errNoStatusMsg             = errors.New("no status message")
	errMsgTooLarge             = errors.New("message too long")
	errDecode                  = errors.New("invalid message")
	errInvalidMsgCode          = errors.New("invalid message code")
	errProtocolVersionMismatch = errors.New("protocol version mismatch")
	errNetworkIDMismatch       = errors.New("network ID mismatch")
	errGenesisMismatch         = errors.New("genesis mismatch")
	errForkIDRejected          = errors.New("fork ID rejected")
)

// Packet represents a p2p message in the `wit` protocol.
type Packet interface {
	Name() string // Name returns a string corresponding to the message type.
	Kind() byte   // Kind returns the message type.
}

// GetWitnessPacket is a packet requesting a witness.
type GetWitnessPacket struct {
	RequestId   uint64
	OriginBlock uint64
	TotalBlocks uint64
}

// WitnessPacketRLPPacket represents a witness response with request ID wrapping.
type WitnessPacketRLPPacket struct {
	RequestId uint64
	WitnessPacketResponse
}

// WitnessPacketResponse represents a witness response, to use when we already
// have the witness rlp encoded.
type WitnessPacketResponse []rlp.RawValue

type NewWitnessPacket struct {
	Witness *stateless.Witness
}

func (w *NewWitnessPacket) Name() string { return "NewWitness" }
func (w *NewWitnessPacket) Kind() byte   { return MsgWitness }
