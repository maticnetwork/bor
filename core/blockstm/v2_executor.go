package blockstm

import (
	"context"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
)

// ---------------------------------------------------------------------------
// V2 BlockSTM executor — interface-based (no core/state dependency)
// ---------------------------------------------------------------------------

// V2Task represents a single transaction for V2 BlockSTM.
type V2Task interface {
	Index() int
	Sender() common.Address
	To() *common.Address // nil for contract creation
	// Authorities returns the unique EIP-7702 authorities of this tx
	// (signers in the SetCode auth list). Empty for non-SetCodeTx.
	// V2 uses these to maintain per-address nonce deltas across the
	// block, since each authority's nonce is bumped at apply time even
	// though it isn't the tx sender.
	Authorities() []common.Address
}

// InFlightTaskMultiplier sets the size of two coupled buffers as a
// function of numWorkers:
//
//   - the dispatcher's backpressure window in startTaskDispatcher
//     (max numWorkers*InFlightTaskMultiplier tasks dispatched ahead
//     of the slowest worker), and
//
//   - the per-block PDB recycle channel in core/parallel_state_processor's
//     v2Env (must be ≥ the dispatcher window so the recycle path never
//     blocks worker reuse).
//
// Both must use the same factor; bumping one without the other can
// stall workers waiting for a recycled PDB or for room in the dispatch
// channel. Defined here as the single source of truth.
const InFlightTaskMultiplier = 4

// V2TxState is the mutable result of executing a task.
type V2TxState interface {
	Validate() bool
	ValidateCategory() string // returns "" if valid, or failure category ("balance", "storage", etc.)
	MarkEstimate()
	CleanupEstimate(oldWriteKeys []Key, oldBalAddrs []common.Address)
	GetWriteKeys() []Key
	GetBalAddrs() []common.Address
	FlushToMVStore()
	SetDeferMVWrites(defer_ bool)
}

// V2SettleFn is called in-order for each validated tx.
type V2SettleFn func(txIdx int, state V2TxState)

// V2Env provides the execution environment for V2 BlockSTM.
type V2Env interface {
	BaseNonce(addr common.Address) uint64
	// BaseCodeSize returns the size of addr's code at pre-block base state.
	BaseCodeSize(addr common.Address) int
	Execute(task V2Task, workerID int, incarnation int,
		senderNonces map[common.Address]uint64,
		coinbase common.Address,
		waitForTx func(int),
		waitForFinal func(int),
		deferWrites bool) V2TxState
	// Recycle returns a V2TxState to the pool for reuse.
	// Called after settlement or after re-execution replaces the old state.
	Recycle(V2TxState)
}

// V2ExecutionResult holds the output of ExecuteV2BlockSTM.
type V2ExecutionResult struct {
	States     []V2TxState
	ExecCount  int
	VFailCount int
	VFailCats  map[string]int // validation failure categories
	VFailIdxs  []int          // task indices that failed validation
	Phase1     time.Duration
	SettleDur  time.Duration // wall time of settle goroutine
	// Validation goroutine wall-time breakdown
	ValWaitDur  time.Duration // time waiting for workers
	ValCheckDur time.Duration // time doing validation checks
	ValReexDur  time.Duration // time blocked on re-execution
	// Non-nil if runValidationLoop recovered from a panic; caller should
	// discard this result and fall back to serial.
	ValidationPanic any
}

