package sequencer

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/0xPolygon/sequence-store-proto/commitment"
	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

type headerGateEngine struct {
	*partialReuseEngine
	reject             bool
	sawSpeculativeBase bool
	block              <-chan struct{}
	lookupNumber       uint64
	lookupResult       chan<- *types.Header
}

func (e *headerGateEngine) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	if e.block != nil {
		<-e.block
	}
	if e.lookupResult != nil {
		e.lookupResult <- chain.GetHeaderByNumber(e.lookupNumber)
	}
	if e.reject {
		return errors.New("test header rejected")
	}
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}
	if chain.GetHeader(header.ParentHash, number-1) == nil {
		return consensus.ErrUnknownAncestor
	}
	if number > chain.CurrentHeader().Number.Uint64()+1 {
		e.sawSpeculativeBase = true
	}
	return nil
}

func TestSessionRejectsReconciledSpeculativeParent(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x71}))
	sealed := sealedFromEnv(t, s)
	cur = handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))

	s.consumer.invalidatePendingFromReason(sealed.Number.Uint64(), "reorged")
	handleOK(t, s, openOn(sealed, h.config, cur))

	if s.env != nil || s.parked != nil || s.consumer.PendingBlock() != nil {
		t.Fatal("reconciled speculative lineage was republished")
	}
}

func TestSessionIgnoresEmptyRecord(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x70}))
	before := s.consumer.PendingBlock()
	generation := s.env.generation

	handleOK(t, s, &pb.Entry{Kind: &pb.Entry_Record{Record: &pb.Record{
		PrefixCommitment: s.head.Bytes(),
	}}})

	if s.env == nil || s.env.generation != generation || s.consumer.PendingBlock() != before {
		t.Fatal("empty record changed the pending view")
	}
}

func TestSessionIndexesExecutedReceiptBeforeNextStateCheckpoint(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x73}))
	for nonce := uint64(0); nonce < pendingEagerPublicationTxs; nonce++ {
		tx := h.transfer(t, nonce)
		cur = handleOK(t, s, recordEntry(marshalTransaction(t, tx), cur))
	}
	before := s.consumer.PendingBlock()
	if before == nil {
		t.Fatal("eager pending block is missing")
	}
	if len(before.Transactions()) != pendingEagerPublicationTxs {
		t.Fatalf("eager pending block has %d transactions", len(before.Transactions()))
	}

	tail := h.transfer(t, pendingEagerPublicationTxs)
	handleOK(t, s, recordEntry(marshalTransaction(t, tail), cur))
	after := s.consumer.PendingBlock()
	if after == nil {
		t.Fatal("pending block disappeared")
	}
	if len(after.Transactions()) != pendingEagerPublicationTxs {
		t.Fatalf("state checkpoint advanced to %d transactions", len(after.Transactions()))
	}
	if _, receipt, ok := s.consumer.LookupPreconf(tail.Hash()); !ok || receipt == nil {
		t.Fatal("successfully executed tail receipt was not indexed")
	}
	if s.env.indexedTxs != pendingEagerPublicationTxs+1 || s.env.publishedTxs != pendingEagerPublicationTxs {
		t.Fatalf("indexed=%d published=%d", s.env.indexedTxs, s.env.publishedTxs)
	}
}

func TestSessionIndexesLargeRecordIncrementally(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x74}))

	rawTransactions := make([][]byte, pendingReceiptPublicationBatch+1)
	txs := make(types.Transactions, len(rawTransactions))
	for nonce := range rawTransactions {
		txs[nonce] = h.transfer(t, uint64(nonce))
		rawTransactions[nonce] = marshalTransaction(t, txs[nonce])
	}
	if !s.applyPreparedTransactions(rawTransactions, nil) {
		t.Fatal("apply record")
	}

	for _, tx := range txs[:pendingReceiptPublicationBatch] {
		if _, receipt, ok := s.consumer.LookupPreconf(tx.Hash()); !ok || receipt == nil {
			t.Fatalf("batched receipt %s was not indexed", tx.Hash())
		}
	}
	if _, _, ok := s.consumer.LookupPreconf(txs[pendingReceiptPublicationBatch].Hash()); ok {
		t.Fatal("incomplete receipt batch was indexed")
	}
	if s.env.indexedTxs != pendingReceiptPublicationBatch {
		t.Fatalf("indexed=%d, want %d", s.env.indexedTxs, pendingReceiptPublicationBatch)
	}
}

