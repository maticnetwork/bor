package state

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/pathdb"
)

func TestWasStorageSlotRead(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
	sdb, _ := New(types.EmptyRootHash, db)

	addr := common.HexToAddress("0x1234")
	slot := common.HexToHash("0xabcd")

	// Slot not read yet
	if sdb.WasStorageSlotRead(addr, slot) {
		t.Error("slot should not be marked as read before any access")
	}

	// Create an account and read its storage
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.Finalise(false)

	// Read the slot
	sdb.GetState(addr, slot)

	// Now it should be marked as read
	if !sdb.WasStorageSlotRead(addr, slot) {
		t.Error("slot should be marked as read after GetState")
	}

	// A different slot should not be marked
	otherSlot := common.HexToHash("0x5678")
	if sdb.WasStorageSlotRead(addr, otherSlot) {
		t.Error("other slot should not be marked as read")
	}

	// A different address should not be marked
	otherAddr := common.HexToAddress("0x5678")
	if sdb.WasStorageSlotRead(otherAddr, slot) {
		t.Error("other address should not be marked as read")
	}
}

func TestFlatDiffOverlay_ReadThrough(t *testing.T) {
	// Create a base state with an account
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
	sdb, _ := New(types.EmptyRootHash, db)

	baseAddr := common.HexToAddress("0xbase")
	sdb.CreateAccount(baseAddr)
	sdb.SetNonce(baseAddr, 1, 0)
	sdb.SetBalance(baseAddr, uint256.NewInt(100), 0)
	root, _, _ := sdb.CommitWithUpdate(0, false, false)

	// Create a FlatDiff with a new account
	overlayAddr := common.HexToAddress("0xoverlay")
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			overlayAddr: {
				Nonce:    42,
				Balance:  uint256.NewInt(200),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:          make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:        make(map[common.Address]struct{}),
		Code:             make(map[common.Hash][]byte),
		ReadStorage:      make(map[common.Address][]common.Hash),
		NonExistentReads: nil,
	}

	// Create StateDB with FlatDiff overlay
	overlayDB, err := NewWithFlatBase(root, db, diff)
	if err != nil {
		t.Fatal(err)
	}

	// Should see the overlay account
	if overlayDB.GetNonce(overlayAddr) != 42 {
		t.Errorf("expected nonce 42 for overlay addr, got %d", overlayDB.GetNonce(overlayAddr))
	}

	// Should still see the base account
	if overlayDB.GetNonce(baseAddr) != 1 {
		t.Errorf("expected nonce 1 for base addr, got %d", overlayDB.GetNonce(baseAddr))
	}
}

func TestCommitSnapshot_CapturesWrites(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
	sdb, _ := New(types.EmptyRootHash, db)

	addr := common.HexToAddress("0x1234")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 10, 0)
	sdb.SetBalance(addr, uint256.NewInt(500), 0)

	slot := common.HexToHash("0xaaaa")
	sdb.SetState(addr, slot, common.HexToHash("0xbbbb"))

	diff := sdb.CommitSnapshot(false)

	// Verify account is captured
	acct, ok := diff.Accounts[addr]
	if !ok {
		t.Fatal("account not captured in FlatDiff")
	}
	if acct.Nonce != 10 {
		t.Errorf("expected nonce 10, got %d", acct.Nonce)
	}

	// Verify storage is captured
	slots, ok := diff.Storage[addr]
	if !ok {
		t.Fatal("storage not captured in FlatDiff")
	}
	if slots[slot] != common.HexToHash("0xbbbb") {
		t.Errorf("expected slot value 0xbbbb, got %x", slots[slot])
	}
}

func TestFlatDiffOverlay_DestructedAccountReturnsNil(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xdead01")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(999), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// FlatDiff marks account as destructed but does NOT add it to Accounts.
	diff := &FlatDiff{
		Accounts:  make(map[common.Address]types.StateAccount),
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}

	overlayDB, err := NewWithFlatBase(root, db, diff)
	require.NoError(t, err)

	require.False(t, overlayDB.Exist(addr), "destructed account should not exist")
	require.True(t, overlayDB.GetBalance(addr).IsZero(), "destructed account balance should be zero")
}

