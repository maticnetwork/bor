package miner

import (
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
)

// testClient describes one reserved client for newTestSnapshot.
type testClient struct {
	ID       uint64
	Senders  []common.Address
	QuotaGas uint64
}

// newTestSnapshot builds a registry snapshot for sequencing tests. capacity 0
// means uncapped (effectiveCeilingGas normalizes it to MaxUint64).
func newTestSnapshot(capacity uint64, clients []testClient) *registryreader.Snapshot {
	m := make(map[common.Address]registryreader.Client)
	for _, c := range clients {
		for _, a := range c.Senders {
			m[a] = registryreader.Client{ID: c.ID, GasQuota: c.QuotaGas}
		}
	}
	return registryreader.NewSnapshot(common.Hash{}, capacity, m)
}

func TestOrderClients(t *testing.T) {
	t.Parallel()

	h1 := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	h2 := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")

	cases := []struct {
		name    string
		parent  common.Hash
		ids     []uint64
		wantLen int
	}{
		{name: "empty", parent: h1, ids: nil, wantLen: 0},
		{name: "single", parent: h1, ids: []uint64{42}, wantLen: 1},
		{name: "three", parent: h1, ids: []uint64{1, 2, 3}, wantLen: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := registryreader.OrderClients(tc.parent, tc.ids)
			require.Len(t, got, tc.wantLen)
			require.Equal(t, got, registryreader.OrderClients(tc.parent, tc.ids), "deterministic for fixed inputs")

			seen := make(map[uint64]struct{}, len(got))
			for _, id := range got {
				seen[id] = struct{}{}
			}
			for _, id := range tc.ids {
				_, ok := seen[id]
				require.Truef(t, ok, "id %d missing from output", id)
			}
		})
	}

	require.NotEqual(t,
		registryreader.OrderClients(h1, []uint64{1, 2, 3}),
		registryreader.OrderClients(h2, []uint64{1, 2, 3}),
		"different parent hashes should reorder >2 clients")
}

func TestFilterReservedTxs(t *testing.T) {
	t.Parallel()

	a := common.HexToAddress("0x01")
	b := common.HexToAddress("0x02")
	c := common.HexToAddress("0x03")
	other := common.HexToAddress("0xFFFF")

	cases := []struct {
		name          string
		registry      *registryreader.Snapshot
		input         map[common.Address][]*txpool.LazyTransaction
		wantClients   map[uint64][]common.Address
		wantRemaining []common.Address
	}{
		{
			name:          "no reserved senders present",
			registry:      newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{common.HexToAddress("0x99")}, QuotaGas: 1}}),
			input:         map[common.Address][]*txpool.LazyTransaction{a: stubTxs(1)},
			wantRemaining: []common.Address{a},
		},
		{
			name: "filters reserved senders out",
			registry: newTestSnapshot(0, []testClient{
				{ID: 7, Senders: []common.Address{a, b}, QuotaGas: 1_000_000},
				{ID: 3, Senders: []common.Address{c}, QuotaGas: 500_000},
			}),
			input: map[common.Address][]*txpool.LazyTransaction{
				a: stubTxs(2), b: stubTxs(1), c: stubTxs(3), other: stubTxs(1),
			},
			wantClients:   map[uint64][]common.Address{7: {a, b}, 3: {c}},
			wantRemaining: []common.Address{other},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			perClient := filterReservedTxs(tc.input, tc.registry)

			require.Len(t, perClient, len(tc.wantClients))
			for cid, senders := range tc.wantClients {
				got, ok := perClient[cid]
				require.Truef(t, ok, "client %d missing", cid)
				require.Len(t, got, len(senders))
				for _, s := range senders {
					require.Contains(t, got, s)
				}
			}

			require.Len(t, tc.input, len(tc.wantRemaining))
			for _, s := range tc.wantRemaining {
				require.Contains(t, tc.input, s)
			}
		})
	}
}

