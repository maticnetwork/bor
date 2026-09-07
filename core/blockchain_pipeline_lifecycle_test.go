package core

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader/whitelist"
)

func TestPipelinedTrieGCAndSlowLogs(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	root := chain.CurrentBlock().Root

	chain.handleImportTrieGC(root, 0, time.Millisecond)
	chain.cfg.ArchiveMode = true
	chain.handleImportTrieGC(root, 1, time.Millisecond)
	chain.cfg.ArchiveMode = false
	chain.SetTrieFlushInterval(0)
	chain.gcproc = time.Second
	chain.maybeFlushChosen(0, chain.cfg.GetTriesInMemory())
	chain.maybeFlushChosen(999, chain.cfg.GetTriesInMemory())

	chain.triegc.Push(common.HexToHash("0x1"), -1)
	chain.triegc.Push(common.HexToHash("0x2"), -2)
	chain.dereferenceUpTo(1)
	require.False(t, chain.triegc.Empty())
	chain.dereferenceUpTo(2)
	require.True(t, chain.triegc.Empty())
	chain.handleImportTrieGC(root, chain.cfg.GetTriesInMemory()+1, time.Millisecond)

	statedb, err := chain.StateAt(root)
	require.NoError(t, err)
	timings := pipelinedImportPersistTimings{
		total:           slowImportPostExecThreshold,
		collect:         slowImportCollectThreshold,
		prefetchDetach:  slowImportSnapshotThreshold,
		residual:        slowImportResidualThreshold,
		witnessCapture:  time.Millisecond,
		prefetchCleanup: time.Millisecond,
		commitSnapshot:  time.Millisecond,
		collectTotal:    time.Millisecond,
		stateSyncFeed:   time.Millisecond,
		reorgCheck:      time.Millisecond,
		setFlatDiff:     time.Millisecond,
		writeHead:       time.Millisecond,
		buildSRCBlock:   time.Millisecond,
		spawnSRC:        time.Millisecond,
		pendingPublish:  time.Millisecond,
	}
	chain.logSlowPipelinedImport(blocks[0], slowImportBlockThreshold, time.Millisecond, time.Millisecond, timings, statedb)
	chain.logSlowNormalImport(
		blocks[0],
		time.Millisecond,
		time.Millisecond,
		time.Millisecond,
		slowImportPostExecThreshold,
		slowImportBlockThreshold,
		statedb,
	)
}

func TestPipelineImportAutoCollectionFailures(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)

	t.Run("SRC error", func(t *testing.T) {
		collected := make(chan struct{})
		pending := &pendingImportSRCState{
			block:       blocks[0],
			src:         &pendingSRCState{err: errors.New("src failed")},
			collectedCh: collected,
		}
		chain.wg.Add(1)
		chain.runImportAutoCollection(pending)
		require.EqualError(t, pending.collectedErr, "src failed")
		require.NotPanics(t, func() { <-collected })
	})

	t.Run("root mismatch", func(t *testing.T) {
		collected := make(chan struct{})
		pending := &pendingImportSRCState{
			block:       blocks[0],
			src:         &pendingSRCState{root: common.HexToHash("0xdead")},
			collectedCh: collected,
		}
		chain.wg.Add(1)
		chain.runImportAutoCollection(pending)
		require.ErrorContains(t, pending.collectedErr, "root mismatch")
		require.NotPanics(t, func() { <-collected })
	})
}

func TestEmitQueuedStateSyncFeed(t *testing.T) {
	chain, _, _ := newPipelineHelperChain(t)
	events := make(chan StateSyncEvent, 1)
	sub := chain.SubscribeStateSyncEvent(events)
	defer sub.Unsubscribe()

	data := &types.StateSyncData{ID: 99}
	chain.SetStateSync([]*types.StateSyncData{data})
	chain.emitStateSyncFeed()
	require.Same(t, data, (<-events).Data)
}

func TestGetStateSyncLockedWithQueuedWriter(t *testing.T) {
	chain, _, _ := newPipelineHelperChain(t)
	data := &types.StateSyncData{ID: 99}
	chain.SetStateSync([]*types.StateSyncData{data})
	require.Equal(t, []*types.StateSyncData{data}, chain.GetStateSync())

	chain.stateSyncMu.RLock()
	lockHeld := true
	defer func() {
		if lockHeld {
			chain.stateSyncMu.RUnlock()
		}
	}()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		chain.SetStateSync(nil)
	}()
	require.Eventually(t, func() bool {
		if chain.stateSyncMu.TryRLock() {
			chain.stateSyncMu.RUnlock()
			return false
		}
		return true
	}, 5*time.Second, time.Millisecond, "writer did not wait for state sync lock")

	readDone := make(chan []*types.StateSyncData, 1)
	go func() { readDone <- chain.getStateSyncLocked() }()
	select {
	case got := <-readDone:
		require.Equal(t, []*types.StateSyncData{data}, got)
	case <-time.After(5 * time.Second):
		chain.stateSyncMu.RUnlock()
		lockHeld = false
		<-writerDone
		t.Fatal("lock-held state sync read blocked behind a queued writer")
	}
	chain.stateSyncMu.RUnlock()
	lockHeld = false
	<-writerDone
}

