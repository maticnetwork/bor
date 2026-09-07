package state

import (
	"math/big"
	"slices"
	"sort"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// ---------------------------------------------------------------------------
// Read descriptors for BlockSTM validation
// ---------------------------------------------------------------------------

// StoreReadDesc tracks a single MVStore read for validation.
// Kept small (~88 bytes) to reduce allocation pressure — store reads
// dominate the read set (90%+ of entries).
type StoreReadDesc struct {
	Key       blockstm.Key // 54 bytes
	WriterIdx int          // txIdx of writer (-1 = base)
	WriterInc int          // incarnation of writer
	StoreVal  interface{}  // actual value read (for value-based validation)
	// ExactWriter disables the value-equality validation fallback for this
	// read. Set when the winning writer's block-order position — not just its
	// value — determined the read result: existence/ordering markers
	// (CreatePath/SuicidePath) and any read resolved relative to a prior
	// SELFDESTRUCT (writerIdx vs suicideIdx). For those reads a different
	// writer producing an equal value can still flip the result (a reordered
	// metamorphic CREATE2/SELFDESTRUCT moves the writer across the destruction
	// boundary), so re-validation must require the same writer/incarnation.
	ExactWriter bool
}

// BalReadDesc tracks a balance delta read for validation.
// Deduplicated per address, so typically only a handful per tx.
type BalReadDesc struct {
	Addr   common.Address
	BalAdd uint256.Int
	BalSub uint256.Int
}

// ---------------------------------------------------------------------------
// ParallelStateDB — implements vm.StateDB without stateObject in MVHashMap
// ---------------------------------------------------------------------------

type BalanceOp struct {
	Addr   common.Address
	Amount uint256.Int
	IsAdd  bool
}

// FeeData holds deferred fee settlement information.
type FeeData struct {
	FeeBurnt             *big.Int
	FeeTipped            *big.Int
	BurntContractAddress common.Address
	SenderInitBalance    *big.Int
	BalancesApplied      bool // true if fee burn+tip already applied during execution
}

// TransferLogFn generates a transfer log during settlement.
type TransferLogFn func(db *StateDB, sender, recipient common.Address, amount, input1, input2, output1, output2 *big.Int)

type TransferRecord struct {
	Sender        common.Address
	Recipient     common.Address
	Amount        uint256.Int
	LogIdx        int // position in the logs slice where this transfer log should be inserted
	BalanceOpsIdx int // index in BalanceOps where this transfer's SubBalance appears
}

type ParallelStateDB struct {
	TxIndex     int
	Incarnation int                      // bumped on re-execution for validation
	base        *SafeBase                // thread-safe pre-block state reads
	rawBase     *StateDB                 // raw base for Witness only
	store       *blockstm.MVStore        // shared versioned values
	bals        *blockstm.MVBalanceStore // shared balance deltas

	// Per-tx local writes (for self-read and settlement)
	localNonces  map[common.Address]uint64
	localStorage map[common.Address]map[common.Hash]common.Hash
	localCode    map[common.Address][]byte

	// Per-tx local balance deltas (for self-read within same tx)
	localBalAdd map[common.Address]*uint256.Int
	localBalSub map[common.Address]*uint256.Int

	// Ordered balance operations for settlement replay
	BalanceOps []BalanceOp

	// Account tracking
	created     map[common.Address]bool
	destructed  map[common.Address]bool
	newContract map[common.Address]bool

	// EVM state — access list and transient storage are the serial StateDB
	// types so both execution paths share EIP-2930/1153 semantics.
	refund           uint64
	accessList       *accessList
	transientStorage transientStorage
	logs             []*types.Log
	logSize          uint
	preimages        map[common.Hash][]byte

	// Snapshot/revert
	journalEntries []parallelJournalEntry
	validRevisions []parallelRevision
	nextRevisionId int

	// Read/write tracking for BlockSTM validation
	trackReads  bool
	StoreReads  []StoreReadDesc
	BalReads    []BalReadDesc
	WriteKeys   []blockstm.Key
	BalAddrs    []common.Address
	balAddrSet  map[common.Address]bool // dedup for BalReads
	balWriteSet map[common.Address]bool // dedup for BalAddrs (O(1) writes)

	// Per-key pipelining: block until a writer tx has produced a value.
	// WaitForTx waits only until the writer's execution has flushed to MVStore;
	// used during validation to re-read a fresh value past an ESTIMATE entry.
	// WaitForFinal additionally waits for the writer to be validated; used during
	// execution so a tx never observes a value the writer later abandons.
	WaitForTx    func(writerIdx int)
	WaitForFinal func(writerIdx int)

	// Deferred MVStore writes: when true, WriteInc calls during execution are
	// skipped. All writes are flushed to MVStore at the end via FlushToMVStore().
	// This ensures concurrent readers only see FINAL values (no intermediate
	// reentrancy guard writes), enabling safe non-blocking re-execution dispatch.
	DeferMVWrites bool

	// Pre-computed sender nonces for same-sender chain ordering.
	SenderNonces map[common.Address]uint64

	// Per-tx cache for balance delta reads
	balCache map[common.Address]*[2]uint256.Int // [0]=add, [1]=sub

	// Per-tx cache for priorDestructedAt — avoids repeated SuicidePath
	// lookups across getters. The cached value is the tx index of the most
	// recent destructor (or -1 for no destruction).
	destructedCache map[common.Address]int
	destructedSeen  map[common.Address]struct{}

	// Per-tx cache for GetCommittedState — ensures SSTORE original is stable
	committedCache map[stateKey]common.Hash

	// Per-tx cache for CodePath reads — ensures repeated GetCode/GetCodeHash of
	// the same address within one tx observe ONE consistent value. The EVM
	// resolves a 7702-delegated account via two separate GetCode(addr) reads
	// (resolveCode + resolveCodeHash); without snapshot isolation a concurrent
	// writer between them can make them follow different delegation targets,
	// building a Contract whose Code and CodeHash disagree (poisoning the shared
	// jumpdest cache). Mirrors balCache/committedCache/destructedCache.
	codeReadCache map[common.Address]codeKeyRead

	// Recorded transfers for log generation during settlement
	Transfers []TransferRecord

	// Callbacks for transfer/fee log generation during settlement
	TransferLogFn TransferLogFn // set by executor: generates transfer log
	FeeLogFn      TransferLogFn // set by executor: generates fee transfer log

	// Deferred fee settlement data
	FeeData  *FeeData
	Coinbase common.Address // block coinbase for fee settlement
	Sender   common.Address // tx sender for fee transfer log

	// Execution result (captured from ApplyMessage for receipt building)
	UsedGas    uint64
	ExecFailed bool
	Panicked   bool // true if execution panicked (always fails validation)
	// ExecErr is set when ApplyMessage returns a consensus-level error
	// (e.g. invalid nonce, insufficient upfront gas, intrinsic gas underflow,
	// blob fork-gating violation). Such a tx must NOT be settled; the caller
	// must abort the block and surface the error like the serial path does.
	ExecErr error
	// BaseReadErr records the first base-state read failure THIS incarnation
	// observed (missing trie node, absent code). Reset clears it, so on the
	// final ParallelStateDB of a tx it reflects only the incarnation that
	// settled: a speculative incarnation chasing stale values may legitimately
	// read outside the canonical state (fatal on witness-backed replay, where
	// such nodes simply don't exist) and is then invalidated and re-executed —
	// only an error observed by the SETTLED incarnation means a zero-ish read
	// reached consensus state, and only that must abort the block.
	BaseReadErr error
}

// noteBaseReadErr records the first base read failure of this incarnation.
func (s *ParallelStateDB) noteBaseReadErr(err error) {
	if err != nil && s.BaseReadErr == nil {
		s.BaseReadErr = err
	}
}

type parallelRevision struct {
	id            int
	journalIdx    int
	balanceOpsIdx int // length of BalanceOps at snapshot time
	logsIdx       int // length of logs at snapshot time
	transfersIdx  int // length of Transfers at snapshot time
}

// parallelJournalEntry and the revert* methods live in
// parallel_statedb_journal.go; the jk* kind constants live there too.

func NewParallelStateDB(txIndex int, base *SafeBase, store *blockstm.MVStore, bals *blockstm.MVBalanceStore) *ParallelStateDB {
	return &ParallelStateDB{
		TxIndex:          txIndex,
		base:             base,
		rawBase:          base.DB,
		store:            store,
		bals:             bals,
		localNonces:      make(map[common.Address]uint64, 2),
		localStorage:     make(map[common.Address]map[common.Hash]common.Hash, 4),
		localCode:        make(map[common.Address][]byte, 1),
		localBalAdd:      make(map[common.Address]*uint256.Int, 4),
		localBalSub:      make(map[common.Address]*uint256.Int, 4),
		created:          make(map[common.Address]bool, 1),
		destructed:       make(map[common.Address]bool, 1),
		newContract:      make(map[common.Address]bool, 1),
		accessList:       newAccessList(),
		transientStorage: newTransientStorage(),
		preimages:        make(map[common.Hash][]byte, 1),
	}
}

// Reset reinitializes a ParallelStateDB for reuse with a new transaction.
// Clears all maps without deallocating — avoids 11+ map allocations per tx.
func (s *ParallelStateDB) Reset(txIndex int, base *SafeBase, store *blockstm.MVStore, bals *blockstm.MVBalanceStore) {
	s.TxIndex = txIndex
	s.Incarnation = 0
	s.base = base
	s.rawBase = base.DB
	s.store = store
	s.bals = bals

	clear(s.localNonces)
	clear(s.localStorage)
	clear(s.localCode)
	clear(s.localBalAdd)
	clear(s.localBalSub)
	clear(s.created)
	clear(s.destructed)
	clear(s.newContract)
	clear(s.transientStorage)
	clear(s.preimages)

	s.BalanceOps = s.BalanceOps[:0]
	s.refund = 0
	clear(s.accessList.addresses)
	s.accessList.slots = s.accessList.slots[:0]
	s.logs = s.logs[:0]
	s.logSize = 0
	s.journalEntries = s.journalEntries[:0]
	s.validRevisions = s.validRevisions[:0]
	s.nextRevisionId = 0
	s.trackReads = false
	s.StoreReads = s.StoreReads[:0]
	s.BalReads = s.BalReads[:0]
	s.WriteKeys = s.WriteKeys[:0]
	s.BalAddrs = s.BalAddrs[:0]
	// clear() is a no-op on nil maps, so balAddrSet/balWriteSet stay nil
	// after the first Reset on a fresh PDB and are allocated by
	// EnableReadTracking. On recycled PDBs the existing maps are reused.
	clear(s.balAddrSet)
	clear(s.balWriteSet)
	s.WaitForTx = nil
	s.WaitForFinal = nil
	s.SenderNonces = nil
	s.balCache = nil
	s.destructedCache = nil
	s.destructedSeen = nil
	s.committedCache = nil
	s.codeReadCache = nil
	s.Transfers = s.Transfers[:0]
	s.FeeData = nil
	s.Coinbase = common.Address{}
	s.Sender = common.Address{}
	s.UsedGas = 0
	s.ExecFailed = false
	s.Panicked = false
	s.ExecErr = nil
	s.BaseReadErr = nil
	s.TransferLogFn = nil
	s.FeeLogFn = nil
}

// ---------- MVStore read with suspension ----------

// readStoreWait reads from MVStore with per-key pipelining.
//
// When encountering an ESTIMATE entry (writer being re-executed), it
// spin-waits on that specific key until the writer re-writes it (DONE)
// or the entry is cleaned up. This enables pipelining:
//
//	tx0 |__prework__|__SSTORE K__|__postwork__|
//	tx1 |__prework__|__spin K____|__SLOAD K___|__postwork__|
//
// For DONE entries from not-yet-validated writers, the value is returned
// immediately — it's the writer's actual SSTORE output. Validation
// catches stale reads if the writer is later invalidated.
func (s *ParallelStateDB) readStoreWait(key blockstm.Key) (interface{}, int, int, bool) {
	for {
		// Atomic read of value + estimate flag — prevents race between
		// reading the value and querying the writer's commit state.
		val, writerIdx, writerInc, found, isEst := s.store.ReadVersionFull(key, s.TxIndex)
		if !found || writerIdx < 0 {
			return val, writerIdx, writerInc, found
		}
		if !isEst {
			// COMMITTED: value is final.
			return val, writerIdx, writerInc, found
		}
		// ESTIMATE: writer is being re-executed.
		if s.handleEstimate(key, writerIdx) {
			continue
		}
		return nil, -1, 0, false
	}
}

// codeKeyRead is a cached CodePath readStoreWait result (see codeReadCache).
type codeKeyRead struct {
	val       interface{}
	writerIdx int
	writerInc int
	found     bool
}

// readCodeKey reads addr's CodePath via readStoreWait, cached per-tx so that the
// EVM's two 7702-resolution reads (resolveCode + resolveCodeHash, each a
// GetCode(addr)) observe ONE consistent value. Without this, a concurrent writer
// landing between the two reads can redirect them to different delegation
// targets, yielding a Contract whose Code and CodeHash disagree.
func (s *ParallelStateDB) readCodeKey(addr common.Address, codeKey blockstm.Key) (interface{}, int, int, bool) {
	if s.codeReadCache != nil {
		if c, ok := s.codeReadCache[addr]; ok {
			return c.val, c.writerIdx, c.writerInc, c.found
		}
	}
	val, writerIdx, writerInc, found := s.readStoreWait(codeKey)
	if s.codeReadCache == nil {
		s.codeReadCache = make(map[common.Address]codeKeyRead, 2)
	}
	s.codeReadCache[addr] = codeKeyRead{val: val, writerIdx: writerIdx, writerInc: writerInc, found: found}
	return val, writerIdx, writerInc, found
}

// handleEstimate decides what readStoreWait should do when it observes
// an ESTIMATE entry. Returns true if the loop should retry (re-exec
// scenario), false if the caller should fall through to the base state
// (first execution / no WaitForFinal).
func (s *ParallelStateDB) handleEstimate(key blockstm.Key, writerIdx int) bool {
	if s.Incarnation == 0 || s.WaitForFinal == nil {
		return false
	}
	s.WaitForFinal(writerIdx)
	if s.store.IsEstimate(key, writerIdx) {
		s.store.Delete(key, writerIdx)
	}
	return true
}

// ---------- Read/write tracking for BlockSTM ----------

// EnableReadTracking enables read set recording for BlockSTM validation.
// Slices are reset to length 0 (preserving backing arrays); maps are
// allocated on first call and cleared in place on subsequent calls so a
// recycled PDB doesn't reallocate them per tx.
func (s *ParallelStateDB) EnableReadTracking() {
	s.trackReads = true
	if s.StoreReads == nil {
		s.StoreReads = make([]StoreReadDesc, 0, 256)
	} else {
		s.StoreReads = s.StoreReads[:0]
	}
	if s.BalReads == nil {
		s.BalReads = make([]BalReadDesc, 0, 8)
	} else {
		s.BalReads = s.BalReads[:0]
	}
	if s.WriteKeys == nil {
		s.WriteKeys = make([]blockstm.Key, 0, 32)
	} else {
		s.WriteKeys = s.WriteKeys[:0]
	}
	if s.BalAddrs == nil {
		s.BalAddrs = make([]common.Address, 0, 8)
	} else {
		s.BalAddrs = s.BalAddrs[:0]
	}
	if s.balAddrSet == nil {
		s.balAddrSet = make(map[common.Address]bool)
	} else {
		clear(s.balAddrSet)
	}
	if s.balWriteSet == nil {
		s.balWriteSet = make(map[common.Address]bool)
	} else {
		clear(s.balWriteSet)
	}
}

func (s *ParallelStateDB) recordStoreRead(key blockstm.Key, writerIdx, writerInc int, val interface{}) {
	s.recordStoreReadEx(key, writerIdx, writerInc, val, false)
}

// recordStoreReadEx records a read, optionally marking it ExactWriter so
// validation cannot accept a different writer by value equality. See
// StoreReadDesc.ExactWriter.
func (s *ParallelStateDB) recordStoreReadEx(key blockstm.Key, writerIdx, writerInc int, val interface{}, exact bool) {
	if !s.trackReads {
		return
	}
	s.StoreReads = append(s.StoreReads, StoreReadDesc{Key: key, WriterIdx: writerIdx, WriterInc: writerInc, StoreVal: val, ExactWriter: exact})
}

func (s *ParallelStateDB) recordBalanceRead(addr common.Address, add, sub uint256.Int) {
	if !s.trackReads {
		return
	}
	if s.balAddrSet[addr] {
		return
	}
	s.balAddrSet[addr] = true
	s.BalReads = append(s.BalReads, BalReadDesc{Addr: addr, BalAdd: add, BalSub: sub})
}

func (s *ParallelStateDB) recordWrite(key blockstm.Key) {
	if !s.trackReads {
		return
	}
	s.WriteKeys = append(s.WriteKeys, key)
}

func (s *ParallelStateDB) recordBalWrite(addr common.Address) {
	if !s.trackReads {
		return
	}
	if s.balWriteSet[addr] {
		return
	}
	s.balWriteSet[addr] = true
	s.BalAddrs = append(s.BalAddrs, addr)
}

// valuesEqual, ValidateResult/ValidationDiag types, Validate /
// ValidateCategory / ValidateDetailed, the validate* helpers,
// storeReadMatches, storeReadFailCategory, and DiagnoseValidation live
// in parallel_statedb_validate.go.

// SetDeferMVWrites enables/disables deferred MVStore writes.
func (s *ParallelStateDB) SetDeferMVWrites(defer_ bool) {
	s.DeferMVWrites = defer_
}

// FlushToMVStore writes all local state to MVStore in one batch.
// Called after execution completes when DeferMVWrites is true.
// This ensures concurrent readers only see FINAL values.
//
// Panicked txs hold partial / inconsistent state — flushing it would pollute
// MVStore and trigger cascading vfails on downstream txs. Skip flush; the
// settle path will refuse to commit a panicked PDB and propagate an error.
func (s *ParallelStateDB) FlushToMVStore() {
	if s.Panicked {
		return
	}
	for addr, nonce := range s.localNonces {
		s.store.WriteInc(blockstm.NewSubpathKey(addr, NoncePath), s.TxIndex, s.Incarnation, nonce)
	}
	for addr, slots := range s.localStorage {
		for key, value := range slots {
			s.store.WriteInc(blockstm.NewStateKey(addr, key), s.TxIndex, s.Incarnation, value)
		}
	}
	for addr, code := range s.localCode {
		s.store.WriteInc(blockstm.NewSubpathKey(addr, CodePath), s.TxIndex, s.Incarnation, code)
	}
	for addr := range s.created {
		s.store.WriteInc(blockstm.NewSubpathKey(addr, CreatePath), s.TxIndex, s.Incarnation, true)
	}
	// Publish self-destructs so later txs see the account as non-existent.
	// Without this, pre-EIP-6780 SELFDESTRUCT in tx A is invisible to tx B's
	// parallel reads, and B can resurrect storage / code on a destroyed
	// account at settle time.
	for addr := range s.destructed {
		s.store.WriteInc(blockstm.NewSubpathKey(addr, SuicidePath), s.TxIndex, s.Incarnation, true)
	}
	s.flushBalanceDeltas()
}

// flushBalanceDeltas writes the tx's net add/sub for each address with a
// single atomic WriteDelta call, preventing concurrent readers from
// observing a half-flushed entry (add written, sub still pending).
func (s *ParallelStateDB) flushBalanceDeltas() {
	balAddrs := make(map[common.Address]struct{}, len(s.localBalAdd)+len(s.localBalSub))
	for addr := range s.localBalAdd {
		balAddrs[addr] = struct{}{}
	}
	for addr := range s.localBalSub {
		balAddrs[addr] = struct{}{}
	}
	for addr := range balAddrs {
		add := s.localBalAdd[addr]
		sub := s.localBalSub[addr]
		if (add != nil && !add.IsZero()) || (sub != nil && !sub.IsZero()) {
			s.bals.WriteDelta(addr, s.TxIndex, add, sub)
		}
	}
}

// MarkEstimate marks all MVStore entries as ESTIMATE and zeros balance
// deltas. ESTIMATE entries remain as dependency markers — readers that
// encounter them spin-wait for the re-execution's SSTORE.
func (s *ParallelStateDB) MarkEstimate() {
	s.store.MarkEstimate(s.TxIndex, s.WriteKeys)
	s.bals.ZeroDelta(s.TxIndex, s.BalAddrs)
}

// CleanupEstimate removes entries still marked ESTIMATE after re-execution
// (keys the new incarnation didn't write) and stale balance entries.
func (s *ParallelStateDB) CleanupEstimate(oldWriteKeys []blockstm.Key, oldBalAddrs []common.Address) {
	s.store.CleanupEstimate(s.TxIndex, oldWriteKeys)
	// Remove balance entries that existed before but not in the new incarnation
	newBalSet := make(map[common.Address]bool, len(s.BalAddrs))
	for _, a := range s.BalAddrs {
		newBalSet[a] = true
	}
	for _, a := range oldBalAddrs {
		if !newBalSet[a] {
			s.bals.DeleteSingle(a, s.TxIndex)
		}
	}
}

// GetWriteKeys returns a copy of the current WriteKeys.
func (s *ParallelStateDB) GetWriteKeys() []blockstm.Key {
	result := make([]blockstm.Key, len(s.WriteKeys))
	copy(result, s.WriteKeys)
	return result
}

// GetBalAddrs returns a copy of the current BalAddrs.
func (s *ParallelStateDB) GetBalAddrs() []common.Address {
	result := make([]common.Address, len(s.BalAddrs))
	copy(result, s.BalAddrs)
	return result
}

// ---------- Existence ----------

// priorDestructedAt returns the block-order tx index of the most recent
// SELFDESTRUCT(addr) by a prior tx in this block, or -1 if no such write
// exists. Reads are recorded for validation so re-execution catches stale
// destruction state.
//
// Cached per-tx: this is consulted on every Exist/GetCode/GetState/GetNonce/
// GetCodeHash call, and the bloom-filter fast path in MVStore handles the
// common no-destructions case. The cache prevents the first read on each
// address from being repeated across the four getters.
//
// The destructed signal alone is not sufficient to short-circuit reads —
// a later same-block tx can recreate addr (CREATE2 / value transfer). The
// caller must compare this index against priorCreatedAt and the latest
// MVStore writer for the specific path being read.
func (s *ParallelStateDB) priorDestructedAt(addr common.Address) int {
	if s.destructedCache != nil {
		if v, ok := s.destructedCache[addr]; ok {
			return v
		}
	}
	suicideKey := blockstm.NewSubpathKey(addr, SuicidePath)
	val, writerIdx, writerInc, found := s.readStoreWait(suicideKey)
	idx := -1
	if found {
		idx = writerIdx
		if _, seen := s.destructedSeen[addr]; !seen {
			s.recordStoreReadEx(suicideKey, writerIdx, writerInc, val, true)
		}
	} else if _, seen := s.destructedSeen[addr]; !seen {
		s.recordStoreRead(suicideKey, -1, 0, nil)
	}
	if s.destructedCache == nil {
		s.destructedCache = make(map[common.Address]int, 1)
		s.destructedSeen = make(map[common.Address]struct{}, 1)
	}
	s.destructedCache[addr] = idx
	s.destructedSeen[addr] = struct{}{}
	return idx
}

// priorCreatedAt returns the tx index of the most recent CREATE/CREATE2/
// SetCode-driven CreateAccount on addr, or -1 if no such write exists.
// Used together with priorDestructedAt to determine whether a prior
// SELFDESTRUCT was followed by recreation.
func (s *ParallelStateDB) priorCreatedAt(addr common.Address) int {
	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	val, writerIdx, writerInc, found := s.readStoreWait(createKey)
	if found {
		s.recordStoreReadEx(createKey, writerIdx, writerInc, val, true)
		return writerIdx
	}
	s.recordStoreRead(createKey, -1, 0, nil)
	return -1
}

func (s *ParallelStateDB) Exist(addr common.Address) bool {
	if s.created[addr] {
		return true
	}
	// Note: do NOT short-circuit on s.destructed[addr]. SELFDESTRUCT only
	// takes effect at tx finalization (cross-tx visibility is published as
	// a SuicidePath write at FlushToMVStore time and read here via
	// priorDestructedAt). Within the current tx, the account remains
	// visible — matching serial StateDB.Exist, whose docstring is explicit
	// that it "returns true for self-destructed accounts within the current
	// transaction." A premature false here causes a parent that re-calls a
	// just-self-destructed callee in the same tx to see no account, which
	// diverges from the EVM and the serial processor.
	//
	// Determine the relative ordering of any prior destruction and any
	// prior creation. A monotonic "destructed → return false" check is
	// wrong: a later same-block tx can recreate addr via CREATE2 or by
	// implicit balance transfer, and those reads must see the recreated
	// account, not the tombstone.
	suicideIdx := s.priorDestructedAt(addr)
	createIdx := s.priorCreatedAt(addr)
	if createIdx > suicideIdx {
		// Most recent action was creation — addr exists.
		return true
	}
	if suicideIdx >= 0 {
		// Most recent action was destruction. The account is gone unless
		// a later tx implicitly recreated it via AddBalance (value
		// transfer doesn't touch CreatePath, only MVBalanceStore) or via a
		// bare nonce bump (SetNonce writes NoncePath, not CreatePath, so it
		// lands here rather than the createIdx branch above). GetBalance /
		// GetNonce record their own reads so validation catches drift.
		if !s.GetBalance(addr).IsZero() {
			return true
		}
		if s.GetNonce(addr) > 0 {
			return true
		}
		return false
	}
	// No prior creation or destruction. Fall through to base + balance +
	// nonce check (handles base-state accounts and addresses made to exist
	// by a prior tx's value transfer or nonce bump).
	exists, berr := s.base.Exist(addr)
	s.noteBaseReadErr(berr)
	if exists {
		return true
	}
	if !s.GetBalance(addr).IsZero() {
		return true
	}
	// Serial StateDB.Exist is getStateObject()!=nil, and an account with
	// nonce>0 is not empty — so it exists. ParallelStateDB historically
	// omitted this nonce check: an account made to exist purely by a prior
	// in-block sender nonce increment (SetNonce writes NoncePath but NOT
	// CreatePath) was reported absent. That flipped EIP-7702
	// applyAuthorization's Exist-gated 12500 refund and the new-account CALL
	// surcharge, over-charging gas and splitting from serial. (A 7702
	// delegation clear is not this case: it also writes CreatePath because
	// SetCode calls CreateAccount, so it returns at the createIdx branch
	// above.) GetNonce records the read so validation now catches drift.
	if s.GetNonce(addr) > 0 {
		return true
	}
	return false
}

func (s *ParallelStateDB) Empty(addr common.Address) bool {
	if !s.Exist(addr) {
		return true
	}
	return s.GetNonce(addr) == 0 && s.GetBalance(addr).IsZero() && s.GetCodeHash(addr) == types.EmptyCodeHash
}

// ---------- Balance (commutative) ----------

func (s *ParallelStateDB) GetBalance(addr common.Address) *uint256.Int {
	add, sub := s.priorBalanceDeltas(addr)
	s.recordBalanceRead(addr, add, sub)

	baseBal, berr := s.base.GetBalance(addr)
	s.noteBaseReadErr(berr)
	result := new(uint256.Int).Set(baseBal)
	result.Add(result, &add)
	result.Sub(result, &sub)
	if a := s.localBalAdd[addr]; a != nil {
		result.Add(result, a)
	}
	if su := s.localBalSub[addr]; su != nil {
		result.Sub(result, su)
	}
	return result
}

// priorBalanceDeltas returns the cumulative (add, sub) deltas for addr
// from prior txs in the block, cached per-address within this tx.
func (s *ParallelStateDB) priorBalanceDeltas(addr common.Address) (add, sub uint256.Int) {
	if s.balCache != nil {
		if c, ok := s.balCache[addr]; ok {
			return c[0], c[1]
		}
	}
	add, sub = s.bals.ReadDelta(addr, s.TxIndex)
	if s.balCache == nil {
		s.balCache = make(map[common.Address]*[2]uint256.Int)
	}
	s.balCache[addr] = &[2]uint256.Int{add, sub}
	return
}

func (s *ParallelStateDB) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkBalance, flags: 1, addr: addr, amt: *amount})
	// Track the address in BalAddrs (for later MarkEstimate / cleanup).
	// FlushToMVStore commits the accumulated delta in a single WriteDelta.
	s.recordBalWrite(addr)
	if s.localBalAdd[addr] == nil {
		s.localBalAdd[addr] = new(uint256.Int)
	}
	s.localBalAdd[addr].Add(s.localBalAdd[addr], amount)
	s.BalanceOps = append(s.BalanceOps, BalanceOp{Addr: addr, Amount: *amount, IsAdd: true})
	return uint256.Int{}
}

