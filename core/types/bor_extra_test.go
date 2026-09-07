// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package types

import (
	"bytes"
	"math/big"
	"reflect"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// wrapExtra wraps an RLP-encoded BlockExtraData/BlockExtraDataPostAustin body in
// the vanity+seal envelope real header.Extra bytes use.
func wrapExtra(body []byte) []byte {
	extra := make([]byte, ExtraVanityLength, ExtraVanityLength+len(body)+ExtraSealLength)
	extra = append(extra, body...)
	extra = append(extra, make([]byte, ExtraSealLength)...)
	return extra
}

// borExtraWithDeps builds a Bor-style Extra (vanity + RLP(BlockExtraData) + seal)
// whose TxDependency holds `deps` empty inner lists.
func borExtraWithDeps(t *testing.T, validatorBytes []byte, deps int, gasTarget, bfcd *uint64) []byte {
	t.Helper()

	body, err := rlp.EncodeToBytes(&BlockExtraData{
		ValidatorBytes:           validatorBytes,
		TxDependency:             make([][]uint64, deps),
		GasTarget:                gasTarget,
		BaseFeeChangeDenominator: bfcd,
	})
	if err != nil {
		t.Fatalf("encode BlockExtraData: %v", err)
	}

	return wrapExtra(body)
}

// allocBytes reports how many bytes f allocates, with GC paused so the delta is
// just f's own work. Must run without t.Parallel(): TotalAlloc is process-wide.
func allocBytes(f func()) uint64 {
	old := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(old)

	runtime.GC()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestBorHeaderExtraDecodeKeepsTxDepsRaw verifies that the pre-seal verification
// accessors read ValidatorBytes and the base-fee params without expanding
// TxDependency, so their allocation stays proportional to len(Extra) rather than
// to the number of dependency entries.
func TestBorHeaderExtraDecodeKeepsTxDepsRaw(t *testing.T) {
	const deps = 200_000

	cfg := &params.ChainConfig{ChainID: big.NewInt(137), CancunBlock: big.NewInt(0), Bor: &params.BorConfig{ValenciaBlock: big.NewInt(0)}}
	gasTarget, bfcd := uint64(30_000_000), uint64(8)
	h := &Header{
		Number: big.NewInt(1),
		Extra:  borExtraWithDeps(t, []byte("val set"), deps, &gasTarget, &bfcd),
	}
	payload := h.Extra[ExtraVanityLength : len(h.Extra)-ExtraSealLength]

	// A full decode into BlockExtraData expands TxDependency into [][]uint64.
	var expanded BlockExtraData
	expandAlloc := allocBytes(func() {
		if err := rlp.DecodeBytes(payload, &expanded); err != nil {
			t.Fatal(err)
		}
	})
	if len(expanded.TxDependency) != deps {
		t.Fatalf("full decode wrong: got %d deps, want %d", len(expanded.TxDependency), deps)
	}

	// The accessor verifyHeader uses keeps TxDependency raw.
	var (
		gotVals       []byte
		gotGT, gotDen *uint64
	)
	accessorAlloc := allocBytes(func() {
		gotVals, gotGT, gotDen = h.GetValidatorBytesAndBaseFeeParams(cfg)
	})

	// Keeping TxDependency raw must change cost, not result.
	if !bytes.Equal(gotVals, []byte("val set")) {
		t.Fatalf("validator bytes: got %q, want %q", gotVals, "val set")
	}
	if gotGT == nil || *gotGT != gasTarget || gotDen == nil || *gotDen != bfcd {
		t.Fatalf("base-fee params: got (%v,%v), want (%d,%d)", gotGT, gotDen, gasTarget, bfcd)
	}

	// The accessor must allocate O(len(Extra)), not O(deps * sizeof([]uint64)).
	if max := uint64(len(h.Extra)) * 8; accessorAlloc > max {
		t.Fatalf("accessor allocated %d bytes for a %d-byte Extra (cap %d): TxDependency was expanded",
			accessorAlloc, len(h.Extra), max)
	}
	if expandAlloc < accessorAlloc*8 {
		t.Fatalf("expected the full decode (%d B) to allocate far more than the raw accessor (%d B)",
			expandAlloc, accessorAlloc)
	}
	t.Logf("full=%d B  accessor=%d B  (%.0fx less)", expandAlloc, accessorAlloc, float64(expandAlloc)/float64(accessorAlloc+1))
}

// TestBorHeaderExtraAccessorsAgree pins the behavior of the single-pass accessor
// against the individual accessors and the full decoder, across the Cancun
// boundary, so the optimization stays semantics-preserving.
func TestBorHeaderExtraAccessorsAgree(t *testing.T) {
	t.Parallel()

	gasTarget, bfcd := uint64(20_000_000), uint64(16)
	h := &Header{
		Number: big.NewInt(10),
		Extra:  borExtraWithDeps(t, []byte("validator-bytes"), 4, &gasTarget, &bfcd),
	}

	// Post-Cancun: combined accessor agrees with GetValidatorBytes / GetBaseFeeParams.
	post := &params.ChainConfig{ChainID: big.NewInt(137), CancunBlock: big.NewInt(0)}
	vals, gt, den := h.GetValidatorBytesAndBaseFeeParams(post)
	if !bytes.Equal(vals, h.GetValidatorBytes(post)) {
		t.Fatal("validator bytes disagree between accessors")
	}
	if gt2, den2 := h.GetBaseFeeParams(post); !reflect.DeepEqual(gt, gt2) || !reflect.DeepEqual(den, den2) {
		t.Fatal("base-fee params disagree between accessors")
	}
	if !bytes.Equal(vals, []byte("validator-bytes")) || gt == nil || *gt != gasTarget || den == nil || *den != bfcd {
		t.Fatalf("unexpected decoded values: vals=%q gt=%v den=%v", vals, gt, den)
	}

	// The execution path still gets a fully expanded TxDependency.
	if full := h.DecodeBlockExtraData(post); full == nil || len(full.TxDependency) != 4 {
		t.Fatalf("DecodeBlockExtraData must still expand TxDependency for execution: %+v", full)
	}

	// Pre-Cancun: validator bytes are the raw envelope and base-fee params are nil.
	pre := &params.ChainConfig{ChainID: big.NewInt(137)} // CancunBlock == nil
	rawVals, pgt, pden := h.GetValidatorBytesAndBaseFeeParams(pre)
	if pgt != nil || pden != nil {
		t.Fatalf("pre-Cancun base-fee params must be nil: got (%v,%v)", pgt, pden)
	}
	if want := h.Extra[ExtraVanityLength : len(h.Extra)-ExtraSealLength]; !bytes.Equal(rawVals, want) {
		t.Fatal("pre-Cancun validator bytes must be the raw envelope")
	}
}

// TestBorHeaderExtraAustinBoundary checks the accessors at the
// activation boundary (N-1/N/N+1): pre-Austin blocks decode real TxDependency
// via BlockExtraData; post-Austin blocks decode via BlockExtraDataPostAustin and
// every accessor returns nil TxDependency.
func TestBorHeaderExtraAustinBoundary(t *testing.T) {
	t.Parallel()

	const austin = 100
	cfg := &params.ChainConfig{
		ChainID:     big.NewInt(137),
		CancunBlock: big.NewInt(0),
		Bor: &params.BorConfig{
			ValenciaBlock: big.NewInt(0),
			AustinBlock:   big.NewInt(austin),
		},
	}
	gasTarget, bfcd := uint64(30_000_000), uint64(8)

	// N-1: pre-Austin, TxDependency is still on the wire and must decode.
	preHeader := &Header{
		Number: big.NewInt(austin - 1),
		Extra:  borExtraWithDeps(t, []byte("val set"), 3, &gasTarget, &bfcd),
	}

	vals, gt, den := preHeader.GetValidatorBytesAndBaseFeeParams(cfg)
	if !bytes.Equal(vals, []byte("val set")) {
		t.Fatalf("pre-Austin validator bytes: got %q", vals)
	}
	if gt == nil || *gt != gasTarget || den == nil || *den != bfcd {
		t.Fatalf("pre-Austin base-fee params: got (%v,%v)", gt, den)
	}
	if full := preHeader.DecodeBlockExtraData(cfg); full == nil || len(full.TxDependency) != 3 {
		t.Fatalf("pre-Austin DecodeBlockExtraData must still expose real TxDependency: %+v", full)
	}

	// N and N+1: post-Austin, no TxDependency slot on the wire at all.
	for _, n := range []int64{austin, austin + 1} {
		body, err := rlp.EncodeToBytes(&BlockExtraDataPostAustin{
			ValidatorBytes:           []byte("val set"),
			GasTarget:                &gasTarget,
			BaseFeeChangeDenominator: &bfcd,
		})
		if err != nil {
			t.Fatalf("encode BlockExtraDataPostAustin: %v", err)
		}
		postHeader := &Header{Number: big.NewInt(n), Extra: wrapExtra(body)}

		vals, gt, den := postHeader.GetValidatorBytesAndBaseFeeParams(cfg)
		if !bytes.Equal(vals, []byte("val set")) {
			t.Fatalf("block %d validator bytes: got %q", n, vals)
		}
		if gt == nil || *gt != gasTarget || den == nil || *den != bfcd {
			t.Fatalf("block %d base-fee params: got (%v,%v)", n, gt, den)
		}
		if vals2 := postHeader.GetValidatorBytes(cfg); !bytes.Equal(vals2, vals) {
			t.Fatalf("block %d: GetValidatorBytes disagrees with combined accessor", n)
		}
		if gt2, den2 := postHeader.GetBaseFeeParams(cfg); !reflect.DeepEqual(gt, gt2) || !reflect.DeepEqual(den, den2) {
			t.Fatalf("block %d: GetBaseFeeParams disagrees with combined accessor", n)
		}

		full := postHeader.DecodeBlockExtraData(cfg)
		if full == nil {
			t.Fatalf("block %d: DecodeBlockExtraData returned nil", n)
		}
		if full.TxDependency != nil {
			t.Fatalf("block %d: TxDependency must be nil post-Austin, got %v", n, full.TxDependency)
		}
		if !bytes.Equal(full.ValidatorBytes, []byte("val set")) || full.GasTarget == nil || *full.GasTarget != gasTarget {
			t.Fatalf("block %d: DecodeBlockExtraData mismatch: %+v", n, full)
		}

		// Round-trip: BlockExtraDataPostAustin must encode/decode stably.
		var decoded BlockExtraDataPostAustin
		if err := rlp.DecodeBytes(body, &decoded); err != nil {
			t.Fatalf("block %d: BlockExtraDataPostAustin decode: %v", n, err)
		}
		reencoded, err := rlp.EncodeToBytes(&decoded)
		if err != nil {
			t.Fatalf("block %d: BlockExtraDataPostAustin re-encode: %v", n, err)
		}
		if !bytes.Equal(reencoded, body) {
			t.Fatalf("block %d: BlockExtraDataPostAustin round-trip mismatch", n)
		}
	}
}

func TestTxDependencyValenciaGate(t *testing.T) {
	const valencia = 100
	cfg := &params.ChainConfig{Bor: &params.BorConfig{ValenciaBlock: big.NewInt(valencia)}}

	validTxDep, err := rlp.EncodeToBytes([][]uint64{{0}, {0, 1}})
	if err != nil {
		t.Fatalf("encode valid txdep: %v", err)
	}

	malformed := map[string]rlp.RawValue{
		"string":         {0x81, 0xff},
		"list-of-int":    {0xc1, 0x01},
		"inner overflow": {0xcb, 0xca, 0x89, 0x01, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, raw := range malformed {
		if txDependencyValidPreValencia(cfg, big.NewInt(valencia-1), raw) {
			t.Errorf("pre-Valencia must reject malformed TxDependency: %s", name)
		}
		for _, n := range []int64{valencia, valencia + 1} {
			if !txDependencyValidPreValencia(cfg, big.NewInt(n), raw) {
				t.Errorf("block %d must accept malformed TxDependency: %s", n, name)
			}
		}
	}

	for _, n := range []int64{valencia - 1, valencia, valencia + 1} {
		if !txDependencyValidPreValencia(cfg, big.NewInt(n), validTxDep) {
			t.Errorf("valid TxDependency must pass at %d", n)
		}
	}

	if txDependencyValidPreValencia(&params.ChainConfig{Bor: &params.BorConfig{}}, big.NewInt(1_000_000), rlp.RawValue{0x81, 0xff}) {
		t.Error("Valencia unset must keep malformed TxDependency rejected")
	}
}
