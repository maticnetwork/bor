package p2p

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"sync"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/proto"
	gproto "github.com/golang/protobuf/proto"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	noise "github.com/libp2p/go-libp2p-noise"
	"github.com/multiformats/go-multiaddr"
)

type libp2pTransportV2 struct {
	b           backendv2
	host        host.Host
	pending     peerList
	feed        event.Feed
	acceptCh    chan *Peer
	legacyProto *libp2pLegacyConnPool
}

const (
	// protoHandshakeV1 is the protocol used right after the peer connection is established
	// to perform an initial handshake
	protoHandshakeV1 = protocol.ID("handshake/0.1")

	// protoLegacyV1 is the protocol used to send any data belonging to the legacy DevP2P protocol
	protoLegacyV1 = protocol.ID("legacy/0.1")
)

func newLibp2pTransportV2(b backendv2) *libp2pTransportV2 {
	return &libp2pTransportV2{
		b:        b,
		acceptCh: make(chan *Peer),
	}
}

func (l *libp2pTransportV2) init(libp2pPort int) {
	listenAddr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", "127.0.0.1", libp2pPort))
	if err != nil {
		panic(err)
	}

	fmt.Println("- listen addr -")
	fmt.Println(listenAddr.String())

	// start libp2p
	host, err := libp2p.New(
		context.Background(),
		libp2p.Security(noise.ID, noise.New),
		libp2p.ListenAddrs(listenAddr),
		libp2p.Identity(toLibP2PCrypto(l.b.LocalPrivateKey())),
	)
	if err != nil {
		panic(err)
	}
	l.host = host

	l.pending = newPeerList()

	// we need to do several things here

	// 1. Start the handshake proto to handle queries
	l.host.SetStreamHandler(protoHandshakeV1, func(stream network.Stream) {
		// TODO: Handle error
		msg := handshakeToProto(l.b.LocalHandshake())
		raw, err := gproto.Marshal(msg)
		if err != nil {
			// I do not think this can fail if the handshake is always the same
			panic(err)
		}

		// reuse the same header format for the handshake message
		hdr := newHeader(0, uint32(len(raw)))
		if _, err := stream.Write(hdr); err != nil {
			panic(err)
		}
		if _, err := stream.Write(raw); err != nil {
			panic(err)
		}
	})

	host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(network network.Network, conn network.Conn) {
			// TODO: Add peer to pending
			conn.RemotePeer()

			go func() {
				// this has to always be in a co-routine in ConnectedF
				l.handleConnected(conn)

				// TODO: if it did not work we have to disconnect here

				// TODO: remove from pending
			}()
		},
		DisconnectedF: func(network network.Network, conn network.Conn) {
			// TODO: Notify server that the node has disconnected
		},
	})

	// 3. Start stream for proto legacy codes since that is handled here
	// star the transport that it is being use for legacy stuff
	l.legacyProto = &libp2pLegacyConnPool{
		host: host,
		addr: map[peer.ID]*libp2pLegacyConn{},
	}
	l.host.SetStreamHandler(protoLegacyV1, l.legacyProto.handleLegacyProtocolStream)
}

func (l *libp2pTransportV2) Accept() (*Peer, error) {
	peer := <-l.acceptCh
	return peer, nil
}

