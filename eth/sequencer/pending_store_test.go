package sequencer

import (
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie"
)

type blockingPendingStateReader struct {
	PendingStateReader
	started chan struct{}
	release chan struct{}
}

func (r *blockingPendingStateReader) NewStateDB() (*state.StateDB, error) {
	close(r.started)
	<-r.release
	return r.PendingStateReader.NewStateDB()
}

func TestPendingStoreClaimLifecycle(t *testing.T) {
	h := startExecHarness(t)
	session := h.session()
	cur := handleOK(t, session, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x41}))
	tx := h.transfer(t, 0)
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	handleOK(t, session, recordEntry(raw, cur))
	publishPendingSnapshot(t, session)

	block, receipts, statedb := session.consumer.Pending()
	reusable := &ReusableExecution{
		HeaderHash: block.Hash(),
		TxRoot:     block.TxHash(),
		StateDB:    statedb.Copy(),
		Result: &core.ProcessResult{
			Receipts: cloneReceipts(receipts),
			Logs:     append([]*types.Log(nil), receipts[0].Logs...),
			GasUsed:  block.GasUsed(),
		},
	}
	if !session.consumer.pendingStore().publish(block, receipts, statedb, reusable, session.env.generation) {
		t.Fatal("sealed publish failed")
	}
	session.consumer.BeginPreconfImport(block)
	claim, ok := session.consumer.ClaimPreconf(block)
	if !ok || claim == nil || claim.StateDB == reusable.StateDB || claim.Result == reusable.Result {
		t.Fatalf("claim = %+v, ok = %v", claim, ok)
	}
	if !session.consumer.canonicalHandoffMatches(block.Hash(), block.NumberU64()+1) {
		t.Fatal("canonical import did not register the handoff")
	}
	if _, ok := session.consumer.ClaimPreconf(block); ok {
		t.Fatal("claimed execution was returned twice")
	}
	session.consumer.RejectClaimedPreconf(block)
	if _, invalid := session.consumer.pendingStore().CheckPreconfInvalidation(block, receipts); invalid {
		t.Fatal("cache rejection was treated as a canonical mismatch")
	}
	if !session.consumer.pendingStore().publish(block, receipts, statedb, reusable, 0) {
		t.Fatal("fresh execution after rejection was not published")
	}
	if _, invalid := session.consumer.pendingStore().CheckPreconfInvalidation(block, receipts); invalid {
		t.Fatal("fresh execution retained the rejected flag")
	}
	if _, ok := session.consumer.ClaimPreconf(block); !ok {
		t.Fatal("fresh execution after rejection was not reusable")
	}
	session.consumer.CompletePreconf(block, nil, false)
	if session.consumer.canonicalHandoffMatches(block.Hash(), block.NumberU64()+1) {
		t.Fatal("failed import retained the canonical handoff")
	}
	if _, ok := session.consumer.ClaimPreconf(block); !ok {
		t.Fatal("failed import did not restore claim")
	}
	session.consumer.CompletePreconf(block, receipts, true)
	if pending, _, _ := session.consumer.Pending(); pending != nil {
		t.Fatalf("committed entry remained pending: %v", pending)
	}
	if session.consumer.pendingStore().publish(block, receipts, statedb, reusable, session.env.generation) {
		t.Fatal("late worker publication was accepted")
	}
}

func TestCanonicalImportHandoffLifecycleWithoutClaim(t *testing.T) {
	consumer := new(Consumer)
	block := types.NewBlockWithHeader(&types.Header{
		Number:     big.NewInt(1),
		ParentHash: common.HexToHash("0x1234"),
	})

	consumer.BeginPreconfImport(block)
	if !consumer.canonicalHandoffMatches(block.Hash(), block.NumberU64()+1) {
		t.Fatal("canonical import did not register the handoff")
	}
	consumer.CompletePreconf(block, nil, false)
	if consumer.canonicalHandoffMatches(block.Hash(), block.NumberU64()+1) {
		t.Fatal("failed canonical import retained the handoff")
	}
}

