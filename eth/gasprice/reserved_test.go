// Copyright 2024 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package gasprice

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

// reservedGasPriceConfig clones BorUnittestChainConfig and layers the
// reserved-blockspace fork on top at forkBlock, satisfying
// checkReservedBlockspaceForkOrder (Cancun and Giugliano at genesis,
// ReservedRegistryContract inherited non-empty). Mirrors the shape of
// core/reserved_fee_test.go's reservedTestConfig without importing core's
// test-only helpers.
func reservedGasPriceConfig(forkBlock *big.Int) *params.ChainConfig {
	cc := *params.BorUnittestChainConfig
	bor := *cc.Bor
	cc.CancunBlock = big.NewInt(0)
	bor.GiuglianoBlock = big.NewInt(0)
	bor.ReservedBlockspaceBlock = forkBlock
	cc.Bor = &bor
	return &cc
}

// buildReservedHeader constructs a post-Cancun header (the RLP-encoded
// BlockExtraData layout carrying the reserved-blockspace fields is only
// active from Cancun on) with the given gas accounting and, when
// reservedGasUsed is non-nil, the reserved-blockspace header fields. Mirrors
// core/reserved_validation_test.go's headerWithReservedFields without
// importing core's test-only helpers.
func buildReservedHeader(t *testing.T, number, gasLimit, gasUsed uint64, reservedGasUsed, reservedCapacity *uint64) *types.Header {
	t.Helper()

	enc, err := rlp.EncodeToBytes(&types.BlockExtraData{TxDependency: [][]uint64{}})
	if err != nil {
		t.Fatalf("encode block extra data: %v", err)
	}

	extra := make([]byte, types.ExtraVanityLength)
	extra = append(extra, enc...)
	extra = append(extra, make([]byte, types.ExtraSealLength)...)

	h := &types.Header{
		Number:   new(big.Int).SetUint64(number),
		GasLimit: gasLimit,
		GasUsed:  gasUsed,
		BaseFee:  new(big.Int),
		Extra:    extra,
	}

	if reservedGasUsed != nil {
		var capacity uint64
		if reservedCapacity != nil {
			capacity = *reservedCapacity
		}
		// The headers this helper builds carry the pre-Austin wire shape, so
		// the write goes through a config without Austin scheduled.
		if err := h.SetReservedFields(&params.ChainConfig{ChainID: big.NewInt(137), CancunBlock: big.NewInt(0)}, *reservedGasUsed, capacity); err != nil {
			t.Fatalf("set reserved fields: %v", err)
		}
	}

	return h
}

func ptrUint64(v uint64) *uint64 { return &v }

// receiptsFor builds Receipts for txs via DeriveFields, marking reservedIdx
// positions reserved (fee-free, EffectiveGasPrice 0) exactly as the
// persisted classification does at read time.
func receiptsFor(t *testing.T, config *params.ChainConfig, block *types.Block, txs []*types.Transaction, reservedIdx []uint64) types.Receipts {
	t.Helper()

	receipts := make(types.Receipts, len(txs))
	var cumulative uint64
	for i, tx := range txs {
		cumulative += tx.Gas()
		receipts[i] = &types.Receipt{Type: tx.Type(), CumulativeGasUsed: cumulative}
	}

	if err := receipts.DeriveFields(config, block.Hash(), block.NumberU64(), block.Time(), block.BaseFee(), nil, txs, reservedIdx); err != nil {
		t.Fatalf("derive fields: %v", err)
	}

	return receipts
}

// fakeOracleBackend is a hand-rolled OracleBackend over synthetic data: the
// existing testBackend in gasprice_test.go builds its chain with
// consensus/ethash and core.GenerateChainWithGenesis, which has no hook to
// stamp the reserved-blockspace header fields, so it cannot carry RBS
// headers.
type fakeOracleBackend struct {
	config           *params.ChainConfig
	blocks           map[uint64]*types.Block
	head             uint64
	receipts         map[common.Hash]types.Receipts
	receiptsErr      map[common.Hash]error
	getReceiptsCalls int
}

