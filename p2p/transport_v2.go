package p2p

import (
	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

type transportV2 interface {
	Dial() (*peer2, error)
	Listen() (*peer2, error)
}

// peer2 is the object we get after we dial. This object is ready AFTER the handshake.
type peer2 struct {
	node  *enode.Node
	flags connFlag
	caps  []Cap  // valid after the protocol handshake
	name  string // valid after the protocol handshake

	rlpx *rlpxTransport
}

// backendv2 is the interface required by transport2 to work
type backendv2 interface {
	LocalPrivateKey() *ecdsa.PrivateKey
	LocalHandshake() *protoHandshake
	OnConnectValidate(c *conn) error
}
