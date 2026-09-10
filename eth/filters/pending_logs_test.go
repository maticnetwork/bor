package filters

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/filtermaps"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

type pendingSnapshotTestBackend struct {
	*testBackend
	pendingAnchor        *types.Header
	pendingBlocks        []*types.Block
	pendingRangeReceipts []types.Receipts
}

type singlePendingSnapshotTestBackend struct {
	*testBackend
}

func (b *singlePendingSnapshotTestBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return b.pendingBlock, b.pendingReceipts
}

type prunedPendingSnapshotTestBackend struct {
	*pendingSnapshotTestBackend
	cutoff uint64
}

type pendingHeaderErrorTestBackend struct {
	*testBackend
	err error
}

func (b *pendingHeaderErrorTestBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if number == rpc.PendingBlockNumber {
		return nil, b.err
	}
	return b.testBackend.HeaderByNumber(ctx, number)
}

type canonicalHeaderErrorTestBackend struct {
	*pendingSnapshotTestBackend
	err error
}

func (b *canonicalHeaderErrorTestBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	if number >= 0 {
		return nil, b.err
	}
	return b.pendingSnapshotTestBackend.testBackend.HeaderByNumber(ctx, number)
}

func (b *prunedPendingSnapshotTestBackend) HistoryPruningCutoff() uint64 {
	return b.cutoff
}

func (b *pendingSnapshotTestBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return b.pendingBlock, b.pendingReceipts
}

func (b *pendingSnapshotTestBackend) PendingLogRange() (*types.Header, []*types.Block, []types.Receipts) {
	return b.pendingAnchor, b.pendingBlocks, b.pendingRangeReceipts
}