// TestSelectReservedTxs exercises the per-client quota selection: ascending-fee
// preference (zero-fee wins scarce quota over fallback-fee), per-sender nonce
// contiguity on quota breach, and gas-limit-based accounting.
func TestSelectReservedTxs(t *testing.T) {
	t.Parallel()

	a := common.HexToAddress("0x0a")
	b := common.HexToAddress("0x0b")

	cases := []struct {
		name         string
		baseFee      *big.Int
		pending      map[common.Address][]*txpool.LazyTransaction
		quota        uint64
		wantSelected map[common.Address]int
		wantUsed     uint64
		wantOverflow map[common.Address]int
	}{
		{
			name:         "all fit",
			baseFee:      nil,
			pending:      map[common.Address][]*txpool.LazyTransaction{a: {gasTx(100), gasTx(100)}},
			quota:        1000,
			wantSelected: map[common.Address]int{a: 2},
			wantUsed:     200,
			wantOverflow: map[common.Address]int{},
		},
		{
			name:         "breach diverts the sender's remaining nonces",
			baseFee:      nil,
			pending:      map[common.Address][]*txpool.LazyTransaction{a: {gasTx(600), gasTx(600), gasTx(600)}},
			quota:        1000,
			wantSelected: map[common.Address]int{a: 1},
			wantUsed:     600,
			wantOverflow: map[common.Address]int{a: 2},
		},
		{
			name:    "ascending-fee preference: zero-fee wins scarce quota",
			baseFee: big.NewInt(100),
			pending: map[common.Address][]*txpool.LazyTransaction{
				a: {feeGasTx(150, 50, 100)}, // fallback-fee, tip 50
				b: {feeGasTx(0, 0, 100)},    // zero-fee (below base fee)
			},
			quota:        100, // room for exactly one
			wantSelected: map[common.Address]int{b: 1},
			wantUsed:     100,
			wantOverflow: map[common.Address]int{a: 1},
		},
		{
			name:         "zero quota: everything overflows",
			baseFee:      nil,
			pending:      map[common.Address][]*txpool.LazyTransaction{a: {gasTx(1)}},
			quota:        0,
			wantSelected: map[common.Address]int{},
			wantUsed:     0,
			wantOverflow: map[common.Address]int{a: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fn := reservedCtor(tc.baseFee)

			selected, used, overflow := selectReservedTxs(tc.pending, tc.quota, fn)
			require.Equal(t, tc.wantUsed, used, "selected declared gas")

			gotSelected := drainBySender(selected)
			require.Len(t, gotSelected, len(tc.wantSelected))
			for addr, n := range tc.wantSelected {
				require.Equalf(t, n, gotSelected[addr], "selected[%s]", addr.Hex())
			}
			gotOverflow := 0
			for _, txs := range overflow {
				gotOverflow += len(txs)
			}
			wantOverflow := 0
			for addr, n := range tc.wantOverflow {
				require.Lenf(t, overflow[addr], n, "overflow[%s]", addr.Hex())
				wantOverflow += n
			}
			require.Equal(t, wantOverflow, gotOverflow)
		})
	}
}

// drainBySender pops a reserved ordering and counts transactions per sender.
func drainBySender(h *transactionsByPriceAndNonce) map[common.Address]int {
	out := make(map[common.Address]int)
	for {
		ltx, _ := h.Peek()
		if ltx == nil {
			break
		}
		from, _ := h.PeekFrom()
		out[from]++
		h.Shift()
	}
	return out
}

// waitForBlockWithTxs blocks until a mined block carrying at least minTxs
// transactions is observed, or fails after timeout.
func waitForBlockWithTxs(t *testing.T, sub *event.TypeMuxSubscription, minTxs int, timeout time.Duration) *types.Block {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-sub.Chan():
			blk := ev.Data.(core.NewMinedBlockEvent).Block
			if len(blk.Transactions()) >= minTxs {
				return blk
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a block with >= %d txs", minTxs)
			return nil
		}
	}
}

// stubTxs builds n minimal LazyTransaction handles. filterReservedTxs only
// inspects the map shape (keyed by sender), so the entries can be minimal.
func stubTxs(n int) []*txpool.LazyTransaction {
	out := make([]*txpool.LazyTransaction, n)
	for i := 0; i < n; i++ {
		out[i] = &txpool.LazyTransaction{Hash: common.Hash{byte(i)}}
	}
	return out
}

// feeTx builds a LazyTransaction with the given (positional) nonce, fee cap and
// tip cap for ordering tests. The slice position is the effective nonce; the
// nonce argument only seeds Hash/Time for determinism.
func feeTx(nonce, feeCap, tipCap uint64) *txpool.LazyTransaction {
	return &txpool.LazyTransaction{
		Hash:      common.Hash{byte(nonce)},
		Time:      time.Unix(0, int64(nonce)),
		GasFeeCap: uint256.NewInt(feeCap),
		GasTipCap: uint256.NewInt(tipCap),
		Gas:       21000,
	}
}

// gasTx builds a zero-fee LazyTransaction with the given gas limit, for quota
// selection tests that don't care about fee ordering.
func gasTx(gas uint64) *txpool.LazyTransaction {
	return &txpool.LazyTransaction{
		GasFeeCap: uint256.NewInt(0),
		GasTipCap: uint256.NewInt(0),
		Gas:       gas,
	}
}

