package state

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/triedb"
)

// ---------------------------------------------------------------------------
// Validate / ValidateCategory / Diagnose — cover the trivial wrappers and
// diagnostic path.
// ---------------------------------------------------------------------------

// TestPDB_Validate_WrappersReturnSameResult verifies Validate and
// ValidateCategory agree with ValidateDetailed.
func TestPDB_Validate_WrappersReturnSameResult(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()

	// Pass case.
	if !pdb.Validate() {
		t.Fatal("Validate on empty read set should pass")
	}
	if cat := pdb.ValidateCategory(); cat != "" {
		t.Fatalf("ValidateCategory on pass: got %q, want empty", cat)
	}

	// Fail case: nonce read becomes stale.
	addr := common.HexToAddress("0x1")
	key := blockstm.NewSubpathKey(addr, NoncePath)
	store.WriteInc(key, 2, 0, uint64(5))
	pdb.GetNonce(addr) // records the read
	store.WriteInc(key, 2, 1, uint64(6))

	if pdb.Validate() {
		t.Fatal("Validate must fail after writer incarnation changed")
	}
	if cat := pdb.ValidateCategory(); cat != "nonce" {
		t.Fatalf("ValidateCategory on fail: got %q, want 'nonce'", cat)
	}
}

// TestPDB_DiagnoseValidation returns per-failure diagnostic entries.
func TestPDB_DiagnoseValidation(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0x1")
	key := blockstm.NewStateKey(addr, common.HexToHash("0x2"))
	store.WriteInc(key, 2, 0, common.HexToHash("0xaa"))
	pdb.GetState(addr, common.HexToHash("0x2"))
	store.WriteInc(key, 2, 1, common.HexToHash("0xbb"))

	diags := pdb.DiagnoseValidation()
	if len(diags) != 1 {
		t.Fatalf("DiagnoseValidation: got %d diags, want 1", len(diags))
	}
	if diags[0].Category != "storage" {
		t.Fatalf("diag category: got %q, want 'storage'", diags[0].Category)
	}
}

// TestPDB_DiagnoseBalanceRead returns a balance diag when cumulative delta
// drifts from the recorded value.
func TestPDB_DiagnoseBalanceRead(t *testing.T) {
	pdb, _, bals := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0x1")
	pdb.GetBalance(addr) // records current delta (zero)
	// Inject a prior delta that wasn't there at record time.
	bals.WriteDelta(addr, 2, uint256.NewInt(100), uint256.NewInt(0))

	diags := pdb.DiagnoseValidation()
	found := false
	for _, d := range diags {
		if d.Category == "balance" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected balance diag, got %+v", diags)
	}
}

// ---------------------------------------------------------------------------
// MarkEstimate / CleanupEstimate / write-key accessors
// ---------------------------------------------------------------------------

// TestPDB_MarkAndCleanupEstimate marks writes as ESTIMATE and cleans up
// those the new incarnation didn't re-write.
func TestPDB_MarkAndCleanupEstimate(t *testing.T) {
	pdb, store, bals := newTestPDB(t, 3)
	pdb.EnableReadTracking()
	addr1 := common.HexToAddress("0x1")
	addr2 := common.HexToAddress("0x2")

	pdb.SetState(addr1, common.HexToHash("0x1"), common.HexToHash("0x11"))
	pdb.SetState(addr2, common.HexToHash("0x2"), common.HexToHash("0x22"))
	pdb.AddBalance(addr1, uint256.NewInt(5), tracing.BalanceChangeUnspecified)
	pdb.FlushToMVStore()

	oldWriteKeys := append([]blockstm.Key{}, pdb.WriteKeys...)
	oldBalAddrs := append([]common.Address{}, pdb.BalAddrs...)

	// Simulate re-exec start.
	pdb.MarkEstimate()
	key1 := blockstm.NewStateKey(addr1, common.HexToHash("0x1"))
	if !store.IsEstimate(key1, 3) {
		t.Fatal("MarkEstimate did not set flag on key1")
	}

	// Re-exec only touches addr1 — addr2 must be cleaned up.
	// Simulate a fresh PDB incarnation by clearing local maps first; in
	// production the executor hands workers a pooled PDB with Reset state.
	clear(pdb.localStorage)
	clear(pdb.localBalAdd)
	clear(pdb.localBalSub)
	pdb.Incarnation = 1
	pdb.WriteKeys = pdb.WriteKeys[:0]
	pdb.BalAddrs = pdb.BalAddrs[:0]
	pdb.SetState(addr1, common.HexToHash("0x1"), common.HexToHash("0x99"))
	pdb.FlushToMVStore()

	pdb.CleanupEstimate(oldWriteKeys, oldBalAddrs)

	// addr1 re-written: DONE.
	if store.IsEstimate(key1, 3) {
		t.Fatal("CleanupEstimate: re-written key still marked estimate")
	}
	// addr2 never re-written: CleanupEstimate must have removed it.
	key2 := blockstm.NewStateKey(addr2, common.HexToHash("0x2"))
	if _, _, _, found, _ := store.ReadVersionFull(key2, 10); found {
		t.Fatal("CleanupEstimate did not remove stale estimate entry")
	}

	// Balance: addr1 is in oldBalAddrs but not re-touched in new incarnation,
	// so CleanupEstimate must delete it.
	if _, _, found := bals.GetTxDelta(addr1, 3); found {
		t.Fatal("CleanupEstimate did not remove stale bal entry (not re-touched)")
	}
}

