// Copyright 2025 The go-ethereum Authors
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

package legacypool

import (
	"crypto/ecdsa"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// smallOccupancyConfig is testTxPoolConfig with a small combined slot ceiling,
// so the reserved-occupancy cap (a percentage of GlobalSlots+GlobalQueue) is
// cheap to reach with a handful of test transactions instead of thousands.
func smallOccupancyConfig(percent uint64) Config {
	cfg := testTxPoolConfig
	cfg.GlobalSlots = 20
	cfg.GlobalQueue = 20
	cfg.ReservedMaxOccupancyPercent = percent
	return cfg
}

// TestReservedOccupancyCapRejectsFloodPreservingNormalHeadroom is the
// regression this whole design exists to prevent: a reserved client with many
// whitelisted addresses (well beyond any single client's realistic
// whitelist, but cheap to construct here) floods the pool with zero-fee
// transactions. Once combined reserved occupancy hits
// ReservedMaxOccupancyPercent of GlobalSlots+GlobalQueue, further reserved
// transactions must be rejected with ErrReservedOccupancyExceeded — and a
// normal fee-paying sender must still be admitted throughout, proving
// normal senders' headroom is genuinely held rather than merely documented.
func TestReservedOccupancyCapRejectsFloodPreservingNormalHeadroom(t *testing.T) {
	t.Parallel()

	const numReserved = 200
	keys := make([]*ecdsa.PrivateKey, numReserved)
	addrs := make([]common.Address, numReserved)
	for i := range keys {
		keys[i], _ = crypto.GenerateKey()
		addrs[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
	}

	cfg := smallOccupancyConfig(50) // cap = (20+20)*50/100 = 20
	pool := setupReservedPoolWithConfig(cfg, big.NewInt(0), addrs...)
	defer pool.Close()

	wantCap := pool.reservedOccupancyCap()
	require.Equal(t, 20, wantCap)

	var admitted, rejected int
	for i, key := range keys {
		tx := zeroFeeTx(t, pool.chainconfig, key, 0, common.Address{0x42})
		err := pool.Add([]*types.Transaction{tx}, true)[0]
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrReservedOccupancyExceeded):
			rejected++
		default:
			t.Fatalf("reserved tx %d: unexpected error %v", i, err)
		}
	}
	require.Equal(t, wantCap, admitted, "admitted reserved transactions must exactly fill the cap")
	require.Positive(t, rejected, "the flood must eventually be rejected once the cap is hit")

	pool.mu.RLock()
	occupancy := pool.reservedOccupancy
	pool.mu.RUnlock()
	require.Equal(t, wantCap, occupancy)

	// A normal, fee-paying sender must still be admitted with reserved
	// occupancy pinned at the cap: the reserved flood never touches the
	// pool's non-reserved headroom.
	otherKey, _ := crypto.GenerateKey()
	otherAddr := crypto.PubkeyToAddress(otherKey.PublicKey)
	testAddBalance(pool, otherAddr, big.NewInt(1_000_000_000_000_000_000))

	normalTx, err := types.SignNewTx(otherKey, types.LatestSigner(pool.chainconfig), &types.DynamicFeeTx{
		ChainID:   pool.chainconfig.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(30_000_000_000),
		GasFeeCap: big.NewInt(30_000_000_000),
		Gas:       21_000,
		To:        &common.Address{0x99},
		Value:     big.NewInt(0),
	})
	require.NoError(t, err)
	require.NoError(t, pool.Add([]*types.Transaction{normalTx}, true)[0])
	require.NotNil(t, pool.Get(normalTx.Hash()), "normal sender's transaction must be admitted while reserved occupancy is saturated")
}

// bigZeroFeeTx is zeroFeeTx with calldata sized to occupy multiple pool
// slots (see numSlots), so tests can tell slot-weighted occupancy accounting
// apart from a flat per-transaction count.
func bigZeroFeeTx(t *testing.T, cfg *params.ChainConfig, key *ecdsa.PrivateKey, nonce uint64, to common.Address, dataLen int) *types.Transaction {
	t.Helper()
	return zeroFeeTxWithGasAndData(t, cfg, key, nonce, to, 2_000_000, make([]byte, dataLen))
}