func (b *fakeOracleBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error) {
	block, err := b.BlockByNumber(ctx, number)
	if block == nil {
		return nil, err
	}
	return block.Header(), nil
}

func (b *fakeOracleBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error) {
	n := uint64(number)
	if number == rpc.LatestBlockNumber {
		n = b.head
	}
	block, ok := b.blocks[n]
	if !ok {
		return nil, nil
	}
	return block, nil
}

func (b *fakeOracleBackend) GetReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	b.getReceiptsCalls++
	if err, ok := b.receiptsErr[hash]; ok {
		return nil, err
	}
	return b.receipts[hash], nil
}

func (b *fakeOracleBackend) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	return nil, nil, nil
}

func (b *fakeOracleBackend) ChainConfig() *params.ChainConfig {
	return b.config
}

func (b *fakeOracleBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return nil
}

// --- Case 1: SuggestTipCap excludes a fallback-fee reserved transaction. ---

func TestSuggestTipCapExcludesReservedTx(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(1))
	keyReserved, _ := crypto.GenerateKey()
	keyNormal, _ := crypto.GenerateKey()
	to := common.HexToAddress("0xaa")

	buildBlock := func(includeReserved bool) (*types.Block, types.Receipts) {
		gasUsed := uint64(21000)
		var reservedGasUsed *uint64
		if includeReserved {
			reservedGasUsed = ptrUint64(21000)
			gasUsed += 21000
		}
		header := buildReservedHeader(t, 1, 30_000_000, gasUsed, reservedGasUsed, nil)
		signer := types.MakeSigner(config, header.Number, header.Time)

		var txs []*types.Transaction
		var reservedIdx []uint64
		if includeReserved {
			// Fallback-fee reserved tx: declares a high tip (well above the
			// hard-enforced 25 gwei ignore price) but executed fee-free, so
			// its receipt carries EffectiveGasPrice 0.
			reservedTx := types.MustSignNewTx(keyReserved, signer, &types.DynamicFeeTx{
				ChainID:   config.ChainID,
				Nonce:     0,
				To:        &to,
				Gas:       21000,
				GasFeeCap: big.NewInt(2000 * params.GWei),
				GasTipCap: big.NewInt(1000 * params.GWei),
			})
			txs = append(txs, reservedTx)
			reservedIdx = []uint64{0}
		}
		normalTx := types.MustSignNewTx(keyNormal, signer, &types.DynamicFeeTx{
			ChainID:   config.ChainID,
			Nonce:     0,
			To:        &to,
			Gas:       21000,
			GasFeeCap: big.NewInt(60 * params.GWei),
			GasTipCap: big.NewInt(30 * params.GWei),
		})
		txs = append(txs, normalTx)

		block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
		receipts := receiptsFor(t, config, block, txs, reservedIdx)
		return block, receipts
	}

	withReserved, receiptsWithReserved := buildBlock(true)
	withoutReserved, receiptsWithoutReserved := buildBlock(false)

	backendA := &fakeOracleBackend{
		config:   config,
		blocks:   map[uint64]*types.Block{1: withReserved},
		head:     1,
		receipts: map[common.Hash]types.Receipts{withReserved.Hash(): receiptsWithReserved},
	}
	backendB := &fakeOracleBackend{
		config:   config,
		blocks:   map[uint64]*types.Block{1: withoutReserved},
		head:     1,
		receipts: map[common.Hash]types.Receipts{withoutReserved.Hash(): receiptsWithoutReserved},
	}

	// Percentile 100 with two unfiltered values [30, 1000] gwei would pick
	// the max (1000); this only matches the no-reserved-tx run if the
	// reserved tx was actually excluded from the sample.
	oracleCfg := Config{Blocks: 1, Percentile: 100}
	oracleA := NewOracle(backendA, oracleCfg, big.NewInt(0))
	oracleB := NewOracle(backendB, oracleCfg, big.NewInt(0))

	gotA, err := oracleA.SuggestTipCap(t.Context())
	if err != nil {
		t.Fatalf("oracleA.SuggestTipCap: %v", err)
	}
	gotB, err := oracleB.SuggestTipCap(t.Context())
	if err != nil {
		t.Fatalf("oracleB.SuggestTipCap: %v", err)
	}
	if gotA.Cmp(gotB) != 0 {
		t.Fatalf("reserved tx polluted the sample: with-reserved=%v without-reserved=%v", gotA, gotB)
	}
	if want := big.NewInt(30 * params.GWei); gotA.Cmp(want) != 0 {
		t.Fatalf("suggestion = %v, want %v", gotA, want)
	}
}

