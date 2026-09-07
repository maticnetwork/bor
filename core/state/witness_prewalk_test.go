package state

import (
	"math/big"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// prewalkFixture builds the production-shaped reader stack from
// witness_readset_test.go with one flat-served hot account, returning the
// statedb wired for witness recording plus the pieces the tests inspect.
func prewalkFixture(t *testing.T) (*StateDB, *readerWithCache, common.Address, *stateless.Witness) {
	t.Helper()
	memDb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memDb, triedb.HashDefaults)
	db := NewDatabase(tdb, nil)

	hot := common.BytesToAddress([]byte("prewalk-hot-account"))

	setup, err := New(types.EmptyRootHash, db)
	if err != nil {
		t.Fatalf("setup state: %v", err)
	}
	setup.SetBalance(hot, uint256.NewInt(7), tracing.BalanceChangeUnspecified)
	root, err := setup.Commit(0, false, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit: %v", err)
	}

	flatInner, err := db.Reader(root)
	if err != nil {
		t.Fatalf("flat inner reader: %v", err)
	}
	tr, err := newTrieReader(root, tdb)
	if err != nil {
		t.Fatalf("trie reader: %v", err)
	}
	flat := &stubFlatReader{inner: flatInner, accounts: map[common.Address]bool{hot: true}}
	multi, err := newMultiStateReader(flat, tr)
	if err != nil {
		t.Fatalf("multi reader: %v", err)
	}
	shared := newReaderWithCache(newReader(stubCodeReader{}, multi))
	sdb, err := NewWithReader(root, db, newReaderWithCacheStats(shared, roleProcess))
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	witness := &stateless.Witness{
		Headers: []*types.Header{{Number: big.NewInt(0), Root: root}},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	sdb.SetWitness(witness)
	return sdb, shared, hot, witness
}

func TestWitnessReadSetPrewalkWalksCachedKeys(t *testing.T) {
	sdb, shared, hot, _ := prewalkFixture(t)

	stop := sdb.StartWitnessReadSetPrewalk()
	defer stop()

	// Flat-served read: lands in the shared cache without touching the trie.
	if got := sdb.GetBalance(hot); got.Uint64() != 7 {
		t.Fatalf("hot balance: got %d, want 7", got.Uint64())
	}

	// The prewalker must claim the key without any collect call. The
	// deadline is generous: polling exits on success, so it only matters on
	// heavily loaded runners.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if v, ok := shared.accounts.Load(hot); ok && v.(*accountCacheEntry).walked.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("prewalker did not walk the cached key")
		}
		time.Sleep(time.Millisecond)
	}

	// Stop is idempotent and safe to invoke repeatedly.
	stop()
	stop()
}

func TestWitnessReadSetPrewalkNoopWithoutWitness(t *testing.T) {
	sdb, shared, hot, _ := prewalkFixture(t)
	sdb.SetWitness(nil)

	// Without a witness the returned stop must still be callable, and no
	// prewalker may start: a cached key must stay unwalked.
	stop := sdb.StartWitnessReadSetPrewalk()
	if got := sdb.GetBalance(hot); got.Uint64() != 7 {
		t.Fatalf("hot balance: got %d, want 7", got.Uint64())
	}
	time.Sleep(20 * witnessReadSetPrewalkEvery)
	if v, ok := shared.accounts.Load(hot); ok && v.(*accountCacheEntry).walked.Load() {
		t.Fatal("prewalker ran despite nil witness")
	}
	stop()

	// Same for a reader chain without the shared cache wrapper.
	plain, err := New(types.EmptyRootHash, NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), triedb.HashDefaults), nil))
	if err != nil {
		t.Fatalf("plain state: %v", err)
	}
	w := &stateless.Witness{
		Headers: []*types.Header{{Number: big.NewInt(0)}},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	plain.SetWitness(w)
	stop = plain.StartWitnessReadSetPrewalk()
	stop()

	// And for a shared cache whose chain has no trie reader at all: the
	// prewalker must refuse to start rather than sweep into a nil reader.
	sdb2, shared2, hot2, _ := prewalkFixture(t)
	flatOnly, err := newMultiStateReader(&stubFlatReader{inner: shared2, accounts: map[common.Address]bool{hot2: true}})
	if err != nil {
		t.Fatalf("flat-only reader: %v", err)
	}
	cacheOnly := newReaderWithCache(newReader(stubCodeReader{}, flatOnly))
	sdb2.reader = newReaderWithCacheStats(cacheOnly, roleProcess)
	stop = sdb2.StartWitnessReadSetPrewalk()
	if _, err := cacheOnly.Account(hot2); err != nil {
		t.Fatalf("cache-only read: %v", err)
	}
	time.Sleep(20 * witnessReadSetPrewalkEvery)
	if v, ok := cacheOnly.accounts.Load(hot2); ok && v.(*accountCacheEntry).walked.Load() {
		t.Fatal("prewalker ran despite missing trie reader")
	}
	stop()
}

