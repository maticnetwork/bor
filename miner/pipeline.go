package miner

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
)

// Pipelined SRC metrics
var (
	pipelineSpeculativeBlocksCounter    = metrics.NewRegisteredCounter("worker/pipelineSpeculativeBlocks", nil)
	pipelineSpeculativeAbortsCounter    = metrics.NewRegisteredCounter("worker/pipelineSpeculativeAborts", nil)
	pipelineEIP2935AbortsCounter        = metrics.NewRegisteredCounter("worker/pipelineEIP2935Aborts", nil)
	pipelineSRCTimer                    = metrics.NewRegisteredTimer("worker/pipelineSRCTime", nil)
	pipelineFlatDiffExtractTimer        = metrics.NewRegisteredTimer("worker/pipelineFlatDiffExtractTime", nil)
	pipelineSpeculativeCommittedCounter = metrics.NewRegisteredCounter("worker/pipelineSpeculativeCommitted", nil) // speculative block broadcast as the real next block — success signal
	pipelineSRCWaitTimer                = metrics.NewRegisteredTimer("worker/pipelineSRCWait", nil)                // time blocked on WaitForSRC (ideally near-zero — means SRC finished before the caller arrived)
	pipelineSealDurationTimer           = metrics.NewRegisteredTimer("worker/pipelineSealDuration", nil)           // engine.Seal latency in the inline path
	// Per-cause abort counters — each increments alongside the aggregate pipelineSpeculativeAbortsCounter.
	pipelineAbortBlockhashCounter = metrics.NewRegisteredCounter("worker/pipelineSpeculativeAborts/blockhash", nil)  // BLOCKHASH(N) was read during speculative N+1
	pipelineAbortSRCFailedCounter = metrics.NewRegisteredCounter("worker/pipelineSpeculativeAborts/src_failed", nil) // WaitForSRC returned an error
	pipelineAbortFallbackCounter  = metrics.NewRegisteredCounter("worker/pipelineSpeculativeAborts/fallback", nil)   // fallbackToSequential entered
	// Announce earliness histogram (ms). Positive = announced before header.Time (PIP-66 working). Negative = announced late.
	pipelineAnnounceEarlinessMs = metrics.NewRegisteredHistogram("worker/pipelineAnnounceEarlinessMs", nil, metrics.NewExpDecaySample(1028, 0.015))
	// Mode gauge — currently always 0 because production-side pipelined SRC is
	// disabled. Keep the metric so existing dashboards can distinguish miner-side
	// production pipelining from import-side pipelining.
	pipelineBuildEnabledGauge = metrics.NewRegisteredGauge("worker/pipeline/enabled", nil)
)

// Production-side pipelined SRC is disabled and no longer exposed as a config
// option. Keep this local constant so the old production-pipeline logging sites
// remain easy to re-enable if this path is revisited.
const productionPipelineLogs = false

const speculativeEmptyRefillLead = 300 * time.Millisecond

// Refill speculative blocks that are still less than 75% full after the first
// txpool snapshot. This catches the common case where the early snapshot grabs
// a small trickle of txs, but the load ramps up before the slot boundary.
const speculativeLowFillRemainingGasDivisor = 4

// speculativeWorkReq is sent to mainLoop's speculative work channel
// when block N's execution is done and we want to speculatively start N+1.
type speculativeWorkReq struct {
	parentHeader  *types.Header          // block N's header (complete except Root)
	flatDiff      *state.FlatDiff        // block N's state mutations
	parentRoot    common.Hash            // root_{N-1} (last committed trie root)
	blockNEnv     *environment           // block N's execution environment (for assembly later)
	stateSyncData []*types.StateSyncData // from FinalizeForPipeline
}

// placeholderParentHash generates a deterministic placeholder hash for use
// as ParentHash in speculative headers. It must not collide with any real
// block hash.
func placeholderParentHash(blockNumber uint64) common.Hash {
	data := append([]byte("pipelined-src-placeholder:"), new(big.Int).SetUint64(blockNumber).Bytes()...)
	return sha256.Sum256(data)
}

// isPipelineEligible checks whether we can use pipelined SRC for block
// production. Production-side pipelining is intentionally disabled; the
// import-side pipelined SRC path remains controlled by the chain pipeline
// config.
func (w *worker) isPipelineEligible(_ uint64) bool {
	// Re-enable reference:
	//
	// if !w.config.EnablePipelinedSRC {
	// 	return false
	// }
	// if w.chainConfig.Bor == nil {
	// 	return false
	// }
	// if len(w.chainConfig.Bor.Sprint) == 0 {
	// 	return false
	// }
	// if !w.IsRunning() || w.syncing.Load() {
	// 	return false
	// }
	// // Pre-Rio: the speculative chain reader provides block N's unsigned header.
	// // When snapshot() walks back and calls ecrecover() on this header, it fails
	// // because the Extra seal bytes are all zeros (Seal() hasn't run yet).
	// // This causes speculative Prepare to always fail with "recovery failed",
	// // making the pipeline useless pre-Rio. Skip it entirely.
	// nextBlockNumber := currentBlockNumber + 1
	// if !w.chainConfig.Bor.IsRio(new(big.Int).SetUint64(nextBlockNumber)) {
	// 	return false
	// }
	// return true
	return false
}

// commitPipelined is the pipelined version of commit(). Instead of calling
// FinalizeAndAssemble (which blocks on IntermediateRoot), it:
//  1. Calls FinalizeForPipeline (state sync, span commits — no IntermediateRoot)
//  2. Extracts FlatDiff
//  3. Sends a speculativeWorkReq to start N+1 execution
//  4. Returns immediately — the SRC goroutine is spawned by commitSpeculativeWork
//     after confirming the speculative Prepare() succeeds. This avoids a trie DB
//     race between the SRC goroutine and the fallback path's inline commit.
func (w *worker) commitPipelined(env *environment, start time.Time) error {
	if !w.IsRunning() {
		return nil
	}

	env = env.copy()

	borEngine, ok := w.engine.(*bor.Bor)
	if !ok {
		log.Error("Pipelined SRC: engine is not Bor")
		return nil
	}

	// Phase 1: Finalize (state sync, span commits) without IntermediateRoot.
	stateSyncData, err := borEngine.FinalizeForPipeline(w.chain, env.header, env.state, &types.Body{
		Transactions: env.txs,
	}, env.receipts)
	if err != nil {
		log.Error("Pipelined SRC: FinalizeForPipeline failed", "err", err)
		return err
	}

	// Phase 2: Extract FlatDiff, record mode-visible side-effects, build the
	// speculative work request. The SRC goroutine is NOT spawned here —
	// commitSpeculativeWork spawns it after confirming Prepare() succeeds.
	req, ok := w.buildSpeculativeReq(env, stateSyncData)
	if !ok {
		return nil
	}

	// Phase 3: Hand off to mainLoop's speculative path.
	select {
	case w.speculativeWorkCh <- req:
	case <-w.exitCh:
	}
	return nil
}

