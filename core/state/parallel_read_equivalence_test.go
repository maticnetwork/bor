package state

import (
	"bytes"
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/blockstm"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/triedb"
)

// equivMutator is the subset of write APIs the table exercises — both
// *StateDB and *ParallelStateDB implement it, so each mutation runs
// verbatim on both engines.
type equivMutator interface {
	SetNonce(common.Address, uint64, tracing.NonceChangeReason)
	AddBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int
	SetCode(common.Address, []byte, tracing.CodeChangeReason) []byte
	SetState(common.Address, common.Hash, common.Hash) common.Hash
	GetBalance(common.Address) *uint256.Int
	SubBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int
	SelfDestruct(common.Address)
}

// TestParallelReadEquivalence is a meta-test over the read API: for every
// account-mutation class a prior in-block tx can perform, it asserts that
// every ParallelStateDB read agrees with serial StateDB after the same
// mutation. The three V2 divergences fixed on this branch were all "one
// read API disagrees with serial for one account shape" — Exist ignoring
// a prior-tx nonce bump being the canonical case. A per-API/per-shape
// table makes that whole failure class regress loudly instead of shipping
// as a bad block.
//
// Each case applies its mutation as tx 0 on both engines:
//   - serial: a StateDB at the base root, mutated then Finalise(true)
//   - V2:     a writer ParallelStateDB (idx 0) mutated then FlushToMVStore,
//     observed by a reader ParallelStateDB (idx 1)
//
// then compares Exist / Empty / GetNonce / GetBalance / GetCode /
// GetCodeHash / GetCodeSize / GetState across the two. addr is exercised
// both when absent from the base state and when pre-funded in it.
func TestParallelReadEquivalence(t *testing.T) {
	addr := common.HexToAddress("0x00000000000000000000000000000000000000e0")
	slot := common.HexToHash("0x01")
	contractCode := []byte{0x60, 0x00, 0x60, 0x00, 0xf3}

	type mutation struct {
		name  string
		apply func(equivMutator)
	}
	mutations := []mutation{
		{"none", func(db equivMutator) {}},
		{"nonce-only", func(db equivMutator) {
			db.SetNonce(addr, 5, tracing.NonceChangeEoACall)
		}},
		{"balance-only", func(db equivMutator) {
			db.AddBalance(addr, uint256.NewInt(1000), tracing.BalanceChangeTransfer)
		}},
		// Realistic deployment via SetNonce(1)+SetCode — the balance-
		// preserving shape (serial getOrNewStateObject; V2 SetCode marks
		// the account created internally). Explicit CreateAccount is
		// intentionally not in the table: the EVM only calls it on a
		// truly-new address (a pre-funded target goes through the
		// balance-preserving CreateContract), and createObject's own
		// docstring warns that overwriting an existing account "might
		// lead to a consensus bug" — exercising it in isolation tests an
		// unreachable primitive, not parity. Likewise bare SetState: an
		// account left empty after Finalise is EIP-158-deleted serially
		// while its MVStore write survives, a divergence SSTORE can't
		// reach because it only runs in an account that already has code.
		{"deploy-contract", func(db equivMutator) {
			db.SetNonce(addr, 1, tracing.NonceChangeContractCreator)
			db.SetCode(addr, contractCode, tracing.CodeChangeUnspecified)
		}},
		{"deploy-with-storage", func(db equivMutator) {
			db.SetNonce(addr, 1, tracing.NonceChangeContractCreator)
			db.SetCode(addr, contractCode, tracing.CodeChangeUnspecified)
			db.SetState(addr, slot, common.HexToHash("0x2a"))
		}},
		{"nonce+balance", func(db equivMutator) {
			db.SetNonce(addr, 3, tracing.NonceChangeEoACall)
			db.AddBalance(addr, uint256.NewInt(7), tracing.BalanceChangeTransfer)
		}},
		{"destruct-after-fund", func(db equivMutator) {
			db.AddBalance(addr, uint256.NewInt(99), tracing.BalanceChangeTransfer)
			// Mirror the EVM handler: it clears the *whole* balance with an
			// explicit SubBalance (SelfDestruct is balance-neutral), which for a
			// base-prefunded account is more than this mutation just added.
			db.SubBalance(addr, db.GetBalance(addr), tracing.BalanceDecreaseSelfdestruct)
			db.SelfDestruct(addr)
		}},
	}

	for _, basePrefunded := range []bool{false, true} {
		for _, m := range mutations {
			name := m.name
			if basePrefunded {
				name += "/base-prefunded"
			} else {
				name += "/base-absent"
			}
			t.Run(name, func(t *testing.T) {
				root, db := buildEquivBase(t, addr, basePrefunded)

				serial, err := New(root, db)
				if err != nil {
					t.Fatal(err)
				}
				m.apply(serial)
				serial.Finalise(true)

				parBase, err := New(root, db)
				if err != nil {
					t.Fatal(err)
				}
				sb := NewSafeBase(parBase, 2)
				store := blockstm.NewMVStore()
				bals := blockstm.NewMVBalanceStore()

				writer := NewParallelStateDB(0, sb, store, bals)
				writer.EnableReadTracking()
				m.apply(writer)
				writer.FlushToMVStore()

				reader := NewParallelStateDB(1, sb, store, bals)
				reader.EnableReadTracking()

				assertReadEquivalence(t, serial, reader, addr, slot)
			})
		}
	}
}

