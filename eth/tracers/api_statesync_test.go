package tracers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"
)

var (
	key, _  = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	address = crypto.PubkeyToAddress(key.PublicKey)

	// State receiver address from BorConfig — must match what the tracer reads
	stateReceiverAddr = common.HexToAddress("0x0000000000000000000000000000000000001001")

	// Target contract that the state receiver forwards calls to.
	// Bytecode: PUSH1(0) PUSH1(0) LOG0 STOP — emits an empty LOG0 on any call.
	targetAddr = common.HexToAddress("0x0000000000000000000000000000000000002000")
	targetCode = []byte{0x60, 0x00, 0x60, 0x00, 0xa0, 0x00}

	// The state receiver contract bytecode which can be used for processing state sync events. It
	// forwards calls via: call(txGas, receiver, 0, add(data, 0x20), mload(data), 0, 0) to the
	// child chain contract.
	stateReceiverCode = common.FromHex("0x608060405234801561001057600080fd5b50600436106100415760003560e01c806319494a17146100465780633434735f146100e15780635407ca671461012b575b600080fd5b6100c76004803603604081101561005c57600080fd5b81019080803590602001909291908035906020019064010000000081111561008357600080fd5b82018360208201111561009557600080fd5b803590602001918460018302840111640100000000831117156100b757600080fd5b9091929391929390505050610149565b604051808215151515815260200191505060405180910390f35b6100e961047a565b604051808273ffffffffffffffffffffffffffffffffffffffff1673ffffffffffffffffffffffffffffffffffffffff16815260200191505060405180910390f35b610133610492565b6040518082815260200191505060405180910390f35b600073fffffffffffffffffffffffffffffffffffffffe73ffffffffffffffffffffffffffffffffffffffff163373ffffffffffffffffffffffffffffffffffffffff1614610200576040517f08c379a00000000000000000000000000000000000000000000000000000000081526004018080602001828103825260128152602001807f4e6f742053797374656d2041646465737321000000000000000000000000000081525060200191505060405180910390fd5b606061025761025285858080601f016020809104026020016040519081016040528093929190818152602001838380828437600081840152601f19601f82011690508083019250505050505050610498565b6104c6565b905060006102788260008151811061026b57fe5b60200260200101516105a3565b905080600160005401146102f4576040517f08c379a000000000000000000000000000000000000000000000000000000000815260040180806020018281038252601b8152602001807f537461746549647320617265206e6f742073657175656e7469616c000000000081525060200191505060405180910390fd5b600080815480929190600101919050555060006103248360018151811061031757fe5b6020026020010151610614565b905060606103458460028151811061033857fe5b6020026020010151610637565b9050610350826106c3565b1561046f576000624c4b409050606084836040516024018083815260200180602001828103825283818151815260200191508051906020019080838360005b838110156103aa57808201518184015260208101905061038f565b50505050905090810190601f1680156103d75780820380516001836020036101000a031916815260200191505b5093505050506040516020818303038152906040527f26c53bea000000000000000000000000000000000000000000000000000000007bffffffffffffffffffffffffffffffffffffffffffffffffffffffff19166020820180517bffffffffffffffffffffffffffffffffffffffffffffffffffffffff8381831617835250505050905060008082516020840160008887f1965050505b505050509392505050565b73fffffffffffffffffffffffffffffffffffffffe81565b60005481565b6104a0610943565b600060208301905060405180604001604052808451815260200182815250915050919050565b60606104d1826106dc565b6104da57600080fd5b60006104e58361072a565b905060608160405190808252806020026020018201604052801561052357816020015b61051061095d565b8152602001906001900390816105085790505b5090506000610535856020015161079b565b8560200151019050600080600090505b848110156105965761055683610824565b915060405180604001604052808381526020018481525084828151811061057957fe5b602002602001018190525081830192508080600101915050610545565b5082945050505050919050565b60008082600001511180156105bd57506021826000015111155b6105c657600080fd5b60006105d5836020015161079b565b9050600081846000015103905060008083866020015101905080519150602083101561060857826020036101000a820491505b81945050505050919050565b6000601582600001511461062757600080fd5b610630826105a3565b9050919050565b6060600082600001511161064a57600080fd5b6000610659836020015161079b565b905060008184600001510390506060816040519080825280601f01601f19166020018201604052801561069b5781602001600182028038833980820191505090505b50905060008160200190506106b78487602001510182856108dc565b81945050505050919050565b600080823b905060008163ffffffff1611915050919050565b600080826000015114156106f35760009050610725565b60008083602001519050805160001a915060c060ff168260ff16101561071e57600092505050610725565b6001925050505b919050565b600080826000015114156107415760009050610796565b60008090506000610755846020015161079b565b84602001510190506000846000015185602001510190505b8082101561078f5761077e82610824565b82019150828060010193505061076d565b8293505050505b919050565b600080825160001a9050608060ff168110156107bb57600091505061081f565b60b860ff168110806107e0575060c060ff1681101580156107df575060f860ff1681105b5b156107ef57600191505061081f565b60c060ff1681101561080f5760018060b80360ff1682030191505061081f565b60018060f80360ff168203019150505b919050565b6000806000835160001a9050608060ff1681101561084557600191506108d2565b60b860ff16811015610862576001608060ff1682030191506108d1565b60c060ff168110156108925760b78103600185019450806020036101000a855104600182018101935050506108d0565b60f860ff168110156108af57600160c060ff1682030191506108cf565b60f78103600185019450806020036101000a855104600182018101935050505b5b5b5b8192505050919050565b60008114156108ea5761093e565b5b602060ff16811061091a5782518252602060ff1683019250602060ff1682019150602060ff16810390506108eb565b6000600182602060ff16036101000a03905080198451168184511681811785525050505b505050565b604051806040016040528060008152602001600081525090565b60405180604001604052806000815260200160008152509056fea265627a7a7231582083fbdacb76f32b4112d0f7db9a596937925824798a0026ba0232322390b5263764736f6c634300050b0032")
)

