package eip1559

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// reservedFeeConfig builds a London+Cancun bor config with the reserved fork
// gated at reservedBlock (nil = never). Pre-Dandeli, so the gas target is
// gasLimit/elasticity (÷2), which keeps the arithmetic in the tests exact and
// readable. Capacity is no longer a config concern (§2.2/§2.3): it arrives via
// the parent header's ReservedCapacity field, set by the caller on the parent
// passed to CalcBaseFee.
func reservedFeeConfig(reservedBlock *big.Int) *params.ChainConfig {
	return &params.ChainConfig{
		ChainID:             big.NewInt(80003),
		HomesteadBlock:      big.NewInt(0),
		EIP150Block:         big.NewInt(0),
		EIP155Block:         big.NewInt(0),
		EIP158Block:         big.NewInt(0),
		ByzantiumBlock:      big.NewInt(0),
		ConstantinopleBlock: big.NewInt(0),
		PetersburgBlock:     big.NewInt(0),
		IstanbulBlock:       big.NewInt(0),
		MuirGlacierBlock:    big.NewInt(0),
		BerlinBlock:         big.NewInt(0),
		LondonBlock:         big.NewInt(0),
		ShanghaiBlock:       big.NewInt(0),
		CancunBlock:         big.NewInt(0),
		Bor: &params.BorConfig{
			Period:                  map[string]uint64{"0": 1},
			Sprint:                  map[string]uint64{"0": 64},
			ReservedBlockspaceBlock: reservedBlock,
		},
	}
}

// extraWithReservedFields encodes a header Extra carrying the reserved-region
// fields. The two preceding optional fields (GasTarget, BaseFeeChangeDenominator)
// must be non-nil for RLP to emit the trailing reserved optionals. capacity nil
// means ReservedCapacity is absent from the wire (mirroring a pre-activation
// or otherwise malformed header); gasUsed is always written when capacity is,
// since the two are stamped together in production.
func extraWithReservedFields(t *testing.T, gasUsed uint64, capacity *uint64) []byte {
	t.Helper()
	zero := uint64(0)
	enc, err := rlp.EncodeToBytes(&types.BlockExtraData{
		GasTarget:                &zero,
		BaseFeeChangeDenominator: &zero,
		ReservedGasUsed:          &gasUsed,
		ReservedCapacity:         capacity,
	})
	require.NoError(t, err)

	extra := make([]byte, types.ExtraVanityLength)
	extra = append(extra, enc...)
	extra = append(extra, make([]byte, types.ExtraSealLength)...)
	return extra
}

// extraWithReserved is the capacity-only convenience form used by tests that
// don't care about ReservedGasUsed netting.
func extraWithReserved(t *testing.T, capacity uint64) []byte {
	t.Helper()
	return extraWithReservedFields(t, 0, &capacity)
}

// TestReservedBaseFee_CapacityReducesTarget pins the capacity anchor: the
// public gas target excludes the reserved quotas (read from the parent
// header), so a block whose usage lands exactly on the reduced target holds
// the base fee steady — whereas with the fork inactive the same usage sits
// below the full target and the fee drops.
func TestReservedBaseFee_CapacityReducesTarget(t *testing.T) {
	t.Parallel()

	const gasLimit = 60_000_000
	const capacity = 20_000_000
	baseFee := big.NewInt(params.InitialBaseFee)

	// Public target = (gasLimit - capacity) / 2 = 20M.
	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  (gasLimit - capacity) / 2,
		BaseFee:  baseFee,
		Extra:    extraWithReserved(t, capacity),
	}

	active := reservedFeeConfig(big.NewInt(0))
	if got := CalcBaseFee(active, parent); got.Cmp(baseFee) != 0 {
		t.Errorf("reserved-active base fee = %s, want unchanged %s (usage == public target)", got, baseFee)
	}

	// Fork inactive: full target = gasLimit/2 = 30M, so 20M usage is below
	// target and the base fee must fall.
	inactive := reservedFeeConfig(nil)
	if got := CalcBaseFee(inactive, parent); got.Cmp(baseFee) >= 0 {
		t.Errorf("reserved-inactive base fee = %s, want < %s (usage below full target)", got, baseFee)
	}
}