func TestCollectStateWitnessStopsPrewalker(t *testing.T) {
	sdb, shared, hot, witness := prewalkFixture(t)

	sdb.StartWitnessReadSetPrewalk()
	if got := sdb.GetBalance(hot); got.Uint64() != 7 {
		t.Fatalf("hot balance: got %d, want 7", got.Uint64())
	}
	// Collect without an explicit stop: it must stop the prewalker itself
	// and drain the key into the witness.
	sdb.CollectStateWitness()
	if len(witness.State) == 0 {
		t.Fatal("collect recorded no state nodes")
	}

	// After collect, the prewalker is gone: a key cached later stays
	// unwalked until the next drain.
	late := common.BytesToAddress([]byte("post-collect-account"))
	sdb.GetBalance(late)
	time.Sleep(20 * witnessReadSetPrewalkEvery)
	if v, ok := shared.accounts.Load(late); ok {
		if v.(*accountCacheEntry).walked.Load() {
			t.Fatal("prewalker still running after CollectStateWitness")
		}
	}
}

func TestResolveCachedKeysIntoTrieIdempotent(t *testing.T) {
	sdb, shared, hot, _ := prewalkFixture(t)

	if got := sdb.GetBalance(hot); got.Uint64() != 7 {
		t.Fatalf("hot balance: got %d, want 7", got.Uint64())
	}
	// The stats wrapper is what a real StateDB carries; the finder must see
	// through it as well as through the bare cache chain.
	if findTrieReader(sdb.reader) == nil {
		t.Fatal("no trie reader through the stats-wrapped chain")
	}
	tr := findTrieReader(shared.Reader)
	if tr == nil {
		t.Fatal("no trie reader in chain")
	}
	if walked := shared.resolveCachedKeysIntoTrie(tr, 4); walked == 0 {
		t.Fatal("first sweep walked nothing")
	}
	// Cache one more key: the next sweep must claim only it, not re-walk
	// keys claimed by the first sweep.
	sdb.GetBalance(common.BytesToAddress([]byte("second-account")))
	if walked := shared.resolveCachedKeysIntoTrie(tr, 4); walked != 1 {
		t.Fatalf("second sweep walked %d keys, want 1", walked)
	}
	// Nothing new cached: the generation fast path skips the Range entirely.
	if walked := shared.resolveCachedKeysIntoTrie(tr, 4); walked != 0 {
		t.Fatalf("third sweep re-walked %d keys, want 0", walked)
	}
}

func TestWitnessReadSetPrewalkRestart(t *testing.T) {
	sdb, shared, hot, _ := prewalkFixture(t)
	base := runtime.NumGoroutine()

	stop1 := sdb.StartWitnessReadSetPrewalk()
	// A second start must stop the first walker rather than leak it.
	stop2 := sdb.StartWitnessReadSetPrewalk()

	deadline := time.Now().Add(30 * time.Second)
	for runtime.NumGoroutine() > base+1 {
		if time.Now().After(deadline) {
			t.Fatalf("first prewalker leaked: %d goroutines, base %d", runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}

	// The replacement walker is live.
	if got := sdb.GetBalance(hot); got.Uint64() != 7 {
		t.Fatalf("hot balance: got %d, want 7", got.Uint64())
	}
	for {
		if v, ok := shared.accounts.Load(hot); ok && v.(*accountCacheEntry).walked.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement prewalker did not walk the cached key")
		}
		time.Sleep(time.Millisecond)
	}
	stop2()
	stop1()
}
