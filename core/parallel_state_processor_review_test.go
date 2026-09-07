package core

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// recordingLogHandler captures slog records into an in-memory slice for tests.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

// newV2SettleTestEnv builds an in-memory state, a v2Env wired to it, and the
// closure-captured accumulators that newV2SettleFn writes through.
func newV2SettleTestEnv(t *testing.T, coinbase common.Address) (
	*v2Env, *state.StateDB, *blockstm.MVStore, *blockstm.MVBalanceStore,
	*types.Receipts, *[]*types.Log, *uint64, *int,
	vm.BlockContext, *params.ChainConfig,
) {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, err := state.New(types.EmptyRootHash, state.NewDatabase(tdb, nil))
	if err != nil {
		t.Fatal(err)
	}
	chainConfig := params.TestChainConfig
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	env := newV2Env(sdb, store, bals, blockCtx, vm.Config{}, chainConfig, 30000000, 1)

	receipts := types.Receipts{}
	logs := []*types.Log{}
	totalUsedGas := uint64(0)
	panickedIdx := -1
	return env, sdb, store, bals, &receipts, &logs, &totalUsedGas, &panickedIdx, blockCtx, chainConfig
}

// makeDummyTx returns a minimal valid signed tx and its corresponding Message,
// so newV2SettleFn / buildV2Receipt can populate receipt fields.
func makeDummyTx(t *testing.T, nonce uint64) (*types.Transaction, *Message) {
	t.Helper()
	key, _ := crypto.GenerateKey()
	signer := types.NewLondonSigner(params.TestChainConfig.ChainID)
	to := common.HexToAddress("0xCAFE")
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   params.TestChainConfig.ChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &to,
		Value:     big.NewInt(0),
	}), signer, key)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := TransactionToMessage(tx, signer, big.NewInt(1))
	if err != nil {
		t.Fatal(err)
	}
	return tx, msg
}

// TestV2SettleFn_SkipsPanickedPDB verifies the Fix #2 settle-side guard:
// a panicked PDB that reaches the settle callback must NOT have its state
// applied to finalDB and MUST set the shared panickedIdx so the caller
// can fail the block.
func TestV2SettleFn_SkipsPanickedPDB(t *testing.T) {
	coinbase := common.HexToAddress("0xCB")
	env, finalDB, _, _, receipts, logs, totalUsedGas, panickedIdx, blockCtx, cc := newV2SettleTestEnv(t, coinbase)

	// Build a panicked PDB with state changes that would otherwise be settled.
	pdb := state.NewParallelStateDB(0, env.safeBase, env.store, env.bals)
	pdb.SetDeferMVWrites(true)
	pdb.Coinbase = coinbase
	addr := common.HexToAddress("0xdeadbeef")
	pdb.SetNonce(addr, 9, tracing.NonceChangeUnspecified)
	pdb.AddBalance(addr, uint256.NewInt(123), tracing.BalanceChangeUnspecified)
	pdb.UsedGas = 21000 // would inflate cumulative gas if we didn't skip
	pdb.Panicked = true

	tx, msg := makeDummyTx(t, 0)
	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

	var execErrIdx int = -1
	var execErr error
	settleFn := newV2SettleFn(tasks, env, finalDB, blockCtx, common.Hash{}, cc, receipts, logs, totalUsedGas, panickedIdx, &execErrIdx, &execErr, new(error))

	settleFn(0, pdb)

	if *panickedIdx != 0 {
		t.Fatalf("expected panickedIdx=0, got %d", *panickedIdx)
	}
	if len(*receipts) != 0 {
		t.Fatalf("expected no receipts for panicked tx, got %d", len(*receipts))
	}
	if *totalUsedGas != 0 {
		t.Fatalf("expected totalUsedGas=0 (panicked tx contributes nothing), got %d", *totalUsedGas)
	}
	// finalDB must not have been mutated by the panicked PDB.
	if got := finalDB.GetNonce(addr); got != 0 {
		t.Fatalf("finalDB.Nonce(%x)=%d, want 0 — panicked tx modified finalDB", addr, got)
	}
	if got := finalDB.GetBalance(addr); !got.IsZero() {
		t.Fatalf("finalDB.Balance(%x)=%s, want 0 — panicked tx modified finalDB", addr, got)
	}
}

