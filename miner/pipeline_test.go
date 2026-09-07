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
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
)

type pipelineSealEngine struct {
	consensus.Engine
	seal func(consensus.ChainHeaderReader, *types.Block, *stateless.Witness, chan<- *consensus.NewSealedBlockEvent, <-chan struct{}) error
}

func (e *pipelineSealEngine) Seal(chain consensus.ChainHeaderReader, block *types.Block, witness *stateless.Witness, results chan<- *consensus.NewSealedBlockEvent, stop <-chan struct{}) error {
	return e.seal(chain, block, witness, results, stop)
}

type pipelinePrepareEngine struct {
	consensus.Engine
	prepare func(consensus.ChainHeaderReader, *types.Header, bool) error
}

func (e *pipelinePrepareEngine) Prepare(chain consensus.ChainHeaderReader, header *types.Header, mining bool) error {
	return e.prepare(chain, header, mining)
}

func TestShouldLateRefillSpeculativeBlock(t *testing.T) {
	t.Parallel()

	newEnv := func(txs int, gasLimit uint64, remainingGas uint64, withGasPool bool) *environment {
		env := &environment{
			header: &types.Header{
				Number:   big.NewInt(1),
				GasLimit: gasLimit,
			},
			txs: make([]*types.Transaction, txs),
		}
		if withGasPool {
			env.gasPool = new(core.GasPool).AddGas(remainingGas)
		}
		return env
	}

	require.True(t, shouldLateRefillSpeculativeBlock(newEnv(0, 1000, 0, false)))
	require.True(t, shouldLateRefillSpeculativeBlock(newEnv(1, 1000, 600, true)))
	require.True(t, shouldLateRefillSpeculativeBlock(newEnv(2, 1000, 0, false)))
	require.False(t, shouldLateRefillSpeculativeBlock(newEnv(1, 1000, 200, true)))
}

func TestPipelineLocalHelpers(t *testing.T) {
	t.Parallel()

	t.Run("placeholder parent hash", func(t *testing.T) {
		first := placeholderParentHash(17)
		require.NotEqual(t, common.Hash{}, first)
		require.Equal(t, first, placeholderParentHash(17))
		require.NotEqual(t, first, placeholderParentHash(18))
	})

	t.Run("disabled pipeline has no worker dependencies", func(t *testing.T) {
		w := new(worker)
		require.False(t, w.isPipelineEligible(1))
		require.NoError(t, w.commitPipelined(nil, time.Now()))
		w.spawnSRCForFinalBlock(nil, common.Hash{}, nil, false)
		require.WithinDuration(t, time.Now(), w.earlyAnnounceTime(nil), time.Second)
	})

	t.Run("session construction and state shift", func(t *testing.T) {
		req := &speculativeWorkReq{parentHeader: &types.Header{Number: big.NewInt(9)}}
		session := newSpecSession(new(worker), req)
		require.Equal(t, uint64(9), session.blockNNumber)
		require.Equal(t, uint64(10), session.nextBlockNumber)
		require.Same(t, req, session.req)

		abort := new(atomic.Bool)
		abort.Store(true)
		fillAbort := true
		fillElapsed := time.Millisecond
		nextHeader := &types.Header{Number: big.NewInt(11)}
		nextState := new(state.StateDB)
		nextEnv := new(environment)
		next := &specNextIteration{
			specHeaderNext:    nextHeader,
			specStateNext:     nextState,
			specEnvNext:       nextEnv,
			coinbaseNext:      common.HexToAddress("0x1234"),
			blockhashAccessed: abort,
			eip2935AbortPtr:   &fillAbort,
			nextBuildStart:    time.Now(),
			fillElapsed:       &fillElapsed,
		}
		sealed := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), Root: common.HexToHash("0x01")})
		session.shiftToNext(sealed, next)
		require.Equal(t, uint64(10), session.blockNNumber)
		require.Equal(t, uint64(11), session.nextBlockNumber)
		require.Equal(t, sealed.Root(), session.rootN)
		require.Equal(t, sealed.Hash(), session.realBlockNHash)
		require.Equal(t, nextHeader, session.specHeader)
		require.Equal(t, nextState, session.specState)
		require.Equal(t, nextEnv, session.specEnv)
		require.Equal(t, next.coinbaseNext, session.coinbase)
		require.True(t, session.eip2935Abort)
		require.Same(t, abort, session.blockhashAccessed)
	})

	t.Run("abort and async write coordination", func(t *testing.T) {
		blockhashAccessed := new(atomic.Bool)
		session := &specSession{blockhashAccessed: blockhashAccessed}
		require.False(t, session.checkCurrentAbort())
		blockhashAccessed.Store(true)
		require.True(t, session.checkCurrentAbort())

		session = &specSession{eip2935Abort: true, blockhashAccessed: new(atomic.Bool)}
		require.True(t, session.checkCurrentAbort())

		done := make(chan struct{})
		close(done)
		session = &specSession{prevDBWriteDone: done}
		session.drainPrevDBWrite()
		require.Nil(t, session.prevDBWriteDone)
	})
}