func TestSessionRejectsOversizedStreamFields(t *testing.T) {
	s := new(session)
	prefix := make([]byte, len(commitment.Head{}))
	parent := make([]byte, common.HashLength)

	open := &pb.Entry{Kind: &pb.Entry_BlockOpen{BlockOpen: &pb.BlockOpen{
		PrefixCommitment: prefix,
		ParentHash:       parent,
		BaseFee:          make([]byte, pendingOpenBaseFeeLimit+1),
	}}}
	if _, _, err := s.fold(open); err == nil {
		t.Fatal("oversized base fee was accepted")
	}
	open.GetBlockOpen().BaseFee = make([]byte, pendingOpenBaseFeeLimit)
	if _, _, err := s.fold(open); err != nil {
		t.Fatalf("maximum-sized base fee was rejected: %v", err)
	}

	seal := &pb.Entry{Kind: &pb.Entry_BlockSeal{BlockSeal: &pb.BlockSeal{
		PrefixCommitment: prefix,
		Header:           make([]byte, pendingSealHeaderLimit+1),
	}}}
	if _, _, err := s.fold(seal); err == nil {
		t.Fatal("oversized seal header was accepted")
	}
	seal.GetBlockSeal().Header = make([]byte, pendingSealHeaderLimit)
	if _, _, err := s.fold(seal); err != nil {
		t.Fatalf("maximum-sized seal header was rejected: %v", err)
	}
	seal.GetBlockSeal().Header = nil
	if _, _, err := s.fold(seal); err == nil {
		t.Fatal("empty seal header was accepted")
	}
}

func TestSessionAllowsSpeculativeParentCanonicalizedBeforePublish(t *testing.T) {
	for _, test := range []struct {
		name           string
		canonicalFirst bool
	}{
		{name: "during handoff"},
		{name: "after handoff", canonicalFirst: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := startExecHarness(t)
			s := h.session()
			cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x72}))
			sealed := sealedFromEnv(t, s)
			handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))
			parentBlock, parentReceipts := s.consumer.PendingBlockAndReceipts()
			open := openOn(sealed, h.config, s.head).GetBlockOpen()

			parent, cacheable, ok := s.resolveOpenParent(sealed.Hash(), open.GetBlockNumber())
			if !ok || cacheable {
				t.Fatalf("speculative parent resolution = %v, %v", ok, cacheable)
			}
			statedb := s.parked
			s.parked = nil
			s.setEnv(newBlockEnv(h.chain, statedb, open, s.sealed))
			s.env.cacheable = false
			block, payload, ok := preparePending(s.env, s.env.header, common.Hash{}, nil)
			if !ok {
				t.Fatal("prepare child")
			}
			if _, ok := s.consumer.ClaimPreconf(parentBlock); !ok {
				t.Fatal("claim parent")
			}
			s.consumer.CompletePreconf(parentBlock, parentReceipts, true)
			h.chain.SetPreconfProvider(s.consumer)
			if test.canonicalFirst {
				if _, err := h.chain.InsertChain(types.Blocks{parentBlock}, false); err != nil {
					t.Fatalf("canonicalize parent: %v", err)
				}
				s.consumer.handleCanonicalHead()
				if s.consumer.handoff.Load() != nil {
					t.Fatal("canonical handoff was not cleared")
				}
				if !s.retryCanonicalOpen(block, payload, parent.Hash(), open.GetBlockNumber()) {
					t.Fatal("canonical parent was skipped after its handoff cleared")
				}
			} else {
				importDone := make(chan error, 1)
				go func() {
					time.Sleep(20 * time.Millisecond)
					_, err := h.chain.InsertChain(types.Blocks{parentBlock}, false)
					importDone <- err
				}()
				s.publishOpen(block, payload, parent.Hash(), open.GetBlockNumber(), false)
				if err := <-importDone; err != nil {
					t.Fatalf("canonicalize parent: %v", err)
				}
			}

			if pending := s.consumer.PendingBlock(); pending == nil || pending.ParentHash() != sealed.Hash() {
				t.Fatalf("pending child = %v", pending)
			}
		})
	}
}

