// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.

package core

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// counterContractBytecode reads a 32-byte slot key from calldata, increments the
// stored value, and writes it back. Exercises one SLOAD + one SSTORE per call,
// with the slot key fully controlled by the caller — letting tests synthesise
// hot/cold/per-sender access patterns without deploying real Solidity.
//
//	PUSH1 0x00     60 00
//	CALLDATALOAD   35
//	DUP1           80
//	SLOAD          54
//	PUSH1 0x01     60 01
//	ADD            01
//	SWAP1          90
//	SSTORE         55
//	STOP           00
var counterContractBytecode = []byte{0x60, 0x00, 0x35, 0x80, 0x54, 0x60, 0x01, 0x01, 0x90, 0x55, 0x00}

var counterContractAddr = common.HexToAddress("0x000000000000000000000000000000000000C0DE")

type irScenario struct {
	name    string
	slotKey func(txIdx int, sender common.Address) common.Hash
}

var irScenarios = []irScenario{
	{
		name: "hot-balanceOf-3slots",
		slotKey: func(txIdx int, _ common.Address) common.Hash {
			return common.BigToHash(big.NewInt(int64(txIdx % 3)))
		},
	},
	{
		name: "uniform-spread-unique-per-tx",
		slotKey: func(txIdx int, _ common.Address) common.Hash {
			return common.BigToHash(big.NewInt(int64(1_000_000 + txIdx)))
		},
	},
	{
		name: "per-sender-counter",
		slotKey: func(_ int, sender common.Address) common.Hash {
			return crypto.Keccak256Hash(sender.Bytes())
		},
	},
}

const (
	irNumSenders = 200
	irNumTrials  = 5
)

type irTrialResult struct {
	prefetchDur     time.Duration
	processDur      time.Duration
	procStats       state.ReaderStats
	procPrefetch    state.PrefetchStats
	prefetchedTxs   int
	processedFailed int
}

// makeSenderKeys returns N deterministic ECDSA keys (seeded from index).
func makeSenderKeys(n int) []*ecdsa.PrivateKey {
	keys := make([]*ecdsa.PrivateKey, n)
	for i := 0; i < n; i++ {
		// 32-byte big-endian seed: index in the last 8 bytes, padded with a
		// non-zero prefix to keep curves happy.
		seed := make([]byte, 32)
		seed[0] = 0xA1
		for j := 0; j < 8; j++ {
			seed[31-j] = byte((uint64(i+1) >> (8 * j)) & 0xff)
		}
		k, err := crypto.ToECDSA(seed)
		if err != nil {
			panic(err)
		}
		keys[i] = k
	}
	return keys
}

func setupIRChain(t testing.TB, senderKeys []*ecdsa.PrivateKey) *BlockChain {
	t.Helper()
	funds := new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
	alloc := types.GenesisAlloc{
		counterContractAddr: {Balance: big.NewInt(0), Code: counterContractBytecode},
	}
	for _, k := range senderKeys {
		alloc[crypto.PubkeyToAddress(k.PublicKey)] = types.Account{Balance: funds}
	}
	gspec := &Genesis{Config: params.TestChainConfig, Alloc: alloc, GasLimit: 30_000_000}

	// Commit genesis to a dedicated db so the chain's own db starts identical.
	genDb := rawdb.NewMemoryDatabase()
	gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig())
	if err != nil {
		t.Fatalf("NewBlockChain: %v", err)
	}
	return chain
}

// buildIRTxs produces one tx per sender for the given scenario. The tx calls
// the counter contract with the slot key as calldata.
func buildIRTxs(t testing.TB, chain *BlockChain, sc irScenario, senderKeys []*ecdsa.PrivateKey) []*types.Transaction {
	t.Helper()
	signer := types.LatestSigner(chain.Config())
	parent := chain.Genesis()
	baseFee := parent.BaseFee()
	if baseFee == nil {
		baseFee = big.NewInt(1_000_000_000)
	}
	gasTipCap := big.NewInt(1)
	gasFeeCap := new(big.Int).Add(baseFee, big.NewInt(1_000_000_000))

	txs := make([]*types.Transaction, len(senderKeys))
	for i, k := range senderKeys {
		sender := crypto.PubkeyToAddress(k.PublicKey)
		slot := sc.slotKey(i, sender)
		txData := &types.DynamicFeeTx{
			ChainID:   chain.Config().ChainID,
			Nonce:     0,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			Gas:       60_000,
			To:        &counterContractAddr,
			Value:     big.NewInt(0),
			Data:      slot.Bytes(),
		}
		signed, err := types.SignNewTx(k, signer, txData)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		txs[i] = signed
	}
	return txs
}

