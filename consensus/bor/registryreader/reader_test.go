package registryreader

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
)

// mockReader is a controllable Reader for exercising BuildSnapshot and the
// Snapshot accessors without an EVM-backed registry.
type mockReader struct {
	has         bool
	root        common.Hash
	whitelist   []common.Address
	totalGas    uint64
	clients     map[common.Address]ClientLookup
	rootErr     error
	wlErr       error
	totalErr    error
	clientErr   error
	clientCalls int
}

func (m *mockReader) HasReservedRegistry() bool { return m.has }

func (m *mockReader) IsReservedAddress(_ *state.StateDB, _ uint64, _ common.Hash, _ common.Address) (bool, error) {
	return false, nil
}

func (m *mockReader) ReservedClientForAddress(_ *state.StateDB, _ uint64, _ common.Hash, a common.Address) (ClientLookup, error) {
	m.clientCalls++
	if m.clientErr != nil {
		return ClientLookup{}, m.clientErr
	}
	return m.clients[a], nil
}

func (m *mockReader) Root(_ *state.StateDB, _ uint64, _ common.Hash) (common.Hash, error) {
	return m.root, m.rootErr
}

func (m *mockReader) WhitelistedAddresses(_ *state.StateDB, _ uint64, _ common.Hash) ([]common.Address, error) {
	return m.whitelist, m.wlErr
}

func (m *mockReader) TotalReservedGas(_ *state.StateDB, _ uint64, _ common.Hash) (uint64, error) {
	return m.totalGas, m.totalErr
}

func addr(b byte) common.Address { return common.Address{19: b} }

