package state

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file is the automated tripwire for an upstream-merge surface that
// would otherwise need a manual checklist:
//
//   "Did upstream go-ethereum add a method to *StateDB that V2's
//    *ParallelStateDB should mirror?"
//
// vm.StateDB conformance is already enforced at compile-time by
// core/vm/statedb_impl_test.go. This test goes further: it forces every
// EXPORTED method on *StateDB to either appear on *ParallelStateDB or
// be explicitly listed in pdbExemptMethods with a category and rationale.
//
// When this test fails after a merge, the engineer has two options:
//   (a) implement the new method on *ParallelStateDB (preferred when the
//       method is part of the EVM-facing surface), or
//   (b) add it to pdbExemptMethods with a category that justifies why it
//       belongs only on the underlying serial path (V1 internals, V2
//       settle helpers, lifecycle, debug, etc.).
//
// The accompanying TestV2DependencyCompileCheck below pins the OTHER
// direction: V2-settle's actual dependencies on *StateDB. If upstream
// renames or removes a method V2 settle calls, that test stops compiling.

// pdbExemptCategory groups exemptions so a future reviewer can see at a
// glance whether a missing method is benign or a real omission.
type pdbExemptCategory string

const (
	catV1Internals    pdbExemptCategory = "V1 BlockSTM internals"
	catV2SettleHelper pdbExemptCategory = "V2 settle helper (called on the underlying StateDB by SettleTo)"
	catLifecycle      pdbExemptCategory = "block lifecycle (commit / prefetcher / copy)"
	catLowLevel       pdbExemptCategory = "low-level / utility"
	catDebug          pdbExemptCategory = "debug / introspection"
	catPipelinedSRC   pdbExemptCategory = "pipelined SRC import"
)

