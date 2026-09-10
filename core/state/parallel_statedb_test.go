package state

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// newTestPDB creates a ParallelStateDB backed by an empty in-memory state.
func newTestPDB(t *testing.T, txIdx int) (*ParallelStateDB, *blockstm.MVStore, *blockstm.MVBalanceStore) {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, err := New(types.EmptyRootHash, NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	base := NewSafeBase(sdb, 2)
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	pdb := NewParallelStateDB(txIdx, base, store, bals)
	pdb.Incarnation = 0
	return pdb, store, bals
}

func uint256New(v uint64) *uint256.Int {
	return uint256.NewInt(v)
}

func TestPDB_SetCode_MarksExist(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xdead")

	if pdb.Exist(addr) {
		t.Fatal("expected Exist=false before SetCode")
	}

	delegationCode := []byte{0xef, 0x01, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14}
	pdb.SetCode(addr, delegationCode, tracing.CodeChangeAuthorization)

	if !pdb.Exist(addr) {
		t.Fatal("expected Exist=true after SetCode")
	}
	if got := pdb.GetCode(addr); len(got) != 23 {
		t.Fatalf("expected 23 bytes, got %d", len(got))
	}
	if got := pdb.GetCodeSize(addr); got != 23 {
		t.Fatalf("expected CodeSize=23, got %d", got)
	}
	if got := pdb.GetCodeHash(addr); got == (common.Hash{}) || got == types.EmptyCodeHash {
		t.Fatal("expected non-empty code hash")
	}
}

func TestPDB_SetCode_ExistingAddress(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xbeef")

	pdb.CreateAccount(addr)
	code := []byte{0x60, 0x00, 0x60, 0x00, 0xfd}
	pdb.SetCode(addr, code, tracing.CodeChangeUnspecified)

	if !pdb.Exist(addr) {
		t.Fatal("expected Exist=true")
	}
	if got := pdb.GetCodeSize(addr); got != 5 {
		t.Fatalf("expected CodeSize=5, got %d", got)
	}
}

func TestPDB_SetCode_Revert(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xcafe")

	snap := pdb.Snapshot()
	pdb.SetCode(addr, []byte{0xef, 0x01, 0x00}, tracing.CodeChangeAuthorization)

	if !pdb.Exist(addr) {
		t.Fatal("expected Exist=true after SetCode")
	}
	pdb.RevertToSnapshot(snap)
	if pdb.Exist(addr) {
		t.Fatal("expected Exist=false after revert")
	}
	if len(pdb.GetCode(addr)) != 0 {
		t.Fatal("expected empty code after revert")
	}
}

func TestPDB_Transfer_Revert(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)

	snap := pdb.Snapshot()
	pdb.Transfers = append(pdb.Transfers, TransferRecord{
		Sender:    common.HexToAddress("0x1"),
		Recipient: common.HexToAddress("0x2"),
	})
	if len(pdb.Transfers) != 1 {
		t.Fatal("expected 1 transfer")
	}
	pdb.RevertToSnapshot(snap)
	if len(pdb.Transfers) != 0 {
		t.Fatalf("expected 0 transfers after revert, got %d", len(pdb.Transfers))
	}
}

func TestPDB_CommittedState_Cached(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	addr := common.HexToAddress("0xaaaa")
	slot := common.HexToHash("0x01")

	key := blockstm.NewStateKey(addr, slot)
	store.WriteInc(key, 2, 0, common.HexToHash("0x42"))
	pdb.EnableReadTracking()

	val1 := pdb.GetCommittedState(addr, slot)
	if val1 != common.HexToHash("0x42") {
		t.Fatalf("expected 0x42, got %s", val1.Hex())
	}

	// Simulate re-execution with different value
	store.WriteInc(key, 2, 1, common.HexToHash("0xff"))

	val2 := pdb.GetCommittedState(addr, slot)
	if val2 != val1 {
		t.Fatalf("GetCommittedState not stable: first=%s second=%s", val1.Hex(), val2.Hex())
	}
}

func TestPDB_Nonce_Validation(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	otherAddr := common.HexToAddress("0xbbbb")
	senderAddr := common.HexToAddress("0xcccc")

	pdb.SenderNonces = map[common.Address]uint64{senderAddr: 10}
	pdb.EnableReadTracking()

	nonceKey := blockstm.NewSubpathKey(otherAddr, NoncePath)
	store.WriteInc(nonceKey, 2, 0, uint64(5))

	nonce := pdb.GetNonce(otherAddr)
	if nonce != 5 {
		t.Fatalf("expected nonce=5, got %d", nonce)
	}

	store.WriteInc(nonceKey, 2, 1, uint64(6))

	result := pdb.ValidateDetailed()
	if result.Valid {
		t.Fatal("expected validation to fail for stale non-sender nonce")
	}
	if result.FailKey != "nonce" {
		t.Fatalf("expected FailKey='nonce', got '%s'", result.FailKey)
	}
}

func TestPDB_SenderNonce_SkipsValidation(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	senderAddr := common.HexToAddress("0xcccc")

	pdb.SenderNonces = map[common.Address]uint64{senderAddr: 10}
	pdb.EnableReadTracking()

	nonceKey := blockstm.NewSubpathKey(senderAddr, NoncePath)
	store.WriteInc(nonceKey, 2, 0, uint64(9))

	nonce := pdb.GetNonce(senderAddr)
	if nonce != 10 {
		t.Fatalf("expected nonce=10 from SenderNonces, got %d", nonce)
	}

	store.WriteInc(nonceKey, 2, 1, uint64(7))

	result := pdb.ValidateDetailed()
	if !result.Valid {
		t.Fatalf("expected validation to pass for sender nonce, got FailKey='%s'", result.FailKey)
	}
}

func TestPDB_Balance_Validation(t *testing.T) {
	pdb, _, bals := newTestPDB(t, 5)
	readOnlyAddr := common.HexToAddress("0xdddd")

	pdb.EnableReadTracking()

	bals.WriteDelta(readOnlyAddr, 2, uint256New(100), nil)

	bal := pdb.GetBalance(readOnlyAddr)
	_ = bal

	bals.WriteDelta(readOnlyAddr, 2, uint256New(200), nil)

	result := pdb.ValidateDetailed()
	if result.Valid {
		t.Fatal("expected validation to fail for stale read-only balance")
	}
	if result.FailKey != "balance" {
		t.Fatalf("expected FailKey='balance', got '%s'", result.FailKey)
	}
}

func TestPDB_Prepare_NoJournal(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	sender := common.HexToAddress("0x1234")
	dest := common.HexToAddress("0x5678")
	coinbase := common.HexToAddress("0x9abc")

	rules := params.Rules{IsEIP2929: true, IsShanghai: true}
	pdb.Prepare(rules, sender, coinbase, &dest, nil, nil)

	if !pdb.AddressInAccessList(sender) {
		t.Fatal("sender should be warm after Prepare")
	}

	snap := pdb.Snapshot()
	pdb.AddAddressToAccessList(common.HexToAddress("0xaaaa"))
	pdb.RevertToSnapshot(snap)

	if !pdb.AddressInAccessList(sender) {
		t.Fatal("sender should STILL be warm after revert")
	}
	if pdb.AddressInAccessList(common.HexToAddress("0xaaaa")) {
		t.Fatal("0xaaaa should NOT be warm after revert")
	}
}

func TestPDB_TransientStorage_Revert(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xdead")
	key := common.HexToHash("0x01")

	rules := params.Rules{IsEIP2929: true, IsShanghai: true}
	pdb.Prepare(rules, common.Address{}, common.Address{}, nil, nil, nil)

	if got := pdb.GetTransientState(addr, key); got != (common.Hash{}) {
		t.Fatalf("expected empty transient, got %s", got.Hex())
	}

	snap := pdb.Snapshot()
	pdb.SetTransientState(addr, key, common.HexToHash("0x01"))

	if got := pdb.GetTransientState(addr, key); got != common.HexToHash("0x01") {
		t.Fatalf("expected 0x01 after TSTORE, got %s", got.Hex())
	}

	pdb.RevertToSnapshot(snap)

	if got := pdb.GetTransientState(addr, key); got != (common.Hash{}) {
		t.Fatalf("expected empty transient after revert, got %s — TSTORE not journaled!", got.Hex())
	}
}

// --- Tests for ValidateDetailed helpers (validateStoreRead, storeReadMatches,
// validateBalanceRead, storeReadFailCategory, flushBalanceDeltas) ---

func TestPDB_ValidateDetailed_PanickedFails(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.Panicked = true
	r := pdb.ValidateDetailed()
	if r.Valid {
		t.Fatal("expected Valid=false when Panicked")
	}
	if r.FailKey != "panic" {
		t.Fatalf("FailKey=%q want %q", r.FailKey, "panic")
	}
}

func TestPDB_ValidateDetailed_FastPathFailsOnEstimate(t *testing.T) {
	// Verify the ESTIMATE-aware fast path: an entry with matching
	// (writerIdx, incarnation) but estimate=true must NOT pass validation.
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x01")
	val := common.HexToHash("0x42")
	stateKey := blockstm.NewStateKey(addr, slot)

	// Tx 2 wrote slot=val (incarnation 0), then was MarkEstimated.
	store.WriteInc(stateKey, 2, 0, val)
	pdb.GetState(addr, slot) // tx 5 reads it
	store.MarkEstimate(2, []blockstm.Key{stateKey})

	r := pdb.ValidateDetailed()
	if r.Valid {
		t.Fatal("expected Valid=false when read entry is ESTIMATE")
	}
	if r.FailKey != "storage" {
		t.Fatalf("FailKey=%q want %q", r.FailKey, "storage")
	}
}

