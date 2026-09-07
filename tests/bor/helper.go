//go:build integration

package bor

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	gomock "go.uber.org/mock/gomock"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk" //nolint:typecheck
	borSpan "github.com/ethereum/go-ethereum/consensus/bor/heimdall/span"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/txpool/legacypool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/secp256k1"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/tests/bor/mocks"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"

	borTypes "github.com/0xPolygon/heimdall-v2/x/bor/types"
	ctypes "github.com/cometbft/cometbft/rpc/core/types"
)

var (

	// Only this account is a validator for the 0th span
	key, _ = crypto.HexToECDSA(privKey)
	addr   = crypto.PubkeyToAddress(key.PublicKey) // 0x71562b71999873DB5b286dF957af199Ec94617F7

	// This account is one the validators for 1st span (0-indexed)
	key2, _ = crypto.HexToECDSA(privKey2)
	addr2   = crypto.PubkeyToAddress(key2.PublicKey) // 0x9fB29AAc15b9A4B7F17c3385939b007540f4d791

	// This account is secondary validator for 1st span (0-indexed)
	key3, _ = crypto.HexToECDSA(privKey3)
	addr3   = crypto.PubkeyToAddress(key3.PublicKey) // 0x96C42C56fdb78294F96B0cFa33c92bed7D75F96a

	keys = []*ecdsa.PrivateKey{key, key2}
)

const (
	privKey  = "b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291"
	privKey2 = "9b28f36fbd67381120752d6172ecdcf10e06ab2d9a1367aac00cdcd6ac7855d3"
	privKey3 = "c8deb0bea5c41afe8e37b4d1bd84e31adff11b09c8c96ff4b605003cce067cd9"

	// The genesis for tests was generated with following parameters
	extraSeal = 65 // Fixed number of extra-data suffix bytes reserved for signer seal

	sprintSize uint64 = 4
	spanSize   uint64 = 8

	validatorHeaderBytesLength = common.AddressLength + 20 // address + power
)

type initializeData struct {
	genesis  *core.Genesis
	ethereum *eth.Ethereum
}