func TestFlatDiffOverlay_DestructAndResurrect(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xdead02")
	slot := common.HexToHash("0x01")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 5, 0)
	sdb.SetState(addr, slot, common.HexToHash("0xbeef"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// FlatDiff has addr in BOTH Destructs and Accounts (destruct + resurrect with new nonce).
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    10,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}

	overlayDB, err := NewWithFlatBase(root, db, diff)
	require.NoError(t, err)

	// The account should be resurrected with the new nonce from FlatDiff.Accounts.
	require.Equal(t, uint64(10), overlayDB.GetNonce(addr))
	require.Equal(t, common.Hash{}, overlayDB.GetState(addr, slot),
		"destruct+resurrect FlatDiff must not expose pre-destruction storage")
}

func TestTrieOnlyReader_SkipsFlatReaders(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xacc001")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(42), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Create StateDB via NewTrieOnly — reads go through trie, not flat/snapshot.
	trieDB, err := NewTrieOnly(root, db)
	require.NoError(t, err)

	// Verify trie reader returns correct data.
	require.Equal(t, uint256.NewInt(42), trieDB.GetBalance(addr))

	// Attach a witness and modify the account via a fresh trie-only StateDB.
	// After IntermediateRoot, the witness should capture trie nodes (non-empty
	// State map). With flat readers the trie is never walked, so the witness
	// would remain empty.
	trieDB2, err := NewTrieOnly(root, db)
	require.NoError(t, err)

	witness := &stateless.Witness{
		Headers: []*types.Header{{}},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	trieDB2.SetWitness(witness)

	// Modify the account so that IntermediateRoot walks the trie and collects
	// witness nodes from the account trie.
	trieDB2.SetBalance(addr, uint256.NewInt(99), 0)
	trieDB2.IntermediateRoot(false)

	require.NotEmpty(t, witness.State, "witness should capture trie nodes when using trie-only reader")
}

func TestNewTrieOnly_ReadsCorrectData(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr1 := common.HexToAddress("0xacc101")
	addr2 := common.HexToAddress("0xacc102")
	addr3 := common.HexToAddress("0xacc103")

	sdb.CreateAccount(addr1)
	sdb.SetBalance(addr1, uint256.NewInt(100), 0)
	sdb.SetNonce(addr1, 1, 0)

	sdb.CreateAccount(addr2)
	sdb.SetBalance(addr2, uint256.NewInt(200), 0)
	sdb.SetNonce(addr2, 5, 0)
	sdb.SetCode(addr2, []byte{0x60, 0x00, 0x60, 0x00}, 0)

	sdb.CreateAccount(addr3)
	sdb.SetBalance(addr3, uint256.NewInt(300), 0)
	slot := common.HexToHash("0xaa01")
	sdb.SetState(addr3, slot, common.HexToHash("0xbb01"))

	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Create via NewTrieOnly and verify all data.
	trieDB, err := NewTrieOnly(root, db)
	require.NoError(t, err)

	require.Equal(t, uint256.NewInt(100), trieDB.GetBalance(addr1))
	require.Equal(t, uint64(1), trieDB.GetNonce(addr1))

	require.Equal(t, uint256.NewInt(200), trieDB.GetBalance(addr2))
	require.Equal(t, uint64(5), trieDB.GetNonce(addr2))
	require.Equal(t, crypto.Keccak256Hash([]byte{0x60, 0x00, 0x60, 0x00}), trieDB.GetCodeHash(addr2))
	require.Equal(t, []byte{0x60, 0x00, 0x60, 0x00}, trieDB.GetCode(addr2))

	require.Equal(t, uint256.NewInt(300), trieDB.GetBalance(addr3))
	require.Equal(t, common.HexToHash("0xbb01"), trieDB.GetState(addr3, slot))
}

func TestPropagateReadsTo_AccountsAndStorage(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr1 := common.HexToAddress("0xaa0001")
	addr2 := common.HexToAddress("0xaa0002")
	slot1 := common.HexToHash("0xcc0001")
	slot2 := common.HexToHash("0xcc0002")

	sdb.CreateAccount(addr1)
	sdb.SetBalance(addr1, uint256.NewInt(111), 0)
	sdb.SetState(addr1, slot1, common.HexToHash("0xdd0001"))
	sdb.SetState(addr1, slot2, common.HexToHash("0xdd0002"))

	sdb.CreateAccount(addr2)
	sdb.SetBalance(addr2, uint256.NewInt(222), 0)

	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Create src and dst StateDBs at the same root.
	src, err := New(root, db)
	require.NoError(t, err)
	dst, err := New(root, db)
	require.NoError(t, err)

	// Read accounts and storage on src.
	src.GetBalance(addr1)
	src.GetBalance(addr2)
	src.GetState(addr1, slot1)
	src.GetState(addr1, slot2)

	// Propagate reads from src to dst.
	src.PropagateReadsTo(dst)

	// dst should now have the accounts and storage in its stateObjects
	// (populated by PropagateReadsTo calling GetBalance/GetState on dst).
	require.Equal(t, uint256.NewInt(111), dst.GetBalance(addr1))
	require.Equal(t, uint256.NewInt(222), dst.GetBalance(addr2))
	require.Equal(t, common.HexToHash("0xdd0001"), dst.GetState(addr1, slot1))
	require.Equal(t, common.HexToHash("0xdd0002"), dst.GetState(addr1, slot2))
}

func TestCommitSnapshot_CapturesDestructs(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xdestruct01")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(500), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Create a new StateDB at the committed root and self-destruct the account.
	sdb2, err := New(root, db)
	require.NoError(t, err)

	sdb2.SelfDestruct(addr)
	diff := sdb2.CommitSnapshot(false)

	_, destructed := diff.Destructs[addr]
	require.True(t, destructed, "self-destructed account should appear in diff.Destructs")
}

// TestPrefetchRoot_FlatDiffAccountUsesCommittedRoot verifies that accounts
// loaded from FlatDiff get their prefetchRoot set to the committed parent's
// storage root, not the FlatDiff's storage root. This is critical for
// pipelined SRC: the prefetcher's NodeReader is opened at the committed
// parent root (grandparent), so it can only resolve trie nodes for that
// state's storage root. Using FlatDiff's root (block N's post-state) would
// cause "Unexpected trie node" hash mismatches.
func TestPrefetchRoot_FlatDiffAccountUsesCommittedRoot(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)

	// --- Set up a committed state with a contract that has storage ---
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xcontract")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.SetState(addr, common.HexToHash("0x01"), common.HexToHash("0xaa"))
	sdb.Finalise(false)

	committedRoot, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Read back the committed account to get its storage root.
	committedSDB, err := New(committedRoot, db)
	require.NoError(t, err)
	committedObj := committedSDB.getStateObject(addr)
	require.NotNil(t, committedObj)
	committedStorageRoot := committedObj.data.Root
	require.NotEqual(t, types.EmptyRootHash, committedStorageRoot, "committed account should have non-empty storage root")

	// --- Simulate block N: modify the contract's storage and extract FlatDiff ---
	sdb2, err := New(committedRoot, db)
	require.NoError(t, err)
	sdb2.SetState(addr, common.HexToHash("0x02"), common.HexToHash("0xbb")) // new slot
	sdb2.Finalise(false)
	diff := sdb2.CommitSnapshot(false)

	// The FlatDiff account has block N's storage root (different from committed).
	flatDiffAcct, ok := diff.Accounts[addr]
	require.True(t, ok, "contract should be in FlatDiff")
	flatDiffStorageRoot := flatDiffAcct.Root
	// The FlatDiff root is the account's root BEFORE IntermediateRoot (i.e.,
	// CommitSnapshot doesn't hash — it captures the current data.Root). So it
	// equals the committed root here. But the key point is that getPrefetchRoot
	// returns the committed root regardless.

	// --- Create a pipelined StateDB with FlatDiff overlay ---
	overlayDB, err := NewWithFlatBase(committedRoot, db, diff)
	require.NoError(t, err)

	// Load the account from FlatDiff
	obj := overlayDB.getStateObject(addr)
	require.NotNil(t, obj)

	// Verify origin/data roots come from FlatDiff
	require.Equal(t, flatDiffStorageRoot, obj.data.Root, "data.Root should be from FlatDiff")

	// Verify prefetchRoot was set to the committed storage root
	require.Equal(t, committedStorageRoot, obj.prefetchRoot, "prefetchRoot should be the committed parent's storage root")

	// Verify getPrefetchRoot returns the committed root (not data.Root)
	require.Equal(t, committedStorageRoot, obj.getPrefetchRoot(), "getPrefetchRoot should return the committed storage root")
}

