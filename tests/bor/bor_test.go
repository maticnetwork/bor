//go:build integration
// +build integration

package bor

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"
	"golang.org/x/crypto/sha3"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	stakeTypes "github.com/0xPolygon/heimdall-v2/x/stake/types"

	ctypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/fdlimit"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	borMilestone "github.com/ethereum/go-ethereum/consensus/bor/heimdall/milestone"
	borSpan "github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/tracers"
	_ "github.com/ethereum/go-ethereum/eth/tracers/live" // register live tracers (noop, supply) so they're available via LiveDirectory.New
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests/bor/mocks"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

var (
	// addr1 = 0x71562b71999873DB5b286dF957af199Ec94617F7
	pkey1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	// addr2 = 0x9fB29AAc15b9A4B7F17c3385939b007540f4d791
	pkey2, _ = crypto.HexToECDSA("9b28f36fbd67381120752d6172ecdcf10e06ab2d9a1367aac00cdcd6ac7855d3")
)

func TestValidatorWentOffline(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Generate a batch of accounts to seal and fund with
	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	// Create an Ethash network based off of the Ropsten config
	// Generate a batch of accounts to seal and fund with
	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)

	var (
		stacks []*node.Node
		nodes  []*eth.Ethereum
		enodes []*enode.Node
	)
	for i := 0; i < 2; i++ {
		// Start the node and wait until it's up
		stack, ethBackend, err := InitMiner(genesis, keys[i], true)
		if err != nil {
			panic(err)
		}
		defer stack.Close()

		for stack.Server().NodeInfo().Ports.Listener == 0 {
			time.Sleep(250 * time.Millisecond)
		}
		// Connect the node to all the previous ones
		for _, n := range enodes {
			stack.Server().AddPeer(n)
		}
		// Start tracking the node and its enode
		stacks = append(stacks, stack)
		nodes = append(nodes, ethBackend)
		enodes = append(enodes, stack.Server().Self())
	}

	// Iterate over all the nodes and start mining
	time.Sleep(3 * time.Second)
	for _, node := range nodes {
		if err := node.StartMining(); err != nil {
			panic(err)
		}
	}

	for {

		// for block 1 to 8, the primary validator is node0
		// for block 9 to 16, the primary validator is node1
		// for block 17 to 24, the primary validator is node0
		// for block 25 to 32, the primary validator is node1
		blockHeaderVal0 := nodes[0].BlockChain().CurrentHeader()

		// we remove peer connection between node0 and node1
		if blockHeaderVal0.Number.Uint64() == 9 {
			stacks[0].Server().RemovePeer(enodes[1])
		}

		// here, node1 is the primary validator, node0 will sign out-of-turn

		// we add peer connection between node1 and node0
		if blockHeaderVal0.Number.Uint64() == 14 {
			stacks[0].Server().AddPeer(enodes[1])
		}

		// reorg happens here, node1 has higher difficulty, it will replace blocks by node0

		if blockHeaderVal0.Number.Uint64() == 30 {

			break
		}

	}

	// check block 10 miner ; expected author is node1 signer
	blockHeaderVal0 := nodes[0].BlockChain().GetHeaderByNumber(10)
	blockHeaderVal1 := nodes[1].BlockChain().GetHeaderByNumber(10)
	authorVal0, err := nodes[0].Engine().Author(blockHeaderVal0)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}
	authorVal1, err := nodes[1].Engine().Author(blockHeaderVal1)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}

	// check both nodes have the same block 10
	assert.Equal(t, authorVal0, authorVal1)

	// check node0 has block mined by node1
	assert.Equal(t, authorVal0, nodes[1].AccountManager().Accounts()[0])

	// check node1 has block mined by node1
	assert.Equal(t, authorVal1, nodes[1].AccountManager().Accounts()[0])

	// check block 11 miner ; expected author is node1 signer
	blockHeaderVal0 = nodes[0].BlockChain().GetHeaderByNumber(11)
	blockHeaderVal1 = nodes[1].BlockChain().GetHeaderByNumber(11)
	authorVal0, err = nodes[0].Engine().Author(blockHeaderVal0)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}
	authorVal1, err = nodes[1].Engine().Author(blockHeaderVal1)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}

	// check both nodes have the same block 11
	assert.Equal(t, authorVal0, authorVal1)

	// check node0 has block mined by node1
	assert.Equal(t, authorVal0, nodes[1].AccountManager().Accounts()[0])

	// check node1 has block mined by node1
	assert.Equal(t, authorVal1, nodes[1].AccountManager().Accounts()[0])

	// check block 12 miner ; expected author is node1 signer
	blockHeaderVal0 = nodes[0].BlockChain().GetHeaderByNumber(12)
	blockHeaderVal1 = nodes[1].BlockChain().GetHeaderByNumber(12)
	authorVal0, err = nodes[0].Engine().Author(blockHeaderVal0)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}
	authorVal1, err = nodes[1].Engine().Author(blockHeaderVal1)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}

	// check both nodes have the same block 12
	assert.Equal(t, authorVal0, authorVal1)

	// check node0 has block mined by node1
	assert.Equal(t, authorVal0, nodes[1].AccountManager().Accounts()[0])

	// check node1 has block mined by node1
	assert.Equal(t, authorVal1, nodes[1].AccountManager().Accounts()[0])

	// check block 17 miner ; expected author is node0 signer
	blockHeaderVal0 = nodes[0].BlockChain().GetHeaderByNumber(17)
	blockHeaderVal1 = nodes[1].BlockChain().GetHeaderByNumber(17)
	authorVal0, err = nodes[0].Engine().Author(blockHeaderVal0)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}
	authorVal1, err = nodes[1].Engine().Author(blockHeaderVal1)
	if err != nil {
		log.Error("Error in getting author", "err", err)
	}

	// check both nodes have the same block 17
	assert.Equal(t, authorVal0, authorVal1)

	// check node0 has block mined by node1
	assert.Equal(t, authorVal0, nodes[0].AccountManager().Accounts()[0])

	// check node1 has block mined by node1
	assert.Equal(t, authorVal1, nodes[0].AccountManager().Accounts()[0])
}

func TestForkWithBlockTime(t *testing.T) {
	cases := []struct {
		name          string
		sprint        map[string]uint64
		blockTime     map[string]uint64
		change        uint64
		producerDelay map[string]uint64
		forkExpected  bool
	}{
		{
			name: "No fork after 2 sprints with producer delay = max block time",
			sprint: map[string]uint64{
				"0": 16,
			},
			blockTime: map[string]uint64{
				"0":  5,
				"16": 2,
				"32": 8,
			},
			change: 2,
			producerDelay: map[string]uint64{
				"0": 8,
			},
			forkExpected: false,
		},
		{
			name: "No Fork after 1 sprint producer delay = max block time",
			sprint: map[string]uint64{
				"0": 16,
			},
			blockTime: map[string]uint64{
				"0":  5,
				"16": 2,
			},
			change: 1,
			producerDelay: map[string]uint64{
				"0": 5,
			},
			forkExpected: false,
		},
		{
			name: "Fork after 4 sprints with producer delay < max block time",
			sprint: map[string]uint64{
				"0": 16,
			},
			blockTime: map[string]uint64{
				"0":  2,
				"64": 5,
			},
			change: 4,
			producerDelay: map[string]uint64{
				"0": 4,
			},
			forkExpected: true,
		},
	}

	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Create an Ethash network based off of the Ropsten config
	// Generate a batch of accounts to seal and fund with
	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)
			genesis.Config.Bor.Sprint = test.sprint
			genesis.Config.Bor.Period = test.blockTime
			genesis.Config.Bor.BackupMultiplier = test.blockTime
			genesis.Config.Bor.ProducerDelay = test.producerDelay

			stacks, nodes, _ := setupMiner(t, 2, genesis)

			defer func() {
				for _, stack := range stacks {
					stack.Close()
				}
			}()

			// Iterate over all the nodes and start mining
			for _, node := range nodes {
				if err := node.StartMining(); err != nil {
					t.Fatal("Error occurred while starting miner", "node", node, "error", err)
				}
			}
			var wg sync.WaitGroup
			blockHeaders := make([]*types.Header, 2)
			ticker := time.NewTicker(time.Duration(test.blockTime["0"]) * time.Second)
			defer ticker.Stop()

			for i := 0; i < 2; i++ {
				wg.Add(1)

				go func(i int) {
					defer wg.Done()

					for range ticker.C {
						log.Info("Fetching header", "node", i, "sprint", test.sprint["0"], "change", test.change, "number", test.sprint["0"]*test.change+10)
						blockHeaders[i] = nodes[i].BlockChain().GetHeaderByNumber(test.sprint["0"]*test.change + 10)
						if blockHeaders[i] != nil {
							break
						}
					}

				}(i)
			}

			wg.Wait()

			// Before the end of sprint
			blockHeaderVal0 := nodes[0].BlockChain().GetHeaderByNumber(test.sprint["0"] - 1)
			blockHeaderVal1 := nodes[1].BlockChain().GetHeaderByNumber(test.sprint["0"] - 1)
			assert.Equal(t, blockHeaderVal0.Hash(), blockHeaderVal1.Hash())
			assert.Equal(t, blockHeaderVal0.Time, blockHeaderVal1.Time)

			author0, err := nodes[0].Engine().Author(blockHeaderVal0)
			if err != nil {
				t.Error("Error occurred while fetching author", "err", err)
			}
			author1, err := nodes[1].Engine().Author(blockHeaderVal1)
			if err != nil {
				t.Error("Error occurred while fetching author", "err", err)
			}
			assert.Equal(t, author0, author1)

			// After the end of sprint
			author2, err := nodes[0].Engine().Author(blockHeaders[0])
			if err != nil {
				t.Error("Error occurred while fetching author", "err", err)
			}
			author3, err := nodes[1].Engine().Author(blockHeaders[1])
			if err != nil {
				t.Error("Error occurred while fetching author", "err", err)
			}

			if test.forkExpected {
				assert.NotEqual(t, blockHeaders[0].Hash(), blockHeaders[1].Hash())
				assert.NotEqual(t, blockHeaders[0].Time, blockHeaders[1].Time)
				assert.NotEqual(t, author2, author3)
			} else {
				assert.Equal(t, blockHeaders[0].Hash(), blockHeaders[1].Hash())
				assert.Equal(t, blockHeaders[0].Time, blockHeaders[1].Time)
				assert.Equal(t, author2, author3)
			}
		})

	}

}

func TestInsertingSpanSizeBlocks(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": 128}
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	res := loadSpanFromFile(t)

	// Create mock heimdall client
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)
	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(nil, fmt.Errorf("span not found")).AnyTimes()
	_bor.SetHeimdallClient(h)

	block := init.genesis.ToBlock()

	spanner := getMockedSpanner(t, borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet).Validators)
	_bor.SetSpanner(spanner)

	// Insert sprintSize # of blocks so that span is fetched at the start of a new sprint.
	for i := uint64(1); i <= spanSize; i++ {
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet).Validators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	borValSet := borSpan.ConvertHeimdallValSetToBorValSet(res.ValidatorSet)

	spanner = getMockedSpanner(t, borValSet.Validators)
	_bor.SetSpanner(spanner)

	// Check validator set at the first block of a new span.
	validators, err := _bor.GetCurrentValidators(context.Background(), block.Hash(), spanSize)
	if err != nil {
		t.Fatalf("%s", err)
	}

	require.Equal(t, 3, len(validators))
	for i, validator := range validators {
		require.Equal(t, validator.Address.Bytes(), borValSet.Validators[i].Address.Bytes())
		require.Equal(t, validator.VotingPower, borValSet.Validators[i].VotingPower)
	}
}

func TestFetchStateSyncEvents_PreMadhugiriHF(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	stateSyncConfirmationDelay := int64(128)
	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": uint64(stateSyncConfirmationDelay)}
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	// Insert blocks for 0th sprint
	block := init.genesis.ToBlock()

	// Create a mock span 0
	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	borValSet := borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet)
	currentValidators := borValSet.Validators

	// Load mock span 0
	res := loadSpanFromFile(t)

	// reate mock bor spanner
	spanner := getMockedSpanner(t, currentValidators)
	_bor.SetSpanner(spanner)

	// Create mock heimdall client
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)

	// Mock state sync events
	fromID := uint64(1)
	// at # sprintSize, events are fetched for [fromID, (block-sprint).Time])
	// as indore hf is enabled, we need to consider the stateSyncConfirmationDelay and
	// we need to predict the time of 4th block (i.e. the sprint end block) to calculate
	// the correct value of to. As per the config, non sprint end primary blocks take
	// 1s and sprint end ones take 6s. This leads to 3*1 + 6 = 9s of added time from genesis.
	to := int64(chain.GetHeaderByNumber(0).Time) + 9 - stateSyncConfirmationDelay
	eventCount := 50

	sample := getSampleEventRecord(t)
	sample.Time = time.Unix(to-int64(eventCount+1), 0) // Last event.Time will be just < to
	eventRecords := generateFakeStateSyncEvents(sample, eventCount)

	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(nil, fmt.Errorf("span not found")).AnyTimes()
	_bor.SetHeimdallClient(h)

	// Insert sprintSize # of blocks so that span is fetched at the start of a new sprint
	for i := uint64(1); i < sprintSize; i++ {
		if IsSpanEnd(i) {
			currentValidators = borValSet.Validators
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, currentValidators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borValSet.Validators, false, nil, nil)

	// Validate the state sync transactions set by consensus.
	validateStateSyncEvents(t, eventRecords, chain.GetStateSync())

	insertNewBlock(t, chain, block)

	// TODO: Ideally bor receipts should be present but as all state-sync events fails in this
	// test, no logs are generated and hence empty receipts aren't written. Fix this.

	// Ensure bor receipts are stored correctly
	// borReceipt := chain.GetBorReceiptByHash(block.Hash())
	// t.Log(borReceipt)
	// require.NotNil(t, borReceipt, "bor receipt expected but found nil")
	// require.Equal(t, uint8(0), borReceipt.Type, "bor receipt should have type 0")

	receipts := chain.GetReceiptsByHash(block.Hash())
	require.Equal(t, 0, len(receipts), "no normal receipts should be found")
}

func TestFetchStateSyncEvents_PostMadhugiriHF(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	stateSyncConfirmationDelay := int64(128)
	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": uint64(stateSyncConfirmationDelay)}
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
		gen.Config.Bor.MadhugiriBlock = big.NewInt(0) // Enable Madhugiri hardfork from genesis.
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	// Insert blocks for 0th sprint
	block := init.genesis.ToBlock()

	// Create a mock span 0
	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	borValSet := borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet)
	currentValidators := borValSet.Validators

	// Load mock span 0
	res := loadSpanFromFile(t)

	// Create mock bor spanner
	spanner := getMockedSpanner(t, currentValidators)
	_bor.SetSpanner(spanner)

	// Create mock heimdall client
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)

	// Mock state sync events
	fromID := uint64(1)
	// at # sprintSize, events are fetched for [fromID, (block-sprint).Time])
	// as indore hf is enabled, we need to consider the stateSyncConfirmationDelay and
	// we need to predict the time of 4th block (i.e. the sprint end block) to calculate
	// the correct value of to. As per the config, non sprint end primary blocks take
	// 1s and sprint end ones take 6s. This leads to 3*1 + 6 = 9s of added time from genesis.
	to := int64(chain.GetHeaderByNumber(0).Time) + 9 - stateSyncConfirmationDelay
	eventCount := 50

	sample := getSampleEventRecord(t)
	sample.Time = time.Unix(to-int64(eventCount+1), 0) // Last event.Time will be just < to
	eventRecords := generateFakeStateSyncEvents(sample, eventCount)

	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(nil, fmt.Errorf("span not found")).AnyTimes()
	_bor.SetHeimdallClient(h)

	// Insert sprintSize # of blocks so that span is fetched at the start of a new sprint
	for i := uint64(1); i < sprintSize; i++ {
		if IsSpanEnd(i) {
			currentValidators = borValSet.Validators
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, currentValidators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borValSet.Validators, false, nil, nil)
	insertNewBlock(t, chain, block)

	// Fetch the last block and check if state-sync tx and receipts are available
	lastBlock := chain.GetBlockByNumber(block.NumberU64())
	txs := lastBlock.Transactions()
	require.Equal(t, 1, len(txs), "state-sync tx should be part of block body")
	require.Equal(t, uint8(types.StateSyncTxType), txs[0].Type(), "transaction should be of state-sync type")

	receipts := chain.GetReceiptsByHash(lastBlock.Hash())
	require.Equal(t, 1, len(receipts), "state-sync receipt should be stored against this block")
	require.Equal(t, uint8(types.StateSyncTxType), receipts[0].Type, "receipt should be of state-sync type")
	require.Equal(t, txs[0].Hash(), receipts[0].TxHash, "state-sync receipt hash should have correct tx hash")

	// Confirm if bor receipts are not stored separately
	borReceipt := chain.GetBorReceiptByHash(lastBlock.Hash())
	require.Nil(t, borReceipt, "bor receipt should not be stored separately")
}