// callTraceLog represents a log entry captured by callTracer with withLog enabled.
type callTraceLog struct {
	Address  common.Address `json:"address"`
	Topics   []common.Hash  `json:"topics"`
	Data     hexutil.Bytes  `json:"data"`
	Position hexutil.Uint   `json:"position"`
}

// callTraceFrame represents a call frame in callTracer output. The struct is recursive:
// each frame can contain sub-calls and logs.
type callTraceFrame struct {
	Type  string           `json:"type"`
	From  common.Address   `json:"from"`
	To    common.Address   `json:"to"`
	Calls []callTraceFrame `json:"calls,omitempty"`
	Logs  []callTraceLog   `json:"logs,omitempty"`
}

// borTestBackend extends testBackend with Bor-specific configuration for testing
// state sync transaction handling across the Madhugiri hardfork.
type borTestBackend struct {
	testBackend

	modifiedBlocks map[uint64]bool
	modifiedHashes map[common.Hash]uint64
}

// BlockByNumber overrides testBackend to read modified blocks from DB.
func (b *borTestBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	if number == rpc.PendingBlockNumber || number == rpc.LatestBlockNumber {
		return b.chain.GetBlockByNumber(b.chain.CurrentBlock().Number.Uint64()), nil
	}

	blockNum := uint64(number)
	if b.modifiedBlocks != nil && b.modifiedBlocks[blockNum] {
		header := b.chain.GetHeaderByNumber(blockNum)
		if header == nil {
			return nil, nil
		}
		body := rawdb.ReadBody(b.chaindb, header.Hash(), blockNum)
		if body == nil {
			return nil, nil
		}
		return types.NewBlockWithHeader(header).WithBody(*body), nil
	}

	return b.chain.GetBlockByNumber(blockNum), nil
}

// BlockByHash overrides testBackend to read modified blocks from DB.
func (b *borTestBackend) BlockByHash(ctx context.Context, hash common.Hash) (*types.Block, error) {
	if b.modifiedHashes != nil {
		if blockNum, ok := b.modifiedHashes[hash]; ok {
			header := b.chain.GetHeaderByNumber(blockNum)
			if header == nil {
				return nil, nil
			}
			body := rawdb.ReadBody(b.chaindb, header.Hash(), blockNum)
			if body == nil {
				return nil, nil
			}
			return types.NewBlockWithHeader(header).WithBody(*body), nil
		}
	}

	return b.chain.GetBlockByHash(hash), nil
}

// newBorChainConfig creates a chain config suitable for Bor state sync testing.
func newBorChainConfig() *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:             big.NewInt(137),
		HomesteadBlock:      big.NewInt(0),
		DAOForkBlock:        big.NewInt(0),
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
		Bor: &params.BorConfig{
			JaipurBlock:           big.NewInt(0),
			DelhiBlock:            big.NewInt(0),
			IndoreBlock:           big.NewInt(0),
			AhmedabadBlock:        big.NewInt(0),
			BhilaiBlock:           big.NewInt(0),
			RioBlock:              big.NewInt(0),
			MadhugiriBlock:        big.NewInt(0),
			MadhugiriProBlock:     big.NewInt(0),
			DandeliBlock:          big.NewInt(0),
			Period:                map[string]uint64{"0": 2},
			ProducerDelay:         map[string]uint64{"0": 2},
			Sprint:                map[string]uint64{"0": 16},
			BackupMultiplier:      map[string]uint64{"0": 2},
			ValidatorContract:     "0x0000000000000000000000000000000000001000",
			StateReceiverContract: "0x0000000000000000000000000000000000001001",
			BurntContract:         map[string]string{"0": "0x000000000000000000000000000000000000dead"},
			Coinbase:              map[string]string{"0": "0x0000000000000000000000000000000000000000"},
		},
	}
}

// newBorTestBackend creates a test backend with Bor chain config, ethash consensus
// (to avoid Bor-specific validation), and the given number of blocks.
func newBorTestBackend(t *testing.T, n int, gspec *core.Genesis, generator func(i int, b *core.BlockGen)) *borTestBackend {
	t.Helper()

	borCfg := newBorChainConfig()
	gspec.Config = borCfg

	backend := &borTestBackend{
		testBackend: testBackend{
			chainConfig: borCfg,
			engine:      ethash.NewFaker(),
			chaindb:     rawdb.NewMemoryDatabase(),
		},
		modifiedBlocks: make(map[uint64]bool),
		modifiedHashes: make(map[common.Hash]uint64),
	}

	_, blocks, _ := core.GenerateChainWithGenesis(gspec, backend.engine, n, generator)

	chain, err := core.NewBlockChain(backend.chaindb, gspec, backend.engine, &core.BlockChainConfig{
		TrieCleanLimit: 256,
		TrieDirtyLimit: 256,
		TrieTimeLimit:  5 * time.Minute,
		SnapshotLimit:  0,
		ArchiveMode:    true,
	})
	if err != nil {
		t.Fatalf("failed to create tester chain: %v", err)
	}

	if len(blocks) > 0 {
		if n, err := chain.InsertChain(blocks, false); err != nil {
			t.Fatalf("block %d: failed to insert into chain: %v", n, err)
		}
	}

	backend.chain = chain
	return backend
}

