package blockstm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// --- Bug #1: Double FlushToMVStore prevention ---
// When a tx fails validation and is re-executed, FlushToMVStore must only
// be called once (inside execute()). A second call would double balance
// deltas because WriteDelta accumulates.

type mockV2State struct {
	flushCount atomic.Int32
	validated  bool
}

func (s *mockV2State) Validate() bool { return s.validated }
func (s *mockV2State) ValidateCategory() string {
	if s.validated {
		return ""
	}
	return "storage"
}
func (s *mockV2State) MarkEstimate()                           {}
func (s *mockV2State) CleanupEstimate([]Key, []common.Address) {}
func (s *mockV2State) GetWriteKeys() []Key                     { return nil }
func (s *mockV2State) GetBalAddrs() []common.Address           { return nil }
func (s *mockV2State) FlushToMVStore()                         { s.flushCount.Add(1) }
func (s *mockV2State) SetDeferMVWrites(bool)                   {}

type mockV2Task struct{ idx int }

func (t *mockV2Task) Index() int                    { return t.idx }
func (t *mockV2Task) Sender() common.Address        { return common.Address{} }
func (t *mockV2Task) To() *common.Address           { return nil }
func (t *mockV2Task) Authorities() []common.Address { return nil }

type mockV2Env struct {
	states []*mockV2State
}

func (e *mockV2Env) BaseNonce(common.Address) uint64 { return 0 }
func (e *mockV2Env) BaseCodeSize(common.Address) int { return 0 }
func (e *mockV2Env) Execute(task V2Task, workerID int, incarnation int,
	senderNonces map[common.Address]uint64, coinbase common.Address,
	waitForTx func(int), waitForFinal func(int), deferWrites bool) V2TxState {
	return e.states[task.Index()]
}
func (e *mockV2Env) Recycle(V2TxState) {}

func TestV2DoubleFlushPrevention(t *testing.T) {
	// Create 2 txs: tx0 passes validation, tx1 fails then passes on re-execution
	states := []*mockV2State{
		{validated: true},  // tx0 always passes
		{validated: false}, // tx1 fails first time
	}
	env := &mockV2Env{states: states}
	tasks := []V2Task{&mockV2Task{0}, &mockV2Task{1}}

	settleCount := 0
	settleFn := func(txIdx int, st V2TxState) {
		settleCount++
		ms := st.(*mockV2State)
		// After first validation failure, make tx1 pass on re-execution
		if txIdx == 1 {
			ms.validated = true
		}
	}

	result := ExecuteV2BlockSTM(context.Background(), tasks, env, common.Address{}, 1, nil, settleFn)

	// tx0: executed once, flushed once
	if states[0].flushCount.Load() != 1 {
		t.Errorf("tx0 FlushToMVStore called %d times, want 1", states[0].flushCount.Load())
	}

	// tx1: executed twice (initial + re-exec), flushed once per execution = 2
	if states[1].flushCount.Load() != 2 {
		t.Errorf("tx1 FlushToMVStore called %d times, want 2 (once per execution)", states[1].flushCount.Load())
	}

	if result.VFailCount != 1 {
		t.Errorf("VFailCount = %d, want 1", result.VFailCount)
	}
	if settleCount != 2 {
		t.Errorf("settleCount = %d, want 2", settleCount)
	}
}

// timedMockV2State extends mockV2State with optional execution delay and
// per-tx timestamp recording, so tests can observe the wall-clock ordering
// of initial executions and re-execution dispatches.
type timedMockV2State struct {
	mockV2State
	failsRemaining int32 // atomic; decrements on each failed Validate()
	mu             struct {
		sync.Mutex
		execStarts    []time.Time
		execEnds      []time.Time
		validateCalls []time.Time
	}
}

func (s *timedMockV2State) Validate() bool {
	s.mu.Lock()
	s.mu.validateCalls = append(s.mu.validateCalls, time.Now())
	s.mu.Unlock()
	if atomic.LoadInt32(&s.failsRemaining) > 0 {
		atomic.AddInt32(&s.failsRemaining, -1)
		return false
	}
	return true
}

func (s *timedMockV2State) ValidateCategory() string {
	if atomic.LoadInt32(&s.failsRemaining) > 0 {
		return "storage"
	}
	return ""
}