func TestPDB_ValidateDetailed_ValueFallbackRejectsEstimate(t *testing.T) {
	// Even when the value matches the recorded read, an ESTIMATE entry
	// must not pass via the value-based fallback — re-execution may bump
	// incarnation and produce a different value.
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x02")
	val := common.HexToHash("0x77")
	stateKey := blockstm.NewStateKey(addr, slot)

	// Tx 2 (inc 0) wrote val. Tx 5 reads it (so rd.StoreVal = val).
	store.WriteInc(stateKey, 2, 0, val)
	if got := pdb.GetState(addr, slot); got != val {
		t.Fatalf("read got %s want %s", got.Hex(), val.Hex())
	}
	// MarkEstimate keeps the same value & version — value match, but ESTIMATE.
	store.MarkEstimate(2, []blockstm.Key{stateKey})

	r := pdb.ValidateDetailed()
	if r.Valid {
		t.Fatal("expected Valid=false: ESTIMATE values must fail value-based fallback")
	}
}

func TestPDB_ValidateDetailed_PassesWhenWriterMatches(t *testing.T) {
	// Sanity: same-version, committed entry passes.
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x03")
	val := common.HexToHash("0x99")
	stateKey := blockstm.NewStateKey(addr, slot)

	store.WriteInc(stateKey, 2, 0, val)
	pdb.GetState(addr, slot)

	r := pdb.ValidateDetailed()
	if !r.Valid {
		t.Fatalf("expected Valid=true, got FailKey=%q", r.FailKey)
	}
}

func TestPDB_ValidateDetailed_PassesWhenBaseUnchanged(t *testing.T) {
	// A read with no MVStore writer (WriterIdx=-1) passes when the store
	// still has no entry for that key.
	pdb, _, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x04")
	pdb.GetState(addr, slot) // base read, no MVStore entry

	r := pdb.ValidateDetailed()
	if !r.Valid {
		t.Fatalf("expected Valid=true for base-only read, got FailKey=%q", r.FailKey)
	}
}

func TestPDB_ValidateDetailed_PassesAfterValueRewrite(t *testing.T) {
	// Value-based fallback: the writer changed but the value is identical.
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x05")
	val := common.HexToHash("0x88")
	stateKey := blockstm.NewStateKey(addr, slot)

	// Tx 2 wrote val with inc 0. Tx 5 reads it.
	store.WriteInc(stateKey, 2, 0, val)
	pdb.GetState(addr, slot)
	// Tx 3 (later) writes the SAME value. Version differs, value matches.
	store.WriteInc(stateKey, 3, 0, val)

	r := pdb.ValidateDetailed()
	if !r.Valid {
		t.Fatalf("expected value-based fallback to pass, got FailKey=%q", r.FailKey)
	}
}

func TestPDB_ValidateDetailed_StorageMismatch(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x06")
	stateKey := blockstm.NewStateKey(addr, slot)

	store.WriteInc(stateKey, 2, 0, common.HexToHash("0x10"))
	pdb.GetState(addr, slot)
	// Different writer + different value → fail.
	store.WriteInc(stateKey, 3, 0, common.HexToHash("0x20"))

	r := pdb.ValidateDetailed()
	if r.Valid {
		t.Fatal("expected Valid=false on storage mismatch")
	}
	if r.FailKey != "storage" {
		t.Fatalf("FailKey=%q want %q", r.FailKey, "storage")
	}
}

func TestPDB_ValidateDetailed_BalanceMatch(t *testing.T) {
	pdb, _, bals := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xeeee")

	bals.WriteDelta(addr, 2, uint256New(100), nil)
	_ = pdb.GetBalance(addr) // captures rd.BalAdd=100

	r := pdb.ValidateDetailed()
	if !r.Valid {
		t.Fatalf("expected balance match, got FailKey=%q", r.FailKey)
	}
}

func TestPDB_ValidateDetailed_BalanceCoinbaseValidated(t *testing.T) {
	// Coinbase balance reads go through the same delta validation as any
	// other address. Fees are applied to the real StateDB during settlement,
	// not through MVBalanceStore, so a contract reading GetBalance(coinbase)
	// sees only body-originated deltas from prior txs — and those must match
	// on re-read for the speculative result to be accepted.
	pdb, _, bals := newTestPDB(t, 5)
	pdb.Coinbase = common.HexToAddress("0xc01b")
	pdb.EnableReadTracking()

	bals.WriteDelta(pdb.Coinbase, 2, uint256New(10), nil)
	_ = pdb.GetBalance(pdb.Coinbase)
	bals.WriteDelta(pdb.Coinbase, 2, uint256New(50), nil) // drift

	r := pdb.ValidateDetailed()
	if r.Valid {
		t.Fatalf("coinbase balance drift must fail validation")
	}
	if r.FailKey != "balance" {
		t.Fatalf("expected FailKey=balance, got %q", r.FailKey)
	}
}

func TestPDB_FlushToMVStore_AtomicAddSubPerAddress(t *testing.T) {
	// flushBalanceDeltas must combine add and sub into ONE WriteDelta call
	// per address — verify the post-flush entry has both components set
	// (not just one half).
	pdb, _, bals := newTestPDB(t, 7)
	addr := common.HexToAddress("0xabba")
	pdb.localBalAdd[addr] = uint256New(300)
	pdb.localBalSub[addr] = uint256New(100)
	pdb.recordBalWrite(addr)

	pdb.FlushToMVStore()

	add, sub, found := bals.GetTxDelta(addr, 7)
	if !found {
		t.Fatal("expected balance entry after flush")
	}
	if add.Uint64() != 300 {
		t.Fatalf("add=%d want 300", add.Uint64())
	}
	if sub.Uint64() != 100 {
		t.Fatalf("sub=%d want 100", sub.Uint64())
	}
}

func TestPDB_FlushToMVStore_SkipsZeroDeltas(t *testing.T) {
	// flushBalanceDeltas must NOT call WriteDelta for an address with zero
	// add and zero sub — preserves the empty entry condition.
	pdb, _, bals := newTestPDB(t, 7)
	addr := common.HexToAddress("0xface")
	pdb.localBalAdd[addr] = uint256.NewInt(0)
	pdb.localBalSub[addr] = uint256.NewInt(0)
	pdb.recordBalWrite(addr)

	pdb.FlushToMVStore()

	if _, _, found := bals.GetTxDelta(addr, 7); found {
		t.Fatal("expected NO balance entry for zero-delta address")
	}
}

func TestPDB_FlushToMVStore_AddOnlyAddress(t *testing.T) {
	// Address with only add (no sub entry at all) still flushes with
	// nil sub — the union of localBalAdd and localBalSub is iterated.
	pdb, _, bals := newTestPDB(t, 7)
	addr := common.HexToAddress("0xa11d")
	pdb.localBalAdd[addr] = uint256New(42)
	pdb.recordBalWrite(addr)

	pdb.FlushToMVStore()

	add, sub, found := bals.GetTxDelta(addr, 7)
	if !found {
		t.Fatal("expected entry for add-only address")
	}
	if add.Uint64() != 42 || !sub.IsZero() {
		t.Fatalf("add=%d sub=%d want 42, 0", add.Uint64(), sub.Uint64())
	}
}

func TestPDB_FlushToMVStore_SubOnlyAddress(t *testing.T) {
	pdb, _, bals := newTestPDB(t, 7)
	addr := common.HexToAddress("0x5b50")
	pdb.localBalSub[addr] = uint256New(7)
	pdb.recordBalWrite(addr)

	pdb.FlushToMVStore()

	add, sub, found := bals.GetTxDelta(addr, 7)
	if !found {
		t.Fatal("expected entry for sub-only address")
	}
	if !add.IsZero() || sub.Uint64() != 7 {
		t.Fatalf("add=%d sub=%d want 0, 7", add.Uint64(), sub.Uint64())
	}
}

// TestPDB_Journal_RevertNonce_DeletesWhenNotHad guards against mutation
// flipping the "had previous value" check. When SetNonce is the first
// write for an address, the journal entry has flags&1 == 0 and a revert
// must DELETE the local nonce, not store a zero value.
func TestPDB_Journal_RevertNonce_DeletesWhenNotHad(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xfeed")
	snap := pdb.Snapshot()
	pdb.SetNonce(addr, 7, tracing.NonceChangeUnspecified)
	if pdb.GetNonce(addr) != 7 {
		t.Fatalf("pre-revert nonce=%d want 7", pdb.GetNonce(addr))
	}
	pdb.RevertToSnapshot(snap)
	// revertNonce must have deleted the entry — not stored zero.
	if _, present := pdb.localNonces[addr]; present {
		t.Fatalf("revertNonce must delete when flags&1==0, but entry is still present")
	}
}