func TestSessionSealVerificationUsesSpeculativeLineage(t *testing.T) {
	newHarness := func(t *testing.T) (*execHarness, *headerGateEngine) {
		t.Helper()
		engine := &headerGateEngine{partialReuseEngine: &partialReuseEngine{Ethash: ethash.NewFullFaker()}}
		return startExecHarnessEngine(t, finalizationConfig(), vm.Config{}, engine), engine
	}

	t.Run("rejected header clears the view", func(t *testing.T) {
		h, engine := newHarness(t)
		s := h.session()
		cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x91}))
		tx := h.transfer(t, 0)
		raw, _ := tx.MarshalBinary()
		cur = handleOK(t, s, recordEntry(raw, cur))
		sealed := sealedFromEnv(t, s)
		engine.reject = true
		handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))

		if s.env != nil || s.parked != nil || s.consumer.PendingBlock() != nil {
			t.Fatal("rejected header retained speculative work")
		}
		if _, _, ok := s.consumer.Index().Lookup(tx.Hash()); ok {
			t.Fatal("rejected header retained its receipt")
		}
	})

	t.Run("second header resolves the first", func(t *testing.T) {
		h, engine := newHarness(t)
		s := h.session()
		cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x92}))
		first := sealedFromEnv(t, s)
		cur = handleOK(t, s, sealEntry(encodeHeader(t, first), cur))
		cur = handleOK(t, s, openOn(first, h.config, cur))
		second := sealedFromEnv(t, s)
		handleOK(t, s, sealEntry(encodeHeader(t, second), cur))

		if !engine.sawSpeculativeBase || s.tip != second.Hash() {
			t.Fatalf("speculative parent verification = %v, tip = %s", engine.sawSpeculativeBase, s.tip)
		}
	})

	t.Run("slow verification keeps the view unsealed", func(t *testing.T) {
		gate := make(chan struct{})
		engine := &headerGateEngine{partialReuseEngine: &partialReuseEngine{Ethash: ethash.NewFullFaker()}, block: gate}
		h := startExecHarnessEngine(t, finalizationConfig(), vm.Config{}, engine)
		s := h.session()
		cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x93}))
		tx := h.transfer(t, 0)
		raw, _ := tx.MarshalBinary()
		cur = handleOK(t, s, recordEntry(raw, cur))
		sealed := sealedFromEnv(t, s)

		previous := preconfSealVerifyTimeout
		preconfSealVerifyTimeout = time.Millisecond
		defer func() { preconfSealVerifyTimeout = previous }()
		handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))
		close(gate)

		if s.env != nil || s.parked == nil || s.tip != sealed.Hash() {
			t.Fatalf("deferred seal state = env:%v parked:%v tip:%s", s.env, s.parked != nil, s.tip)
		}
		receipt, _, ok := s.consumer.Index().Lookup(tx.Hash())
		if !ok || receipt.BlockHash != (common.Hash{}) {
			t.Fatalf("deferred seal receipt = %v, found = %v", receipt, ok)
		}
	})

	t.Run("slow verification owns its lineage snapshot", func(t *testing.T) {
		engine := &headerGateEngine{partialReuseEngine: &partialReuseEngine{Ethash: ethash.NewFullFaker()}}
		h := startExecHarnessEngine(t, finalizationConfig(), vm.Config{}, engine)
		s := h.session()
		cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x95}))
		sealed := sealedFromEnv(t, s)

		lookupNumber := uint64(10_000)
		expected := &types.Header{Number: new(big.Int).SetUint64(lookupNumber), Extra: []byte{0x95}}
		s.verified = map[uint64]*types.Header{lookupNumber: expected}
		gate := make(chan struct{})
		lookupResult := make(chan *types.Header, 1)
		engine.block = gate
		engine.lookupNumber = lookupNumber
		engine.lookupResult = lookupResult

		previous := preconfSealVerifyTimeout
		preconfSealVerifyTimeout = time.Millisecond
		defer func() { preconfSealVerifyTimeout = previous }()
		handleOK(t, s, sealEntry(encodeHeader(t, sealed), cur))

		delete(s.verified, lookupNumber)
		close(gate)
		select {
		case got := <-lookupResult:
			if got == nil || got.Hash() != expected.Hash() {
				t.Fatalf("verification lineage header = %v, want %s", got, expected.Hash())
			}
		case <-time.After(time.Second):
			t.Fatal("verification did not resume")
		}
		waitFor(t, time.Second, func() bool { return !s.consumer.sealVerify.Load() })
	})
}