func validateStateSyncEvents(t *testing.T, expected []*clerk.EventRecordWithTime, got []*types.StateSyncData) {
	require.Equal(t, len(expected), len(got), "number of state sync events should be equal")

	for i := 0; i < len(expected); i++ {
		require.Equal(t, expected[i].ID, got[i].ID, fmt.Sprintf("state sync ids should be equal - index: %d, expected: %d, got: %d", i, expected[i].ID, got[i].ID))
	}
}

// countingTracerEvents records the depth-bearing events (OnEnter, OnExit) that
// our counting layer observes after they have already passed through
// WrapStateSyncHooks. Methods are mutex-guarded because the live tracer is
// invoked from the chain processor goroutine.
type countingTracerEvents struct {
	mu       sync.Mutex
	onEnters []countedEnter
	onExits  []countedExit
}

type countedEnter struct {
	depth int
	from  common.Address
	to    common.Address
}

type countedExit struct {
	depth int
}

func (r *countingTracerEvents) recordEnter(e countedEnter) {
	r.mu.Lock()
	r.onEnters = append(r.onEnters, e)
	r.mu.Unlock()
}

func (r *countingTracerEvents) recordExit(e countedExit) {
	r.mu.Lock()
	r.onExits = append(r.onExits, e)
	r.mu.Unlock()
}

func (r *countingTracerEvents) snapshot() ([]countedEnter, []countedExit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	enters := make([]countedEnter, len(r.onEnters))
	exits := make([]countedExit, len(r.onExits))
	copy(enters, r.onEnters)
	copy(exits, r.onExits)
	return enters, exits
}

// reset clears the recorded events. Used between block-build and block-import
// in tests so the assertions only see the import-path execution.
func (r *countingTracerEvents) reset() {
	r.mu.Lock()
	r.onEnters = nil
	r.onExits = nil
	r.mu.Unlock()
}

// registerCountingNoop registers a live tracer that takes the real `noop` live
// tracer (registered by eth/tracers/live/noop.go's init()) and overrides only
// OnEnter and OnExit with thin counters that record the call and then forward
// to noop. All other hooks pass through to noop unchanged — including
// OnTxStart and OnTxEnd.
//
// The full hook chain at runtime is:
//
//	WrapStateSyncHooks  →  counting layer  →  real noop
//
// Why we don't wrap OnTxStart and OnTxEnd: WrapStateSyncHooks itself emits the
// synthetic root frame from inside its OnTxStart (a synthetic OnEnter at
// depth 0) and closes it from inside its OnTxEnd (a synthetic OnExit at
// depth 0). Both reach the inner via our counting OnEnter / OnExit. So:
//
//   - Observing an OnEnter with depth=0, from=BorSystemAddress, to=stateReceiver
//     is *direct* evidence that WrapStateSyncHooks.OnTxStart fired and
//     successfully forwarded its synthetic frame through every wrapping layer
//     to reach noop.
//   - Observing the matching OnExit at depth=0 is *direct* evidence that
//     WrapStateSyncHooks.OnTxEnd fired the synthetic close.
//
// Wrapping OnTxStart / OnTxEnd directly would only restate the wrapper's
// contract; observing the synthetic depth-0 events at OnEnter / OnExit
// validates that contract end-to-end through the full production wiring.
func registerCountingNoop(t *testing.T, name string) *countingTracerEvents {
	t.Helper()
	events := &countingTracerEvents{}
	tracers.LiveDirectory.Register(name, func(config json.RawMessage) (*tracing.Hooks, error) {
		inner, err := tracers.LiveDirectory.New("noop", config)
		if err != nil {
			return nil, fmt.Errorf("failed to instantiate inner noop tracer: %w", err)
		}
		// Struct-copy so every hook noop set (OnTxStart, OnTxEnd, OnBlockStart,
		// OnGenesisBlock, OnSystemCallStart, etc.) keeps its real noop
		// implementation. Override only OnEnter and OnExit.
		wrapped := *inner
		innerOnEnter := inner.OnEnter
		wrapped.OnEnter = func(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
			events.recordEnter(countedEnter{depth: depth, from: from, to: to})
			if innerOnEnter != nil {
				innerOnEnter(depth, typ, from, to, input, gas, value)
			}
		}
		innerOnExit := inner.OnExit
		wrapped.OnExit = func(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
			events.recordExit(countedExit{depth: depth})
			if innerOnExit != nil {
				innerOnExit(depth, output, gasUsed, err, reverted)
			}
		}
		return &wrapped, nil
	})
	return events
}

// TestStateSyncTracing_LiveTracerDoesNotPanic enables a live tracer (wired via
// eth.Config.VMTrace, which goes through the same wrapping path as production)
// on a Madhugiri chain and processes a sprint-end block containing a state-sync
// transaction with multiple bridge events.
//
// The tracer used is a thin counting layer wrapping the real `noop` live
// tracer registered in eth/tracers/live/noop.go — same production code path,
// not a hand-rolled mock. backend.go applies bortracing.WrapStateSyncHooks
// around the resulting tracer just as it would in production. The counting
// layer records observed events so the test can assert:
//
//  1. The full chain.InsertChain flow does not panic, even though state-sync
//     consists of N independent top-level EVM calls that would otherwise break
//     tracers expecting one root per tx.
//  2. The tracer observes OnTxStart for the state-sync tx, a single synthetic
//     OnEnter at depth 0 (the wrapper's injected root), and a matching
//     OnExit(depth=0) before OnTxEnd. Real commitState calls appear shifted to
//     depth>=1.
//  3. The synthetic events reach the inner noop tracer (verified by the
//     counter being incremented before forwarding) — proving the wrapper's
//     passthrough wiring is correct.
func TestStateSyncTracing_LiveTracerDoesNotPanic(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	const tracerName = "test-counted-noop"
	recorder := registerCountingNoop(t, tracerName)

	stateSyncConfirmationDelay := int64(128)
	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": uint64(stateSyncConfirmationDelay)}
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
		gen.Config.Bor.MadhugiriBlock = big.NewInt(0) // Madhugiri from genesis.
	}
	init := buildEthereumInstanceWithVMTrace(t, rawdb.NewMemoryDatabase(), tracerName, updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	block := init.genesis.ToBlock()
	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	borValSet := borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet)
	currentValidators := borValSet.Validators

	res := loadSpanFromFile(t)
	spanner := getMockedSpanner(t, currentValidators)
	_bor.SetSpanner(spanner)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	h := createMockHeimdall(ctrl, &span0, res)

	fromID := uint64(1)
	to := int64(chain.GetHeaderByNumber(0).Time) + 9 - stateSyncConfirmationDelay
	const eventCount = 5 // Multiple events so synthetic-root wrapping is meaningful.

	sample := getSampleEventRecord(t)
	sample.Time = time.Unix(to-int64(eventCount+1), 0)
	eventRecords := generateFakeStateSyncEvents(sample, eventCount)

	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(nil, fmt.Errorf("span not found")).AnyTimes()
	_bor.SetHeimdallClient(h)

	// Build out the sprint up to (but not including) the sprint-end block.
	for i := uint64(1); i < sprintSize; i++ {
		if IsSpanEnd(i) {
			currentValidators = borValSet.Validators
		}
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, currentValidators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	// Sprint-end block: this is the one that carries the state-sync tx.
	//
	// buildNextBlock invokes Bor.FinalizeAndAssemble, which also runs the
	// state-sync events through the same wrapped tracer (since c.vmConfig.Tracer
	// is the same wrapped hooks). But the miner path does NOT fire OnTxStart
	// on the tracer (see the TODO in Bor.FinalizeAndAssemble), so the wrapper's
	// `active` flag stays false and the miner-path commitState events fire
	// unshifted at depth=0. The state-sync tracing fix this PR introduces is
	// for the *import* path (state_processor.Process), so reset the counter
	// after buildNextBlock to isolate the import-path assertions.
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borValSet.Validators, false, nil, nil)
	recorder.reset()
	insertNewBlock(t, chain, block)

	// Sanity: the block has a state-sync tx in body and a matching receipt.
	lastBlock := chain.GetBlockByNumber(block.NumberU64())
	txs := lastBlock.Transactions()
	require.Equal(t, 1, len(txs), "state-sync tx should be in the sprint-end block body")
	require.Equal(t, uint8(types.StateSyncTxType), txs[0].Type(), "last tx should be state-sync type")

	// State-sync events are uniquely identified in the OnEnter stream by their
	// (from, to) pair: from=BorSystemAddress, to=StateReceiverContract. This is
	// true for both:
	//   - the synthetic root frame WrapStateSyncHooks.OnTxStart emits at depth 0
	//   - each real commitState call (shifted by the wrapper from depth 0 to 1)
	// Other depth-0 OnEnter events in the block come from system calls like
	// ProcessBeaconBlockRoot (from=params.SystemAddress, not BorSystemAddress)
	// or from regular txs (different to-addresses), so the address filter
	// isolates state-sync activity precisely.
	enters, exits := recorder.snapshot()
	stateReceiver := common.HexToAddress(init.genesis.Config.Bor.StateReceiverContract)

	var (
		stateSyncEntersAtDepth0 int
		stateSyncEntersAtDepth1 int
	)
	for _, e := range enters {
		if e.from != params.BorSystemAddress || e.to != stateReceiver {
			continue
		}
		switch e.depth {
		case 0:
			stateSyncEntersAtDepth0++
		case 1:
			stateSyncEntersAtDepth1++
		}
	}

	require.Equal(t, 1, stateSyncEntersAtDepth0,
		"expected exactly one synthetic OnEnter at depth 0 from BorSystemAddress to StateReceiverContract — "+
			"this is the depth-0 frame WrapStateSyncHooks.OnTxStart emits before any real commitState call, "+
			"so seeing it once proves OnTxStart fired and its synthetic frame reached the inner tracer")
	require.Equal(t, eventCount, stateSyncEntersAtDepth1,
		"expected one OnEnter at depth 1 per state-sync event — the wrapper shifts each real commitState's "+
			"top-level OnEnter from depth 0 to depth 1")

	// Sanity-check OnExit pairing: every OnEnter must have a matching OnExit at
	// the same depth. We count exits at depths 0 and 1 across the whole event
	// stream; depth-1 exits should equal eventCount (one per commitState), and
	// depth-0 exits must include the synthetic close from WrapStateSyncHooks.OnTxEnd.
	// (Other depth-0 exits exist from system calls like BeaconBlockRoot, so we
	// only assert lower bounds here; the unit tests cover exact pairing.)
	var depth0Exits, depth1Exits int
	for _, e := range exits {
		switch e.depth {
		case 0:
			depth0Exits++
		case 1:
			depth1Exits++
		}
	}
	require.GreaterOrEqual(t, depth0Exits, 1,
		"expected at least one OnExit at depth 0 — WrapStateSyncHooks.OnTxEnd emits a synthetic depth-0 close")
	require.Equal(t, eventCount, depth1Exits,
		"expected one OnExit at depth 1 per state-sync event")
}

func TestFetchStateSyncEvents_2(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	stateSyncConfirmationDelay := int64(128)
	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": uint64(stateSyncConfirmationDelay)}
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	// Create a mock span 0
	span0 := createMockSpan(addr, chain.Config().ChainID.String())

	// Load mock span 1
	res := loadSpanFromFile(t)

	borValSet := borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet)
	spanner := getMockedSpanner(t, borValSet.Validators)
	_bor.SetSpanner(spanner)

	// add the block producer
	res.ValidatorSet.Validators = append(res.ValidatorSet.Validators, &stakeTypes.Validator{
		Signer:      addr.String(),
		VotingPower: 4500,
	})

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)

	// Mock State Sync events
	// at # sprintSize, events are fetched for [fromID, (block-sprint).Time])
	// as indore hf is enabled, we need to consider the stateSyncConfirmationDelay and
	// we need to predict the time of 4th block (i.e. the sprint end block) to calculate
	// the correct value of to. As per the config, non sprint end primary blocks take
	// 1s and sprint end ones take 6s. This leads to 3*1 + 6 = 9s of added time from genesis.
	fromID := uint64(1)
	to := int64(chain.GetHeaderByNumber(0).Time) + 9 - stateSyncConfirmationDelay
	sample := getSampleEventRecord(t)

	// First query will be from [id=1, (block-sprint).Time]
	// Insert 5 events in this time range.
	eventRecords := []*clerk.EventRecordWithTime{
		buildStateEvent(sample, 1, 1), // id = 1, time = 1
		buildStateEvent(sample, 2, 3), // id = 2, time = 3
		buildStateEvent(sample, 3, 2), // id = 3, time = 2
		buildStateEvent(sample, 4, 5), // id = 4, time = 5
		// event with id 5 is missing
		buildStateEvent(sample, 6, 4), // id = 6, time = 4
	}

	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(nil, fmt.Errorf("span not found")).AnyTimes()
	_bor.SetHeimdallClient(h)

	// Insert the blocks for the 0th sprint.
	block := init.genesis.ToBlock()

	// Set the current validators from span0
	currentValidators := span0.ValidatorSet.Validators
	for i := uint64(1); i <= sprintSize; i++ {
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet).Validators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	lastStateID, _ := _bor.GenesisContractsClient.LastStateId(nil, sprintSize, block.Hash())

	// State 6 was not written.
	require.Equal(t, uint64(4), lastStateID.Uint64())

	// Same calculation for from and to as above
	fromID = uint64(5)
	to = int64(chain.GetHeaderByNumber(sprintSize).Time) + 9 - stateSyncConfirmationDelay

	eventRecords = []*clerk.EventRecordWithTime{
		buildStateEvent(sample, 5, 7),
		buildStateEvent(sample, 6, 4),
	}
	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).AnyTimes()

	for i := sprintSize + 1; i <= spanSize; i++ {
		// Update the validator set at the end of span and update the respective mocks
		if IsSpanEnd(i) {
			currentValidators = res.ValidatorSet.Validators

			// Set the spanner to point to new validator set
			spanner := getMockedSpanner(t, borSpan.ConvertHeimdallValSetToBorValSet(res.ValidatorSet).Validators)
			_bor.SetSpanner(spanner)

			// Update the span0's validator set to new validator set. This will be used in verify header when we query
			// span to compare validator's set with header's extradata. Even though our span store has old validator set
			// stored in cache, we're updating the underlying pointer here and hence we don't need to update the cache.
			span0.ValidatorSet.Validators = currentValidators
		} else {
			currentValidators = []*stakeTypes.Validator{{
				Signer:      addr.String(),
				VotingPower: 10,
			}}
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValSetToBorValSet(res.ValidatorSet).Validators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	lastStateID, _ = _bor.GenesisContractsClient.LastStateId(nil, spanSize, block.Hash())
	require.Equal(t, uint64(6), lastStateID.Uint64())
}

func TestOutOfTurnSigning(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": 128}
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	span0 := createMockSpan(addr, chain.Config().ChainID.String())

	res := loadSpanFromFile(t)
	res.ValidatorSet.Validators = append(res.ValidatorSet.Validators, &stakeTypes.Validator{
		Signer:      addr.String(),
		VotingPower: 10,
	})

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)
	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(&span0, nil).AnyTimes()
	_bor.SetHeimdallClient(h)

	spanner := getMockedSpanner(t, borSpan.ConvertHeimdallValSetToBorValSet(res.ValidatorSet).Validators)
	_bor.SetSpanner(spanner)

	block := init.genesis.ToBlock()

	setDifficulty := func(header *types.Header) {
		if IsSprintStart(header.Number.Uint64()) {
			header.Difficulty = big.NewInt(int64(len(res.ValidatorSet.Validators)))
		}
	}

	currentValidators := span0.ValidatorSet.Validators
	for i := uint64(1); i < spanSize; i++ {
		// Update the validator set before sprint end (so that it is returned when called for next block)
		// E.g. In this case, update on block 3 as snapshot of block 3 will be called for block 4's verification
		// Sprint length is 4 for this test
		if i == chain.Config().Bor.CalculateSprint(i)-1 {
			currentValidators = res.ValidatorSet.Validators

			// Update the span0's validator set to new validator set. This will be used in verify header when we query
			// span to compare validator's set with header's extradata. Even though our span store has old validator set
			// stored in cache, we're updating the underlying pointer here and hence we don't need to update the cache.
			span0.ValidatorSet.Validators = currentValidators
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{setDifficulty}, nil)
		insertNewBlock(t, chain, block)
	}

	// Insert the spanSize-th block.
	// This account is one the out-of-turn validators for the 1st (0-indexed) span.
	signer := "c8deb0bea5c41afe8e37b4d1bd84e31adff11b09c8c96ff4b605003cce067cd9"
	signerKey, _ := hex.DecodeString(signer)
	newKey, _ := crypto.HexToECDSA(signer)
	newAddr := crypto.PubkeyToAddress(newKey.PublicKey)
	expectedSuccessionNumber := 2

	parentTime := block.Time()

	setParentTime := func(header *types.Header) {
		header.Time = parentTime + 1
	}

	const turn = 1

	setDifficulty = func(header *types.Header) {
		header.Difficulty = big.NewInt(int64(len(res.ValidatorSet.Validators)) - turn)
	}

	block = buildNextBlock(t, _bor, chain, block, signerKey, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(res.ValidatorSet.Validators), false, []modifyHeaderFunc{setParentTime, setDifficulty}, nil)
	_, err := chain.InsertChain([]*types.Block{block}, false)
	require.Equal(t,
		bor.BlockTooSoonError{Number: spanSize, Succession: expectedSuccessionNumber},
		*err.(*bor.BlockTooSoonError))

	expectedDifficulty := uint64(len(res.ValidatorSet.Validators) - expectedSuccessionNumber - turn) // len(validators) - succession
	header := block.Header()

	diff := bor.CalcProducerDelay(header.Number.Uint64(), expectedSuccessionNumber, init.genesis.Config.Bor)
	header.Time += diff

	sign(t, header, signerKey, init.genesis.Config.Bor)

	block = types.NewBlockWithHeader(header)

	_, err = chain.InsertChain([]*types.Block{block}, false)
	require.NotNil(t, err)
	require.Equal(t,
		bor.WrongDifficultyError{Number: spanSize, Expected: expectedDifficulty, Actual: 3, Signer: newAddr.Bytes()},
		*err.(*bor.WrongDifficultyError))

	header.Difficulty = new(big.Int).SetUint64(expectedDifficulty)
	sign(t, header, signerKey, init.genesis.Config.Bor)
	block = types.NewBlockWithHeader(header)

	_, err = chain.InsertChain([]*types.Block{block}, false)
	require.Nil(t, err)
}

