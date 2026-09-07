package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
)

// witnessRegenRoundTrip replays a block through the production V2 processor
// with witness recording enabled — over the production shared-reader stack,
// with the block author pre-warmed into the shared cache the way a flat
// reader serves it — then proves the regenerated witness is equivalent to
// the original: a stateless replay from each must produce identical gas,
// receipt root and state root.
func witnessRegenRoundTrip(pb *preparedBlock, diskdb ethdb.Database, config *params.ChainConfig, engine consensus.Engine) error {
	// Reference replay from the original witness, anchored against the real
	// block's roots. Without the anchor the round trip is only self-consistent:
	// a fixture witness whose replay silently diverges from mainnet (zero-ish
	// reads swallowed on both sides) would still "pass" against itself.
	refState, refReceipt, refRes, err := executeStatelessSerial(config, pb.block, pb.witness, &pb.author, engine, diskdb)
	if err != nil {
		return fmt.Errorf("replay from original witness: %w", err)
	}
	if refState != pb.stateRoot {
		return fmt.Errorf("original witness replay diverges from real block: state root %x, want %x", refState, pb.stateRoot)
	}
	if refReceipt != pb.receiptRoot {
		return fmt.Errorf("original witness replay diverges from real block: receipt root %x, want %x", refReceipt, pb.receiptRoot)
	}

	// Production-shaped reader stack over the witness-backed state. The
	// prefetch-role read warms the author into the shared cache so fee
	// credits during settle are cache hits, mirroring mainnet where the flat
	// reader serves hot accounts without touching the trie.
	db := state.NewDatabase(pb.tdb, nil)
	prefetchReader, _, parallelReader, err := db.ReadersWithCacheStatsTriple(pb.witness.Root())
	if err != nil {
		return fmt.Errorf("readers: %w", err)
	}
	if _, err := prefetchReader.Account(pb.author); err != nil {
		return fmt.Errorf("warm author: %w", err)
	}
	finalDB, err := state.NewWithReader(pb.witness.Root(), db, parallelReader)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}

	hc := &benchHeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine}
	w2, err := stateless.NewWitness(pb.block.Header(), hc)
	if err != nil {
		return fmt.Errorf("new witness: %w", err)
	}
	w2.Headers = append([]*types.Header{}, pb.witness.Headers...)
	finalDB.SetWitness(w2)

	// Same construction as processV2, on the reader-stacked statedb.
	bc := &BlockChain{
		chainConfig: config,
		hc:          &HeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine},
	}
	res, err := NewV2StateProcessor(hc, bc, 4).Process(pb.block, finalDB, benchVMConfig, &pb.author, context.Background())
	if err != nil {
		return fmt.Errorf("v2 process: %w", err)
	}
	if got, want := types.DeriveSha(res.Receipts, trie.NewStackTrie(nil)), refReceipt; got != want {
		return fmt.Errorf("v2 execution diverged from serial: receipt root %x, want %x", got, want)
	}
	// Production always computes the post-state root on the V2 statedb
	// (ProcessBlock → ValidateState → IntermediateRoot); that is also where
	// update- and deletion-path trie nodes get recorded into the witness.
	if got := finalDB.IntermediateRoot(config.IsEIP158(pb.block.Number())); got != refState {
		return fmt.Errorf("v2 execution diverged from serial: state root %x, want %x", got, refState)
	}

	// The regenerated witness must be equivalent to the original.
	gotState, gotReceipt, gotRes, err := executeStatelessSerial(config, pb.block, w2, &pb.author, engine, diskdb)
	if err != nil {
		return fmt.Errorf("replay from regenerated witness: %w", err)
	}
	if gotRes.GasUsed != refRes.GasUsed {
		return fmt.Errorf("regenerated witness incomplete: gas %d, want %d", gotRes.GasUsed, refRes.GasUsed)
	}
	if gotReceipt != refReceipt {
		return fmt.Errorf("regenerated witness incomplete: receipt root %x, want %x", gotReceipt, refReceipt)
	}
	if gotState != refState {
		missing := 0
		for node := range pb.witness.State {
			if _, ok := w2.State[node]; !ok {
				missing++
			}
		}
		return fmt.Errorf("regenerated witness incomplete: state root %x, want %x (original nodes=%d regenerated nodes=%d missing=%d)",
			gotState, refState, len(pb.witness.State), len(w2.State), missing)
	}
	return nil
}

