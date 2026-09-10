package state

import (
	"testing"

	"github.com/holiman/uint256"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
)

// Coverage for two parallel-vs-serial branches added by the bad-block fix. The
// cross-package V2 parity tests exercise them, but those live in package core
// and don't count toward core/state coverage; both guard a consensus
// divergence, so they're pinned here directly.

// TestPDB_Exist_DestructedThenNonceBump hits the destructed-branch nonce check:
// an account self-destructed by tx0 then given a bare nonce bump by tx1
// (NoncePath only, landing after the suicide) has zero balance but nonce>0, so
// it exists — matching serial StateDB.Exist. The balance fallback can't see
// this shape; only the nonce check does.
func TestPDB_Exist_DestructedThenNonceBump(t *testing.T) {
	pdb0, store, bals := newTestPDB(t, 0)
	addr := common.HexToAddress("0xab57")

	// tx0: fund, then self-destruct — SuicidePath at tx0, balance back to 0.
	pdb0.AddBalance(addr, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
	pdb0.SetDeferMVWrites(true)
	pdb0.EnableReadTracking()
	// Mirror the EVM handler: it clears the balance, SelfDestruct does not.
	pdb0.SubBalance(addr, uint256.NewInt(100), tracing.BalanceDecreaseSelfdestruct)
	pdb0.SelfDestruct(addr)
	pdb0.FlushToMVStore()

	// tx1: bare nonce bump at writerIdx 1, after the tx0 suicide and with no
	// CreatePath write — the only shape that reaches the nonce fallback.
	pdb1 := NewParallelStateDB(1, pdb0.base, store, bals)
	pdb1.EnableReadTracking()
	pdb1.SetNonce(addr, 9, tracing.NonceChangeUnspecified)
	pdb1.FlushToMVStore()

	// tx2: the read under test.
	pdb2 := NewParallelStateDB(2, pdb0.base, store, bals)
	pdb2.EnableReadTracking()

	if got := pdb2.GetBalance(addr); !got.IsZero() {
		t.Fatalf("precondition: balance must be 0 to isolate the nonce branch, got %s", got)
	}
	if got := pdb2.GetNonce(addr); got != 9 {
		t.Fatalf("precondition: post-destruct nonce must survive as 9, got %d", got)
	}
	if !pdb2.Exist(addr) {
		t.Fatal("Exist: destructed account with a later nonce bump must exist (serial parity)")
	}
}

// TestPDB_SettleBalanceOpsAndLogs_TransferPositionMismatch hits the
// position-match guard in tryEmitTransferAt: a leading non-transfer op at index
// 0 pushes the recorded transfer's Sub/Add pair to indices 1/2, so the first
// call sees opIdx != BalanceOpsIdx and skips instead of pairing by shape.
func TestPDB_SettleBalanceOpsAndLogs_TransferPositionMismatch(t *testing.T) {
	pdb, _, _ := newTestPDB(t, 0)
	final := settleFinalDB(t)

	other := common.HexToAddress("0x9")
	sender := common.HexToAddress("0x1")
	recipient := common.HexToAddress("0x2")
	amt := *uint256.NewInt(10)

	final.AddBalance(sender, uint256.NewInt(100), tracing.BalanceChangeUnspecified)
	final.AddBalance(other, uint256.NewInt(100), tracing.BalanceChangeUnspecified)

	pdb.BalanceOps = []BalanceOp{
		{Addr: other, Amount: *uint256.NewInt(20), IsAdd: false},
		{Addr: sender, Amount: amt, IsAdd: false},
		{Addr: recipient, Amount: amt, IsAdd: true},
	}
	pdb.Transfers = []TransferRecord{{Sender: sender, Recipient: recipient, Amount: amt, BalanceOpsIdx: 1}}

	pdb.settleBalanceOpsAndLogs(final)

	if got := final.GetBalance(other).Uint64(); got != 80 {
		t.Fatalf("other (leading non-transfer op): got %d, want 80", got)
	}
	if got := final.GetBalance(sender).Uint64(); got != 90 {
		t.Fatalf("sender: got %d, want 90", got)
	}
	if got := final.GetBalance(recipient).Uint64(); got != 10 {
		t.Fatalf("recipient: got %d, want 10", got)
	}
}
