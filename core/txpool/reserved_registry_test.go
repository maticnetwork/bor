package txpool_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/contract/registrytest"
	"github.com/ethereum/go-ethereum/core/txpool"
)

// TestTxPool_ReservedRegistry_NilByDefault asserts the accessor on a freshly
// zeroed TxPool returns nil so nil-handle consumers don't panic.
func TestTxPool_ReservedRegistry_NilByDefault(t *testing.T) {
	var pool *txpool.TxPool
	require.Nil(t, pool.ReservedRegistry())

	pool = &txpool.TxPool{}
	require.Nil(t, pool.ReservedRegistry())
}

// TestTxPool_ReservedRegistry_SeesContractState wires a real registry-backed
// reader into the pool and verifies the pool reports the same view as the
// underlying contract for both registered and unregistered addresses.
func TestTxPool_ReservedRegistry_SeesContractState(t *testing.T) {
	h := registrytest.NewHarness(t)

	pool := &txpool.TxPool{}
	pool.SetReservedRegistry(h.Reader)

	reader := pool.ReservedRegistry()
	require.NotNil(t, reader)
	require.True(t, reader.HasReservedRegistry())

	reserved, err := reader.IsReservedAddress(nil, 0, common.Hash{}, h.ReservedAddr)
	require.NoError(t, err)
	require.True(t, reserved, "txpool must see registered address as reserved")

	reserved, err = reader.IsReservedAddress(nil, 0, common.Hash{}, h.UnreservedAddr)
	require.NoError(t, err)
	require.False(t, reserved)
}
