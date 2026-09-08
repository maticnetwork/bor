package ethapi

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

type preconfBackend interface {
	GetPreconfTransaction(common.Hash) (*types.Transaction, *types.Receipt, bool)
}

type preconfReceiptSubscriber interface {
	SubscribePreconfReceipts(chan<- core.PreconfReceiptsEvent) event.Subscription
}

func getPreconfTransaction(backend Backend, hash common.Hash) (*types.Transaction, *types.Receipt, bool) {
	provider, ok := backend.(preconfBackend)
	if !ok {
		return nil, nil, false
	}
	return provider.GetPreconfTransaction(hash)
}

func preconfTransactionReceipt(backend Backend, hash common.Hash) map[string]interface{} {
	result, _, _ := preconfTransactionReceiptAt(backend, hash)
	return result
}

func preconfTransactionReceiptAt(backend Backend, hash common.Hash) (map[string]interface{}, uint64, bool) {
	tx, receipt, ok := getPreconfTransaction(backend, hash)
	if !ok || tx == nil || receipt == nil || receipt.BlockNumber == nil {
		return nil, 0, false
	}
	blockTime := uint64(0)
	if block := pendingBlock(backend); block != nil && block.Number().Cmp(receipt.BlockNumber) == 0 {
		blockTime = block.Time()
	}
	return marshalPreconfTransactionReceipt(backend, tx, receipt, blockTime)
}

func marshalPreconfTransactionReceipt(backend Backend, tx *types.Transaction, receipt *types.Receipt, blockTime uint64) (map[string]interface{}, uint64, bool) {
	if tx == nil || receipt == nil || receipt.BlockNumber == nil {
		return nil, 0, false
	}
	copyReceipt := *receipt
	copyReceipt.Logs = make([]*types.Log, len(receipt.Logs))
	for i, entry := range receipt.Logs {
		if entry == nil {
			continue
		}
		copyLog := *entry
		copyLog.BlockHash = common.Hash{}
		copyReceipt.Logs[i] = &copyLog
	}
	blockNumber := receipt.BlockNumber.Uint64()
	signer := types.LatestSigner(backend.ChainConfig())
	if blockTime != 0 {
		signer = types.MakeSigner(backend.ChainConfig(), receipt.BlockNumber, blockTime)
	}
	result := marshalReceipt(&copyReceipt, common.Hash{}, blockNumber, signer, tx, int(receipt.TransactionIndex), false)
	result["blockHash"] = nil
	result["preconfirmation"] = true
	return result, blockNumber, true
}
