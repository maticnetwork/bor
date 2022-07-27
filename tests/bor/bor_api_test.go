//go:build integration

package bor

import (
	"context"
	//"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestGetTransactionReceiptsByBlock(t *testing.T) {
	// Initialise Bor client instance
	//init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase())
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()

	// Fetch mocked heimdall client
	//ctrl := gomock.NewController(t)
	//defer ctrl.Finish()

	heimdallSpan := generateDummySpan(t)
	_, ctrl := getMockedHeimdallClient(t, heimdallSpan)
	defer ctrl.Finish()

	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	// Mock /bor/span/1
	//res, _ := loadSpanFromFile(t)

	//h := mocks.NewMockIHeimdallClient(ctrl)

	/*h.EXPECT().Span(gomock.Any(), uint64(1)).Return(&res.Result, nil).MinTimes(1)
	h.EXPECT().Close().MinTimes(1)
	h.EXPECT().FetchCheckpoint(gomock.Any(), int64(-1)).Return(&checkpoint.Checkpoint{
		Proposer:   res.Result.SelectedProducers[0].Address,
		StartBlock: big.NewInt(0),
		EndBlock:   big.NewInt(int64(spanSize)),
	}, nil).AnyTimes()

	// Mock State Sync events
	// at # sprintSize, events are fetched for [fromID, (block-sprint).Time)
	fromID := uint64(1)
	to := int64(chain.GetHeaderByNumber(0).Time)
	sample := getSampleEventRecord(t)

	// First query will be from [id=1, (block-sprint).Time]
	eventRecords := []*clerk.EventRecordWithTime{
		buildStateEvent(sample, 1, 1),
		buildStateEvent(sample, 2, 2),
		buildStateEvent(sample, 3, 3),
	}

	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).MinTimes(1)
	_bor.SetHeimdallClient(h)*/

	// Insert blocks for 0th sprint
	db := init.ethereum.ChainDb()
	block := init.genesis.ToBlock(db)

	signer := types.LatestSigner(init.genesis.Config)
	toAddress := common.HexToAddress("0x000000000000000000000000000000000000aaaa")

	//currentValidators := []*valset.Validator{valset.NewValidator(addr, 10)}
	currentValidators := generateRandomValSet(t)
	txHashes := map[int]common.Hash{} // blockNumber -> txHash

	var (
		err   error
		nonce uint64
		tx    *types.Transaction
		txs   []*types.Transaction
	)

	// Block no.s which are multiple of 3 will have txs
	for i := uint64(1); i <= sprintSize; i++ {
		s := IsSpanEnd(i)
		if s {
			//currentValidators = []*valset.Validator{valset.NewValidator(addr, 10)}
			currentValidators = generateRandomValSet(t)
		}

		if i%3 == 0 {
			txdata := &types.LegacyTx{
				Nonce:    nonce,
				To:       &toAddress,
				Gas:      30000,
				GasPrice: newGwei(5),
			}

			nonce++

			tx = types.NewTx(txdata)
			tx, err = types.SignTx(tx, signer, key)
			require.Nil(t, err, "an incorrect transaction or signer")

			txs = []*types.Transaction{tx}
		} else {
			txs = nil
		}

		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, txs, currentValidators)
		insertNewBlock(t, chain, block)

		if len(txs) != 0 {
			txHashes[int(block.Number().Uint64())] = tx.Hash()
		}
	}

	// state 6 was not written
	//
	/*fromID := uint64(4)
	to := int64(chain.GetHeaderByNumber(sprintSize).Time)
	sample := getSampleEventRecord(t)

	eventRecords := []*clerk.EventRecordWithTime{
		buildStateEvent(sample, 4, 4),
		buildStateEvent(sample, 5, 5),
	}

	h.EXPECT().StateSyncEvents(gomock.Any(), fromID, to).Return(eventRecords, nil).MinTimes(1)
	*/
	// Empty blocks for rest of the span
	for i := sprintSize + 1; i <= spanSize; i++ {
		block = buildNextBlock(t, _bor, chain, block, nil, init.genesis.Config.Bor, nil, currentValidators)
		insertNewBlock(t, chain, block)
	}

	ethAPI := ethapi.NewPublicBlockChainAPI(init.ethereum.APIBackend)
	txPoolAPI := ethapi.NewPublicTransactionPoolAPI(init.ethereum.APIBackend, nil)

	// Assertions
	for n := 0; n < int(spanSize)+1; n++ {
		rpcNumber := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(n))

		txs, err := ethAPI.GetTransactionReceiptsByBlock(context.Background(), rpcNumber)
		require.Nil(t, err)

		tx := txPoolAPI.GetTransactionByBlockNumberAndIndex(context.Background(), rpc.BlockNumber(n), 0)

		blockMap, err := ethAPI.GetBlockByNumber(context.Background(), rpc.BlockNumber(n), true)
		require.Nil(t, err)

		expectedTxHash, ok := txHashes[n]
		// FIXME: add `IsSprintStart(uint64(n)) || IsSpanStart(uint64(n))` after adding a full state receiver contract
		if ok {
			require.Len(t, txs, 1)

			require.NotNil(t, tx, "not nil receipt expected")

			require.Equal(t, expectedTxHash, tx.Hash, "got different from expected receipt")

			blockTxs, ok := blockMap["transactions"].([]interface{})
			require.Len(t, blockTxs, 1)

			blockTx, ok := blockTxs[0].(*ethapi.RPCTransaction)
			require.True(t, ok)
			require.Equal(t, expectedTxHash, blockTx.Hash)
		} else {
			require.Len(t, txs, 0)

			require.Nil(t, tx, "nil receipt expected")

			blockTxs, _ := blockMap["transactions"].([]interface{})
			require.Len(t, blockTxs, 0)
		}
	}
}