func TestPipelineSpeculativeReaderBranches(t *testing.T) {
	inner := newMockChainHeaderReader()
	parent := &types.Header{Number: big.NewInt(4), Extra: []byte("parent")}
	inner.addHeader(parent)
	pending := &types.Header{Number: big.NewInt(5), ParentHash: parent.Hash()}
	placeholder := common.HexToHash("0xfeed")
	reader := newSpeculativeChainReader(inner, pending, placeholder)

	require.Nil(t, reader.CurrentHeader())
	require.Same(t, pending, reader.GetHeader(placeholder, 5))
	require.Same(t, pending, reader.GetHeaderByNumber(5))
	require.Same(t, pending, reader.GetHeaderByHash(placeholder))
	require.Same(t, parent, reader.GetHeaderByHash(parent.Hash()))
	require.Equal(t, big.NewInt(1), reader.GetTd(placeholder, 5))
	require.Equal(t, big.NewInt(1), reader.GetTd(parent.Hash(), 4))

	genesisPending := &types.Header{Number: new(big.Int), ParentHash: parent.Hash()}
	genesisReader := newSpeculativeChainReader(inner, genesisPending, placeholder)
	require.Equal(t, big.NewInt(1), genesisReader.GetTd(placeholder, 0))

	context := newSpeculativeChainContext(reader, nil)
	require.Nil(t, context.Engine())
}

func TestRebindReceiptsToSealedBlock(t *testing.T) {
	t.Parallel()

	sealed := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(42)})
	originalLogs := []*types.Log{
		{Address: common.HexToAddress("0x1"), Data: []byte{1}},
		{Address: common.HexToAddress("0x2"), Data: []byte{2}},
	}
	receipts := []*types.Receipt{
		{Logs: originalLogs},
		{Logs: []*types.Log{{Address: common.HexToAddress("0x3")}}},
	}

	bound, logs := rebindReceiptsToSealedBlock(receipts, sealed)
	require.Len(t, bound, len(receipts))
	require.Len(t, logs, 3)
	for index, receipt := range bound {
		require.NotSame(t, receipts[index], receipt)
		require.Equal(t, sealed.Hash(), receipt.BlockHash)
		require.Equal(t, sealed.Number(), receipt.BlockNumber)
		require.Equal(t, uint(index), receipt.TransactionIndex)
		for logIndex, entry := range receipt.Logs {
			require.NotSame(t, receipts[index].Logs[logIndex], entry)
			require.Equal(t, sealed.Hash(), entry.BlockHash)
		}
	}
}