// injectStateSyncTx appends a state-sync transaction to the specified block's body
// and registers a canonical tx-lookup entry so RPC paths that resolve a tx by hash
// (e.g. TraceTransaction → GetCanonicalTransaction) can find it. This simulates
// post-Madhugiri blocks that have state-sync txs in their body.
func (b *borTestBackend) injectStateSyncTx(blockNum uint64, stateSyncTx *types.Transaction) error {
	block := b.chain.GetBlockByNumber(blockNum)
	if block == nil {
		return nil
	}

	existingBody := block.Body()
	newTxs := make([]*types.Transaction, len(existingBody.Transactions)+1)
	copy(newTxs, existingBody.Transactions)
	newTxs[len(newTxs)-1] = stateSyncTx

	rawdb.WriteBody(b.chaindb, block.Hash(), blockNum, &types.Body{
		Transactions: newTxs,
		Uncles:       existingBody.Uncles,
		Withdrawals:  existingBody.Withdrawals,
	})

	// In production, Bor.Finalize indexes the state-sync tx via WriteTxLookupEntries.
	// The test backend bypasses Finalize, so we mimic that side effect here.
	rawdb.WriteTxLookupEntries(b.chaindb, blockNum, []common.Hash{stateSyncTx.Hash()})

	b.modifiedBlocks[blockNum] = true
	b.modifiedHashes[block.Hash()] = blockNum
	return nil
}

// newStateSyncEvents creates numEvents state-sync events targeting targetAddr.
func newStateSyncEvents(numEvents int) []*types.StateSyncData {
	events := make([]*types.StateSyncData, numEvents)
	for i := range events {
		events[i] = &types.StateSyncData{
			ID:       uint64(i + 1),
			Contract: targetAddr,
			Data:     []byte(fmt.Sprintf("event-%d", i+1)),
			TxHash:   common.BigToHash(big.NewInt(int64(0xaaa0 + i + 1))),
		}
	}
	return events
}

// newStateSyncTestSetup creates a common test setup: a chain of numBlocks blocks (each with
// one normal tx), a state-sync tx with numEvents events injected into block 2, and the API.
func newStateSyncTestSetup(t *testing.T, numBlocks, numEvents int) (*borTestBackend, *API, uint64) {
	t.Helper()

	gspec := &core.Genesis{
		Alloc: types.GenesisAlloc{
			address:           {Balance: big.NewInt(params.Ether)},
			stateReceiverAddr: {Code: stateReceiverCode, Balance: big.NewInt(0)},
			targetAddr:        {Code: targetCode, Balance: big.NewInt(0)},
		},
	}

	backend := newBorTestBackend(t, numBlocks, gspec, func(i int, b *core.BlockGen) {
		tx, _ := types.SignTx(types.NewTx(&types.LegacyTx{
			Nonce:    uint64(i),
			To:       &address,
			Value:    big.NewInt(1000),
			Gas:      params.TxGas,
			GasPrice: new(big.Int).Mul(b.BaseFee(), big.NewInt(2)),
		}), b.Signer(), key)
		b.AddTx(tx)
	})

	stateSyncTx := types.NewTx(&types.StateSyncTx{
		StateSyncData: newStateSyncEvents(numEvents),
	})

	stateSyncBlock := uint64(2)
	err := backend.injectStateSyncTx(stateSyncBlock, stateSyncTx)
	require.NoError(t, err, "failed to inject state-sync tx")

	api := NewAPI(backend)
	return backend, api, stateSyncBlock
}

// callTracerConfig returns a TraceConfig that uses callTracer with log collection enabled.
func callTracerConfig() *TraceConfig {
	name := "callTracer"
	cfg := json.RawMessage(`{"withLog": true}`)
	return &TraceConfig{Tracer: &name, TracerConfig: cfg}
}

// validateStateSyncCallTrace unmarshals a callTracer result and validates the full trace
// structure for a state-sync transaction with expectedEvents bridge events.
func validateStateSyncCallTrace(t *testing.T, raw json.RawMessage, expectedEvents int) {
	t.Helper()

	var trace callTraceFrame
	require.NoError(t, json.Unmarshal(raw, &trace), "failed to unmarshal call trace")

	// Synthetic root frame: CALL from BorSystemAddress to StateReceiverContract.
	require.Equal(t, "CALL", trace.Type, "invalid root frame type (expected CALL)")
	require.Equal(t, params.BorSystemAddress, trace.From, "invalid root frame from (expected BorSystemAddress)")
	require.Equal(t, stateReceiverAddr, trace.To, "invalid root frame to (expected StateReceiverContract)")

	// One sub-call per state-sync event (each is a commitState call).
	require.Equal(t, expectedEvents, len(trace.Calls),
		"expected %d sub-calls (one per state-sync event), got %d", expectedEvents, len(trace.Calls))

	for i, call := range trace.Calls {
		// Each commitState: CALL from BorSystemAddress to StateReceiverContract.
		require.Equal(t, "CALL", call.Type, "invalid sub-call[%d] type (expected CALL)", i)
		require.Equal(t, params.BorSystemAddress, call.From, "invalid sub-call[%d] from (expected BorSystemAddress)", i)
		require.Equal(t, stateReceiverAddr, call.To, "invalid sub-call[%d] to (expected StateReceiverContract)", i)

		// Inside commitState, StateReceiver forwards to targetAddr via low-level call.
		// Find the nested call to targetAddr and validate it emitted LOG0.
		target := findCallTo(call.Calls, targetAddr)
		require.NotNil(t, target, "sub-call[%d]: expected nested call to target %s", i, targetAddr)
		require.NotEmpty(t, target.Logs, "sub-call[%d]: target call should have logs", i)
		require.Equal(t, targetAddr, target.Logs[0].Address, "sub-call[%d]: log address", i)
		require.Empty(t, target.Logs[0].Topics, "sub-call[%d]: log should have no topics (LOG0)", i)
		require.Empty(t, target.Logs[0].Data, "sub-call[%d]: log should have empty data", i)
	}
}