func setupMiner(t *testing.T, n int, genesis *core.Genesis) ([]*node.Node, []*eth.Ethereum, []*enode.Node) {
	t.Helper()

	// Create an Ethash network based off of the Ropsten config
	var (
		stacks []*node.Node
		nodes  []*eth.Ethereum
		enodes []*enode.Node
	)

	for i := 0; i < n; i++ {
		// Start the node and wait until it's up
		stack, ethBackend, err := InitMiner(genesis, keys[i], true)
		if err != nil {
			t.Fatal("Error occurred while initialising miner", "error", err)
		}

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

	return stacks, nodes, enodes
}

func buildEthereumInstance(t *testing.T, db ethdb.Database, updateGenesis ...func(gen *core.Genesis)) *initializeData {
	return buildEthereumInstanceWithVMTrace(t, db, "", updateGenesis...)
}

// buildEthereumInstanceWithVMTrace is like buildEthereumInstance but also wires
// up a live tracer registered under vmTraceName. Pass an empty string to skip.
// The named tracer must already be registered with tracers.LiveDirectory.
func buildEthereumInstanceWithVMTrace(t *testing.T, db ethdb.Database, vmTraceName string, updateGenesis ...func(gen *core.Genesis)) *initializeData {
	genesisData, err := ioutil.ReadFile("./testdata/genesis.json")
	if err != nil {
		t.Fatalf("%s", err)
	}

	gen := &core.Genesis{}
	if err := json.Unmarshal(genesisData, gen); err != nil {
		t.Fatalf("%s", err)
	}
	for _, update := range updateGenesis {
		update(gen)
	}

	ethConf := &eth.Config{
		Genesis:     gen,
		BorLogs:     true,
		StateScheme: "hash",
		VMTrace:     vmTraceName,
	}
	ethConf.Genesis.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	ethereum := utils.CreateBorEthereum(ethConf)
	if err != nil {
		t.Fatalf("failed to register Ethereum protocol: %v", err)
	}

	ethConf.Genesis.MustCommit(ethereum.ChainDb(), triedb.NewDatabase(ethereum.ChainDb(), triedb.HashDefaults))

	ethereum.Engine().(*bor.Bor).Authorize(addr, func(account accounts.Account, s string, data []byte) ([]byte, error) {
		return crypto.Sign(crypto.Keccak256(data), key)
	})

	return &initializeData{
		genesis:  gen,
		ethereum: ethereum,
	}
}

func insertNewBlock(t *testing.T, chain *core.BlockChain, block *types.Block) {
	t.Helper()

	if _, err := chain.InsertChain([]*types.Block{block}, false); err != nil {
		t.Fatalf("%s", err)
	}
}

type modifyHeaderFunc func(header *types.Header)
type modifyBlockFunc func(block *types.Block, receipts []*types.Receipt) *types.Block

// encodeBlockExtraDataForTest wraps types.EncodeBlockExtraData for callers in
// this file that never populate the Giugliano gas-target fields.
func encodeBlockExtraDataForTest(chainConfig *params.ChainConfig, number *big.Int, validatorBytes []byte) ([]byte, error) {
	return types.EncodeBlockExtraData(chainConfig, number, validatorBytes, nil, nil)
}

func buildHeader(t *testing.T, chain *core.BlockChain, parentBlock *types.Block, signer []byte, borConfig *params.BorConfig, currentValidators []*valset.Validator, modifyHeader []modifyHeaderFunc) *types.Header {
	t.Helper()

	header := &types.Header{
		Number:     big.NewInt(int64(parentBlock.Number().Uint64() + 1)),
		Difficulty: big.NewInt(int64(parentBlock.Difficulty().Uint64())),
		GasLimit:   parentBlock.GasLimit(),
		ParentHash: parentBlock.Hash(),
	}
	number := header.Number.Uint64()

	if signer == nil {
		signer = getSignerKey(header.Number.Uint64())
	}

	// Similar to the logic in bor consensus
	header.Time = parentBlock.Time() + bor.CalcProducerDelay(header.Number.Uint64(), 0, borConfig)
	// Keeping this causes some e2e tests to fail because they work under certain time assumptions
	// if header.Time < uint64(time.Now().Unix()) {
	// 	header.Time = uint64(time.Now().Unix())
	// }

	// Similar to logic in bor consensus (prepare)
	header.Extra = make([]byte, 32+65) // vanity + extraSeal
	if len(header.Extra) < types.ExtraVanityLength {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, types.ExtraVanityLength-len(header.Extra))...)
	}
	header.Extra = header.Extra[:types.ExtraVanityLength]

	var isSprintEnd bool
	if (number+1)%chain.Config().Bor.CalculateSprint(number) == 0 {
		isSprintEnd = true
	}
	isSpanStart := IsSpanStart(number)

	if isSpanStart {
		header.Difficulty = new(big.Int).SetInt64(int64(len(currentValidators)))
	}

	if isSprintEnd {
		sort.Sort(valset.ValidatorsByAddress(currentValidators))

		// Extra data is encoded differently after cancun
		if chain.Config().IsCancun(header.Number) {
			var tempValidatorBytes []byte
			for _, validator := range currentValidators {
				tempValidatorBytes = append(tempValidatorBytes, validator.HeaderBytes()...)
			}

			blockExtraDataBytes, err := encodeBlockExtraDataForTest(chain.Config(), header.Number, tempValidatorBytes)
			if err != nil {
				t.Fatalf("error while encoding block extra data: %v", err)
			}
			header.Extra = append(header.Extra, blockExtraDataBytes...)
		} else {
			validatorBytes := make([]byte, len(currentValidators)*validatorHeaderBytesLength)
			header.Extra = make([]byte, 32+len(validatorBytes)+65) // vanity + validatorBytes + extraSeal

			for i, val := range currentValidators {
				copy(validatorBytes[i*validatorHeaderBytesLength:], val.HeaderBytes())
			}

			copy(header.Extra[32:], validatorBytes)
		}
	} else if chain.Config().IsCancun(header.Number) {
		blockExtraDataBytes, err := encodeBlockExtraDataForTest(chain.Config(), header.Number, nil)
		if err != nil {
			t.Fatalf("error while encoding block extra data: %v", err)
		}

		header.Extra = append(header.Extra, blockExtraDataBytes...)
	}

	header.Extra = append(header.Extra, make([]byte, types.ExtraSealLength)...)

	if chain.Config().IsLondon(header.Number) {
		header.BaseFee = eip1559.CalcBaseFee(chain.Config(), parentBlock.Header())

		if !chain.Config().IsLondon(parentBlock.Number()) {
			parentGasLimit := parentBlock.GasLimit() * params.ElasticityMultiplier
			header.GasLimit = core.CalcGasLimit(parentGasLimit, parentGasLimit)
		}
	}

	for _, fn := range modifyHeader {
		fn(header)
	}

	return header
}