func TestSealViaPrivateChannel(t *testing.T) {
	t.Parallel()

	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(3)})
	sealError := errors.New("cannot seal")

	tests := []struct {
		name       string
		seal       func(chan<- *consensus.NewSealedBlockEvent) error
		stopWorker bool
		wantBlock  *types.Block
		wantError  string
	}{
		{
			name: "success",
			seal: func(results chan<- *consensus.NewSealedBlockEvent) error {
				results <- &consensus.NewSealedBlockEvent{Block: block}
				return nil
			},
			wantBlock: block,
		},
		{
			name: "engine error",
			seal: func(chan<- *consensus.NewSealedBlockEvent) error {
				return sealError
			},
			wantError: "seal failed: cannot seal",
		},
		{
			name: "nil event",
			seal: func(results chan<- *consensus.NewSealedBlockEvent) error {
				results <- nil
				return nil
			},
			wantError: "nil sealed block from Seal",
		},
		{
			name: "nil block",
			seal: func(results chan<- *consensus.NewSealedBlockEvent) error {
				results <- &consensus.NewSealedBlockEvent{}
				return nil
			},
			wantError: "nil sealed block from Seal",
		},
		{
			name: "worker stopped",
			seal: func(chan<- *consensus.NewSealedBlockEvent) error {
				return nil
			},
			stopWorker: true,
			wantError:  "worker stopped during inline seal",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			exitCh := make(chan struct{})
			if test.stopWorker {
				close(exitCh)
			}
			engine := &pipelineSealEngine{
				seal: func(_ consensus.ChainHeaderReader, _ *types.Block, _ *stateless.Witness, results chan<- *consensus.NewSealedBlockEvent, _ <-chan struct{}) error {
					return test.seal(results)
				},
			}
			w := &worker{engine: engine, exitCh: exitCh}
			sealed, err := w.sealViaPrivateChannel(block)
			if test.wantError != "" {
				require.EqualError(t, err, test.wantError)
				require.Nil(t, sealed)
				return
			}
			require.NoError(t, err)
			require.Same(t, test.wantBlock, sealed)
		})
	}
}

func TestAnnounceInlineSealedBlock(t *testing.T) {
	t.Parallel()

	mux := new(event.TypeMux)
	sub := mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()
	received := make(chan *event.TypeMuxEvent, 2)
	go func() {
		received <- <-sub.Chan()
		received <- <-sub.Chan()
	}()

	w := &worker{mux: mux}
	block := types.NewBlockWithHeader(&types.Header{
		Number:     big.NewInt(7),
		GasUsed:    21_000,
		Time:       uint64(time.Now().Add(time.Second).Unix()),
		ActualTime: time.Now().Add(time.Second),
	})
	started := time.Now().Add(-time.Millisecond)
	w.announceInlineSealedBlock(block, started)

	select {
	case muxEvent := <-received:
		event, ok := muxEvent.Data.(core.NewMinedBlockEvent)
		require.True(t, ok)
		require.Same(t, block, event.Block)
		require.False(t, event.SealedAt.IsZero())
	case <-time.After(time.Second):
		t.Fatal("mined block event was not announced")
	}

	// A zero build start deliberately skips the build-to-announce timer path.
	w.announceInlineSealedBlock(block, time.Time{})
	require.NotNil(t, <-received)
}

