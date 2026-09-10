package core

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// fakeParityReader is a state-independent registryreader.Reader with a fixed
// whitelist, so TestReservedTxIndexesParity_ProcessorsAgree can wire a real
// classification (registryreader.BuildSnapshot -> ClassifyReserved) without a
// deployed registry contract.
type fakeParityReader struct {
	addrs map[common.Address]struct{}
	quota uint64
}

func (f *fakeParityReader) HasReservedRegistry() bool { return true }

func (f *fakeParityReader) IsReservedAddress(_ *state.StateDB, _ uint64, _ common.Hash, addr common.Address) (bool, error) {
	_, ok := f.addrs[addr]
	return ok, nil
}

func (f *fakeParityReader) ReservedClientForAddress(_ *state.StateDB, _ uint64, _ common.Hash, addr common.Address) (registryreader.ClientLookup, error) {
	if _, ok := f.addrs[addr]; !ok {
		return registryreader.ClientLookup{}, nil
	}
	return registryreader.ClientLookup{ClientID: big.NewInt(1), GasQuota: f.quota, Active: true}, nil
}

func (f *fakeParityReader) Root(_ *state.StateDB, _ uint64, _ common.Hash) (common.Hash, error) {
	return common.HexToHash("0x1"), nil
}

func (f *fakeParityReader) WhitelistedAddresses(_ *state.StateDB, _ uint64, _ common.Hash) ([]common.Address, error) {
	out := make([]common.Address, 0, len(f.addrs))
	for a := range f.addrs {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeParityReader) TotalReservedGas(_ *state.StateDB, _ uint64, _ common.Hash) (uint64, error) {
	return f.quota, nil
}

// TestReservedTxIndexesParity_ProcessorsAgree builds one block with a mix of
// reserved and non-reserved senders (all sharing one client, all within
// quota) and confirms the serial, parallel-v1, and parallel-v2 (BlockSTM)
// processors - the three independent ProcessResult assembly sites - derive
// identical ReservedTxIndexes and ReservedClientUsage for it. A registry
// reader is wired via BlockChain.SetReservedRegistry with a fixed in-memory
// fake (real registryreader.BuildSnapshot/ClassifyReserved, no deployed
// contract), and each processor re-executes the same block from the same
// parent state.
//
// This is a real run of each processor's Process method - including a real
// registryreader.BuildSnapshot/ClassifyReserved classification pass, just
// against a fixed fake reader instead of a deployed contract - not a
// re-derivation of their inputs, so it would catch a future divergence in
// how a site builds its (txs, signer, ReservedTxs) triple. It does not
// exercise a real on-chain registry contract, base-fee capacity, or the
// miner's sealing path - those are covered by
// tests/bor/reserved_receipts_test.go end-to-end (for the default processor
// pairing) and by TestSumReservedGasUsed/TestDeriveFieldsReserved at the
// unit level.
func TestReservedTxIndexesParity_ProcessorsAgree(t *testing.T) {
	var (
		keyReservedA, _ = crypto.GenerateKey()
		keyReservedB, _ = crypto.GenerateKey()
		keyNormal, _    = crypto.GenerateKey()
		addrReservedA   = crypto.PubkeyToAddress(keyReservedA.PublicKey)
		addrReservedB   = crypto.PubkeyToAddress(keyReservedB.PublicKey)
		addrNormal      = crypto.PubkeyToAddress(keyNormal.PublicKey)
		recipient       = common.HexToAddress("0xaa")
	)

	config := reservedActiveConfig(t)
	// checkReservedBlockspaceForkOrder requires ReservedBlockspaceBlock to be
	// at or after CancunBlock; BorUnittestChainConfig (reservedActiveConfig's
	// base) doesn't schedule Cancun/Shanghai, so activate the full pre-Cancun
	// fork ladder from genesis too.
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
			Gas: 21000, GasPrice: big.NewInt(params.InitialBaseFee * 2),
		})
		require.NoError(t, err)
		return tx
	}

	// Block generation doesn't need reserved-aware execution - the generated
	// block is only a vehicle for a valid header/body; each processor below
	// re-executes it from the parent state with the registry wired in.
	genDb, blocks, _ := GenerateChainWithGenesis(genesis, ethash.NewFaker(), 1, func(i int, b *BlockGen) {
		b.AddTx(mkTx(keyReservedA, 0))
		b.AddTx(mkTx(keyNormal, 0))
		b.AddTx(mkTx(keyReservedB, 0))
	})
	require.Len(t, blocks, 1)
	block := blocks[0]

	// NewParallelBlockChain (not NewBlockChain) so bc.parallelSpeculativeProcesses
	// is actually set: ParallelStateProcessor.Process reads it at call time, and
	// leaving it at its zero value hangs blockstm.ExecuteParallel forever.
	bc, err := NewParallelBlockChain(genDb, genesis, ethash.NewFaker(), DefaultConfig(), 2, false)
	require.NoError(t, err)
	defer bc.Stop()

	bc.SetReservedRegistry(&fakeParityReader{
		addrs: map[common.Address]struct{}{addrReservedA: {}, addrReservedB: {}},
		quota: 100_000,
	})

	parentRoot := bc.Genesis().Root()
	freshState := func() *state.StateDB {
		st, err := bc.StateAt(parentRoot)
		require.NoError(t, err)
		return st
	}

	want := []uint64{0, 2} // addrReservedA at index 0, addrReservedB at index 2
	// Both reserved senders share fakeParityReader's single client (id 1), so
	// Used is the summed declared gas of both reserved transactions (21000
	// each, from mkTx's Gas field) and Quota is the fake reader's quota.
	wantUsage := map[uint64]registryreader.ClientUsage{1: {Used: 42000, Quota: 100_000}}

	serialRes, err := bc.processor.Process(block, freshState(), vm.Config{}, nil, context.Background())
	require.NoError(t, err)
	require.Equal(t, want, serialRes.ReservedTxIndexes, "serial processor")
	require.Equal(t, wantUsage, serialRes.ReservedClientUsage, "serial processor usage")

	v1 := NewParallelStateProcessor(bc.hc, bc)
	v1Res, err := v1.Process(block, freshState(), vm.Config{}, nil, context.Background())
	require.NoError(t, err)
	require.Equal(t, want, v1Res.ReservedTxIndexes, "parallel-v1 (ParallelStateProcessor)")
	require.Equal(t, wantUsage, v1Res.ReservedClientUsage, "parallel-v1 (ParallelStateProcessor) usage")

	v2 := NewV2StateProcessor(bc.hc, bc, 2)
	v2Res, err := v2.Process(block, freshState(), vm.Config{}, nil, context.Background())
	require.NoError(t, err)
	require.Equal(t, want, v2Res.ReservedTxIndexes, "parallel-v2 (BlockSTM)")
	require.Equal(t, wantUsage, v2Res.ReservedClientUsage, "parallel-v2 (BlockSTM) usage")
}

