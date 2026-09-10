package sequencer

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
)

func publishPendingSnapshot(t *testing.T, s *session) {
	t.Helper()
	block, payload, ok := preparePending(s.env, s.env.header, common.Hash{}, nil)
	if !ok {
		t.Fatal("prepare pending snapshot failed")
	}
	s.consumer.publishMu.Lock()
	defer s.consumer.publishMu.Unlock()
	if !s.consumer.publishPending(block, payload, s.env.generation) {
		t.Fatal("publish pending snapshot failed")
	}
	s.env.markPendingPublished(time.Now())
}

func TestPendingHelperNilInputs(t *testing.T) {
	if removedLogs(nil) != nil {
		t.Fatal("nil view returned logs")
	}
	if cloneProcessResult(nil) != nil {
		t.Fatal("nil process result returned a clone")
	}
	if cloneReceipt(nil) != nil {
		t.Fatal("nil receipt returned a clone")
	}
	if block, err := blockFromExecution(nil, nil, nil); err == nil || block != nil {
		t.Fatalf("nil header returned block %v and error %v", block, err)
	}
}

func TestCloneProcessResultDeepCopy(t *testing.T) {
	source := &core.ProcessResult{
		Receipts: types.Receipts{{
			PostState: []byte{1},
			Logs: []*types.Log{{
				Topics: []common.Hash{{2}},
				Data:   []byte{3},
			}},
		}},
		Requests: [][]byte{{4}},
		Logs: []*types.Log{{
			Topics: []common.Hash{{5}},
			Data:   []byte{6},
		}},
		GasUsed: 7,
	}

	clone := cloneProcessResult(source)
	clone.Receipts[0].PostState[0] = 8
	clone.Receipts[0].Logs[0].Topics[0][0] = 9
	clone.Receipts[0].Logs[0].Data[0] = 10
	clone.Requests[0][0] = 11
	clone.Logs[0].Topics[0][0] = 12
	clone.Logs[0].Data[0] = 13

	if source.Receipts[0].PostState[0] != 1 || source.Receipts[0].Logs[0].Topics[0][0] != 2 || source.Receipts[0].Logs[0].Data[0] != 3 {
		t.Fatal("receipt clone shares backing storage")
	}
	if source.Requests[0][0] != 4 || source.Logs[0].Topics[0][0] != 5 || source.Logs[0].Data[0] != 6 {
		t.Fatal("process result clone shares backing storage")
	}
	if clone.GasUsed != source.GasUsed {
		t.Fatalf("gas used = %d, want %d", clone.GasUsed, source.GasUsed)
	}
}

func TestCloneProcessResultPreservesEmptyTopics(t *testing.T) {
	source := &core.ProcessResult{
		Receipts: types.Receipts{{Logs: []*types.Log{{Topics: []common.Hash{}}}}},
		Logs:     []*types.Log{{Topics: []common.Hash{}}},
	}
	clone := cloneProcessResult(source)

	logs := map[string]*types.Log{
		"receipt": clone.Receipts[0].Logs[0],
		"result":  clone.Logs[0],
	}
	for name, entry := range logs {
		if entry.Topics == nil {
			t.Fatalf("%s topics are nil", name)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal %s log: %v", name, err)
		}
		if !bytes.Contains(encoded, []byte(`"topics":[]`)) {
			t.Fatalf("%s log = %s, want empty topics array", name, encoded)
		}
	}
}