// TestPrefetchRoot_NormalAccountFallsBackToDataRoot verifies that accounts
// loaded from the committed state (not FlatDiff) have prefetchRoot=zero,
// and getPrefetchRoot falls back to data.Root.
func TestPrefetchRoot_NormalAccountFallsBackToDataRoot(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)

	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xnormal")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.SetState(addr, common.HexToHash("0x01"), common.HexToHash("0xaa"))
	sdb.Finalise(false)

	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Load the account normally (no FlatDiff)
	sdb2, err := New(root, db)
	require.NoError(t, err)

	obj := sdb2.getStateObject(addr)
	require.NotNil(t, obj)

	// prefetchRoot should be zero (not set for non-FlatDiff accounts)
	require.Equal(t, common.Hash{}, obj.prefetchRoot, "prefetchRoot should be zero for non-FlatDiff accounts")

	// getPrefetchRoot should fall back to data.Root
	require.Equal(t, obj.data.Root, obj.getPrefetchRoot(), "getPrefetchRoot should fall back to data.Root")
}

// TestPrefetchRoot_NewAccountInFlatDiff verifies that an account created in
// block N (exists in FlatDiff but not in committed state) gets the empty
// storage root as its prefetch root since there's nothing to prefetch at the
// committed parent root.
func TestPrefetchRoot_NewAccountInFlatDiff(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)

	// Commit an empty state
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	committedRoot, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// FlatDiff with a new account that doesn't exist in committed state
	newAddr := common.HexToAddress("0xnew")
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			newAddr: {
				Nonce:    1,
				Balance:  uint256.NewInt(100),
				Root:     crypto.Keccak256Hash([]byte("fake-storage-root")), // non-empty root
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:          make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:        make(map[common.Address]struct{}),
		Code:             make(map[common.Hash][]byte),
		ReadStorage:      make(map[common.Address][]common.Hash),
		NonExistentReads: nil,
	}

	overlayDB, err := NewWithFlatBase(committedRoot, db, diff)
	require.NoError(t, err)

	obj := overlayDB.getStateObject(newAddr)
	require.NotNil(t, obj)

	// Account is new (not in committed state), so storage prefetching must not
	// use the FlatDiff account's post-state root with the committed-parent
	// reader. The empty storage root makes those prefetches a no-op.
	require.Equal(t, types.EmptyRootHash, obj.prefetchRoot, "prefetchRoot should be empty root for new accounts not in committed state")

	// getPrefetchRoot returns the committed-parent-compatible root, not the
	// FlatDiff post-state storage root.
	require.Equal(t, types.EmptyRootHash, obj.getPrefetchRoot(), "getPrefetchRoot should return empty root for new accounts")
}