func (s *ParallelStateDB) SubBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkBalance, addr: addr, amt: *amount})
	s.recordBalWrite(addr)
	if s.localBalSub[addr] == nil {
		s.localBalSub[addr] = new(uint256.Int)
	}
	s.localBalSub[addr].Add(s.localBalSub[addr], amount)
	s.BalanceOps = append(s.BalanceOps, BalanceOp{Addr: addr, Amount: *amount, IsAdd: false})
	return uint256.Int{}
}

func (s *ParallelStateDB) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) uint256.Int {
	prev := s.GetBalance(addr)
	if amount.Gt(prev) {
		diff := new(uint256.Int).Sub(amount, prev)
		return s.AddBalance(addr, diff, reason)
	}
	diff := new(uint256.Int).Sub(prev, amount)
	return s.SubBalance(addr, diff, reason)
}

// ---------- Nonce (versioned) ----------

func (s *ParallelStateDB) GetNonce(addr common.Address) uint64 {
	if n, ok := s.localNonces[addr]; ok {
		return n
	}
	// Pre-computed sender nonce: deterministic, no MVStore lookup needed.
	if n, ok := s.SenderNonces[addr]; ok {
		return n
	}
	suicideIdx := s.priorDestructedAt(addr)
	nonceKey := blockstm.NewSubpathKey(addr, NoncePath)
	if val, writerIdx, writerInc, found := s.readStoreWait(nonceKey); found {
		s.recordStoreReadEx(nonceKey, writerIdx, writerInc, val, suicideIdx >= 0)
		// Only honor the nonce write if it landed AFTER the destruction.
		// Otherwise the destruction wiped it.
		if writerIdx > suicideIdx {
			return val.(uint64)
		}
		return 0
	}
	if suicideIdx >= 0 {
		// Destroyed and no later writer → 0; record so a later writer invalidates.
		s.recordStoreRead(nonceKey, -1, 0, uint64(0))
		return 0
	}
	baseNonce, berr := s.base.GetNonce(addr)
	s.noteBaseReadErr(berr)
	s.recordStoreRead(nonceKey, -1, 0, baseNonce)
	return baseNonce
}