// feeGasTx builds a LazyTransaction with explicit fee cap, tip cap and gas
// limit, for quota tests that exercise the ascending-fee preference.
func feeGasTx(feeCap, tipCap, gas uint64) *txpool.LazyTransaction {
	return &txpool.LazyTransaction{
		GasFeeCap: uint256.NewInt(feeCap),
		GasTipCap: uint256.NewInt(tipCap),
		Gas:       gas,
	}
}

// reservedCtor returns a reserved-ordering constructor bound to baseFee, for
// unit tests that exercise sequencing/selection without a full worker/env.
func reservedCtor(baseFee *big.Int) transactionsByPriceAndNonceFn {
	return func(txs map[common.Address][]*txpool.LazyTransaction) *transactionsByPriceAndNonce {
		return newReservedTransactionsByNonce(nil, txs, baseFee, nil)
	}
}

// TestExtractReservedTxs covers the worker-level reserved extraction: nil and
// empty registries are no-ops; a populated registry yields one ordered group
// per client and re-adds quota overflow to the pending map.
func TestExtractReservedTxs(t *testing.T) {
	t.Parallel()

	a := common.HexToAddress("0x0a")
	n := common.HexToAddress("0x0e")
	fn := reservedCtor(nil)

	t.Run("nil registry is a no-op", func(t *testing.T) {
		t.Parallel()
		pending := map[common.Address][]*txpool.LazyTransaction{a: {gasTx(100)}}
		require.Nil(t, extractReservedTxs(nil, common.Hash{}, pending, fn))
		require.Contains(t, pending, a, "pending untouched")
	})

	t.Run("empty registry is a no-op", func(t *testing.T) {
		t.Parallel()
		pending := map[common.Address][]*txpool.LazyTransaction{a: {gasTx(100)}}
		require.Nil(t, extractReservedTxs(newTestSnapshot(0, nil), common.Hash{}, pending, fn))
		require.Contains(t, pending, a, "pending untouched")
	})

	t.Run("client within quota; normal sender untouched", func(t *testing.T) {
		t.Parallel()
		registry := newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{a}, QuotaGas: 1_000_000}})
		pending := map[common.Address][]*txpool.LazyTransaction{a: {gasTx(100), gasTx(100)}, n: {gasTx(100)}}

		groups := extractReservedTxs(registry, common.Hash{}, pending, fn)
		require.Len(t, groups, 1)
		require.Equal(t, map[common.Address]int{a: 2}, drainBySender(groups[0]))

		require.NotContains(t, pending, a, "reserved sender removed from pending")
		require.Contains(t, pending, n, "normal sender retained")
	})

	t.Run("quota overflow re-added to pending", func(t *testing.T) {
		t.Parallel()
		registry := newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{a}, QuotaGas: 100}})
		pending := map[common.Address][]*txpool.LazyTransaction{a: {gasTx(100), gasTx(100)}}

		groups := extractReservedTxs(registry, common.Hash{}, pending, fn)
		require.Len(t, groups, 1)
		require.Equal(t, map[common.Address]int{a: 1}, drainBySender(groups[0]))
		require.Len(t, pending[a], 1, "overflow tx re-added to pending for the normal pass")
	})
}

// TestExtractReservedTxs_PerClientQuota pins that placement is bounded by
// per-client quota only — there is no global capacity ceiling. The registry
// guarantees Σ quotas == capacity, so a cross-client cap could never bind and
// would only desync the producer from the verifier's ceiling-free
// classification.
func TestExtractReservedTxs_PerClientQuota(t *testing.T) {
	t.Parallel()

	a := common.HexToAddress("0x0a") // client 1's sender
	b := common.HexToAddress("0x0b") // client 2's sender
	fn := reservedCtor(nil)

	t.Run("per-client quotas are independent (no global ceiling)", func(t *testing.T) {
		t.Parallel()
		// capacity == Σ quotas (the registry invariant); each client's two
		// 300-gas txs fit its own 600 quota. Client order cannot starve either
		// one — both are fully served.
		registry := newTestSnapshot(1200, []testClient{
			{ID: 1, Senders: []common.Address{a}, QuotaGas: 600},
			{ID: 2, Senders: []common.Address{b}, QuotaGas: 600},
		})
		pending := map[common.Address][]*txpool.LazyTransaction{
			a: {gasTx(300), gasTx(300)},
			b: {gasTx(300), gasTx(300)},
		}

		groups := extractReservedTxs(registry, common.Hash{}, pending, fn)
		require.Len(t, groups, 2, "both clients fully served, independent of order")
		require.Empty(t, pending, "nothing diverted")
	})

	t.Run("a client overflowing its own quota diverts only its excess", func(t *testing.T) {
		t.Parallel()
		// Client 1 quota 600 with three 300-gas txs: first two fit, the third
		// overflows to the normal pass. Client 2 is unaffected.
		registry := newTestSnapshot(1200, []testClient{
			{ID: 1, Senders: []common.Address{a}, QuotaGas: 600},
			{ID: 2, Senders: []common.Address{b}, QuotaGas: 600},
		})
		pending := map[common.Address][]*txpool.LazyTransaction{
			a: {gasTx(300), gasTx(300), gasTx(300)},
			b: {gasTx(300)},
		}

		groups := extractReservedTxs(registry, common.Hash{}, pending, fn)
		require.Len(t, groups, 2, "both clients contribute a group")
		require.Len(t, pending[a], 1, "client 1's third tx diverted to normal")
		require.Empty(t, pending[b], "client 2 fully within quota")
	})
}

