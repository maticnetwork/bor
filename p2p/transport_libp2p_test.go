package p2p

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/assert"
)

func newMsg(code uint64, data []byte) Msg {
	return Msg{
		Code:    code,
		Size:    uint32(len(data)),
		Payload: bytes.NewReader(data),
	}
}

func TestTransportLibP2P(t *testing.T) {
	doneCh := make(chan struct{})

	var p0 MsgReadWriter
	var p1 MsgReadWriter

	b0 := newMockBackend(5000).WithLibP2P(5001).WithProtocol(Protocol{
		Run: func(peer *Peer, rw MsgReadWriter) error {
			p0 = rw
			<-doneCh
			return nil
		},
	})
	l0 := newLibp2pTransportV2(b0)
	l0.init(5001)

	b1 := newMockBackend(6000).WithLibP2P(6001).WithProtocol(Protocol{
		Run: func(peer *Peer, rw MsgReadWriter) error {
			p1 = rw
			<-doneCh
			return nil
		},
	})
	l1 := newLibp2pTransportV2(b1)
	l1.init(6001)

	fmt.Println(b0.Enode())
	l1.Dial(b0.Enode())

	time.Sleep(1 * time.Second)

	go func() {
		fmt.Println(p0.WriteMsg(newMsg(1, []byte{0x1})))
	}()

	msg, err := p1.ReadMsg()
	fmt.Println(msg)
	fmt.Println(err)

	// time.Sleep(5 * time.Second)
}

func TestTransportLibP2P_NodeAddr(t *testing.T) {
	// TODO
	// ExtractPublicKey

	nn := enode.MustParse("enode://1dd9d65c4552b5eb43d5ad55a2ee3f56c6cbc1c64a5c8d659f51fcd51bace24351232b8d7821617d2b29b54b81cdefb9b3e9c37d7fd5f63270bcc9e1a6f6a439@[::]:52150?libp2p=2525")

	ma, err := enode.EnodeToMultiAddr(nn)
	assert.NoError(t, err)

	fmt.Println(ma)

	nn.Pubkey()
}

func TestEnodePeerIDConversion(t *testing.T) {
	nn := enode.MustParse("enode://1dd9d65c4552b5eb43d5ad55a2ee3f56c6cbc1c64a5c8d659f51fcd51bace24351232b8d7821617d2b29b54b81cdefb9b3e9c37d7fd5f63270bcc9e1a6f6a439@[::]:52150?libp2p=2525")

	raw, _ := nn.ID().MarshalText()
	fmt.Println(string(raw))
	fmt.Println(nn.String())

	// fmt.Println(enodeToPeerID(nn.ID()))

}