// bigReplacementTx is bigZeroFeeTx with a trivial positive fallback fee, so
// it clears list.Add's same-nonce replacement threshold against a zero-fee
// incumbent (any strictly positive fee beats a 0/0 predecessor - see the
// price-bump math in list.Add) while still being large enough to span
// multiple pool slots.
func bigReplacementTx(t *testing.T, cfg *params.ChainConfig, key *ecdsa.PrivateKey, nonce uint64, to common.Address, dataLen int) *types.Transaction {
	t.Helper()
	tx, err := types.SignNewTx(key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       2_000_000,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      make([]byte, dataLen),
	})
	require.NoError(t, err)
	return tx
}

// TestReservedOccupancyCapBlocksOversizedReplacement guards the fix for a
// real cap bypass: a same-nonce replacement isn't occupancy-neutral once
// slot-weighting means the incumbent and its replacement can differ in
// size, but the admission gate used to skip the cap check for ANY
// same-nonce transaction on the pre-existing (correct-under-flat-counting,
// now-stale) assumption that a replacement never changes combined
// occupancy. A reserved sender could fill their cap with minimal 1-slot
// transactions, then replace each at a trivial cost with a maximal 4-slot
// (txMaxSize) transaction at the same nonce - every replacement skipping
// the cap check entirely - ending up occupying up to 4x the intended
// share while reservedOccupancy kept reporting exactly the cap. The
// replacement must now be checked (and, if it fits, correctly costed)
// like any other admission.
func TestReservedOccupancyCapBlocksOversizedReplacement(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	cfg := smallOccupancyConfig(50) // cap = (20+20)*50/100 = 20
	pool := setupReservedPoolWithConfig(cfg, big.NewInt(0), addr)
	defer pool.Close()
	testAddBalance(pool, addr, big.NewInt(1))

	wantCap := pool.reservedOccupancyCap()
	require.Equal(t, 20, wantCap)

	// Fill the cap exactly with minimal (1-slot) transactions at distinct
	// nonces - no calldata, well under txSlotSize.
	for i := 0; i < wantCap; i++ {
		tx := zeroFeeTx(t, pool.chainconfig, key, uint64(i), common.Address{0x42})
		require.NoError(t, pool.Add([]*types.Transaction{tx}, true)[0])
	}
	pool.mu.RLock()
	before := pool.reservedOccupancy
	pool.mu.RUnlock()
	require.Equal(t, wantCap, before)

	// Replace nonce 0's 1-slot transaction with a maximal-size one. Sized
	// well under txMaxSize (4 slots) but past 2 slots, same as the probe in
	// TestReservedOccupancyCapWeightsBySlotsNotTxCount, so the delta versus
	// the 1-slot incumbent is unambiguous.
	oversized := bigReplacementTx(t, pool.chainconfig, key, 0, common.Address{0x42}, 70_000)
	require.Equal(t, 3, numSlots(oversized), "test fixture must actually grow the occupied slot count")

	err := pool.Add([]*types.Transaction{oversized}, true)[0]
	require.ErrorIs(t, err, ErrReservedOccupancyExceeded,
		"an oversized same-nonce replacement must be checked against the cap like any other admission, not skipped outright")

	pool.mu.RLock()
	after := pool.reservedOccupancy
	pool.mu.RUnlock()
	require.Equal(t, before, after, "a rejected replacement must not silently change occupancy")
	require.Nil(t, pool.Get(oversized.Hash()), "the rejected replacement must not have replaced the incumbent")
}

// TestReservedOccupancyReplacementAppliesSizeDelta is the successful-path
// counterpart to TestReservedOccupancyCapBlocksOversizedReplacement: a
// same-nonce replacement that DOES fit under the cap must still update
// reservedOccupancy by the real size delta, not leave it stale at the
// incumbent's smaller size for the rest of the transaction's pending
// lifetime (which would let the sender's real footprint silently drift
// above what the counter reports).
func TestReservedOccupancyReplacementAppliesSizeDelta(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	// Plenty of headroom - this test is about the delta being applied at
	// all, not about hitting the cap.
	pool := setupReservedPoolWithConfig(testTxPoolConfig, big.NewInt(0), addr)
	defer pool.Close()
	testAddBalance(pool, addr, big.NewInt(1))

	small := zeroFeeTx(t, pool.chainconfig, key, 0, common.Address{0x42})
	require.NoError(t, pool.Add([]*types.Transaction{small}, true)[0])
	pool.mu.RLock()
	afterSmall := pool.reservedOccupancy
	pool.mu.RUnlock()
	require.Equal(t, numSlots(small), afterSmall)

	big3 := bigReplacementTx(t, pool.chainconfig, key, 0, common.Address{0x42}, 70_000)
	require.Greater(t, numSlots(big3), numSlots(small), "test fixture must actually grow the slot count")
	require.NoError(t, pool.Add([]*types.Transaction{big3}, true)[0])

	pool.mu.RLock()
	afterReplace := pool.reservedOccupancy
	pool.mu.RUnlock()
	require.Equal(t, numSlots(big3), afterReplace,
		"occupancy must reflect the replacement's real size, not the incumbent's stale one")

	// Layer-2 cross-check: an independent from-scratch recompute must agree.
	pool.mu.Lock()
	recomputed := pool.recomputeReservedOccupancy()
	pool.mu.Unlock()
	require.Equal(t, afterReplace, recomputed)
}

