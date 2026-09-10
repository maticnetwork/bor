package miner

import (
	"errors"
	"math/big"
	"testing"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"go.uber.org/mock/gomock"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/api"
	"github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	borSpan "github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/tests/bor/mocks"
	"github.com/ethereum/go-ethereum/triedb"
)

type DefaultBorMiner struct {
	Miner   *Miner
	Mux     *event.TypeMux //nolint:staticcheck
	Cleanup func(skipMiner bool)

	Ctrl               *gomock.Controller
	EthAPIMock         api.Caller
	HeimdallClientMock bor.IHeimdallClient
	ContractMock       bor.GenesisContract
}

func NewBorDefaultMiner(t *testing.T) *DefaultBorMiner {
	t.Helper()

	ctrl := gomock.NewController(t)

	ethAPI := api.NewMockCaller(ctrl)
	ethAPI.EXPECT().Call(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

	// Mock span 0 for heimdall
	span0 := createMockSpanForTest(common.Address{0x1}, "1337")

	validators := make([]*valset.Validator, len(span0.ValidatorSet.Validators))
	for i, v := range span0.ValidatorSet.Validators {
		validators[i] = &valset.Validator{
			Address:     common.HexToAddress(v.Signer),
			VotingPower: v.VotingPower,
		}
	}

	spanner := bor.NewMockSpanner(ctrl)
	spanner.EXPECT().GetCurrentValidatorsByHash(gomock.Any(), gomock.Any(), gomock.Any()).Return(borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet).Validators, nil).AnyTimes()

	heimdallClient := mocks.NewMockIHeimdallClient(ctrl)
	heimdallWSClient := mocks.NewMockIHeimdallWSClient(ctrl)
	heimdallClient.EXPECT().GetSpan(gomock.Any(), uint64(0)).Return(&span0, nil).AnyTimes()
	heimdallClient.EXPECT().GetLatestSpan(gomock.Any()).Return(&span0, nil).AnyTimes()
	heimdallClient.EXPECT().FetchMilestone(gomock.Any()).Return(&milestone.Milestone{}, nil).AnyTimes()
	heimdallClient.EXPECT().FetchStatus(gomock.Any()).Return(&ctypes.SyncInfo{CatchingUp: false}, nil).AnyTimes()
	heimdallClient.EXPECT().Close().Times(1)

	genesisContracts := bor.NewMockGenesisContract(ctrl)

	miner, mux, cleanup := createBorMiner(t, ethAPI, spanner, heimdallClient, heimdallWSClient, genesisContracts)

	return &DefaultBorMiner{
		Miner:              miner,
		Mux:                mux,
		Cleanup:            cleanup,
		Ctrl:               ctrl,
		EthAPIMock:         ethAPI,
		HeimdallClientMock: heimdallClient,
		ContractMock:       genesisContracts,
	}
}

// //nolint:staticcheck
func createBorMiner(t *testing.T, ethAPIMock api.Caller, spanner bor.Spanner, heimdallClientMock bor.IHeimdallClient, heimdallClientWSMock bor.IHeimdallWSClient, contractMock bor.GenesisContract) (*Miner, *event.TypeMux, func(skipMiner bool)) {
	t.Helper()

	// Create Ethash config
	chainDB, genspec, chainConfig := NewDBForFakes(t)

	engine := NewFakeBor(t, chainDB, chainConfig, ethAPIMock, spanner, heimdallClientMock, heimdallClientWSMock, contractMock)

	// Create Ethereum backend
	bc, err := core.NewBlockChain(chainDB, genspec, engine, core.DefaultConfig())
	if err != nil {
		t.Fatalf("can't create new chain %v", err)
	}

	statedb, _ := state.New(common.Hash{}, state.NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil))
	blockchain := &testBlockChainBor{chainConfig, statedb, 10000000, new(event.Feed)}

	pool := legacypool.New(testTxPoolConfigBor, blockchain)
	txpool, _ := txpool.New(testTxPoolConfigBor.PriceLimit, blockchain, []txpool.SubPool{pool})

	backend := NewMockBackendBor(bc, txpool)

	// Create event Mux
	mux := new(event.TypeMux)

	config := Config{
		Etherbase: common.HexToAddress("123456789"),
	}

	// Create Miner
	miner := New(backend, &config, chainConfig, mux, engine, nil, false)

	cleanup := func(skipMiner bool) {
		bc.Stop()
		engine.Close()

		if !skipMiner {
			miner.Close()
		}
	}

	return miner, mux, cleanup
}

type TensingObject interface {
	Helper()
	Fatalf(format string, args ...any)
}

