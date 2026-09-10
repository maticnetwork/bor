package state

import (
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// The tests in this file exercise pipelined-SRC-authored code paths in
// statedb.go at fine enough resolution to kill specific mutation-testing
// survivors. Each assertion targets a distinct branch, call site, or return
// path reported by diffguard's T1/T2 mutation pass.

// ---- mutatedStorageKeys ----

func TestMutatedStorageKeys_MissingAddr(t *testing.T) {
	diff := &FlatDiff{Storage: make(map[common.Address]map[common.Hash]common.Hash)}
	require.Nil(t, diff.mutatedStorageKeys(common.HexToAddress("0x1234")))
}

func TestMutatedStorageKeys_PresentAddr(t *testing.T) {
	addr := common.HexToAddress("0x1234")
	diff := &FlatDiff{
		Storage: map[common.Address]map[common.Hash]common.Hash{
			addr: {
				common.HexToHash("0xaa"): common.HexToHash("0x01"),
				common.HexToHash("0xbb"): common.HexToHash("0x02"),
			},
		},
	}
	got := diff.mutatedStorageKeys(addr)
	require.Len(t, got, 2)
	seen := map[common.Hash]bool{}
	for _, k := range got {
		seen[k] = true
	}
	require.True(t, seen[common.HexToHash("0xaa")])
	require.True(t, seen[common.HexToHash("0xbb")])
}

// ---- touchAddressAndStorage / TouchAllAddresses ----

func TestTouchAddressAndStorage_LoadsBalanceWithNoSlots(t *testing.T) {
	// Kills: removal of dst.GetBalance(addr) when slots slice is empty. Without
	// that call, a FlatDiff that names an account but no slots would leave dst
	// untracked entirely.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xtouch1")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(99), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts:    map[common.Address]types.StateAccount{addr: {}},
		Storage:     make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:   make(map[common.Address]struct{}),
		Code:        make(map[common.Hash][]byte),
		ReadStorage: make(map[common.Address][]common.Hash),
	}

	dst, err := New(root, db)
	require.NoError(t, err)
	diff.TouchAllAddresses(dst)

	_, loaded := dst.stateObjects[addr]
	require.True(t, loaded, "TouchAllAddresses must load addr even when the slot list is empty")
}

func TestTouchAddressAndStorage_LoadsEachSlot(t *testing.T) {
	// Kills: removal of dst.GetCommittedState(addr, slot) inside the loop.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xtouch2")
	slot1 := common.HexToHash("0xa1")
	slot2 := common.HexToHash("0xa2")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	sdb.SetState(addr, slot1, common.HexToHash("0xb1"))
	sdb.SetState(addr, slot2, common.HexToHash("0xb2"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{addr: {}},
		Storage: map[common.Address]map[common.Hash]common.Hash{
			addr: {slot1: {}, slot2: {}},
		},
		Destructs:   make(map[common.Address]struct{}),
		Code:        make(map[common.Hash][]byte),
		ReadStorage: make(map[common.Address][]common.Hash),
	}

	dst, err := New(root, db)
	require.NoError(t, err)
	diff.TouchAllAddresses(dst)

	obj := dst.getStateObject(addr)
	require.NotNil(t, obj)
	_, s1 := obj.originStorage[slot1]
	_, s2 := obj.originStorage[slot2]
	require.True(t, s1, "slot1 must be tracked in dst.originStorage")
	require.True(t, s2, "slot2 must be tracked in dst.originStorage")
}

func TestTouchAllAddresses_ReadSetSlotsLoaded(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xtouch3")
	slot := common.HexToHash("0xcafe")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(42), 0)
	sdb.SetState(addr, slot, common.HexToHash("0xbeef"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts:    make(map[common.Address]types.StateAccount),
		Storage:     make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:   make(map[common.Address]struct{}),
		Code:        make(map[common.Hash][]byte),
		ReadSet:     []common.Address{addr},
		ReadStorage: map[common.Address][]common.Hash{addr: {slot}},
	}

	dst, err := New(root, db)
	require.NoError(t, err)
	diff.TouchAllAddresses(dst)

	obj := dst.getStateObject(addr)
	require.NotNil(t, obj)
	_, loaded := obj.originStorage[slot]
	require.True(t, loaded, "ReadSet slot must be tracked in dst.originStorage")
}

func TestTouchAllAddresses_DestructsLoadBalance(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xtouch4")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(5), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts:  make(map[common.Address]types.StateAccount),
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}

	dst, err := New(root, db)
	require.NoError(t, err)
	diff.TouchAllAddresses(dst)

	_, ok := dst.stateObjects[addr]
	require.True(t, ok, "destruct entry must cause dst to load addr via GetBalance")
}