// TestPDB_GetWriteKeys_BalAddrs returns copies of the tracked sets.
func TestPDB_GetWriteKeys_BalAddrs(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0x1")
	pdb.SetState(addr, common.HexToHash("0x1"), common.HexToHash("0x11"))
	pdb.AddBalance(addr, uint256.NewInt(5), tracing.BalanceChangeUnspecified)

	wk := pdb.GetWriteKeys()
	if len(wk) != 1 {
		t.Fatalf("GetWriteKeys: got %d, want 1", len(wk))
	}
	ba := pdb.GetBalAddrs()
	if len(ba) != 1 || ba[0] != addr {
		t.Fatalf("GetBalAddrs: got %v, want [%x]", ba, addr)
	}
	// Mutations to the returned slices must not touch the originals.
	wk[0] = blockstm.Key{}
	ba[0] = common.Address{}
	if pdb.WriteKeys[0] == (blockstm.Key{}) {
		t.Fatal("GetWriteKeys returned aliased slice")
	}
	if pdb.BalAddrs[0] == (common.Address{}) {
		t.Fatal("GetBalAddrs returned aliased slice")
	}
}

// TestPDB_SetDeferMVWrites flips the flag and is read by flush helpers.
func TestPDB_SetDeferMVWrites(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 0)
	pdb.EnableReadTracking()
	pdb.SetDeferMVWrites(true)

	pdb.SetCode(common.HexToAddress("0x1"), []byte{0x60}, tracing.CodeChangeUnspecified)
	// Deferred: write must not land in MVStore until FlushToMVStore runs.
	key := blockstm.NewSubpathKey(common.HexToAddress("0x1"), CodePath)
	if _, _, _, found, _ := store.ReadVersionFull(key, 10); found {
		t.Fatal("DeferMVWrites=true: code write landed immediately")
	}
}

// ---------------------------------------------------------------------------
// Refund / access list / transient accessors
// ---------------------------------------------------------------------------

// TestPDB_Refund covers Add/Sub/Get in one flow.
func TestPDB_Refund(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)

	pdb.AddRefund(100)
	if got := pdb.GetRefund(); got != 100 {
		t.Fatalf("GetRefund after add: got %d, want 100", got)
	}
	pdb.SubRefund(30)
	if got := pdb.GetRefund(); got != 70 {
		t.Fatalf("GetRefund after sub: got %d, want 70", got)
	}
}

// TestPDB_SubRefund_UnderflowPanics verifies the defensive panic.
func TestPDB_SubRefund_UnderflowPanics(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("SubRefund must panic on underflow")
		}
	}()
	pdb.SubRefund(1)
}

// TestPDB_AccessList covers Add/SlotInAccessList/AddressInAccessList.
func TestPDB_AccessList(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabb")
	slot := common.HexToHash("0x1")

	if pdb.AddressInAccessList(addr) {
		t.Fatal("empty access list reported addr present")
	}
	if ok, _ := pdb.SlotInAccessList(addr, slot); ok {
		t.Fatal("empty access list reported addr slot")
	}

	pdb.AddAddressToAccessList(addr)
	if !pdb.AddressInAccessList(addr) {
		t.Fatal("AddAddressToAccessList: addr not present")
	}
	pdb.AddSlotToAccessList(addr, slot)
	ok1, ok2 := pdb.SlotInAccessList(addr, slot)
	if !ok1 || !ok2 {
		t.Fatalf("SlotInAccessList: got (%v, %v), want (true, true)", ok1, ok2)
	}
}

// ---------------------------------------------------------------------------
// Self-destruct family
// ---------------------------------------------------------------------------

