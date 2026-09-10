package core

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/params"
)

// This file pins the contract that every params.ChainConfig.IsX fork
// rule referenced by the V1 state processor is consciously addressed by
// the V2 state processor — either by referencing it too, or by an
// explicit exemption with a documented rationale.
//
// Background: when upstream go-ethereum adds a new fork rule (e.g.,
// IsOsaka), it typically also adds a `if config.IsOsaka(...) { ... }`
// branch somewhere in core/state_processor.go to gate new behaviour.
// V2's parallel_state_processor.go has its own `Process` and
// `finalizeV2Block` functions that mirror those branches. If the V2
// author misses the new rule on an upstream merge, blocks crossing the
// fork will execute differently between V1 and V2 and the state root
// will diverge.
//
// This test fails when:
//   * A new IsX is added to params.ChainConfig and no entry exists in
//     forkExpectations.
//   * A fork referenced by V1 is missing from V2 (or vice versa)
//     without an explicit exemption.
//   * An exemption is set but no rationale is documented.

// forkExpect captures the expected reference status of one fork rule
// in each processor path. inV1 / inV2 mean: "do we expect the V1 / V2
// state-processor source code to reference this fork rule somewhere
// within its file?"
//
// The reason for asymmetry must always have a rationale.
type forkExpect struct {
	inV1      bool
	inV2      bool
	rationale string // required when inV1 != inV2
}

// forkExpectations classifies every fork on params.ChainConfig.
//
// The default classification — both inV1 and inV2 false — is for forks
// that are gated entirely inside the EVM (vm/...) or by the consensus
// engine, never inside the state processor itself. The v2/v1 columns
// only need to be true for fork rules that BRANCH at the state-processor
// level (system calls, intermediate-root post-state handling, request
// processing, etc.).
var forkExpectations = map[string]forkExpect{
	// Pre-Byzantium forks affect EVM gas/precompiles only. Neither
	// state processor branches on them directly.
	"IsHomestead":        {inV1: false, inV2: false},
	"IsEIP150":           {inV1: false, inV2: false},
	"IsEIP155":           {inV1: false, inV2: false},
	"IsDAOFork":          {inV1: false, inV2: false}, // ApplyDAOHardFork is gated by config.DAOForkSupport, not IsDAOFork.
	"IsConstantinople":   {inV1: false, inV2: false},
	"IsPetersburg":       {inV1: false, inV2: false},
	"IsIstanbul":         {inV1: false, inV2: false},
	"IsBerlin":           {inV1: false, inV2: false},
	"IsMuirGlacier":      {inV1: false, inV2: false},
	"IsArrowGlacier":     {inV1: false, inV2: false},
	"IsGrayGlacier":      {inV1: false, inV2: false},
	"IsShanghai":         {inV1: false, inV2: false}, // EIP-3651 warm coinbase happens inside Prepare; not gated here.
	"IsCancun":           {inV1: false, inV2: false}, // BeaconRoot system call is gated by block.BeaconRoot != nil.
	"IsTerminalPoWBlock": {inV1: false, inV2: false},
	"IsPostMerge":        {inV1: false, inV2: false},
	"IsVerkleGenesis":    {inV1: false, inV2: false},
	"IsEIP4762":          {inV1: false, inV2: false},
	"IsOsaka":            {inV1: false, inV2: false},
	"IsAmsterdam":        {inV1: false, inV2: false}, // EIP-7843 SLOTNUM / EIP-8024 / BAL precompile-touch are EVM/consensus-gated, not state-processor-gated.

	// State-processor-level forks that BOTH paths must gate.
	"IsByzantium": {inV1: true, inV2: true}, // selects intermediate root vs receipt status
	"IsEIP158":    {inV1: true, inV2: true}, // empty-account deletion at finalise
	"IsLondon":    {inV1: true, inV2: true}, // EIP-1559 fee burn + receipt fields
	"IsPrague":    {inV1: true, inV2: true}, // EIP-2935 history storage system call
	"IsVerkle":    {inV1: true, inV2: true}, // EIP-2935 also fires under Verkle
}