// buildSpeculativeReq extracts block N's FlatDiff, resolves the committed
// parent root, and composes the speculativeWorkReq for block N+1.
// Returns ok=false only when the parent header cannot be located (treated as
// a soft failure — the caller skips pipelining rather than returning an error,
// matching the pre-refactor behavior).
func (w *worker) buildSpeculativeReq(env *environment, stateSyncData []*types.StateSyncData) (*speculativeWorkReq, bool) {
	flatDiffStart := time.Now()
	flatDiff := env.state.CommitSnapshot(w.chainConfig.IsEIP158(env.header.Number))
	pipelineFlatDiffExtractTimer.Update(time.Since(flatDiffStart))

	parent := w.chain.GetHeader(env.header.ParentHash, env.header.Number.Uint64()-1)
	if parent == nil {
		log.Error("Pipelined SRC: parent not found", "parentHash", env.header.ParentHash)
		return nil, false
	}

	w.chain.SetLastFlatDiff(flatDiff, env.header.Number.Uint64(), parent.Root, common.Hash{})
	// Counts block N as "entering the pipeline." If Prepare() fails and
	// fallbackToSequential produces the block inline, this counter is slightly
	// inflated — block was produced sequentially, not speculatively.
	pipelineSpeculativeBlocksCounter.Inc(1)

	return &speculativeWorkReq{
		parentHeader:  env.header,
		flatDiff:      flatDiff,
		parentRoot:    parent.Root,
		blockNEnv:     env,
		stateSyncData: stateSyncData,
	}, true
}

// spawnSRCForFinalBlock conditionally spawns the SRC goroutine + publishes the
// FlatDiff for the last block of a pipeline run (used by sealBlockViaTaskCh).
func (w *worker) spawnSRCForFinalBlock(finalHeader *types.Header, rootN common.Hash, flatDiff *state.FlatDiff, spawnSRC bool) {
	if !spawnSRC {
		return
	}
	tmpBlock := types.NewBlockWithHeader(finalHeader)
	// Miner pipeline always produces witnesses for now. allowOwnWitness=true
	// explicitly permits SRC to create its own witness when no execution
	// witness is handed in by the caller. nil detached prefetcher — the
	// miner-side path does not currently hand execution prefetcher state to
	// SRC, so SRC falls back to the plain pathdb reader chain.
	w.chain.SpawnSRCGoroutine(tmpBlock, rootN, flatDiff, true, nil, true, nil, false)
	w.chain.SetLastFlatDiff(flatDiff, finalHeader.Number.Uint64(), rootN, common.Hash{})
}

// shouldLateRefillSpeculativeBlock reports whether a speculative block should
// take one more txpool snapshot shortly before the slot boundary.
func shouldLateRefillSpeculativeBlock(env *environment) bool {
	if len(env.txs) == 0 {
		return true
	}
	if env.gasPool == nil {
		return true
	}

	// Skip the top-up when the block is already mostly full. Otherwise, give it
	// one late snapshot to catch txs that arrived after the initial early fill.
	return env.gasPool.Gas() > env.header.GasLimit/speculativeLowFillRemainingGasDivisor
}

// fillSpeculativeTransactions snapshots the txpool once immediately, and if
// the speculative block is still underfilled, gives it one more pass shortly
// before the slot boundary. This avoids sealing low/empty speculative blocks
// simply because the initial early snapshot raced ahead of incoming load.
func (w *worker) fillSpeculativeTransactions(env *environment, interrupt *atomic.Int32) time.Duration {
	fillStart := time.Now()
	err := w.fillTransactions(interrupt, env, nil)
	totalFill := time.Since(fillStart)

	if err != nil || !shouldLateRefillSpeculativeBlock(env) {
		return totalFill
	}

	remaining := time.Until(env.header.GetActualTime())
	if remaining <= speculativeEmptyRefillLead {
		return totalFill
	}

	timer := time.NewTimer(remaining - speculativeEmptyRefillLead)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-w.exitCh:
		return totalFill
	}

	refillStart := time.Now()
	_ = w.fillTransactions(interrupt, env, nil)
	totalFill += time.Since(refillStart)

	return totalFill
}

// commitSpeculativeWork handles a speculativeWorkReq: executes block N+1
// speculatively using the FlatDiff overlay, then waits for SRC(N) to complete,
// assembles block N, and sends it for sealing. Then it finalizes N+1 and
// seals it as well.
//
// Returns true when mainLoop should requeue normal work after this function
// returns. This is needed for:
//   - Abort (EIP-2935/BLOCKHASH): the speculative block was discarded, so the
//     block slot must be rebuilt sequentially.
//   - Normal pipeline exit: the last block was sent to sealBlockViaTaskCh, and
//     there is a race where ChainHeadEvent may arrive at newWorkLoop before
//     pendingWorkBlock is cleared, causing the event to be skipped.
//
// Returns false when the pipeline fell back to sequential (fallbackToSequential
// already sealed block N via taskCh → resultLoop → ChainHeadEvent). Retrying
// work in this case creates a tight loop that keeps restarting Seal() with
// fresh timestamps, preventing any block from ever being sealed.
func (w *worker) commitSpeculativeWork(req *speculativeWorkReq) (shouldRetry bool, abortRecovery bool) {
	// Default: retry commitWork after this function returns. Fallback paths
	// set shouldRetry = false because they already sealed block N via taskCh
	// (resultLoop handles it).
	shouldRetry = true
	// Ensure pendingWorkBlock is cleared on every exit path.
	defer w.pendingWorkBlock.Store(0)

	s := newSpecSession(w, req)
	if !s.setupInitial() {
		return false, false
	}
	defer func() { <-s.initialFillDone }()

	if !s.waitForSRCAndSealBlockN() {
		return s.exitDuringBlockN, false
	}
	<-s.initialFillDone

	for {
		switch s.runOneIteration() {
		case iterContinue:
			continue
		case iterBreakAbort:
			abortRecovery = true
		case iterExitEarly:
			return false, false
		}
		break
	}
	if s.prevDBWriteDone != nil {
		<-s.prevDBWriteDone
	}
	return shouldRetry, abortRecovery
}

// iterResult enumerates how a single pipeline iteration ends.
type iterResult int

const (
	iterContinue   iterResult = iota // shifted to the next block; keep looping
	iterBreak                        // normal exit (error, last block sealed via taskCh)
	iterBreakAbort                   // speculative block was discarded (abortRecovery=true)
	iterExitEarly                    // w.exitCh fired mid-iteration; caller returns shouldRetry=false
)

// runOneIteration finalizes the current speculative block, prepares the next
// one, seals the current block, and shifts state. Each return value tells
// commitSpeculativeWork how to proceed; see iterResult.
func (s *specSession) runOneIteration() iterResult {
	if s.checkCurrentAbort() {
		return iterBreakAbort
	}
	s.drainPrevDBWrite()

	finalSpecHeader, flatDiff, stateSyncData, ok := s.finalizeCurrent()
	if !ok {
		return iterBreak
	}
	// Last block in pipeline (eligibility failed) → seal via taskCh so
	// resultLoop emits ChainHeadEvent and normal production resumes.
	if !s.w.isPipelineEligible(s.nextBlockNumber) || !s.w.IsRunning() {
		s.w.sealBlockViaTaskCh(s.borEngine, finalSpecHeader, s.specState, s.specEnv.txs,
			s.specEnv.receipts, stateSyncData, s.rootN, flatDiff, true, s.curBuildStart)
		return iterBreak
	}

	next, cont := s.prepareNextIteration(finalSpecHeader, flatDiff, stateSyncData)
	if !cont {
		return iterBreak
	}
	sealed, exitEarly, ok := s.sealCurrentAndAdvance(finalSpecHeader, stateSyncData, next)
	if exitEarly {
		return iterExitEarly
	}
	if !ok {
		return iterBreak
	}
	s.shiftToNext(sealed, next)
	return iterContinue
}

