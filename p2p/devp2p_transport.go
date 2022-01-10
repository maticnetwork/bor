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
		conn.Close()
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
		conn.Close()
		fmt.Println("__ __XXXX__")
		return nil, err
	}
	return peer, nil
}

func (r *devp2pTransportV2) connect(rawConn net.Conn, flags connFlag, dialDest *enode.Node) (*Peer, error) {
	fmt.Println("-- connect ", dialDest.ID())

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
		return nil, err
	}

	// after this point we can disconnect with error messages
	disconnect := func(reason error) {
		if r, ok := err.(DiscReason); ok {
			tt.WriteMsg(encodeDiscMsg(r))
		} else {
			tt.WriteMsg(encodeDiscMsg(DiscProtocolError))
		}
	}

	peer := &Peer{
		log:        log.Root(),
		flags:      flags,
		localAddr:  rawConn.LocalAddr(),
		remoteAddr: rawConn.RemoteAddr(),
		node:       dialDest,
	}

	// TODO: First validation goes here
	fmt.Println("A")
	if dialDest != nil {
		peer.node = dialDest
	} else {
		peer.node = nodeFromConn(remotePubkey, rawConn)
	}
	fmt.Println("A2")
	if err := r.b.ValidatePreHandshake(peer); err != nil {
		disconnect(err)
		fmt.Println("YYY")
		return nil, err
	}
	fmt.Println("A1")
	// Run the capability negotiation handshake.
	phs, err := tt.doProtoHandshake(r.b.LocalHandshake())
	if err != nil {
		return nil, err
	}
	if id := peer.node.ID(); !bytes.Equal(crypto.Keccak256(phs.ID), id[:]) {
		disconnect(DiscUnexpectedIdentity)
		return nil, DiscUnexpectedIdentity
	}
	fmt.Println("A2")
	peer.caps = phs.Caps
	peer.name = phs.Name

	if err := r.b.ValidatePostHandshake(peer); err != nil {
		disconnect(err)
		fmt.Println("YYY")
		return nil, err
	}
	fmt.Println("A3")
	// here come the funny stuff
	session := newRlpxSession(log.Root(), peer, tt, r.b.GetProtocols())
	peer.closeFn = func(reason DiscReason) {
		session.Close(reason)
	}

	go func() {
		// run the session until it is over
		remoteRequested, err := session.run()

		r.b.Disconnected(peerDisconnected{
			Id:              dialDest.ID(),
			RemoteRequested: remoteRequested,
			Error:           err,
		})
	}()

	return peer, nil
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
