package state

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

// stubFlatReader stands in for the snapshot/pathdb flat reader: it serves a
// fixed set of hot keys without touching any trie and errors for everything
// else so the multi reader falls through to the trie reader.
type stubFlatReader struct {
	inner    StateReader
	accounts map[common.Address]bool
	slots    map[storageKey]bool
}

var errNotInFlat = errors.New("not in flat reader")

func (s *stubFlatReader) Account(addr common.Address) (*types.StateAccount, error) {
	if s.accounts[addr] {
		return s.inner.Account(addr)
	}
	return nil, errNotInFlat
}

func (s *stubFlatReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	if s.slots[storageKey{addr: addr, slot: slot}] {
		return s.inner.Storage(addr, slot)
	}
	return common.Hash{}, errNotInFlat
}

type stubCodeReader struct{}

func (stubCodeReader) Code(addr common.Address, codeHash common.Hash) ([]byte, error) {
	return nil, nil
}

func (stubCodeReader) CodeSize(addr common.Address, codeHash common.Hash) (int, error) {
	return 0, nil
}

// TestCollectStateWitnessIncludesFlatServedReads pins the V2 witness gap seen
// on mainnet: a key first resolved by the flat reader (snapshot / pathdb) is
// stored in the shared reader cache, so execution reads it without ever
// touching the trie reader. Witness collection walks trie tracers only, so
// unless collection re-resolves cached keys through the trie, the produced
// witness lacks that key's trie paths and a stateless consumer sees the
// account as nonexistent.
func TestCollectStateWitnessIncludesFlatServedReads(t *testing.T) {
	memDb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memDb, triedb.HashDefaults)
	db := NewDatabase(tdb, nil)

	var (
		hot      = common.BytesToAddress([]byte("hot-account"))
		cold     = common.BytesToAddress([]byte("cold-account"))
		contract = common.BytesToAddress([]byte("contract"))
		slot     = common.HexToHash("0x01")
		value    = common.HexToHash("0x42")
	)

	setup, err := New(types.EmptyRootHash, db)
	if err != nil {
		t.Fatalf("setup state: %v", err)
	}
	setup.SetBalance(hot, uint256.NewInt(11), tracing.BalanceChangeUnspecified)
	setup.SetBalance(cold, uint256.NewInt(22), tracing.BalanceChangeUnspecified)
	setup.SetBalance(contract, uint256.NewInt(33), tracing.BalanceChangeUnspecified)
	setup.SetState(contract, slot, value)
	root, err := setup.Commit(0, false, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit: %v", err)
	}

	// Production-shaped stack: flat reader layered over the trie reader, one
	// shared value cache, prefetch and execution roles wrapping the same cache.
	flatInner, err := db.Reader(root)
	if err != nil {
		t.Fatalf("flat inner reader: %v", err)
	}
	tr, err := newTrieReader(root, tdb)
	if err != nil {
		t.Fatalf("trie reader: %v", err)
	}
	flat := &stubFlatReader{
		inner:    flatInner,
		accounts: map[common.Address]bool{hot: true},
		slots:    map[storageKey]bool{{addr: contract, slot: slot}: true},
	}
	multi, err := newMultiStateReader(flat, tr)
	if err != nil {
		t.Fatalf("multi reader: %v", err)
	}
	shared := newReaderWithCache(newReader(stubCodeReader{}, multi))
	prefetchReader := newReaderWithCacheStats(shared, rolePrefetch)
	execReader := newReaderWithCacheStats(shared, roleProcess)

	// The import prefetcher warms the shared cache through the flat reader.
	if _, err := prefetchReader.Account(hot); err != nil {
		t.Fatalf("prefetch hot account: %v", err)
	}
	if _, err := prefetchReader.Storage(contract, slot); err != nil {
		t.Fatalf("prefetch hot slot: %v", err)
	}

	// Execution: hot key reads are cache hits, the cold account falls through
	// to the trie reader.
	sdb, err := NewWithReader(root, db, execReader)
	if err != nil {
		t.Fatalf("exec state: %v", err)
	}
	witness := &stateless.Witness{
		Headers: []*types.Header{{Number: big.NewInt(0), Root: root}},
		Codes:   make(map[string]struct{}),
		State:   make(map[string]struct{}),
	}
	sdb.SetWitness(witness)

	if got := sdb.GetBalance(hot); got.Uint64() != 11 {
		t.Fatalf("exec hot balance: got %d, want 11", got.Uint64())
	}
	if got := sdb.GetState(contract, slot); got != value {
		t.Fatalf("exec slot: got %x, want %x", got, value)
	}
	if got := sdb.GetBalance(cold); got.Uint64() != 22 {
		t.Fatalf("exec cold balance: got %d, want 22", got.Uint64())
	}
	sdb.CollectStateWitness()

	// Replay the same reads on a state rebuilt purely from the witness.
	replayDb := triedb.NewDatabase(witness.MakeHashDB(rawdb.NewMemoryDatabase()), triedb.HashDefaults)
	replay, err := New(root, NewDatabase(replayDb, nil))
	if err != nil {
		t.Fatalf("replay state (pre-state root missing from witness): %v", err)
	}
	if got := replay.GetBalance(cold); got.Uint64() != 22 {
		t.Fatalf("control failed: trie-served cold account missing from witness (balance %d, dbErr %v)", got.Uint64(), replay.Error())
	}
	if got := replay.GetBalance(hot); got.Uint64() != 11 {
		t.Errorf("flat-served account missing from witness: balance %d, want 11 (dbErr %v)", got.Uint64(), replay.Error())
	}
	if got := replay.GetState(contract, slot); got != value {
		t.Errorf("flat-served slot missing from witness: got %x, want %x (dbErr %v)", got, value, replay.Error())
	}
}