// specSession holds the rotating per-invocation state of commitSpeculativeWork.
// It exists so the orchestrator can decompose the 600-line original into
// focused methods that share state through the receiver — avoiding 15-parameter
// helper signatures. Fields are mutated through shiftToNext() as each
// speculative block is sealed.
type specSession struct {
	w         *worker
	req       *speculativeWorkReq
	borEngine *bor.Bor

	blockNHeader    *types.Header
	blockNNumber    uint64
	nextBlockNumber uint64

	// Current speculative block state (rotates each iteration).
	specHeader        *types.Header
	specState         *state.StateDB
	specEnv           *environment
	coinbase          common.Address
	blockhashAccessed *atomic.Bool // set true if speculative block read BLOCKHASH(N)
	eip2935Abort      bool         // set by initial-fill goroutine (for first iteration)
	curBuildStart     time.Time    // wall clock when this block's fill began

	// Updated as blocks are sealed.
	realBlockNHash   common.Hash
	rootN            common.Hash
	lastSealedHeader *types.Header

	// Iteration coordination.
	initialFillDone  chan struct{}
	prevDBWriteDone  chan struct{}
	exitDuringBlockN bool
}

// specNextIteration bundles everything prepareNextIteration allocates for the
// next speculative block, so sealCurrentAndAdvance/shiftToNext can consume it
// without 10-parameter helper signatures.
type specNextIteration struct {
	specHeaderNext    *types.Header
	specStateNext     *state.StateDB
	specEnvNext       *environment
	coinbaseNext      common.Address
	blockhashAccessed *atomic.Bool // *atomic.Bool for the next block
	eip2935AbortPtr   *bool        // set true by fill goroutine if EIP-2935 slot read
	nextBuildStart    time.Time
	fillDone          chan struct{}
	fillElapsed       *time.Duration // pointer so goroutine writes are visible after <-fillDone
	srcSpawnTime      time.Time
}

func newSpecSession(w *worker, req *speculativeWorkReq) *specSession {
	blockNNumber := req.parentHeader.Number.Uint64()
	return &specSession{
		w:               w,
		req:             req,
		blockNHeader:    req.parentHeader,
		blockNNumber:    blockNNumber,
		nextBlockNumber: blockNNumber + 1,
		curBuildStart:   time.Now(),
	}
}

// setupInitial builds the first speculative environment (N+1), runs Prepare,
// spawns SRC for block N, and starts the initial fill goroutine. Returns
// false if Prepare fails or state cannot be opened — in both cases the
// function has already called fallbackToSequential and the caller should
// return shouldRetry=false.
func (s *specSession) setupInitial() bool {
	log.Debug("Pipelined SRC: starting speculative execution", "speculativeBlock", s.nextBlockNumber, "parent", s.blockNNumber)

	borEngine, ok := s.w.engine.(*bor.Bor)
	if !ok {
		log.Error("Pipelined SRC: engine is not Bor")
		return false
	}
	s.borEngine = borEngine

	specReader, specContext, specHeader, coinbase := s.buildInitialSpecHeader()
	if err := s.w.engine.Prepare(specReader, specHeader, false); err != nil {
		log.Warn("Pipelined SRC: speculative Prepare failed, falling back", "err", err)
		s.w.fallbackToSequential(s.req)
		return false
	}
	s.specHeader = specHeader
	s.coinbase = coinbase

	// Prepare() succeeded — spawn the background SRC goroutine for block N.
	// Done AFTER Prepare to avoid a trie DB race with fallbackToSequential's
	// inline FinalizeAndAssemble on the same parent root.
	tmpBlock := types.NewBlockWithHeader(s.req.parentHeader)
	// Miner pipeline always produces witnesses for now. allowOwnWitness=true
	// explicitly permits SRC to create its own witness when no execution
	// witness is handed in by the caller. nil detached prefetcher — the
	// miner-side path does not currently hand execution prefetcher state to
	// SRC, so SRC falls back to the plain pathdb reader chain.
	s.w.chain.SpawnSRCGoroutine(tmpBlock, s.req.parentRoot, s.req.flatDiff, true, nil, true, nil, false)

	specState, err := s.w.chain.StateAtWithFlatDiff(s.req.parentRoot, s.req.flatDiff)
	if err != nil {
		log.Error("Pipelined SRC: failed to open speculative state", "err", err)
		s.w.chain.WaitForSRC() //nolint:errcheck
		s.w.fallbackToSequential(s.req)
		return false
	}
	specState.StartPrefetcher("miner-speculative", nil, nil)
	s.specState = specState

	blockN1Header := s.w.chain.GetHeader(s.blockNHeader.ParentHash, s.blockNNumber-1)
	if blockN1Header == nil {
		log.Error("Pipelined SRC: grandparent header not found")
		s.w.chain.WaitForSRC() //nolint:errcheck
		s.w.fallbackToSequential(s.req)
		return false
	}

	var blockhashAccessed atomic.Bool
	s.blockhashAccessed = &blockhashAccessed
	s.specEnv = s.buildSpecEnv(specHeader, specState, coinbase, specContext, blockN1Header, s.blockNNumber, s.newBlockNHashResolver())
	s.resetTxPoolState(s.blockNHeader, s.req.parentRoot, s.req.flatDiff)
	s.startInitialFillGoroutine()
	return true
}

// buildInitialSpecHeader constructs the header for speculative execution of
// block N+1 while block N is still being sealed. It intentionally does NOT
// reuse makeHeader because the inputs diverge fundamentally: the parent is a
// placeholder hash (block N not sealed yet), the timestamp is deterministic
// (blockN.Time + bor period — no genParams / user input), the gas limit uses
// config.GasCeil directly (no dynamic base-fee adjustment), and engine.Prepare
// is deliberately skipped (it would fail against the placeholder parent). The
// overlap is limited to coinbase resolution — unified via resolveCoinbase so
// both headers pick the same address and don't diverge on state root.
func (s *specSession) buildInitialSpecHeader() (*speculativeChainReader, *speculativeChainContext, *types.Header, common.Address) {
	placeholder := placeholderParentHash(s.blockNNumber)
	specReader := newSpeculativeChainReader(s.w.chain, s.blockNHeader, placeholder)
	specContext := newSpeculativeChainContext(specReader, s.w.engine)
	coinbase := s.w.resolveCoinbase(s.nextBlockNumber, s.w.etherbase())
	specHeader := &types.Header{
		ParentHash: placeholder,
		Number:     new(big.Int).SetUint64(s.nextBlockNumber),
		GasLimit:   core.CalcGasLimit(s.blockNHeader.GasLimit, s.w.config.GasCeil),
		Time:       s.blockNHeader.Time + s.w.chainConfig.Bor.CalculatePeriod(s.nextBlockNumber),
		Coinbase:   coinbase,
	}
	if s.w.chainConfig.IsLondon(specHeader.Number) {
		specHeader.BaseFee = eip1559.CalcBaseFee(s.w.chainConfig, s.blockNHeader)
	}
	return specReader, specContext, specHeader, coinbase
}