// TestWriteBlockWithState_NilReceiptsSuppressesReservedWrite is the focused
// unit-level check for the stateless writers' invariant: writeBlockWithState
// must not persist a reserved-tx side-table entry when receipts is nil, even
// when a non-nil index list is passed alongside it - exactly the call shape
// the stateless sequential/parallel/deferred-retry writers now use, since
// they thread their real ReservedTxIndexes through rather than special-casing
// a literal nil at each call site. A full stateless-mode run (witness
// generation and verification) is disproportionate for this one invariant,
// so this drives writeBlockWithState directly.
func TestWriteBlockWithState_NilReceiptsSuppressesReservedWrite(t *testing.T) {
	_, _, bc, err := newCanonical(ethash.NewFaker(), 1, true, rawdb.HashScheme)
	require.NoError(t, err)
	defer bc.Stop()

	block := bc.GetBlockByNumber(1)
	require.NotNil(t, block)
	statedb, err := bc.StateAt(block.Root())
	require.NoError(t, err)

	_, err = bc.writeBlockWithState(block, nil, nil, statedb, []uint64{0, 1})
	require.NoError(t, err)

	require.Nil(t, rawdb.ReadReservedTxIndexes(bc.db, block.Hash(), block.NumberU64()),
		"nil receipts must suppress the reserved-tx write regardless of what indexes are passed")
}
