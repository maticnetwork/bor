package miner

import (
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// recordingSequencer captures the worker's sequencer callbacks.
type recordingSequencer struct {
	mu      sync.Mutex
	refresh time.Duration
	opens   []uint64
	seals   []uint64
	txs     int
	order   []byte

	adoptable *AdoptedWindow // handed out once when the queried block matches
	adopted   int
	verdict   SealVerdict
	resyncN   int  // number of rebuild signals a test wants served
	contested bool // when set, AwaitSequenced reports the window contested
}

func (r *recordingSequencer) OpenBlock(number uint64, _ uint64, _ common.Hash, _ uint64, _ *big.Int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.opens = append(r.opens, number)
	r.order = append(r.order, 'o')
}

func (r *recordingSequencer) PublishTx(*types.Transaction) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.txs++
	r.order = append(r.order, 't')
}

func (r *recordingSequencer) SealBlock(block *types.Block) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seals = append(r.seals, block.NumberU64())
	r.order = append(r.order, 's')
}

func (r *recordingSequencer) AdoptWindow(number uint64, parent common.Hash) *AdoptedWindow {
	r.mu.Lock()
	defer r.mu.Unlock()

	a := r.adoptable
	if a == nil || a.Number != number || a.ParentHash != parent {
		return nil
	}

	r.adoptable = nil
	r.adopted++

	return a
}

func (r *recordingSequencer) AwaitSequenced(time.Duration, uint64, []*types.Transaction) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return !r.contested
}

// verdict is what ConfirmSeal reports; the zero value (SealUnknown) lets
// every existing test proceed to broadcast unchanged.
func (r *recordingSequencer) ConfirmSeal(time.Duration) SealVerdict {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.verdict
}

func (r *recordingSequencer) ResyncNeeded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.resyncN <= 0 {
		return false
	}

	r.resyncN--

	return true
}

func (r *recordingSequencer) RefreshInterval() time.Duration {
	return r.refresh
}

func (r *recordingSequencer) snapshot() (opens, seals []uint64, txs int, order []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]uint64(nil), r.opens...), append([]uint64(nil), r.seals...), r.txs, append([]byte(nil), r.order...)
}

// A muted build (a signer Seal would refuse) keeps every sequencer hook
// silent while still building: no open, no adoption read, and the barrier
// seals through without consulting the store.
func TestMutedBuildKeepsSequencerSilent(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)
	w.running.Store(true) // the hooks gate on IsRunning; no loops are started

	parent := b.chain.CurrentBlock()
	number := new(big.Int).Add(parent.Number, big.NewInt(1))
	header := &types.Header{
		Number:     number,
		ParentHash: parent.Hash(),
		Time:       parent.Time + 1,
		GasLimit:   parent.GasLimit,
		BaseFee:    big.NewInt(1),
	}

	env := &environment{header: header, sequencerMuted: true}
	w.sequencerOpen(env)

	rec.adoptable = &AdoptedWindow{Number: number.Uint64(), ParentHash: parent.Hash()}
	genParams := &generateParams{production: true, sequencerMuted: true}
	w.fetchAdoption(genParams, header)

	// The mock refuses AwaitSequenced (contested): only the muted gate can
	// let the seal through.
	rec.contested = true

	if !w.sealBarrier(env) {
		t.Fatal("muted build must seal without consulting the store")
	}

	if genParams.adoption != nil {
		t.Fatal("muted build must not adopt a store window")
	}

	rec.mu.Lock()
	opens, adopted := len(rec.opens), rec.adopted
	rec.mu.Unlock()

	if opens != 0 || adopted != 0 {
		t.Fatalf("muted build touched the store: opens=%d adopted=%d", opens, adopted)
	}
}

func newSequencerTestWorker(t *testing.T) (*worker, *testWorkerBackend, *recordingSequencer) {
	t.Helper()

	db := rawdb.NewMemoryDatabase()
	config := *params.AllCliqueProtocolChanges
	config.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}

	// Rio from genesis: adoption is Rio-gated (sequencerActive), and the
	// template config carries a Bor section with no Rio block scheduled.
	borCfg := *config.Bor
	borCfg.RioBlock = big.NewInt(0)
	config.Bor = &borCfg

	engine := clique.New(config.Clique, db)

	w, b, _ := newTestWorker(t, DefaultTestConfig(), &config, engine, db, false, 0)
	t.Cleanup(w.close)

	rec := &recordingSequencer{}
	w.sequencer = rec

	return w, b, rec
}