// makeWorkloadHeader synthesises a header on top of genesis suitable for EVM
// context construction. We avoid GenerateChain/Process to keep the test path
// focused on prefetch/process semantics rather than block-validation noise.
func makeWorkloadHeader(parent *types.Block) *types.Header {
	return &types.Header{
		ParentHash: parent.Hash(),
		Coinbase:   common.Address{0xC0},
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		GasLimit:   parent.GasLimit(),
		Time:       parent.Time() + 1,
		BaseFee:    parent.BaseFee(),
		Difficulty: big.NewInt(1),
	}
}

// runIRTrial does one prefetch+process pass on a fresh state at genesis with
// the given intermediateRootPrefetch flag. Each call reads from a fresh
// StateAtWithReaders so reader stats are isolated per trial.
func runIRTrial(t testing.TB, chain *BlockChain, txs []*types.Transaction, flag bool) irTrialResult {
	t.Helper()
	parent := chain.Genesis()
	header := makeWorkloadHeader(parent)

	statedb, throwaway, _, processReader, err := chain.StateAtWithReaders(parent.Root())
	if err != nil {
		t.Fatalf("StateAtWithReaders: %v", err)
	}

	prefetcher := NewStatePrefetcher(chain.Config(), chain.HeaderChain())
	signer := types.MakeSigner(chain.Config(), header.Number, header.Time)
	cfg := vm.Config{}

	var fails atomic.Int64
	var interrupt atomic.Bool

	// --- Prefetch phase: drive prefetchOneTx sequentially, in-process. ---
	prefetchStart := time.Now()
	for i, tx := range txs {
		_, _ = prefetcher.prefetchOneTx(
			tx, i, header, throwaway, throwaway.Reader(),
			signer, cfg, flag, &interrupt, &fails,
		)
	}
	prefetchDur := time.Since(prefetchStart)

	// --- Process phase: real ApplyTransactionWithEVM on the main statedb. ---
	evmCtx := NewEVMBlockContext(header, chain, nil)
	evm := vm.NewEVM(evmCtx, statedb, chain.Config(), cfg)
	gp := NewGasPool(header.GasLimit * uint64(len(txs)))
	procFails := 0

	processStart := time.Now()
	for i, tx := range txs {
		statedb.SetTxContext(tx.Hash(), i)
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			procFails++
			continue
		}
		if _, err := ApplyTransactionWithEVM(
			msg, gp, statedb, header.Number, header.Hash(), header.Time, tx, evm,
		); err != nil {
			procFails++
		}
	}
	processDur := time.Since(processStart)

	return irTrialResult{
		prefetchDur:     prefetchDur,
		processDur:      processDur,
		procStats:       processReader.GetStats(),
		procPrefetch:    processReader.GetPrefetchStats(),
		prefetchedTxs:   len(txs) - int(fails.Load()),
		processedFailed: procFails,
	}
}

func avgTrialResult(results []irTrialResult) irTrialResult {
	n := int64(len(results))
	if n == 0 {
		return irTrialResult{}
	}
	var avg irTrialResult
	for _, r := range results {
		avg.prefetchDur += r.prefetchDur
		avg.processDur += r.processDur
		avg.procStats.AccountHit += r.procStats.AccountHit
		avg.procStats.AccountMiss += r.procStats.AccountMiss
		avg.procStats.StorageHit += r.procStats.StorageHit
		avg.procStats.StorageMiss += r.procStats.StorageMiss
		avg.procPrefetch.AccountHitFromPrefetch += r.procPrefetch.AccountHitFromPrefetch
		avg.procPrefetch.StorageHitFromPrefetch += r.procPrefetch.StorageHitFromPrefetch
		avg.procPrefetch.AccountInsert += r.procPrefetch.AccountInsert
		avg.procPrefetch.StorageInsert += r.procPrefetch.StorageInsert
		avg.procPrefetch.AccountHitFromPrefetchUnique += r.procPrefetch.AccountHitFromPrefetchUnique
		avg.prefetchedTxs += r.prefetchedTxs
		avg.processedFailed += r.processedFailed
	}
	avg.prefetchDur /= time.Duration(n)
	avg.processDur /= time.Duration(n)
	avg.procStats.AccountHit /= n
	avg.procStats.AccountMiss /= n
	avg.procStats.StorageHit /= n
	avg.procStats.StorageMiss /= n
	avg.procPrefetch.AccountHitFromPrefetch /= n
	avg.procPrefetch.StorageHitFromPrefetch /= n
	avg.procPrefetch.AccountInsert /= n
	avg.procPrefetch.StorageInsert /= n
	avg.procPrefetch.AccountHitFromPrefetchUnique /= n
	avg.prefetchedTxs /= int(n)
	avg.processedFailed /= int(n)
	return avg
}

