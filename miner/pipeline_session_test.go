package miner

import (
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/bor"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func newPipelineWorkerFixture(t *testing.T, configure func(*params.ChainConfig)) (*worker, *testWorkerBackend) {
	t.Helper()

	chainConfig := *params.BorUnittestChainConfig
	borConfig := *chainConfig.Bor
	borConfig.RioBlock = big.NewInt(0)
	chainConfig.Bor = &borConfig
	if configure != nil {
		configure(&chainConfig)
	}

	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	t.Cleanup(ctrl.Finish)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })

	w, backend, cleanup := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	t.Cleanup(func() {
		select {
		case <-w.exitCh:
		default:
			cleanup()
		}
	})
	return w, backend
}

func newPipelineRequestFixture(t *testing.T, configure func(*params.ChainConfig)) (*worker, *testWorkerBackend, *speculativeWorkReq) {
	t.Helper()

	w, backend := newPipelineWorkerFixture(t, configure)

	parent := backend.chain.CurrentBlock()
	statedb, _, prefetchReader, processReader, err := backend.chain.StateAtWithReaders(parent.Root)
	require.NoError(t, err)
	work, err := w.prepareWork(&generateParams{
		timestamp:      uint64(time.Now().Unix()),
		coinbase:       testBankAddress,
		parentHash:     parent.Hash(),
		statedb:        statedb,
		prefetchReader: prefetchReader,
		processReader:  processReader,
	}, false)
	require.NoError(t, err)

	req, ok := w.buildSpeculativeReq(work, nil)
	require.True(t, ok)
	return w, backend, req
}

func newPipelineSessionFixture(t *testing.T, configure func(*params.ChainConfig)) (*worker, *testWorkerBackend, *specSession) {
	t.Helper()

	w, backend, req := newPipelineRequestFixture(t, configure)
	session := newSpecSession(w, req)
	require.True(t, session.setupInitial())
	return w, backend, session
}

func TestSpecSessionAdvancesPipelinedBlock(t *testing.T) {
	w, backend, session := newPipelineSessionFixture(t, nil)

	<-session.initialFillDone
	require.True(t, session.waitForSRCAndSealBlockN())
	require.Equal(t, uint64(1), backend.chain.CurrentBlock().Number.Uint64())

	finalHeader, flatDiff, stateSyncData, ok := session.finalizeCurrent()
	require.True(t, ok)
	next, ok := session.prepareNextIteration(finalHeader, flatDiff, stateSyncData)
	require.True(t, ok)

	sealed, exitEarly, ok := session.sealCurrentAndAdvance(finalHeader, stateSyncData, next)
	require.True(t, ok)
	require.False(t, exitEarly)
	session.shiftToNext(sealed, next)
	session.drainPrevDBWrite()

	require.Equal(t, uint64(2), backend.chain.CurrentBlock().Number.Uint64())
	require.Equal(t, uint64(2), session.blockNNumber)
	require.Equal(t, uint64(3), session.nextBlockNumber)
	require.Equal(t, sealed.Hash(), session.realBlockNHash)
	require.Equal(t, sealed.Root(), session.rootN)
	require.NotNil(t, session.specEnv)
	require.NotNil(t, session.specState)
	require.NotNil(t, session.blockhashAccessed)
	require.NotNil(t, session.specEnv.evm)
	require.Zero(t, session.specEnv.tcount)
	require.Equal(t, core.CalcGasLimit(finalHeader.GasLimit, w.config.GasCeil), session.specHeader.GasLimit)
}