func TestSessionReanchorsAfterDeferredSprintSeal(t *testing.T) {
	gate := make(chan struct{})
	config := finalizationConfig()
	config.Sprint = map[string]uint64{"0": 4}
	engine := &headerGateEngine{partialReuseEngine: &partialReuseEngine{Ethash: ethash.NewFullFaker()}, block: gate}
	h := startExecHarnessEngine(t, config, vm.Config{}, engine)
	s := h.session()

	cur := handleOK(t, s, openOn(h.chain.CurrentBlock(), h.config, commitment.Head{0x94}))
	tx := h.transfer(t, 0)
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	cur = handleOK(t, s, recordEntry(raw, cur))
	sealed := sealedFromEnv(t, s)

	previous := preconfSealVerifyTimeout
	preconfSealVerifyTimeout = time.Millisecond
	defer func() { preconfSealVerifyTimeout = previous }()
	defer close(gate)

	err = s.handle(sealEntry(encodeHeader(t, sealed), cur))
	if !errors.Is(err, errPreconfReanchor) {
		t.Fatalf("handle deferred sprint seal: %v", err)
	}
	if s.env != nil || s.parked != nil || s.consumer.PendingBlock() != nil {
		t.Fatal("deferred sprint seal retained speculative state")
	}
	if _, _, ok := s.consumer.Index().Lookup(tx.Hash()); ok {
		t.Fatal("deferred sprint seal retained its receipt")
	}
}

