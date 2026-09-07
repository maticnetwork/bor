package core

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/params"
)

func newPipelineHelperChain(t *testing.T) (*BlockChain, *Genesis, []*types.Block) {
	t.Helper()

	genesis := &Genesis{
		Config:  params.AllEthashProtocolChanges,
		BaseFee: big.NewInt(params.InitialBaseFee),
	}
	engine := ethash.NewFaker()
	_, blocks, _ := GenerateChainWithGenesis(genesis, engine, 2, nil)
	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, pipelinedConfig(rawdb.HashScheme))
	require.NoError(t, err)
	t.Cleanup(chain.Stop)
	return chain, genesis, blocks
}

func TestPipelinedStateAccessHelpers(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	genesis := chain.CurrentBlock()
	address := common.HexToAddress("0x1234")
	diff := &state.FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			address: {
				Nonce:    7,
				Balance:  uint256.NewInt(11),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
	}

	chain.SetLastFlatDiff(diff, 1, genesis.Root, blocks[0].Root())
	require.Same(t, diff, chain.GetLastFlatDiff())

	overlay, err := chain.PostExecState(blocks[0].Header())
	require.NoError(t, err)
	require.Equal(t, uint64(7), overlay.GetNonce(address))

	committed, err := chain.PostExecState(genesis)
	require.NoError(t, err)
	require.Zero(t, committed.GetNonce(address))

	opened, err := chain.StateAtWithFlatDiff(genesis.Root, diff)
	require.NoError(t, err)
	require.Equal(t, uint64(7), opened.GetNonce(address))
}

func TestPendingSRCStateHelpers(t *testing.T) {
	root := common.HexToHash("0x1234")
	witness := []byte{1, 2, 3}

	_, _, err := waitForSRCState(nil)
	require.EqualError(t, err, "no pending SRC goroutine")

	pendingErr := &pendingSRCState{err: errors.New("src failed")}
	_, _, err = waitForSRCState(pendingErr)
	require.EqualError(t, err, "src failed")

	pending := &pendingSRCState{root: root, witness: witness}
	gotRoot, gotWitness, err := waitForSRCState(pending)
	require.NoError(t, err)
	require.Equal(t, root, gotRoot)
	require.Equal(t, witness, gotWitness)

	chain, _, _ := newPipelineHelperChain(t)
	chain.pendingSRC = pending
	gotRoot, gotWitness, err = chain.WaitForSRC()
	require.NoError(t, err)
	require.Equal(t, root, gotRoot)
	require.Equal(t, witness, gotWitness)
}

func TestPipelineImportModeAndOverlapGuardBranches(t *testing.T) {
	require.Equal(t, "disabled", pipelineImportMode(nil))
	require.Equal(t, pipelineImportModeDirect, pipelineImportMode(&PipelineImportOpts{}))
	require.Equal(t, pipelineImportModeFlatDiff, pipelineImportMode(&PipelineImportOpts{FlatDiff: new(state.FlatDiff)}))
	require.Equal(t, "custom", pipelineImportMode(&PipelineImportOpts{Mode: "custom"}))

	require.False(t, pendingImportSRCCollected(nil))
	open := &pendingImportSRCState{collectedCh: make(chan struct{})}
	require.False(t, pendingImportSRCCollected(open))
	close(open.collectedCh)
	require.True(t, pendingImportSRCCollected(open))

	now := time.Now()
	var nilSRC *pendingSRCState
	nilSRC.markStarted(now)
	nilSRC.markDone(now)
	require.Zero(t, nilSRC.executionOverlap(now, now.Add(time.Second)))

	src := new(pendingSRCState)
	require.Zero(t, src.executionOverlap(time.Time{}, now))
	require.Zero(t, src.executionOverlap(now, now))
	recordPipelinedImportSRCOverlapSplit(nil)
	recordPipelinedImportSRCOverlapSplit(src)

	src.nextExecClassified.Store(true)
	src.startNanos.Store(now.UnixNano())
	src.doneNanos.Store(now.Add(-time.Second).UnixNano())
	recordPipelinedImportSRCOverlapSplit(src)
}