func TestGetLogsReadsPendingView(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, _ := newTestFilterSystem(db, Config{})
	rangeBackend := &pendingSnapshotTestBackend{testBackend: backend}
	api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)

	wanted := common.HexToAddress("0x1111111111111111111111111111111111111111")
	ignored := common.HexToAddress("0x2222222222222222222222222222222222222222")
	genesis := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
	anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: genesis.Hash(), Time: 2})
	rawdb.WriteBlock(db, genesis)
	rawdb.WriteCanonicalHash(db, genesis.Hash(), 0)
	rawdb.WriteBlock(db, anchor)
	rawdb.WriteCanonicalHash(db, anchor.Hash(), 1)
	rawdb.WriteHeadBlockHash(db, anchor.Hash())
	backend.startFilterMaps(0, false, filtermaps.DefaultParams)
	defer backend.stopFilterMaps()
	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(2), ParentHash: anchor.Hash()}).WithBody(types.Body{Transactions: types.Transactions{tx}})
	receipts := types.Receipts{{TxHash: tx.Hash(), Logs: []*types.Log{{Address: wanted}, {Address: ignored}}}}
	backend.setPending(block, receipts)
	rangeBackend.pendingAnchor = anchor.Header()
	rangeBackend.pendingBlocks = []*types.Block{block}
	rangeBackend.pendingRangeReceipts = []types.Receipts{receipts}
	backend.pendingError = errors.New("split pending read")

	for _, crit := range []FilterCriteria{
		{
			FromBlock: big.NewInt(rpc.PendingBlockNumber.Int64()),
			ToBlock:   big.NewInt(rpc.PendingBlockNumber.Int64()),
			Addresses: []common.Address{wanted},
		},
		{Pending: true, Addresses: []common.Address{wanted}},
		{FromBlock: big.NewInt(1), ToBlock: big.NewInt(rpc.PendingBlockNumber.Int64()), Addresses: []common.Address{wanted}},
		{ToBlock: big.NewInt(rpc.PendingBlockNumber.Int64()), Addresses: []common.Address{wanted}},
	} {
		logs, err := api.GetLogs(t.Context(), crit)
		if err != nil {
			t.Fatalf("GetLogs: %v", err)
		}
		if len(logs) != 1 || logs[0].Address != wanted {
			t.Fatalf("pending logs = %v", logs)
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := api.GetLogs(cancelled, FilterCriteria{Pending: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pending logs error = %v", err)
	}
	backend.pendingReceipts = nil
	if _, err := api.GetLogs(t.Context(), FilterCriteria{Pending: true}); !errors.Is(err, errPendingLogsIncomplete) {
		t.Fatalf("incomplete pending logs error = %v", err)
	}
	backend.setPending(block, types.Receipts{{TxHash: common.HexToHash("0xdead")}})
	if _, err := api.GetLogs(t.Context(), FilterCriteria{Pending: true}); !errors.Is(err, errPendingLogsIncomplete) {
		t.Fatalf("mismatched pending receipt error = %v", err)
	}
}

func TestGetFilterLogsReadsPendingView(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, sys := newTestFilterSystem(db, Config{})
	api := NewFilterAPI(sys, true)

	wanted := common.HexToAddress("0x1111111111111111111111111111111111111111")
	ignored := common.HexToAddress("0x2222222222222222222222222222222222222222")
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	backend.setPending(block, types.Receipts{{Logs: []*types.Log{{Address: wanted}, {Address: ignored}}}})

	for _, crit := range []FilterCriteria{
		{Pending: true, Addresses: []common.Address{wanted}},
		{FromBlock: big.NewInt(rpc.PendingBlockNumber.Int64()), Addresses: []common.Address{wanted}},
	} {
		id, err := api.NewFilter(crit)
		if err != nil {
			t.Fatalf("NewFilter: %v", err)
		}
		logs, err := api.GetFilterLogs(t.Context(), id)
		api.UninstallFilter(id)
		if err != nil {
			t.Fatalf("GetFilterLogs: %v", err)
		}
		if len(logs) != 1 || logs[0].Address != wanted {
			t.Fatalf("pending filter logs = %v", logs)
		}
	}
}

func TestPendingLogValidationAndFallback(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, sys := newTestFilterSystem(db, Config{LogQueryLimit: 1})
	api := NewFilterAPI(sys, true)
	logs := make(chan []*types.Log)
	hash := common.HexToHash("0x1")

	tests := []struct {
		name string
		crit ethereum.FilterQuery
		want error
	}{
		{name: "topics", crit: ethereum.FilterQuery{Topics: make([][]common.Hash, maxTopics+1)}, want: errExceedMaxTopics},
		{name: "addresses", crit: ethereum.FilterQuery{Addresses: make([]common.Address, 2)}, want: errExceedLogQueryLimit},
		{name: "subtopics", crit: ethereum.FilterQuery{Topics: [][]common.Hash{{hash, hash}}}, want: errExceedLogQueryLimit},
		{name: "block hash", crit: ethereum.FilterQuery{BlockHash: &hash}, want: errInvalidBlockRange},
		{name: "from block", crit: ethereum.FilterQuery{FromBlock: big.NewInt(1)}, want: errInvalidBlockRange},
		{name: "to block", crit: ethereum.FilterQuery{ToBlock: big.NewInt(1)}, want: errInvalidBlockRange},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := api.events.SubscribePendingLogs(test.crit, logs); err != test.want {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	atLimit := ethereum.FilterQuery{
		Addresses: []common.Address{{}},
		Topics:    make([][]common.Hash, maxTopics),
	}
	for i := range atLimit.Topics {
		atLimit.Topics[i] = []common.Hash{hash}
	}
	sub, err := api.events.SubscribePendingLogs(atLimit, logs)
	if err != nil {
		t.Fatalf("criteria at limits: %v", err)
	}
	sub.Unsubscribe()

	pending := big.NewInt(rpc.PendingBlockNumber.Int64())
	invalidRanges := []FilterCriteria{
		{BlockHash: &hash, FromBlock: pending},
		{FromBlock: pending, ToBlock: big.NewInt(1)},
		{Pending: true, ToBlock: pending},
	}
	for _, crit := range invalidRanges {
		if _, err := api.GetLogs(t.Context(), crit); err != errInvalidBlockRange {
			t.Fatalf("invalid pending range error = %v", err)
		}
	}
	if _, err := api.GetLogs(t.Context(), FilterCriteria{FromBlock: pending}); err != errPendingLogsUnsupported {
		t.Fatalf("missing pending view error = %v", err)
	}
	for _, crit := range []FilterCriteria{
		{FromBlock: big.NewInt(1), ToBlock: pending},
		{ToBlock: pending},
	} {
		if _, err := api.GetLogs(t.Context(), crit); err != errPendingLogsUnsupported {
			t.Fatalf("missing pending range error = %v", err)
		}
	}

	backend.setPending(types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)}), nil)
	backend.pendingError = errors.New("pending receipts unavailable")
	if _, err := api.GetLogs(t.Context(), FilterCriteria{FromBlock: pending}); err != backend.pendingError {
		t.Fatalf("pending receipt error = %v", err)
	}
	backend.pendingError = nil
	backend.pendingReceipts = types.Receipts{nil}
	if _, err := api.GetLogs(t.Context(), FilterCriteria{FromBlock: pending}); !errors.Is(err, errPendingLogsIncomplete) {
		t.Fatalf("nil pending receipt error = %v", err)
	}
}

func TestGetLogsThroughPendingRejectsFutureRange(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, _ := newTestFilterSystem(db, Config{})
	rangeBackend := &pendingSnapshotTestBackend{testBackend: backend}
	api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
	anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()})
	rangeBackend.pendingAnchor = anchor.Header()
	rangeBackend.pendingBlocks = []*types.Block{block}
	rangeBackend.pendingRangeReceipts = []types.Receipts{nil}

	_, err := api.GetLogs(t.Context(), FilterCriteria{
		FromBlock: big.NewInt(2),
		ToBlock:   big.NewInt(rpc.PendingBlockNumber.Int64()),
	})
	if err != errInvalidBlockRange {
		t.Fatalf("future pending range error = %v, want %v", err, errInvalidBlockRange)
	}
}

