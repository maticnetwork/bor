package legacypool

import (
	"crypto/ecdsa"
	"math/big"
	"sync"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
)

// fakeRegistry is a state-independent registryreader.Reader: the reserved set is
// fixed at construction. It lets the pool build a snapshot without deploying the
// real contract into the test state, exercising the snapshot→isReserved path.
type fakeRegistry struct {
	reserved map[common.Address]registryreader.ClientLookup
	capacity uint64
}

func newFakeRegistry(reserved ...common.Address) *fakeRegistry {
	f := &fakeRegistry{reserved: make(map[common.Address]registryreader.ClientLookup)}
	for i, a := range reserved {
		f.reserved[a] = registryreader.ClientLookup{
			ClientID: big.NewInt(int64(i + 1)), GasQuota: 30_000_000, Active: true,
		}
		f.capacity += 30_000_000
	}
	return f
}

func (f *fakeRegistry) HasReservedRegistry() bool { return true }
func (f *fakeRegistry) IsReservedAddress(_ *state.StateDB, _ uint64, _ common.Hash, a common.Address) (bool, error) {
	c, ok := f.reserved[a]
	return ok && c.Active, nil
}

func (f *fakeRegistry) ReservedClientForAddress(_ *state.StateDB, _ uint64, _ common.Hash, a common.Address) (registryreader.ClientLookup, error) {
	return f.reserved[a], nil
}

func (f *fakeRegistry) Root(_ *state.StateDB, _ uint64, _ common.Hash) (common.Hash, error) {
	return common.HexToHash("0x1"), nil
}

func (f *fakeRegistry) WhitelistedAddresses(_ *state.StateDB, _ uint64, _ common.Hash) ([]common.Address, error) {
	addrs := make([]common.Address, 0, len(f.reserved))
	for a := range f.reserved {
		addrs = append(addrs, a)
	}
	return addrs, nil
}

func (f *fakeRegistry) TotalReservedGas(_ *state.StateDB, _ uint64, _ common.Hash) (uint64, error) {
	return f.capacity, nil
}

// register and deregister simulate registry governance (a client being added
// to or removed from the registry) between pool admissions or promotion
// passes. Real deregistration takes effect for the pool only once the next
// head event rebuilds the snapshot (see refreshReservedSnapshot).
func (f *fakeRegistry) register(addr common.Address) {
	if _, ok := f.reserved[addr]; ok {
		return
	}
	f.reserved[addr] = registryreader.ClientLookup{
		ClientID: big.NewInt(int64(len(f.reserved) + 1)), GasQuota: 30_000_000, Active: true,
	}
	f.capacity += 30_000_000
}

func (f *fakeRegistry) deregister(addr common.Address) {
	if _, ok := f.reserved[addr]; !ok {
		return
	}
	delete(f.reserved, addr)
	f.capacity -= 30_000_000
}

// refreshReservedSnapshot forces the pool to rebuild its reserved-set
// snapshot from the current registry state, mirroring what every real head
// event does (see LegacyPool.reset). Tests use it to observe the effect of
// mutating the fake registry without driving a full chain reorg.
func refreshReservedSnapshot(pool *LegacyPool) {
	pool.mu.Lock()
	statedb, head := pool.currentState, pool.currentHead.Load()
	pool.mu.Unlock()
	pool.rebuildReservedSnapshot(statedb, head)
}

// deregisterReserved deregisters addr from the pool's fake registry and
// forces the snapshot rebuild that a real head event would perform,
// simulating registry governance followed by the next block. Returns the
// fake registry so a caller that needs to mutate it again (e.g. to
// re-register) doesn't have to repeat the type assertion.
func deregisterReserved(t *testing.T, pool *LegacyPool, addr common.Address) *fakeRegistry {
	t.Helper()
	fr, ok := pool.reservedRegistry.(*fakeRegistry)
	if !ok {
		t.Fatalf("unexpected registry type %T", pool.reservedRegistry)
	}
	fr.deregister(addr)
	refreshReservedSnapshot(pool)
	return fr
}