func TestPipelineTimingAndChainHelpers(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	borEngine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer ctrl.Finish()
	defer borEngine.(*bor.Bor).Close()

	w, backend, cleanup := newTestWorker(t, DefaultTestConfig(), &chainConfig, borEngine, rawdb.NewMemoryDatabase(), false, 0)
	cleanup()

	parent := backend.chain.CurrentHeader()
	wrappedEngine := &pipelineSealEngine{
		Engine: borEngine,
		seal: func(_ consensus.ChainHeaderReader, _ *types.Block, _ *stateless.Witness, _ chan<- *consensus.NewSealedBlockEvent, _ <-chan struct{}) error {
			return nil
		},
	}
	w.engine = wrappedEngine

	t.Run("early announce uses parent time", func(t *testing.T) {
		header := &types.Header{
			ParentHash: parent.Hash(),
			Number:     new(big.Int).Add(parent.Number, common.Big1),
			ActualTime: time.Now().Add(time.Hour),
		}
		require.Equal(t, parent.GetActualTime(), w.earlyAnnounceTime(header))

		header.ParentHash = common.HexToHash("0xdead")
		require.Equal(t, header.GetActualTime(), w.earlyAnnounceTime(header))
		require.WithinDuration(t, time.Now(), w.earlyAnnounceTime(nil), time.Second)
		require.WithinDuration(t, time.Now(), w.earlyAnnounceTime(&types.Header{Number: new(big.Int)}), time.Second)
	})

	t.Run("wait for current chain head", func(t *testing.T) {
		session := &specSession{w: w}
		hash, ok := session.waitForChainHead(parent.Number.Uint64())
		require.True(t, ok)
		require.Equal(t, parent.Hash(), hash)
	})

	t.Run("wait for chain head stops with worker", func(t *testing.T) {
		exitCh := make(chan struct{})
		close(exitCh)
		stoppedWorker := &worker{chain: backend.chain, exitCh: exitCh}
		session := &specSession{w: stoppedWorker}
		hash, ok := session.waitForChainHead(parent.Number.Uint64() + 1)
		require.False(t, ok)
		require.Equal(t, common.Hash{}, hash)
		require.True(t, session.exitDuringBlockN)
	})

	t.Run("resolve grandparent prefers unwritten sealed header", func(t *testing.T) {
		lastSealed := &types.Header{Number: new(big.Int).Set(parent.Number)}
		session := &specSession{
			w:                w,
			blockNNumber:     parent.Number.Uint64(),
			lastSealedHeader: lastSealed,
		}
		require.Same(t, lastSealed, session.resolveGrandparent())
		session.lastSealedHeader = nil
		require.Equal(t, parent, session.resolveGrandparent())
		session.blockNNumber++
		require.Nil(t, session.resolveGrandparent())
	})

	t.Run("announce wait already elapsed", func(t *testing.T) {
		session := &specSession{w: w}
		header := &types.Header{
			Number:     big.NewInt(1),
			ActualTime: time.Now().Add(-time.Second),
		}
		require.False(t, session.waitForParentAnnounceTime(header, nil))
	})

	t.Run("announce wait drains on shutdown", func(t *testing.T) {
		exitCh := make(chan struct{})
		close(exitCh)
		stoppedWorker := &worker{
			chain:  backend.chain,
			engine: wrappedEngine,
			exitCh: exitCh,
		}
		fillDone := make(chan struct{})
		writeDone := make(chan struct{})
		close(fillDone)
		close(writeDone)
		session := &specSession{w: stoppedWorker, prevDBWriteDone: writeDone}
		header := &types.Header{
			Number:     big.NewInt(1),
			ActualTime: time.Now().Add(time.Hour),
		}
		require.True(t, session.waitForParentAnnounceTime(header, fillDone))
	})

	t.Run("pipeline retry queues one new build", func(t *testing.T) {
		retryWorker := &worker{
			chain:     backend.chain,
			exitCh:    make(chan struct{}),
			newWorkCh: make(chan *newWorkReq, 1),
		}
		retryWorker.schedulePipelineRetry()
		select {
		case request := <-retryWorker.newWorkCh:
			require.NotZero(t, request.timestamp)
			require.Equal(t, uint64(1), retryWorker.pendingWorkBlock.Load())
		case <-time.After(time.Second):
			t.Fatal("pipeline retry was not queued")
		}

		retryWorker.pendingWorkBlock.Store(1)
		retryWorker.schedulePipelineRetry()
		require.Never(t, func() bool {
			return len(retryWorker.newWorkCh) != 0
		}, 75*time.Millisecond, 5*time.Millisecond)
	})

	t.Run("pipeline retry stops with worker", func(t *testing.T) {
		exitCh := make(chan struct{})
		close(exitCh)
		retryWorker := &worker{
			chain:     backend.chain,
			exitCh:    exitCh,
			newWorkCh: make(chan *newWorkReq, 1),
		}
		retryWorker.schedulePipelineRetry()
		require.Never(t, func() bool {
			return len(retryWorker.newWorkCh) != 0
		}, 75*time.Millisecond, 5*time.Millisecond)
	})
}