// TestIntermediateRootPrefetch_AccuracyVsCost evaluates whether computing
// IntermediateRoot inside the prefetcher's throwaway state warms caches that
// the main process phase actually reuses. For each scenario it runs M trials
// in each mode and prints a comparison table. No assertions — this is a
// research test; read the t.Log output to make a call.
func TestIntermediateRootPrefetch_AccuracyVsCost(t *testing.T) {
	if testing.Short() {
		t.Skip("research test; skipped under -short")
	}

	senderKeys := makeSenderKeys(irNumSenders)

	type runReport struct {
		scenario string
		flag     bool
		avg      irTrialResult
	}
	var reports []runReport

	for _, sc := range irScenarios {
		for _, flag := range []bool{false, true} {
			results := make([]irTrialResult, 0, irNumTrials)
			for trial := 0; trial < irNumTrials; trial++ {
				// Fresh chain per trial keeps shared trie/snapshot caches in
				// equivalent cold state for each (scenario, flag) combination.
				chain := setupIRChain(t, senderKeys)
				txs := buildIRTxs(t, chain, sc, senderKeys)
				results = append(results, runIRTrial(t, chain, txs, flag))
				chain.Stop()
			}
			reports = append(reports, runReport{sc.name, flag, avgTrialResult(results)})
		}
	}

	// Header.
	t.Log("")
	t.Logf("=== IntermediateRootPrefetch evaluation (%d trials × %d senders/scenario) ===",
		irNumTrials, irNumSenders)
	t.Log("")
	t.Logf("%-30s %-6s %12s %12s %10s %10s %12s %12s %12s",
		"scenario", "flag", "prefetch_ms", "process_ms",
		"acctHit", "acctMiss", "storHit", "storMiss", "storHitFromPF")

	// Print rows + per-scenario deltas.
	for i := 0; i < len(reports); i += 2 {
		off, on := reports[i], reports[i+1] // false, true (we iterate in that order)
		printIRRow(t, off)
		printIRRow(t, on)

		totalStorReads := off.avg.procStats.StorageHit + off.avg.procStats.StorageMiss
		var accuracyDeltaPct float64
		if totalStorReads > 0 {
			diff := on.avg.procPrefetch.StorageHitFromPrefetch - off.avg.procPrefetch.StorageHitFromPrefetch
			accuracyDeltaPct = 100.0 * float64(diff) / float64(totalStorReads)
		}
		var costDeltaPct float64
		if off.avg.prefetchDur > 0 {
			costDeltaPct = 100.0 * float64(on.avg.prefetchDur-off.avg.prefetchDur) / float64(off.avg.prefetchDur)
		}
		t.Logf("  → %s: accuracy_delta=%+.2f%% (StorageHitFromPrefetch on vs off, normalized by total storage reads),  cost_delta=%+.2f%% (prefetch wall time)",
			off.scenario, accuracyDeltaPct, costDeltaPct)
		t.Log("")
	}
}

func printIRRow(t testing.TB, r struct {
	scenario string
	flag     bool
	avg      irTrialResult
}) {
	t.Helper()
	t.Logf("%-30s %-6s %12.3f %12.3f %10d %10d %12d %12d %12d",
		r.scenario,
		fmt.Sprintf("%t", r.flag),
		float64(r.avg.prefetchDur.Microseconds())/1000.0,
		float64(r.avg.processDur.Microseconds())/1000.0,
		r.avg.procStats.AccountHit,
		r.avg.procStats.AccountMiss,
		r.avg.procStats.StorageHit,
		r.avg.procStats.StorageMiss,
		r.avg.procPrefetch.StorageHitFromPrefetch,
	)
}

// ---------------------------------------------------------------------------
// Pebble-backed evaluation: does IntermediateRoot during prefetch warm pebble
// & the hashdb clean cache in a way that speeds up the post-process Commit?
// ---------------------------------------------------------------------------

// irMeterCount returns the current cumulative count of a registered Meter, or
// 0 if it does not exist. Safe to call before the meter is first used.
func irMeterCount(name string) int64 {
	return metrics.GetOrRegisterMeter(name, nil).Snapshot().Count()
}

type irPebbleResult struct {
	prefetchDur            time.Duration
	processDur             time.Duration
	commitDur              time.Duration
	cleanHitDeltaPrefetch  int64 // hashdb clean cache hits during prefetch phase
	cleanMissDeltaPrefetch int64 // ... and misses (= disk reads of trie nodes)
	dirtyHitDeltaPrefetch  int64
	cleanHitDeltaProcCmt   int64 // hits during process+commit phase
	cleanMissDeltaProcCmt  int64
	dirtyHitDeltaProcCmt   int64 // dirty cache hits — recently-written nodes still in memory
	commitNodes            int64 // nodes flushed by Commit's hashdb write
}