// --- Case 2: the header fast gate must not trigger GetReceipts. ---

func TestGetBlockValuesFastGateSkipsReceipts(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(1))
	to := common.HexToAddress("0xaa")

	// preForkConfig activates the fork far above the sampled block: even a
	// header that carries reserved fields must not trigger a receipts load
	// pre-fork, because the fields are only consensus-checked once the fork
	// is active.
	preForkConfig := reservedGasPriceConfig(big.NewInt(1000))

	cases := []struct {
		name            string
		config          *params.ChainConfig
		reservedGasUsed *uint64
	}{
		{"absent", config, nil},
		{"zero", config, ptrUint64(0)},
		{"pre-fork header content ignored", preForkConfig, ptrUint64(21000)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			header := buildReservedHeader(t, 1, 30_000_000, 21000, tc.reservedGasUsed, nil)
			signer := types.MakeSigner(tc.config, header.Number, header.Time)
			key, _ := crypto.GenerateKey()
			tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
				ChainID:   tc.config.ChainID,
				Nonce:     0,
				To:        &to,
				Gas:       21000,
				GasFeeCap: big.NewInt(60 * params.GWei),
				GasTipCap: big.NewInt(30 * params.GWei),
			})
			block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: []*types.Transaction{tx}})

			// No receipts registered: if GetReceipts is ever called, the
			// backend still returns cleanly (nil, nil), so the assertion
			// below on the call counter is what actually pins the gate.
			backend := &fakeOracleBackend{
				config: tc.config,
				blocks: map[uint64]*types.Block{1: block},
				head:   1,
			}
			oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 60}, big.NewInt(0))

			result := make(chan results, 1)
			quit := make(chan struct{})
			oracle.getBlockValues(t.Context(), 1, sampleNumber, oracle.ignorePrice, result, quit)
			res := <-result
			if res.err != nil {
				t.Fatalf("getBlockValues error: %v", res.err)
			}
			if backend.getReceiptsCalls != 0 {
				t.Fatalf("GetReceipts called %d times, want 0", backend.getReceiptsCalls)
			}
			if want := big.NewInt(30 * params.GWei); len(res.values) != 1 || res.values[0].Cmp(want) != 0 {
				t.Fatalf("values = %v, want [%v]", res.values, want)
			}
		})
	}
}

// --- Case 3: an overflowed reserved transaction is sampled normally. ---

func TestGetBlockValuesIncludesOverflowedReservedTx(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(1))
	to := common.HexToAddress("0xaa")

	header := buildReservedHeader(t, 1, 30_000_000, 42000, ptrUint64(21000), nil)
	signer := types.MakeSigner(config, header.Number, header.Time)

	keyReserved, _ := crypto.GenerateKey()
	keyOverflow, _ := crypto.GenerateKey()
	reservedTx := types.MustSignNewTx(keyReserved, signer, &types.DynamicFeeTx{
		ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 21000,
		GasFeeCap: big.NewInt(2000 * params.GWei), GasTipCap: big.NewInt(1000 * params.GWei),
	})
	// Overflowed: sent by a reserved-registry client, but the quota was
	// exhausted so it executed in the normal region and paid a real fee -
	// its receipt carries a nonzero EffectiveGasPrice.
	overflowTx := types.MustSignNewTx(keyOverflow, signer, &types.DynamicFeeTx{
		ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 21000,
		GasFeeCap: big.NewInt(80 * params.GWei), GasTipCap: big.NewInt(40 * params.GWei),
	})
	txs := []*types.Transaction{reservedTx, overflowTx}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
	receipts := receiptsFor(t, config, block, txs, []uint64{0})

	backend := &fakeOracleBackend{
		config:   config,
		blocks:   map[uint64]*types.Block{1: block},
		head:     1,
		receipts: map[common.Hash]types.Receipts{block.Hash(): receipts},
	}
	oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 60}, big.NewInt(0))

	result := make(chan results, 1)
	quit := make(chan struct{})
	oracle.getBlockValues(t.Context(), 1, sampleNumber, oracle.ignorePrice, result, quit)
	res := <-result
	if res.err != nil {
		t.Fatalf("getBlockValues error: %v", res.err)
	}
	if want := big.NewInt(40 * params.GWei); len(res.values) != 1 || res.values[0].Cmp(want) != 0 {
		t.Fatalf("values = %v, want [%v] (overflowed tx only)", res.values, want)
	}
}