// TestReservedOccupancyCapWeightsBySlotsNotTxCount guards the fix for a real
// gap: reservedOccupancy used to bump by a flat 1 per transaction while its
// cap is computed in the pool's own slot unit (numSlots, the same one
// pool.all.Slots() uses for real fullness). A handful of large-calldata
// reserved transactions could then occupy far more of the pool's actual
// capacity than a transaction-count-based cap would ever admit — exactly the
// aggregate-starvation risk ReservedMaxOccupancyPercent exists to bound,
// just via few big transactions instead of many small ones. A single
// reserved sender submitting 3-slot transactions must get rejected once
// their combined SLOT count would exceed the cap, well before their
// transaction COUNT does.
func TestReservedOccupancyCapWeightsBySlotsNotTxCount(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	cfg := smallOccupancyConfig(50) // cap = (20+20)*50/100 = 20
	pool := setupReservedPoolWithConfig(cfg, big.NewInt(0), addr)
	defer pool.Close()

	// Gives addr a real state object (CodeHash = EmptyCodeHash) so
	// checkDelegationLimit's short-circuit takes effect; without this, a
	// never-touched address's GetCodeHash is the zero hash rather than
	// EmptyCodeHash, and the pool treats it as delegated, capping it at one
	// in-flight nonce regardless of the reserved-occupancy cap under test.
	testAddBalance(pool, addr, big.NewInt(1))

	wantCap := pool.reservedOccupancyCap()
	require.Equal(t, 20, wantCap)

	// Sized well under txMaxSize (4 slots) but comfortably past 2 slots, so
	// numSlots(tx) == 3 regardless of minor RLP/signature overhead.
	probe := bigZeroFeeTx(t, pool.chainconfig, key, 0, common.Address{0x42}, 70_000)
	slotsPerTx := numSlots(probe)
	require.Equal(t, 3, slotsPerTx, "test fixture must actually span multiple slots")

	// A flat per-transaction counter would tolerate wantCap (20) of these
	// before rejecting anything. Slot-weighted, it must reject well before
	// the transaction count reaches that: floor(20/3) = 6 fit, a 7th (21
	// slots) does not.
	maxByTxCount := wantCap
	maxBySlotCount := wantCap / slotsPerTx
	require.Less(t, maxBySlotCount, maxByTxCount, "fixture must actually distinguish the two accounting schemes")

	var admitted int
	for i := 0; i <= maxByTxCount; i++ {
		tx := bigZeroFeeTx(t, pool.chainconfig, key, uint64(i), common.Address{0x42}, 70_000)
		err := pool.Add([]*types.Transaction{tx}, true)[0]
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrReservedOccupancyExceeded):
			pool.mu.RLock()
			occupancy := pool.reservedOccupancy
			pool.mu.RUnlock()
			require.Equal(t, maxBySlotCount, admitted,
				"must reject once SLOT count would exceed the cap, not transaction count")
			require.Equal(t, maxBySlotCount*slotsPerTx, occupancy)
			return
		default:
			t.Fatalf("tx %d: unexpected error %v", i, err)
		}
	}
	t.Fatalf("flood of %d multi-slot transactions never hit the occupancy cap; slot-weighting regressed", maxByTxCount+1)
}