// singleRegenBlockHex is the block whose fixture lives as plain git objects
// under core/testdata/witness_regen (witness, block, and just its code
// blobs), unlike the git-lfs witness set — so this one round trip runs on
// any clone with no lfs pull.
const singleRegenBlockHex = "0x4EC6D13"

// skipUnlessCodesArchive skips a test when the shared codes archive is still
// a git LFS pointer, so a checkout without lfs skips instead of failing on
// missing code.
func skipUnlessCodesArchive(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(witnessDir, "codes.tar.gz"))
	if err == nil && isLFSPointer(data) {
		t.Skipf("testdata not materialized: codes.tar.gz: %v", errLFSPointer)
	}
}

// loadSingleWitnessRegenBlock loads the self-contained fixture. The files are
// regular committed objects, so failures here are real regressions, not a
// matter of un-pulled testdata.
func loadSingleWitnessRegenBlock(t *testing.T, blockHex string) (testBlockData, ethdb.Database) {
	t.Helper()
	dir := filepath.Join("testdata", "witness_regen")
	blockData, err := os.ReadFile(filepath.Join(dir, blockHex+".block"))
	if err != nil {
		t.Fatalf("reading fixture block: %v", err)
	}
	witness, err := loadWitnessFromJSON(filepath.Join(dir, blockHex+".witness.gz"))
	if err != nil {
		t.Fatalf("loading fixture witness: %v", err)
	}
	block, stateRoot, receiptRoot, err := parseBlockFromJSON(blockData)
	if err != nil {
		t.Fatalf("parsing fixture block: %v", err)
	}
	codes, err := os.Open(filepath.Join(dir, "codes.tar.gz"))
	if err != nil {
		t.Fatalf("opening fixture codes: %v", err)
	}
	defer codes.Close()
	diskdb := rawdb.NewMemoryDatabase()
	if err := loadCodesFromTarGz(diskdb, codes); err != nil {
		t.Fatalf("loading fixture codes: %v", err)
	}
	return testBlockData{witness: witness, block: block, stateRoot: stateRoot, receiptRoot: receiptRoot}, diskdb
}

// TestV2WitnessRegenerationSingleBlock runs the round trip on one mainnet
// block and additionally anchors the replays against the fixture's real
// state and receipt roots. Its fixture is committed as plain git objects,
// so it runs everywhere — no git lfs required.
func TestV2WitnessRegenerationSingleBlock(t *testing.T) {
	bd, diskdb := loadSingleWitnessRegenBlock(t, singleRegenBlockHex)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	pb := prepareBlocks([]testBlockData{bd}, diskdb, config)[0]

	// Anchor: a replay from the original witness must reproduce the real
	// chain's roots for this block.
	stateRoot, receiptRoot, res, err := executeStatelessSerial(config, pb.block, pb.witness, &pb.author, engine, diskdb)
	if err != nil {
		t.Fatalf("replay from fixture witness: %v", err)
	}
	if res.GasUsed != pb.block.GasUsed() {
		t.Fatalf("fixture replay gas %d, want %d", res.GasUsed, pb.block.GasUsed())
	}
	if receiptRoot != pb.receiptRoot {
		t.Fatalf("fixture replay receipt root %x, want %x", receiptRoot, pb.receiptRoot)
	}
	if stateRoot != pb.stateRoot {
		t.Fatalf("fixture replay state root %x, want %x", stateRoot, pb.stateRoot)
	}

	if err := witnessRegenRoundTrip(&pb, diskdb, config, engine); err != nil {
		t.Fatal(err)
	}
}

// loadAllWitnessRegenBlocks enumerates every block/witness pair present in
// the testdata directory, not just the embedded quick set.
func loadAllWitnessRegenBlocks(t *testing.T) ([]testBlockData, ethdb.Database) {
	t.Helper()
	skipUnlessCodesArchive(t)
	entries, err := os.ReadDir(witnessDir)
	if err != nil {
		t.Skipf("witness directory %s not readable: %v", witnessDir, err)
	}
	diskdb := newCodeCachingDB(filepath.Join(witnessDir, "codes"))
	if err := diskdb.loadCodesFromDisk(); err != nil {
		t.Fatalf("loading codes archive: %v", err)
	}

	var blocks []testBlockData
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".witness.gz") {
			continue
		}
		blockHex := strings.TrimSuffix(name, ".witness.gz")
		blockData, err := os.ReadFile(filepath.Join(witnessDir, blockHex+".block"))
		if err != nil {
			continue
		}
		if isLFSPointer(blockData) {
			t.Skipf("testdata not materialized: %s: %v", blockHex, errLFSPointer)
		}
		witness, err := loadWitnessFromJSON(filepath.Join(witnessDir, name))
		if err != nil {
			if errors.Is(err, errLFSPointer) {
				t.Skipf("testdata not materialized: %v", err)
			}
			t.Fatalf("loading witness %s: %v", blockHex, err)
		}
		block, stateRoot, receiptRoot, err := parseBlockFromJSON(blockData)
		if err != nil {
			t.Fatalf("parsing block %s: %v", blockHex, err)
		}
		blocks = append(blocks, testBlockData{
			witness: witness, block: block,
			stateRoot: stateRoot, receiptRoot: receiptRoot,
		})
	}
	if len(blocks) == 0 {
		t.Skip("no witness blocks found in testdata")
	}
	t.Logf("loaded %d blocks from %s", len(blocks), witnessDir)
	return blocks, diskdb
}