func TestPendingImportCollectionHelpers(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)

	root, err := chain.collectPendingImportSRC()
	require.EqualError(t, err, "no pending import SRC")
	require.Equal(t, common.Hash{}, root)
	require.NoError(t, chain.flushPendingImportSRC(false))

	collected := make(chan struct{})
	close(collected)
	pending := &pendingImportSRCState{
		block:         blocks[0],
		collectedCh:   collected,
		collectedRoot: blocks[0].Root(),
	}
	chain.pendingImportSRC = pending
	root, err = chain.collectPendingImportSRC()
	require.NoError(t, err)
	require.Equal(t, blocks[0].Root(), root)

	// A failed collect surfaces the error and drops the pending entry as part
	// of the rollback: leaving it in place made every later insert re-collect
	// the same failure, so the node stopped following the chain. A follow-up
	// flush therefore finds nothing pending.
	pending.collectedErr = errors.New("collect failed")
	_, err = chain.collectPendingImportSRC()
	require.EqualError(t, err, "collect failed")
	require.Nil(t, chain.pendingImportSRC)
	require.NoError(t, chain.flushPendingImportSRC(false))

	// flush still surfaces the error when it is the one collecting; without
	// rollback (the shutdown shape) it only clears the pending entry.
	chain.pendingImportSRC = &pendingImportSRCState{
		block:        blocks[0],
		collectedCh:  collected,
		collectedErr: errors.New("collect failed"),
	}
	require.EqualError(t, chain.flushPendingImportSRC(false), "collect failed")
	require.Nil(t, chain.pendingImportSRC)
}

// TestSealingStateNeverServesOverlay pins the block-production state
// contract: StateAt serves the FlatDiff overlay during the pipelined window
// (RPC readers need the head's post-state), but the sealing accessors must
// refuse it — an overlay statedb is rooted at the grandparent with the
// parent's writes unjournaled, so a header root computed over it omits the
// parent block's state changes and every importer rejects the sealed block.
func TestSealingStateNeverServesOverlay(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	genesis := chain.CurrentBlock()
	addr := common.HexToAddress("0x5ea1")
	diff := &state.FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			addr: {
				Nonce:    7,
				Balance:  uint256.NewInt(0),
				Root:     types.EmptyRootHash,
				CodeHash: types.EmptyCodeHash.Bytes(),
			},
		},
	}
	// Pretend blocks[0] was pipeline-imported: its root is head but not
	// committed; the overlay carries its post-state.
	chain.SetLastFlatDiff(diff, blocks[0].NumberU64(), genesis.Root, blocks[0].Root())

	sdb, err := chain.StateAt(blocks[0].Root())
	require.NoError(t, err)
	require.Equal(t, uint64(7), sdb.GetNonce(addr), "RPC-style reads serve the overlay")

	_, err = chain.SealingStateAt(blocks[0].Root())
	require.Error(t, err, "sealing must not fall back to the overlay for an uncommitted root")
	_, _, _, _, err = chain.SealingStateAtWithReaders(blocks[0].Root())
	require.Error(t, err, "sealing readers must not fall back to the overlay for an uncommitted root")

	sdb, err = chain.SealingStateAt(genesis.Root)
	require.NoError(t, err)
	require.Zero(t, sdb.GetNonce(addr))
	st, throwaway, prefetchReader, processReader, err := chain.SealingStateAtWithReaders(genesis.Root)
	require.NoError(t, err)
	require.NotNil(t, throwaway)
	require.NotNil(t, prefetchReader)
	require.NotNil(t, processReader)
	require.Zero(t, st.GetNonce(addr))
}