func TestTouchAllAddresses_NonExistentReadsRegistered(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	missing := common.HexToAddress("0xtouch5-missing")
	diff := &FlatDiff{
		Accounts:         make(map[common.Address]types.StateAccount),
		Storage:          make(map[common.Address]map[common.Hash]common.Hash),
		Destructs:        make(map[common.Address]struct{}),
		Code:             make(map[common.Hash][]byte),
		NonExistentReads: []common.Address{missing},
	}

	dst, err := New(root, db)
	require.NoError(t, err)
	diff.TouchAllAddresses(dst)

	_, ok := dst.nonExistentReads[missing]
	require.True(t, ok, "NonExistentReads addr must be tracked via GetBalance")
}

// ---- captureMutation (via CommitSnapshot) ----

func TestCommitSnapshot_DestructedAccountExcludedFromAccounts(t *testing.T) {
	// Kills: removal of `if op.isDelete()` early return. Without it, a destructed
	// account flows into diff.Accounts alongside Destructs.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xcap1")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(100), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)
	sdb2.SelfDestruct(addr)
	diff := sdb2.CommitSnapshot(false)

	require.Contains(t, diff.Destructs, addr)
	require.NotContains(t, diff.Accounts, addr, "destructed addr must not also appear in Accounts")
}

func TestCommitSnapshot_DirtyCodeCaptured(t *testing.T) {
	// Kills: removal of the dirtyCode branch that populates diff.Code.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xcap2")
	code := []byte{0x60, 0x01, 0x60, 0x00, 0xf3}
	sdb.CreateAccount(addr)
	sdb.SetCode(addr, code, 0)

	diff := sdb.CommitSnapshot(false)

	codeHash := common.BytesToHash(crypto.Keccak256(code))
	got, ok := diff.Code[codeHash]
	require.True(t, ok, "dirty code must populate diff.Code")
	require.Equal(t, code, got)
}

func TestCaptureMutation_OrphanMutationIsSkipped(t *testing.T) {
	// Kills: removal of the `if !ok { return }` guard at line 2119. Without the
	// guard, the following `diff.Accounts[addr] = obj.data` dereferences a nil
	// stateObject and panics.
	//
	// Normal execution never produces an orphan mutation (every Set* path that
	// records a mutation also installs a stateObject), so we construct the
	// state by hand.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xorphan")
	sdb.mutations[addr] = &mutation{typ: update}

	require.NotPanics(t, func() { _ = sdb.CommitSnapshot(false) })
	diff := sdb.CommitSnapshot(false)
	require.NotContains(t, diff.Accounts, addr,
		"orphan mutation (no stateObject) must not produce a diff.Accounts entry")
}

func TestCommitSnapshot_NoCodeWithoutDirtyFlag(t *testing.T) {
	// Complements the previous test: verifies diff.Code stays empty when no code
	// is deployed, so the dirtyCode branch is genuinely guarded.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xcap3")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)

	diff := sdb.CommitSnapshot(false)
	require.Empty(t, diff.Code)
}

// ---- captureObjectStorage ----

func TestCaptureObjectStorage_NoPendingLeavesStorageEmpty(t *testing.T) {
	// Kills: `len(pendingStorage) > 0` → `>= 0` (always true). Under mutation,
	// an empty pendingStorage would still add an empty map to diff.Storage[addr].
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xcos1")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(7), 0)

	diff := sdb.CommitSnapshot(false)

	require.Contains(t, diff.Accounts, addr)
	_, hasStorage := diff.Storage[addr]
	require.False(t, hasStorage, "addr with no pending writes must not have a Storage entry (even empty)")
}

