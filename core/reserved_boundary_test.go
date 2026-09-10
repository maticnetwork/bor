package core

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

// boundaryParentRoot returns the parent state root for the i-th generated
// block: the genesis root for the first block, the previous block's root
// otherwise. Shared by both boundary tests so the parent-state walk cannot
// drift between them.
func boundaryParentRoot(bc *BlockChain, blocks types.Blocks, i int) common.Hash {
	if i == 0 {
		return bc.Genesis().Root()
	}
	return blocks[i-1].Root()
}

// boundaryStateAt resolves a fresh StateDB at root, failing the test on error.
func boundaryStateAt(t *testing.T, bc *BlockChain, root common.Hash) *state.StateDB {
	t.Helper()
	st, err := bc.StateAt(root)
	require.NoError(t, err)
	return st
}

// cloneReceiptsForDerive returns a deep-enough copy of rs (each *Receipt and
// each of its *Log dereferenced and copied) so that Receipts.DeriveFields -
// which mutates its receiver's fields, including nested Log fields, in place
// - can run against the copy without disturbing the original ProcessResult.
// Callers compare the roots/blooms/gas of the original res.Receipts before
// this runs, so mutating a clone rather than the original is precautionary,
// not required for correctness, but keeps each processor's ProcessResult
// intact for any later inspection in the same test.
func cloneReceiptsForDerive(rs types.Receipts) types.Receipts {
	out := make(types.Receipts, len(rs))
	for i, r := range rs {
		cp := *r
		cp.Logs = make([]*types.Log, len(r.Logs))
		for j, l := range r.Logs {
			lc := *l
			cp.Logs[j] = &lc
		}
		out[i] = &cp
	}
	return out
}