func (s *ParallelStateDB) SetNonce(addr common.Address, nonce uint64, reason tracing.NonceChangeReason) {
	prev, had := s.localNonces[addr]
	var flags uint8
	if had {
		flags = 1
	}
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkNonce, flags: flags, addr: addr, prevU: prev})
	s.localNonces[addr] = nonce
	nonceKey := blockstm.NewSubpathKey(addr, NoncePath)
	// Nonces are always deferred to FlushToMVStore regardless of DeferMVWrites:
	// per optimization log H3, publishing intermediate nonces during CREATE /
	// CALL caused more conflict-induced re-executions than it prevented.
	s.recordWrite(nonceKey)
}

// ---------- Code (versioned) ----------

func (s *ParallelStateDB) GetCode(addr common.Address) []byte {
	if code, ok := s.localCode[addr]; ok {
		return code
	}
	suicideIdx := s.priorDestructedAt(addr)
	codeKey := blockstm.NewSubpathKey(addr, CodePath)
	if val, writerIdx, writerInc, found := s.readCodeKey(addr, codeKey); found {
		s.recordStoreReadEx(codeKey, writerIdx, writerInc, val, suicideIdx >= 0)
		if writerIdx > suicideIdx {
			return val.([]byte)
		}
		// Code write predates the most recent destruction → wiped.
		return nil
	}
	if suicideIdx >= 0 {
		s.recordStoreRead(codeKey, -1, 0, []byte(nil))
		return nil
	}
	baseCode, berr := s.base.GetCode(addr)
	s.noteBaseReadErr(berr)
	s.recordStoreRead(codeKey, -1, 0, baseCode)
	return baseCode
}