// TestPDB_Journal_RevertNonce_RestoresWhenHad covers the complementary
// branch: a second SetNonce (when a prior value existed) must be reverted
// by restoring the prior value, not deleting.
func TestPDB_Journal_RevertNonce_RestoresWhenHad(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xbeef")
	pdb.SetNonce(addr, 3, tracing.NonceChangeUnspecified)
	snap := pdb.Snapshot()
	pdb.SetNonce(addr, 9, tracing.NonceChangeUnspecified)
	if pdb.GetNonce(addr) != 9 {
		t.Fatalf("pre-revert nonce=%d want 9", pdb.GetNonce(addr))
	}
	pdb.RevertToSnapshot(snap)
	if got := pdb.GetNonce(addr); got != 3 {
		t.Fatalf("post-revert nonce=%d want 3 (prior value restored)", got)
	}
}

func TestPDB_StoreReadFailCategory(t *testing.T) {
	addr := common.HexToAddress("0xabcd")
	cases := []struct {
		key  blockstm.Key
		want string
	}{
		{blockstm.NewSubpathKey(addr, NoncePath), "nonce"},
		{blockstm.NewSubpathKey(addr, CodePath), "code"},
		{blockstm.NewSubpathKey(addr, CreatePath), "create"},
		{blockstm.NewStateKey(addr, common.HexToHash("0x01")), "storage"},
	}
	for _, c := range cases {
		got := storeReadFailCategory(c.key)
		if got != c.want {
			t.Errorf("key=%v: got %q want %q", c.key, got, c.want)
		}
	}
}

// ===========================================================================
// Tier-1 mutation kill tests
//
// Each test below pins a specific boundary / negation / return-value
// mutation flagged as SURVIVED by diffguard's mutation testing.
// ===========================================================================

// ---------------------------------------------------------------------------
// storeReadMatches — boundary tests
// ---------------------------------------------------------------------------

// TestStoreReadMatches_ExactVersionMatch locks the `writer == rd.WriterIdx
// && inc == rd.WriterInc` fast path: matching pair returns true; off-by-one
// in either writer or inc with a DIFFERENT current value returns false.
// Uses distinct curVal vs StoreVal to distinguish the version-match path
// from the value-match fallback.
func TestStoreReadMatches_ExactVersionMatch(t *testing.T) {
	rd := &StoreReadDesc{WriterIdx: 4, WriterInc: 2, StoreVal: uint64(99)}

	// Exact version match — returns true even though curVal != StoreVal.
	if !storeReadMatches(rd, uint64(123), 4, 2, true, false) {
		t.Fatal("exact (writer=4, inc=2) should match on version alone")
	}
	// Off-by-one writer with different curVal: neither version nor value path.
	if storeReadMatches(rd, uint64(123), 5, 2, true, false) {
		t.Fatal("writer off-by-one with curVal mismatch must fail")
	}
	// Off-by-one inc with different curVal.
	if storeReadMatches(rd, uint64(123), 4, 3, true, false) {
		t.Fatal("inc off-by-one with curVal mismatch must fail")
	}
}

// TestStoreReadMatches_ValueFallbackRequiresFound verifies the value-equal
// fallback: a value match is accepted only when found is true AND the
// recorded StoreVal is non-nil. The fallback must not shadow the !found
// base-read case.
func TestStoreReadMatches_ValueFallbackRequiresFound(t *testing.T) {
	rd := &StoreReadDesc{WriterIdx: 2, WriterInc: 0, StoreVal: uint64(7)}

	// Different writer but same value + found=true → accept.
	if !storeReadMatches(rd, uint64(7), 3, 0, true, false) {
		t.Fatal("value match with found=true must pass")
	}
	// Different writer, same value but found=false → reject (base-read path).
	if storeReadMatches(rd, uint64(7), -1, 0, false, false) {
		t.Fatal("value match with found=false and non-nil StoreVal must not pass")
	}
	// StoreVal nil + found=true → rejects value path.
	rdNil := &StoreReadDesc{WriterIdx: 2, WriterInc: 0, StoreVal: nil}
	if storeReadMatches(rdNil, uint64(7), 3, 0, true, false) {
		t.Fatal("nil StoreVal must not use value-match path")
	}
}

// TestStoreReadMatches_BaseReadMatchesNil locks the third branch: read came
// from base (writer==-1, StoreVal==nil) — must accept on !found.
func TestStoreReadMatches_BaseReadMatchesNil(t *testing.T) {
	rd := &StoreReadDesc{WriterIdx: -1, WriterInc: 0, StoreVal: nil}
	if !storeReadMatches(rd, nil, -1, 0, false, false) {
		t.Fatal("base read with nil StoreVal must match on !found")
	}
	// But if a writer now exists (found=true), the base read no longer matches.
	if storeReadMatches(rd, uint64(1), 2, 0, true, false) {
		t.Fatal("base read must not match when a writer exists")
	}
}

// TestStoreReadMatches_EstimateForcesNotFound pins the ESTIMATE
// path: only a simultaneous !found && nil StoreVal is acceptable. Flipping
// the found or StoreVal boundary produces mismatch.
func TestStoreReadMatches_EstimateForcesNotFound(t *testing.T) {
	rd := &StoreReadDesc{WriterIdx: 2, WriterInc: 0, StoreVal: nil}
	if !storeReadMatches(rd, nil, -1, 0, false, true) {
		t.Fatal("estimate + !found + nil StoreVal must match")
	}
	// found=true under estimate must fail.
	if storeReadMatches(rd, uint64(1), 2, 0, true, true) {
		t.Fatal("estimate + found=true must not match")
	}
	// Non-nil StoreVal under estimate must fail even when !found.
	rdNonNil := &StoreReadDesc{WriterIdx: 2, WriterInc: 0, StoreVal: uint64(9)}
	if storeReadMatches(rdNonNil, nil, -1, 0, false, true) {
		t.Fatal("estimate + non-nil StoreVal must not match")
	}
}

// ---------------------------------------------------------------------------
// Journal revert — state-outcome assertions per kind
// ---------------------------------------------------------------------------

// TestPDB_Journal_RevertStorage_DeletesWhenNotHad verifies revertStorage's
// `else` branch: when no prior value existed, revert must remove the slot
// from localStorage.
func TestPDB_Journal_RevertStorage_DeletesWhenNotHad(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x01")

	snap := pdb.Snapshot()
	pdb.SetState(addr, slot, common.HexToHash("0xdead"))
	if got := pdb.localStorage[addr][slot]; got != common.HexToHash("0xdead") {
		t.Fatalf("pre-revert localStorage: got %s, want 0xdead", got.Hex())
	}
	pdb.RevertToSnapshot(snap)

	// No prior — slot must be deleted from the per-address map.
	if _, ok := pdb.localStorage[addr][slot]; ok {
		t.Fatal("revertStorage must delete when flags&1 == 0")
	}
}

// TestPDB_Journal_RevertStorage_RestoresWhenHad covers the had-prev branch:
// two writes; revert to snapshot after the second restores the first.
func TestPDB_Journal_RevertStorage_RestoresWhenHad(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x01")

	pdb.SetState(addr, slot, common.HexToHash("0x11"))
	snap := pdb.Snapshot()
	pdb.SetState(addr, slot, common.HexToHash("0x22"))
	pdb.RevertToSnapshot(snap)

	if got := pdb.localStorage[addr][slot]; got != common.HexToHash("0x11") {
		t.Fatalf("revertStorage must restore prior value: got %s, want 0x11", got.Hex())
	}
}

// TestPDB_Journal_RevertBalance_Add reverts an AddBalance: localBalAdd
// must drop by the reverted amount.
func TestPDB_Journal_RevertBalance_Add(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")

	snap := pdb.Snapshot()
	pdb.AddBalance(addr, uint256.NewInt(50), tracing.BalanceChangeUnspecified)
	if pdb.localBalAdd[addr] == nil || pdb.localBalAdd[addr].Uint64() != 50 {
		t.Fatal("pre-revert localBalAdd not set")
	}
	pdb.RevertToSnapshot(snap)

	if pdb.localBalAdd[addr] != nil && pdb.localBalAdd[addr].Uint64() != 0 {
		t.Fatalf("revertBalance(add): got %d, want 0", pdb.localBalAdd[addr].Uint64())
	}
}

// TestPDB_Journal_RevertBalance_Sub reverts a SubBalance: localBalSub must
// drop. Separate from Add to pin the `flags&1 == 0` branch in revertBalance.
func TestPDB_Journal_RevertBalance_Sub(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")

	snap := pdb.Snapshot()
	pdb.SubBalance(addr, uint256.NewInt(20), tracing.BalanceChangeUnspecified)
	if pdb.localBalSub[addr] == nil || pdb.localBalSub[addr].Uint64() != 20 {
		t.Fatal("pre-revert localBalSub not set")
	}
	pdb.RevertToSnapshot(snap)

	if pdb.localBalSub[addr] != nil && pdb.localBalSub[addr].Uint64() != 0 {
		t.Fatalf("revertBalance(sub): got %d, want 0", pdb.localBalSub[addr].Uint64())
	}
}

