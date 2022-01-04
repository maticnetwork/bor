package p2p

import (
	"crypto/ecdsa"
	"fmt"
	"net"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/assert"
)

func TestTranport_Rlpx(t *testing.T) {
	m1 := &mockProtocol{}
	b1 := newMockBackend(8000).WithProtocol(m1.Protocol())
	t1 := &rlpxTransportV2{b: b1}
	t1.Listen(b1.Addr())

	m2 := &mockProtocol{}
	b2 := newMockBackend(9000).WithProtocol(m2.Protocol())
	t2 := &rlpxTransportV2{b: b2}
	t2.Listen(b2.Addr())

	go func() {
		if _, err := t2.Accept(); err != nil {
			panic(err)
		}
		proto2 := <-m2.ch
		if err := proto2.rw.WriteMsg(Msg{Code: 11}); err != nil {
			panic(err)
		}
	}()

	_, err := t1.Dial(b2.Enode())
	assert.NoError(t, err)

	proto1 := <-m1.ch
	msg, err := proto1.rw.ReadMsg()
	assert.NoError(t, err)
	fmt.Println(msg.Code)

	// time.Sleep(35 * time.Second)
}

type mockProtocol struct {
	ch chan *streamRef
}

func (m *mockProtocol) Name(n string) {

}

func (m *mockProtocol) Protocol() Protocol {
	m.ch = make(chan *streamRef, 10)

	return Protocol{
		Name:    "mock",
		Version: 4,
		Length:  20,
		Run: func(peer *Peer, rw MsgReadWriter) error {
			m.ch <- &streamRef{peer, rw}

			done := make(chan bool)
			<-done // run needs to block
			return nil
		},
	}
}

type streamRef struct {
	peer *Peer
	rw   MsgReadWriter
}

func newMockBackend(port int) *mockBackend {
	prv, _ := crypto.GenerateKey()
	pub := crypto.FromECDSAPub(&prv.PublicKey)[1:]

	proto := &protoHandshake{
		Version: 3,
		ID:      pub,
		Caps:    []Cap{{"a", 0}, {"b", 2}, {"mock", 4}},
	}

	mock := &mockBackend{
		port:  port,
		prv:   prv,
		proto: proto,
	}
	return mock
}

type mockBackend struct {
	port       int
	libp2pPort int
	prv        *ecdsa.PrivateKey
	proto      *protoHandshake
	protocols  []Protocol
}

func (m *mockBackend) WithLibP2P(libp2pPort int) *mockBackend {
	m.libp2pPort = libp2pPort
	return m
}

func (m *mockBackend) WithProtocol(p Protocol) *mockBackend {
	m.protocols = append(m.protocols, p)
	return m
}

func (m *mockBackend) GetProtocols() []Protocol {
	return m.protocols
}

func (m *mockBackend) OnConnectValidate(c *conn) error {
	return nil
}

func (m *mockBackend) LocalPrivateKey() *ecdsa.PrivateKey {
	return m.prv
}

func (m *mockBackend) LocalHandshake() *protoHandshake {
	return m.proto
}

func (m *mockBackend) Addr() string {
	return fmt.Sprintf("127.0.0.1:%d", m.port)
}

func (m *mockBackend) Enode() *enode.Node {
	return enode.NewV4(&m.prv.PublicKey, net.ParseIP("127.0.0.1"), m.port, m.port, m.libp2pPort)
}