// TestFlushPendingImportSRCRollsBack pins that the flush path performs the
// same rollback as the collect path when asked to: reorg/gap and
// ProcessBlock-error flushes hold chainmu and continue (or abort) around a
// head that was published before verification, so a failed collection must
// not leave the rejected block canonical there.
func TestFlushPendingImportSRCRollsBack(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	_, err := chain.InsertChain(blocks, false)
	require.NoError(t, err)
	require.Equal(t, blocks[1].Hash(), chain.CurrentBlock().Hash())

	collected := make(chan struct{})
	close(collected)
	chain.SetLastFlatDiff(&state.FlatDiff{}, blocks[1].NumberU64(), blocks[0].Root(), blocks[1].Root())
	chain.pendingImportSRC = &pendingImportSRCState{
		block:        blocks[1],
		collectedCh:  collected,
		collectedErr: errors.New("collect failed"),
	}

	require.EqualError(t, chain.flushPendingImportSRC(true), "collect failed")
	require.Nil(t, chain.pendingImportSRC)
	require.Equal(t, blocks[0].Hash(), chain.CurrentBlock().Hash(),
		"head must move back to the rejected block's parent")
	require.Equal(t, common.Hash{}, rawdb.ReadCanonicalHash(chain.db, blocks[1].NumberU64()),
		"rejected block must lose its canonical hash")
	require.Nil(t, chain.GetLastFlatDiff(),
		"rejected block's post-state overlay must stop being served")
}