func TestSnapshotDirtyStorageSlots_UsesCommittedPrefetchRootForFlatDiff(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)

	addr := common.HexToAddress("0xflat")
	slot := common.HexToHash("0x01")

	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.SetState(addr, slot, common.HexToHash("0xaa"))
	sdb.Finalise(false)

	committedRoot, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	committedDB, err := New(committedRoot, db)
	require.NoError(t, err)
	committedObj := committedDB.getStateObject(addr)
	require.NotNil(t, committedObj)
	committedStorageRoot := committedObj.data.Root
	require.NotEqual(t, types.EmptyRootHash, committedStorageRoot)

	postStorageRoot := crypto.Keccak256Hash([]byte("block-n-post-storage-root"))
	require.NotEqual(t, committedStorageRoot, postStorageRoot)

	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     postStorageRoot,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:     make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:   make(map[common.Address]struct{}),
		Code:        make(map[common.Hash][]byte),
		ReadStorage: make(map[common.Address][]common.Hash),
	}

	overlayDB, err := NewWithFlatBase(committedRoot, db, diff)
	require.NoError(t, err)
	overlayDB.SetState(addr, common.HexToHash("0x02"), common.HexToHash("0xbb"))

	slots := overlayDB.snapshotDirtyStorageSlots()
	require.Len(t, slots, 1)
	require.Equal(t, addr, slots[0].addr)
	require.Equal(t, committedStorageRoot, slots[0].root)
	require.NotEqual(t, postStorageRoot, slots[0].root)
}

func TestSnapshotDirtyStorageSlots_SkipsNewFlatDiffAccount(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)

	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	committedRoot, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	addr := common.HexToAddress("0xnew")
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    1,
				Balance:  uint256.NewInt(100),
				Root:     crypto.Keccak256Hash([]byte("block-n-post-storage-root")),
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:     make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:   make(map[common.Address]struct{}),
		Code:        make(map[common.Hash][]byte),
		ReadStorage: make(map[common.Address][]common.Hash),
	}

	overlayDB, err := NewWithFlatBase(committedRoot, db, diff)
	require.NoError(t, err)
	overlayDB.SetState(addr, common.HexToHash("0x01"), common.HexToHash("0xbb"))

	require.Empty(t, overlayDB.snapshotDirtyStorageSlots())
}