// TestSelectReservedTxs_Interrupt pins the interrupt behavior: when block
// building is already interrupted, the scan heap constructor yields an empty
// heap, so the client contributes nothing and nothing lands in overflow — the
// transactions simply stay in the pool for a later block.
func TestSelectReservedTxs_Interrupt(t *testing.T) {
	t.Parallel()

	interrupted := new(atomic.Bool)
	interrupted.Store(true)
	fn := func(txs map[common.Address][]*txpool.LazyTransaction) *transactionsByPriceAndNonce {
		return newReservedTransactionsByNonce(nil, txs, nil, interrupted)
	}

	a := common.HexToAddress("0x0a")
	pending := map[common.Address][]*txpool.LazyTransaction{a: {gasTx(100), gasTx(100)}}

	selected, used, overflow := selectReservedTxs(pending, 1_000, fn)
	require.True(t, selected.Empty(), "interrupted scan selects nothing")
	require.Zero(t, used)
	require.Empty(t, overflow, "nothing diverted to the normal pass")
}

// Reserved-region ordering tests (ascending pop, never-drop, clone/Shift/interrupt
// behavior) live in ordering_test.go alongside the normal-market ordering tests.

// TestSequenceTxs validates the full grouping: priority first, then reserved
// clients, then normal; empty groups are omitted; reserved overflow lands in
// the normal group.
func TestSequenceTxs(t *testing.T) {
	t.Parallel()

	p := common.HexToAddress("0x0a") // priority
	r := common.HexToAddress("0x0b") // reserved
	n := common.HexToAddress("0x0c") // normal

	newEnv := func() *environment {
		return &environment{header: &types.Header{Number: big.NewInt(1), BaseFee: nil}}
	}

	t.Run("priority, reserved and normal groups in order", func(t *testing.T) {
		t.Parallel()
		w := &worker{prio: []common.Address{p}, reservedSnapshotOverride: newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{r}, QuotaGas: 1_000_000}})}
		pending := map[common.Address][]*txpool.LazyTransaction{p: {gasTx(100)}, r: {gasTx(100)}, n: {gasTx(100)}}

		seq := w.sequenceTxs(newEnv(), w.reservedSnapshotOverride, pending)
		require.Len(t, seq, 3)
		require.Equal(t, map[common.Address]int{p: 1}, drainBySender(seq[0]), "priority group first")
		require.Equal(t, map[common.Address]int{r: 1}, drainBySender(seq[1]), "reserved group next")
		require.Equal(t, map[common.Address]int{n: 1}, drainBySender(seq[2]), "normal group last")
	})

	t.Run("no prio, no registry: single normal group", func(t *testing.T) {
		t.Parallel()
		w := &worker{}
		pending := map[common.Address][]*txpool.LazyTransaction{n: {gasTx(100)}}

		seq := w.sequenceTxs(newEnv(), w.reservedSnapshotOverride, pending)
		require.Len(t, seq, 1)
		require.Equal(t, map[common.Address]int{n: 1}, drainBySender(seq[0]))
	})

	t.Run("empty pending yields no groups", func(t *testing.T) {
		t.Parallel()
		w := &worker{prio: []common.Address{p}, reservedSnapshotOverride: newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{r}, QuotaGas: 100}})}
		seq := w.sequenceTxs(newEnv(), w.reservedSnapshotOverride, map[common.Address][]*txpool.LazyTransaction{})
		require.Empty(t, seq)
	})

	t.Run("reserved overflow joins the normal group", func(t *testing.T) {
		t.Parallel()
		w := &worker{reservedSnapshotOverride: newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{r}, QuotaGas: 100}})}
		pending := map[common.Address][]*txpool.LazyTransaction{r: {gasTx(100), gasTx(100)}, n: {gasTx(100)}}

		seq := w.sequenceTxs(newEnv(), w.reservedSnapshotOverride, pending)
		require.Len(t, seq, 2)
		require.Equal(t, map[common.Address]int{r: 1}, drainBySender(seq[0]), "reserved group: one tx fits quota")
		require.Equal(t, map[common.Address]int{n: 1, r: 1}, drainBySender(seq[1]), "normal group: normal sender + reserved overflow")
	})

	t.Run("multiple clients yield one group each in deterministic order", func(t *testing.T) {
		t.Parallel()
		r2 := common.HexToAddress("0x0d")
		senderOf := map[uint64]common.Address{1: r, 2: r2}
		w := &worker{reservedSnapshotOverride: newTestSnapshot(0, []testClient{
			{ID: 1, Senders: []common.Address{r}, QuotaGas: 1_000_000},
			{ID: 2, Senders: []common.Address{r2}, QuotaGas: 1_000_000},
		})}
		pending := map[common.Address][]*txpool.LazyTransaction{r: {gasTx(100)}, r2: {gasTx(100)}, n: {gasTx(100)}}

		env := newEnv()
		order := registryreader.OrderClients(env.header.ParentHash, []uint64{1, 2})

		seq := w.sequenceTxs(env, w.reservedSnapshotOverride, pending)
		require.Len(t, seq, 3)
		require.Equal(t, map[common.Address]int{senderOf[order[0]]: 1}, drainBySender(seq[0]), "first client in visit order")
		require.Equal(t, map[common.Address]int{senderOf[order[1]]: 1}, drainBySender(seq[1]), "second client in visit order")
		require.Equal(t, map[common.Address]int{n: 1}, drainBySender(seq[2]), "normal group last")
	})

	t.Run("below-base-fee overflow is dropped from the normal group", func(t *testing.T) {
		t.Parallel()
		// Quota fits one of the two zero-fee txs. The overflow tx re-enters
		// the normal pass, where standard EIP-1559 admission drops it
		// (GasFeeCap < BaseFee) — per spec it stays in the pool for a later
		// block instead of entering this one.
		w := &worker{reservedSnapshotOverride: newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{r}, QuotaGas: 100}})}
		pending := map[common.Address][]*txpool.LazyTransaction{
			r: {feeGasTx(0, 0, 100), feeGasTx(0, 0, 100)},
			n: {feeGasTx(200, 100, 100)},
		}

		env := newEnv()
		env.header.BaseFee = big.NewInt(100)

		seq := w.sequenceTxs(env, w.reservedSnapshotOverride, pending)
		require.Len(t, seq, 2)
		require.Equal(t, map[common.Address]int{r: 1}, drainBySender(seq[0]), "reserved group: one zero-fee tx within quota")
		require.Equal(t, map[common.Address]int{n: 1}, drainBySender(seq[1]), "normal group: zero-fee overflow not admitted")
	})

	t.Run("registered sender is excluded from the priority pass", func(t *testing.T) {
		t.Parallel()
		// Consensus parity: a verifier rederives classification from the ordered
		// body and cannot see the operator-local priority list, so a registered
		// sender must always be classified by the quota walk, never consumed by
		// the priority pass. Here the sender is both prioritized and registered;
		// it flows through the reserved pass (quota fits both txs), producing a
		// single reserved group and no priority group.
		w := &worker{prio: []common.Address{r}, reservedSnapshotOverride: newTestSnapshot(0, []testClient{{ID: 1, Senders: []common.Address{r}, QuotaGas: 1_000_000}})}
		pending := map[common.Address][]*txpool.LazyTransaction{r: {gasTx(100), gasTx(100)}}

		seq := w.sequenceTxs(newEnv(), w.reservedSnapshotOverride, pending)
		require.Len(t, seq, 1, "single reserved group; no priority group")
		require.True(t, seq[0].reserved, "registered sender classified by the reserved pass")
		require.Equal(t, map[common.Address]int{r: 2}, drainBySender(seq[0]), "both txs within quota")
	})
}

