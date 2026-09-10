package core

import (
	"context"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// fakeReservedReader is a state-independent registryreader.Reader for tests
// that don't need a real registry contract. has gates HasReservedRegistry;
// clients (keyed by address) drives the whitelisted set, per-address lookups,
// and the raw total, so the zero value ("empty but present" reserved set)
// and a populated fixture (capacity/effectiveFrom behavior) share one type.
type fakeReservedReader struct {
	has     bool
	clients map[common.Address]registryreader.ClientLookup
}

func (f fakeReservedReader) HasReservedRegistry() bool { return f.has }

func (f fakeReservedReader) IsReservedAddress(_ *state.StateDB, _ uint64, _ common.Hash, a common.Address) (bool, error) {
	_, ok := f.clients[a]
	return ok, nil
}

func (f fakeReservedReader) ReservedClientForAddress(_ *state.StateDB, _ uint64, _ common.Hash, a common.Address) (registryreader.ClientLookup, error) {
	return f.clients[a], nil
}

func (f fakeReservedReader) Root(*state.StateDB, uint64, common.Hash) (common.Hash, error) {
	return common.Hash{}, nil
}

func (f fakeReservedReader) WhitelistedAddresses(*state.StateDB, uint64, common.Hash) ([]common.Address, error) {
	addrs := make([]common.Address, 0, len(f.clients))
	for a := range f.clients {
		addrs = append(addrs, a)
	}
	return addrs, nil
}

func (f fakeReservedReader) TotalReservedGas(*state.StateDB, uint64, common.Hash) (uint64, error) {
	var total uint64
	for _, c := range f.clients {
		total += c.GasQuota
	}
	return total, nil
}

// twoClientFakeReader builds a fakeReservedReader with one client effective
// immediately and one whose effectiveFrom is in the future, so
// EffectiveCapacity (what every processor/getter must report) differs from
// the raw registry total (TotalReservedGas) — the split the header-capacity
// work (§2.2) exists to price correctly.
func twoClientFakeReader(immediate, future common.Address, immediateQuota, futureQuota, futureEffectiveFrom uint64) fakeReservedReader {
	return fakeReservedReader{
		has: true,
		clients: map[common.Address]registryreader.ClientLookup{
			immediate: {ClientID: big.NewInt(1), GasQuota: immediateQuota, Active: true},
			future:    {ClientID: big.NewInt(2), GasQuota: futureQuota, Active: true, EffectiveFrom: futureEffectiveFrom},
		},
	}
}

func reservedActiveConfig(t *testing.T) *params.ChainConfig {
	t.Helper()
	cfg := *params.BorUnittestChainConfig
	bor := *cfg.Bor
	bor.ReservedBlockspaceBlock = big.NewInt(0) // active from genesis
	if bor.ReservedRegistryContract == "" {
		bor.ReservedRegistryContract = params.DefaultReservedRegistryContract
	}
	cfg.Bor = &bor
	return &cfg
}

// TestReservedSnapshotForBlock_StatelessHeaderChain pins the wiring that
// ExecuteStateless depends on: the reserved-blockspace reader must reach the
// classification path through a bare HeaderChain (the stateless/witness backend).
// Without a wired reader, a post-fork block with a configured registry is a hard
// error (a silent empty set would split the state root); with one wired, the
// snapshot builds.
func TestReservedSnapshotForBlock_StatelessHeaderChain(t *testing.T) {
	t.Parallel()

	activeCfg := reservedActiveConfig(t)
	header := &types.Header{Number: big.NewInt(10)}

	t.Run("wired reader classifies", func(t *testing.T) {
		hc := &HeaderChain{config: activeCfg, reservedRegistry: fakeReservedReader{has: true}}
		snap, err := ReservedSnapshotForBlock(hc, nil, header)
		require.NoError(t, err)
		require.NotNil(t, snap, "a wired reader must yield a snapshot, even an empty one")
	})

	t.Run("missing reader is a hard error", func(t *testing.T) {
		// A bare HeaderChain (the pre-fix stateless case): exposes ReservedRegistry()
		// but returns nil, so classification cannot proceed safely.
		hc := &HeaderChain{config: activeCfg}
		_, err := ReservedSnapshotForBlock(hc, nil, header)
		require.Error(t, err)
	})

	t.Run("pre-fork skips classification", func(t *testing.T) {
		cfg := *activeCfg
		bor := *cfg.Bor
		bor.ReservedBlockspaceBlock = big.NewInt(1000) // not yet active at #10
		cfg.Bor = &bor
		hc := &HeaderChain{config: &cfg} // no reader, but none needed pre-fork
		snap, err := ReservedSnapshotForBlock(hc, nil, header)
		require.NoError(t, err)
		require.Nil(t, snap)
	})

	t.Run("no registry configured stays dark", func(t *testing.T) {
		cfg := *activeCfg
		bor := *cfg.Bor
		bor.ReservedRegistryContract = "" // fork active but feature dark
		cfg.Bor = &bor
		hc := &HeaderChain{config: &cfg}
		snap, err := ReservedSnapshotForBlock(hc, nil, header)
		require.NoError(t, err)
		require.Nil(t, snap)
	})
}

// TestReservedSnapshotForBlock_StatelessEffectiveCapacity pins that the
// stateless (HeaderChain-only) execution path computes the identical
// EffectiveCapacity a full BlockChain would: both route through the same
// registryreader.BuildSnapshot given the same reader and parent state, so
// there is exactly one implementation to get right, not one per chain type.
func TestReservedSnapshotForBlock_StatelessEffectiveCapacity(t *testing.T) {
	t.Parallel()

	immediate := common.HexToAddress("0x00000000000000000000000000000000001111")
	future := common.HexToAddress("0x00000000000000000000000000000000002222")
	const immediateQuota, futureQuota, futureEffectiveFrom = 10_000_000, 5_000_000, 1_000
	reader := twoClientFakeReader(immediate, future, immediateQuota, futureQuota, futureEffectiveFrom)

	activeCfg := reservedActiveConfig(t)
	header := &types.Header{Number: big.NewInt(1)}

	hc := &HeaderChain{config: activeCfg, reservedRegistry: reader}
	snap, err := ReservedSnapshotForBlock(hc, nil, header)
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.Equal(t, uint64(immediateQuota), snap.EffectiveCapacity(),
		"future-effective client must be excluded from effective capacity")
	require.Equal(t, uint64(immediateQuota+futureQuota), snap.Capacity(),
		"raw capacity (totalReservedGas) includes the future client")
}

// reservedCapacityParityChain builds a real *BlockChain (ethash faker engine,
// so no Bor validator/signing setup is needed) with the reserved fork active
// from genesis and a fake registry reader wired in. Cancun and Giugliano are
// co-activated at genesis to satisfy checkReservedBlockspaceForkOrder.
func reservedCapacityParityChain(t *testing.T, reader registryreader.Reader) *BlockChain {
	t.Helper()
	cfg := reservedActiveConfig(t)
	cfg.ShanghaiBlock = big.NewInt(0)
	cfg.CancunBlock = big.NewInt(0)
	bor := *cfg.Bor
	bor.GiuglianoBlock = big.NewInt(0)
	cfg.Bor = &bor

	db := rawdb.NewMemoryDatabase()
	gspec := &Genesis{BaseFee: big.NewInt(params.InitialBaseFee), Config: cfg}
	chain, err := NewBlockChain(db, gspec, ethash.NewFullFaker(), DefaultConfig().WithStateScheme(rawdb.HashScheme))
	require.NoError(t, err)
	chain.SetReservedRegistry(reader)
	return chain
}

// TestReservedCapacityParity_ProcessorsAgree pins the wiring at the three
// ProcessResult-assembly sites named in §2.3: serial (state_processor.go),
// parallel V1 (parallel_state_processor.go, ParallelStateProcessor) and
// parallel V2 BlockSTM (parallel_state_processor.go, V2StateProcessor). All
// three must populate ReservedCapacity from the SAME registry snapshot's
// EffectiveCapacity(), read at the parent state — independent of the block's
// transactions (there are none in this block; capacity is a snapshot
// property, not a function of what executed). The future-effective client's
// quota is part of the raw registry total but not yet part of
// EffectiveCapacity, so a processor that accidentally read the raw total
// instead would be caught here.
func TestReservedCapacityParity_ProcessorsAgree(t *testing.T) {
	t.Parallel()

	immediate := common.HexToAddress("0x00000000000000000000000000000000001111")
	future := common.HexToAddress("0x00000000000000000000000000000000002222")
	const immediateQuota, futureQuota, futureEffectiveFrom = 10_000_000, 5_000_000, 1_000 // far beyond block 1
	reader := twoClientFakeReader(immediate, future, immediateQuota, futureQuota, futureEffectiveFrom)
	const wantCapacity = uint64(immediateQuota)

	chain := reservedCapacityParityChain(t, reader)
	defer chain.Stop()

	genesis := chain.Genesis()
	header := &types.Header{
		Number:     big.NewInt(1),
		ParentHash: genesis.Hash(),
		GasLimit:   genesis.GasLimit(),
		BaseFee:    genesis.BaseFee(),
		Time:       genesis.Time() + 1,
	}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{})
	author := common.HexToAddress("0x00000000000000000000000000000000000c0b0")

	freshState := func(t *testing.T) *state.StateDB {
		t.Helper()
		st, err := chain.StateAt(genesis.Root())
		require.NoError(t, err)
		return st
	}

	serialRes, err := NewStateProcessor(chain).Process(block, freshState(t), vm.Config{}, &author, context.Background())
	require.NoError(t, err)
	require.Equal(t, wantCapacity, serialRes.ReservedCapacity,
		"serial processor must report the effective capacity, not the raw total")

	v1Res, err := NewParallelStateProcessor(chain, chain).Process(block, freshState(t), vm.Config{}, &author, context.Background())
	require.NoError(t, err)
	require.Equal(t, wantCapacity, v1Res.ReservedCapacity, "parallel (V1) processor must agree with serial")

	v2Res, err := NewV2StateProcessor(chain, chain, 1).Process(block, freshState(t), vm.Config{}, &author, context.Background())
	require.NoError(t, err)
	require.Equal(t, wantCapacity, v2Res.ReservedCapacity, "parallel (V2 BlockSTM) processor must agree with serial")
}