// --- Case 4: a receipts error or count mismatch degrades to no-exclusion. ---

func TestGetBlockValuesDegradesOnReceiptProblems(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(1))
	to := common.HexToAddress("0xaa")

	header := buildReservedHeader(t, 1, 30_000_000, 42000, ptrUint64(21000), nil)
	signer := types.MakeSigner(config, header.Number, header.Time)

	keyReserved, _ := crypto.GenerateKey()
	keyNormal, _ := crypto.GenerateKey()
	reservedTx := types.MustSignNewTx(keyReserved, signer, &types.DynamicFeeTx{
		ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 21000,
		GasFeeCap: big.NewInt(2000 * params.GWei), GasTipCap: big.NewInt(1000 * params.GWei),
	})
	normalTx := types.MustSignNewTx(keyNormal, signer, &types.DynamicFeeTx{
		ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 21000,
		GasFeeCap: big.NewInt(60 * params.GWei), GasTipCap: big.NewInt(30 * params.GWei),
	})
	txs := []*types.Transaction{reservedTx, normalTx}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})

	assertUnfiltered := func(t *testing.T, backend *fakeOracleBackend) {
		t.Helper()
		oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 60}, big.NewInt(0))
		result := make(chan results, 1)
		quit := make(chan struct{})
		oracle.getBlockValues(t.Context(), 1, sampleNumber, oracle.ignorePrice, result, quit)
		res := <-result
		if res.err != nil {
			t.Fatalf("getBlockValues error: %v", res.err)
		}
		// Degraded to no-exclusion: both the reserved and the normal tip
		// are sampled, ascending, matching the fully unfiltered set.
		want := []int64{30, 1000}
		if len(res.values) != len(want) {
			t.Fatalf("values = %v, want %d entries", res.values, len(want))
		}
		for i, w := range want {
			if wantVal := big.NewInt(w * params.GWei); res.values[i].Cmp(wantVal) != 0 {
				t.Fatalf("values[%d] = %v, want %v", i, res.values[i], wantVal)
			}
		}
	}

	t.Run("GetReceipts error", func(t *testing.T) {
		t.Parallel()
		backend := &fakeOracleBackend{
			config:      config,
			blocks:      map[uint64]*types.Block{1: block},
			head:        1,
			receiptsErr: map[common.Hash]error{block.Hash(): errors.New("boom")},
		}
		assertUnfiltered(t, backend)
	})

	t.Run("receipt count mismatch", func(t *testing.T) {
		t.Parallel()
		// One short of len(txs); even though the single receipt present
		// looks reserved, the count check must short-circuit before it is
		// ever examined.
		receipts := types.Receipts{
			{Type: reservedTx.Type(), CumulativeGasUsed: 21000, EffectiveGasPrice: new(big.Int)},
		}
		backend := &fakeOracleBackend{
			config:   config,
			blocks:   map[uint64]*types.Block{1: block},
			head:     1,
			receipts: map[common.Hash]types.Receipts{block.Hash(): receipts},
		}
		assertUnfiltered(t, backend)
	})
}

// --- Case 5: FeeHistory's normalGasUsedRatio. ---