func TestPendingStoreStateCopyDoesNotBlockClaim(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), GasLimit: 30_000_000})
	reusable := &ReusableExecution{
		HeaderHash: block.Hash(),
		TxRoot:     block.TxHash(),
		StateDB:    stateDB.Copy(),
		Result:     &core.ProcessResult{},
	}
	store := NewPendingStore(nil)
	claimGeneration := store.begin(block.NumberU64(), block.ParentHash(), true)
	if !store.publish(block, nil, stateDB, reusable, claimGeneration) {
		t.Fatal("publish failed")
	}
	started := make(chan struct{})
	release := make(chan struct{})
	store.mu.Lock()
	entry := store.entries[pendingKey{number: block.NumberU64(), parent: block.ParentHash()}]
	entry.RPCView.State = &blockingPendingStateReader{PendingStateReader: entry.RPCView.State, started: started, release: release}
	store.mu.Unlock()

	pendingDone := make(chan struct{})
	go func() {
		store.Pending()
		close(pendingDone)
	}()
	<-started
	claimDone := make(chan bool, 1)
	go func() {
		_, ok := store.ClaimPreconf(block)
		claimDone <- ok
	}()
	select {
	case ok := <-claimDone:
		if !ok {
			close(release)
			<-pendingDone
			t.Fatal("claim failed while pending state was copied")
		}
	case <-time.After(time.Second):
		close(release)
		<-pendingDone
		<-claimDone
		t.Fatal("pending state copy blocked canonical claim")
	}
	close(release)
	<-pendingDone
}

