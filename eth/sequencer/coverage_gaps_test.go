package sequencer

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

type failingPendingStateReader struct {
	PendingStateReader
	err error
}

func (r *failingPendingStateReader) NewStateDB() (*state.StateDB, error) {
	return nil, r.err
}

// A tx whose fee cap is below the base fee cannot yield a tip; the derived
// effective price must not panic and still covers the base fee term.
func TestEffectiveGasPriceUnderpricedCap(t *testing.T) {
	key, _ := crypto.GenerateKey()
	signer := types.LatestSignerForChainID(big.NewInt(1))
	to := common.Address{0x01}

	starved := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
		ChainID: big.NewInt(1), To: &to, Gas: 21000,
		GasFeeCap: big.NewInt(10), GasTipCap: big.NewInt(5),
	})

	if got := effectiveGasPrice(starved, big.NewInt(30)); got.Cmp(big.NewInt(30)) != 0 {
		t.Fatalf("starved cap must degrade to the base fee, got %v", got)
	}
}

func TestBigEqualNilHandling(t *testing.T) {
	if !bigEqual(nil, nil) || bigEqual(nil, big.NewInt(1)) || bigEqual(big.NewInt(1), nil) {
		t.Fatal("nil comparisons must be exact")
	}
}

// Entry helpers must reject shapes they cannot represent instead of
// misclassifying them.
func TestEntryHelperEdges(t *testing.T) {
	empty := &pb.Entry{}

	if _, err := foldEntry(commitment.Head{}, empty); err == nil {
		t.Fatal("unknown entry kind must fail the fold")
	}

	if setEntryPrefix(empty, commitment.Head{}) {
		t.Fatal("unknown entry kind must not accept a prefix")
	}

	if entryPrefix(empty) != nil {
		t.Fatal("unknown entry kind carries no prefix")
	}

	if _, _, err := refoldEntry(commitment.Head{}, journalItem{entry: empty}); err == nil {
		t.Fatal("unknown entry kind must not refold")
	}

	open := openEntry(commitment.OpenContext{Number: 1, BaseFee: big.NewInt(1)}, commitment.Head{})
	record := recordEntry([]byte{0x01}, commitment.Head{})
	seal := sealEntry([]byte{0x0a}, commitment.Head{})

	if contentEqual(open, record) || contentEqual(record, seal) || contentEqual(seal, open) {
		t.Fatal("cross-kind entries must never be content-equal")
	}

	short := recordEntry([]byte{0x01}, commitment.Head{})
	short.GetRecord().Transactions = [][]byte{{0x01}, {0x02}}

	if contentEqual(record, short) {
		t.Fatal("records with different tx counts must differ")
	}

	if !contentEqual(seal, sealEntry([]byte{0x0a}, commitment.Head{0xff})) {
		t.Fatal("seal content ignores the prefix commitment")
	}

	if contentEqual(empty, empty) {
		t.Fatal("unknown kinds must never be content-equal")
	}
}

// A consumer pointed at a chain that is not deterministic yet keeps
// retrying without touching the store, and shuts down cleanly.
func TestConsumerRunWaitsForDeterminism(t *testing.T) {
	preRio := startExecHarnessBor(t, &params.BorConfig{
		RioBlock:      big.NewInt(1_000_000),
		BurntContract: map[string]string{"0": "0x000000000000000000000000000000000000dead"},
	})

	consumer, err := NewConsumer("127.0.0.1:1", preRio.chain)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	consumer.Start()
	time.Sleep(50 * time.Millisecond)
	consumer.Close()
}

// A dial target grpc cannot parse fails the session and retries rather
// than crashing the loop.
func TestConsumerSurvivesAnUndialableEndpoint(t *testing.T) {
	rio := startExecHarness(t)

	consumer, err := NewConsumer("bad scheme://\x00", rio.chain)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}

	consumer.Start()
	time.Sleep(50 * time.Millisecond)
	consumer.Close()
}

