package p2p

import (
	"testing"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/stretchr/testify/assert"
)

func TestPeerList(t *testing.T) {
	p := newPeerList()
	assert.Equal(t, 0, p.len())

	assert.True(t, p.add(enode.ID{0x1}))
	assert.Equal(t, 1, p.len())

	assert.False(t, p.add(enode.ID{0x1}))
	assert.Equal(t, 1, p.len())

	assert.True(t, p.add(enode.ID{0x2}))
	assert.Equal(t, 2, p.len())

	assert.True(t, p.del(enode.ID{0x2}))
	assert.False(t, p.del(enode.ID{0x2}))
}
