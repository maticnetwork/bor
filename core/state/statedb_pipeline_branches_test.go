package state

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestTrieOnlyConstructorsRejectUnknownRoot(t *testing.T) {
	db := NewDatabaseForTesting()
	missingRoot := common.HexToHash("0xdead")

	statedb, err := NewTrieOnly(missingRoot, db)
	require.Error(t, err)
	require.Nil(t, statedb)

	snapshot := NewWarmSnapshot([]TrieWarmNodes{{
		Nodes: map[string][]byte{"": {0x80}},
	}})
	statedb, err = NewTrieOnlyWithSnapshot(missingRoot, db, snapshot)
	require.Error(t, err)
	require.Nil(t, statedb)
}

func TestFlatDiffInternalBranchHelpers(t *testing.T) {
	db := NewDatabaseForTesting()
	statedb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0x1234")

	require.False(t, statedb.hasAccountMutation(addr))
	statedb.journal.dirty(addr)
	require.True(t, statedb.hasAccountMutation(addr))
	statedb.clearJournalAndRefund()
	statedb.mutations[addr] = &mutation{typ: update}
	require.True(t, statedb.hasAccountMutation(addr))

	var nilDiff *FlatDiff
	account, exists, destructed := nilDiff.accountOverlay(addr)
	require.Equal(t, types.StateAccount{}, account)
	require.False(t, exists)
	require.False(t, destructed)

	statedb.applyFlatPureDestructFast(addr)
	require.Nil(t, statedb.getStateObject(addr))

	diff := &FlatDiff{
		Accounts:  map[common.Address]types.StateAccount{addr: {Nonce: 7}},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	statedb.applyFlatMutationFast(diff, addr, diff.Accounts[addr])
	obj := statedb.getStateObject(addr)
	require.NotNil(t, obj)
	require.Equal(t, uint64(7), obj.Nonce())
	require.True(t, obj.Balance().IsZero())

	statedb.applyFlatPureDestructFast(addr)
	require.True(t, obj.selfDestructed)
}

func TestWitnessCollectionWithoutPrefetcher(t *testing.T) {
	db := NewDatabaseForTesting()
	statedb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	witness, err := stateless.NewWitness(&types.Header{Number: common.Big1}, nil)
	require.NoError(t, err)
	statedb.SetWitness(witness)
	statedb.witnessStats = stateless.NewWitnessStats()

	nodes := map[string][]byte{"": {0x80}, "\x01": {0xc0}}
	statedb.addWitnessNodes(nodes, common.HexToHash("0x01"))
	require.Len(t, witness.State, 2)

	addr := common.HexToAddress("0x5678")
	slot := common.HexToHash("0x01")
	statedb.CreateAccount(addr)
	statedb.SetBalance(addr, uint256.NewInt(1), 0)
	obj := statedb.getStateObject(addr)
	require.NotNil(t, obj)
	obj.originStorage[slot] = common.Hash{}
	statedb.addObjectWitness(obj)
	require.NoError(t, statedb.Error())
}

func TestFinaliseFastPrefetchSkipsMissingDirtyObject(t *testing.T) {
	db := NewDatabaseForTesting()
	statedb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	statedb.StartPrefetcher("missing-dirty-object", nil, nil)
	t.Cleanup(statedb.StopPrefetcher)

	missing := common.HexToAddress("0xbeef")
	statedb.journal.dirty(missing)
	require.Empty(t, statedb.snapshotDirtyStorageSlots())
	statedb.FinaliseFastWithPrefetch(false)
}

func TestCommittedStoragePrefetchAndWitnessBranches(t *testing.T) {
	addr := common.HexToAddress("0xaffeaffeaffeaffeaffeaffeaffeaffeaffeaffe")
	slot := common.HexToHash("0xaaa")

	base := filledStateDB()
	root, _, err := base.CommitWithUpdate(1, false, false)
	require.NoError(t, err)

	t.Run("storage reads and writes use the committed prefetch root", func(t *testing.T) {
		statedb, err := New(root, base.db)
		require.NoError(t, err)
		statedb.StartPrefetcher("committed-storage", nil, nil)
		t.Cleanup(statedb.StopPrefetcher)

		require.NotEqual(t, common.Hash{}, statedb.GetState(addr, slot))
		statedb.SetState(addr, slot, common.BigToHash(big.NewInt(999)))
		statedb.Finalise(false)
		require.NotEqual(t, common.Hash{}, statedb.IntermediateRoot(false))
	})

	t.Run("witness rereads read-only storage without a prefetcher", func(t *testing.T) {
		statedb, err := New(root, base.db)
		require.NoError(t, err)
		witness, err := stateless.NewWitness(&types.Header{Number: common.Big1}, nil)
		require.NoError(t, err)
		statedb.SetWitness(witness)

		require.NotEqual(t, common.Hash{}, statedb.GetState(addr, slot))
		require.NotEqual(t, common.Hash{}, statedb.IntermediateRoot(false))
		require.NotEmpty(t, witness.State)
	})

	t.Run("destructed read-only object is included in witness collection", func(t *testing.T) {
		statedb, err := New(root, base.db)
		require.NoError(t, err)
		witness, err := stateless.NewWitness(&types.Header{Number: common.Big1}, nil)
		require.NoError(t, err)
		statedb.SetWitness(witness)
		require.NotEqual(t, common.Hash{}, statedb.GetState(addr, slot))
		statedb.SelfDestruct(addr)
		statedb.IntermediateRoot(false)
		require.NotEmpty(t, witness.State)
	})
}

func TestPrefetchedObjectWitnessAndReadOnlyDestructSkip(t *testing.T) {
	db := NewDatabaseForTesting()
	statedb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	witness, err := stateless.NewWitness(&types.Header{Number: common.Big1}, nil)
	require.NoError(t, err)
	statedb.SetWitness(witness)

	addr := common.HexToAddress("0x1234")
	root := common.HexToHash("0x9876")
	statedb.CreateAccount(addr)
	obj := statedb.getStateObject(addr)
	obj.data.Root = root
	tr := &blockingPrefetchTrie{witness: map[string][]byte{"node": {0xc0}}}
	term := make(chan struct{})
	close(term)
	prefetcher := &triePrefetcher{
		root:     statedb.originalRoot,
		term:     make(chan struct{}),
		fetchers: make(map[string]*subfetcher),
	}
	prefetcher.fetchers[prefetcher.trieID(obj.addrHash(), root)] = &subfetcher{
		trie: tr,
		term: term,
	}
	statedb.prefetcher = prefetcher

	require.Same(t, tr, obj.getPrefetchedTrie())
	statedb.addObjectWitness(obj)
	require.NotEmpty(t, witness.State)

	diff := &FlatDiff{ReadStorage: make(map[common.Address][]common.Hash)}
	statedb.stateObjectsDestruct[addr] = obj
	statedb.captureReadOnlyAccount(diff, addr, obj)
	require.Empty(t, diff.ReadSet)
}

func TestDetachedPrefetcherReturnsWarmSnapshotInput(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	term := make(chan struct{})
	close(term)
	// Built through the real constructor so the metric meters are non-nil:
	// StopAndCollectWarmSnapshot ends in report(), and another test in the
	// package flips the global metrics.Enable() — with a hand-rolled
	// &triePrefetcher{} literal that ordering turns report() into a nil
	// Meter.Mark panic.
	prefetcher := newTriePrefetcher(filledStateDB().db, common.Hash{}, "test", false)
	prefetcher.fetchers = map[string]*subfetcher{
		"loaded": {
			trie: &blockingPrefetchTrie{witness: map[string][]byte{"node": {0xc0}}},
			stop: stop,
			term: term,
		},
	}
	input, stats := (&DetachedPrefetcher{prefetcher: prefetcher}).StopAndCollectWarmSnapshot()
	require.NotNil(t, input)
	require.Equal(t, 1, stats.LoadedFetchers)
}

func TestTerminatedPrefetcherErrorsAreSoft(t *testing.T) {
	addr := common.HexToAddress("0xaffeaffeaffeaffeaffeaffeaffeaffeaffeaffe")
	slot := common.HexToHash("0xaaa")
	base := filledStateDB()
	root, _, err := base.CommitWithUpdate(1, false, false)
	require.NoError(t, err)

	statedb, err := New(root, base.db)
	require.NoError(t, err)
	statedb.StartPrefetcher("terminated-prefetcher", nil, nil)
	statedb.prefetcher.terminate(false)

	require.NotEqual(t, common.Hash{}, statedb.GetState(addr, slot))
	statedb.SetState(addr, slot, common.HexToHash("0x1234"))
	statedb.Finalise(false)
	require.NoError(t, statedb.Error())
}

func TestObjectOwnedTrieWitnessBranch(t *testing.T) {
	statedb, err := New(types.EmptyRootHash, NewDatabaseForTesting())
	require.NoError(t, err)
	witness, err := stateless.NewWitness(&types.Header{Number: common.Big1}, nil)
	require.NoError(t, err)
	statedb.SetWitness(witness)

	addr := common.HexToAddress("0x4567")
	statedb.CreateAccount(addr)
	obj := statedb.getStateObject(addr)
	obj.trie = &blockingPrefetchTrie{witness: map[string][]byte{"owned": {0xc0}}}
	statedb.addObjectWitness(obj)
	require.NotEmpty(t, witness.State)
}
