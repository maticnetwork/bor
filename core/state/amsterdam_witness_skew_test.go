package state

import (
	"math/big"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// The EIP-7928 read on a destructed account walks that account's storage trie
// through the StateDB's reader, and every node a reader touches is harvested
// into the block witness by CollectStateWitness. So the read is not only a
// wasted disk access before Amsterdam — it changes what the witness contains.
//
// That is what makes the gate matter rather than being a tidiness fix. Witness
// content is not committed to any header field, so a producer and a consumer
// disagreeing about it is invisible to consensus: the consumer simply demands
// nodes the producer never recorded and fails the import. This test pins the
// direction of that difference so a future change cannot quietly reintroduce
// it.
func TestDestructedReadWitnessSkew(t *testing.T) {
	t.Parallel()

	var (
		contract = common.BytesToAddress([]byte("destructed-contract"))
		readSlot = common.HexToHash("0x01")
	)

	memDb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memDb, triedb.HashDefaults)
	db := NewDatabase(tdb, nil)

	// Commit an account whose storage trie has enough slots to hold internal
	// nodes, so a read has a proof path to pull in rather than a lone leaf.
	setup, err := New(types.EmptyRootHash, db)
	if err != nil {
		t.Fatalf("setup state: %v", err)
	}
	setup.SetBalance(contract, uint256.NewInt(1), tracing.BalanceChangeUnspecified)
	for i := int64(1); i <= 32; i++ {
		setup.SetState(contract, common.BigToHash(big.NewInt(i)), common.BigToHash(big.NewInt(i*7)))
	}
	root, err := setup.Commit(0, false, false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit: %v", err)
	}

	// collect drives one committed-state read on a destructed account under the
	// given rules and returns the witness gathered from the reader.
	collect := func(t *testing.T, amsterdam bool) *stateless.Witness {
		t.Helper()

		tr, err := newTrieReader(root, tdb)
		if err != nil {
			t.Fatalf("trie reader: %v", err)
		}
		sdb, err := NewWithReader(root, db, newReader(stubCodeReader{}, tr))
		if err != nil {
			t.Fatalf("state: %v", err)
		}
		witness := &stateless.Witness{
			Headers: []*types.Header{{Number: big.NewInt(0), Root: root}},
			Codes:   make(map[string]struct{}),
			State:   make(map[string]struct{}),
		}
		sdb.SetWitness(witness)

		// Same setup on both sides, so the account-trie access it costs cancels
		// out and the only difference left is the gated storage read.
		obj := sdb.getOrNewStateObject(contract)
		sdb.stateObjectsDestruct[contract] = obj

		cfg := *params.TestChainConfig
		if amsterdam {
			cfg.ShanghaiBlock = big.NewInt(0)
			cfg.AmsterdamBlock = big.NewInt(0)
		}
		sdb.Prepare(cfg.Rules(common.Big0, false, 0), contract, common.Address{}, nil, nil, nil)

		if got := sdb.GetCommittedState(contract, readSlot); got != (common.Hash{}) {
			t.Fatalf("destructed account should read empty, got %x", got)
		}
		if err := sdb.Error(); err != nil {
			t.Fatalf("state error: %v", err)
		}
		sdb.CollectStateWitness()
		return witness
	}

	witnessOff := collect(t, false)
	witnessOn := collect(t, true)

	t.Logf("witness state nodes: gated-off=%d gated-on=%d", len(witnessOff.State), len(witnessOn.State))

	extra := 0
	for node := range witnessOn.State {
		if _, ok := witnessOff.State[node]; !ok {
			extra++
		}
	}
	if extra == 0 {
		t.Fatal("ungating the read added no witness nodes: the skew this gate exists to prevent is not being reproduced, so this test would not catch a regression")
	}
	t.Logf("nodes required by the reading path but absent without it: %d", extra)

	// The dangerous direction: everything the non-reading producer recorded is
	// also present for the reading consumer, so the failure can only come from
	// what the consumer additionally demands.
	for node := range witnessOff.State {
		if _, ok := witnessOn.State[node]; !ok {
			t.Fatal("gated-off witness holds a node the gated-on witness lacks; the difference is not a clean superset")
		}
	}
}