func TestPipelineSessionFailureAndExitBranches(t *testing.T) {
	t.Run("missing parent skips speculative request", func(t *testing.T) {
		w, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		session.req.blockNEnv.header.ParentHash = common.HexToHash("0xdead")
		req, ok := w.buildSpeculativeReq(session.req.blockNEnv, nil)
		require.False(t, ok)
		require.Nil(t, req)
	})

	t.Run("non Bor engine rejects pipeline operations", func(t *testing.T) {
		w, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		w.close()
		wrapped := &pipelineSealEngine{Engine: w.engine, seal: func(consensus.ChainHeaderReader, *types.Block, *stateless.Witness, chan<- *consensus.NewSealedBlockEvent, <-chan struct{}) error {
			return nil
		}}
		w.engine = wrapped
		w.running.Store(true)
		require.NoError(t, w.commitPipelined(session.req.blockNEnv, time.Now()))
		w.running.Store(false)
		require.False(t, newSpecSession(w, session.req).setupInitial())
		w.fallbackToSequential(session.req)
	})

	t.Run("abort flags stop an iteration", func(t *testing.T) {
		session := &specSession{
			eip2935Abort:      true,
			blockhashAccessed: new(atomic.Bool),
		}
		require.Equal(t, iterBreakAbort, session.runOneIteration())
	})

	t.Run("missing SRC fails resolvers and sealing", func(t *testing.T) {
		w, _ := newPipelineWorkerFixture(t, nil)
		session := &specSession{
			w:               w,
			borEngine:       w.engine.(*bor.Bor),
			blockNHeader:    &types.Header{Number: big.NewInt(1)},
			nextBlockNumber: 2,
			specHeader:      &types.Header{Number: big.NewInt(2)},
		}

		require.Equal(t, common.Hash{}, session.newBlockNHashResolver()())
		require.Equal(t, common.Hash{}, session.makeNextHashResolver(session.specHeader)())

		fillDone := make(chan struct{})
		close(fillDone)
		next := &specNextIteration{fillDone: fillDone, srcSpawnTime: time.Now()}
		sealed, exitEarly, ok := session.sealCurrentAndAdvance(session.specHeader, nil, next)
		require.False(t, ok)
		require.False(t, exitEarly)
		require.Nil(t, sealed)

		w.sealBlockViaTaskCh(
			w.engine.(*bor.Bor),
			session.specHeader,
			session.specState,
			nil,
			nil,
			nil,
			session.rootN,
			new(state.FlatDiff),
			false,
			time.Now(),
		)
	})

	t.Run("invalid speculative roots are soft failures", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		session.resetTxPoolState(session.blockNHeader, common.HexToHash("0xdead"), new(state.FlatDiff))
	})

	t.Run("chain head mismatch is rejected", func(t *testing.T) {
		_, backend, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		hash, ok := session.waitForChainHead(0)
		require.False(t, ok)
		require.Equal(t, common.Hash{}, hash)
		require.Equal(t, uint64(1), backend.chain.CurrentBlock().Number.Uint64())
	})

	t.Run("Prague finalization records parent hash", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, func(config *params.ChainConfig) {
			config.ShanghaiBlock = big.NewInt(0)
			config.CancunBlock = big.NewInt(0)
			config.PragueBlock = big.NewInt(0)
		})
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		header, diff, _, ok := session.finalizeCurrent()
		require.True(t, ok)
		require.NotNil(t, header)
		require.NotNil(t, diff)
	})
}

func TestCommitSpeculativeWorkCompletesLastPipelineBlock(t *testing.T) {
	w, backend, req := newPipelineRequestFixture(t, nil)

	shouldRetry, abortRecovery := w.commitSpeculativeWork(req)
	require.True(t, shouldRetry)
	require.False(t, abortRecovery)
	require.Eventually(t, func() bool {
		return backend.chain.CurrentBlock().Number.Uint64() == 2
	}, time.Second, 10*time.Millisecond)
	require.Zero(t, w.pendingWorkBlock.Load())
}