// knownIncompleteWitnessFixtures lists fixture blocks whose witness/code
// archive is missing an EIP-7702 authority's pre-state code blob. Canonical
// execution reads that code in validateAuthorization (ParseDelegation needs
// the bytes), so the witness contract says it belongs in the witness — but
// the fixtures were captured by a client whose V2 base reads silently
// nil-served the miss, so the blob was never recorded. Serial stateless
// replay still anchors on these blocks only because the two outcomes
// converge: real code is a delegation designator (accept) and missing code
// reads as empty (also accept). The strict per-incarnation base-read gate
// on this branch surfaces the gap as "code is not found <hash>".
//
// Each entry pins the exact missing hash. Whether a given run trips over
// the gap is scheduling-dependent (the failing read only reaches the base
// when the speculative incarnation runs early enough and then validates),
// so a listed block passing is fine; only a DIFFERENT error is a real
// failure. Drop entries once the fixtures are regenerated by a client that
// records these reads.
var knownIncompleteWitnessFixtures = map[uint64]string{
	83014074: "408c1e105324ac38691b945bae4afcbb31690ae0ade2b0c036d75093ec34c303",
	83014100: "a13989c9f027731e625aa6c559a28baecd3e1fbfd9a51717c6a082a81edcf127",
	83020871: "b36e620cc2e8dce98d89a450271d476662191bf075f7d1cbb7c6f2e718ad0069",
}

// checkKnownIncompleteFixture reports how the sweep should treat err for
// blockNum: skip=true means the error is the pinned expected failure for a
// known-incomplete fixture; mismatch is non-nil when the listed block
// failed some other way.
func checkKnownIncompleteFixture(blockNum uint64, err error) (skip bool, mismatch error) {
	hash, listed := knownIncompleteWitnessFixtures[blockNum]
	if !listed || err == nil {
		return false, nil
	}
	if !strings.Contains(err.Error(), "code is not found "+hash) {
		return false, fmt.Errorf("block %d failed differently than its pinned fixture gap: %w", blockNum, err)
	}
	return true, nil
}

// TestV2WitnessRegenerationAllBlocks runs the round trip over every witness
// block in testdata. Heavier than the single-block variant; skipped in -short.
func TestV2WitnessRegenerationAllBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full witness regeneration sweep in short mode")
	}
	blocks, diskdb := loadAllWitnessRegenBlocks(t)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	failed := 0
	for i := range blocks {
		pb := prepareBlocks(blocks[i:i+1], diskdb, config)[0]
		err := witnessRegenRoundTrip(&pb, diskdb, config, engine)
		if skip, mismatch := checkKnownIncompleteFixture(pb.block.NumberU64(), err); skip {
			t.Logf("block %d: known-incomplete fixture (7702 authority code): %v", pb.block.NumberU64(), err)
			continue
		} else if mismatch != nil {
			failed++
			t.Error(mismatch)
			continue
		}
		if err != nil {
			failed++
			t.Errorf("block %d (%d txs, %d gas): %v",
				pb.block.NumberU64(), len(pb.block.Transactions()), pb.block.GasUsed(), err)
		}
	}
	t.Logf("witness regeneration round trip: %d/%d blocks ok", len(blocks)-failed, len(blocks))
}