func buildNextBlock(t *testing.T, _bor consensus.Engine, chain *core.BlockChain, parentBlock *types.Block, signer []byte, borConfig *params.BorConfig, txs []*types.Transaction, currentValidators []*valset.Validator, skipSealing bool, modifyHeader []modifyHeaderFunc, modifyBody []modifyBlockFunc) *types.Block {
	t.Helper()

	// Build a new header based on parent block
	header := buildHeader(t, chain, parentBlock, signer, borConfig, currentValidators, modifyHeader)
	state, err := chain.State()
	if err != nil {
		t.Fatalf("%s", err)
	}

	b := &blockGen{header: header}
	for _, tx := range txs {
		b.addTxWithChain(chain, state, tx, addr)
	}

	// Finalize and seal the block
	var block *types.Block
	block, b.receipts, _, err = _bor.FinalizeAndAssemble(chain, b.header, state, &types.Body{
		Transactions: b.txs,
	}, b.receipts)
	if err != nil {
		panic(fmt.Sprintf("error finalizing block: %v", err))
	}

	for _, fn := range modifyBody {
		block = fn(block, b.receipts)
	}

	// Write state changes to db
	root, err := state.Commit(block.NumberU64(), chain.Config().IsEIP158(b.header.Number), false)
	if err != nil {
		panic(fmt.Sprintf("state write error: %v", err))
	}

	if err := state.Database().TrieDB().Commit(root, false); err != nil {
		panic(fmt.Sprintf("trie write error: %v", err))
	}

	res := make(chan *consensus.NewSealedBlockEvent, 1)
	if skipSealing {
		header := block.Header()
		sign(t, header, signer, borConfig)
		return types.NewBlock(header, block.Body(), b.receipts, trie.NewStackTrie(nil))
	}

	// Create a nil witness for testing
	var witness *stateless.Witness
	err = _bor.Seal(chain, block, witness, res, nil)
	if err != nil {
		// an error case - sign manually
		sign(t, header, signer, borConfig)
		return types.NewBlockWithHeader(header)
	}

	sealedEvent := <-res
	return sealedEvent.Block
}

type blockGen struct {
	txs      []*types.Transaction
	receipts []*types.Receipt
	gasPool  *core.GasPool
	header   *types.Header
}

func (b *blockGen) addTxWithChain(bc *core.BlockChain, statedb *state.StateDB, tx *types.Transaction, coinbase common.Address) {
	if b.gasPool == nil {
		b.setCoinbase(coinbase)
	}

	statedb.SetTxContext(tx.Hash(), len(b.txs))

	context := core.NewEVMBlockContext(b.header, bc, nil)
	evm := vm.NewEVM(context, statedb, bc.Config(), vm.Config{})
	receipt, err := core.ApplyTransaction(evm, b.gasPool, statedb, b.header, tx, &b.header.GasUsed)
	if err != nil {
		panic(err)
	}

	b.txs = append(b.txs, tx)
	b.receipts = append(b.receipts, receipt)
}