func TestPendingStoreClaimRequiresExactHeader(t *testing.T) {
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	tx := types.NewTransaction(0, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	block := types.NewBlock(
		&types.Header{Number: big.NewInt(3), ParentHash: common.HexToHash("0x1"), GasLimit: 30_000_000},
		&types.Body{Transactions: types.Transactions{tx}}, nil, trie.NewStackTrie(nil),
	)
	reusable := &ReusableExecution{
		HeaderHash: block.Hash(),
		TxRoot:     block.TxHash(),
		StateDB:    stateDB.Copy(),
		Result:     &core.ProcessResult{},
	}
	for _, test := range []struct {
		name   string
		mutate func(*types.Header)
	}{
		{name: "state root", mutate: func(header *types.Header) { header.Root[0] ^= 0xff }},
		{name: "gas used", mutate: func(header *types.Header) { header.GasUsed++ }},
		{name: "extra data", mutate: func(header *types.Header) { header.Extra = []byte{1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewPendingStore(nil)
			generation := store.begin(block.NumberU64(), block.ParentHash(), true)
			if !store.publish(block, nil, stateDB, reusable, generation) {
				t.Fatal("publish failed")
			}
			header := block.Header()
			test.mutate(header)
			if _, ok := store.ClaimPreconf(block.WithSeal(header)); ok {
				t.Fatal("claim accepted a different sealed header")
			}
		})
	}
}

func TestPendingStoreDefersInvalidationDuringImport(t *testing.T) {
	newClaimedStore := func(t *testing.T) (ethdb.Database, *PendingStore, *types.Block) {
		t.Helper()
		db := rawdb.NewMemoryDatabase()
		store := NewPendingStore(db)
		stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		tx := types.NewTransaction(0, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
		receipts := types.Receipts{{
			TxHash:      tx.Hash(),
			BlockNumber: big.NewInt(3),
			Logs:        []*types.Log{{Address: common.Address{2}}},
		}}
		block := types.NewBlock(
			&types.Header{Number: big.NewInt(3), ParentHash: common.HexToHash("0x1"), GasLimit: 30_000_000},
			&types.Body{Transactions: types.Transactions{tx}}, receipts, trie.NewStackTrie(nil),
		)
		reusable := &ReusableExecution{
			HeaderHash: block.Hash(),
			TxRoot:     block.TxHash(),
			StateDB:    stateDB.Copy(),
			Result:     &core.ProcessResult{Receipts: receipts},
		}
		generation := store.begin(block.NumberU64(), block.ParentHash(), true)
		if !store.publish(block, receipts, stateDB, reusable, generation) {
			t.Fatal("publish failed")
		}
		if _, ok := store.ClaimPreconf(block); !ok {
			t.Fatal("claim failed")
		}
		return db, store, block
	}

	t.Run("successful import", func(t *testing.T) {
		db, store, block := newClaimedStore(t)
		if logs := store.invalidateFrom(0, "session_lost"); len(logs) != 0 {
			t.Fatalf("removed logs before import completion = %v", logs)
		}
		store.mu.RLock()
		entry := store.entries[pendingKey{number: block.NumberU64(), parent: block.ParentHash()}]
		store.mu.RUnlock()
		if entry == nil || entry.phase != PendingImporting {
			t.Fatalf("importing entry = %+v", entry)
		}
		if logs := store.CompletePreconf(block, true); len(logs) != 0 {
			t.Fatalf("removed logs after successful import = %v", logs)
		}
		if records := rawdb.ReadInvalidPreconfs(db, 1); len(records) != 0 {
			t.Fatalf("invalidation records = %+v", records)
		}
	})

	t.Run("aborted import", func(t *testing.T) {
		db, store, block := newClaimedStore(t)
		store.invalidateFrom(0, "session_lost")
		logs := store.CompletePreconf(block, false)
		if len(logs) != 1 || !logs[0].Removed {
			t.Fatalf("removed logs = %+v", logs)
		}
		if records := rawdb.ReadInvalidPreconfs(db, 1); len(records) != 1 || records[0].Number != block.NumberU64() || records[0].Reason != "session_lost" {
			t.Fatalf("invalidation records = %+v", records)
		}
	})

	t.Run("rejected import", func(t *testing.T) {
		db, store, block := newClaimedStore(t)
		store.invalidateFrom(0, "session_lost")
		logs := store.RejectClaimedPreconf(block)
		if len(logs) != 1 || !logs[0].Removed {
			t.Fatalf("removed logs = %+v", logs)
		}
		if pending, _, _ := store.Pending(); pending != nil {
			t.Fatalf("rejected entry remained pending: %v", pending)
		}
		if records := rawdb.ReadInvalidPreconfs(db, 1); len(records) != 1 || records[0].Number != block.NumberU64() || records[0].Reason != "session_lost" {
			t.Fatalf("invalidation records = %+v", records)
		}
	})
}

func TestPendingStoreImportOwnsHeight(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	store := NewPendingStore(db)
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{
		Number:     big.NewInt(3),
		ParentHash: common.HexToHash("0x1"),
		GasLimit:   30_000_000,
	})
	reusable := &ReusableExecution{
		HeaderHash: block.Hash(),
		TxRoot:     block.TxHash(),
		StateDB:    stateDB.Copy(),
		Result:     &core.ProcessResult{},
	}
	claimGeneration := store.begin(block.NumberU64(), block.ParentHash(), true)
	if !store.publish(block, nil, stateDB, reusable, claimGeneration) {
		t.Fatal("publish failed")
	}
	if _, ok := store.ClaimPreconf(block); !ok {
		t.Fatal("claim failed")
	}

	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	store.mu.RLock()
	claimed := store.entries[key]
	store.mu.RUnlock()
	if generation := store.begin(key.number, key.parent, true); generation != claimed.generation {
		t.Fatalf("same-key begin generation = %d, want %d", generation, claimed.generation)
	}
	if store.publish(block, nil, stateDB, reusable, claimed.generation) {
		t.Fatal("same-key publication replaced an importing entry")
	}

	competing := types.NewBlockWithHeader(&types.Header{
		Number:     new(big.Int).Set(block.Number()),
		ParentHash: common.HexToHash("0x2"),
		GasLimit:   block.GasLimit(),
	})
	competingKey := pendingKey{number: competing.NumberU64(), parent: competing.ParentHash()}
	if generation := store.begin(competingKey.number, competingKey.parent, true); generation != claimed.generation {
		t.Fatalf("competing begin generation = %d, want %d", generation, claimed.generation)
	}
	if store.publish(competing, nil, stateDB, nil, 0) {
		t.Fatal("competing publication replaced an importing entry")
	}
	store.mu.RLock()
	gotClaimed := store.entries[key]
	gotCompeting := store.entries[competingKey]
	store.mu.RUnlock()
	if gotClaimed != claimed || claimed.phase != PendingImporting || gotCompeting != nil {
		t.Fatalf("entries changed during import: claimed=%+v competing=%+v", gotClaimed, gotCompeting)
	}
	if records := rawdb.ReadInvalidPreconfs(db, rawdb.InvalidPreconfQueryLimit); len(records) != 0 {
		t.Fatalf("invalidation records during import = %+v", records)
	}

	store.CompletePreconf(block, true)
	generation := store.begin(competingKey.number, competingKey.parent, true)
	if !store.publish(competing, nil, stateDB, nil, generation) {
		t.Fatal("publication remained blocked after import completion")
	}
}

func TestRejectedOpenPreservesImportReceipt(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x73}))
	tx := h.transfer(t, 0)
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	handleOK(t, s, recordEntry(raw, cur))
	block, receipts, statedb := s.consumer.Pending()
	reusable := &ReusableExecution{
		HeaderHash: block.Hash(), TxRoot: block.TxHash(), StateDB: statedb.Copy(),
		Result: &core.ProcessResult{Receipts: cloneReceipts(receipts), GasUsed: block.GasUsed()},
	}
	store := s.consumer.pendingStore()
	if !store.publish(block, receipts, statedb, reusable, s.env.generation) {
		t.Fatal("publish reusable execution")
	}
	if _, ok := s.consumer.ClaimPreconf(block); !ok {
		t.Fatal("claim reusable execution")
	}
	payload, ok := makePendingPayload(block, receipts, statedb, nil)
	if !ok {
		t.Fatal("pending payload")
	}
	s.publishOpen(block, payload, block.ParentHash(), block.NumberU64(), true)
	if _, _, ok := s.consumer.LookupPreconf(tx.Hash()); !ok {
		t.Fatal("rejected open erased the importing receipt")
	}
}

