package sequencer

import (
	"crypto/ecdsa"
	"math/big"
	"strings"
	"testing"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// execHarness is a real imported chain whose head state speculative blocks
// execute on: a bor chain config (Rio at genesis, deterministic coinbase,
// burnt contract for the London fee path) with one funded account.
type execHarness struct {
	chain  *core.BlockChain
	config *params.ChainConfig
	key    *ecdsa.PrivateKey
	addr   common.Address
	signer types.Signer
	next   *types.Block // generated but not imported; height CurrentBlock+1
}

func startExecHarness(t *testing.T) *execHarness {
	t.Helper()
	return startExecHarnessVM(t, vm.Config{})
}

func startExecHarnessVM(t *testing.T, vmConfig vm.Config, configure ...func(*params.ChainConfig)) *execHarness {
	t.Helper()

	return startExecHarnessConfig(t, &params.BorConfig{
		Sprint:   map[string]uint64{"0": 16},
		RioBlock: big.NewInt(0),
		Coinbase: map[string]string{
			"0": "0x000000000000000000000000000000000000ba5e",
		},
		BurntContract: map[string]string{
			"0": "0x000000000000000000000000000000000000dead",
		},
	}, vmConfig, configure...)
}

func startExecHarnessBor(t *testing.T, bor *params.BorConfig) *execHarness {
	t.Helper()
	return startExecHarnessConfig(t, bor, vm.Config{})
}

func startExecHarnessConfig(t *testing.T, bor *params.BorConfig, vmConfig vm.Config, configure ...func(*params.ChainConfig)) *execHarness {
	t.Helper()
	return startExecHarnessEngine(t, bor, vmConfig, &partialReuseEngine{Ethash: ethash.NewFullFaker()}, configure...)
}

func startExecHarnessEngine(t *testing.T, bor *params.BorConfig, vmConfig vm.Config, engine consensus.Engine, configure ...func(*params.ChainConfig)) *execHarness {
	t.Helper()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	addr := crypto.PubkeyToAddress(key.PublicKey)

	config := *params.TestChainConfig
	config.Bor = bor
	for _, apply := range configure {
		apply(&config)
	}

	genesis := &core.Genesis{
		Config:   &config,
		GasLimit: 30_000_000,
		Alloc: types.GenesisAlloc{
			addr: {Balance: new(big.Int).Mul(big.NewInt(1_000_000), big.NewInt(params.Ether))},
		},
	}

	// Generate one block more than is imported: tests that need a canonical
	// import event (eviction) insert the spare later.
	_, blocks, _ := core.GenerateChainWithGenesis(genesis, engine, 4, nil)

	blockchainConfig := core.DefaultConfig()
	blockchainConfig.VmConfig = vmConfig
	chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, blockchainConfig)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}

	t.Cleanup(chain.Stop)

	if _, err := chain.InsertChain(blocks[:3], false); err != nil {
		t.Fatalf("insert: %v", err)
	}

	return &execHarness{
		chain:  chain,
		config: &config,
		key:    key,
		addr:   addr,
		signer: types.LatestSigner(&config),
		next:   blocks[3],
	}
}

func (h *execHarness) session() *session {
	return newSession(&Consumer{chain: h.chain, index: NewIndex()})
}

func (h *execHarness) transfer(t *testing.T, nonce uint64) *types.Transaction {
	t.Helper()

	return types.MustSignNewTx(h.key, h.signer, &types.LegacyTx{
		Nonce:    nonce,
		To:       &h.addr,
		Gas:      21000,
		GasPrice: big.NewInt(10 * params.GWei),
		Value:    big.NewInt(1),
	})
}

func (h *execHarness) rawTransfer(t *testing.T, nonce uint64) []byte {
	t.Helper()

	raw, err := h.transfer(t, nonce).MarshalBinary()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	return raw
}

// openOn builds a block-open entry extending a parent header.
func openOn(parent *types.Header, config *params.ChainConfig, prefix commitment.Head) *pb.Entry {
	return openEntry(commitment.OpenContext{
		Number:     parent.Number.Uint64() + 1,
		Timestamp:  parent.Time + 2,
		ParentHash: [32]byte(parent.Hash()),
		GasLimit:   parent.GasLimit,
		BaseFee:    eip1559.CalcBaseFee(config, parent),
	}, prefix)
}

