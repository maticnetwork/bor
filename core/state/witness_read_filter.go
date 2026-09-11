package state

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
)

// WitnessReadFilter decides which reads may contribute trie nodes to a block's
// witness under BlockSTM v2.
//
// V2 collects the witness from state shared by every worker — the reader's
// account/storage cache and SafeBase's code cache — with no record of which
// incarnation performed a read, and nothing prunes those caches when an
// incarnation is invalidated: ParallelStateDB.Reset clears only task-local
// maps and re-points at the same shared SafeBase. A read made solely by an
// incarnation that was thrown away is therefore recorded like any other.
//
// That matters only when a transaction's *access set* changes between the
// discarded incarnation and the one that commits — a branch on a slot another
// transaction in the same block writes, say, reaching a different account
// depending on what it saw. Then the witness carries nodes no committed
// execution needs, and whether it does depends on worker timing, so two
// honest nodes publish different bytes for the same block. WIT2's BP-signed
// commitment assumes exactly the opposite.
//
// The filter gates the witness *walk* rather than the caches, because a walk
// is irreversible: resolving a key records its nodes in the trie reader's
// tracers, and nothing can take them back out afterwards. So a key that some
// incarnation read is held back until a committed incarnation has read it,
// and is never walked if none does.
//
// Keys no task read at all — the import prefetcher, engine finalization,
// system calls, the settle path — are not held back. That default is what
// keeps this from re-introducing the incomplete witnesses that made read-set
// collection necessary: anything the filter has not positively seen as a
// speculative task read walks exactly as it does today. A read path that
// bypasses recordStoreRead/recordBalanceRead degrades toward today's
// behaviour (a possibly larger witness), never toward a missing node.
//
// The zero value is not usable; callers take a nil *WitnessReadFilter to mean
// "no filtering", which is the correct behaviour for serial execution and for
// any path that never runs BlockSTM.
type WitnessReadFilter struct {
	// taskAccounts/taskSlots hold keys read by some incarnation, committed or
	// not. committedAccounts/committedSlots hold the subset read by an
	// incarnation that reached the settle callback.
	taskAccounts      sync.Map // common.Address -> struct{}
	committedAccounts sync.Map // common.Address -> struct{}
	taskSlots         sync.Map // stateKey -> struct{}
	committedSlots    sync.Map // stateKey -> struct{}
}

// NewWitnessReadFilter returns a filter ready for a single block's execution.
func NewWitnessReadFilter() *WitnessReadFilter {
	return &WitnessReadFilter{}
}

// RecordTaskAccount notes that an incarnation read an account-level key. It
// must be called as the read happens, not at settle: the prewalker sweeps
// concurrently with execution, and a key it walks before the filter knows the
// read was speculative is already in the tracers for good.
func (f *WitnessReadFilter) RecordTaskAccount(addr common.Address) {
	if f == nil {
		return
	}
	f.taskAccounts.Store(addr, struct{}{})
}

// RecordTaskSlot notes that an incarnation read a storage slot. Same timing
// requirement as RecordTaskAccount.
func (f *WitnessReadFilter) RecordTaskSlot(addr common.Address, slot common.Hash) {
	if f == nil {
		return
	}
	f.taskSlots.Store(stateKey{addr: addr, slot: slot}, struct{}{})
}

// CommitAccount promotes an account-level key to committed, releasing it to
// the next walk.
func (f *WitnessReadFilter) CommitAccount(addr common.Address) {
	if f == nil {
		return
	}
	f.committedAccounts.Store(addr, struct{}{})
}

// CommitSlot promotes a storage slot to committed.
func (f *WitnessReadFilter) CommitSlot(addr common.Address, slot common.Hash) {
	if f == nil {
		return
	}
	f.committedSlots.Store(stateKey{addr: addr, slot: slot}, struct{}{})
}

// AccountWalkable reports whether addr's trie path may be resolved into the
// witness now. A nil filter walks everything, which is what serial execution
// and the non-BlockSTM paths want.
func (f *WitnessReadFilter) AccountWalkable(addr common.Address) bool {
	if f == nil {
		return true
	}
	if _, isTask := f.taskAccounts.Load(addr); !isTask {
		return true
	}
	_, committed := f.committedAccounts.Load(addr)
	return committed
}

// SlotWalkable reports whether a storage slot's trie path may be resolved into
// the witness now.
func (f *WitnessReadFilter) SlotWalkable(addr common.Address, slot common.Hash) bool {
	if f == nil {
		return true
	}
	key := stateKey{addr: addr, slot: slot}
	if _, isTask := f.taskSlots.Load(key); !isTask {
		return true
	}
	_, committed := f.committedSlots.Load(key)
	return committed
}