func TestPendingStoreMismatchAndNoTransactionCountLimit(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	store := NewPendingStore(db)
	header := &types.Header{Number: big.NewInt(3), ParentHash: common.HexToHash("0x1"), GasLimit: 30_000_000}
	stateDB, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	tx := types.NewTransaction(0, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
	block := types.NewBlock(header, &types.Body{Transactions: types.Transactions{tx}}, nil, trie.NewStackTrie(nil))
	if !store.publish(block, nil, stateDB, nil, 0) {
		t.Fatal("building publish failed")
	}
	mismatch := types.NewBlock(header, &types.Body{}, nil, trie.NewStackTrie(nil))
	reason, invalid := store.CheckPreconfInvalidation(mismatch, nil)
	if !invalid || reason != "canonical_mismatch" {
		t.Fatalf("invalidation = %q, %v", reason, invalid)
	}

	txs := make(types.Transactions, 4097)
	for index := range txs {
		txs[index] = types.NewTransaction(uint64(index), common.Address{1}, big.NewInt(0), 21_000, big.NewInt(1), nil)
	}
	highCount := types.NewBlock(&types.Header{Number: big.NewInt(4), GasLimit: uint64(len(txs)) * 21_000}, &types.Body{Transactions: txs}, nil, trie.NewStackTrie(nil))
	if !store.publish(highCount, nil, stateDB, nil, 0) {
		t.Fatal("valid high-transaction-count block was rejected")
	}
}

func TestPendingStoreEntryLimit(t *testing.T) {
	store := NewPendingStore(nil)
	for number := uint64(1); number <= pendingEntryLimit; number++ {
		if generation := store.begin(number, common.Hash{byte(number)}, true); generation == 0 {
			t.Fatalf("entry %d rejected before limit", number)
		}
	}
	if generation := store.begin(pendingEntryLimit+1, common.Hash{0xff}, true); generation != 0 {
		t.Fatalf("entry beyond limit accepted with generation %d", generation)
	}
	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	block := types.NewBlockWithHeader(&types.Header{Number: new(big.Int).SetUint64(pendingEntryLimit + 1), ParentHash: common.Hash{0xfe}})
	if store.publish(block, nil, statedb, nil, 0) {
		t.Fatal("publication beyond entry limit succeeded")
	}
	store.mu.Lock()
	store.removeLocked(pendingKey{number: 1, parent: common.Hash{1}})
	store.mu.Unlock()
	if generation := store.begin(pendingEntryLimit+1, common.Hash{0xff}, true); generation == 0 {
		t.Fatal("entry rejected after capacity became available")
	}
}