// TestPDB_Journal_RevertCreate clears all three sets (created, newContract,
// localCode) — pins the `delete(...)` statements against statement_deletion.
func TestPDB_Journal_RevertCreate(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")

	snap := pdb.Snapshot()
	pdb.CreateContract(addr)
	pdb.SetCode(addr, []byte{0x01}, tracing.CodeChangeUnspecified)
	if !pdb.created[addr] || !pdb.newContract[addr] {
		t.Fatal("pre-revert: created/newContract not set")
	}
	pdb.RevertToSnapshot(snap)

	if pdb.created[addr] {
		t.Fatal("revertCreate must delete from created")
	}
	if pdb.newContract[addr] {
		t.Fatal("revertCreate must delete from newContract")
	}
	if _, ok := pdb.localCode[addr]; ok {
		t.Fatal("revertCreate must delete from localCode")
	}
}

// TestPDB_Journal_RevertAccessSlot removes an access-list slot on revert
// without panicking on the missing-address branch.
func TestPDB_Journal_RevertAccessSlot(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x11")

	pdb.AddAddressToAccessList(addr) // add first so AddSlotToAccessList adds the slot only
	snap := pdb.Snapshot()
	pdb.AddSlotToAccessList(addr, slot)
	if _, ok := pdb.accessList.Contains(addr, slot); !ok {
		t.Fatal("slot not in access list before revert")
	}
	pdb.RevertToSnapshot(snap)
	if _, ok := pdb.accessList.Contains(addr, slot); ok {
		t.Fatal("revertAccessSlot must remove the slot")
	}
}

// ---------------------------------------------------------------------------
// SettleTo sub-functions
// ---------------------------------------------------------------------------

// settleFinalDB returns a fresh in-memory StateDB that SettleTo can write
// to without affecting the base used by the PDB.
func settleFinalDB(t *testing.T) *StateDB {
	t.Helper()
	pdb, _, _ := newTestPDB(t, 0)
	return pdb.rawBase.Copy()
}

// TestPDB_SettleNonces writes the tx's nonce changes into the final
// StateDB.
func TestPDB_SettleNonces(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	addr := common.HexToAddress("0xabcd")

	pdb.SetNonce(addr, 7, tracing.NonceChangeUnspecified)

	pdb.settleNonces(final)

	if got := final.GetNonce(addr); got != 7 {
		t.Fatalf("nonce: got %d, want 7", got)
	}
}

// TestPDB_SettleStorage writes the tx's storage slot changes into the
// final StateDB.
func TestPDB_SettleStorage(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x01")

	pdb.SetState(addr, slot, common.HexToHash("0xdead"))

	pdb.settleStorage(final)

	if got := final.GetState(addr, slot); got != common.HexToHash("0xdead") {
		t.Fatalf("storage: got %s, want 0xdead", got.Hex())
	}
}

// TestPDB_SettleCode writes the tx's localCode entries into the final
// StateDB, computing each contract's code hash.
func TestPDB_SettleCode(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	addr := common.HexToAddress("0xabcd")
	code := []byte{0x60, 0x00, 0x60, 0x00, 0xfd} // PUSH1 0 PUSH1 0 REVERT

	pdb.localCode[addr] = code
	pdb.settleCode(final)

	got := final.GetCode(addr)
	if len(got) != len(code) {
		t.Fatalf("code len: got %d, want %d", len(got), len(code))
	}
	for i := range code {
		if got[i] != code[i] {
			t.Fatalf("code byte %d: got %x, want %x", i, got[i], code[i])
		}
	}
}

// TestPDB_TryEmitTransferAt_NoTransfersReturnsFalse pins the first guard:
// if transferIdx is at the end, the function must return false immediately.
func TestPDB_TryEmitTransferAt_NoTransfersReturnsFalse(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.BalanceOps = []BalanceOp{
		{Addr: common.HexToAddress("0x1"), Amount: *uint256.NewInt(5), IsAdd: false},
	}
	final := settleFinalDB(t)
	tIdx, lIdx := 0, 0
	if pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("tryEmitTransferAt must return false when transferIdx exhausted")
	}
}

// TestPDB_TryEmitTransferAt_SenderOpMustBeSub pins the IsAdd check on the
// sender op: a +5 sender op cannot match a Sub-based transfer.
func TestPDB_TryEmitTransferAt_SenderOpMustBeSub(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	amt := *uint256.NewInt(5)

	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: true}, // wrong — should be Sub
		{Addr: recipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt}}
	final := settleFinalDB(t)
	tIdx, lIdx := 0, 0
	if pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("sender op must be Sub (IsAdd=false) for the pair to match")
	}
}

// TestPDB_TryEmitTransferAt_PairMismatch fails when the second op's
// recipient address doesn't match the transfer record.
func TestPDB_TryEmitTransferAt_PairMismatch(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	wrongRecipient := common.HexToAddress("0x9")
	amt := *uint256.NewInt(5)

	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: false},
		{Addr: wrongRecipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt}}
	final := settleFinalDB(t)
	tIdx, lIdx := 0, 0
	if pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("pair mismatch must prevent emission")
	}
}

// TestPDB_TryEmitTransferAt_HappyPath emits the transfer and advances both
// counters — pins return-true, transferIdx++, and the SubBalance+AddBalance
// calls on final.
func TestPDB_TryEmitTransferAt_HappyPath(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	amt := *uint256.NewInt(5)

	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: false},
		{Addr: recipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt}}

	final := settleFinalDB(t)
	// Seed sender so SubBalance has funds.
	final.AddBalance(sender, uint256.NewInt(100), tracing.BalanceChangeUnspecified)

	tIdx, lIdx := 0, 0
	if !pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("happy-path must return true")
	}
	if tIdx != 1 {
		t.Fatalf("transferIdx: got %d, want 1", tIdx)
	}
	if got := final.GetBalance(sender).Uint64(); got != 95 {
		t.Fatalf("sender balance: got %d, want 95", got)
	}
	if got := final.GetBalance(recipient).Uint64(); got != 5 {
		t.Fatalf("recipient balance: got %d, want 5", got)
	}
}

// TestPDB_EmitTransferLog_ZeroAmount pins the early-return guard at line 1373:
// zero amount must skip the TransferLogFn call.
func TestPDB_EmitTransferLog_ZeroAmount(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	sender := common.HexToAddress("0x1")
	called := false
	pdb.TransferLogFn = func(_ *StateDB, _, _ common.Address, _, _, _, _, _ *big.Int) {
		called = true
	}
	final := settleFinalDB(t)
	zero := uint256.NewInt(0)
	tr := &TransferRecord{Sender: sender, Recipient: common.HexToAddress("0x2")}

	pdb.emitTransferLog(final, tr, zero)
	if called {
		t.Fatal("zero-amount transfer must not invoke TransferLogFn")
	}
}

// TestPDB_EmitTransferLog_NilFn pins the other half of the early-return:
// nil TransferLogFn must be tolerated without panic.
func TestPDB_EmitTransferLog_NilFn(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.TransferLogFn = nil
	final := settleFinalDB(t)
	tr := &TransferRecord{Sender: common.HexToAddress("0x1"), Recipient: common.HexToAddress("0x2")}
	pdb.emitTransferLog(final, tr, uint256.NewInt(5))
	// Success: no panic.
}

// TestPDB_ApplyFeeData_Nil is the nil-FeeData early return. Without FeeData,
// applyFeeData must not touch final balances.
func TestPDB_ApplyFeeData_Nil(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	pdb.FeeData = nil
	pdb.applyFeeData(final, uint256.NewInt(0))
	// No panic = pass.
}

// TestPDB_ApplyFeeData_BurnAndTip covers the happy path: non-zero burn and
// tip are added to their target addresses.
func TestPDB_ApplyFeeData_BurnAndTip(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	coinbase := common.HexToAddress("0xc0")
	burner := common.HexToAddress("0xb1")
	pdb.Coinbase = coinbase
	pdb.FeeData = &FeeData{
		FeeBurnt:             big.NewInt(7),
		FeeTipped:            big.NewInt(3),
		BurntContractAddress: burner,
		SenderInitBalance:    big.NewInt(100),
	}

	pdb.applyFeeData(final, uint256.NewInt(50))

	if got := final.GetBalance(burner).Uint64(); got != 7 {
		t.Fatalf("burn balance: got %d, want 7", got)
	}
	if got := final.GetBalance(coinbase).Uint64(); got != 3 {
		t.Fatalf("coinbase tip: got %d, want 3", got)
	}
}

// TestPDB_ApplyFeeData_BalancesAppliedSkipsReapply pins the
// `!FeeData.BalancesApplied` guard: when set true, no balance changes happen.
func TestPDB_ApplyFeeData_BalancesAppliedSkipsReapply(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	coinbase := common.HexToAddress("0xc0")
	burner := common.HexToAddress("0xb1")
	pdb.Coinbase = coinbase
	pdb.FeeData = &FeeData{
		FeeBurnt:             big.NewInt(7),
		FeeTipped:            big.NewInt(3),
		BurntContractAddress: burner,
		SenderInitBalance:    big.NewInt(100),
		BalancesApplied:      true,
	}

	pdb.applyFeeData(final, uint256.NewInt(50))

	if got := final.GetBalance(burner).Uint64(); got != 0 {
		t.Fatalf("burn must not be reapplied: got %d", got)
	}
	if got := final.GetBalance(coinbase).Uint64(); got != 0 {
		t.Fatalf("coinbase must not be reapplied: got %d", got)
	}
}