// TestReservedBuild_NilRegistry confirms that with no registry wired (production
// default) the worker still builds a block including the submitted tx.
func TestReservedBuild_NilRegistry(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer w.close()

	require.Nil(t, w.reservedSnapshotOverride, "snapshot override defaults to nil")

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()
	w.start()

	require.NoError(t, b.txPool.Add([]*types.Transaction{b.newRandomTxWithNonce(false, 0)}, false)[0])

	// The worker may seal an empty pre-commit block first; wait for the one
	// carrying the transaction.
	waitForBlockWithTxs(t, sub, 1, 5*time.Second)
}

// TestReservedBuild_HappyPath registers the test bank as a reserved client and
// confirms its transactions are included end-to-end through the wired sequencing
// path. (Single funded sender, so positional reserved/normal distinction is
// covered by the unit tests above, not here.)
func TestReservedBuild_HappyPath(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer w.close()

	w.setReservedSnapshot(newTestSnapshot(0, []testClient{
		{ID: 1, Senders: []common.Address{testBankAddress}, QuotaGas: 10_000_000},
	}))

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	errs := b.txPool.Add([]*types.Transaction{
		b.newRandomTxWithNonce(false, 0),
		b.newRandomTxWithNonce(false, 1),
	}, false)
	for _, err := range errs {
		require.NoError(t, err)
	}
	w.start()

	got := waitForBlockWithTxs(t, sub, 2, 15*time.Second)

	signer := types.LatestSigner(&chainConfig)
	for i, tx := range got.Transactions() {
		from, err := types.Sender(signer, tx)
		require.NoError(t, err)
		require.Equalf(t, testBankAddress, from, "tx %d should be from the reserved sender", i)
	}
}

