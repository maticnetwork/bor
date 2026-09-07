// Copyright 2021 The go-ethereum Authors
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

package state

import (
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/testrand"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/triedb"
)

func filledStateDB() *StateDB {
	state, _ := New(types.EmptyRootHash, NewDatabaseForTesting())

	// Create an account and check if the retrieved balance is correct
	addr := common.HexToAddress("0xaffeaffeaffeaffeaffeaffeaffeaffeaffeaffe")
	skey := common.HexToHash("aaa")
	sval := common.HexToHash("bbb")

	state.SetBalance(addr, uint256.NewInt(42), tracing.BalanceChangeUnspecified) // Change the account trie
	state.SetCode(addr, []byte("hello"), tracing.CodeChangeUnspecified)          // Change an external metadata
	state.SetState(addr, skey, sval)                                             // Change the storage trie
	for i := 0; i < 100; i++ {
		sk := common.BigToHash(big.NewInt(int64(i)))
		state.SetState(addr, sk, sk) // Change the storage trie
	}

	return state
}

func TestUseAfterTerminate(t *testing.T) {
	db := filledStateDB()
	prefetcher := newTriePrefetcher(db.db, db.originalRoot, "", true)
	skey := common.HexToHash("aaa")

	if err := prefetcher.prefetch(common.Hash{}, db.originalRoot, common.Address{}, nil, []common.Hash{skey}, false); err != nil {
		t.Errorf("Prefetch failed before terminate: %v", err)
	}
	prefetcher.terminate(false)

	if err := prefetcher.prefetch(common.Hash{}, db.originalRoot, common.Address{}, nil, []common.Hash{skey}, false); err == nil {
		t.Errorf("Prefetch succeeded after terminate: %v", err)
	}
	if tr := prefetcher.trie(common.Hash{}, db.originalRoot); tr == nil {
		t.Errorf("Prefetcher returned nil trie after terminate")
	}
}

func TestDetachedPrefetcherLifecycle(t *testing.T) {
	db := filledStateDB()
	db.StartPrefetcher("detach-lifecycle", nil, nil)
	if db.prefetcher == nil {
		t.Fatal("expected StartPrefetcher to install a prefetcher")
	}

	detached := db.DetachPrefetcher()
	if detached == nil {
		t.Fatal("expected DetachPrefetcher to return a handle")
	}
	if db.prefetcher != nil {
		t.Fatal("DetachPrefetcher left StateDB.prefetcher installed")
	}
	if second := db.DetachPrefetcher(); second != nil {
		t.Fatal("second DetachPrefetcher returned a handle after prefetcher was detached")
	}

	stats := detached.Stop()
	if stats.Fetchers == 0 {
		t.Fatal("detached Stop reported zero fetchers; expected the account-trie prefetcher")
	}
	again := detached.Stop()
	if again.Fetchers != 0 || again.Drain != 0 || again.Report != 0 {
		t.Fatalf("second detached Stop returned non-zero stats: %+v", again)
	}
}

func TestDetachedPrefetcherCollectsWarmSnapshot(t *testing.T) {
	db := filledStateDB()
	db.StartPrefetcher("detach-warm-snapshot", nil, nil)

	// Resolve an account through the prefetcher before detaching it so the
	// collection path has a real account-trie witness to hand to SRC.
	addr := common.HexToAddress("0x0000000000000000000000000000000000000001")
	if err := db.prefetcher.prefetch(common.Hash{}, db.originalRoot, common.Address{}, []common.Address{addr}, nil, false); err != nil {
		t.Fatalf("prefetch account: %v", err)
	}
	detached := db.DetachPrefetcher()
	if detached == nil {
		t.Fatal("expected detached prefetcher")
	}
	input, stats := detached.StopAndCollectWarmSnapshot()
	if stats.Fetchers == 0 {
		t.Fatalf("collection reported no fetchers: %+v", stats)
	}
	if stats.LoadedFetchers > 0 && input == nil {
		t.Fatalf("loaded fetchers did not produce snapshot input: %+v", stats)
	}

	again, emptyStats := detached.StopAndCollectWarmSnapshot()
	if again != nil || emptyStats.Fetchers != 0 {
		t.Fatalf("second collection returned input=%v stats=%+v", again, emptyStats)
	}
}