// TestPDB_SettleAccountSet_Created creates the account on final when not
// already present — pins the `if !final.Exist(addr) { final.CreateAccount }`.
func TestPDB_SettleAccountSet_Created(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	addr := common.HexToAddress("0xabcd")
	pdb.created[addr] = true

	pdb.settleAccountSet(final)
	if !final.Exist(addr) {
		t.Fatal("settleAccountSet must create the account on final")
	}
}

// TestPDB_SettleAccountSet_Preimages writes recorded preimages to final.
func TestPDB_SettleAccountSet_Preimages(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	h := common.HexToHash("0xdead")
	pdb.preimages[h] = []byte{0x01, 0x02, 0x03}

	pdb.settleAccountSet(final)
	if got := final.Preimages()[h]; len(got) != 3 || got[0] != 0x01 {
		t.Fatalf("preimage not propagated: got %x", got)
	}
}

// ---------------------------------------------------------------------------
// Reset — per-field clearing
// ---------------------------------------------------------------------------

// TestPDB_Reset_ClearsLocalState mutates every per-tx tracked field and
// confirms Reset zeroes them. Blocks statement_deletion mutations on the
// many `clear(...)` / `= s.X[:0]` calls.
func TestPDB_Reset_ClearsLocalState(t *testing.T) {
	pdb, store, bals := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")

	pdb.localNonces[addr] = 5
	pdb.localStorage[addr] = map[common.Hash]common.Hash{{1}: {2}}
	pdb.localCode[addr] = []byte{0x01}
	pdb.localBalAdd[addr] = uint256.NewInt(10)
	pdb.localBalSub[addr] = uint256.NewInt(3)
	pdb.created[addr] = true
	pdb.destructed[addr] = true
	pdb.newContract[addr] = true
	pdb.preimages[common.Hash{0xaa}] = []byte{1}
	pdb.BalanceOps = append(pdb.BalanceOps, BalanceOp{Addr: addr})
	pdb.refund = 99
	pdb.logs = append(pdb.logs, &types.Log{})
	pdb.logSize = 1

	base := NewSafeBase(pdb.rawBase, 2)
	pdb.Reset(1, base, store, bals)

	if len(pdb.localNonces) != 0 || len(pdb.localStorage) != 0 || len(pdb.localCode) != 0 {
		t.Fatalf("Reset did not clear local maps")
	}
	if len(pdb.localBalAdd) != 0 || len(pdb.localBalSub) != 0 {
		t.Fatalf("Reset did not clear balance deltas")
	}
	if len(pdb.created) != 0 || len(pdb.destructed) != 0 || len(pdb.newContract) != 0 {
		t.Fatalf("Reset did not clear account sets")
	}
	if len(pdb.preimages) != 0 || len(pdb.BalanceOps) != 0 || len(pdb.logs) != 0 {
		t.Fatalf("Reset did not clear preimages/balanceOps/logs")
	}
	if pdb.refund != 0 || pdb.logSize != 0 {
		t.Fatalf("Reset did not zero refund/logSize: refund=%d logSize=%d", pdb.refund, pdb.logSize)
	}
	if pdb.TxIndex != 1 {
		t.Fatalf("Reset did not update TxIndex: got %d, want 1", pdb.TxIndex)
	}
}

// TestPDB_Reset_ClearsValidationTracking mutates trackReads + read/write
// sets, then Reset must zero them.
func TestPDB_Reset_ClearsValidationTracking(t *testing.T) {
	pdb, store, bals := newTestPDB(t, 0)
	pdb.trackReads = true
	pdb.StoreReads = append(pdb.StoreReads, StoreReadDesc{})
	pdb.BalReads = append(pdb.BalReads, BalReadDesc{})
	pdb.WriteKeys = append(pdb.WriteKeys, blockstm.Key{})
	pdb.BalAddrs = append(pdb.BalAddrs, common.Address{})
	pdb.balAddrSet = map[common.Address]bool{{0xaa}: true}

	base := NewSafeBase(pdb.rawBase, 2)
	pdb.Reset(1, base, store, bals)

	if pdb.trackReads {
		t.Fatal("Reset did not clear trackReads")
	}
	if len(pdb.StoreReads) != 0 || len(pdb.BalReads) != 0 {
		t.Fatal("Reset did not clear Reads slices")
	}
	if len(pdb.WriteKeys) != 0 || len(pdb.BalAddrs) != 0 {
		t.Fatal("Reset did not clear WriteKeys/BalAddrs")
	}
	if len(pdb.balAddrSet) != 0 {
		t.Fatal("Reset did not clear balAddrSet")
	}
}

// TestPDB_Reset_ClearsSettlementContext pins resets for settlement-only
// fields: FeeData, Coinbase, Sender, TransferLogFn, FeeLogFn, Panicked,
// ExecFailed, UsedGas.
func TestPDB_Reset_ClearsSettlementContext(t *testing.T) {
	pdb, store, bals := newTestPDB(t, 0)
	pdb.FeeData = &FeeData{FeeBurnt: big.NewInt(1)}
	pdb.Coinbase = common.HexToAddress("0xc0")
	pdb.Sender = common.HexToAddress("0x5e")
	pdb.TransferLogFn = func(*StateDB, common.Address, common.Address, *big.Int, *big.Int, *big.Int, *big.Int, *big.Int) {
	}
	pdb.FeeLogFn = pdb.TransferLogFn
	pdb.Panicked = true
	pdb.ExecFailed = true
	pdb.UsedGas = 123

	base := NewSafeBase(pdb.rawBase, 2)
	pdb.Reset(1, base, store, bals)

	if pdb.FeeData != nil {
		t.Fatal("Reset did not clear FeeData")
	}
	if pdb.Coinbase != (common.Address{}) || pdb.Sender != (common.Address{}) {
		t.Fatal("Reset did not clear Coinbase/Sender")
	}
	if pdb.TransferLogFn != nil || pdb.FeeLogFn != nil {
		t.Fatal("Reset did not nil log fns")
	}
	if pdb.Panicked || pdb.ExecFailed || pdb.UsedGas != 0 {
		t.Fatal("Reset did not clear Panicked/ExecFailed/UsedGas")
	}
}
func TestPDB_SkipNonceForSenderChain_NonNonceKey(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	addr := common.HexToAddress("0xabcd")
	pdb.SenderNonces = map[common.Address]uint64{addr: 10}
	pdb.EnableReadTracking()

	// Record a state read (not a nonce key).
	stateKey := blockstm.NewStateKey(addr, common.HexToHash("0x1"))
	store.WriteInc(stateKey, 2, 0, common.HexToHash("0xaa"))
	pdb.GetState(addr, common.HexToHash("0x1"))

	// Invalidate that state read by bumping the writer's incarnation.
	store.WriteInc(stateKey, 2, 1, common.HexToHash("0xbb"))

	result := pdb.ValidateDetailed()
	if result.Valid {
		t.Fatal("state read must not be skipped by SenderNonces check")
	}
	if result.FailKey != "storage" {
		t.Fatalf("expected FailKey=storage, got %q", result.FailKey)
	}
}

// TestPDB_DiagnoseBalanceRead_PassPath pins line 676: when cumulative
// balance delta matches the recorded value, diagnoseBalanceRead must
// return ok=false (no diagnostic). Complements the earlier drift test.
func TestPDB_DiagnoseBalanceRead_PassPath(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0x1")

	// Record the balance read at cumulative delta = 0. Don't inject any
	// new writer — the stored read matches the current state.
	pdb.GetBalance(addr)

	diags := pdb.DiagnoseValidation()
	for _, d := range diags {
		if d.Category == "balance" {
			t.Fatalf("balance diag unexpected: %+v", d)
		}
	}
}

// TestPDB_SubRefund_BoundaryEqual pins the `>` boundary at line 1073: when
// gas equals refund, SubRefund must succeed and zero the counter.
func TestPDB_SubRefund_BoundaryEqual(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	pdb.AddRefund(50)
	// gas == refund: must succeed.
	pdb.SubRefund(50)
	if pdb.GetRefund() != 0 {
		t.Fatalf("SubRefund(gas==refund): got refund=%d, want 0", pdb.GetRefund())
	}
}

// TestPDB_Snapshot_IdsIncrement pins `nextRevisionId++` at line 1166: two
// snapshots must yield distinct IDs so RevertToSnapshot can target the
// specific revision.
func TestPDB_Snapshot_IdsIncrement(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0x1")

	s1 := pdb.Snapshot()
	pdb.AddRefund(10)
	s2 := pdb.Snapshot()
	pdb.AddRefund(20)

	if s1 == s2 {
		t.Fatal("consecutive snapshots must produce distinct IDs")
	}

	// Revert to s2 should only undo the second refund.
	pdb.RevertToSnapshot(s2)
	if got := pdb.GetRefund(); got != 10 {
		t.Fatalf("revert to s2: got %d, want 10", got)
	}
	// Revert to s1 should undo the first too.
	pdb.RevertToSnapshot(s1)
	if got := pdb.GetRefund(); got != 0 {
		t.Fatalf("revert to s1: got %d, want 0", got)
	}
	_ = addr
}

