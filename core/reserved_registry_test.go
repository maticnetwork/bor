package core_test

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/contract/registrytest"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/params"
)

// TestBlockChain_ReservedRegistry_NilByDefault asserts that a chain built
// without wiring a registry returns nil from the accessor — block validation
// paths must tolerate this rather than dereferencing.
func TestBlockChain_ReservedRegistry_NilByDefault(t *testing.T) {
	chain := newBareBlockChain(t)
	defer chain.Stop()

	require.Nil(t, chain.ReservedRegistry(), "fresh BlockChain must report no registry until SetReservedRegistry is called")
}

// TestBlockChain_ReservedRegistry_SeesContractState wires a registry-backed
// reader into the chain and confirms the chain's accessor surfaces the same
// view as the harness reader.
func TestBlockChain_ReservedRegistry_SeesContractState(t *testing.T) {
	h := registrytest.NewHarness(t)
	chain := newBareBlockChain(t)
	defer chain.Stop()

	chain.SetReservedRegistry(h.Reader)

	reader := chain.ReservedRegistry()
	require.NotNil(t, reader)
	require.True(t, reader.HasReservedRegistry())

	reserved, err := reader.IsReservedAddress(nil, 0, common.Hash{}, h.ReservedAddr)
	require.NoError(t, err)
	require.True(t, reserved)

	reserved, err = reader.IsReservedAddress(nil, 0, common.Hash{}, h.UnreservedAddr)
	require.NoError(t, err)
	require.False(t, reserved)
}

func newBareBlockChain(t *testing.T) *core.BlockChain {
	t.Helper()
	db := rawdb.NewMemoryDatabase()
	gspec := &core.Genesis{
		BaseFee: big.NewInt(params.InitialBaseFee),
		Config:  params.AllEthashProtocolChanges,
	}
	engine := ethash.NewFullFaker()
	chain, err := core.NewBlockChain(db, gspec, engine, core.DefaultConfig().WithStateScheme(rawdb.HashScheme))
	require.NoError(t, err)
	return chain
}