func TestPendingComparisonFailureBranches(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	if sameTransactions(nil, fixture.block) {
		t.Fatal("nil block matched transactions")
	}
	if sameTransactionPrefix(nil, fixture.block) {
		t.Fatal("nil block matched a transaction prefix")
	}
	if sameExecutionContext(nil, fixture.block.Header()) {
		t.Fatal("nil header matched execution context")
	}
	if result, ok := prefixProcessResult(nil); ok || result != nil {
		t.Fatalf("nil view returned result %+v, %v", result, ok)
	}

	empty := types.NewBlockWithHeader(fixture.block.Header())
	if result, ok := prefixProcessResult(&PendingRPCView{Header: empty.Header(), Block: empty}); ok || result != nil {
		t.Fatalf("empty view returned result %+v, %v", result, ok)
	}

	receipt := cloneReceipt(fixture.receipt)
	receipt.TxHash = common.HexToHash("0xbad")
	view := &PendingRPCView{
		Header: fixture.block.Header(),
		Block:  fixture.block,
		Receipts: map[common.Hash]*types.Receipt{
			fixture.tx.Hash(): receipt,
		},
	}
	if result, ok := prefixProcessResult(view); ok || result != nil {
		t.Fatalf("mismatched receipt returned result %+v, %v", result, ok)
	}
	if sameReceiptPrefix(fixture.block, &PendingRPCView{Receipts: map[common.Hash]*types.Receipt{}}, nil) {
		t.Fatal("missing receipts matched a canonical prefix")
	}
}

func TestPrefixClaimStateFailures(t *testing.T) {
	h := startExecHarness(t)

	t.Run("stale head", func(t *testing.T) {
		store := NewPendingStore(nil)
		_, candidate := publishPrefixWithTail(t, store)
		consumer := &Consumer{chain: h.chain, index: NewIndex(), store: store}
		consumer.reconciled.Store(&types.Header{Number: big.NewInt(99)})
		if execution, ok := consumer.ClaimPreconfPrefix(candidate); ok || execution != nil {
			t.Fatalf("stale claim = %+v, %v", execution, ok)
		}
	})

	for _, test := range []struct {
		name  string
		state func(PendingStateReader) PendingStateReader
	}{
		{name: "missing state", state: func(PendingStateReader) PendingStateReader { return nil }},
		{name: "state error", state: func(reader PendingStateReader) PendingStateReader {
			return &failingPendingStateReader{PendingStateReader: reader, err: errors.New("state unavailable")}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewPendingStore(nil)
			_, candidate := publishPrefixWithTail(t, store)
			key := pendingKey{number: candidate.NumberU64(), parent: candidate.ParentHash()}
			store.mu.Lock()
			entry := store.entries[key]
			entry.RPCView.State = test.state(entry.RPCView.State)
			generation := entry.generation
			store.mu.Unlock()

			consumer := &Consumer{chain: h.chain, index: NewIndex(), store: store}
			session := &session{consumer: consumer}
			env := &blockEnv{generation: generation}
			worker := &preconfWorker{session: session, env: env, generation: generation}
			session.env = env
			session.worker = worker
			consumer.worker.Store(worker)
			if execution, ok := consumer.ClaimPreconfPrefix(candidate); ok || execution != nil {
				t.Fatalf("failed-state claim = %+v, %v", execution, ok)
			}
		})
	}
}

func TestPendingStoreRejectedClaimClearsIndex(t *testing.T) {
	fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
	store := NewPendingStore(nil)
	reusable := &ReusableExecution{
		HeaderHash: fixture.block.Hash(),
		TxRoot:     fixture.block.TxHash(),
		StateDB:    fixture.state.Copy(),
		Result:     &core.ProcessResult{Receipts: types.Receipts{fixture.receipt}},
	}
	generation := store.begin(fixture.block.NumberU64(), fixture.block.ParentHash(), true)
	if !store.publish(fixture.block, types.Receipts{fixture.receipt}, fixture.state, reusable, generation) {
		t.Fatal("publish failed")
	}
	if _, ok := store.ClaimPreconf(fixture.block); !ok {
		t.Fatal("claim failed")
	}
	store.invalidateFromMemory(fixture.block.NumberU64(), "session_lost")
	index := NewIndex()
	index.Add(fixture.tx, fixture.receipt)
	consumer := &Consumer{store: store, index: index}
	consumer.RejectClaimedPreconf(fixture.block)
	if _, _, ok := index.Lookup(fixture.tx.Hash()); ok {
		t.Fatal("rejected claim remained indexed")
	}
}