func TestSignerNotFound(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	span0 := createMockSpan(addr, chain.Config().ChainID.String())

	res := loadSpanFromFile(t)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)
	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()
	_bor.SetHeimdallClient(h)

	block := init.genesis.ToBlock()

	// Random signer account that is not a part of the validator set.
	const signer = "3714d99058cd64541433d59c6b391555b2fd9b54629c2b717a6c9c00d1127b6b"
	signerKey, _ := hex.DecodeString(signer)
	newKey, _ := crypto.HexToECDSA(signer)
	newAddr := crypto.PubkeyToAddress(newKey.PublicKey)

	_bor.Authorize(newAddr, func(account accounts.Account, s string, data []byte) ([]byte, error) {
		return crypto.Sign(crypto.Keccak256(data), newKey)
	})

	block = buildNextBlock(t, _bor, chain, block, signerKey, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(res.ValidatorSet.Validators), false, nil, nil)

	_, err := chain.InsertChain([]*types.Block{block}, false)
	require.Equal(t,
		*err.(*bor.UnauthorizedSignerError),
		bor.UnauthorizedSignerError{
			Number:         1,
			Signer:         newAddr.Bytes(),
			AllowedSigners: borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet).Validators,
		})
}

// TestEIP1559Transition tests the following:
//
//  1. A transaction whose gasFeeCap is greater than the baseFee is valid.
//  2. Gas accounting for access lists on EIP-1559 transactions is correct.
//  3. Only the transaction's tip will be received by the coinbase.
//  4. The transaction sender pays for both the tip and baseFee.
//  5. The coinbase receives only the partially realized tip when
//     gasFeeCap - gasTipCap < baseFee.
//  6. Legacy transaction behave as expected (e.g. gasPrice = gasFeeCap = gasTipCap).
func TestEIP1559Transition(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	var (
		aa = common.HexToAddress("0x000000000000000000000000000000000000aaaa")

		// Generate a canonical chain to act as the main dataset
		db     = rawdb.NewMemoryDatabase()
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		key3, _ = crypto.HexToECDSA("225171aed3793cba1c029832886d69785b7e77a54a44211226b447aa2d16b058")

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)
		funds = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
		gspec = &core.Genesis{
			Config: params.BorUnittestChainConfig,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
				addr3: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)

	gspec.Config.BerlinBlock = common.Big0
	gspec.Config.LondonBlock = common.Big0
	genesis := gspec.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))
	signer := types.LatestSigner(gspec.Config)

	blocks, _ := core.GenerateChain(gspec.Config, genesis, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to 0xAAAA
		accesses := types.AccessList{types.AccessTuple{
			Address:     aa,
			StorageKeys: []common.Hash{{0}},
		}}

		txdata := &types.DynamicFeeTx{
			ChainID:    gspec.Config.ChainID,
			Nonce:      0,
			To:         &aa,
			Gas:        30000,
			GasFeeCap:  newGwei(5),
			GasTipCap:  big.NewInt(2),
			AccessList: accesses,
			Data:       []byte{},
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key1)

		b.AddTx(tx)
	})

	diskdb := rawdb.NewMemoryDatabase()
	gspec.MustCommit(diskdb, triedb.NewDatabase(diskdb, triedb.HashDefaults))

	chain, err := core.NewBlockChain(diskdb, gspec, engine, core.DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	// 1+2: Ensure EIP-1559 access lists are accounted for via gas usage.
	expectedGas := params.TxGas + params.TxAccessListAddressGas + params.TxAccessListStorageKeyGas +
		vm.GasQuickStep*2 + params.WarmStorageReadCostEIP2929 + params.ColdSloadCostEIP2929
	if block.GasUsed() != expectedGas {
		t.Fatalf("incorrect amount of gas spent: expected %d, got %d", expectedGas, block.GasUsed())
	}

	state, _ := chain.State()

	// 3: Ensure that miner received only the tx's tip.
	actual := state.GetBalance(block.Coinbase()).ToBig()
	expected := new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*block.Transactions()[0].GasTipCap().Uint64()),
		ethash.ConstantinopleBlockReward.ToBig(),
	)
	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// check burnt contract balance
	actual = state.GetBalance(common.HexToAddress(params.BorUnittestChainConfig.Bor.CalculateBurntContract(block.NumberU64()))).ToBig()
	expected = new(big.Int).Mul(new(big.Int).SetUint64(block.GasUsed()), block.BaseFee())
	burntContractBalance := expected
	if actual.Cmp(expected) != 0 {
		t.Fatalf("burnt contract balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (tip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr1).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (block.Transactions()[0].GasTipCap().Uint64() + block.BaseFee().Uint64()))
	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}

	blocks, _ = core.GenerateChain(gspec.Config, block, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{2})

		txdata := &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key2)

		b.AddTx(tx)
	})

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block = chain.GetBlockByNumber(2)
	state, _ = chain.State()
	effectiveTip := block.Transactions()[0].GasTipCap().Uint64() - block.BaseFee().Uint64()

	// 6+5: Ensure that miner received only the tx's effective tip.
	actual = state.GetBalance(block.Coinbase()).ToBig()
	expected = new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*effectiveTip),
		ethash.ConstantinopleBlockReward.ToBig(),
	)
	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// check burnt contract balance
	actual = state.GetBalance(common.HexToAddress(params.BorUnittestChainConfig.Bor.CalculateBurntContract(block.NumberU64()))).ToBig()
	expected = new(big.Int).Add(burntContractBalance, new(big.Int).Mul(new(big.Int).SetUint64(block.GasUsed()), block.BaseFee()))
	burntContractBalance = expected
	if actual.Cmp(expected) != 0 {
		t.Fatalf("burnt contract balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (effectiveTip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr2).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (effectiveTip + block.BaseFee().Uint64()))
	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}

	blocks, _ = core.GenerateChain(gspec.Config, block, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{3})

		txdata := &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key3)

		b.AddTx(tx)

		accesses := types.AccessList{types.AccessTuple{
			Address:     aa,
			StorageKeys: []common.Hash{{0}},
		}}

		txdata2 := &types.DynamicFeeTx{
			ChainID:    gspec.Config.ChainID,
			Nonce:      1,
			To:         &aa,
			Gas:        30000,
			GasFeeCap:  newGwei(5),
			GasTipCap:  big.NewInt(2),
			AccessList: accesses,
			Data:       []byte{},
		}
		tx = types.NewTx(txdata2)
		tx, _ = types.SignTx(tx, signer, key3)

		b.AddTx(tx)

	})

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block = chain.GetBlockByNumber(3)
	state, _ = chain.State()

	// check burnt contract balance
	actual = state.GetBalance(common.HexToAddress(params.BorUnittestChainConfig.Bor.CalculateBurntContract(block.NumberU64()))).ToBig()
	burntAmount := new(big.Int).Mul(
		block.BaseFee(),
		big.NewInt(int64(block.GasUsed())),
	)
	expected = new(big.Int).Add(burntContractBalance, burntAmount)
	if actual.Cmp(expected) != 0 {
		t.Fatalf("burnt contract balance incorrect: expected %d, got %d", expected, actual)
	}
}

func TestBurnContract(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	var (
		aa = common.HexToAddress("0x000000000000000000000000000000000000aaaa")

		// Generate a canonical chain to act as the main dataset
		db     = rawdb.NewMemoryDatabase()
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		key3, _ = crypto.HexToECDSA("225171aed3793cba1c029832886d69785b7e77a54a44211226b447aa2d16b058")

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)
		funds = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
		gspec = &core.Genesis{
			Config: params.BorUnittestChainConfig,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
				addr3: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)

	gspec.Config.BerlinBlock = common.Big0
	gspec.Config.LondonBlock = common.Big0
	gspec.Config.Bor.BurntContract = map[string]string{
		"0": "0x000000000000000000000000000000000000aaab",
		"1": "0x000000000000000000000000000000000000aaac",
		"2": "0x000000000000000000000000000000000000aaad",
		"3": "0x000000000000000000000000000000000000aaae",
	}
	genesis := gspec.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))
	signer := types.LatestSigner(gspec.Config)

	blocks, _ := core.GenerateChain(gspec.Config, genesis, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to 0xAAAA
		accesses := types.AccessList{types.AccessTuple{
			Address:     aa,
			StorageKeys: []common.Hash{{0}},
		}}

		txdata := &types.DynamicFeeTx{
			ChainID:    gspec.Config.ChainID,
			Nonce:      0,
			To:         &aa,
			Gas:        30000,
			GasFeeCap:  newGwei(5),
			GasTipCap:  big.NewInt(2),
			AccessList: accesses,
			Data:       []byte{},
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key1)

		b.AddTx(tx)
	})

	diskdb := rawdb.NewMemoryDatabase()
	gspec.MustCommit(diskdb, triedb.NewDatabase(diskdb, triedb.HashDefaults))

	chain, err := core.NewBlockChain(diskdb, gspec, engine, core.DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	// 1+2: Ensure EIP-1559 access lists are accounted for via gas usage.
	expectedGas := params.TxGas + params.TxAccessListAddressGas + params.TxAccessListStorageKeyGas +
		vm.GasQuickStep*2 + params.WarmStorageReadCostEIP2929 + params.ColdSloadCostEIP2929
	if block.GasUsed() != expectedGas {
		t.Fatalf("incorrect amount of gas spent: expected %d, got %d", expectedGas, block.GasUsed())
	}

	state, _ := chain.State()

	// 3: Ensure that miner received only the tx's tip.
	actual := state.GetBalance(block.Coinbase()).ToBig()
	expected := new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*block.Transactions()[0].GasTipCap().Uint64()),
		ethash.ConstantinopleBlockReward.ToBig(),
	)
	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// check burnt contract balance
	actual = state.GetBalance(common.HexToAddress(gspec.Config.Bor.CalculateBurntContract(block.NumberU64()))).ToBig()
	expected = new(big.Int).Mul(new(big.Int).SetUint64(block.GasUsed()), block.BaseFee())
	if actual.Cmp(expected) != 0 {
		t.Fatalf("burnt contract balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (tip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr1).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (block.Transactions()[0].GasTipCap().Uint64() + block.BaseFee().Uint64()))
	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}

	blocks, _ = core.GenerateChain(gspec.Config, block, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{2})

		txdata := &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key2)

		b.AddTx(tx)
	})

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block = chain.GetBlockByNumber(2)
	state, _ = chain.State()
	effectiveTip := block.Transactions()[0].GasTipCap().Uint64() - block.BaseFee().Uint64()

	// 6+5: Ensure that miner received only the tx's effective tip.
	actual = state.GetBalance(block.Coinbase()).ToBig()
	expected = new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*effectiveTip),
		ethash.ConstantinopleBlockReward.ToBig(),
	)
	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// check burnt contract balance
	actual = state.GetBalance(common.HexToAddress(gspec.Config.Bor.CalculateBurntContract(block.NumberU64()))).ToBig()
	expected = new(big.Int).Mul(new(big.Int).SetUint64(block.GasUsed()), block.BaseFee())
	if actual.Cmp(expected) != 0 {
		t.Fatalf("burnt contract balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (effectiveTip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr2).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (effectiveTip + block.BaseFee().Uint64()))
	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}

	blocks, _ = core.GenerateChain(gspec.Config, block, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{3})

		txdata := &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}
		tx := types.NewTx(txdata)
		tx, _ = types.SignTx(tx, signer, key3)

		b.AddTx(tx)
	})

	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block = chain.GetBlockByNumber(3)
	state, _ = chain.State()
	effectiveTip = block.Transactions()[0].GasTipCap().Uint64() - block.BaseFee().Uint64()

	// 6+5: Ensure that miner received only the tx's effective tip.
	actual = state.GetBalance(block.Coinbase()).ToBig()
	expected = new(big.Int).Add(
		new(big.Int).SetUint64(block.GasUsed()*effectiveTip),
		ethash.ConstantinopleBlockReward.ToBig(),
	)
	if actual.Cmp(expected) != 0 {
		t.Fatalf("miner balance incorrect: expected %d, got %d", expected, actual)
	}

	// check burnt contract balance
	actual = state.GetBalance(common.HexToAddress(gspec.Config.Bor.CalculateBurntContract(block.NumberU64()))).ToBig()
	expected = new(big.Int).Mul(new(big.Int).SetUint64(block.GasUsed()), block.BaseFee())
	if actual.Cmp(expected) != 0 {
		t.Fatalf("burnt contract balance incorrect: expected %d, got %d", expected, actual)
	}

	// 4: Ensure the tx sender paid for the gasUsed * (effectiveTip + block baseFee).
	actual = new(big.Int).Sub(funds, state.GetBalance(addr3).ToBig())
	expected = new(big.Int).SetUint64(block.GasUsed() * (effectiveTip + block.BaseFee().Uint64()))
	if actual.Cmp(expected) != 0 {
		t.Fatalf("sender balance incorrect: expected %d, got %d", expected, actual)
	}
}

func TestBurnContractContractFetch(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	config := params.BorUnittestChainConfig
	config.Bor.BurntContract = map[string]string{
		"10":  "0x000000000000000000000000000000000000aaab",
		"100": "0x000000000000000000000000000000000000aaad",
	}

	burnContractAddr10 := config.Bor.CalculateBurntContract(10)
	burnContractAddr11 := config.Bor.CalculateBurntContract(11)
	burnContractAddr99 := config.Bor.CalculateBurntContract(99)
	burnContractAddr100 := config.Bor.CalculateBurntContract(100)
	burnContractAddr101 := config.Bor.CalculateBurntContract(101)

	if burnContractAddr10 != "0x000000000000000000000000000000000000aaab" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaab", burnContractAddr10)
	}
	if burnContractAddr11 != "0x000000000000000000000000000000000000aaab" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaab", burnContractAddr11)
	}
	if burnContractAddr99 != "0x000000000000000000000000000000000000aaab" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaab", burnContractAddr99)
	}
	if burnContractAddr100 != "0x000000000000000000000000000000000000aaad" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaad", burnContractAddr100)
	}
	if burnContractAddr101 != "0x000000000000000000000000000000000000aaad" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaad", burnContractAddr101)
	}

	config.Bor.BurntContract = map[string]string{
		"10":   "0x000000000000000000000000000000000000aaab",
		"100":  "0x000000000000000000000000000000000000aaad",
		"1000": "0x000000000000000000000000000000000000aaae",
	}

	burnContractAddr10 = config.Bor.CalculateBurntContract(10)
	burnContractAddr11 = config.Bor.CalculateBurntContract(11)
	burnContractAddr99 = config.Bor.CalculateBurntContract(99)
	burnContractAddr100 = config.Bor.CalculateBurntContract(100)
	burnContractAddr101 = config.Bor.CalculateBurntContract(101)
	burnContractAddr999 := config.Bor.CalculateBurntContract(999)
	burnContractAddr1000 := config.Bor.CalculateBurntContract(1000)
	burnContractAddr1001 := config.Bor.CalculateBurntContract(1001)

	if burnContractAddr10 != "0x000000000000000000000000000000000000aaab" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaab", burnContractAddr10)
	}
	if burnContractAddr11 != "0x000000000000000000000000000000000000aaab" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaab", burnContractAddr11)
	}
	if burnContractAddr99 != "0x000000000000000000000000000000000000aaab" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaab", burnContractAddr99)
	}
	if burnContractAddr100 != "0x000000000000000000000000000000000000aaad" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaad", burnContractAddr100)
	}
	if burnContractAddr101 != "0x000000000000000000000000000000000000aaad" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaad", burnContractAddr101)
	}
	if burnContractAddr999 != "0x000000000000000000000000000000000000aaad" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaad", burnContractAddr999)
	}
	if burnContractAddr1000 != "0x000000000000000000000000000000000000aaae" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaae", burnContractAddr1000)
	}
	if burnContractAddr1001 != "0x000000000000000000000000000000000000aaae" {
		t.Fatalf("incorrect burnt contract address: expected %s, got %s", "0x000000000000000000000000000000000000aaae", burnContractAddr1001)
	}
}