// borUnittestCancunConfig returns a copy of BorUnittestChainConfig with Shanghai
// and Cancun active from genesis. BlockExtraData (and therefore the
// ReservedGasUsed header field) is only RLP-encoded into Header.Extra
// post-Cancun, which BorUnittestChainConfig doesn't reach.
func borUnittestCancunConfig() params.ChainConfig {
	chainConfig := *params.BorUnittestChainConfig
	chainConfig.ShanghaiBlock = big.NewInt(0)
	chainConfig.CancunBlock = big.NewInt(0)
	return chainConfig
}

// borUnittestReservedConfig returns a Cancun-enabled unittest config with the
// ReservedBlockspace fork active from genesis. The BorConfig is deep-copied so
// scheduling the fork can't leak into the shared BorUnittestChainConfig.
func borUnittestReservedConfig() params.ChainConfig {
	chainConfig := borUnittestCancunConfig()
	borCopy := *chainConfig.Bor
	borCopy.ReservedBlockspaceBlock = big.NewInt(0)
	// Giugliano must not activate after reserved blockspace (fork-order check), so
	// co-activate it from genesis.
	borCopy.GiuglianoBlock = big.NewInt(0)
	chainConfig.Bor = &borCopy
	return chainConfig
}

// TestReservedBuild_HeaderGasUsed confirms the producer records the reserved
// pass's actual gas total in BlockExtraData.ReservedGasUsed. With every block
// transaction committed through the reserved group, the total must equal the
// header's GasUsed.
func TestReservedBuild_HeaderGasUsed(t *testing.T) {
	chainConfig := borUnittestReservedConfig()
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	// noempty=true: skip the empty pre-seal block. Giugliano (active here to satisfy
	// the reserved fork-order rule) enables the builder/prefetch path, and under the
	// fake engine's instant seal the empty pre-seal would win the write race for the
	// block number, hiding the full block that carries the reserved txs.
	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), true, 0)
	defer w.close()

	w.setReservedSnapshot(newTestSnapshot(0, []testClient{
		{ID: 1, Senders: []common.Address{testBankAddress}, QuotaGas: 10_000_000},
	}))

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	errs := b.txPool.Add([]*types.Transaction{
		b.newRandomTxWithNonce(false, 0),
		b.newRandomTxWithNonce(false, 1),
	}, false)
	for _, err := range errs {
		require.NoError(t, err)
	}
	w.start()

	got := waitForBlockWithTxs(t, sub, 2, 15*time.Second)

	reserved := got.Header().GetReservedGasUsed(&chainConfig)
	require.NotNil(t, reserved, "reserved pass active: header must carry ReservedGasUsed")
	require.Equal(t, got.GasUsed(), *reserved, "all txs are reserved, so reserved gas equals block gas used")
}