// ExecuteV2BlockSTM runs transactions with pipelined parallel execution.
// conflictAddrs is an optional set of To addresses that caused vfails in recent
// blocks. Txs to these addresses are chained together (cross-contract prediction).
//
// ctx is checked at dispatch and validation boundaries; cancellation causes
// the executor to stop dispatching new tasks and return early. The partial
// result is intended to be discarded by the caller (e.g. when serial wins
// the parallel-vs-serial race).
func ExecuteV2BlockSTM(
	ctx context.Context,
	tasks []V2Task,
	env V2Env,
	coinbase common.Address,
	numWorkers int,
	conflictAddrs map[common.Address]bool,
	settleFn V2SettleFn,
) *V2ExecutionResult {
	n := len(tasks)
	if n == 0 {
		return &V2ExecutionResult{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	x := newV2ExecCtx(tasks, env, coinbase, numWorkers, conflictAddrs)
	x.ctx = ctx
	startTime := time.Now()

	taskCh, wg := x.startWorkers()
	dispatcherDone := x.startTaskDispatcher(taskCh)
	settleWg, settleStart, settleEnd := x.startSettlement(settleFn)

	valDone := make(chan struct{})
	go x.runValidationLoop(valDone)

	<-valDone        // validation finished — no more re-executions
	<-dispatcherDone // wait for dispatcher to stop sending before closing taskCh
	close(taskCh)
	wg.Wait()         // wait for workers to exit
	x.reexecWg.Wait() // detached re-exec goroutines may outlive validation on cancellation
	settleWg.Wait()

	return x.buildResult(startTime, settleStart, settleEnd)
}

// v2ExecCtx holds shared state for one ExecuteV2BlockSTM invocation.
// Pulled out of the long function so each phase reads as a focused
// method against a stable receiver instead of a closure soup.
type v2ExecCtx struct {
	ctx          context.Context // for cancellation; checked at dispatch and validation boundaries
	tasks        []V2Task
	env          V2Env
	coinbase     common.Address
	numWorkers   int
	n            int
	states       []V2TxState
	incarnations []int
	execCount    atomic.Int64
	vfailCount   atomic.Int64
	vfailCats    map[string]int
	vfailCatsMu  sync.Mutex
	vfailIdxs    []int
	vfailed      []atomic.Bool
	toPrev       []int
	completionCh []chan struct{}
	finalized    []atomic.Bool
	execDone     []chan struct{}
	chSettle     chan int
	reexecWg     sync.WaitGroup // re-exec goroutines launched by dispatchReexec

	taskSenderNonces []map[common.Address]uint64

	valWaitDur  time.Duration
	valCheckDur time.Duration
	valReexDur  time.Duration

	validationPanic any // non-nil if runValidationLoop recovered from a panic
}

func newV2ExecCtx(tasks []V2Task, env V2Env, coinbase common.Address,
	numWorkers int, conflictAddrs map[common.Address]bool) *v2ExecCtx {
	n := len(tasks)
	x := &v2ExecCtx{
		tasks:        tasks,
		env:          env,
		coinbase:     coinbase,
		numWorkers:   numWorkers,
		n:            n,
		states:       make([]V2TxState, n),
		incarnations: make([]int, n),
		vfailCats:    make(map[string]int),
		vfailed:      make([]atomic.Bool, n),
		completionCh: make([]chan struct{}, n),
		finalized:    make([]atomic.Bool, n),
		execDone:     make([]chan struct{}, n),
	}
	for i := range x.completionCh {
		x.completionCh[i] = make(chan struct{})
	}
	for i := range x.execDone {
		x.execDone[i] = make(chan struct{})
	}
	x.taskSenderNonces = computeSenderNonces(tasks, env)
	x.toPrev = computeToPrev(tasks, conflictAddrs)
	return x
}

// computeSenderNonces returns per-task per-sender pre-computed nonces for
// same-sender chains (length >= 2). The pre-compute is only sound when
// nothing but the sender's own txs can move its nonce during the block,
// because a SenderNonces hit short-circuits PDB.GetNonce without
// recording a read — validation can never invalidate a stale value.
// Two classes of sender are therefore excluded and fall through to the
// MVStore-aware path, which records reads so validation re-executes the
// tx once a mid-block nonce change (or its absence) lands in the store:
//
//   - Senders appearing in any tx's EIP-7702 auth list — whether earlier
//     or later in the block. The auth-list applier bumps authority nonces,
//     and we can't tell at scheduling time whether an auth-list entry will
//     be applied (apply-time check requires auth.nonce == account.nonce
//     and a valid signature on the current chainID), so any pre-bump we
//     put here would be wrong for failed auths.
//
//   - Senders carrying code at base state. Only a delegation designator
//     is possible there for a valid sender (EIP-3607 bars other code), and
//     a call into a delegated account runs the delegate code in the
//     account's own frame — a CREATE inside it bumps the sender's nonce
//     mid-block, invalidating any pre-computed chain positions after it.
//
// Tasks with no chain and no exclusion get nil — same as the original
// optimisation, so PDB.GetNonce reads the base nonce directly.
func computeSenderNonces(tasks []V2Task, env V2Env) []map[common.Address]uint64 {
	// Any address that is an authority in any tx may have its nonce
	// dynamically bumped by the auth-list applier. Don't pre-compute
	// nonces for those addresses — let the MVStore-aware path handle
	// the dependency.
	var authoritySet map[common.Address]struct{}
	for i := range tasks {
		for _, auth := range tasks[i].Authorities() {
			if authoritySet == nil {
				authoritySet = make(map[common.Address]struct{})
			}
			authoritySet[auth] = struct{}{}
		}
	}

	chains := groupBySender(tasks, env)
	out := make([]map[common.Address]uint64, len(tasks))
	for sender, c := range chains {
		if precomputeSenderNonces(sender, c, authoritySet, env) {
			assignSenderNonces(out, sender, c)
		}
	}
	return out
}

// precomputeSenderNonces reports whether sender's chain qualifies for the
// pre-compute: length >= 2, not an EIP-7702 authority in this block, and no
// code at base state. Chain length is checked first because it is free,
// while the base-code lookup can hit disk and singleton senders (the common
// case) should never pay for it.
func precomputeSenderNonces(sender common.Address, c *senderChain, authoritySet map[common.Address]struct{}, env V2Env) bool {
	if len(c.taskIdxs) < 2 {
		return false
	}
	if _, isAuth := authoritySet[sender]; isAuth {
		return false
	}
	return env.BaseCodeSize(sender) == 0
}

// senderChain is the per-sender precomputed nonce chain.
type senderChain struct {
	baseNonce uint64
	taskIdxs  []int
}

func groupBySender(tasks []V2Task, env V2Env) map[common.Address]*senderChain {
	chains := make(map[common.Address]*senderChain)
	for i := range tasks {
		sender := tasks[i].Sender()
		c, ok := chains[sender]
		if !ok {
			c = &senderChain{baseNonce: env.BaseNonce(sender)}
			chains[sender] = c
		}
		c.taskIdxs = append(c.taskIdxs, i)
	}
	return chains
}

func assignSenderNonces(out []map[common.Address]uint64, sender common.Address, c *senderChain) {
	if len(c.taskIdxs) < 2 {
		return
	}
	for pos, idx := range c.taskIdxs {
		if out[idx] == nil {
			out[idx] = make(map[common.Address]uint64)
		}
		out[idx][sender] = c.baseNonce + uint64(pos)
	}
}

// computeToPrev builds the per-task predecessor map used for execution-time
// dependency prediction:
//  1. Same-To chaining for contracts with >= minChainSize txs in the block.
//  2. Cross-contract chaining for txs to conflict-prone addresses (failed in
//     a recent block) — catches indirect conflicts via shared state like a
//     hot USDC slot all DEX routers touch.
func computeToPrev(tasks []V2Task, conflictAddrs map[common.Address]bool) []int {
	const minChainSize = 3
	toCount := countToAddrs(tasks)
	out := make([]int, len(tasks))
	chains := chainState{
		lastTo:       make(map[common.Address]int),
		lastConflict: -1,
	}
	for i := range tasks {
		out[i] = chains.predecessor(tasks[i].To(), i, toCount, conflictAddrs, minChainSize)
	}
	return out
}

// countToAddrs counts occurrences of each non-nil To address across tasks.
func countToAddrs(tasks []V2Task) map[common.Address]int {
	out := make(map[common.Address]int)
	for i := range tasks {
		if to := tasks[i].To(); to != nil {
			out[*to]++
		}
	}
	return out
}

// chainState tracks the most recent task index per same-To chain and the
// running cross-contract conflict chain. Updated as computeToPrev iterates.
type chainState struct {
	lastTo       map[common.Address]int
	lastConflict int
}

// predecessor returns the toPrev value for task i (with To address `to`)
// and updates the chain-state trackers in place. Returns -1 if no predicted
// predecessor applies.
func (c *chainState) predecessor(to *common.Address, i int,
	toCount map[common.Address]int, conflictAddrs map[common.Address]bool,
	minChainSize int) int {
	if to == nil {
		return -1
	}
	prev := -1
	if toCount[*to] >= minChainSize {
		if p, ok := c.lastTo[*to]; ok {
			prev = p
		}
		c.lastTo[*to] = i
	}
	if prev == -1 && conflictAddrs[*to] {
		if c.lastConflict >= 0 {
			prev = c.lastConflict
		}
		c.lastConflict = i
	}
	return prev
}

// waitForTx blocks until the writer at writerIdx has at least executed
// (its first FlushToMVStore is visible) — used by per-key pipelining.
//
// Honours ctx.Done() so a worker blocked here unblocks promptly when the
// caller cancels (e.g. serial wins the parallel-vs-serial race).
func (x *v2ExecCtx) waitForTx(writerIdx int) {
	if writerIdx < 0 || writerIdx >= x.n {
		return
	}
	if x.finalized[writerIdx].Load() {
		return
	}
	if x.ctx == nil {
		<-x.execDone[writerIdx]
		return
	}
	select {
	case <-x.execDone[writerIdx]:
	case <-x.ctx.Done():
	}
}

// waitForFinal blocks until the writer at writerIdx is finalized (validated
// and post-re-exec, if any). Like waitForTx, honours ctx.Done().
func (x *v2ExecCtx) waitForFinal(writerIdx int) {
	if writerIdx < 0 || writerIdx >= x.n {
		return
	}
	if x.finalized[writerIdx].Load() {
		return
	}
	if x.ctx == nil {
		<-x.execDone[writerIdx]
		if x.finalized[writerIdx].Load() {
			return
		}
		<-x.completionCh[writerIdx]
		return
	}
	select {
	case <-x.execDone[writerIdx]:
	case <-x.ctx.Done():
		return
	}
	if x.finalized[writerIdx].Load() {
		return
	}
	select {
	case <-x.completionCh[writerIdx]:
	case <-x.ctx.Done():
	}
}

// execute runs (or re-runs) tx taskIdx — waits for predicted predecessors,
// invokes env.Execute, flushes writes, and stores the resulting V2TxState.
//
// On ctx cancellation we return early without populating x.states[taskIdx].
// The worker loop still closes execDone[taskIdx] (line 382), which unblocks
// any later worker waiting at <-execDone[prev] in the cascading toPrev
// chain. Without these ctx selects, a worker that hits the same-To
// predecessor wait while runValidationLoop is being torn down can hang
// forever — completionCh[k-1] is closed only by finishReexec/finalizePass,
// both of which the loop deliberately skips on cancellation.
func (x *v2ExecCtx) execute(taskIdx, workerID int) {
	// If the immediately preceding tx failed validation AND shares the same
	// To address, wait for it. Independent txs (different contract) can
	// proceed speculatively — the toPrev chain handles same-contract deps.
	if taskIdx > 0 && x.vfailed[taskIdx-1].Load() && x.toPrev[taskIdx] == taskIdx-1 {
		if !x.finalized[taskIdx-1].Load() {
			select {
			case <-x.completionCh[taskIdx-1]:
			case <-x.ctx.Done():
				return
			}
		}
	}
	if prev := x.toPrev[taskIdx]; prev >= 0 {
		select {
		case <-x.execDone[prev]:
		case <-x.ctx.Done():
			return
		}
	}
	st := x.env.Execute(x.tasks[taskIdx], workerID, x.incarnations[taskIdx],
		x.taskSenderNonces[taskIdx], x.coinbase, x.waitForTx, x.waitForFinal, true)
	if st != nil {
		st.FlushToMVStore()
	}
	x.states[taskIdx] = st
	x.execCount.Add(1)
}

// startWorkers launches numWorkers goroutines reading from a buffered
// task channel, executing each task and closing its execDone signal.
func (x *v2ExecCtx) startWorkers() (chan int, *sync.WaitGroup) {
	taskCh := make(chan int, x.numWorkers)
	var wg sync.WaitGroup
	for w := 0; w < x.numWorkers; w++ {
		wg.Add(1)
		wID := w
		go func() {
			defer wg.Done()
			for taskIdx := range taskCh {
				x.execute(taskIdx, wID)
				close(x.execDone[taskIdx])
			}
		}()
	}
	return taskCh, &wg
}

// startTaskDispatcher feeds task indices into taskCh in order, applying
// backpressure so at most numWorkers*InFlightTaskMultiplier tasks are in
// flight at once. Stops dispatching when ctx is cancelled — the partial
// result is intended to be discarded by the caller.
func (x *v2ExecCtx) startTaskDispatcher(taskCh chan int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		window := x.numWorkers * InFlightTaskMultiplier
		for i := 0; i < x.n; i++ {
			if x.ctx.Err() != nil {
				return
			}
			if i >= window {
				select {
				case <-x.execDone[i-window]:
				case <-x.ctx.Done():
					return
				}
			}
			select {
			case taskCh <- i:
			case <-x.ctx.Done():
				return
			}
		}
	}()
	return done
}

// startSettlement spawns the in-order settlement goroutine if settleFn
// is set, returning a WaitGroup and pointers updated when settlement
// starts/ends. If settleFn is nil, no goroutine is launched and chSettle
// stays nil so the validation loop can no-op its sends.
func (x *v2ExecCtx) startSettlement(settleFn V2SettleFn) (*sync.WaitGroup, *time.Time, *time.Time) {
	var wg sync.WaitGroup
	start := time.Now()
	end := start
	if settleFn == nil {
		return &wg, &start, &end
	}
	x.chSettle = make(chan int, x.n)
	wg.Add(1)
	go func() {
		defer wg.Done()
		start = time.Now()
		for txIdx := range x.chSettle {
			settleFn(txIdx, x.states[txIdx])
		}
		end = time.Now()
	}()
	return &wg, &start, &end
}

// finishReexec blocks until tx idx's re-execution completes, then finalizes
// it, optionally enqueues for settlement, and clears the pending channel.
// No-op when no re-execution is pending for idx.
//
// Estimate cleanup is performed by the dispatchReexec goroutine BEFORE it
// closes reexecDone[idx] — by the time we observe that close here, the
// new incarnation's WriteKeys are committed and any old ESTIMATE entries
// the re-exec did not re-write have already been removed.
func (x *v2ExecCtx) finishReexec(reexecDone []chan struct{}, idx int) {
	if reexecDone[idx] == nil {
		return
	}
	tReex := time.Now()
	<-reexecDone[idx]
	x.valReexDur += time.Since(tReex)
	reexecDone[idx] = nil
	x.finalized[idx].Store(true)
	close(x.completionCh[idx])
	// ctx cancellation can leave x.states[idx] nil — settleFn would panic.
	if x.chSettle != nil && x.states[idx] != nil {
		x.chSettle <- idx
	}
}

// finalizePass marks tx i as finalized after a successful validation and
// queues it for settlement.
func (x *v2ExecCtx) finalizePass(i int) {
	x.finalized[i].Store(true)
	close(x.completionCh[i])
	if x.chSettle != nil {
		x.chSettle <- i
	}
}

// dispatchReexec records a validation failure for tx i, marks the old
// state as ESTIMATE, bumps the incarnation, and launches a goroutine to
// re-execute the tx. Returns the channel that closes when the re-exec
// goroutine finishes.
//
// MUST be called only from the validation goroutine. The vfailIdxs append
// is unsynchronized; if a future change adds another caller it must move
// to a locked structure (matching vfailCats below). The fan-out goroutines
// it launches do not call back into dispatchReexec.
func (x *v2ExecCtx) dispatchReexec(i int) chan struct{} {
	x.vfailCount.Add(1)
	x.vfailIdxs = append(x.vfailIdxs, i)
	if x.states[i] != nil {
		if cat := x.states[i].ValidateCategory(); cat != "" {
			x.vfailCatsMu.Lock()
			x.vfailCats[cat]++
			x.vfailCatsMu.Unlock()
		}
	}
	var oldWriteKeys []Key
	var oldBalAddrs []common.Address
	oldState := x.states[i]
	if oldState != nil {
		oldWriteKeys = oldState.GetWriteKeys()
		oldBalAddrs = oldState.GetBalAddrs()
		oldState.MarkEstimate()
	}
	x.vfailed[i].Store(true)
	x.incarnations[i]++

	ch := make(chan struct{})
	x.reexecWg.Add(1)
	// Re-exec runs in its own goroutine; multiple re-execs for different
	// txs can be in flight concurrently. Cross-goroutine state isolation:
	//   - x.execute writes only x.states[idx] (per-idx slot) — distinct idx
	//     for each goroutine, so no overlap.
	//   - CleanupEstimate operates on x.states[idx] AND on (store, bals)
	//     entries keyed by idx — both are sharded with their own locks.
	//   - env.Recycle pushes onto a buffered channel — concurrent-safe.
	// Order: CleanupEstimate completes before close(ch) (and therefore
	// before finishReexec returns), so finalize/settle observes a clean store.
	go func(idx int, owk []Key, oba []common.Address, old V2TxState) {
		defer x.reexecWg.Done()
		x.execute(idx, x.numWorkers)
		if x.states[idx] != nil {
			x.states[idx].CleanupEstimate(owk, oba)
		}
		if old != nil && old != x.states[idx] {
			x.env.Recycle(old)
		}
		close(ch)
	}(i, oldWriteKeys, oldBalAddrs, oldState)
	return ch
}

// validateOne handles validation + re-exec dispatch for tx i. Stalls only
// when tx i depends on a still-pending re-execution.
//
// Settle order (and therefore final state-root determinism) depends on
// the invariant that, when validateOne(i) runs, reexecDone[j] is nil for
// every j < i-1. The loop preserves this by induction: validateOne(j+1)
// always finishes reexecDone[j] (via the i-1 check below) before
// returning, so by the time we reach validateOne(i), only reexecDone[i-1]
// can be non-nil. The toPrev check below is a hint for predicted
// predecessors and is redundant for ordering; the i-1 check carries the
// invariant. A future "skip-ahead" optimisation that bypasses
// validateOne(j) for some j would break settle order — keep this in mind.
func (x *v2ExecCtx) validateOne(reexecDone []chan struct{}, i int) bool {
	x.assertSettleOrder(reexecDone, i)
	if prev := x.toPrev[i]; prev >= 0 && reexecDone[prev] != nil {
		x.finishReexec(reexecDone, prev)
	}
	if i > 0 && x.vfailed[i-1].Load() && reexecDone[i-1] != nil {
		x.finishReexec(reexecDone, i-1)
	}

	tWait := time.Now()
	// If the dispatcher has already exited due to ctx cancellation, the
	// task may never have been pushed to a worker, so execDone[i] will
	// never close. Select on ctx.Done() to avoid hanging in that case.
	select {
	case <-x.execDone[i]:
	case <-x.ctx.Done():
		x.valWaitDur += time.Since(tWait)
		return false
	}
	x.valWaitDur += time.Since(tWait)

	tCheck := time.Now()
	if x.states[i] != nil && x.states[i].Validate() {
		x.valCheckDur += time.Since(tCheck)
		x.finalizePass(i)
		return true
	}
	x.valCheckDur += time.Since(tCheck)
	reexecDone[i] = x.dispatchReexec(i)
	return true
}

// runValidationLoop is the main validation goroutine. It validates each tx
// in order, dispatching re-executions non-blocking and only stalling for
// re-exec completion when a later tx depends on a still-pending one (via
// toPrev or vfailed[i-1]). After the main loop, it drains any leftovers.
func (x *v2ExecCtx) runValidationLoop(valDone chan struct{}) {
	defer close(valDone)

	// Defer order (LIFO): recover runs first to capture the panic, then
	// chSettle is closed so the settle goroutine drains even on panic.
	settleClosed := false
	defer func() {
		if x.chSettle != nil && !settleClosed {
			close(x.chSettle)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			x.validationPanic = r
			log.Error("V2 validation panic — falling back to serial",
				"err", r, "stack", string(debug.Stack()))
		}
	}()

	reexecDone := make([]chan struct{}, x.n)
	cancelled := false
	for i := 0; i < x.n; i++ {
		if x.ctx.Err() != nil {
			// Caller cancelled (typically because the serial processor won
			// the parallel-vs-serial race). Stop validating; the partial
			// result will be discarded.
			cancelled = true
			break
		}
		if !x.validateOne(reexecDone, i) {
			// validateOne saw cancellation while waiting for execDone.
			cancelled = true
			break
		}
	}
	if !cancelled {
		for i := 0; i < x.n; i++ {
			x.finishReexec(reexecDone, i)
		}
		x.assertReexecVisitedExactlyOnce(reexecDone)
	}
	// On cancellation we skip the reexec drain: workers and re-exec
	// goroutines blocked on waitForTx / waitForFinal exit promptly via
	// the ctx.Done() branches in those helpers, so there's nothing to
	// drain. Closing chSettle terminates the settlement goroutine.
	if x.chSettle != nil {
		close(x.chSettle)
		settleClosed = true
	}
}

// buildResult collects timings and per-tx state into a V2ExecutionResult.
func (x *v2ExecCtx) buildResult(startTime time.Time, settleStart, settleEnd *time.Time) *V2ExecutionResult {
	phase1Dur := time.Since(startTime)
	var sd time.Duration
	if !settleEnd.IsZero() {
		sd = settleEnd.Sub(*settleStart)
	}
	return &V2ExecutionResult{
		States:          x.states,
		ExecCount:       int(x.execCount.Load()),
		VFailCount:      int(x.vfailCount.Load()),
		VFailCats:       x.vfailCats,
		VFailIdxs:       x.vfailIdxs,
		Phase1:          phase1Dur,
		SettleDur:       sd,
		ValWaitDur:      x.valWaitDur,
		ValCheckDur:     x.valCheckDur,
		ValReexDur:      x.valReexDur,
		ValidationPanic: x.validationPanic,
	}
}
