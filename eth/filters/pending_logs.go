package filters

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func (api *FilterAPI) subscribeLogs(crit FilterCriteria, logs chan []*types.Log) (*Subscription, error) {
	if crit.Pending || isPendingRange(crit) {
		if !onlyPendingRange(crit) {
			return nil, errInvalidBlockRange
		}
		query := crit.query()
		query.FromBlock = nil
		query.ToBlock = nil
		return api.events.SubscribePendingLogs(query, logs)
	}
	return api.events.SubscribeLogs(crit.query(), logs)
}

func (es *EventSystem) validateLogCriteria(crit ethereum.FilterQuery) error {
	if len(crit.Topics) > maxTopics {
		return errExceedMaxTopics
	}
	limit := es.sys.cfg.LogQueryLimit
	if limit == 0 {
		return nil
	}
	if len(crit.Addresses) > limit {
		return errExceedLogQueryLimit
	}
	for _, topics := range crit.Topics {
		if len(topics) > limit {
			return errExceedLogQueryLimit
		}
	}
	return nil
}

func (es *EventSystem) SubscribePendingLogs(crit ethereum.FilterQuery, logs chan []*types.Log) (*Subscription, error) {
	if err := es.validateLogCriteria(crit); err != nil {
		return nil, err
	}
	if crit.BlockHash != nil || crit.FromBlock != nil || crit.ToBlock != nil {
		return nil, errInvalidBlockRange
	}
	sub := &subscription{
		id: rpc.NewID(), typ: PendingLogsSubscription, logsCrit: crit, created: time.Now(), logs: logs,
		txs: make(chan []*types.Transaction), headers: make(chan *types.Header),
		receipts: make(chan []*ReceiptWithTx), installed: make(chan struct{}), err: make(chan error),
	}
	return es.subscribe(sub), nil
}

func isPendingRange(crit FilterCriteria) bool {
	return crit.FromBlock != nil && crit.FromBlock.Int64() == rpc.PendingBlockNumber.Int64() ||
		crit.ToBlock != nil && crit.ToBlock.Int64() == rpc.PendingBlockNumber.Int64()
}

func isCanonicalToPendingRange(crit FilterCriteria) bool {
	return !crit.Pending && crit.BlockHash == nil &&
		crit.ToBlock != nil && crit.ToBlock.Int64() == rpc.PendingBlockNumber.Int64() &&
		(crit.FromBlock == nil || crit.FromBlock.Int64() != rpc.PendingBlockNumber.Int64())
}

type pendingLogsBackend interface {
	PendingBlockAndReceipts() (*types.Block, types.Receipts)
}

type pendingLogsRangeBackend interface {
	PendingLogRange() (*types.Header, []*types.Block, []types.Receipts)
}

func (api *FilterAPI) getPendingLogs(ctx context.Context, crit FilterCriteria) ([]*types.Log, error) {
	if !onlyPendingRange(crit) {
		return nil, errInvalidBlockRange
	}
	_, logs, err := api.pendingLogsSnapshot(ctx, crit)
	return logs, err
}

func (api *FilterAPI) getLogsThroughPending(ctx context.Context, crit FilterCriteria) ([]*types.Log, error) {
	if !isCanonicalToPendingRange(crit) {
		return nil, errInvalidBlockRange
	}
	for attempt := 0; attempt < 2; attempt++ {
		logs, retry, err := api.getLogsThroughPendingSnapshot(ctx, crit)
		if err != nil {
			if attempt == 0 && errors.Is(err, errPendingLogsIncomplete) {
				continue
			}
			return nil, err
		}
		if !retry {
			return logs, err
		}
	}
	return nil, errPendingLogsIncomplete
}

func (api *FilterAPI) getLogsThroughPendingSnapshot(ctx context.Context, crit FilterCriteria) ([]*types.Log, bool, error) {
	anchor, blocks, receipts, err := api.pendingLogRange(ctx)
	if err != nil {
		return nil, false, err
	}
	blocks, receipts, err = validatePendingLogRange(anchor, blocks, receipts)
	if err != nil {
		return nil, false, err
	}
	from, err := api.pendingRangeStart(ctx, crit, anchor)
	if err != nil {
		return nil, false, err
	}
	last := blocks[len(blocks)-1].NumberU64()
	if from > last {
		return nil, false, errInvalidBlockRange
	}
	if err := checkBlockRangeLimit(int64(from), int64(last), anchor.Number.Uint64(), api.sys.cfg.RangeLimit); err != nil {
		return nil, false, err
	}

	logs := make([]*types.Log, 0)
	if from <= anchor.Number.Uint64() {
		canonicalCrit := crit
		canonicalCrit.FromBlock = new(big.Int).SetUint64(from)
		canonicalCrit.ToBlock = new(big.Int).Set(anchor.Number)
		canonicalLogs, err := api.GetLogs(ctx, canonicalCrit)
		if err != nil {
			return nil, false, err
		}
		logs = append(logs, canonicalLogs...)
	}
	canonicalAnchor, err := api.sys.backend.HeaderByNumber(ctx, rpc.BlockNumber(anchor.Number.Int64()))
	if err != nil {
		return nil, false, err
	}
	if canonicalAnchor == nil || canonicalAnchor.Hash() != anchor.Hash() {
		return nil, true, nil
	}
	for i, block := range blocks {
		if block.NumberU64() < from {
			continue
		}
		pendingLogs := filterLogs(receiptLogs(receipts[i]), nil, nil, crit.Addresses, crit.Topics)
		logs = append(logs, pendingLogs...)
	}
	return returnLogs(logs), false, nil
}