func TestPendingStoreBeginFailureBranches(t *testing.T) {
	t.Run("same key", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		store := NewPendingStore(db)
		parent := common.HexToHash("0x1")
		if store.begin(3, parent, true) == 0 || store.begin(3, parent, true) == 0 {
			t.Fatal("same-key replacement failed")
		}
		if records := rawdb.ReadInvalidPreconfs(db, 1); len(records) != 1 || records[0].Reason != "superseded" {
			t.Fatalf("same-key invalidations = %+v", records)
		}
	})

	t.Run("full after supersession", func(t *testing.T) {
		db := rawdb.NewMemoryDatabase()
		store := NewPendingStore(db)
		store.entries[pendingKey{number: 3, parent: common.HexToHash("0x1")}] = &pendingEntry{Number: 3}
		for i := 0; i < pendingEntryLimit; i++ {
			key := pendingKey{number: uint64(i + 10), parent: common.BigToHash(big.NewInt(int64(i + 1)))}
			store.entries[key] = &pendingEntry{Number: key.number}
		}
		if generation := store.begin(3, common.HexToHash("0x2"), true); generation != 0 {
			t.Fatalf("full store returned generation %d", generation)
		}
		if records := rawdb.ReadInvalidPreconfs(db, 1); len(records) != 1 || records[0].Reason != "superseded" {
			t.Fatalf("capacity invalidations = %+v", records)
		}
	})
}