func (l *libp2pTransportV2) handleConnected(conn network.Conn) error {
	stream, err := l.host.NewStream(context.Background(), conn.RemotePeer(), protoHandshakeV1)
	if err != nil {
		return err
	}

	msg := &proto.HandshakeResponse{}
	hdr := header(make([]byte, headerSize))
	if _, err := stream.Read(hdr); err != nil {
		return err
	}

	data := make([]byte, hdr.Length())
	if _, err := stream.Read(data); err != nil {
		return err
	}

	if err := gproto.Unmarshal(data, msg); err != nil {
		return err
	}

	// TODO: Validate the handshake message
	// TODO: Call validation functions in Server.

	// local := l.b.LocalHandshake()
	remote := protoToHandshake(msg)

	// take remote and get its enode.ID since at this point we only have the peer.ID
	// and we need to validate its correct.
	enodeAddr, err := enode.Parse(enode.ValidSchemes, "enode://"+hex.EncodeToString(remote.ID))
	if err != nil {
		panic(err)
	}
	remotePeerID, err := enodeAddr.PeerID()
	if err != nil {
		panic(err)
	}
	if remotePeerID != conn.RemotePeer() {
		panic(fmt.Errorf("safe check, this two should match"))
	}

	peer := &Peer{
		log:        log.Root(),
		node:       enodeAddr,
		peerID:     remotePeerID,
		flags:      0,
		caps:       remote.Caps,
		name:       remote.Name,
		localAddr:  &net.TCPAddr{}, // placeholder
		remoteAddr: &net.TCPAddr{}, // placeholder
		closeFn: func(reason DiscReason) {
			panic("TODO")
		},
	}
	l.handlePeer(peer)

	// if the connection is incomming, we are adding this peer as part of
	// the Accept method, so we have to call the acceptCh to update
	if conn.Stat().Direction == network.DirInbound {
		l.acceptCh <- peer
	} else {
		// send an event on the feed to notify
		// this could be another channel as well
		l.feed.Send(&libp2pEvent{
			Peer: peer,
		})
	}

	return nil
}

func (l *libp2pTransportV2) handlePeer(peer *Peer) {
	// create a transport wrapper for the protocol
	wrapper := l.legacyProto.handleConn(peer)

	// start the p2p.Protocols
	for _, p := range l.b.GetProtocols() {
		// run each protocol with this peer
		go func(p Protocol) {
			if err := p.Run(peer, wrapper); err != nil {
				panic(err)
			}
		}(p)
	}
}

// libp2pLegacyConnPool is a pool of connections to handle the legacy
// DevP2P Ethereum messages and protocol communication that happens
// through the rlpx protocol and is message based
type libp2pLegacyConnPool struct {
	lock sync.Mutex

	host host.Host

	addr map[peer.ID]*libp2pLegacyConn
}

func (l *libp2pLegacyConnPool) handleConn(peer *Peer) *libp2pLegacyConn {
	l.lock.Lock()
	defer l.lock.Unlock()

	conn := &libp2pLegacyConn{
		peer:   peer.peerID,
		host:   l.host,
		readCh: make(chan network.Stream),
	}
	l.addr[peer.peerID] = conn

	return conn
}

func (l *libp2pLegacyConnPool) handleLegacyProtocolStream(stream network.Stream) {
	l.lock.Lock()
	defer l.lock.Unlock()

	id := stream.Conn().RemotePeer()

	fmt.Println("__ handle legacy stream __")
	fmt.Println(id)

	c, ok := l.addr[id]
	if ok {
		c.handoffStream(stream)
	}
}

// libp2pLegacyConn is a legacy DevP2P message based connection running
// on top of the libp2p protocol and using streams to exchange messages.
type libp2pLegacyConn struct {
	peer peer.ID
	host host.Host

	// read stream
	read     network.Stream
	readLock sync.Mutex
	readCh   chan network.Stream

	// write stream
	write     network.Stream
	writeLock sync.Mutex
}

func (l *libp2pLegacyConn) handoffStream(stream network.Stream) {
	l.readCh <- stream
}

func (l *libp2pLegacyConn) ReadMsg() (Msg, error) {
	fmt.Println("_ READ MSG _")

	l.readLock.Lock()
	defer l.readLock.Unlock()

	if l.read == nil {
		// TODO: Close channel?
		stream := <-l.readCh
		l.read = stream
	}

	fmt.Println("_ READ MSG CONN _")

	hdr := header(make([]byte, headerSize))
	if _, err := l.read.Read(hdr); err != nil {
		return Msg{}, err
	}

	data := make([]byte, hdr.Length())
	if _, err := l.read.Read(data); err != nil {
		return Msg{}, err
	}

	msg := Msg{
		Code:    hdr.Code(),
		Payload: bytes.NewReader(data),
		Size:    uint32(len(data)),
	}
	fmt.Println("_ READ MSG MSG _", hdr.Code(), len(data))
	return msg, nil
}