// TestReservedOccupancyLayerAgreement is the direct counterpart of
// TestListAggregatesConsistency for the pool-wide reserved-occupancy
// counter: after a mixed sequence of admissions, a same-nonce replacement,
// a gap-filling promotion, a demotion, and an unrelated normal-sender
// admission, the incrementally-tracked counter (Layer 1) must agree with a
// from-scratch recompute (Layer 2) at every step.
func TestReservedOccupancyLayerAgreement(t *testing.T) {
	t.Parallel()

	r1Key, _ := crypto.GenerateKey()
	r1 := crypto.PubkeyToAddress(r1Key.PublicKey)
	r2Key, _ := crypto.GenerateKey()
	r2 := crypto.PubkeyToAddress(r2Key.PublicKey)

	pool := setupReservedPoolWithConfig(testTxPoolConfig, big.NewInt(0), r1, r2)
	defer pool.Close()

	n1Key, _ := crypto.GenerateKey()
	n1Addr := crypto.PubkeyToAddress(n1Key.PublicKey)
	testAddBalance(pool, n1Addr, big.NewInt(1_000_000_000_000_000_000))
	// A materialized (non-zero-balance) account is required for both reserved
	// senders below: an account the state has never touched at all reports
	// GetCodeHash as the zero hash rather than types.EmptyCodeHash, which
	// otherwise misclassifies it as delegated and caps it at one in-flight
	// transaction — unrelated to reserved-occupancy tracking, but this test
	// needs several in-flight transactions per sender to exercise it.
	testAddBalance(pool, r1, big.NewInt(1))
	testAddBalance(pool, r2, big.NewInt(1))

	cfg := pool.chainconfig

	assertAgreement := func(step string) {
		pool.mu.Lock()
		incremental := pool.reservedOccupancy
		recomputed := pool.recomputeReservedOccupancy()
		pool.mu.Unlock()
		require.Equal(t, recomputed, incremental, "reservedOccupancy drifted from ground truth after %s", step)
	}

	// r1: pending nonce0, then a gapped nonce2 (queued), then nonce1 fills
	// the gap and promotes both nonce1 and nonce2 to pending.
	tx0 := zeroFeeTx(t, cfg, r1Key, 0, common.Address{0x1})
	require.NoError(t, pool.Add([]*types.Transaction{tx0}, true)[0])
	assertAgreement("r1 nonce0 admission")

	tx2 := zeroFeeTx(t, cfg, r1Key, 2, common.Address{0x1})
	require.NoError(t, pool.Add([]*types.Transaction{tx2}, true)[0])
	assertAgreement("r1 nonce2 admission (gapped, queued)")

	tx1 := zeroFeeTx(t, cfg, r1Key, 1, common.Address{0x1})
	require.NoError(t, pool.Add([]*types.Transaction{tx1}, true)[0])
	assertAgreement("r1 nonce1 admission (fills the gap, promotes nonce1+nonce2)")

	// r1: same-nonce replacement of nonce0 with a positive fallback fee.
	replacement, err := types.SignNewTx(r1Key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 100_000, To: &common.Address{0x2}, Value: big.NewInt(0),
	})
	require.NoError(t, err)
	require.NoError(t, pool.Add([]*types.Transaction{replacement}, true)[0])
	assertAgreement("r1 nonce0 replacement")

	// r2: two pending transactions, then simulate nonce0 having been mined
	// (advance the on-chain nonce) and run demoteUnexecutables directly —
	// nonce0 is forwarded away outright while nonce1 stays pending.
	txR2a := zeroFeeTx(t, cfg, r2Key, 0, common.Address{0x3})
	txR2b := zeroFeeTx(t, cfg, r2Key, 1, common.Address{0x3})
	errs := pool.Add([]*types.Transaction{txR2a, txR2b}, true)
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assertAgreement("r2 pending admissions")

	testSetNonce(pool, r2, 1)
	pool.mu.Lock()
	pool.demoteUnexecutables()
	pool.mu.Unlock()
	assertAgreement("r2 demoteUnexecutables (forwards nonce0 away)")

	// An unrelated normal sender's admission must not perturb reserved
	// bookkeeping either way.
	normalTx, err := types.SignNewTx(n1Key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(30_000_000_000), GasFeeCap: big.NewInt(30_000_000_000),
		Gas: 21_000, To: &common.Address{0x4}, Value: big.NewInt(0),
	})
	require.NoError(t, err)
	require.NoError(t, pool.Add([]*types.Transaction{normalTx}, true)[0])
	assertAgreement("unrelated normal sender admission")

	// Deregistering r1 and forcing the Layer-2 recompute (mirroring what the
	// next real head event does) must also leave the two layers agreeing.
	deregisterReserved(t, pool, r1)
	pool.mu.Lock()
	pool.reconcileReservedOccupancy()
	pool.mu.Unlock()
	assertAgreement("r1 deregistration + Layer-2 recompute")
}