func TestProcessBlockNormalGasUsedRatio(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(10))
	backend := &fakeOracleBackend{config: config}
	oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 60}, big.NewInt(0))

	t.Run("pre-fork equals gasUsedRatio", func(t *testing.T) {
		header := &types.Header{Number: big.NewInt(1), GasLimit: 1_000_000, GasUsed: 600_000, BaseFee: new(big.Int)}
		bf := &blockFees{blockNumber: 1, header: header}
		oracle.processBlock(bf, nil)

		if bf.results.gasUsedRatio != 0.6 {
			t.Fatalf("gasUsedRatio = %v, want 0.6", bf.results.gasUsedRatio)
		}
		if bf.results.normalGasUsedRatio != bf.results.gasUsedRatio {
			t.Fatalf("normalGasUsedRatio = %v, want gasUsedRatio %v", bf.results.normalGasUsedRatio, bf.results.gasUsedRatio)
		}
	})

	t.Run("post-fork matches the formula", func(t *testing.T) {
		header := buildReservedHeader(t, 10, 1_000_000, 600_000, ptrUint64(200_000), nil)
		bf := &blockFees{blockNumber: 10, header: header}
		oracle.processBlock(bf, nil)

		if bf.results.gasUsedRatio != 0.6 {
			t.Fatalf("gasUsedRatio = %v, want 0.6", bf.results.gasUsedRatio)
		}
		const want = 0.5 // (600_000-200_000) / (1_000_000-200_000)
		if bf.results.normalGasUsedRatio != want {
			t.Fatalf("normalGasUsedRatio = %v, want %v", bf.results.normalGasUsedRatio, want)
		}
	})

	t.Run("reserved gas equal to gas limit reports 0", func(t *testing.T) {
		header := buildReservedHeader(t, 10, 1_000_000, 1_000_000, ptrUint64(1_000_000), nil)
		bf := &blockFees{blockNumber: 10, header: header}
		oracle.processBlock(bf, nil)

		if bf.results.normalGasUsedRatio != 0.0 {
			t.Fatalf("normalGasUsedRatio = %v, want 0", bf.results.normalGasUsedRatio)
		}
	})
}

// --- Case 6: FeeHistory rewards exclude reserved transactions. ---

func TestProcessBlockRewardExcludesReserved(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(1))
	backend := &fakeOracleBackend{config: config}
	oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 60}, big.NewInt(0))

	keyReserved, _ := crypto.GenerateKey()
	keyNormal, _ := crypto.GenerateKey()
	to := common.HexToAddress("0xaa")

	t.Run("excludes reserved and reduces the threshold base", func(t *testing.T) {
		header := buildReservedHeader(t, 1, 1_000_000, 200_000, ptrUint64(100_000), nil)
		signer := types.MakeSigner(config, header.Number, header.Time)
		reservedTx := types.MustSignNewTx(keyReserved, signer, &types.DynamicFeeTx{
			ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 100_000,
			GasFeeCap: big.NewInt(2000 * params.GWei), GasTipCap: big.NewInt(1000 * params.GWei),
		})
		normalTx := types.MustSignNewTx(keyNormal, signer, &types.DynamicFeeTx{
			ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 100_000,
			GasFeeCap: big.NewInt(80 * params.GWei), GasTipCap: big.NewInt(40 * params.GWei),
		})
		txs := []*types.Transaction{reservedTx, normalTx}
		block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
		receipts := receiptsFor(t, config, block, txs, []uint64{0})

		bf := &blockFees{blockNumber: 1, header: block.Header(), block: block, receipts: receipts}
		oracle.processBlock(bf, []float64{100})

		if len(bf.results.reward) != 1 {
			t.Fatalf("reward length = %d, want 1", len(bf.results.reward))
		}
		if want := big.NewInt(40 * params.GWei); bf.results.reward[0].Cmp(want) != 0 {
			t.Fatalf("reward = %v, want %v (the normal tx only)", bf.results.reward[0], want)
		}
	})

	t.Run("all reserved returns the all-zero row", func(t *testing.T) {
		header := buildReservedHeader(t, 1, 1_000_000, 200_000, ptrUint64(200_000), nil)
		signer := types.MakeSigner(config, header.Number, header.Time)
		tx0 := types.MustSignNewTx(keyReserved, signer, &types.DynamicFeeTx{
			ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 100_000,
			GasFeeCap: big.NewInt(2000 * params.GWei), GasTipCap: big.NewInt(1000 * params.GWei),
		})
		tx1 := types.MustSignNewTx(keyNormal, signer, &types.DynamicFeeTx{
			ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 100_000,
			GasFeeCap: big.NewInt(80 * params.GWei), GasTipCap: big.NewInt(40 * params.GWei),
		})
		txs := []*types.Transaction{tx0, tx1}
		block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
		receipts := receiptsFor(t, config, block, txs, []uint64{0, 1})

		bf := &blockFees{blockNumber: 1, header: block.Header(), block: block, receipts: receipts}
		oracle.processBlock(bf, []float64{50})

		if len(bf.results.reward) != 1 || bf.results.reward[0].Sign() != 0 {
			t.Fatalf("reward = %v, want a single all-zero entry", bf.results.reward)
		}
	})
}