// EIP1559 is not supported without EIP155. An error is expected
func TestEIP1559TransitionWithEIP155(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	var (
		aa = common.HexToAddress("0x000000000000000000000000000000000000aaaa")

		// Generate a canonical chain to act as the main dataset
		db     = rawdb.NewMemoryDatabase()
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		key3, _ = crypto.HexToECDSA("225171aed3793cba1c029832886d69785b7e77a54a44211226b447aa2d16b058")

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)
		funds = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
		gspec = &core.Genesis{
			Config: params.BorUnittestChainConfig,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
				addr3: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)

	genesis := gspec.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	// Use signer without chain ID
	signer := types.HomesteadSigner{}

	_, _ = core.GenerateChain(gspec.Config, genesis, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})
		// One transaction to 0xAAAA
		accesses := types.AccessList{types.AccessTuple{
			Address:     aa,
			StorageKeys: []common.Hash{{0}},
		}}

		txdata := &types.DynamicFeeTx{
			ChainID:    gspec.Config.ChainID,
			Nonce:      0,
			To:         &aa,
			Gas:        30000,
			GasFeeCap:  newGwei(5),
			GasTipCap:  big.NewInt(2),
			AccessList: accesses,
			Data:       []byte{},
		}

		var err error

		tx := types.NewTx(txdata)
		tx, err = types.SignTx(tx, signer, key1)

		require.ErrorIs(t, err, types.ErrTxTypeNotSupported)
	})
}

// it is up to a user to use protected transactions. so if a transaction is unprotected no errors related to chainID are expected.
// transactions are checked in 2 places: transaction pool and blockchain processor.
func TestTransitionWithoutEIP155(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	var (
		aa = common.HexToAddress("0x000000000000000000000000000000000000aaaa")

		// Generate a canonical chain to act as the main dataset
		db     = rawdb.NewMemoryDatabase()
		engine = ethash.NewFaker()

		// A sender who makes transactions, has some funds
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		key2, _ = crypto.HexToECDSA("8a1f9a8f95be41cd7ccb6168179afb4504aefe388d1e14474d32c45c72ce7b7a")
		key3, _ = crypto.HexToECDSA("225171aed3793cba1c029832886d69785b7e77a54a44211226b447aa2d16b058")

		addr1 = crypto.PubkeyToAddress(key1.PublicKey)
		addr2 = crypto.PubkeyToAddress(key2.PublicKey)
		addr3 = crypto.PubkeyToAddress(key3.PublicKey)
		funds = new(big.Int).Mul(common.Big1, big.NewInt(params.Ether))
		gspec = &core.Genesis{
			Config: params.BorUnittestChainConfig,
			Alloc: types.GenesisAlloc{
				addr1: {Balance: funds},
				addr2: {Balance: funds},
				addr3: {Balance: funds},
				// The address 0xAAAA sloads 0x00 and 0x01
				aa: {
					Code: []byte{
						byte(vm.PC),
						byte(vm.PC),
						byte(vm.SLOAD),
						byte(vm.SLOAD),
					},
					Nonce:   0,
					Balance: big.NewInt(0),
				},
			},
		}
	)

	genesis := gspec.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	// Use signer without chain ID
	signer := types.HomesteadSigner{}
	//signer := types.FrontierSigner{}

	blocks, _ := core.GenerateChain(gspec.Config, genesis, engine, db, 1, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.Address{1})

		txdata := &types.LegacyTx{
			Nonce:    0,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}

		var err error

		tx := types.NewTx(txdata)
		tx, err = types.SignTx(tx, signer, key1)

		require.Nil(t, err)
		require.False(t, tx.Protected())

		from, err := types.Sender(types.EIP155Signer{}, tx)
		require.Equal(t, addr1, from)
		require.Nil(t, err)

		b.AddTx(tx)
	})

	diskdb := rawdb.NewMemoryDatabase()
	gspec.MustCommit(diskdb, triedb.NewDatabase(diskdb, triedb.HashDefaults))

	chain, err := core.NewBlockChain(diskdb, gspec, engine, core.DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}
	if n, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("block %d: failed to insert into chain: %v", n, err)
	}

	block := chain.GetBlockByNumber(1)

	require.Len(t, block.Transactions(), 1)
}

func TestJaipurFork(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))

	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	block := init.genesis.ToBlock()

	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	res := loadSpanFromFile(t)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)
	_bor.SetHeimdallClient(h)

	spanner := getMockedSpanner(t, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(res.ValidatorSet.Validators))
	_bor.SetSpanner(spanner)

	currentValidators := span0.ValidatorSet.Validators
	for i := uint64(1); i < sprintSize; i++ {
		// Update the validator set before sprint end (so that it is returned when called for next block)
		// E.g. In this case, update on block 3 as snapshot of block 3 will be called for block 4's verification
		if i == sprintSize-1 {
			currentValidators = res.ValidatorSet.Validators

			// Update the span0's validator set to new validator set. This will be used in verify header when we query
			// span to compare validator's set with header's extradata. Even though our span store has old validator set
			// stored in cache, we're updating the underlying pointer here and hence we don't need to update the cache.
			span0.ValidatorSet.Validators = currentValidators
		}
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, nil, nil)
		insertNewBlock(t, chain, block)

		if block.Number().Uint64() == init.genesis.Config.Bor.JaipurBlock.Uint64()-1 {
			require.Equal(t, testSealHash(block.Header(), init.genesis.Config.Bor), bor.SealHash(block.Header(), init.genesis.Config.Bor))
		}

		if block.Number().Uint64() == init.genesis.Config.Bor.JaipurBlock.Uint64() {
			require.Equal(t, testSealHash(block.Header(), init.genesis.Config.Bor), bor.SealHash(block.Header(), init.genesis.Config.Bor))
		}
	}
}

// SealHash returns the hash of a block prior to it being sealed.
func testSealHash(header *types.Header, c *params.BorConfig) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()
	testEncodeSigHeader(hasher, header, c)
	hasher.Sum(hash[:0])
	return hash
}

func testEncodeSigHeader(w io.Writer, header *types.Header, c *params.BorConfig) {
	enc := []any{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-65], // Yes, this will panic if extra is too short.
		header.MixDigest,
		header.Nonce,
	}
	if c.IsJaipur(header.Number) {
		if header.BaseFee != nil {
			enc = append(enc, header.BaseFee)
		}
	}
	if err := rlp.Encode(w, enc); err != nil {
		panic("can't encode: " + err.Error())
	}
}

// TestEarlyBlockAnnouncementPostBhilai_Primary tests for different cases of early block announcement
// acting as a primary block producer. It ensures that consensus handles the header time and
// block announcement time correctly.
func TestEarlyBlockAnnouncementPostBhilai_Primary(t *testing.T) {
	t.Skip("Skipping PIP-66 unitl it is enabled back")
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Setup forks from genesis block with 2s block time for simplicity
	updateGenesis := func(gen *core.Genesis) {
		gen.Timestamp = uint64(time.Now().Unix())
		gen.Config.Bor.Period = map[string]uint64{"0": 2}
		gen.Config.Bor.Sprint = map[string]uint64{"0": 16}
		gen.Config.LondonBlock = common.Big0
		gen.Config.ShanghaiBlock = common.Big0
		gen.Config.CancunBlock = common.Big0
		gen.Config.PragueBlock = common.Big0
		gen.Config.Bor.JaipurBlock = common.Big0
		gen.Config.Bor.DelhiBlock = common.Big0
		gen.Config.Bor.IndoreBlock = common.Big0
		gen.Config.Bor.BhilaiBlock = common.Big0
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)

	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	res := loadSpanFromFile(t)

	// Create mock heimdall client
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)
	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()
	_bor.SetHeimdallClient(h)

	block := init.genesis.ToBlock()
	currentValidators := span0.ValidatorSet.Validators

	spanner := getMockedSpanner(t, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators))
	_bor.SetSpanner(spanner)

	// Pre-define succession as 0 as all the tests are for primary
	succession := 0
	getSuccession := func() int {
		return succession
	}
	updateTime := func(header *types.Header) {
		// This logic matches with consensus.Prepare function. It's done explicitly here
		// because other tests aren't designed to use current time and hence might break.
		if header.Time < uint64(time.Now().Unix()) {
			header.Time = uint64(time.Now().Unix())
		} else {
			if chain.Config().Bor.IsBhilai(header.Number) && getSuccession() == 0 {
				period := chain.Config().Bor.CalculatePeriod(header.Number.Uint64())
				startTime := time.Unix(int64(header.Time-period), 0)
				time.Sleep(time.Until(startTime))
			}
		}
	}

	// Build block 1 normally
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime}, nil)
	i, err := chain.InsertChain([]*types.Block{block}, false)
	// Block verified and imported successfully
	require.NoError(t, err, "error inserting block #1")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #1")

	// Case 1: Block announced before header time should be accepted
	// Block 2
	// The previous was built early but `updateTime` function will ensure block building
	// doesn't start before the block's 2s time window.
	waitingTime := time.Until(time.Unix(int64(block.Time()), 0))
	// Capture the expected header time based on the logic used in bor consensus
	headerTime := block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor)
	// Define a max possible delay which is time until header time + waiting time defined above
	maxDelay := time.Until(time.Unix(int64(headerTime), 0)) + waitingTime
	// Track time taken to build, and seal (basically announce) the block
	start := time.Now()
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime}, nil)
	blockAnnouncementTime := time.Since(start)
	// The building + sealing time should be less than the expected pre-bhilai block building time (~2s)
	require.LessOrEqual(t, blockAnnouncementTime, maxDelay, fmt.Sprintf("block announcement happened after header time"))
	// The building + sealing time should be slightly greater than the waiting time
	require.Greater(t, blockAnnouncementTime, waitingTime, fmt.Sprintf("block announcement time is less than waiting time"))
	// Block verified and imported successfully
	i, err = chain.InsertChain([]*types.Block{block}, false)
	require.NoError(t, err, "error inserting block #2")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #2")

	// Case 2: Delayed block (after header time) should be accepted
	// Block 3
	// Wait until header.Time + 1s before building the block
	headerTime = block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor)
	time.Sleep(time.Until(time.Unix(int64(headerTime)+1, 0)))
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime}, nil)
	require.Greater(t, block.Time(), headerTime, "block time should be greated than expected header time")
	// Block verified and imported successfully
	i, err = chain.InsertChain([]*types.Block{block}, false)
	require.NoError(t, err, "error inserting block #3")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #3")

	// Build block 4 normally
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime}, nil)
	i, err = chain.InsertChain([]*types.Block{block}, false)
	// Block verified and imported successfully
	require.NoError(t, err, "error inserting block #4")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #4")

	// Case 3: Block announced before it's expected time (header.Time - 2s) should be rejected
	// Block 5
	// Use signer to sign block instead of using `bor.Seal` call. This is done to immediately
	// build the next block instead of waiting for the delay (using bor.Seal will not lead
	// to block being rejected).
	updateTimeWithoutSleep := func(header *types.Header) {
		// This logic matches with consensus.Prepare function. It's done explicitly here
		// because other tests aren't designed to use current time and hence might break.
		if header.Time < uint64(time.Now().Unix()) {
			header.Time = uint64(time.Now().Unix())
		}
	}
	signer, err := hex.DecodeString(privKey)
	tempBlock := buildNextBlock(t, _bor, chain, block, signer, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), true, []modifyHeaderFunc{updateTimeWithoutSleep}, nil)
	i, err = chain.InsertChain([]*types.Block{tempBlock}, false)
	// No error is expected here because block will be added to future chain and is
	// technically valid (according to insert chain function)
	require.NoError(t, err, "error inserting block #5")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #5")
	// Block is invalid according to consensus rules and should return appropriate error
	err = engine.VerifyHeader(chain, tempBlock.Header())
	require.ErrorIs(t, err, consensus.ErrFutureBlock, "incorrect error while verifying block #5")

	// Build block 5 again normally
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime}, nil)
	i, err = chain.InsertChain([]*types.Block{block}, false)
	// Block verified and imported successfully
	require.NoError(t, err, "error inserting block #5")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #5")

	// Case 4: Block with tweaked header time ahead of expected time should be rejected
	// Block 6
	// Set the header time to be 1s earlier than the expected header time
	setTime := func(header *types.Header) {
		header.Time = block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor) - 1
	}
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{setTime}, nil)
	// Consensus verification will fail and this error will float up unlike future block error
	// as we've tweaked the header time which is not allowed.
	i, err = chain.InsertChain([]*types.Block{block}, false)
	require.Equal(t, bor.ErrInvalidTimestamp, err, "incorrect error while inserting block #5")
	require.Equal(t, 0, i, "incorrect number of blocks inserted while inserting block #5")
}