func TestGetLogsThroughPendingIncludesEveryPendingBlock(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, _ := newTestFilterSystem(db, Config{})
	rangeBackend := &pendingSnapshotTestBackend{testBackend: backend}
	api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
	anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
	rawdb.WriteBlock(db, anchor)
	rawdb.WriteCanonicalHash(db, anchor.Hash(), 0)
	rawdb.WriteHeadBlockHash(db, anchor.Hash())

	address := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tx1 := types.NewTransaction(0, common.Address{}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	block1 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()}).WithBody(types.Body{Transactions: types.Transactions{tx1}})
	tx2 := types.NewTransaction(1, common.Address{}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	block2 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(2), ParentHash: block1.Hash()}).WithBody(types.Body{Transactions: types.Transactions{tx2}})
	receipts1 := types.Receipts{{TxHash: tx1.Hash(), Logs: []*types.Log{{Address: address, BlockNumber: 1}}}}
	receipts2 := types.Receipts{{TxHash: tx2.Hash(), Logs: []*types.Log{{Address: address, BlockNumber: 2}}}}
	rangeBackend.pendingAnchor = anchor.Header()
	rangeBackend.pendingBlocks = []*types.Block{block1, block2}
	rangeBackend.pendingRangeReceipts = []types.Receipts{receipts1, receipts2}

	logs, err := api.GetLogs(t.Context(), FilterCriteria{
		FromBlock: big.NewInt(1),
		ToBlock:   big.NewInt(rpc.PendingBlockNumber.Int64()),
		Addresses: []common.Address{address},
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 2 || logs[0].BlockNumber != 1 || logs[1].BlockNumber != 2 {
		t.Fatalf("pending range logs = %v", logs)
	}
}

func TestGetLogsThroughPendingCombinesCanonicalAndPending(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, _ := newTestFilterSystem(db, Config{})
	rangeBackend := &pendingSnapshotTestBackend{testBackend: backend}
	api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
	address := common.HexToAddress("0x1111111111111111111111111111111111111111")
	genesis := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
	canonicalTx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	canonicalReceipt := &types.Receipt{
		TxHash: canonicalTx.Hash(), BlockNumber: big.NewInt(1), Status: types.ReceiptStatusSuccessful,
		Logs: []*types.Log{{Address: address, BlockNumber: 1, TxHash: canonicalTx.Hash()}},
	}
	canonicalHeader := &types.Header{
		Number: big.NewInt(1), ParentHash: genesis.Hash(), Time: 2,
		Bloom: types.CreateBloom(canonicalReceipt),
	}
	canonical := types.NewBlockWithHeader(canonicalHeader).WithBody(types.Body{Transactions: types.Transactions{canonicalTx}})
	canonicalReceipt.BlockHash = canonical.Hash()
	canonicalReceipt.Logs[0].BlockHash = canonical.Hash()
	for _, block := range []*types.Block{genesis, canonical} {
		rawdb.WriteBlock(db, block)
		rawdb.WriteCanonicalHash(db, block.Hash(), block.NumberU64())
	}
	rawdb.WriteHeadBlockHash(db, canonical.Hash())
	rawdb.WriteReceipts(db, canonical.Hash(), canonical.NumberU64(), types.Receipts{canonicalReceipt})
	backend.startFilterMaps(0, false, filtermaps.DefaultParams)
	defer backend.stopFilterMaps()

	pendingTx := types.NewTransaction(1, common.Address{}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	pending := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(2), ParentHash: canonical.Hash()}).WithBody(types.Body{Transactions: types.Transactions{pendingTx}})
	pendingReceipts := types.Receipts{{
		TxHash: pendingTx.Hash(), BlockNumber: big.NewInt(2), Status: types.ReceiptStatusSuccessful,
		Logs: []*types.Log{{Address: address, BlockNumber: 2, TxHash: pendingTx.Hash()}},
	}}
	rangeBackend.pendingAnchor = canonical.Header()
	rangeBackend.pendingBlocks = []*types.Block{pending}
	rangeBackend.pendingRangeReceipts = []types.Receipts{pendingReceipts}

	logs, err := api.GetLogs(t.Context(), FilterCriteria{
		FromBlock: big.NewInt(1),
		ToBlock:   big.NewInt(rpc.PendingBlockNumber.Int64()),
		Addresses: []common.Address{address},
	})
	if err != nil {
		t.Fatalf("GetLogs: %v", err)
	}
	if len(logs) != 2 || logs[0].BlockNumber != 1 || logs[1].BlockNumber != 2 {
		t.Fatalf("canonical and pending logs = %v", logs)
	}
}

