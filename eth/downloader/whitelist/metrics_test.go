package whitelist

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestCheckpointInvalidMetricsDoNotDecrementValidMeters(t *testing.T) {
	chainBefore := CheckpointChainMeter.Snapshot().Count()
	peerBefore := CheckpointPeerMeter.Snapshot().Count()

	reportCheckpointMetrics(false, true, false)
	reportCheckpointMetrics(false, false, true)

	if got := CheckpointChainMeter.Snapshot().Count() - chainBefore; got != 0 {
		t.Fatalf("invalid checkpoint chain changed valid meter by %d", got)
	}
	if got := CheckpointPeerMeter.Snapshot().Count() - peerBefore; got != 0 {
		t.Fatalf("invalid checkpoint peer changed valid meter by %d", got)
	}
}

func TestMilestoneMetricsOnlyCountValidResults(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	svc := NewService(db, false, 0)

	m, ok := svc.milestoneService.(*milestone)
	if !ok {
		t.Fatalf("expected milestoneService to be *milestone, got %T", svc.milestoneService)
	}

	chain := []*types.Header{{Number: big.NewInt(10)}}

	// Force the chain through the locked-milestone rejection path.
	m.finality.Lock()
	m.Locked = true
	m.LockedMilestoneNumber = 10
	m.LockedMilestoneHash = common.Hash{0x01}
	m.finality.Unlock()

	chainBefore := MilestoneChainMeter.Snapshot().Count()
	valid, err := m.IsValidChain(chain[0], chain)
	if err != nil {
		t.Fatalf("unexpected invalid-chain error: %v", err)
	}
	if valid {
		t.Fatal("expected chain to be rejected by locked milestone")
	}
	if got := MilestoneChainMeter.Snapshot().Count() - chainBefore; got != 0 {
		t.Fatalf("invalid milestone chain changed valid meter by %d", got)
	}

	// A valid result must still increment the valid-chain meter.
	m.finality.Lock()
	m.Locked = false
	m.finality.Unlock()

	chainBefore = MilestoneChainMeter.Snapshot().Count()
	valid, err = m.IsValidChain(chain[0], chain)
	if err != nil {
		t.Fatalf("unexpected valid-chain error: %v", err)
	}
	if !valid {
		t.Fatal("expected chain to be valid")
	}
	if got := MilestoneChainMeter.Snapshot().Count() - chainBefore; got != 1 {
		t.Fatalf("valid milestone chain changed valid meter by %d, want 1", got)
	}

	// Seed a milestone so peer validation compares the returned hash.
	expectedHash := common.Hash{0x01}
	m.Process(10, expectedHash)

	peerBefore := MilestonePeerMeter.Snapshot().Count()
	valid, err = m.IsValidPeer(func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error) {
		return []*types.Header{{Number: big.NewInt(10)}}, []common.Hash{{0x02}}, nil
	})
	if valid {
		t.Fatal("expected peer with mismatching milestone hash to be invalid")
	}
	if err == nil {
		t.Fatal("expected mismatching peer to return an error")
	}
	if got := MilestonePeerMeter.Snapshot().Count() - peerBefore; got != 0 {
		t.Fatalf("invalid milestone peer changed valid meter by %d", got)
	}

	// A matching peer must still increment the valid-peer meter.
	peerBefore = MilestonePeerMeter.Snapshot().Count()
	valid, err = m.IsValidPeer(func(number uint64, amount int, skip int, reverse bool) ([]*types.Header, []common.Hash, error) {
		return []*types.Header{{Number: big.NewInt(10)}}, []common.Hash{expectedHash}, nil
	})
	if err != nil {
		t.Fatalf("unexpected valid-peer error: %v", err)
	}
	if !valid {
		t.Fatal("expected peer with matching milestone hash to be valid")
	}
	if got := MilestonePeerMeter.Snapshot().Count() - peerBefore; got != 1 {
		t.Fatalf("valid milestone peer changed valid meter by %d, want 1", got)
	}
}