// TestEarlyBlockAnnouncementPostBhilai_NonPrimary tests for different cases of early block announcement
// acting as a non-primary block producer. It ensures that consensus handles the header time and
// block announcement time correctly.
func TestEarlyBlockAnnouncementPostBhilai_NonPrimary(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Setup forks from genesis block with 2s block time for simplicity
	updateGenesis := func(gen *core.Genesis) {
		gen.Timestamp = uint64(time.Now().Unix())
		gen.Config.Bor.Period = map[string]uint64{"0": 2}
		gen.Config.Bor.Sprint = map[string]uint64{"0": 16}
		gen.Config.Bor.ProducerDelay = map[string]uint64{"0": 4}
		gen.Config.Bor.BackupMultiplier = map[string]uint64{"0": 2}
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": 128}
		gen.Config.LondonBlock = common.Big0
		gen.Config.ShanghaiBlock = common.Big0
		gen.Config.CancunBlock = common.Big0
		gen.Config.PragueBlock = common.Big0
		gen.Config.Bor.JaipurBlock = common.Big0
		gen.Config.Bor.DelhiBlock = common.Big0
		gen.Config.Bor.IndoreBlock = common.Big0
		gen.Config.Bor.BhilaiBlock = common.Big0
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)

	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	// Use 3 validators from the start to allow out-of-turn block production
	res1 := loadSpanFromFile(t)
	res1.StartBlock = 0
	res1.EndBlock = 255
	res2 := loadSpanFromFile(t)

	// key2 and addr2 belong to the primary validator, authorize consensus to sign messages
	engine.(*bor.Bor).Authorize(addr2, func(account accounts.Account, s string, data []byte) ([]byte, error) {
		return crypto.Sign(crypto.Keccak256(data), key2)
	})

	// Create mock heimdall client
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, res1, res2)
	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).
		Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()
	_bor.SetHeimdallClient(h)

	block := init.genesis.ToBlock()
	currentValidators := res1.ValidatorSet.Validators

	spanner := getMockedSpanner(t, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators))
	_bor.SetSpanner(spanner)

	succession := 0
	getSuccession := func() int {
		return succession
	}
	updateTime := func(header *types.Header) {
		// This logic matches with consensus.Prepare function. It's done explicitly here
		// because other tests aren't designed to use current time and hence might break.
		if header.Time < uint64(time.Now().Unix()) {
			header.Time = uint64(time.Now().Unix())
		} else {
			if chain.Config().Bor.IsBhilai(header.Number) && getSuccession() == 0 {
				period := chain.Config().Bor.CalculatePeriod(header.Number.Uint64())
				startTime := time.Unix(int64(header.Time-period), 0)
				time.Sleep(time.Until(startTime))
			}
		}
	}

	// Build block 1 normally with the primary validator
	updateDiff := func(header *types.Header) {
		// We need to explicitly set it otherwise it derives value from
		// parent block (which is genesis) which we don't want.
		header.Difficulty = new(big.Int).SetUint64(3)
	}
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime, updateDiff}, nil)
	i, err := chain.InsertChain([]*types.Block{block}, false)
	require.NoError(t, err, "error inserting block #1")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #1")

	// Going ahead, all blocks will be built by the tertiary (backup) validator. Authorize consensus
	// to sign messages on behalf of it's private keys
	engine.(*bor.Bor).Authorize(addr3, func(account accounts.Account, s string, data []byte) ([]byte, error) {
		sig, err := crypto.Sign(crypto.Keccak256(data), key3)
		return sig, err
	})

	// All blocks from this point will be built by the tertiary validator. Set the succession to 2
	succession = 2

	// Case 1: Build a block from tertiary validator with header.Time set before block 1's time
	// As the time in header is invalid, the block should be rejected due to invalid timestamp.
	// Use signer to sign block instead of using `bor.Seal` call. This is done to immediately
	// build the next block instead of waiting for the delay.
	// Block 2
	signer, _ := hex.DecodeString(privKey3)
	updateHeader := func(header *types.Header) {
		header.Difficulty = new(big.Int).SetUint64(1)
		header.Time = block.Time() - 1
	}
	tempBlock := buildNextBlock(t, _bor, chain, block, signer, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), true, []modifyHeaderFunc{updateTime, updateHeader}, nil)
	i, err = chain.InsertChain([]*types.Block{tempBlock}, false)
	require.Equal(t, bor.ErrInvalidTimestamp, err, "incorrect error while inserting block #2")
	require.Equal(t, 0, i, "incorrect number of blocks inserted while inserting block #2")

	// Case 2: Build a block from tertiary validator with header.Time set correctly (previous + 6s).
	// Announce the block early before the previous block's announcement window is over. This should
	// lead to future block error from consensus.
	// Block 2 again, build with correct time but announce early
	updateHeader = func(header *types.Header) {
		header.Difficulty = new(big.Int).SetUint64(1)
		// Succession is 2 because of tertiary validator
		header.Time = block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor)
	}
	tempBlock = buildNextBlock(t, _bor, chain, block, signer, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), true, []modifyHeaderFunc{updateTime, updateHeader}, nil)
	// Block is invalid according to consensus rules and should return appropriate error
	// Insert chain would accept the block as future block so we don't attempt calling it.
	err = engine.VerifyHeader(chain, tempBlock.Header())
	require.ErrorIs(t, err, consensus.ErrFutureBlock, "incorrect error while verifying block #2")

	// Case 3: Happy case. Build a correct block and ensure the sealing function waits until expected
	// header time before announcing the block. Non-primary validators can't announce blocks early.
	var expectedBlockBuildingTime time.Duration
	updateHeader = func(header *types.Header) {
		header.Difficulty = new(big.Int).SetUint64(1)
		header.Time = block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor)
		// Capture the expected header time based on the logic used in bor consensus
		expectedBlockBuildingTime = time.Until(time.Unix(int64(header.Time), 0))
	}
	// Capture the time taken in block building (mainly sealing due to delay)
	start := time.Now()
	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), false, []modifyHeaderFunc{updateTime, updateHeader}, nil)
	blockAnnouncementTime := time.Since(start)
	// The building + sealing time should be greater than ideal time (6s for tertiary validator)
	// as early block announcement is not allowed for non-primary validators.
	require.GreaterOrEqual(t, blockAnnouncementTime, expectedBlockBuildingTime, fmt.Sprintf("block #2 announcement happened before header time for non-primary validator"))
	i, err = chain.InsertChain([]*types.Block{block}, false)
	require.NoError(t, err, "error inserting block #2")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #2")

	// Case 4: Build a block from tertiary validator with correct header time but try to announce it
	// before it's expected time (i.e. 6s here). Early announcements for non-primary validators
	// should be rejected with a future block error from consensus.
	// Block 3 (tertiary)
	updateHeader = func(header *types.Header) {
		header.Difficulty = new(big.Int).SetUint64(1)
		header.Time = block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor)
	}
	block = buildNextBlock(t, _bor, chain, block, signer, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), true, []modifyHeaderFunc{updateTime, updateHeader}, nil)

	// reject if announced early (here: parent block time + 2s)
	time.Sleep(2 * time.Second)
	err = engine.VerifyHeader(chain, block.Header())
	require.ErrorIs(t, err, consensus.ErrFutureBlock, "incorrect error while verifying block #3")

	// reject if announced early (here: parent block time + 4s)
	time.Sleep(2 * time.Second)
	err = engine.VerifyHeader(chain, block.Header())
	require.ErrorIs(t, err, consensus.ErrFutureBlock, "incorrect error while verifying block #3")

	// accept if announced after expected header.Time (here: parent block time + 6s)
	time.Sleep(2 * time.Second)
	err = engine.VerifyHeader(chain, block.Header())
	require.NoError(t, err, "error verifying block #3")

	i, err = chain.InsertChain([]*types.Block{block}, false)
	require.NoError(t, err, "error inserting block #3")
	require.Equal(t, 1, i, "incorrect number of blocks inserted while inserting block #3")

	// Case 5: Build a block from tertiary validator with an incorrect header time (1s before parent block) and
	// announce it on time. This case is different than case 1 because header time is tweaked by only 1s compared
	// to 7s in that case. Consensus should reject this block with a too soon error (instead of invalid timestamp
	// in case 1).
	updateHeader = func(header *types.Header) {
		header.Difficulty = new(big.Int).SetUint64(1)
		header.Time = block.Time() + bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor) - 1
	}
	// Capture time to wait until the expected header time before announcing the block
	timeToWait := time.Until(time.Unix(int64(block.Time()+bor.CalcProducerDelay(block.NumberU64(), getSuccession(), init.genesis.Config.Bor)), 0))
	block = buildNextBlock(t, _bor, chain, block, signer, init.genesis.Config.Bor, nil, borSpan.ConvertHeimdallValidatorsToBorValidatorsByRef(currentValidators), true, []modifyHeaderFunc{updateTime, updateHeader}, nil)

	// Wait for expected time + some buffer
	time.Sleep(timeToWait)
	time.Sleep(100 * time.Millisecond)

	err = engine.VerifyHeader(chain, block.Header())
	require.Equal(t,
		bor.BlockTooSoonError{Number: 4, Succession: 2},
		*err.(*bor.BlockTooSoonError))
}

// TestCustomBlockTimeMining tests that a miner can successfully create blocks with a custom block time
// different from the consensus period. It sets consensus period to 1s and custom miner block time to 1.75s,
// then verifies that approximately 34 blocks (60s / 1.75s) are mined in 1 minute.
func TestCustomBlockTimeMining(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 1}        // Consensus period: 1s
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}       // Sprint size: 16 blocks
	genesis.Config.Bor.ProducerDelay = map[string]uint64{"0": 0} // No producer delay
	genesis.Config.Bor.BackupMultiplier = map[string]uint64{"0": 2}

	genesis.Config.Bor.RioBlock = common.Big0

	customBlockTime := 1750 * time.Millisecond

	stack, ethBackend, err := InitMinerWithBlockTime(genesis, keys[0], true, customBlockTime)
	require.NoError(t, err)
	defer stack.Close()

	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	borEngine := ethBackend.Engine().(*bor.Bor)
	borEngine.Authorize(crypto.PubkeyToAddress(keys[0].PublicKey), func(account accounts.Account, s string, data []byte) ([]byte, error) {
		return crypto.Sign(crypto.Keccak256(data), keys[0])
	})

	startBlock := ethBackend.BlockChain().CurrentBlock().Number.Uint64()
	startTime := time.Now()

	err = ethBackend.StartMining()
	require.NoError(t, err)

	testDuration := 60 * time.Second
	time.Sleep(testDuration)

	ethBackend.StopMining()

	endBlock := ethBackend.BlockChain().CurrentBlock().Number.Uint64()
	actualDuration := time.Since(startTime)

	blocksMinedCount := endBlock - startBlock

	expectedBlocks := float64(actualDuration.Seconds()) / customBlockTime.Seconds()

	// Allow 5% tolerance for timing variations
	tolerance := 0.05
	minExpectedBlocks := uint64(expectedBlocks * (1 - tolerance))
	maxExpectedBlocks := uint64(expectedBlocks * (1 + tolerance))

	log.Info("Custom block time mining test results",
		"startBlock", startBlock,
		"endBlock", endBlock,
		"blocksMinedCount", blocksMinedCount,
		"duration", actualDuration,
		"customBlockTime", customBlockTime,
		"expectedBlocks", expectedBlocks,
		"minExpected", minExpectedBlocks,
		"maxExpected", maxExpectedBlocks)

	require.GreaterOrEqual(t, blocksMinedCount, minExpectedBlocks,
		fmt.Sprintf("Too few blocks mined. Expected at least %d, got %d", minExpectedBlocks, blocksMinedCount))
	require.LessOrEqual(t, blocksMinedCount, maxExpectedBlocks,
		fmt.Sprintf("Too many blocks mined. Expected at most %d, got %d", maxExpectedBlocks, blocksMinedCount))
}

// TestInvalidStateSyncInBlockBody tests that a block containing invalid state sync event data
// in form of a state-sync tx in block body will be rejected by consensus.
func TestInvalidStateSyncInBlockBody(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	updateGenesis := func(gen *core.Genesis) {
		gen.Config.Bor.Sprint = map[string]uint64{"0": sprintSize}
		gen.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": 128}
		gen.Config.Bor.MadhugiriBlock = big.NewInt(0) // Enable Madhugiri hardfork from genesis.
	}
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), updateGenesis)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	// Insert blocks for 0th sprint
	block := init.genesis.ToBlock()

	// Create a mock span 0
	span0 := createMockSpan(addr, chain.Config().ChainID.String())
	borValSet := borSpan.ConvertHeimdallValSetToBorValSet(span0.ValidatorSet)
	currentValidators := borValSet.Validators

	// Load mock span 0
	res := loadSpanFromFile(t)

	// Create mock bor spanner
	spanner := getMockedSpanner(t, currentValidators)
	_bor.SetSpanner(spanner)

	// Create mock heimdall client
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := createMockHeimdall(ctrl, &span0, res)

	// Mock state sync events
	eventCount := 1
	sample := getSampleEventRecord(t)
	eventRecords := generateFakeStateSyncEvents(sample, eventCount)

	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).Return(eventRecords, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(nil, fmt.Errorf("span not found")).AnyTimes()
	_bor.SetHeimdallClient(h)

	// Insert sprintSize # of blocks so that span is fetched at the start of a new sprint
	for i := uint64(1); i < sprintSize; i++ {
		if IsSpanEnd(i) {
			currentValidators = borValSet.Validators
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, currentValidators, false, nil, nil)
		insertNewBlock(t, chain, block)
	}

	// Create a malicious block with arbitrary state-sync tx which is different from what's actually applied
	// on state.
	createMaliciousBlock := func(block *types.Block, receipts []*types.Receipt) *types.Block {
		maliciousBody := &types.Body{
			Transactions: []*types.Transaction{types.NewTx(&types.StateSyncTx{
				StateSyncData: []*types.StateSyncData{{
					ID:       1,
					Contract: common.HexToAddress("0x0000000000000000000000000000000000001000"),
					Data:     []byte{0x01, 0x02, 0x03},
					TxHash:   common.HexToHash("0xabcdef"),
				}},
			})},
		}
		return types.NewBlock(block.Header(), maliciousBody, receipts, trie.NewStackTrie(nil))
	}

	block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, borValSet.Validators, false, nil, []modifyBlockFunc{createMaliciousBlock})
	txs := block.Transactions()
	require.Equal(t, 1, len(txs), "state-sync tx should be part of block body")
	require.Equal(t, uint8(types.StateSyncTxType), txs[0].Type(), "transaction should be of state-sync type")

	// Try inserting the malicious block. Due to mismatch in tx data and data from heimdall, receipt
	// shouldn't be applied and an error should be returned while inserting the block.
	_, err := chain.InsertChain([]*types.Block{block}, false)
	require.Error(t, err, "insert chain successed for block with invalid state-sync tx in body")
	require.ErrorIs(t, err, core.ErrStateSyncMismatch, "received incorrect error for invalid state-sync tx in block body")
}

