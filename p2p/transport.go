package p2p

import (
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

// backendv2 is the interface required by transport2 to work
type backendv2 interface {
	// Merge this in a single function?
	LocalPrivateKey() *ecdsa.PrivateKey
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

type peerDisconnected struct {
	// Id of the node being disconnected
	Id enode.ID

	// If the disconnection was requested by the remote peer
	RemoteRequested bool

	// The error code for the disconnection
	Error error
}