func TestWorkerConfigurationAndInterruptHelpers(t *testing.T) {
	t.Parallel()

	config := DefaultTestConfig()
	exitCh := make(chan struct{})
	recommitCh := make(chan time.Duration, 1)
	w := &worker{
		config:             config,
		exitCh:             exitCh,
		resubmitIntervalCh: recommitCh,
	}

	w.setGasCeil(12_345_678)
	require.Equal(t, uint64(12_345_678), config.GasCeil)
	w.setGasTip(big.NewInt(987))
	require.Equal(t, big.NewInt(987), w.tip.ToBig())
	prioritized := []common.Address{common.HexToAddress("0x1"), common.HexToAddress("0x2")}
	w.setPrio(prioritized)
	require.Equal(t, prioritized, w.prio)

	w.setRecommitInterval(150 * time.Millisecond)
	require.Equal(t, 150*time.Millisecond, <-recommitCh)
	close(exitCh)
	w.setRecommitInterval(time.Second)

	var nilInterrupt *buildInterruptState
	require.Nil(t, nilInterrupt.timeoutFlag())
	require.Nil(t, nilInterrupt.flagSetAtPtr())
	interrupt := newBuildInterruptState()
	require.Same(t, &interrupt.timedOut, interrupt.timeoutFlag())
	require.Same(t, &interrupt.flagSetAt, interrupt.flagSetAtPtr())

	timeout, setAt := w.interruptStateForEnv(nil)
	require.Same(t, &w.interruptBlockBuilding, timeout)
	require.Same(t, &w.interruptFlagSetAt, setAt)

	require.ErrorIs(t, signalToErr(commitInterruptNewHead), errBlockInterruptedByNewHead)
	require.ErrorIs(t, signalToErr(commitInterruptResubmit), errBlockInterruptedByRecommit)
	require.ErrorIs(t, signalToErr(commitInterruptTimeout), errBlockInterruptedByTimeout)
	require.Panics(t, func() { signalToErr(999) })
}

func TestPipelinedHeaderConstruction(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer ctrl.Finish()
	defer engine.(*bor.Bor).Close()

	w, backend, cleanup := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer cleanup()

	parent := backend.chain.CurrentHeader()
	req := &speculativeWorkReq{parentHeader: parent}
	session := newSpecSession(w, req)
	reader, context, header, coinbase := session.buildInitialSpecHeader()

	require.Equal(t, placeholderParentHash(parent.Number.Uint64()), header.ParentHash)
	require.Equal(t, parent.Number.Uint64()+1, header.Number.Uint64())
	require.Equal(t, parent.Time+chainConfig.Bor.CalculatePeriod(header.Number.Uint64()), header.Time)
	require.Equal(t, coinbase, header.Coinbase)
	require.Equal(t, parent, reader.GetHeader(header.ParentHash, parent.Number.Uint64()))
	require.Same(t, engine, context.Engine())

	fallback := common.HexToAddress("0x9999")
	require.Equal(t, fallback, w.resolveCoinbase(header.Number.Uint64(), fallback))

	withoutBor := &worker{chainConfig: &params.ChainConfig{}}
	require.Equal(t, fallback, withoutBor.resolveCoinbase(1, fallback))
}