func (s *ParallelStateDB) GetCodeSize(addr common.Address) int {
	return len(s.GetCode(addr))
}

func (s *ParallelStateDB) GetCodeHash(addr common.Address) common.Hash {
	// For existing accounts, return the stored code hash.
	// Only recompute for accounts with newly set code.
	if _, ok := s.localCode[addr]; ok {
		code := s.GetCode(addr)
		if len(code) == 0 {
			return types.EmptyCodeHash
		}
		return crypto.Keccak256Hash(code)
	}
	suicideIdx := s.priorDestructedAt(addr)
	// For code set by prior txs, check MVStore via the same ESTIMATE-aware
	// path as GetCode and record the read so validation can catch a stale
	// value when the writer is later invalidated.
	codeKey := blockstm.NewSubpathKey(addr, CodePath)
	if val, writerIdx, writerInc, found := s.readCodeKey(addr, codeKey); found {
		s.recordStoreReadEx(codeKey, writerIdx, writerInc, val, suicideIdx >= 0)
		// Honor the code write only if it happened after the destruction
		// (otherwise the destruction wiped it).
		if writerIdx > suicideIdx {
			code := val.([]byte)
			if len(code) == 0 {
				return types.EmptyCodeHash
			}
			return crypto.Keccak256Hash(code)
		}
		// Fall through: account may have been recreated without code.
	} else {
		// No prior writer: record a base read with StoreVal=nil so validation
		// vfails this tx if a later writer for codeKey appears between our
		// observation and validation (would change EXTCODEHASH or CREATE2
		// collision results).
		s.recordStoreRead(codeKey, -1, 0, nil)
	}
	if suicideIdx >= 0 {
		// Destruction is the most recent code-affecting event for addr.
		// If a later tx has recreated the account (CREATE2 / value transfer),
		// EXTCODEHASH should return EmptyCodeHash; if not recreated, zero.
		// Exist() handles both cases (CreatePath, balance, base).
		if s.Exist(addr) {
			return types.EmptyCodeHash
		}
		return common.Hash{}
	}
	// For base (pre-block) accounts, use the stored code hash.
	baseHash, berr := s.base.GetCodeHash(addr)
	s.noteBaseReadErr(berr)
	if baseHash != (common.Hash{}) {
		return baseHash
	}
	// Account created by prior tx without code → EmptyCodeHash
	// Non-existent account → zero hash
	if s.Exist(addr) {
		return types.EmptyCodeHash
	}
	return common.Hash{}
}