func setupIRPebbleChain(t testing.TB, senderKeys []*ecdsa.PrivateKey) (*BlockChain, ethdbCloser) {
	t.Helper()
	dir := t.TempDir()
	pdb, err := pebble.New(dir, 32, 32, "", false)
	if err != nil {
		t.Fatalf("pebble.New: %v", err)
	}
	db := rawdb.NewDatabase(pdb)

	funds := new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
	alloc := types.GenesisAlloc{
		counterContractAddr: {Balance: big.NewInt(0), Code: counterContractBytecode},
	}
	for _, k := range senderKeys {
		alloc[crypto.PubkeyToAddress(k.PublicKey)] = types.Account{Balance: funds}
	}
	gspec := &Genesis{Config: params.TestChainConfig, Alloc: alloc, GasLimit: 30_000_000}
	gspec.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	chain, err := NewBlockChain(db, gspec, ethash.NewFaker(), DefaultConfig())
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewBlockChain pebble: %v", err)
	}
	return chain, ethdbCloser(func() {
		chain.Stop()
		_ = db.Close()
	})
}

type ethdbCloser func()

// prewarmIRPebble inserts one block where each sender executes the scenario tx,
// seeding storage trie nodes on disk. Returns the parent block + the next nonce
// for trial txs.
func prewarmIRPebble(
	t testing.TB,
	chain *BlockChain,
	sc irScenario,
	senderKeys []*ecdsa.PrivateKey,
) (parent *types.Block, nextNonce uint64) {
	t.Helper()
	signer := types.LatestSigner(chain.Config())
	gspecBlock := chain.Genesis()

	// GenerateChain uses its own db for state computation; replay through
	// InsertChain afterwards lands the result on the chain's pebble db.
	genDb := rawdb.NewMemoryDatabase()
	chain.Genesis()
	gspec := &Genesis{
		Config: chain.Config(),
		Alloc: types.GenesisAlloc{
			counterContractAddr: {Balance: big.NewInt(0), Code: counterContractBytecode},
		},
		GasLimit: 30_000_000,
	}
	funds := new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
	for _, k := range senderKeys {
		gspec.Alloc[crypto.PubkeyToAddress(k.PublicKey)] = types.Account{Balance: funds}
	}
	gspec.MustCommit(genDb, triedb.NewDatabase(genDb, triedb.HashDefaults))

	blocks, _ := GenerateChain(chain.Config(), gspecBlock, ethash.NewFaker(), genDb, 1, func(i int, gen *BlockGen) {
		baseFee := gen.BaseFee()
		gasTipCap := big.NewInt(1)
		gasFeeCap := new(big.Int).Add(baseFee, big.NewInt(1_000_000_000))
		for senderIdx, k := range senderKeys {
			sender := crypto.PubkeyToAddress(k.PublicKey)
			slot := sc.slotKey(senderIdx, sender)
			tx, err := types.SignNewTx(k, signer, &types.DynamicFeeTx{
				ChainID:   chain.Config().ChainID,
				Nonce:     gen.TxNonce(sender),
				GasTipCap: gasTipCap,
				GasFeeCap: gasFeeCap,
				Gas:       60_000,
				To:        &counterContractAddr,
				Value:     big.NewInt(0),
				Data:      slot.Bytes(),
			})
			if err != nil {
				t.Fatalf("prewarm sign: %v", err)
			}
			gen.AddTx(tx)
		}
	})
	if _, err := chain.InsertChain(blocks, false); err != nil {
		t.Fatalf("prewarm InsertChain: %v", err)
	}
	return blocks[len(blocks)-1], 1
}

// buildIRTxsAt builds workload txs starting at `nonce` (used after prewarm).
func buildIRTxsAt(
	t testing.TB,
	chain *BlockChain,
	sc irScenario,
	senderKeys []*ecdsa.PrivateKey,
	nonce uint64,
	parent *types.Block,
) []*types.Transaction {
	t.Helper()
	signer := types.LatestSigner(chain.Config())
	baseFee := parent.BaseFee()
	if baseFee == nil {
		baseFee = big.NewInt(1_000_000_000)
	}
	gasTipCap := big.NewInt(1)
	gasFeeCap := new(big.Int).Add(baseFee, big.NewInt(1_000_000_000))

	txs := make([]*types.Transaction, len(senderKeys))
	for i, k := range senderKeys {
		sender := crypto.PubkeyToAddress(k.PublicKey)
		slot := sc.slotKey(i, sender)
		signed, err := types.SignNewTx(k, signer, &types.DynamicFeeTx{
			ChainID:   chain.Config().ChainID,
			Nonce:     nonce,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			Gas:       60_000,
			To:        &counterContractAddr,
			Value:     big.NewInt(0),
			Data:      slot.Bytes(),
		})
		if err != nil {
			t.Fatalf("trial sign: %v", err)
		}
		txs[i] = signed
	}
	return txs
}