// The worker publishes the full lifecycle — open before transactions,
// transactions before the seal — for produced blocks.
func TestSequencerLifecycleHooks(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)

	// Finality has ratified this chain, as it has in any steady state.
	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())

	w.start()
	defer w.stop()

	b.txPool.Add([]*types.Transaction{b.newRandomTx(false)}, false)

	// The first sealed block may predate the transaction; poll until every
	// hook kind has fired.
	deadline := time.Now().Add(10 * time.Second)

	var (
		opens, seals []uint64
		txs          int
		order        []byte
	)

	for {
		opens, seals, txs, order = rec.snapshot()
		if len(opens) > 0 && len(seals) > 0 && txs > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("hooks missed: opens=%d seals=%d txs=%d", len(opens), len(seals), txs)
		}

		time.Sleep(50 * time.Millisecond)
	}

	firstOpen, firstSeal := -1, -1

	for i, k := range order {
		if k == 'o' && firstOpen < 0 {
			firstOpen = i
		}

		if k == 's' && firstSeal < 0 {
			firstSeal = i
		}
	}

	if firstSeal < firstOpen {
		t.Fatalf("seal published before any open: order %q", order)
	}
}

// fillUntilAnnounce fills immediately, then keeps polling the pool until
// the announce margin, so a transaction arriving mid-block is committed
// without waiting for the next slot.
func TestFillUntilAnnounceCommitsLateTx(t *testing.T) {
	t.Parallel()

	w, b, _ := newSequencerTestWorker(t)

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	// The harness's preloaded txs are signed for another chain id and never
	// enter this pool; drive the test with the backend's own tx builder,
	// with explicit nonces. (Committed txs stay "pending" in the pool until
	// import, so tcount is the observable throughout.)
	b.txPool.Add([]*types.Transaction{b.newRandomTxWithNonce(false, 0)}, true)
	waitFor(t, 5*time.Second, func() bool {
		return countPendingTransactions(b) >= 1
	})

	// The loop runs until ~announce time; give it a window.
	work.header.Time = uint64(time.Now().Unix()) + 3

	go func() {
		time.Sleep(300 * time.Millisecond)
		b.txPool.Add([]*types.Transaction{b.newRandomTxWithNonce(false, 1)}, true)
	}()

	if err := w.fillUntilAnnounce(nil, work, genParams, 100*time.Millisecond); err != nil {
		t.Fatalf("fill until announce: %v", err)
	}

	// The initial fill commits the first tx; a later poll must pick up the
	// late one on top.
	if work.tcount < 2 {
		t.Fatalf("late transaction not committed: tcount %d, want >= 2", work.tcount)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// With the announce time already past, the poll loop exits right after the
// initial fill — no lingering until a stale deadline.
func TestFillUntilAnnounceLateBlockExitsImmediately(t *testing.T) {
	t.Parallel()

	w, _, _ := newSequencerTestWorker(t)

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	work.header.Time = uint64(time.Now().Unix()) - 10

	start := time.Now()

	if err := w.fillUntilAnnounce(nil, work, genParams, 100*time.Millisecond); err != nil {
		t.Fatalf("fill until announce: %v", err)
	}

	if time.Since(start) > time.Second {
		t.Fatal("loop did not exit immediately for a late block")
	}
}

// A mid-loop interrupt aborts exactly as it does on the stock path.
func TestFillUntilAnnounceInterrupt(t *testing.T) {
	t.Parallel()

	w, _, _ := newSequencerTestWorker(t)

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	work.header.Time = uint64(time.Now().Unix()) + 5

	interrupt := new(atomic.Int32)

	go func() {
		time.Sleep(200 * time.Millisecond)
		interrupt.Store(commitInterruptNewHead)
	}()

	if err := w.fillUntilAnnounce(interrupt, work, genParams, 50*time.Millisecond); !errors.Is(err, errBlockInterruptedByNewHead) {
		t.Fatalf("err = %v, want interrupt", err)
	}
}

// The poll gate: zero unless a sequencer is attached, the worker is
// producing, and a poll cadence is configured.
func TestSequencerPollGate(t *testing.T) {
	t.Parallel()

	w, _, rec := newSequencerTestWorker(t)

	// Attached but not producing.
	rec.refresh = 100 * time.Millisecond
	if got := w.sequencerPoll(big.NewInt(1)); got != 0 {
		t.Fatalf("poll while not producing = %v", got)
	}

	// No sequencer at all.
	w.sequencer = nil
	if got := w.sequencerPoll(big.NewInt(1)); got != 0 {
		t.Fatalf("poll without sequencer = %v", got)
	}
}

// An adoptable window is applied verbatim: the header inherits the adopted
// context and the window's transactions are committed before the pool's.
func TestAdoptedWindowSeedsBlock(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)

	parent := b.chain.CurrentBlock()
	adopted := &AdoptedWindow{
		Number:     parent.Number.Uint64() + 1,
		Timestamp:  parent.Time + 2,
		ParentHash: parent.Hash(),
		GasLimit:   parent.GasLimit,
		BaseFee:    eip1559.CalcBaseFee(b.chain.Config(), parent),
		Txs: []*types.Transaction{
			b.newRandomTxWithNonce(false, 0),
			b.newRandomTxWithNonce(false, 1),
		},
	}

	rec.mu.Lock()
	rec.adoptable = adopted
	rec.mu.Unlock()

	// Finality has ratified this chain, as it has in any steady state.
	b.setMilestone(parent.Number.Uint64(), parent.Hash())

	// The empty pre-seal shortcut would race the seeded block to the same
	// height; production sequencing targets post-Rio, where it is skipped.
	w.noempty.Store(true)

	w.start()
	defer w.stop()

	var block *types.Block

	waitFor(t, 10*time.Second, func() bool {
		rec.mu.Lock()
		adoptions := rec.adopted
		rec.mu.Unlock()

		block = b.chain.GetBlockByNumber(adopted.Number)

		return adoptions == 1 && block != nil && len(block.Transactions()) >= 2
	})

	if block.Time() != adopted.Timestamp {
		t.Fatalf("sealed time %d, want adopted %d", block.Time(), adopted.Timestamp)
	}

	if block.GasLimit() != adopted.GasLimit {
		t.Fatalf("sealed gas limit %d, want adopted %d", block.GasLimit(), adopted.GasLimit)
	}

	for i, tx := range adopted.Txs {
		if block.Transactions()[i].Hash() != tx.Hash() {
			t.Fatalf("tx %d = %s, want adopted %s", i, block.Transactions()[i].Hash(), tx.Hash())
		}
	}
}

// A window for a block that is not the one being built is left alone.
func TestAdoptionSkipsMismatchedWindow(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)

	parent := b.chain.CurrentBlock()
	rec.mu.Lock()
	rec.adoptable = &AdoptedWindow{
		Number:     parent.Number.Uint64() + 5, // not the next block
		Timestamp:  parent.Time + 2,
		ParentHash: parent.Hash(),
		GasLimit:   parent.GasLimit,
		BaseFee:    eip1559.CalcBaseFee(b.chain.Config(), parent),
	}
	rec.mu.Unlock()

	// Finality has ratified this chain, as it has in any steady state.
	b.setMilestone(parent.Number.Uint64(), parent.Hash())

	w.start()
	defer w.stop()

	waitFor(t, 10*time.Second, func() bool {
		return b.chain.GetBlockByNumber(parent.Number.Uint64()+1) != nil
	})

	rec.mu.Lock()
	defer rec.mu.Unlock()

	if rec.adopted != 0 {
		t.Fatal("mismatched window must not be adopted")
	}
}

