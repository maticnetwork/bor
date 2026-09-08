package state

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// countingReader counts storage reads reaching the underlying reader.
type countingReader struct {
	Reader
	storageReads int
}

func (r *countingReader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	r.storageReads++
	return r.Reader.Storage(addr, slot)
}

// EIP-7928 makes GetCommittedState read a destructed account's slot purely to
// record the access for the block-level access list. The read walks the trie, so
// under a witness-building reader it adds the account's storage proof path to the
// witness. A node performing it against a witness from a producer that did not
// would fail the import, so it stays behind the Amsterdam gate that the access
// list itself lives behind.
func TestDestructedSlotReadIsAmsterdamGated(t *testing.T) {
	t.Parallel()

	addr := common.HexToAddress("0xdead")
	slot := common.HexToHash("0x01")

	newDestructedState := func(t *testing.T) (*StateDB, *countingReader) {
		t.Helper()

		db := NewDatabase(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil), nil)
		inner, err := db.Reader(types.EmptyRootHash)
		if err != nil {
			t.Fatalf("reader: %v", err)
		}
		counting := &countingReader{Reader: inner}

		statedb, err := NewWithReader(types.EmptyRootHash, db, counting)
		if err != nil {
			t.Fatalf("statedb: %v", err)
		}

		// Put the account in the destructed set so GetCommittedState takes the
		// branch under test.
		obj := statedb.getOrNewStateObject(addr)
		statedb.stateObjectsDestruct[addr] = obj
		counting.storageReads = 0

		return statedb, counting
	}

	t.Run("pre-amsterdam does not read", func(t *testing.T) {
		t.Parallel()

		statedb, counting := newDestructedState(t)
		statedb.Prepare(params.Rules{}, addr, common.Address{}, nil, nil, nil)

		if got := statedb.GetCommittedState(addr, slot); got != (common.Hash{}) {
			t.Errorf("expected empty slot, got %x", got)
		}
		if counting.storageReads != 0 {
			t.Errorf("expected no reader access before Amsterdam, got %d", counting.storageReads)
		}
	})

	t.Run("post-amsterdam reads", func(t *testing.T) {
		t.Parallel()

		statedb, counting := newDestructedState(t)
		statedb.Prepare(params.Rules{IsAmsterdam: true}, addr, common.Address{}, nil, nil, nil)

		if got := statedb.GetCommittedState(addr, slot); got != (common.Hash{}) {
			t.Errorf("expected empty slot, got %x", got)
		}
		if counting.storageReads != 1 {
			t.Errorf("expected one reader access after Amsterdam, got %d", counting.storageReads)
		}
	})

	t.Run("copy carries the gate", func(t *testing.T) {
		t.Parallel()

		statedb, _ := newDestructedState(t)
		statedb.Prepare(params.Rules{IsAmsterdam: true}, addr, common.Address{}, nil, nil, nil)

		if !statedb.Copy().amsterdam {
			t.Error("Copy dropped the Amsterdam gate")
		}
	})
}