func (s *ParallelStateDB) SetCode(addr common.Address, code []byte, reason tracing.CodeChangeReason) []byte {
	prev := s.GetCode(addr)
	_, hadCode := s.localCode[addr]
	var flags uint8
	if hadCode {
		flags = 1
	}
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkCode, flags: flags, addr: addr, code: prev})
	s.localCode[addr] = code
	codeKey := blockstm.NewSubpathKey(addr, CodePath)
	if !s.DeferMVWrites {
		s.store.WriteInc(codeKey, s.TxIndex, s.Incarnation, code)
	}
	s.recordWrite(codeKey)
	// Ensure the account is marked as existing so Exist() returns true.
	// In serial StateDB, SetCode calls getOrNewStateObject which creates it.
	// Critical for EIP-7702: applyAuthorization calls SetCode on the authority,
	// and the EVM's Call checks Exist before executing code.
	if !s.created[addr] {
		s.CreateAccount(addr)
	}
	return prev
}

// ---------- Storage (versioned) ----------

func (s *ParallelStateDB) GetState(addr common.Address, key common.Hash) common.Hash {
	// Note: do NOT short-circuit on s.destructed[addr]. Storage is wiped
	// only at tx finalization; within the current tx, slot reads must
	// continue to return live values (own writes via localStorage, prior-tx
	// writes via the MVStore, base state otherwise). Serial StateDB.GetState
	// makes no selfDestructed check for the same reason. Cross-tx visibility
	// is handled below via priorDestructedAt against SuicidePath writes.
	if slots, ok := s.localStorage[addr]; ok {
		if val, ok := slots[key]; ok {
			return val
		}
	}
	suicideIdx := s.priorDestructedAt(addr)
	stateKey := blockstm.NewStateKey(addr, key)
	if val, writerIdx, writerInc, found := s.readStoreWait(stateKey); found {
		s.recordStoreReadEx(stateKey, writerIdx, writerInc, val, suicideIdx >= 0)
		// Honor the slot write only if it landed AFTER the destruction.
		// Otherwise the destruction wiped storage and recreation alone
		// doesn't restore old slots.
		if writerIdx > suicideIdx {
			return val.(common.Hash)
		}
		return common.Hash{}
	}
	if suicideIdx >= 0 {
		// Destroyed and no later writer → wiped. Don't fall through to base:
		// recreation doesn't restore pre-destruction slots.
		s.recordStoreRead(stateKey, -1, 0, common.Hash{})
		return common.Hash{}
	}
	baseVal, berr := s.base.GetState(addr, key)
	s.noteBaseReadErr(berr)
	s.recordStoreRead(stateKey, -1, 0, baseVal)
	return baseVal
}