// applyAdoption applies only a window that matches the build exactly and
// passes every consensus bound; anything else drops the adoption.
func TestApplyAdoptionValidation(t *testing.T) {
	t.Parallel()

	w, b, _ := newSequencerTestWorker(t)

	parent := b.chain.CurrentBlock()
	base := func() *AdoptedWindow {
		return &AdoptedWindow{
			Number:     parent.Number.Uint64() + 1,
			Timestamp:  parent.Time + 2,
			ParentHash: parent.Hash(),
			GasLimit:   parent.GasLimit,
			BaseFee:    eip1559.CalcBaseFee(b.chain.Config(), parent),
			Txs:        []*types.Transaction{b.newRandomTxWithNonce(false, 0)},
		}
	}

	cases := []struct {
		name      string
		mutate    func(*AdoptedWindow)
		wantAdopt bool
	}{
		{name: "valid", mutate: func(*AdoptedWindow) {}, wantAdopt: true},
		{name: "wrong number", mutate: func(a *AdoptedWindow) { a.Number += 3 }},
		{name: "wrong parent", mutate: func(a *AdoptedWindow) { a.ParentHash = common.Hash{0x99} }},
		{name: "timestamp at parent", mutate: func(a *AdoptedWindow) { a.Timestamp = parent.Time }},
		{name: "timestamp too far in future", mutate: func(a *AdoptedWindow) {
			a.Timestamp = uint64(time.Now().Unix()) + maxAdoptedFutureSeconds + 10
		}},
		{name: "gas limit out of bound", mutate: func(a *AdoptedWindow) { a.GasLimit = parent.GasLimit * 3 }},
		{name: "base fee mismatch", mutate: func(a *AdoptedWindow) { a.BaseFee = big.NewInt(1) }},
		{name: "nil base fee", mutate: func(a *AdoptedWindow) { a.BaseFee = nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			genParams := &generateParams{coinbase: testBankAddress}

			header, _, err := w.makeHeader(genParams, false)
			if err != nil {
				t.Fatalf("makeHeader: %v", err)
			}

			a := base()
			tc.mutate(a)
			genParams.adoption = a

			w.applyAdoption(genParams, header)

			if got := genParams.adoption != nil; got != tc.wantAdopt {
				t.Fatalf("adoption kept = %v, want %v", got, tc.wantAdopt)
			}

			if tc.wantAdopt {
				if header.Time != a.Timestamp || header.GasLimit != a.GasLimit {
					t.Fatalf("header not rewritten: time %d gas %d", header.Time, header.GasLimit)
				}

				if header.ActualTime.Before(time.Now().Add(adoptionSeedBudget / 2)) {
					t.Fatal("announce deadline missing the seed budget")
				}
			}
		})
	}
}