// sealedFromEnv builds the sealed header a correct producer would have
// announced for the session's in-progress block: same open context, the
// re-executed gas, receipts root, and state root.
func sealedFromEnv(t *testing.T, s *session) *types.Header {
	t.Helper()

	env := s.env
	if env == nil {
		t.Fatal("no block in progress")
	}

	header := types.CopyHeader(env.header)
	header.Difficulty = big.NewInt(1)
	body := &types.Body{Transactions: append(types.Transactions(nil), env.txs...)}
	assembled, _, _, err := s.consumer.chain.Engine().FinalizeAndAssemble(
		s.consumer.chain, header, env.statedb.Copy(), body, cloneReceipts(env.receipts),
	)
	if err != nil {
		t.Fatalf("finalize test seal: %v", err)
	}
	return assembled.Header()
}

func executionSealFromEnv(env *blockEnv) *types.Header {
	header := types.CopyHeader(env.header)
	header.Root = env.statedb.Copy().IntermediateRoot(true)
	block := types.NewBlock(
		header,
		&types.Body{Transactions: append(types.Transactions(nil), env.txs...)},
		cloneReceipts(env.receipts),
		trie.NewStackTrie(nil),
	)
	return block.Header()
}

func encodeHeader(t *testing.T, header *types.Header) []byte {
	t.Helper()

	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatalf("encode header: %v", err)
	}

	return raw
}

// handleOK drives one entry through the session, failing the test on a
// position error, and returns the advanced head for the next entry.
func handleOK(t *testing.T, s *session, entry *pb.Entry) commitment.Head {
	t.Helper()

	if err := s.handle(entry); err != nil {
		t.Fatalf("handle: %v", err)
	}

	return s.head
}

func TestSessionExecutesConsecutiveSpeculativeBlocks(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	head := h.chain.CurrentBlock()

	cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))

	tx1 := h.transfer(t, 0)
	raw1, _ := tx1.MarshalBinary()
	cur = handleOK(t, s, recordEntry(raw1, cur))

	receipt, _, ok := s.consumer.index.Lookup(tx1.Hash())
	if !ok || receipt.BlockHash != (common.Hash{}) || receipt.Status != types.ReceiptStatusSuccessful {
		t.Fatalf("pre-seal receipt wrong: ok=%v receipt=%+v", ok, receipt)
	}

	sealed1 := sealedFromEnv(t, s)
	cur = handleOK(t, s, sealEntry(encodeHeader(t, sealed1), cur))

	if s.env != nil || s.parked == nil || s.tip != sealed1.Hash() {
		t.Fatal("seal must park the post-state and set the speculative tip")
	}

	receipt, _, _ = s.consumer.index.Lookup(tx1.Hash())
	if receipt.BlockHash != sealed1.Hash() {
		t.Fatalf("sealed receipt carries %s, want %s", receipt.BlockHash, sealed1.Hash())
	}

	cur = handleOK(t, s, openOn(sealed1, h.config, cur))

	if s.env == nil || s.parked != nil {
		t.Fatal("open on an unimported parent did not start execution")
	}

	tx2 := h.transfer(t, 1)
	raw2, _ := tx2.MarshalBinary()
	cur = handleOK(t, s, recordEntry(raw2, cur))

	tx3 := h.transfer(t, 2)
	raw3, _ := tx3.MarshalBinary()
	cur = handleOK(t, s, recordEntry(raw3, cur))

	sealed2 := sealedFromEnv(t, s)
	handleOK(t, s, sealEntry(encodeHeader(t, sealed2), cur))

	for _, tx := range []*types.Transaction{tx2, tx3} {
		receipt, _, ok := s.consumer.index.Lookup(tx.Hash())
		if !ok || receipt.BlockHash != sealed2.Hash() {
			t.Fatalf("speculative receipt = %+v, ok=%v", receipt, ok)
		}
	}
	if s.env != nil || s.parked == nil || s.tip != sealed2.Hash() {
		t.Fatal("second speculative seal did not advance the execution tip")
	}
	if s.sealed[sealed1.Number.Uint64()] != sealed1.Hash() || s.sealed[sealed2.Number.Uint64()] != sealed2.Hash() {
		t.Fatal("sealed hashes must accumulate for BLOCKHASH resolution")
	}

	handleOK(t, s, openOn(head, h.config, s.head))
	for _, tx := range []*types.Transaction{tx1, tx2, tx3} {
		if _, _, ok := s.consumer.index.Lookup(tx.Hash()); ok {
			t.Fatalf("canonical re-anchor retained receipt %s", tx.Hash())
		}
	}
	store := s.consumer.pendingStore()
	store.mu.RLock()
	for key := range store.entries {
		if key.number > head.Number.Uint64()+1 {
			store.mu.RUnlock()
			t.Fatalf("canonical re-anchor retained speculative height %d", key.number)
		}
	}
	store.mu.RUnlock()
}