func TestDetachedPrefetcherNilAndEmpty(t *testing.T) {
	var nilDetached *DetachedPrefetcher
	if stats := nilDetached.Stop(); stats.Fetchers != 0 || stats.Drain != 0 || stats.Report != 0 {
		t.Fatalf("nil Stop returned non-zero stats: %+v", stats)
	}
	if input, stats := nilDetached.StopAndCollectWarmSnapshot(); input != nil || stats.Fetchers != 0 || stats.Drain != 0 || stats.Report != 0 {
		t.Fatalf("nil StopAndCollectWarmSnapshot returned input=%v stats=%+v", input, stats)
	}

	empty := &DetachedPrefetcher{}
	if stats := empty.Stop(); stats.Fetchers != 0 || stats.Drain != 0 || stats.Report != 0 {
		t.Fatalf("empty Stop returned non-zero stats: %+v", stats)
	}
	if input, stats := empty.StopAndCollectWarmSnapshot(); input != nil || stats.Fetchers != 0 || stats.Drain != 0 || stats.Report != 0 {
		t.Fatalf("empty StopAndCollectWarmSnapshot returned input=%v stats=%+v", input, stats)
	}
}

func TestTriePrefetcherWarmSnapshotCollection(t *testing.T) {
	accountNodes := map[string][]byte{
		"account-a": {1, 2, 3},
		"account-b": {4},
	}
	storageNodes := map[string][]byte{
		"storage-a": {5, 6},
	}
	accountFetcher := &subfetcher{
		trie:  &blockingPrefetchTrie{witness: accountNodes},
		owner: common.Hash{},
	}
	storageOwner := common.HexToHash("0x1234")
	storageFetcher := &subfetcher{
		trie:  &blockingPrefetchTrie{witness: storageNodes},
		owner: storageOwner,
	}
	emptyFetcher := &subfetcher{}
	prefetcher := &triePrefetcher{
		fetchers: map[string]*subfetcher{
			"account": accountFetcher,
			"storage": storageFetcher,
			"empty":   emptyFetcher,
		},
	}

	nodes, stats := prefetcher.snapshotWarmNodes()
	if len(nodes) != 2 {
		t.Fatalf("warm snapshot groups = %d, want 2", len(nodes))
	}
	if stats.Fetchers != 3 || stats.LoadedFetchers != 2 {
		t.Fatalf("fetcher stats = %+v, want 3 total and 2 loaded", stats)
	}
	if stats.AccountFetchers != 1 || stats.AccountNodes != 2 || stats.AccountBytes != 4 {
		t.Fatalf("account stats = %+v", stats)
	}
	if stats.StorageFetchers != 1 || stats.StorageNodes != 1 || stats.StorageBytes != 2 {
		t.Fatalf("storage stats = %+v", stats)
	}
	if prefetcher.fetcherCount() != 3 {
		t.Fatalf("fetcherCount = %d, want 3", prefetcher.fetcherCount())
	}

	var nilPrefetcher *triePrefetcher
	if nodes, stats := nilPrefetcher.snapshotWarmNodes(); nodes != nil || stats.Fetchers != 0 {
		t.Fatalf("nil prefetcher returned nodes=%v stats=%+v", nodes, stats)
	}
	if nilPrefetcher.fetcherCount() != 0 {
		t.Fatal("nil prefetcher reported fetchers")
	}
	empty := &triePrefetcher{fetchers: make(map[string]*subfetcher)}
	if nodes, stats := empty.snapshotWarmNodes(); nodes != nil || stats.Fetchers != 0 {
		t.Fatalf("empty prefetcher returned nodes=%v stats=%+v", nodes, stats)
	}
}