func TestBuildSnapshot(t *testing.T) {
	errBoom := errors.New("boom")
	a1, a2 := addr(1), addr(2)
	base := func() *mockReader {
		return &mockReader{
			has:       true,
			root:      common.HexToHash("0xabc"),
			whitelist: []common.Address{a1, a2},
			totalGas:  60_000_000,
			clients: map[common.Address]ClientLookup{
				a1: {ClientID: big.NewInt(1), GasQuota: 30_000_000, Active: true},
				a2: {ClientID: big.NewInt(2), GasQuota: 30_000_000, Active: true},
			},
		}
	}

	t.Run("nil reader yields nil snapshot", func(t *testing.T) {
		snap, err := BuildSnapshot(nil, nil, 1, common.Hash{}, 2)
		if err != nil || snap != nil {
			t.Fatalf("snap=%v err=%v, want nil,nil", snap, err)
		}
	})

	t.Run("registry not configured yields nil snapshot", func(t *testing.T) {
		snap, err := BuildSnapshot(&mockReader{has: false}, nil, 1, common.Hash{}, 2)
		if err != nil || snap != nil {
			t.Fatalf("snap=%v err=%v, want nil,nil", snap, err)
		}
	})

	t.Run("happy path populates root, capacity and clients", func(t *testing.T) {
		r := base()
		snap, err := BuildSnapshot(r, nil, 7, common.Hash{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		if snap.Root() != r.root {
			t.Errorf("root=%s, want %s", snap.Root(), r.root)
		}
		if snap.Capacity() != 60_000_000 {
			t.Errorf("capacity=%d, want 60000000", snap.Capacity())
		}
		if snap.EffectiveCapacity() != 60_000_000 {
			t.Errorf("effectiveCapacity=%d, want 60000000 (no future-effective client in this fixture)", snap.EffectiveCapacity())
		}
		if r.clientCalls != len(r.whitelist) {
			t.Errorf("client lookups=%d, want %d", r.clientCalls, len(r.whitelist))
		}
		if !snap.IsReserved(a1) || !snap.IsReserved(a2) {
			t.Error("both whitelisted addresses should be reserved")
		}
		if id, ok := snap.Lookup(a1); !ok || id != 1 {
			t.Errorf("Lookup(a1)=(%d,%v), want (1,true)", id, ok)
		}
		if got := snap.Quota(2); got != 30_000_000 {
			t.Errorf("Quota(2)=%d, want 30000000", got)
		}
		if ids := snap.Clients(); len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
			t.Errorf("Clients()=%v, want [1 2] sorted", ids)
		}
	})

	for _, tc := range []struct {
		name  string
		mutta func(*mockReader)
	}{
		{"root error", func(m *mockReader) { m.rootErr = errBoom }},
		{"whitelist error", func(m *mockReader) { m.wlErr = errBoom }},
		{"total gas error", func(m *mockReader) { m.totalErr = errBoom }},
		{"per-client error", func(m *mockReader) { m.clientErr = errBoom }},
	} {
		t.Run(tc.name+" propagates", func(t *testing.T) {
			r := base()
			tc.mutta(r)
			snap, err := BuildSnapshot(r, nil, 7, common.Hash{}, 8)
			if !errors.Is(err, errBoom) {
				t.Fatalf("err=%v, want boom", err)
			}
			if snap != nil {
				t.Errorf("snap=%v, want nil on error", snap)
			}
		})
	}
}

// TestBuildSnapshotEffectiveFiltering pins the build-time activation
// resolution: inactive clients and clients whose effectiveFrom is beyond the
// snapshot's effectiveAt block never enter the stored set, so lookups need no
// block number.
func TestBuildSnapshotEffectiveFiltering(t *testing.T) {
	a1, a2 := addr(1), addr(2)
	reader := func() *mockReader {
		return &mockReader{
			has:       true,
			root:      common.HexToHash("0xabc"),
			whitelist: []common.Address{a1, a2},
			totalGas:  60_000_000,
			clients: map[common.Address]ClientLookup{
				a1: {ClientID: big.NewInt(1), GasQuota: 30_000_000, Active: true, EffectiveFrom: 100},
				a2: {ClientID: big.NewInt(2), GasQuota: 30_000_000, Active: false},
			},
		}
	}

	tests := []struct {
		name        string
		effectiveAt uint64
		wantA1      bool
	}{
		{"before effectiveFrom", 99, false},
		{"exactly at effectiveFrom", 100, true},
		{"after effectiveFrom", 101, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := BuildSnapshot(reader(), nil, tt.effectiveAt-1, common.Hash{}, tt.effectiveAt)
			if err != nil {
				t.Fatal(err)
			}
			if got := snap.IsReserved(a1); got != tt.wantA1 {
				t.Errorf("IsReserved(a1) at %d = %v, want %v", tt.effectiveAt, got, tt.wantA1)
			}
			if snap.IsReserved(a2) {
				t.Error("inactive client must never be reserved")
			}
			if snap.IsReserved(addr(9)) {
				t.Error("unknown account must never be reserved")
			}
		})
	}
}

