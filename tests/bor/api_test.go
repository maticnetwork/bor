package bor

import (
	"context"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests/bor/mocks"
)

func TestGetTransactionReceiptsByBlock(t *testing.T) {

	// var (
	// 	key1, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	// )

	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	db := init.ethereum.ChainDb()
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)

	// aa := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	// signer := types.LatestSigner(chain.Config())

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
		buildStateEvent(sample, 1, 3), // id = 1, time = 1
		buildStateEvent(sample, 2, 1), // id = 2, time = 3
		buildStateEvent(sample, 3, 2), // id = 3, time = 2

	}
	h.On("FetchStateSyncEvents", fromID, to).Return(eventRecords, nil)
	_bor.SetHeimdallClient(h)

	// Insert blocks for 0th sprint

	block := init.genesis.ToBlock(db)

	for i := uint64(1); i <= 4; i++ {
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor)
		insertNewBlock(t, chain, block)
	}

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
