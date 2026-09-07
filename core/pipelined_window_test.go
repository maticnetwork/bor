package core

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

// TestPipelinedImportSRC_WindowReads pins the state-read semantics that RPC
// handlers depend on during the pipelined window: the interval where the chain
// head has advanced to block N but N's state root is not yet committed because
// the SRC goroutine is still running. The srcHoldForTesting hook keeps that
// window open deterministically.
//
// During the window:
//   - StateAt(N.Root) must serve reads via the FlatDiff overlay (this is what
//     eth_getBalance/eth_call/eth_estimateGas at "latest" resolve through).
//   - state.New(N.Root) — a direct trie open, no overlay — must fail: the
//     window is genuinely open.
//   - trie.NewStateTrie at N.Root must fail: proofs need committed trie
//     nodes the overlay doesn't have — which is why eth_getProof and
//     debug_storageRangeAt gate on WaitForPipelinedStateCommit, verified
//     here to block during the window and release when SRC settles.
//   - The committed parent root must remain fully readable.
//
// After release, the same queries must succeed against the committed trie with
// values identical to those the overlay served.
func TestPipelinedImportSRC_WindowReads(t *testing.T) {
	testPipelinedImportSRCWindowReads(t, rawdb.HashScheme)
	testPipelinedImportSRCWindowReads(t, rawdb.PathScheme)
}

func testPipelinedImportSRCWindowReads(t *testing.T, scheme string) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		txValue   = big.NewInt(10000)
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	numBlocks := 5
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, txValue, params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})
	held := blocks[numBlocks-1]

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	require.NoError(t, err)

	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	chain.srcHoldForTesting = func(blockNumber uint64) {
		if blockNumber == held.NumberU64() {
			<-gate
		}
	}
	defer func() {
		release()
		chain.Stop()
	}()

	_, err = chain.InsertChain(blocks, false)
	require.NoError(t, err)

	overlayBalance := assertWindowReads(t, chain, blocks, recipient, addr)

	// WaitForPipelinedStateCommit must block while the window is open (the
	// eth_getProof / debug_storageRangeAt gate), return once SRC settles, and
	// be a no-op for roots other than the pending head's.
	require.NoError(t, chain.WaitForPipelinedStateCommit(context.Background(), blocks[0].Root()))
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- chain.WaitForPipelinedStateCommit(context.Background(), held.Root())
	}()
	select {
	case err := <-waitDone:
		t.Fatalf("WaitForPipelinedStateCommit returned (%v) while SRC is held", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case err := <-waitDone:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForPipelinedStateCommit did not return after SRC release")
	}
	requireRootCommitted(t, chain, held.Root())

	assertSettledReads(t, chain, held, recipient, overlayBalance)
}

// assertWindowReads verifies read behavior while the SRC window for the last
// block is held open, and returns the recipient balance the overlay served.
func assertWindowReads(t *testing.T, chain *BlockChain, blocks []*types.Block, recipient, sender common.Address) *big.Int {
	t.Helper()
	held := blocks[len(blocks)-1]
	parent := blocks[len(blocks)-2]

	require.Equal(t, held.NumberU64(), chain.CurrentBlock().Number.Uint64())
	require.Equal(t, held.Hash(), chain.CurrentBlock().Hash())

	// The overlay path: what eth_getBalance / eth_call at "latest" resolve
	// through while the window is open.
	statedb, err := chain.StateAt(held.Root())
	require.NoError(t, err, "StateAt(head root) must succeed via FlatDiff overlay during the window")
	balance := statedb.GetBalance(recipient).ToBig()
	expected := new(big.Int).Mul(big.NewInt(10000), big.NewInt(int64(len(blocks))))
	require.Zero(t, balance.Cmp(expected), "overlay recipient balance: got %s, want %s", balance, expected)
	require.Equal(t, uint64(len(blocks)), statedb.GetNonce(sender))

	// A direct trie open without the overlay must fail — proves the root is
	// genuinely uncommitted and the window is open.
	_, err = state.New(held.Root(), chain.statedb)
	require.Error(t, err, "state.New(head root) must fail while SRC is held")

	// A raw trie open at the head root fails during the window — the reason
	// eth_getProof / debug_storageRangeAt gate on WaitForPipelinedStateCommit
	// before opening tries.
	_, err = trie.NewStateTrie(trie.StateTrieID(held.Root()), chain.triedb)
	require.Error(t, err, "NewStateTrie(head root) must fail while SRC is held")

	// The committed parent root stays fully readable.
	parentState, err := chain.StateAt(parent.Root())
	require.NoError(t, err)
	require.Equal(t, uint64(len(blocks)-1), parentState.GetNonce(sender))

	return balance
}