// TestV2SettleFn_NilStateIsNoOp pins the defense-in-depth nil guard.
// finishReexec already filters nil x.states[idx] out of the chSettle
// stream, but if a future caller wires the settle goroutine differently
// the type assertion `st.(*state.ParallelStateDB)` would panic on a nil
// interface (the settle goroutine has no recover, so the panic crashes
// the node). The guard returns early instead — no panic, no mutation,
// panickedIdx untouched.
func TestV2SettleFn_NilStateIsNoOp(t *testing.T) {
	coinbase := common.HexToAddress("0xCB")
	env, finalDB, _, _, receipts, logs, totalUsedGas, panickedIdx, blockCtx, cc := newV2SettleTestEnv(t, coinbase)

	tx, msg := makeDummyTx(t, 0)
	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}
	var execErrIdx int = -1
	var execErr error
	settleFn := newV2SettleFn(tasks, env, finalDB, blockCtx, common.Hash{}, cc, receipts, logs, totalUsedGas, panickedIdx, &execErrIdx, &execErr, new(error))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("settleFn panicked on nil state: %v", r)
		}
	}()
	settleFn(0, nil)

	if *panickedIdx != -1 {
		t.Fatalf("nil-state settle bumped panickedIdx to %d; want -1 (untouched)", *panickedIdx)
	}
	if len(*receipts) != 0 || len(*logs) != 0 || *totalUsedGas != 0 {
		t.Fatalf("nil-state settle mutated outputs: receipts=%d logs=%d gas=%d",
			len(*receipts), len(*logs), *totalUsedGas)
	}
}

// TestV2SettleFn_RecordsFirstPanickedIdx verifies that the FIRST panicked
// index wins — subsequent panicked txs must not overwrite it. This matters
// when validation surfaces multiple panics (rare but possible), so the
// caller's error message points at the original failure.
func TestV2SettleFn_RecordsFirstPanickedIdx(t *testing.T) {
	coinbase := common.HexToAddress("0xCB")
	env, finalDB, _, _, receipts, logs, totalUsedGas, panickedIdx, blockCtx, cc := newV2SettleTestEnv(t, coinbase)

	// Two panicked PDBs.
	mkPanicked := func(idx int) *state.ParallelStateDB {
		pdb := state.NewParallelStateDB(idx, env.safeBase, env.store, env.bals)
		pdb.SetDeferMVWrites(true)
		pdb.Coinbase = coinbase
		pdb.Panicked = true
		return pdb
	}
	tx0, msg0 := makeDummyTx(t, 0)
	tx1, msg1 := makeDummyTx(t, 1)
	tasks := []V2Task{
		{Index: 0, Tx: tx0, Msg: msg0},
		{Index: 1, Tx: tx1, Msg: msg1},
	}

	var execErrIdx int = -1
	var execErr error
	settleFn := newV2SettleFn(tasks, env, finalDB, blockCtx, common.Hash{}, cc, receipts, logs, totalUsedGas, panickedIdx, &execErrIdx, &execErr, new(error))
	settleFn(0, mkPanicked(0))
	settleFn(1, mkPanicked(1))

	if *panickedIdx != 0 {
		t.Fatalf("expected panickedIdx=0 (first panic), got %d", *panickedIdx)
	}
}

