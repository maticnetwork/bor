package server

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/command/server/proto"
	"github.com/stretchr/testify/assert"
)

func TestGatherBlocks(t *testing.T) {
	type c struct {
		ABlock *big.Int
		BBlock *big.Int
	}
	val := &c{
		BBlock: new(big.Int).SetInt64(1),
	}

	expect := []*proto.StatusResponse_Fork{
		{
			Name:     "A",
			Disabled: true,
		},
		{
			Name:  "B",
			Block: 1,
		},
	}

	res := gatherForks(val)
	assert.Equal(t, res, expect)
}
