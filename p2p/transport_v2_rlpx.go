package p2p

import (
	"bytes"
	"context"
	"fmt"
	"net"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type rlpxTransportV2 struct {
	b   backendv2
	lis net.Listener
}

func (r *rlpxTransportV2) Listen(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	r.lis = lis
	return nil
}

func (r *rlpxTransportV2) Dial(enode *enode.Node) (*Peer, error) {
	// TODO
	dialer := &tcpDialer{
		d: &net.Dialer{Timeout: defaultDialTimeout},
	}
	conn, err := dialer.Dial(context.Background(), enode)
	if err != nil {
		return nil, err
	}
	peer, err := r.connect(conn, 0, enode)
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func (r *rlpxTransportV2) Accept() (*Peer, error) {
	// TODO
	conn, err := r.lis.Accept()
	if err != nil {
		return nil, err
	}
	peer, err := r.connect(conn, 0, nil)
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func (r *rlpxTransportV2) connect(rawConn net.Conn, flags connFlag, dialDest *enode.Node) (*Peer, error) {

	// transport connection
	var tt *rlpxTransport
	if dialDest == nil {
		tt = newRLPX(rawConn, nil).(*rlpxTransport)
	} else {
		tt = newRLPX(rawConn, dialDest.Pubkey()).(*rlpxTransport)
	}

	// Run the RLPx handshake.
	remotePubkey, err := tt.doEncHandshake(r.b.LocalPrivateKey())
	if err != nil {
		panic(err)
	}

	cc := &conn{
		fd:        rawConn,
		transport: tt,
		flags:     flags,
	}

	if dialDest != nil {
		cc.node = dialDest
	} else {
		cc.node = nodeFromConn(remotePubkey, rawConn)
	}

	// Run the capability negotiation handshake.
	phs, err := tt.doProtoHandshake(r.b.LocalHandshake())
	if err != nil {
		return nil, err
	}
	if id := cc.node.ID(); !bytes.Equal(crypto.Keccak256(phs.ID), id[:]) {
		return nil, DiscUnexpectedIdentity
	}

	cc.caps = phs.Caps
	cc.name = phs.Name

	// here come the funny stuff
	peer := newPeer(log.Root(), cc, r.b.GetProtocols())
	go func() {
		fmt.Println("- run -")
		a, b := peer.run()
		fmt.Println("- rlpx peer done -", a, b)
	}()

	// we cannot return the raw conn and allt he transport must be done with the protocols
	pp := &Peer{}

	return pp, nil
}
