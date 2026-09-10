package sequencer

import (
	"math/big"
	"testing"
	"time"

	pb "github.com/0xPolygon/sequence-store-proto/sequencestore/v1"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func TestResolveOpenParentValidatesLineage(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock()
	statedb, err := h.chain.StateAt(head.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	t.Run("canonical height", func(t *testing.T) {
		s := h.session()
		if header, cacheable, ok := s.resolveOpenParent(head.Hash(), head.Number.Uint64()+2); ok || cacheable || header != nil {
			t.Fatalf("invalid canonical lineage returned header %v, cacheable %v, ok %v", header, cacheable, ok)
		}
	})
	t.Run("speculative parent displaced", func(t *testing.T) {
		s := h.session()
		s.tip = common.Hash{9}
		s.tipNumber = head.Number.Uint64()
		s.parked = statedb.Copy()
		if header, cacheable, ok := s.resolveOpenParent(s.tip, s.tipNumber+1); ok || cacheable || header != nil {
			t.Fatalf("displaced speculative parent returned header %v, cacheable %v, ok %v", header, cacheable, ok)
		}
	})
	t.Run("speculative height", func(t *testing.T) {
		s := h.session()
		s.tip = common.Hash{8}
		s.tipNumber = head.Number.Uint64() + 100
		s.tipHeader = head
		s.parked = statedb.Copy()
		if header, cacheable, ok := s.resolveOpenParent(s.tip, s.tipNumber+2); ok || cacheable || header != nil {
			t.Fatalf("invalid speculative height returned header %v, cacheable %v, ok %v", header, cacheable, ok)
		}
	})
	t.Run("missing speculative header", func(t *testing.T) {
		s := h.session()
		s.tip = common.Hash{7}
		s.tipNumber = head.Number.Uint64() + 100
		s.parked = statedb.Copy()
		if header, cacheable, ok := s.resolveOpenParent(s.tip, s.tipNumber+1); ok || cacheable || header != nil {
			t.Fatalf("missing speculative header returned header %v, cacheable %v, ok %v", header, cacheable, ok)
		}
	})
	t.Run("speculative tip", func(t *testing.T) {
		s := h.session()
		s.tip = common.Hash{6}
		s.tipNumber = head.Number.Uint64() + 100
		s.tipHeader = types.CopyHeader(head)
		s.parked = statedb.Copy()
		header, cacheable, ok := s.resolveOpenParent(s.tip, s.tipNumber+1)
		if !ok || cacheable || header != s.tipHeader {
			t.Fatalf("speculative tip returned header %v, cacheable %v, ok %v", header, cacheable, ok)
		}
	})
}

func TestSessionRejectsStaleOpenPublication(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock()

	t.Run("canonical parent advanced", func(t *testing.T) {
		s := h.session()
		s.publishOpen(nil, pendingPayload{}, common.Hash{1}, head.Number.Uint64()+1, true)
		if s.env != nil || s.parked != nil {
			t.Fatal("stale canonical open retained speculative state")
		}
	})
	t.Run("reconciliation anchor stale", func(t *testing.T) {
		s := h.session()
		statedb, err := h.chain.StateAt(head.Root)
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		header := &types.Header{Number: new(big.Int).Add(head.Number, common.Big1), ParentHash: head.Hash(), GasLimit: head.GasLimit}
		block := types.NewBlockWithHeader(header)
		payload, ok := makePendingPayload(block, nil, statedb, nil)
		if !ok {
			t.Fatal("pending payload rejected")
		}
		s.env = &blockEnv{header: header}
		s.parked = statedb
		s.consumer.reconciled.Store(&types.Header{Number: big.NewInt(999)})
		s.publishOpen(block, payload, head.Hash(), block.NumberU64(), false)
		if s.env != nil || s.parked != nil {
			t.Fatal("stale anchor retained speculative state")
		}
	})
}

func TestSessionRejectsInvalidOpenContext(t *testing.T) {
	h := startExecHarness(t)
	s := h.session()
	open := openOn(h.chain.CurrentBlock(), h.config, [32]byte{1}).GetBlockOpen()
	open.GasLimit = params.MaxGasLimit + 1
	s.applyOpen(open)
	if s.env != nil {
		t.Fatal("invalid open context started execution")
	}
}

func TestExecuteRecordStopsOnInterruptAndPublicationFailure(t *testing.T) {
	h := startExecHarness(t)
	header := &types.Header{Number: big.NewInt(10), GasLimit: 30_000_000}

	t.Run("interrupted", func(t *testing.T) {
		s := h.session()
		s.env = &blockEnv{header: types.CopyHeader(header)}
		s.env.interrupt.Store(true)
		s.executeRecord(&pb.Record{Transactions: [][]byte{{1}}})
		if s.env != nil {
			t.Fatal("interrupted execution retained environment")
		}
	})
}

func TestCompletePreconfPrefixRejectsInvalidInputs(t *testing.T) {
	h := startExecHarness(t)
	head := h.chain.CurrentBlock()
	statedb, err := h.chain.StateAt(head.Root)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	header := &types.Header{
		ParentHash: head.Hash(),
		Number:     new(big.Int).Add(head.Number, common.Big1),
		Time:       head.Time + 2,
		GasLimit:   head.GasLimit,
		BaseFee:    big.NewInt(1),
		Difficulty: big.NewInt(1),
	}
	block := types.NewBlockWithHeader(header)
	if execution, err := h.session().consumer.completePreconfPrefix(block, nil); err == nil || execution != nil {
		t.Fatalf("incomplete prefix returned execution %v and error %v", execution, err)
	}
	invalid := &pendingPrefix{
		Transactions: types.Transactions{h.transfer(t, 0)},
		StateDB:      statedb.Copy(),
		Result:       &core.ProcessResult{},
	}
	if env, err := newBlockEnvFromPrefix(h.chain, block, invalid); err == nil || env != nil {
		t.Fatalf("invalid prefix returned environment %v and error %v", env, err)
	}
	stateSyncBlock := block.WithBody(types.Body{Transactions: types.Transactions{types.NewTx(&types.StateSyncTx{})}})
	prefix := &pendingPrefix{StateDB: statedb.Copy(), Result: &core.ProcessResult{}}
	if execution, err := h.session().consumer.completePreconfPrefix(stateSyncBlock, prefix); err == nil || execution != nil {
		t.Fatalf("state-sync suffix returned execution %v and error %v", execution, err)
	}
}

func TestPendingPublicationWorkIsLinear(t *testing.T) {
	const transactionCount = 6000
	now := time.Unix(1, 0)
	env := &blockEnv{header: &types.Header{GasLimit: transactionCount*21_000 + 1}}
	publications := 0
	publishedWork := 0
	lastPublished := 0
	maxGap := 0
	for index := 0; index < transactionCount; index++ {
		now = now.Add(500 * time.Microsecond)
		env.txs = append(env.txs, nil)
		env.header.GasUsed += 21_000
		if !env.shouldPublishPending(now) {
			continue
		}
		publications++
		publishedWork += len(env.txs)
		if gap := len(env.txs) - lastPublished; gap > maxGap {
			maxGap = gap
		}
		lastPublished = len(env.txs)
		env.markPendingPublished(now)
		env.publishedTxs = len(env.txs)
		env.publishedGas = env.header.GasUsed
	}
	if gap := transactionCount - lastPublished; gap > maxGap {
		maxGap = gap
	}
	if publications != 5 || publishedWork != 7_383 || lastPublished != 3_818 || maxGap != 2_182 {
		t.Fatalf("publications = %d, work = %d, last = %d, max gap = %d", publications, publishedWork, lastPublished, maxGap)
	}
	maxPublishedWork := pendingEagerPublicationTxs + pendingRPCPublicationLimit*transactionCount
	if publishedWork > maxPublishedWork {
		t.Fatalf("cumulative pending publication work = %d, want <= %d", publishedWork, maxPublishedWork)
	}
	if env.postEagerPublications != pendingRPCPublicationLimit {
		t.Fatalf("post-eager publications = %d, want %d", env.postEagerPublications, pendingRPCPublicationLimit)
	}
}

func TestPendingPublicationCadence(t *testing.T) {
	now := time.Unix(1, 0)
	tests := []struct {
		name             string
		txs              int
		published        int
		gasUsed          uint64
		publishedGas     uint64
		lastPublishedAt  time.Time
		postPublications int
		want             bool
	}{
		{"unchanged", 17, 17, 21_000, 21_000, now.Add(-pendingRPCPublishFallbackDelay), 0, false},
		{"eager", 16, 15, 21_000, 0, now, pendingRPCPublicationLimit, true},
		{"batched eager catchup", 20, 15, 420_000, 315_000, now, 0, true},
		{"minimum delay", 17, 16, 500_000, 336_000, now.Add(-pendingRPCMinPublishDelay + time.Nanosecond), 0, false},
		{"before time fallback", 17, 16, 357_000, 336_000, now.Add(-pendingRPCPublishFallbackDelay + time.Nanosecond), 0, false},
		{"time fallback", 17, 16, 357_000, 336_000, now.Add(-pendingRPCPublishFallbackDelay), 0, true},
		{"clock moved backwards", 17, 16, 500_000, 336_000, now.Add(time.Nanosecond), 0, false},
		{"time reserve exhausted", 17, 16, 357_000, 336_000, now.Add(-pendingRPCPublishFallbackDelay), pendingRPCTimeFallbackLimit, false},
		{"gas after time reserve", 17, 16, 736_000, 336_000, now.Add(-pendingRPCPublishFallbackDelay), pendingRPCTimeFallbackLimit, true},
		{"budget exhausted", 17, 16, 500_000, 336_000, now.Add(-pendingRPCPublishFallbackDelay), pendingRPCPublicationLimit, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := &blockEnv{
				header:                &types.Header{GasLimit: 1_600_000, GasUsed: test.gasUsed},
				txs:                   make([]*types.Transaction, test.txs),
				publishedTxs:          test.published,
				publishedGas:          test.publishedGas,
				lastPublishedAt:       test.lastPublishedAt,
				postEagerPublications: test.postPublications,
			}
			if got := env.shouldPublishPending(now); got != test.want {
				t.Fatalf("shouldPublishPending = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPendingPublicationGasThreshold(t *testing.T) {
	now := time.Unix(1, 0)
	tests := []struct {
		name         string
		gasLimit     uint64
		gasUsed      uint64
		publishedGas uint64
		want         bool
	}{
		{"minimum below", 1, 20_999, 0, false},
		{"minimum boundary", 1, 21_000, 0, true},
		{"exact below", 1_600_000, 735_999, 336_000, false},
		{"exact boundary", 1_600_000, 736_000, 336_000, true},
		{"rounded below", 1_600_001, 736_000, 336_000, false},
		{"rounded boundary", 1_600_001, 736_001, 336_000, true},
		{"gas regression", 1_600_000, 335_999, 336_000, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := &blockEnv{
				header:          &types.Header{GasLimit: test.gasLimit, GasUsed: test.gasUsed},
				txs:             make([]*types.Transaction, pendingEagerPublicationTxs+1),
				publishedTxs:    pendingEagerPublicationTxs,
				publishedGas:    test.publishedGas,
				lastPublishedAt: now.Add(-pendingRPCMinPublishDelay),
			}
			if got := env.shouldPublishPending(now); got != test.want {
				t.Fatalf("shouldPublishPending = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPendingPublicationTimeFallbackIsBounded(t *testing.T) {
	now := time.Unix(1, 0)
	env := &blockEnv{
		header:          &types.Header{GasLimit: ^uint64(0), GasUsed: 16 * 21_000},
		txs:             make([]*types.Transaction, pendingEagerPublicationTxs),
		publishedTxs:    pendingEagerPublicationTxs,
		publishedGas:    pendingEagerPublicationTxs * 21_000,
		lastPublishedAt: now,
	}
	work := 0
	for index := 1; index <= pendingRPCTimeFallbackLimit; index++ {
		env.txs = append(env.txs, nil)
		env.header.GasUsed += 21_000
		now = now.Add(pendingRPCPublishFallbackDelay)
		if !env.shouldPublishPending(now) || !env.shouldPublishPending(now) {
			t.Fatalf("time publication %d was not ready", index)
		}
		if env.postEagerPublications != index-1 {
			t.Fatal("publication decision consumed the budget")
		}
		env.markPendingPublished(now)
		env.publishedTxs = len(env.txs)
		env.publishedGas = env.header.GasUsed
		work += len(env.txs)
	}
	if work != 35 || env.postEagerPublications != pendingRPCTimeFallbackLimit {
		t.Fatalf("work = %d, publications = %d", work, env.postEagerPublications)
	}
	env.txs = append(env.txs, nil)
	env.header.GasUsed += 21_000
	now = now.Add(pendingRPCPublishFallbackDelay)
	if env.shouldPublishPending(now) {
		t.Fatal("time fallback exceeded its publication budget")
	}
	env.header.GasLimit = 1_600_000
	env.header.GasUsed = env.publishedGas + 400_000
	if !env.shouldPublishPending(now) {
		t.Fatal("time fallback consumed the reserved gas budget")
	}
	env.markPendingPublished(now)
	if env.postEagerPublications != pendingRPCTimeFallbackLimit+1 {
		t.Fatalf("post-eager publications = %d", env.postEagerPublications)
	}
}

func TestPendingPublicationImprovesPartialBlockFreshness(t *testing.T) {
	now := time.Unix(1, 0)
	env := &blockEnv{header: &types.Header{GasLimit: 200_000_000}}
	publications := 0
	lastPublished := 0
	maxGap := 0
	for index := 0; index < 200; index++ {
		now = now.Add(5 * time.Millisecond)
		env.txs = append(env.txs, nil)
		env.header.GasUsed += 21_000
		if !env.shouldPublishPending(now) {
			continue
		}
		publications++
		if gap := len(env.txs) - lastPublished; gap > maxGap {
			maxGap = gap
		}
		lastPublished = len(env.txs)
		env.markPendingPublished(now)
		env.publishedTxs = len(env.txs)
		env.publishedGas = env.header.GasUsed
	}
	if gap := 200 - lastPublished; gap > maxGap {
		maxGap = gap
	}
	if publications != 3 || lastPublished != 96 || maxGap != 104 || env.postEagerPublications != 2 {
		t.Fatalf("publications = %d, last = %d, max gap = %d, post-eager = %d", publications, lastPublished, maxGap, env.postEagerPublications)
	}
}