var pdbExemptMethods = map[string]pdbExemptCategory{
	// V1 BlockSTM internals — V2 uses MVStore + MVBalanceStore + StoreReads
	// instead of MVHashMap + readList/writeList, so none of these are
	// applicable on a ParallelStateDB.
	"AddEmptyMVHashMap":     catV1Internals,
	"ApplyMVWriteSet":       catV1Internals,
	"ClearReadMap":          catV1Internals,
	"ClearWriteMap":         catV1Internals,
	"DepTxIndex":            catV1Internals,
	"EnableConcurrentReads": catV1Internals,
	"FlushMVWriteSet":       catV1Internals,
	"GetMVHashmap":          catV1Internals,
	"GetReadMapDump":        catV1Internals,
	"GetWriteMapDump":       catV1Internals,
	"HadInvalidRead":        catV1Internals,
	"MVFullWriteList":       catV1Internals,
	"MVReadList":            catV1Internals,
	"MVReadMap":             catV1Internals,
	"MVWriteList":           catV1Internals,
	"SetIncarnation":        catV1Internals,
	"SetMVHashmap":          catV1Internals,
	"Version":               catV1Internals,

	// V2 settle helpers — invoked on the underlying *StateDB by
	// ParallelStateDB.SettleTo. PDB has the user-facing journaled API
	// (AddBalance, SetCode, …); the *Direct variants intentionally bypass
	// journaling and are not part of the EVM-visible interface.
	"AddBalanceDirect":            catV2SettleHelper,
	"SubBalanceDirect":            catV2SettleHelper,
	"SetNonceDirect":              catV2SettleHelper,
	"SetStorageDirectWithOrigins": catV2SettleHelper,
	"FinaliseFast":                catV2SettleHelper,
	"FinaliseFastWithPrefetch":    catV2SettleHelper,
	"StorageCache":                catV2SettleHelper,
	"SkipTimers":                  catV2SettleHelper,
	"SetTxContext":                catV2SettleHelper,
	"SetWitness":                  catV2SettleHelper,
	// Witness collection — V2's V2StateProcessor.Process calls
	// statedb.CollectStateWitness on the underlying *StateDB after settle
	// to pull in worker-side trie reads. PDB doesn't need a counterpart.
	"CollectStateWitness": catV2SettleHelper,
	// Started once per block on finalDB before workers run; walks the shared
	// read cache into the witness concurrently with execution. Per-tx PDBs
	// never orchestrate block-level witness recording.
	"StartWitnessReadSetPrewalk": catV2SettleHelper,
	// V2 calls this on the underlying *StateDB at SafeBase construction
	// to flush pre-block dirty/pending storage (system calls, DAO fork)
	// into the shared trieReader storage cache. PDB never needs to do
	// this — its writes are tracked through MVStore.
	"OverlayPendingStorageInto": catV2SettleHelper,

	// Block lifecycle — the final commit / copy / prefetcher always run on
	// the underlying StateDB; PDB is per-tx and recycled, not committed.
	"Commit":                catLifecycle,
	"CommitWithUpdate":      catLifecycle,
	"IntermediateRoot":      catLifecycle,
	"StartPrefetcher":       catLifecycle,
	"StopPrefetcher":        catLifecycle,
	"ResetPrefetcher":       catLifecycle,
	"Copy":                  catLifecycle,
	"CopyWithoutLogHistory": catLifecycle,

	// Pipelined SRC import — FlatDiff capture/replay, read propagation,
	// and detached prefetcher handoff are block-level StateDB operations.
	// ParallelStateDB instances are per-transaction workers; V2 settles
	// into the underlying StateDB before these methods run.
	"ApplyFlatDiff":              catPipelinedSRC,
	"ApplyFlatDiffForCommit":     catPipelinedSRC,
	"ApplyFlatDiffForCommitFast": catPipelinedSRC,
	"CommitSnapshot":             catPipelinedSRC,
	"DetachPrefetcher":           catPipelinedSRC,
	"PropagateReadsTo":           catPipelinedSRC,
	"SetFlatDiffRef":             catPipelinedSRC,
	"WasStorageSlotRead":         catPipelinedSRC,

	// Low-level / utility — not part of the EVM-facing surface.
	"Database":              catLowLevel,
	"Error":                 catLowLevel,
	"GetOrNewStateObject":   catLowLevel,
	"GetTrie":               catLowLevel,
	"Preimages":             catLowLevel,
	"Reader":                catLowLevel,
	"SetStorage":            catLowLevel,
	"StorageTrie":           catLowLevel,
	"TxIndex":               catLowLevel,
	"ValidateKnownAccounts": catLowLevel,

	// Debug / introspection — purely for tooling.
	"Dump":            catDebug,
	"DumpToCollector": catDebug,
	"RawDump":         catDebug,
	"IterativeDump":   catDebug,
}