// TestPDB_CrossTxSelfDestructVisibility verifies Fix #2: when tx A
// self-destructs an account and flushes to MVStore, tx B's reads see the
// account as gone. Without the fix (no SuicidePath publish + no priorDestructed
// gate in the getters), B would see stale base-state code/storage/nonce.
//
// Pre-EIP-6780 semantics: SelfDestruct moves the account into the destruct
// set at end of tx. Subsequent txs in the same block see Exist=false, code
// empty, storage zero, nonce zero, EXTCODEHASH zero.
func TestPDB_CrossTxSelfDestructVisibility(t *testing.T) {
	pdbA, store, bals := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabbcc")
	slot := common.HexToHash("0x01")

	// Pre-block: addr has code, storage, nonce, balance (simulate via PDB writes
	// from tx 0). Without a base-state account this still exercises the V2
	// MVStore path: SetCode/SetState/SetNonce land in MVStore on flush, then
	// SelfDestruct publishes the SuicidePath marker.
	pdbA.AddBalance(addr, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
	pdbA.SetCode(addr, []byte{0x60, 0x00}, tracing.CodeChangeUnspecified)
	pdbA.SetNonce(addr, 7, tracing.NonceChangeUnspecified)
	pdbA.SetState(addr, slot, common.HexToHash("0xdeadbeef"))
	pdbA.SetDeferMVWrites(true)
	pdbA.EnableReadTracking()
	// Mirror the EVM handler: it clears the balance, SelfDestruct does not.
	pdbA.SubBalance(addr, uint256.NewInt(100), tracing.BalanceDecreaseSelfdestruct)
	pdbA.SelfDestruct(addr)
	pdbA.FlushToMVStore()

	// Now tx B reads addr.
	base := pdbA.base
	pdbB := NewParallelStateDB(1, base, store, bals)
	pdbB.EnableReadTracking()

	if pdbB.Exist(addr) {
		t.Error("Exist: expected false after prior tx self-destruct, got true")
	}
	if got := pdbB.GetCode(addr); got != nil {
		t.Errorf("GetCode: expected nil after prior tx self-destruct, got %x", got)
	}
	if got := pdbB.GetCodeHash(addr); got != (common.Hash{}) {
		t.Errorf("GetCodeHash: expected zero hash, got %v", got)
	}
	if got := pdbB.GetNonce(addr); got != 0 {
		t.Errorf("GetNonce: expected 0, got %d", got)
	}
	if got := pdbB.GetState(addr, slot); got != (common.Hash{}) {
		t.Errorf("GetState: expected zero, got %v", got)
	}
	if got := pdbB.GetCommittedState(addr, slot); got != (common.Hash{}) {
		t.Errorf("GetCommittedState: expected zero, got %v", got)
	}
}

// TestPDB_CrossTxSelfDestructThenRecreate verifies that a same-block sequence
// of SELFDESTRUCT(A) followed by recreation (CreateAccount or value transfer)
// produces the right read for a subsequent tx. Without ordering-aware checks,
// priorDestructed acted as a permanent same-block tombstone — Exist /
// EXTCODEHASH / SLOAD all returned the destroyed view even after recreation,
// diverging from serial semantics.
//
// Two recreation paths exercised:
//  1. Explicit CREATE (CreateAccount / SetCode) → CreatePath written
//  2. Implicit recreation via value transfer → only MVBalanceStore touched,
//     no CreatePath write. Exist's balance fallback must still return true.
func TestPDB_CrossTxSelfDestructThenRecreate(t *testing.T) {
	t.Run("explicit create after destruct", func(t *testing.T) {
		pdb0, store, bals := newTestPDB(t, 0)
		addr := common.HexToAddress("0xaabbcc")

		// tx0: pre-existing addr destructed.
		pdb0.AddBalance(addr, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
		pdb0.SetCode(addr, []byte{0x60, 0x00}, tracing.CodeChangeUnspecified)
		pdb0.SetDeferMVWrites(true)
		pdb0.EnableReadTracking()
		pdb0.SelfDestruct(addr)
		pdb0.FlushToMVStore()

		// tx1: explicit recreate (CreateAccount writes CreatePath).
		pdb1 := NewParallelStateDB(1, pdb0.base, store, bals)
		pdb1.SetDeferMVWrites(true)
		pdb1.EnableReadTracking()
		pdb1.CreateAccount(addr)
		pdb1.FlushToMVStore()

		// tx2 reads. After recreation, account should exist, code is empty,
		// nonce is 0, EXTCODEHASH is EmptyCodeHash, storage stays wiped.
		pdb2 := NewParallelStateDB(2, pdb0.base, store, bals)
		pdb2.EnableReadTracking()

		if !pdb2.Exist(addr) {
			t.Error("Exist: expected true after recreate, got false")
		}
		if got := pdb2.GetCodeHash(addr); got != types.EmptyCodeHash {
			t.Errorf("GetCodeHash: expected EmptyCodeHash, got %v", got)
		}
		if got := pdb2.GetNonce(addr); got != 0 {
			t.Errorf("GetNonce: expected 0, got %d", got)
		}
		if got := pdb2.GetCode(addr); len(got) != 0 {
			t.Errorf("GetCode: expected empty, got %x", got)
		}
	})

	t.Run("implicit recreate via value transfer", func(t *testing.T) {
		pdb0, store, bals := newTestPDB(t, 0)
		addr := common.HexToAddress("0xddeeff")

		// tx0: pre-existing addr destructed.
		pdb0.AddBalance(addr, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
		pdb0.SetCode(addr, []byte{0x60, 0x01}, tracing.CodeChangeUnspecified)
		pdb0.SetDeferMVWrites(true)
		pdb0.EnableReadTracking()
		pdb0.SelfDestruct(addr)
		pdb0.FlushToMVStore()

		// tx1: implicit recreate via value transfer (no CreateAccount call,
		// only AddBalance → MVBalanceStore). CreatePath stays at tx0's
		// value (or empty); Exist must use the balance fallback.
		pdb1 := NewParallelStateDB(1, pdb0.base, store, bals)
		pdb1.SetDeferMVWrites(true)
		pdb1.EnableReadTracking()
		pdb1.AddBalance(addr, uint256.NewInt(50), tracing.BalanceChangeUnspecified)
		pdb1.FlushToMVStore()

		pdb2 := NewParallelStateDB(2, pdb0.base, store, bals)
		pdb2.EnableReadTracking()

		if !pdb2.Exist(addr) {
			t.Error("Exist: expected true after value-transfer recreate, got false")
		}
		// Recreated empty account: nonce=0, code empty, EXTCODEHASH=EmptyCodeHash.
		if got := pdb2.GetCodeHash(addr); got != types.EmptyCodeHash {
			t.Errorf("GetCodeHash: expected EmptyCodeHash, got %v", got)
		}
		if got := pdb2.GetNonce(addr); got != 0 {
			t.Errorf("GetNonce: expected 0, got %d", got)
		}
	})
}

// TestPDB_SelfDestruct marks the account as destructed without touching its
// balance. The balance is cleared by the EVM opcode handler's explicit
// SubBalance, not by SelfDestruct; the negative assertion below pins that split
// so re-adding the zeroing here cannot slip through unnoticed.
func TestPDB_SelfDestruct(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabb")
	pdb.AddBalance(addr, uint256.NewInt(42), tracing.BalanceChangeUnspecified)

	pdb.SelfDestruct(addr)
	if !pdb.HasSelfDestructed(addr) {
		t.Fatal("HasSelfDestructed: false after SelfDestruct")
	}
	if got := pdb.GetBalance(addr).Uint64(); got != 42 {
		t.Fatalf("SelfDestruct must leave the balance alone: got %d, want 42", got)
	}

	pdb.SubBalance(addr, uint256.NewInt(42), tracing.BalanceDecreaseSelfdestruct)
	if got := pdb.GetBalance(addr).Uint64(); got != 0 {
		t.Fatalf("balance after the handler's SubBalance: got %d, want 0", got)
	}
}

// TestPDB_SelfDestruct_RecordsSuicidePathWrite pins that SelfDestruct
// adds the SuicidePath key to WriteKeys so MarkEstimate / CleanupEstimate
// can reach the FlushToMVStore-written entry on re-execution. Without this,
// a stale SuicidePath entry from incarnation N survives into incarnation
// N+1's view and a downstream reader can pass validation against state
// that no longer exists — a state-root divergence path.
func TestPDB_SelfDestruct_RecordsSuicidePathWrite(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking() // recordWrite is gated on trackReads
	addr := common.HexToAddress("0xaabb")
	pdb.AddBalance(addr, uint256.NewInt(42), tracing.BalanceChangeUnspecified)

	pdb.SelfDestruct(addr)

	wantKey := blockstm.NewSubpathKey(addr, SuicidePath)
	found := false
	for _, k := range pdb.WriteKeys {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("SelfDestruct did not record SuicidePath write — MarkEstimate/CleanupEstimate would miss it on re-execution")
	}

	// Repeated SelfDestruct in the same tx must NOT add a duplicate entry.
	beforeLen := len(pdb.WriteKeys)
	pdb.SelfDestruct(addr)
	if len(pdb.WriteKeys) != beforeLen {
		t.Fatalf("repeated SelfDestruct duplicated SuicidePath in WriteKeys: %d → %d", beforeLen, len(pdb.WriteKeys))
	}
}

// TestPDB_IsNewContract_CreatedThisTx reports true for a contract created in
// this tx. This is the signal the EVM handler uses to decide whether a
// SELFDESTRUCT actually destructs, replacing SelfDestruct6780's second return.
func TestPDB_IsNewContract_CreatedThisTx(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabb")
	pdb.CreateContract(addr)
	pdb.AddBalance(addr, uint256.NewInt(5), tracing.BalanceChangeUnspecified)

	if !pdb.IsNewContract(addr) {
		t.Fatal("IsNewContract: false for a contract created in this tx")
	}
	pdb.SelfDestruct(addr)
	if !pdb.HasSelfDestructed(addr) {
		t.Fatal("HasSelfDestructed: false after SelfDestruct on a new contract")
	}
}

// TestPDB_IsNewContract_PreExisting reports false for a contract that predates
// this tx, so the EVM handler transfers the balance without destructing.
func TestPDB_IsNewContract_PreExisting(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabb")
	pdb.AddBalance(addr, uint256.NewInt(5), tracing.BalanceChangeUnspecified)
	// Note: no CreateContract — contract was not created this tx.

	if pdb.IsNewContract(addr) {
		t.Fatal("IsNewContract: true for a pre-existing contract")
	}
	if pdb.HasSelfDestructed(addr) {
		t.Fatal("HasSelfDestructed: must not be set without a SelfDestruct call")
	}
}

// ---------------------------------------------------------------------------
// SetBalance / balance helpers
// ---------------------------------------------------------------------------

// TestPDB_SetBalance_Up uses AddBalance path when new > prev.
func TestPDB_SetBalance_Up(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabb")
	pdb.AddBalance(addr, uint256.NewInt(10), tracing.BalanceChangeUnspecified)
	pdb.SetBalance(addr, uint256.NewInt(25), tracing.BalanceChangeUnspecified)
	if got := pdb.GetBalance(addr).Uint64(); got != 25 {
		t.Fatalf("SetBalance up: got %d, want 25", got)
	}
}

// TestPDB_SetBalance_Down uses SubBalance path when new < prev.
func TestPDB_SetBalance_Down(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xaabb")
	pdb.AddBalance(addr, uint256.NewInt(30), tracing.BalanceChangeUnspecified)
	pdb.SetBalance(addr, uint256.NewInt(10), tracing.BalanceChangeUnspecified)
	if got := pdb.GetBalance(addr).Uint64(); got != 10 {
		t.Fatalf("SetBalance down: got %d, want 10", got)
	}
}

// ---------------------------------------------------------------------------
// Empty / GetCodeHash / GetStateAndCommittedState / GetStorageRoot
// ---------------------------------------------------------------------------

// TestPDB_Empty_NonExistent returns true.
func TestPDB_Empty_NonExistent(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	if !pdb.Empty(common.HexToAddress("0xdead")) {
		t.Fatal("Empty on non-existent addr returned false")
	}
}

// TestPDB_Empty_NonZeroBalance returns false.
func TestPDB_Empty_NonZeroBalance(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	pdb.AddBalance(addr, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
	if pdb.Empty(addr) {
		t.Fatal("Empty on addr with non-zero balance returned true")
	}
}

// TestPDB_GetCodeHash_EmptyCode returns EmptyCodeHash after SetCode([]).
func TestPDB_GetCodeHash_EmptyCode(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	pdb.SetCode(addr, nil, tracing.CodeChangeUnspecified)
	if h := pdb.GetCodeHash(addr); h != types.EmptyCodeHash {
		t.Fatalf("GetCodeHash on empty code: got %s, want EmptyCodeHash", h.Hex())
	}
}

// TestPDB_GetStateAndCommittedState returns (localCurrent, base-committed).
func TestPDB_GetStateAndCommittedState(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x1")
	pdb.SetState(addr, slot, common.HexToHash("0x99"))

	cur, cmt := pdb.GetStateAndCommittedState(addr, slot)
	if cur != common.HexToHash("0x99") {
		t.Fatalf("current state: got %s, want 0x99", cur.Hex())
	}
	// Base empty → committed should be zero.
	if cmt != (common.Hash{}) {
		t.Fatalf("committed state: got %s, want zero", cmt.Hex())
	}
}

// TestPDB_GetStorageRoot delegates to SafeBase.
func TestPDB_GetStorageRoot(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	// Just exercise — empty account has zero root.
	_ = pdb.GetStorageRoot(common.HexToAddress("0xabcd"))
}

// ---------------------------------------------------------------------------
// Logs / preimages / accessors
// ---------------------------------------------------------------------------

// TestPDB_AddLog_AndLogs captures a log and returns it via Logs().
func TestPDB_AddLog_AndLogs(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	log := &types.Log{Address: common.HexToAddress("0x1")}
	pdb.AddLog(log)

	if got := pdb.Logs(); len(got) != 1 || got[0] != log {
		t.Fatalf("Logs: got %v, want 1 log", got)
	}
}

// TestPDB_AddPreimage stores and returns preimage via Inner StateDB.
func TestPDB_AddPreimage(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	h := common.HexToHash("0x1")
	pdb.AddPreimage(h, []byte{0xaa})
	if got, ok := pdb.preimages[h]; !ok || len(got) != 1 {
		t.Fatalf("AddPreimage: got %x ok=%v", got, ok)
	}
}

// TestPDB_GetLogs stamps txHash/blockNumber/blockHash/blockTime on captured logs.
func TestPDB_GetLogs(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.AddLog(&types.Log{})
	txHash := common.HexToHash("0xaa")
	out := pdb.GetLogs(txHash, 42, common.HexToHash("0xbb"), 1234)
	if len(out) != 1 || out[0].TxHash != txHash || out[0].BlockNumber != 42 || out[0].BlockTimestamp != 1234 {
		t.Fatalf("GetLogs: got %+v, want stamped", out[0])
	}
}

// TestPDB_TransientStorage covers set/get and same-value skip.
func TestPDB_TransientStorage(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0x1")
	key := common.HexToHash("0x1")
	v := common.HexToHash("0x22")

	pdb.SetTransientState(addr, key, v)
	if got := pdb.GetTransientState(addr, key); got != v {
		t.Fatalf("GetTransientState: got %s, want 0x22", got.Hex())
	}
	// Setting the same value must be a no-op (no new journal entry).
	nJournal := len(pdb.journalEntries)
	pdb.SetTransientState(addr, key, v)
	if len(pdb.journalEntries) != nJournal {
		t.Fatal("SetTransientState same-value must not journal")
	}
}

// ---------------------------------------------------------------------------
// Inner / Witness / AccessEvents trivial accessors
// ---------------------------------------------------------------------------

// TestPDB_InnerAccessors exercises wrappers used by Bor consensus hooks.
func TestPDB_InnerAccessors(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	if pdb.Inner() == nil {
		t.Fatal("Inner returned nil")
	}
	if pdb.Witness() != nil {
		t.Fatal("Witness: V2 always returns nil")
	}
	if pdb.AccessEvents() != nil {
		t.Fatal("AccessEvents: V2 always returns nil")
	}
}

// TestPDB_RecordTransfer appends a TransferRecord at the current log index.
func TestPDB_RecordTransfer(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.AddLog(&types.Log{})
	ok := pdb.RecordTransfer(common.HexToAddress("0x1"), common.HexToAddress("0x2"), uint256.NewInt(7))
	if !ok {
		t.Fatal("RecordTransfer returned false")
	}
	if len(pdb.Transfers) != 1 || pdb.Transfers[0].LogIdx != 1 {
		t.Fatalf("Transfers: got %+v, want LogIdx=1", pdb.Transfers)
	}
}

// TestPDB_Finalise is a no-op but must not panic.
func TestPDB_Finalise(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.Finalise(true)
}

// ---------------------------------------------------------------------------
// SettleTo end-to-end + settleBalanceOpsAndLogs
// ---------------------------------------------------------------------------

// TestPDB_SettleTo drives the full settlement: nonces, storage, code,
// balance ops with a transfer, self-destruct, preimages, and fee data.
func TestPDB_SettleTo(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)

	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	coinbase := common.HexToAddress("0xc0")
	pdb.Coinbase = coinbase

	// Seed sender balance on final so SubBalance doesn't underflow.
	final.AddBalance(sender, uint256.NewInt(100), tracing.BalanceChangeUnspecified)

	// Build tx local state.
	pdb.SetNonce(sender, 1, tracing.NonceChangeUnspecified)
	pdb.SetState(sender, common.HexToHash("0x1"), common.HexToHash("0xaa"))
	pdb.localCode[sender] = []byte{0x60, 0x00}
	amt := *uint256.NewInt(10)
	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: false},
		{Addr: recipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt}}
	pdb.AddLog(&types.Log{Address: sender})

	pdb.SettleTo(final)

	if got := final.GetNonce(sender); got != 1 {
		t.Fatalf("nonce after SettleTo: got %d, want 1", got)
	}
	if got := final.GetBalance(sender).Uint64(); got != 90 {
		t.Fatalf("sender balance after SettleTo: got %d, want 90", got)
	}
	if got := final.GetBalance(recipient).Uint64(); got != 10 {
		t.Fatalf("recipient balance after SettleTo: got %d, want 10", got)
	}
}

// TestPDB_SettleBalanceOpsAndLogs covers the fallback (non-transfer) paths:
// pure AddBalance and SubBalance ops without any Transfers applied directly.
func TestPDB_SettleBalanceOpsAndLogs_NoTransfers(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	a := common.HexToAddress("0x1")

	// Seed so SubBalance has funds.
	final.AddBalance(a, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
	pdb.BalanceOps = []BalanceOp{
		{Addr: a, Amount: *uint256.NewInt(20), IsAdd: true},
		{Addr: a, Amount: *uint256.NewInt(5), IsAdd: false},
	}
	pdb.AddLog(&types.Log{Address: a})

	pdb.settleBalanceOpsAndLogs(final)

	if got := final.GetBalance(a).Uint64(); got != 115 {
		t.Fatalf("balance after ops: got %d, want 115", got)
	}
}

// ---------------------------------------------------------------------------
// valuesEqual — cover the []byte branch and default branch
// ---------------------------------------------------------------------------

// TestValuesEqual_Bytes covers the []byte byte-wise compare path.
func TestValuesEqual_Bytes(t *testing.T) {
	if !valuesEqual([]byte{1, 2, 3}, []byte{1, 2, 3}) {
		t.Fatal("equal byte slices must compare equal")
	}
	if valuesEqual([]byte{1, 2, 3}, []byte{1, 2}) {
		t.Fatal("different-length byte slices must not compare equal")
	}
	if valuesEqual([]byte{1, 2, 3}, []byte{1, 2, 4}) {
		t.Fatal("different-content byte slices must not compare equal")
	}
	// Mismatched type: b is not []byte.
	if valuesEqual([]byte{1}, uint64(1)) {
		t.Fatal("bytes vs uint64 must not compare equal")
	}
}

// TestValuesEqual_Default compares non-byte values via the typed switch.
func TestValuesEqual_Default(t *testing.T) {
	if !valuesEqual(uint64(5), uint64(5)) {
		t.Fatal("equal uint64 must compare equal")
	}
	if valuesEqual(uint64(5), uint64(6)) {
		t.Fatal("different uint64 must not compare equal")
	}
	h1 := common.HexToHash("0x1")
	if !valuesEqual(h1, common.HexToHash("0x1")) {
		t.Fatal("equal hashes must compare equal")
	}
}

// TestValuesEqual_Bool covers the bool case (suicide/create subpaths).
func TestValuesEqual_Bool(t *testing.T) {
	if !valuesEqual(true, true) {
		t.Fatal("equal bools must compare equal")
	}
	if valuesEqual(true, false) {
		t.Fatal("different bools must not compare equal")
	}
	if valuesEqual(true, uint64(1)) {
		t.Fatal("bool vs uint64 must not compare equal")
	}
}

// TestValuesEqual_Nil covers nil-interface absence-read comparisons.
func TestValuesEqual_Nil(t *testing.T) {
	if !valuesEqual(nil, nil) {
		t.Fatal("two nils must compare equal")
	}
	if valuesEqual(nil, uint64(0)) {
		t.Fatal("nil vs typed zero must not compare equal")
	}
	if valuesEqual(uint64(0), nil) {
		t.Fatal("typed zero vs nil must not compare equal")
	}
}

// TestValuesEqual_UnsupportedTypePanics ensures the default branch panics
// rather than silently calling interface{} == . A new MVStore value type
// that doesn't get an explicit case must surface loudly in CI.
func TestValuesEqual_UnsupportedTypePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for unsupported value type")
		}
	}()
	type unsupported struct{ x int }
	valuesEqual(unsupported{1}, unsupported{1})
}