// resolveCoinbase matches the importer's NewEVMBlockContext(header, chain, nil)
// logic: post-Rio uses BorConfig.CalculateCoinbase, otherwise the caller-provided
// fallback (genParams.coinbase for makeHeader, etherbase for the speculative
// path). Unifying this ensures the speculative header and the later real header
// resolve coinbase identically — a mismatch would cause a state root divergence.
func (w *worker) resolveCoinbase(blockNumber uint64, fallback common.Address) common.Address {
	var coinbase common.Address
	if w.chainConfig.Bor != nil && w.chainConfig.Bor.IsRio(new(big.Int).SetUint64(blockNumber)) {
		coinbase = common.HexToAddress(w.chainConfig.Bor.CalculateCoinbase(blockNumber))
	}
	if coinbase == (common.Address{}) {
		coinbase = fallback
	}
	return coinbase
}

// newBlockNHashResolver returns a lazy resolver for block N's signed hash used
// by SpeculativeGetHashFn. Block N's hash isn't known until SRC completes
// because it depends on the state root — if a speculative tx calls BLOCKHASH(N)
// we wait for SRC, compute the pre-seal hash, and the hashAccessed flag on the
// outer speculative block triggers a discard (pre-seal hash ≠ final on-chain).
func (s *specSession) newBlockNHashResolver() func() common.Hash {
	var (
		hash     common.Hash
		resolved bool
		mu       sync.Mutex
	)
	blockNHeader := s.blockNHeader
	return func() common.Hash {
		mu.Lock()
		defer mu.Unlock()
		if resolved {
			return hash
		}
		root, _, err := s.w.chain.WaitForSRC()
		if err != nil {
			log.Error("Pipelined SRC: SRC failed during BLOCKHASH resolution", "err", err)
			return common.Hash{}
		}
		finalHeader := types.CopyHeader(blockNHeader)
		finalHeader.Root = root
		finalHeader.UncleHash = types.CalcUncleHash(nil)
		hash = finalHeader.Hash()
		resolved = true
		return hash
	}
}

// buildSpecEnv assembles the *environment used for speculative transaction
// execution. Used by both the initial setup and each loop iteration's
// next-block preparation.
func (s *specSession) buildSpecEnv(header *types.Header, state *state.StateDB, coinbase common.Address, specContext *speculativeChainContext, grandparent *types.Header, grandparentNumber uint64, srcDone func() common.Hash) *environment {
	specGetHash := core.SpeculativeGetHashFn(grandparent, specContext, grandparentNumber, srcDone, s.blockhashAccessed)
	evmContext := core.NewEVMBlockContext(header, specContext, &coinbase)
	evmContext.GetHash = specGetHash
	env := &environment{
		signer:         types.MakeSigner(s.w.chainConfig, header.Number, header.Time),
		state:          state,
		size:           uint64(header.Size()),
		coinbase:       coinbase,
		buildInterrupt: newBuildInterruptState(),
		header:         header,
		evm:            vm.NewEVM(evmContext, state, s.w.chainConfig, vm.Config{}),
	}
	env.evm.SetInterrupt(env.buildInterrupt.timeoutFlag())
	env.tcount = 0
	return env
}

// resetTxPoolState publishes a fresh speculative state to the txpool so tx
// selection sees the new block's post-parent view (nonces, balances).
func (s *specSession) resetTxPoolState(parent *types.Header, parentRoot common.Hash, flatDiff *state.FlatDiff) {
	specTxPoolState, err := s.w.chain.StateAtWithFlatDiff(parentRoot, flatDiff)
	if err != nil {
		log.Error("Pipelined SRC: failed to create txpool speculative state", "err", err)
		return
	}
	s.w.eth.TxPool().SetSpeculativeState(parent, specTxPoolState)
}

// startInitialFillGoroutine kicks off the speculative tx fill for N+1 and
// the EIP-2935 abort check. The goroutine closes initialFillDone when done;
// s.eip2935Abort is only safe to read after <-s.initialFillDone.
func (s *specSession) startInitialFillGoroutine() {
	s.initialFillDone = make(chan struct{})
	go func() {
		defer close(s.initialFillDone)
		stop := createInterruptTimer(
			s.specHeader.Number.Uint64(),
			s.specHeader.GetActualTime(),
			s.specEnv.buildInterrupt.timeoutFlag(),
			s.specEnv.buildInterrupt.flagSetAtPtr(),
			true,
		)
		var interrupt atomic.Int32
		s.w.fillSpeculativeTransactions(s.specEnv, &interrupt)
		stop()
		// Final discard log is emitted in the main loop so each aborted block is logged once.
		if s.w.chainConfig.IsPrague(s.specHeader.Number) {
			dangerousSlot := common.BigToHash(new(big.Int).SetUint64(s.blockNNumber % params.HistoryServeWindow))
			if s.specState.WasStorageSlotRead(params.HistoryStorageAddress, dangerousSlot) {
				s.eip2935Abort = true
				pipelineEIP2935AbortsCounter.Inc(1)
			}
		}
	}()
}

// waitForSRCAndSealBlockN waits for block N's SRC goroutine to complete,
// assembles block N with the real root, submits it via taskCh, and waits for
// resultLoop to persist it. Returns false on any failure; sets
// exitDuringBlockN when the failure was w.exitCh.
func (s *specSession) waitForSRCAndSealBlockN() bool {
	srcStart := time.Now()
	root, witnessN, err := s.w.chain.WaitForSRC()
	srcWaitN := time.Since(srcStart)
	pipelineSRCTimer.Update(srcWaitN)
	pipelineSRCWaitTimer.Update(srcWaitN)
	if err != nil {
		log.Error("Pipelined SRC: SRC(N) failed", "block", s.blockNNumber, "err", err)
		pipelineSpeculativeAbortsCounter.Inc(1)
		pipelineAbortSRCFailedCounter.Inc(1)
		return false
	}
	finalHeaderN := types.CopyHeader(s.blockNHeader)
	finalHeaderN.Root = root
	blockN, receiptsN, err := s.borEngine.AssembleBlock(s.w.chain, finalHeaderN, s.req.blockNEnv.state, &types.Body{
		Transactions: s.req.blockNEnv.txs,
	}, s.req.blockNEnv.receipts, root, s.req.stateSyncData)
	if err != nil {
		log.Error("Pipelined SRC: AssembleBlock(N) failed", "err", err)
		return false
	}
	// Block N uses the pipelined write path to avoid a double CommitWithUpdate
	// from the SRC goroutine and writeBlockWithState. Witness from SRC is complete.
	select {
	case s.w.taskCh <- &task{receipts: receiptsN, state: s.req.blockNEnv.state, block: blockN, createdAt: time.Now(), pipelined: true, witnessBytes: witnessN}:
		if productionPipelineLogs {
			log.Info("Pipelined SRC: block N sent for sealing", "number", blockN.Number(), "txs", len(blockN.Transactions()), "root", root)
		}
	case <-s.w.exitCh:
		s.exitDuringBlockN = true
		return false
	}
	realHash, ok := s.waitForChainHead(blockN.NumberU64())
	if !ok {
		return false
	}
	s.realBlockNHash = realHash
	s.rootN = root
	return true
}