func TestValidateOpenExecutionContext(t *testing.T) {
	h := startExecHarness(t)
	parent := h.chain.CurrentBlock()
	valid := openOn(parent, h.config, commitment.Head{}).GetBlockOpen()
	if err := validateOpenExecutionContext(h.chain, parent, valid); err != nil {
		t.Fatalf("valid context: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*pb.BlockOpen)
	}{
		{name: "parent relative gas limit", mutate: func(open *pb.BlockOpen) { open.GasLimit = parent.GasLimit * 2 }},
		{name: "maximum gas limit", mutate: func(open *pb.BlockOpen) { open.GasLimit = params.MaxGasLimit + 1 }},
		{name: "base fee", mutate: func(open *pb.BlockOpen) {
			open.BaseFee = new(big.Int).Add(new(big.Int).SetBytes(open.BaseFee), common.Big1).Bytes()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := proto.Clone(valid).(*pb.BlockOpen)
			test.mutate(candidate)
			if err := validateOpenExecutionContext(h.chain, parent, candidate); err == nil {
				t.Fatal("invalid execution context was accepted")
			}
		})
	}
}

func TestSpeculativeHeaderChainLookups(t *testing.T) {
	h := startExecHarness(t)
	canonical := h.chain.CurrentBlock()
	speculative := types.CopyHeader(canonical)
	speculative.Number = new(big.Int).Add(canonical.Number, common.Big1)
	speculative.ParentHash = canonical.Hash()
	chain := &speculativeHeaderChain{
		ChainHeaderReader: h.chain,
		headers:           map[uint64]*types.Header{speculative.Number.Uint64(): speculative},
	}

	if got := chain.GetHeaderByNumber(speculative.Number.Uint64()); got != speculative {
		t.Fatalf("speculative header by number = %v", got)
	}
	if got := chain.GetHeaderByHash(speculative.Hash()); got != speculative {
		t.Fatalf("speculative header by hash = %v", got)
	}
	if got := chain.GetHeaderByNumber(canonical.Number.Uint64()); got.Hash() != canonical.Hash() {
		t.Fatalf("canonical header by number = %v", got)
	}
	if got := chain.GetHeaderByHash(canonical.Hash()); got.Hash() != canonical.Hash() {
		t.Fatalf("canonical header by hash = %v", got)
	}
}

func TestPublishSealDropsOldSpeculativeHeaders(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	stateDB, err := h.chain.StateAt(h.chain.CurrentBlock().Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	header := &types.Header{Number: big.NewInt(300), ParentHash: h.chain.CurrentBlock().Hash(), GasLimit: 30_000_000}
	block := types.NewBlockWithHeader(header)
	payload, ok := makePendingPayload(block, nil, stateDB, nil)
	if !ok {
		t.Fatal("pending payload")
	}
	payload.finalized = true
	s.env = &blockEnv{header: header, statedb: stateDB, generation: s.consumer.pendingStore().begin(300, header.ParentHash, false)}
	s.sealed = map[uint64]common.Hash{1: common.HexToHash("0x1")}
	s.verified = map[uint64]*types.Header{1: {Number: big.NewInt(1)}}
	s.publishSeal(block, payload, header, block.Hash())
	if _, ok := s.sealed[1]; ok {
		t.Fatal("old speculative hash was retained")
	}
	if _, ok := s.verified[1]; ok {
		t.Fatal("old speculative header was retained")
	}
}

func TestBlockEnvCollectsStatelessWitness(t *testing.T) {
	h := startExecHarnessVM(t, vm.Config{StatelessSelfValidation: true})
	head := h.chain.CurrentBlock()

	statedb, err := h.chain.StateAt(head.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	open := openOn(head, h.config, commitment.Head{0x01})
	env := newBlockEnv(h.chain, statedb, open.GetBlockOpen(), nil)
	witness := env.statedb.Witness()
	if witness == nil {
		t.Fatal("stateless self-validation did not attach a witness")
	}
	if len(witness.Headers) != 1 || witness.Headers[0].Hash() != head.Hash() {
		t.Fatalf("witness parent headers = %v, want [%s]", witness.Headers, head.Hash())
	}
}

func TestBlockEnvSpeculativeBlockhash(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock()
	number := head.Number.Uint64()

	statedb, err := h.chain.StateAt(head.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	speculativeHash := common.Hash{0x5e, 0xed}
	open := &pb.BlockOpen{
		BlockNumber:    number + 2,
		BlockTimestamp: head.Time + 4,
		ParentHash:     speculativeHash.Bytes(),
		GasLimit:       head.GasLimit,
		BaseFee:        big.NewInt(params.InitialBaseFee).Bytes(),
	}

	env := newBlockEnv(h.chain, statedb, open, map[uint64]common.Hash{number + 1: speculativeHash})

	if got := env.evm.Context.GetHash(number + 1); got != speculativeHash {
		t.Fatalf("speculative ancestor resolved to %s", got)
	}
	if got := env.evm.Context.GetHash(1); got != h.chain.GetCanonicalHash(1) {
		t.Fatalf("canonical ancestor resolved to %s", got)
	}
	if got := env.evm.Context.GetHash(number + 10); got != (common.Hash{}) {
		t.Fatalf("unknown height resolved to %s", got)
	}
}
