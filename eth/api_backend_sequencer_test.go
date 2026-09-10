package eth

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/sequencer"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rpc"
)

type apiSequenceConsumer struct {
	block         *types.Block
	receipts      types.Receipts
	state         *state.StateDB
	parentState   *state.StateDB
	index         *sequencer.Index
	feed          event.Feed
	receiptFeed   event.Feed
	subscribed    bool
	snapshotCalls int
	metadataCalls int
	rangeAnchor   *types.Header
	rangeBlocks   []*types.Block
	rangeReceipts []types.Receipts
	snapshotErr   error
	headBlock     *types.Block
	headState     *state.StateDB
	headErr       error
}

func (c *apiSequenceConsumer) HeadPendingView() (*types.Block, types.Receipts, *state.StateDB, error) {
	return c.headBlock, nil, c.headState, c.headErr
}

func (c *apiSequenceConsumer) PendingSnapshot(context.Context) (*types.Block, types.Receipts, *state.StateDB, error) {
	c.snapshotCalls++
	return c.block, c.receipts, c.state, c.snapshotErr
}

func (c *apiSequenceConsumer) PendingBlock() *types.Block {
	return c.block
}

func (c *apiSequenceConsumer) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	c.metadataCalls++
	return c.block, c.receipts
}

func (c *apiSequenceConsumer) PendingLogRange() (*types.Header, []*types.Block, []types.Receipts) {
	return c.rangeAnchor, c.rangeBlocks, c.rangeReceipts
}

func (c *apiSequenceConsumer) PendingParentState(context.Context, *types.Block) (*state.StateDB, error) {
	return c.parentState, nil
}

func (c *apiSequenceConsumer) PendingNonce(address common.Address) (uint64, bool, error) {
	if c.state == nil {
		return 0, false, nil
	}
	if err := c.state.Error(); err != nil {
		return 0, true, err
	}
	return c.state.GetNonce(address), true, nil
}

func (c *apiSequenceConsumer) LookupPreconf(hash common.Hash) (*types.Transaction, *types.Receipt, bool) {
	receipt, tx, ok := c.index.Lookup(hash)
	return tx, receipt, ok
}

func (c *apiSequenceConsumer) SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription {
	c.subscribed = true
	return c.feed.Subscribe(ch)
}

func (c *apiSequenceConsumer) SubscribePreconfReceipts(ch chan<- core.PreconfReceiptsEvent) event.Subscription {
	return c.receiptFeed.Subscribe(ch)
}

func (c *apiSequenceConsumer) Close() {}

func TestSequencerPendingBackend(t *testing.T) {
	b := initBackend(false)
	defer b.eth.blockchain.Stop()
	defer b.eth.txPool.Close()

	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	statedb.SetNonce(address, 4, tracing.NonceChangeUnspecified)
	tx := makeTx(0, nil, nil, key)
	header := &types.Header{Number: big.NewInt(1), ParentHash: b.eth.blockchain.CurrentBlock().Hash(), GasLimit: 30_000_000}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: types.Transactions{tx}})
	receipt := &types.Receipt{TxHash: tx.Hash(), BlockNumber: big.NewInt(1)}
	index := sequencer.NewIndex()
	index.Add(tx, receipt)
	consumer := &apiSequenceConsumer{
		block: block, receipts: types.Receipts{receipt}, state: statedb, parentState: statedb, index: index,
		rangeAnchor: b.eth.blockchain.CurrentBlock(), rangeBlocks: []*types.Block{block}, rangeReceipts: []types.Receipts{{receipt}},
	}
	b.eth.seqConsumer = consumer

	gotHeader, err := b.HeaderByNumber(t.Context(), rpc.PendingBlockNumber)
	if err != nil || gotHeader.Hash() != block.Header().Hash() {
		t.Fatalf("pending header = %v, %v", gotHeader, err)
	}
	gotBlock, err := b.BlockByNumber(t.Context(), rpc.PendingBlockNumber)
	if err != nil || gotBlock.Hash() != block.Hash() {
		t.Fatalf("pending block = %v, %v", gotBlock, err)
	}
	gotBlock, gotReceipts, gotState := b.Pending()
	if gotBlock != nil || gotReceipts != nil || gotState != nil {
		t.Fatalf("legacy pending view unexpectedly used sequencer state: %v, %v, %v", gotBlock, gotReceipts, gotState)
	}
	gotState, gotHeader, err = b.StateAndHeaderByNumber(t.Context(), rpc.PendingBlockNumber)
	if err != nil || gotState != statedb || gotHeader.Hash() != block.Header().Hash() {
		t.Fatalf("pending state/header = %v, %v, %v", gotState, gotHeader, err)
	}
	gotReceipts, err = b.GetReceipts(t.Context(), block.Hash())
	if err != nil || len(gotReceipts) != 1 || gotReceipts[0] != receipt {
		t.Fatalf("pending receipts = %v, %v", gotReceipts, err)
	}
	anchor, blocks, receiptSets := b.PendingLogRange()
	if anchor.Hash() != b.eth.blockchain.CurrentBlock().Hash() || len(blocks) != 1 || blocks[0] != block || len(receiptSets) != 1 || receiptSets[0][0] != receipt {
		t.Fatalf("pending log range = %v, %v, %v", anchor, blocks, receiptSets)
	}
	gotTx, gotReceipt, ok := b.GetPreconfTransaction(tx.Hash())
	if !ok || gotTx != tx || gotReceipt != receipt {
		t.Fatalf("preconf transaction = %v, %v, %v", gotTx, gotReceipt, ok)
	}
	if nonce, err := b.GetPoolNonce(t.Context(), address); err != nil || nonce != 4 {
		t.Fatalf("pending nonce = %d, %v", nonce, err)
	}
	parentState, err := b.PendingParentState(t.Context(), block)
	if err != nil || parentState != statedb {
		t.Fatalf("pending parent state = %v, %v", parentState, err)
	}
	b.eth.seqConsumer = nil
	parentState, err = b.PendingParentState(t.Context(), block)
	if err != nil || parentState != nil {
		t.Fatalf("pending parent state without consumer = %v, %v", parentState, err)
	}
	b.eth.seqConsumer = consumer
	logs := make(chan []*types.Log, 1)
	sub := b.SubscribePendingLogsEvent(logs)
	sub.Unsubscribe()
	if !consumer.subscribed {
		t.Fatal("pending log subscription did not use the sequencer consumer")
	}
}