// findCallTo recursively searches call frames for a CALL to the given address.
func findCallTo(calls []callTraceFrame, addr common.Address) *callTraceFrame {
	for i, call := range calls {
		if call.To == addr {
			return &calls[i]
		}
		if found := findCallTo(call.Calls, addr); found != nil {
			return found
		}
	}
	return nil
}

// TestTraceBlockByNumber_WithStateSyncTx tests end-to-end state-sync tracing using the actual
// StateReceiver contract bytecode and mirrors what happens in actual networks. During the test
// we do the following things:
//
//  1. Deploy StateReceiver at 0x1001 with mainnet bytecode (which has `stateReceive` method).
//  2. A simple "target" contract is deployed at a separate address — it just emits LOG0 on any call.
//  3. A StateSyncTx carries bridge events whose Contract field points to the target is injected.
//  4. callTracer with logs is used to generate a trace in which the bridge events are executed inside
//     EVM calling the StateReceiver contract.
//  5. Verify if the output trace matches the expected output of call trace. While tracing state-sync
//     transactions, a synthetic root call frame is injected under which all bridge events are
//     executed as sub-calls. This is needed to satisfy callTracer's invariant of having a single
//     root frame per transaction.
func TestTraceBlockByNumber_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		numEvents int
	}{
		{"single event", 1},
		{"multiple events", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, tt.numEvents)
			defer backend.chain.Stop()

			block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
			require.NotNil(t, block)
			txs := block.Transactions()
			require.Equal(t, 2, len(txs), "expected 2 transactions (1 normal + 1 state-sync)")

			results, err := api.TraceBlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock), callTracerConfig())
			require.NoError(t, err)
			require.Equal(t, len(txs), len(results))
			for i, result := range results {
				require.Empty(t, result.Error, "trace result[%d] error", i)
				require.Equal(t, txs[i].Hash(), result.TxHash, "trace result[%d] tx hash", i)
			}

			raw, ok := results[1].Result.(json.RawMessage)
			require.True(t, ok, "expected json.RawMessage, got %T", results[1].Result)
			validateStateSyncCallTrace(t, raw, tt.numEvents)
		})
	}
}

// TestTraceBlockByHash_WithStateSyncTx tests end-to-end state-sync tracing using the actual
// StateReceiver contract bytecode and mirrors what happens in actual networks. Follows same
// steps as trace by block.
func TestTraceBlockByHash_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	numOfStateSyncEvents := 2
	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, numOfStateSyncEvents)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs))

	results, err := api.TraceBlockByHash(context.Background(), block.Hash(), callTracerConfig())
	require.NoError(t, err)
	require.Equal(t, len(txs), len(results))
	for i, result := range results {
		require.Empty(t, result.Error, "trace result[%d] error", i)
		require.Equal(t, txs[i].Hash(), result.TxHash, "trace result[%d] tx hash", i)
	}

	raw, ok := results[1].Result.(json.RawMessage)
	require.True(t, ok, "expected json.RawMessage, got %T", results[1].Result)
	validateStateSyncCallTrace(t, raw, numOfStateSyncEvents)
}

// TestTraceBlockByHash_WithStateSyncTx tests end-to-end state-sync tracing using the actual
// StateReceiver contract bytecode and mirrors what happens in actual networks. Follows same
// steps as trace by block but for a range of blocks.
func TestTraceChain_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	numOfStateSyncEvents := 2
	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, numOfStateSyncEvents)
	defer backend.chain.Stop()

	from, err := backend.BlockByNumber(context.Background(), rpc.BlockNumber(0))
	require.NoError(t, err)
	to, err := backend.BlockByNumber(context.Background(), rpc.BlockNumber(3))
	require.NoError(t, err)

	// traceChain traces the range (from, to] — excludes start, includes end.
	results := api.traceChain(from, to, callTracerConfig(), nil)
	require.NotNil(t, results)

	for res := range results {
		block, err := backend.BlockByNumber(context.Background(), rpc.BlockNumber(uint64(res.Block)))
		require.NoError(t, err)
		txs := block.Transactions()
		require.Equal(t, len(txs), len(res.Traces), "block %d trace count", res.Block)

		for i, result := range res.Traces {
			require.Empty(t, result.Error, "block %d trace[%d] error", res.Block, i)
			require.Equal(t, txs[i].Hash(), result.TxHash, "block %d trace[%d] tx hash", res.Block, i)
		}

		if res.Block == hexutil.Uint64(stateSyncBlock) {
			raw, ok := res.Traces[1].Result.(json.RawMessage)
			require.True(t, ok, "expected json.RawMessage, got %T", res.Traces[1].Result)
			validateStateSyncCallTrace(t, raw, numOfStateSyncEvents)
		}
	}
}