// TestReservedBaseFee_NetsReservedGasUsed pins the used-side netting: the
// parent's ReservedGasUsed is subtracted from gas used before the controller
// runs, so reserved consumption doesn't move the public base fee.
func TestReservedBaseFee_NetsReservedGasUsed(t *testing.T) {
	t.Parallel()

	const gasLimit = 60_000_000
	const capacity = 20_000_000
	baseFee := big.NewInt(params.InitialBaseFee)
	cfg := reservedFeeConfig(big.NewInt(0))

	// Public target = (60M - 20M)/2 = 20M. Total used 35M, of which 15M is
	// reserved → public used = 20M == target → base fee unchanged.
	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  35_000_000,
		BaseFee:  baseFee,
		Extra:    extraWithReservedFields(t, 15_000_000, ptr(uint64(capacity))),
	}
	if got := CalcBaseFee(cfg, parent); got.Cmp(baseFee) != 0 {
		t.Errorf("base fee = %s, want unchanged %s (public used == target after netting)", got, baseFee)
	}

	// Same total usage but zero reserved gas: public used = 35M > 20M target,
	// so the base fee must rise. Proves the netting is what held it steady above.
	parentNoReserved := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  35_000_000,
		BaseFee:  baseFee,
		Extra:    extraWithReservedFields(t, 0, ptr(uint64(capacity))),
	}
	if got := CalcBaseFee(cfg, parentNoReserved); got.Cmp(baseFee) <= 0 {
		t.Errorf("base fee = %s, want > %s (full usage above target without netting)", got, baseFee)
	}
}

// TestReservedBaseFee_CapacityExceedsLimitFallsBack guards the liveness path
// (§2.2): if the registry's effective capacity is validly at or above the
// block gas limit, the target falls back to the full-limit curve instead of
// going to zero (which would divide by zero) or negative.
func TestReservedBaseFee_CapacityExceedsLimitFallsBack(t *testing.T) {
	t.Parallel()

	const gasLimit = 30_000_000
	baseFee := big.NewInt(params.InitialBaseFee)
	cfg := reservedFeeConfig(big.NewInt(0))

	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  gasLimit / 2, // == full target → unchanged
		BaseFee:  baseFee,
		Extra:    extraWithReserved(t, gasLimit+1), // capacity > limit
	}

	var got *big.Int
	require.NotPanics(t, func() { got = CalcBaseFee(cfg, parent) },
		"CalcBaseFee must not panic when reserved capacity >= gas limit")
	if got.Cmp(baseFee) != 0 {
		t.Errorf("fallback base fee = %s, want unchanged %s (full target)", got, baseFee)
	}
}

// TestReservedBaseFee_CapacityEqualsLimitFallsBack pins the >= boundary in
// reservedAwareGasTarget: when reserved capacity equals the gas limit the public
// target would be zero, so the guard must fall back to the full target (not
// divide by a zero target). Distinguishes >= from > at the exact boundary.
func TestReservedBaseFee_CapacityEqualsLimitFallsBack(t *testing.T) {
	t.Parallel()

	const gasLimit = 30_000_000
	baseFee := big.NewInt(params.InitialBaseFee)
	cfg := reservedFeeConfig(big.NewInt(0))

	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  gasLimit / 2, // == full target → unchanged
		BaseFee:  baseFee,
		Extra:    extraWithReserved(t, gasLimit), // capacity == limit
	}

	var got *big.Int
	require.NotPanics(t, func() { got = CalcBaseFee(cfg, parent) },
		"CalcBaseFee must not panic when reserved capacity == gas limit")
	if got.Cmp(baseFee) != 0 {
		t.Errorf("fallback base fee = %s, want unchanged %s (full target)", got, baseFee)
	}
}

// TestReservedBaseFee_NilCapacityFallsBack covers the defensive nil branch:
// a post-fork parent that (abnormally) carries no ReservedCapacity field
// prices against the full target rather than panicking on a nil dereference.
// Validated headers always carry the field post-fork; this only guards
// CalcBaseFee itself, which must stay total for any header it's handed.
func TestReservedBaseFee_NilCapacityFallsBack(t *testing.T) {
	t.Parallel()

	const gasLimit = 60_000_000
	baseFee := big.NewInt(params.InitialBaseFee)
	cfg := reservedFeeConfig(big.NewInt(0))

	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  gasLimit / 2, // == full target → unchanged
		BaseFee:  baseFee,
		Extra:    extraWithReservedFields(t, 0, nil), // ReservedCapacity absent
	}

	var got *big.Int
	require.NotPanics(t, func() { got = CalcBaseFee(cfg, parent) },
		"CalcBaseFee must not panic on a missing ReservedCapacity field")
	if got.Cmp(baseFee) != 0 {
		t.Errorf("fallback base fee = %s, want unchanged %s (full target)", got, baseFee)
	}
}