func (b *blockGen) setCoinbase(addr common.Address) {
	if b.gasPool != nil {
		if len(b.txs) > 0 {
			panic("coinbase must be set before adding transactions")
		}

		panic("coinbase can only be set once")
	}

	b.header.Coinbase = addr
	b.gasPool = new(core.GasPool).AddGas(b.header.GasLimit)
}

func sign(t *testing.T, header *types.Header, signer []byte, c *params.BorConfig) {
	t.Helper()

	sig, err := secp256k1.Sign(crypto.Keccak256(bor.BorRLP(header, c)), signer)
	if err != nil {
		t.Fatalf("%s", err)
	}

	copy(header.Extra[len(header.Extra)-extraSeal:], sig)
}

//nolint:unused
func stateSyncEventsPayload(t *testing.T) []*clerk.EventRecordWithTime {
	t.Helper()

	stateData, err := os.ReadFile("./testdata/states.json")
	if err != nil {
		t.Fatalf("%s", err)
	}

	res := make([]*clerk.EventRecordWithTime, 0)
	if err := json.Unmarshal(stateData, &res); err != nil {
		t.Fatalf("%s", err)
	}

	return res
}

//nolint:unused
func loadSpanFromFile(t *testing.T) *borTypes.Span {
	t.Helper()

	spanData, err := os.ReadFile("./testdata/span.json")
	if err != nil {
		t.Fatalf("%s", err)
	}

	res := &borTypes.Span{}

	if err := json.Unmarshal(spanData, res); err != nil {
		t.Fatalf("%s", err)
	}

	return res
}

func getSignerKey(number uint64) []byte {
	signerKey := privKey

	if IsSpanStart(number) {
		// validator set in the new span has changed
		signerKey = privKey2
	}

	newKey, _ := hex.DecodeString(signerKey)

	return newKey
}

func getMockedHeimdallClient(t *testing.T, heimdallSpan *borTypes.Span) (*mocks.MockIHeimdallClient, *gomock.Controller) {
	t.Helper()

	ctrl := gomock.NewController(t)
	h := mocks.NewMockIHeimdallClient(ctrl)

	h.EXPECT().GetSpan(gomock.Any(), uint64(1)).Return(heimdallSpan, nil).AnyTimes()
	h.EXPECT().StateSyncEvents(gomock.Any(), gomock.Any(), gomock.Any()).Return([]*clerk.EventRecordWithTime{getSampleEventRecord(t)}, nil).AnyTimes()
	h.EXPECT().FetchStatus(gomock.Any()).Return(&ctypes.SyncInfo{CatchingUp: false}, nil).AnyTimes()

	return h, ctrl
}