func TestGetLogsThroughPendingRejectsGapAndFullRangeLimit(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, _ := newTestFilterSystem(db, Config{})
	rangeBackend := &pendingSnapshotTestBackend{testBackend: backend}
	anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), Time: 1})
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(12), ParentHash: anchor.Hash()})
	rangeBackend.pendingAnchor = anchor.Header()
	rangeBackend.pendingBlocks = []*types.Block{block}
	rangeBackend.pendingRangeReceipts = []types.Receipts{nil}

	api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
	if _, err := api.GetLogs(t.Context(), FilterCriteria{FromBlock: big.NewInt(10), ToBlock: big.NewInt(rpc.PendingBlockNumber.Int64())}); !errors.Is(err, errPendingLogsIncomplete) {
		t.Fatalf("gap error = %v", err)
	}

	block11 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(11), ParentHash: anchor.Hash()})
	block12 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(12), ParentHash: block11.Hash()})
	rangeBackend.pendingBlocks = []*types.Block{block11, block12}
	rangeBackend.pendingRangeReceipts = []types.Receipts{nil, nil}
	api = NewFilterAPI(NewFilterSystem(rangeBackend, Config{RangeLimit: 1}), true)
	if _, err := api.GetLogs(t.Context(), FilterCriteria{FromBlock: big.NewInt(10), ToBlock: big.NewInt(rpc.PendingBlockNumber.Int64())}); err != errExceedBlockRangeLimit {
		t.Fatalf("range limit error = %v", err)
	}
}