func NewDBForFakes(t TensingObject) (ethdb.Database, *core.Genesis, *params.ChainConfig) {
	t.Helper()

	memdb := memorydb.New()
	chainDB := rawdb.NewDatabase(memdb)
	addr := common.HexToAddress("12345")
	genesis := core.DeveloperGenesisBlock(11_500_000, &addr)

	chainConfig, _, err, _ := core.SetupGenesisBlock(chainDB, triedb.NewDatabase(chainDB, triedb.HashDefaults), genesis)
	if err != nil {
		t.Fatalf("can't create new chain config: %v", err)
	}

	// Make a copy of BorConfig to avoid race conditions with parallel tests
	if chainConfig.Bor != nil {
		borCopy := *chainConfig.Bor
		chainConfig.Bor = &borCopy
	}

	chainConfig.Bor.Period = map[string]uint64{
		"0": 1,
	}
	chainConfig.Bor.Sprint = map[string]uint64{
		"0": 64,
	}

	return chainDB, genesis, chainConfig
}

func NewFakeBor(t TensingObject, chainDB ethdb.Database, chainConfig *params.ChainConfig, ethAPIMock api.Caller, spanner bor.Spanner, heimdallClientMock bor.IHeimdallClient, heimdallClientWSMock bor.IHeimdallWSClient, contractMock bor.GenesisContract) consensus.Engine {
	t.Helper()

	if chainConfig.Bor == nil {
		chainConfig.Bor = params.BorUnittestChainConfig.Bor
	}

	return bor.New(chainConfig, chainDB, ethAPIMock, spanner, heimdallClientMock, heimdallClientWSMock, contractMock, false, 0, vm.Config{})
}

func createMockSpanForTest(address common.Address, chainId string) borTypes.Span {
	// Mock span 0 for heimdall calls
	validator := valset.Validator{
		ID:               0,
		Address:          address,
		VotingPower:      100,
		ProposerPriority: 0,
	}
	validatorSet := valset.ValidatorSet{
		Validators: []*valset.Validator{&validator},
		Proposer:   &validator,
	}
	span0 := borTypes.Span{
		Id:                0,
		StartBlock:        0,
		EndBlock:          255,
		ValidatorSet:      borSpan.ConvertBorValSetToHeimdallValSet(&validatorSet),
		SelectedProducers: borSpan.ConvertBorValidatorsToHeimdallValidators([]*valset.Validator{&validator}),
		BorChainId:        chainId,
	}

	return span0
}

var (
	// Test chain configurations
	testTxPoolConfigBor legacypool.Config
)

// TODO - Arpit, Duplicate Functions
type mockBackendBor struct {
	bc     *core.BlockChain
	txPool *txpool.TxPool
}

func NewMockBackendBor(bc *core.BlockChain, txPool *txpool.TxPool) *mockBackendBor {
	return &mockBackendBor{
		bc:     bc,
		txPool: txPool,
	}
}

func (m *mockBackendBor) BlockChain() *core.BlockChain {
	return m.bc
}

// PeerCount implements Backend. Returns a constant; tests using
// mockBackendBor don't drive the peer count.
func (*mockBackendBor) PeerCount() int {
	return 1
}

func (m *mockBackendBor) TxPool() *txpool.TxPool {
	return m.txPool
}

func (m *mockBackendBor) StateAtBlock(block *types.Block, reexec uint64, base *state.StateDB, checkLive bool, preferDisk bool) (statedb *state.StateDB, err error) {
	return nil, errors.New("not supported")
}

// TODO - Arpit, Duplicate Functions
type testBlockChainBor struct {
	config        *params.ChainConfig
	statedb       *state.StateDB
	gasLimit      uint64
	chainHeadFeed *event.Feed
}

func (bc *testBlockChainBor) Config() *params.ChainConfig {
	return bc.config
}

func (bc *testBlockChainBor) CurrentBlock() *types.Header {
	return &types.Header{
		Number:   new(big.Int),
		GasLimit: bc.gasLimit,
	}
}

func (bc *testBlockChainBor) GetBlock(hash common.Hash, number uint64) *types.Block {
	return types.NewBlock(bc.CurrentBlock(), nil, nil, nil)
}

func (bc *testBlockChainBor) StateAt(common.Hash) (*state.StateDB, error) {
	return bc.statedb, nil
}

func (bc *testBlockChainBor) PostExecState(header *types.Header) (*state.StateDB, error) {
	return bc.statedb, nil
}

func (bc *testBlockChainBor) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return bc.chainHeadFeed.Subscribe(ch)
}