// TestBuildSnapshotEffectiveCapacityExcludesFutureClient pins the capacity
// split from §2.2: the registry's totalReservedGas (Capacity) is bumped by
// createClient immediately, including for a client whose effectiveFrom is
// still ahead, while EffectiveCapacity — the value the header stamps — only
// sums quotas of clients this snapshot actually classifies.
func TestBuildSnapshotEffectiveCapacityExcludesFutureClient(t *testing.T) {
	a1, a2 := addr(1), addr(2)
	r := &mockReader{
		has:       true,
		root:      common.HexToHash("0xabc"),
		whitelist: []common.Address{a1, a2},
		totalGas:  50_000_000, // raw total already counts both clients.
		clients: map[common.Address]ClientLookup{
			a1: {ClientID: big.NewInt(1), GasQuota: 30_000_000, Active: true},
			a2: {ClientID: big.NewInt(2), GasQuota: 20_000_000, Active: true, EffectiveFrom: 100},
		},
	}

	// Before a2's effectiveFrom: raw capacity still counts it, effective
	// capacity does not.
	snap, err := BuildSnapshot(r, nil, 49, common.Hash{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Capacity(); got != 50_000_000 {
		t.Errorf("Capacity()=%d, want 50000000 (raw total includes the future client)", got)
	}
	if got := snap.EffectiveCapacity(); got != 30_000_000 {
		t.Errorf("EffectiveCapacity()=%d, want 30000000 (future client excluded)", got)
	}
	if snap.IsReserved(a2) {
		t.Error("a2 must not classify as reserved before its effectiveFrom")
	}

	// At and after the boundary: a2 joins the effective set without any new
	// registry transaction landing in the block.
	snap, err = BuildSnapshot(r, nil, 99, common.Hash{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Capacity(); got != 50_000_000 {
		t.Errorf("Capacity()=%d, want 50000000", got)
	}
	if got := snap.EffectiveCapacity(); got != 50_000_000 {
		t.Errorf("EffectiveCapacity()=%d, want 50000000 (future client now effective)", got)
	}
	if !snap.IsReserved(a2) {
		t.Error("a2 must classify as reserved at its effectiveFrom boundary")
	}
}

func TestBuildSnapshotRejectsInvalidClientID(t *testing.T) {
	a1 := addr(1)
	r := &mockReader{
		has:       true,
		whitelist: []common.Address{a1},
		clients: map[common.Address]ClientLookup{
			a1: {ClientID: nil, GasQuota: 1, Active: true},
		},
	}
	if _, err := BuildSnapshot(r, nil, 1, common.Hash{}, 2); err == nil {
		t.Fatal("expected error for nil client id")
	}
}

func TestSnapshotNilSafe(t *testing.T) {
	var snap *Snapshot
	if snap.IsReserved(addr(1)) {
		t.Error("nil snapshot must not classify anything as reserved")
	}
	if _, ok := snap.Lookup(addr(1)); ok {
		t.Error("nil snapshot Lookup must miss")
	}
	if snap.Quota(1) != 0 {
		t.Error("nil snapshot Quota must be 0")
	}
	if snap.Clients() != nil {
		t.Error("nil snapshot Clients must be nil")
	}
	if snap.Capacity() != 0 {
		t.Error("nil snapshot Capacity must be 0")
	}
	if snap.EffectiveCapacity() != 0 {
		t.Error("nil snapshot EffectiveCapacity must be 0")
	}
	if snap.Root() != (common.Hash{}) {
		t.Error("nil snapshot Root must be zero hash")
	}
}

// TestBuildSnapshotExcludesNonFreeFeeMode pins the fee-mode gate: only
// free-mode (feeMode 0) clients enter the effective set; routed (1) and any
// future mode classify as normal senders (see FeeModeFree).
func TestBuildSnapshotExcludesNonFreeFeeMode(t *testing.T) {
	a1, a2, a3 := addr(1), addr(2), addr(3)
	r := &mockReader{
		has:       true,
		root:      common.HexToHash("0xabc"),
		whitelist: []common.Address{a1, a2, a3},
		totalGas:  60_000_000, // raw total counts every active client, any fee mode.
		clients: map[common.Address]ClientLookup{
			a1: {ClientID: big.NewInt(1), GasQuota: 30_000_000, Active: true, FeeMode: FeeModeFree},
			a2: {ClientID: big.NewInt(2), GasQuota: 20_000_000, Active: true, FeeMode: 1},
			a3: {ClientID: big.NewInt(3), GasQuota: 10_000_000, Active: true, FeeMode: 7},
		},
	}

	snap, err := BuildSnapshot(r, nil, 7, common.Hash{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.IsReserved(a1) {
		t.Error("free-mode client must be reserved")
	}
	if snap.IsReserved(a2) {
		t.Error("routed-mode client must not be reserved")
	}
	if snap.IsReserved(a3) {
		t.Error("unknown-fee-mode client must not be reserved")
	}
	if got := snap.Capacity(); got != 60_000_000 {
		t.Errorf("Capacity()=%d, want 60000000 (raw total unchanged)", got)
	}
	if got := snap.EffectiveCapacity(); got != 30_000_000 {
		t.Errorf("EffectiveCapacity()=%d, want 30000000 (non-free clients excluded)", got)
	}
	if ids := snap.Clients(); len(ids) != 1 || ids[0] != 1 {
		t.Errorf("Clients()=%v, want [1]", ids)
	}
}
