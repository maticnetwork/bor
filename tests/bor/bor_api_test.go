//go:build integration
// +build integration

package bor

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/bor/clerk"
	"github.com/ethereum/go-ethereum/consensus/bor/valset"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/tests/bor/mocks"
)

func TestGetTransactionReceiptsByBlock(t *testing.T) {
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()
	_bor := engine.(*bor.Bor)

	// Mock /bor/span/1
	res, _ := loadSpanFromFile(t)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	h := mocks.NewMockIHeimdallClient(ctrl)
	h.EXPECT().Span(uint64(1)).Return(&res.Result, nil).AnyTimes()

	// Mock State Sync events
	// at # sprintSize, events are fetched for [fromID, (block-sprint).Time)
	fromID := uint64(1)
	to := int64(chain.GetHeaderByNumber(0).Time)
	sample := getSampleEventRecord(t)

	// First query will be from [id=1, (block-sprint).Time]
	// Insert 5 events in this time range
	eventRecords := []*clerk.EventRecordWithTime{
		buildStateEvent(sample, 1, 3), // id = 1, time = 1
		buildStateEvent(sample, 2, 1), // id = 2, time = 3
		buildStateEvent(sample, 3, 2), // id = 3, time = 2
		// event with id 5 is missing
		buildStateEvent(sample, 4, 5), // id = 4, time = 5
		buildStateEvent(sample, 6, 4), // id = 6, time = 4
	}

	h.EXPECT().StateSyncEvents(fromID, to).Return(eventRecords, nil).AnyTimes()
	_bor.SetHeimdallClient(h)

	// Insert blocks for 0th sprint
	db := init.ethereum.ChainDb()
	block := init.genesis.ToBlock(db)

	signer := types.LatestSigner(init.genesis.Config)
	aa := common.HexToAddress("0x000000000000000000000000000000000000aaaa")

	var err error
	var txs []*types.Transaction

	currentValidators := []*valset.Validator{valset.NewValidator(addr, 10)}

	for i := uint64(1); i <= sprintSize; i++ {
		txdata := &types.LegacyTx{
			Nonce:    i - 1,
			To:       &aa,
			Gas:      30000,
			GasPrice: newGwei(5),
		}

		if IsSpanEnd(i) {
			currentValidators = []*valset.Validator{valset.NewValidator(addr, 10)}
		}

		tx := types.NewTx(txdata)
		tx, err = types.SignTx(tx, signer, key)
		if err != nil {
			t.Fatalf("an incorrect transaction or signer: %v", err)
		}

		if i == 1 {
			txs = []*types.Transaction{tx}
		} else {
			txs = nil
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, txs, currentValidators)
		insertNewBlock(t, chain, block)
	}

	// state 6 was not written
	//
	fromID = uint64(5)
	to = int64(chain.GetHeaderByNumber(sprintSize).Time)

	eventRecords = []*clerk.EventRecordWithTime{
		buildStateEvent(sample, 5, 7),
		buildStateEvent(sample, 6, 4),
	}
	h.EXPECT().StateSyncEvents(fromID, to).Return(eventRecords, nil).AnyTimes()

	for i := sprintSize + 1; i <= spanSize; i++ {
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, currentValidators)
		insertNewBlock(t, chain, block)
	}

	for n := 0; n < 6; n++ {
		rpcNumber := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(n))
		ethAPI := ethapi.NewPublicBlockChainAPI(init.ethereum.APIBackend)

		ans, err := ethAPI.GetTransactionReceiptsByBlock(context.Background(), rpcNumber)
		if err != nil {
			t.Fatal("on GetTransactionReceiptsByBlock", err)
		}

		t.Logf("\n ANS ::: %v", ans)

		blockMap, err := ethAPI.GetBlockByNumber(context.Background(), rpc.BlockNumber(n), true)

		t.Log(blockMap["transactions"], err)
	}
}