// runIRPebbleTrial: prefetch, then process+commit on a fresh state at parent,
// recording hashdb clean/dirty meter deltas around each phase. The Commit step
// is where pebble warming would pay off — that's the headline measurement.
func runIRPebbleTrial(
	t testing.TB,
	chain *BlockChain,
	parent *types.Block,
	txs []*types.Transaction,
	flag bool,
) irPebbleResult {
	t.Helper()
	header := makeWorkloadHeader(parent)

	statedb, throwaway, _, _, err := chain.StateAtWithReaders(parent.Root())
	if err != nil {
		t.Fatalf("StateAtWithReaders: %v", err)
	}

	prefetcher := NewStatePrefetcher(chain.Config(), chain.HeaderChain())
	signer := types.MakeSigner(chain.Config(), header.Number, header.Time)
	cfg := vm.Config{}

	var fails atomic.Int64
	var interrupt atomic.Bool

	cleanHit0 := irMeterCount("hashdb/memcache/clean/hit")
	cleanMiss0 := irMeterCount("hashdb/memcache/clean/miss")
	dirtyHit0 := irMeterCount("hashdb/memcache/dirty/hit")
	commitNodes0 := irMeterCount("hashdb/memcache/commit/nodes")

	prefetchStart := time.Now()
	for i, tx := range txs {
		_, _ = prefetcher.prefetchOneTx(
			tx, i, header, throwaway, throwaway.Reader(),
			signer, cfg, flag, &interrupt, &fails,
		)
	}
	prefetchDur := time.Since(prefetchStart)

	cleanHit1 := irMeterCount("hashdb/memcache/clean/hit")
	cleanMiss1 := irMeterCount("hashdb/memcache/clean/miss")
	dirtyHit1 := irMeterCount("hashdb/memcache/dirty/hit")

	// --- Process phase ---
	evmCtx := NewEVMBlockContext(header, chain, nil)
	evm := vm.NewEVM(evmCtx, statedb, chain.Config(), cfg)
	gp := NewGasPool(header.GasLimit * uint64(len(txs)))

	processStart := time.Now()
	for i, tx := range txs {
		statedb.SetTxContext(tx.Hash(), i)
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			continue
		}
		_, _ = ApplyTransactionWithEVM(
			msg, gp, statedb, header.Number, header.Hash(), header.Time, tx, evm,
		)
	}
	processDur := time.Since(processStart)

	// --- Commit phase: where pebble/clean-cache warming would actually pay. ---
	commitStart := time.Now()
	_, err = statedb.Commit(
		header.Number.Uint64(),
		chain.Config().IsEIP158(header.Number),
		chain.Config().IsCancun(header.Number),
	)
	commitDur := time.Since(commitStart)
	if err != nil {
		t.Fatalf("statedb.Commit: %v", err)
	}

	cleanHit2 := irMeterCount("hashdb/memcache/clean/hit")
	cleanMiss2 := irMeterCount("hashdb/memcache/clean/miss")
	dirtyHit2 := irMeterCount("hashdb/memcache/dirty/hit")
	commitNodes2 := irMeterCount("hashdb/memcache/commit/nodes")

	return irPebbleResult{
		prefetchDur:            prefetchDur,
		processDur:             processDur,
		commitDur:              commitDur,
		cleanHitDeltaPrefetch:  cleanHit1 - cleanHit0,
		cleanMissDeltaPrefetch: cleanMiss1 - cleanMiss0,
		dirtyHitDeltaPrefetch:  dirtyHit1 - dirtyHit0,
		cleanHitDeltaProcCmt:   cleanHit2 - cleanHit1,
		cleanMissDeltaProcCmt:  cleanMiss2 - cleanMiss1,
		dirtyHitDeltaProcCmt:   dirtyHit2 - dirtyHit1,
		commitNodes:            commitNodes2 - commitNodes0,
	}
}