func TestSubfetcherPrefetchHelpers(t *testing.T) {
	tr := newBlockingPrefetchTrie()
	tr.releaseBlockedPrefetch()
	sf := &subfetcher{
		trie:  tr,
		addr:  common.HexToAddress("0x1234"),
		ioSem: make(chan struct{}, 1),
	}
	if !sf.prefetchAccounts([]common.Address{common.HexToAddress("0x1"), common.HexToAddress("0x2")}) {
		t.Fatal("prefetchAccounts returned false")
	}
	if !sf.prefetchStorage([][]byte{{1}, {2}}) {
		t.Fatal("prefetchStorage returned false")
	}
	if calls, items := tr.accountStats(); calls != 1 || items != 2 {
		t.Fatalf("account prefetch calls/items = %d/%d, want 1/2", calls, items)
	}
	tr.lock.Lock()
	storageCalls, storageItems := tr.storageCalls, tr.storageItems
	tr.lock.Unlock()
	if storageCalls != 1 || storageItems != 2 {
		t.Fatalf("storage prefetch calls/items = %d/%d, want 1/2", storageCalls, storageItems)
	}
	if len(sf.ioSem) != 0 {
		t.Fatal("I/O semaphore token leaked")
	}

	var nilFetcher *subfetcher
	if nodes := nilFetcher.warmNodes(); nodes != nil {
		t.Fatalf("nil fetcher returned warm nodes: %v", nodes)
	}
	if nodes := (&subfetcher{}).warmNodes(); nodes != nil {
		t.Fatalf("fetcher without trie returned warm nodes: %v", nodes)
	}
	if nodes := sf.warmNodes(); nodes != nil {
		t.Fatalf("trie without witness nodes returned: %v", nodes)
	}
}

func TestSubfetcherScheduleTerminationRaces(t *testing.T) {
	t.Run("already stopped", func(t *testing.T) {
		stop := make(chan struct{})
		close(stop)
		sf := &subfetcher{stop: stop, term: make(chan struct{})}
		if err := sf.schedule([]common.Address{{1}}, nil, true); err != errTerminated {
			t.Fatalf("schedule error = %v, want %v", err, errTerminated)
		}
	})

	t.Run("stopped while waiting for queue lock", func(t *testing.T) {
		stop := make(chan struct{})
		sf := &subfetcher{stop: stop, term: make(chan struct{})}
		sf.lock.Lock()
		result := make(chan error, 1)
		go func() {
			result <- sf.schedule([]common.Address{{1}}, nil, true)
		}()
		time.Sleep(10 * time.Millisecond)
		close(stop)
		sf.lock.Unlock()
		if err := <-result; err != errTerminated {
			t.Fatalf("schedule error = %v, want %v", err, errTerminated)
		}
	})
}

func TestSubfetcherDrainTerminateKeepsQueuedTasks(t *testing.T) {
	db := filledStateDB()
	slot := common.HexToHash("aaa")
	sf := &subfetcher{
		db:            db.db,
		state:         db.originalRoot,
		root:          db.originalRoot,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		term:          make(chan struct{}),
		seenReadAddr:  make(map[common.Address]struct{}),
		seenWriteAddr: make(map[common.Address]struct{}),
		seenReadSlot:  make(map[common.Hash]struct{}),
		seenWriteSlot: make(map[common.Hash]struct{}),
		tasks:         []*subfetcherTask{{slot: &slot}},
	}
	sf.terminate(true)

	if got := len(sf.tasks); got != 1 {
		t.Fatalf("full-drain terminate changed queued task count to %d, want 1", got)
	}
}

func TestTriePrefetcherDrainTerminateCompletesQueuedWork(t *testing.T) {
	db := NewDatabaseForTesting()
	tr := newBlockingPrefetchTrie()
	t.Cleanup(tr.releaseBlockedPrefetch)
	prefetcher := newTriePrefetcher(&blockingPrefetchDB{
		Database: db,
		triedb:   db.TrieDB(),
		trie:     tr,
	}, common.Hash{}, "drain-queued-work", false)
	addr1 := common.HexToAddress("0x1")
	addr2 := common.HexToAddress("0x2")

	if err := prefetcher.prefetch(common.Hash{}, common.Hash{}, common.Address{}, []common.Address{addr1}, nil, false); err != nil {
		t.Fatalf("first prefetch failed: %v", err)
	}
	select {
	case <-tr.started:
	case <-time.After(time.Second):
		t.Fatalf("first prefetch did not start")
	}
	if err := prefetcher.prefetch(common.Hash{}, common.Hash{}, common.Address{}, []common.Address{addr2}, nil, false); err != nil {
		t.Fatalf("second prefetch failed: %v", err)
	}

	done := make(chan struct{})
	go func() {
		prefetcher.terminate(false)
		close(done)
	}()
	select {
	case <-done:
		t.Fatalf("full-drain terminate returned before in-flight account chunk completed")
	case <-time.After(20 * time.Millisecond):
	}
	tr.releaseBlockedPrefetch()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("full-drain terminate did not return after queued work completed")
	}

	if calls, items := tr.accountStats(); calls != 2 || items != 2 {
		t.Fatalf("executed account prefetch calls/items = %d/%d, want 2/2", calls, items)
	}
	if err := prefetcher.prefetch(common.Hash{}, common.Hash{}, common.Address{}, []common.Address{addr2}, nil, false); err != errTerminated {
		t.Fatalf("prefetch after terminate error = %v, want %v", err, errTerminated)
	}
}