// TestReservedBaseFee_ZeroCapacity pins capacity = 0: the public target
// equals the full-limit curve (excluding nothing), same result as the fork
// being inactive, but exercised through the active reserved-aware branch.
func TestReservedBaseFee_ZeroCapacity(t *testing.T) {
	t.Parallel()

	const gasLimit = 60_000_000
	baseFee := big.NewInt(params.InitialBaseFee)
	cfg := reservedFeeConfig(big.NewInt(0))

	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: gasLimit,
		GasUsed:  gasLimit / 2, // == full target (gasLimit/2) → unchanged
		BaseFee:  baseFee,
		Extra:    extraWithReserved(t, 0),
	}

	if got := CalcBaseFee(cfg, parent); got.Cmp(baseFee) != 0 {
		t.Errorf("base fee = %s, want unchanged %s (zero capacity excludes nothing)", got, baseFee)
	}
}

// TestReservedBaseFee_ForkBoundary is the N-1/N/N+1 matrix, from the
// perspective of the block being PRICED (not its parent): CalcBaseFee for
// block M reads parent(M-1)'s reserved fields, gated on
// IsReservedBlockspace(parent.Number). Pricing the fork-activation block
// itself (M = fork) uses the full target since its parent (fork-1, pre-fork)
// carries no reserved fields; only pricing fork+1 onward reads the parent's
// stamped capacity.
func TestReservedBaseFee_ForkBoundary(t *testing.T) {
	t.Parallel()

	const gasLimit = 60_000_000
	const capacity = 20_000_000
	const forkBlock = 100
	baseFee := big.NewInt(params.InitialBaseFee)
	cfg := reservedFeeConfig(big.NewInt(forkBlock))

	// Usage that lands on the REDUCED (capacity-aware) target: (60M-20M)/2 = 20M.
	// Under the full-limit curve (fork inactive for this parent) that usage is
	// below target (30M), so the fee would fall; under the reduced target it's
	// exactly on target, so the fee holds steady. This makes the two regimes
	// observably different.
	usage := uint64((gasLimit - capacity) / 2)

	tests := []struct {
		name        string
		pricedBlock int64 // the block CalcBaseFee computes the fee FOR
		parentExtra []byte
		wantSteady  bool // true: base fee unchanged; false: base fee must fall
	}{
		{
			name:        "priced block N (=fork): parent N-1 is pre-fork, full target",
			pricedBlock: forkBlock,
			parentExtra: nil,
			wantSteady:  false,
		},
		{
			name:        "priced block N+1: parent N is the fork-activation block, reserved-aware",
			pricedBlock: forkBlock + 1,
			parentExtra: extraWithReserved(t, capacity),
			wantSteady:  true,
		},
		{
			name:        "priced block N+2: parent N+1 is post-fork, reserved-aware",
			pricedBlock: forkBlock + 2,
			parentExtra: extraWithReserved(t, capacity),
			wantSteady:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &types.Header{
				Number:   big.NewInt(tt.pricedBlock - 1),
				GasLimit: gasLimit,
				GasUsed:  usage,
				BaseFee:  baseFee,
				Extra:    tt.parentExtra,
			}
			got := CalcBaseFee(cfg, parent)
			if tt.wantSteady {
				if got.Cmp(baseFee) != 0 {
					t.Errorf("base fee = %s, want unchanged %s (reserved-aware target)", got, baseFee)
				}
			} else if got.Cmp(baseFee) >= 0 {
				t.Errorf("base fee = %s, want < %s (full target, usage below it)", got, baseFee)
			}
		})
	}
}

// TestReservedBaseFee_VerifyAcceptsProducerValue checks that the reserved-aware
// base fee a producer computes passes strict (pre-Lisovo) header verification,
// since VerifyEIP1559Header recomputes via CalcBaseFee on that path.
func TestReservedBaseFee_VerifyAcceptsProducerValue(t *testing.T) {
	t.Parallel()

	cfg := reservedFeeConfig(big.NewInt(0)) // Lisovo nil → strict verify
	baseFee := big.NewInt(params.InitialBaseFee)
	parent := &types.Header{
		Number:   big.NewInt(1),
		GasLimit: 60_000_000,
		GasUsed:  50_000_000,
		BaseFee:  baseFee,
		Extra:    extraWithReserved(t, 20_000_000),
	}
	expected := CalcBaseFee(cfg, parent)
	child := &types.Header{
		Number:   big.NewInt(2),
		GasLimit: 60_000_000,
		GasUsed:  0,
		BaseFee:  expected,
	}
	require.NoError(t, VerifyEIP1559Header(cfg, parent, child),
		"strict verification must accept the reserved-aware base fee")
}

func ptr(v uint64) *uint64 { return &v }