func TestPendingStoreRejectsInvalidPrefixResults(t *testing.T) {
	t.Run("receipt gas", func(t *testing.T) {
		store := NewPendingStore(nil)
		_, candidate := publishPrefixWithTail(t, store)
		key := pendingKey{number: candidate.NumberU64(), parent: candidate.ParentHash()}
		store.mu.Lock()
		store.entries[key].RPCView.Header.GasUsed++
		store.mu.Unlock()
		if prefix, ok := store.claimPreconfPrefix(candidate); ok || prefix != nil {
			t.Fatalf("invalid result prefix = %+v, %v", prefix, ok)
		}
	})

	t.Run("state sync", func(t *testing.T) {
		fixture := newPendingRPCCoverageFixture(t, 3, common.HexToHash("0x1"))
		tx := types.NewTx(&types.StateSyncTx{})
		receipt := &types.Receipt{TxHash: tx.Hash(), CumulativeGasUsed: 1, BlockNumber: big.NewInt(3)}
		header := fixture.block.Header()
		header.GasUsed = 1
		block := types.NewBlock(header, &types.Body{Transactions: types.Transactions{tx}}, types.Receipts{receipt}, trie.NewStackTrie(nil))
		tail := types.NewTransaction(1, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
		candidate := block.WithBody(types.Body{Transactions: types.Transactions{tx, tail}})
		store := NewPendingStore(nil)
		generation := store.begin(block.NumberU64(), block.ParentHash(), true)
		if !store.publish(block, types.Receipts{receipt}, fixture.state, nil, generation) {
			t.Fatal("publish failed")
		}
		if prefix, ok := store.claimPreconfPrefix(candidate); ok || prefix != nil {
			t.Fatalf("state-sync prefix = %+v, %v", prefix, ok)
		}
	})
}

func TestExecutionFailureBranches(t *testing.T) {
	canonicalizeProcessResult(nil, nil)
	new(speculativeFinalizationChain).SetStateSync(nil)

	t.Run("header verification", func(t *testing.T) {
		engine := &headerGateEngine{partialReuseEngine: &partialReuseEngine{Ethash: ethash.NewFullFaker()}}
		h := startExecHarnessEngine(t, finalizationConfig(), vm.Config{}, engine)
		parent := h.chain.CurrentBlock()
		statedb, err := h.chain.StateAt(parent.Root)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		env := newBlockEnv(h.chain, statedb, openOn(parent, h.config, commitment.Head{}).GetBlockOpen(), nil)
		sealed := executionSealFromEnv(env)
		engine.reject = true
		if _, _, err := env.finalizeSeal(h.chain, sealed); err == nil {
			t.Fatal("rejected header was finalized")
		}
	})

	t.Run("interrupted after record", func(t *testing.T) {
		s := &session{env: &blockEnv{header: &types.Header{Number: big.NewInt(1)}}}
		s.env.interrupt.Store(true)
		s.executeRecord(&pb.Record{})
		if s.env != nil {
			t.Fatal("interrupted environment remained active")
		}
	})

	t.Run("pending payload unavailable", func(t *testing.T) {
		tx := types.NewTransaction(0, common.Address{1}, big.NewInt(1), 21_000, big.NewInt(1), nil)
		s := &session{
			consumer: &Consumer{},
			env:      &blockEnv{header: &types.Header{Number: big.NewInt(1), GasLimit: 30_000_000}, txs: types.Transactions{tx}},
		}
		s.executeRecord(&pb.Record{})
		if s.env != nil {
			t.Fatal("unpublishable environment remained active")
		}
	})

	t.Run("stale publication", func(t *testing.T) {
		h := startExecHarness(t)
		parent := h.chain.CurrentBlock()
		statedb, err := h.chain.StateAt(parent.Root)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		consumer := &Consumer{chain: h.chain, index: NewIndex(), store: NewPendingStore(nil)}
		consumer.reconciled.Store(&types.Header{Number: big.NewInt(99)})
		tx := h.transfer(t, 0)
		s := &session{
			consumer: consumer,
			env: &blockEnv{
				header:  &types.Header{ParentHash: parent.Hash(), Number: new(big.Int).Add(parent.Number, common.Big1), GasLimit: parent.GasLimit},
				statedb: statedb,
				txs:     types.Transactions{tx},
			},
		}
		s.executeRecord(&pb.Record{})
		if s.env != nil {
			t.Fatal("stale publication retained environment")
		}
	})
}

func TestSessionPublicationFailureBranches(t *testing.T) {
	h := startExecHarness(t)
	parent := h.chain.CurrentBlock()
	statedb, err := h.chain.StateAt(parent.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	header := &types.Header{ParentHash: parent.Hash(), Number: new(big.Int).Add(parent.Number, common.Big1), GasLimit: parent.GasLimit}
	block := types.NewBlockWithHeader(header)
	payload, ok := makePendingPayload(block, nil, statedb, nil)
	if !ok {
		t.Fatal("pending payload rejected")
	}

	t.Run("entry capacity", func(t *testing.T) {
		store := NewPendingStore(nil)
		for i := 0; i < pendingEntryLimit; i++ {
			key := pendingKey{number: 1, parent: common.BigToHash(big.NewInt(int64(i + 1)))}
			store.entries[key] = &pendingEntry{Number: key.number}
		}
		consumer := &Consumer{chain: h.chain, index: NewIndex(), store: store}
		s := &session{consumer: consumer, env: &blockEnv{header: header, statedb: statedb}}
		s.publishOpen(block, payload, parent.Hash(), block.NumberU64(), true)
		if s.env != nil {
			t.Fatal("capacity failure retained environment")
		}
	})

	t.Run("stale seal", func(t *testing.T) {
		consumer := &Consumer{chain: h.chain, index: NewIndex(), store: NewPendingStore(nil)}
		consumer.reconciled.Store(&types.Header{Number: big.NewInt(99)})
		s := &session{consumer: consumer, env: &blockEnv{header: header, statedb: statedb}}
		s.publishSeal(block, payload, header, block.Hash())
		if s.env != nil {
			t.Fatal("stale seal retained environment")
		}
	})
}

func TestValidatePreLondonOpenContext(t *testing.T) {
	h := startExecHarnessVM(t, vm.Config{}, func(config *params.ChainConfig) {
		forkBlock := big.NewInt(1_000_000)
		config.LondonBlock = forkBlock
		config.ArrowGlacierBlock = forkBlock
		config.GrayGlacierBlock = forkBlock
	})
	parent := h.chain.CurrentBlock()
	open := openOn(parent, h.config, commitment.Head{}).GetBlockOpen()
	if err := validateOpenExecutionContext(h.chain, parent, open); err != nil {
		t.Fatalf("pre-London context: %v", err)
	}
}
