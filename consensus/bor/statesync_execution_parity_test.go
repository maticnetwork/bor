package bor

import (
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/contract"
	"github.com/ethereum/go-ethereum/consensus/bor/statefull"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// recordingGenesisContract uses the production call path and captures cumulative gas.
type recordingGenesisContract struct {
	client  *contract.GenesisContractsClient
	gasUsed uint64
}

func (c *recordingGenesisContract) CommitState(event *clerk.EventRecordWithTime, state vm.StateDB, header *types.Header, chain statefull.ChainContext, config vm.Config) (uint64, error) {
	gasUsed, err := c.client.CommitState(event, state, header, chain, config)
	if err != nil {
		return 0, err
	}
	c.gasUsed += gasUsed
	return gasUsed, nil
}

func (*recordingGenesisContract) LastStateId(*state.StateDB, uint64, common.Hash) (*big.Int, error) {
	return new(big.Int), nil
}

func commitStateDatabase(t *testing.T, code map[common.Address][]byte, funded common.Address) (*state.CachingDB, common.Hash) {
	t.Helper()
	memoryDB := rawdb.NewMemoryDatabase()
	trieDB := triedb.NewDatabase(memoryDB, triedb.HashDefaults)
	stateDatabase := state.NewDatabase(trieDB, nil)
	statedb, err := state.New(common.Hash{}, stateDatabase)
	require.NoError(t, err)
	for address, bytecode := range code {
		statedb.SetCode(address, bytecode, tracing.CodeChangeUnspecified)
	}
	statedb.AddBalance(funded, uint256.NewInt(params.Ether), tracing.BalanceChangeUnspecified)
	root, err := statedb.Commit(0, false, false)
	require.NoError(t, err)
	require.NoError(t, trieDB.Commit(root, false))
	return stateDatabase, root
}

func newStateSyncParityHeader(chain *core.BlockChain, author common.Address, now time.Time) *types.Header {
	return &types.Header{
		Number:     big.NewInt(16),
		ParentHash: chain.HeaderChain().GetHeaderByNumber(0).Hash(),
		Coinbase:   author,
		Difficulty: big.NewInt(1),
		GasLimit:   params.MaxGasLimit,
		BaseFee:    new(big.Int),
		Time:       uint64(now.Unix()),
	}
}

// The first invocation stores 0x2a transiently. The second persists TLOAD(0)
// in slot one, exposing whether records share transaction-scoped storage.
var transientStateSyncCode = common.FromHex("0x6000541560175760005c600155600160005260206000f35b6001600055602a60005d600160005260206000f3")

func runTransientStorageParity(t *testing.T, gasBoundBlock *big.Int) common.Hash {
	t.Helper()
	now := time.Now()
	receiver := common.HexToAddress("0x1001")
	author := common.HexToAddress("0x1")
	borConfig := gasBoundBorConfig(gasBoundBlock)
	borConfig.BurntContract = map[string]string{"0": common.Address{}.Hex()}
	borConfig.ValidatorContract = common.HexToAddress("0x1000").Hex()
	chainConfig := newAllForksChainConfig(borConfig)
	chainConfig.ShanghaiBlock = big.NewInt(0)
	chainConfig.CancunBlock = big.NewInt(0)
	require.True(t, chainConfig.Rules(big.NewInt(16), false, uint64(now.Unix())).IsCancun)
	sp := &fakeSpanner{vals: []*valset.Validator{{Address: author, VotingPower: 1}}}
	chain, b := newChainAndBorForTestWithConfig(t, sp, chainConfig, true, author, uint64(now.Unix())-200)
	events := gasBoundEvents(2, now.Add(-60*time.Second))
	b.SetHeimdallClient(&mockHeimdallClient{events: events})
	header := newStateSyncParityHeader(chain, author, now)
	chainContext := statefull.ChainContext{Chain: chain.HeaderChain(), Bor: b}
	stateDatabase, root := commitStateDatabase(t, map[common.Address][]byte{receiver: transientStateSyncCode}, author)
	newState := func() *state.StateDB {
		statedb, err := state.New(root, stateDatabase)
		require.NoError(t, err)
		return statedb
	}

	recorder := &recordingGenesisContract{client: contract.NewGenesisContractsClient(chainConfig, borConfig.ValidatorContract, borConfig.StateReceiverContract, nil)}
	b.GenesisContractsClient = recorder
	liveState := newState()
	stateSyncs, err := b.CommitStates(liveState, header, chainContext)
	require.NoError(t, err)
	require.Len(t, stateSyncs, 2)

	stateSyncTx := types.NewTx(&types.StateSyncTx{StateSyncData: stateSyncs})
	message, err := core.TransactionToMessage(stateSyncTx, types.MakeSigner(chainConfig, header.Number, header.Time), header.BaseFee)
	require.NoError(t, err)
	traceState := newState()
	traceState.SetTxContext(stateSyncTx.Hash(), 0)
	traceEVM := vm.NewEVM(core.NewEVMBlockContext(header, chainContext, &header.Coinbase), traceState, chainConfig, vm.Config{})
	traceResult, err := statefull.ApplyStateSyncEvents(t.Context(), traceEVM, stateSyncTx, message, receiver)
	require.NoError(t, err)
	require.NoError(t, traceResult.Err)
	require.Equal(t, recorder.gasUsed, traceResult.UsedGas)
	observedSlot := common.BigToHash(big.NewInt(1))
	require.Equal(t, common.BigToHash(big.NewInt(1)), liveState.GetState(receiver, common.Hash{}))
	require.Equal(t, liveState.GetState(receiver, observedSlot), traceState.GetState(receiver, observedSlot))
	require.Equal(t, liveState.Copy().IntermediateRoot(true), traceState.Copy().IntermediateRoot(true))
	return liveState.GetState(receiver, observedSlot)
}

// TestCommitStates_TransientStorageTraceParity pins record isolation with real
// Cancun opcodes and verifies that trace replay follows the same fork boundary.
func TestCommitStates_TransientStorageTraceParity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		gasBoundBlock *big.Int
		wantObserved  common.Hash
	}{
		{"before activation", big.NewInt(17), common.BigToHash(big.NewInt(0x2a))},
		{"after activation", big.NewInt(0), common.Hash{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.wantObserved, runTransientStorageParity(t, test.gasBoundBlock))
		})
	}
}
