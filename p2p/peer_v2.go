package p2p

import (
	"net"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type Peer struct {
	// use this only to report info
	conn *conn

	// logger for the peer
	log log.Logger

	created mclock.AbsTime
}

func (p *Peer) Disconnect(reason DiscReason) {

}

func (p *Peer) Inbound() bool {
	return p.is(inboundConn)
}

func (p *Peer) is(f connFlag) bool {
	return p.conn.is(f)
}

func (p *Peer) set(f connFlag, val bool) {
	p.conn.set(f, val)
}

// ID returns the node's public key.
func (p *Peer) ID() enode.ID {
	return p.conn.node.ID()
}

func (p *Peer) Log() log.Logger {
	return p.log
}

// Node returns the peer's node descriptor.
func (p *Peer) Node() *enode.Node {
	return p.conn.node
}

// Name returns an abbreviated form of the name
func (p *Peer) Name() string {
	s := p.conn.name
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// Fullname returns the node name that the remote node advertised.
func (p *Peer) Fullname() string {
	return p.conn.name
}

// Caps returns the capabilities (supported subprotocols) of the remote peer.
func (p *Peer) Caps() []Cap {
	return p.conn.caps
}

// RunningCap returns true if the peer is actively connected using any of the
// enumerated versions of a specific protocol, meaning that at least one of the
// versions is supported by both this node and the peer p.
func (p *Peer) RunningCap(protocol string, versions []uint) bool {
	panic("not implemented")
}

// RemoteAddr returns the remote address of the network connection.
func (p *Peer) RemoteAddr() net.Addr {
	return p.conn.fd.RemoteAddr()
}

// LocalAddr returns the local address of the network connection.
func (p *Peer) LocalAddr() net.Addr {
	return p.conn.fd.LocalAddr()
}

// NewPeer returns a peer for testing purposes.
func NewPeer(id enode.ID, name string, caps []Cap) *Peer {
	/*
		pipe, _ := net.Pipe()
		node := enode.SignNull(new(enr.Record), id)
		conn := &conn{fd: pipe, transport: nil, node: node, caps: caps, name: name}
		peer := newPeer(log.Root(), conn, nil)
		close(peer.closed) // ensures Disconnect doesn't block
		return peer
	*/
	panic("TODO")
}

// NewPeerPipe creates a peer for testing purposes.
// The message pipe given as the last parameter is closed when
// Disconnect is called on the peer.
func NewPeerPipe(id enode.ID, name string, caps []Cap, pipe *MsgPipeRW) *Peer {
	/*
		p := NewPeer(id, name, caps)
		p.testPipe = pipe
		return p
	*/
	panic("TODO")
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *Peer1) Info() *PeerInfo {
	pp := &Peer{
		conn: p.rw,
	}
	return pp.Info()
}

func (p *Peer) Info() *PeerInfo {
	// Gather the protocol capabilities
	var caps []string
	for _, cap := range p.Caps() {
		caps = append(caps, cap.String())
	}
	// Assemble the generic peer metadata
	info := &PeerInfo{
		Enode:     p.Node().URLv4(),
		ID:        p.ID().String(),
		Name:      p.Fullname(),
		Caps:      caps,
		Protocols: make(map[string]interface{}),
	}
	if p.Node().Seq() > 0 {
		info.ENR = p.Node().String()
	}
	info.Network.LocalAddress = p.LocalAddr().String()
	info.Network.RemoteAddress = p.RemoteAddr().String()
	//info.Network.Inbound = p.rw.is(inboundConn)
	//info.Network.Trusted = p.rw.is(trustedConn)
	//info.Network.Static = p.rw.is(staticDialedConn)

	// Gather all the running protocol infos
	/*
		for _, proto := range p.running {
			protoInfo := interface{}("unknown")
			if query := proto.proto.PeerInfo; query != nil {
				if metadata := query(p.ID()); metadata != nil {
					protoInfo = metadata
				} else {
					protoInfo = "handshake"
				}
			}
			info.Protocols[proto.proto.Name] = protoInfo
		}
	*/

	return info
}