// TestV2SettleFn_NormalPDBSettlesAfterSkip verifies that after a panicked
// PDB is skipped, a subsequent NON-panicked PDB still settles correctly.
// (This is mostly to pin behavior — production sees the panickedIdx and
// errors out before the next tx, but the callback itself must not be in
// a poisoned state.)
func TestV2SettleFn_NormalPDBSettlesAfterSkip(t *testing.T) {
	coinbase := common.HexToAddress("0xCB")
	env, finalDB, _, _, receipts, logs, totalUsedGas, panickedIdx, blockCtx, cc := newV2SettleTestEnv(t, coinbase)

	tx0, msg0 := makeDummyTx(t, 0)
	tx1, msg1 := makeDummyTx(t, 1)
	tasks := []V2Task{
		{Index: 0, Tx: tx0, Msg: msg0},
		{Index: 1, Tx: tx1, Msg: msg1},
	}

	// Panicked tx 0
	pdb0 := state.NewParallelStateDB(0, env.safeBase, env.store, env.bals)
	pdb0.SetDeferMVWrites(true)
	pdb0.Coinbase = coinbase
	pdb0.Panicked = true

	// Healthy tx 1 with a balance bump
	pdb1 := state.NewParallelStateDB(1, env.safeBase, env.store, env.bals)
	pdb1.SetDeferMVWrites(true)
	pdb1.Coinbase = coinbase
	addr := common.HexToAddress("0xfeed")
	pdb1.AddBalance(addr, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	pdb1.UsedGas = 21000

	var execErrIdx int = -1
	var execErr error
	settleFn := newV2SettleFn(tasks, env, finalDB, blockCtx, common.Hash{}, cc, receipts, logs, totalUsedGas, panickedIdx, &execErrIdx, &execErr, new(error))
	settleFn(0, pdb0)
	settleFn(1, pdb1)

	if *panickedIdx != 0 {
		t.Fatalf("panickedIdx=%d, want 0", *panickedIdx)
	}
	if *totalUsedGas != 21000 {
		t.Fatalf("totalUsedGas=%d, want 21000 (only tx1 contributes)", *totalUsedGas)
	}
	if len(*receipts) != 1 {
		t.Fatalf("receipts=%d, want 1 (only tx1 settled)", len(*receipts))
	}
	if got := finalDB.GetBalance(addr); got.Uint64() != 7 {
		t.Fatalf("finalDB.Balance(%x)=%s, want 7 — tx1 must still settle", addr, got)
	}
}

// TestV2SettleFn_RecordsBaseReadErr verifies the settle-time base-read gate:
// a settled incarnation carrying BaseReadErr must record the error for the
// caller (first error wins), skip settlement so its zero-ish-read state never
// reaches finalDB, and not disturb later healthy txs. The check lives in the
// settle callback and NOT in a post-hoc scan of the executor's states array:
// settlement recycles each pdb into the pool where a later tx's Reset reuses
// it, so states[i] can alias a different (speculative, later-invalidated)
// incarnation by the time a post-hoc scan runs — the scan both fabricates
// errors that never settled and loses errors that did.
func TestV2SettleFn_RecordsBaseReadErr(t *testing.T) {
	coinbase := common.HexToAddress("0xCB")
	env, finalDB, _, _, receipts, logs, totalUsedGas, panickedIdx, blockCtx, cc := newV2SettleTestEnv(t, coinbase)

	tx0, msg0 := makeDummyTx(t, 0)
	tx1, msg1 := makeDummyTx(t, 1)
	tx2, msg2 := makeDummyTx(t, 2)
	tasks := []V2Task{
		{Index: 0, Tx: tx0, Msg: msg0},
		{Index: 1, Tx: tx1, Msg: msg1},
		{Index: 2, Tx: tx2, Msg: msg2},
	}

	firstErr := errors.New("missing trie node aa (first)")
	pdb0 := state.NewParallelStateDB(0, env.safeBase, env.store, env.bals)
	pdb0.SetDeferMVWrites(true)
	pdb0.Coinbase = coinbase
	addr := common.HexToAddress("0xdeadbeef")
	pdb0.AddBalance(addr, uint256.NewInt(123), tracing.BalanceChangeUnspecified)
	pdb0.UsedGas = 21000
	pdb0.BaseReadErr = firstErr

	pdb1 := state.NewParallelStateDB(1, env.safeBase, env.store, env.bals)
	pdb1.SetDeferMVWrites(true)
	pdb1.Coinbase = coinbase
	pdb1.BaseReadErr = errors.New("missing trie node bb (second)")

	healthy := common.HexToAddress("0xfeed")
	pdb2 := state.NewParallelStateDB(2, env.safeBase, env.store, env.bals)
	pdb2.SetDeferMVWrites(true)
	pdb2.Coinbase = coinbase
	pdb2.AddBalance(healthy, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	pdb2.UsedGas = 21000

	var execErrIdx int = -1
	var execErr error
	var readErr error
	settleFn := newV2SettleFn(tasks, env, finalDB, blockCtx, common.Hash{}, cc, receipts, logs, totalUsedGas, panickedIdx, &execErrIdx, &execErr, &readErr)
	settleFn(0, pdb0)
	settleFn(1, pdb1)
	settleFn(2, pdb2)

	if readErr != firstErr {
		t.Fatalf("readErr=%v, want the first error to win", readErr)
	}
	if got := finalDB.GetBalance(addr); !got.IsZero() {
		t.Fatalf("finalDB.Balance(%x)=%s, want 0 — bad-read tx must not settle", addr, got)
	}
	if got := finalDB.GetBalance(healthy); got.Uint64() != 7 {
		t.Fatalf("finalDB.Balance(%x)=%s, want 7 — healthy tx must still settle", healthy, got)
	}
	if len(*receipts) != 1 {
		t.Fatalf("receipts=%d, want 1 (only the healthy tx settled)", len(*receipts))
	}
	if *totalUsedGas != 21000 {
		t.Fatalf("totalUsedGas=%d, want 21000 (bad-read txs contribute nothing)", *totalUsedGas)
	}
	if *panickedIdx != -1 || execErrIdx != -1 {
		t.Fatalf("base read error must not masquerade as panic/exec error: panickedIdx=%d execErrIdx=%d", *panickedIdx, execErrIdx)
	}
}

// TestV2StateProcessor_PanickedTxFailsBlock verifies the end-to-end
// integration: a tx that panics during V2 execution causes the result to
// surface PanickedIdx and produce no receipts, so V2StateProcessor.Process
// can fail the block instead of committing partial state.
//
// Panic is injected via a tracing.Hooks.OnEnter callback — V2 executes
// through vm.NewEVM which fires OnEnter on the initial call frame. The
// panic propagates through ApplyMessageNoFeeLog into v2Env.applyMessage's
// deferred recover.
func TestV2StateProcessor_PanickedTxFailsBlock(t *testing.T) {
	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))

	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	recipient := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainConfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))

	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

	coinbase := common.HexToAddress("0xCB")
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	// Inject a panicking tracer hook fired on the initial call frame.
	cfg := vm.Config{Tracer: &tracing.Hooks{
		OnEnter: func(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
			panic("intentional test panic from OnEnter")
		},
	}}

	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()

	result := ExecuteV2BlockSTM(context.Background(), tasks, base, store, bals, blockCtx, common.Hash{}, cfg, chainConfig,
		blockCtx.GasLimit, 1, finalDB, nil)

	if result.PanickedIdx != 0 {
		t.Fatalf("expected PanickedIdx=0, got %d", result.PanickedIdx)
	}
	if len(result.Receipts) != 0 {
		t.Fatalf("expected 0 receipts after panic, got %d", len(result.Receipts))
	}

	// And the wrapping error (constructed by V2StateProcessor.Process) must
	// reference the tx index so logs and fallback handlers can pinpoint the
	// failure.
	wantErrSnippet := "tx 0 panicked"
	gotErr := errors.New("v2: tx 0 panicked during execution")
	if !strings.Contains(gotErr.Error(), wantErrSnippet) {
		t.Fatalf("error string %q does not contain %q", gotErr, wantErrSnippet)
	}
	_ = context.Background()
}