// TestDynamicGasLimit_LowBaseFee tests that when base fee is below the target-buffer,
// the gas limit decreases toward the minimum.
func TestDynamicGasLimit_LowBaseFee(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Generate a batch of accounts to seal and fund with
	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	// Initialize genesis with a gas limit
	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)

	// Note: When transitioning from pre-London to London fork, gas limit is multiplied
	// by ElasticityMultiplier (2x). So post-London gas limit starts at ~2x genesis.
	// We set min/max relative to this expected post-London gas limit.
	postLondonGasLimit := genesis.GasLimit * 2 // After elasticity multiplier

	// Configure dynamic gas limit with a high target base fee
	// Since initial base fee is typically 1 gwei (params.InitialBaseFee = 1000000000),
	// setting target to 100 gwei means base fee will be below target-buffer,
	// so gas limit should decrease toward min.
	dynamicConfig := DynamicGasLimitConfig{
		EnableDynamicGasLimit: true,
		GasLimitMin:           postLondonGasLimit / 2, // Min = half of post-London
		GasLimitMax:           postLondonGasLimit * 2, // Max = double post-London
		TargetBaseFee:         100_000_000_000,        // 100 gwei (high target)
		BaseFeeBuffer:         10_000_000_000,         // 10 gwei buffer
	}

	// Start the miner with dynamic gas limit enabled
	stack, ethBackend, err := InitMinerWithDynamicGasLimit(genesis, keys[0], true, dynamicConfig)
	require.NoError(t, err)
	defer stack.Close()

	// Wait for the node to be ready
	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start mining
	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for several blocks to be mined
	targetBlockNum := uint64(30)
	timeout := time.After(90 * time.Second)

	for {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for blocks to be mined")
		default:
			currentBlock := ethBackend.BlockChain().CurrentHeader()
			if currentBlock.Number.Uint64() >= targetBlockNum {
				goto checkResults
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

checkResults:
	// Get the gas limits from mined blocks
	chain := ethBackend.BlockChain()

	// Get first London block's gas limit as baseline (after elasticity adjustment)
	firstLondonHeader := chain.GetHeaderByNumber(1)
	require.NotNil(t, firstLondonHeader)
	firstLondonGasLimit := firstLondonHeader.GasLimit

	t.Logf("Genesis gas limit: %d", genesis.GasLimit)
	t.Logf("First London block gas limit: %d", firstLondonGasLimit)
	t.Logf("Dynamic config - Min: %d, Max: %d, TargetBaseFee: %d, Buffer: %d",
		dynamicConfig.GasLimitMin, dynamicConfig.GasLimitMax,
		dynamicConfig.TargetBaseFee, dynamicConfig.BaseFeeBuffer)

	// Track gas limit changes - after initial blocks, should be decreasing
	var gasLimitDecreasing bool
	var lastGasLimit uint64

	for i := uint64(1); i <= targetBlockNum; i++ {
		header := chain.GetHeaderByNumber(i)
		if header == nil {
			continue
		}

		if i == 1 {
			lastGasLimit = header.GasLimit
		} else if header.GasLimit < lastGasLimit {
			gasLimitDecreasing = true
		}

		t.Logf("Block %d: GasLimit=%d, BaseFee=%s",
			i, header.GasLimit, header.BaseFee.String())

		lastGasLimit = header.GasLimit
	}

	// Verify that gas limit has been decreasing (since base fee is below target-buffer)
	// The base fee starts at InitialBaseFee (1 gwei) which is below target-buffer (90 gwei)
	assert.True(t, gasLimitDecreasing, "Gas limit should be decreasing when base fee is below target-buffer")

	// Verify the final gas limit is less than the first London block's gas limit
	finalHeader := chain.GetHeaderByNumber(targetBlockNum)
	assert.Less(t, finalHeader.GasLimit, firstLondonGasLimit,
		"Final gas limit should be less than first London block gas limit when decreasing")
}

// TestDynamicGasLimit_HighBaseFee tests that when base fee is above the target+buffer,
// the gas limit increases toward the maximum.
func TestDynamicGasLimit_HighBaseFee(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Generate a batch of accounts to seal and fund with
	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	// Initialize genesis
	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)

	// Note: When transitioning from pre-London to London fork, gas limit is multiplied
	// by ElasticityMultiplier (2x). So post-London gas limit starts at ~2x genesis.
	postLondonGasLimit := genesis.GasLimit * 2 // After elasticity multiplier

	// Configure dynamic gas limit with a very low target base fee
	// This ensures the base fee (even at initial 1 gwei) will be above target+buffer,
	// so gas limit should increase toward max.
	dynamicConfig := DynamicGasLimitConfig{
		EnableDynamicGasLimit: true,
		GasLimitMin:           postLondonGasLimit / 2, // Min = half of post-London
		GasLimitMax:           postLondonGasLimit * 2, // Max = double post-London
		TargetBaseFee:         100_000_000,            // 0.1 gwei (very low target)
		BaseFeeBuffer:         50_000_000,             // 0.05 gwei buffer
	}

	// Start the miner with dynamic gas limit enabled
	stack, ethBackend, err := InitMinerWithDynamicGasLimit(genesis, keys[0], true, dynamicConfig)
	require.NoError(t, err)
	defer stack.Close()

	// Wait for the node to be ready
	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start mining
	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for several blocks to be mined
	targetBlockNum := uint64(30)
	timeout := time.After(90 * time.Second)

	for {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for blocks to be mined")
		default:
			currentBlock := ethBackend.BlockChain().CurrentHeader()
			if currentBlock.Number.Uint64() >= targetBlockNum {
				goto checkResults
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

checkResults:
	// Get the gas limits from mined blocks
	chain := ethBackend.BlockChain()

	// Get first London block's gas limit as baseline (after elasticity adjustment)
	firstLondonHeader := chain.GetHeaderByNumber(1)
	require.NotNil(t, firstLondonHeader)
	firstLondonGasLimit := firstLondonHeader.GasLimit

	t.Logf("Genesis gas limit: %d", genesis.GasLimit)
	t.Logf("First London block gas limit: %d", firstLondonGasLimit)
	t.Logf("Dynamic config - Min: %d, Max: %d, TargetBaseFee: %d, Buffer: %d",
		dynamicConfig.GasLimitMin, dynamicConfig.GasLimitMax,
		dynamicConfig.TargetBaseFee, dynamicConfig.BaseFeeBuffer)

	// Track gas limit changes
	var gasLimitIncreasing bool
	var lastGasLimit uint64

	for i := uint64(1); i <= targetBlockNum; i++ {
		header := chain.GetHeaderByNumber(i)
		if header == nil {
			continue
		}

		if i == 1 {
			lastGasLimit = header.GasLimit
		} else if header.GasLimit > lastGasLimit {
			gasLimitIncreasing = true
		}

		t.Logf("Block %d: GasLimit=%d, BaseFee=%s",
			i, header.GasLimit, header.BaseFee.String())

		lastGasLimit = header.GasLimit
	}

	// Verify that gas limit has been increasing (since base fee is above target+buffer)
	// The base fee starts at InitialBaseFee (1 gwei = 1000000000) which is above target+buffer (0.15 gwei)
	assert.True(t, gasLimitIncreasing, "Gas limit should be increasing when base fee is above target+buffer")

	// Verify the final gas limit is greater than first London block
	finalHeader := chain.GetHeaderByNumber(targetBlockNum)
	assert.Greater(t, finalHeader.GasLimit, firstLondonGasLimit,
		"Final gas limit should be greater than first London gas limit when increasing")
}

// TestDynamicGasLimit_WithinBuffer tests that when base fee is within the buffer range,
// the gas limit remains stable (follows the parent's gas limit).
func TestDynamicGasLimit_WithinBuffer(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Generate a batch of accounts to seal and fund with
	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	// Initialize genesis
	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)

	// Note: London fork activates at block 0, so the first London block (block 1)
	// will have gas limit = genesis.GasLimit * elasticity_multiplier (2x)
	postLondonGasLimit := genesis.GasLimit * 2

	// Configure dynamic gas limit with target matching initial base fee
	// InitialBaseFee is 1 gwei (1000000000 wei)
	// Set target to 1 gwei with a large buffer so base fee stays within range
	dynamicConfig := DynamicGasLimitConfig{
		EnableDynamicGasLimit: true,
		GasLimitMin:           postLondonGasLimit / 2, // Min = half of post-London limit
		GasLimitMax:           postLondonGasLimit * 2, // Max = double post-London limit
		TargetBaseFee:         1_000_000_000,          // 1 gwei (matches InitialBaseFee)
		BaseFeeBuffer:         500_000_000,            // 0.5 gwei buffer (so range is 0.5-1.5 gwei)
	}

	// Start the miner with dynamic gas limit enabled
	stack, ethBackend, err := InitMinerWithDynamicGasLimit(genesis, keys[0], true, dynamicConfig)
	require.NoError(t, err)
	defer stack.Close()

	// Wait for the node to be ready
	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start mining
	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for several blocks to be mined
	targetBlockNum := uint64(15)
	timeout := time.After(60 * time.Second)

	for {
		select {
		case <-timeout:
			t.Fatal("Timeout waiting for blocks to be mined")
		default:
			currentBlock := ethBackend.BlockChain().CurrentHeader()
			if currentBlock.Number.Uint64() >= targetBlockNum {
				goto checkResults
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

checkResults:
	// Get the gas limits from mined blocks
	chain := ethBackend.BlockChain()

	// Get first London block's gas limit as baseline (after elasticity adjustment)
	firstLondonHeader := chain.GetHeaderByNumber(1)
	require.NotNil(t, firstLondonHeader)
	firstLondonGasLimit := firstLondonHeader.GasLimit

	t.Logf("Genesis gas limit: %d", genesis.GasLimit)
	t.Logf("First London block gas limit: %d", firstLondonGasLimit)
	t.Logf("Dynamic config - Min: %d, Max: %d, TargetBaseFee: %d, Buffer: %d",
		dynamicConfig.GasLimitMin, dynamicConfig.GasLimitMax,
		dynamicConfig.TargetBaseFee, dynamicConfig.BaseFeeBuffer)

	// Track gas limit changes - should remain relatively stable
	var maxDeviation uint64

	for i := uint64(1); i <= targetBlockNum; i++ {
		header := chain.GetHeaderByNumber(i)
		if header == nil {
			continue
		}

		var deviation uint64
		if header.GasLimit > firstLondonGasLimit {
			deviation = header.GasLimit - firstLondonGasLimit
		} else {
			deviation = firstLondonGasLimit - header.GasLimit
		}

		if deviation > maxDeviation {
			maxDeviation = deviation
		}

		t.Logf("Block %d: GasLimit=%d, BaseFee=%s, Deviation=%d",
			i, header.GasLimit, header.BaseFee.String(), deviation)
	}

	// When within buffer, gas limit should stay close to parent's gas limit
	// Allow for small natural variations but not significant movement toward min/max
	// The deviation should be much smaller than the difference between min and max
	maxAllowedDeviation := (dynamicConfig.GasLimitMax - dynamicConfig.GasLimitMin) / 4
	assert.Less(t, maxDeviation, maxAllowedDeviation,
		"Gas limit should remain relatively stable when base fee is within buffer range")
}

// TestLateBlockNotEmpty tests that when a parent block is sealed late,
// blocks still have sufficient time to include transactions.
// This verifies the fix for empty blocks caused by late parent blocks.
func TestLateBlockNotEmpty(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0)

	// Start a single miner node
	stack, ethBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer stack.Close()

	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start mining
	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for initial blocks
	log.Info("Waiting for initial blocks...")
	for {
		time.Sleep(500 * time.Millisecond)
		if ethBackend.BlockChain().CurrentBlock().Number.Uint64() >= 3 {
			break
		}
	}

	// Stop mining and wait for it to fully stop
	ethBackend.StopMining()
	time.Sleep(500 * time.Millisecond)

	// Capture parent block
	parentBlock := ethBackend.BlockChain().CurrentBlock()
	parentNumber := parentBlock.Number.Uint64()
	parentTime := parentBlock.Time

	log.Info("Parent block", "number", parentNumber, "time", parentTime)

	// Add transactions BEFORE waiting (they should be in pool when we resume)
	txpool := ethBackend.TxPool()
	senderKey := pkey1
	senderAddr := crypto.PubkeyToAddress(senderKey.PublicKey)
	recipientAddr := crypto.PubkeyToAddress(pkey2.PublicKey)
	nonce := txpool.Nonce(senderAddr)
	signer := types.LatestSignerForChainID(genesis.Config.ChainID)

	// Start goroutine to continuously add transactions
	stopTxs := make(chan struct{})
	txNonce := nonce
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopTxs:
				return
			case <-ticker.C:
				// Add 5 transactions every 100ms
				for i := 0; i < 5; i++ {
					tx := types.NewTransaction(
						txNonce,
						recipientAddr,
						big.NewInt(1000),
						21000,
						big.NewInt(30000000000),
						nil,
					)
					signedTx, _ := types.SignTx(tx, signer, senderKey)
					if txpool.Add([]*types.Transaction{signedTx}, true)[0] == nil {
						txNonce++
					}
				}
			}
		}
	}()
	defer close(stopTxs)

	// Wait a bit for initial transactions to be added
	time.Sleep(300 * time.Millisecond)

	pending, queued := txpool.Stats()
	log.Info("Txpool after initial txs", "pending", pending, "queued", queued)
	require.Greater(t, pending, 0, "Expected transactions in pending")

	// Wait for parent to become older than block period (simulates late parent)
	blockPeriod := time.Duration(genesis.Config.Bor.Period["0"]) * time.Second

	// Wait until parent age > blockPeriod (not just equal)
	var parentAge int64
	for {
		parentBlock = ethBackend.BlockChain().CurrentBlock()
		parentNumber = parentBlock.Number.Uint64()
		parentTime = parentBlock.Time
		parentAge = time.Now().Unix() - int64(parentTime)

		if parentAge > int64(blockPeriod.Seconds()) {
			log.Info("Parent is now late", "number", parentNumber, "age", parentAge, "blockPeriod", blockPeriod.Seconds())
			break
		}

		time.Sleep(100 * time.Millisecond)
	}

	// Resume mining
	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for blocks to be mined and check that they ALL contain transactions
	log.Info("Waiting for blocks after resume...")
	blocksToCheck := uint64(3)
	maxWait := 10 * time.Second
	deadline := time.Now().Add(maxWait)
	allBlocksChecked := false

	var currentNumber uint64
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		currentNumber = ethBackend.BlockChain().CurrentBlock().Number.Uint64()

		if currentNumber >= parentNumber+blocksToCheck {
			allBlocksChecked = true
			break
		}
	}

	require.True(t, allBlocksChecked, "Expected %d blocks to be mined", blocksToCheck)

	// Verify ALL blocks after parent contain transactions
	totalTxsInBlocks := 0
	actualNow := time.Now().Unix()
	parentAge = actualNow - int64(parentTime) // Update parent age for error messages

	for i := uint64(1); i <= blocksToCheck; i++ {
		block := ethBackend.BlockChain().GetBlockByNumber(parentNumber + i)
		require.NotNil(t, block)
		txCount := len(block.Transactions())
		totalTxsInBlocks += txCount

		// Calculate expected build time
		expectedMinTime := int64(parentTime) + int64(blockPeriod.Seconds())
		actualBuildTime := int64(block.Time()) - actualNow
		timeFromParent := int64(block.Time()) - int64(parentTime)

		log.Info("Block check",
			"number", block.Number().Uint64(),
			"txCount", txCount,
			"blockTime", block.Time(),
			"actualNow", actualNow,
			"buildTime", actualBuildTime,
			"timeFromParent", timeFromParent,
			"expectedMin", expectedMinTime)

		// KEY ASSERTION: With the fix, ALL blocks should contain transactions
		// when there are pending transactions in the pool
		require.Greater(t, txCount, 0,
			"Block %d is empty! With late block fix, all blocks should include "+
				"transactions when txpool has pending txs. Parent age was %d seconds. "+
				"Block time: %d, Now: %d, Build time available: %d seconds.",
			block.Number().Uint64(), parentAge, block.Time(), actualNow, actualBuildTime)
	}

	log.Info("SUCCESS: All blocks after late parent contain transactions",
		"blocksChecked", blocksToCheck,
		"totalTxs", totalTxsInBlocks,
		"parentAge", parentAge)

	ethBackend.StopMining()
}

// TestVerifyPendingHeadersSpanRotationReorg tests that verifyPendingHeaders correctly
// detects invalid headers after a span rotation and rewinds the chain.
//
// Test scenario:
//  1. Validator 1 builds 18 blocks using span 0 (which has validator 1 as producer for all blocks)
//  2. A new span (span 1) is committed at block 16, assigning validator 2 as producer from block 16
//  3. Validator 2 receives blocks 1-18 from validator 1
//  4. When verifyPendingHeaders() is called, it should detect blocks 16-18 are invalid
//     (signed by validator 1, but span 1 says validator 2 should be the producer)
//  5. The chain should rewind to block 15 and validator 2 should rebuild blocks 16+
func TestVerifyPendingHeadersSpanRotationReorg(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		var err error
		faucets[i], err = crypto.GenerateKey()
		require.NoError(t, err)
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 1}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.ProducerDelay = map[string]uint64{"0": 4}
	genesis.Config.Bor.BackupMultiplier = map[string]uint64{"0": 2}
	genesis.Config.Bor.StateSyncConfirmationDelay = map[string]uint64{"0": 128}

	genesis.Config.Bor.RioBlock = big.NewInt(0)

	validator1Addr := crypto.PubkeyToAddress(keys[0].PublicKey)
	validator2Addr := crypto.PubkeyToAddress(keys[1].PublicKey)
	chainId := genesis.Config.ChainID.String()

	span0 := createSpanWithProducer(validator1Addr, 0, 0, 255, chainId)

	span1 := createSpanWithProducer(validator2Addr, 1, 16, 271, chainId)

	var (
		stacks  []*node.Node
		nodes   []*eth.Ethereum
		enodes  []*enode.Node
		borEngs []*bor.Bor
	)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for i := 0; i < 2; i++ {
		h := mocks.NewMockIHeimdallClient(ctrl)
		h.EXPECT().Close().AnyTimes()
		h.EXPECT().GetSpan(gomock.Any(), uint64(0)).Return(&span0, nil).AnyTimes()
		h.EXPECT().GetLatestSpan(gomock.Any()).Return(&span0, nil).AnyTimes()
		h.EXPECT().FetchCheckpoint(gomock.Any(), int64(-1)).Return(nil, fmt.Errorf("no checkpoint available")).AnyTimes()
		h.EXPECT().FetchMilestone(gomock.Any()).Return(nil, fmt.Errorf("no milestone available")).AnyTimes()
		h.EXPECT().FetchStatus(gomock.Any()).Return(&ctypes.SyncInfo{CatchingUp: false}, nil).AnyTimes()
		h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()

		stack, ethBackend, err := InitMinerWithHeimdall(genesis, keys[i], h)
		if err != nil {
			t.Fatal("Error occurred while initializing miner", "error", err)
		}
		defer stack.Close()

		for stack.Server().NodeInfo().Ports.Listener == 0 {
			time.Sleep(250 * time.Millisecond)
		}

		borEng := ethBackend.Engine().(*bor.Bor)

		for _, n := range enodes {
			stack.Server().AddPeer(n)
		}

		stacks = append(stacks, stack)
		nodes = append(nodes, ethBackend)
		enodes = append(enodes, stack.Server().Self())
		borEngs = append(borEngs, borEng)
	}

	time.Sleep(3 * time.Second)

	for _, node := range nodes {
		if err := node.StartMining(); err != nil {
			t.Fatal("Error occurred while starting miner", "error", err)
		}
	}

	for {
		blockHeaderVal0 := nodes[0].BlockChain().CurrentHeader()
		if blockHeaderVal0.Number.Uint64() >= 18 {
			log.Info("Chain reached block 18", "number", blockHeaderVal0.Number.Uint64())
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	originalSigners := make(map[uint64]common.Address)
	for blockNum := uint64(16); blockNum <= uint64(18); blockNum++ {
		header := nodes[0].BlockChain().GetHeaderByNumber(blockNum)
		require.NotNil(t, header, "Block %d should exist", blockNum)
		author, err := nodes[0].Engine().Author(header)
		require.NoError(t, err)
		originalSigners[blockNum] = author
		log.Info("Original block signer before span rotation",
			"blockNum", blockNum,
			"signer", author,
			"isValidator1", author == validator1Addr)
	}

	log.Info("Simulating span rotation - updating Heimdall client to return span 1")

	h2 := mocks.NewMockIHeimdallClient(ctrl)
	h2.EXPECT().Close().AnyTimes()
	h2.EXPECT().GetSpan(gomock.Any(), uint64(0)).Return(&span0, nil).AnyTimes()
	h2.EXPECT().GetSpan(gomock.Any(), uint64(1)).Return(&span1, nil).AnyTimes()
	h2.EXPECT().GetLatestSpan(gomock.Any()).Return(&span1, nil).AnyTimes()
	h2.EXPECT().FetchCheckpoint(gomock.Any(), int64(-1)).Return(nil, fmt.Errorf("no checkpoint available")).AnyTimes()
	h2.EXPECT().FetchMilestone(gomock.Any()).Return(&borMilestone.Milestone{EndBlock: 15}, nil).AnyTimes()
	h2.EXPECT().FetchStatus(gomock.Any()).Return(&ctypes.SyncInfo{CatchingUp: false}, nil).AnyTimes()
	h2.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()

	borEngs[1].SetHeimdallClient(h2)

	// Update spanner on node 1 to return validator 2 as producer for blocks 16+
	spanner2 := getMockedSpannerWithSpanRotation(t, validator1Addr, validator2Addr, 16)
	borEngs[1].SetSpanner(spanner2)

	borEngs[1].PurgeCache()
	log.Info("Purged caches on validator 2 to apply new span data")

	log.Info("Waiting for header verification loop to detect invalid headers...")

	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var chainStateChanged bool
	for !chainStateChanged {
		select {
		case <-timeout:
			log.Warn("Timeout waiting for chain state change")
			chainStateChanged = true
		case <-ticker.C:
			for blockNum := uint64(16); blockNum <= uint64(18); blockNum++ {
				header := nodes[1].BlockChain().GetHeaderByNumber(blockNum)
				if header == nil {
					log.Info("Block rewound (no longer exists)", "blockNum", blockNum)
					chainStateChanged = true
					break
				}
				author, err := nodes[1].Engine().Author(header)
				if err == nil && author != originalSigners[blockNum] {
					log.Info("Block has new signer", "blockNum", blockNum, "newSigner", author, "originalSigner", originalSigners[blockNum])
					chainStateChanged = true
					break
				}
			}
			if !chainStateChanged {
				log.Debug("Chain state unchanged, waiting...", "currentHead", nodes[1].BlockChain().CurrentHeader().Number.Uint64())
			}
		}
	}

	// Check validator 2's current head
	finalHeaderVal1 := nodes[1].BlockChain().CurrentHeader()
	log.Info("Validator 2 final head after verification",
		"number", finalHeaderVal1.Number.Uint64(),
		"hash", finalHeaderVal1.Hash())

	block15 := nodes[1].BlockChain().GetHeaderByNumber(15)
	require.NotNil(t, block15, "Block 15 should still exist")

	for blockNum := uint64(16); blockNum <= uint64(18); blockNum++ {
		header := nodes[1].BlockChain().GetHeaderByNumber(blockNum)
		if header == nil {
			log.Info("Block not found on validator 2's chain (chain has rewound)",
				"blockNum", blockNum)
			continue
		}

		author, err := nodes[1].Engine().Author(header)
		require.NoError(t, err, "Failed to get author for block %d", blockNum)

		originalSigner := originalSigners[blockNum]

		log.Info("Block author check",
			"blockNum", blockNum,
			"currentAuthor", author,
			"originalSigner", originalSigner,
			"validator1Addr", validator1Addr,
			"validator2Addr", validator2Addr)

		require.NotEqual(t, originalSigner, author,
			"Block %d should NOT be signed by the original signer (%s) after span rotation. "+
				"This indicates verifyPendingHeaders() did not detect the invalid headers.", blockNum, originalSigner)
	}
}

// createSpanWithProducer creates a span with a single producer
func createSpanWithProducer(producer common.Address, spanId, startBlock, endBlock uint64, chainId string) borTypes.Span {
	validator := valset.Validator{
		ID:               0,
		Address:          producer,
		VotingPower:      1000,
		ProposerPriority: 0,
	}
	validatorSet := valset.ValidatorSet{
		Validators: []*valset.Validator{&validator},
		Proposer:   &validator,
	}
	return borTypes.Span{
		Id:                spanId,
		StartBlock:        startBlock,
		EndBlock:          endBlock,
		ValidatorSet:      borSpan.ConvertBorValSetToHeimdallValSet(&validatorSet),
		SelectedProducers: borSpan.ConvertBorValidatorsToHeimdallValidators([]*valset.Validator{&validator}),
		BorChainId:        chainId,
	}
}

// getMockedSpannerWithSpanRotation creates a spanner that returns different validators
// based on block number (validator1 before rotationBlock, validator2 from rotationBlock onwards)
func getMockedSpannerWithSpanRotation(t *testing.T, validator1, validator2 common.Address, rotationBlock uint64) *bor.MockSpanner {
	t.Helper()

	ctrl := gomock.NewController(t)
	spanner := bor.NewMockSpanner(ctrl)

	// Return different validators based on block number
	spanner.EXPECT().GetCurrentValidatorsByHash(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, hash common.Hash, blockNum uint64) ([]*valset.Validator, error) {
			if blockNum >= rotationBlock {
				return []*valset.Validator{{ID: 1, Address: validator2, VotingPower: 1000}}, nil
			}
			return []*valset.Validator{{ID: 0, Address: validator1, VotingPower: 1000}}, nil
		}).AnyTimes()

	spanner.EXPECT().GetCurrentValidatorsByBlockNrOrHash(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, blockNrOrHash rpc.BlockNumberOrHash, blockNum uint64) ([]*valset.Validator, error) {
			if blockNum >= rotationBlock {
				return []*valset.Validator{{ID: 1, Address: validator2, VotingPower: 1000}}, nil
			}
			return []*valset.Validator{{ID: 0, Address: validator1, VotingPower: 1000}}, nil
		}).AnyTimes()

	// GetCurrentSpan returns the new span after rotation.
	// Note: This spanner is only installed after span rotation, so it returns the new span.
	// Block-number based validation is handled by GetCurrentValidatorsByHash.
	validator2Val := valset.Validator{ID: 1, Address: validator2, VotingPower: 1000}
	span1Mock := &borTypes.Span{
		Id:                1,
		StartBlock:        rotationBlock,
		EndBlock:          rotationBlock + 255,
		ValidatorSet:      borSpan.ConvertBorValSetToHeimdallValSet(&valset.ValidatorSet{Validators: []*valset.Validator{&validator2Val}, Proposer: &validator2Val}),
		SelectedProducers: borSpan.ConvertBorValidatorsToHeimdallValidators([]*valset.Validator{&validator2Val}),
	}
	spanner.EXPECT().GetCurrentSpan(gomock.Any(), gomock.Any(), gomock.Any()).Return(span1Mock, nil).AnyTimes()

	spanner.EXPECT().CommitSpan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	return spanner
}

