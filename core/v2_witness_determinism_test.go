package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// v2StatsHandler scrapes the "V2 block stats" debug record so a test can prove
// the block it replayed actually exercised BlockSTM's conflict path. Without
// that proof a passing determinism check is worthless: a block whose txs never
// conflict never re-executes, so there is no scheduling divergence to detect.
type v2StatsHandler struct {
	slog.Handler

	mu     sync.Mutex
	execs  int64
	vfails int64
	txs    int64
}

func (h *v2StatsHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *v2StatsHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "V2 block stats" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "execs":
			h.execs = a.Value.Int64()
		case "vfails":
			h.vfails = a.Value.Int64()
		case "txs":
			h.txs = a.Value.Int64()
		}
		return true
	})
	return nil
}

func (h *v2StatsHandler) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.execs, h.vfails, h.txs = 0, 0, 0
}

func (h *v2StatsHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *v2StatsHandler) WithGroup(string) slog.Handler      { return h }

// regenerateWitnessV2 replays a block through the production V2 processor with
// witness recording enabled and returns the witness that came out. The setup
// mirrors witnessRegenRoundTrip exactly -- same reader stack, same author
// pre-warm, same post-state root computation -- so the only variable is the
// worker count and the scheduling that follows from it.
func regenerateWitnessV2(pb *preparedBlock, config *params.ChainConfig, engine consensus.Engine, workers int) (*stateless.Witness, error) {
	db := state.NewDatabase(pb.tdb, nil)

	prefetchReader, _, parallelReader, err := db.ReadersWithCacheStatsTriple(pb.witness.Root())
	if err != nil {
		return nil, fmt.Errorf("readers: %w", err)
	}
	if _, err := prefetchReader.Account(pb.author); err != nil {
		return nil, fmt.Errorf("warm author: %w", err)
	}

	finalDB, err := state.NewWithReader(pb.witness.Root(), db, parallelReader)
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}

	hc := &benchHeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine}

	w, err := stateless.NewWitness(pb.block.Header(), hc)
	if err != nil {
		return nil, fmt.Errorf("new witness: %w", err)
	}
	w.Headers = append([]*types.Header{}, pb.witness.Headers...)
	finalDB.SetWitness(w)

	bc := &BlockChain{
		chainConfig: config,
		hc:          &HeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine},
	}
	if _, err := NewV2StateProcessor(hc, bc, workers).Process(pb.block, finalDB, benchVMConfig, &pb.author, context.Background()); err != nil {
		return nil, fmt.Errorf("v2 process: %w", err)
	}

	// Production always computes the post-state root on the V2 statedb, and
	// that is where update- and deletion-path trie nodes reach the witness.
	finalDB.IntermediateRoot(config.IsEIP158(pb.block.Number()))

	return w, nil
}

// TestV2WitnessDeterminism pins the property WIT2's signed-hash check depends
// on: regenerating the witness for one block must yield byte-identical output
// every time, regardless of how many BlockSTM workers ran it.
//
// If this fails, two honest nodes executing the same block publish different
// witness bytes for it. Consumers still execute fine -- a witness that carries
// too much is merely large, not wrong -- but the BP-signed commitment no longer
// identifies the artifact, so byte verification cannot distinguish a peer that
// tampered from one that simply scheduled its workers differently.
func TestV2WitnessDeterminism(t *testing.T) {
	bd, diskdb := loadSingleWitnessRegenBlock(t, singleRegenBlockHex)

	stats := &v2StatsHandler{}
	prev := log.Root()
	log.SetDefault(log.NewLogger(stats))
	defer log.SetDefault(prev)

	assertWitnessDeterministic(t, bd, diskdb, stats, true)
}