// witnessRegenPipelinedRoundTrip is witnessRegenRoundTrip with the pipelined
// SRC completing the witness instead of the inline IntermediateRoot: after V2
// execution (prewalker included), the FlatDiff is extracted exactly as
// persistPipelinedImport does and replayed on a trie-only statedb carrying the
// SAME witness object, mirroring runSRCCompute. This settles two things the
// inline round trip cannot: that SRC extends the exec-side witness rather than
// rebuilding it from the FlatDiff read set (keys rescued by the prewalker only
// ever lived in the reader cache and are absent from that read set), and that
// the SRC-computed root matches the header against witness-backed state.
func witnessRegenPipelinedRoundTrip(pb *preparedBlock, diskdb ethdb.Database, config *params.ChainConfig, engine consensus.Engine) error {
	// Anchored the same way as witnessRegenRoundTrip — see the note there.
	refState, refReceipt, refRes, err := executeStatelessSerial(config, pb.block, pb.witness, &pb.author, engine, diskdb)
	if err != nil {
		return fmt.Errorf("replay from original witness: %w", err)
	}
	if refState != pb.stateRoot {
		return fmt.Errorf("original witness replay diverges from real block: state root %x, want %x", refState, pb.stateRoot)
	}
	if refReceipt != pb.receiptRoot {
		return fmt.Errorf("original witness replay diverges from real block: receipt root %x, want %x", refReceipt, pb.receiptRoot)
	}

	db := state.NewDatabase(pb.tdb, nil)
	prefetchReader, _, parallelReader, err := db.ReadersWithCacheStatsTriple(pb.witness.Root())
	if err != nil {
		return fmt.Errorf("readers: %w", err)
	}
	if _, err := prefetchReader.Account(pb.author); err != nil {
		return fmt.Errorf("warm author: %w", err)
	}
	finalDB, err := state.NewWithReader(pb.witness.Root(), db, parallelReader)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}

	hc := &benchHeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine}
	w2, err := stateless.NewWitness(pb.block.Header(), hc)
	if err != nil {
		return fmt.Errorf("new witness: %w", err)
	}
	w2.Headers = append([]*types.Header{}, pb.witness.Headers...)
	finalDB.SetWitness(w2)

	bc := &BlockChain{
		chainConfig: config,
		hc:          &HeaderChain{config: config, chainDb: pb.memdb, headerCache: pb.headerCache, engine: engine},
	}
	res, err := NewV2StateProcessor(hc, bc, 4).Process(pb.block, finalDB, benchVMConfig, &pb.author, context.Background())
	if err != nil {
		return fmt.Errorf("v2 process: %w", err)
	}
	if got, want := types.DeriveSha(res.Receipts, trie.NewStackTrie(nil)), refReceipt; got != want {
		return fmt.Errorf("v2 execution diverged from serial: receipt root %x, want %x", got, want)
	}

	// Pipelined completion: extract the FlatDiff as persistPipelinedImport
	// does, then run the SRC side of the split on a trie-only statedb that
	// carries the same witness object (openSRCStateDB + runSRCCompute).
	deleteEmptyObjects := config.IsEIP158(pb.block.Number())
	flatDiff := finalDB.CommitSnapshot(deleteEmptyObjects)
	finalDB.StopPrefetcher()

	tmpDB, err := state.NewTrieOnly(pb.witness.Root(), db)
	if err != nil {
		return fmt.Errorf("open SRC state: %w", err)
	}
	tmpDB.SetWitness(w2)
	tmpDB.ApplyFlatDiffForCommit(flatDiff)
	preloadFlatDiffReads(tmpDB, flatDiff)
	tmpDB.CollectStateWitness()
	srcRoot, _, err := tmpDB.CommitWithUpdate(pb.block.NumberU64(), deleteEmptyObjects, config.IsCancun(pb.block.Number()))
	if err != nil {
		return fmt.Errorf("SRC commit: %w", err)
	}
	if srcRoot != refState {
		return fmt.Errorf("SRC root diverged: %x, want %x", srcRoot, refState)
	}

	// The SRC-completed witness must replay statelessly to identical results.
	gotState, gotReceipt, gotRes, err := executeStatelessSerial(config, pb.block, w2, &pb.author, engine, diskdb)
	if err != nil {
		return fmt.Errorf("replay from SRC-completed witness: %w", err)
	}
	if gotRes.GasUsed != refRes.GasUsed {
		return fmt.Errorf("SRC-completed witness incomplete: gas %d, want %d", gotRes.GasUsed, refRes.GasUsed)
	}
	if gotReceipt != refReceipt {
		return fmt.Errorf("SRC-completed witness incomplete: receipt root %x, want %x", gotReceipt, refReceipt)
	}
	if gotState != refState {
		missing := 0
		for node := range pb.witness.State {
			if _, ok := w2.State[node]; !ok {
				missing++
			}
		}
		return fmt.Errorf("SRC-completed witness incomplete: state root %x, want %x (original nodes=%d completed nodes=%d missing=%d)",
			gotState, refState, len(pb.witness.State), len(w2.State), missing)
	}
	return nil
}