// Application problems void the speculative work and skip until an open
// re-anchors on a canonical parent.
func TestSessionSkipPaths(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock()

	t.Run("open with unknown parent", func(t *testing.T) {
		s := h.session()
		unknown := &types.Header{
			Number:   big.NewInt(99),
			Time:     head.Time,
			GasLimit: head.GasLimit,
			BaseFee:  big.NewInt(params.InitialBaseFee),
		}
		handleOK(t, s, openOn(unknown, h.config, commitment.Head{0x01}))

		if s.env != nil {
			t.Fatal("unknown parent must skip")
		}
	})

	t.Run("open height is not parent height plus one", func(t *testing.T) {
		s := h.session()

		// Seal a block first so the skip has parked state to void.
		cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
		sealed := sealedFromEnv(t, s)
		cur = handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))

		open := openEntry(commitment.OpenContext{
			Number:     head.Number.Uint64() + 5,
			Timestamp:  head.Time + 2,
			ParentHash: [32]byte(head.Hash()),
			GasLimit:   head.GasLimit,
			BaseFee:    eip1559.CalcBaseFee(h.config, head),
		}, cur)
		handleOK(t, s, open)

		if s.env != nil {
			t.Fatal("wrong height must skip")
		}

		if s.parked != nil {
			t.Fatal("the skip must void the parked speculative state")
		}
	})

	t.Run("producer rebuild replaces the in-progress block", func(t *testing.T) {
		s := h.session()
		cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
		cur = handleOK(t, s, recordEntry(h.rawTransfer(t, 0), cur))

		tx1Hash := h.transfer(t, 0).Hash()
		if _, _, ok := s.consumer.index.Lookup(tx1Hash); !ok {
			t.Fatal("first generation's receipt should be indexed")
		}

		// A second open at the same height: the first generation is voided
		// and the new one re-anchors on the canonical parent.
		cur = handleOK(t, s, openOn(head, h.config, cur))

		if s.env == nil {
			t.Fatal("rebuild on a canonical parent must re-anchor")
		}

		if _, _, ok := s.consumer.index.Lookup(tx1Hash); ok {
			t.Fatal("rebuild must clear the voided generation's receipts")
		}

		cur = handleOK(t, s, recordEntry(h.rawTransfer(t, 0), cur))
		sealed := sealedFromEnv(t, s)
		handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))

		if _, _, ok := s.consumer.index.Lookup(tx1Hash); !ok {
			t.Fatal("rebuilt generation must serve its receipt")
		}
	})

	t.Run("divergent execution voids the block", func(t *testing.T) {
		s := h.session()
		cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
		cur = handleOK(t, s, recordEntry(h.rawTransfer(t, 0), cur))
		// Nonce 9 cannot apply on this state: re-execution diverges.
		handleOK(t, s, recordEntry(h.rawTransfer(t, 9), cur))

		if s.env != nil {
			t.Fatal("failed apply must void the block")
		}

		if _, _, ok := s.consumer.index.Lookup(h.transfer(t, 0).Hash()); ok {
			t.Fatal("voiding must clear the height's receipts")
		}
	})

	t.Run("undecodable transaction bytes", func(t *testing.T) {
		s := h.session()
		cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
		handleOK(t, s, recordEntry([]byte{0xff, 0xfe}, cur))

		if s.env != nil {
			t.Fatal("garbage tx bytes must void the block")
		}
	})

	t.Run("undecodable sealed header", func(t *testing.T) {
		s := h.session()
		cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
		handleOK(t, s, sealEntry([]byte{0x01, 0x02}, cur))

		if s.env != nil {
			t.Fatal("garbage seal must void the block")
		}
	})

	t.Run("seal cross-check failure", func(t *testing.T) {
		s := h.session()
		cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
		cur = handleOK(t, s, recordEntry(h.rawTransfer(t, 0), cur))

		sealed := sealedFromEnv(t, s)
		sealed.GasUsed++
		handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))

		if s.env != nil || s.parked != nil {
			t.Fatal("mismatched seal must void, not park")
		}
	})

	t.Run("records and seals while skipping are ignored", func(t *testing.T) {
		s := h.session()
		cur := handleOK(t, s, recordEntry(h.rawTransfer(t, 0), commitment.Head{0x01}))
		handleOK(t, s, sealEntry(encodeHeader(t, head), cur))

		if s.env != nil || len(s.consumer.index.byHash) != 0 {
			t.Fatal("entries without an open must be no-ops")
		}
	})
}

