package sequencer

import (
	"encoding/binary"
	"errors"
	"math/big"
	"testing"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func finalizationConfig() *params.BorConfig {
	return &params.BorConfig{
		Sprint:   map[string]uint64{"0": 16},
		RioBlock: big.NewInt(0),
		Coinbase: map[string]string{
			"0": "0x000000000000000000000000000000000000ba5e",
		},
		BurntContract: map[string]string{
			"0": "0x000000000000000000000000000000000000dead",
		},
	}
}

func finalizableEnv(t *testing.T) (*execHarness, *blockEnv, *types.Header) {
	t.Helper()

	h := startExecHarnessBor(t, finalizationConfig())
	parent := h.chain.CurrentBlock()
	statedb, err := h.chain.StateAt(parent.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	env := newBlockEnv(h.chain, statedb, openOn(parent, h.config, commitment.Head{}).GetBlockOpen(), nil)
	if _, _, err := env.applyRaw(h.rawTransfer(t, 0)); err != nil {
		t.Fatalf("apply transaction: %v", err)
	}

	header := types.CopyHeader(env.header)
	header.Difficulty = h.chain.Engine().CalcDifficulty(h.chain, header.Time, parent)
	body := &types.Body{Transactions: append(types.Transactions(nil), env.txs...)}
	assembled, _, _, err := h.chain.Engine().FinalizeAndAssemble(h.chain, header, env.statedb.Copy(), body, cloneReceipts(env.receipts))
	if err != nil {
		t.Fatalf("prepare finalized header: %v", err)
	}
	return h, env, assembled.Header()
}

func TestBlockEnvCanonicalHash(t *testing.T) {
	h := startExecHarness(t)
	parent := h.chain.CurrentBlock()
	statedb, err := h.chain.StateAt(parent.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	env := newBlockEnv(h.chain, statedb, openOn(parent, h.config, commitment.Head{}).GetBlockOpen(), nil)
	if got := env.evm.Context.GetHash(parent.Number.Uint64()); got != parent.Hash() {
		t.Fatalf("canonical parent hash = %s, want %s", got, parent.Hash())
	}
}

func TestBlockEnvProcessesPragueParentHash(t *testing.T) {
	config := *params.TestChainConfig
	config.ShanghaiBlock = big.NewInt(0)
	config.CancunBlock = big.NewInt(0)
	config.PragueBlock = big.NewInt(0)
	config.Bor = finalizationConfig()
	genesis := &core.Genesis{
		Config:   &config,
		GasLimit: 30_000_000,
		Alloc: types.GenesisAlloc{
			params.HistoryStorageAddress: {Nonce: 1, Code: params.HistoryStorageCode},
		},
	}
	chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), genesis, ethash.NewFaker(), core.DefaultConfig())
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	t.Cleanup(chain.Stop)

	parent := chain.CurrentBlock()
	statedb, err := chain.StateAt(parent.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	open := openOn(parent, &config, commitment.Head{}).GetBlockOpen()
	env := newBlockEnv(chain, statedb, open, nil)
	var key common.Hash
	binary.BigEndian.PutUint64(key[24:], (open.GetBlockNumber()-1)%params.HistoryServeWindow)
	if got := env.statedb.GetState(params.HistoryStorageAddress, key); got != parent.Hash() {
		t.Fatalf("stored parent hash = %s, want %s", got, parent.Hash())
	}
}

func TestFinalizeSealBuildsReusableExecution(t *testing.T) {
	h, env, sealed := finalizableEnv(t)
	env.rpcView = new(PendingRPCView)
	stateSync := &types.StateSyncData{ID: 1}
	h.chain.SetStateSync([]*types.StateSyncData{stateSync})
	s := &session{consumer: &Consumer{chain: h.chain, index: NewIndex()}, env: env}
	assembled, reusable, verified, ok := s.sealResult(sealed)
	if !ok || !verified {
		t.Fatal("seal result was rejected")
	}
	if assembled.Hash() != sealed.Hash() {
		t.Fatalf("assembled hash = %s, want %s", assembled.Hash(), sealed.Hash())
	}
	if reusable == nil || reusable.HeaderHash != sealed.Hash() || reusable.TxRoot != assembled.TxHash() {
		t.Fatalf("reusable execution = %+v", reusable)
	}
	if reusable.Result == nil || reusable.Result.GasUsed != sealed.GasUsed || len(reusable.Result.Receipts) != 1 {
		t.Fatalf("reusable result = %+v", reusable.Result)
	}
	if receipt := reusable.Result.Receipts[0]; receipt.BlockHash != assembled.Hash() || receipt.BlockNumber.Cmp(assembled.Number()) != 0 {
		t.Fatalf("reusable receipt metadata = hash %s number %v", receipt.BlockHash, receipt.BlockNumber)
	}
	if got := h.chain.GetStateSync(); len(got) != 1 || got[0] != stateSync {
		t.Fatalf("canonical state sync data changed during speculative finalization: %+v", got)
	}
	if got := reusable.StateDB.Copy().IntermediateRoot(true); got != sealed.Root {
		t.Fatalf("reusable root = %s, want %s", got, sealed.Root)
	}
	if env.header.Hash() != sealed.Hash() || env.evm.StateDB != env.statedb {
		t.Fatal("environment was not advanced to finalized state")
	}
	if len(env.txs) != 1 || len(env.receipts) != 1 || env.txs[0].Hash() != reusable.Result.Receipts[0].TxHash {
		t.Fatal("finalized environment body is inconsistent")
	}
	if env.rpcView != nil {
		t.Fatal("finalized environment retained the pre-finalization receipt view")
	}
}

func TestFinalizeSealDoesNotCacheSpeculativeParentState(t *testing.T) {
	h, env, sealed := finalizableEnv(t)
	env.cacheable = false
	assembled, reusable, err := env.finalizeSeal(h.chain, sealed)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if assembled == nil || reusable != nil {
		t.Fatalf("assembled = %v, reusable = %+v", assembled, reusable)
	}
}

func TestFinalizeSealRejectsUnavailableAndDivergentResults(t *testing.T) {
	t.Run("context mismatch", func(t *testing.T) {
		h, env, sealed := finalizableEnv(t)
		env.header.Time++
		if _, _, err := env.finalizeSeal(h.chain, sealed); !errors.Is(err, errSealMismatch) {
			t.Fatalf("error = %v, want %v", err, errSealMismatch)
		}
	})

	t.Run("gas used mismatch", func(t *testing.T) {
		h, env, sealed := finalizableEnv(t)
		sealed.GasUsed++
		if _, _, err := env.finalizeSeal(h.chain, sealed); !errors.Is(err, errSealMismatch) {
			t.Fatalf("error = %v, want %v", err, errSealMismatch)
		}
	})

	for _, tc := range []struct {
		name   string
		sprint map[string]uint64
	}{
		{name: "zero sprint", sprint: map[string]uint64{"0": 0}},
		{name: "sprint boundary", sprint: map[string]uint64{"0": 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, env, sealed := finalizableEnv(t)
			h.chain.Config().Bor.Sprint = tc.sprint
			if _, _, err := env.finalizeSeal(h.chain, sealed); !errors.Is(err, errCachedFinalizationUnavailable) {
				t.Fatalf("error = %v, want %v", err, errCachedFinalizationUnavailable)
			}
		})
	}

	t.Run("assembled mismatch", func(t *testing.T) {
		h, env, sealed := finalizableEnv(t)
		sealed.Root[0] ^= 0xff
		if _, _, err := env.finalizeSeal(h.chain, sealed); !errors.Is(err, errSealMismatch) {
			t.Fatalf("error = %v, want %v", err, errSealMismatch)
		}
	})
}

func TestSealResultFallbackAndMismatch(t *testing.T) {
	t.Run("cache unavailable", func(t *testing.T) {
		h, env, sealed := finalizableEnv(t)
		h.chain.Config().Bor.Sprint = map[string]uint64{"0": 4}
		sealed.Root = env.statedb.IntermediateRoot(env.evm.ChainConfig().IsEIP158(env.header.Number))
		s := &session{consumer: &Consumer{chain: h.chain, index: NewIndex()}, env: env}
		if err := env.checkSeal(sealed); err != nil {
			t.Fatalf("fallback seal check: %v", err)
		}

		assembled, reusable, verified, ok := s.sealResult(sealed)
		if !ok || !verified || assembled == nil || reusable != nil {
			t.Fatalf("assembled = %v, reusable = %v, ok = %v", assembled, reusable, ok)
		}
		_, payload, ok := preparePending(env, assembled.Header(), assembled.Hash(), reusable)
		if !ok || !payload.finalized {
			t.Fatal("execution-only seal was not marked finalized")
		}
	})

	t.Run("cache unavailable with seal mismatch", func(t *testing.T) {
		h, env, sealed := finalizableEnv(t)
		h.chain.Config().Bor.Sprint = map[string]uint64{"0": 4}
		sealed.Root = env.statedb.IntermediateRoot(env.evm.ChainConfig().IsEIP158(env.header.Number))
		sealed.Root[0] ^= 0xff
		s := &session{consumer: &Consumer{chain: h.chain, index: NewIndex()}, env: env}
		if _, _, _, ok := s.sealResult(sealed); ok {
			t.Fatal("divergent seal was accepted")
		}
	})

	t.Run("cache unavailable with body mismatch", func(t *testing.T) {
		h, env, sealed := finalizableEnv(t)
		h.chain.Config().Bor.Sprint = map[string]uint64{"0": 4}
		sealed.Root = env.statedb.IntermediateRoot(env.evm.ChainConfig().IsEIP158(env.header.Number))
		sealed.TxHash[0] ^= 0xff
		s := &session{consumer: &Consumer{chain: h.chain, index: NewIndex()}, env: env}
		if _, _, _, ok := s.sealResult(sealed); ok {
			t.Fatal("divergent body was accepted")
		}
	})
}

func TestRecordInputLimitClearsSpeculativeWork(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{}))
	s.env.inputBytes = pendingInputLimit
	s.applyRecord(&pb.Record{Transactions: [][]byte{{0x01}}})
	if s.env != nil || s.parked != nil {
		t.Fatal("limited record retained speculative work")
	}
}