type blockingPrefetchDB struct {
	Database
	triedb *triedb.Database
	trie   *blockingPrefetchTrie
}

func (db *blockingPrefetchDB) OpenTrie(common.Hash) (Trie, error) {
	return db.trie, nil
}

func (db *blockingPrefetchDB) OpenStorageTrie(common.Hash, common.Address, common.Hash, Trie) (Trie, error) {
	return db.trie, nil
}

func (db *blockingPrefetchDB) TrieDB() *triedb.Database {
	return db.triedb
}

type blockingPrefetchTrie struct {
	Trie

	started     chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once

	lock         sync.Mutex
	accountCalls int
	accountItems int
	storageCalls int
	storageItems int
	witness      map[string][]byte
}

func (t *blockingPrefetchTrie) Witness() map[string][]byte {
	return t.witness
}

func newBlockingPrefetchTrie() *blockingPrefetchTrie {
	return &blockingPrefetchTrie{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (t *blockingPrefetchTrie) PrefetchAccount(addrs []common.Address) error {
	t.lock.Lock()
	t.accountCalls++
	t.accountItems += len(addrs)
	first := t.accountCalls == 1
	t.lock.Unlock()
	if first {
		t.once.Do(func() { close(t.started) })
		<-t.release
	}
	return nil
}

func (t *blockingPrefetchTrie) releaseBlockedPrefetch() {
	t.releaseOnce.Do(func() { close(t.release) })
}

func (t *blockingPrefetchTrie) PrefetchStorage(_ common.Address, keys [][]byte) error {
	t.lock.Lock()
	t.storageCalls++
	t.storageItems += len(keys)
	first := t.storageCalls == 1
	t.lock.Unlock()
	if first {
		t.once.Do(func() { close(t.started) })
		<-t.release
	}
	return nil
}

func (t *blockingPrefetchTrie) accountStats() (int, int) {
	t.lock.Lock()
	defer t.lock.Unlock()

	return t.accountCalls, t.accountItems
}

func TestVerklePrefetcher(t *testing.T) {
	disk := rawdb.NewMemoryDatabase()
	db := triedb.NewDatabase(disk, triedb.VerkleDefaults)
	sdb := NewDatabase(db, nil)

	state, err := New(types.EmptyRootHash, sdb)
	if err != nil {
		t.Fatalf("failed to initialize state: %v", err)
	}
	// Create an account and check if the retrieved balance is correct
	addr := testrand.Address()
	skey := testrand.Hash()
	sval := testrand.Hash()

	state.SetBalance(addr, uint256.NewInt(42), tracing.BalanceChangeUnspecified) // Change the account trie
	state.SetCode(addr, []byte("hello"), tracing.CodeChangeUnspecified)          // Change an external metadata
	state.SetState(addr, skey, sval)                                             // Change the storage trie
	root, _ := state.Commit(0, true, false)

	state, _ = New(root, sdb)
	sRoot := state.GetStorageRoot(addr)
	fetcher := newTriePrefetcher(sdb, root, "", false)

	// Read account
	fetcher.prefetch(common.Hash{}, root, common.Address{}, []common.Address{addr}, nil, false)

	// Read storage slot
	fetcher.prefetch(crypto.Keccak256Hash(addr.Bytes()), sRoot, addr, nil, []common.Hash{skey}, false)

	fetcher.terminate(false)
	accountTrie := fetcher.trie(common.Hash{}, root)
	storageTrie := fetcher.trie(crypto.Keccak256Hash(addr.Bytes()), sRoot)

	rootA := accountTrie.Hash()
	rootB := storageTrie.Hash()
	if rootA != rootB {
		t.Fatal("Two different tries are retrieved")
	}
}

// newTerminatedSubfetcher creates a subfetcher that is already terminated,
// suitable for testing used()/appendUsed() without needing a real trie.
func newTerminatedSubfetcher(db Database, state common.Hash, owner common.Hash, root common.Hash, addr common.Address) *subfetcher {
	sf := &subfetcher{
		db:            db,
		state:         state,
		owner:         owner,
		root:          root,
		addr:          addr,
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		term:          make(chan struct{}),
		seenReadAddr:  make(map[common.Address]struct{}),
		seenWriteAddr: make(map[common.Address]struct{}),
		seenReadSlot:  make(map[common.Hash]struct{}),
		seenWriteSlot: make(map[common.Hash]struct{}),
	}
	close(sf.term)
	return sf
}

// TestConcurrentUsed verifies that calling used() concurrently from multiple
// goroutines on different subfetchers produces correct results. Run with -race
// to detect data races.
func TestConcurrentUsed(t *testing.T) {
	db := filledStateDB()
	prefetcher := newTriePrefetcher(db.db, db.originalRoot, "concurrent-used", false)

	const N = 10
	const addrCount = 100
	const slotCount = 50

	type fetcherKey struct {
		owner common.Hash
		root  common.Hash
	}
	keys := make([]fetcherKey, N)
	for i := 0; i < N; i++ {
		owner := common.Hash{byte(i + 1)}
		root := common.Hash{byte(i + 1)}
		keys[i] = fetcherKey{owner: owner, root: root}

		sf := newTerminatedSubfetcher(db.db, db.originalRoot, owner, root, common.Address{byte(i)})
		prefetcher.fetchers[prefetcher.trieID(owner, root)] = sf
	}

	// Spawn N goroutines, each calling used() on its own subfetcher
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			addrs := make([]common.Address, addrCount)
			for j := range addrs {
				addrs[j] = common.Address{byte(idx), byte(j)}
			}
			slots := make([]common.Hash, slotCount)
			for j := range slots {
				slots[j] = common.Hash{byte(idx), byte(j)}
			}
			prefetcher.used(keys[idx].owner, keys[idx].root, addrs, slots)
		}(i)
	}
	wg.Wait()

	// Verify each subfetcher received exactly the expected data
	for i := 0; i < N; i++ {
		id := prefetcher.trieID(keys[i].owner, keys[i].root)
		fetcher := prefetcher.fetchers[id]
		if fetcher == nil {
			t.Fatalf("subfetcher %d not found", i)
		}
		if got := len(fetcher.usedAddr); got != addrCount {
			t.Errorf("subfetcher %d: len(usedAddr) = %d, want %d", i, got, addrCount)
		}
		if got := len(fetcher.usedSlot); got != slotCount {
			t.Errorf("subfetcher %d: len(usedSlot) = %d, want %d", i, got, slotCount)
		}
		for j := 0; j < addrCount && j < len(fetcher.usedAddr); j++ {
			expected := common.Address{byte(i), byte(j)}
			if fetcher.usedAddr[j] != expected {
				t.Errorf("subfetcher %d: usedAddr[%d] = %x, want %x", i, j, fetcher.usedAddr[j], expected)
				break
			}
		}
		for j := 0; j < slotCount && j < len(fetcher.usedSlot); j++ {
			expected := common.Hash{byte(i), byte(j)}
			if fetcher.usedSlot[j] != expected {
				t.Errorf("subfetcher %d: usedSlot[%d] = %x, want %x", i, j, fetcher.usedSlot[j], expected)
				break
			}
		}
	}
}