func TestPendingRangeStartUsesPruningCutoffForEarliest(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	backend, _ := newTestFilterSystem(db, Config{})
	pruned := &prunedPendingSnapshotTestBackend{
		pendingSnapshotTestBackend: &pendingSnapshotTestBackend{testBackend: backend},
		cutoff:                     7,
	}
	api := NewFilterAPI(NewFilterSystem(pruned, Config{}), true)
	anchor := &types.Header{Number: big.NewInt(10)}

	from, err := api.pendingRangeStart(t.Context(), FilterCriteria{FromBlock: big.NewInt(rpc.EarliestBlockNumber.Int64())}, anchor)
	if err != nil || from != 7 {
		t.Fatalf("earliest pending range start = %d, %v", from, err)
	}
}

func TestGetLogsThroughPendingRetryAndErrorPaths(t *testing.T) {
	pending := big.NewInt(rpc.PendingBlockNumber.Int64())

	t.Run("invalid criteria", func(t *testing.T) {
		_, sys := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		api := NewFilterAPI(sys, true)
		if _, err := api.getLogsThroughPending(t.Context(), FilterCriteria{Pending: true}); err != errInvalidBlockRange {
			t.Fatalf("error = %v, want %v", err, errInvalidBlockRange)
		}
	})

	t.Run("anchor changes twice", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
		block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()})
		rangeBackend := &pendingSnapshotTestBackend{
			testBackend:          backend,
			pendingAnchor:        anchor.Header(),
			pendingBlocks:        []*types.Block{block},
			pendingRangeReceipts: []types.Receipts{nil},
		}
		api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
		_, err := api.getLogsThroughPending(t.Context(), FilterCriteria{FromBlock: big.NewInt(1), ToBlock: pending})
		if err != errPendingLogsIncomplete {
			t.Fatalf("error = %v, want %v", err, errPendingLogsIncomplete)
		}
	})

	t.Run("canonical header read fails", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
		block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()})
		wantErr := errors.New("canonical header unavailable")
		rangeBackend := &canonicalHeaderErrorTestBackend{
			pendingSnapshotTestBackend: &pendingSnapshotTestBackend{
				testBackend:          backend,
				pendingAnchor:        anchor.Header(),
				pendingBlocks:        []*types.Block{block},
				pendingRangeReceipts: []types.Receipts{nil},
			},
			err: wantErr,
		}
		api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
		_, _, err := api.getLogsThroughPendingSnapshot(t.Context(), FilterCriteria{FromBlock: big.NewInt(1), ToBlock: pending})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	})

	t.Run("range start fails", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
		block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()})
		rangeBackend := &pendingSnapshotTestBackend{
			testBackend:          backend,
			pendingAnchor:        anchor.Header(),
			pendingBlocks:        []*types.Block{block},
			pendingRangeReceipts: []types.Receipts{nil},
		}
		api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
		_, _, err := api.getLogsThroughPendingSnapshot(t.Context(), FilterCriteria{
			FromBlock: big.NewInt(rpc.SafeBlockNumber.Int64()),
			ToBlock:   pending,
		})
		if err == nil {
			t.Fatal("expected safe-header lookup error")
		}
	})

	t.Run("canonical history is pruned", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), Time: 1})
		block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(11), ParentHash: anchor.Hash()})
		pruned := &prunedPendingSnapshotTestBackend{
			pendingSnapshotTestBackend: &pendingSnapshotTestBackend{
				testBackend:          backend,
				pendingAnchor:        anchor.Header(),
				pendingBlocks:        []*types.Block{block},
				pendingRangeReceipts: []types.Receipts{nil},
			},
			cutoff: 2,
		}
		api := NewFilterAPI(NewFilterSystem(pruned, Config{}), true)
		if _, _, err := api.getLogsThroughPendingSnapshot(t.Context(), FilterCriteria{FromBlock: big.NewInt(1), ToBlock: pending}); err == nil {
			t.Fatal("expected pruned-history error")
		}
	})

	t.Run("skips pending blocks below start", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		backend, _ := newTestFilterSystem(db, Config{})
		anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
		block1 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()})
		block2 := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(2), ParentHash: block1.Hash()})
		rawdb.WriteBlock(db, anchor)
		rawdb.WriteCanonicalHash(db, anchor.Hash(), 0)
		rawdb.WriteHeadBlockHash(db, anchor.Hash())
		rangeBackend := &pendingSnapshotTestBackend{
			testBackend:          backend,
			pendingAnchor:        anchor.Header(),
			pendingBlocks:        []*types.Block{block1, block2},
			pendingRangeReceipts: []types.Receipts{nil, nil},
		}
		api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
		logs, retry, err := api.getLogsThroughPendingSnapshot(t.Context(), FilterCriteria{FromBlock: big.NewInt(2), ToBlock: pending})
		if err != nil || retry || len(logs) != 0 {
			t.Fatalf("logs, retry, error = %v, %t, %v", logs, retry, err)
		}
	})
}