func avgIRPebbleResult(results []irPebbleResult) irPebbleResult {
	n := int64(len(results))
	if n == 0 {
		return irPebbleResult{}
	}
	var avg irPebbleResult
	for _, r := range results {
		avg.prefetchDur += r.prefetchDur
		avg.processDur += r.processDur
		avg.commitDur += r.commitDur
		avg.cleanHitDeltaPrefetch += r.cleanHitDeltaPrefetch
		avg.cleanMissDeltaPrefetch += r.cleanMissDeltaPrefetch
		avg.dirtyHitDeltaPrefetch += r.dirtyHitDeltaPrefetch
		avg.cleanHitDeltaProcCmt += r.cleanHitDeltaProcCmt
		avg.cleanMissDeltaProcCmt += r.cleanMissDeltaProcCmt
		avg.dirtyHitDeltaProcCmt += r.dirtyHitDeltaProcCmt
		avg.commitNodes += r.commitNodes
	}
	avg.prefetchDur /= time.Duration(n)
	avg.processDur /= time.Duration(n)
	avg.commitDur /= time.Duration(n)
	avg.cleanHitDeltaPrefetch /= n
	avg.cleanMissDeltaPrefetch /= n
	avg.dirtyHitDeltaPrefetch /= n
	avg.cleanHitDeltaProcCmt /= n
	avg.cleanMissDeltaProcCmt /= n
	avg.dirtyHitDeltaProcCmt /= n
	avg.commitNodes /= n
	return avg
}

// TestIntermediateRootPrefetch_PebbleAccuracyVsCost mirrors the in-memory test
// but uses a real pebble-backed chain with prewarmed parent block, runs Commit
// after process, and reports hashdb cache deltas. Headline: does flag=true
// reduce clean-cache misses (= disk reads) during process+commit?
func TestIntermediateRootPrefetch_PebbleAccuracyVsCost(t *testing.T) {
	if testing.Short() {
		t.Skip("research test; skipped under -short")
	}

	senderKeys := makeSenderKeys(irNumSenders)

	type runReport struct {
		scenario string
		flag     bool
		avg      irPebbleResult
	}
	var reports []runReport

	for _, sc := range irScenarios {
		for _, flag := range []bool{false, true} {
			results := make([]irPebbleResult, 0, irNumTrials)
			for trial := 0; trial < irNumTrials; trial++ {
				chain, closer := setupIRPebbleChain(t, senderKeys)
				parent, nextNonce := prewarmIRPebble(t, chain, sc, senderKeys)
				txs := buildIRTxsAt(t, chain, sc, senderKeys, nextNonce, parent)
				results = append(results, runIRPebbleTrial(t, chain, parent, txs, flag))
				closer()
			}
			reports = append(reports, runReport{sc.name, flag, avgIRPebbleResult(results)})
		}
	}

	t.Log("")
	t.Logf("=== Pebble-backed IntermediateRootPrefetch evaluation (%d trials × %d senders, prewarm=1 block) ===",
		irNumTrials, irNumSenders)
	t.Log("")
	t.Logf("%-30s %-6s %10s %10s %10s | %8s %8s %8s | %8s %8s %8s",
		"scenario", "flag",
		"prefetch_ms", "process_ms", "commit_ms",
		"pf_clnH", "pf_clnM", "pf_drtH",
		"cmt_clnH", "cmt_clnM", "cmt_drtH")

	for _, r := range reports {
		t.Logf("%-30s %-6s %10.3f %10.3f %10.3f | %8d %8d %8d | %8d %8d %8d",
			r.scenario, fmt.Sprintf("%t", r.flag),
			float64(r.avg.prefetchDur.Microseconds())/1000.0,
			float64(r.avg.processDur.Microseconds())/1000.0,
			float64(r.avg.commitDur.Microseconds())/1000.0,
			r.avg.cleanHitDeltaPrefetch,
			r.avg.cleanMissDeltaPrefetch,
			r.avg.dirtyHitDeltaPrefetch,
			r.avg.cleanHitDeltaProcCmt,
			r.avg.cleanMissDeltaProcCmt,
			r.avg.dirtyHitDeltaProcCmt,
		)
	}
	t.Log("")

	// Per-scenario deltas: did flag=true reduce process+commit clean misses?
	for i := 0; i < len(reports); i += 2 {
		off, on := reports[i], reports[i+1]
		var missDelta float64
		if off.avg.cleanMissDeltaProcCmt > 0 {
			missDelta = 100.0 * float64(on.avg.cleanMissDeltaProcCmt-off.avg.cleanMissDeltaProcCmt) /
				float64(off.avg.cleanMissDeltaProcCmt)
		}
		var commitDelta float64
		if off.avg.commitDur > 0 {
			commitDelta = 100.0 * float64(on.avg.commitDur-off.avg.commitDur) / float64(off.avg.commitDur)
		}
		var prefetchCostDelta float64
		if off.avg.prefetchDur > 0 {
			prefetchCostDelta = 100.0 * float64(on.avg.prefetchDur-off.avg.prefetchDur) / float64(off.avg.prefetchDur)
		}
		t.Logf("  → %s: process+commit clean_miss delta = %+.1f%%, commit wall delta = %+.1f%%, prefetch cost delta = %+.1f%%",
			off.scenario, missDelta, commitDelta, prefetchCostDelta)
	}
}