// waitForChainHead blocks until the chain head reaches blockNum (up to 30s)
// so we can read the real (signed) block N hash from the canonical chain.
// resultLoop writes the final header after Seal() modifies Extra, so we
// can't use blockN.Hash() directly.
func (s *specSession) waitForChainHead(blockNum uint64) (common.Hash, bool) {
	waitDeadline := time.After(30 * time.Second)
	for {
		if current := s.w.chain.CurrentBlock(); current != nil && current.Number.Uint64() >= blockNum {
			if current.Number.Uint64() != blockNum {
				log.Error("Pipelined SRC: chain head mismatch after waiting", "expected", blockNum, "got", current.Number.Uint64())
				return common.Hash{}, false
			}
			return current.Hash(), true
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-waitDeadline:
			log.Error("Pipelined SRC: timed out waiting for block N to be written", "number", blockNum)
			return common.Hash{}, false
		case <-s.w.exitCh:
			s.exitDuringBlockN = true
			return common.Hash{}, false
		}
	}
}

// checkCurrentAbort inspects the abort flags set by the current fill goroutine:
// EIP-2935 history-slot read, or BLOCKHASH(N) read before SRC resolved. Returns
// true when the speculative block must be discarded (caller sets abortRecovery).
func (s *specSession) checkCurrentAbort() bool {
	if s.eip2935Abort {
		log.Warn("Pipelined SRC: discarding speculative block — EIP-2935 slot accessed", "block", s.nextBlockNumber)
		pipelineSpeculativeAbortsCounter.Inc(1)
		return true
	}
	if s.blockhashAccessed.Load() {
		log.Warn("Pipelined SRC: discarding speculative block — BLOCKHASH(N) was accessed",
			"block", s.nextBlockNumber, "pendingBlockN", s.blockNNumber)
		pipelineSpeculativeAbortsCounter.Inc(1)
		pipelineAbortBlockhashCounter.Inc(1)
		return true
	}
	return false
}

// drainPrevDBWrite waits for the previous iteration's async DB write before
// FinalizeForPipeline runs. FinalizeForPipeline may read block headers and
// state sync / span data from the chain DB — if the previous inline-sealed
// block hasn't persisted, those lookups fail.
func (s *specSession) drainPrevDBWrite() {
	if s.prevDBWriteDone != nil {
		<-s.prevDBWriteDone
		s.prevDBWriteDone = nil
	}
}

// finalizeCurrent runs FinalizeForPipeline on the current speculative block,
// extracts its FlatDiff, and returns the final header + stateSync data.
// Returns ok=false if FinalizeForPipeline errors (caller should break).
func (s *specSession) finalizeCurrent() (*types.Header, *state.FlatDiff, []*types.StateSyncData, bool) {
	finalSpecHeader := types.CopyHeader(s.specHeader)
	finalSpecHeader.ParentHash = s.realBlockNHash
	if s.w.chainConfig.IsPrague(finalSpecHeader.Number) {
		evmCtx := core.NewEVMBlockContext(finalSpecHeader, s.w.chain, &s.coinbase)
		vmenv := vm.NewEVM(evmCtx, s.specState, s.w.chainConfig, vm.Config{})
		core.ProcessParentBlockHash(s.realBlockNHash, vmenv)
	}
	stateSyncData, err := s.borEngine.FinalizeForPipeline(s.w.chain, finalSpecHeader, s.specState, &types.Body{
		Transactions: s.specEnv.txs,
	}, s.specEnv.receipts)
	if err != nil {
		log.Error("Pipelined SRC: FinalizeForPipeline failed", "block", s.nextBlockNumber, "err", err)
		return nil, nil, nil, false
	}
	flatDiff := s.specState.CommitSnapshot(s.w.chainConfig.IsEIP158(finalSpecHeader.Number))
	return finalSpecHeader, flatDiff, stateSyncData, true
}

// prepareNextIteration builds the N+2 speculative environment: header,
// Prepare (may seal current via taskCh on failure), SRC spawn for current
// block, state open, EVM+srcDone for next, fill goroutine. cont=false means
// the main loop should break (we already handed off the current block).
func (s *specSession) prepareNextIteration(finalSpecHeader *types.Header, flatDiff *state.FlatDiff, stateSyncData []*types.StateSyncData) (*specNextIteration, bool) {
	specHeaderNext, specContextNext, coinbaseNext, ok := s.buildAndPrepareNextHeader(finalSpecHeader, flatDiff, stateSyncData)
	if !ok {
		return nil, false
	}
	srcSpawnTime := s.spawnSRCForCurrent(finalSpecHeader, flatDiff)
	specStateNext, specEnvNext, blockhashAccessedNext, ok := s.openNextSpecEnv(finalSpecHeader, flatDiff, stateSyncData, specHeaderNext, specContextNext, coinbaseNext)
	if !ok {
		return nil, false
	}
	s.resetTxPoolState(finalSpecHeader, s.rootN, flatDiff)
	fillDone, eip2935AbortPtr, fillElapsedPtr := s.startNextFillGoroutine(specHeaderNext, specEnvNext, specStateNext)
	return &specNextIteration{
		specHeaderNext:    specHeaderNext,
		specStateNext:     specStateNext,
		specEnvNext:       specEnvNext,
		coinbaseNext:      coinbaseNext,
		blockhashAccessed: blockhashAccessedNext,
		eip2935AbortPtr:   eip2935AbortPtr,
		nextBuildStart:    time.Now(),
		fillDone:          fillDone,
		fillElapsed:       fillElapsedPtr,
		srcSpawnTime:      srcSpawnTime,
	}, true
}

// buildAndPrepareNextHeader constructs the next speculative header (N+2),
// runs Prepare via the speculative chain reader, and on Prepare failure
// hands off the CURRENT speculative block via taskCh (spawnSRC=true) before
// returning ok=false so the caller can break out of the loop.
func (s *specSession) buildAndPrepareNextHeader(finalSpecHeader *types.Header, flatDiff *state.FlatDiff, stateSyncData []*types.StateSyncData) (*types.Header, *speculativeChainContext, common.Address, bool) {
	nextNextBlockNumber := s.nextBlockNumber + 1
	specReaderNext := newSpeculativeChainReader(s.w.chain, finalSpecHeader, placeholderParentHash(s.nextBlockNumber))
	specContextNext := newSpeculativeChainContext(specReaderNext, s.w.engine)
	coinbaseNext := s.w.resolveCoinbase(nextNextBlockNumber, s.w.etherbase())
	specHeaderNext := &types.Header{
		ParentHash: placeholderParentHash(s.nextBlockNumber),
		Number:     new(big.Int).SetUint64(nextNextBlockNumber),
		GasLimit:   core.CalcGasLimit(finalSpecHeader.GasLimit, s.w.config.GasCeil),
		Time:       finalSpecHeader.Time + s.w.chainConfig.Bor.CalculatePeriod(nextNextBlockNumber),
		Coinbase:   coinbaseNext,
	}
	if s.w.chainConfig.IsLondon(specHeaderNext.Number) {
		specHeaderNext.BaseFee = eip1559.CalcBaseFee(s.w.chainConfig, finalSpecHeader)
	}
	if err := s.w.engine.Prepare(specReaderNext, specHeaderNext, false); err != nil {
		log.Warn("Pipelined SRC: Prepare failed for next block, sealing current", "block", nextNextBlockNumber, "err", err)
		s.w.sealBlockViaTaskCh(s.borEngine, finalSpecHeader, s.specState, s.specEnv.txs, s.specEnv.receipts, stateSyncData, s.rootN, flatDiff, true, s.curBuildStart)
		return nil, nil, common.Address{}, false
	}
	return specHeaderNext, specContextNext, coinbaseNext, true
}