func TestPendingRangeStartVariants(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	_, sys := newTestFilterSystem(db, Config{})
	api := NewFilterAPI(sys, true)
	anchor := &types.Header{Number: big.NewInt(10)}
	finalized := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(7), Time: 1})
	rawdb.WriteBlock(db, finalized)
	rawdb.WriteFinalizedBlockHash(db, finalized.Hash())

	tests := []struct {
		name    string
		from    rpc.BlockNumber
		want    uint64
		wantErr bool
	}{
		{name: "latest", from: rpc.LatestBlockNumber, want: 10},
		{name: "finalized", from: rpc.FinalizedBlockNumber, want: 7},
		{name: "safe backend error", from: rpc.SafeBlockNumber, wantErr: true},
		{name: "unsupported special number", from: rpc.PendingBlockNumber, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := api.pendingRangeStart(t.Context(), FilterCriteria{FromBlock: big.NewInt(test.from.Int64())}, anchor)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("start, error = %d, %v; want %d, error=%t", got, err, test.want, test.wantErr)
			}
		})
	}

	rawdb.WriteFinalizedBlockHash(db, common.Hash{})
	if _, err := api.pendingRangeStart(t.Context(), FilterCriteria{FromBlock: big.NewInt(rpc.FinalizedBlockNumber.Int64())}, anchor); !errors.Is(err, ethereum.NotFound) {
		t.Fatalf("missing finalized header error = %v", err)
	}
}