func TestPreparePendingOwnsRPCSnapshot(t *testing.T) {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	address := common.Address{1}
	statedb.SetNonce(address, 7, tracing.NonceChangeUnspecified)
	tx := types.NewTransaction(0, address, big.NewInt(1), 21_000, big.NewInt(1), nil)
	sourceHash := common.Hash{2}
	receipt := &types.Receipt{
		TxHash:            tx.Hash(),
		BlockHash:         sourceHash,
		BlockNumber:       big.NewInt(1),
		Status:            types.ReceiptStatusSuccessful,
		GasUsed:           21_000,
		CumulativeGasUsed: 21_000,
		PostState:         []byte{3},
		Logs: []*types.Log{{
			BlockHash: sourceHash,
			TxHash:    tx.Hash(),
			Topics:    []common.Hash{{4}},
			Data:      []byte{5},
		}},
	}
	env := &blockEnv{
		header:   &types.Header{Number: big.NewInt(1), GasLimit: 30_000_000, GasUsed: 21_000},
		statedb:  statedb,
		txs:      types.Transactions{tx},
		receipts: types.Receipts{receipt},
	}
	block, unsealed, ok := preparePending(env, env.header, common.Hash{}, nil)
	if !ok {
		t.Fatal("prepare unsealed view")
	}
	unsealedReceipt := unsealed.view.Receipts[tx.Hash()]
	repeated, ok := preparePendingPayload(env, block, common.Hash{}, nil)
	if !ok || repeated.view.Receipts[tx.Hash()] != unsealedReceipt {
		t.Fatal("unchanged receipt prefix was copied again")
	}
	nextTx := types.NewTransaction(1, address, big.NewInt(1), 21_000, big.NewInt(1), nil)
	nextReceipt := &types.Receipt{
		TxHash:            nextTx.Hash(),
		BlockNumber:       big.NewInt(1),
		Status:            types.ReceiptStatusSuccessful,
		GasUsed:           21_000,
		CumulativeGasUsed: 42_000,
		PostState:         []byte{10},
		Logs:              []*types.Log{{TxHash: nextTx.Hash(), Data: []byte{11}}},
	}
	env.txs = append(env.txs, nextTx)
	env.receipts = append(env.receipts, nextReceipt)
	env.header.GasUsed = 42_000
	block, extended, ok := preparePending(env, env.header, common.Hash{}, nil)
	if !ok {
		t.Fatal("prepare extended view")
	}
	if extended.view.Receipts[tx.Hash()] != unsealedReceipt {
		t.Fatal("extended view copied the existing receipt prefix")
	}
	if got := extended.view.Receipts[nextTx.Hash()]; got == nextReceipt || got.PostState[0] != 10 {
		t.Fatal("extended view did not own the new receipt suffix")
	}
	if len(extended.view.Logs) != 2 || extended.view.Logs[0] != unsealedReceipt.Logs[0] ||
		extended.view.Logs[1] != extended.view.Receipts[nextTx.Hash()].Logs[0] {
		t.Fatal("extended view changed log ordering")
	}
	sealedHash := common.Hash{6}
	sealed, ok := preparePendingPayload(env, block, sealedHash, nil)
	if !ok {
		t.Fatal("prepare sealed view")
	}
	if sealed.view.Block != block {
		t.Fatal("sealed payload rebuilt the assembled block")
	}
	if unsealed.finalized || !sealed.finalized {
		t.Fatalf("finalized markers = %v, %v", unsealed.finalized, sealed.finalized)
	}
	sealedReceipt := sealed.view.Receipts[tx.Hash()]
	if receipt.BlockHash != sourceHash || receipt.Logs[0].BlockHash != sourceHash {
		t.Fatal("preparing a view mutated the execution receipt")
	}
	if unsealedReceipt.BlockHash != (common.Hash{}) || unsealedReceipt.Logs[0].BlockHash != (common.Hash{}) {
		t.Fatal("unsealed view retained a provisional block hash")
	}
	if sealedReceipt.BlockHash != sealedHash || sealedReceipt.Logs[0].BlockHash != sealedHash {
		t.Fatal("sealed view did not receive the sealed block hash")
	}
	if sealedReceipt == unsealedReceipt {
		t.Fatal("sealed view reused an unsealed receipt")
	}

	receipt.PostState[0] = 7
	receipt.Logs[0].Topics[0][0] = 8
	receipt.Logs[0].Data[0] = 9
	nextReceipt.PostState[0] = 12
	nextReceipt.Logs[0].Data[0] = 13
	statedb.SetNonce(address, 10, tracing.NonceChangeUnspecified)
	for name, view := range map[string]*PendingRPCView{"unsealed": unsealed.view, "extended": extended.view, "sealed": sealed.view} {
		got := view.Receipts[tx.Hash()]
		if got.PostState[0] != 3 || got.Logs[0].Topics[0][0] != 4 || got.Logs[0].Data[0] != 5 {
			t.Fatalf("%s view shares receipt storage with execution", name)
		}
		if view.State.GetNonce(address) != 7 {
			t.Fatalf("%s view shares mutable execution state", name)
		}
	}
	for name, view := range map[string]*PendingRPCView{"extended": extended.view, "sealed": sealed.view} {
		got := view.Receipts[nextTx.Hash()]
		if got.PostState[0] != 10 || got.Logs[0].Data[0] != 11 {
			t.Fatalf("%s view shares new receipt storage with execution", name)
		}
	}
	returned := receiptsFromView(unsealed.view.Block, unsealed.view)
	returned[0].PostState[0] = 11
	if unsealed.view.Receipts[tx.Hash()].PostState[0] != 3 {
		t.Fatal("receipt returned to a caller mutated the stored view")
	}
}