func (s *ParallelStateDB) GetCommittedState(addr common.Address, key common.Hash) common.Hash {
	// Returns the "original" value for SSTORE gas accounting. In serial
	// execution, this is the value after Finalise (includes prior txs' writes).
	// In V2, read from MVStore (prior txs' writes) or base state.
	// Cache on first access to ensure stability across multiple SSTOREs.
	ck := stateKey{addr: addr, slot: key}
	if v, ok := s.committedCache[ck]; ok {
		return v
	}
	suicideIdx := s.priorDestructedAt(addr)
	mvKey := blockstm.NewStateKey(addr, key)
	var result common.Hash
	if val, writerIdx, writerInc, found := s.readStoreWait(mvKey); found {
		s.recordStoreReadEx(mvKey, writerIdx, writerInc, val, suicideIdx >= 0)
		if writerIdx > suicideIdx {
			result = val.(common.Hash)
		}
		// else: destroyed after this write → result stays zero
	} else {
		if suicideIdx < 0 {
			var berr error
			result, berr = s.base.GetCommittedState(addr, key)
			s.noteBaseReadErr(berr)
		}
		s.recordStoreRead(mvKey, -1, 0, result)
	}
	if s.committedCache == nil {
		s.committedCache = make(map[stateKey]common.Hash)
	}
	s.committedCache[ck] = result
	return result
}