// timedMockV2Env supplies fresh timedMockV2States and runs each tx's
// "execution" as a sleep of execDelay. It records start/end timestamps
// keyed by tx index so the test can compare the timeline of tx i's
// initial vs tx j's re-execution dispatch.
type timedMockV2Env struct {
	delays []time.Duration
	states []*timedMockV2State
}

func (e *timedMockV2Env) BaseNonce(common.Address) uint64 { return 0 }
func (e *timedMockV2Env) BaseCodeSize(common.Address) int { return 0 }
func (e *timedMockV2Env) Execute(task V2Task, workerID int, incarnation int,
	senderNonces map[common.Address]uint64, coinbase common.Address,
	waitForTx func(int), waitForFinal func(int), deferWrites bool) V2TxState {
	idx := task.Index()
	s := e.states[idx]
	s.mu.Lock()
	s.mu.execStarts = append(s.mu.execStarts, time.Now())
	s.mu.Unlock()
	if e.delays[idx] > 0 {
		time.Sleep(e.delays[idx])
	}
	s.mu.Lock()
	s.mu.execEnds = append(s.mu.execEnds, time.Now())
	s.mu.Unlock()
	return s
}
func (e *timedMockV2Env) Recycle(V2TxState) {}

// TestV2ValidationLoopSerializationBlocksReexec verifies that V2's strict
// in-order validation loop delays a failing tx's re-execution until the
// validator has walked past every earlier tx — even when those earlier
// txs are independent and slow. The test demonstrates a real bottleneck
// the design accepts: re-execution of tx N cannot start until
// validateOne(N) has run, and validateOne(N) cannot run until
// validateOne(0..N-1) have all completed.
//
// Setup:
//   - tx0: slow initial execution (200ms), validates successfully
//   - tx1: instant initial execution, fails validation once → needs re-exec
//
// Both run on the worker pool concurrently from the start, so tx1's
// initial execution finishes long before tx0's. In an "ideal" parallel
// design, tx1's re-execution could start right after its initial
// completes (microseconds). In V2, it has to wait for validateOne(0),
// which blocks on tx0's slow initial.
//
// Expected observation: time(tx1 reexec start) - time(tx1 initial end)
// ≈ tx0's execDelay (~200ms), not ≈0.
func TestV2ValidationLoopSerializationBlocksReexec(t *testing.T) {
	const slow = 200 * time.Millisecond

	states := []*timedMockV2State{
		{}, // tx0: passes immediately
		{}, // tx1: fails once
	}
	atomic.StoreInt32(&states[1].failsRemaining, 1)

	env := &timedMockV2Env{
		delays: []time.Duration{slow, 0},
		states: states,
	}
	tasks := []V2Task{&mockV2Task{0}, &mockV2Task{1}}

	// Plenty of workers so initial execs of tx0 and tx1 can run together.
	_ = ExecuteV2BlockSTM(context.Background(), tasks, env, common.Address{}, 4, nil, func(int, V2TxState) {})

	// Pull the timestamps. Tx1 was "executed" twice — initial + re-exec.
	states[1].mu.Lock()
	defer states[1].mu.Unlock()
	if len(states[1].mu.execStarts) != 2 {
		t.Fatalf("tx1 expected 2 execution starts, got %d", len(states[1].mu.execStarts))
	}
	tx1InitialEnd := states[1].mu.execEnds[0]
	tx1ReexecStart := states[1].mu.execStarts[1]
	gap := tx1ReexecStart.Sub(tx1InitialEnd)

	t.Logf("tx0 initial took ~%s (slow)", slow)
	t.Logf("tx1 initial→reexec gap: %s", gap)

	// tx1's re-execution should be delayed by approximately tx0's slow
	// initial execution. Allow a generous tolerance (CI variance), but
	// require the gap to be a meaningful fraction of tx0's delay so we're
	// actually demonstrating the bottleneck.
	if gap < slow/2 {
		t.Errorf("tx1 re-exec started only %s after its initial — expected at least %s "+
			"(should be blocked behind tx0's %s initial via validateOne(0))",
			gap, slow/2, slow)
	}
	// And it shouldn't be *more* than tx0's delay plus a little — that would
	// indicate something else is wrong.
	if gap > slow+200*time.Millisecond {
		t.Errorf("tx1 re-exec gap %s far exceeds tx0's slow initial %s — unexpected delay",
			gap, slow)
	}
}

type panickingV2State struct{}