// TestPDB_SetCode_ReturnsPrevCode pins `return prev` at line 993: SetCode
// must return the previous code so the EVM can record it in receipts.
func TestPDB_SetCode_ReturnsPrevCode(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	first := []byte{0x60, 0x00}
	second := []byte{0xfe}

	if got := pdb.SetCode(addr, first, tracing.CodeChangeUnspecified); len(got) != 0 {
		t.Fatalf("SetCode initial prev: got %x, want empty", got)
	}
	prev := pdb.SetCode(addr, second, tracing.CodeChangeUnspecified)
	if len(prev) != len(first) || prev[0] != 0x60 {
		t.Fatalf("SetCode prev: got %x, want %x", prev, first)
	}
}

// TestPDB_Journal_RevertCode_HadPrev pins the `hadPrev := j.flags&1 != 0`
// branch in revertCode: a SetCode over existing code must, on revert,
// restore the earlier code (not delete it).
func TestPDB_Journal_RevertCode_HadPrev(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	first := []byte{0x60, 0x01}

	// Establish existing code.
	pdb.SetCode(addr, first, tracing.CodeChangeUnspecified)

	snap := pdb.Snapshot()
	pdb.SetCode(addr, []byte{0xfd}, tracing.CodeChangeUnspecified)
	pdb.RevertToSnapshot(snap)

	got := pdb.GetCode(addr)
	if len(got) != len(first) || got[0] != 0x60 || got[1] != 0x01 {
		t.Fatalf("revertCode(hadPrev): got %x, want %x", got, first)
	}
}

// TestPDB_Journal_RevertCode_NoPrev pins the `else` branch of revertCode:
// a first-time SetCode must, on revert, delete the code entirely.
func TestPDB_Journal_RevertCode_NoPrev(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")

	snap := pdb.Snapshot()
	pdb.SetCode(addr, []byte{0x60, 0x01}, tracing.CodeChangeUnspecified)
	pdb.RevertToSnapshot(snap)

	if got := pdb.GetCode(addr); len(got) != 0 {
		t.Fatalf("revertCode(!hadPrev): got %x, want empty", got)
	}
}

// TestPDB_FlushToMVStore_CreatesAccount pins the write at line 713 (the
// create-key WriteInc inside FlushToMVStore).
func TestPDB_FlushToMVStore_CreatesAccount(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 3)
	pdb.EnableReadTracking()
	pdb.SetDeferMVWrites(true)
	addr := common.HexToAddress("0xabcd")
	pdb.CreateAccount(addr)
	pdb.FlushToMVStore()

	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	if _, _, _, found, _ := store.ReadVersionFull(createKey, 10); !found {
		t.Fatal("FlushToMVStore did not write the create key")
	}
}

// TestPDB_CreateAccount_WritesMVStore pins line 1152: CreateAccount (when
// DeferMVWrites is false) must write the create marker to MVStore.
func TestPDB_CreateAccount_WritesMVStore(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 3)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xabcd")
	// DeferMVWrites is false by default — CreateAccount should write.
	pdb.CreateAccount(addr)

	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	if _, _, _, found, _ := store.ReadVersionFull(createKey, 10); !found {
		t.Fatal("CreateAccount did not write to MVStore (DeferMVWrites=false)")
	}
}

// TestPDB_Exist_RecordsBaseRead pins line 813: when an address is absent
// from MVStore and base, Exist must record a "not exists" read (StoreVal=
// false) so that validation catches concurrent prior-tx creation.
func TestPDB_Exist_RecordsBaseRead(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0xdead")

	if pdb.Exist(addr) {
		t.Fatal("Exist on unseen addr must return false")
	}

	// Simulate a prior tx now creating the account.
	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	store.WriteInc(createKey, 2, 0, true)

	// Validate must fail — our stored read (StoreVal=false) disagrees with
	// the now-committed true value from tx 2.
	result := pdb.ValidateDetailed()
	if result.Valid {
		t.Fatal("validation must fail: recorded 'not exists' but tx 2 created")
	}
}

// TestPDB_TryEmitTransferAt_EmitsIntermediateLogs pins the log-loop at
// lines 1358-1360: logs accumulated before tr.LogIdx must be flushed to
// final BEFORE the transfer log.
func TestPDB_TryEmitTransferAt_EmitsIntermediateLogs(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	amt := *uint256.NewInt(5)

	// Seed final for SubBalance on sender.
	final.AddBalance(sender, uint256.NewInt(100), tracing.BalanceChangeUnspecified)

	// Two logs emitted before the transfer.
	pdb.logs = []*types.Log{
		{Address: common.HexToAddress("0xaa")},
		{Address: common.HexToAddress("0xbb")},
	}
	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: false},
		{Addr: recipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt, LogIdx: 2}}

	tIdx, lIdx := 0, 0
	if !pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("tryEmitTransferAt happy-path must return true")
	}
	// logIdx must have advanced past both intermediate logs.
	if lIdx != 2 {
		t.Fatalf("logIdx after emit: got %d, want 2 (both intermediate logs emitted)", lIdx)
	}
}

// TestPDB_TryEmitTransferAt_NoIntermediateLogs covers the other boundary
// of the log loop: with LogIdx == 0, zero intermediate logs are emitted.
func TestPDB_TryEmitTransferAt_NoIntermediateLogs(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	amt := *uint256.NewInt(5)

	final.AddBalance(sender, uint256.NewInt(100), tracing.BalanceChangeUnspecified)

	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: false},
		{Addr: recipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt, LogIdx: 0}}

	tIdx, lIdx := 0, 0
	if !pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("tryEmitTransferAt must succeed")
	}
	if lIdx != 0 {
		t.Fatalf("logIdx with LogIdx=0: got %d, want 0 (no intermediate emission)", lIdx)
	}
}

// ---------------------------------------------------------------------------
// Exist — all three return paths
// ---------------------------------------------------------------------------