// ---------------------------------------------------------------------------
// GetCodeHash branches
// ---------------------------------------------------------------------------

// TestPDB_GetCodeHash_LocalCode returns Keccak256 of freshly-set code.
func TestPDB_GetCodeHash_LocalCode(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0x1")
	code := []byte{0x60, 0x00, 0xfd}
	pdb.SetCode(addr, code, tracing.CodeChangeUnspecified)

	h := pdb.GetCodeHash(addr)
	if h == (common.Hash{}) || h == types.EmptyCodeHash {
		t.Fatalf("GetCodeHash: got %s, want non-empty", h.Hex())
	}
}

// TestPDB_GetCodeHash_FromMVStore reads prior-tx code via MVStore and
// computes its hash.
func TestPDB_GetCodeHash_FromMVStore(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	addr := common.HexToAddress("0x1")
	code := []byte{0x60, 0x01, 0x60, 0x01}
	codeKey := blockstm.NewSubpathKey(addr, CodePath)
	store.WriteInc(codeKey, 2, 0, code)

	h := pdb.GetCodeHash(addr)
	if h == (common.Hash{}) || h == types.EmptyCodeHash {
		t.Fatalf("GetCodeHash from MVStore: got %s", h.Hex())
	}
}

// TestPDB_GetCodeHash_MVStoreEmpty returns EmptyCodeHash when MVStore has
// zero-length code.
func TestPDB_GetCodeHash_MVStoreEmpty(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	addr := common.HexToAddress("0x1")
	codeKey := blockstm.NewSubpathKey(addr, CodePath)
	store.WriteInc(codeKey, 2, 0, []byte{})

	if h := pdb.GetCodeHash(addr); h != types.EmptyCodeHash {
		t.Fatalf("GetCodeHash MVStore empty: got %s, want EmptyCodeHash", h.Hex())
	}
}