// TestReservedBuild_HeaderCapacity confirms the producer stamps the sequencing
// snapshot's EFFECTIVE capacity (Σ quotas of the classified client set) into
// the header, not the registry's raw total. newTestSnapshot's raw-capacity
// argument is set to a value far from the client's quota specifically so a
// producer bug that stamped the raw total instead would be caught here.
func TestReservedBuild_HeaderCapacity(t *testing.T) {
	chainConfig := borUnittestReservedConfig()
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), true, 0)
	defer w.close()

	const rawCapacity = 999_000_000
	const clientQuota = 10_000_000
	w.setReservedSnapshot(newTestSnapshot(rawCapacity, []testClient{
		{ID: 1, Senders: []common.Address{testBankAddress}, QuotaGas: clientQuota},
	}))

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	errs := b.txPool.Add([]*types.Transaction{
		b.newRandomTxWithNonce(false, 0),
	}, false)
	for _, err := range errs {
		require.NoError(t, err)
	}
	w.start()

	got := waitForBlockWithTxs(t, sub, 1, 15*time.Second)

	capacity := got.Header().GetReservedCapacity(&chainConfig)
	require.NotNil(t, capacity, "reserved pass active: header must carry ReservedCapacity")
	require.Equal(t, uint64(clientQuota), *capacity,
		"capacity must be the snapshot's effective capacity (Σ quotas), not the raw registry total")
}

// TestReservedBuild_NoTxsHeaderCapacity pins the noTxs (eth/catalyst
// Engine-API payload) build path: generateWork's noTxs branch skips
// fillTransactions entirely, so writeReservedFields must still run there —
// otherwise the header would keep Prepare's placeholder ReservedCapacity=0,
// which fails validateReservedFields as soon as the registry carves out any
// capacity, even for a genuinely empty payload with nothing to disagree about.
func TestReservedBuild_NoTxsHeaderCapacity(t *testing.T) {
	chainConfig := borUnittestReservedConfig()
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer w.close()

	const clientQuota = uint64(10_000_000)
	w.setReservedSnapshot(newTestSnapshot(0, []testClient{
		{ID: 1, Senders: []common.Address{testBankAddress}, QuotaGas: clientQuota},
	}))

	parent := b.chain.CurrentBlock()
	r := w.getSealingBlock(&generateParams{
		parentHash: parent.Hash(),
		timestamp:  parent.Time + 1,
		coinbase:   testBankAddress,
		forceTime:  true,
		noTxs:      true,
	})
	require.NoError(t, r.err)
	require.NotNil(t, r.block)
	require.Empty(t, r.block.Transactions(), "this is the no-tx payload build path")

	capacity := r.block.Header().GetReservedCapacity(&chainConfig)
	require.NotNil(t, capacity, "noTxs build must still carry ReservedCapacity")
	require.Equal(t, clientQuota, *capacity,
		"capacity must be the snapshot's effective capacity even when fillTransactions never ran")

	gasUsed := r.block.Header().GetReservedGasUsed(&chainConfig)
	require.NotNil(t, gasUsed, "noTxs build must still carry ReservedGasUsed")
	require.Equal(t, uint64(0), *gasUsed, "no transactions were included")
}

// TestReservedBuild_HeaderAbsentPreFork pins wire compatibility: before the
// ReservedBlockspace fork, post-Cancun blocks must not carry the
// ReservedGasUsed field at all — their Extra encoding stays byte-identical to
// pre-reserved-blockspace blocks.
func TestReservedBuild_HeaderAbsentPreFork(t *testing.T) {
	chainConfig := borUnittestCancunConfig()
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer w.close()

	require.Nil(t, w.reservedSnapshotOverride, "snapshot override defaults to nil")

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()
	w.start()

	require.NoError(t, b.txPool.Add([]*types.Transaction{b.newRandomTxWithNonce(false, 0)}, false)[0])

	got := waitForBlockWithTxs(t, sub, 1, 15*time.Second)
	require.Nil(t, got.Header().GetReservedGasUsed(&chainConfig), "pre-fork: field must be absent from the wire")
	require.Nil(t, got.Header().GetReservedCapacity(&chainConfig), "pre-fork: field must be absent from the wire")
}