// TestConcurrentUsedParallelism verifies that concurrent used() calls on
// different subfetchers actually run in parallel rather than serializing behind
// a global lock. The test compares wall-clock time of parallel vs sequential
// execution and asserts at least a 2x speedup.
func TestConcurrentUsedParallelism(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping parallelism test in short mode")
	}
	if raceEnabled {
		// Race instrumentation serializes atomic/mutex ops, so measured
		// parallel speedup is not meaningful. The test guards against a
		// global-lock regression; -race coverage isn't the right tool.
		t.Skip("skipping parallelism test under -race: instrumentation distorts wall-clock speedup")
	}

	const N = 50          // number of subfetchers / goroutines
	const M = 5000        // iterations per goroutine
	const batchSize = 100 // addresses per used() call

	type fetcherKey struct {
		owner common.Hash
		root  common.Hash
	}

	newPrefetcherWithSubfetchers := func() (*triePrefetcher, []fetcherKey) {
		db := filledStateDB()
		p := newTriePrefetcher(db.db, db.originalRoot, "parallelism", false)
		keys := make([]fetcherKey, N)
		for i := 0; i < N; i++ {
			// Encode index into two bytes to support N > 255
			owner := common.Hash{byte(i/255 + 1), byte(i%255 + 1)}
			root := common.Hash{byte(i/255 + 1), byte(i%255 + 1)}
			keys[i] = fetcherKey{owner: owner, root: root}

			sf := newTerminatedSubfetcher(db.db, db.originalRoot, owner, root, common.Address{byte(i)})
			p.fetchers[p.trieID(owner, root)] = sf
		}
		return p, keys
	}

	// Pre-create address batches (outside the timed section)
	batches := make([][]common.Address, N)
	for i := 0; i < N; i++ {
		batches[i] = make([]common.Address, batchSize)
		for j := range batches[i] {
			batches[i][j] = common.Address{byte(i), byte(j)}
		}
	}

	// Sequential baseline: single goroutine makes all N*M calls
	p1, keys1 := newPrefetcherWithSubfetchers()
	seqStart := time.Now()
	for iter := 0; iter < M; iter++ {
		for i := 0; i < N; i++ {
			p1.used(keys1[i].owner, keys1[i].root, batches[i], nil)
		}
	}
	sequential := time.Since(seqStart)

	// Parallel: N goroutines each make M calls on their own subfetcher
	p2, keys2 := newPrefetcherWithSubfetchers()
	var wg sync.WaitGroup
	parStart := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for iter := 0; iter < M; iter++ {
				p2.used(keys2[idx].owner, keys2[idx].root, batches[idx], nil)
			}
		}(i)
	}
	wg.Wait()
	parallel := time.Since(parStart)

	speedup := float64(sequential) / float64(parallel)
	t.Logf("sequential=%v parallel=%v speedup=%.1fx", sequential, parallel, speedup)
	if speedup < 2.0 {
		t.Errorf("expected at least 2x speedup, got %.1fx (sequential=%v, parallel=%v)", speedup, sequential, parallel)
	}
}