// spawnSRCForCurrent starts the SRC goroutine that computes the state root
// for the current speculative block (now finalized) while the next block's
// execution runs. Returns the srcSpawnTime used for pipelineSRCTimer.
func (s *specSession) spawnSRCForCurrent(finalSpecHeader *types.Header, flatDiff *state.FlatDiff) time.Time {
	srcSpawnTime := time.Now()
	tmpBlockCur := types.NewBlockWithHeader(finalSpecHeader)
	// Miner pipeline always produces witnesses for now. allowOwnWitness=true
	// explicitly permits SRC to create its own witness when no execution
	// witness is handed in by the caller. nil detached prefetcher — the
	// miner-side path does not currently hand execution prefetcher state to
	// SRC, so SRC falls back to the plain pathdb reader chain.
	s.w.chain.SpawnSRCGoroutine(tmpBlockCur, s.rootN, flatDiff, true, nil, true, nil, false)
	s.w.chain.SetLastFlatDiff(flatDiff, finalSpecHeader.Number.Uint64(), s.rootN, common.Hash{})
	if productionPipelineLogs {
		log.Info("Pipelined SRC: spawned SRC, starting speculative exec", "srcBlock", s.nextBlockNumber, "specExecBlock", s.nextBlockNumber+1)
	}
	return srcSpawnTime
}

// openNextSpecEnv opens the state + environment for the next speculative
// block (N+2). On failure (state open error or grandparent not found), hands
// off the CURRENT speculative block via taskCh with spawnSRC=false (SRC for
// the current block is already in flight from spawnSRCForCurrent).
func (s *specSession) openNextSpecEnv(finalSpecHeader *types.Header, flatDiff *state.FlatDiff, stateSyncData []*types.StateSyncData, specHeaderNext *types.Header, specContextNext *speculativeChainContext, coinbaseNext common.Address) (*state.StateDB, *environment, *atomic.Bool, bool) {
	specStateNext, err := s.w.chain.StateAtWithFlatDiff(s.rootN, flatDiff)
	if err != nil {
		log.Error("Pipelined SRC: failed to open speculative state for next block", "block", s.nextBlockNumber+1, "err", err)
		s.w.sealBlockViaTaskCh(s.borEngine, finalSpecHeader, s.specState, s.specEnv.txs, s.specEnv.receipts, stateSyncData, s.rootN, flatDiff, false, s.curBuildStart)
		return nil, nil, nil, false
	}
	specStateNext.StartPrefetcher("miner-speculative", nil, nil)

	grandparent := s.resolveGrandparent()
	if grandparent == nil {
		log.Error("Pipelined SRC: grandparent header not found for next block", "number", s.blockNNumber)
		s.w.sealBlockViaTaskCh(s.borEngine, finalSpecHeader, s.specState, s.specEnv.txs, s.specEnv.receipts, stateSyncData, s.rootN, flatDiff, false, s.curBuildStart)
		return nil, nil, nil, false
	}

	blockhashAccessedNext := new(atomic.Bool)
	specGetHashNext := core.SpeculativeGetHashFn(grandparent, specContextNext, s.nextBlockNumber, s.makeNextHashResolver(finalSpecHeader), blockhashAccessedNext)
	evmContextNext := core.NewEVMBlockContext(specHeaderNext, specContextNext, &coinbaseNext)
	evmContextNext.GetHash = specGetHashNext

	specEnvNext := &environment{
		signer:         types.MakeSigner(s.w.chainConfig, specHeaderNext.Number, specHeaderNext.Time),
		state:          specStateNext,
		size:           uint64(specHeaderNext.Size()),
		coinbase:       coinbaseNext,
		buildInterrupt: newBuildInterruptState(),
		header:         specHeaderNext,
		evm:            vm.NewEVM(evmContextNext, specStateNext, s.w.chainConfig, vm.Config{}),
	}
	specEnvNext.evm.SetInterrupt(specEnvNext.buildInterrupt.timeoutFlag())
	specEnvNext.tcount = 0
	return specStateNext, specEnvNext, blockhashAccessedNext, true
}

// resolveGrandparent returns the grandparent header for the next iteration.
// Prefers lastSealedHeader (the async DB write may not have persisted yet)
// and falls back to the chain DB.
func (s *specSession) resolveGrandparent() *types.Header {
	if s.lastSealedHeader != nil && s.lastSealedHeader.Number.Uint64() == s.blockNNumber {
		return s.lastSealedHeader
	}
	return s.w.chain.GetHeaderByNumber(s.blockNNumber)
}

// makeNextHashResolver returns a lazy resolver for the current speculative
// block's signed hash, used by SpeculativeGetHashFn of the NEXT speculative
// block. Mirrors newBlockNHashResolver but for mid-pipeline iterations.
func (s *specSession) makeNextHashResolver(finalSpecHeader *types.Header) func() common.Hash {
	var (
		hash     common.Hash
		resolved bool
		mu       sync.Mutex
	)
	return func() common.Hash {
		mu.Lock()
		defer mu.Unlock()
		if resolved {
			return hash
		}
		rootSpec, _, err := s.w.chain.WaitForSRC()
		if err != nil {
			log.Error("Pipelined SRC: SRC failed during BLOCKHASH resolution", "err", err)
			return common.Hash{}
		}
		finalH := types.CopyHeader(finalSpecHeader)
		finalH.Root = rootSpec
		finalH.UncleHash = types.CalcUncleHash(nil)
		hash = finalH.Hash()
		resolved = true
		return hash
	}
}

// startNextFillGoroutine fills N+2 speculatively in parallel with the current
// block's seal, and flags EIP-2935 aborts for N+2. Returns the done channel
// and pointers to the abort/elapsed fields set by the goroutine (only safe to
// read after <-fillDone).
func (s *specSession) startNextFillGoroutine(headerNext *types.Header, envNext *environment, stateNext *state.StateDB) (chan struct{}, *bool, *time.Duration) {
	fillDone := make(chan struct{})
	var (
		eip2935Abort bool
		fillElapsed  time.Duration
	)
	go func() {
		defer close(fillDone)
		stop := createInterruptTimer(
			headerNext.Number.Uint64(),
			headerNext.GetActualTime(),
			envNext.buildInterrupt.timeoutFlag(),
			envNext.buildInterrupt.flagSetAtPtr(),
			true,
		)
		var interrupt atomic.Int32
		fillElapsed = s.w.fillSpeculativeTransactions(envNext, &interrupt)
		stop()
		if s.w.chainConfig.IsPrague(headerNext.Number) {
			dangerousSlot := common.BigToHash(new(big.Int).SetUint64(s.nextBlockNumber % params.HistoryServeWindow))
			if stateNext.WasStorageSlotRead(params.HistoryStorageAddress, dangerousSlot) {
				eip2935Abort = true
				pipelineEIP2935AbortsCounter.Inc(1)
			}
		}
	}()
	return fillDone, &eip2935Abort, &fillElapsed
}