// assertSettledReads verifies that, once SRC has committed the held block's
// root, direct trie reads succeed and match the values the overlay served.
func assertSettledReads(t *testing.T, chain *BlockChain, held *types.Block, recipient common.Address, overlayBalance *big.Int) {
	t.Helper()

	statedb, err := chain.StateAt(held.Root())
	require.NoError(t, err)
	require.Zero(t, statedb.GetBalance(recipient).ToBig().Cmp(overlayBalance),
		"balance served after settle must match the overlay-served value")

	direct, err := state.New(held.Root(), chain.statedb)
	require.NoError(t, err, "state.New(head root) must succeed after settle")
	require.Zero(t, direct.GetBalance(recipient).ToBig().Cmp(overlayBalance))

	tr, err := trie.NewStateTrie(trie.StateTrieID(held.Root()), chain.triedb)
	require.NoError(t, err, "NewStateTrie(head root) must succeed after settle")
	proofDb := memorydb.New()
	accountKey := crypto.Keccak256(recipient.Bytes())
	require.NoError(t, tr.Prove(accountKey, proofDb))
	val, err := trie.VerifyProof(held.Root(), accountKey, proofDb)
	require.NoError(t, err, "account proof at settled head root must verify")
	require.NotEmpty(t, val, "proof must resolve to a non-empty account")
}

// requireRootCommitted polls until the given root is directly openable
// (i.e. SRC has committed it), failing the test after a timeout.
func requireRootCommitted(t *testing.T, chain *BlockChain, root common.Hash) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		if _, err := state.New(root, chain.statedb); err == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for SRC to commit root %x", root)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestPipelinedImportSRC_RootMismatchRollback covers the failure path the
// pipeline's happy path defers all verification to: a block whose header root
// disagrees with the root SRC computes. Because the pipeline publishes the head
// before verifying, the rollback has to undo the durable markers and the
// overlay, not just the head pointer — and the collector must not try to take
// chainmu, which the thread waiting on it already holds.
//
// The batch is inserted from a goroutine with a deadline so a regression to the
// deadlocking shape fails the test instead of hanging the suite.
func TestPipelinedImportSRC_RootMismatchRollback(t *testing.T) {
	testPipelinedImportSRCRootMismatchRollback(t, rawdb.HashScheme)
	testPipelinedImportSRCRootMismatchRollback(t, rawdb.PathScheme)
}