func TestPendingLogRangeFallbacks(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		_, sys := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		api := NewFilterAPI(sys, true)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, _, _, err := api.pendingLogRange(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want %v", err, context.Canceled)
		}
	})

	t.Run("empty range snapshot", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		rangeBackend := &pendingSnapshotTestBackend{testBackend: backend, pendingAnchor: &types.Header{Number: big.NewInt(0)}}
		api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
		if _, _, _, err := api.pendingLogRange(t.Context()); err != errPendingLogsUnsupported {
			t.Fatalf("error = %v, want %v", err, errPendingLogsUnsupported)
		}
	})

	t.Run("pending header error", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		wantErr := errors.New("pending header unavailable")
		errorBackend := &pendingHeaderErrorTestBackend{testBackend: backend, err: wantErr}
		api := NewFilterAPI(NewFilterSystem(errorBackend, Config{}), true)
		if _, _, _, err := api.pendingLogRange(t.Context()); !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
	})

	t.Run("missing canonical anchor", func(t *testing.T) {
		backend, sys := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		backend.setPending(types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)}), nil)
		api := NewFilterAPI(sys, true)
		if _, _, _, err := api.pendingLogRange(t.Context()); err != errPendingLogsUnsupported {
			t.Fatalf("error = %v, want %v", err, errPendingLogsUnsupported)
		}
	})

	t.Run("receipt error", func(t *testing.T) {
		backend, sys := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		backend.setPending(types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)}), nil)
		backend.pendingError = errors.New("pending receipts unavailable")
		api := NewFilterAPI(sys, true)
		if _, _, _, err := api.pendingLogRange(t.Context()); !errors.Is(err, backend.pendingError) {
			t.Fatalf("error = %v, want %v", err, backend.pendingError)
		}
	})

	t.Run("fallback snapshot", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		backend, _ := newTestFilterSystem(db, Config{})
		anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(0), Time: 1})
		block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), ParentHash: anchor.Hash()})
		rawdb.WriteBlock(db, anchor)
		rawdb.WriteCanonicalHash(db, anchor.Hash(), 0)
		rawdb.WriteHeadBlockHash(db, anchor.Hash())
		backend.setPending(block, nil)
		snapshotBackend := &singlePendingSnapshotTestBackend{testBackend: backend}
		api := NewFilterAPI(NewFilterSystem(snapshotBackend, Config{}), true)
		gotAnchor, blocks, receipts, err := api.pendingLogRange(t.Context())
		if err != nil || gotAnchor.Hash() != anchor.Hash() || len(blocks) != 1 || blocks[0].Hash() != block.Hash() || len(receipts) != 1 || receipts[0] != nil {
			t.Fatalf("anchor, blocks, receipts, error = %v, %v, %v, %v", gotAnchor, blocks, receipts, err)
		}
	})

	t.Run("snapshot backend has no block", func(t *testing.T) {
		backend, _ := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
		rangeBackend := &pendingSnapshotTestBackend{testBackend: backend}
		api := NewFilterAPI(NewFilterSystem(rangeBackend, Config{}), true)
		if _, _, err := api.pendingBlockReceipts(t.Context()); err != errPendingLogsUnsupported {
			t.Fatalf("error = %v, want %v", err, errPendingLogsUnsupported)
		}
	})
}

func TestValidatePendingLogRangeFailures(t *testing.T) {
	anchor := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), Time: 1})
	stale := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10)})
	tx := types.NewTransaction(0, common.Address{}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(11), ParentHash: anchor.Hash()}).WithBody(types.Body{Transactions: types.Transactions{tx}})

	tests := []struct {
		name     string
		anchor   *types.Header
		blocks   []*types.Block
		receipts []types.Receipts
	}{
		{name: "nil anchor", blocks: []*types.Block{block}, receipts: []types.Receipts{nil}},
		{name: "nil anchor number", anchor: &types.Header{}, blocks: []*types.Block{block}, receipts: []types.Receipts{nil}},
		{name: "empty blocks", anchor: anchor.Header()},
		{name: "receipt count mismatch", anchor: anchor.Header(), blocks: []*types.Block{block}},
		{name: "all blocks already canonical", anchor: anchor.Header(), blocks: []*types.Block{stale}, receipts: []types.Receipts{nil}},
		{name: "nil block", anchor: anchor.Header(), blocks: []*types.Block{nil}, receipts: []types.Receipts{nil}},
		{name: "invalid receipt", anchor: anchor.Header(), blocks: []*types.Block{block}, receipts: []types.Receipts{types.Receipts{{TxHash: common.HexToHash("0xdead")}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := validatePendingLogRange(test.anchor, test.blocks, test.receipts); err != errPendingLogsIncomplete {
				t.Fatalf("error = %v, want %v", err, errPendingLogsIncomplete)
			}
		})
	}
}

func TestOnlyPendingRangeRejectsConcreteStart(t *testing.T) {
	if onlyPendingRange(FilterCriteria{FromBlock: big.NewInt(1)}) {
		t.Fatal("concrete start unexpectedly accepted as a pending-only range")
	}
}
