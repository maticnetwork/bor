package state

import (
	"reflect"
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
// Differential test harness: run a Scenario through the serial StateDB and
// the parallel ParallelStateDB+SettleTo path, assert byte-identical state
// root and probe outcomes.
// ---------------------------------------------------------------------------

// pdbOp is a side-effecting operation applicable to both *StateDB and
// *ParallelStateDB. We take a typed dispatcher rather than vm.StateDB
// because some ops (notably SelfDestructIfNew + CreateContract) differ in
// serial-only test shortcuts.
type pdbOp interface {
	applyTo(sdb sdbIface)
	name() string
}

// sdbIface is the minimal surface both StateDB and ParallelStateDB support
// for differential testing. We can't use vm.StateDB directly (circular
// import); the subset below is sufficient for every scenario below.
type sdbIface interface {
	AddBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int
	SubBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int
	SetBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int
	SetNonce(common.Address, uint64, tracing.NonceChangeReason)
	SetCode(common.Address, []byte, tracing.CodeChangeReason) []byte
	SetState(common.Address, common.Hash, common.Hash) common.Hash
	SelfDestruct(common.Address)
	IsNewContract(common.Address) bool
	CreateAccount(common.Address)
	CreateContract(common.Address)
	AddRefund(uint64)
	SubRefund(uint64)
	SetTransientState(common.Address, common.Hash, common.Hash)
	AddAddressToAccessList(common.Address)
	Snapshot() int
	RevertToSnapshot(int)
}

// probe captures an observable quantity after all ops finish.
type probe struct {
	kind string // "balance", "nonce", "codehash", "storage", "exist", "refund"
	addr common.Address
	slot common.Hash // storage only
}

// scenario is a sequence of operations plus expected probes.
type scenario struct {
	name   string
	setup  []pdbOp // applied to the pre-block state on both paths
	ops    []pdbOp // tx body — executed on serial StateDB directly and on PDB+SettleTo
	probes []probe
}

// ---------------------------------------------------------------------------
// Op implementations
// ---------------------------------------------------------------------------

type opAddBalance struct {
	addr common.Address
	amt  *uint256.Int
}

func (o opAddBalance) applyTo(s sdbIface) {
	s.AddBalance(o.addr, o.amt, tracing.BalanceChangeUnspecified)
}
func (o opAddBalance) name() string { return "AddBalance" }

type opSubBalance struct {
	addr common.Address
	amt  *uint256.Int
}

func (o opSubBalance) applyTo(s sdbIface) {
	s.SubBalance(o.addr, o.amt, tracing.BalanceChangeUnspecified)
}
func (o opSubBalance) name() string { return "SubBalance" }

type opSetBalance struct {
	addr common.Address
	amt  *uint256.Int
}

func (o opSetBalance) applyTo(s sdbIface) {
	s.SetBalance(o.addr, o.amt, tracing.BalanceChangeUnspecified)
}
func (o opSetBalance) name() string { return "SetBalance" }

type opSetNonce struct {
	addr common.Address
	n    uint64
}

func (o opSetNonce) applyTo(s sdbIface) {
	s.SetNonce(o.addr, o.n, tracing.NonceChangeUnspecified)
}
func (o opSetNonce) name() string { return "SetNonce" }

type opSetCode struct {
	addr common.Address
	code []byte
}

func (o opSetCode) applyTo(s sdbIface) {
	s.SetCode(o.addr, o.code, tracing.CodeChangeUnspecified)
}
func (o opSetCode) name() string { return "SetCode" }

type opSetState struct {
	addr      common.Address
	slot, val common.Hash
}

func (o opSetState) applyTo(s sdbIface) {
	s.SetState(o.addr, o.slot, o.val)
}
func (o opSetState) name() string { return "SetState" }

type opSelfDestruct struct{ addr common.Address }

func (o opSelfDestruct) applyTo(s sdbIface) {
	s.SelfDestruct(o.addr)
}
func (o opSelfDestruct) name() string { return "SelfDestruct" }

// opSelfDestructIfNew mirrors what core/vm's opSelfdestruct6780 now does at the
// StateDB level: the same-tx-creation check is a separate IsNewContract query and
// SelfDestruct only flips the destruct flag. It replaces the old
// SelfDestruct6780 op, which combined both and also cleared the balance.
type opSelfDestructIfNew struct{ addr common.Address }

func (o opSelfDestructIfNew) applyTo(s sdbIface) {
	if s.IsNewContract(o.addr) {
		s.SelfDestruct(o.addr)
	}
}
func (o opSelfDestructIfNew) name() string { return "SelfDestructIfNew" }

type opCreateAccount struct{ addr common.Address }

func (o opCreateAccount) applyTo(s sdbIface) {
	s.CreateAccount(o.addr)
}
func (o opCreateAccount) name() string { return "CreateAccount" }

type opCreateContract struct{ addr common.Address }

// applyTo always creates the account first — StateDB.CreateContract panics
// on missing state object, while ParallelStateDB's CreateContract already
// composes CreateAccount. Doing both on serial keeps them equivalent.
func (o opCreateContract) applyTo(s sdbIface) {
	if _, isPDB := s.(*ParallelStateDB); !isPDB {
		s.CreateAccount(o.addr)
	}
	s.CreateContract(o.addr)
}
func (o opCreateContract) name() string { return "CreateContract" }

type opAddRefund struct{ gas uint64 }

func (o opAddRefund) applyTo(s sdbIface) { s.AddRefund(o.gas) }
func (o opAddRefund) name() string       { return "AddRefund" }

type opTransient struct {
	addr      common.Address
	slot, val common.Hash
}

func (o opTransient) applyTo(s sdbIface) {
	s.SetTransientState(o.addr, o.slot, o.val)
}
func (o opTransient) name() string { return "SetTransientState" }

// opGetBalance is a read-producing op — exercises the read-tracking path
// and makes subsequent writer conflicts detectable by V2 validation.
type opGetBalance struct{ addr common.Address }

func (o opGetBalance) applyTo(s sdbIface) {
	if r, ok := s.(interface {
		GetBalance(common.Address) *uint256.Int
	}); ok {
		_ = r.GetBalance(o.addr)
	}
}
func (o opGetBalance) name() string { return "GetBalance" }

type opGetNonce struct{ addr common.Address }

func (o opGetNonce) applyTo(s sdbIface) {
	if r, ok := s.(interface {
		GetNonce(common.Address) uint64
	}); ok {
		_ = r.GetNonce(o.addr)
	}
}
func (o opGetNonce) name() string { return "GetNonce" }

type opGetState struct {
	addr common.Address
	slot common.Hash
}

func (o opGetState) applyTo(s sdbIface) {
	if r, ok := s.(interface {
		GetState(common.Address, common.Hash) common.Hash
	}); ok {
		_ = r.GetState(o.addr, o.slot)
	}
}
func (o opGetState) name() string { return "GetState" }

// opIfStateEquals applies `inner` only when the read slot equals `expect`.
// This is the one dynamic op — it lets a script's write set change across
// incarnations based on what a prior tx wrote. Essential for exercising
// ESTIMATE cleanup (when incarnation N writes a key that incarnation N+1
// does NOT re-write).
type opIfStateEquals struct {
	addr   common.Address
	slot   common.Hash
	expect common.Hash
	inner  []pdbOp
}

func (o opIfStateEquals) applyTo(s sdbIface) {
	r, ok := s.(interface {
		GetState(common.Address, common.Hash) common.Hash
	})
	if !ok {
		return
	}
	if r.GetState(o.addr, o.slot) == o.expect {
		for _, op := range o.inner {
			op.applyTo(s)
		}
	}
}
func (o opIfStateEquals) name() string { return "IfStateEquals" }

// opAddLog appends a log. The harness compares log output between paths.
type opAddLog struct{ log *types.Log }

func (o opAddLog) applyTo(s sdbIface) {
	if r, ok := s.(interface{ AddLog(*types.Log) }); ok {
		// Shallow-copy so both paths get independent log pointers.
		cp := *o.log
		cp.Topics = append([]common.Hash{}, o.log.Topics...)
		cp.Data = append([]byte{}, o.log.Data...)
		r.AddLog(&cp)
	}
}
func (o opAddLog) name() string { return "AddLog" }

// opRefundAdd / opRefundSub exercise the refund counter path.
type opRefundAdd struct{ gas uint64 }

func (o opRefundAdd) applyTo(s sdbIface) { s.AddRefund(o.gas) }
func (o opRefundAdd) name() string       { return "RefundAdd" }

type opRefundSub struct{ gas uint64 }

func (o opRefundSub) applyTo(s sdbIface) { s.SubRefund(o.gas) }
func (o opRefundSub) name() string       { return "RefundSub" }

// opRevertAfter takes a snapshot, runs inner ops, then reverts. Used to
// exercise the journal's revert semantics across all op types.
type opRevertAfter struct{ inner []pdbOp }

func (o opRevertAfter) applyTo(s sdbIface) {
	id := s.Snapshot()
	for _, op := range o.inner {
		op.applyTo(s)
	}
	s.RevertToSnapshot(id)
}
func (o opRevertAfter) name() string { return "RevertAfter" }

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// newDiffStateDB constructs a fresh in-memory StateDB at EmptyRootHash.
func newDiffStateDB(t *testing.T) (*StateDB, Database) {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	db := NewDatabase(tdb, nil)
	sdb, err := New(types.EmptyRootHash, db)
	if err != nil {
		t.Fatalf("new statedb: %v", err)
	}
	return sdb, db
}

// commitAndReopen persists the current state and opens a fresh StateDB at
// the committed root, matching how Bor reopens state between blocks.
func commitAndReopen(t *testing.T, sdb *StateDB, db Database, block uint64) (*StateDB, common.Hash) {
	t.Helper()
	sdb.Finalise(true)
	root, err := sdb.Commit(block, true, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	fresh, err := New(root, db)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return fresh, root
}

// collectProbes reads the probed state from a settled StateDB.
type probeResult struct {
	kind string
	addr common.Address
	slot common.Hash
	val  interface{}
}

// logSummary captures the observable content of a types.Log without the
// thash/blocknum metadata — those are stamped at settlement time and
// diverge between paths in harmless ways (tx hash is zero on both paths,
// but block number defaults differ).
type logSummary struct {
	addr   common.Address
	topics []common.Hash
	data   []byte
}

func collectStateDBLogs(sdb *StateDB) []logSummary {
	// Flatten every bucket — serial and V2 both emit under the default
	// zero thash, but a loop handles any other key too.
	var all []logSummary
	for _, bucket := range sdb.logs {
		for _, l := range bucket {
			topics := make([]common.Hash, len(l.Topics))
			copy(topics, l.Topics)
			all = append(all, logSummary{
				addr:   l.Address,
				topics: topics,
				data:   append([]byte{}, l.Data...),
			})
		}
	}
	return all
}

func collectProbes(sdb *StateDB, probes []probe) []probeResult {
	out := make([]probeResult, 0, len(probes))
	for _, p := range probes {
		pr := probeResult{kind: p.kind, addr: p.addr, slot: p.slot}
		switch p.kind {
		case "balance":
			pr.val = sdb.GetBalance(p.addr).Uint64()
		case "nonce":
			pr.val = sdb.GetNonce(p.addr)
		case "codehash":
			pr.val = sdb.GetCodeHash(p.addr)
		case "code":
			pr.val = append([]byte{}, sdb.GetCode(p.addr)...)
		case "storage":
			pr.val = sdb.GetState(p.addr, p.slot)
		case "exist":
			pr.val = sdb.Exist(p.addr)
		}
		out = append(out, pr)
	}
	return out
}

// runSerial applies setup → commit → ops → intermediate root.
func runSerial(t *testing.T, sc scenario) (common.Hash, []probeResult, []logSummary, uint64) {
	t.Helper()
	sdb, db := newDiffStateDB(t)
	for _, op := range sc.setup {
		op.applyTo(sdb)
	}
	sdb, _ = commitAndReopen(t, sdb, db, 0)
	for _, op := range sc.ops {
		op.applyTo(sdb)
	}
	refund := sdb.GetRefund()
	sdb.Finalise(true)
	root := sdb.IntermediateRoot(true)
	return root, collectProbes(sdb, sc.probes), collectStateDBLogs(sdb), refund
}

// runParallel applies setup → commit, then runs ops through a ParallelStateDB
// and settles onto a sibling StateDB opened at the same committed root.
func runParallel(t *testing.T, sc scenario) (common.Hash, []probeResult, []logSummary, uint64) {
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

	pdb := NewParallelStateDB(0,
		NewSafeBase(baseDB, 1),
		blockstm.NewMVStore(),
		blockstm.NewMVBalanceStore())
	for _, op := range sc.ops {
		op.applyTo(pdb)
	}
	refund := pdb.GetRefund()
	pdb.SettleTo(finalDB)
	finalRoot := finalDB.IntermediateRoot(true)
	return finalRoot, collectProbes(finalDB, sc.probes), collectStateDBLogs(finalDB), refund
}

// runDifferential is the assertion body: serial and parallel must match
// on state root, probes, logs, AND refund counter. Logs + refund catch
// receipt-level drift that an equal state root alone wouldn't expose.
func runDifferential(t *testing.T, sc scenario) {
	t.Helper()
	serialRoot, serialProbes, serialLogs, serialRefund := runSerial(t, sc)
	parallelRoot, parallelProbes, parallelLogs, parallelRefund := runParallel(t, sc)

	if serialRoot != parallelRoot {
		t.Fatalf("%s: root mismatch\n  serial   = %s\n  parallel = %s",
			sc.name, serialRoot.Hex(), parallelRoot.Hex())
	}
	if !reflect.DeepEqual(serialProbes, parallelProbes) {
		t.Fatalf("%s: probe mismatch\n  serial   = %+v\n  parallel = %+v",
			sc.name, serialProbes, parallelProbes)
	}
	if !reflect.DeepEqual(serialLogs, parallelLogs) {
		t.Fatalf("%s: log mismatch\n  serial   = %+v\n  parallel = %+v",
			sc.name, serialLogs, parallelLogs)
	}
	if serialRefund != parallelRefund {
		t.Fatalf("%s: refund mismatch\n  serial   = %d\n  parallel = %d",
			sc.name, serialRefund, parallelRefund)
	}
}

// ---------------------------------------------------------------------------
// Scenarios
// ---------------------------------------------------------------------------

func u(n uint64) *uint256.Int { return uint256.NewInt(n) }
func h(n uint64) common.Hash  { return common.BigToHash(uint256.NewInt(n).ToBig()) }
func a(b byte) common.Address { return common.Address{b} }

func diffScenarios() []scenario {
	alice := a(1)
	bob := a(2)
	carol := a(3)
	return []scenario{
		// ---- Balance ----
		{
			name:   "add_balance_new_addr",
			ops:    []pdbOp{opAddBalance{alice, u(100)}},
			probes: []probe{{kind: "balance", addr: alice}, {kind: "exist", addr: alice}},
		},
		{
			name: "add_then_sub",
			ops: []pdbOp{
				opAddBalance{alice, u(100)},
				opSubBalance{alice, u(30)},
			},
			probes: []probe{{kind: "balance", addr: alice}},
		},
		{
			name:   "set_balance_up",
			setup:  []pdbOp{opAddBalance{alice, u(10)}},
			ops:    []pdbOp{opSetBalance{alice, u(50)}},
			probes: []probe{{kind: "balance", addr: alice}},
		},
		{
			name:   "set_balance_down",
			setup:  []pdbOp{opAddBalance{alice, u(50)}},
			ops:    []pdbOp{opSetBalance{alice, u(10)}},
			probes: []probe{{kind: "balance", addr: alice}},
		},
		{
			name:  "transfer_between_addrs",
			setup: []pdbOp{opAddBalance{alice, u(100)}},
			ops: []pdbOp{
				opSubBalance{alice, u(30)},
				opAddBalance{bob, u(30)},
			},
			probes: []probe{
				{kind: "balance", addr: alice},
				{kind: "balance", addr: bob},
			},
		},

		// ---- Nonce ----
		{
			name:   "nonce_set_new",
			ops:    []pdbOp{opSetNonce{alice, 7}, opAddBalance{alice, u(1)}},
			probes: []probe{{kind: "nonce", addr: alice}},
		},
		{
			name:   "nonce_bump",
			setup:  []pdbOp{opSetNonce{alice, 5}, opAddBalance{alice, u(1)}},
			ops:    []pdbOp{opSetNonce{alice, 6}},
			probes: []probe{{kind: "nonce", addr: alice}},
		},

		// ---- Code ----
		{
			name:   "setcode_new_addr",
			ops:    []pdbOp{opSetCode{alice, []byte{0x60, 0x00}}, opAddBalance{alice, u(1)}},
			probes: []probe{{kind: "codehash", addr: alice}, {kind: "code", addr: alice}},
		},
		{
			name:   "setcode_overwrite",
			setup:  []pdbOp{opSetCode{alice, []byte{0xaa}}, opAddBalance{alice, u(1)}},
			ops:    []pdbOp{opSetCode{alice, []byte{0xbb, 0xcc}}},
			probes: []probe{{kind: "codehash", addr: alice}, {kind: "code", addr: alice}},
		},
		{
			name:   "setcode_empty_resets_hash",
			setup:  []pdbOp{opSetCode{alice, []byte{0xaa}}, opAddBalance{alice, u(1)}},
			ops:    []pdbOp{opSetCode{alice, nil}},
			probes: []probe{{kind: "codehash", addr: alice}},
		},

		// ---- Storage ----
		{
			name:   "sstore_new_slot",
			setup:  []pdbOp{opAddBalance{alice, u(1)}},
			ops:    []pdbOp{opSetState{alice, h(1), h(0xaa)}},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},
		{
			name:   "sstore_overwrite",
			setup:  []pdbOp{opAddBalance{alice, u(1)}, opSetState{alice, h(1), h(0xaa)}},
			ops:    []pdbOp{opSetState{alice, h(1), h(0xbb)}},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},
		{
			name:   "sstore_zero_clears",
			setup:  []pdbOp{opAddBalance{alice, u(1)}, opSetState{alice, h(1), h(0xaa)}},
			ops:    []pdbOp{opSetState{alice, h(1), common.Hash{}}},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},
		{
			name:  "sstore_multi_slots",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			ops: []pdbOp{
				opSetState{alice, h(1), h(0x11)},
				opSetState{alice, h(2), h(0x22)},
				opSetState{alice, h(3), h(0x33)},
			},
			probes: []probe{
				{kind: "storage", addr: alice, slot: h(1)},
				{kind: "storage", addr: alice, slot: h(2)},
				{kind: "storage", addr: alice, slot: h(3)},
			},
		},

		// ---- Self-destruct ----
		{
			name:   "selfdestruct_existing",
			setup:  []pdbOp{opAddBalance{alice, u(100)}},
			ops:    []pdbOp{opSelfDestruct{alice}},
			probes: []probe{{kind: "balance", addr: alice}, {kind: "exist", addr: alice}},
		},
		// Follow opSelfdestruct6780's EVM flow: SubBalance, AddBalance to
		// beneficiary, then the IsNewContract-gated SelfDestruct. SelfDestruct is
		// balance-neutral on both StateDB and ParallelStateDB — the EVM opcode
		// handler owns the SubBalance + AddBalance pair.
		// (PDB used to drain balance on its own here, double-charging the
		// SubBalance and zeroing the contract on the self-beneficiary case
		// from spec-tests stCallCodes/...SuicideEnd; fixed.)
		{
			name: "selfdestruct6780_new_contract",
			ops: []pdbOp{
				opCreateContract{alice},
				opAddBalance{alice, u(5)},
				opSubBalance{alice, u(5)},
				opAddBalance{bob, u(5)},
				opSelfDestructIfNew{alice},
			},
			probes: []probe{
				{kind: "balance", addr: alice},
				{kind: "balance", addr: bob},
				{kind: "exist", addr: alice},
			},
		},
		{
			name:  "selfdestruct6780_existing_contract",
			setup: []pdbOp{opAddBalance{alice, u(5)}},
			ops: []pdbOp{
				opSubBalance{alice, u(5)},
				opAddBalance{bob, u(5)},
				opSelfDestructIfNew{alice},
			},
			probes: []probe{
				{kind: "balance", addr: alice},
				{kind: "balance", addr: bob},
				{kind: "exist", addr: alice},
			},
		},

		// ---- Create ----
		{
			name:   "create_then_addbalance",
			ops:    []pdbOp{opCreateAccount{alice}, opAddBalance{alice, u(10)}},
			probes: []probe{{kind: "exist", addr: alice}, {kind: "balance", addr: alice}},
		},
		{
			name: "create_contract_with_code",
			ops: []pdbOp{
				opCreateContract{alice},
				opSetCode{alice, []byte{0xfe}},
				opAddBalance{alice, u(1)},
			},
			probes: []probe{{kind: "codehash", addr: alice}},
		},

		// ---- Revert ----
		{
			name:  "revert_balance_add",
			setup: []pdbOp{opAddBalance{alice, u(50)}},
			ops: []pdbOp{
				opRevertAfter{inner: []pdbOp{opAddBalance{alice, u(30)}}},
			},
			probes: []probe{{kind: "balance", addr: alice}},
		},
		{
			name:  "revert_storage_write",
			setup: []pdbOp{opAddBalance{alice, u(1)}, opSetState{alice, h(1), h(0xaa)}},
			ops: []pdbOp{
				opRevertAfter{inner: []pdbOp{opSetState{alice, h(1), h(0xbb)}}},
			},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},
		{
			// Note: the SetCode is in ops (not setup) so its journal entry
			// has the correct pre-value to restore on revert. A SetCode in
			// setup would commit to trie and revert semantics on re-opened
			// state aren't equivalent across the two paths.
			name:  "revert_setcode",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			ops: []pdbOp{
				opSetCode{alice, []byte{0xaa}},
				opRevertAfter{inner: []pdbOp{opSetCode{alice, []byte{0xfd}}}},
			},
			probes: []probe{{kind: "codehash", addr: alice}, {kind: "code", addr: alice}},
		},
		{
			name:  "revert_selfdestruct_preserves_balance",
			setup: []pdbOp{opAddBalance{alice, u(42)}},
			ops: []pdbOp{
				opRevertAfter{inner: []pdbOp{opSelfDestruct{alice}}},
			},
			probes: []probe{{kind: "balance", addr: alice}, {kind: "exist", addr: alice}},
		},
		// Regression: a SelfDestruct on an already-destructed account must
		// not be journaled a second time — otherwise its revert un-destructs
		// the account, diverging from StateDB. Found by FuzzV2Differential.
		{
			name: "selfdestruct_then_revert_second_destruct_keeps_destructed",
			ops: []pdbOp{
				opAddBalance{alice, u(1)},
				opSelfDestruct{alice},
				opAddBalance{alice, u(1)},
				opRevertAfter{inner: []pdbOp{
					opAddBalance{alice, u(1)},
					opSelfDestruct{alice},
				}},
			},
			probes: []probe{{kind: "balance", addr: alice}, {kind: "exist", addr: alice}},
		},

		// ---- Mixed / stress ----
		{
			name: "multi_address_storage_and_balance",
			ops: []pdbOp{
				opAddBalance{alice, u(10)},
				opAddBalance{bob, u(20)},
				opAddBalance{carol, u(30)},
				opSetState{alice, h(1), h(0x11)},
				opSetState{bob, h(1), h(0x22)},
				opSetState{carol, h(1), h(0x33)},
			},
			probes: []probe{
				{kind: "balance", addr: alice}, {kind: "balance", addr: bob}, {kind: "balance", addr: carol},
				{kind: "storage", addr: alice, slot: h(1)},
				{kind: "storage", addr: bob, slot: h(1)},
				{kind: "storage", addr: carol, slot: h(1)},
			},
		},

		// ---- Transient storage (doesn't affect root; verify no collateral damage) ----
		{
			name:  "transient_storage_isolated",
			setup: []pdbOp{opAddBalance{alice, u(1)}},
			ops: []pdbOp{
				opTransient{alice, h(1), h(0xaa)},
				opSetState{alice, h(1), h(0xbb)}, // persistent write should still land
			},
			probes: []probe{{kind: "storage", addr: alice, slot: h(1)}},
		},

		// ---- Logs ----
		{
			name: "logs_ordered",
			ops: []pdbOp{
				opAddBalance{alice, u(1)},
				opAddLog{&types.Log{Address: alice, Topics: []common.Hash{h(0x11)}, Data: []byte{0x01}}},
				opAddLog{&types.Log{Address: bob, Topics: []common.Hash{h(0x22), h(0x33)}, Data: []byte{0x02, 0x03}}},
			},
			probes: []probe{{kind: "balance", addr: alice}},
		},
		// Logs emitted inside a reverted snapshot must NOT survive.
		{
			name: "logs_reverted",
			ops: []pdbOp{
				opAddBalance{alice, u(1)},
				opAddLog{&types.Log{Address: alice, Topics: []common.Hash{h(0x01)}}},
				opRevertAfter{inner: []pdbOp{
					opAddLog{&types.Log{Address: alice, Topics: []common.Hash{h(0x99)}}},
				}},
				opAddLog{&types.Log{Address: alice, Topics: []common.Hash{h(0x02)}}},
			},
			probes: []probe{{kind: "balance", addr: alice}},
		},

		// ---- Refund counter ----
		{
			name: "refund_add_sub",
			ops: []pdbOp{
				opRefundAdd{100},
				opRefundSub{30},
			},
			probes: []probe{}, // no state probes — harness compares refund directly
		},
		// Refund changes inside a reverted snapshot must revert too.
		{
			name: "refund_reverted",
			ops: []pdbOp{
				opRefundAdd{50},
				opRevertAfter{inner: []pdbOp{
					opRefundAdd{999},
					opRefundSub{25},
				}},
				opRefundAdd{10},
			},
			probes: []probe{}, // final refund must be 60 on both paths
		},
	}
}

func TestV2Differential(t *testing.T) {
	for _, sc := range diffScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			runDifferential(t, sc)
		})
	}
}