// --- Case 7: nil EffectiveGasPrice (pending shape) is treated as not-reserved. ---

func TestNilEffectiveGasPriceTreatedAsNotReserved(t *testing.T) {
	t.Parallel()

	config := reservedGasPriceConfig(big.NewInt(1))
	header := buildReservedHeader(t, 1, 1_000_000, 200_000, ptrUint64(100_000), nil)
	signer := types.MakeSigner(config, header.Number, header.Time)
	to := common.HexToAddress("0xaa")

	keyA, _ := crypto.GenerateKey()
	keyB, _ := crypto.GenerateKey()
	txA := types.MustSignNewTx(keyA, signer, &types.DynamicFeeTx{
		ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 100_000,
		GasFeeCap: big.NewInt(2000 * params.GWei), GasTipCap: big.NewInt(1000 * params.GWei),
	})
	txB := types.MustSignNewTx(keyB, signer, &types.DynamicFeeTx{
		ChainID: config.ChainID, Nonce: 0, To: &to, Gas: 100_000,
		GasFeeCap: big.NewInt(80 * params.GWei), GasTipCap: big.NewInt(40 * params.GWei),
	})
	txs := []*types.Transaction{txA, txB}
	block := types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})

	// Pending-shape receipts: never derived, so EffectiveGasPrice stays nil
	// for every transaction, exactly as the miner leaves them.
	receipts := types.Receipts{
		{Type: txA.Type(), TxHash: txA.Hash(), GasUsed: 100_000},
		{Type: txB.Type(), TxHash: txB.Hash(), GasUsed: 100_000},
	}

	backend := &fakeOracleBackend{
		config:   config,
		blocks:   map[uint64]*types.Block{1: block},
		head:     1,
		receipts: map[common.Hash]types.Receipts{block.Hash(): receipts},
	}
	oracle := NewOracle(backend, Config{Blocks: 1, Percentile: 100}, big.NewInt(0))

	t.Run("sampling", func(t *testing.T) {
		result := make(chan results, 1)
		quit := make(chan struct{})
		oracle.getBlockValues(t.Context(), 1, sampleNumber, oracle.ignorePrice, result, quit)
		res := <-result
		if res.err != nil {
			t.Fatalf("getBlockValues error: %v", res.err)
		}
		if len(res.values) != 2 {
			t.Fatalf("values = %v, want both transactions sampled", res.values)
		}
	})

	t.Run("reward", func(t *testing.T) {
		bf := &blockFees{blockNumber: 1, header: header, block: block, receipts: receipts}
		oracle.processBlock(bf, []float64{100})
		// Neither tx excluded: the percentile walk covers both, landing on
		// txA's 1000 gwei reward. If txA had wrongly been treated as
		// reserved, only txB (40 gwei) would remain.
		if want := big.NewInt(1000 * params.GWei); bf.results.reward[0].Cmp(want) != 0 {
			t.Fatalf("reward = %v, want %v", bf.results.reward[0], want)
		}
	})
}