func (s *panickingV2State) Validate() bool                          { panic("synthetic validation panic") }
func (s *panickingV2State) ValidateCategory() string                { return "" }
func (s *panickingV2State) MarkEstimate()                           {}
func (s *panickingV2State) CleanupEstimate([]Key, []common.Address) {}
func (s *panickingV2State) GetWriteKeys() []Key                     { return nil }
func (s *panickingV2State) GetBalAddrs() []common.Address           { return nil }
func (s *panickingV2State) FlushToMVStore()                         {}
func (s *panickingV2State) SetDeferMVWrites(bool)                   {}

type panickingV2Env struct{ s *panickingV2State }

func (e *panickingV2Env) BaseNonce(common.Address) uint64 { return 0 }
func (e *panickingV2Env) BaseCodeSize(common.Address) int { return 0 }
func (e *panickingV2Env) Execute(task V2Task, workerID int, incarnation int,
	senderNonces map[common.Address]uint64, coinbase common.Address,
	waitForTx func(int), waitForFinal func(int), deferWrites bool) V2TxState {
	return e.s
}
func (e *panickingV2Env) Recycle(V2TxState) {}

// TestV2ValidationPanicIsRecovered: a panic in Validate() must surface
// via ValidationPanic rather than crashing the process; settle goroutine
// must still exit so ExecuteV2BlockSTM doesn't hang on wg.Wait.
func TestV2ValidationPanicIsRecovered(t *testing.T) {
	env := &panickingV2Env{s: &panickingV2State{}}
	tasks := []V2Task{&mockV2Task{0}}
	settleFn := func(int, V2TxState) {}

	result := ExecuteV2BlockSTM(context.Background(), tasks, env, common.Address{}, 2, nil, settleFn)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ValidationPanic == nil {
		t.Fatal("ValidationPanic must be non-nil")
	}
	if s, ok := result.ValidationPanic.(string); !ok || s != "synthetic validation panic" {
		t.Errorf("ValidationPanic = %v, want sentinel string", result.ValidationPanic)
	}
}

// nonceMockTask lets tests override Sender/Authorities per task.
type nonceMockTask struct {
	idx    int
	sender common.Address
	auths  []common.Address
}

func (t *nonceMockTask) Index() int                    { return t.idx }
func (t *nonceMockTask) Sender() common.Address        { return t.sender }
func (t *nonceMockTask) To() *common.Address           { return nil }
func (t *nonceMockTask) Authorities() []common.Address { return t.auths }

// nonceMockEnv reads BaseNonce and BaseCodeSize from per-address maps.
type nonceMockEnv struct {
	nonces    map[common.Address]uint64
	codeSizes map[common.Address]int
}

func (e *nonceMockEnv) BaseNonce(addr common.Address) uint64 {
	return e.nonces[addr]
}

func (e *nonceMockEnv) BaseCodeSize(addr common.Address) int {
	return e.codeSizes[addr]
}
func (e *nonceMockEnv) Execute(V2Task, int, int, map[common.Address]uint64, common.Address, func(int), func(int), bool) V2TxState {
	return nil
}
func (e *nonceMockEnv) Recycle(V2TxState) {}