// TestReservedOccupancyPromotionRejectionDecrementsOnce pins §3.3's trickiest
// row: a queued transaction handed to promoteTx (as queue.promoteExecutables
// hands it every one of its readies) but rejected because a better
// transaction already occupies that pending nonce is a genuine loss for
// combined occupancy — it left the queue bucket but never entered the
// pending one — so the counter must decrement by exactly one, not zero.
func TestReservedOccupancyPromotionRejectionDecrementsOnce(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	cfg := pool.chainconfig

	better, err := types.SignNewTx(key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(10), GasFeeCap: big.NewInt(10),
		Gas: 100_000, To: &common.Address{0x1}, Value: big.NewInt(0),
	})
	require.NoError(t, err)
	require.NoError(t, pool.Add([]*types.Transaction{better}, true)[0])

	worse, err := types.SignNewTx(key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID: cfg.ChainID, Nonce: 0, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(1),
		Gas: 100_000, To: &common.Address{0x2}, Value: big.NewInt(0),
	})
	require.NoError(t, err)

	pool.mu.Lock()
	defer pool.mu.Unlock()

	before := pool.reservedOccupancy
	require.Equal(t, 1, before)

	// promoteTx assumes its tx is already tracked in `all` — exactly the
	// invariant queue.promoteExecutables' readies satisfy in the real path.
	pool.all.Add(worse)
	inserted := pool.promoteTx(addr, worse.Hash(), worse)
	require.False(t, inserted, "the worse transaction must be rejected in favor of the pending incumbent")
	require.Equal(t, before-1, pool.reservedOccupancy, "a promotion rejection must decrement the counter by exactly one")
}

// TestReservedOccupancyPendingQueueRoundTripIsNetZero pins that a pending->
// queue demotion and its queue->pending re-promotion never move
// reservedOccupancy, at any point in the round trip: the transaction is
// always in exactly one of the two buckets the combined counter tracks
// together.
func TestReservedOccupancyPendingQueueRoundTripIsNetZero(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	tx := zeroFeeTx(t, pool.chainconfig, key, 0, common.Address{0x1})
	require.NoError(t, pool.Add([]*types.Transaction{tx}, true)[0])

	pool.mu.Lock()
	before := pool.reservedOccupancy
	require.Equal(t, 1, before)

	// Demote: pending -> queue via the same addAll=false path
	// demoteUnexecutables/removeTx use for invalids/gapped transactions.
	pending := pool.pending[addr]
	removed, _ := pending.Remove(tx)
	require.True(t, removed)
	if pending.Empty() {
		delete(pool.pending, addr)
	}
	pool.enqueueTx(tx.Hash(), tx, false)
	require.Equal(t, before, pool.reservedOccupancy, "pending->queue demotion must be net zero")
	pool.mu.Unlock()

	// Re-promote: queue -> pending via the normal promotion pipeline.
	pool.pendingNonces.setIfLower(addr, tx.Nonce())
	pool.mu.Lock()
	pool.promoteExecutables([]common.Address{addr})
	require.Equal(t, before, pool.reservedOccupancy, "queue->pending re-promotion must be net zero")
	pool.mu.Unlock()

	require.NotNil(t, pool.Get(tx.Hash()))
	require.Equal(t, txpool.TxStatusPending, pool.Status(tx.Hash()))
}