// assertReadEquivalence compares every read API between a serial StateDB
// and a V2 ParallelStateDB reader for addr (and one storage slot).
func assertReadEquivalence(t *testing.T, serial *StateDB, par *ParallelStateDB, addr common.Address, slot common.Hash) {
	t.Helper()
	if got, want := par.Exist(addr), serial.Exist(addr); got != want {
		t.Errorf("Exist: parallel=%v serial=%v", got, want)
	}
	if got, want := par.Empty(addr), serial.Empty(addr); got != want {
		t.Errorf("Empty: parallel=%v serial=%v", got, want)
	}
	if got, want := par.GetNonce(addr), serial.GetNonce(addr); got != want {
		t.Errorf("GetNonce: parallel=%d serial=%d", got, want)
	}
	if got, want := par.GetBalance(addr), serial.GetBalance(addr); got.Cmp(want) != 0 {
		t.Errorf("GetBalance: parallel=%s serial=%s", got, want)
	}
	if got, want := par.GetCode(addr), serial.GetCode(addr); !bytes.Equal(got, want) {
		t.Errorf("GetCode: parallel=%x serial=%x", got, want)
	}
	if got, want := par.GetCodeHash(addr), serial.GetCodeHash(addr); got != want {
		t.Errorf("GetCodeHash: parallel=%s serial=%s", got, want)
	}
	if got, want := par.GetCodeSize(addr), serial.GetCodeSize(addr); got != want {
		t.Errorf("GetCodeSize: parallel=%d serial=%d", got, want)
	}
	if got, want := par.GetState(addr, slot), serial.GetState(addr, slot); got != want {
		t.Errorf("GetState: parallel=%s serial=%s", got, want)
	}
}

// buildEquivBase returns a committed base state (and its database) where
// addr either does not exist or is pre-funded with a balance + nonce, so
// each mutation can be exercised against both starting points.
func buildEquivBase(t *testing.T, addr common.Address, prefunded bool) (common.Hash, Database) {
	t.Helper()
	memdb := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(memdb, triedb.HashDefaults)
	db := NewDatabase(tdb, nil)
	pre, err := New(types.EmptyRootHash, db)
	if err != nil {
		t.Fatal(err)
	}
	if prefunded {
		pre.AddBalance(addr, uint256.NewInt(500), tracing.BalanceChangeTransfer)
		pre.SetNonce(addr, 1, tracing.NonceChangeEoACall)
	}
	root, err := pre.Commit(0, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatal(err)
	}
	return root, db
}
