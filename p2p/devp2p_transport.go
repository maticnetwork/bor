package p2p

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"fmt"
	"net"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

type devp2pTransportV2 struct {
	b   backendv2
	lis net.Listener
}

func (r *devp2pTransportV2) Listen(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	r.lis = lis
	return nil
}

func (r *devp2pTransportV2) Dial(enode *enode.Node) (*Peer, error) {
	dialer := &net.Dialer{Timeout: defaultDialTimeout}
	conn, err := dialer.DialContext(context.Background(), "tcp", nodeAddr(enode).String())
	if err != nil {
		return nil, err
	}
	peer, err := r.connect(conn, 0, enode)
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func (r *devp2pTransportV2) Accept() (*Peer, error) {
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

func (r *devp2pTransportV2) connect(rawConn net.Conn, flags connFlag, dialDest *enode.Node) (*Peer, error) {

	// transport connection
	var tt *rlpxTransport
	if dialDest == nil {
		tt = newRLPX(rawConn, nil)
	} else {
		tt = newRLPX(rawConn, dialDest.Pubkey())
	}

	// Run the RLPx handshake.
	remotePubkey, err := tt.doEncHandshake(r.b.LocalPrivateKey())
	if err != nil {
		panic(err)
	}

	pp := &Peer{
		flags:      flags,
		localAddr:  rawConn.LocalAddr(),
		remoteAddr: rawConn.RemoteAddr(),
	}

	// TODO: First validation goes here

	if dialDest != nil {
		pp.node = dialDest
	} else {
		pp.node = nodeFromConn(remotePubkey, rawConn)
	}

	// Run the capability negotiation handshake.
	phs, err := tt.doProtoHandshake(r.b.LocalHandshake())
	if err != nil {
		return nil, err
	}
	if id := pp.node.ID(); !bytes.Equal(crypto.Keccak256(phs.ID), id[:]) {
		return nil, DiscUnexpectedIdentity
	}

	pp.caps = phs.Caps
	pp.name = phs.Name

	// here come the funny stuff
	peer := newRlpxSession(log.Root(), pp, tt, r.b.GetProtocols())
	go func() {
		fmt.Println("- run -")
		a, b := peer.run()
		fmt.Println("- rlpx peer done -", a, b)
		// notify!
	}()

	pp.closeFn = func(reason error) {
		peer.Close(DiscProtocolError)
	}

	// we cannot return the raw conn and allt he transport must be done with the protocols

	return pp, nil
}

func nodeFromConn(pubkey *ecdsa.PublicKey, conn net.Conn) *enode.Node {
	var ip net.IP
	var port int
	if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		ip = tcp.IP
		port = tcp.Port
	}
	return enode.NewV4(pubkey, ip, port, port)
}