// TestPDB_GetCodeHash_NonExistent returns zero hash for addrs that neither
// exist in base nor have MVStore code.
func TestPDB_GetCodeHash_NonExistent(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	if h := pdb.GetCodeHash(common.HexToAddress("0xdead")); h != (common.Hash{}) {
		t.Fatalf("GetCodeHash(non-existent): got %s, want zero", h.Hex())
	}
}

// ---------------------------------------------------------------------------
// handleEstimate
// ---------------------------------------------------------------------------

// TestHandleEstimate_FirstIncarnationReturnsFalse pins the `Incarnation == 0
// || WaitForFinal == nil` short-circuit: no spin-wait on first execution.
func TestHandleEstimate_FirstIncarnationReturnsFalse(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.Incarnation = 0
	k := blockstm.NewAddressKey(common.HexToAddress("0x1"))
	store.WriteInc(k, 2, 0, uint64(1))

	if pdb.handleEstimate(k, 2) {
		t.Fatal("handleEstimate(Incarnation=0): must return false (no spin)")
	}
}

// TestHandleEstimate_WaitsThenDeletes exercises the re-exec path:
// Incarnation>0, WaitForFinal set, entry still estimate after wait → Delete.
func TestHandleEstimate_WaitsThenDeletes(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.Incarnation = 1
	pdb.WaitForFinal = func(int) {}

	addr := common.HexToAddress("0x1")
	k := blockstm.NewAddressKey(addr)
	store.WriteInc(k, 2, 0, uint64(1))
	store.MarkEstimate(2, []blockstm.Key{k})

	if !pdb.handleEstimate(k, 2) {
		t.Fatal("handleEstimate(Incarnation=1, est): expected true (retry loop)")
	}
	// Entry must have been deleted since it was still estimate after wait.
	if _, _, _, found, _ := store.ReadVersionFull(k, 10); found {
		t.Fatal("handleEstimate did not delete the lingering estimate entry")
	}
}