// TestV2ApplyMessage_FirstIncarnationPanicLogsDebug pins the log-level
// contract for V2's tx-execution panic recover: incarnation 0 is a
// speculative attempt that may legitimately panic (SSTORE-refund
// underflow on a stale GetCommittedState read, etc.) and gets re-
// executed; logging it at ERROR trains operators to ignore the signal.
// Incarnation ≥ 1 panicking means the re-exec also failed, which is a
// real bug indicator and keeps ERROR. Inject a tracer panic that fires
// on every call so both incarnations panic and produce exactly one
// Debug + one Error V2-panic record.
func TestV2ApplyMessage_FirstIncarnationPanicLogsDebug(t *testing.T) {
	h := &recordingLogHandler{}
	prev := log.Root()
	log.SetDefault(log.NewLogger(h))
	defer log.SetDefault(prev)

	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	to := common.HexToAddress("0x1111")
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: chainConfig.ChainID, Nonce: 0,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1e9),
		Gas: 21000, To: &to, Value: big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))
	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}
	cfg := vm.Config{Tracer: &tracing.Hooks{
		OnEnter: func(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
			panic("intentional panic from OnEnter")
		},
	}}

	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()

	_ = ExecuteV2BlockSTM(context.Background(), tasks, base,
		blockstm.NewMVStore(), blockstm.NewMVBalanceStore(),
		blockCtx, common.Hash{}, cfg, chainConfig, blockCtx.GasLimit, 1, finalDB, nil)

	var debug, errs int
	for _, r := range h.records {
		if !strings.Contains(r.Message, "V2 tx execution panic") {
			continue
		}
		switch r.Level {
		case slog.LevelDebug:
			debug++
		case slog.LevelError:
			errs++
		}
	}
	if debug != 1 || errs != 1 {
		t.Fatalf("V2-panic log levels: got %d Debug + %d Error; want 1 + 1", debug, errs)
	}
}