func TestCaptureObjectStorage_SplitsPendingAndRead(t *testing.T) {
	// Kills:
	//  - removal of readSlots append (line 2146)
	//  - removal of `if len(readSlots) > 0 { ... ReadStorage[addr] = readSlots }` (line 2150)
	//  - inversion of the `len(originStorage) == 0` guard (line 2141)
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xcos2")
	readSlot := common.HexToHash("0xa1")
	writeSlot := common.HexToHash("0xa2")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	sdb.SetState(addr, readSlot, common.HexToHash("0xb1"))
	sdb.SetState(addr, writeSlot, common.HexToHash("0xb2"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)
	// Read-only access to readSlot (loads origin only).
	_ = sdb2.GetState(addr, readSlot)
	// Write writeSlot so the account is mutated.
	sdb2.SetState(addr, writeSlot, common.HexToHash("0xb3"))

	diff := sdb2.CommitSnapshot(false)

	slots := diff.Storage[addr]
	require.Contains(t, slots, writeSlot, "writeSlot must be in Storage")
	require.NotContains(t, slots, readSlot, "readSlot must NOT be in Storage")

	reads := diff.ReadStorage[addr]
	require.Contains(t, reads, readSlot, "readSlot must be in ReadStorage")
	require.NotContains(t, reads, writeSlot, "writeSlot must NOT be in ReadStorage")
}

// ---- captureReadOnlyAccount ----

func TestCaptureReadOnlyAccount_AddsToReadSet(t *testing.T) {
	// Kills: removal of the `len(originStorage) == 0` early-return guard
	// (line 2167). Without it, diff.ReadStorage[addr] would be populated with
	// an empty slice for read-only accounts that didn't touch any slots.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xro1")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(55), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)
	_ = sdb2.GetBalance(addr)

	diff := sdb2.CommitSnapshot(false)

	require.Contains(t, diff.ReadSet, addr)
	require.NotContains(t, diff.Accounts, addr, "read-only access must not populate Accounts")
	require.NotContains(t, diff.ReadStorage, addr,
		"read-only addr without slot accesses must not have a ReadStorage entry")
}

func TestCaptureReadOnlyAccount_SkipMutated(t *testing.T) {
	// Kills: removal of `if isMutation { return }` guard (line 2160).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xro2")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(77), 0)

	diff := sdb.CommitSnapshot(false)

	require.Contains(t, diff.Accounts, addr)
	require.NotContains(t, diff.ReadSet, addr, "mutated addr must not appear in ReadSet")
}

func TestCaptureReadOnlyAccount_SkipDestructed(t *testing.T) {
	// Kills: removal of `if isDestruct { return }` guard (line 2163).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xro3")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(88), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)
	sdb2.SelfDestruct(addr)

	diff := sdb2.CommitSnapshot(false)

	require.Contains(t, diff.Destructs, addr)
	require.NotContains(t, diff.ReadSet, addr, "destructed addr must not appear in ReadSet")
}

func TestCaptureReadOnlyAccount_PopulatesReadStorage(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xro4")
	slot := common.HexToHash("0xdead")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	sdb.SetState(addr, slot, common.HexToHash("0xcafe"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)
	_ = sdb2.GetBalance(addr)
	_ = sdb2.GetState(addr, slot)

	diff := sdb2.CommitSnapshot(false)
	require.Contains(t, diff.ReadSet, addr)
	require.Contains(t, diff.ReadStorage[addr], slot)
}

// ---- captureNonExistentRead ----

func TestCaptureNonExistentRead_AddsMissingAddr(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	missing := common.HexToAddress("0xnr1")
	_ = sdb.GetBalance(missing)

	diff := sdb.CommitSnapshot(false)
	require.Contains(t, diff.NonExistentReads, missing)
}

func TestCaptureNonExistentRead_SkipMutated(t *testing.T) {
	// Kills: removal of `if isMutation { return }` guard (line 2181). An address
	// that was looked up (missing) and then created must not appear in
	// NonExistentReads.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xnr2")

	_ = sdb.GetBalance(addr)
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)

	diff := sdb.CommitSnapshot(false)

	require.Contains(t, diff.Accounts, addr)
	require.NotContains(t, diff.NonExistentReads, addr, "created addr must not leak into NonExistentReads")
}

func TestCaptureNonExistentRead_SkipOrphanMutation(t *testing.T) {
	// Kills: removal of the `if isMutation { return }` guard at line 2181.
	// Normal flow can't exercise this because: when an addr is in mutations,
	// it's also in stateObjects, so the 2184 guard catches it anyway — making
	// 2181 observationally equivalent. We distinguish them with an orphan
	// mutation: mutations[addr] set but stateObjects[addr] absent. Under the
	// 2181 mutation, execution falls through to the 2184 check (which also
	// passes since stateObjects is empty) and appends to NonExistentReads.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xnr_orphan")
	sdb.mutations[addr] = &mutation{typ: update}
	if sdb.nonExistentReads == nil {
		sdb.nonExistentReads = make(map[common.Address]struct{})
	}
	sdb.nonExistentReads[addr] = struct{}{}

	diff := sdb.CommitSnapshot(false)
	require.NotContains(t, diff.NonExistentReads, addr,
		"orphan mutation in nonExistentReads must be filtered by the isMutation guard")
}

func TestCaptureNonExistentRead_SkipExistingStateObject(t *testing.T) {
	// Kills: removal of `if _, ok := stateObjects[addr]; ok { return }` guard
	// (line 2184). Normally unreachable — force the state by seeding
	// nonExistentReads directly for an addr that already has a stateObject.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xnr3")

	// Load into stateObjects via GetBalance (account doesn't exist yet, so no
	// state object is created). Instead, create + finalise to ensure it's in
	// stateObjects, then manually inject nonExistentReads. Without finalise the
	// account stays in mutations, which is already covered by the previous test.
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	sdb.Finalise(false)
	// Drop the mutation so captureNonExistentRead's `isMutation` guard is not hit.
	delete(sdb.mutations, addr)
	// Inject non-existent read for an addr that now lives in stateObjects.
	if sdb.nonExistentReads == nil {
		sdb.nonExistentReads = make(map[common.Address]struct{})
	}
	sdb.nonExistentReads[addr] = struct{}{}

	diff := sdb.CommitSnapshot(false)

	require.NotContains(t, diff.NonExistentReads, addr,
		"addr present in stateObjects must be excluded from NonExistentReads")
}

// ---- ApplyFlatDiff ----

func TestApplyFlatDiff_DestructPopulatesDestructMap(t *testing.T) {
	// Kills: removal of `if !already { s.stateObjectsDestruct[addr] = newObject(...) }` (line 2208).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafd1")
	diff := &FlatDiff{
		Accounts:  make(map[common.Address]types.StateAccount),
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}
	sdb.ApplyFlatDiff(diff)
	_, ok := sdb.stateObjectsDestruct[addr]
	require.True(t, ok, "ApplyFlatDiff must register destruct in stateObjectsDestruct")
}

func TestApplyFlatDiff_PreservesExistingDestructEntry(t *testing.T) {
	// Kills: inversion of the `!already` guard (line 2208). If the guard flips,
	// the pre-existing entry would be overwritten by a blank newObject.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafd2")

	sentinel := newObject(sdb, addr, &types.StateAccount{Nonce: 999})
	sdb.stateObjectsDestruct[addr] = sentinel

	diff := &FlatDiff{
		Accounts:  make(map[common.Address]types.StateAccount),
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}
	sdb.ApplyFlatDiff(diff)

	require.Same(t, sentinel, sdb.stateObjectsDestruct[addr],
		"pre-existing destruct entry must be preserved")
}

func TestApplyFlatDiff_InstallsAccountInStateObjects(t *testing.T) {
	// Covers applyFlatAccountOverlay: newObject + stateObjects[addr] = obj.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xafo1")
	acct := types.StateAccount{
		Nonce:    7,
		Balance:  uint256.NewInt(123),
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}
	diff := &FlatDiff{
		Accounts:  map[common.Address]types.StateAccount{addr: acct},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	sdb.ApplyFlatDiff(diff)

	obj, ok := sdb.stateObjects[addr]
	require.True(t, ok, "overlayed account must be in stateObjects")
	require.Equal(t, uint64(7), obj.data.Nonce)
	require.Equal(t, uint256.NewInt(123), obj.data.Balance)
}

func TestApplyFlatDiff_InstallsCodeOverlay(t *testing.T) {
	// Covers applyFlatAccountOverlay code branch.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xafo2")
	code := []byte{0x60, 0x11, 0x60, 0x00, 0xf3}
	codeHash := common.BytesToHash(crypto.Keccak256(code))
	acct := types.StateAccount{
		Nonce:    1,
		Balance:  uint256.NewInt(0),
		Root:     types.EmptyRootHash,
		CodeHash: codeHash.Bytes(),
	}
	diff := &FlatDiff{
		Accounts:  map[common.Address]types.StateAccount{addr: acct},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      map[common.Hash][]byte{codeHash: code},
	}
	sdb.ApplyFlatDiff(diff)

	obj, ok := sdb.stateObjects[addr]
	require.True(t, ok)
	require.Equal(t, code, obj.code, "FlatDiff code must be carried in obj.code")
	require.False(t, obj.dirtyCode, "overlayed code must NOT be marked dirty")
}

func TestApplyFlatDiff_InstallsStorageOverlay(t *testing.T) {
	// Covers applyFlatAccountOverlay storage branch.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)

	addr := common.HexToAddress("0xafo3")
	slot := common.HexToHash("0xa1")
	value := common.HexToHash("0xb1")
	acct := types.StateAccount{
		Nonce:    1,
		Balance:  uint256.NewInt(0),
		Root:     types.EmptyRootHash,
		CodeHash: types.EmptyCodeHash.Bytes(),
	}
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{addr: acct},
		Storage: map[common.Address]map[common.Hash]common.Hash{
			addr: {slot: value},
		},
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	sdb.ApplyFlatDiff(diff)

	obj, ok := sdb.stateObjects[addr]
	require.True(t, ok)
	got, loaded := obj.originStorage[slot]
	require.True(t, loaded, "FlatDiff slot must populate originStorage")
	require.Equal(t, value, got)
}

func TestNewWithFlatBase_SuccessInstallsFlatDiffRef(t *testing.T) {
	// Covers the success path and the `if flatDiff != nil` branch.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts:  make(map[common.Address]types.StateAccount),
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	overlay, err := NewWithFlatBase(root, db, diff)
	require.NoError(t, err)
	require.NotNil(t, overlay)
	require.Same(t, diff, overlay.flatDiffRef, "flatDiffRef must reference the supplied diff")
}

func TestNewWithFlatBase_NilFlatDiffLeavesRefNil(t *testing.T) {
	// Covers the `if flatDiff != nil` guard (negative case).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	overlay, err := NewWithFlatBase(root, db, nil)
	require.NoError(t, err)
	require.NotNil(t, overlay)
	require.Nil(t, overlay.flatDiffRef, "nil FlatDiff must not overwrite flatDiffRef")
}

func TestApplyFlatMutation_StorageWritesApplied(t *testing.T) {
	// Covers applyFlatMutation's storage loop (SetState per slot).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafm_storage")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)

	slot := common.HexToHash("0xabc")
	value := common.HexToHash("0xdef")
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage: map[common.Address]map[common.Hash]common.Hash{
			addr: {slot: value},
		},
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	require.Equal(t, value, sdb2.GetState(addr, slot), "applyFlatMutation must SetState for each storage slot")
}

// ---- ApplyFlatDiffForCommit / applyFlatMutation ----

func TestApplyFlatDiffForCommit_PureDestructTriggersSelfDestruct(t *testing.T) {
	// Kills: removal of `s.SelfDestruct(addr)` call (line 2258).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafc1")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(42), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts:  make(map[common.Address]types.StateAccount),
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	obj := sdb2.getStateObject(addr)
	require.NotNil(t, obj)
	require.True(t, obj.selfDestructed, "SelfDestruct must have been invoked")
}

func TestApplyFlatDiffForCommit_ResurrectionSkipsSelfDestruct(t *testing.T) {
	// Kills:
	//  - removal of `if resurrected { continue }` (line 2255). Without the
	//    skip, SelfDestruct(addr) runs before applyFlatMutation, which zeros
	//    the balance on the pre-block object — the same object that
	//    applyFlatMutation later snapshots into stateObjectsDestruct. Asserting
	//    that the snapshot still carries the ORIGINAL (non-zero) balance
	//    distinguishes the two paths.
	//  - removal of `s.SelfDestruct(addr)` call on the non-resurrected path is
	//    covered by TestApplyFlatDiffForCommit_PureDestructTriggersSelfDestruct.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafc2")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(5), 0)
	sdb.SetNonce(addr, 1, 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    100,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	require.Equal(t, uint64(100), sdb2.GetNonce(addr), "resurrected account must adopt new nonce")

	prev, destructed := sdb2.stateObjectsDestruct[addr]
	require.True(t, destructed, "pre-block object must be recorded in stateObjectsDestruct")
	require.Equal(t, uint64(1), prev.data.Nonce, "stateObjectsDestruct must snapshot PRE-block nonce")
	require.Equal(t, uint256.NewInt(5), prev.data.Balance,
		"stateObjectsDestruct must carry PRE-block balance; if SelfDestruct ran it would be zero")
	require.False(t, prev.selfDestructed,
		"prev must NOT be marked self-destructed — that would indicate SelfDestruct was called")
}

func TestApplyFlatMutation_SetNonceCalled(t *testing.T) {
	// Kills: removal of `s.SetNonce(...)` (line 2291).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafm1")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.SetBalance(addr, uint256.NewInt(10), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    50,
				Balance:  uint256.NewInt(10),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	require.Equal(t, uint64(50), sdb2.GetNonce(addr))
}

func TestApplyFlatMutation_SetBalanceCalled(t *testing.T) {
	// Kills: removal of `s.SetBalance(...)` (line 2292).
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafm2")
	sdb.CreateAccount(addr)
	sdb.SetNonce(addr, 1, 0)
	sdb.SetBalance(addr, uint256.NewInt(10), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)

	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    1,
				Balance:  uint256.NewInt(9999),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      make(map[common.Hash][]byte),
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	require.Equal(t, uint256.NewInt(9999), sdb2.GetBalance(addr))
}

func TestApplyFlatMutation_DestructBranchDeletesStateObject(t *testing.T) {
	// Kills:
	//  - removal of `if destructed` branch entirely (line 2270)
	//  - removal of `if !already` inner guard (line 2271)
	//  - inversion of `prev != nil` check (line 2272)
	//  - removal of `delete(s.stateObjects, addr)` (line 2276)
	//
	// We distinguish these by asserting:
	//  - stateObjectsDestruct[addr] contains the PRE-block nonce (7), not the new one.
	//  - the post-commit GetNonce is the NEW value (99), which is only possible if
	//    the old stateObjects entry was deleted so SetNonce created a fresh object.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafm3")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(500), 0)
	sdb.SetNonce(addr, 7, 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)
	// Pre-load so stateObjects contains the pre-block object.
	require.Equal(t, uint64(7), sdb2.GetNonce(addr))
	_, had := sdb2.stateObjects[addr]
	require.True(t, had)

	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    99,
				Balance:  uint256.NewInt(1),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: map[common.Address]struct{}{addr: {}},
		Code:      make(map[common.Hash][]byte),
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	prev, destructed := sdb2.stateObjectsDestruct[addr]
	require.True(t, destructed, "stateObjectsDestruct must contain addr")
	require.Equal(t, uint64(7), prev.data.Nonce,
		"stateObjectsDestruct must hold pre-block nonce; mutating delete(stateObjects) leaves the same pointer here and corrupts this to 99")

	require.Equal(t, uint64(99), sdb2.GetNonce(addr))
	require.Equal(t, uint256.NewInt(1), sdb2.GetBalance(addr))
}

func TestApplyFlatMutation_WithCodeCallsSetCode(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xafm4")
	sdb.CreateAccount(addr)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	sdb2, err := New(root, db)
	require.NoError(t, err)

	code := []byte{0x60, 0x02, 0x60, 0x00, 0xf3}
	codeHash := common.BytesToHash(crypto.Keccak256(code))
	diff := &FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    1,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: codeHash.Bytes(),
			},
		},
		Storage:   make(map[common.Address]map[common.Hash]common.Hash),
		Destructs: make(map[common.Address]struct{}),
		Code:      map[common.Hash][]byte{codeHash: code},
	}
	sdb2.ApplyFlatDiffForCommit(diff)

	require.Equal(t, code, sdb2.GetCode(addr))
	require.Equal(t, codeHash, sdb2.GetCodeHash(addr))
}