// sealCurrentAndAdvance waits for SRC of the current speculative block,
// assembles it, waits for header.Time, inline-seals + broadcasts, and hands
// back the sealed block. Returns exitEarly=true if w.exitCh fired during the
// timestamp wait (caller returns false, abortRecovery).
func (s *specSession) sealCurrentAndAdvance(finalSpecHeader *types.Header, stateSyncData []*types.StateSyncData, next *specNextIteration) (*types.Block, bool, bool) {
	srcWaitStart := time.Now()
	rootSpec, witnessSpec, err := s.w.chain.WaitForSRC()
	srcWaitElapsed := time.Since(srcWaitStart)
	pipelineSRCTimer.Update(time.Since(next.srcSpawnTime))
	pipelineSRCWaitTimer.Update(srcWaitElapsed)
	if err != nil {
		log.Error("Pipelined SRC: SRC failed", "block", s.nextBlockNumber, "err", err)
		pipelineSpeculativeAbortsCounter.Inc(1)
		pipelineAbortSRCFailedCounter.Inc(1)
		<-next.fillDone
		return nil, false, false
	}
	if productionPipelineLogs {
		log.Info("Pipelined SRC: SRC completed", "block", s.nextBlockNumber, "srcWait", srcWaitElapsed)
	}
	blockSpec, receiptsSpec, err := s.borEngine.AssembleBlock(s.w.chain, finalSpecHeader, s.specState, &types.Body{
		Transactions: s.specEnv.txs,
	}, s.specEnv.receipts, rootSpec, stateSyncData)
	if err != nil {
		log.Error("Pipelined SRC: AssembleBlock failed", "block", s.nextBlockNumber, "err", err)
		<-next.fillDone
		return nil, false, false
	}
	// Update pendingWorkBlock BEFORE inline write so that newWorkLoop skips
	// the ChainHeadEvent for this block. pendingWorkBlock = nextBlockNumber+1
	// means "working on nextBlockNumber+1, so skip ChainHeadEvent for nextBlockNumber".
	s.w.pendingWorkBlock.Store(s.nextBlockNumber + 1)
	if exit := s.waitForParentAnnounceTime(finalSpecHeader, next.fillDone); exit {
		return nil, true, false
	}
	sealedBlock, dbWriteDone, err := s.w.inlineSealAndBroadcast(blockSpec, receiptsSpec, s.specState, witnessSpec, s.curBuildStart)
	if err != nil {
		log.Error("Pipelined SRC: inline seal failed", "block", s.nextBlockNumber, "err", err)
		<-next.fillDone
		return nil, false, false
	}
	<-next.fillDone
	s.prevDBWriteDone = dbWriteDone
	pipelineSpeculativeBlocksCounter.Inc(1)
	if productionPipelineLogs {
		log.Info("Pipelined SRC: block sealed (inline)", "number", sealedBlock.Number(),
			"txs", len(sealedBlock.Transactions()), "root", rootSpec, "fillBlock", s.nextBlockNumber+1, "fillElapsed", *next.fillElapsed)
	}
	return sealedBlock, false, true
}

// waitForParentAnnounceTime blocks until the parent slot boundary is reached,
// draining the fill and previous-DB-write channels on shutdown so goroutines
// aren't left hanging. Giugliano+ primary producers may announce before the
// child block timestamp, but not before the parent timestamp.
func (s *specSession) waitForParentAnnounceTime(finalSpecHeader *types.Header, fillDone chan struct{}) bool {
	delay := time.Until(s.w.earlyAnnounceTime(finalSpecHeader))
	if delay <= 0 {
		return false
	}
	select {
	case <-time.After(delay):
		return false
	case <-s.w.exitCh:
		<-fillDone
		if s.prevDBWriteDone != nil {
			<-s.prevDBWriteDone
		}
		return true
	}
}

// shiftToNext rotates the session's per-iteration state to the block just
// prepared by prepareNextIteration. Called after a successful inline seal.
func (s *specSession) shiftToNext(sealed *types.Block, next *specNextIteration) {
	s.lastSealedHeader = sealed.Header()
	s.blockNNumber = s.nextBlockNumber
	s.nextBlockNumber++
	s.rootN = sealed.Root()
	s.realBlockNHash = sealed.Hash()
	s.specHeader = next.specHeaderNext
	s.specState = next.specStateNext
	s.specEnv = next.specEnvNext
	s.coinbase = next.coinbaseNext
	s.eip2935Abort = *next.eip2935AbortPtr
	s.blockhashAccessed = next.blockhashAccessed
	s.curBuildStart = next.nextBuildStart
}

// fallbackToSequential computes the state root inline and assembles block N
// without a background SRC goroutine. This avoids trie DB races between
// background and inline commits.
func (w *worker) fallbackToSequential(req *speculativeWorkReq) {
	if productionPipelineLogs {
		log.Info("Pipelined SRC: falling back to sequential execution")
	}
	pipelineSpeculativeAbortsCounter.Inc(1)
	pipelineAbortFallbackCounter.Inc(1)

	borEngine, ok := w.engine.(*bor.Bor)
	if !ok {
		return
	}

	root := req.blockNEnv.state.IntermediateRoot(w.chainConfig.IsEIP158(req.blockNEnv.header.Number))

	block, receipts, err := borEngine.AssembleBlock(w.chain, req.blockNEnv.header, req.blockNEnv.state, &types.Body{
		Transactions: req.blockNEnv.txs,
	}, req.blockNEnv.receipts, root, req.stateSyncData)
	if err != nil {
		log.Error("Pipelined SRC: AssembleBlock failed during fallback", "err", err)
		return
	}

	select {
	case w.taskCh <- &task{receipts: receipts, state: req.blockNEnv.state, block: block, createdAt: time.Now()}:
		if productionPipelineLogs {
			log.Info("Pipelined SRC: fallback block sealed", "number", block.Number(), "root", root)
		}
	case <-w.exitCh:
	}
}

// sealBlockViaTaskCh spawns SRC (if needed), waits for the root, assembles the
// block, and sends it through the normal taskCh → taskLoop → Seal → resultLoop
// path. Used for the last block in a pipeline run so that resultLoop emits
// ChainHeadEvent and normal block production resumes immediately.
func (w *worker) sealBlockViaTaskCh(
	borEngine *bor.Bor,
	finalHeader *types.Header,
	statedb *state.StateDB,
	txs []*types.Transaction,
	receipts []*types.Receipt,
	stateSyncData []*types.StateSyncData,
	rootN common.Hash,
	flatDiff *state.FlatDiff,
	spawnSRC bool, // false if SRC goroutine is already running
	buildStart time.Time, // wall clock when this block's speculative fill began — for worker/build_to_announce
) {
	w.spawnSRCForFinalBlock(finalHeader, rootN, flatDiff, spawnSRC)
	pipelineSpeculativeBlocksCounter.Inc(1)

	rootSpec, witnessSpec, err := w.chain.WaitForSRC()
	if err != nil {
		log.Error("Pipelined SRC: SRC failed", "block", finalHeader.Number, "err", err)
		return
	}

	block, blockReceipts, err := borEngine.AssembleBlock(w.chain, finalHeader, statedb, &types.Body{
		Transactions: txs,
	}, receipts, rootSpec, stateSyncData)
	if err != nil {
		log.Error("Pipelined SRC: AssembleBlock failed", "block", finalHeader.Number, "err", err)
		return
	}

	// Speculative Prepare() was called without sleeping. Wait only until the
	// parent slot boundary, preserving early announcement for primary producers.
	if delay := time.Until(w.earlyAnnounceTime(finalHeader)); delay > 0 {
		select {
		case <-time.After(delay):
		case <-w.exitCh:
			return
		}
	}

	select {
	case w.taskCh <- &task{receipts: blockReceipts, state: statedb, block: block, createdAt: time.Now(), productionStart: buildStart, pipelined: true, witnessBytes: witnessSpec}:
		if productionPipelineLogs {
			log.Info("Pipelined SRC: block sealed", "number", block.Number(),
				"txs", len(block.Transactions()), "root", rootSpec)
		}
	case <-w.exitCh:
	}
}