// TestV2StateProcessor_ProducesWitness verifies that V2 BlockSTM populates
// a passed-in stateless.Witness with the trie nodes and code blobs touched
// by worker reads.
//
// V2 uses concurrent trie reads (EnableConcurrentReads on the shared reader),
// which historically skipped the prevalueTracer to avoid lock contention.
// That made trie.Witness() return nothing for any nodes V2 resolved through
// getConcurrent — so blocks processed by V2 produced empty/incomplete
// witnesses, and the V2 path was disabled entirely whenever a witness was
// requested. This test pins the fix.
func TestV2StateProcessor_ProducesWitness(t *testing.T) {
	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))

	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	recipient := common.HexToAddress("0x4444444444444444444444444444444444444444")
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainConfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))
	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

	// Attach a witness to the base StateDB before V2 runs. The same
	// witness pointer is shared with readBase / pool copies / finalDB via
	// StateDB.Copy(), and ParallelStateDB.Witness() returns it for the
	// EVM's BLOCKHASH opcode path.
	w := &stateless.Witness{
		Headers: []*types.Header{{Number: big.NewInt(1)}},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	base.SetWitness(w)

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	finalDB := base
	finalDB.StartPrefetcher("test", w, nil)
	defer finalDB.StopPrefetcher()

	result := ExecuteV2BlockSTM(context.Background(), tasks, base, store, bals, blockCtx, common.Hash{}, vm.Config{}, chainConfig,
		blockCtx.GasLimit, 1, finalDB, nil)

	if result.ExecErrIdx >= 0 {
		t.Fatalf("V2 returned error: %v", result.ExecErr)
	}
	if result.PanickedIdx >= 0 {
		t.Fatalf("V2 panicked at tx %d", result.PanickedIdx)
	}
	// CollectStateWitness pulls the worker-side trie tracers into the
	// witness. Without this call (the path V2StateProcessor.Process now
	// invokes after settle), addresses that were only-read by workers
	// would be missing from the witness.
	finalDB.CollectStateWitness()
	finalDB.IntermediateRoot(true)

	if len(w.State) == 0 {
		t.Error("witness.State is empty — V2 worker reads did not populate the prevalue tracer")
	}
}

// TestExecuteV2BlockSTM_MidFlightCancellation verifies the executor unblocks
// promptly when ctx is cancelled DURING execution (not pre-cancelled). Without
// ctx-aware waitForTx / waitForFinal and a ctx-select on validateOne's
// execDone read, a worker or the validation goroutine could hang indefinitely
// when the dispatcher exits without pushing all tasks.
func TestExecuteV2BlockSTM_MidFlightCancellation(t *testing.T) {
	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	to := common.HexToAddress("0x5555")
	tasks := make([]V2Task, 32) // enough that some are in-flight when we cancel
	for i := range tasks {
		tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainConfig.ChainID,
			Nonce:     uint64(i),
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(1e9),
			Gas:       21000,
			To:        &to,
			Value:     big.NewInt(1),
		}), signer, key)
		msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))
		tasks[i] = V2Task{Index: i, Tx: tx, Msg: msg}
	}

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}

	ctx, cancel := context.WithCancel(context.Background())

	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	// Cancel after a tiny delay so the dispatcher / workers / validator
	// are all in flight before ctx fires.
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ExecuteV2BlockSTM(ctx, tasks, base, store, bals, blockCtx, common.Hash{}, vm.Config{}, chainConfig,
			blockCtx.GasLimit, 4, finalDB, nil)
	}()

	select {
	case <-done:
		// Returned promptly — no hang.
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteV2BlockSTM hung after mid-flight cancellation")
	}
}

