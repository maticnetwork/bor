package p2p

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/network"
)

// things required by conn to work
// 1. Remote address in net.Conn (only)
// 2. WriterReader interface for the connection
type libp2pTransport struct {
	localAddr  *net.TCPAddr
	remoteAddr *net.TCPAddr
	stream     network.Stream
	closeCh    chan struct{}
	wlock      sync.Mutex
}

func (l *libp2pTransport) init() {
	l.closeCh = make(chan struct{})
}

func (l *libp2pTransport) ReadMsg() (Msg, error) {
	hdr := header(make([]byte, headerSize))
	if _, err := l.stream.Read(hdr); err != nil {
		panic(err)
	}

	data := make([]byte, hdr.Length())
	if _, err := l.stream.Read(data); err != nil {
		panic(err)
	}

	msg := Msg{
		Code:    hdr.Code(),
		Payload: bytes.NewReader(data),
	}
	fmt.Printf("Recv: %d %d\n", msg.Code, msg.Size)
	return msg, nil
}

func (l *libp2pTransport) WriteMsg(msg Msg) error {
	fmt.Printf("Send: %d %d\n", msg.Code, msg.Size)

	l.wlock.Lock()
	defer l.wlock.Unlock()

	hdr := header(make([]byte, headerSize))
	hdr.encode(msg.Code, msg.Size)

	if _, err := l.stream.Write(hdr); err != nil {
		panic(err)
	}

	// do we have to block this function??
	if _, err := io.Copy(l.stream, msg.Payload); err != nil {
		panic(err)
	}
	return nil
}

func (l *libp2pTransport) doEncHandshake(prv *ecdsa.PrivateKey) (*ecdsa.PublicKey, error) {
	panic("unimplemented")
}

func (l *libp2pTransport) doProtoHandshake(our *protoHandshake) (*protoHandshake, error) {
	panic("unimplemented")
}

func (l *libp2pTransport) close(err error) {
	close(l.closeCh)
	panic("unimplemented")
}

func (l *libp2pTransport) RemoteAddr() net.Addr {
	return l.remoteAddr
}

func (l *libp2pTransport) Read(b []byte) (n int, err error) {
	panic("unimplemented")
}

func (l *libp2pTransport) Write(b []byte) (n int, err error) {
	panic("unimplemented")
}

func (l *libp2pTransport) Close() error {
	panic("unimplemented")
}

func (l *libp2pTransport) LocalAddr() net.Addr {
	return l.localAddr
}

func (l *libp2pTransport) SetDeadline(t time.Time) error {
	panic("unimplemented")
}

func (l *libp2pTransport) SetReadDeadline(t time.Time) error {
	panic("unimplemented")
}

func (l *libp2pTransport) SetWriteDeadline(t time.Time) error {
	panic("unimplemented")
}

// transport format

const (
	sizeOfCode   = 8
	sizeOfLength = 4
	headerSize   = sizeOfCode + sizeOfLength
)

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