// TestReservedBoundaryParity_ProcessorsAgree extends the single-block parity
// pattern in TestReservedTxIndexesParity_ProcessorsAgree across the
// reserved-blockspace fork boundary itself: a 5-block chain with the fork
// activating at block 3, so blocks 1-2 exercise the pre-fork (classification
// off) path and blocks 3-5 exercise the post-fork path, on the same chain and
// the same registry wiring. Every block is re-executed from its own parent
// state by all three ProcessResult assembly sites - serial, parallel V1
// (ParallelStateProcessor), and parallel V2 (BlockSTM) - and asserted
// identical on every field ProcessResult carries plus the derived receipt
// roots, bloom, and state root that ValidateState checks downstream. A
// divergence at exactly the fork-transition block (3) is the failure mode
// most likely to survive a single-block test but not this one.
//
// As in the precedent, block generation uses an ethash faker chain purely as
// a body vehicle (GenerateChain never touches header.Extra or runs
// reserved-aware execution - see core/evm.go's ReservedSnapshotForBlock and
// the miner's writeReservedFields, neither of which GenerateChain calls); the
// reserved senders' transactions carry ordinary fallback fees so they are
// valid to generate under vanilla fee rules; the registry wiring on the
// re-executing chain then reclassifies them fee-free from block 3 onward.
func TestReservedBoundaryParity_ProcessorsAgree(t *testing.T) {
	var (
		keyReservedA, _ = crypto.GenerateKey()
		keyReservedB, _ = crypto.GenerateKey()
		keyNormal, _    = crypto.GenerateKey()
		addrReservedA   = crypto.PubkeyToAddress(keyReservedA.PublicKey)
		addrReservedB   = crypto.PubkeyToAddress(keyReservedB.PublicKey)
		addrNormal      = crypto.PubkeyToAddress(keyNormal.PublicKey)
		recipient       = common.HexToAddress("0xbb")
	)

	const (
		forkBlock  = 3
		numBlocks  = 5
		quota      = 100_000
		perTxGas   = 21000
		reservedTx = 2 * perTxGas // addrReservedA + addrReservedB, one tx each per block
	)

	config := reservedActiveConfig(t)
	config.Bor.ReservedBlockspaceBlock = big.NewInt(forkBlock)
	// checkReservedBlockspaceForkOrder requires ReservedBlockspaceBlock to be
	// at or after CancunBlock/GiuglianoBlock; BorUnittestChainConfig
	// (reservedActiveConfig's base) doesn't schedule Cancun/Shanghai, so
	// activate the full pre-Cancun fork ladder from genesis too, mirroring
	// reserved_parity_test.go's mutation of the same shared config helper.
	config.LondonBlock = big.NewInt(0)
	config.ShanghaiBlock = big.NewInt(0)
	config.CancunBlock = big.NewInt(0)
	config.Bor.GiuglianoBlock = big.NewInt(0)
	signer := types.LatestSigner(config)

	genesis := &Genesis{
		Config:  config,
		BaseFee: big.NewInt(params.InitialBaseFee),
		Alloc: types.GenesisAlloc{
			addrReservedA: {Balance: big.NewInt(1e18)},
			addrReservedB: {Balance: big.NewInt(1e18)},
			addrNormal:    {Balance: big.NewInt(1e18)},
		},
	}

	mkTx := func(key *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce: nonce, To: &recipient, Value: big.NewInt(1),
			Gas: perTxGas, GasPrice: big.NewInt(params.InitialBaseFee * 2),
		})
		require.NoError(t, err)
		return tx
	}

	// Each block carries one ordinary-fee tx from each of the two reserved
	// senders and the normal sender, always in the same [reservedA, normal,
	// reservedB] order, so the expected reserved indexes ([0, 2]) are
	// constant across every post-fork block.
	genDb, blocks, _ := GenerateChainWithGenesis(genesis, ethash.NewFaker(), numBlocks, func(i int, b *BlockGen) {
		n := uint64(i)
		b.AddTx(mkTx(keyReservedA, n))
		b.AddTx(mkTx(keyNormal, n))
		b.AddTx(mkTx(keyReservedB, n))
	})
	require.Len(t, blocks, numBlocks)

	// NewParallelBlockChain (not NewBlockChain) so bc.parallelSpeculativeProcesses
	// is actually set: ParallelStateProcessor.Process reads it at call time, and
	// leaving it at its zero value hangs blockstm.ExecuteParallel forever.
	bc, err := NewParallelBlockChain(genDb, genesis, ethash.NewFaker(), DefaultConfig(), 2, false)
	require.NoError(t, err)
	defer bc.Stop()

	bc.SetReservedRegistry(&fakeParityReader{
		addrs: map[common.Address]struct{}{addrReservedA: {}, addrReservedB: {}},
		quota: quota,
	})

	wantReservedIdx := []uint64{0, 2}

	for i, block := range blocks {
		blockNum := block.NumberU64()
		t.Run(fmt.Sprintf("block_%d", blockNum), func(t *testing.T) {
			root := boundaryParentRoot(bc, blocks, i)

			serialSdb := boundaryStateAt(t, bc, root)
			v1Sdb := boundaryStateAt(t, bc, root)
			v2Sdb := boundaryStateAt(t, bc, root)

			serialRes, err := bc.processor.Process(block, serialSdb, vm.Config{}, nil, context.Background())
			require.NoError(t, err, "serial processor")

			v1 := NewParallelStateProcessor(bc.hc, bc)
			v1Res, err := v1.Process(block, v1Sdb, vm.Config{}, nil, context.Background())
			require.NoError(t, err, "parallel-v1 (ParallelStateProcessor)")

			v2 := NewV2StateProcessor(bc.hc, bc, 2)
			v2Res, err := v2.Process(block, v2Sdb, vm.Config{}, nil, context.Background())
			require.NoError(t, err, "parallel-v2 (BlockSTM)")

			// 1. Receipt root.
			serialReceiptRoot := types.DeriveSha(serialRes.Receipts, trie.NewStackTrie(nil))
			require.Equal(t, serialReceiptRoot, types.DeriveSha(v1Res.Receipts, trie.NewStackTrie(nil)), "receipt root: serial vs v1")
			require.Equal(t, serialReceiptRoot, types.DeriveSha(v2Res.Receipts, trie.NewStackTrie(nil)), "receipt root: serial vs v2")

			// 2. Bloom.
			serialBloom := types.MergeBloom(serialRes.Receipts)
			require.Equal(t, serialBloom, types.MergeBloom(v1Res.Receipts), "bloom: serial vs v1")
			require.Equal(t, serialBloom, types.MergeBloom(v2Res.Receipts), "bloom: serial vs v2")

			// 3. Gas used.
			require.Equal(t, serialRes.GasUsed, v1Res.GasUsed, "gas used: serial vs v1")
			require.Equal(t, serialRes.GasUsed, v2Res.GasUsed, "gas used: serial vs v2")

			// 4. Post-execution state root.
			serialRoot := serialSdb.IntermediateRoot(config.IsEIP158(block.Number()))
			v1Root := v1Sdb.IntermediateRoot(config.IsEIP158(block.Number()))
			v2Root := v2Sdb.IntermediateRoot(config.IsEIP158(block.Number()))
			require.Equal(t, serialRoot, v1Root, "state root: serial vs v1")
			require.Equal(t, serialRoot, v2Root, "state root: serial vs v2")

			// 5. Reserved fields.
			require.Equal(t, serialRes.ReservedGasUsed, v1Res.ReservedGasUsed, "ReservedGasUsed: serial vs v1")
			require.Equal(t, serialRes.ReservedGasUsed, v2Res.ReservedGasUsed, "ReservedGasUsed: serial vs v2")
			require.Equal(t, serialRes.ReservedCapacity, v1Res.ReservedCapacity, "ReservedCapacity: serial vs v1")
			require.Equal(t, serialRes.ReservedCapacity, v2Res.ReservedCapacity, "ReservedCapacity: serial vs v2")
			require.Equal(t, serialRes.ReservedTxIndexes, v1Res.ReservedTxIndexes, "ReservedTxIndexes: serial vs v1")
			require.Equal(t, serialRes.ReservedTxIndexes, v2Res.ReservedTxIndexes, "ReservedTxIndexes: serial vs v2")

			// Boundary shape: the fork gate itself, not just cross-processor
			// agreement on whatever it produces.
			if blockNum < forkBlock {
				require.Empty(t, serialRes.ReservedTxIndexes, "pre-fork block %d must classify nothing", blockNum)
				require.Zero(t, serialRes.ReservedGasUsed, "pre-fork block %d must report zero reserved gas", blockNum)
				require.Zero(t, serialRes.ReservedCapacity, "pre-fork block %d must report zero reserved capacity", blockNum)
			} else {
				require.Equal(t, wantReservedIdx, serialRes.ReservedTxIndexes, "post-fork block %d reserved indexes", blockNum)
				require.EqualValues(t, reservedTx, serialRes.ReservedGasUsed, "post-fork block %d reserved gas", blockNum)
				require.EqualValues(t, quota, serialRes.ReservedCapacity, "post-fork block %d reserved capacity", blockNum)
			}

			// Receipt-derivation determinism: rederive on a clone of each
			// processor's own receipts (using that processor's own
			// ReservedTxIndexes, already proven identical above) and check
			// the resulting EffectiveGasPrice per transaction agrees across
			// all three, and matches the reserved/fee-paying split.
			derive := func(res *ProcessResult) types.Receipts {
				cloned := cloneReceiptsForDerive(res.Receipts)
				require.NoError(t, cloned.DeriveFields(config, block.Hash(), block.NumberU64(), block.Time(), block.BaseFee(), nil, block.Transactions(), res.ReservedTxIndexes))
				return cloned
			}
			serialDerived := derive(serialRes)
			v1Derived := derive(v1Res)
			v2Derived := derive(v2Res)

			reservedIdx := make(map[int]bool, len(serialRes.ReservedTxIndexes))
			for _, idx := range serialRes.ReservedTxIndexes {
				reservedIdx[int(idx)] = true
			}
			for idx := range block.Transactions() {
				sEGP := serialDerived[idx].EffectiveGasPrice
				v1EGP := v1Derived[idx].EffectiveGasPrice
				v2EGP := v2Derived[idx].EffectiveGasPrice

				if reservedIdx[idx] {
					require.Zero(t, sEGP.Sign(), "tx %d: reserved txs must derive EffectiveGasPrice 0, got %s", idx, sEGP)
				} else {
					require.NotZero(t, sEGP.Sign(), "tx %d: fee-paying txs must derive a nonzero EffectiveGasPrice, got %s", idx, sEGP)
				}
				require.Zero(t, sEGP.Cmp(v1EGP), "tx %d EffectiveGasPrice: serial=%s vs v1=%s", idx, sEGP, v1EGP)
				require.Zero(t, sEGP.Cmp(v2EGP), "tx %d EffectiveGasPrice: serial=%s vs v2=%s", idx, sEGP, v2EGP)
			}
		})
	}
}

