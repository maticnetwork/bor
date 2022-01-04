package p2p

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/assert"
)

func TestTransportLibP2P(t *testing.T) {
	b0 := newMockBackend(5000).WithLibP2P(5001)
	l0 := &libp2pTransportV2{b: b0}
	l0.init(5001)

	b1 := newMockBackend(6000).WithLibP2P(6001)
	l1 := &libp2pTransportV2{b: b1}
	l1.init(6001)

	fmt.Println(b0.Enode())
	l1.Dial(b0.Enode())

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