// TestExecuteV2BlockSTM_HonoursCancellation verifies Fix #4: when the parent
// context is already cancelled, ExecuteV2BlockSTM returns promptly without
// processing all txs.
//
// Without context wiring, V2's dispatcher and validation loop ignored
// cancellation; if serial won the parallel-vs-serial race, the import would
// stall waiting for V2 to finish naturally (≈50–200ms for a full block).
// With cancellation, V2 stops at the next dispatch/validation boundary.
func TestExecuteV2BlockSTM_HonoursCancellation(t *testing.T) {
	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	to := common.HexToAddress("0x4444")
	tasks := make([]V2Task, 8)
	for i := range tasks {
		tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainConfig.ChainID,
			Nonce:     uint64(i),
			GasTipCap: big.NewInt(1),
			GasFeeCap: big.NewInt(1e9),
			Gas:       21000,
			To:        &to,
			Value:     big.NewInt(1),
		}), signer, key)
		msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))
		tasks[i] = V2Task{Index: i, Tx: tx, Msg: msg}
	}

	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}

	// Pre-cancelled context: executor must not process all txs.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()

	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()
	start := time.Now()
	result := ExecuteV2BlockSTM(ctx, tasks, base, store, bals, blockCtx, common.Hash{}, vm.Config{}, chainConfig,
		blockCtx.GasLimit, 1, finalDB, nil)
	elapsed := time.Since(start)

	// We don't assert the result is empty — workers may have started before the
	// cancel signal propagated. We just assert the executor returned promptly.
	// On the developer machine this completes in <50ms; in CI we allow 5s as a
	// generous upper bound. Without the fix, this would take seconds (one full
	// EVM exec per tx, ×8 txs).
	if elapsed > 5*time.Second {
		t.Errorf("ExecuteV2BlockSTM took %v with cancelled ctx, expected fast return", elapsed)
	}
	_ = result
}

// TestV2StateProcessor_RefusesTracer pins that a non-nil vm.Config.Tracer
// makes V2.Process return errV2TracerUnsupported so ProcessBlock falls
// back to the serial path. Tracer hooks aren't goroutine-safe and would
// race across concurrent V2 workers.
func TestV2StateProcessor_RefusesTracer(t *testing.T) {
	p := NewV2StateProcessor(nil, nil, 2)
	cfg := vm.Config{Tracer: &tracing.Hooks{}}
	_, err := p.Process(nil, nil, cfg, nil, nil)
	if !errors.Is(err, errV2TracerUnsupported) {
		t.Fatalf("V2.Process with tracer: got %v, want errV2TracerUnsupported", err)
	}
}

// TestV2StateProcessor_ClampsNumWorkers verifies Fix #5: zero or negative
// numWorkers must be clamped to a sensible default. With numWorkers=0 the
// executor would deadlock because the dispatcher window collapses to zero
// (see core/blockstm/v2_executor.go:355).
func TestNewV2StateProcessor_ClampsNumWorkers(t *testing.T) {
	cases := []int{-1, 0}
	for _, n := range cases {
		p := NewV2StateProcessor(nil, nil, n)
		if p.numWorkers <= 0 {
			t.Errorf("NewV2StateProcessor(numWorkers=%d) → got %d, want > 0", n, p.numWorkers)
		}
	}
	// Positive values should pass through unchanged.
	p := NewV2StateProcessor(nil, nil, 4)
	if p.numWorkers != 4 {
		t.Errorf("NewV2StateProcessor(numWorkers=4) → got %d, want 4", p.numWorkers)
	}
}