// pathsForV1 / pathsForV2 are the files whose source the test scans
// for fork-rule references. Only state-processor code goes here — the
// EVM (vm/) is shared between V1 and V2 so it's irrelevant for parity.
var (
	pathsForV1 = []string{"state_processor.go"}
	pathsForV2 = []string{
		"parallel_state_processor.go",
		// Note: parallel_state_processor.go contains BOTH V1's
		// ParallelStateProcessor (the old MVHashMap-based parallel
		// path) and V2's V2StateProcessor. We grep the whole file —
		// for parity purposes any fork reference, regardless of
		// which struct uses it, satisfies "V2 has it". We accept
		// the extra coverage; the false-positive risk is low because
		// V1 ExecutionTask and V2 newV2SettleFn share most fork
		// gates anyway.
	}
)

// TestV2ForkParity asserts that every params.ChainConfig.IsX method is
// classified in forkExpectations and that the actual references in
// V1/V2 source match the classification.
func TestV2ForkParity(t *testing.T) {
	cfgType := reflect.TypeOf(&params.ChainConfig{})
	var allForks []string
	for i := 0; i < cfgType.NumMethod(); i++ {
		name := cfgType.Method(i).Name
		if strings.HasPrefix(name, "Is") && len(name) > 2 {
			allForks = append(allForks, name)
		}
	}
	sort.Strings(allForks)

	v1Source := readSources(t, pathsForV1)
	v2Source := readSources(t, pathsForV2)

	var unclassified, asymmDrift, missingRationale []string
	seenInTable := make(map[string]bool)

	for _, fork := range allForks {
		expect, ok := forkExpectations[fork]
		if !ok {
			unclassified = append(unclassified, fork)
			continue
		}
		seenInTable[fork] = true

		actualV1 := strings.Contains(v1Source, "."+fork+"(")
		actualV2 := strings.Contains(v2Source, "."+fork+"(")

		if actualV1 != expect.inV1 || actualV2 != expect.inV2 {
			asymmDrift = append(asymmDrift,
				fork+": expected (V1="+yn(expect.inV1)+", V2="+yn(expect.inV2)+
					") got (V1="+yn(actualV1)+", V2="+yn(actualV2)+")")
		}
		if expect.inV1 != expect.inV2 && strings.TrimSpace(expect.rationale) == "" {
			missingRationale = append(missingRationale, fork)
		}
	}

	// Stale entries.
	var stale []string
	for name := range forkExpectations {
		if !seenInTable[name] {
			stale = append(stale, name)
		}
	}

	if len(unclassified) > 0 {
		t.Errorf(`params.ChainConfig has new fork rules not classified in forkExpectations:

  %s

Add each to forkExpectations as either:
    {inV1: false, inV2: false}                       (gated entirely in vm/, not the state processor)
    {inV1: true,  inV2: true}                        (both processors must branch on it)
    {inV1: ..., inV2: ..., rationale: "..."}         (asymmetric — explain why)`,
			strings.Join(unclassified, "\n  "))
	}
	if len(asymmDrift) > 0 {
		sort.Strings(asymmDrift)
		t.Errorf(`Fork-reference state in V1/V2 sources doesn't match forkExpectations:

  %s

Either update the expectations (intentional change) or align the source.`,
			strings.Join(asymmDrift, "\n  "))
	}
	if len(missingRationale) > 0 {
		sort.Strings(missingRationale)
		t.Errorf("Asymmetric expectations need a rationale: %v", missingRationale)
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("forkExpectations has entries no longer on params.ChainConfig: %v", stale)
	}
}

func readSources(t *testing.T, files []string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range files {
		// Files are relative to this package's directory.
		path := filepath.Join(".", f)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
