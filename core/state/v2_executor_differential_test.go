package state

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
)

// ---------------------------------------------------------------------------
// Multi-tx executor differential harness.
//
// Runs a list of transactions (each one an op-script + sender/to) through
// two paths:
//
//  1. Serial: apply every tx's ops to a shared StateDB in order.
//  2. V2: wrap each tx as a V2Task, run ExecuteV2BlockSTM with N workers,
//     settle each ParallelStateDB onto a finalDB via V2SettleFn.
//
// After both complete, assert byte-identical state root and per-probe
// equivalence. This is the only harness that actually exercises
// ExecuteV2BlockSTM's conflict-detection + re-execution logic.
// ---------------------------------------------------------------------------

// txScript is one transaction's inputs for the harness.
type txScript struct {
	sender common.Address
	to     *common.Address
	ops    []pdbOp
}

// exScenario is a full multi-tx differential scenario.
type exScenario struct {
	name    string
	setup   []pdbOp    // applied to pre-block state on both paths (then committed)
	txs     []txScript // executed in order on serial, and concurrently via V2
	probes  []probe
	workers int // V2 worker count (default 4)
}

// ---------------------------------------------------------------------------
// V2Task + V2Env stubs for tests
// ---------------------------------------------------------------------------

type exTask struct {
	idx    int
	sender common.Address
	to     *common.Address
}

func (t *exTask) Index() int                    { return t.idx }
func (t *exTask) Sender() common.Address        { return t.sender }
func (t *exTask) To() *common.Address           { return t.to }
func (t *exTask) Authorities() []common.Address { return nil }

// exEnv is a V2Env that runs each task's op script through a
// ParallelStateDB. The env is intentionally minimal — no EVM, no gas
// accounting — because the harness's job is to test V2 executor's
// conflict-detection, not contract execution.
type exEnv struct {
	safeBase *SafeBase
	store    *blockstm.MVStore
	bals     *blockstm.MVBalanceStore

	scripts []txScript

	poolMu sync.Mutex
	pool   []*ParallelStateDB

	// Track latest PDB per task idx — settle callback fetches it.
	statesMu sync.Mutex
	states   map[int]*ParallelStateDB
}

func newExEnv(base *StateDB, poolSize int, scripts []txScript) *exEnv {
	return &exEnv{
		safeBase: NewSafeBase(base, poolSize),
		store:    blockstm.NewMVStore(),
		bals:     blockstm.NewMVBalanceStore(),
		scripts:  scripts,
		states:   make(map[int]*ParallelStateDB),
	}
}

func (e *exEnv) BaseNonce(addr common.Address) uint64 {
	nonce, _ := e.safeBase.GetNonce(addr)
	return nonce
}

func (e *exEnv) BaseCodeSize(addr common.Address) int {
	size, _ := e.safeBase.GetCodeSize(addr)
	return size
}

func (e *exEnv) acquire(idx int) *ParallelStateDB {
	e.poolMu.Lock()
	defer e.poolMu.Unlock()
	if n := len(e.pool); n > 0 {
		pdb := e.pool[n-1]
		e.pool = e.pool[:n-1]
		return pdb
	}
	return NewParallelStateDB(idx, e.safeBase, e.store, e.bals)
}

func (e *exEnv) Recycle(s blockstm.V2TxState) {
	pdb := s.(*ParallelStateDB)
	// Reset for reuse but leave base/store/bals attached.
	pdb.Reset(0, e.safeBase, e.store, e.bals)
	e.poolMu.Lock()
	e.pool = append(e.pool, pdb)
	e.poolMu.Unlock()
}