// earlyAnnounceTime returns the earliest safe local announcement time for a
// prepared block. Post-Giugliano verification allows primary-producer blocks to
// arrive before their own timestamp, but not before the parent timestamp.
func (w *worker) earlyAnnounceTime(header *types.Header) time.Time {
	if header == nil || header.Number == nil || header.Number.Sign() == 0 {
		return time.Now()
	}
	if borEngine, ok := w.engine.(*bor.Bor); ok {
		return borEngine.EarliestAnnounceTime(w.chain, header)
	}
	if parent := w.chain.GetHeader(header.ParentHash, header.Number.Uint64()-1); parent != nil {
		return parent.GetActualTime()
	}
	return header.GetActualTime()
}

// inlineSealAndBroadcast seals a pipelined block using a private channel
// (bypassing taskLoop/resultLoop), broadcasts it to peers immediately, and
// writes to the chain DB asynchronously. This avoids blocking the pipeline
// on the DB write — the next iteration can start as soon as the block is sealed.
//
// Returns the sealed block and a channel that closes when the async DB write
// completes. The caller must wait on writeDone before the node can serve the
// block data from DB, but the pipeline can proceed immediately.
//
// Uses emitHeadEvent=false to avoid a deadlock: mainLoop is blocked in
// commitSpeculativeWork, so chainHeadFeed.Send would eventually block when
// newWorkLoop's channel fills up.
func (w *worker) inlineSealAndBroadcast(block *types.Block, receipts []*types.Receipt, statedb *state.StateDB, witnessBytes []byte, buildStart time.Time) (*types.Block, chan struct{}, error) {
	sealedBlock, err := w.sealViaPrivateChannel(block)
	if err != nil {
		return nil, nil, err
	}
	hash := sealedBlock.Hash()
	sealedReceipts, logs := rebindReceiptsToSealedBlock(receipts, sealedBlock)

	log.Info("Successfully sealed new block", "number", sealedBlock.Number(),
		"sealhash", w.engine.SealHash(sealedBlock.Header()), "hash", hash, "elapsed", "inline")

	// Cache the witness so the WIT protocol can serve it to stateless peers
	// immediately, without waiting for the async DB write.
	if len(witnessBytes) > 0 {
		w.chain.CacheWitness(hash, witnessBytes)
	}

	w.announceInlineSealedBlock(sealedBlock, buildStart)
	w.clearPending(sealedBlock.NumberU64())

	// Write to chain DB asynchronously — the pipeline can proceed with the
	// next iteration using sealedBlock.Hash() directly, without waiting for
	// the DB write to complete.
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeStart := time.Now()
		_, err := w.chain.WriteBlockAndSetHeadPipelined(sealedBlock, sealedReceipts, logs, statedb, false, witnessBytes)
		writeBlockAndSetHeadTimer.UpdateSince(writeStart)
		if err != nil {
			log.Error("Pipelined SRC: async DB write failed", "block", sealedBlock.Number(), "err", err)
		}
	}()
	return sealedBlock, writeDone, nil
}

// sealViaPrivateChannel runs engine.Seal on a private channel (no contention
// with the shared resultCh) and waits up to 5s for the sealed block.
// For primary producers on Bhilai+, delay=0, so the wait is effectively
// bounded by the Seal signature computation.
func (w *worker) sealViaPrivateChannel(block *types.Block) (*types.Block, error) {
	sealCh := make(chan *consensus.NewSealedBlockEvent, 1)
	stopCh := make(chan struct{})
	sealStart := time.Now()
	if err := w.engine.Seal(w.chain, block, nil, sealCh, stopCh); err != nil {
		return nil, fmt.Errorf("seal failed: %w", err)
	}
	select {
	case ev := <-sealCh:
		pipelineSealDurationTimer.UpdateSince(sealStart)
		if ev == nil || ev.Block == nil {
			return nil, errors.New("nil sealed block from Seal")
		}
		return ev.Block, nil
	case <-time.After(5 * time.Second):
		close(stopCh)
		return nil, errors.New("inline seal timed out")
	case <-w.exitCh:
		close(stopCh)
		return nil, errors.New("worker stopped during inline seal")
	}
}

// rebindReceiptsToSealedBlock copies receipts with BlockHash/BlockNumber/
// TransactionIndex pointing at the sealed block, deep-copies logs, and
// returns the flat logs slice (same behavior as resultLoop's receipt fixup).
func rebindReceiptsToSealedBlock(receipts []*types.Receipt, sealedBlock *types.Block) ([]*types.Receipt, []*types.Log) {
	hash := sealedBlock.Hash()
	sealedReceipts := make([]*types.Receipt, len(receipts))
	var logs []*types.Log
	for i, r := range receipts {
		receipt := new(types.Receipt)
		sealedReceipts[i] = receipt
		*receipt = *r
		receipt.BlockHash = hash
		receipt.BlockNumber = sealedBlock.Number()
		receipt.TransactionIndex = uint(i)
		receipt.Logs = make([]*types.Log, len(r.Logs))
		for j, l := range r.Logs {
			logCopy := new(types.Log)
			receipt.Logs[j] = logCopy
			*logCopy = *l
			logCopy.BlockHash = hash
		}
		logs = append(logs, receipt.Logs...)
	}
	return sealedReceipts, logs
}

// announceInlineSealedBlock emits the pipelined-sealed block to peers and
// updates the build-to-announce / earliness / committed / throughput metrics.
// Broadcast happens BEFORE the async DB write so peers don't wait on disk.
func (w *worker) announceInlineSealedBlock(sealedBlock *types.Block, buildStart time.Time) {
	announceAt := time.Now()
	// Positive when announced before header.GetActualTime (PIP-66 early). Negative when late.
	earlyMs := sealedBlock.Header().GetActualTime().Sub(announceAt).Milliseconds()
	pipelineAnnounceEarlinessMs.Update(earlyMs)
	pipelineSpeculativeCommittedCounter.Inc(1)
	if !buildStart.IsZero() {
		workerBuildToAnnounceTimer.UpdateSince(buildStart)
	}
	w.mux.Post(core.NewMinedBlockEvent{Block: sealedBlock, SealedAt: announceAt})
	sealedBlocksCounter.Inc(1)
	if sealedBlock.Transactions().Len() == 0 {
		sealedEmptyBlocksCounter.Inc(1)
	}
	workerGasUsedPerBlockHistogram.Update(int64(sealedBlock.GasUsed()))
	workerTxsPerBlockHistogram.Update(int64(sealedBlock.Transactions().Len()))
}