// TestBlockProduction_Basic verifies that a single miner can produce multiple
// consecutive blocks correctly.
func TestBlockProduction_Basic(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0) // Enable Rio so snapshot uses spanByBlockNumber (no ecrecover needed)

	// Start a single miner.
	stack, ethBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer stack.Close()

	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start mining
	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for the miner to produce at least 10 blocks
	targetBlock := uint64(10)
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			currentNum := ethBackend.BlockChain().CurrentBlock().Number.Uint64()
			t.Fatalf("Timed out waiting for block %d, current block: %d", targetBlock, currentNum)
		default:
			time.Sleep(500 * time.Millisecond)
			if ethBackend.BlockChain().CurrentBlock().Number.Uint64() >= targetBlock {
				goto done
			}
		}
	}
done:

	chain := ethBackend.BlockChain()
	currentNum := chain.CurrentBlock().Number.Uint64()
	t.Logf("Miner produced %d blocks", currentNum)

	// Verify chain integrity: each block's parent hash matches the previous block's hash
	for i := uint64(1); i <= currentNum; i++ {
		block := chain.GetBlockByNumber(i)
		require.NotNil(t, block, "block %d not found", i)

		if i > 0 {
			parent := chain.GetBlockByNumber(i - 1)
			require.NotNil(t, parent, "parent block %d not found", i-1)
			require.Equal(t, parent.Hash(), block.ParentHash(),
				"block %d ParentHash mismatch: expected %x, got %x", i, parent.Hash(), block.ParentHash())
		}

		// Verify state root is valid (can open state at this root)
		_, err := chain.StateAt(block.Root())
		require.NoError(t, err, "cannot open state at block %d root %x", i, block.Root())
	}
}

// TestBlockProduction_WithTransactions verifies that the miner correctly
// includes transactions in blocks.
func TestBlockProduction_WithTransactions(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0)

	stack, ethBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer stack.Close()

	for stack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	err = ethBackend.StartMining()
	require.NoError(t, err)

	// Wait for a few blocks first
	for ethBackend.BlockChain().CurrentBlock().Number.Uint64() < 2 {
		time.Sleep(500 * time.Millisecond)
	}

	// Submit transactions
	txpool := ethBackend.TxPool()
	senderKey := pkey1
	recipientAddr := crypto.PubkeyToAddress(pkey2.PublicKey)
	signer := types.LatestSignerForChainID(genesis.Config.ChainID)

	nonce := txpool.Nonce(crypto.PubkeyToAddress(senderKey.PublicKey))
	txCount := 10

	for i := 0; i < txCount; i++ {
		tx := types.NewTransaction(
			nonce+uint64(i),
			recipientAddr,
			big.NewInt(1000),
			21000,
			big.NewInt(30000000000),
			nil,
		)
		signedTx, err := types.SignTx(tx, signer, senderKey)
		require.NoError(t, err)
		errs := txpool.Add([]*types.Transaction{signedTx}, true)
		require.Nil(t, errs[0], "failed to add tx %d", i)
	}

	// Wait for transactions to be included in blocks
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for transactions to be included")
		default:
			time.Sleep(500 * time.Millisecond)
			// Check if all transactions have been mined
			currentNonce := txpool.Nonce(crypto.PubkeyToAddress(senderKey.PublicKey))
			if currentNonce >= nonce+uint64(txCount) {
				goto txsDone
			}
		}
	}
txsDone:

	chain := ethBackend.BlockChain()
	currentNum := chain.CurrentBlock().Number.Uint64()
	t.Logf("All %d transactions included by block %d", txCount, currentNum)

	// Give block insertion time to settle before scanning the chain.
	time.Sleep(2 * time.Second)
	currentNum = chain.CurrentBlock().Number.Uint64()

	// Verify we can find the transactions in the blocks
	totalTxs := 0
	for i := uint64(1); i <= currentNum; i++ {
		block := chain.GetBlockByNumber(i)
		if block != nil {
			totalTxs += len(block.Transactions())
		}
	}
	require.GreaterOrEqual(t, totalTxs, txCount,
		"expected at least %d transactions across all blocks, got %d", txCount, totalTxs)
}

// TestPipelinedImportSRC_BasicImport verifies that a non-mining node with
// pipelined import SRC enabled correctly syncs blocks from a block-producing
// peer. The importer computes state roots in the background (overlapping
// SRC(N) with tx execution of N+1) and should arrive at the same chain state
// as the BP.
func TestPipelinedImportSRC_BasicImport(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0)

	// Start a normal BP (no pipeline on mining side)
	bpStack, bpBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer bpStack.Close()

	for bpStack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start a non-mining importer with pipelined import SRC
	importerStack, importerBackend, err := InitImporterWithPipelinedSRC(genesis, keys[1], true)
	require.NoError(t, err)
	defer importerStack.Close()

	for importerStack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Connect the two peers
	connectAndWaitForPeers(t, importerStack, bpStack)

	// Start mining on the BP
	err = bpBackend.StartMining()
	require.NoError(t, err)

	// Wait for the BP to produce at least 20 blocks
	targetBlock := uint64(20)
	deadline := time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			bpNum := bpBackend.BlockChain().CurrentBlock().Number.Uint64()
			t.Fatalf("Timed out waiting for BP to reach block %d, current: %d", targetBlock, bpNum)
		default:
			time.Sleep(500 * time.Millisecond)
			if bpBackend.BlockChain().CurrentBlock().Number.Uint64() >= targetBlock {
				goto bpDone
			}
		}
	}
bpDone:

	bpNum := bpBackend.BlockChain().CurrentBlock().Number.Uint64()
	t.Logf("BP produced %d blocks, waiting for importer to sync", bpNum)

	// Wait for the importer to sync up to the target
	deadline = time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			importerNum := importerBackend.BlockChain().CurrentBlock().Number.Uint64()
			t.Fatalf("Timed out waiting for importer to reach block %d, current: %d", targetBlock, importerNum)
		default:
			time.Sleep(500 * time.Millisecond)
			if importerBackend.BlockChain().CurrentBlock().Number.Uint64() >= targetBlock {
				goto importerDone
			}
		}
	}
importerDone:

	importerNum := importerBackend.BlockChain().CurrentBlock().Number.Uint64()
	t.Logf("Importer synced to block %d", importerNum)

	// Allow async DB writes to flush
	time.Sleep(2 * time.Second)

	// Use the minimum of both chains for comparison
	bpNum = bpBackend.BlockChain().CurrentBlock().Number.Uint64()
	importerNum = importerBackend.BlockChain().CurrentBlock().Number.Uint64()
	compareUpTo := bpNum
	if importerNum < compareUpTo {
		compareUpTo = importerNum
	}

	bpChain := bpBackend.BlockChain()
	importerChain := importerBackend.BlockChain()

	for i := uint64(1); i <= compareUpTo; i++ {
		bpBlock := bpChain.GetBlockByNumber(i)
		require.NotNil(t, bpBlock, "BP missing block %d", i)

		importerBlock := importerChain.GetBlockByNumber(i)
		require.NotNil(t, importerBlock, "importer missing block %d", i)

		// Block hashes must match
		require.Equal(t, bpBlock.Hash(), importerBlock.Hash(),
			"block %d hash mismatch: BP=%x importer=%x", i, bpBlock.Hash(), importerBlock.Hash())

		// State roots must match
		require.Equal(t, bpBlock.Root(), importerBlock.Root(),
			"block %d state root mismatch: BP=%x importer=%x", i, bpBlock.Root(), importerBlock.Root())

		// Verify the importer can open state at each block's root
		_, err := importerChain.StateAt(importerBlock.Root())
		require.NoError(t, err, "importer cannot open state at block %d root %x", i, importerBlock.Root())
	}

	t.Logf("Verified %d blocks: hashes, state roots, and state accessibility all match", compareUpTo)
}

// TestPipelinedImportSRC_WithTransactions verifies that a non-mining node with
// pipelined import SRC correctly imports blocks containing transactions. It
// checks that transaction receipts exist and that account balances match
// between the BP and the importer.
func TestPipelinedImportSRC_WithTransactions(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0)

	// Start BP without pipeline
	bpStack, bpBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer bpStack.Close()

	for bpStack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start importer with pipelined import SRC
	importerStack, importerBackend, err := InitImporterWithPipelinedSRC(genesis, keys[1], true)
	require.NoError(t, err)
	defer importerStack.Close()

	for importerStack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Connect peers
	connectAndWaitForPeers(t, importerStack, bpStack)

	// Start mining
	err = bpBackend.StartMining()
	require.NoError(t, err)

	// Wait for a few blocks before submitting transactions
	for bpBackend.BlockChain().CurrentBlock().Number.Uint64() < 2 {
		time.Sleep(500 * time.Millisecond)
	}

	// Submit ETH transfer transactions to the BP
	txpool := bpBackend.TxPool()
	senderKey := pkey1
	senderAddr := crypto.PubkeyToAddress(senderKey.PublicKey)
	recipientAddr := crypto.PubkeyToAddress(pkey2.PublicKey)
	signer := types.LatestSignerForChainID(genesis.Config.ChainID)

	nonce := txpool.Nonce(senderAddr)
	txCount := 10
	transferAmount := big.NewInt(1000)

	for i := 0; i < txCount; i++ {
		tx := types.NewTransaction(
			nonce+uint64(i),
			recipientAddr,
			transferAmount,
			21000,
			big.NewInt(30000000000),
			nil,
		)
		signedTx, err := types.SignTx(tx, signer, senderKey)
		require.NoError(t, err)
		errs := txpool.Add([]*types.Transaction{signedTx}, true)
		require.Nil(t, errs[0], "failed to add tx %d", i)
	}

	// Wait for all transactions to be mined on the BP
	deadline := time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for transactions to be mined on BP")
		default:
			time.Sleep(500 * time.Millisecond)
			if txpool.Nonce(senderAddr) >= nonce+uint64(txCount) {
				goto txsMined
			}
		}
	}
txsMined:

	bpNum := bpBackend.BlockChain().CurrentBlock().Number.Uint64()
	t.Logf("All %d transactions mined on BP by block %d", txCount, bpNum)

	// Wait for the importer to sync past the block containing the last tx
	targetBlock := bpNum
	deadline = time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			importerNum := importerBackend.BlockChain().CurrentBlock().Number.Uint64()
			t.Fatalf("Timed out waiting for importer to reach block %d, current: %d", targetBlock, importerNum)
		default:
			time.Sleep(500 * time.Millisecond)
			if importerBackend.BlockChain().CurrentBlock().Number.Uint64() >= targetBlock {
				goto importerSynced
			}
		}
	}