// TestComputeSenderNonces_ExcludesAuthorities pins the production fix for
// the "nonce too low" sync-time bug seen on bor mainnet shortly after the
// EIP-7702 sender-chain rewrite landed. computeSenderNonces must NOT
// pre-compute SenderNonces for any address that appears as an EIP-7702
// authority anywhere in the block — otherwise an auth that fails at
// apply time (auth.nonce mismatch with current account nonce, signature
// recovery failure, etc.) would over-count the address's nonce delta,
// PDB.GetNonce would short-circuit to that bogus value without recording
// a read, validation could not invalidate it, and a later legacy tx
// FROM that address would be rejected with a "nonce too low" error.
func TestComputeSenderNonces_ExcludesAuthorities(t *testing.T) {
	addrA := common.HexToAddress("0xA")
	addrB := common.HexToAddress("0xB")
	addrC := common.HexToAddress("0xC")

	t.Run("same-sender chain without auth: chain-pos publish", func(t *testing.T) {
		// Two txs from A, no auth interaction → publish 0 and 1.
		tasks := []V2Task{
			&nonceMockTask{idx: 0, sender: addrA},
			&nonceMockTask{idx: 1, sender: addrA},
		}
		env := &nonceMockEnv{nonces: map[common.Address]uint64{addrA: 100}}
		out := computeSenderNonces(tasks, env)
		if got := out[0][addrA]; got != 100 {
			t.Errorf("tx0 SenderNonces[A] = %d, want 100", got)
		}
		if got := out[1][addrA]; got != 101 {
			t.Errorf("tx1 SenderNonces[A] = %d, want 101", got)
		}
	})

	t.Run("sender appears as authority in same block: no publish", func(t *testing.T) {
		// tx0: SetCodeTx, sender=B, auth=[A]. tx1, tx2 sent from A.
		// A is authority-bumpable → no SenderNonces for tx1/tx2; their
		// PDB.GetNonce falls through to MVStore which records reads
		// validation can invalidate.
		tasks := []V2Task{
			&nonceMockTask{idx: 0, sender: addrB, auths: []common.Address{addrA}},
			&nonceMockTask{idx: 1, sender: addrA},
			&nonceMockTask{idx: 2, sender: addrA},
		}
		env := &nonceMockEnv{nonces: map[common.Address]uint64{addrA: 0, addrB: 0}}
		out := computeSenderNonces(tasks, env)
		// B is not an authority: still gets nil because chain length is 1.
		if out[0] != nil {
			t.Errorf("tx0 SenderNonces = %v, want nil (single-tx sender)", out[0])
		}
		// A IS an authority: skip pre-compute even for same-sender chain.
		if out[1] != nil {
			t.Errorf("tx1 SenderNonces = %v, want nil (sender is auth)", out[1])
		}
		if out[2] != nil {
			t.Errorf("tx2 SenderNonces = %v, want nil (sender is auth)", out[2])
		}
	})

	t.Run("sender appears as authority in LATER tx: still no publish", func(t *testing.T) {
		// Order matters: A is authorised by tx[2] (which executes last
		// in tx-index order). The auth-list scan is a prefix-free pass
		// so A is exempt from publish for tx[0] and tx[1] too — both
		// MUST observe any nonce bump tx[2] eventually applies.
		tasks := []V2Task{
			&nonceMockTask{idx: 0, sender: addrA},
			&nonceMockTask{idx: 1, sender: addrA},
			&nonceMockTask{idx: 2, sender: addrC, auths: []common.Address{addrA}},
		}
		env := &nonceMockEnv{nonces: map[common.Address]uint64{addrA: 50, addrC: 0}}
		out := computeSenderNonces(tasks, env)
		if out[0] != nil || out[1] != nil {
			t.Errorf("A is auth in tx[2]; want nil for tx[0] and tx[1], got %v / %v", out[0], out[1])
		}
	})

	t.Run("non-auth chain plus an unrelated auth: chain still publishes", func(t *testing.T) {
		// tx[0]: SetCodeTx, sender=B, auth=[C].
		// tx[1], tx[2]: sent from A (chain length 2). A is not an auth → publish.
		tasks := []V2Task{
			&nonceMockTask{idx: 0, sender: addrB, auths: []common.Address{addrC}},
			&nonceMockTask{idx: 1, sender: addrA},
			&nonceMockTask{idx: 2, sender: addrA},
		}
		env := &nonceMockEnv{nonces: map[common.Address]uint64{addrA: 7}}
		out := computeSenderNonces(tasks, env)
		if got := out[1][addrA]; got != 7 {
			t.Errorf("tx1 SenderNonces[A] = %d, want 7", got)
		}
		if got := out[2][addrA]; got != 8 {
			t.Errorf("tx2 SenderNonces[A] = %d, want 8", got)
		}
	})

	// A sender delegated in an EARLIER block carries a designator at base
	// state but appears in no auth list of this block, so authoritySet
	// misses it. A CALL into it runs the delegate code in its own frame;
	// a CREATE there bumps the sender's nonce mid-block, invalidating any
	// pre-computed chain position after it. Excluding senders with
	// base-state code routes them through the MVStore path instead.
	t.Run("sender has base-state code: no publish", func(t *testing.T) {
		tasks := []V2Task{
			&nonceMockTask{idx: 0, sender: addrA},
			&nonceMockTask{idx: 1, sender: addrA},
		}
		env := &nonceMockEnv{
			nonces:    map[common.Address]uint64{addrA: 0},
			codeSizes: map[common.Address]int{addrA: 23}, // ef0100 || 20-byte target
		}
		out := computeSenderNonces(tasks, env)
		if out[0] != nil || out[1] != nil {
			t.Errorf("sender has base code; want nil for both, got %v / %v", out[0], out[1])
		}
	})
}