func TestRunSRCComputeFailureBranches(t *testing.T) {
	t.Run("panic is converted to an SRC error", func(t *testing.T) {
		chain, _, blocks := newPipelineHelperChain(t)
		chain.srcHoldForTesting = func(uint64) { panic("boom") }
		pending := new(pendingSRCState)
		pending.wg.Add(1)
		chain.wg.Add(1)

		chain.runSRCCompute(pending, blocks[0], chain.CurrentBlock().Root, new(state.FlatDiff), false, nil, false, nil, false)
		require.ErrorContains(t, pending.err, "SRC goroutine panicked: boom")
	})

	t.Run("missing parent state is reported", func(t *testing.T) {
		chain, _, blocks := newPipelineHelperChain(t)
		pending := new(pendingSRCState)
		pending.wg.Add(1)
		chain.wg.Add(1)

		chain.runSRCCompute(pending, blocks[0], common.HexToHash("0xdead"), new(state.FlatDiff), false, nil, false, nil, false)
		require.Error(t, pending.err)
	})
}

func TestPipelinePersistFailureBranches(t *testing.T) {
	newState := func(t *testing.T, chain *BlockChain) *state.StateDB {
		t.Helper()
		statedb, err := chain.StateAt(chain.CurrentBlock().Root)
		require.NoError(t, err)
		statedb.StartPrefetcher("pipeline-failure-test", nil, nil)
		return statedb
	}

	t.Run("previous SRC collection error adjusts iterator", func(t *testing.T) {
		chain, _, blocks := newPipelineHelperChain(t)
		collected := make(chan struct{})
		close(collected)
		chain.pendingImportSRC = &pendingImportSRCState{
			block:        blocks[0],
			collectedCh:  collected,
			collectedErr: errors.New("collect failed"),
		}

		adjustBack, err := chain.persistPipelinedImport(
			blocks[1], blocks[0].Header(), newState(t, chain), nil, nil, time.Now(), 0, 0, false,
		)
		require.True(t, adjustBack)
		require.EqualError(t, err, "collect failed")
	})

	t.Run("reorg validator error is returned", func(t *testing.T) {
		chain, _, blocks := newPipelineHelperChain(t)
		validateErr := errors.New("validator failed")
		chain.forker = NewForkChoice(chain, nil, newChainValidatorFake(func(*types.Header, []*types.Header) (bool, error) {
			return false, validateErr
		}))

		adjustBack, err := chain.persistPipelinedImport(
			blocks[0], chain.CurrentHeader(), newState(t, chain), nil, nil, time.Now(), 0, 0, false,
		)
		require.False(t, adjustBack)
		require.ErrorIs(t, err, validateErr)
	})

	t.Run("reorg mismatch is returned", func(t *testing.T) {
		chain, _, blocks := newPipelineHelperChain(t)
		chain.forker = NewForkChoice(chain, nil, newChainValidatorFake(func(*types.Header, []*types.Header) (bool, error) {
			return false, nil
		}))

		adjustBack, err := chain.persistPipelinedImport(
			blocks[0], chain.CurrentHeader(), newState(t, chain), nil, nil, time.Now(), 0, 0, false,
		)
		require.False(t, adjustBack)
		require.ErrorIs(t, err, whitelist.ErrMismatch)
	})

	t.Run("post-write fork-choice error clears pending head", func(t *testing.T) {
		chain, _, blocks := newPipelineHelperChain(t)
		chain.forker = NewForkChoice(newChainReaderFake(func(common.Hash, uint64) *big.Int {
			return nil
		}), nil, nil)

		adjustBack, err := chain.persistPipelinedImport(
			blocks[0], chain.CurrentHeader(), newState(t, chain), nil, nil, time.Now(), 0, 0, false,
		)
		require.False(t, adjustBack)
		require.ErrorContains(t, err, "missing td")
		require.Equal(t, common.Hash{}, chain.pendingImportHeadHash)
	})
}

func TestPipelineForkChoiceAndFlushFailureBranches(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	chain.forker = NewForkChoice(newChainReaderFake(func(common.Hash, uint64) *big.Int {
		return nil
	}), nil, nil)
	status, err := chain.resolvePostWriteStatus(blocks[0], false)
	require.Equal(t, NonStatTy, status)
	require.ErrorContains(t, err, "missing td")

	collected := make(chan struct{})
	close(collected)
	chain.pendingImportSRC = &pendingImportSRCState{
		block:        blocks[0],
		collectedCh:  collected,
		collectedErr: errors.New("flush failed"),
	}
	mismatch := types.NewBlockWithHeader(&types.Header{
		ParentHash: common.HexToHash("0xdead"),
		Number:     big.NewInt(2),
	})
	opts := chain.buildPipelineImportOpts(mismatch, chain.CurrentHeader())
	require.Equal(t, pipelineImportModeDirect, opts.Mode)
	require.Nil(t, chain.pendingImportSRC)
}

func TestCapTrieWithZeroDirtyLimit(t *testing.T) {
	chain, _, _ := newPipelineHelperChain(t)
	chain.cfg.TrieDirtyLimit = 0
	require.NotPanics(t, chain.capTrieIfDirty)
}