// ---------------------------------------------------------------------------
// HEAVY-COLD-CONTRACT scenario: simulates a fat contract that hasn't been
// touched recently. Storage trie has ~10k pre-populated slots so its nodes
// don't fit in a constrained clean cache. Trial writes a handful of slots —
// commit must walk merkle paths and load siblings from disk. Hypothesis:
// IntermediateRoot during prefetch warms those exact paths in the clean cache,
// reducing commit-time disk reads.
// ---------------------------------------------------------------------------

const (
	irHeavySlots        = 100_000
	irHeavyTrialTouches = 50
	irHeavyCleanLimitMB = 0 // disable clean cache entirely → all trie reads go to pebble
	irHeavyDirtyLimitMB = 1
	irHeavyTrials       = 5
)

// heavyContractAddr is a different address from counterContractAddr so the two
// tests can coexist without genesis-allocation collisions.
var heavyContractAddr = common.HexToAddress("0x000000000000000000000000000000000000BEEF")

// makeHeavyStorage produces a deterministic ~10k-slot allocation. Keys are
// derived from sequential indices via keccak so they distribute uniformly
// across the storage trie (mimics balance-style mappings on real ERC20s).
func makeHeavyStorage(n int) map[common.Hash]common.Hash {
	out := make(map[common.Hash]common.Hash, n)
	for i := 0; i < n; i++ {
		k := crypto.Keccak256Hash([]byte(fmt.Sprintf("heavy-key-%d", i)))
		// Non-zero values so SLOAD pays full cost; bit pattern derived from i
		// to keep it deterministic.
		v := common.BigToHash(big.NewInt(int64(i + 1)))
		out[k] = v
	}
	return out
}

// pickHeavyTrialKeys deterministically chooses m keys out of the n pre-populated
// slots. Spread across the keyspace so multiple trie subtrees are touched.
func pickHeavyTrialKeys(n, m int) []common.Hash {
	out := make([]common.Hash, m)
	stride := n / m
	for i := 0; i < m; i++ {
		out[i] = crypto.Keccak256Hash([]byte(fmt.Sprintf("heavy-key-%d", i*stride)))
	}
	return out
}

func setupIRHeavyChain(t testing.TB, senderKeys []*ecdsa.PrivateKey) (*BlockChain, ethdbCloser) {
	t.Helper()
	dir := t.TempDir()
	pdb, err := pebble.New(dir, 32, 32, "", false)
	if err != nil {
		t.Fatalf("pebble.New: %v", err)
	}
	db := rawdb.NewDatabase(pdb)

	funds := new(big.Int).Mul(big.NewInt(1000), big.NewInt(params.Ether))
	alloc := types.GenesisAlloc{
		heavyContractAddr: {
			Balance: big.NewInt(0),
			Code:    counterContractBytecode,
			Storage: makeHeavyStorage(irHeavySlots),
		},
	}
	for _, k := range senderKeys {
		alloc[crypto.PubkeyToAddress(k.PublicKey)] = types.Account{Balance: funds}
	}
	gspec := &Genesis{Config: params.TestChainConfig, Alloc: alloc, GasLimit: 30_000_000}
	gspec.MustCommit(db, triedb.NewDatabase(db, triedb.HashDefaults))

	cfg := DefaultConfig()
	cfg.TrieCleanLimit = irHeavyCleanLimitMB
	cfg.TrieDirtyLimit = irHeavyDirtyLimitMB

	chain, err := NewBlockChain(db, gspec, ethash.NewFaker(), cfg)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewBlockChain heavy: %v", err)
	}
	// Force-flush any genesis dirties to disk + cap dirty cache at 0 so the
	// trial starts with the heaviest possible reliance on disk reads.
	if err := chain.StateCache().TrieDB().Commit(chain.Genesis().Root(), false); err != nil {
		t.Fatalf("triedb genesis commit: %v", err)
	}

	return chain, ethdbCloser(func() {
		chain.Stop()
		_ = db.Close()
	})
}