// TestUsedStateCorrectAfterReport verifies that report() correctly reads
// usedAddr/usedSlot after concurrent used() calls. It exercises both the
// account-trie and storage-trie branches in report(). Run with -race to
// detect data races on the usedLock.
func TestUsedStateCorrectAfterReport(t *testing.T) {
	metrics.Enable()

	db := filledStateDB()
	prefetcher := newTriePrefetcher(db.db, db.originalRoot, "report", false)

	// Account-trie subfetcher: root matches p.root
	acctOwner, acctRoot := common.Hash{}, db.originalRoot
	acctSF := newTerminatedSubfetcher(db.db, db.originalRoot, acctOwner, acctRoot, common.Address{})
	prefetcher.fetchers[prefetcher.trieID(acctOwner, acctRoot)] = acctSF

	// Storage-trie subfetchers: root != p.root
	const storageN = 5
	type fetcherKey struct {
		owner common.Hash
		root  common.Hash
	}
	storageKeys := make([]fetcherKey, storageN)
	for i := 0; i < storageN; i++ {
		owner := common.Hash{byte(i + 1)}
		root := common.Hash{byte(i + 1)}
		storageKeys[i] = fetcherKey{owner: owner, root: root}

		sf := newTerminatedSubfetcher(db.db, db.originalRoot, owner, root, common.Address{byte(i)})
		prefetcher.fetchers[prefetcher.trieID(owner, root)] = sf
	}

	// Concurrently call used() on all subfetchers
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		addrs := []common.Address{{0x01}, {0x02}, {0x03}}
		prefetcher.used(acctOwner, acctRoot, addrs, nil)
	}()
	for i := 0; i < storageN; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			slots := []common.Hash{{byte(idx)}, {byte(idx + 100)}}
			prefetcher.used(storageKeys[idx].owner, storageKeys[idx].root, nil, slots)
		}(i)
	}
	wg.Wait()

	// report() should not panic or race
	prefetcher.report()

	// Verify account subfetcher received its addresses
	if got := len(acctSF.usedAddr); got != 3 {
		t.Errorf("account subfetcher: len(usedAddr) = %d, want 3", got)
	}
	// Verify each storage subfetcher received its slots
	for i := 0; i < storageN; i++ {
		id := prefetcher.trieID(storageKeys[i].owner, storageKeys[i].root)
		fetcher := prefetcher.fetchers[id]
		if got := len(fetcher.usedSlot); got != 2 {
			t.Errorf("storage subfetcher %d: len(usedSlot) = %d, want 2", i, got)
		}
	}
}
