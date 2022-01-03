package p2p

import (
	"bytes"
	"context"
	"net"

	"github.com/ethereum/go-ethereum/crypto"
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

func (r *rlpxTransportV2) Dial(enode *enode.Node) (*peer2, error) {
	dialer := &tcpDialer{
		d: &net.Dialer{Timeout: defaultDialTimeout},
	}
	conn, err := dialer.Dial(context.Background(), enode)
	if err != nil {
		return nil, err
	}
	peer, err := r.connect(conn, enode)
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func (r *rlpxTransportV2) Accept() (*peer2, error) {
	conn, err := r.lis.Accept()
	if err != nil {
		return nil, err
	}
	peer, err := r.connect(conn, nil)
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func (r *rlpxTransportV2) connect(conn net.Conn, dialDest *enode.Node) (*peer2, error) {

	// transport connection
	var tt *rlpxTransport
	if dialDest == nil {
		tt = newRLPX(conn, nil).(*rlpxTransport)
	} else {
		tt = newRLPX(conn, dialDest.Pubkey()).(*rlpxTransport)
	}

	// Run the RLPx handshake.
	remotePubkey, err := tt.doEncHandshake(r.b.PrivateKey())
	if err != nil {
		return nil, err
	}

	p := &peer2{
		rlpx: tt,
	}

	if dialDest != nil {
		p.node = dialDest
	} else {
		p.node = nodeFromConn(remotePubkey, conn)
	}

	// Run the capability negotiation handshake.
	phs, err := tt.doProtoHandshake(r.b.LocalHandshake())
	if err != nil {
		return nil, err
	}
	if id := p.node.ID(); !bytes.Equal(crypto.Keccak256(phs.ID), id[:]) {
		return nil, DiscUnexpectedIdentity
	}

	p.caps = phs.Caps
	p.name = phs.Name

	return p, nil
}