func testPipelinedImportSRCRootMismatchRollback(t *testing.T, scheme string) {
	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 2, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(10000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	// Corrupt the first block's state root only. Receipt root, bloom and gas
	// stay valid, so ValidateStateCheap admits it and the divergence is caught
	// by SRC — the exact path the recovery code exists for. The second block is
	// re-parented onto the corrupted hash so the batch stays contiguous and the
	// collect of block 1's SRC happens while chainmu is held for block 2.
	badHeader := types.CopyHeader(blocks[0].Header())
	badHeader.Root = common.HexToHash("0xdeadbeef")
	bad := types.NewBlockWithHeader(badHeader).WithBody(*blocks[0].Body())

	nextHeader := types.CopyHeader(blocks[1].Header())
	nextHeader.ParentHash = bad.Hash()
	next := types.NewBlockWithHeader(nextHeader).WithBody(*blocks[1].Body())

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, pipelinedConfig(scheme))
	require.NoError(t, err)
	defer chain.Stop()

	done := make(chan error, 1)
	go func() {
		_, err := chain.InsertChain(types.Blocks{bad, next}, false)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "inserting a block whose root SRC disproves must fail")
	case <-time.After(60 * time.Second):
		t.Fatal("InsertChain deadlocked on the root-mismatch recovery path")
	}

	// Head must be back at genesis, with the rejected block's durable markers
	// removed — a stale canonical hash would keep serving it over RPC.
	require.Equal(t, uint64(0), chain.CurrentBlock().Number.Uint64(),
		"head must roll back to the parent of the rejected block")
	require.NotEqual(t, bad.Hash(), chain.GetCanonicalHash(bad.NumberU64()),
		"canonical hash of the rejected block must be removed")
	for _, tx := range bad.Transactions() {
		require.Nil(t, rawdb.ReadTxLookupEntry(chain.db, tx.Hash()),
			"tx lookup entry of the rejected block must be removed")
	}

	// The overlay must stop advertising the rejected post-state, and the
	// pending entry must be cleared — leaving it set is what wedges every
	// later import behind the same failure.
	chain.pendingImportSRCMu.Lock()
	pending := chain.pendingImportSRC
	chain.pendingImportSRCMu.Unlock()
	require.Nil(t, pending, "pending import SRC must be cleared after a failed collect")
	require.Nil(t, chain.GetLastFlatDiff(), "FlatDiff overlay must be dropped after rollback")

	// The node must keep following the chain: the honest blocks now import.
	inserted, err := chain.InsertChain(blocks, false)
	require.NoError(t, err, "import must recover after a rejected block (inserted %d)", inserted)
	require.Equal(t, blocks[len(blocks)-1].NumberU64(), chain.CurrentBlock().Number.Uint64())
}

// TestPipelinedImportSRC_PipelineActuallyRuns is the positive control for the
// rest of the pipelined suite: every other test asserts an outcome the serial
// path produces identically, so all of them would still pass if the pipeline
// were silently disabled. This one fails in that case — it requires the
// pipelined block counter to advance under the pipelined config, and to stay
// put under the default config on the same chain.
func TestPipelinedImportSRC_PipelineActuallyRuns(t *testing.T) {
	metrics.Enable()

	var (
		key, _    = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0x00000000000000000000000000000000deadbeef")
		funds     = new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
		gspec     = &Genesis{
			Config:  params.AllEthashProtocolChanges,
			Alloc:   types.GenesisAlloc{addr: {Balance: funds}},
			BaseFee: big.NewInt(params.InitialBaseFee),
		}
		signer = types.LatestSigner(gspec.Config)
		engine = ethash.NewFaker()
	)

	const numBlocks = 4
	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, numBlocks, func(i int, gen *BlockGen) {
		tx, _ := types.SignTx(
			types.NewTransaction(gen.TxNonce(addr), recipient, big.NewInt(10000), params.TxGas, gen.header.BaseFee, nil),
			signer, key,
		)
		gen.AddTx(tx)
	})

	insert := func(cfg *BlockChainConfig) int64 {
		t.Helper()
		before := pipelineImportBlocksCounter.Snapshot().Count()
		chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, cfg)
		require.NoError(t, err)
		defer chain.Stop()
		_, err = chain.InsertChain(blocks, false)
		require.NoError(t, err)
		// The last block's SRC is left in flight by design; flushing settles it
		// so the counter reflects the whole batch.
		require.NoError(t, chain.flushPendingImportSRC(true))
		return pipelineImportBlocksCounter.Snapshot().Count() - before
	}

	pipelined := insert(pipelinedConfig(rawdb.PathScheme))
	require.Equal(t, int64(numBlocks), pipelined,
		"pipelined config must route every block through the SRC pipeline")

	serial := insert(DefaultConfig().WithStateScheme(rawdb.PathScheme))
	require.Zero(t, serial, "default config must not run the pipeline")
}
