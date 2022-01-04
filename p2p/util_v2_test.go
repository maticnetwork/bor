package p2p

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/p2p/enode"
)

func TestPeerList(t *testing.T) {
	p := newPeerList()
	fmt.Println(p.len())

	fmt.Println(p.add(enode.ID{0x1}))
	fmt.Println(p.len())

	fmt.Println(p.add(enode.ID{0x1}))
	fmt.Println(p.len())
}
