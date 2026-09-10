package core

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

// witnessErrorFixture builds one block plus a valid witness for it, and returns
// a genesis spec callers can use to stand up further chains at the same
// pre-state.
func witnessErrorFixture(t *testing.T) (*Genesis, *types.Block, *stateless.Witness) {
	t.Helper()

	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	addr := crypto.PubkeyToAddress(key.PublicKey)
	gspec := &Genesis{
		Config: params.TestChainConfig,
		Alloc:  types.GenesisAlloc{addr: {Balance: big.NewInt(10000000000000000)}},
	}

	engine := ethash.NewFaker()

	_, blocks, _ := GenerateChainWithGenesis(gspec, engine, 1, func(i int, b *BlockGen) {
		b.SetCoinbase(common.Address{1})
		tx, _ := types.SignTx(types.NewTransaction(0, common.HexToAddress("0x1234"), big.NewInt(1000), 21000, big.NewInt(2000000000), nil), types.HomesteadSigner{}, key)
		b.AddTx(tx)
	})

	producer, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, engine, DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create witness-producing chain: %v", err)
	}
	defer producer.Stop()

	witness, _, err := producer.insertChain(types.Blocks{blocks[0]}, true, true)
	if err != nil {
		t.Fatalf("failed to build witness: %v", err)
	}

	if witness == nil {
		t.Fatal("witness-producing insert returned no witness")
	}

	return gspec, blocks[0], witness
}

// corruptWitness returns a copy of witness with its trie nodes stripped. The
// header set is left intact so the copy still passes ValidateWitnessPreState
// and the failure surfaces where it matters: during execution, when the trie
// cannot be reconstructed. That is the shape of the real defect — a witness
// that looks structurally fine and does not reproduce the block.
func corruptWitness(witness *stateless.Witness) *stateless.Witness {
	bad := witness.Copy()
	bad.State = make(map[string]struct{})

	return bad
}

// TestWitnessErrorClassification pins the contract the fetcher and the eth
// handler both rely on: a failure raised while executing against a supplied
// witness is reported as witness-attributable, an ordinary failure is not, and
// wrapping does not hide the underlying sentinel from errors.Is.
func TestWitnessErrorClassification(t *testing.T) {
	t.Run("wrapping preserves the cause", func(t *testing.T) {
		err := WitnessError(ErrStatelessStateRootMismatch)

		if !IsWitnessError(err) {
			t.Fatal("wrapped error is not reported as witness-attributable")
		}

		if !errors.Is(err, ErrStatelessStateRootMismatch) {
			t.Fatal("wrapping hid the underlying sentinel from errors.Is")
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		if err := WitnessError(nil); err != nil {
			t.Fatalf("WitnessError(nil) = %v, want nil", err)
		}
	})

	t.Run("unrelated errors are not attributed", func(t *testing.T) {
		if IsWitnessError(errors.New("chain stopped")) {
			t.Fatal("an unrelated error was reported as witness-attributable")
		}

		if IsWitnessError(nil) {
			t.Fatal("nil was reported as witness-attributable")
		}
	})
}

// TestProcessBlockWithWitnessesAttributesFailures is the stateless-node case: a
// witness that does not reconstruct the block must fail in a way the caller can
// recognise as the witness's fault, so it knows to ask a different peer rather
// than treat the block as unimportable.
func TestProcessBlockWithWitnessesAttributesFailures(t *testing.T) {
	gspec, block, witness := witnessErrorFixture(t)

	cfg := DefaultConfig()
	cfg.Stateless = true

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), cfg)
	if err != nil {
		t.Fatalf("failed to create stateless chain: %v", err)
	}
	defer chain.Stop()

	// Sanity check: the intact witness is accepted, so a rejection below is
	// about the corruption and not about the fixture.
	if _, _, err := chain.ProcessBlockWithWitnesses(block, witness); err != nil {
		t.Fatalf("valid witness rejected: %v", err)
	}

	_, _, err = chain.ProcessBlockWithWitnesses(block, corruptWitness(witness))
	if err == nil {
		t.Fatal("corrupt witness accepted")
	}

	if !IsWitnessError(err) {
		t.Fatalf("corrupt witness failure not attributed to the witness: %v", err)
	}
}

// TestInsertChainWithWitnessesFallsBackToLocalState covers the state-holding
// node: there the witness is only an execution accelerator swapped in as the
// trie read backend, so a bad one is recoverable without any network round
// trip. This asserts both halves of that — the failure is attributed to the
// witness, and re-running the same insert without one succeeds.
func TestInsertChainWithWitnessesFallsBackToLocalState(t *testing.T) {
	gspec, block, witness := witnessErrorFixture(t)

	chain, err := NewBlockChain(rawdb.NewMemoryDatabase(), gspec, ethash.NewFaker(), DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create chain: %v", err)
	}
	defer chain.Stop()

	_, err = chain.InsertChainWithWitnesses(types.Blocks{block}, false, []*stateless.Witness{corruptWitness(witness)})
	if err == nil {
		t.Fatal("corrupt witness accepted by a state-holding chain")
	}

	if !IsWitnessError(err) {
		t.Fatalf("corrupt witness failure not attributed to the witness: %v", err)
	}

	if chain.CurrentBlock().Number.Uint64() != 0 {
		t.Fatal("chain advanced despite the witness-backed import failing")
	}

	// The block itself is fine: executed against local state it imports, which
	// is what makes the fallback safe to take.
	if _, err := chain.InsertChainWithWitnesses(types.Blocks{block}, false, nil); err != nil {
		t.Fatalf("full-execution retry failed: %v", err)
	}

	if got := chain.CurrentBlock().Number.Uint64(); got != block.NumberU64() {
		t.Fatalf("chain head = %d after the fallback, want %d", got, block.NumberU64())
	}
}
