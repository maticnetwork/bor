package p2p

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/proto"
	gproto "github.com/golang/protobuf/proto"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	noise "github.com/libp2p/go-libp2p-noise"
	"github.com/multiformats/go-multiaddr"
)

type libp2pTransportV2 struct {
	b       backendv2
	host    host.Host
	pending peerList
	feed    event.Feed
}

const (
	// protoHandshakeV1 is the protocol used right after the peer connection is established
	// to perform an initial handshake
	protoHandshakeV1 = protocol.ID("handshake/0.1")

	// protoLegacyV1 is the protocol used to send any data belonging to the legacy DevP2P protocol
	protoLegacyV1 = protocol.ID("legacy/0.1")
)

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
		// TODO: Send our handshake message
		msg := &proto.HandshakeResponse{}
		raw, err := gproto.Marshal(msg)
		if err != nil {
			panic(err)
		}

		hdr := header(make([]byte, headerSize))
		hdr.encode(0, uint32(len(raw)))

		if _, err := stream.Write(hdr); err != nil {
			panic(err)
		}
		if _, err := stream.Write(raw); err != nil {
			panic(err)
		}
	})

	host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(network network.Network, conn network.Conn) {
			peerID := conn.RemotePeer()

			// TODO: Convert peer.ID to enode.ID
			// Add enode.ID to l.pending

			go func() {
				// this has to always be in a co-routine in ConnectedF
				err := l.handleConnected(conn)
				// TODO: if it did not work we have to disconnect here

				// send event feed
				l.feed.Send(&libp2pEvent{
					ID:    peerID,
					Error: err,
				})
				// TODO: disable pending
			}()
		},
		DisconnectedF: func(network network.Network, conn network.Conn) {
			// TODO: Notify server that the node has disconnected
		},
	})

	// 3. Start stream for proto legacy codes since that is handled here
	l.host.SetStreamHandler(protoLegacyV1, l.handleLegacyProtocolStream)
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
	return nil
}

func (l *libp2pTransportV2) handleLegacyProtocolStream(network.Stream) {
	// legacy protocol that will receive all the messages
}

func (l *libp2pTransportV2) Dial(node *enode.Node) error {
	mAddr, err := node.GetMultiAddr()
	if err != nil {
		return err
	}

	addrInfo, err := peer.AddrInfoFromP2pAddr(mAddr)
	if err != nil {
		return err
	}

	if err := l.host.Connect(context.Background(), *addrInfo); err != nil {
		// this means that it could not connect
		return err
	}

	ch := make(chan *libp2pEvent, 1)
	sub := l.feed.Subscribe(ch)
	defer sub.Unsubscribe()

	for ev := range ch {
		if ev.ID == addrInfo.ID {
			fmt.Println("- done -")
			break
		}
	}

	// TODO: here we have to start the session

	return nil
}

type libp2pEvent struct {
	ID    peer.ID
	Error error
}