func (l *libp2pLegacyConn) WriteMsg(msg Msg) error {
	log.Info("libp2p write msg", "code", msg.Code, "size", msg.Size)

	l.writeLock.Lock()
	defer l.writeLock.Unlock()

	if l.write == nil {
		// open connection
		stream, err := l.host.NewStream(context.Background(), l.peer, protoLegacyV1)
		if err != nil {
			panic(err)
		}
		l.write = stream
	}

	hdr := newHeader(msg.Code, msg.Size)
	if _, err := l.write.Write(hdr); err != nil {
		panic(err)
	}

	raw, err := ioutil.ReadAll(msg.Payload)
	if err != nil {
		panic(err)
	}
	if _, err := l.write.Write(raw); err != nil {
		panic(err)
	}
	return nil
}

func (l *libp2pTransportV2) Dial(node *enode.Node) (*Peer, error) {
	mAddr, err := node.GetMultiAddr()
	if err != nil {
		return nil, err
	}

	addrInfo, err := peer.AddrInfoFromP2pAddr(mAddr)
	if err != nil {
		return nil, err
	}

	fmt.Println(l)
	fmt.Println(l.host)

	if err := l.host.Connect(context.Background(), *addrInfo); err != nil {
		// this means that it could not connect
		return nil, err
	}

	ch := make(chan *libp2pEvent, 1)
	sub := l.feed.Subscribe(ch)
	defer sub.Unsubscribe()

	var peer *Peer
	for ev := range ch {
		if ev.Peer.peerID == addrInfo.ID {
			peer = ev.Peer
			break
		}
	}

	return peer, nil
}

type libp2pEvent struct {
	Peer *Peer
}

func handshakeToProto(obj *protoHandshake) *proto.HandshakeResponse {
	caps := []*proto.Cap{}
	for _, i := range obj.Caps {
		caps = append(caps, &proto.Cap{
			Name:    i.Name,
			Version: int64(i.Version),
		})
	}
	return &proto.HandshakeResponse{
		Version:    int64(obj.Version),
		Name:       obj.Name,
		Caps:       caps,
		ListenPort: int64(obj.ListenPort),
		Id:         hex.EncodeToString(obj.ID),
	}
}

func protoToHandshake(obj *proto.HandshakeResponse) *protoHandshake {
	caps := []Cap{}
	for _, i := range obj.Caps {
		caps = append(caps, Cap{
			Name:    i.Name,
			Version: uint(i.Version),
		})
	}
	id, _ := hex.DecodeString(obj.Id)
	return &protoHandshake{
		Version:    uint64(obj.Version),
		Name:       obj.Name,
		Caps:       caps,
		ListenPort: uint64(obj.ListenPort),
		ID:         id,
	}
}

// transport format

const (
	sizeOfCode   = 8
	sizeOfLength = 4
	headerSize   = sizeOfCode + sizeOfLength
)

func newHeader(code uint64, length uint32) header {
	hdr := header(make([]byte, headerSize))
	hdr.encode(code, length)
	return hdr
}

type header []byte

func (h header) Code() uint64 {
	return binary.BigEndian.Uint64(h[0:8])
}

func (h header) Length() uint32 {
	return binary.BigEndian.Uint32(h[8:12])
}

func (h header) encode(code uint64, length uint32) {
	binary.BigEndian.PutUint64(h[0:8], code)
	binary.BigEndian.PutUint32(h[8:12], length)
}

func toLibP2PCrypto(privkey *ecdsa.PrivateKey) crypto.PrivKey {
	return crypto.PrivKey((*crypto.Secp256k1PrivateKey)(privkey))
}
