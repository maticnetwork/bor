package bor

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/statefull"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// TestSystemTxVMConfig verifies that system transactions (span commits and
// state-sync events) are traced only when the caller traces the state they
// are applied to. The node-wide live tracer stored in c.vmConfig must never
// leak into contexts that pass a plain state (miner and eth_simulateV1 via
// FinalizeAndAssemble, historical state regeneration via Finalize): those run
// outside the import goroutine, and invoking the singleton live tracer
// concurrently corrupts it.
func TestSystemTxVMConfig(t *testing.T) {
	t.Parallel()

	liveTracer := &tracing.Hooks{
		OnEnter: func(depth int, typ byte, from, to common.Address, input []byte, gas uint64, value *big.Int) {
			t.Error("live tracer must not be invoked for untraced states")
		},
	}
	c := &Bor{vmConfig: vm.Config{Tracer: liveTracer, NoBaseFee: true}}

	plainState, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	require.NoError(t, err)

	// A plain state (regeneration, miner, eth_simulateV1) must not get the
	// live tracer, but keeps the rest of the config.
	cfg := c.systemTxVMConfig(plainState)
	require.Nil(t, cfg.Tracer)
	require.True(t, cfg.NoBaseFee)

	// A hooked state (canonical import with a live tracer) keeps being traced
	// with the hooks that trace the state itself.
	importHooks := &tracing.Hooks{}
	cfg = c.systemTxVMConfig(state.NewHookedState(plainState, importHooks))
	require.Same(t, importHooks, cfg.Tracer)
}

// capturingGenesisContract records the vm.Config each CommitState call receives.
type capturingGenesisContract struct {
	lastStateID uint64
	captured    []vm.Config
}

func (m *capturingGenesisContract) CommitState(event *clerk.EventRecordWithTime, state vm.StateDB, header *types.Header, chCtx statefull.ChainContext, vmCfg vm.Config) (uint64, error) {
	m.captured = append(m.captured, vmCfg)
	return 0, nil
}

func (m *capturingGenesisContract) LastStateId(st *state.StateDB, number uint64, hash common.Hash) (*big.Int, error) {
	return big.NewInt(int64(m.lastStateID)), nil
}

// TestCommitStates_SystemTxTracer asserts end to end through CommitStates that
// state-sync system transactions receive the tracer of the state they mutate:
// the hooks tracing the state during canonical import, and no tracer at all
// for plain states (miner and eth_simulateV1 via FinalizeAndAssemble,
// historical state regeneration via Finalize) — never the node-wide live
// tracer stored in c.vmConfig.
func TestCommitStates_SystemTxTracer(t *testing.T) {
	t.Parallel()

	addr1 := common.HexToAddress("0x1")
	sp := &fakeSpanner{vals: []*valset.Validator{{Address: addr1, VotingPower: 1}}}

	now := time.Now()
	chain, b := newChainAndBorForTest(t, sp, indoreBorConfig(), true, addr1, uint64(now.Unix()))

	// The node-wide live tracer configured at startup: must never reach system txs.
	b.vmConfig = vm.Config{Tracer: &tracing.Hooks{}, NoBaseFee: true}

	gc := &capturingGenesisContract{lastStateID: 0}
	b.GenesisContractsClient = gc
	b.SetHeimdallClient(&mockHeimdallClient{
		events: []*clerk.EventRecordWithTime{
			{
				EventRecord: clerk.EventRecord{ID: 1, Contract: common.HexToAddress("0x1001"), Data: []byte{0x01}, ChainID: "1"},
				Time:        now.Add(-10 * time.Second),
			},
		},
	})

	genesis := chain.HeaderChain().GetHeaderByNumber(0)
	header := &types.Header{Number: big.NewInt(16), ParentHash: genesis.Hash(), Time: uint64(now.Unix())}
	cx := statefull.ChainContext{Chain: chain.HeaderChain(), Bor: b}

	// Plain state (regeneration, miner, eth_simulateV1): no tracer, rest of the
	// engine config preserved.
	_, err := b.CommitStates(newStateDBForTest(t, genesis.Root), header, cx)
	require.NoError(t, err)
	require.Len(t, gc.captured, 1)
	require.Nil(t, gc.captured[0].Tracer)
	require.True(t, gc.captured[0].NoBaseFee)

	// Hooked state (canonical import with a live tracer): traced with the hooks
	// tracing the state itself.
	gc.captured = nil
	importHooks := &tracing.Hooks{}
	_, err = b.CommitStates(state.NewHookedState(newStateDBForTest(t, genesis.Root), importHooks), header, cx)
	require.NoError(t, err)
	require.Len(t, gc.captured, 1)
	require.Same(t, importHooks, gc.captured[0].Tracer)
}