func TestPendingComparisonsRejectMismatches(t *testing.T) {
	txA := types.NewTransaction(0, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	txB := types.NewTransaction(1, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	base := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), TxHash: common.Hash{1}})
	one := base.WithBody(types.Body{Transactions: types.Transactions{txA}})
	empty := base.WithBody(types.Body{})
	different := base.WithBody(types.Body{Transactions: types.Transactions{txB}})

	if sameTransactions(one, empty) {
		t.Fatal("blocks with different transaction counts matched")
	}
	if sameTransactions(one, different) {
		t.Fatal("blocks with different transactions matched")
	}
	receipt := &types.Receipt{TxHash: txA.Hash(), Status: types.ReceiptStatusSuccessful}
	view := &PendingRPCView{Receipts: map[common.Hash]*types.Receipt{txA.Hash(): receipt}}
	if sameReceipts(one, view, nil) {
		t.Fatal("receipt slices with different lengths matched")
	}
}

func TestSameConsensusReceipts(t *testing.T) {
	receipt := &types.Receipt{
		Type:              types.DynamicFeeTxType,
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: 42,
		Bloom:             types.Bloom{1},
		Logs: []*types.Log{{
			Address: common.Address{2},
			Topics:  []common.Hash{{3}},
			Data:    []byte{4},
		}},
		TxHash:    common.Hash{5},
		BlockHash: common.Hash{6},
	}
	canonical := cloneReceipt(receipt)
	canonical.TxHash = common.Hash{7}
	canonical.BlockHash = common.Hash{8}
	if !sameConsensusReceipts(types.Receipts{receipt}, types.Receipts{canonical}) {
		t.Fatal("derived receipt fields changed consensus equality")
	}

	mutations := []struct {
		name   string
		mutate func(*types.Receipt)
	}{
		{"type", func(copy *types.Receipt) { copy.Type++ }},
		{"status", func(copy *types.Receipt) { copy.Status = types.ReceiptStatusFailed }},
		{"cumulative gas", func(copy *types.Receipt) { copy.CumulativeGasUsed++ }},
		{"bloom", func(copy *types.Receipt) { copy.Bloom[0]++ }},
		{"log address", func(copy *types.Receipt) { copy.Logs[0].Address[0]++ }},
		{"log topic", func(copy *types.Receipt) { copy.Logs[0].Topics[0][0]++ }},
		{"log data", func(copy *types.Receipt) { copy.Logs[0].Data[0]++ }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := cloneReceipt(receipt)
			mutation.mutate(changed)
			if sameConsensusReceipts(types.Receipts{receipt}, types.Receipts{changed}) {
				t.Fatal("consensus receipt mismatch was accepted")
			}
		})
	}
}

func TestPendingStateReaderAccessors(t *testing.T) {
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	address := common.Address{1}
	slot := common.Hash{2}
	value := common.Hash{3}
	code := []byte{4, 5}
	statedb.SetBalance(address, uint256.NewInt(6), tracing.BalanceChangeUnspecified)
	statedb.SetNonce(address, 7, tracing.NonceChangeUnspecified)
	statedb.SetCode(address, code, tracing.CodeChangeUnspecified)
	statedb.SetState(address, slot, value)
	reader := &pendingStateReader{state: statedb}

	balance := reader.GetBalance(address)
	if balance.Uint64() != 6 || reader.GetNonce(address) != 7 || reader.GetStorage(address, slot) != value {
		t.Fatal("state reader returned incorrect account data")
	}
	balance.SetUint64(8)
	if reader.GetBalance(address).Uint64() != 6 {
		t.Fatal("balance mutation leaked into state")
	}
	returnedCode := reader.GetCode(address)
	returnedCode[0] = 9
	if reader.GetCode(address)[0] != 4 {
		t.Fatal("code mutation leaked into state")
	}
	copy, err := reader.NewStateDB()
	if err != nil {
		t.Fatalf("copy state: %v", err)
	}
	copy.SetNonce(address, 10, tracing.NonceChangeUnspecified)
	if reader.GetNonce(address) != 7 {
		t.Fatal("state copy mutation leaked into source")
	}
}

type failingStateReader struct {
	err error
}

func (r failingStateReader) Account(common.Address) (*types.StateAccount, error) {
	return nil, r.err
}

func (r failingStateReader) Storage(common.Address, common.Hash) (common.Hash, error) {
	return common.Hash{}, r.err
}

func (r failingStateReader) Code(common.Address, common.Hash) ([]byte, error) {
	return nil, r.err
}

func (r failingStateReader) CodeSize(common.Address, common.Hash) (int, error) {
	return 0, r.err
}

func TestPendingStateReaderRejectsErroredState(t *testing.T) {
	failure := errors.New("state read failed")
	statedb, err := state.NewWithReader(types.EmptyRootHash, state.NewDatabaseForTesting(), failingStateReader{err: failure})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	statedb.GetBalance(common.Address{1})

	if copy, err := (&pendingStateReader{state: statedb}).NewStateDB(); !errors.Is(err, failure) || copy != nil {
		t.Fatalf("errored state returned copy %v and error %v", copy, err)
	}
}
