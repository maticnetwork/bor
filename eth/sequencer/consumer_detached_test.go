package sequencer

import (
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/trie"
)

type detachedSessionFixture struct {
	h      *execHarness
	s      *session
	block  *types.Block
	txs    types.Transactions
	stream commitment.Head
}

type detachedSealGateEngine struct {
	*partialReuseEngine
	entered chan<- struct{}
	release <-chan struct{}
}

func (e *detachedSealGateEngine) VerifyHeader(consensus.ChainHeaderReader, *types.Header) error {
	e.entered <- struct{}{}
	<-e.release
	return nil
}

func newDetachedSessionFixture(t *testing.T) *detachedSessionFixture {
	t.Helper()

	h := partialReuseHarness(t)
	txs := types.Transactions{h.transfer(t, 0), h.transfer(t, 1)}
	block, receipts := buildPartialReuseBlock(t, h, txs)
	s := h.session()
	stream := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0xd1}))
	stream = handleOK(t, s, recordEntry(marshalTransaction(t, txs[0]), stream))

	if reason := s.consumer.CompletePreconf(block, receipts, true); reason != "" {
		t.Fatalf("matching canonical completion returned %q", reason)
	}
	assertPendingEntryAbsent(t, s.consumer.pendingStore(), block)
	if s.env == nil {
		t.Fatal("canonical completion cleared the active execution")
	}

	stream = handleOK(t, s, recordEntry(marshalTransaction(t, txs[1]), stream))
	if s.env == nil || detachedCanonicalHash(s.env) != block.Hash() {
		t.Fatal("rejected checkpoint did not detach the active execution")
	}
	assertPendingEntryAbsent(t, s.consumer.pendingStore(), block)
	if pending := s.consumer.PendingBlock(); pending != nil {
		t.Fatalf("suffix checkpoint resurrected pending block %v", pending)
	}
	for _, tx := range txs {
		if _, _, ok := s.consumer.index.Lookup(tx.Hash()); ok {
			t.Fatalf("transaction %s remained in the speculative index", tx.Hash())
		}
	}
	return &detachedSessionFixture{h: h, s: s, block: block, txs: txs, stream: stream}
}

func TestSessionContinuesAfterMatchingCanonicalCompletion(t *testing.T) {
	fixture := newDetachedSessionFixture(t)
	s := fixture.s

	handleOK(t, s, sealEntry(encodeHeader(t, fixture.block.Header()), fixture.stream))
	if s.env != nil || s.parked == nil || s.tip != fixture.block.Hash() {
		t.Fatalf("matching seal state = env:%v parked:%v tip:%s", s.env != nil, s.parked != nil, s.tip)
	}
	if nonce := s.parked.GetNonce(fixture.h.addr); nonce != uint64(len(fixture.txs)) {
		t.Fatalf("parked nonce = %d, want %d", nonce, len(fixture.txs))
	}

	open := openOn(fixture.block.Header(), fixture.h.config, s.head).GetBlockOpen()
	parent, cacheable, ok := s.resolveOpenParent(fixture.block.Hash(), open.GetBlockNumber())
	if !ok || cacheable || parent.Hash() != fixture.block.Hash() {
		t.Fatalf("next parent resolution = parent:%v cacheable:%v ok:%v", parent, cacheable, ok)
	}
	parked := s.parked
	s.parked = nil
	s.setEnv(newBlockEnv(fixture.h.chain, parked, open, s.sealed))
	s.env.cacheable = false
	if s.env.statedb != parked || s.env.header.ParentHash != fixture.block.Hash() {
		t.Fatal("next open did not extend the parked post-state")
	}
	child, payload, ok := preparePending(s.env, s.env.header, common.Hash{}, nil)
	if !ok {
		t.Fatal("prepare next pending block")
	}
	if _, err := fixture.h.chain.InsertChain(types.Blocks{fixture.block}, false); err != nil {
		t.Fatalf("import canonical parent: %v", err)
	}
	s.consumer.handleCanonicalHead()
	s.publishOpen(child, payload, fixture.block.Hash(), open.GetBlockNumber(), false)
	if pending := s.consumer.PendingBlock(); pending == nil || pending.ParentHash() != fixture.block.Hash() {
		t.Fatalf("next pending block = %v", pending)
	}
}

func TestSessionClearsRejectedNoncanonicalGeneration(t *testing.T) {
	h := partialReuseHarness(t)
	txs := types.Transactions{h.transfer(t, 0), h.transfer(t, 1)}
	s := publishPrefix(t, h, txs[:1])
	oldGeneration := s.env.generation
	newGeneration := s.consumer.pendingStore().begin(s.env.header.Number.Uint64(), s.env.header.ParentHash, true)
	if newGeneration == oldGeneration {
		t.Fatal("pending generation was not replaced")
	}

	s.applyRecord(recordEntry(marshalTransaction(t, txs[1]), s.head).GetRecord())
	if s.env != nil || s.parked != nil {
		t.Fatal("noncanonical generation rejection retained speculative execution")
	}
	if _, _, ok := s.consumer.index.Lookup(txs[1].Hash()); ok {
		t.Fatal("rejected generation indexed the suffix receipt")
	}
}

