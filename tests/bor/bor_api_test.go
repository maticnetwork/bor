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
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestGetTransactionReceiptsByBlock(t *testing.T) {

	// Initialise Bor client instance
	currentValidators := generateRandomValSet(t)

	init := buildEthereumInstance(t, rawdb.NewMemoryDatabase(), currentValidators)
	chain := init.ethereum.BlockChain()
	engine := init.ethereum.Engine()

	heimdallSpan := generateDummySpan(t, currentValidators)
	// RMV
	log.Info("Heimdall Span", "Validators", heimdallSpan.ValidatorSet.Validators, "Producers", heimdallSpan.SelectedProducers)
	h, ctrl := getMockedHeimdallClient(t, heimdallSpan)
	defer ctrl.Finish()

	_bor := engine.(*bor.Bor)
	defer _bor.Close()

	_bor.SetHeimdallClient(h)

	// Insert blocks for 0th sprint
	db := init.ethereum.ChainDb()
	block := init.genesis.ToBlock(db)

	signer := types.LatestSigner(init.genesis.Config)
	toAddress := common.HexToAddress("0x000000000000000000000000000000000000aaaa")
	txHashes := map[int]common.Hash{} // blockNumber -> txHash
	//signer := currentValidators[0]

	var (
		err   error
		nonce uint64
		tx    *types.Transaction
		txs   []*types.Transaction
	)

	// Block no.s which are multiple of 3 will have txs
	for i := uint64(1); i <= sprintSize; i++ {

		if IsSpanEnd(i) {
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
			tx, err = types.SignTx(tx, signer, addrs[currentValidators[0].Address])
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
	/*fromID = uint64(4)
	to = int64(chain.GetHeaderByNumber(sprintSize).Time)
	sample = getSampleEventRecord(t)

	eventRecords = []*clerk.EventRecordWithTime{
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