// ---------------------------------------------------------------------------
// emitTransferLog — pair and self-transfer paths
// ---------------------------------------------------------------------------

// TestPDB_EmitTransferLog_Pair invokes TransferLogFn with computed pre/post
// balances for sender+recipient.
func TestPDB_EmitTransferLog_Pair(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	amt := uint256.NewInt(5)

	// Seed final as if the transfer had just been applied.
	final.AddBalance(sender, uint256.NewInt(95), tracing.BalanceChangeUnspecified)
	final.AddBalance(recipient, uint256.NewInt(5), tracing.BalanceChangeUnspecified)

	var called int
	pdb.TransferLogFn = func(_ *StateDB, s, r common.Address, _, _, _, _, _ *big.Int) {
		called++
		if s != sender || r != recipient {
			t.Fatalf("wrong addrs: got s=%x r=%x", s, r)
		}
	}
	tr := &TransferRecord{Sender: sender, Recipient: recipient, Amount: *amt}
	pdb.emitTransferLog(final, tr, amt)
	if called != 1 {
		t.Fatalf("TransferLogFn called %d times, want 1", called)
	}
}

// TestPDB_EmitTransferLog_SelfTransfer takes the sender==recipient branch.
func TestPDB_EmitTransferLog_SelfTransfer(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	addr := common.HexToAddress("0x1")
	amt := uint256.NewInt(5)
	final.AddBalance(addr, uint256.NewInt(100), tracing.BalanceChangeUnspecified)

	var called bool
	pdb.TransferLogFn = func(_ *StateDB, s, r common.Address, _, in1, in2, o1, o2 *big.Int) {
		called = true
		if s != r {
			t.Fatal("self-transfer: sender != recipient")
		}
		if in1.Cmp(in2) != 0 || o1.Cmp(o2) != 0 || in1.Cmp(o1) != 0 {
			t.Fatalf("self-transfer: pre/post balances must all match: %v %v %v %v", in1, in2, o1, o2)
		}
	}
	tr := &TransferRecord{Sender: addr, Recipient: addr, Amount: *amt}
	pdb.emitTransferLog(final, tr, amt)
	if !called {
		t.Fatal("TransferLogFn not invoked for self-transfer")
	}
}