// TestPDB_Exist_DestructedNoBaseReturnsFalse exercises the fall-through
// path for Exist after a same-tx SelfDestruct on an address that has no
// backing presence: not created in this tx, not in base, balance zeroed
// by the SelfDestruct itself, and no prior-tx destruct/create record in
// the MVStore. Exist must return false because the address truly does
// not exist anywhere — NOT because s.destructed[addr] short-circuits the
// read. EVM SELFDESTRUCT defers actual deletion to tx finalization, so a
// monotonic "destructed → false" check would diverge from serial within
// the current tx (see TestPDB_Exist_BaseAddrSelfDestructedInTxReturnsTrue
// for the case that pins the corrected within-tx semantics).
func TestPDB_Exist_DestructedNoBaseReturnsFalse(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	addr := common.HexToAddress("0xabcd")
	pdb.AddBalance(addr, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
	// Mirror the EVM handler: it clears the balance, SelfDestruct does not.
	pdb.SubBalance(addr, uint256.NewInt(1), tracing.BalanceDecreaseSelfdestruct)
	pdb.SelfDestruct(addr)
	if pdb.Exist(addr) {
		t.Fatal("Exist on a destructed address with no base/created/balance presence must return false")
	}
}

// TestPDB_Exist_BaseAddrSelfDestructedInTxReturnsTrue pins the within-tx
// SELFDESTRUCT visibility invariant: a contract that exists in the base
// state and is self-destructed by the current tx must still report as
// existing, matching serial StateDB.Exist (statedb.go:705-709, whose
// docstring is explicit: "returns true for self-destructed accounts
// within the current transaction"). The destruction tombstone is
// published cross-tx via the SuicidePath write at FlushToMVStore, where
// later txs in the same block see it through priorDestructedAt — that
// path is independent of this same-tx assertion. A premature false here
// causes a parent that re-calls a just-self-destructed callee in the
// same tx to see no account, which diverges from the EVM and breaks
// gas accounting (CALL value-transfer surcharge via Empty).
func TestPDB_Exist_BaseAddrSelfDestructedInTxReturnsTrue(t *testing.T) {
	sdb, _ := newDiffStateDB(t)
	addr := common.HexToAddress("0xabcd")
	// Pre-existing contract in base: code + balance + nonce make this a
	// realistic self-destruct target rather than a phantom address.
	sdb.SetCode(addr, []byte{0x60, 0x40}, tracing.CodeChangeUnspecified)
	sdb.AddBalance(addr, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
	sdb.SetNonce(addr, 1, tracing.NonceChangeUnspecified)

	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	pdb := NewParallelStateDB(0, NewSafeBase(sdb, 2), store, bals)

	pdb.SelfDestruct(addr)
	if !pdb.Exist(addr) {
		t.Fatal("Exist on a base-resident address self-destructed in the current tx must return true (within-tx visibility)")
	}
}

// TestPDB_Exist_MVStoreCreateReturnsTrue pins lines 797-798: when the
// create-key is found in MVStore, Exist must return true AND record the
// read so later writes are caught by validation.
func TestPDB_Exist_MVStoreCreateReturnsTrue(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	addr := common.HexToAddress("0xabcd")
	pdb.EnableReadTracking()

	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	store.WriteInc(createKey, 2, 0, true)

	if !pdb.Exist(addr) {
		t.Fatal("Exist on MVStore-created addr must return true")
	}
	// Must have recorded the read.
	found := false
	for _, rd := range pdb.StoreReads {
		if rd.Key == createKey && rd.WriterIdx == 2 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Exist on MVStore-hit must record the store read")
	}
}

// ---------------------------------------------------------------------------
// GetCode — return value on MVStore hit
// ---------------------------------------------------------------------------

// TestPDB_GetCode_MVStoreReturnsValue pins line 931: GetCode from MVStore
// must return the exact stored bytes, not nil.
func TestPDB_GetCode_MVStoreReturnsValue(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	addr := common.HexToAddress("0xabcd")
	code := []byte{0x60, 0x01, 0x60, 0x02, 0x01}
	codeKey := blockstm.NewSubpathKey(addr, CodePath)
	store.WriteInc(codeKey, 2, 0, code)

	got := pdb.GetCode(addr)
	if len(got) != len(code) {
		t.Fatalf("GetCode length: got %d, want %d", len(got), len(code))
	}
	for i := range code {
		if got[i] != code[i] {
			t.Fatalf("GetCode[%d]: got %x, want %x", i, got[i], code[i])
		}
	}
}

// TestPDB_GetCode_BaseReturnsValue pins line 935: GetCode falling through
// to base must propagate the base's code bytes. Set the code on the base
// StateDB BEFORE wrapping it in a SafeBase (SafeBase's pool snapshots the
// state at construction).
func TestPDB_GetCode_BaseReturnsValue(t *testing.T) {
	sdb, _ := newDiffStateDB(t)
	addr := common.HexToAddress("0xabcd")
	sdb.SetCode(addr, []byte{0x60, 0x40}, tracing.CodeChangeUnspecified)

	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	pdb := NewParallelStateDB(0, NewSafeBase(sdb, 2), store, bals)

	got := pdb.GetCode(addr)
	if len(got) != 2 || got[0] != 0x60 || got[1] != 0x40 {
		t.Fatalf("GetCode(base-hit): got %x, want 6040", got)
	}
}

// ---------------------------------------------------------------------------
// storeReadMatches — base-read branch must pass
// ---------------------------------------------------------------------------

// TestStoreReadMatches_NotFoundNilStoreValPasses pins line 578: when a
// read was recorded from base (writer=-1, StoreVal=nil) and MVStore still
// has no entry, the match returns true. Flipping the return to `false`
// would cause every base-only tx to fail validation.
func TestStoreReadMatches_NotFoundNilStoreValPasses(t *testing.T) {
	rd := &StoreReadDesc{WriterIdx: -1, WriterInc: 0, StoreVal: nil}
	if !storeReadMatches(rd, nil, -1, 0, false, false) {
		t.Fatal("!found + nil StoreVal must return true — base-only validation")
	}
}

// ---------------------------------------------------------------------------
// diagnoseStoreRead / diagnoseBalanceRead — return tuple values
// ---------------------------------------------------------------------------

// TestPDB_DiagnoseStoreRead_MatchReturnsNoDiag pins line 652-653: when
// the stored read still matches MVStore, diagnoseStoreRead returns
// (ValidationDiag{}, false).
func TestPDB_DiagnoseStoreRead_MatchReturnsNoDiag(t *testing.T) {
	pdb, store, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0x1")
	key := blockstm.NewStateKey(addr, common.HexToHash("0x1"))
	store.WriteInc(key, 2, 0, common.HexToHash("0xaa"))
	pdb.GetState(addr, common.HexToHash("0x1"))
	// No later writer → read still matches.

	diags := pdb.DiagnoseValidation()
	for _, d := range diags {
		if d.Category == "storage" && d.Addr == addr {
			t.Fatalf("expected no storage diag on matching read, got %+v", d)
		}
	}
}

// TestPDB_DiagnoseBalanceRead_MatchReturnsNoDiag pins line 676: matching
// cumulative delta returns (ValidationDiag{}, false) from the balance
// diagnostic.
func TestPDB_DiagnoseBalanceRead_MatchReturnsNoDiagExplicit(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 5)
	pdb.EnableReadTracking()
	addr := common.HexToAddress("0x1")

	// Record a read; no writers ever write → delta stays matching.
	pdb.GetBalance(addr)

	diags := pdb.DiagnoseValidation()
	for _, d := range diags {
		if d.Category == "balance" {
			t.Fatalf("expected no balance diag when cumulative matches, got %+v", d)
		}
	}
}

// ---------------------------------------------------------------------------
// tryEmitTransferAt boundary at opIdx+1 >= len check
// ---------------------------------------------------------------------------

// TestPDB_TryEmitTransferAt_LastOpNoPair pins line 1357: when the sender
// op is at the last index in BalanceOps, there's no pair to form → must
// return false.
func TestPDB_TryEmitTransferAt_LastOpNoPair(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	sender := common.HexToAddress("0x1")
	amt := *uint256.NewInt(5)

	pdb.BalanceOps = []BalanceOp{
		{Addr: sender, Amount: amt, IsAdd: false},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: common.HexToAddress("0x2"), Amount: amt}}

	final := settleFinalDB(t)
	tIdx, lIdx := 0, 0
	if pdb.tryEmitTransferAt(final, 0, &tIdx, &lIdx) {
		t.Fatal("tryEmitTransferAt with no paired op must return false")
	}
}

// ---------------------------------------------------------------------------
// applyFeeData — FeeTipped Sign() > 0 boundary + FeeLogFn short-circuit
// ---------------------------------------------------------------------------

// TestPDB_ApplyFeeData_ZeroTipDoesNothing pins line 1423: when FeeTipped
// is zero, AddBalance on coinbase must NOT fire.
func TestPDB_ApplyFeeData_ZeroTipDoesNothing(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	coinbase := common.HexToAddress("0xc0")
	pdb.Coinbase = coinbase
	pdb.FeeData = &FeeData{
		FeeBurnt:          big.NewInt(7),
		FeeTipped:         big.NewInt(0),
		SenderInitBalance: big.NewInt(100),
	}
	pdb.applyFeeData(final, uint256.NewInt(50))

	if got := final.GetBalance(coinbase).Uint64(); got != 0 {
		t.Fatalf("coinbase got tip for zero FeeTipped: balance=%d", got)
	}
}

// TestPDB_ApplyFeeData_FeeLogFnSkipOnZeroTip pins line 1430: FeeLogFn
// must not be invoked when FeeTipped is zero, even if FeeLogFn is set.
func TestPDB_ApplyFeeData_FeeLogFnSkipOnZeroTip(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	pdb.Coinbase = common.HexToAddress("0xc0")
	pdb.FeeData = &FeeData{
		FeeTipped:         big.NewInt(0),
		SenderInitBalance: big.NewInt(100),
	}
	called := false
	pdb.FeeLogFn = func(*StateDB, common.Address, common.Address, *big.Int, *big.Int, *big.Int, *big.Int, *big.Int) {
		called = true
	}
	pdb.applyFeeData(final, uint256.NewInt(50))
	if called {
		t.Fatal("FeeLogFn invoked despite zero tip")
	}
}

// TestPDB_ApplyFeeData_FeeLogFnCalledWithPositiveTip covers the other side
// of the line-1430 guard: when tip is positive and FeeLogFn is set, the
// log is emitted with correct args.
func TestPDB_ApplyFeeData_FeeLogFnCalledWithPositiveTip(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)
	coinbase := common.HexToAddress("0xc0")
	sender := common.HexToAddress("0x5e")
	pdb.Coinbase = coinbase
	pdb.Sender = sender
	pdb.FeeData = &FeeData{
		FeeTipped:         big.NewInt(3),
		SenderInitBalance: big.NewInt(100),
	}
	called := 0
	var gotTip *big.Int
	pdb.FeeLogFn = func(_ *StateDB, s, r common.Address, amt, _, _, _, _ *big.Int) {
		called++
		gotTip = amt
	}
	pdb.applyFeeData(final, uint256.NewInt(50))
	if called != 1 {
		t.Fatalf("FeeLogFn calls: got %d, want 1", called)
	}
	if gotTip.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("tip arg: got %v, want 3", gotTip)
	}
}

// hasStoreRead reports whether sr contains an entry for k.
func hasStoreRead(sr []StoreReadDesc, k blockstm.Key) bool {
	for _, r := range sr {
		if r.Key == k {
			return true
		}
	}
	return false
}

// recordPriorDestruct simulates a prior tx D destroying addr by writing
// SuicidePath_addr to the shared MVStore at writer index destructorIdx.
func recordPriorDestruct(store *blockstm.MVStore, addr common.Address, destructorIdx int) {
	store.WriteInc(blockstm.NewSubpathKey(addr, SuicidePath), destructorIdx, 0, true)
}