// checkSeal distinguishes context, gas, receipts, and state divergence.
func TestCheckSealMismatches(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	head := h.chain.CurrentBlock()

	cur := handleOK(t, s, openOn(head, h.config, commitment.Head{0x01}))
	handleOK(t, s, recordEntry(h.rawTransfer(t, 0), cur))

	good := executionSealFromEnv(s.env)
	if err := s.env.checkSeal(good); err != nil {
		t.Fatalf("self-consistent seal must pass: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*types.Header)
		want   string
	}{
		{"number", func(h *types.Header) { h.Number.Add(h.Number, big.NewInt(1)) }, "open context mismatch"},
		{"time", func(h *types.Header) { h.Time++ }, "open context mismatch"},
		{"parent", func(h *types.Header) { h.ParentHash[0] ^= 1 }, "open context mismatch"},
		{"gas limit", func(h *types.Header) { h.GasLimit++ }, "open context mismatch"},
		{"base fee", func(h *types.Header) { h.BaseFee.Add(h.BaseFee, big.NewInt(1)) }, "open context mismatch"},
		{"gas used", func(h *types.Header) { h.GasUsed++ }, "gas used"},
		{"receipts root", func(h *types.Header) { h.ReceiptHash[0] ^= 1 }, "receipts root"},
		{"state root", func(h *types.Header) { h.Root[0] ^= 1 }, "state root"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := types.CopyHeader(good)
			tc.mutate(tampered)

			err := s.env.checkSeal(tampered)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q divergence, got %v", tc.want, err)
			}
		})
	}
}

// BLOCKHASH resolves speculative ancestors from the session's sealed map,
// canonical blocks from the chain, and everything else to zero.
func TestEffectiveGasPrice(t *testing.T) {
	key, _ := crypto.GenerateKey()
	signer := types.LatestSignerForChainID(big.NewInt(1))
	to := common.Address{0x01}

	legacy := types.MustSignNewTx(key, signer, &types.LegacyTx{
		To: &to, Gas: 21000, GasPrice: big.NewInt(40),
	})
	dynamic := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID: big.NewInt(1), To: &to, Gas: 21000,
		GasFeeCap: big.NewInt(100), GasTipCap: big.NewInt(7),
	})

	if got := effectiveGasPrice(legacy, nil); got.Cmp(big.NewInt(40)) != 0 {
		t.Fatalf("nil base fee must fall back to the gas price, got %v", got)
	}

	if got := effectiveGasPrice(dynamic, big.NewInt(30)); got.Cmp(big.NewInt(37)) != 0 {
		t.Fatalf("dynamic fee must be tip plus base, got %v", got)
	}

	if got := effectiveGasPrice(legacy, big.NewInt(30)); got.Cmp(big.NewInt(40)) != 0 {
		t.Fatalf("legacy under base fee must cap at its gas price, got %v", got)
	}
}