func (s *ParallelStateDB) GetStateAndCommittedState(addr common.Address, key common.Hash) (common.Hash, common.Hash) {
	return s.GetState(addr, key), s.GetCommittedState(addr, key)
}

func (s *ParallelStateDB) SetState(addr common.Address, key, value common.Hash) common.Hash {
	prev := s.GetState(addr, key)
	_, had := s.localStorage[addr][key]
	var flags uint8
	if had {
		flags = 1
	}
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkStorage, flags: flags, addr: addr, key: key, prev: prev})
	if s.localStorage[addr] == nil {
		s.localStorage[addr] = make(map[common.Hash]common.Hash)
	}
	s.localStorage[addr][key] = value
	mvKey := blockstm.NewStateKey(addr, key)
	// Storage writes are always deferred to FlushToMVStore regardless of
	// DeferMVWrites: per optimization log H3, publishing intermediate values
	// (e.g. reentrancy guards mid-execution) caused more vfails than it
	// saved. SetCode/CreateAccount write eagerly because they are effectively
	// monotonic per tx.
	s.recordWrite(mvKey)
	return prev
}

// GetStorageRoot returns the storage trie root used by the EVM CREATE/CREATE2
// "address-already-in-use" check (see core/vm/evm.go: a non-empty storage
// root makes the address ineligible for deployment).
//
// V2 must mask the base storage root once a prior tx in the block has
// SELFDESTRUCT'd this account (or marked it suicided in the current tx).
// Otherwise CREATE2 redeploys onto a destructed-but-still-rooted address
// fail with "address has existing storage", and the post-state diverges
// from serial — visible as the spec-test stCallCodes / stCreate2 recreate
// regressions.
//
// Cross-tx destruction is recorded in the MV store at SuicidePath; the
// priorDestructedAt lookup also records the read so validation can catch
// a later tx writing SuicidePath after we've returned the un-masked root.
func (s *ParallelStateDB) GetStorageRoot(addr common.Address) common.Hash {
	if s.created[addr] {
		// CREATE / CREATE2 in this tx → storage starts empty.
		return common.Hash{}
	}
	if s.destructed[addr] {
		// Same-tx SELFDESTRUCT: storage gets wiped at finalisation.
		return common.Hash{}
	}
	if s.priorDestructedAt(addr) >= 0 {
		// A prior tx in this block destructed addr; storage is gone unless
		// a later tx already recreated it (caller layer should keep
		// CreatePath in sync — Exist() handles that).
		return common.Hash{}
	}
	root, berr := s.base.GetStorageRoot(addr)
	s.noteBaseReadErr(berr)
	return root
}

// ---------- Refund ----------

func (s *ParallelStateDB) AddRefund(gas uint64) {
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkRefund, prevU: s.refund})
	s.refund += gas
}
func (s *ParallelStateDB) SubRefund(gas uint64) {
	if gas > s.refund {
		panic("refund counter below zero")
	}
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkRefund, prevU: s.refund})
	s.refund -= gas
}
func (s *ParallelStateDB) GetRefund() uint64 { return s.refund }

// ---------- Access list ----------

func (s *ParallelStateDB) AddressInAccessList(addr common.Address) bool {
	return s.accessList.ContainsAddress(addr)
}

func (s *ParallelStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	return s.accessList.Contains(addr, slot)
}

func (s *ParallelStateDB) AddAddressToAccessList(addr common.Address) {
	if s.accessList.AddAddress(addr) {
		s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkAccessAddr, addr: addr})
	}
}

func (s *ParallelStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	addrAdded, slotAdded := s.accessList.AddSlot(addr, slot)
	if addrAdded {
		s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkAccessAddr, addr: addr})
	}
	if slotAdded {
		s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkAccessSlot, addr: addr, key: slot})
	}
}

// ---------- Transient storage (EIP-1153) ----------

func (s *ParallelStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return s.transientStorage.Get(addr, key)
}

func (s *ParallelStateDB) SetTransientState(addr common.Address, key, value common.Hash) {
	prev := s.GetTransientState(addr, key)
	if prev == value {
		return
	}
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkTransient, addr: addr, key: key, prev: prev})
	s.transientStorage.Set(addr, key, value)
}

// ---------- Self-destruct ----------

func (s *ParallelStateDB) SelfDestruct(addr common.Address) uint256.Int {
	bal := *s.GetBalance(addr)
	// Only journal a destruct entry when this call actually flips the flag
	// — matches StateDB.SelfDestruct, where repeated calls within a tx skip
	// the journal. Without this guard, reverting a second SelfDestruct
	// un-destructs an account that was already destructed pre-snapshot.
	if !s.destructed[addr] {
		s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkDestruct, addr: addr})
		s.destructed[addr] = true
		// FlushToMVStore writes (SuicidePath_addr, txIdx, inc, true) for
		// every entry in s.destructed; record the matching key so that
		// MarkEstimate / CleanupEstimate reach it on re-execution. Other
		// MVStore-targeting writers (SetNonce / SetCode / SetState /
		// CreateAccount) all call recordWrite for the same reason —
		// without this, a stale SuicidePath entry from incarnation N
		// survives into incarnation N+1's view and a downstream tx that
		// observed it can pass validation against state that no longer
		// exists.
		s.recordWrite(blockstm.NewSubpathKey(addr, SuicidePath))
	}
	s.SubBalance(addr, &bal, tracing.BalanceDecreaseSelfdestruct)
	return bal
}