func createMockSpan(address common.Address, chainId string) borTypes.Span {
	// Mock span 0 for heimdall calls
	validator := valset.Validator{
		ID:               0,
		Address:          address,
		VotingPower:      10,
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

func createMockHeimdall(ctrl *gomock.Controller, span0, span1 *borTypes.Span) *mocks.MockIHeimdallClient {
	h := mocks.NewMockIHeimdallClient(ctrl)

	h.EXPECT().Close().AnyTimes()
	h.EXPECT().GetSpan(gomock.Any(), uint64(0)).Return(span0, nil).AnyTimes()
	h.EXPECT().GetSpan(gomock.Any(), uint64(1)).Return(span1, nil).AnyTimes()
	h.EXPECT().GetLatestSpan(gomock.Any()).Return(span1, nil).AnyTimes()
	// Return errors for checkpoint and milestone to prevent background verification from
	// comparing empty hashes against the chain and triggering unwanted chain rewinds
	h.EXPECT().FetchCheckpoint(gomock.Any(), int64(-1)).Return(nil, errors.New("no checkpoint")).AnyTimes()
	h.EXPECT().FetchMilestone(gomock.Any()).Return(nil, errors.New("no milestone")).AnyTimes()
	h.EXPECT().FetchStatus(gomock.Any()).Return(&ctypes.SyncInfo{CatchingUp: false}, nil).AnyTimes()

	return h
}

func getMockedSpanner(t *testing.T, validators []*valset.Validator) *bor.MockSpanner {
	t.Helper()

	mockSpan := &borTypes.Span{
		Id:         0,
		StartBlock: 0,
		EndBlock:   0,
	}

	ctrl := gomock.NewController(t)
	spanner := bor.NewMockSpanner(ctrl)
	spanner.EXPECT().GetCurrentValidatorsByHash(gomock.Any(), gomock.Any(), gomock.Any()).Return(validators, nil).AnyTimes()
	spanner.EXPECT().GetCurrentValidatorsByBlockNrOrHash(gomock.Any(), gomock.Any(), gomock.Any()).Return(validators, nil).AnyTimes()
	spanner.EXPECT().GetCurrentSpan(gomock.Any(), gomock.Any(), gomock.Any()).Return(mockSpan, nil).AnyTimes()
	spanner.EXPECT().CommitSpan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	return spanner
}

func generateFakeStateSyncEvents(sample *clerk.EventRecordWithTime, count int) []*clerk.EventRecordWithTime {
	events := make([]*clerk.EventRecordWithTime, count)
	event := *sample
	event.ID = 1
	events[0] = &clerk.EventRecordWithTime{}
	*events[0] = event

	for i := 1; i < count; i++ {
		event.ID = uint64(i + 1)
		event.Time = event.Time.Add(1 * time.Second)
		events[i] = &clerk.EventRecordWithTime{}
		*events[i] = event
	}

	return events
}

func buildStateEvent(sample *clerk.EventRecordWithTime, id uint64, timeStamp int64) *clerk.EventRecordWithTime {
	event := *sample
	event.ID = id
	event.Time = time.Unix(timeStamp, 0)

	return &event
}

func getSampleEventRecord(t *testing.T) *clerk.EventRecordWithTime {
	t.Helper()

	eventRecords := stateSyncEventsPayload(t)
	eventRecords[0].Time = time.Unix(1, 0)

	return eventRecords[0]
}

func newGwei(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(params.GWei))
}

func IsSpanEnd(number uint64) bool {
	return (number+1)%spanSize == 0
}

func IsSpanStart(number uint64) bool {
	return number%spanSize == 0
}

func IsSprintStart(number uint64) bool {
	return number%sprintSize == 0
}

func IsSprintEnd(number uint64) bool {
	return (number+1)%sprintSize == 0
}

func InitGenesis(t *testing.T, faucets []*ecdsa.PrivateKey, fileLocation string, sprintSize uint64) *core.Genesis {
	t.Helper()

	// sprint size = 8 in genesis
	genesisData, err := os.ReadFile(fileLocation)
	if err != nil {
		t.Fatalf("%s", err)
	}

	genesis := &core.Genesis{}

	if err := json.Unmarshal(genesisData, genesis); err != nil {
		t.Fatalf("%s", err)
	}

	genesis.Config.ChainID = big.NewInt(15001)
	genesis.Config.EIP150Block = big.NewInt(0)

	genesis.Config.Bor.Sprint["0"] = sprintSize

	return genesis
}

func InitMiner(genesis *core.Genesis, privKey *ecdsa.PrivateKey, withoutHeimdall bool) (*node.Node, *eth.Ethereum, error) {
	return InitMinerWithOptions(genesis, privKey, withoutHeimdall, 0, nil)
}

// InitMinerWithHeimdall creates a miner node with a mock HeimdallClient injected during initialization.
// This is the preferred way to test with mock Heimdall clients as it avoids caching issues.
func InitMinerWithHeimdall(genesis *core.Genesis, privKey *ecdsa.PrivateKey, heimdallClient bor.IHeimdallClient) (*node.Node, *eth.Ethereum, error) {
	return InitMinerWithOptions(genesis, privKey, false, 0, heimdallClient)
}

// DynamicGasLimitConfig holds configuration for dynamic gas limit testing
type DynamicGasLimitConfig struct {
	EnableDynamicGasLimit bool
	GasLimitMin           uint64
	GasLimitMax           uint64
	TargetBaseFee         uint64
	BaseFeeBuffer         uint64
}

// InitMinerWithDynamicGasLimit creates a miner with dynamic gas limit configuration
func InitMinerWithDynamicGasLimit(genesis *core.Genesis, privKey *ecdsa.PrivateKey, withoutHeimdall bool, dynamicConfig DynamicGasLimitConfig) (*node.Node, *eth.Ethereum, error) {
	// Define the basic configurations for the Ethereum node
	datadir, err := os.MkdirTemp("", "InitMiner-"+uuid.New().String())
	if err != nil {
		return nil, nil, err
	}

	config := &node.Config{
		Name:    "geth",
		Version: params.Version,
		DataDir: datadir,
		P2P: p2p.Config{
			ListenAddr:  "0.0.0.0:0",
			NoDiscovery: true,
			MaxPeers:    25,
		},
		UseLightweightKDF: true,
	}
	// Create the node and configure a full Ethereum node on it
	stack, err := node.New(config)
	if err != nil {
		return nil, nil, err
	}

	ethBackend, err := eth.New(stack, &ethconfig.Config{
		Genesis:         genesis,
		NetworkId:       genesis.Config.ChainID.Uint64(),
		SyncMode:        downloader.FullSync,
		DatabaseCache:   256,
		DatabaseHandles: 256,
		TxPool:          legacypool.DefaultConfig,
		GPO:             ethconfig.Defaults.GPO,
		Miner: miner.Config{
			Etherbase:             crypto.PubkeyToAddress(privKey.PublicKey),
			GasCeil:               genesis.GasLimit * 11 / 10,
			GasPrice:              big.NewInt(1),
			Recommit:              time.Second,
			EnableDynamicGasLimit: dynamicConfig.EnableDynamicGasLimit,
			GasLimitMin:           dynamicConfig.GasLimitMin,
			GasLimitMax:           dynamicConfig.GasLimitMax,
			TargetBaseFee:         dynamicConfig.TargetBaseFee,
			BaseFeeBuffer:         dynamicConfig.BaseFeeBuffer,
		},
		WithoutHeimdall: withoutHeimdall,
	})

	if err != nil {
		return nil, nil, err
	}

	// register backend to account manager with keystore for signing
	keydir := stack.KeyStoreDir()

	n, p := keystore.StandardScryptN, keystore.StandardScryptP
	kStore := keystore.NewKeyStore(keydir, n, p)

	_, err = kStore.ImportECDSA(privKey, "")

	if err != nil {
		return nil, nil, err
	}

	acc := kStore.Accounts()[0]
	err = kStore.Unlock(acc, "")

	if err != nil {
		return nil, nil, err
	}

	// proceed to authorize the local account manager in any case
	ethBackend.AccountManager().AddBackend(kStore)

	err = stack.Start()

	return stack, ethBackend, err
}

func InitMinerWithBlockTime(genesis *core.Genesis, privKey *ecdsa.PrivateKey, withoutHeimdall bool, blockTime time.Duration) (*node.Node, *eth.Ethereum, error) {
	return InitMinerWithOptions(genesis, privKey, withoutHeimdall, blockTime, nil)
}

// InitMinerWithOptions is the base function for creating miner nodes with various options.
func InitMinerWithOptions(genesis *core.Genesis, privKey *ecdsa.PrivateKey, withoutHeimdall bool, blockTime time.Duration, heimdallClient bor.IHeimdallClient) (*node.Node, *eth.Ethereum, error) {
	// Define the basic configurations for the Ethereum node
	datadir, err := os.MkdirTemp("", "InitMiner-"+uuid.New().String())
	if err != nil {
		return nil, nil, err
	}

	config := &node.Config{
		Name:    "geth",
		Version: params.Version,
		DataDir: datadir,
		P2P: p2p.Config{
			ListenAddr:  "0.0.0.0:0",
			NoDiscovery: true,
			MaxPeers:    25,
		},
		UseLightweightKDF: true,
	}
	// Create the node and configure a full Ethereum node on it
	stack, err := node.New(config)
	if err != nil {
		return nil, nil, err
	}

	ethBackend, err := eth.New(stack, &ethconfig.Config{
		Genesis:         genesis,
		NetworkId:       genesis.Config.ChainID.Uint64(),
		SyncMode:        downloader.FullSync,
		DatabaseCache:   256,
		DatabaseHandles: 256,
		TxPool:          legacypool.DefaultConfig,
		GPO:             ethconfig.Defaults.GPO,
		Miner: miner.Config{
			Etherbase:           crypto.PubkeyToAddress(privKey.PublicKey),
			GasCeil:             genesis.GasLimit * 11 / 10,
			GasPrice:            big.NewInt(1),
			Recommit:            time.Second,
			BlockTime:           blockTime,
			CommitInterruptFlag: true,
		},
		WithoutHeimdall:        withoutHeimdall,
		OverrideHeimdallClient: heimdallClient,
	})

	if err != nil {
		return nil, nil, err
	}

	// register backend to account manager with keystore for signing
	keydir := stack.KeyStoreDir()

	n, p := keystore.StandardScryptN, keystore.StandardScryptP
	kStore := keystore.NewKeyStore(keydir, n, p)

	_, err = kStore.ImportECDSA(privKey, "")

	if err != nil {
		return nil, nil, err
	}

	acc := kStore.Accounts()[0]
	err = kStore.Unlock(acc, "")

	if err != nil {
		return nil, nil, err
	}

	// proceed to authorize the local account manager in any case
	ethBackend.AccountManager().AddBackend(kStore)

	err = stack.Start()

	return stack, ethBackend, err
}

// InitImporterWithPipelinedSRC creates a non-mining node with pipelined import
// SRC enabled. The node will import blocks from peers using the pipelined state
// root computation path. A validator key is still needed for the keystore (used
// for P2P identity / account manager) but the node does NOT start mining.
func InitImporterWithPipelinedSRC(genesis *core.Genesis, privKey *ecdsa.PrivateKey, withoutHeimdall bool) (*node.Node, *eth.Ethereum, error) {
	stack, err := newPipelineTestNode("InitImporter-")
	if err != nil {
		return nil, nil, err
	}
	ethBackend, err := eth.New(stack, &ethconfig.Config{
		Genesis:         genesis,
		NetworkId:       genesis.Config.ChainID.Uint64(),
		SyncMode:        downloader.FullSync,
		DatabaseCache:   256,
		DatabaseHandles: 256,
		TxPool:          legacypool.DefaultConfig,
		GPO:             ethconfig.Defaults.GPO,
		Miner: miner.Config{
			Etherbase: crypto.PubkeyToAddress(privKey.PublicKey),
			GasCeil:   genesis.GasLimit * 11 / 10,
			GasPrice:  big.NewInt(1),
			Recommit:  time.Second,
		},
		WithoutHeimdall:          withoutHeimdall,
		EnablePipelinedImportSRC: true,
		PipelinedImportSRCLogs:   true,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := importValidatorKey(stack, ethBackend, privKey); err != nil {
		return nil, nil, err
	}
	// The debug/tracing namespace is registered by the CLI server layer in
	// production, not by eth.New — register it here so RPC tests can exercise
	// debug_* methods against the pipelined importer.
	stack.RegisterAPIs(tracers.APIs(ethBackend.APIBackend))
	return stack, ethBackend, stack.Start()
}

// newPipelineTestNode creates a headless node.Node in a fresh temp datadir
// with P2P discovery disabled. Shared between the miner and importer test
// setups since their node-level configuration is identical.
func newPipelineTestNode(dirPrefix string) (*node.Node, error) {
	datadir, err := os.MkdirTemp("", dirPrefix+uuid.New().String())
	if err != nil {
		return nil, err
	}
	return node.New(&node.Config{
		Name:    "geth",
		Version: params.Version,
		DataDir: datadir,
		P2P: p2p.Config{
			ListenAddr:  "0.0.0.0:0",
			NoDiscovery: true,
			MaxPeers:    25,
		},
		UseLightweightKDF: true,
	})
}

// importValidatorKey imports the validator's ECDSA key into the node's
// keystore, unlocks the imported account, and registers the keystore with
// the eth account manager so mining / signing paths can use it.
func importValidatorKey(stack *node.Node, ethBackend *eth.Ethereum, privKey *ecdsa.PrivateKey) error {
	kStore := keystore.NewKeyStore(stack.KeyStoreDir(), keystore.StandardScryptN, keystore.StandardScryptP)
	if _, err := kStore.ImportECDSA(privKey, ""); err != nil {
		return err
	}
	if err := kStore.Unlock(kStore.Accounts()[0], ""); err != nil {
		return err
	}
	ethBackend.AccountManager().AddBackend(kStore)
	return nil
}

// connectAndWaitForPeers statically peers the two nodes and blocks until the
// connection is live on both servers. Two failure modes make a naive AddPeer
// unreliable on slow hosts:
//
//   - Self() publishes its TCP port asynchronously after the listener starts,
//     so an early AddPeer can capture a port-0 enode. The dial scheduler
//     dedupes static nodes by ID (p2p/dial.go addStaticCh handling), so later
//     re-adds never update the bad record — the dialer keeps dialing a dead
//     address forever. Wait for both enodes to carry a real port before the
//     first AddPeer.
//
//   - A transiently failed dial (e.g. a loaded CI runner) parks the target in
//     the scheduler's history for dialHistoryExpiration (35s), so a 60s
//     deadline covers barely one retry. Use a deadline long enough for
//     several history windows.
func connectAndWaitForPeers(t *testing.T, a, b *node.Node) {
	t.Helper()
	deadline := time.After(120 * time.Second)
	for a.Server().Self().TCP() == 0 || b.Server().Self().TCP() == 0 {
		select {
		case <-deadline:
			t.Fatalf("nodes failed to publish listener ports: a=%d b=%d",
				a.Server().Self().TCP(), b.Server().Self().TCP())
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
	// Dial from one side only: mutual AddPeer invites simultaneous-connect
	// collisions where each server can drop the other's inbound connection
	// as a duplicate of its own in-flight dial, and because failed dials
	// enter the dialer's per-node history (checkDial: errRecentlyDialed,
	// ~35s) aligned retries can keep colliding until the deadline. A single
	// static dial can't collide, and the scheduler retries it on its own
	// after each history expiry.
	a.Server().AddPeer(b.Server().Self())
	for a.Server().PeerCount() == 0 || b.Server().PeerCount() == 0 {
		select {
		case <-deadline:
			t.Fatalf("nodes failed to peer within deadline: peers a=%d b=%d (a=%s b=%s)",
				a.Server().PeerCount(), b.Server().PeerCount(),
				a.Server().Self(), b.Server().Self())
		default:
			time.Sleep(250 * time.Millisecond)
		}
	}
}