// reservedChainConfigAt builds a chain config with the reserved-blockspace
// fork at an explicit block, so tests can exercise the pre-fork/post-fork
// boundary.
func reservedChainConfigAt(forkBlock *big.Int) *params.ChainConfig {
	cfg := *params.BorUnittestChainConfig // London active at 0
	bor := *cfg.Bor
	bor.ReservedBlockspaceBlock = forkBlock // fork gate; the reserved set comes from the registry
	cfg.Bor = &bor
	return &cfg
}

func setupReservedPool(reserved common.Address) *LegacyPool {
	return setupReservedPoolAt(reserved, big.NewInt(0))
}

// setupReservedPoolAt is setupReservedPool with an explicit fork block.
func setupReservedPoolAt(reserved common.Address, forkBlock *big.Int) *LegacyPool {
	return setupReservedPoolWithConfig(testTxPoolConfig, forkBlock, reserved)
}

// setupReservedPoolWithConfig is setupReservedPoolAt generalized to a
// caller-supplied pool config and an arbitrary number of reserved addresses,
// so occupancy-cap tests can shrink GlobalSlots/GlobalQueue/
// ReservedMaxOccupancyPercent to make the cap cheap to reach without waiting
// on the package-level testTxPoolConfig defaults.
func setupReservedPoolWithConfig(cfg Config, forkBlock *big.Int, reserved ...common.Address) *LegacyPool {
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	chainCfg := reservedChainConfigAt(forkBlock)
	bc := newTestBlockChain(chainCfg, 10_000_000, statedb, new(event.Feed))
	pool := New(cfg, bc)
	if err := pool.Init(cfg.PriceLimit, bc.CurrentBlock(), newReserver()); err != nil {
		panic(err)
	}
	<-pool.initDoneCh
	// A realistic positive tip floor (PIP-35 ~25 gwei). Reserved senders must
	// bypass it; everyone else is held to it.
	pool.SetGasTip(big.NewInt(30_000_000_000))
	// Wire the registry source (post-Init, as the backend does) and build the snapshot.
	pool.SetReservedRegistry(newFakeRegistry(reserved...))
	return pool
}

func zeroFeeTx(t *testing.T, cfg *params.ChainConfig, key *ecdsa.PrivateKey, nonce uint64, to common.Address) *types.Transaction {
	t.Helper()
	return zeroFeeTxWithGasAndData(t, cfg, key, nonce, to, 100_000, nil)
}

