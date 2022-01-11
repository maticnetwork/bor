package p2p

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newMsg(code uint64, data []byte) Msg {
	return Msg{
		Code:    code,
		Size:    uint32(len(data)),
		Payload: bytes.NewReader(data),
	}
}

func TestTransportLibP2P_ReadWrite(t *testing.T) {
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

	l1.Dial(b0.Enode())

	time.Sleep(1 * time.Second)

	go func() {
		p0.WriteMsg(newMsg(1, []byte{0x1}))
	}()

	msg, err := p1.ReadMsg()
	assert.NoError(t, err)
	assert.Equal(t, uint64(1), msg.Code)
}