func TestSequencerPendingMetadataAvoidsStateCopy(t *testing.T) {
	b := initBackend(false)
	defer b.eth.blockchain.Stop()
	defer b.eth.txPool.Close()

	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	statedb.SetNonce(address, 4, tracing.NonceChangeUnspecified)
	tx := makeTx(0, nil, nil, key)
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), GasLimit: 30_000_000}).WithBody(types.Body{Transactions: types.Transactions{tx}})
	receipt := &types.Receipt{TxHash: tx.Hash(), BlockNumber: big.NewInt(1)}
	consumer := &apiSequenceConsumer{block: block, receipts: types.Receipts{receipt}, state: statedb, index: sequencer.NewIndex()}
	b.eth.seqConsumer = consumer

	if _, err := b.HeaderByNumber(t.Context(), rpc.PendingBlockNumber); err != nil {
		t.Fatalf("pending header: %v", err)
	}
	if _, err := b.BlockByNumber(t.Context(), rpc.PendingBlockNumber); err != nil {
		t.Fatalf("pending block: %v", err)
	}
	if _, err := b.GetReceipts(t.Context(), block.Hash()); err != nil {
		t.Fatalf("pending receipts: %v", err)
	}
	consumer.metadataCalls = 0
	if _, err := b.GetReceipts(t.Context(), b.eth.blockchain.CurrentBlock().Hash()); err != nil {
		t.Fatalf("canonical receipts: %v", err)
	}
	if consumer.metadataCalls != 0 {
		t.Fatalf("canonical receipt lookup copied pending receipts: calls=%d", consumer.metadataCalls)
	}
	if _, err := b.GetPoolNonce(t.Context(), address); err != nil {
		t.Fatalf("pending nonce: %v", err)
	}
	if consumer.snapshotCalls != 0 {
		t.Fatalf("metadata materialized pending state: snapshots=%d", consumer.snapshotCalls)
	}
}

func TestCanonicalReceiptsTakePrecedenceOverSequencerView(t *testing.T) {
	b := initBackend(false)
	defer b.eth.blockchain.Stop()
	defer b.eth.txPool.Close()

	canonical := b.eth.blockchain.GetBlockByHash(b.eth.blockchain.CurrentBlock().Hash())
	consumer := &apiSequenceConsumer{
		block:    canonical,
		receipts: types.Receipts{{Status: types.ReceiptStatusFailed}},
		index:    sequencer.NewIndex(),
	}
	b.eth.seqConsumer = consumer

	receipts, err := b.GetReceipts(t.Context(), canonical.Hash())
	if err != nil {
		t.Fatalf("canonical receipts: %v", err)
	}
	if len(receipts) != 0 {
		t.Fatalf("sequencer receipts won canonical lookup: %+v", receipts)
	}
	if consumer.metadataCalls != 0 {
		t.Fatalf("canonical lookup consulted sequencer metadata %d times", consumer.metadataCalls)
	}
}

