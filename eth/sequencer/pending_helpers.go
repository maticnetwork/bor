package sequencer

import (
	"bytes"
	"errors"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

func buildRPCView(block *types.Block, receipts types.Receipts, statedb *state.StateDB, previous *PendingRPCView, blockHash common.Hash) *PendingRPCView {
	receiptMap := make(map[common.Hash]*types.Receipt, len(receipts))
	logs := make([]*types.Log, 0)
	start := 0
	if reusableReceiptPrefix(previous, block, len(receipts), blockHash) {
		for hash, receipt := range previous.Receipts {
			receiptMap[hash] = receipt
		}
		logs = append(logs, previous.Logs...)
		start = len(previous.Block.Transactions())
	}
	for index := start; index < len(receipts); index++ {
		copy := cloneReceiptWithBlockHash(receipts[index], blockHash)
		if index < len(block.Transactions()) {
			receiptMap[block.Transactions()[index].Hash()] = copy
		}
		logs = append(logs, copy.Logs...)
	}
	stateCopy := statedb.CopyWithoutLogHistory()
	return &PendingRPCView{
		Header:           block.Header(),
		Block:            block,
		State:            &pendingStateReader{state: stateCopy},
		Receipts:         receiptMap,
		Logs:             logs,
		receiptBlockHash: blockHash,
	}
}

func reusableReceiptPrefix(previous *PendingRPCView, block *types.Block, receiptCount int, blockHash common.Hash) bool {
	if previous == nil || previous.Block == nil || previous.receiptBlockHash != blockHash ||
		len(previous.Block.Transactions()) > receiptCount || len(previous.Receipts) != len(previous.Block.Transactions()) {
		return false
	}
	return sameExecutionContext(previous.Header, block.Header()) && sameTransactionPrefix(previous.Block, block)
}

func receiptsFromView(block *types.Block, view *PendingRPCView) types.Receipts {
	receipts := make(types.Receipts, 0, len(block.Transactions()))
	for _, tx := range block.Transactions() {
		if receipt := view.Receipts[tx.Hash()]; receipt != nil {
			receipts = append(receipts, cloneReceiptWithBlockHash(receipt, view.receiptBlockHash))
		}
	}
	return receipts
}

func removedLogs(view *PendingRPCView) []*types.Log {
	if view == nil {
		return nil
	}
	logs := make([]*types.Log, len(view.Logs))
	for index, entry := range view.Logs {
		if entry == nil {
			continue
		}
		copy := *entry
		copy.BlockHash = view.receiptBlockHash
		copy.Removed = true
		logs[index] = &copy
	}
	return logs
}

func removedEntryLogs(entry *pendingEntry) []*types.Log {
	if entry == nil {
		return nil
	}
	if entry.RPCView != nil && entry.RPCView.Block != nil &&
		len(entry.RPCView.Block.Transactions()) >= len(entry.executedTransactions) {
		return removedLogs(entry.RPCView)
	}
	var logs []*types.Log
	for _, receipt := range entry.executedReceipts {
		for _, logEntry := range receipt.Logs {
			if logEntry == nil {
				logs = append(logs, nil)
				continue
			}
			copy := *logEntry
			copy.Removed = true
			logs = append(logs, &copy)
		}
	}
	return logs
}

func cloneProcessResult(result *core.ProcessResult) *core.ProcessResult {
	if result == nil {
		return nil
	}
	requests := make([][]byte, len(result.Requests))
	for index := range result.Requests {
		requests[index] = append([]byte(nil), result.Requests[index]...)
	}
	logs := make([]*types.Log, len(result.Logs))
	for index, entry := range result.Logs {
		if entry == nil {
			continue
		}
		copy := *entry
		copy.Topics = append([]common.Hash{}, entry.Topics...)
		copy.Data = append([]byte(nil), entry.Data...)
		logs[index] = &copy
	}
	return &core.ProcessResult{Receipts: cloneReceipts(result.Receipts), Requests: requests, Logs: logs, GasUsed: result.GasUsed}
}

func cloneReceipt(receipt *types.Receipt) *types.Receipt {
	if receipt == nil {
		return nil
	}
	copy := *receipt
	copy.PostState = append([]byte(nil), receipt.PostState...)
	copy.Logs = make([]*types.Log, len(receipt.Logs))
	for index, entry := range receipt.Logs {
		if entry == nil {
			continue
		}
		logCopy := *entry
		logCopy.Topics = append([]common.Hash{}, entry.Topics...)
		logCopy.Data = append([]byte(nil), entry.Data...)
		copy.Logs[index] = &logCopy
	}
	return &copy
}

func cloneReceiptWithBlockHash(receipt *types.Receipt, blockHash common.Hash) *types.Receipt {
	copy := cloneReceipt(receipt)
	copy.BlockHash = blockHash
	for _, entry := range copy.Logs {
		if entry == nil {
			continue
		}
		entry.BlockHash = blockHash
	}
	return copy
}

func cloneReceipts(receipts []*types.Receipt) types.Receipts {
	clones := make(types.Receipts, len(receipts))
	for index, receipt := range receipts {
		clones[index] = cloneReceipt(receipt)
	}
	return clones
}

func sameTransactions(a, b *types.Block) bool {
	if a == nil || b == nil || a.TxHash() != b.TxHash() {
		return false
	}
	aTxs, bTxs := a.Transactions(), b.Transactions()
	if len(aTxs) != len(bTxs) {
		return false
	}
	for index := range aTxs {
		if aTxs[index].Hash() != bTxs[index].Hash() {
			return false
		}
	}
	return true
}

func sameTransactionPrefix(prefix, block *types.Block) bool {
	if prefix == nil || block == nil {
		return false
	}
	prefixTxs, blockTxs := prefix.Transactions(), block.Transactions()
	if len(prefixTxs) > len(blockTxs) {
		return false
	}
	for index := range prefixTxs {
		if prefixTxs[index].Hash() != blockTxs[index].Hash() {
			return false
		}
	}
	return true
}

func sameTransactionSlicePrefix(prefix, block types.Transactions) bool {
	if len(prefix) > len(block) {
		return false
	}
	for index := range prefix {
		if prefix[index] == nil || block[index] == nil || prefix[index].Hash() != block[index].Hash() {
			return false
		}
	}
	return true
}

func sameExecutionContext(prefix, canonical *types.Header) bool {
	if prefix == nil || canonical == nil || prefix.Number == nil || canonical.Number == nil {
		return false
	}
	if canonical.ParentBeaconRoot != nil || canonical.ExcessBlobGas != nil || canonical.BlobGasUsed != nil ||
		canonical.WithdrawalsHash != nil || canonical.RequestsHash != nil {
		return false
	}
	return prefix.ParentHash == canonical.ParentHash && prefix.Number.Cmp(canonical.Number) == 0 &&
		prefix.Time == canonical.Time && prefix.GasLimit == canonical.GasLimit &&
		bigEqual(prefix.BaseFee, canonical.BaseFee) && bigEqual(prefix.Difficulty, canonical.Difficulty)
}

func prefixProcessResult(view *PendingRPCView) (*core.ProcessResult, bool) {
	if view == nil || view.Header == nil || view.Block == nil {
		return nil, false
	}
	txs := view.Block.Transactions()
	receipts := receiptsFromView(view.Block, view)
	if len(txs) == 0 || len(receipts) != len(txs) || receipts[len(receipts)-1].CumulativeGasUsed != view.Header.GasUsed {
		return nil, false
	}
	logs := make([]*types.Log, 0)
	for index, receipt := range receipts {
		if receipt == nil || receipt.TxHash != txs[index].Hash() {
			return nil, false
		}
		logs = append(logs, receipt.Logs...)
	}
	return &core.ProcessResult{Receipts: receipts, Logs: logs, GasUsed: view.Header.GasUsed}, true
}

func sameReceipts(block *types.Block, view *PendingRPCView, canonical types.Receipts) bool {
	pending := receiptsFromView(block, view)
	return len(pending) == len(canonical) && sameConsensusReceipts(pending, canonical)
}

func sameReceiptPrefix(block *types.Block, view *PendingRPCView, canonical types.Receipts) bool {
	pending := receiptsFromView(block, view)
	return len(pending) == len(block.Transactions()) && len(pending) <= len(canonical) &&
		sameConsensusReceipts(pending, canonical[:len(pending)])
}

func sameReceiptSlicePrefix(pending, canonical types.Receipts) bool {
	return len(pending) <= len(canonical) && sameConsensusReceipts(pending, canonical[:len(pending)])
}

func sameConsensusReceipts(left, right types.Receipts) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameConsensusReceipt(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameConsensusReceipt(left, right *types.Receipt) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.Type != right.Type || left.Status != right.Status ||
		left.CumulativeGasUsed != right.CumulativeGasUsed || left.Bloom != right.Bloom ||
		!bytes.Equal(left.PostState, right.PostState) || len(left.Logs) != len(right.Logs) {
		return false
	}
	for index := range left.Logs {
		leftLog, rightLog := left.Logs[index], right.Logs[index]
		if leftLog == nil || rightLog == nil {
			if leftLog != rightLog {
				return false
			}
			continue
		}
		if leftLog.Address != rightLog.Address || !slices.Equal(leftLog.Topics, rightLog.Topics) ||
			!bytes.Equal(leftLog.Data, rightLog.Data) {
			return false
		}
	}
	return true
}

func blockFromExecution(header *types.Header, txs types.Transactions, receipts types.Receipts) (*types.Block, error) {
	if header == nil {
		return nil, errors.New("nil pending header")
	}
	return types.NewBlock(types.CopyHeader(header), &types.Body{Transactions: txs}, receipts, trie.NewStackTrie(nil)), nil
}