// TestIntermediateRoots_WithStateSyncTx verifies that intermediate state roots are correctly
// computed for blocks containing state-sync transactions. This test doesn't use a tracer. It
// validates execution correctness rather than trace output.
func TestIntermediateRoots_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs))

	results, err := api.IntermediateRoots(context.Background(), block.Hash(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, len(results), "expected 2 intermediate roots (one per tx)")

	expectedStateRoots := []common.Hash{
		common.HexToHash("0x23eda0b1dbe747a8daedaf94b811a393de400047812394476dac190a5e9a8fd4"),
		common.HexToHash("0x1b5bcf33b31f2d38b498594a348bc176b9e05b46cba3ed3701ba739c012bc757"),
	}
	for i, result := range results {
		require.Equal(t, expectedStateRoots[i], result, "state root mismatch at index %d", i)
	}
}

// TestIntermediateRoots_WithNonNilConfig exercises the non-nil-config branch
// Passing a non-nil config must not change the result vs the default
// (no config). Covers the trivial-but-untouched config-handling path.
func TestIntermediateRoots_WithNonNilConfig(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)

	withConfig, err := api.IntermediateRoots(context.Background(), block.Hash(), &TraceConfig{})
	require.NoError(t, err)

	withoutConfig, err := api.IntermediateRoots(context.Background(), block.Hash(), nil)
	require.NoError(t, err)

	require.Equal(t, withoutConfig, withConfig, "reexec override should not change roots for an already-archived chain")
}

// TestIntermediateRoots_ContextCancelled exercises the in-loop `ctx.Err()` check at
// A pre-cancelled context must abort before any tx is processed and surface the
// cancellation as an error.
func TestIntermediateRoots_ContextCancelled(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := api.IntermediateRoots(ctx, block.Hash(), nil)
	require.ErrorIs(t, err, context.Canceled)
}

// TestIntermediateRoots_FailingTx_ReturnsPartialRoots exercises the documented bad-block
// behaviour: if a tx in the block fails to execute, IntermediateRoots MUST return the
// partial root list collected so far with err=nil (rather than failing the whole RPC call).
// This is intentional because the function is also called on bad blocks where the caller
// wants the roots that led up to the failure.
//
// Body layout: [normal_tx, bad_tx]
//   - normal_tx is the standard funded transfer set up by newStateSyncTestSetup. It
//     succeeds and contributes one intermediate root.
//   - bad_tx is a signed tx with a nonce far above the sender's actual nonce, triggering
//     ErrNonceTooHigh in core.ApplyMessage.
//
// Expected: roots == [root_after_normal_tx], err == nil. The first root is hardcoded
// against the canonical post-normal-tx state root pinned by TestIntermediateRoots_WithStateSyncTx
// — identical setup, so the first root must match byte-for-byte (regression guard).
func TestIntermediateRoots_FailingTx_ReturnsPartialRoots(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	block := backend.chain.GetBlockByNumber(stateSyncBlock)
	require.NotNil(t, block)

	// Construct a tx that fails at ApplyMessage time: nonce way above the sender's
	// actual one → ErrNonceTooHigh.
	signer := types.MakeSigner(backend.chainConfig, block.Number(), block.Time())
	badTx, err := types.SignTx(types.NewTx(&types.LegacyTx{
		Nonce:    999, // sender's real nonce is small
		To:       &address,
		Value:    big.NewInt(1),
		Gas:      params.TxGas,
		GasPrice: new(big.Int).Mul(block.BaseFee(), big.NewInt(2)),
	}), signer, key)
	require.NoError(t, err)

	// Append the failing tx AFTER the normal tx so the loop processes normal_tx
	// successfully (appending one root) before hitting bad_tx and bailing.
	existing := block.Body()
	newTxs := append(existing.Transactions, badTx)
	rawdb.WriteBody(backend.chaindb, block.Hash(), stateSyncBlock, &types.Body{
		Transactions: newTxs,
		Uncles:       existing.Uncles,
		Withdrawals:  existing.Withdrawals,
	})
	rawdb.WriteTxLookupEntries(backend.chaindb, stateSyncBlock, []common.Hash{badTx.Hash()})
	backend.modifiedBlocks[stateSyncBlock] = true
	backend.modifiedHashes[block.Hash()] = stateSyncBlock

	roots, err := api.IntermediateRoots(context.Background(), block.Hash(), nil)
	require.NoError(t, err, "IntermediateRoots must not error on a failing tx — it returns partial roots")
	require.Equal(t, 1, len(roots), "exactly one root expected: normal_tx succeeded, bad_tx aborted the loop")
	require.Equal(t,
		common.HexToHash("0x23eda0b1dbe747a8daedaf94b811a393de400047812394476dac190a5e9a8fd4"),
		roots[0],
		"partial root regression check — must match the canonical post-normal-tx state root")
}

// TestTraceTransaction_WithStateSyncTx exercises the `debug_traceTransaction` RPC entry
// point against the state-sync tx hash directly. This path differs from TraceBlockBy*
// because it (a) routes through GetCanonicalTransaction → StateAtTransaction and
// (b) hits the synthetic-root construction inside traceTx with a real canonical-tx
// lookup. Verifies the full state-sync wrapper from the most user-facing entry point.
func TestTraceTransaction_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	numEvents := 2
	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, numEvents)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs))

	stateSyncTxHash := txs[1].Hash()
	result, err := api.TraceTransaction(context.Background(), stateSyncTxHash, callTracerConfig())
	require.NoError(t, err)

	raw, ok := result.(json.RawMessage)
	require.True(t, ok, "expected json.RawMessage, got %T", result)
	validateStateSyncCallTrace(t, raw, numEvents)
}

