package enode

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMultiAddr(t *testing.T) {
	enode := MustParse("enode://1dd9d65c4552b5eb43d5ad55a2ee3f56c6cbc1c64a5c8d659f51fcd51bace24351232b8d7821617d2b29b54b81cdefb9b3e9c37d7fd5f63270bcc9e1a6f6a439@[::]:52150?libp2p=2525")

	ma, err := EnodeToMultiAddr(enode)
	assert.NoError(t, err)
	assert.Equal(t, ma.String(), "/ip6/::/tcp/2525/p2p/16Uiu2HAmEfWq7SDmG1FmstB4MdGc2CXvnWWNFUTR2RvEf1guiF6a")
}