func TestSpecSessionSetupAndEnvironmentHelpers(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer ctrl.Finish()
	defer engine.(*bor.Bor).Close()

	w, backend, cleanup := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer cleanup()

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

	pipelineWorker := &worker{
		config:            w.config,
		chainConfig:       &chainConfig,
		engine:            engine,
		eth:               backend,
		chain:             backend.chain,
		exitCh:            make(chan struct{}),
		taskCh:            make(chan *task, 1),
		speculativeWorkCh: make(chan *speculativeWorkReq, 1),
	}
	pipelineWorker.setEtherbase(testBankAddress)
	pipelineWorker.running.Store(true)
	require.NoError(t, pipelineWorker.commitPipelined(work, time.Now()))
	req := <-pipelineWorker.speculativeWorkCh

	session := newSpecSession(pipelineWorker, req)
	require.False(t, session.setupInitial(), "Bor Prepare rejects the placeholder-parent header")
	require.NotNil(t, session.borEngine)
	select {
	case fallback := <-pipelineWorker.taskCh:
		require.Equal(t, req.parentHeader.Number, fallback.block.Number())
		require.Equal(t, req.parentHeader.ParentHash, fallback.block.ParentHash())
	case <-time.After(time.Second):
		t.Fatal("setup failure did not hand the block to sequential sealing")
	}

	reader, context, header, coinbase := session.buildInitialSpecHeader()
	header.Difficulty = common.Big1
	specState, err := backend.chain.StateAtWithFlatDiff(req.parentRoot, req.flatDiff)
	require.NoError(t, err)
	session.blockhashAccessed = new(atomic.Bool)
	expectedHash := common.HexToHash("0x1234")
	env := session.buildSpecEnv(header, specState, coinbase, context, parent, parent.Number.Uint64(), func() common.Hash {
		return expectedHash
	})
	require.Equal(t, header.Number, env.header.Number)
	require.Equal(t, coinbase, env.coinbase)
	require.NotNil(t, env.evm)
	require.NotNil(t, env.buildInterrupt)
	require.Equal(t, parent, reader.GetHeader(parent.Hash(), parent.Number.Uint64()))

	session.specHeader = header
	session.specState = specState
	session.specEnv = env
	session.resetTxPoolState(req.parentHeader, req.parentRoot, req.flatDiff)
	session.startInitialFillGoroutine()
	<-session.initialFillDone

	pipelineWorker.spawnSRCForFinalBlock(req.parentHeader, req.parentRoot, req.flatDiff, true)
	resolver := session.newBlockNHashResolver()
	first := resolver()
	require.NotEqual(t, common.Hash{}, first)
	require.Equal(t, first, resolver(), "the SRC result should be memoized")

	session.borEngine = engine.(*bor.Bor)
	session.realBlockNHash = first
	session.rootN = req.parentRoot
	session.specHeader = header
	session.specState = specState
	session.specEnv = env
	finalHeader, flatDiff, stateSyncData, ok := session.finalizeCurrent()
	require.True(t, ok)
	require.NotNil(t, finalHeader)
	require.NotNil(t, flatDiff)
	require.Nil(t, stateSyncData)
	require.Equal(t, first, finalHeader.ParentHash)

	session.spawnSRCForCurrent(finalHeader, flatDiff)
	nextResolver := session.makeNextHashResolver(finalHeader)
	nextHash := nextResolver()
	require.NotEqual(t, common.Hash{}, nextHash)
	require.Equal(t, nextHash, nextResolver(), "mid-pipeline resolver should memoize the SRC hash")

	fillDone, abort, elapsed := session.startNextFillGoroutine(header, env, specState)
	<-fillDone
	require.False(t, *abort)
	require.GreaterOrEqual(t, *elapsed, time.Duration(0))
}

func TestCommitPipelinedBuildsSpeculativeRequest(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer ctrl.Finish()
	defer engine.(*bor.Bor).Close()

	w, backend, cleanup := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer cleanup()

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

	pipelineWorker := &worker{
		chainConfig:       &chainConfig,
		engine:            engine,
		chain:             backend.chain,
		exitCh:            make(chan struct{}),
		speculativeWorkCh: make(chan *speculativeWorkReq, 1),
	}
	pipelineWorker.running.Store(true)
	require.NoError(t, pipelineWorker.commitPipelined(work, time.Now()))

	select {
	case req := <-pipelineWorker.speculativeWorkCh:
		require.Equal(t, work.header, req.parentHeader)
		require.NotNil(t, req.flatDiff)
		require.Equal(t, parent.Root, req.parentRoot)
		require.NotSame(t, work, req.blockNEnv)
		require.Equal(t, work.header, req.blockNEnv.header)
	case <-time.After(time.Second):
		t.Fatal("commitPipelined did not publish a speculative request")
	}
}

func TestPipelinedRequestCompletesSingleIteration(t *testing.T) {
	chainConfig := *params.BorUnittestChainConfig
	borConfig := *chainConfig.Bor
	borConfig.RioBlock = big.NewInt(0)
	chainConfig.Bor = &borConfig
	engine, ctrl := getFakeBorFromConfig(t, &chainConfig)
	defer ctrl.Finish()
	defer engine.(*bor.Bor).Close()

	w, backend, cleanup := newTestWorker(t, DefaultTestConfig(), &chainConfig, engine, rawdb.NewMemoryDatabase(), false, 0)
	defer cleanup()

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

	w.running.Store(true)
	require.NoError(t, w.commitPipelined(work, time.Now()))
	require.Eventually(t, func() bool {
		return backend.chain.CurrentBlock().Number.Uint64() >= 1
	}, 10*time.Second, 20*time.Millisecond)
	w.stop()
}