func buildHeavyTrialTxs(
	t testing.TB,
	chain *BlockChain,
	senderKeys []*ecdsa.PrivateKey,
	parent *types.Block,
) []*types.Transaction {
	t.Helper()
	signer := types.LatestSigner(chain.Config())
	baseFee := parent.BaseFee()
	if baseFee == nil {
		baseFee = big.NewInt(1_000_000_000)
	}
	gasTipCap := big.NewInt(1)
	gasFeeCap := new(big.Int).Add(baseFee, big.NewInt(1_000_000_000))

	keys := pickHeavyTrialKeys(irHeavySlots, irHeavyTrialTouches)
	txs := make([]*types.Transaction, irHeavyTrialTouches)
	for i := 0; i < irHeavyTrialTouches; i++ {
		sender := senderKeys[i%len(senderKeys)]
		signed, err := types.SignNewTx(sender, signer, &types.DynamicFeeTx{
			ChainID:   chain.Config().ChainID,
			Nonce:     0,
			GasTipCap: gasTipCap,
			GasFeeCap: gasFeeCap,
			Gas:       60_000,
			To:        &heavyContractAddr,
			Value:     big.NewInt(0),
			Data:      keys[i].Bytes(),
		})
		if err != nil {
			t.Fatalf("heavy trial sign: %v", err)
		}
		txs[i] = signed
	}
	return txs
}

// runIRHeavyTrial: same shape as runIRPebbleTrial but starts from a chain with
// constrained caches and a heavy genesis contract.
func runIRHeavyTrial(
	t testing.TB,
	chain *BlockChain,
	parent *types.Block,
	txs []*types.Transaction,
	flag bool,
) irPebbleResult {
	return runIRPebbleTrial(t, chain, parent, txs, flag)
}

func TestIntermediateRootPrefetch_HeavyColdContract(t *testing.T) {
	if testing.Short() {
		t.Skip("research test; skipped under -short")
	}

	// Need at least irHeavyTrialTouches senders; build that many.
	senderKeys := makeSenderKeys(irHeavyTrialTouches)

	type runReport struct {
		flag bool
		avg  irPebbleResult
	}
	var reports []runReport

	for _, flag := range []bool{false, true} {
		results := make([]irPebbleResult, 0, irHeavyTrials)
		for trial := 0; trial < irHeavyTrials; trial++ {
			chain, closer := setupIRHeavyChain(t, senderKeys)
			parent := chain.Genesis()
			txs := buildHeavyTrialTxs(t, chain, senderKeys, parent)
			results = append(results, runIRHeavyTrial(t, chain, parent, txs, flag))
			closer()
		}
		reports = append(reports, runReport{flag, avgIRPebbleResult(results)})
	}

	t.Log("")
	t.Logf("=== Heavy-cold-contract evaluation: %d slots in genesis, %d touches per trial, %d trials, TrieCleanLimit=%dMB ===",
		irHeavySlots, irHeavyTrialTouches, irHeavyTrials, irHeavyCleanLimitMB)
	t.Log("")
	t.Logf("%-6s %10s %10s %10s | %8s %8s %8s | %8s %8s %8s",
		"flag",
		"prefetch_ms", "process_ms", "commit_ms",
		"pf_clnH", "pf_clnM", "pf_drtH",
		"cmt_clnH", "cmt_clnM", "cmt_drtH")
	for _, r := range reports {
		t.Logf("%-6s %10.3f %10.3f %10.3f | %8d %8d %8d | %8d %8d %8d",
			fmt.Sprintf("%t", r.flag),
			float64(r.avg.prefetchDur.Microseconds())/1000.0,
			float64(r.avg.processDur.Microseconds())/1000.0,
			float64(r.avg.commitDur.Microseconds())/1000.0,
			r.avg.cleanHitDeltaPrefetch,
			r.avg.cleanMissDeltaPrefetch,
			r.avg.dirtyHitDeltaPrefetch,
			r.avg.cleanHitDeltaProcCmt,
			r.avg.cleanMissDeltaProcCmt,
			r.avg.dirtyHitDeltaProcCmt,
		)
	}

	off, on := reports[0], reports[1]
	commitMissReductionPct := 0.0
	if off.avg.cleanMissDeltaProcCmt > 0 {
		commitMissReductionPct = 100.0 * float64(off.avg.cleanMissDeltaProcCmt-on.avg.cleanMissDeltaProcCmt) /
			float64(off.avg.cleanMissDeltaProcCmt)
	}
	commitTimeDeltaPct := 0.0
	if off.avg.commitDur > 0 {
		commitTimeDeltaPct = 100.0 * float64(on.avg.commitDur-off.avg.commitDur) / float64(off.avg.commitDur)
	}
	prefetchCostPct := 0.0
	if off.avg.prefetchDur > 0 {
		prefetchCostPct = 100.0 * float64(on.avg.prefetchDur-off.avg.prefetchDur) / float64(off.avg.prefetchDur)
	}
	t.Logf("")
	t.Logf("HEADLINE — flag=true reduces commit clean_miss by %+.1f%%, commit wall by %+.1f%% (negative = faster), prefetch costs %+.1f%% more",
		commitMissReductionPct, commitTimeDeltaPct, prefetchCostPct)
}