// fetchAdoption queries for exactly the prepared header's height and
// parent, and only while running — it sits after engine.Prepare, so only
// an authorized build ever reads the store.
func TestFetchAdoptionUsesPreparedHeader(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)

	parent := b.chain.CurrentBlock()
	header := &types.Header{
		Number:     new(big.Int).Add(parent.Number, common.Big1),
		ParentHash: parent.Hash(),
	}

	rec.mu.Lock()
	rec.adoptable = &AdoptedWindow{
		Number:     header.Number.Uint64(),
		ParentHash: header.ParentHash,
	}
	rec.mu.Unlock()

	// Not running: no query at all.
	genParams := &generateParams{production: true}
	w.fetchAdoption(genParams, header)

	if genParams.adoption != nil {
		t.Fatal("fetch while not producing")
	}

	w.start()
	defer w.stop()

	// Payload building (production unset) must never touch the sequencer.
	genParams = &generateParams{}
	w.fetchAdoption(genParams, header)

	if genParams.adoption != nil {
		t.Fatal("payload build queried the sequencer")
	}

	genParams = &generateParams{production: true}
	w.fetchAdoption(genParams, header)

	if genParams.adoption == nil {
		t.Fatal("prepared header did not resolve the window")
	}
}

// An adopted window is already published and preconfirmed, so the seed
// commits all of it. A partial seed would leave the block short of what
// consumers were promised and displace the remainder to the next height.
func TestSeedAdoptedCommitsWholeWindow(t *testing.T) {
	t.Parallel()

	w, b, _ := newSequencerTestWorker(t)

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	window := &AdoptedWindow{Txs: []*types.Transaction{
		b.newRandomTxWithNonce(false, 0),
		b.newRandomTxWithNonce(false, 1),
		b.newRandomTxWithNonce(false, 2),
	}}

	w.seedAdopted(work, window)

	if work.tcount != len(window.Txs) {
		t.Fatalf("seed committed %d of %d adopted txs", work.tcount, len(window.Txs))
	}
}