func (s *ParallelStateDB) HasSelfDestructed(addr common.Address) bool {
	return s.destructed[addr]
}

func (s *ParallelStateDB) SelfDestruct6780(addr common.Address) (uint256.Int, bool) {
	if s.newContract[addr] {
		return s.SelfDestruct(addr), true
	}
	// EIP-6780: SELFDESTRUCT on a non-same-tx-created contract MUST NOT
	// touch balances here — the EVM opcode handler (opSelfdestruct6780)
	// already performed SubBalance(addr) + AddBalance(beneficiary) before
	// invoking us. Serial StateDB.SelfDestruct6780 is a pure read in this
	// branch for the same reason. Mirroring that contract is the only way
	// the self-beneficiary case (CALLCODE → callee SELFDESTRUCT(caller))
	// preserves the contract's balance: SubBalance(addr) + AddBalance(addr)
	// nets to zero, but a third SubBalance here drains the contract.
	// Found via spec-tests stCallCodes/...SuicideEnd.
	return *s.GetBalance(addr), false
}

// ---------- Account creation ----------

func (s *ParallelStateDB) CreateAccount(addr common.Address) {
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkCreate, addr: addr})
	s.created[addr] = true
	createKey := blockstm.NewSubpathKey(addr, CreatePath)
	if !s.DeferMVWrites {
		s.store.WriteInc(createKey, s.TxIndex, s.Incarnation, true)
	}
	s.recordWrite(createKey)
}

func (s *ParallelStateDB) CreateContract(addr common.Address) {
	s.newContract[addr] = true
	s.CreateAccount(addr)
}

// ---------- Snapshot / Revert ----------

func (s *ParallelStateDB) Snapshot() int {
	id := s.nextRevisionId
	s.nextRevisionId++
	s.validRevisions = append(s.validRevisions, parallelRevision{
		id:            id,
		journalIdx:    len(s.journalEntries),
		balanceOpsIdx: len(s.BalanceOps),
		logsIdx:       len(s.logs),
		transfersIdx:  len(s.Transfers),
	})
	return id
}

func (s *ParallelStateDB) RevertToSnapshot(revid int) {
	// Find the revision
	idx := sort.Search(len(s.validRevisions), func(i int) bool {
		return s.validRevisions[i].id >= revid
	})
	if idx == len(s.validRevisions) || s.validRevisions[idx].id != revid {
		panic("invalid snapshot id")
	}
	rev := s.validRevisions[idx]

	// Undo journal entries in reverse
	for i := len(s.journalEntries) - 1; i >= rev.journalIdx; i-- {
		s.journalEntries[i].revert(s)
	}
	s.journalEntries = s.journalEntries[:rev.journalIdx]

	// Truncate BalanceOps, logs, and Transfers to snapshot state
	s.BalanceOps = s.BalanceOps[:rev.balanceOpsIdx]
	s.logs = s.logs[:rev.logsIdx]
	s.logSize = uint(rev.logsIdx)
	s.Transfers = s.Transfers[:rev.transfersIdx]

	s.validRevisions = s.validRevisions[:idx]
}

// ---------- Logs / Preimages ----------

func (s *ParallelStateDB) AddLog(l *types.Log) {
	s.journalEntries = append(s.journalEntries, parallelJournalEntry{kind: jkLog, prevU: uint64(len(s.logs))})
	s.logs = append(s.logs, l)
	s.logSize++
}

func (s *ParallelStateDB) AddPreimage(hash common.Hash, preimage []byte) {
	if _, ok := s.preimages[hash]; !ok {
		s.preimages[hash] = slices.Clone(preimage)
	}
}

func (s *ParallelStateDB) Logs() []*types.Log { return s.logs }

// ---------- Prepare ----------

func (s *ParallelStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	s.accessList = newAccessList()
	s.transientStorage = newTransientStorage()

	// Add initial warm addresses directly to the access list WITHOUT journaling.
	// These must survive inner call reverts — matching StateDB.Prepare behavior
	// where al.AddAddress is called directly (not through the journaling wrapper).
	// If journaled, a revert removes the sender from warm → SLOADs charge cold
	// gas (2100 vs 100) → cascading gas overuse → tx reverts incorrectly.
	s.accessList.AddAddress(sender)
	if dest != nil {
		s.accessList.AddAddress(*dest)
	}
	for _, addr := range precompiles {
		s.accessList.AddAddress(addr)
	}
	for _, el := range txAccesses {
		s.accessList.AddAddress(el.Address)
		for _, key := range el.StorageKeys {
			s.accessList.AddSlot(el.Address, key)
		}
	}
	if rules.IsShanghai { // EIP-3651: warm coinbase (match StateDB's check)
		s.accessList.AddAddress(coinbase)
	}
}

// ---------- Misc ----------

// Finalise is called at the end of each tx. In parallel mode, it's a no-op
// since actual finalisation happens during settlement on the real StateDB.
func (s *ParallelStateDB) Finalise(deleteEmptyObjects bool) {}

// Inner returns the underlying StateDB. Required by Bor consensus.
func (s *ParallelStateDB) Inner() *StateDB { return s.rawBase }

// Witness returns the shared *Witness so BLOCKHASH writes from V2 workers
// land where finalDB sees them. The share is established by the caller
// after statedb.Copy() (which deep-copies). Witness locks its own mutations.
func (s *ParallelStateDB) Witness() *stateless.Witness { return s.rawBase.Witness() }
func (s *ParallelStateDB) AccessEvents() *AccessEvents { return nil }

func (s *ParallelStateDB) RecordTransfer(sender, recipient common.Address, amount *uint256.Int) bool {
	// Record the transfer for later log generation during settlement.
	// The LogIdx marks where in the log stream this transfer occurred.
	// BalanceOpsIdx records where in the BalanceOps slice this transfer's SubBalance will appear.
	s.Transfers = append(s.Transfers, TransferRecord{
		Sender:        sender,
		Recipient:     recipient,
		Amount:        *amount,
		LogIdx:        len(s.logs), // current log position
		BalanceOpsIdx: len(s.BalanceOps),
	})
	return true
}

// SettleTo and its helpers (settleNonces, settleStorage, settleCode,
// settleBalanceOpsAndLogs, tryEmitTransferAt, emitTransferLog,
// settleAccountSet, applyFeeData, GetLogs) live in
// parallel_statedb_settle.go.