// TestReservedBuild_Positional proves the reserved-first builder preference
// end-to-end with two funded senders: the registered sender's lower-priced
// transaction must precede an unregistered sender's higher-priced one, which is
// the opposite of pure price ordering.
func TestReservedBuild_Positional(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer w.close()

	w.setReservedSnapshot(newTestSnapshot(0, []testClient{
		{ID: 1, Senders: []common.Address{testUserAddress}, QuotaGas: 10_000_000},
	}))

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()
	w.start()

	// The shared genesis funds only the bank, so give the reserved sender a
	// balance first and wait for that transfer to mine.
	fund, err := types.SignTx(
		types.NewTransaction(0, testUserAddress, big.NewInt(100_000_000_000_000_000), params.TxGas, big.NewInt(30*params.InitialBaseFee), nil),
		types.HomesteadSigner{}, testBankKey)
	require.NoError(t, err)
	require.NoError(t, b.txPool.Add([]*types.Transaction{fund}, false)[0])
	waitForBlockWithTxs(t, sub, 1, 40*time.Second)

	// Pause sealing while both contenders are admitted — with one-second
	// blocks, sequential adds can otherwise split them across two blocks and
	// void the positional comparison.
	w.stop()

	// Reserved sender pays 26 gwei, normal sender 100 gwei: price ordering
	// would put the bank first, reserved sequencing must not.
	userTx, err := types.SignTx(
		types.NewTransaction(0, testBankAddress, big.NewInt(1000), params.TxGas, big.NewInt(26*params.InitialBaseFee), nil),
		types.HomesteadSigner{}, testUserKey)
	require.NoError(t, err)
	bankTx, err := types.SignTx(
		types.NewTransaction(1, testUserAddress, big.NewInt(1000), params.TxGas, big.NewInt(100*params.InitialBaseFee), nil),
		types.HomesteadSigner{}, testBankKey)
	require.NoError(t, err)

	// The pool resets asynchronously from chain-head events, so the user's
	// funding may not be visible to it yet — retry admission until it is.
	require.Eventually(t, func() bool {
		return b.txPool.Add([]*types.Transaction{userTx}, false)[0] == nil
	}, 20*time.Second, 100*time.Millisecond, "user tx not admitted after funding")
	require.NoError(t, b.txPool.Add([]*types.Transaction{bankTx}, false)[0])
	w.start()

	got := waitForBlockWithTxs(t, sub, 2, 40*time.Second)

	pos := make(map[common.Hash]int, len(got.Transactions()))
	for i, tx := range got.Transactions() {
		pos[tx.Hash()] = i
	}
	userPos, ok := pos[userTx.Hash()]
	require.True(t, ok, "reserved tx missing from block")
	bankPos, ok := pos[bankTx.Hash()]
	require.True(t, ok, "normal tx missing from block")
	require.Less(t, userPos, bankPos, "reserved sender's cheaper tx must precede the normal sender's pricier tx")
}

// TestReservedBuild_Overflow sets a quota that fits only one tx and confirms the
// second (quota overflow) tx still lands in the block via the normal group.
func TestReservedBuild_Overflow(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer w.close()

	// Quota fits exactly one value-transfer (gas limit params.TxGas = 21000).
	w.setReservedSnapshot(newTestSnapshot(0, []testClient{
		{ID: 1, Senders: []common.Address{testBankAddress}, QuotaGas: params.TxGas},
	}))

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	errs := b.txPool.Add([]*types.Transaction{
		b.newRandomTxWithNonce(false, 0),
		b.newRandomTxWithNonce(false, 1),
	}, false)
	for _, err := range errs {
		require.NoError(t, err)
	}
	w.start()

	got := waitForBlockWithTxs(t, sub, 2, 15*time.Second)
	// One tx fit the reserved quota; the second overflowed to the normal group.
	// Both are included in the block.
	require.Len(t, got.Transactions(), 2, "both txs included (1 reserved + 1 normal overflow)")
}

// TestReservedBuild_PersistsReservedTxIndexes locks in the miner-sealed write
// path for the reserved-tx side table: commit() reads the reserved set off
// the pre-copy environment (environment.copy() does not carry the evm field
// over), never off the copy it and both of its callers pass around, or every
// miner-sealed block would silently lose its reserved classification for the
// on-disk side table regardless of what state_transition executed.
func TestReservedBuild_PersistsReservedTxIndexes(t *testing.T) {
	chainConfig := borUnittestReservedConfig()
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer engine.Close()
	defer ctrl.Finish()

	db := rawdb.NewMemoryDatabase()
	w, b, _ := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, db, true, 0)
	defer w.close()

	w.setReservedSnapshot(newTestSnapshot(0, []testClient{
		{ID: 1, Senders: []common.Address{testBankAddress}, QuotaGas: 10_000_000},
	}))

	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	errs := b.txPool.Add([]*types.Transaction{
		b.newRandomTxWithNonce(false, 0),
		b.newRandomTxWithNonce(false, 1),
	}, false)
	for _, err := range errs {
		require.NoError(t, err)
	}
	w.start()

	got := waitForBlockWithTxs(t, sub, 2, 15*time.Second)

	indexes := rawdb.ReadReservedTxIndexes(db, got.Hash(), got.NumberU64())
	require.Equal(t, []uint64{0, 1}, indexes, "both txs are from the sole reserved sender and must be recorded")
}
