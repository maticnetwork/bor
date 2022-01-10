package p2p

import (
	"fmt"
	"net"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/libp2p/go-libp2p-core/peer"
)

type connFlag int32

const (
	dynDialedConn connFlag = 1 << iota
	staticDialedConn
	inboundConn
	trustedConn
)

// PeerEventType is the type of peer events emitted by a p2p.Server
type PeerEventType string

const (
	// PeerEventTypeAdd is the type of event emitted when a peer is added
	// to a p2p.Server
	PeerEventTypeAdd PeerEventType = "add"

	// PeerEventTypeDrop is the type of event emitted when a peer is
	// dropped from a p2p.Server
	PeerEventTypeDrop PeerEventType = "drop"

	// PeerEventTypeMsgSend is the type of event emitted when a
	// message is successfully sent to a peer
	PeerEventTypeMsgSend PeerEventType = "msgsend"

	// PeerEventTypeMsgRecv is the type of event emitted when a
	// message is received from a peer
	PeerEventTypeMsgRecv PeerEventType = "msgrecv"
)

// PeerEvent is an event emitted when peers are either added or dropped from
// a p2p.Server or when a message is sent or received on a peer connection
type PeerEvent struct {
	Type          PeerEventType `json:"type"`
	Peer          enode.ID      `json:"peer"`
	Error         string        `json:"error,omitempty"`
	Protocol      string        `json:"protocol,omitempty"`
	MsgCode       *uint64       `json:"msg_code,omitempty"`
	MsgSize       *uint32       `json:"msg_size,omitempty"`
	LocalAddress  string        `json:"local,omitempty"`
	RemoteAddress string        `json:"remote,omitempty"`
}

type Peer struct {
	// logger for the peer
	log log.Logger

	// enode address of the remote node
	node *enode.Node

	// libp2p peer id (only for libp2p connections)
	peerID peer.ID

	// remote connected address
	remoteAddr net.Addr

	// local address of the peer (isnt this our local addr?)
	localAddr net.Addr

	// bitmap of options of the connection
	flags connFlag

	// list of capabilities (valid after protocol handshake)
	caps []Cap

	// name of the node (valid after protocol handshake)
	name string

	// closeFn closes the connection with the Peer
	closeFn func(reason DiscReason)

	// running is the list of running protocols
	running map[string]uint

	// Is this still required?
	created mclock.AbsTime
}

func (p *Peer) Disconnect(err error) {
	// add a reason to this
	if reason, ok := err.(DiscReason); ok {
		p.closeFn(reason)
	} else {
		panic("provisional")
		fmt.Println("-- not defined --", err)
		p.closeFn(DiscUselessPeer)
	}
}

func (p *Peer) Inbound() bool {
	return p.is(inboundConn)
}

func (p *Peer) is(f connFlag) bool {
	flags := connFlag(atomic.LoadInt32((*int32)(&p.flags)))
	return flags&f != 0
}

func (p *Peer) set(f connFlag, val bool) {
	for {
		oldFlags := connFlag(atomic.LoadInt32((*int32)(&p.flags)))
		flags := oldFlags
		if val {
			flags |= f
		} else {
			flags &= ^f
		}
		if atomic.CompareAndSwapInt32((*int32)(&p.flags), int32(oldFlags), int32(flags)) {
			return
		}
	}
}

func (p *Peer) setRunning(running map[string]uint) {
	p.running = running
}

// ID returns the node's public key.
func (p *Peer) ID() enode.ID {
	return p.node.ID()
}

func (p *Peer) Log() log.Logger {
	return p.log
}

// Node returns the peer's node descriptor.
func (p *Peer) Node() *enode.Node {
	return p.node
}

// Name returns an abbreviated form of the name
func (p *Peer) Name() string {
	s := p.name
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// Fullname returns the node name that the remote node advertised.
func (p *Peer) Fullname() string {
	return p.name
}

// Caps returns the capabilities (supported subprotocols) of the remote peer.
func (p *Peer) Caps() []Cap {
	return p.caps
}

// RunningCap returns true if the peer is actively connected using any of the
// enumerated versions of a specific protocol, meaning that at least one of the
// versions is supported by both this node and the peer p.
func (p *Peer) RunningCap(protocol string, versions []uint) bool {
	if proto, ok := p.running[protocol]; ok {
		for _, ver := range versions {
			if proto == ver {
				return true
			}
		}
	}
	return false
}

// RemoteAddr returns the remote address of the network connection.
func (p *Peer) RemoteAddr() net.Addr {
	return p.remoteAddr
}

// LocalAddr returns the local address of the network connection.
func (p *Peer) LocalAddr() net.Addr {
	return p.localAddr
}

func (p *Peer) String() string {
	s := p.flags.String()
	if (p.node.ID() != enode.ID{}) {
		s += " " + p.node.ID().String()
	}
	s += " " + p.RemoteAddr().String()
	return s
}

func (f connFlag) String() string {
	s := ""
	if f&trustedConn != 0 {
		s += "-trusted"
	}
	if f&dynDialedConn != 0 {
		s += "-dyndial"
	}
	if f&staticDialedConn != 0 {
		s += "-staticdial"
	}
	if f&inboundConn != 0 {
		s += "-inbound"
	}
	if s != "" {
		s = s[1:]
	}
	return s
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

/*
// Info gathers and returns a collection of metadata known about a peer.
func (p *Peer1) Info() *PeerInfo {
	pp := &Peer{
		conn: p.rw,
	}
	return pp.Info()
}
*/

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
	info.Network.Inbound = p.is(inboundConn)
	info.Network.Trusted = p.is(trustedConn)
	info.Network.Static = p.is(staticDialedConn)

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

// PeerInfo represents a short summary of the information known about a connected
// peer. Sub-protocol independent fields are contained and initialized here, with
// protocol specifics delegated to all connected sub-protocols.
type PeerInfo struct {
	ENR     string   `json:"enr,omitempty"` // Ethereum Node Record
	Enode   string   `json:"enode"`         // Node URL
	ID      string   `json:"id"`            // Unique node identifier
	Name    string   `json:"name"`          // Name of the node, including client type, version, OS, custom data
	Caps    []string `json:"caps"`          // Protocols advertised by this peer
	Network struct {
		LocalAddress  string `json:"localAddress"`  // Local endpoint of the TCP data connection
		RemoteAddress string `json:"remoteAddress"` // Remote endpoint of the TCP data connection
		Inbound       bool   `json:"inbound"`
		Trusted       bool   `json:"trusted"`
		Static        bool   `json:"static"`
	} `json:"network"`
	Protocols map[string]interface{} `json:"protocols"` // Sub-protocol specific metadata fields
}