// Adoption is Rio-gated on bor chains: pre-Rio, every validator builds
// every height and an inherited timestamp is invalid for any other signer.
func TestSequencerRioGate(t *testing.T) {
	t.Parallel()

	if sequencerActive(&params.BorConfig{RioBlock: big.NewInt(1_000_000)}, big.NewInt(1)) {
		t.Fatal("pre-Rio bor chain must not sequence")
	}

	if !sequencerActive(&params.BorConfig{RioBlock: big.NewInt(0)}, big.NewInt(1)) {
		t.Fatal("post-Rio bor chain must sequence")
	}

	if !sequencerActive(nil, big.NewInt(1)) {
		t.Fatal("non-bor chain (tests) must allow sequencing")
	}
}

// The adoption timestamp floor: the parent period under bor, parent.Time+1
// without a bor config. Exercised on the pure helper — swapping a live
// worker's chainConfig to vary it races the worker's newWorkLoop, which
// re-reads chainConfig.Bor on every veblop tick.
func TestAdoptionMinTime(t *testing.T) {
	t.Parallel()

	if got := adoptionMinTime(nil, 41, 7); got != 42 {
		t.Fatalf("nil-bor floor %d, want parent.Time+1", got)
	}

	borCfg := &params.BorConfig{Period: map[string]uint64{"0": 2}}
	if got := adoptionMinTime(borCfg, 40, 7); got != 42 {
		t.Fatalf("bor floor %d, want parent.Time+period", got)
	}
}

// sequencerPoll gates on the full sequencing predicate and forwards the
// publisher's cadence: zero when the node is not producing, the configured
// interval when it is.
func TestSequencerPollGating(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)
	rec.refresh = 75 * time.Millisecond

	n := new(big.Int).Add(b.chain.CurrentBlock().Number, big.NewInt(1))

	if got := w.sequencerPoll(n); got != 0 {
		t.Fatalf("poll while stopped = %v, want 0", got)
	}

	w.start()

	if got := w.sequencerPoll(n); got != 75*time.Millisecond {
		t.Fatalf("poll while running = %v, want 75ms", got)
	}
}