// TestTraceTransaction_StateSyncTx_NotIndexed_ReturnsErrTxNotFound is the negative
// counterpart: if a state-sync tx has not been written to the canonical lookup index
// (e.g. txindexer behind), the RPC must surface errTxNotFound rather than returning
// stale or undefined data.
func TestTraceTransaction_StateSyncTx_NotIndexed_ReturnsErrTxNotFound(t *testing.T) {
	t.Parallel()

	backend, api, _ := newStateSyncTestSetup(t, 3, 1)
	defer backend.chain.Stop()

	// A hash that nothing in the chain knows about.
	unknown := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	_, err := api.TraceTransaction(context.Background(), unknown, callTracerConfig())
	require.ErrorIs(t, err, errTxNotFound)
}

// TestTraceBlock_RLP_WithStateSyncTx covers the third block-level entry point —
// `debug_traceBlock(rlpEncodedBlock, config)` — which decodes the block from RLP
// before tracing. This validates that the state-sync handling holds when the block
// arrives via the RLP-blob path rather than canonical-store lookup.
func TestTraceBlock_RLP_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	numEvents := 2
	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, numEvents)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs))

	rlpBytes, err := rlp.EncodeToBytes(block)
	require.NoError(t, err)

	results, err := api.TraceBlock(context.Background(), rlpBytes, callTracerConfig())
	require.NoError(t, err)
	require.Equal(t, len(txs), len(results))
	for i, r := range results {
		require.Empty(t, r.Error, "trace result[%d] error", i)
		require.Equal(t, txs[i].Hash(), r.TxHash, "trace result[%d] tx hash", i)
	}

	raw, ok := results[1].Result.(json.RawMessage)
	require.True(t, ok, "expected json.RawMessage, got %T", results[1].Result)
	validateStateSyncCallTrace(t, raw, numEvents)
}

// TestStandardTraceBlockToFile_WithStateSyncTx covers `debug_standardTraceBlockToFile`,
// which writes structLogger output to per-tx temp files. It validates that the hooks
// behave as expected for state-sync transactions and avoid regressions.
//   - The function must produce one dump file per traced tx.
//   - Each dump must be non-empty and valid newline-delimited JSON.
//   - The state-sync tx's dump must contain at least one LOG opcode (emitted by the
//     synthetic target the state receiver forwards to).
//
// We intentionally don't pin exact opcodes — the standard logger output is verbose
// and brittle to gas changes. Structural checks catch the regressions that matter.
func TestStandardTraceBlockToFile_WithStateSyncTx(t *testing.T) {
	t.Parallel()

	numEvents := 2
	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, numEvents)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)

	dumps, err := api.StandardTraceBlockToFile(context.Background(), block.Hash(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, len(dumps), "expected one dump per tx (regular + state-sync)")

	defer func() {
		for _, d := range dumps {
			_ = os.Remove(d)
		}
	}()

	for i, path := range dumps {
		content, err := os.ReadFile(path)
		require.NoError(t, err, "reading dump %d", i)
		require.NotEmpty(t, content, "dump %d is empty", i)

		// Each non-empty line must be valid JSON — the struct logger writes one
		// frame per opcode plus a final summary.
		lines := bytes.Split(content, []byte{'\n'})
		nonEmpty := 0
		for _, line := range lines {
			if len(line) == 0 {
				continue
			}
			var anyJSON map[string]any
			require.NoError(t, json.Unmarshal(line, &anyJSON), "dump %d line is not JSON: %s", i, line)
			nonEmpty++
		}
		require.Greater(t, nonEmpty, 0, "dump %d had no JSON lines", i)
	}

	// State-sync dump (index 1) must show at least one LOG opcode — the bridge events
	// invoke the target contract which emits LOG0 on every call.
	stateSyncDump, err := os.ReadFile(dumps[1])
	require.NoError(t, err)
	require.Contains(t, string(stateSyncDump), `"opName":"LOG0"`,
		"state-sync dump should contain LOG0 opcode from target contract")
}

// TestStandardTraceBlockToFile_StateSyncTx_HashFilter exercises the `TxHash` config
// option that restricts standard tracing to a single tx. When pointed at the state-sync
// tx, only one dump file must be produced — confirming the per-tx filter respects the
// state-sync branch added by this PR (without the filter, both txs would be traced).
func TestStandardTraceBlockToFile_StateSyncTx_HashFilter(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 1)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs))

	stateSyncTx := txs[1]
	dumps, err := api.StandardTraceBlockToFile(context.Background(), block.Hash(), &StdTraceConfig{TxHash: stateSyncTx.Hash()})
	require.NoError(t, err)
	require.Equal(t, 1, len(dumps), "TxHash filter should produce a single dump")

	defer func() { _ = os.Remove(dumps[0]) }()

	content, err := os.ReadFile(dumps[0])
	require.NoError(t, err)
	require.Contains(t, string(content), `"opName":"LOG0"`,
		"the single dump should be the state-sync trace (LOG0 from target)")
}

// TestTraceBlockByNumber_StateSyncTx_EmptyEvents is the boundary case for
// ApplyStateSyncEvents' early-return branch: a state-sync tx with zero events must
// still produce a valid trace result (synthetic root frame with no sub-calls), not
// an error and not a missing entry in the results array.
func TestTraceBlockByNumber_StateSyncTx_EmptyEvents(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 0)
	defer backend.chain.Stop()

	results, err := api.TraceBlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock), callTracerConfig())
	require.NoError(t, err)
	require.Equal(t, 2, len(results), "results array still contains an entry for the empty state-sync tx")
	require.Empty(t, results[1].Error)

	raw, ok := results[1].Result.(json.RawMessage)
	require.True(t, ok)

	var trace callTraceFrame
	require.NoError(t, json.Unmarshal(raw, &trace))
	require.Equal(t, "CALL", trace.Type, "synthetic root frame still emitted")
	require.Equal(t, params.BorSystemAddress, trace.From)
	require.Empty(t, trace.Calls, "no bridge-event sub-calls when events is empty")
}