// TestPrefetchRoot_DeepCopyPreserves verifies that stateObject.deepCopy
// preserves the prefetchRoot field, which is important for StateDB.Copy()
// used by the block-level prefetcher.
func TestPrefetchRoot_DeepCopyPreserves(t *testing.T) {
	db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)

	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xcopy")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.SetState(addr, common.HexToHash("0x01"), common.HexToHash("0xaa"))
	sdb.Finalise(false)

	committedRoot, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Simulate a FlatDiff account with a different storage root
	sdb2, err := New(committedRoot, db)
	require.NoError(t, err)
	sdb2.SetState(addr, common.HexToHash("0x02"), common.HexToHash("0xbb"))
	sdb2.Finalise(false)
	diff := sdb2.CommitSnapshot(false)

	// Create overlay StateDB and load account
	overlayDB, err := NewWithFlatBase(committedRoot, db, diff)
	require.NoError(t, err)
	obj := overlayDB.getStateObject(addr)
	require.NotNil(t, obj)
	require.NotEqual(t, common.Hash{}, obj.prefetchRoot)

	// Copy the StateDB and verify prefetchRoot is preserved
	copiedDB := overlayDB.Copy()
	copiedObj := copiedDB.getStateObject(addr)
	require.NotNil(t, copiedObj)
	require.Equal(t, obj.prefetchRoot, copiedObj.prefetchRoot, "deepCopy should preserve prefetchRoot")
	require.Equal(t, obj.getPrefetchRoot(), copiedObj.getPrefetchRoot(), "getPrefetchRoot should match after deepCopy")
}

