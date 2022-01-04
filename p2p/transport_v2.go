package p2p

import (
	"crypto/ecdsa"
)

type transportV2 interface {
	Dial() (*Peer, error)
	Listen() (*Peer, error)
}

// backendv2 is the interface required by transport2 to work
type backendv2 interface {
	// Merge this in a single function?
	LocalPrivateKey() *ecdsa.PrivateKey
	LocalHandshake() *protoHandshake

	// This is executed after the initial handhsake and before protocol negotiation
	OnConnectValidate(c *conn) error

	// Returns the list of legacy protocols
	GetProtocols() []Protocol
}
