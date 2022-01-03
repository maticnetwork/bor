package enode

import (
	"fmt"
	"net"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
)

func (n *Node) IsMultiAddr() bool {
	var libp2pPort LibP2PEntry
	if err := n.Load(&libp2pPort); err != nil {
		return false
	}
	return true
}

func (n *Node) GetMultiAddr() (multiaddr.Multiaddr, error) {
	return EnodeToMultiAddr(n)
}

func EnodeToMultiAddr(node *Node) (multiaddr.Multiaddr, error) {
	// convert the key to libp2p.crypto format
	key := crypto.PubKey((*crypto.Secp256k1PublicKey)(node.Pubkey()))
	id, err := peer.IDFromPublicKey(key)
	if err != nil {
		return nil, fmt.Errorf("could not get peer id: %v", err)
	}
	ipAddr := node.IP().String()

	// we use the enr registry of the node to set the libp2p port
	var libp2pPort LibP2PEntry
	if err := node.Load(&libp2pPort); err != nil {
		return nil, err
	}

	ipProto := "ip6"
	if parsedIP := net.ParseIP(ipAddr); parsedIP.To4() != nil {
		ipProto = "ip4"
	}
	//return multiaddr.NewMultiaddr(fmt.Sprintf("/%s/%s/%s/%d/p2p/%s", ipProto, ipAddr, "tcp", uint(libp2pPort), id.String()))
	return multiaddr.NewMultiaddr(fmt.Sprintf("/%s/%s/%s/%d/p2p/%s", ipProto, ipAddr, "tcp", uint(libp2pPort), id.String()))
}

// LibP2PEntry is the ENR key which holds the LibP2P port of the node.
type LibP2PEntry uint16

func (l LibP2PEntry) ENRKey() string { return "libp2p" }