importerSynced:

	// Allow async DB writes to flush
	time.Sleep(2 * time.Second)

	importerNum := importerBackend.BlockChain().CurrentBlock().Number.Uint64()
	t.Logf("Importer synced to block %d", importerNum)

	bpChain := bpBackend.BlockChain()
	importerChain := importerBackend.BlockChain()

	// Re-read current block numbers after the flush delay
	bpNum = bpChain.CurrentBlock().Number.Uint64()
	importerNum = importerChain.CurrentBlock().Number.Uint64()
	compareUpTo := bpNum
	if importerNum < compareUpTo {
		compareUpTo = importerNum
	}

	// Verify blocks, state roots, and transaction counts match
	totalBpTxs := 0
	totalImporterTxs := 0
	for i := uint64(1); i <= compareUpTo; i++ {
		bpBlock := bpChain.GetBlockByNumber(i)
		require.NotNil(t, bpBlock, "BP missing block %d", i)

		importerBlock := importerChain.GetBlockByNumber(i)
		require.NotNil(t, importerBlock, "importer missing block %d", i)

		require.Equal(t, bpBlock.Hash(), importerBlock.Hash(),
			"block %d hash mismatch", i)
		require.Equal(t, bpBlock.Root(), importerBlock.Root(),
			"block %d state root mismatch", i)

		// Transaction counts must match per block
		require.Equal(t, len(bpBlock.Transactions()), len(importerBlock.Transactions()),
			"block %d tx count mismatch: BP=%d importer=%d", i,
			len(bpBlock.Transactions()), len(importerBlock.Transactions()))

		totalBpTxs += len(bpBlock.Transactions())
		totalImporterTxs += len(importerBlock.Transactions())

		// Verify receipts exist on the importer for each transaction
		for j, tx := range importerBlock.Transactions() {
			receipt, _, _, _ := rawdb.ReadReceipt(importerBackend.ChainDb(), tx.Hash(), importerChain.Config())
			require.NotNil(t, receipt, "importer missing receipt for tx %d in block %d (hash=%x)", j, i, tx.Hash())
		}
	}

	require.GreaterOrEqual(t, totalBpTxs, txCount,
		"expected at least %d transactions across BP blocks, got %d", txCount, totalBpTxs)
	require.Equal(t, totalBpTxs, totalImporterTxs,
		"total tx count mismatch: BP=%d importer=%d", totalBpTxs, totalImporterTxs)

	// Verify account balances match between BP and importer at the latest
	// common block — this confirms the pipelined SRC produced correct state.
	bpState, err := bpChain.StateAt(bpChain.GetBlockByNumber(compareUpTo).Root())
	require.NoError(t, err, "cannot open BP state at block %d", compareUpTo)

	importerState, err := importerChain.StateAt(importerChain.GetBlockByNumber(compareUpTo).Root())
	require.NoError(t, err, "cannot open importer state at block %d", compareUpTo)

	bpRecipientBal := bpState.GetBalance(recipientAddr)
	importerRecipientBal := importerState.GetBalance(recipientAddr)
	require.Equal(t, bpRecipientBal.String(), importerRecipientBal.String(),
		"recipient balance mismatch at block %d: BP=%s importer=%s",
		compareUpTo, bpRecipientBal, importerRecipientBal)

	bpSenderBal := bpState.GetBalance(senderAddr)
	importerSenderBal := importerState.GetBalance(senderAddr)
	require.Equal(t, bpSenderBal.String(), importerSenderBal.String(),
		"sender balance mismatch at block %d: BP=%s importer=%s",
		compareUpTo, bpSenderBal, importerSenderBal)

	t.Logf("Verified %d blocks with %d total transactions, balances match", compareUpTo, totalImporterTxs)
}

// TestPipelinedImportSRC_SelfDestruct verifies that a contract which
// self-destructs in its constructor is correctly handled by the FlatDiff
// overlay during pipelined import. Without the Destructs check in
// getStateObject, the importer would fall through to the trie reader and
// see stale pre-destruct state from the committed parent root.
//
// Post-Cancun (EIP-6780), SELFDESTRUCT only fully destroys an account when
// called in the same transaction that created the contract, so the test uses
// a constructor that immediately self-destructs.
func TestPipelinedImportSRC_SelfDestruct(t *testing.T) {
	t.Parallel()
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}

	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 16)
	genesis.Config.Bor.Period = map[string]uint64{"0": 2}
	genesis.Config.Bor.Sprint = map[string]uint64{"0": 16}
	genesis.Config.Bor.RioBlock = big.NewInt(0)

	// Start a normal BP (no pipeline on mining side)
	bpStack, bpBackend, err := InitMiner(genesis, keys[0], true)
	require.NoError(t, err)
	defer bpStack.Close()

	for bpStack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Start importer with pipelined import SRC
	importerStack, importerBackend, err := InitImporterWithPipelinedSRC(genesis, keys[1], true)
	require.NoError(t, err)
	defer importerStack.Close()

	for importerStack.Server().NodeInfo().Ports.Listener == 0 {
		time.Sleep(250 * time.Millisecond)
	}

	// Connect peers
	connectAndWaitForPeers(t, importerStack, bpStack)

	// Start mining
	err = bpBackend.StartMining()
	require.NoError(t, err)

	// Wait for a few blocks so we're past cancunBlock=3
	for bpBackend.BlockChain().CurrentBlock().Number.Uint64() < 5 {
		time.Sleep(500 * time.Millisecond)
	}

	// Deploy a contract whose constructor immediately self-destructs,
	// sending its value back to CALLER.
	// Init code: CALLER (0x33) SELFDESTRUCT (0xFF) = 0x33FF
	selfDestructInitCode := []byte{byte(vm.CALLER), byte(vm.SELFDESTRUCT)}
	deployValue := big.NewInt(1_000_000_000_000_000_000) // 1 ETH

	txpool := bpBackend.TxPool()
	senderKey := pkey1
	senderAddr := crypto.PubkeyToAddress(senderKey.PublicKey)
	signer := types.LatestSignerForChainID(genesis.Config.ChainID)

	nonce := txpool.Nonce(senderAddr)

	// Predict the contract address
	contractAddr := crypto.CreateAddress(senderAddr, nonce)
	t.Logf("Deploying self-destruct contract at predicted address %s with nonce %d", contractAddr.Hex(), nonce)

	// Record sender balance before deployment
	bpChain := bpBackend.BlockChain()
	preState, err := bpChain.StateAt(bpChain.CurrentBlock().Root)
	require.NoError(t, err)
	senderBalBefore := preState.GetBalance(senderAddr)
	t.Logf("Sender balance before deploy: %s", senderBalBefore.String())

	// Create the deployment tx with value
	deployTx, err := types.SignTx(
		types.NewContractCreation(nonce, deployValue, 100_000, big.NewInt(30_000_000_000), selfDestructInitCode),
		signer, senderKey,
	)
	require.NoError(t, err)

	errs := txpool.Add([]*types.Transaction{deployTx}, true)
	require.Nil(t, errs[0], "failed to add deploy tx")

	// Also send a normal transfer in the NEXT block to force pipeline overlap.
	// This ensures block N+1 uses the FlatDiff from block N (which has the destruct).
	nonce++
	recipientAddr := crypto.PubkeyToAddress(pkey2.PublicKey)
	transferTx, err := types.SignTx(
		types.NewTransaction(nonce, recipientAddr, big.NewInt(1000), 21000, big.NewInt(30_000_000_000), nil),
		signer, senderKey,
	)
	require.NoError(t, err)
	errs = txpool.Add([]*types.Transaction{transferTx}, true)
	require.Nil(t, errs[0], "failed to add transfer tx")

	// Wait for both txs to be mined
	deadline := time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("Timed out waiting for transactions to be mined on BP")
		default:
			time.Sleep(500 * time.Millisecond)
			if txpool.Nonce(senderAddr) >= nonce+1 {
				goto txsMined
			}
		}
	}
txsMined:

	bpNum := bpBackend.BlockChain().CurrentBlock().Number.Uint64()
	t.Logf("Transactions mined on BP by block %d", bpNum)

	// Wait for importer to sync
	targetBlock := bpNum
	deadline = time.After(120 * time.Second)
	for {
		select {
		case <-deadline:
			importerNum := importerBackend.BlockChain().CurrentBlock().Number.Uint64()
			t.Fatalf("Timed out waiting for importer to reach block %d, current: %d", targetBlock, importerNum)
		default:
			time.Sleep(500 * time.Millisecond)
			if importerBackend.BlockChain().CurrentBlock().Number.Uint64() >= targetBlock {
				goto importerSynced
			}
		}
	}
importerSynced:

	// Allow async DB writes to flush
	time.Sleep(2 * time.Second)

	importerChain := importerBackend.BlockChain()
	importerNum := importerChain.CurrentBlock().Number.Uint64()
	t.Logf("Importer synced to block %d", importerNum)

	// Re-read BP chain head
	bpNum = bpChain.CurrentBlock().Number.Uint64()
	compareUpTo := bpNum
	if importerNum < compareUpTo {
		compareUpTo = importerNum
	}

	// Verify block hashes and state roots match
	for i := uint64(1); i <= compareUpTo; i++ {
		bpBlock := bpChain.GetBlockByNumber(i)
		require.NotNil(t, bpBlock, "BP missing block %d", i)

		importerBlock := importerChain.GetBlockByNumber(i)
		require.NotNil(t, importerBlock, "importer missing block %d", i)

		require.Equal(t, bpBlock.Hash(), importerBlock.Hash(),
			"block %d hash mismatch", i)
		require.Equal(t, bpBlock.Root(), importerBlock.Root(),
			"block %d state root mismatch", i)
	}

	// Verify the self-destructed contract is gone on BOTH chains
	bpState, err := bpChain.StateAt(bpChain.GetBlockByNumber(compareUpTo).Root())
	require.NoError(t, err)
	importerState, err := importerChain.StateAt(importerChain.GetBlockByNumber(compareUpTo).Root())
	require.NoError(t, err)

	// Contract should have zero balance (ETH sent back to sender via SELFDESTRUCT)
	bpContractBal := bpState.GetBalance(contractAddr)
	importerContractBal := importerState.GetBalance(contractAddr)
	require.True(t, bpContractBal.IsZero(), "BP: contract should have zero balance, got %s", bpContractBal)
	require.True(t, importerContractBal.IsZero(), "importer: contract should have zero balance, got %s", importerContractBal)

	// Contract should have no code
	bpCode := bpState.GetCode(contractAddr)
	importerCode := importerState.GetCode(contractAddr)
	require.Empty(t, bpCode, "BP: contract should have no code")
	require.Empty(t, importerCode, "importer: contract should have no code")

	// Contract nonce should be zero (fully destroyed)
	require.Equal(t, uint64(0), bpState.GetNonce(contractAddr), "BP: contract nonce should be 0")
	require.Equal(t, uint64(0), importerState.GetNonce(contractAddr), "importer: contract nonce should be 0")

	// Sender balances must match between BP and importer
	bpSenderBal := bpState.GetBalance(senderAddr)
	importerSenderBal := importerState.GetBalance(senderAddr)
	require.Equal(t, bpSenderBal.String(), importerSenderBal.String(),
		"sender balance mismatch: BP=%s importer=%s", bpSenderBal, importerSenderBal)

	t.Logf("Verified: contract %s fully destroyed, sender balances match (BP=%s, importer=%s)",
		contractAddr.Hex(), bpSenderBal, importerSenderBal)
}

// TestProducerRecoversAfterMiningRestart is an integration-level regression
// test for the family of "elected but silent" producer stalls
// (post-mortem INC-37 for the 2026-05-07 Amoy chain halt).
//
// Behavioral property under test: after a brief mining stop/start cycle on
// an active block producer — which closes the worker's startup race window
// (the same window the leak family exercised in production) — the producer
// must eventually seal blocks again. None of the leak paths fixed in this
// family should be able to permanently wedge the producer state machine
// across a restart.
//
// The leak paths that the unit tests precisely cover and that this
// integration test exercises behaviorally:
//
//  1. miner.commitWork syncing-leak early return (unit test:
//     TestCommitWorkLeaksPendingWorkBlockWhenSyncing).
//  2. miner.taskLoop missing pendingTasks cleanup on Bor.Seal stop-branch
//     exits (cleanup wired via SealWithStopHook onStopExit callback;
//     unit test: TestTaskLoopInterruptPreservesPendingTasks).
//  3. Bor.Seal second-select silent default drop (unit test:
//     TestSeal_BlocksOnFullResultChannelInsteadOfSilentDrop).
//
// A fourth leak path — the mainLoop PeerCount==0 dropped-newWorkReq — was
// closed in the same family by removing the gate entirely; the path no
// longer exists, so there's no unit test pair for it.
//
// Integration-level limitations:
//   - The race condition for (2) and the resultCh-full condition for (3)
//     are timing-sensitive and not reliably reproduced in a 2-node test
//     without artificial backpressure.
//
// What this test DOES guarantee end-to-end: if any change introduces a
// state-machine bug that prevents a producer from sealing again after a
// brief mining stop/start cycle, this test catches the regression at the
// integration level (multiple producers, real P2P, real chain insertion).
func TestProducerRecoversAfterMiningRestart(t *testing.T) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, true)))
	fdlimit.Raise(2048)

	// Faucets to fund (unused by this test but expected by InitGenesis).
	faucets := make([]*ecdsa.PrivateKey, 128)
	for i := 0; i < len(faucets); i++ {
		faucets[i], _ = crypto.GenerateKey()
	}
	genesis := InitGenesis(t, faucets, "./testdata/genesis_2val.json", 8)

	var (
		stacks []*node.Node
		nodes  []*eth.Ethereum
		enodes []*enode.Node
	)
	for i := 0; i < 2; i++ {
		stack, ethBackend, err := InitMiner(genesis, keys[i], true)
		if err != nil {
			t.Fatalf("error initializing miner %d: %v", i, err)
		}
		defer stack.Close()

		for stack.Server().NodeInfo().Ports.Listener == 0 {
			time.Sleep(250 * time.Millisecond)
		}
		for _, n := range enodes {
			stack.Server().AddPeer(n)
		}
		stacks = append(stacks, stack)
		nodes = append(nodes, ethBackend)
		enodes = append(enodes, stack.Server().Self())
	}

	// Let P2P stabilize then start mining on both nodes.
	time.Sleep(3 * time.Second)
	for _, n := range nodes {
		if err := n.StartMining(); err != nil {
			t.Fatalf("StartMining failed: %v", err)
		}
	}

	// Phase 1: wait for the chain to advance to at least block 5 — confirms
	// both producers are healthy before we disrupt anything.
	waitForBlock := func(target uint64, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			h := nodes[0].BlockChain().CurrentHeader()
			if h != nil && h.Number.Uint64() >= target {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		head := nodes[0].BlockChain().CurrentHeader()
		var got uint64
		if head != nil {
			got = head.Number.Uint64()
		}
		t.Fatalf("chain did not reach block %d within %s (head=%d)", target, timeout, got)
	}
	waitForBlock(5, 30*time.Second)

	// Phase 2: stop mining on node 0 and remove its peer link to node 1.
	// This recreates the "producer is briefly isolated" condition the
	// leak family hit in production (the startup race against P2P peer
	// establishment). With node 0 stopped, node 1 should continue alone.
	headBeforeStop := nodes[0].BlockChain().CurrentHeader().Number.Uint64()
	t.Logf("phase 2: head before stop=%d, stopping mining on node 0 and removing peer", headBeforeStop)
	nodes[0].StopMining()
	stacks[0].Server().RemovePeer(enodes[1])

	// Brief pause to let the producer-state machine settle into the
	// "no peers, mining stopped" state. This is the window where the
	// leaked-wedge bugs would have set pendingWorkBlock/pendingTasks and
	// not cleared them. After the fixes, the state-machine should be
	// reusable on the next StartMining.
	time.Sleep(2 * time.Second)

	// Phase 3: re-add peer, restart mining on node 0. With any of the
	// leak paths present, node 0 could sit silently — no seals — until
	// the process is restarted. With the fixes, node 0 must produce
	// blocks again within a short recovery window.
	stacks[0].Server().AddPeer(enodes[1])
	if err := nodes[0].StartMining(); err != nil {
		t.Fatalf("StartMining (restart) failed: %v", err)
	}

	// Record the seal count by node 0 at the moment of restart.
	countSealsByNode0 := func() int {
		head := nodes[0].BlockChain().CurrentHeader()
		if head == nil {
			return 0
		}
		count := 0
		signerAddr0 := nodes[0].AccountManager().Accounts()[0]
		for n := head.Number.Uint64(); n > 0; n-- {
			h := nodes[0].BlockChain().GetHeaderByNumber(n)
			if h == nil {
				continue
			}
			author, err := nodes[0].Engine().Author(h)
			if err == nil && author == signerAddr0 {
				count++
			}
			// Only look at recent blocks to avoid O(chain) work.
			if head.Number.Uint64()-n > 50 {
				break
			}
		}
		return count
	}
	sealsBefore := countSealsByNode0()

	// Phase 4: require that node 0 seals at least one new block within
	// the recovery window. In a healthy state machine, the next sprint
	// boundary that hands node 0 the producer slot will produce a block;
	// in a wedged state machine, no new seals appear no matter how long
	// we wait. 30s is generous given the sprint length and producer
	// alternation pattern of genesis_2val.json (sprint=8, alternating
	// producers means node 0 is primary every other sprint).
	recoveryDeadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(recoveryDeadline) {
		if countSealsByNode0() > sealsBefore {
			t.Logf("phase 4: node 0 recovered — produced new block after restart (seals %d → %d)",
				sealsBefore, countSealsByNode0())
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	headAfter := nodes[0].BlockChain().CurrentHeader().Number.Uint64()
	t.Fatalf("node 0 did not seal a new block within 45s after StopMining/StartMining cycle "+
		"(head before stop=%d, head after recovery window=%d, seals by node 0: before=%d after=%d). "+
		"This indicates a leaked-wedge regression in the producer state machine — see post-mortem "+
		"INC-37 for the family of bugs this test guards against.",
		headBeforeStop, headAfter, sealsBefore, countSealsByNode0())
}