func TestPipelineSessionAdditionalRecoveryBranches(t *testing.T) {
	t.Run("setup falls back when speculative root is unavailable", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		req.parentRoot = common.HexToHash("0xdead")
		require.False(t, newSpecSession(w, req).setupInitial())
	})

	t.Run("block N fails without pending SRC", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		session := newSpecSession(w, req)
		session.borEngine = w.engine.(*bor.Bor)
		require.False(t, session.waitForSRCAndSealBlockN())
	})

	t.Run("next environment rejects bad root", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		finalHeader, flatDiff, syncData, ok := session.finalizeCurrent()
		require.True(t, ok)

		nextHeader, nextContext, coinbase, ok := session.buildAndPrepareNextHeader(finalHeader, flatDiff, syncData)
		require.True(t, ok)
		session.rootN = common.HexToHash("0xdead")
		nextState, nextEnv, accessed, ok := session.openNextSpecEnv(
			finalHeader, flatDiff, syncData, nextHeader, nextContext, coinbase,
		)
		require.False(t, ok)
		require.Nil(t, nextState)
		require.Nil(t, nextEnv)
		require.Nil(t, accessed)
	})

	t.Run("next environment requires grandparent", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		finalHeader, flatDiff, syncData, ok := session.finalizeCurrent()
		require.True(t, ok)
		nextHeader, nextContext, coinbase, ok := session.buildAndPrepareNextHeader(finalHeader, flatDiff, syncData)
		require.True(t, ok)

		session.blockNNumber = 999
		session.lastSealedHeader = nil
		_, _, _, ok = session.openNextSpecEnv(finalHeader, flatDiff, syncData, nextHeader, nextContext, coinbase)
		require.False(t, ok)
	})

	t.Run("Prague next fill detects history access", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, func(config *params.ChainConfig) {
			config.ShanghaiBlock = big.NewInt(0)
			config.CancunBlock = big.NewInt(0)
			config.PragueBlock = big.NewInt(0)
		})
		<-session.initialFillDone

		slot := common.BigToHash(new(big.Int).SetUint64(session.nextBlockNumber % params.HistoryServeWindow))
		session.specState.CreateAccount(params.HistoryStorageAddress)
		session.specState.GetState(params.HistoryStorageAddress, slot)
		done, aborted, _ := session.startNextFillGoroutine(session.specHeader, session.specEnv, session.specState)
		<-done
		require.True(t, *aborted)
	})

	t.Run("inline broadcast returns seal error", func(t *testing.T) {
		w, _, _ := newPipelineRequestFixture(t, nil)
		w.close()
		sealErr := errors.New("seal failed")
		w.engine = &pipelineSealEngine{
			Engine: w.engine,
			seal: func(consensus.ChainHeaderReader, *types.Block, *stateless.Witness, chan<- *consensus.NewSealedBlockEvent, <-chan struct{}) error {
				return sealErr
			},
		}
		block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
		sealed, done, err := w.inlineSealAndBroadcast(block, nil, nil, nil, time.Now())
		require.ErrorIs(t, err, sealErr)
		require.Nil(t, sealed)
		require.Nil(t, done)
	})
}

func TestCommitPipelinedAdditionalBranches(t *testing.T) {
	t.Run("finalize rejects unexpected withdrawals", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		w.running.Store(true)
		hash := common.HexToHash("0x1")
		req.blockNEnv.header.WithdrawalsHash = &hash
		require.ErrorIs(t, w.commitPipelined(req.blockNEnv, time.Now()), consensus.ErrUnexpectedWithdrawals)
	})

	t.Run("missing parent skips handoff", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		w.running.Store(true)
		req.blockNEnv.header.ParentHash = common.HexToHash("0xdead")
		require.NoError(t, w.commitPipelined(req.blockNEnv, time.Now()))
		select {
		case <-w.speculativeWorkCh:
			t.Fatal("unexpected speculative request")
		default:
		}
	})

	t.Run("worker exit cancels handoff", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		w.close()
		w.running.Store(true)
		w.speculativeWorkCh = make(chan *speculativeWorkReq)

		require.NoError(t, w.commitPipelined(req.blockNEnv, time.Now()))
	})
}

