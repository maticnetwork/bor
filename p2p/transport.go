package p2p

import (
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// backend is an interface implemented by the Server to be used
// by the transport implementations.
type backend interface {
	// LocalPrivateKey returns the private key of the local node
	LocalPrivateKey() *ecdsa.PrivateKey

	// LocalHandshake returns the handshake of the local node
	LocalHandshake() *protoHandshake

	// ValidatePreHandshake runs the validations before any protocol handshake
	ValidatePreHandshake(c *Peer) error

	// ValidatePostHandshake runs the validations after the protocol handshake
	ValidatePostHandshake(c *Peer) error

	// Returns the list of legacy protocols
	GetProtocols() []Protocol

	// Disconnect is used to notify when a peer disconnected in
	Disconnected(peerDisconnected)
}

// peerDisconnected is a struct that returns context information
// about a peer disconnection from any of the transports.
type peerDisconnected struct {
	// Id of the node being disconnected
	Id enode.ID

	// If the disconnection was requested by the remote peer
	RemoteRequested bool

	// The error code for the disconnection
	Error error
}
