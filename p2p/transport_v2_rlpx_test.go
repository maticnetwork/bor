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
	b1 := newMockBackend(8000)
	t1 := &rlpxTransportV2{b: b1}
	t1.Listen(b1.Addr())

	b2 := newMockBackend(9000)
	t2 := &rlpxTransportV2{b: b2}
	t2.Listen(b2.Addr())

	//timeout because no listener
	//_, err := t1.Dial(b2.Enode())
	//assert.Error(t, err)

	go func() {
		p, err := t2.Accept()
		if err != nil {
			panic(err)
		}
		if err := p.rlpx.WriteMsg(Msg{Code: 11}); err != nil {
			panic(err)
		}
	}()

	p1, err := t1.Dial(b2.Enode())
	assert.NoError(t, err)

	msg, err := p1.rlpx.ReadMsg()
	assert.NoError(t, err)
	assert.Equal(t, msg.Code, uint64(11))
}

func newMockBackend(port int) *mockBackend {
	prv, _ := crypto.GenerateKey()
	pub := crypto.FromECDSAPub(&prv.PublicKey)[1:]

	proto := &protoHandshake{
		Version: 3,
		ID:      pub,
		Caps:    []Cap{{"a", 0}, {"b", 2}},
	}

	mock := &mockBackend{
		port:  port,
		prv:   prv,
		proto: proto,
	}
	return mock
}

type mockBackend struct {
	port  int
	prv   *ecdsa.PrivateKey
	proto *protoHandshake
}

func (m *mockBackend) PrivateKey() *ecdsa.PrivateKey {
	return m.prv
}

func (m *mockBackend) LocalHandshake() *protoHandshake {
	return m.proto
}

func (m *mockBackend) Addr() string {
	return fmt.Sprintf("127.0.0.1:%d", m.port)
}

func (m *mockBackend) Enode() *enode.Node {
	return enode.NewV4(&m.prv.PublicKey, net.ParseIP("127.0.0.1"), m.port, m.port)
}