func (e *exEnv) Execute(task blockstm.V2Task, workerID int, incarnation int,
	senderNonces map[common.Address]uint64,
	coinbase common.Address,
	waitForTx func(int),
	waitForFinal func(int),
	deferWrites bool,
) blockstm.V2TxState {
	idx := task.Index()
	pdb := e.acquire(idx)
	// Reset to this task's state.
	pdb.Reset(idx, e.safeBase, e.store, e.bals)
	pdb.Incarnation = incarnation
	pdb.SenderNonces = senderNonces
	pdb.Coinbase = coinbase
	pdb.WaitForTx = waitForTx
	pdb.WaitForFinal = waitForFinal
	pdb.SetDeferMVWrites(deferWrites)
	pdb.EnableReadTracking()

	for _, op := range e.scripts[idx].ops {
		op.applyTo(pdb)
	}

	e.statesMu.Lock()
	e.states[idx] = pdb
	e.statesMu.Unlock()
	return pdb
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

func runExecutorSerial(t *testing.T, sc exScenario) (common.Hash, []probeResult, []logSummary) {
	t.Helper()
	sdb, db := newDiffStateDB(t)
	for _, op := range sc.setup {
		op.applyTo(sdb)
	}
	sdb, _ = commitAndReopen(t, sdb, db, 0)

	for _, tx := range sc.txs {
		for _, op := range tx.ops {
			op.applyTo(sdb)
		}
		sdb.Finalise(true) // between-tx finalise matches V2's SettleTo flow
	}
	root := sdb.IntermediateRoot(true)
	return root, collectProbes(sdb, sc.probes), collectStateDBLogs(sdb)
}

func runExecutorV2(t *testing.T, sc exScenario) (common.Hash, []probeResult, []logSummary) {
	t.Helper()
	sdb, db := newDiffStateDB(t)
	for _, op := range sc.setup {
		op.applyTo(sdb)
	}
	_, root := commitAndReopen(t, sdb, db, 0)

	baseDB, err := New(root, db)
	if err != nil {
		t.Fatalf("reopen base: %v", err)
	}
	finalDB, err := New(root, db)
	if err != nil {
		t.Fatalf("reopen final: %v", err)
	}

	workers := sc.workers
	if workers == 0 {
		workers = 4
	}
	env := newExEnv(baseDB, workers+1, sc.txs)

	tasks := make([]blockstm.V2Task, len(sc.txs))
	for i, tx := range sc.txs {
		tasks[i] = &exTask{idx: i, sender: tx.sender, to: tx.to}
	}

	// SettleFn calls SettleTo for each finalized tx in order.
	settleFn := blockstm.V2SettleFn(func(txIdx int, state blockstm.V2TxState) {
		pdb := state.(*ParallelStateDB)
		pdb.SettleTo(finalDB)
	})

	_ = blockstm.ExecuteV2BlockSTM(context.Background(), tasks, env, common.Address{}, workers, nil, settleFn)

	finalRoot := finalDB.IntermediateRoot(true)
	return finalRoot, collectProbes(finalDB, sc.probes), collectStateDBLogs(finalDB)
}

func runExecutorDifferential(t *testing.T, sc exScenario) {
	t.Helper()
	serialRoot, serialProbes, serialLogs := runExecutorSerial(t, sc)
	v2Root, v2Probes, v2Logs := runExecutorV2(t, sc)

	if serialRoot != v2Root {
		t.Fatalf("%s: root mismatch\n  serial = %s\n  v2     = %s",
			sc.name, serialRoot.Hex(), v2Root.Hex())
	}
	if !reflect.DeepEqual(serialProbes, v2Probes) {
		t.Fatalf("%s: probe mismatch\n  serial = %+v\n  v2     = %+v",
			sc.name, serialProbes, v2Probes)
	}
	if !reflect.DeepEqual(serialLogs, v2Logs) {
		t.Fatalf("%s: log mismatch\n  serial = %+v\n  v2     = %+v",
			sc.name, serialLogs, v2Logs)
	}
}

// ---------------------------------------------------------------------------
// Scenarios targeting validation-miss cases
// ---------------------------------------------------------------------------

func exScenarios() []exScenario {
	alice := a(1)
	bob := a(2)
	carol := a(3)

	return []exScenario{
		// ---- Baseline: independent txs, no conflicts ----
		{
			name:  "independent_txs",
			setup: []pdbOp{opAddBalance{alice, u(100)}, opAddBalance{bob, u(100)}, opAddBalance{carol, u(100)}},
			txs: []txScript{
				{sender: alice, ops: []pdbOp{opAddBalance{alice, u(10)}}},
				{sender: bob, ops: []pdbOp{opAddBalance{bob, u(20)}}},
				{sender: carol, ops: []pdbOp{opAddBalance{carol, u(30)}}},
			},
			probes: []probe{
				{kind: "balance", addr: alice}, {kind: "balance", addr: bob}, {kind: "balance", addr: carol},
			},
		},

		// ---- Chain of storage writes: each tx reads prior tx's write ----
		// V2 must detect read-after-write and re-execute downstream txs.
		{
			name:  "storage_linear_chain",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			txs: []txScript{
				{sender: alice, ops: []pdbOp{opSetState{alice, h(1), h(0x11)}}},
				{sender: alice, ops: []pdbOp{opGetState{alice, h(1)}, opSetState{alice, h(1), h(0x22)}}},
				{sender: alice, ops: []pdbOp{opGetState{alice, h(1)}, opSetState{alice, h(1), h(0x33)}}},
			},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},

		// ---- Commutative balance accumulation across many txs ----
		// All deltas must be preserved even if re-execution happens.
		{
			name:  "balance_cumulative",
			setup: []pdbOp{opAddBalance{alice, u(0)}},
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opAddBalance{alice, u(1)}}},
				{sender: a(11), ops: []pdbOp{opAddBalance{alice, u(2)}}},
				{sender: a(12), ops: []pdbOp{opAddBalance{alice, u(3)}}},
				{sender: a(13), ops: []pdbOp{opAddBalance{alice, u(4)}}},
				{sender: a(14), ops: []pdbOp{opAddBalance{alice, u(5)}}},
			},
			probes: []probe{{kind: "balance", addr: alice}}, // expect 15
		},

		// ---- Read-then-write by later tx; earlier tx also writes ----
		// Tests that B (which read X then wrote X+1) sees A's write of X=100
		// and correctly computes X=101 even under parallel scheduling.
		{
			name:  "read_write_dependency",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opSetState{alice, h(1), h(100)}}},
				{sender: a(11), ops: []pdbOp{
					opGetState{alice, h(1)},
					opSetState{alice, h(1), h(101)},
				}},
			},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},

		// ---- Balance transfer chain: Alice → Bob → Carol ----
		{
			name:  "balance_transfer_chain",
			setup: []pdbOp{opAddBalance{alice, u(100)}},
			txs: []txScript{
				{sender: alice, ops: []pdbOp{
					opGetBalance{alice},
					opSubBalance{alice, u(50)},
					opAddBalance{bob, u(50)},
				}},
				{sender: bob, ops: []pdbOp{
					opGetBalance{bob},
					opSubBalance{bob, u(20)},
					opAddBalance{carol, u(20)},
				}},
			},
			probes: []probe{
				{kind: "balance", addr: alice},
				{kind: "balance", addr: bob},
				{kind: "balance", addr: carol},
			},
		},

		// ---- All txs write same key: maximum conflict, maximum re-exec ----
		// Final value must equal the last tx's write.
		{
			name:  "all_conflict_same_slot",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opSetState{alice, h(1), h(0x01)}}},
				{sender: a(11), ops: []pdbOp{opSetState{alice, h(1), h(0x02)}}},
				{sender: a(12), ops: []pdbOp{opSetState{alice, h(1), h(0x03)}}},
				{sender: a(13), ops: []pdbOp{opSetState{alice, h(1), h(0x04)}}},
				{sender: a(14), ops: []pdbOp{opSetState{alice, h(1), h(0x05)}}},
			},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}}, // expect 0x05
		},

		// ---- Cross-address storage writes with reads ----
		{
			name:  "cross_address_reads_writes",
			setup: []pdbOp{opAddBalance{alice, u(1)}, opAddBalance{bob, u(1)}},
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opSetState{alice, h(1), h(0xaa)}, opSetState{bob, h(1), h(0xbb)}}},
				{sender: a(11), ops: []pdbOp{
					opGetState{alice, h(1)},
					opGetState{bob, h(1)},
					opSetState{alice, h(2), h(0xcc)},
				}},
			},
			probes: []probe{
				{kind: "storage", addr: alice, slot: h(1)},
				{kind: "storage", addr: alice, slot: h(2)},
				{kind: "storage", addr: bob, slot: h(1)},
			},
		},

		// ---- Mixed balance + storage conflicts ----
		{
			name:  "mixed_balance_and_storage",
			setup: []pdbOp{opAddBalance{alice, u(100)}, opAddBalance{bob, u(1)}},
			txs: []txScript{
				{sender: alice, ops: []pdbOp{
					opGetBalance{alice},
					opSubBalance{alice, u(10)},
					opSetState{bob, h(1), h(0xaa)},
				}},
				{sender: alice, ops: []pdbOp{
					opGetBalance{alice},
					opGetState{bob, h(1)},
					opSubBalance{alice, u(20)},
					opSetState{bob, h(2), h(0xbb)},
				}},
			},
			probes: []probe{
				{kind: "balance", addr: alice},
				{kind: "storage", addr: bob, slot: h(1)},
				{kind: "storage", addr: bob, slot: h(2)},
			},
		},

		// ---- Nonce chain from same sender ----
		{
			name:  "same_sender_nonce_chain",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			txs: []txScript{
				{sender: alice, ops: []pdbOp{opSetNonce{alice, 1}}},
				{sender: alice, ops: []pdbOp{opGetNonce{alice}, opSetNonce{alice, 2}}},
				{sender: alice, ops: []pdbOp{opGetNonce{alice}, opSetNonce{alice, 3}}},
				{sender: alice, ops: []pdbOp{opGetNonce{alice}, opSetNonce{alice, 4}}},
			},
			probes: []probe{{kind: "nonce", addr: alice}}, // expect 4
		},

		// ---- Stress: many independent txs, high parallelism ----
		{
			name:    "many_independent",
			setup:   []pdbOp{}, // no setup — each tx creates its own addr
			txs:     manyIndependentTxs(20),
			probes:  manyIndependentProbes(20),
			workers: 8,
		},

		// ---- ESTIMATE cleanup: tx's write set shrinks on re-execution ----
		// tx 0 writes slot 1 = 0xaa.
		// tx 1's first incarnation reads slot 1 from base (0x00), so the
		// IfStateEquals{0x00} branch fires and tx 1 writes slot 2 = 0xee.
		// After tx 0 commits, tx 1's read of slot 1 becomes stale → vfail →
		// re-exec. Second incarnation reads slot 1 = 0xaa, the branch is
		// skipped, and tx 1 writes NO slot 2. The ESTIMATE marker left on
		// slot 2 must be cleaned up so tx 2's read sees "not written".
		//
		// tx 2 reads slot 2; the final state must match serial (slot 2 stays
		// at its base value, not whatever tx 1 wrote in incarnation 0).
		{
			name:  "estimate_cleanup_write_set_shrink",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opSetState{alice, h(1), h(0xaa)}}},
				{sender: a(11), ops: []pdbOp{
					opIfStateEquals{alice, h(1), h(0x00), []pdbOp{
						opSetState{alice, h(2), h(0xee)},
					}},
				}},
				{sender: a(12), ops: []pdbOp{opGetState{alice, h(2)}}},
			},
			// After re-exec, tx 1's branch doesn't fire, so slot 2 stays 0.
			probes: []probe{
				{kind: "storage", addr: alice, slot: h(1)},
				{kind: "storage", addr: alice, slot: h(2)},
			},
		},

		// ---- Re-exec causes balance delta change ----
		// tx 0 writes slot 1 = 0xaa. tx 1 reads slot 1; if still 0 it
		// AddBalance(alice, 100), else AddBalance(alice, 1). First incarnation
		// reads 0 → +100; after tx 0 commits, re-exec reads 0xaa → +1. The
		// final delta must be 1, not 100.
		{
			name:  "estimate_cleanup_balance_delta_changes",
			setup: []pdbOp{opAddBalance{alice, u(1000)}},
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opSetState{alice, h(1), h(0xaa)}}},
				{sender: a(11), ops: []pdbOp{
					opIfStateEquals{alice, h(1), h(0x00),
						[]pdbOp{opAddBalance{alice, u(100)}}},
					opIfStateEquals{alice, h(1), h(0xaa),
						[]pdbOp{opAddBalance{alice, u(1)}}},
				}},
			},
			probes: []probe{
				{kind: "balance", addr: alice}, // expect 1001, not 1100
				{kind: "storage", addr: alice, slot: h(1)},
			},
		},

		// ---- Deep chain: tx N reads tx N-1's slot ----
		// Forces a serial re-exec chain through the whole block.
		{
			name:    "deep_rw_chain",
			setup:   []pdbOp{opAddBalance{alice, u(1)}},
			workers: 2,
			txs: []txScript{
				{sender: a(10), ops: []pdbOp{opSetState{alice, h(1), h(1)}}},
				{sender: a(11), ops: []pdbOp{opGetState{alice, h(1)}, opSetState{alice, h(2), h(2)}}},
				{sender: a(12), ops: []pdbOp{opGetState{alice, h(2)}, opSetState{alice, h(3), h(3)}}},
				{sender: a(13), ops: []pdbOp{opGetState{alice, h(3)}, opSetState{alice, h(4), h(4)}}},
				{sender: a(14), ops: []pdbOp{opGetState{alice, h(4)}, opSetState{alice, h(5), h(5)}}},
			},
			probes: []probe{
				{kind: "storage", addr: alice, slot: h(1)},
				{kind: "storage", addr: alice, slot: h(2)},
				{kind: "storage", addr: alice, slot: h(3)},
				{kind: "storage", addr: alice, slot: h(4)},
				{kind: "storage", addr: alice, slot: h(5)},
			},
		},

		// ---- Single tx block: edge of task-array indexing ----
		{
			name:  "single_tx_block",
			setup: []pdbOp{opAddBalance{alice, u(100)}},
			txs: []txScript{
				{sender: alice, ops: []pdbOp{opSubBalance{alice, u(5)}, opAddBalance{bob, u(5)}}},
			},
			probes: []probe{
				{kind: "balance", addr: alice},
				{kind: "balance", addr: bob},
			},
		},

		// ---- Many independent txs at high scale — stresses scheduler ----
		{
			name:    "many_independent_100",
			setup:   []pdbOp{},
			txs:     manyIndependentTxs(100),
			probes:  manyIndependentProbes(100),
			workers: 16,
		},

		// ---- Deeply-conflicting block at high scale ----
		// 50 txs all writing the same slot. Worst-case validation load.
		// Final value should equal the last tx's write on both paths.
		{
			name:    "many_conflicting_50",
			setup:   []pdbOp{opAddBalance{alice, u(1)}},
			txs:     manyConflictingTxs(alice, h(1), 50),
			probes:  []probe{{kind: "storage", addr: alice, slot: h(1)}},
			workers: 8,
		},
	}
}

// manyConflictingTxs produces n scripts all writing the same (addr, slot)
// with increasing values — creates maximum validation contention.
func manyConflictingTxs(addr common.Address, slot common.Hash, n int) []txScript {
	out := make([]txScript, n)
	for i := 0; i < n; i++ {
		out[i] = txScript{
			sender: common.Address{byte(100 + i)},
			ops:    []pdbOp{opSetState{addr, slot, h(uint64(i + 1))}},
		}
	}
	return out
}

func manyIndependentTxs(n int) []txScript {
	out := make([]txScript, n)
	for i := 0; i < n; i++ {
		addr := common.Address{byte(100 + i)}
		out[i] = txScript{sender: addr, ops: []pdbOp{opAddBalance{addr, uint256.NewInt(uint64(i + 1))}}}
	}
	return out
}

func manyIndependentProbes(n int) []probe {
	out := make([]probe, n)
	for i := 0; i < n; i++ {
		out[i] = probe{kind: "balance", addr: common.Address{byte(100 + i)}}
	}
	return out
}

func TestV2ExecutorDifferential(t *testing.T) {
	for _, sc := range exScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			runExecutorDifferential(t, sc)
		})
	}
}
