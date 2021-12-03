package bor

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/stretchr/testify/assert"
)

func TestGenesisContractChange(t *testing.T) {
	addr0 := common.Address{0x1}

	b := &Bor{
		config: &params.BorConfig{
			Sprint: 10, // skip sprint transactions in sprint
			BlockAlloc: map[string]interface{}{
				// write as interface since that is how it is decoded in genesis
				"1": map[string]interface{}{
					addr0.Hex(): map[string]interface{}{
						"code":    hexutil.Bytes{0x1, 0x2},
						"balance": "0",
					},
				},
			},
		},
	}

	genspec := &core.Genesis{
		Alloc: map[common.Address]core.GenesisAccount{
			addr0: {
				Balance: big.NewInt(0),
				Code:    []byte{0x1, 0x1},
			},
		},
	}

	db := rawdb.NewMemoryDatabase()
	genesis := genspec.MustCommit(db)

	statedb, err := state.New(genesis.Root(), state.NewDatabase(db), nil)
	assert.NoError(t, err)

	assert.Equal(t, statedb.GetCode(addr0), []byte{0x1, 0x1})

	config := params.ChainConfig{}
	chain, err := core.NewBlockChain(db, nil, &config, b, vm.Config{}, nil, nil)
	assert.NoError(t, err)

	h := &types.Header{
		ParentHash: genesis.Hash(),
		Number:     big.NewInt(1),
	}
	b.Finalize(chain, h, statedb, nil, nil)

	// write state to database
	root, err := statedb.Commit(false)
	assert.NoError(t, err)
	assert.NoError(t, statedb.Database().TrieDB().Commit(root, true, nil))

	statedb1, err := state.New(h.Root, state.NewDatabase(db), nil)
	assert.NoError(t, err)

	assert.Equal(t, statedb1.GetCode(addr0), []byte{0x1, 0x2})
}