// TestPDBMethodParity fails when *StateDB grows an exported method that
// has no *ParallelStateDB equivalent and is not in the exemption table.
//
// This is the single sharpest tool against the upstream-merge drift
// problem on the StateDB surface — every new method must be classified.
func TestPDBMethodParity(t *testing.T) {
	sdbType := reflect.TypeOf(&StateDB{})
	pdbType := reflect.TypeOf(&ParallelStateDB{})

	pdbMethods := make(map[string]reflect.Method)
	for i := 0; i < pdbType.NumMethod(); i++ {
		m := pdbType.Method(i)
		pdbMethods[m.Name] = m
	}

	var missing []string
	var staleExempt []string
	seenExemptions := make(map[string]bool)

	for i := 0; i < sdbType.NumMethod(); i++ {
		m := sdbType.Method(i)
		if _, ok := pdbMethods[m.Name]; ok {
			// Both have it — signature must match (excluding receiver).
			if !methodsCompatible(m, pdbMethods[m.Name]) {
				t.Errorf("%s: signature mismatch — StateDB has %s, ParallelStateDB has %s",
					m.Name, m.Type.String(), pdbMethods[m.Name].Type.String())
			}
			continue
		}
		if _, ok := pdbExemptMethods[m.Name]; ok {
			seenExemptions[m.Name] = true
			continue
		}
		missing = append(missing, m.Name)
	}

	// Find exemptions that no longer correspond to a real StateDB method
	// (e.g., a method removed upstream — keeps the allowlist tidy).
	for name := range pdbExemptMethods {
		if !seenExemptions[name] {
			staleExempt = append(staleExempt, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf(`*StateDB methods with no *ParallelStateDB equivalent and no exemption (drift detected):

  %s

Either implement these methods on *ParallelStateDB, or add them to
pdbExemptMethods with a category and rationale.`,
			strings.Join(missing, "\n  "))
	}
	if len(staleExempt) > 0 {
		sort.Strings(staleExempt)
		t.Errorf(`pdbExemptMethods entries no longer correspond to a real *StateDB method:

  %s

Remove these from pdbExemptMethods.`, strings.Join(staleExempt, "\n  "))
	}
}

// methodsCompatible reports whether two methods have the same signature
// modulo their receiver type (which is always *StateDB vs
// *ParallelStateDB). We compare input/output type strings of all
// non-receiver positions.
func methodsCompatible(a, b reflect.Method) bool {
	at, bt := a.Type, b.Type
	if at.NumIn() != bt.NumIn() || at.NumOut() != bt.NumOut() {
		return false
	}
	// Skip arg 0 (receiver) on both sides.
	for i := 1; i < at.NumIn(); i++ {
		if at.In(i).String() != bt.In(i).String() {
			return false
		}
	}
	for i := 0; i < at.NumOut(); i++ {
		if at.Out(i).String() != bt.Out(i).String() {
			return false
		}
	}
	return true
}

// TestV2DependencyCompileCheck pins the methods on *StateDB that V2 settle
// actively uses. The function below references each one by value; if a
// dependency is renamed, removed, or has its signature changed upstream,
// this file stops compiling — failing the build immediately on `go build`,
// long before any test runs.
//
// This is the OTHER half of the parity story: TestPDBMethodParity catches
// new methods that V2 should mirror, this catches existing methods V2
// already mirrors going away.
func TestV2DependencyCompileCheck(t *testing.T) {
	// The act of taking these method values is enough — the test body
	// exists only to produce a runtime no-op the linter won't strip.
	if v2DependencyCompileCheck == nil {
		t.Fatal("unreachable")
	}
}

// v2DependencyCompileCheck never executes — its purpose is purely
// compile-time. Each line below is a known V2 settle / executor
// dependency on *StateDB. When upstream go-ethereum renames or changes
// the signature of any method here, the build fails on this file.
//
// Add a new line whenever V2 introduces a new dependency on *StateDB.
// Remove a line only when V2 stops using a method.
var v2DependencyCompileCheck = func() any {
	var s *StateDB
	// Read APIs that V2 settle uses to capture pre-tx state and to
	// resolve origins for storage commits.
	_ = s.GetBalance
	_ = s.GetCode
	_ = s.GetCodeHash
	_ = s.GetCommittedState
	_ = s.GetState
	_ = s.GetStorageRoot
	_ = s.Exist

	// Direct setters — V2 settle bypasses journaling and writes through
	// these. Their signatures (and side effects) must remain stable.
	_ = s.AddBalanceDirect
	_ = s.SubBalanceDirect
	_ = s.SetNonceDirect
	_ = s.SetStorageDirectWithOrigins

	// Journaled setters that V2 settle still uses (rare cases like
	// SetCode / SelfDestruct / CreateAccount where the side effects on
	// state object lifecycle are needed).
	_ = s.SetCode
	_ = s.SelfDestruct
	_ = s.CreateAccount
	_ = s.AddPreimage
	_ = s.AddLog

	// Lifecycle hooks V2 calls on the underlying StateDB.
	_ = s.SetTxContext
	_ = s.FinaliseFastWithPrefetch
	_ = s.IntermediateRoot
	_ = s.StartPrefetcher
	_ = s.StopPrefetcher
	_ = s.SkipTimers
	_ = s.StorageCache
	_ = s.Copy

	return nil
}

func init() {
	// Force the dependency-check value to be evaluated so the compiler
	// can't dead-code-eliminate it. The runtime cost is one initialization
	// of a no-op closure that returns nil.
	_ = fmt.Sprintf("%v", v2DependencyCompileCheck())
}