// TestReservedBoundaryNoReaderFailsClosed pins the fail-closed contract at
// ReservedSnapshotForBlock (core/evm.go): once the fork is active and a
// registry contract is configured, a chain with no reader wired must refuse
// to process the block rather than silently classify nothing - a silent
// empty set would disagree with any peer that does have a reader wired, since
// the reserved senders would pay real fees on this node and none on that one.
// The same 5-block, fork-at-3 setup as the parity test above is reused
// (unwired here), so block 2 (pre-fork, no registry read needed) must still
// succeed on all three processors while block 3 (first post-fork block) must
// hard-error on all three.
func TestReservedBoundaryNoReaderFailsClosed(t *testing.T) {
	var (
		key, _    = crypto.GenerateKey()
		addr      = crypto.PubkeyToAddress(key.PublicKey)
		recipient = common.HexToAddress("0xcc")
	)

	const forkBlock = 3

	config := reservedActiveConfig(t)
	config.Bor.ReservedBlockspaceBlock = big.NewInt(forkBlock)
	config.LondonBlock = big.NewInt(0)
	config.ShanghaiBlock = big.NewInt(0)
	config.CancunBlock = big.NewInt(0)
	config.Bor.GiuglianoBlock = big.NewInt(0)
	signer := types.LatestSigner(config)

	genesis := &Genesis{
		Config:  config,
		BaseFee: big.NewInt(params.InitialBaseFee),
		Alloc: types.GenesisAlloc{
			addr: {Balance: big.NewInt(1e18)},
		},
	}

	mkTx := func(nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(key, signer, &types.LegacyTx{
			Nonce: nonce, To: &recipient, Value: big.NewInt(1),
			Gas: 21000, GasPrice: big.NewInt(params.InitialBaseFee * 2),
		})
		require.NoError(t, err)
		return tx
	}

	genDb, blocks, _ := GenerateChainWithGenesis(genesis, ethash.NewFaker(), forkBlock, func(i int, b *BlockGen) {
		b.AddTx(mkTx(uint64(i)))
	})
	require.Len(t, blocks, forkBlock)

	// No SetReservedRegistry call: this is exactly the condition under test.
	bc, err := NewParallelBlockChain(genDb, genesis, ethash.NewFaker(), DefaultConfig(), 2, false)
	require.NoError(t, err)
	defer bc.Stop()

	preFork := blocks[1] // block number 2, still below forkBlock (3)
	require.EqualValues(t, 2, preFork.NumberU64())
	preForkRoot := boundaryParentRoot(bc, blocks, 1)

	_, err = bc.processor.Process(preFork, boundaryStateAt(t, bc, preForkRoot), vm.Config{}, nil, context.Background())
	require.NoError(t, err, "serial must process the pre-fork block without a reader")
	_, err = NewParallelStateProcessor(bc.hc, bc).Process(preFork, boundaryStateAt(t, bc, preForkRoot), vm.Config{}, nil, context.Background())
	require.NoError(t, err, "parallel-v1 must process the pre-fork block without a reader")
	_, err = NewV2StateProcessor(bc.hc, bc, 2).Process(preFork, boundaryStateAt(t, bc, preForkRoot), vm.Config{}, nil, context.Background())
	require.NoError(t, err, "parallel-v2 must process the pre-fork block without a reader")

	atFork := blocks[2] // block number 3, the fork's first active block
	require.EqualValues(t, forkBlock, atFork.NumberU64())
	atForkRoot := boundaryParentRoot(bc, blocks, 2)

	_, err = bc.processor.Process(atFork, boundaryStateAt(t, bc, atForkRoot), vm.Config{}, nil, context.Background())
	require.Error(t, err, "serial must fail closed at the fork boundary without a reader")
	_, err = NewParallelStateProcessor(bc.hc, bc).Process(atFork, boundaryStateAt(t, bc, atForkRoot), vm.Config{}, nil, context.Background())
	require.Error(t, err, "parallel-v1 must fail closed at the fork boundary without a reader")
	_, err = NewV2StateProcessor(bc.hc, bc, 2).Process(atFork, boundaryStateAt(t, bc, atForkRoot), vm.Config{}, nil, context.Background())
	require.Error(t, err, "parallel-v2 must fail closed at the fork boundary without a reader")
}