// TestV2WitnessDeterminismAllBlocks widens the same check over every witness
// fixture present. The single-block test runs everywhere; this one needs the
// git-lfs set and skips without it, so a clone with lfs gets breadth while a
// clone without it still gets the guard.
func TestV2WitnessDeterminismAllBlocks(t *testing.T) {
	blocks, diskdb := loadEmbeddedBlocks(t)
	if len(blocks) == 0 {
		t.Skip("no embedded blocks available")
	}

	stats := &v2StatsHandler{}
	prev := log.Root()
	log.SetDefault(log.NewLogger(stats))
	defer log.SetDefault(prev)

	contended := 0

	for _, bd := range blocks {
		if len(bd.block.Transactions()) < 20 {
			continue
		}
		t.Run(fmt.Sprintf("block_%d", bd.block.NumberU64()), func(t *testing.T) {
			// Fixtures vary in completeness and contention, so a single block
			// failing either precondition is skipped rather than fatal; the
			// aggregate contention check below keeps the run from being vacuous.
			if assertWitnessDeterministic(t, bd, diskdb, stats, false) {
				contended++
			}
		})
	}

	if contended == 0 {
		t.Fatal("no fixture block re-executed a transaction: this run cannot observe " +
			"scheduling-dependent witness divergence")
	}

	t.Logf("blocks exercising BlockSTM re-execution: %d", contended)
}

// assertWitnessDeterministic regenerates bd's witness across a spread of worker
// counts and requires every run to produce identical bytes. It returns whether
// the block actually re-executed a transaction; requireContention makes the
// absence of that a failure rather than a signal to the caller.
func assertWitnessDeterministic(t *testing.T, bd testBlockData, diskdb ethdb.Database, stats *v2StatsHandler, requireContention bool) bool {
	t.Helper()

	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	type sample struct {
		workers int
		hash    common.Hash
		nodes   int
		codes   int
	}

	stats.reset()

	// Repeat identical worker counts as well as varying them: scheduling
	// divergence shows up across runs at a fixed width, not only across widths.
	widths := []int{4, 4, 4, 4, 1, 2, 8, 8}

	samples := make([]sample, 0, len(widths))

	for _, workers := range widths {
		pb := prepareBlocks([]testBlockData{bd}, diskdb, config)[0]

		w, err := regenerateWitnessV2(&pb, config, engine, workers)
		if err != nil {
			if !requireContention {
				t.Skipf("fixture not replayable: %v", err)
			}
			t.Fatalf("regenerating witness with %d workers: %v", workers, err)
		}

		hash, err := stateless.WitnessCommitHashFromWitness(w)
		if err != nil {
			t.Fatalf("hashing witness with %d workers: %v", workers, err)
		}

		samples = append(samples, sample{workers: workers, hash: hash, nodes: len(w.State), codes: len(w.Codes)})
	}

	for _, s := range samples {
		t.Logf("workers=%-2d nodes=%-6d codes=%-4d commit=%x", s.workers, s.nodes, s.codes, s.hash[:12])
	}

	// Control: the fixture must actually re-execute transactions, otherwise
	// this test cannot observe the divergence it exists to catch.
	stats.mu.Lock()
	execs, vfails, txs := stats.execs, stats.vfails, stats.txs
	stats.mu.Unlock()

	t.Logf("contention control: txs=%d execs=%d vfails=%d (re-executions=%d)", txs, execs, vfails, execs-txs)

	if execs <= txs && requireContention {
		t.Fatalf("fixture exercised no re-execution (txs=%d execs=%d): this block cannot "+
			"reveal scheduling-dependent witness divergence, so the result above is vacuous", txs, execs)
	}

	want := samples[0]

	for i, got := range samples[1:] {
		if got.hash != want.hash {
			t.Errorf("run %d (workers=%d) produced a different witness than run 0 (workers=%d): "+
				"commit %x vs %x, nodes %d vs %d, codes %d vs %d",
				i+1, got.workers, want.workers, got.hash[:12], want.hash[:12], got.nodes, want.nodes, got.codes, want.codes)
		}
	}

	return execs > txs
}