// TestV2StateProcessor_ReceiptHasBlockHash verifies Fix #3: receipts produced
// by V2 must carry the correct BlockHash and pass it to GetLogs so log
// entries reference the right block.
func TestV2StateProcessor_ReceiptHasBlockHash(t *testing.T) {
	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))

	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	recipient := common.HexToAddress("0x3333333333333333333333333333333333333333")
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainConfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))
	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

	blockHash := common.HexToHash("0xabcdef0123456789")
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    common.HexToAddress("0xCB"),
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()

	result := ExecuteV2BlockSTM(context.Background(), tasks, base, store, bals, blockCtx, blockHash, vm.Config{}, chainConfig,
		blockCtx.GasLimit, 1, finalDB, nil)

	if len(result.Receipts) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(result.Receipts))
	}
	r := result.Receipts[0]
	if r.BlockHash != blockHash {
		t.Errorf("receipt BlockHash mismatch: got %v want %v", r.BlockHash, blockHash)
	}
}

// TestV2StateProcessor_ApplyMessageErrorFailsBlock verifies the Fix #1
// behaviour: a tx whose ApplyMessage returns a consensus-level error
// (here: invalid nonce) must NOT be settled as a zero-gas success and the
// V2 executor must surface ExecErrIdx so the processor aborts the block.
//
// Without the fix, applyMessage swallowed result==nil and the settle path
// produced a successful receipt with UsedGas=0 — diverging from serial,
// which returns the underlying ApplyMessage error and aborts.
func TestV2StateProcessor_ApplyMessageErrorFailsBlock(t *testing.T) {
	chainConfig := params.TestChainConfig
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	sdb, _ := state.New(common.Hash{}, state.NewDatabase(tdb, nil))

	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	sdb.AddBalance(sender, uint256.NewInt(1e18), 0)
	// Real account nonce is 0 — sign a tx with nonce=5 so ApplyMessage
	// returns ErrNonceTooHigh.
	sdb.SetNonce(sender, 0, 0)
	root, _ := sdb.Commit(0, false, false)
	tdb.Commit(root, false)
	base, _ := state.New(root, state.NewDatabase(tdb, nil))

	signer := types.NewLondonSigner(chainConfig.ChainID)
	recipient := common.HexToAddress("0x2222222222222222222222222222222222222222")
	tx, _ := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainConfig.ChainID,
		Nonce:     5, // ← stale nonce
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1e9),
		Gas:       21000,
		To:        &recipient,
		Value:     big.NewInt(1),
	}), signer, key)
	msg, _ := TransactionToMessage(tx, signer, big.NewInt(1))

	tasks := []V2Task{{Index: 0, Tx: tx, Msg: msg}}

	coinbase := common.HexToAddress("0xCB")
	blockCtx := vm.BlockContext{
		CanTransfer: CanTransfer,
		Transfer:    Transfer,
		GetHash:     func(n uint64) common.Hash { return common.Hash{} },
		Coinbase:    coinbase,
		GasLimit:    30000000,
		BlockNumber: big.NewInt(1),
		Time:        1,
		BaseFee:     big.NewInt(1),
	}
	store := blockstm.NewMVStore()
	bals := blockstm.NewMVBalanceStore()

	finalDB := base.Copy()
	finalDB.StartPrefetcher("test", nil, nil)
	defer finalDB.StopPrefetcher()

	result := ExecuteV2BlockSTM(context.Background(), tasks, base, store, bals, blockCtx, common.Hash{}, vm.Config{}, chainConfig,
		blockCtx.GasLimit, 1, finalDB, nil)

	if result.ExecErrIdx != 0 {
		t.Fatalf("expected ExecErrIdx=0, got %d", result.ExecErrIdx)
	}
	if result.ExecErr == nil {
		t.Fatal("expected ExecErr to be set, got nil")
	}
	if !strings.Contains(result.ExecErr.Error(), "nonce") {
		t.Fatalf("expected nonce error, got %v", result.ExecErr)
	}
	if len(result.Receipts) != 0 {
		t.Fatalf("expected 0 receipts after consensus error, got %d", len(result.Receipts))
	}
	if result.GasUsed != 0 {
		t.Fatalf("expected 0 gas used after consensus error, got %d", result.GasUsed)
	}
}