// ---------------------------------------------------------------------------
// Tier-1 mutation kill tests — targeted at survivors flagged by diffguard.
// Each pins a specific boundary / branch / boolean / return-value mutation
// that prior tests did not kill.
// ---------------------------------------------------------------------------

// TestPDB_EnableReadTracking_InitializesBalAddrs pins the `s.BalAddrs == nil`
// guard at parallel_statedb.go:335 (EnableReadTracking). Flipping == to !=
// would skip the make() on a fresh PDB; recordBalWrite would still work via
// nil-append, but cap(BalAddrs) would stay 0 instead of the documented 8 —
// inviting reallocation churn on every per-tx write. Lock the pre-allocated
// capacity in here.
func TestPDB_EnableReadTracking_InitializesBalAddrs(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	if pdb.BalAddrs != nil {
		t.Fatalf("precondition: fresh PDB should have nil BalAddrs, got len=%d cap=%d",
			len(pdb.BalAddrs), cap(pdb.BalAddrs))
	}

	pdb.EnableReadTracking()

	if pdb.BalAddrs == nil {
		t.Fatal("EnableReadTracking did not initialize BalAddrs (still nil after call)")
	}
	if cap(pdb.BalAddrs) < 8 {
		t.Fatalf("EnableReadTracking allocated BalAddrs with cap=%d, want >=8 (the documented hint)",
			cap(pdb.BalAddrs))
	}
}

