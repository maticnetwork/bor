package bor

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests/bor/mocks"
)

// SetCoinbase sets the coinbase of the generated block.
// It can be called at most once.
func (b *BlockGen) SetCoinbase(addr common.Address) {
	if b.gasPool != nil {
		if len(b.txs) > 0 {
			panic("coinbase must be set before adding transactions")
		}
		panic("coinbase can only be set once")
	}
	b.header.Coinbase = addr
	b.gasPool = new(core.GasPool).AddGas(b.header.GasLimit)
}

// AddTxWithChain adds a transaction to the generated block. If no coinbase has
// been set, the block's coinbase is set to the zero address.
//
// AddTxWithChain panics if the transaction cannot be executed. In addition to
// the protocol-imposed limitations (gas limit, etc.), there are some
// further limitations on the content of transactions that can be
// added. If contract code relies on the BLOCKHASH instruction,
// the block in chain will be returned.
// This method is called by AddTx(tx) => AddTxWithChain(nil, tx)
func (b *BlockGen) AddTxWithChain(bc *core.BlockChain, tx *types.Transaction) {
	if b.gasPool == nil {
		b.SetCoinbase(common.Address{})
	}
	b.statedb.Prepare(tx.Hash(), len(b.txs))
	receipt, err := core.ApplyTransaction(b.config, bc, &b.header.Coinbase, b.gasPool, b.statedb, b.header, tx, &b.header.GasUsed, vm.Config{})
	if err != nil {
		panic(err)
	}

	b.txs = append(b.txs, tx)
	b.receipts = append(b.receipts, receipt)

}
func TestGetTransactionReceiptsByBlock(t *testing.T) {

	var (
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	)

	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	db := init.ethereum.ChainDb()
	chain := init.ethereum.BlockChain()
	fmt.Printf("VMCONFIG API TEST :: %+v", chain.GetVMConfig())
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)

	aa := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	signer := types.LatestSigner(chain.Config())

	// Mock /bor/span/1
	res, _ := loadSpanFromFile(t)
	h := &mocks.IHeimdallClient{}
	h.On("FetchWithRetry", spanPath, "").Return(res, nil)

	// Mock State Sync events
	// at # sprintSize, events are fetched for [fromID, (block-sprint).Time)
	fromID := uint64(1)
	to := int64(chain.GetHeaderByNumber(0).Time)
	sample := getSampleEventRecord(t)

	eventRecords := []*bor.EventRecordWithTime{
		buildStateEvent(sample, 1, 3), // id = 1, time = 3
		buildStateEvent(sample, 2, 1), // id = 2, time = 1
		buildStateEvent(sample, 3, 2), // id = 3, time = 2
	}

	h.On("FetchStateSyncEvents", fromID, to).Return(eventRecords, nil)
	_bor.SetHeimdallClient(h)

	// Insert blocks for 0th sprint

	block := init.genesis.ToBlock(db)

	for i := uint64(1); i <= sprintSize; i++ {
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, func(j int, b *BlockGen) {
			txdata := &types.LegacyTx{
				Nonce:    0,
				To:       &aa,
				Gas:      30000,
				GasPrice: newGwei(5),
			}
			tx := types.NewTx(txdata)
			tx, _ = types.SignTx(tx, signer, key1)

			b.AddTxWithChain(nil, tx)
		})
		insertNewBlock(t, chain, block)

	}

	// lastStateID, _ := _bor.GenesisContractsClient.LastStateId(sprintSize)
	// fmt.Println("lastStateID", lastStateID)

	// blocks, _ := core.GenerateChain(params.TestChainConfig, block, engine, db, 1, func(i int, b *core.BlockGen) {

	// 	txdata := &types.LegacyTx{
	// 		Nonce:    0,
	// 		To:       &aa,
	// 		Gas:      30000,
	// 		GasPrice: newGwei(5),
	// 	}
	// 	tx := types.NewTx(txdata)
	// 	tx, _ = types.SignTx(tx, signer, key1)

	// 	b.AddTx(tx)
	// })

	// chain.InsertChain(blocks)

	rpcNumber := rpc.BlockNumberOrHashWithNumber(3)
	ans, err := _bor.EthAPI().GetTransactionReceiptsByBlock(context.Background(), rpcNumber)
	if err != nil {
		t.Fatal(err)
	}

	fmt.Printf("\n ANS ::: %v", ans)

}

func TestGetTransactionReceiptsByBlock2(t *testing.T) {
	var (
		db      = rawdb.NewMemoryDatabase()
		key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr1   = crypto.PubkeyToAddress(key1.PublicKey)
		gspec   = &core.Genesis{
			Config: params.TestChainConfig,
			Alloc:  core.GenesisAlloc{addr1: {Balance: big.NewInt(10000000000000000)}},
		}
		genesis = gspec.MustCommit(db)
		signer  = types.LatestSigner(gspec.Config)
	)

	node, err := node.New(&node.Config{})
	if err != nil {
		t.Fatal(err)
	}
	eth, err := eth.New(node, &ethconfig.Config{})
	if err != nil {
		t.Fatal(err)
	}

	blockchainAPI := ethapi.NewPublicBlockChainAPI(eth.APIBackend)
	// engine := ethash.NewFaker()
	engine := bor.New(params.TestChainConfig, db, blockchainAPI, "", false)
	blockchain, _ := core.NewBlockChain(db, nil, gspec.Config, engine, vm.Config{}, nil, nil)
	defer blockchain.Stop()

	var _bor consensus.Engine = engine
	bor2 := _bor.(*bor.Bor)

	// Mock /bor/span/1
	res, _ := loadSpanFromFile(t)
	h := &mocks.IHeimdallClient{}
	h.On("FetchWithRetry", spanPath, "").Return(res, nil)

	fromID := uint64(1)
	to := int64(blockchain.GetHeaderByNumber(0).Time)
	sample := getSampleEventRecord(t)

	eventRecords := []*bor.EventRecordWithTime{
		buildStateEvent(sample, 1, 3), // id = 1, time = 3
		buildStateEvent(sample, 2, 1), // id = 2, time = 1
		buildStateEvent(sample, 3, 2), // id = 3, time = 2
	}

	h.On("FetchStateSyncEvents", fromID, to).Return(eventRecords, nil)
	bor2.SetHeimdallClient(h)

	chain, _ := core.GenerateChain(gspec.Config, genesis, engine, db, 5, func(i int, gen *core.BlockGen) {
		tx, err := types.SignTx(types.NewContractCreation(gen.TxNonce(addr1), new(big.Int), 1000000, gen.BaseFee(), nil), signer, key1)
		if i == 2 {
			gen.OffsetTime(-9)
		}
		if err != nil {
			t.Fatalf("failed to create tx: %v", err)
		}

		tx2, err := types.SignTx(types.NewContractCreation(gen.TxNonce(addr1)+1, new(big.Int), 1000000, gen.BaseFee(), nil), signer, key1)

		if err != nil {
			t.Fatalf("failed to create tx: %v", err)
		}

		gen.AddTx(tx)
		gen.AddTx(tx2)
	})
	if _, err := blockchain.InsertChain(chain); err != nil {
		t.Fatalf("failed to insert chain: %v", err)
	}
}