// TestV2WitnessRegenerationPipelinedSRC round-trips the no-LFS fixture block
// through the pipelined witness split (V2 exec + SRC completion).
func TestV2WitnessRegenerationPipelinedSRC(t *testing.T) {
	bd, diskdb := loadSingleWitnessRegenBlock(t, singleRegenBlockHex)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}
	pb := prepareBlocks([]testBlockData{bd}, diskdb, config)[0]

	if err := witnessRegenPipelinedRoundTrip(&pb, diskdb, config, engine); err != nil {
		t.Fatal(err)
	}
}

// TestV2WitnessRegenerationPipelinedSRCAllBlocks runs the pipelined round trip
// over every witness fixture. Same gating as TestV2WitnessRegenerationAllBlocks.
func TestV2WitnessRegenerationPipelinedSRCAllBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full witness set in -short mode")
	}
	blocks, diskdb := loadAllWitnessRegenBlocks(t)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	failed := 0
	for _, bd := range blocks {
		pb := prepareBlocks([]testBlockData{bd}, diskdb, config)[0]
		err := witnessRegenPipelinedRoundTrip(&pb, diskdb, config, engine)
		if skip, mismatch := checkKnownIncompleteFixture(pb.block.NumberU64(), err); skip {
			t.Logf("block %d: known-incomplete fixture (7702 authority code): %v", pb.block.NumberU64(), err)
			continue
		} else if mismatch != nil {
			failed++
			t.Error(mismatch)
			continue
		}
		if err != nil {
			failed++
			t.Errorf("block %s: %v", bd.block.Number(), err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d/%d blocks failed the pipelined witness round trip", failed, len(blocks))
	}
}

// injectWitnessIntoHashDB adds a witness's headers and state nodes into an
// existing hash-keyed database. Witness.MakeHashDB always creates a fresh
// database, but the chained round trip needs the union of two consecutive
// witnesses in one database so both root generations are resolvable.
func injectWitnessIntoHashDB(db ethdb.Database, w *stateless.Witness) {
	for _, header := range w.Headers {
		rawdb.WriteHeader(db, header)
	}
	for node := range w.State {
		blob := []byte(node)
		rawdb.WriteLegacyTrieNode(db, crypto.Keccak256Hash(blob), blob)
	}
}

// witnessRegenChainedPipelinedRoundTrip reproduces the steady-state pipelined
// import shape across two consecutive blocks, which the single-block round
// trip structurally cannot: block N executes while SRC(N-1) has not committed,
// so its readers sit at root_{N-2} and every trie node captured during
// execution is of the root_{N-2} generation — while the witness for block N
// must carry root_{N-1}-generation nodes. The only source of correct-
// generation nodes is SRC(N)'s re-read at root_{N-1}, which is driven
// entirely by the FlatDiff read-set. The round trip therefore fails whenever
// that read-set is not a complete record of what execution read.
func witnessRegenChainedPipelinedRoundTrip(prev, cur *testBlockData, diskdb ethdb.Database, config *params.ChainConfig, engine consensus.Engine) error {
	// Union hash-db: root_{N-2}-generation nodes come from prev's witness,
	// root_{N-1}-generation nodes from cur's witness plus SRC(N-1)'s commit.
	memdb := prev.witness.MakeHashDB(diskdb)
	injectWitnessIntoHashDB(memdb, cur.witness)
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	db := state.NewDatabase(tdb, nil)

	rootN2 := prev.witness.Root()
	rootN1 := cur.witness.Root()
	if rootN1 != prev.stateRoot {
		return fmt.Errorf("fixture pair not chained: block %d pre-state root %x, block %d post-state root %x",
			cur.block.NumberU64(), rootN1, prev.block.NumberU64(), prev.stateRoot)
	}

	headerCache := lru.NewCache[common.Hash, *types.Header](256)
	for _, h := range prev.witness.Headers {
		headerCache.Add(h.Hash(), h)
	}
	for _, h := range cur.witness.Headers {
		headerCache.Add(h.Hash(), h)
	}
	authorPrev := getAuthor(config, prev.witness.Header())
	authorCur := getAuthor(config, cur.witness.Header())
	hc := &benchHeaderChain{config: config, chainDb: memdb, headerCache: headerCache, engine: engine}
	bc := &BlockChain{
		chainConfig: config,
		hc:          &HeaderChain{config: config, chainDb: memdb, headerCache: headerCache, engine: engine},
	}

	// Anchored reference for block N — see the note in witnessRegenRoundTrip.
	refState, refReceipt, refRes, err := executeStatelessSerial(config, cur.block, cur.witness, &authorCur, engine, diskdb)
	if err != nil {
		return fmt.Errorf("replay from original witness: %w", err)
	}
	if refState != cur.stateRoot {
		return fmt.Errorf("original witness replay diverges from real block: state root %x, want %x", refState, cur.stateRoot)
	}
	if refReceipt != cur.receiptRoot {
		return fmt.Errorf("original witness replay diverges from real block: receipt root %x, want %x", refReceipt, cur.receiptRoot)
	}

	// Block N-1 enters the pipeline in direct mode: readers at the committed
	// root_{N-2}, no overlay (buildPipelineImportOpts' first-block branch).
	prefetchPrev, _, parallelPrev, err := db.ReadersWithCacheStatsTriple(rootN2)
	if err != nil {
		return fmt.Errorf("readers for block N-1: %w", err)
	}
	if _, err := prefetchPrev.Account(authorPrev); err != nil {
		return fmt.Errorf("warm author N-1: %w", err)
	}
	execPrev, err := state.NewWithReader(rootN2, db, parallelPrev)
	if err != nil {
		return fmt.Errorf("open state for block N-1: %w", err)
	}
	// A witness-producing node records a witness on every block; attaching
	// one to block N-1 keeps its CommitSnapshot on the production path (the
	// read-surface handling differs between witness-on and witness-off).
	w1, err := stateless.NewWitness(prev.block.Header(), hc)
	if err != nil {
		return fmt.Errorf("new witness for block N-1: %w", err)
	}
	w1.Headers = append([]*types.Header{}, prev.witness.Headers...)
	execPrev.SetWitness(w1)
	resPrev, err := NewV2StateProcessor(hc, bc, 4).Process(prev.block, execPrev, benchVMConfig, &authorPrev, context.Background())
	if err != nil {
		return fmt.Errorf("v2 process block N-1: %w", err)
	}
	if got := types.DeriveSha(resPrev.Receipts, trie.NewStackTrie(nil)); got != prev.receiptRoot {
		return fmt.Errorf("block N-1 execution diverged from real block: receipt root %x, want %x", got, prev.receiptRoot)
	}
	deleteEmptyPrev := config.IsEIP158(prev.block.Number())
	flatDiffPrev := execPrev.CommitSnapshot(deleteEmptyPrev)
	execPrev.StopPrefetcher()

	// SRC(N-1), witness-off shape (runSRCCompute's makeWitness=false branch):
	// only its side effect matters here — committing root_{N-1} into the
	// shared triedb so SRC(N) can open at it. Production runs this
	// concurrently with block N's execution; sequencing it first changes
	// nothing block N observes, since its readers are pinned to root_{N-2}.
	srcPrev, err := state.NewTrieOnly(rootN2, db)
	if err != nil {
		return fmt.Errorf("open SRC state for block N-1: %w", err)
	}
	srcPrev.ApplyFlatDiffForCommitFast(flatDiffPrev)
	committedRoot, _, err := srcPrev.CommitWithUpdate(prev.block.NumberU64(), deleteEmptyPrev, config.IsCancun(prev.block.Number()))
	if err != nil {
		return fmt.Errorf("SRC commit block N-1: %w", err)
	}
	if committedRoot != rootN1 {
		return fmt.Errorf("SRC(N-1) root diverged: %x, want %x", committedRoot, rootN1)
	}

	// Block N in steady-state pipelined shape: readers still at root_{N-2},
	// block N-1's post-state served by the FlatDiff overlay reference
	// (setupBlockReaders + applyFlatDiffOverlayToAll).
	prefetchCur, _, parallelCur, err := db.ReadersWithCacheStatsTriple(rootN2)
	if err != nil {
		return fmt.Errorf("readers for block N: %w", err)
	}
	if _, err := prefetchCur.Account(authorCur); err != nil {
		return fmt.Errorf("warm author N: %w", err)
	}
	execCur, err := state.NewWithReader(rootN2, db, parallelCur)
	if err != nil {
		return fmt.Errorf("open state for block N: %w", err)
	}
	execCur.SetFlatDiffRef(flatDiffPrev)

	w2, err := stateless.NewWitness(cur.block.Header(), hc)
	if err != nil {
		return fmt.Errorf("new witness: %w", err)
	}
	w2.Headers = append([]*types.Header{}, cur.witness.Headers...)
	execCur.SetWitness(w2)

	res, err := NewV2StateProcessor(hc, bc, 4).Process(cur.block, execCur, benchVMConfig, &authorCur, context.Background())
	if err != nil {
		return fmt.Errorf("v2 process block N: %w", err)
	}
	if got := types.DeriveSha(res.Receipts, trie.NewStackTrie(nil)); got != refReceipt {
		return fmt.Errorf("v2 execution diverged from serial: receipt root %x, want %x", got, refReceipt)
	}

	// SRC(N): the witness-completing side of the split, opening at the
	// freshly committed root_{N-1} — the only stage that can put correct-
	// generation proof nodes into w2.
	deleteEmptyCur := config.IsEIP158(cur.block.Number())
	flatDiffCur := execCur.CommitSnapshot(deleteEmptyCur)
	execCur.StopPrefetcher()

	srcCur, err := state.NewTrieOnly(rootN1, db)
	if err != nil {
		return fmt.Errorf("open SRC state for block N: %w", err)
	}
	srcCur.SetWitness(w2)
	srcCur.ApplyFlatDiffForCommit(flatDiffCur)
	preloadFlatDiffReads(srcCur, flatDiffCur)
	srcCur.CollectStateWitness()
	srcRoot, _, err := srcCur.CommitWithUpdate(cur.block.NumberU64(), deleteEmptyCur, config.IsCancun(cur.block.Number()))
	if err != nil {
		return fmt.Errorf("SRC commit block N: %w", err)
	}
	if srcRoot != refState {
		return fmt.Errorf("SRC root diverged: %x, want %x", srcRoot, refState)
	}

	// The SRC-completed witness must replay statelessly to identical results.
	gotState, gotReceipt, gotRes, err := executeStatelessSerial(config, cur.block, w2, &authorCur, engine, diskdb)
	if err != nil {
		return fmt.Errorf("replay from SRC-completed witness: %w", err)
	}
	if gotRes.GasUsed != refRes.GasUsed {
		dbErr := debugStatelessReplayDBError(config, cur.block, w2, &authorCur, engine, diskdb)
		return fmt.Errorf("SRC-completed witness incomplete: gas %d, want %d (dbErr: %v; owner: %s)",
			gotRes.GasUsed, refRes.GasUsed, dbErr, debugClassifyMissingNodeOwner(dbErr, flatDiffCur))
	}
	if gotReceipt != refReceipt {
		return fmt.Errorf("SRC-completed witness incomplete: receipt root %x, want %x", gotReceipt, refReceipt)
	}
	if gotState != refState {
		missing := 0
		for node := range cur.witness.State {
			if _, ok := w2.State[node]; !ok {
				missing++
			}
		}
		return fmt.Errorf("SRC-completed witness incomplete: state root %x, want %x (original nodes=%d completed nodes=%d missing=%d, dbErr: %v)",
			gotState, refState, len(cur.witness.State), len(w2.State), missing,
			debugStatelessReplayDBError(config, cur.block, w2, &authorCur, engine, diskdb))
	}
	return nil
}

// debugStatelessReplayDBError reruns a stateless replay and returns the
// statedb's internally accumulated read error, which executeStatelessSerial
// deliberately swallows (a serial statedb records read failures and keeps
// returning zero values). Diagnostic only — names the first missing node.
func debugStatelessReplayDBError(config *params.ChainConfig, block *types.Block, witness *stateless.Witness, author *common.Address, engine consensus.Engine, diskdb ethdb.Database) error {
	memdb := witness.MakeHashDB(diskdb)
	db, err := state.New(witness.Root(), state.NewDatabase(triedb.NewDatabase(memdb, triedb.HashDefaults), nil))
	if err != nil {
		return err
	}
	headerChain := &HeaderChain{
		config:      config,
		chainDb:     memdb,
		headerCache: lru.NewCache[common.Hash, *types.Header](256),
		engine:      engine,
	}
	if _, err := NewStateProcessor(headerChain).Process(block, db, benchVMConfig, author, context.Background()); err != nil {
		return err
	}
	db.IntermediateRoot(config.IsEIP158(block.Number()))
	return db.Error()
}

// debugClassifyMissingNodeOwner maps a missing storage-trie node's owner hash
// back to an address in the FlatDiff and reports which read/write bucket the
// owning account sits in. Diagnostic only: distinguishes "read escaped every
// record" from "recorded but preload captured the wrong generation".
func debugClassifyMissingNodeOwner(dbErr error, diff *state.FlatDiff) string {
	var missing *trie.MissingNodeError
	if !errors.As(dbErr, &missing) || missing.Owner == (common.Hash{}) {
		return "n/a"
	}
	buckets := map[string][]common.Address{}
	for addr := range diff.Accounts {
		buckets["Accounts"] = append(buckets["Accounts"], addr)
	}
	for addr := range diff.Storage {
		buckets["Storage"] = append(buckets["Storage"], addr)
	}
	for _, addr := range diff.ReadSet {
		buckets["ReadSet"] = append(buckets["ReadSet"], addr)
	}
	for addr := range diff.ReadStorage {
		buckets["ReadStorage"] = append(buckets["ReadStorage"], addr)
	}
	for addr := range diff.Destructs {
		buckets["Destructs"] = append(buckets["Destructs"], addr)
	}
	for _, addr := range diff.NonExistentReads {
		buckets["NonExistentReads"] = append(buckets["NonExistentReads"], addr)
	}
	found := ""
	for bucket, addrs := range buckets {
		for _, addr := range addrs {
			if crypto.Keccak256Hash(addr.Bytes()) == missing.Owner {
				found += fmt.Sprintf("%x in %s (%d ReadStorage slots); ", addr, bucket, len(diff.ReadStorage[addr]))
			}
		}
	}
	if found == "" {
		return "owner not in any FlatDiff bucket"
	}
	return found
}

// TestV2WitnessRegenerationPipelinedSRCChained runs the chained round trip on
// every consecutive fixture pair. Pairs touching a known-incomplete 7702
// fixture are skipped so the two gaps stay untangled.
func TestV2WitnessRegenerationPipelinedSRCChained(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping chained witness sweep in short mode")
	}
	blocks, diskdb := loadAllWitnessRegenBlocks(t)
	config := params.BorMainnetChainConfig
	engine := &benchConsensus{}

	byNumber := make(map[uint64]int, len(blocks))
	for i := range blocks {
		byNumber[blocks[i].block.NumberU64()] = i
	}
	pairs := 0
	for i := range blocks {
		cur := &blocks[i]
		num := cur.block.NumberU64()
		prevIdx, ok := byNumber[num-1]
		if !ok {
			continue
		}
		if _, listed := knownIncompleteWitnessFixtures[num]; listed {
			continue
		}
		if _, listed := knownIncompleteWitnessFixtures[num-1]; listed {
			continue
		}
		prev := &blocks[prevIdx]
		pairs++
		t.Run(fmt.Sprintf("%d", num), func(t *testing.T) {
			if err := witnessRegenChainedPipelinedRoundTrip(prev, cur, diskdb, config, engine); err != nil {
				t.Error(err)
			}
		})
	}
	if pairs == 0 {
		t.Skip("no consecutive fixture pairs available")
	}
	t.Logf("chained pipelined round trip over %d consecutive pairs", pairs)
}