func TestPipelinedHeadMarkerAndWait(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	block := blocks[0]

	chain.markPendingImportHeadState(block)
	require.Equal(t, block.Hash(), chain.pendingImportHeadHash)
	require.Equal(t, block.Root(), chain.pendingImportHeadRoot)
	require.False(t, chain.pendingImportHeadStart.IsZero())

	chain.clearPendingImportHeadState(blocks[1])
	require.Equal(t, block.Hash(), chain.pendingImportHeadHash)
	chain.clearPendingImportHeadState(block)
	require.Equal(t, common.Hash{}, chain.pendingImportHeadHash)
	require.Equal(t, common.Hash{}, chain.pendingImportHeadRoot)
	require.True(t, chain.pendingImportHeadStart.IsZero())

	require.NoError(t, chain.WaitForPipelinedStateCommit(context.Background(), block.Root()))

	collected := make(chan struct{})
	chain.pendingImportSRC = &pendingImportSRCState{block: block, collectedCh: collected}
	require.NoError(t, chain.WaitForPipelinedStateCommit(context.Background(), blocks[1].Root()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, chain.WaitForPipelinedStateCommit(ctx, block.Root()), context.Canceled)

	close(collected)
	require.NoError(t, chain.WaitForPipelinedStateCommit(context.Background(), block.Root()))

	chain.pendingImportSRC = &pendingImportSRCState{
		block:       block,
		blockStart:  time.Now(),
		collectedCh: make(chan struct{}),
	}
	require.False(t, chain.HasRecentPipelinedHeadState(common.HexToHash("0xdead"), block.Root()))
	chain.pendingImportSRC = nil
}

func TestPipelinedWitnessWaitAndReaderOverlays(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	block := blocks[0]
	chain.pipelinedMakeWitness.Store(true)

	require.Nil(t, chain.waitForPipelinedWitness(common.HexToHash("0xdead")))
	require.Nil(t, chain.pollWitnessCache(common.HexToHash("0xbeef"), time.Millisecond, time.Millisecond))

	pollHash := chain.CurrentBlock().Hash()
	go func() {
		time.Sleep(5 * time.Millisecond)
		chain.CacheWitness(pollHash, []byte{1, 2})
	}()
	require.Equal(t, []byte{1, 2}, chain.waitForPipelinedWitness(pollHash))
	require.Equal(t, []byte{1, 2}, chain.GetWitnessUncachedWait(pollHash))
	chain.pipelinedMakeWitness.Store(false)
	require.Nil(t, chain.GetWitnessUncachedWait(common.HexToHash("0xabcd")))

	collected := make(chan struct{})
	close(collected)
	chain.pendingImportSRC = &pendingImportSRCState{
		block:       block,
		makeWitness: true,
		collectedCh: collected,
	}
	chain.WriteWitness(block.Hash(), []byte{3, 4})
	chain.witnessCache.Purge()
	witness, matched := chain.waitForPendingSRCWitness(block.Hash())
	require.True(t, matched)
	require.Equal(t, []byte{3, 4}, witness)

	chain.pendingImportSRC.makeWitness = false
	witness, matched = chain.waitForPendingSRCWitness(block.Hash())
	require.True(t, matched)
	require.Nil(t, witness)
	chain.pendingImportSRC = nil

	parentRoot := chain.CurrentBlock().Root
	address := common.HexToAddress("0x1234")
	diff := &state.FlatDiff{Accounts: map[common.Address]types.StateAccount{
		address: {Nonce: 11, Balance: uint256.NewInt(1)},
	}}
	missingBlockRoot := common.HexToHash("0xcafe")
	chain.SetLastFlatDiff(diff, 1, parentRoot, missingBlockRoot)

	overlay, _, _, _, err := chain.StateAtWithReaders(missingBlockRoot)
	require.NoError(t, err)
	require.Equal(t, uint64(11), overlay.GetNonce(address))

	chain.SetLastFlatDiff(diff, 1, common.HexToHash("0xbad"), missingBlockRoot)
	_, err = chain.StateAt(missingBlockRoot)
	require.Error(t, err)
}

func TestBuildPipelineImportOptions(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	parent := chain.CurrentHeader()

	direct := chain.buildPipelineImportOpts(blocks[0], parent)
	require.Equal(t, pipelineImportModeDirect, direct.Mode)
	require.Equal(t, parent.Root, direct.CommittedParentRoot)

	collected := make(chan struct{})
	close(collected)
	pending := &pendingImportSRCState{
		block:         blocks[0],
		flatDiff:      &state.FlatDiff{},
		committedRoot: parent.Root,
		src:           &pendingSRCState{},
		collectedCh:   collected,
	}
	chain.pendingImportSRC = pending
	hit := chain.buildPipelineImportOpts(blocks[1], blocks[0].Header())
	require.Equal(t, pipelineImportModeFlatDiff, hit.Mode)
	require.Equal(t, blocks[0].Hash(), hit.PendingHash)
	require.True(t, hit.PendingCollected)

	chain.pendingImportSRC = pending
	mismatch := types.NewBlockWithHeader(&types.Header{
		ParentHash: common.HexToHash("0xbeef"),
		Number:     big.NewInt(2),
	})
	direct = chain.buildPipelineImportOpts(mismatch, parent)
	require.Equal(t, pipelineImportModeDirect, direct.Mode)
	require.Nil(t, chain.pendingImportSRC)
}

func TestPipelinedWitnessPublicationAndRootVerification(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	pending := &pendingImportSRCState{block: blocks[0]}

	require.True(t, chain.verifyImportSRCRoot(pending, blocks[0].Root()))
	chain.publishImportWitness(pending, nil)

	events := make(chan WitnessReadyEvent, 1)
	sub := chain.SubscribeWitnessReadyEvent(events)
	defer sub.Unsubscribe()

	witness := []byte{1, 2, 3}
	chain.publishImportWitness(pending, witness)
	require.Equal(t, witness, chain.GetWitness(blocks[0].Hash()))
	select {
	case event := <-events:
		require.Equal(t, blocks[0].Hash(), event.BlockHash)
		require.Equal(t, blocks[0].NumberU64(), event.BlockNumber)
	case <-time.After(time.Second):
		t.Fatal("witness-ready event was not published")
	}

	require.False(t, chain.verifyImportSRCRoot(pending, common.HexToHash("0xdead")))
	require.ErrorContains(t, pending.collectedErr, "root mismatch")
	require.Equal(t, uint64(0), chain.CurrentBlock().Number.Uint64())
}

func TestPipelinedBlockWritePaths(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	statedb, err := chain.StateAt(chain.CurrentBlock().Root)
	require.NoError(t, err)

	status, err := chain.WriteBlockAndSetHeadPipelined(blocks[0], nil, nil, statedb, false, nil)
	require.NoError(t, err)
	require.Equal(t, CanonStatTy, status)
	require.Equal(t, blocks[0].Hash(), chain.CurrentBlock().Hash())

	closedChain := &BlockChain{chainmu: syncx.NewClosableMutex()}
	closedChain.chainmu.Close()
	status, err = closedChain.WriteBlockAndSetHeadPipelined(nil, nil, nil, nil, false, nil)
	require.ErrorIs(t, err, errChainStopped)
	require.Equal(t, NonStatTy, status)

	unknown := types.NewBlockWithHeader(&types.Header{
		ParentHash: common.HexToHash("0xdead"),
		Number:     big.NewInt(3),
		Difficulty: common.Big1,
	})
	status, _, err = chain.writePipelinedBlockAndResolveStatus(unknown, nil, nil, statedb, nil)
	require.ErrorIs(t, err, consensus.ErrUnknownAncestor)
	require.Equal(t, NonStatTy, status)

	status, err = chain.writeBlockAndSetHeadPipelined(unknown, nil, nil, statedb, false, nil)
	require.ErrorIs(t, err, consensus.ErrUnknownAncestor)
	require.Equal(t, NonStatTy, status)
}

func TestPipelinedBlockBatchStoresWitness(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	statedb, err := chain.StateAt(chain.CurrentBlock().Root)
	require.NoError(t, err)

	witness := []byte{1, 2, 3, 4}
	status, err := chain.WriteBlockAndSetHeadPipelined(blocks[0], nil, nil, statedb, false, witness)
	require.NoError(t, err)
	require.Equal(t, CanonStatTy, status)
	require.Equal(t, witness, chain.GetWitness(blocks[0].Hash()))
}

func TestPipelinedStateSyncLogsAndEvents(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	statedb, err := chain.StateAt(chain.CurrentBlock().Root)
	require.NoError(t, err)

	first := &types.Log{Address: common.HexToAddress("0x1"), Index: 0}
	second := &types.Log{Address: common.HexToAddress("0x2"), Index: 1}
	statedb.AddLog(first)
	statedb.AddLog(second)
	stateSyncLogs := chain.writeBorStateSyncLogs(
		chain.db.NewBatch(),
		blocks[0],
		[]*types.Receipt{{CumulativeGasUsed: 21_000}},
		[]*types.Log{first},
		statedb,
	)
	require.Len(t, stateSyncLogs, 1)
	require.Equal(t, second.Address, stateSyncLogs[0].Address)

	sideEvents := make(chan ChainSideEvent, 1)
	secondHeadEvents := make(chan Chain2HeadEvent, 1)
	sideSub := chain.SubscribeChainSideEvent(sideEvents)
	secondHeadSub := chain.SubscribeChain2HeadEvent(secondHeadEvents)
	defer sideSub.Unsubscribe()
	defer secondHeadSub.Unsubscribe()
	chain.emitPostWriteEvents(blocks[0], nil, nil, nil, SideStatTy, false)
	require.Equal(t, blocks[0].Hash(), (<-sideEvents).Header.Hash())
	require.Equal(t, Chain2HeadForkEvent, (<-secondHeadEvents).Type)

	chainEvents := make(chan ChainEvent, 1)
	headEvents := make(chan ChainHeadEvent, 1)
	logEvents := make(chan []*types.Log, 2)
	stateSyncEvents := make(chan StateSyncEvent, 1)
	chainSub := chain.SubscribeChainEvent(chainEvents)
	headSub := chain.SubscribeChainHeadEvent(headEvents)
	logSub := chain.SubscribeLogsEvent(logEvents)
	stateSyncSub := chain.SubscribeStateSyncEvent(stateSyncEvents)
	defer chainSub.Unsubscribe()
	defer headSub.Unsubscribe()
	defer logSub.Unsubscribe()
	defer stateSyncSub.Unsubscribe()

	syncData := &types.StateSyncData{ID: 1}
	chain.SetStateSync([]*types.StateSyncData{syncData})
	rawdb.WriteBlock(chain.db, blocks[0])
	chain.emitPostWriteEvents(blocks[0], nil, []*types.Log{first}, []*types.Log{second}, CanonStatTy, true)
	require.Equal(t, blocks[0].Hash(), (<-chainEvents).Header.Hash())
	require.Equal(t, blocks[0].Hash(), (<-headEvents).Header.Hash())
	require.Equal(t, first.Address, (<-logEvents)[0].Address)
	require.Equal(t, second.Address, (<-logEvents)[0].Address)
	require.Same(t, syncData, (<-stateSyncEvents).Data)
}

func TestPipelinedSRCReaderAndPreloadBranches(t *testing.T) {
	chain, _, blocks := newPipelineHelperChain(t)
	parentRoot := chain.CurrentBlock().Root

	recordDetachedPrefetchStats(state.PrefetcherSnapshotStats{
		Drain:          time.Millisecond,
		Report:         time.Millisecond,
		Collect:        time.Millisecond,
		Fetchers:       2,
		LoadedFetchers: 1,
		AccountNodes:   3,
		StorageNodes:   4,
		AccountBytes:   5,
		StorageBytes:   6,
	}, false)
	recordDetachedPrefetchStats(state.PrefetcherSnapshotStats{
		Drain:          time.Millisecond,
		Report:         time.Millisecond,
		Collect:        time.Millisecond,
		Fetchers:       2,
		LoadedFetchers: 1,
		AccountNodes:   3,
		StorageNodes:   4,
		AccountBytes:   5,
		StorageBytes:   6,
	}, true)
	require.Nil(t, finishDetachedPrefetcher(nil, false))

	_, _, err := chain.openSRCStateDB(common.HexToHash("0xdead"), blocks[0], false, nil, nil)
	require.Error(t, err)
	_, _, err = chain.openSRCStateDB(common.HexToHash("0xdead"), blocks[0], true, nil, nil)
	require.Error(t, err)

	tmpDB, witness, err := chain.openSRCStateDB(parentRoot, blocks[0], true, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, tmpDB)
	require.NotNil(t, witness)

	readAddress := common.HexToAddress("0x100")
	mutatedAddress := common.HexToAddress("0x200")
	destructedAddress := common.HexToAddress("0x300")
	resurrectedAddress := common.HexToAddress("0x400")
	slot := common.HexToHash("0x1")
	diff := &state.FlatDiff{
		Accounts: map[common.Address]types.StateAccount{
			mutatedAddress:     {Balance: uint256.NewInt(1)},
			resurrectedAddress: {Balance: uint256.NewInt(2)},
		},
		Storage: map[common.Address]map[common.Hash]common.Hash{},
		Destructs: map[common.Address]struct{}{
			destructedAddress:  {},
			resurrectedAddress: {},
		},
		ReadSet: []common.Address{readAddress},
		ReadStorage: map[common.Address][]common.Hash{
			readAddress:    {slot},
			mutatedAddress: {slot},
		},
		NonExistentReads: []common.Address{common.HexToAddress("0x500")},
	}
	preloadFlatDiffReads(tmpDB, diff)
	recordAndPreloadSRCWitnessReads(tmpDB, diff)
	emitSRCStateDBMetrics(tmpDB)

	pending := new(pendingSRCState)
	chain.encodeAndCachePendingWitness(pending, nil, blocks[0])
	chain.encodeAndCachePendingWitness(pending, &stateless.Witness{}, blocks[0])
	require.NotEmpty(t, pending.witness)
}