func TestSpecSessionMoreFailureBranches(t *testing.T) {
	t.Run("commit handles setup failure", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		req.parentRoot = common.HexToHash("0xdead")
		retry, recovery := w.commitSpeculativeWork(req)
		require.False(t, retry)
		require.False(t, recovery)
		require.Zero(t, w.pendingWorkBlock.Load())
	})

	t.Run("initial setup requires grandparent", func(t *testing.T) {
		w, _, req := newPipelineRequestFixture(t, nil)
		w.close()
		req.parentHeader.ParentHash = common.HexToHash("0xdead")

		require.False(t, newSpecSession(w, req).setupInitial())
	})

	t.Run("initial fill detects Prague history access", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, func(config *params.ChainConfig) {
			config.ShanghaiBlock = big.NewInt(0)
			config.CancunBlock = big.NewInt(0)
			config.PragueBlock = big.NewInt(0)
		})
		<-session.initialFillDone

		slot := common.BigToHash(new(big.Int).SetUint64(session.blockNNumber % params.HistoryServeWindow))
		session.specState.CreateAccount(params.HistoryStorageAddress)
		session.specState.GetState(params.HistoryStorageAddress, slot)
		session.startInitialFillGoroutine()
		<-session.initialFillDone
		require.True(t, session.eip2935Abort)
	})

	t.Run("iteration stops when finalization fails", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		hash := common.HexToHash("0x1")
		session.specHeader.RequestsHash = &hash
		require.Equal(t, iterBreak, session.runOneIteration())
	})

	t.Run("prepare wrapper propagates next state failure", func(t *testing.T) {
		_, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		finalHeader, flatDiff, syncData, ok := session.finalizeCurrent()
		require.True(t, ok)
		session.rootN = common.HexToHash("0xdead")

		next, ok := session.prepareNextIteration(finalHeader, flatDiff, syncData)
		require.False(t, ok)
		require.Nil(t, next)
	})

	t.Run("prepare failure hands current block to task path", func(t *testing.T) {
		w, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		finalHeader, flatDiff, syncData, ok := session.finalizeCurrent()
		require.True(t, ok)

		w.close()
		prepareErr := errors.New("prepare failed")
		w.engine = &pipelinePrepareEngine{
			Engine: w.engine,
			prepare: func(consensus.ChainHeaderReader, *types.Header, bool) error {
				return prepareErr
			},
		}
		next, ok := session.prepareNextIteration(finalHeader, flatDiff, syncData)
		require.False(t, ok)
		require.Nil(t, next)
	})
}

func TestSealCurrentAndAdvanceExitAndSealFailure(t *testing.T) {
	newPreparedSession := func(t *testing.T) (*worker, *specSession, *types.Header, []*types.StateSyncData, *specNextIteration) {
		t.Helper()
		w, _, session := newPipelineSessionFixture(t, nil)
		<-session.initialFillDone
		require.True(t, session.waitForSRCAndSealBlockN())
		finalHeader, flatDiff, syncData, ok := session.finalizeCurrent()
		require.True(t, ok)
		next, ok := session.prepareNextIteration(finalHeader, flatDiff, syncData)
		require.True(t, ok)
		return w, session, finalHeader, syncData, next
	}

	t.Run("worker exit interrupts announce wait", func(t *testing.T) {
		w, session, finalHeader, syncData, next := newPreparedSession(t)
		w.close()
		finalHeader.ActualTime = time.Now().Add(time.Hour)

		sealed, exitEarly, ok := session.sealCurrentAndAdvance(finalHeader, syncData, next)
		require.False(t, ok)
		require.True(t, exitEarly)
		require.Nil(t, sealed)
	})

	t.Run("inline seal error stops iteration", func(t *testing.T) {
		w, session, finalHeader, syncData, next := newPreparedSession(t)
		w.close()
		sealErr := errors.New("seal failed")
		w.engine = &pipelineSealEngine{
			Engine: w.engine,
			seal: func(consensus.ChainHeaderReader, *types.Block, *stateless.Witness, chan<- *consensus.NewSealedBlockEvent, <-chan struct{}) error {
				return sealErr
			},
		}
		finalHeader.ActualTime = time.Now().Add(-time.Second)

		sealed, exitEarly, ok := session.sealCurrentAndAdvance(finalHeader, syncData, next)
		require.False(t, ok)
		require.False(t, exitEarly)
		require.Nil(t, sealed)
	})
}
