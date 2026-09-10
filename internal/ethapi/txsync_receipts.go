package ethapi

import (
	"context"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

type txSyncReceiptResult struct {
	receipt map[string]interface{}
	err     error
}

type txSyncReceiptRun struct {
	stop          chan struct{}
	chainEvents   chan core.ChainEvent
	chainSub      event.Subscription
	preconfEvents chan core.PreconfReceiptsEvent
	preconfSub    event.Subscription
}

type txSyncReceiptHub struct {
	api     *TransactionAPI
	mu      sync.Mutex
	waiters map[common.Hash]map[chan txSyncReceiptResult]struct{}
	run     *txSyncReceiptRun
}

type txSyncReceiptDelivery struct {
	targets map[chan txSyncReceiptResult]struct{}
	result  txSyncReceiptResult
}

func newTxSyncReceiptHub(api *TransactionAPI) *txSyncReceiptHub {
	return &txSyncReceiptHub{
		api:     api,
		waiters: make(map[common.Hash]map[chan txSyncReceiptResult]struct{}),
	}
}

func (h *txSyncReceiptHub) register(hash common.Hash) (<-chan txSyncReceiptResult, func()) {
	updates := make(chan txSyncReceiptResult, 1)
	h.mu.Lock()
	if h.waiters[hash] == nil {
		h.waiters[hash] = make(map[chan txSyncReceiptResult]struct{})
	}
	h.waiters[hash][updates] = struct{}{}
	if h.run == nil {
		h.startLocked()
	}
	h.mu.Unlock()

	var once sync.Once
	return updates, func() {
		once.Do(func() { h.unregister(hash, updates) })
	}
}

func (h *txSyncReceiptHub) startLocked() {
	run := &txSyncReceiptRun{
		stop:        make(chan struct{}),
		chainEvents: make(chan core.ChainEvent, 128),
	}
	run.chainSub = h.api.b.SubscribeChainEvent(run.chainEvents)
	if source, ok := h.api.b.(preconfReceiptSubscriber); ok && h.api.b.PreconfEnabled() {
		run.preconfEvents = make(chan core.PreconfReceiptsEvent, 4096)
		run.preconfSub = source.SubscribePreconfReceipts(run.preconfEvents)
	}
	h.run = run
	go h.loop(run)
}

func (h *txSyncReceiptHub) unregister(hash common.Hash, updates chan txSyncReceiptResult) {
	h.mu.Lock()
	if targets := h.waiters[hash]; targets != nil {
		delete(targets, updates)
		if len(targets) == 0 {
			delete(h.waiters, hash)
		}
	}
	run := h.stopIfIdleLocked()
	h.mu.Unlock()
	if run != nil {
		close(run.stop)
	}
}

func (h *txSyncReceiptHub) stopIfIdleLocked() *txSyncReceiptRun {
	if len(h.waiters) != 0 || h.run == nil {
		return nil
	}
	run := h.run
	h.run = nil
	return run
}

func (h *txSyncReceiptHub) loop(run *txSyncReceiptRun) {
	defer run.chainSub.Unsubscribe()
	if run.preconfSub != nil {
		defer run.preconfSub.Unsubscribe()
	}

	chainErr := run.chainSub.Err()
	var preconfErr <-chan error
	if run.preconfSub != nil {
		preconfErr = run.preconfSub.Err()
	}
	for {
		if run.preconfEvents != nil {
			select {
			case hashes, open := <-run.preconfEvents:
				if !open {
					run.preconfEvents = nil
					preconfErr = nil
				} else {
					h.preconfirmed(hashes)
				}
				continue
			default:
			}
		}
		select {
		case <-run.stop:
			return
		case event, open := <-run.chainEvents:
			if !open {
				h.fail(run, errSubClosed)
				return
			}
			h.canonical(event)
		case err, open := <-chainErr:
			h.fail(run, subscriptionFailure(err, open))
			return
		case hashes, open := <-run.preconfEvents:
			if !open {
				run.preconfEvents = nil
				preconfErr = nil
				continue
			}
			h.preconfirmed(hashes)
		case <-preconfErr:
			run.preconfEvents = nil
			preconfErr = nil
		}
	}
}

func (h *txSyncReceiptHub) preconfirmed(event core.PreconfReceiptsEvent) {
	h.mu.Lock()
	wanted := make([]int, 0, len(event.Receipts))
	for index, receipt := range event.Receipts {
		if receipt != nil && len(h.waiters[receipt.TxHash]) != 0 {
			wanted = append(wanted, index)
		}
	}
	h.mu.Unlock()

	results := make(map[common.Hash]txSyncReceiptResult, len(wanted))
	for _, index := range wanted {
		if index >= len(event.Transactions) {
			continue
		}
		receipt := event.Receipts[index]
		marshaled, _, ok := marshalPreconfTransactionReceipt(h.api.b, event.Transactions[index], receipt, event.BlockTime)
		if ok {
			results[receipt.TxHash] = txSyncReceiptResult{receipt: marshaled}
		}
	}
	h.deliverBatch(results)
}

func (h *txSyncReceiptHub) canonical(event core.ChainEvent) {
	if len(event.Receipts) == 0 || len(event.Receipts) != len(event.Transactions) {
		return
	}
	h.mu.Lock()
	indexes := make([]int, 0, len(event.Receipts))
	for index, receipt := range event.Receipts {
		if receipt != nil && len(h.waiters[receipt.TxHash]) != 0 {
			indexes = append(indexes, index)
		}
	}
	h.mu.Unlock()
	results := make(map[common.Hash]txSyncReceiptResult, len(indexes))
	for _, index := range indexes {
		receipt := h.canonicalReceipt(event.Receipts[index], event.Transactions[index])
		if receipt != nil {
			results[event.Receipts[index].TxHash] = txSyncReceiptResult{receipt: receipt}
		}
	}
	h.deliverBatch(results)
}

func (h *txSyncReceiptHub) canonicalReceipt(receipt *types.Receipt, tx *types.Transaction) map[string]interface{} {
	if receipt == nil || tx == nil {
		return nil
	}
	if receipt.BlockNumber == nil || receipt.BlockHash == (common.Hash{}) {
		return h.api.receiptIfAvailable(context.Background(), receipt.TxHash)
	}
	return MarshalReceipt(
		receipt,
		receipt.BlockHash,
		receipt.BlockNumber.Uint64(),
		types.LatestSigner(h.api.b.ChainConfig()),
		tx,
		int(receipt.TransactionIndex),
	)
}

func (h *txSyncReceiptHub) deliverBatch(results map[common.Hash]txSyncReceiptResult) {
	if len(results) == 0 {
		return
	}
	h.mu.Lock()
	deliveries := make([]txSyncReceiptDelivery, 0, len(results))
	for hash, result := range results {
		targets := h.waiters[hash]
		if len(targets) == 0 {
			continue
		}
		delete(h.waiters, hash)
		deliveries = append(deliveries, txSyncReceiptDelivery{targets: targets, result: result})
	}
	run := h.stopIfIdleLocked()
	h.mu.Unlock()
	for _, delivery := range deliveries {
		for target := range delivery.targets {
			target <- delivery.result
		}
	}
	if run != nil {
		close(run.stop)
	}
}

func (h *txSyncReceiptHub) fail(run *txSyncReceiptRun, err error) {
	h.mu.Lock()
	if h.run != run {
		h.mu.Unlock()
		return
	}
	h.run = nil
	waiters := h.waiters
	h.waiters = make(map[common.Hash]map[chan txSyncReceiptResult]struct{})
	h.mu.Unlock()
	for _, targets := range waiters {
		for target := range targets {
			target <- txSyncReceiptResult{err: err}
		}
	}
}
