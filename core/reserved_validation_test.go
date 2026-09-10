package core

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/consensus/bor/registryreader"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// reservedValidationConfig clones the unittest config with Cancun active from
// genesis and the reserved fork either at `forkBlock` (nil = never).
func reservedValidationConfig(forkBlock *big.Int) *params.ChainConfig {
	cc := *params.BorUnittestChainConfig
	bor := *cc.Bor
	cc.CancunBlock = big.NewInt(0)
	bor.ReservedBlockspaceBlock = forkBlock
	cc.Bor = &bor
	return &cc
}

// headerWithReservedFields builds a post-Cancun header whose Extra encodes a
// BlockExtraData carrying the given ReservedGasUsed/ReservedCapacity (nil =
// field absent; SetReservedFields only writes when gasUsed is non-nil, since
// the two fields are always stamped together).
func headerWithReservedFields(t *testing.T, number int64, gasUsed, capacity *uint64) *types.Header {
	t.Helper()
	enc, err := rlp.EncodeToBytes(&types.BlockExtraData{TxDependency: [][]uint64{}})
	require.NoError(t, err)
	extra := make([]byte, types.ExtraVanityLength)
	extra = append(extra, enc...)
	extra = append(extra, make([]byte, types.ExtraSealLength)...)
	h := &types.Header{Number: big.NewInt(number), Extra: extra}
	if gasUsed != nil {
		var c uint64
		if capacity != nil {
			c = *capacity
		}
		require.NoError(t, h.SetReservedFields(&params.ChainConfig{ChainID: big.NewInt(137), CancunBlock: big.NewInt(0)}, *gasUsed, c))
	}
	return h
}

// TestValidateReservedFields covers the anti-cheat guard: the header's
// stamped ReservedGasUsed and ReservedCapacity must equal, respectively, the
// gas the reserved (fee-free) txs actually used and the registry snapshot's
// effective capacity (res.ReservedGasUsed / res.ReservedCapacity), post-fork;
// pre-fork the check is skipped.
func TestValidateReservedFields(t *testing.T) {
	t.Parallel()
	fork := reservedValidationConfig(big.NewInt(0))
	n := uint64(50_000)
	cap10m := uint64(10_000_000)
	cap20m := uint64(20_000_000)

	cases := []struct {
		name            string
		cfg             *params.ChainConfig
		number          int64
		headerGasUsed   *uint64 // nil = field absent
		headerCapacity  *uint64
		resReservedGas  uint64
		resReservedCap  uint64
		wantGasMismatch bool
		wantCapMismatch bool
	}{
		{"match", fork, 100, &n, &cap10m, 50_000, 10_000_000, false, false},
		{"gas mismatch is rejected", fork, 100, &n, &cap10m, 40_000, 10_000_000, true, false},
		{"capacity mismatch is rejected", fork, 100, &n, &cap10m, 50_000, 20_000_000, false, true},
		{"both mismatch: gas checked first", fork, 100, &n, &cap10m, 40_000, 20_000_000, true, false},
		{"absent header fields compare as 0 — matches res 0", fork, 100, nil, nil, 0, 0, false, false},
		{"absent header fields, res nonzero gas — mismatch", fork, 100, nil, nil, 30_000, 0, true, false},
		{"absent header fields, res nonzero capacity — mismatch", fork, 100, nil, nil, 0, 5_000, false, true},
		// Boundary: fork at block 100.
		{"pre-fork (N-1) skips the check regardless", reservedValidationConfig(big.NewInt(100)), 99, nil, nil, 99_999, 99_999, false, false},
		{"at fork (N) enforces", reservedValidationConfig(big.NewInt(100)), 100, &n, &cap10m, 40_000, 10_000_000, true, false},
		{"post-fork (N+1) enforces", reservedValidationConfig(big.NewInt(100)), 101, &n, &cap20m, 50_000, 10_000_000, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &BlockValidator{config: tc.cfg}
			h := headerWithReservedFields(t, tc.number, tc.headerGasUsed, tc.headerCapacity)
			err := v.validateReservedFields(h, &ProcessResult{ReservedGasUsed: tc.resReservedGas, ReservedCapacity: tc.resReservedCap})
			switch {
			case tc.wantGasMismatch:
				require.ErrorIs(t, err, ErrReservedGasUsedMismatch)
			case tc.wantCapMismatch:
				require.ErrorIs(t, err, ErrReservedCapacityMismatch)
			default:
				require.NoError(t, err)
			}
		})
	}
}

// TestSumReservedGasUsed covers the receipt↔tx hash matching, empty-set
// short-circuit, and the missing-receipt (contributes 0) branch.
func TestSumReservedGasUsed(t *testing.T) {
	t.Parallel()
	signer := types.LatestSignerForChainID(big.NewInt(1))
	keyA, _ := crypto.GenerateKey()
	keyB, _ := crypto.GenerateKey()
	a := crypto.PubkeyToAddress(keyA.PublicKey)
	b := crypto.PubkeyToAddress(keyB.PublicKey)

	to := crypto.PubkeyToAddress(keyA.PublicKey)
	mkTx := func(key *ecdsa.PrivateKey, nonce uint64) *types.Transaction {
		tx, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
			ChainID: big.NewInt(1), Nonce: nonce, Gas: 21000, To: &to,
			GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(0),
		})
		require.NoError(t, err)
		return tx
	}
	txA0 := mkTx(keyA, 0)
	txA1 := mkTx(keyA, 1)
	txB0 := mkTx(keyB, 0)
	txs := types.Transactions{txA0, txA1, txB0}
	receipts := types.Receipts{
		{TxHash: txA0.Hash(), GasUsed: 21_000},
		{TxHash: txA1.Hash(), GasUsed: 30_000},
		{TxHash: txB0.Hash(), GasUsed: 40_000},
	}

	// a0 and b0 reserved; a1 not in the set. Matched by hash regardless of
	// order, but indexes are positions within txs (txA0=0, txA1=1, txB0=2).
	reserved := map[registryreader.ReservedKey]struct{}{
		{From: a, Nonce: 0}: {},
		{From: b, Nonce: 0}: {},
	}
	gas, idx := sumReservedGasUsed(txs, receipts, signer, reserved)
	require.Equal(t, uint64(21_000+40_000), gas)
	require.Equal(t, []uint64{0, 2}, idx)

	// Empty set short-circuits to (0, nil).
	gas, idx = sumReservedGasUsed(txs, receipts, signer, nil)
	require.Zero(t, gas)
	require.Nil(t, idx)
	gas, idx = sumReservedGasUsed(txs, receipts, signer, map[registryreader.ReservedKey]struct{}{})
	require.Zero(t, gas)
	require.Nil(t, idx)

	// A reserved key whose tx has no receipt contributes 0 gas (cannot happen
	// in a valid block; guards against a silent nonzero on a malformed input)
	// but is still reported as reserved by index - classification does not
	// depend on receipt presence.
	onlyA1 := map[registryreader.ReservedKey]struct{}{{From: a, Nonce: 1}: {}}
	gas, idx = sumReservedGasUsed(txs, types.Receipts{{TxHash: txA0.Hash(), GasUsed: 21_000}}, signer, onlyA1)
	require.Zero(t, gas)
	require.Equal(t, []uint64{1}, idx)
}
