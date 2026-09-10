package registrytest

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/stretchr/testify/require"
)

func TestHarness_DeploysAndAnswersQueries(t *testing.T) {
	h := NewHarness(t)

	require.True(t, h.Reader.HasReservedRegistry(), "deployed registry must report HasReservedRegistry")

	got, err := h.Reader.IsReservedAddress(nil, 0, common.Hash{}, h.ReservedAddr)
	require.NoError(t, err)
	require.True(t, got, "address registered via createClient must be reserved")

	got, err = h.Reader.IsReservedAddress(nil, 0, common.Hash{}, h.UnreservedAddr)
	require.NoError(t, err)
	require.False(t, got, "unregistered address must not be reserved")

	lookup, err := h.Reader.ReservedClientForAddress(nil, 0, common.Hash{}, h.ReservedAddr)
	require.NoError(t, err)
	require.True(t, lookup.Active)
	require.Equal(t, uint64(10_000_000), lookup.GasQuota)
	require.NotNil(t, lookup.ClientID)
	require.Equal(t, int64(1), lookup.ClientID.Int64(), "createClient assigns id starting at 1")
}

func TestSnapshot_BuildsFromRegistry(t *testing.T) {
	h := NewHarness(t)

	snap, err := registryreader.BuildSnapshot(h.Reader, nil, 1, common.Hash{}, 1)
	require.NoError(t, err)
	require.NotNil(t, snap)

	// The whitelisted address classifies reserved; the other does not.
	require.True(t, snap.IsReserved(h.ReservedAddr))
	require.False(t, snap.IsReserved(h.UnreservedAddr))

	// Snapshot mirrors the registry: capacity = the one client's quota, and a
	// non-zero root it can be cached against.
	require.Equal(t, uint64(10_000_000), snap.Capacity())
	require.NotEqual(t, common.Hash{}, snap.Root())

	// nil snapshot (no registry) classifies nothing and is safe.
	var none *registryreader.Snapshot
	require.False(t, none.IsReserved(h.ReservedAddr))
	require.Equal(t, uint64(0), none.Capacity())
}

// TestSnapshot_ExcludesRoutedFeeModeClient pins the fee-mode gate against the
// real registry bytecode: a feeMode 1 client counts toward the contract's raw
// totalReservedGas but never enters the effective set (see
// registryreader.FeeModeFree).
func TestSnapshot_ExcludesRoutedFeeModeClient(t *testing.T) {
	h := NewHarness(t)

	routedAdmin := common.HexToAddress("0x00000000000000000000000000000000000000ee")
	routedSender := common.HexToAddress("0x00000000000000000000000000000000000000ff")
	h.CreateClient(t, routedAdmin, 5_000_000, 1, []common.Address{routedSender})

	snap, err := registryreader.BuildSnapshot(h.Reader, nil, 1, common.Hash{}, 1)
	require.NoError(t, err)
	require.NotNil(t, snap)

	require.True(t, snap.IsReserved(h.ReservedAddr), "free-mode client stays reserved")
	require.False(t, snap.IsReserved(routedSender), "routed-mode client must not classify reserved")
	require.Equal(t, uint64(15_000_000), snap.Capacity(), "raw totalReservedGas counts both clients")
	require.Equal(t, uint64(10_000_000), snap.EffectiveCapacity(), "effective capacity excludes the routed client")
}