func TestSessionRetainsOnlyMatchingCanonicalTransition(t *testing.T) {
	for _, test := range []struct {
		name   string
		parent func(*types.Block) common.Hash
		keep   bool
	}{
		{name: "matching parent", parent: func(block *types.Block) common.Hash { return block.Hash() }, keep: true},
		{name: "unrelated parent", parent: func(*types.Block) common.Hash { return common.HexToHash("0xbad") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := partialReuseHarness(t)
			s := h.session()
			if !s.consumer.pendingHeadCurrent() {
				t.Fatal("initialize reconciled head")
			}
			parent, _ := buildPartialReuseBlock(t, h, nil)
			if _, err := h.chain.InsertChain(types.Blocks{parent}, false); err != nil {
				t.Fatalf("import parent: %v", err)
			}
			statedb, err := h.chain.StateAt(parent.Root())
			if err != nil {
				t.Fatalf("parent state: %v", err)
			}
			open := openOn(parent.Header(), h.config, commitment.Head{}).GetBlockOpen()
			open.ParentHash = test.parent(parent).Bytes()
			s.setEnv(newBlockEnv(h.chain, statedb, open, nil))
			s.activateEnv()
			s.applyRecord(recordEntry(marshalTransaction(t, h.transfer(t, 0)), s.head).GetRecord())
			if kept := s.env != nil; kept != test.keep {
				t.Fatalf("execution retained = %v, want %v", kept, test.keep)
			}
		})
	}
}

func TestSessionResolvesCanonicalParentDuringFutureHandoff(t *testing.T) {
	h := partialReuseHarness(t)
	s := h.session()
	if !s.consumer.pendingHeadCurrent() {
		t.Fatal("initialize reconciled head")
	}
	parent, _ := buildPartialReuseBlock(t, h, nil)
	if _, err := h.chain.InsertChain(types.Blocks{parent}, false); err != nil {
		t.Fatalf("import parent: %v", err)
	}
	future, _ := buildPartialReuseBlock(t, h, nil)
	s.consumer.BeginPreconfImport(future)

	header, cacheable, ok := s.resolveOpenParent(parent.Hash(), future.NumberU64())
	if !ok || !cacheable || header == nil || header.Hash() != parent.Hash() {
		t.Fatalf("parent resolution = header:%v cacheable:%v ok:%v", header, cacheable, ok)
	}
	if !s.consumer.canonicalHandoffMatches(future.Hash(), future.NumberU64()+1) {
		t.Fatal("reconciling the parent cleared the future canonical handoff")
	}
}

func TestConsumerRetainsDeclinedPrefixWorker(t *testing.T) {
	h := partialReuseHarness(t)
	prefixTx := h.transfer(t, 0)
	s := publishPrefix(t, h, types.Transactions{prefixTx})
	worker := s.consumer.worker.Load()
	txs := make(types.Transactions, 2_000)
	txs[0] = prefixTx
	dummy := types.NewTransaction(1, h.addr, common.Big1, 21_000, common.Big1, nil)
	for index := 1; index < len(txs); index++ {
		txs[index] = dummy
	}
	candidate := types.NewBlock(types.CopyHeader(s.env.header), &types.Body{Transactions: txs}, nil, trie.NewStackTrie(nil))
	s.consumer.BeginPreconfImport(candidate)
	if execution, ok := s.consumer.ClaimPreconfPrefix(candidate); ok || execution != nil {
		t.Fatalf("low-value prefix claim = %+v, %v", execution, ok)
	}
	if s.env == nil || s.consumer.worker.Load() != worker || worker == nil || worker.env.interrupt.Load() {
		t.Fatal("declined prefix did not retain the matching worker")
	}
	next := h.transfer(t, 1)
	s.applyRecord(recordEntry(marshalTransaction(t, next), s.head).GetRecord())
	if _, _, ok := s.consumer.index.Lookup(next.Hash()); !ok {
		t.Fatal("retained worker did not publish the next preconfirmation receipt")
	}
}

func TestSessionClearsDetachedSealMismatch(t *testing.T) {
	fixture := newDetachedSessionFixture(t)
	mismatch := types.CopyHeader(fixture.block.Header())
	mismatch.Extra = append(append([]byte(nil), mismatch.Extra...), 0xff)
	fixture.s.applySeal(sealEntry(encodeHeader(t, mismatch), fixture.stream).GetBlockSeal())

	if fixture.s.env != nil || fixture.s.parked != nil || fixture.s.tip != (common.Hash{}) {
		t.Fatal("detached seal mismatch retained speculative execution")
	}
	if pending := fixture.s.consumer.PendingBlock(); pending != nil {
		t.Fatalf("detached seal mismatch published pending block %v", pending)
	}
}

func TestSessionClearsSealMismatchDuringCanonicalCompletion(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	engine := &detachedSealGateEngine{
		partialReuseEngine: &partialReuseEngine{Ethash: ethash.NewFullFaker()},
		entered:            entered,
		release:            release,
	}
	h := startExecHarnessEngine(t, finalizationConfig(), vm.Config{}, engine)
	txs := types.Transactions{h.transfer(t, 0), h.transfer(t, 1)}
	block, receipts := buildPartialReuseBlock(t, h, txs)
	s := publishPrefix(t, h, txs)
	mismatch := types.CopyHeader(block.Header())
	mismatch.Extra = append(append([]byte(nil), mismatch.Extra...), 0xff)
	seal := sealEntry(encodeHeader(t, mismatch), s.head).GetBlockSeal()
	done := make(chan struct{})
	go func() {
		s.applySeal(seal)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("seal verification did not start")
	}
	s.consumer.CompletePreconf(block, receipts, true)
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("seal verification did not finish")
	}
	if s.env != nil || s.parked != nil || s.tip != (common.Hash{}) {
		t.Fatal("seal mismatch racing canonical completion retained speculative execution")
	}
}

func marshalTransaction(t *testing.T, tx *types.Transaction) []byte {
	t.Helper()
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	return raw
}

func assertPendingEntryAbsent(t *testing.T, store *PendingStore, block *types.Block) {
	t.Helper()
	key := pendingKey{number: block.NumberU64(), parent: block.ParentHash()}
	store.mu.RLock()
	_, exists := store.entries[key]
	store.mu.RUnlock()
	if exists {
		t.Fatal("canonical completion retained the pending entry")
	}
}