// ---- NewWithFlatBase ----

func TestNewWithFlatBase_PropagatesReaderError(t *testing.T) {
	// Kills: replace-return-value mutation on `return nil, err` (line 2306).
	db := NewDatabaseForTesting()
	// Root is not present in the DB, so the underlying New() returns an error
	// when opening the trie reader.
	badRoot := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	sdb, err := NewWithFlatBase(badRoot, db, nil)
	require.Error(t, err, "bad root must surface the reader error")
	require.Nil(t, sdb)
}

// ---- WasStorageSlotRead ----

func TestWasStorageSlotRead_AddrNotLoadedReturnsFalse(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	missing := common.HexToAddress("0xws1")
	require.False(t, sdb.WasStorageSlotRead(missing, common.HexToHash("0x01")))
}

func TestWasStorageSlotRead_SlotNotReadReturnsFalse(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xws2")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	require.False(t, sdb.WasStorageSlotRead(addr, common.HexToHash("0x01")))
}

// ---- PropagateReadsTo ----

func TestPropagateReadsTo_LoadsAddrIntoDst(t *testing.T) {
	// Kills: removal of `dst.GetBalance(addr)` call (line 2494). Assertion
	// inspects dst.stateObjects directly BEFORE issuing any read that would
	// incidentally populate it.
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xprop1")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	src, err := New(root, db)
	require.NoError(t, err)
	dst, err := New(root, db)
	require.NoError(t, err)

	src.GetBalance(addr)
	src.PropagateReadsTo(dst)

	_, ok := dst.stateObjects[addr]
	require.True(t, ok, "PropagateReadsTo must populate dst.stateObjects via GetBalance")
}

func TestPropagateReadsTo_LoadsStorageSlotsIntoDst(t *testing.T) {
	db := NewDatabaseForTesting()
	sdb, err := New(types.EmptyRootHash, db)
	require.NoError(t, err)
	addr := common.HexToAddress("0xprop2")
	slot := common.HexToHash("0xcaffe")
	sdb.CreateAccount(addr)
	sdb.SetBalance(addr, uint256.NewInt(1), 0)
	sdb.SetState(addr, slot, common.HexToHash("0xbadc0de"))
	root, _, err := sdb.CommitWithUpdate(0, false, false)
	require.NoError(t, err)

	src, err := New(root, db)
	require.NoError(t, err)
	dst, err := New(root, db)
	require.NoError(t, err)

	src.GetBalance(addr)
	src.GetState(addr, slot)
	src.PropagateReadsTo(dst)

	dstObj, ok := dst.stateObjects[addr]
	require.True(t, ok)
	_, slotLoaded := dstObj.originStorage[slot]
	require.True(t, slotLoaded, "origin slot must propagate to dst")
}
