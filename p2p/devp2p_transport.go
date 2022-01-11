package p2p

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"net"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
)

/*
TODO:
- Add a Close function to stop the listener
- Figure out how to configure the transport with info from the server
	- Configure the NAT (more fields in the backend object)
*/

type devp2pTransportV2 struct {
	b   backend
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
	conn, err := r.lis.Accept()
	if err != nil {
		return nil, err
	}
	peer, err := r.connect(conn, 0, nil)
	if err != nil {
		conn.Close()
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
	if dialDest != nil {
		peer.node = dialDest
	} else {
		peer.node = nodeFromConn(remotePubkey, rawConn)
	}
	if err := r.b.ValidatePreHandshake(peer); err != nil {
		disconnect(err)
		return nil, err
	}

	// Run the capability negotiation handshake.
	phs, err := tt.doProtoHandshake(r.b.LocalHandshake())
	if err != nil {
		return nil, err
	}
	if id := peer.node.ID(); !bytes.Equal(crypto.Keccak256(phs.ID), id[:]) {
		disconnect(DiscUnexpectedIdentity)
		return nil, DiscUnexpectedIdentity
	}

	peer.caps = phs.Caps
	peer.name = phs.Name

	if err := r.b.ValidatePostHandshake(peer); err != nil {
		disconnect(err)
		return nil, err
	}
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