// TestPipelinedSRC_RootParity_NewVsTrieOnly is a consensus-critical parity
// check for mitigation (2.5): when the SRC goroutine runs without producing a
// witness, openSRCStateDB uses state.New (multi-reader, flat-reader-eligible)
// instead of state.NewTrieOnly. Both reader paths must produce byte-identical
// state roots from the same FlatDiff applied at the same parent root —
// otherwise consensus would split between witness-producing and witness-off
// nodes.
//
// The FlatDiff exercises every shape that touches origin reads:
//   - balance/nonce mutation on existing account
//   - storage zero-write (slot deletion) and fresh storage write
//   - pure self-destruct (no resurrection)
//   - self-destruct followed by resurrection with new state
//   - code deploy on a new account
//   - read-only access to an existing account (flatDiff.ReadSet)
//   - non-existent address probe (flatDiff.NonExistentReads)
//
// Uses path scheme so state.New actually wires a flat reader (pathdb
// StateReader), making the trie-only vs multi-reader distinction
// observable. Under hash scheme without a snapshot the multi-reader
// degenerates to trie-only and the test would be trivially true.
func TestPipelinedSRC_RootParity_NewVsTrieOnly(t *testing.T) {
	disk := rawdb.NewMemoryDatabase()
	defer disk.Close()
	tdb := triedb.NewDatabase(disk, &triedb.Config{PathDB: pathdb.Defaults})
	defer tdb.Close()
	sdb := NewDatabase(tdb, nil)

	addrMutate := common.HexToAddress("0xa1")    // existing → balance/nonce mutation
	addrZeroSlot := common.HexToAddress("0xa2")  // existing storage → zero a slot, write a fresh slot
	addrPureDest := common.HexToAddress("0xa3")  // existing → pure self-destruct
	addrResurrect := common.HexToAddress("0xa4") // existing → destruct + resurrect with new state
	addrReadOnly := common.HexToAddress("0xa5")  // existing → read-only access only
	addrCodeNew := common.HexToAddress("0xa6")   // new → balance + code deploy
	addrNonExist := common.HexToAddress("0xa7")  // never exists → probed only

	slotZeroed := common.HexToHash("0x01")
	slotFresh := common.HexToHash("0x02")
	slotResurrectOld := common.HexToHash("0x03")
	slotResurrectNew := common.HexToHash("0x04")
	slotReadOnly := common.HexToHash("0x05")

	// --- Build initial committed state ---
	initial, err := New(types.EmptyRootHash, sdb)
	require.NoError(t, err)

	initial.CreateAccount(addrMutate)
	initial.SetBalance(addrMutate, uint256.NewInt(100), 0)
	initial.SetNonce(addrMutate, 1, 0)

	initial.CreateAccount(addrZeroSlot)
	initial.SetBalance(addrZeroSlot, uint256.NewInt(50), 0)
	initial.SetState(addrZeroSlot, slotZeroed, common.HexToHash("0xbeef"))

	initial.CreateAccount(addrPureDest)
	initial.SetBalance(addrPureDest, uint256.NewInt(200), 0)

	initial.CreateAccount(addrResurrect)
	initial.SetBalance(addrResurrect, uint256.NewInt(300), 0)
	initial.SetNonce(addrResurrect, 5, 0)
	initial.SetCode(addrResurrect, []byte{0x60, 0x01}, 0)
	initial.SetState(addrResurrect, slotResurrectOld, common.HexToHash("0xbbbb"))

	initial.CreateAccount(addrReadOnly)
	initial.SetBalance(addrReadOnly, uint256.NewInt(400), 0)
	initial.SetState(addrReadOnly, slotReadOnly, common.HexToHash("0xdddd"))

	parentRoot, _, err := initial.CommitWithUpdate(0, false, false)
	require.NoError(t, err)
	require.NoError(t, tdb.Commit(parentRoot, false))

	// Confirm the multi-reader path will actually wire a flat reader at
	// parentRoot. state.New silently falls back to trie-only if
	// triedb.StateReader errors, which would let the parity assertion below
	// pass without exercising the mitigation. Asserting StateReader resolves
	// makes the test fail loudly if path-mode setup ever regresses.
	if _, err := tdb.StateReader(parentRoot); err != nil {
		t.Fatalf("path-scheme StateReader unavailable at parentRoot — "+
			"multi-reader would silently fall back to trie-only, defeating the parity test: %v", err)
	}

	// --- Simulate block N execution at parentRoot to produce a FlatDiff ---
	exec, err := New(parentRoot, sdb)
	require.NoError(t, err)

	exec.SetBalance(addrMutate, uint256.NewInt(150), 0)
	exec.SetNonce(addrMutate, 2, 0)

	exec.SetState(addrZeroSlot, slotZeroed, common.Hash{})
	exec.SetState(addrZeroSlot, slotFresh, common.HexToHash("0x1234"))

	exec.SelfDestruct(addrPureDest)

	// Destruct in one tx, resurrect in the next. Finalise between the two so
	// the destructed object lands in stateObjectsDestruct before the new
	// CreateAccount replaces stateObjects[addr]; without this, the new object
	// overwrites the destruct trail and CommitSnapshot would emit only an
	// Accounts entry, not the destruct+resurrect shape we want to exercise.
	exec.SelfDestruct(addrResurrect)
	exec.Finalise(false)
	exec.CreateAccount(addrResurrect)
	exec.SetBalance(addrResurrect, uint256.NewInt(999), 0)
	exec.SetNonce(addrResurrect, 1, 0)
	exec.SetCode(addrResurrect, []byte{0x60, 0x02}, 0)
	exec.SetState(addrResurrect, slotResurrectNew, common.HexToHash("0xffff"))

	exec.CreateAccount(addrCodeNew)
	exec.SetBalance(addrCodeNew, uint256.NewInt(77), 0)
	exec.SetCode(addrCodeNew, []byte{0x60, 0x03}, 0)

	exec.GetBalance(addrReadOnly)
	exec.GetState(addrReadOnly, slotReadOnly)

	exec.GetBalance(addrNonExist)

	flatDiff := exec.CommitSnapshot(false)

	// Sanity: FlatDiff captured every shape we exercise.
	require.Contains(t, flatDiff.Accounts, addrMutate)
	require.Contains(t, flatDiff.Accounts, addrZeroSlot)
	require.Contains(t, flatDiff.Destructs, addrPureDest)
	require.Contains(t, flatDiff.Destructs, addrResurrect)
	require.Contains(t, flatDiff.Accounts, addrResurrect)
	require.Contains(t, flatDiff.Accounts, addrCodeNew)
	zeroedSlots, ok := flatDiff.Storage[addrZeroSlot]
	require.True(t, ok)
	require.Equal(t, common.Hash{}, zeroedSlots[slotZeroed])
	require.Equal(t, common.HexToHash("0x1234"), zeroedSlots[slotFresh])

	// --- Path A: NewTrieOnly (witness-producing path) ---
	trieOnlyDB, err := NewTrieOnly(parentRoot, sdb)
	require.NoError(t, err)
	trieOnlyDB.ApplyFlatDiffForCommit(flatDiff)
	rootTrieOnly := trieOnlyDB.IntermediateRoot(false)

	// --- Path B: state.New (witness-off multi-reader path) ---
	multiDB, err := New(parentRoot, sdb)
	require.NoError(t, err)
	multiDB.ApplyFlatDiffForCommit(flatDiff)
	rootMulti := multiDB.IntermediateRoot(false)

	// --- Path C: state.New + fast witness-off replay path ---
	fastDB, err := New(parentRoot, sdb)
	require.NoError(t, err)
	fastDB.ApplyFlatDiffForCommitFast(flatDiff)
	rootFast := fastDB.IntermediateRoot(false)

	// --- Parity assertion: byte-identical state roots ---
	require.Equal(t, rootTrieOnly, rootMulti,
		"state root must be byte-identical between NewTrieOnly and state.New paths — "+
			"any divergence is a consensus-splitting bug between witness-producing and witness-off nodes")
	require.Equal(t, rootTrieOnly, rootFast,
		"fast FlatDiff replay must produce the same root as the journaled SRC replay")

	// Cross-check against direct execution of the same mutations
	// on a fresh StateDB at parentRoot (no FlatDiff replay). This catches any
	// hypothetical bug where ApplyFlatDiffForCommit produces a root that
	// differs from the original execution.
	direct, err := New(parentRoot, sdb)
	require.NoError(t, err)
	direct.SetBalance(addrMutate, uint256.NewInt(150), 0)
	direct.SetNonce(addrMutate, 2, 0)
	direct.SetState(addrZeroSlot, slotZeroed, common.Hash{})
	direct.SetState(addrZeroSlot, slotFresh, common.HexToHash("0x1234"))
	direct.SelfDestruct(addrPureDest)
	direct.SelfDestruct(addrResurrect)
	// Mirror the exec-path transaction boundary: Finalise after SelfDestruct
	// so the destructed object is recorded in stateObjectsDestruct before the
	// resurrection. Without this, the cross-check would diverge from the
	// FlatDiff path because the FlatDiff captured a destruct+resurrect shape
	// that only exists when there is a Finalise between the two operations.
	direct.Finalise(false)
	direct.CreateAccount(addrResurrect)
	direct.SetBalance(addrResurrect, uint256.NewInt(999), 0)
	direct.SetNonce(addrResurrect, 1, 0)
	direct.SetCode(addrResurrect, []byte{0x60, 0x02}, 0)
	direct.SetState(addrResurrect, slotResurrectNew, common.HexToHash("0xffff"))
	direct.CreateAccount(addrCodeNew)
	direct.SetBalance(addrCodeNew, uint256.NewInt(77), 0)
	direct.SetCode(addrCodeNew, []byte{0x60, 0x03}, 0)
	rootDirect := direct.IntermediateRoot(false)
	require.Equal(t, rootDirect, rootTrieOnly,
		"FlatDiff replay path must produce the same root as direct execution")
}