// zeroFeeTxWithGasAndData is zeroFeeTx with caller-chosen gas and calldata,
// for tests that need a specific transaction size (e.g. bigZeroFeeTx, sized
// to span multiple pool slots) without duplicating the DynamicFeeTx literal.
func zeroFeeTxWithGasAndData(t *testing.T, cfg *params.ChainConfig, key *ecdsa.PrivateKey, nonce uint64, to common.Address, gas uint64, data []byte) *types.Transaction {
	t.Helper()
	tx, err := types.SignNewTx(key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     nonce,
		GasTipCap: big.NewInt(0),
		GasFeeCap: big.NewInt(0),
		Gas:       gas,
		To:        &to,
		Value:     big.NewInt(0),
		Data:      data,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// fallbackFeeTx builds a signed dynamic-fee tx carrying a positive fallback
// fee (Primer §6.3): a reserved sender uses this fee only if its tx overflows
// quota, but it must clear the pool's tip floor regardless of classification.
// gas is fixed at 100_000 so gas*feeCap dwarfs any small test balance,
// modelling a sender that holds value but not gas headroom.
func fallbackFeeTx(t *testing.T, cfg *params.ChainConfig, key *ecdsa.PrivateKey, nonce uint64, value, feeCap *big.Int) *types.Transaction {
	t.Helper()
	to := common.Address{0x42}
	tx, err := types.SignNewTx(key, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     nonce,
		GasTipCap: feeCap,
		GasFeeCap: feeCap,
		Gas:       100_000,
		To:        &to,
		Value:     value,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// TestReservedZeroFeeTxAdmittedAndPending verifies the core behaviour:
// a zero-fee tx from a reserved sender is admitted past the tip floor, kept in
// the pool, and surfaced by Pending even under a high miner MinTip — whereas the
// same tx from a non-reserved sender is rejected at admission.
func TestReservedZeroFeeTxAdmittedAndPending(t *testing.T) {
	t.Parallel()

	reservedKey, _ := crypto.GenerateKey()
	reservedAddr := crypto.PubkeyToAddress(reservedKey.PublicKey)
	pool := setupReservedPool(reservedAddr)
	defer pool.Close()

	otherKey, _ := crypto.GenerateKey()
	otherAddr := crypto.PubkeyToAddress(otherKey.PublicKey)

	testAddBalance(pool, reservedAddr, big.NewInt(1_000_000))
	testAddBalance(pool, otherAddr, big.NewInt(1_000_000))

	cfg := pool.chainconfig

	// Reserved sender: zero-fee tx must be admitted.
	rtx := zeroFeeTx(t, cfg, reservedKey, 0, common.Address{0x42})
	if err := pool.Add([]*types.Transaction{rtx}, true)[0]; err != nil {
		t.Fatalf("reserved zero-fee tx rejected: %v", err)
	}
	if pool.Get(rtx.Hash()) == nil {
		t.Fatal("reserved zero-fee tx not kept in pool")
	}

	// Non-reserved sender: identical zero-fee tx must be rejected at the floor.
	otx := zeroFeeTx(t, cfg, otherKey, 0, common.Address{0x42})
	if err := pool.Add([]*types.Transaction{otx}, true)[0]; err == nil {
		t.Fatal("non-reserved zero-fee tx should be rejected at the tip floor")
	}

	// The reserved tx must survive a high miner tip filter so it reaches the miner.
	pending := pool.Pending(txpool.PendingFilter{
		MinTip:  uint256.NewInt(30_000_000_000),
		BaseFee: uint256.NewInt(1_000_000_000),
	}, nil)
	if got := pending[reservedAddr]; len(got) != 1 || got[0].Hash != rtx.Hash() {
		t.Fatalf("reserved zero-fee tx missing from Pending under high MinTip: %v", got)
	}
}

// TestReservedZeroFeeReplacement pins the replacement rule:
// same-nonce replacement is priced entirely through the fallback-fee fields
// under the standard bump rule. A zero-fee tx can never replace a zero-fee tx
// ("10% over zero is still zero" — the strict-increase check rejects it),
// while strictly positive fallback fees win the slot. This keeps same-nonce
// pool content convergent across nodes regardless of arrival order.
func TestReservedZeroFeeReplacement(t *testing.T) {
	t.Parallel()

	reservedKey, _ := crypto.GenerateKey()
	reservedAddr := crypto.PubkeyToAddress(reservedKey.PublicKey)
	pool := setupReservedPool(reservedAddr)
	defer pool.Close()

	testAddBalance(pool, reservedAddr, big.NewInt(1_000_000))
	cfg := pool.chainconfig

	first := zeroFeeTx(t, cfg, reservedKey, 0, common.Address{0xaa})
	if err := pool.Add([]*types.Transaction{first}, true)[0]; err != nil {
		t.Fatalf("first reserved tx rejected: %v", err)
	}

	// Same nonce, still zero fee: rejected — zero cannot out-bid zero.
	second := zeroFeeTx(t, cfg, reservedKey, 0, common.Address{0xbb})
	if err := pool.Add([]*types.Transaction{second}, true)[0]; err == nil {
		t.Fatal("zero-fee replacement of a zero-fee tx must be rejected")
	}
	if pool.Get(first.Hash()) == nil {
		t.Fatal("incumbent zero-fee tx must remain in the pool")
	}

	// Same nonce with positive fallback fees: wins the slot under the standard
	// bump rule (strictly above zero clears both the strict-increase check and
	// the zero threshold). The fallback fees stay below the 30 gwei tip floor,
	// which reserved senders bypass at admission.
	third, err := types.SignNewTx(reservedKey, types.LatestSigner(cfg), &types.DynamicFeeTx{
		ChainID:   cfg.ChainID,
		Nonce:     0,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(1),
		Gas:       100_000,
		To:        &common.Address{0xcc},
		Value:     big.NewInt(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add([]*types.Transaction{third}, true)[0]; err != nil {
		t.Fatalf("fallback-fee replacement rejected: %v", err)
	}
	if pool.Get(third.Hash()) == nil {
		t.Fatal("fallback-fee replacement not in pool")
	}
	if pool.Get(first.Hash()) != nil {
		t.Fatal("replaced zero-fee tx should have been evicted")
	}

	// And a zero-fee tx cannot displace the fallback-fee incumbent either.
	fourth := zeroFeeTx(t, cfg, reservedKey, 0, common.Address{0xdd})
	if err := pool.Add([]*types.Transaction{fourth}, true)[0]; err == nil {
		t.Fatal("zero-fee tx must not replace a fallback-fee incumbent")
	}
}

// reservedFallbackFeeBalance is a balance that covers a fallbackFeeTx's value
// (1000 wei) many times over but is dwarfed by its full cost (value +
// 100_000 gas * a >=30 gwei feeCap, i.e. >= 3e15). It models a reserved
// sender that holds value but not gas headroom (Primer §6.3, §8.1).
var reservedFallbackFeeBalance = big.NewInt(5_000)

// TestReservedFallbackFeeAdmissionForkGate pins POS-3671's admission gate: a
// fallback-fee tx from a sender whose balance covers only the tx's value is
// rejected pre-fork (priced at full cost, same as any other sender) and
// admitted post-fork (priced at value alone via EffectiveCost).
func TestReservedFallbackFeeAdmissionForkGate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		forkBlock *big.Int
		wantErr   bool
	}{
		{name: "pre-fork rejected", forkBlock: big.NewInt(100), wantErr: true}, // fork in the future
		{name: "post-fork admitted", forkBlock: big.NewInt(0), wantErr: false}, // fork already active
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, _ := crypto.GenerateKey()
			addr := crypto.PubkeyToAddress(key.PublicKey)
			pool := setupReservedPoolAt(addr, tt.forkBlock)
			defer pool.Close()

			testAddBalance(pool, addr, reservedFallbackFeeBalance)
			tx := fallbackFeeTx(t, pool.chainconfig, key, 0, big.NewInt(1000), big.NewInt(30_000_000_000))
			err := pool.Add([]*types.Transaction{tx}, true)[0]
			if tt.wantErr {
				if err == nil {
					t.Fatal("fallback-fee tx from a value-only-balance sender should be rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("fallback-fee tx from a value-only-balance reserved sender rejected: %v", err)
			}
			if pool.Get(tx.Hash()) == nil {
				t.Fatal("admitted fallback-fee tx not kept in pool")
			}
		})
	}
}

// TestReservedFallbackFeePendingFilterKeepsValueOnlyBalance pins the pending-
// side half of §4.3: demoteUnexecutables' balance-driven list.Filter call
// prices a reserved sender's pending fallback-fee tx on the value basis, so
// it survives repeated per-head revalidation despite never covering its full
// cost.
func TestReservedFallbackFeePendingFilterKeepsValueOnlyBalance(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	testAddBalance(pool, addr, reservedFallbackFeeBalance)
	tx := fallbackFeeTx(t, pool.chainconfig, key, 0, big.NewInt(1000), big.NewInt(30_000_000_000))
	if err := pool.Add([]*types.Transaction{tx}, true)[0]; err != nil {
		t.Fatalf("admission rejected: %v", err)
	}
	if pool.Status(tx.Hash()) != txpool.TxStatusPending {
		t.Fatalf("tx should be pending, got status %v", pool.Status(tx.Hash()))
	}

	// Repeated pending-side revalidation must not drop it: its full cost never
	// changes and always exceeds the balance, so only the value basis keeps it.
	for i := 0; i < 3; i++ {
		pool.mu.Lock()
		pool.demoteUnexecutables()
		pool.mu.Unlock()
		if pool.Get(tx.Hash()) == nil {
			t.Fatalf("fallback-fee tx dropped by pending Filter on pass %d", i)
		}
		if pool.Status(tx.Hash()) != txpool.TxStatusPending {
			t.Fatalf("tx should remain pending on pass %d, got status %v", i, pool.Status(tx.Hash()))
		}
	}
}

// TestReservedFallbackFeeDeregistrationDropsPending pins the self-healing
// half of §4.2: once a sender is deregistered, the next head's pending-side
// Filter prices it at full cost again and drops what the balance can no
// longer cover, with one block of lag (the snapshot rebuild).
func TestReservedFallbackFeeDeregistrationDropsPending(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	testAddBalance(pool, addr, reservedFallbackFeeBalance)
	tx := fallbackFeeTx(t, pool.chainconfig, key, 0, big.NewInt(1000), big.NewInt(30_000_000_000))
	if err := pool.Add([]*types.Transaction{tx}, true)[0]; err != nil {
		t.Fatalf("admission rejected: %v", err)
	}

	// One Filter pass while still reserved: survives (value basis).
	pool.mu.Lock()
	pool.demoteUnexecutables()
	pool.mu.Unlock()
	if pool.Get(tx.Hash()) == nil {
		t.Fatal("fallback-fee tx dropped while sender still reserved")
	}

	deregisterReserved(t, pool, addr) // simulates the next head's snapshot rebuild

	// Next Filter pass: sender is now priced at full cost, which the balance
	// never covered, so it must be dropped.
	pool.mu.Lock()
	pool.demoteUnexecutables()
	pool.mu.Unlock()
	if pool.Get(tx.Hash()) != nil {
		t.Fatal("fallback-fee tx should have been dropped after deregistration")
	}
}

// TestReservedSnapshotSwapRace exercises the atomic snapshot pointer under
// -race: admissions read it (via isReserved/effectiveCost) concurrently with
// repeated snapshot rebuilds, mirroring how a real head event's
// rebuildReservedSnapshot races against concurrent pool.Add calls.
func TestReservedSnapshotSwapRace(t *testing.T) {
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	testAddBalance(pool, addr, big.NewInt(1_000_000))
	cfg := pool.chainconfig

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				refreshReservedSnapshot(pool)
			}
		}
	}()

	for i := 0; i < 100; i++ {
		tx := zeroFeeTx(t, cfg, key, uint64(i), common.Address{0x42})
		pool.Add([]*types.Transaction{tx}, false)
	}
	close(stop)
	wg.Wait()
}

// TestExistingExpenditureBasisConsistency pins that the pool's
// ExistingExpenditure callback picks its basis (value vs full cost) from the
// sender's *current* classification at each admission, not a basis cached
// from an earlier one, when classification flips between two admissions of
// the same sender.
func TestExistingExpenditureBasisConsistency(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	// Affords one tx's full cost (value(1000) + 100_000 gas * 30 gwei) with
	// room to spare, but not two of them: balance is one wei short of 2x
	// that cost, so a same-basis overdraft check for a second, identically
	// priced tx must reject it.
	const gas = 100_000
	feeCapInt := big.NewInt(30_000_000_000)
	cost1 := new(big.Int).Add(big.NewInt(1000), new(big.Int).Mul(big.NewInt(gas), feeCapInt))
	balance := new(big.Int).Sub(new(big.Int).Mul(big.NewInt(2), cost1), big.NewInt(1))
	testAddBalance(pool, addr, balance)

	feeCap := feeCapInt // exactly the pool's tip floor
	tx1 := fallbackFeeTx(t, pool.chainconfig, key, 0, big.NewInt(1000), feeCap)
	if err := pool.Add([]*types.Transaction{tx1}, true)[0]; err != nil {
		t.Fatalf("tx1 admission while reserved rejected: %v", err)
	}

	fr := deregisterReserved(t, pool, addr)

	// tx2's own cost fits the balance alone; only an overdraft check that
	// correctly switched ExistingExpenditure to the full-cost basis (now
	// including tx1's *full* cost, not its admitted value) overshoots by the
	// value/cost gap and rejects it. A basis stuck on tx1's value would wrongly
	// admit it (1000 + tx2's cost fits comfortably).
	tx2 := fallbackFeeTx(t, pool.chainconfig, key, 1, big.NewInt(1000), feeCap)
	if err := pool.Add([]*types.Transaction{tx2}, true)[0]; err == nil {
		t.Fatal("tx2 should be rejected: overdraft check must price tx1 at full cost once non-reserved")
	}

	// Flipping back to reserved and retrying the identical tx must now
	// succeed: ExistingExpenditure is back on the value basis (tx1's value
	// alone), which comfortably fits alongside tx2's own value.
	fr.register(addr)
	refreshReservedSnapshot(pool)
	if err := pool.Add([]*types.Transaction{tx2}, true)[0]; err != nil {
		t.Fatalf("tx2 admission after re-registering rejected: %v", err)
	}
}

// TestReservedQueueLifecyclePromotesOnGapFill pins §4.3's queue-side half: a
// nonce-gapped fallback-fee tx from a value-only-balance reserved sender is
// admitted into the future queue, survives promotion passes while the gap
// remains (queue.promoteExecutables' Filter call on the value basis), and
// promotes to pending once the gap fills.
func TestReservedQueueLifecyclePromotesOnGapFill(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	testAddBalance(pool, addr, reservedFallbackFeeBalance)
	feeCap := big.NewInt(30_000_000_000)

	gapped := fallbackFeeTx(t, pool.chainconfig, key, 1, big.NewInt(1000), feeCap) // nonce 1, nonce 0 missing
	if err := pool.Add([]*types.Transaction{gapped}, true)[0]; err != nil {
		t.Fatalf("gapped fallback-fee tx rejected: %v", err)
	}
	if pool.Status(gapped.Hash()) != txpool.TxStatusQueued {
		t.Fatalf("gapped tx should be queued, got status %v", pool.Status(gapped.Hash()))
	}

	// A promotion pass with the gap still open must not drop it: the queue's
	// balance-driven Filter prices the reserved sender on the value basis.
	pool.mu.Lock()
	pool.promoteExecutables([]common.Address{addr})
	pool.mu.Unlock()
	if pool.Get(gapped.Hash()) == nil {
		t.Fatal("gapped fallback-fee tx dropped by queue Filter while still reserved")
	}
	if pool.Status(gapped.Hash()) != txpool.TxStatusQueued {
		t.Fatalf("tx should remain queued, got status %v", pool.Status(gapped.Hash()))
	}

	// Fill the gap: nonce 0, small and easily affordable even without the
	// reserved waiver. Admission (sync) drives the reorg that promotes both.
	filler := fallbackFeeTx(t, pool.chainconfig, key, 0, big.NewInt(1000), feeCap)
	if err := pool.Add([]*types.Transaction{filler}, true)[0]; err != nil {
		t.Fatalf("gap-filling tx rejected: %v", err)
	}
	if pool.Status(filler.Hash()) != txpool.TxStatusPending {
		t.Fatalf("gap-filling tx should be pending, got status %v", pool.Status(filler.Hash()))
	}
	if pool.Status(gapped.Hash()) != txpool.TxStatusPending {
		t.Fatalf("previously gapped tx should have promoted to pending, got status %v", pool.Status(gapped.Hash()))
	}
}

// TestReservedQueueLifecycleDropsAfterDeregistration pins §4.2's self-healing
// claim for the queue side: a queued fallback-fee tx from a sender that gets
// deregistered while still gapped is dropped at the next promotion pass, once
// priced at full cost.
func TestReservedQueueLifecycleDropsAfterDeregistration(t *testing.T) {
	t.Parallel()

	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)
	pool := setupReservedPool(addr)
	defer pool.Close()

	testAddBalance(pool, addr, reservedFallbackFeeBalance)
	feeCap := big.NewInt(30_000_000_000)

	gapped := fallbackFeeTx(t, pool.chainconfig, key, 1, big.NewInt(1000), feeCap)
	if err := pool.Add([]*types.Transaction{gapped}, true)[0]; err != nil {
		t.Fatalf("gapped fallback-fee tx rejected: %v", err)
	}

	deregisterReserved(t, pool, addr)

	pool.mu.Lock()
	pool.promoteExecutables([]common.Address{addr})
	pool.mu.Unlock()
	if pool.Get(gapped.Hash()) != nil {
		t.Fatal("gapped fallback-fee tx should have been dropped after deregistration")
	}
	if pool.Status(gapped.Hash()) != txpool.TxStatusUnknown {
		t.Fatalf("dropped tx should be unknown, got status %v", pool.Status(gapped.Hash()))
	}
}