func (api *FilterAPI) pendingRangeStart(ctx context.Context, crit FilterCriteria, anchor *types.Header) (uint64, error) {
	if crit.FromBlock == nil {
		return anchor.Number.Uint64(), nil
	}
	from := rpc.BlockNumber(crit.FromBlock.Int64())
	if from >= 0 {
		return uint64(from), nil
	}
	switch from {
	case rpc.EarliestBlockNumber:
		return api.sys.backend.HistoryPruningCutoff(), nil
	case rpc.LatestBlockNumber:
		return anchor.Number.Uint64(), nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		header, err := api.sys.backend.HeaderByNumber(ctx, from)
		if err != nil {
			return 0, err
		}
		if header == nil {
			return 0, ethereum.NotFound
		}
		return header.Number.Uint64(), nil
	default:
		return 0, errInvalidBlockRange
	}
}

func (api *FilterAPI) pendingLogRange(ctx context.Context) (*types.Header, []*types.Block, []types.Receipts, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if backend, ok := api.sys.backend.(pendingLogsRangeBackend); ok {
		anchor, blocks, receipts := backend.PendingLogRange()
		if anchor == nil || len(blocks) == 0 {
			return nil, nil, nil, errPendingLogsUnsupported
		}
		return anchor, blocks, receipts, nil
	}
	block, receipts, err := api.pendingBlockReceipts(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	anchor := api.sys.backend.CurrentHeader()
	if anchor == nil || block == nil {
		return nil, nil, nil, errPendingLogsUnsupported
	}
	return anchor, []*types.Block{block}, []types.Receipts{receipts}, nil
}

func validatePendingLogRange(anchor *types.Header, blocks []*types.Block, receipts []types.Receipts) ([]*types.Block, []types.Receipts, error) {
	if anchor == nil || anchor.Number == nil || len(blocks) == 0 || len(blocks) != len(receipts) {
		return nil, nil, errPendingLogsIncomplete
	}
	for len(blocks) > 0 && blocks[0] != nil && blocks[0].NumberU64() <= anchor.Number.Uint64() {
		blocks = blocks[1:]
		receipts = receipts[1:]
	}
	if len(blocks) == 0 {
		return nil, nil, errPendingLogsIncomplete
	}
	wantNumber, wantParent := anchor.Number.Uint64()+1, anchor.Hash()
	for i, block := range blocks {
		if block == nil || block.NumberU64() != wantNumber || block.ParentHash() != wantParent {
			return nil, nil, errPendingLogsIncomplete
		}
		if err := validatePendingReceipts(block, receipts[i]); err != nil {
			return nil, nil, err
		}
		wantNumber, wantParent = wantNumber+1, block.Hash()
	}
	return blocks, receipts, nil
}

func (api *FilterAPI) pendingLogsSnapshot(ctx context.Context, crit FilterCriteria) (*types.Block, []*types.Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	block, receipts, err := api.pendingBlockReceipts(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := validatePendingReceipts(block, receipts); err != nil {
		return nil, nil, err
	}
	logs := filterLogs(receiptLogs(receipts), nil, nil, crit.Addresses, crit.Topics)
	return block, returnLogs(logs), nil
}

func (api *FilterAPI) pendingBlockReceipts(ctx context.Context) (*types.Block, types.Receipts, error) {
	if backend, ok := api.sys.backend.(pendingLogsBackend); ok {
		block, receipts := backend.PendingBlockAndReceipts()
		if block == nil {
			return nil, nil, errPendingLogsUnsupported
		}
		return block, receipts, nil
	}
	header, err := api.sys.backend.HeaderByNumber(ctx, rpc.PendingBlockNumber)
	if err != nil {
		return nil, nil, err
	}
	if header == nil {
		return nil, nil, errPendingLogsUnsupported
	}
	receipts, err := api.sys.backend.GetReceipts(ctx, header.Hash())
	return nil, receipts, err
}

func validatePendingReceipts(block *types.Block, receipts types.Receipts) error {
	for _, receipt := range receipts {
		if receipt == nil {
			return errPendingLogsIncomplete
		}
	}
	if block == nil {
		return nil
	}
	txs := block.Transactions()
	if len(receipts) != len(txs) {
		return errPendingLogsIncomplete
	}
	for i, receipt := range receipts {
		if receipt.TxHash != txs[i].Hash() {
			return errPendingLogsIncomplete
		}
	}
	return nil
}

func receiptLogs(receipts types.Receipts) []*types.Log {
	var logs []*types.Log
	for _, receipt := range receipts {
		logs = append(logs, receipt.Logs...)
	}
	return logs
}

func onlyPendingRange(crit FilterCriteria) bool {
	if crit.BlockHash != nil {
		return false
	}
	if crit.FromBlock == nil {
		return crit.Pending && crit.ToBlock == nil
	}
	if crit.FromBlock != nil && crit.FromBlock.Int64() != rpc.PendingBlockNumber.Int64() {
		return false
	}
	return crit.ToBlock == nil || crit.ToBlock.Int64() == rpc.PendingBlockNumber.Int64()
}