// TestIntermediateRoots_Genesis_Rejected validates the explicit guard:
//
//	if block.NumberU64() == 0 { return nil, errors.New("genesis is not traceable") }
//
// IntermediateRoots shares this guard with TraceBlockByNumber; the state-sync code
// path added by this PR sits inside the same function so it's worth pinning the
// pre-condition explicitly for IntermediateRoots too.
func TestIntermediateRoots_Genesis_Rejected(t *testing.T) {
	t.Parallel()

	backend, api, _ := newStateSyncTestSetup(t, 3, 1)
	defer backend.chain.Stop()

	genesis, err := backend.BlockByNumber(context.Background(), rpc.BlockNumber(0))
	require.NoError(t, err)

	_, err = api.IntermediateRoots(context.Background(), genesis.Hash(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "genesis is not traceable")
}

// TestIntermediateRoots_UnknownBlock surfaces the "block not found" path. The block
// resolution happens before the state-sync branch but is on the same RPC surface,
// so an explicit test prevents accidental swallowing of the error.
func TestIntermediateRoots_UnknownBlock(t *testing.T) {
	t.Parallel()

	backend, api, _ := newStateSyncTestSetup(t, 3, 1)
	defer backend.chain.Stop()

	_, err := api.IntermediateRoots(context.Background(), common.HexToHash("0xcafebabe"), nil)
	require.Error(t, err, "unknown block hash must error")
}

// TestTraceBlockByHash_UnknownBlock confirms that the same not-found semantics hold
// at the by-hash entry point. This is the broadest user-facing surface; surfacing
// nil block as an error rather than nil result is part of the RPC contract.
func TestTraceBlockByHash_UnknownBlock(t *testing.T) {
	t.Parallel()

	backend, api, _ := newStateSyncTestSetup(t, 3, 1)
	defer backend.chain.Stop()

	_, err := api.TraceBlockByHash(context.Background(), common.HexToHash("0xfeedface"), nil)
	require.Error(t, err)
}

// TestTraceBlockByNumber_Genesis_Rejected confirms the genesis guard is enforced at
// the by-number entry point too — even when the rest of the chain has state-sync
// activity, the genesis block itself must remain untraceable.
func TestTraceBlockByNumber_Genesis_Rejected(t *testing.T) {
	t.Parallel()

	backend, api, _ := newStateSyncTestSetup(t, 3, 1)
	defer backend.chain.Stop()

	_, err := api.TraceBlockByNumber(context.Background(), rpc.BlockNumber(0), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "genesis is not traceable")
}

// TestTraceTransaction_StateSync_PrestateTracer runs the prestate tracer for a state-sync
// transaction. Because the hooks are wrapped to handle state-sync transactions, the tracer
// should work without any issues (errors or panic). It's a regression tests for a panic
// observed while tracing a state-sync transaction where the actual sender and receiver
// addresses were not populated in the lookup leading to error while looking up storage.
func TestTraceTransaction_StateSync_PrestateTracer(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs))
	stateSyncTxHash := txs[1].Hash()

	// Run both modes (default and diffMode) — both share the lookupStorage path.
	cases := []struct {
		name   string
		config *TraceConfig
	}{
		{"default", prestateTracerConfig(nil)},
		{"diffMode", prestateTracerConfig(json.RawMessage(`{"diffMode": true}`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := api.TraceTransaction(context.Background(), stateSyncTxHash, tc.config)
			require.NoError(t, err, "prestateTracer should not panic or error on state-sync tx")

			raw, ok := result.(json.RawMessage)
			require.True(t, ok, "expected json.RawMessage, got %T", result)
			require.NotEmpty(t, raw, "prestate output must not be empty")

			// The state receiver contract must appear in the prestate — verifying that
			// OnEnter populated it correctly via the synthetic root frame.
			var anyJSON map[string]any
			require.NoError(t, json.Unmarshal(raw, &anyJSON))

			var stateMap map[string]any
			if tc.config != nil && tc.config.TracerConfig != nil {
				// diffMode response shape: {"pre": {...}, "post": {...}}
				pre, ok := anyJSON["pre"].(map[string]any)
				require.True(t, ok, "diffMode response should contain a 'pre' object")
				stateMap = pre
			} else {
				stateMap = anyJSON
			}
			// stateReceiverAddr is rendered with the EIP-55 mixed-case checksum in JSON.
			require.Contains(t, stateMap, stateReceiverAddr.Hex(),
				"prestate must contain the state receiver contract address")
		})
	}
}