// TestV2WitnessRegenerationPipelinedPrewalkFires guards the pipelined round
// trip against silently losing its bite: the whole point of running it on the
// witness-backed harness is that the shared reader cache serves hot keys
// without trie tracing, so if the prewalker stops firing here the round trip
// no longer exercises the blind spot it exists to cover. Not parallel — it
// reads a process-global counter delta.
func TestV2WitnessRegenerationPipelinedPrewalkFires(t *testing.T) {
	metrics.Enable()
	bd, diskdb := loadSingleWitnessRegenBlock(t, singleRegenBlockHex)
	pb := prepareBlocks([]testBlockData{bd}, diskdb, params.BorMainnetChainConfig)[0]

	before := metrics.GetOrRegisterCounter("chain/witness/readset/prewalk/keys", nil).Snapshot().Count()
	if err := witnessRegenPipelinedRoundTrip(&pb, diskdb, params.BorMainnetChainConfig, &benchConsensus{}); err != nil {
		t.Fatal(err)
	}
	delta := metrics.GetOrRegisterCounter("chain/witness/readset/prewalk/keys", nil).Snapshot().Count() - before
	t.Logf("prewalked keys during pipelined round trip: %d", delta)
	if delta == 0 {
		t.Fatal("prewalker did not fire — the round trip is not exercising the cache-hit path")
	}
}