// A zero poll is the one-shot fill: fillBlock must return immediately, not
// hold the block open until the announce deadline.
func TestFillBlockZeroPollIsOneShot(t *testing.T) {
	t.Parallel()

	w, _, rec := newSequencerTestWorker(t)
	rec.refresh = 0
	w.start()

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	work.header.ActualTime = time.Now().Add(2 * time.Second)

	start := time.Now()

	if err := w.fillBlock(nil, work, genParams); err != nil {
		t.Fatalf("fillBlock: %v", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("zero poll held the block open %v — one-shot fill expected", elapsed)
	}
}

// With a Bor config the adoption timestamp floor is parent.Time plus the
// configured block period, not the +1 fallback: a window between the two
// floors is rejected.
func TestApplyAdoptionTimestampFloor(t *testing.T) {
	t.Parallel()

	w, b, _ := newSequencerTestWorker(t)

	parent := b.chain.CurrentBlock()
	genParams := &generateParams{coinbase: testBankAddress}

	header, _, err := w.makeHeader(genParams, false)
	if err != nil {
		t.Fatalf("makeHeader: %v", err)
	}

	floor := adoptionMinTime(w.chainConfig.Bor, parent.Time, parent.Number.Uint64()+1)

	// Base fee copied from the prepared header (the value applyAdoption
	// compares against): the timestamp floor is then the only bound that
	// can reject this window.
	window := func(ts uint64) *AdoptedWindow {
		return &AdoptedWindow{
			Number:     parent.Number.Uint64() + 1,
			Timestamp:  ts,
			ParentHash: parent.Hash(),
			GasLimit:   header.GasLimit,
			BaseFee:    new(big.Int).Set(header.BaseFee),
		}
	}

	genParams.adoption = window(floor - 1)
	w.applyAdoption(genParams, header)

	if genParams.adoption != nil {
		t.Fatal("window below the timestamp floor must be rejected")
	}

	genParams.adoption = window(floor)
	w.applyAdoption(genParams, header)

	if genParams.adoption == nil {
		t.Fatal("window exactly on the floor must be adopted")
	}
}

// A competing producer at our height aborts the fill so the next work cycle
// adopts their window: the build must not seal content that diverges from
// the sequence consumers already saw.
func TestFillBlockRestartsOnResync(t *testing.T) {
	t.Parallel()

	w, _, rec := newSequencerTestWorker(t)
	rec.refresh = 20 * time.Millisecond
	w.start()

	// The worker's own production loop also consumes the signal; arm
	// enough for this explicit build to observe one.
	rec.mu.Lock()
	rec.resyncN = 64
	rec.mu.Unlock()

	genParams := &generateParams{coinbase: testBankAddress}

	work, err := w.prepareWork(genParams, false)
	if err != nil {
		t.Fatalf("prepareWork: %v", err)
	}

	work.header.ActualTime = time.Now().Add(3 * time.Second)

	if err := w.fillBlock(nil, work, genParams); !errors.Is(err, errRebuildForSequence) {
		t.Fatalf("fillBlock err = %v, want errRebuildForSequence", err)
	}

	// Each read consumes one signal: with none armed the build proceeds.
	rec.mu.Lock()
	rec.resyncN = 0
	rec.mu.Unlock()

	if rec.ResyncNeeded() {
		t.Fatal("resync signal must be consumed, not sticky")
	}
}

// A contested window must not clear the barrier, so the seal path declines
// to commit it. The barrier's own semantics are covered in the sequencer
// package; this pins the worker-side gate.
func TestContestedWindowBlocksSeal(t *testing.T) {
	t.Parallel()

	w, b, rec := newSequencerTestWorker(t)
	w.start()

	n := new(big.Int).Add(b.chain.CurrentBlock().Number, big.NewInt(1))

	if !w.sequencingActive(n) {
		t.Fatal("sequencing must be active for the gate to apply")
	}

	if !w.sequencer.AwaitSequenced(time.Second, 1, nil) {
		t.Fatal("an uncontested window should clear")
	}

	rec.mu.Lock()
	rec.contested = true
	rec.mu.Unlock()

	if w.sequencer.AwaitSequenced(time.Second, 1, nil) {
		t.Fatal("a contested window must fail the gate that precedes commit")
	}
}

// A refused seal never reaches the chain: the store elected another
// producer's block for the height, and broadcasting ours would fork an
// already-decided height. The worker keeps sealing (each attempt is
// discarded), so refusal costs blocks, never liveness machinery.
func TestSealGateRefusalStopsBroadcast(t *testing.T) {
	w, b, rec := newSequencerTestWorker(t)

	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())

	rec.mu.Lock()
	rec.verdict = SealRefused
	rec.mu.Unlock()

	w.start()
	defer w.stop()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, seals, _, _ := rec.snapshot()
		if len(seals) > 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("worker never sealed")
		}

		time.Sleep(20 * time.Millisecond)
	}

	if got := b.chain.CurrentBlock().Number.Uint64(); got != 0 {
		t.Fatalf("chain advanced to %d on refused seals: a refused block "+
			"was written and broadcast", got)
	}
}

// A refused seal must also clear its pending task. The devnet incident: the
// elected producer's refused block left its task in pendingTasks — nothing
// ever advances the chain past a refused height, so clearPending never ran —
// and the veblop stall fallback (decideVeblopFallback) read the leaked task
// as sealing-in-flight and skipped recovery forever. The chain froze one
// height below with the producer never building again.
func TestSealGateRefusalClearsPendingTask(t *testing.T) {
	w, b, rec := newSequencerTestWorker(t)

	head := b.chain.CurrentBlock()
	b.setMilestone(head.Number.Uint64(), head.Hash())

	rec.mu.Lock()
	rec.verdict = SealRefused
	rec.mu.Unlock()

	w.start()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, seals, _, _ := rec.snapshot()
		if len(seals) >= 1 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("worker never sealed")
		}

		time.Sleep(20 * time.Millisecond)
	}

	// Stop building so no fresh task races the assertion; leaked tasks have
	// no other way out (clearPending needs chain progress a refused height
	// never makes).
	w.stop()

	deadline = time.Now().Add(5 * time.Second)
	for {
		w.pendingMu.RLock()
		pending := len(w.pendingTasks)
		w.pendingMu.RUnlock()

		if pending == 0 {
			break
		}

		if time.Now().After(deadline) {
			w.pendingMu.RLock()
			for h, task := range w.pendingTasks {
				t.Logf("leaked task: sealhash=%x block=%d created=%v", h[:4], task.block.NumberU64(), task.createdAt)
			}
			w.pendingMu.RUnlock()
			t.Fatalf("%d pending tasks leaked by refused seals: the veblop stall "+
				"fallback reads them as sealing-in-flight and never recovers", pending)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
