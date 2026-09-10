package sequencer

import (
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestPendingViewTracksCompleteRecordsAndIsolatesState(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()

	cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x71}))
	block, receipts, firstState := s.consumer.Pending()
	if block == nil || block.NumberU64() != h.chain.CurrentBlock().Number.Uint64()+1 {
		t.Fatalf("open pending block = %v", block)
	}
	if len(receipts) != 0 {
		t.Fatalf("open receipts = %d, want 0", len(receipts))
	}

	tx := h.transfer(t, 0)
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal tx: %v", err)
	}
	handleOK(t, s, recordEntry(raw, cur))
	publishPendingSnapshot(t, s)

	block, receipts, secondState := s.consumer.Pending()
	if len(block.Transactions()) != 1 || block.Transactions()[0].Hash() != tx.Hash() {
		t.Fatalf("pending transactions = %v", block.Transactions())
	}
	if len(receipts) != 1 || receipts[0].TxHash != tx.Hash() {
		t.Fatalf("pending receipts = %v", receipts)
	}
	if got := secondState.GetNonce(h.addr); got != 1 {
		t.Fatalf("pending nonce = %d, want 1", got)
	}

	firstState.SetNonce(h.addr, 99, tracing.NonceChangeUnspecified)
	_, _, freshState := s.consumer.Pending()
	if got := freshState.GetNonce(h.addr); got != 1 {
		t.Fatalf("request-local mutation leaked: nonce = %d", got)
	}

	sealed := sealedFromEnv(t, s)
	handleOK(t, s, sealEntry(encodeHeader(t, sealed), s.head))
	block, receipts, _ = s.consumer.Pending()
	if receipts[0].BlockHash != sealed.Hash() {
		t.Fatalf("sealed pending receipt hash = %s, want %s", receipts[0].BlockHash, sealed.Hash())
	}

	s.consumer.invalidatePendingFrom(block.NumberU64())
	if block, _, _ := s.consumer.Pending(); block != nil {
		t.Fatalf("pending block survived invalidation: %v", block)
	}
}

func TestPendingInvalidationEmitsRemovedLogs(t *testing.T) {
	header := &types.Header{Number: big.NewInt(7)}
	entry := &types.Log{BlockNumber: 7, BlockHash: common.HexToHash("0x7")}
	block := types.NewBlockWithHeader(header)
	key := pendingKey{number: 7}
	c := &Consumer{store: &PendingStore{entries: map[pendingKey]*pendingEntry{
		key: {Number: 7, RPCView: &PendingRPCView{Block: block, Logs: []*types.Log{entry}}},
	}, active: key, hasActive: true}}

	logs := make(chan []*types.Log, 1)
	sub := c.SubscribePendingLogs(logs)
	defer sub.Unsubscribe()

	c.invalidatePendingFrom(7)
	got := <-logs
	if len(got) != 1 || !got[0].Removed || entry.Removed {
		t.Fatalf("removed logs = %v, source removed = %v", got, entry.Removed)
	}
}

func TestPendingLogDeliveryPreservesLifecycleOrder(t *testing.T) {
	c := new(Consumer)
	logs := make(chan []*types.Log)
	sub := c.SubscribePendingLogs(logs)
	defer sub.Unsubscribe()

	added := []*types.Log{{BlockNumber: 7}}
	removed := []*types.Log{{BlockNumber: 7, Removed: true}}
	c.publishMu.Lock()
	c.enqueuePendingLogs(added)
	c.enqueuePendingLogs(removed)
	c.publishMu.Unlock()

	for index, wantRemoved := range []bool{false, true} {
		select {
		case got := <-logs:
			if len(got) != 1 || got[0].Removed != wantRemoved {
				t.Fatalf("event %d = %+v, want removed=%v", index, got, wantRemoved)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", index)
		}
	}
}

func TestPendingLogQueueIsBoundedAndStops(t *testing.T) {
	c := new(Consumer)
	blocked := make(chan []*types.Log)
	sub := c.SubscribePendingLogs(blocked)
	c.enqueuePendingLogs([]*types.Log{{BlockNumber: 0}})
	deadline := time.Now().Add(time.Second)
	for {
		c.logsMu.Lock()
		dispatching := c.logsBusy && len(c.logsQueue) == 0
		c.logsMu.Unlock()
		if dispatching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending log dispatcher did not start")
		}
		time.Sleep(time.Millisecond)
	}

	droppedBefore := preconfPendingLogsDropped.Snapshot().Count()
	for index := 0; index < pendingLogsQueueLimit+10; index++ {
		c.enqueuePendingLogs([]*types.Log{{BlockNumber: uint64(index)}})
	}
	c.logsMu.Lock()
	queued := len(c.logsQueue)
	c.logsMu.Unlock()
	if queued != pendingLogsQueueLimit {
		t.Fatalf("pending log queue = %d", queued)
	}
	if dropped := preconfPendingLogsDropped.Snapshot().Count() - droppedBefore; dropped != 10 {
		t.Fatalf("dropped pending log batches = %d", dropped)
	}

	c.Close()
	select {
	case <-sub.Err():
	case <-time.After(time.Second):
		t.Fatal("pending log subscription survived consumer close")
	}

	c.enqueuePendingLogs([]*types.Log{{BlockNumber: 99}})
	c.logsMu.Lock()
	queued = len(c.logsQueue)
	c.logsMu.Unlock()
	if queued != 0 {
		t.Fatalf("closed consumer queued %d log batches", queued)
	}
	lateSub := c.SubscribePendingLogs(make(chan []*types.Log))
	select {
	case <-lateSub.Err():
	case <-time.After(time.Second):
		t.Fatal("subscription created after close remained active")
	}
}