// TestPDB_DestructedMiss_RecordsRead pins the cross-tx invalidation contract
// for the four PDB getters on the (prior-destructor + MVStore miss) path.
//
// Pre-fix: GetState, GetCommittedState, GetCode, GetNonce returned the zero
// value without recording the read; validation then could not invalidate
// the reader when a re-executed prior tx later wrote the key. That allowed
// V2 to settle a zero where serial saw the real value — a state-root
// divergence. The sibling GetCodeHash already records the miss on this
// path; these tests pin the same contract on the other four.
func TestPDB_DestructedMiss_RecordsRead(t *testing.T) {
	addr := common.HexToAddress("0xa")
	slot := common.HexToHash("0x1")
	const destructorIdx = 1
	const readerIdx = 5

	t.Run("GetState", func(t *testing.T) {
		pdb, store, _ := newTestPDB(t, readerIdx)
		pdb.EnableReadTracking()
		recordPriorDestruct(store, addr, destructorIdx)

		got := pdb.GetState(addr, slot)
		if got != (common.Hash{}) {
			t.Fatalf("GetState on destructed-miss: got %v, want zero", got)
		}
		key := blockstm.NewStateKey(addr, slot)
		if !hasStoreRead(pdb.StoreReads, key) {
			t.Fatalf("GetState did not record read for stateKey(%x, %x); a later writer cannot invalidate this tx",
				addr, slot)
		}
	})

	t.Run("GetCommittedState", func(t *testing.T) {
		pdb, store, _ := newTestPDB(t, readerIdx)
		pdb.EnableReadTracking()
		recordPriorDestruct(store, addr, destructorIdx)

		got := pdb.GetCommittedState(addr, slot)
		if got != (common.Hash{}) {
			t.Fatalf("GetCommittedState on destructed-miss: got %v, want zero", got)
		}
		key := blockstm.NewStateKey(addr, slot)
		if !hasStoreRead(pdb.StoreReads, key) {
			t.Fatalf("GetCommittedState did not record read for stateKey(%x, %x)",
				addr, slot)
		}
	})

	t.Run("GetCode", func(t *testing.T) {
		pdb, store, _ := newTestPDB(t, readerIdx)
		pdb.EnableReadTracking()
		recordPriorDestruct(store, addr, destructorIdx)

		got := pdb.GetCode(addr)
		if got != nil {
			t.Fatalf("GetCode on destructed-miss: got %v, want nil", got)
		}
		key := blockstm.NewSubpathKey(addr, CodePath)
		if !hasStoreRead(pdb.StoreReads, key) {
			t.Fatalf("GetCode did not record read for codeKey(%x)", addr)
		}
	})

	t.Run("GetNonce", func(t *testing.T) {
		pdb, store, _ := newTestPDB(t, readerIdx)
		pdb.EnableReadTracking()
		recordPriorDestruct(store, addr, destructorIdx)

		got := pdb.GetNonce(addr)
		if got != 0 {
			t.Fatalf("GetNonce on destructed-miss: got %d, want 0", got)
		}
		key := blockstm.NewSubpathKey(addr, NoncePath)
		if !hasStoreRead(pdb.StoreReads, key) {
			t.Fatalf("GetNonce did not record read for nonceKey(%x)", addr)
		}
	})
}

// TestPDB_GetCommittedState_NoDestructor_FallsThroughToBase pins the
// no-destructor branch of GetCommittedState: with suicideIdx < 0 and no
// MVStore writer for the slot, result must come from base state — kills
// the `remove if body` mutation on the base-read assignment.
func TestPDB_GetCommittedState_NoDestructor_FallsThroughToBase(t *testing.T) {
	addr := common.HexToAddress("0xa")
	slot := common.HexToHash("0x1")
	want := common.HexToHash("0xbeef")

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, err := New(types.EmptyRootHash, NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	sdb.SetState(addr, slot, want)
	base := NewSafeBase(sdb, 2)
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	pdb := NewParallelStateDB(5, base, store, bals)
	pdb.EnableReadTracking()

	got := pdb.GetCommittedState(addr, slot)
	if got != want {
		t.Fatalf("GetCommittedState with no destructor: got %s, want %s (must fall through to base)",
			got.Hex(), want.Hex())
	}
}

// TestPDB_GetCommittedState_DestructorAtZero pins the boundary of the
// `suicideIdx < 0` test: a destructor at tx index 0 must NOT trigger the
// base read — destructed accounts have wiped storage. Kills the `< -> <=`
// boundary mutation, which would otherwise flip the test to true when
// suicideIdx == 0 and incorrectly serve the pre-destruction base value.
func TestPDB_GetCommittedState_DestructorAtZero(t *testing.T) {
	addr := common.HexToAddress("0xa")
	slot := common.HexToHash("0x1")
	preDestructionVal := common.HexToHash("0xbeef")

	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, err := New(types.EmptyRootHash, NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	sdb.SetState(addr, slot, preDestructionVal)
	base := NewSafeBase(sdb, 2)
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	// Tx 0 destructed addr; reader is tx 5. priorDestructedAt returns 0,
	// which is the exact boundary the < 0 check guards.
	store.WriteInc(blockstm.NewSubpathKey(addr, SuicidePath), 0, 0, true)
	pdb := NewParallelStateDB(5, base, store, bals)
	pdb.EnableReadTracking()

	got := pdb.GetCommittedState(addr, slot)
	if got != (common.Hash{}) {
		t.Fatalf("GetCommittedState with destructor at tx 0: got %s, want zero (pre-destruction base must not bleed through)",
			got.Hex())
	}
}

// TestPDB_DestructedMiss_LaterWriterInvalidates is the end-to-end pin:
// (1) tx D destroys A; (2) tx Y at a higher index reads (A, slot) and
// settles zero; (3) the destructor or some later tx then writes (A, slot)
// with a real value. After step (3), Y.Validate() must return false so
// the executor re-executes Y. Pre-fix this returned true (false negative).
func TestPDB_DestructedMiss_LaterWriterInvalidates(t *testing.T) {
	addr := common.HexToAddress("0xa")
	slot := common.HexToHash("0x1")
	stateK := blockstm.NewStateKey(addr, slot)
	const destructorIdx = 1
	const writerIdx = 3
	const readerIdx = 5

	pdb, store, _ := newTestPDB(t, readerIdx)
	pdb.EnableReadTracking()
	recordPriorDestruct(store, addr, destructorIdx)

	// Reader observes zero (no slot writer yet).
	if got := pdb.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("pre-write read: got %v, want zero", got)
	}

	// Later writer lands AFTER the destructor (writerIdx > destructorIdx),
	// which is the case that produces serial=value vs v2=zero divergence.
	realVal := common.HexToHash("0xbeef")
	store.WriteInc(stateK, writerIdx, 0, realVal)

	if pdb.Validate() {
		t.Fatal("Validate=true after a later writer for the destructed-miss key — the reader settles a stale zero while serial would observe the new value")
	}
}

// TestPDB_Witness_SharedAcrossCopyViaSetWitness pins the share-by-reference
// invariant that V2StateProcessor.Process establishes between finalDB and
// readBase. StateDB.Copy() deep-copies the witness (statedb.go:1294), so a
// fresh Copy()'d statedb sees a distinct *Witness; readBase.SetWitness must
// be threaded immediately after the Copy() to restore identity.
//
// Without the SetWitness restore, V2 workers writing to PDB.Witness()
// (BLOCKHASH path) target the orphan witness on readBase; finalDB.Witness()
// never observes the entry because Headers has no equivalent of the
// CollectStateWitness / CollectCodeWitness merge.
func TestPDB_Witness_SharedAcrossCopyViaSetWitness(t *testing.T) {
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	finalDB, err := New(types.EmptyRootHash, NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	witness := &stateless.Witness{
		Headers: []*types.Header{},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	finalDB.SetWitness(witness)

	// Deep copy → readBase carries its own *Witness; the bug surfaces here
	// without the SetWitness call.
	readBase := finalDB.Copy()
	if readBase.Witness() == finalDB.Witness() {
		t.Fatal("StateDB.Copy() unexpectedly returned a shared witness; bug premise no longer holds")
	}
	// The V2 processor's fix: re-share the witness pointer.
	readBase.SetWitness(finalDB.Witness())

	// PDB.Witness() delegates to rawBase (readBase) — must return the same
	// object finalDB sees.
	pdb := NewParallelStateDB(0, NewSafeBase(readBase, 2), blockstm.NewMVStore(), blockstm.NewMVBalanceStore())
	if pdb.Witness() != finalDB.Witness() {
		t.Fatal("PDB.Witness() identity diverged from finalDB.Witness() — BLOCKHASH writes would land in the wrong witness")
	}

	// End-to-end: appending a header via the PDB-visible Witness must be
	// observable through finalDB.Witness().
	header := &types.Header{Number: big.NewInt(42)}
	pdb.Witness().Headers = append(pdb.Witness().Headers, header)
	got := finalDB.Witness().Headers
	if len(got) != 1 || got[0] != header {
		t.Fatalf("finalDB.Witness().Headers = %v; want [header(42)] via PDB write", got)
	}
}

// TestPDB_AddPreimage_ClonesAndKeepsFirst pins parity with serial
// StateDB.AddPreimage: clone the caller's slice (opKeccak256 passes one
// backed by EVM memory, which subsequent ops can mutate) and keep the
// first-recorded value (don't overwrite on later AddPreimage(hash, …)).
func TestPDB_AddPreimage_ClonesAndKeepsFirst(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	hash := common.HexToHash("0xabcd")

	preimage := []byte{1, 2, 3, 4}
	pdb.AddPreimage(hash, preimage)

	// Mutating the caller's buffer must not corrupt the recorded value.
	preimage[0] = 0xff
	if got := pdb.preimages[hash]; got[0] != 1 {
		t.Fatalf("AddPreimage did not clone: caller mutation leaked into preimages, got %x", got)
	}

	// A second AddPreimage for the same hash must NOT overwrite.
	pdb.AddPreimage(hash, []byte{9, 9, 9, 9})
	if got := pdb.preimages[hash][0]; got != 1 {
		t.Fatalf("AddPreimage overwrote on second call: got %x, want 0x01 (first write)", got)
	}
}