func TestApplyFlatDiffForCommitFast_PreservesParentStorageRootAfterOverlayExecution(t *testing.T) {
	db := NewDatabaseForTesting()

	addr := common.HexToAddress("0xoverlay-root")
	slot := common.HexToHash("0x01")

	initial, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	initial.CreateAccount(addr)
	initial.SetBalance(addr, uint256.NewInt(1), 0)
	root0, _, err := initial.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Block N changes storage. The resulting FlatDiff is used as an overlay
	// while block N+1 executes before root_N is available.
	blockNExec, err := New(root0, db)
	require.NoError(t, err)
	blockNExec.SetState(addr, slot, common.HexToHash("0xdead"))
	diffN := blockNExec.CommitSnapshot(false)

	blockNSRC, err := New(root0, db)
	require.NoError(t, err)
	blockNSRC.ApplyFlatDiffForCommit(diffN)
	rootN, _, err := blockNSRC.CommitWithUpdate(1, false, false)
	require.NoError(t, err)

	// Block N+1 sees block N's storage through the FlatDiff overlay, but its
	// account data.Root is still rooted at root0 because CommitSnapshot does
	// not hash storage. A fast replay at rootN must preserve rootN's account
	// storage root instead of copying this stale metadata root.
	blockN1Exec, err := NewWithFlatBase(root0, db, diffN)
	require.NoError(t, err)
	require.Equal(t, common.HexToHash("0xdead"), blockN1Exec.GetState(addr, slot))
	blockN1Exec.SetBalance(addr, uint256.NewInt(2), 0)
	diffN1 := blockN1Exec.CommitSnapshot(false)
	require.Contains(t, diffN1.Accounts, addr)
	require.NotContains(t, diffN1.Storage, addr)

	direct, err := New(rootN, db)
	require.NoError(t, err)
	direct.SetBalance(addr, uint256.NewInt(2), 0)
	rootDirect := direct.IntermediateRoot(false)

	slow, err := New(rootN, db)
	require.NoError(t, err)
	slow.ApplyFlatDiffForCommit(diffN1)
	rootSlow := slow.IntermediateRoot(false)

	fast, err := New(rootN, db)
	require.NoError(t, err)
	fast.ApplyFlatDiffForCommitFast(diffN1)
	rootFast := fast.IntermediateRoot(false)

	require.Equal(t, rootDirect, rootSlow)
	require.Equal(t, rootSlow, rootFast,
		"fast replay must preserve the parent root's storage trie when FlatDiff account metadata came from an overlay execution")
}