// TestTraceCallMany_StateSyncBlock_BaseState verifies that TraceCallMany builds its
// base state from a block whose trailing transaction is a state-sync tx without ever
// applying that synthetic tx as a normal message. Because the state-sync tx is always
// last, an in-range transactionIndex replays only the normal txs, and the full-block
// case uses the committed post-block state — neither path executes the state-sync tx.
func TestTraceCallMany_StateSyncBlock_BaseState(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	block, _ := backend.BlockByNumber(context.Background(), rpc.BlockNumber(stateSyncBlock))
	require.NotNil(t, block)
	txs := block.Transactions()
	require.Equal(t, 2, len(txs), "expected 1 normal tx + 1 trailing state-sync tx")
	stateSyncIdx := len(txs) - 1 // index of the trailing state-sync tx
	pastEnd := len(txs)

	to := targetAddr
	bundles := []Bundle{{Transactions: []ethapi.TransactionArgs{{
		From:  &address,
		To:    &to,
		Value: (*hexutil.Big)(big.NewInt(1)),
	}}}}

	byNumber := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(stateSyncBlock))
	cases := []struct {
		name string
		sc   StateContext
	}{
		{"full post-block state by hash", StateContext{BlockNumber: rpc.BlockNumberOrHashWithHash(block.Hash(), false)}},
		{"index at state-sync tx replays normal txs only", StateContext{BlockNumber: byNumber, TransactionIndex: &stateSyncIdx}},
		{"index past all txs falls back to post-block state", StateContext{BlockNumber: byNumber, TransactionIndex: &pastEnd}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := api.TraceCallMany(context.Background(), bundles, tc.sc, nil)
			require.NoError(t, err, "base-state setup must not apply the state-sync tx as a normal message")
			require.Len(t, res, 1)
			require.Len(t, res[0], 1)

			raw, ok := res[0][0].(json.RawMessage)
			require.True(t, ok, "expected json.RawMessage, got %T", res[0][0])
			var out logger.ExecutionResult
			require.NoError(t, json.Unmarshal(raw, &out))
			require.False(t, out.Failed, "traced call should succeed")
		})
	}
}

// prestateTracerConfig builds a TraceConfig for the prestateTracer with the given
// JSON config blob (or nil for the default config).
func prestateTracerConfig(cfg json.RawMessage) *TraceConfig {
	name := "prestateTracer"
	return &TraceConfig{Tracer: &name, TracerConfig: cfg}
}

// TestTraceBlockByNumber_StateSync_JSTracer exercises the `traceBlockParallel` code
// path at. That path only fires when the configured tracer is JS (gated by
// `DefaultDirectory.IsJS`). The native-tracer matrix test above does NOT reach
// this path because every native tracer routes through the sequential `traceBlock`.
//
// The JS source below is a minimal valid tracer object: it provides the three required
// methods (step / fault / result) but does no real work. The point is structural — we
// want to drive the parallel-trace orchestration AND its post-loop state-sync sequential
// replay block end-to-end, and assert no panic / no per-tx error.
func TestTraceBlockByNumber_StateSync_JSTracer(t *testing.T) {
	t.Parallel()

	backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
	defer backend.chain.Stop()

	// Minimal JS tracer literal: returns the constant "ok" for every tx so the test
	// doesn't assert on tracer output structure (which is tracer-author concern), only
	// on the orchestration path firing without panic / error.
	jsTracer := `{
		step: function(log, db) {},
		fault: function(log, db) {},
		result: function(ctx, db) { return "ok"; }
	}`

	results, err := api.TraceBlockByNumber(context.Background(),
		rpc.BlockNumber(stateSyncBlock),
		&TraceConfig{Tracer: &jsTracer})
	require.NoError(t, err, "TraceBlockByNumber with JS tracer must not error")
	require.Equal(t, 2, len(results), "expected 2 trace results (regular tx + state-sync tx)")

	for i, r := range results {
		require.Empty(t, r.Error, "trace[%d] returned per-tx error %q", i, r.Error)
		require.NotNil(t, r.Result, "trace[%d] result is nil", i)
	}
}

// TestTraceBlockByNumber_StateSync_AllRegisteredTracers is a multi-tracer matrix test
// which runs all registered tracers against a block with state-sync transaction. The main
// goal is to catch issues or regressions in tracers that were not previously tested with
// state-sync transactions and the new wrapped hooks which wraps all existing tracers
// and introduces some additional logic affecting the tracing lifecycle.
//
// This test enumerates registered tracer names and runs TraceBlockByNumber for each.
// It does NOT validate output structure — that's tracer-specific. It only confirms
// the absence of panics, non-nil errors, and per-tx error strings, which is enough
// to catch a particular class of bugs.
func TestTraceBlockByNumber_StateSync_AllRegisteredTracers(t *testing.T) {
	t.Parallel()

	// Per-tracer configs. nil means "no TracerConfig field needed".
	// muxTracer requires a config map of inner tracer names → their configs.
	tracerConfigs := map[string]json.RawMessage{
		"4byteTracer":             nil,
		"callTracer":              nil,
		"flatCallTracer":          nil,
		"erc7562Tracer":           nil,
		"keccak256PreimageTracer": nil,
		"noopTracer":              nil,
		"prestateTracer":          nil,
		"muxTracer":               json.RawMessage(`{"callTracer": {}, "prestateTracer": {}, "noopTracer": {}}`),
	}

	for name, cfg := range tracerConfigs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			backend, api, stateSyncBlock := newStateSyncTestSetup(t, 3, 2)
			defer backend.chain.Stop()

			tracerName := name
			results, err := api.TraceBlockByNumber(context.Background(),
				rpc.BlockNumber(stateSyncBlock),
				&TraceConfig{Tracer: &tracerName, TracerConfig: cfg})
			require.NoError(t, err, "%s: TraceBlockByNumber returned an error", name)
			require.Equal(t, 2, len(results), "%s: expected 2 traces (regular + state-sync)", name)

			for i, r := range results {
				require.Empty(t, r.Error, "%s: trace[%d] returned per-tx error %q", name, i, r.Error)
				// Tracers that always produce JSON output (everything except noop) should
				// return a non-nil result. noopTracer returns an empty JSON object — also
				// acceptable.
				require.NotNil(t, r.Result, "%s: trace[%d] result is nil", name, i)
			}
		})
	}
}