// TestReservedOccupancyDeregistrationPurgedByRecompute pins the
// deregistration edge case: a sender's occupied slots stop counting toward
// the cap only once purged by the next reset()'s Layer-2 recompute (the
// admission-time counter doesn't retroactively change just because the
// registry snapshot flipped), and once purged, a still-reserved sender is
// not double-penalized by the deregistered one's stale contribution.
func TestReservedOccupancyDeregistrationPurgedByRecompute(t *testing.T) {
	t.Parallel()

	r1Key, _ := crypto.GenerateKey()
	r1 := crypto.PubkeyToAddress(r1Key.PublicKey)
	r2Key, _ := crypto.GenerateKey()
	r2 := crypto.PubkeyToAddress(r2Key.PublicKey)

	pool := setupReservedPoolWithConfig(testTxPoolConfig, big.NewInt(0), r1, r2)
	defer pool.Close()
	// r2 needs a materialized account: it gets a second in-flight
	// transaction below, and an untouched account misclassifies as
	// delegated (see the identical note in TestReservedOccupancyLayerAgreement).
	testAddBalance(pool, r2, big.NewInt(1))

	tx1 := zeroFeeTx(t, pool.chainconfig, r1Key, 0, common.Address{0x1})
	require.NoError(t, pool.Add([]*types.Transaction{tx1}, true)[0])
	tx2 := zeroFeeTx(t, pool.chainconfig, r2Key, 0, common.Address{0x2})
	require.NoError(t, pool.Add([]*types.Transaction{tx2}, true)[0])

	pool.mu.RLock()
	before := pool.reservedOccupancy
	pool.mu.RUnlock()
	require.Equal(t, 2, before)

	// Deregister r1: the registry snapshot flips immediately, but tx1 is
	// still physically sitting in the pool (eviction hasn't caught up) —
	// admission-time bookkeeping is unaffected until the next recompute.
	deregisterReserved(t, pool, r1)

	pool.mu.Lock()
	require.Equal(t, before, pool.reservedOccupancy, "deregistration alone must not change the counter before a recompute")
	pool.reconcileReservedOccupancy()
	after := pool.reservedOccupancy
	pool.mu.Unlock()
	require.Equal(t, 1, after, "r1's slot must be purged from the cap once it is no longer reserved")

	// r2 must not be double-penalized by r1's stale contribution: a fresh
	// reserved transaction for r2 is still admitted against the corrected figure.
	tx2b := zeroFeeTx(t, pool.chainconfig, r2Key, 1, common.Address{0x2})
	require.NoError(t, pool.Add([]*types.Transaction{tx2b}, true)[0])
}

// TestSanitizeReservedMaxOccupancyPercent pins sanitize()'s clamp: zero
// (whether explicitly set or left at its unset zero value), and any value
// over 100, both fall back to the default; any value in (0,100] is honored
// unchanged.
func TestSanitizeReservedMaxOccupancyPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   uint64
		want uint64
	}{
		{"unset (zero value) falls back to default", 0, DefaultConfig.ReservedMaxOccupancyPercent},
		{"explicit zero falls back to default", 0, DefaultConfig.ReservedMaxOccupancyPercent},
		{"over 100 falls back to default", 150, DefaultConfig.ReservedMaxOccupancyPercent},
		{"boundary value 1 is honored", 1, 1},
		{"boundary value 100 is honored", 100, 100},
		{"typical valid value is honored", 75, 75},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{ReservedMaxOccupancyPercent: tt.in}
			got := cfg.sanitize()
			require.Equal(t, tt.want, got.ReservedMaxOccupancyPercent)
		})
	}
}

// TestReservedOccupancyConcurrentAdmissionRace extends the reserved -race
// suite to the new counter: many reserved senders admit transactions
// concurrently while a separate goroutine repeatedly rebuilds the reserved
// snapshot (mirroring a real head event racing concurrent pool.Add calls,
// as TestReservedSnapshotSwapRace already does for the snapshot pointer
// alone), exercising reservedOccupancy's read/write sites under -race.
func TestReservedOccupancyConcurrentAdmissionRace(t *testing.T) {
	const numSenders = 8

	keys := make([]*ecdsa.PrivateKey, numSenders)
	addrs := make([]common.Address, numSenders)
	for i := range keys {
		keys[i], _ = crypto.GenerateKey()
		addrs[i] = crypto.PubkeyToAddress(keys[i].PublicKey)
	}

	cfg := smallOccupancyConfig(50)
	pool := setupReservedPoolWithConfig(cfg, big.NewInt(0), addrs...)
	defer pool.Close()
	for _, addr := range addrs {
		// Materialize each account: each sender submits many sequential
		// in-flight nonces below, and an untouched account misclassifies as
		// delegated (see the identical note in TestReservedOccupancyLayerAgreement).
		testAddBalance(pool, addr, big.NewInt(1))
	}

	var refresherWG sync.WaitGroup
	stop := make(chan struct{})
	refresherWG.Add(1)
	go func() {
		defer refresherWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				refreshReservedSnapshot(pool)
			}
		}
	}()

	var sendersWG sync.WaitGroup
	for i, key := range keys {
		sendersWG.Add(1)
		go func(i int, key *ecdsa.PrivateKey) {
			defer sendersWG.Done()
			for n := 0; n < 50; n++ {
				tx := zeroFeeTx(t, pool.chainconfig, key, uint64(n), common.Address{byte(i)})
				pool.Add([]*types.Transaction{tx}, false)
			}
		}(i, key)
	}
	sendersWG.Wait()
	close(stop)
	refresherWG.Wait()
}
