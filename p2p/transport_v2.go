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

	// This is executed after the initial handhsake and before protocol negotiation
	OnConnectValidate(c *Peer) error

	// Returns the list of legacy protocols
	GetProtocols() []Protocol

	// Disconnect is used to notify when a peer disconnected in the transport
	Disconnected(peer enode.ID)
}