// TestFlatDiffOverlay_DestructByExecutingBlock covers the case
// TestFlatDiffOverlay_DestructAndResurrect does not: the destruct is performed
// by the block currently executing over the overlay, not encoded inside the
// FlatDiff. The overlay holds the parent block's post-state, so it is exactly
// the "previous database" a same-block destruct must not consult — reading it
// would return the pre-destruct value where a non-pipelined node returns zero.
func TestFlatDiffOverlay_DestructByExecutingBlock(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xdead03")
	slot := common.HexToHash("0x01")
	stale := common.HexToHash("0xbeef")

	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 5, 0)
	sdb.SetState(addr, slot, common.HexToHash("0x1111"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Parent block rewrote the slot; the overlay carries that post-state.
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    6,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   map[common.Address]map[common.Hash]common.Hash{addr: {slot: stale}},
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}

	overlayDB, err := NewWithFlatBase(root, db, diff)
	require.NoError(t, err)

	// Sanity: without a destruct the overlay is authoritative for the slot.
	require.Equal(t, stale, overlayDB.GetState(addr, slot))

	// Now the executing block destructs the account and recreates it at the
	// same address, then reads the slot it never rewrote.
	overlayDB.SelfDestruct(addr)
	overlayDB.Finalise(true)
	overlayDB.CreateAccount(addr)

	require.Equal(t, common.Hash{}, overlayDB.GetState(addr, slot),
		"slot must read as zero after an in-block destruct; the overlay is the previous database")
	require.Equal(t, common.Hash{}, overlayDB.GetCommittedState(addr, slot),
		"committed read must also be zero after an in-block destruct")
}

// TestFlatDiffOverlay_ParentDestructKeepsOverlayStorage is the mirror image of
// TestFlatDiffOverlay_DestructByExecutingBlock, pinning why same-block and
// parent-block destructs are tracked in separate sets: ApplyFlatDiff seeds
// stateObjectsDestruct with the PARENT block's destructs, and a resurrected
// account's overlay storage — written after that destruct — must still be
// served. Folding the two sets into one (or hoisting the stateObjectsDestruct
// check above the overlay probe) would silently zero these reads.
func TestFlatDiffOverlay_ParentDestructKeepsOverlayStorage(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xdead04")
	oldSlot := common.HexToHash("0x01")
	newSlot := common.HexToHash("0x02")

	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 5, 0)
	sdb.SetState(addr, oldSlot, common.HexToHash("0x1111"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	// Parent block destructed addr, resurrected it, and wrote newSlot.
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    10,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   map[common.Address]map[common.Hash]common.Hash{addr: {newSlot: common.HexToHash("0x2222")}},
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}

	overlayDB, err := NewWithFlatBase(root, db, diff)
	require.NoError(t, err)

	require.Equal(t, uint64(10), overlayDB.GetNonce(addr))
	require.Equal(t, common.HexToHash("0x2222"), overlayDB.GetState(addr, newSlot),
		"parent-block destruct must not shadow overlay storage written after the resurrection")
	require.Equal(t, common.HexToHash("0x2222"), overlayDB.GetCommittedState(addr, newSlot),
		"committed read must also serve the overlay for a parent-destructed, resurrected account")
	require.Equal(t, common.Hash{}, overlayDB.GetState(addr, oldSlot),
		"pre-destruct storage stays hidden")
}