func TestSequencerPendingBackendFallback(t *testing.T) {
	b := initBackend(false)
	defer b.eth.blockchain.Stop()
	defer b.eth.txPool.Close()

	if block, receipts, statedb := b.Pending(); block != nil || receipts != nil || statedb != nil {
		t.Fatalf("pending fallback = %v, %v, %v", block, receipts, statedb)
	}
	if _, err := b.HeaderByNumber(t.Context(), rpc.PendingBlockNumber); err == nil {
		t.Fatal("missing pending header did not fail")
	}
	if _, err := b.BlockByNumber(t.Context(), rpc.PendingBlockNumber); err == nil {
		t.Fatal("missing pending block did not fail")
	}
	if _, _, err := b.StateAndHeaderByNumber(t.Context(), rpc.PendingBlockNumber); err == nil {
		t.Fatal("missing pending state did not fail")
	}
	if tx, receipt, ok := b.GetPreconfTransaction(common.Hash{}); ok || tx != nil || receipt != nil {
		t.Fatalf("missing preconf transaction = %v, %v, %v", tx, receipt, ok)
	}
	if _, err := b.GetReceipts(t.Context(), b.eth.blockchain.CurrentBlock().Hash()); err != nil {
		t.Fatalf("canonical receipt fallback: %v", err)
	}
	logs := make(chan []*types.Log, 1)
	sub := b.SubscribePendingLogsEvent(logs)
	sub.Unsubscribe()
}

func TestSequencerPendingSnapshotFailures(t *testing.T) {
	b := initBackend(false)
	defer b.eth.blockchain.Stop()
	defer b.eth.txPool.Close()

	if block := b.PendingBlock(); block != nil {
		t.Fatalf("pending block = %v", block)
	}
	if block, receipts := b.PendingBlockAndReceipts(); block != nil || receipts != nil {
		t.Fatalf("pending metadata = %v, %v", block, receipts)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, err := b.PendingSnapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error = %v", err)
	}

	want := errors.New("pending snapshot failed")
	b.eth.seqConsumer = &apiSequenceConsumer{index: sequencer.NewIndex(), snapshotErr: want}
	if _, _, _, err := b.PendingSnapshot(t.Context()); !errors.Is(err, want) {
		t.Fatalf("consumer snapshot error = %v", err)
	}
	b.eth.seqConsumer = &apiSequenceConsumer{index: sequencer.NewIndex()}
	if _, _, _, err := b.PendingSnapshot(t.Context()); err == nil {
		t.Fatal("empty snapshot did not fail")
	}
}

// A non-mining RPC node with the sequencer enabled has no miner pending, and
// between importing a block and receiving the next open the consumer has no
// active entry. The backend must then serve the consumer's head fallback so
// pending state stays available instead of returning -32000; without it,
// go-ethereum bind's default eth_getCode(addr,"pending") gas preflight fails
// for ~150ms every block.
func TestSequencerPendingSnapshotHeadFallback(t *testing.T) {
	b := initBackend(false)
	defer b.eth.blockchain.Stop()
	defer b.eth.txPool.Close()

	head := b.eth.blockchain.CurrentBlock()
	statedb, err := b.eth.blockchain.StateAt(head.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	empty := types.NewBlockWithHeader(&types.Header{
		ParentHash: head.Hash(),
		Number:     new(big.Int).Add(head.Number, big.NewInt(1)),
	})

	// No live entry (PendingSnapshot nil), no miner, but a head view is offered.
	b.eth.seqConsumer = &apiSequenceConsumer{index: sequencer.NewIndex(), headBlock: empty, headState: statedb}

	block, _, gotState, err := b.PendingSnapshot(t.Context())
	if err != nil {
		t.Fatalf("head fallback should serve pending, got error: %v", err)
	}
	if block != empty || gotState != statedb {
		t.Fatal("backend did not fall back to the consumer head view")
	}
	// The block accessors must serve the same head view, so
	// eth_getBlockByNumber("pending") does not return null in the gap.
	if pb := b.PendingBlock(); pb != empty {
		t.Fatalf("PendingBlock did not use the head fallback: %v", pb)
	}
	if pb, _ := b.PendingBlockAndReceipts(); pb != empty {
		t.Fatalf("PendingBlockAndReceipts did not use the head fallback: %v", pb)
	}
}