// TestPDB_PriorDestructedAt_RecordsAbsenceRead pins the else-if branch at
// parallel_statedb.go:531 (priorDestructedAt). Removing the body drops
// `s.recordStoreRead(suicideKey, -1, 0, nil)` — the absence read that lets
// validation catch a later writer for SuicidePath_addr. Without it, a tx
// that observed "addr is not destructed" can pass validation even if a
// concurrent prior tx subsequently destructs addr.
func TestPDB_PriorDestructedAt_RecordsAbsenceRead(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xfeed")

	// No SuicidePath entry in MVStore yet; the lookup misses.
	if got := pdb.priorDestructedAt(addr); got != -1 {
		t.Fatalf("priorDestructedAt with no MVStore entry: got %d, want -1", got)
	}

	// The miss must be recorded as a base-read (WriterIdx=-1, StoreVal=nil)
	// in StoreReads — that's the validation hook.
	suicideKey := blockstm.NewSubpathKey(addr, SuicidePath)
	found := false
	for _, rd := range pdb.StoreReads {
		if rd.Key == suicideKey && rd.WriterIdx == -1 && rd.StoreVal == nil {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("priorDestructedAt must record an absence read for SuicidePath; without it, " +
			"validation cannot detect a concurrent prior tx destructing addr")
	}

	// Sanity: now write a SuicidePath entry from tx 2 and assert validation fails.
	store.WriteInc(suicideKey, 2, 0, true)
	r := pdb.ValidateDetailed()
	if r.Valid {
		t.Fatal("validation must fail: recorded 'not destructed' but tx 2 destructed addr")
	}
}

// TestPDB_Exist_DestructedInBaseReturnsFalse pins the `if suicideIdx >= 0`
// branch at parallel_statedb.go:576. Removing the body lets a destructed
// addr fall through to `s.base.Exist(addr)` and incorrectly return true
// when the account exists in base state. We need addr to ALSO exist in
// base so the fallthrough path is observable — without that, Exist
// returns false on the fallthrough too (zero balance, no base account)
// and the mutation looks behaviourally equivalent.
func TestPDB_Exist_DestructedInBaseReturnsFalse(t *testing.T) {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, err := New(types.EmptyRootHash, NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	addr := common.HexToAddress("0xdead")
	// Seed the base StateDB so the account DOES exist there.
	sdb.SetCode(addr, []byte{0x01, 0x02}, tracing.CodeChangeUnspecified)

	base := NewSafeBase(sdb, 2)
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	pdb := NewParallelStateDB(5, base, store, bals)
	pdb.Incarnation = 0

	// Simulate a prior tx (tx=2) destructing addr — write to MVStore directly.
	suicideKey := blockstm.NewSubpathKey(addr, SuicidePath)
	store.WriteInc(suicideKey, 2, 0, true)

	// With the destruct branch in place, Exist returns false. With the branch
	// removed, Exist falls through and base.Exist(addr) returns true (since
	// we seeded the base above), incorrectly making Exist return true.
	if pdb.Exist(addr) {
		t.Fatal("Exist must return false for a prior-destructed addr even when " +
			"the account exists in base state — the destruct should win")
	}
}

// TestPDB_CreateAccount_WritesTrueValue pins the literal `true` at
// parallel_statedb.go:1014 (CreateAccount → store.WriteInc). Flipping it
// to false would have CreateAccount publish (CreatePath_addr, txIdx, inc,
// false) — readers would then see the create-marker as false instead of
// true, defeating the value-based fallback in storeReadMatches.
func TestPDB_CreateAccount_WritesTrueValue(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 3)
	addr := common.HexToAddress("0xc0de")
	pdb.CreateAccount(addr)

	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	val, _, _, found, _ := store.ReadVersionFull(createKey, 10)
	if !found {
		t.Fatal("CreateAccount did not write CreatePath to MVStore (DeferMVWrites=false)")
	}
	b, ok := val.(bool)
	if !ok {
		t.Fatalf("CreatePath MVStore value type: got %T, want bool", val)
	}
	if !b {
		t.Fatal("CreateAccount wrote CreatePath=false; must be true (account-exists marker)")
	}
}

// TestPDB_DiagnoseBalanceRead_MatchReturnsFalse pins the `false` literal at
// parallel_statedb_validate.go:215 (diagnoseBalanceRead). Flipping to true
// would have a MATCHING balance read produce a phantom diagnostic with
// zero-valued fields — DiagnoseValidation aggregates these and downstream
// vfail-attribution would see a flood of empty "balance" diagnostics on
// every successfully-validated block.
func TestPDB_DiagnoseBalanceRead_MatchReturnsFalse(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xbeef")

	// Read the balance: with no MVBalanceStore entries, the cumulative delta
	// is zero on both the recorded read and the live re-read. The diagnose
	// path must report ok=false (no diag) — and must NOT append a phantom
	// zero-valued ValidationDiag to the result.
	pdb.GetBalance(addr)

	diags := pdb.DiagnoseValidation()
	if len(diags) != 0 {
		t.Fatalf("matching balance read produced %d diagnostics, want 0; "+
			"flipping the false return to true would emit phantom zero-valued diags: %+v",
			len(diags), diags)
	}
}
