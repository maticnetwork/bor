package eth

import (
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/protocols/wit"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

// witHandler implements the eth.Backend interface to handle the various network
// packets that are sent as replies or broadcasts.
type witHandler handler

func (h *witHandler) Chain() *core.BlockChain { return h.chain }

// RunPeer is invoked when a peer joins on the `wit` protocol.
func (h *witHandler) RunPeer(peer *wit.Peer, hand wit.Handler) error {
	return (*handler)(h).runWitPeer(peer, hand)
}

// PeerInfo retrieves all known `wit` information about a peer.
func (h *witHandler) PeerInfo(id enode.ID) interface{} {
	if p := h.peers.peer(id.String()); p != nil {
		if p.witPeer != nil {
			return p.witPeer.info()
		}
	}

	return nil
}

// PSP - implement this
// Handle is invoked from a peer's message handler when it receives a new remote
// message that the handler couldn't consume and serve itself.
func (h *witHandler) Handle(peer *wit.Peer, packet wit.Packet) error {
	return nil
}
